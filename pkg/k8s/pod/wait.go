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
	"log/slog"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
)

// WaitForPodSucceeded waits for a pod to reach the Succeeded phase.
// Returns nil on PodSucceeded, error on PodFailed, error on timeout.
// Performs an initial Get to catch already-terminal pods, then uses the
// watch API for efficient monitoring.
func WaitForPodSucceeded(ctx context.Context, client kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	slog.Info("waiting for pod to reach Succeeded state", "name", name)

	// Fast path: pod may already be in a terminal phase.
	current, err := client.CoreV1().Pods(namespace).Get(timeoutCtx, name, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to get pod", err)
	}
	if done, checkErr := checkPodPhase(current); done {
		return checkErr
	}

	watcher, err := client.CoreV1().Pods(namespace).Watch(
		timeoutCtx,
		metav1.ListOptions{
			FieldSelector: "metadata.name=" + name,
		},
	)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to watch pod", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return errors.Wrap(errors.ErrCodeTimeout, "pod wait timeout", timeoutCtx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return errors.New(errors.ErrCodeInternal, "watch channel closed unexpectedly")
			}

			watchedPod, ok := event.Object.(*corev1.Pod)
			if !ok {
				continue
			}

			slog.Info("pod current phase", "name", watchedPod.Name, "status", watchedPod.Status.Phase)

			if done, checkErr := checkPodPhase(watchedPod); done {
				return checkErr
			}
		}
	}
}

// checkPodPhase returns (true, nil) for Succeeded, (true, error) for Failed,
// and (false, nil) when the pod is still running/pending.
func checkPodPhase(p *corev1.Pod) (bool, error) {
	switch p.Status.Phase {
	case corev1.PodSucceeded:
		slog.Info("pod successfully completed", "name", p.Name)
		return true, nil
	case corev1.PodFailed:
		return true, errors.NewWithContext(errors.ErrCodeInternal, "pod failed", map[string]interface{}{
			"namespace": p.Namespace,
			"name":      p.Name,
			"reason":    p.Status.Reason,
			"message":   p.Status.Message,
		})
	case corev1.PodPending, corev1.PodRunning, corev1.PodUnknown:
		return false, nil
	default:
		return false, nil
	}
}

// WaitForPodReady waits for a pod to become ready within the specified timeout.
// Returns nil if pod becomes ready, error if timeout or pod fails.
func WaitForPodReady(ctx context.Context, client kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return wait.PollUntilContextCancel(timeoutCtx, defaults.PodPollInterval, true, func(ctx context.Context) (bool, error) {
		pod, err := client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, errors.Wrap(errors.ErrCodeInternal, "failed to get pod", err)
		}

		// Check if pod is ready
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
				return true, nil
			}
		}

		// Check for failed state
		if pod.Status.Phase == corev1.PodFailed {
			return false, errors.NewWithContext(errors.ErrCodeInternal, "pod failed", map[string]interface{}{
				"namespace": namespace,
				"name":      name,
				"reason":    pod.Status.Reason,
				"message":   pod.Status.Message,
			})
		}

		return false, nil
	})
}
