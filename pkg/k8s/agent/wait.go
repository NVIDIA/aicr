// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
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

	return pod.StreamLogs(ctx, d.clientset, d.config.Namespace, podName, w)
}

// GetPodLogs retrieves logs from the Job's Pod.
func (d *Deployer) GetPodLogs(ctx context.Context) (string, error) {
	// Find Pod for this Job
	podName, err := d.findPodName(ctx)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to find pod for log retrieval", err)
	}

	return pod.GetPodLogs(ctx, d.clientset, d.config.Namespace, podName)
}

// WaitForPodReady waits for the Job's Pod to be in Running state.
// This is useful for streaming logs before Job completes.
func (d *Deployer) WaitForPodReady(ctx context.Context, timeout time.Duration) error {
	watchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Discover pod name by polling with K8s standard utility
	var podName string
	err := wait.PollUntilContextCancel(watchCtx, defaults.PodPollInterval, true, func(ctx context.Context) (bool, error) {
		pods, listErr := d.clientset.CoreV1().Pods(d.config.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=aicr",
		})
		if listErr != nil {
			return false, errors.Wrap(errors.ErrCodeInternal, "failed to list Pods", listErr)
		}
		if len(pods.Items) > 0 {
			podName = pods.Items[0].Name
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return errors.New(errors.ErrCodeTimeout, fmt.Sprintf("timeout waiting for Pod creation after %v", timeout))
	}

	// Calculate remaining timeout
	deadline, ok := watchCtx.Deadline()
	if !ok {
		return errors.New(errors.ErrCodeInternal, "context deadline not set")
	}
	remainingTimeout := time.Until(deadline)
	if remainingTimeout <= 0 {
		return errors.New(errors.ErrCodeTimeout, fmt.Sprintf("timeout waiting for Pod ready after %v", timeout))
	}

	// Wait for pod to be ready using shared function
	return pod.WaitForPodReady(ctx, d.clientset, d.config.Namespace, podName, remainingTimeout)
}

// findPodName finds the pod name by label selector for this Job.
func (d *Deployer) findPodName(ctx context.Context) (string, error) {
	pods, err := d.clientset.CoreV1().Pods(d.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=aicr",
	})
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to list Pods", err)
	}

	if len(pods.Items) == 0 {
		return "", errors.New(errors.ErrCodeNotFound, fmt.Sprintf("no Pods found for Job %s", d.config.JobName))
	}

	return pods.Items[0].Name, nil
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
