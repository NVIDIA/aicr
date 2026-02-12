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
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/NVIDIA/eidos/pkg/errors"
	"github.com/NVIDIA/eidos/pkg/k8s"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
)

// waitForJobCompletion waits for the Job to complete successfully or fail.
func (d *Deployer) waitForJobCompletion(ctx context.Context, timeout time.Duration) error {
	// Use watch API for efficient polling
	watcher, err := d.clientset.BatchV1().Jobs(d.config.Namespace).Watch(
		ctx,
		metav1.ListOptions{
			FieldSelector: fmt.Sprintf("metadata.name=%s", d.config.JobName),
			Watch:         true,
		},
	)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to watch Job", err)
	}
	defer watcher.Stop()

	// Create timeout context
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		select {
		case <-timeoutCtx.Done():
			return errors.New(errors.ErrCodeTimeout, fmt.Sprintf("timeout waiting for Job completion after %v", timeout))

		case event, ok := <-watcher.ResultChan():
			if !ok {
				return errors.New(errors.ErrCodeInternal, "watch channel closed unexpectedly")
			}

			if event.Type == watch.Error {
				return errors.New(errors.ErrCodeInternal, fmt.Sprintf("watch error: %v", event.Object))
			}

			job, ok := event.Object.(*batchv1.Job)
			if !ok {
				continue
			}

			// Check for completion
			for _, condition := range job.Status.Conditions {
				if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
					return nil // Job completed successfully
				}
				if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
					return errors.New(errors.ErrCodeInternal, fmt.Sprintf("job failed: %s", condition.Message))
				}
			}
		}
	}
}

// getSnapshotFromConfigMap retrieves the snapshot data from ConfigMap.
func (d *Deployer) getSnapshotFromConfigMap(ctx context.Context) ([]byte, error) {
	// Parse ConfigMap name from output URI
	namespace, name, err := parseConfigMapName(d.config.Output)
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

// deleteConfigMap deletes the snapshot ConfigMap.
//
//nolint:unused // Kept for future debugging purposes
func (d *Deployer) deleteConfigMap(ctx context.Context) error {
	namespace, name, err := parseConfigMapName(d.config.Output)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse ConfigMap URI", err)
	}

	err = d.clientset.CoreV1().ConfigMaps(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	return k8s.IgnoreNotFound(err)
}

// StreamLogs streams logs from the Job's Pod to the provided writer.
// It will follow the logs until the context is canceled.
// Returns when the context is canceled or an error occurs.
func (d *Deployer) StreamLogs(ctx context.Context, w io.Writer, prefix string) error {
	// Find Pod for this Job
	pods, err := d.clientset.CoreV1().Pods(d.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=eidos",
	})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to list Pods", err)
	}

	if len(pods.Items) == 0 {
		return errors.New(errors.ErrCodeNotFound, fmt.Sprintf("no Pods found for Job %s", d.config.JobName))
	}

	// Get logs from first Pod with Follow=true
	pod := pods.Items[0]
	req := d.clientset.CoreV1().Pods(d.config.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Follow: true,
	})

	logs, err := req.Stream(ctx)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to stream logs", err)
	}
	defer logs.Close()

	// Stream logs line by line with prefix
	scanner := bufio.NewScanner(logs)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if prefix != "" {
				fmt.Fprintf(w, "%s %s\n", prefix, scanner.Text())
			} else {
				fmt.Fprintln(w, scanner.Text())
			}
		}
	}

	return scanner.Err()
}

// GetPodLogs retrieves logs from the Job's Pod.
func (d *Deployer) GetPodLogs(ctx context.Context) (string, error) {
	// Find Pod for this Job
	pods, err := d.clientset.CoreV1().Pods(d.config.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=eidos",
	})
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to list Pods", err)
	}

	if len(pods.Items) == 0 {
		return "", errors.New(errors.ErrCodeNotFound, fmt.Sprintf("no Pods found for Job %s", d.config.JobName))
	}

	// Get logs from first Pod (there should only be one)
	pod := pods.Items[0]
	req := d.clientset.CoreV1().Pods(d.config.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{})

	logs, err := req.Stream(ctx)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to stream logs", err)
	}
	defer logs.Close()

	buf := new(bytes.Buffer)
	if _, err := io.Copy(buf, logs); err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to read logs", err)
	}

	return buf.String(), nil
}

// WaitForPodReady waits for the Job's Pod to be in Running state.
// This is useful for streaming logs before Job completes.
func (d *Deployer) WaitForPodReady(ctx context.Context, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, timeout, true,
		func(ctx context.Context) (bool, error) {
			pods, err := d.clientset.CoreV1().Pods(d.config.Namespace).List(ctx, metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/name=eidos",
			})
			if err != nil {
				return false, err
			}

			if len(pods.Items) == 0 {
				return false, nil // Pod not created yet
			}

			pod := pods.Items[0]
			if pod.Status.Phase == corev1.PodRunning {
				return true, nil
			}

			// Check for failed Pod
			if pod.Status.Phase == corev1.PodFailed {
				return false, errors.New(errors.ErrCodeInternal, fmt.Sprintf("pod failed: %s", pod.Status.Message))
			}

			return false, nil // Keep waiting
		},
	)
}

// parseConfigMapName parses a ConfigMap URI (cm://namespace/name) and returns namespace, name.
// Returns error if the URI format is invalid.
func parseConfigMapName(uri string) (namespace, name string, err error) {
	// Expected format: cm://namespace/name
	if !strings.HasPrefix(uri, "cm://") {
		return "", "", errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid ConfigMap URI format: expected cm://namespace/name, got %q", uri))
	}

	// Remove cm:// prefix
	path := strings.TrimPrefix(uri, "cm://")

	// Split into namespace/name
	parts := strings.SplitN(path, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("invalid ConfigMap URI format: expected cm://namespace/name, got %q", uri))
	}

	return parts[0], parts[1], nil
}
