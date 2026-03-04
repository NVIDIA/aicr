// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package pod

import (
	"context"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// WaitForJobCompletion waits for a Kubernetes Job to complete successfully or fail.
// Returns nil if job completes successfully, error if job fails or context deadline exceeded.
//
// This function uses the Kubernetes watch API for efficient monitoring instead of polling.
func WaitForJobCompletion(ctx context.Context, client kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	// Create timeout context
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Use watch API for efficient monitoring
	watcher, err := client.BatchV1().Jobs(namespace).Watch(
		timeoutCtx,
		metav1.ListOptions{
			FieldSelector: "metadata.name=" + name,
		},
	)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to watch Job", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return errors.Wrap(errors.ErrCodeTimeout, "job completion timeout", timeoutCtx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return errors.New(errors.ErrCodeInternal, "watch channel closed unexpectedly")
			}

			job, ok := event.Object.(*batchv1.Job)
			if !ok {
				continue
			}

			// Check job status conditions
			for _, condition := range job.Status.Conditions {
				if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
					return nil
				}
				if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
					return errors.NewWithContext(errors.ErrCodeInternal, "job failed", map[string]interface{}{
						"namespace": namespace,
						"name":      name,
						"reason":    condition.Reason,
						"message":   condition.Message,
					})
				}
			}
		}
	}
}
