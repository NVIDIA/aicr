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

package helper

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	podutil "github.com/NVIDIA/aicr/pkg/k8s/pod"

	"github.com/NVIDIA/aicr/pkg/defaults"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// PodLifecycle handles creation, verification, and cleanup of a pod.
type PodLifecycle struct {
	ClientSet kubernetes.Interface
	Namespace string
}

// CreatePodFromTemplate creates a pod from a YAML template file.
func (p *PodLifecycle) CreatePodFromTemplate(ctx context.Context, templatePath string, data map[string]string) (*v1.Pod, error) {
	pod, err := LoadPodFromTemplate(templatePath, data)
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to load template", err)
	}

	createCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	createdPod, err := p.ClientSet.CoreV1().Pods(p.Namespace).Create(createCtx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to create pod", err)
	}

	slog.Info("Successfully created pod", "namespace", createdPod.Namespace, "name", createdPod.Name)
	return createdPod, nil
}

// WaitForPodByName waits for a pod with the given name to be created in the namespace.
func (p *PodLifecycle) WaitForPodByName(ctx context.Context, podName string, timeout time.Duration) (*v1.Pod, error) {
	slog.Info("Waiting for pod to be created", "name", podName)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Fast path: pod may already exist.
	foundPod, err := p.ClientSet.CoreV1().Pods(p.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err == nil {
		slog.Info("Found pod", "name", podName, "status", foundPod.Status.Phase)
		return foundPod, nil
	}
	if !errors.IsNotFound(err) {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "error getting pod", err)
	}

	// Watch for the pod to appear.
	watcher, err := p.ClientSet.CoreV1().Pods(p.Namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + podName,
	})
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to watch for pod", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "timed out waiting for pod to be created", ctx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				// Watch channel closed — re-check pod directly.
				recheck, recheckErr := p.ClientSet.CoreV1().Pods(p.Namespace).Get(ctx, podName, metav1.GetOptions{})
				if recheckErr != nil {
					return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "watch closed and pod not found", recheckErr)
				}
				slog.Info("Found pod", "name", podName, "status", recheck.Status.Phase)
				return recheck, nil
			}

			if event.Type == watch.Deleted {
				continue
			}

			watchedPod, ok := event.Object.(*v1.Pod)
			if !ok {
				continue
			}
			slog.Info("Found pod", "name", podName, "status", watchedPod.Status.Phase)
			return watchedPod, nil
		}
	}
}

// WaitForPodSuccess waits for a pod to reach Succeeded phase.
func (p *PodLifecycle) WaitForPodSuccess(ctx context.Context, v1Pod *v1.Pod, timeout time.Duration) error {
	return podutil.WaitForPodSucceeded(ctx, p.ClientSet, v1Pod.Namespace, v1Pod.Name, timeout)
}

// GetPodLogs retrieves logs from a pod.
//
//nolint:unparam // string return used by callers
func (p *PodLifecycle) GetPodLogs(ctx context.Context, pod *v1.Pod) (string, error) {
	if len(pod.Spec.Containers) == 0 {
		return "", aicrErrors.New(aicrErrors.ErrCodeInternal, "pod has no containers")
	}
	return podutil.GetPodLogs(ctx, p.ClientSet, pod.Namespace, pod.Name, pod.Spec.Containers[0].Name)
}

// CleanupPod deletes a pod.
func (p *PodLifecycle) CleanupPod(ctx context.Context, pod *v1.Pod) error {
	cleanupCtx, cancel := context.WithTimeout(ctx, defaults.K8sJobCompletionTimeout)
	defer cancel()

	slog.Info("Cleaning up pod", "namespace", pod.Namespace, "name", pod.Name)
	return p.ClientSet.CoreV1().Pods(p.Namespace).Delete(cleanupCtx, pod.Name, metav1.DeleteOptions{})
}

// podPredicate checks whether a pod has reached the desired state.
// Returns (done, error): done=true means stop watching, error non-nil means failure.
type podPredicate func(pod *v1.Pod) (bool, error)

// watchPodUntil watches a pod until the predicate returns done=true or the context expires.
// It handles the standard watch lifecycle: fast-path check, watch creation, channel closure
// recovery, and deletion detection.
func (p *PodLifecycle) watchPodUntil(ctx context.Context, pod *v1.Pod, description string, check podPredicate) error {
	// Fast path: pod may already satisfy the predicate.
	currentPod, err := p.ClientSet.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to get pod", err)
	}
	if done, phaseErr := check(currentPod); done {
		return phaseErr
	}

	// Watch for state changes.
	watcher, err := p.ClientSet.CoreV1().Pods(pod.Namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector:   "metadata.name=" + pod.Name,
		ResourceVersion: currentPod.ResourceVersion,
	})
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to watch pod", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "timeout waiting for pod to be "+description, ctx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				recheck, recheckErr := p.ClientSet.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
				if recheckErr != nil {
					return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "watch closed and failed to get pod", recheckErr)
				}
				if done, phaseErr := check(recheck); done {
					return phaseErr
				}
				return aicrErrors.New(aicrErrors.ErrCodeInternal, "watch channel closed, pod still not "+description)
			}

			if event.Type == watch.Deleted {
				return aicrErrors.New(aicrErrors.ErrCodeInternal, "pod was deleted while waiting for "+description)
			}

			watchedPod, ok := event.Object.(*v1.Pod)
			if !ok {
				continue
			}
			if done, phaseErr := check(watchedPod); done {
				return phaseErr
			}
		}
	}
}

// WaitForPodRunning waits for a pod to reach Running phase.
func (p *PodLifecycle) WaitForPodRunning(ctx context.Context, pod *v1.Pod, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	slog.Info("Waiting for pod to reach Running state", "name", pod.Name)
	return p.watchPodUntil(waitCtx, pod, "Running", checkPodRunningOrTerminal)
}

// checkPodRunningOrTerminal returns true if the pod is in Running, Succeeded, or Failed phase.
// For Running and Succeeded it returns nil error; for Failed it returns an error.
func checkPodRunningOrTerminal(pod *v1.Pod) (bool, error) {
	switch pod.Status.Phase { //nolint:exhaustive // Pending and Unknown are not terminal
	case v1.PodRunning:
		slog.Info("Pod is now in Running state", "name", pod.Name)
		return true, nil
	case v1.PodSucceeded:
		slog.Info("Pod reached Succeeded state", "name", pod.Name)
		return true, nil
	case v1.PodFailed:
		return true, aicrErrors.New(aicrErrors.ErrCodeInternal, "pod entered Failed phase while waiting for Running")
	default:
		return false, nil
	}
}

// LoadPodFromTemplate reads and processes a pod template file with variable substitution.
func LoadPodFromTemplate(templatePath string, data map[string]string) (*v1.Pod, error) {
	content, err := os.ReadFile(filepath.Clean(templatePath)) //nolint:gosec // G703 -- path from embedded template config
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to read template", err)
	}

	yamlContent := string(content)
	for key, value := range data {
		yamlContent = strings.ReplaceAll(yamlContent, "${"+key+"}", value)
	}
	pod := &v1.Pod{}
	if err := yaml.Unmarshal([]byte(yamlContent), pod); err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to parse template", err)
	}

	return pod, nil
}
