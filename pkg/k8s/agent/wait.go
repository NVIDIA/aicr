// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package agent

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// waitForJobCompletion waits for the Job to complete successfully or fail.
func (d *Deployer) waitForJobCompletion(ctx context.Context, timeout time.Duration) error {
	return pod.WaitForJobCompletion(ctx, d.clientset, d.config.Namespace, d.config.JobName, timeout)
}

// getSnapshotFromConfigMap retrieves the snapshot data from ConfigMap.
func (d *Deployer) getSnapshotFromConfigMap(ctx context.Context) ([]byte, error) {
	// Parse ConfigMap name from output URI
	namespace, name, err := pod.ParseConfigMapURI(d.config.Output)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse ConfigMap URI", err)
	}

	// Get ConfigMap
	cm, err := d.clientset.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("failed to get ConfigMap %s/%s", namespace, name), err)
	}

	// Extract snapshot data
	snapshot, ok := cm.Data["snapshot.yaml"]
	if !ok {
		return nil, errors.New(errors.ErrCodeNotFound, fmt.Sprintf("ConfigMap %s/%s does not contain 'snapshot.yaml' key", namespace, name))
	}

	return []byte(snapshot), nil
}

// StreamLogs streams logs from the Job's Pod to the provided writer.
// It will follow the logs until the context is canceled.
// Returns when the context is canceled or an error occurs.
func (d *Deployer) StreamLogs(ctx context.Context, w io.Writer, prefix string) error {
	// Find Pod for this Job
	podName, err := d.findPodName(ctx)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to find pod for log streaming", err)
	}

	// Stream logs using shared function
	// Note: shared function doesn't support prefix, so we need to wrap the writer if prefix is needed
	if prefix != "" {
		w = &prefixWriter{writer: w, prefix: prefix}
	}

	return pod.StreamLogs(ctx, d.clientset, d.config.Namespace, podName, "", w)
}

// GetPodLogs retrieves logs from the Job's Pod.
func (d *Deployer) GetPodLogs(ctx context.Context) (string, error) {
	// Find Pod for this Job
	podName, err := d.findPodName(ctx)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to find pod for log retrieval", err)
	}

	return pod.GetPodLogs(ctx, d.clientset, d.config.Namespace, podName, "")
}

// WaitForPodReady waits for the Job's Pod to be in Running state.
// This is useful for streaming logs before Job completes.
func (d *Deployer) WaitForPodReady(ctx context.Context, timeout time.Duration) error {
	watchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Discover pod name via watch (or fast-path List for existing pods).
	podName, err := d.findOrWatchPodName(watchCtx)
	if err != nil {
		return err
	}

	deadline, ok := watchCtx.Deadline()
	if !ok {
		return errors.New(errors.ErrCodeInternal, "context deadline not set")
	}
	remainingTimeout := time.Until(deadline)
	if remainingTimeout <= 0 {
		return errors.New(errors.ErrCodeTimeout, fmt.Sprintf("timeout waiting for Pod ready after %v", timeout))
	}

	return pod.WaitForPodReady(ctx, d.clientset, d.config.Namespace, podName, remainingTimeout)
}

// findPodName finds the pod name by label selector for this Job.
// One-shot: returns ErrCodeNotFound if no pod is currently labeled.
// Skips pods that are being deleted or have already failed so an
// orphaned pod from a prior run is not selected.
func (d *Deployer) findPodName(ctx context.Context) (string, error) {
	pods, err := d.clientset.CoreV1().Pods(d.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: agentLabelSelector,
	})
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to list Pods", err)
	}

	name := pickLivePod(pods.Items)
	if name == "" {
		return "", errors.New(errors.ErrCodeNotFound, fmt.Sprintf("no Pods found for Job %s", d.config.JobName))
	}
	return name, nil
}

// pickLivePod returns the name of the youngest pod that is neither being
// deleted nor in a Failed phase. Returns "" if no usable pod exists.
func pickLivePod(pods []corev1.Pod) string {
	var best *corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase == corev1.PodFailed {
			continue
		}
		if best == nil || p.CreationTimestamp.After(best.CreationTimestamp.Time) {
			best = p
		}
	}
	if best == nil {
		return ""
	}
	return best.Name
}

// findOrWatchPodName returns the agent pod's name. If the pod already exists
// (List), return immediately; otherwise watch for an Added event until ctx is
// canceled.
func (d *Deployer) findOrWatchPodName(ctx context.Context) (string, error) {
	pods, err := d.clientset.CoreV1().Pods(d.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: agentLabelSelector,
	})
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to list Pods", err)
	}
	if name := pickLivePod(pods.Items); name != "" {
		return name, nil
	}

	watcher, err := d.clientset.CoreV1().Pods(d.config.Namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector:   agentLabelSelector,
		ResourceVersion: pods.ResourceVersion,
	})
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to watch Pods", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", errors.Wrap(errors.ErrCodeTimeout, "timeout waiting for Pod creation", ctx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return "", errors.New(errors.ErrCodeUnavailable, "Pod watch channel closed before pod observed")
			}
			p, isPod := event.Object.(*corev1.Pod)
			if !isPod {
				continue
			}
			return p.Name, nil
		}
	}
}

// prefixWriter wraps an io.Writer to add a prefix to each line.
type prefixWriter struct {
	writer io.Writer
	prefix string
}

func (pw *prefixWriter) Write(p []byte) (n int, err error) {
	line := fmt.Sprintf("%s %s", pw.prefix, string(p))
	_, err = pw.writer.Write([]byte(line))
	if err != nil {
		return 0, errors.Wrap(errors.ErrCodeInternal, "failed to write prefixed log line", err)
	}
	return len(p), nil
}
