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

package pod

import (
	"context"
	"log/slog"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
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
			keyNamespace: p.Namespace,
			keyName:      p.Name,
			keyReason:    p.Status.Reason,
			keyMessage:   p.Status.Message,
		})
	case corev1.PodPending, corev1.PodRunning, corev1.PodUnknown:
		return false, nil
	default:
		return false, nil
	}
}

// WaitForTermination watches a pod and returns nil once it has reached a
// terminal state — either the pod object has been deleted (the API server
// emitted a watch.Deleted event or a subsequent Get returns NotFound) or its
// phase is Succeeded or Failed. Unlike WaitForPodSucceeded, a Failed phase is
// NOT treated as an error here: the caller has already decided that any
// terminal disposition is acceptable (e.g., RBAC cleanup races).
//
// If the watch channel closes before a terminal state is observed, this
// function performs ONE retry by re-issuing the watch starting from the most
// recent ResourceVersion observed. If the second watch also closes without
// reaching a terminal state, an ErrCodeUnavailable error is returned so
// callers can decide log severity rather than swallow the failure.
//
// Context cancellation/timeout is surfaced as an ErrCodeTimeout error.
func WaitForTermination(ctx context.Context, client kubernetes.Interface, namespace, name string) error {
	// Fast path: pod may already be deleted or terminal.
	current, err := client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return errors.Wrap(errors.ErrCodeInternal, "failed to get pod", err)
	}
	if isPodTerminal(current) {
		return nil
	}

	resourceVersion := current.ResourceVersion

	// First watch attempt.
	terminal, lastRV, err := watchUntilTerminal(ctx, client, namespace, name, resourceVersion)
	if err != nil {
		return err
	}
	if terminal {
		return nil
	}

	// Watch closed before terminal state — retry once with the latest RV.
	slog.Debug("pod watch closed before terminal state, retrying", "namespace", namespace, "pod", name)
	terminal, _, err = watchUntilTerminal(ctx, client, namespace, name, lastRV)
	if err != nil {
		return err
	}
	if terminal {
		return nil
	}

	return errors.WrapWithContext(errors.ErrCodeUnavailable,
		"pod watch closed before terminal state after retry", nil,
		map[string]any{"namespace": namespace, "pod": name})
}

// watchUntilTerminal opens a Watch on the named pod starting at resourceVersion
// and consumes events until either:
//   - the pod reaches a terminal state (returns terminal=true)
//   - the pod is Deleted (returns terminal=true)
//   - the watch channel closes (returns terminal=false, lastRV=most recent RV)
//   - the context is canceled (returns ErrCodeTimeout)
//   - the Watch call itself fails (returns ErrCodeInternal)
//
// On a successful terminal observation, lastRV may be empty.
func watchUntilTerminal(ctx context.Context, client kubernetes.Interface, namespace, name, resourceVersion string) (terminal bool, lastRV string, err error) {
	watcher, watchErr := client.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector:   "metadata.name=" + name,
		ResourceVersion: resourceVersion,
	})
	if watchErr != nil {
		return false, resourceVersion, errors.Wrap(errors.ErrCodeInternal, "failed to watch pod", watchErr)
	}
	defer watcher.Stop()

	lastRV = resourceVersion
	for {
		select {
		case <-ctx.Done():
			return false, lastRV, errors.Wrap(errors.ErrCodeTimeout, "pod termination wait timeout", ctx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return false, lastRV, nil
			}
			if event.Type == watch.Deleted {
				return true, lastRV, nil
			}
			watchedPod, isPod := event.Object.(*corev1.Pod)
			if !isPod {
				continue
			}
			if watchedPod.ResourceVersion != "" {
				lastRV = watchedPod.ResourceVersion
			}
			if isPodTerminal(watchedPod) {
				return true, lastRV, nil
			}
		}
	}
}

// isPodTerminal reports whether a pod's phase is terminal (Succeeded or Failed).
func isPodTerminal(p *corev1.Pod) bool {
	return p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed
}

// WaitForPodReady waits for a pod to become ready within the specified timeout.
// Returns nil if pod becomes ready, error if timeout or pod fails.
// Uses the watch API for efficient monitoring with a fast-path Get for
// pods that are already ready or failed.
func WaitForPodReady(ctx context.Context, client kubernetes.Interface, namespace, name string, timeout time.Duration) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Fast path: pod may already be ready or failed.
	current, err := client.CoreV1().Pods(namespace).Get(timeoutCtx, name, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to get pod", err)
	}
	if done, checkErr := checkPodReady(current); done {
		return checkErr
	}

	watcher, err := client.CoreV1().Pods(namespace).Watch(timeoutCtx, metav1.ListOptions{
		FieldSelector:   "metadata.name=" + name,
		ResourceVersion: current.ResourceVersion,
	})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to watch pod", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-timeoutCtx.Done():
			return errors.Wrap(errors.ErrCodeTimeout, "pod ready wait timeout", timeoutCtx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return errors.New(errors.ErrCodeUnavailable, "pod watch channel closed before ready")
			}
			watchedPod, isPod := event.Object.(*corev1.Pod)
			if !isPod {
				continue
			}
			if done, checkErr := checkPodReady(watchedPod); done {
				return checkErr
			}
		}
	}
}

// checkPodReady returns (true, nil) when the pod is Ready, (true, error) when
// the pod has Failed, and (false, nil) otherwise.
func checkPodReady(p *corev1.Pod) (bool, error) {
	for _, condition := range p.Status.Conditions {
		if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
			return true, nil
		}
	}
	if p.Status.Phase == corev1.PodFailed {
		return true, errors.NewWithContext(errors.ErrCodeInternal, "pod failed", map[string]interface{}{
			keyNamespace: p.Namespace,
			keyName:      p.Name,
			keyReason:    p.Status.Reason,
			keyMessage:   p.Status.Message,
		})
	}
	return false, nil
}
