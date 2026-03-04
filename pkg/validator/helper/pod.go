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
	"bytes"
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
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/yaml"
)

// PodLifecycle handles creation, verification, and cleanup of a pod
type PodLifecycle struct {
	ClientSet  kubernetes.Interface
	RESTConfig *rest.Config
	Namespace  string
}

// CreatePodFromTemplate creates a pod from a YAML template file
func (p *PodLifecycle) CreatePodFromTemplate(ctx context.Context, templatePath string, data map[string]string) (*v1.Pod, error) {
	pod, err := loadPodFromTemplate(templatePath, data)
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

// WaitForPodByName waits for a pod with the given name to be created in the namespace
// and returns the pod object when found or an error if the timeout is reached
func (p *PodLifecycle) WaitForPodByName(ctx context.Context, podName string, timeout time.Duration) (*v1.Pod, error) {
	slog.Info("Waiting for pod to be created", "name", podName)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var foundPod *v1.Pod
	var err error

	// Poll until pod is found or timeout occurs
	ticker := time.NewTicker(defaults.PodPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "timed out waiting for pod to be created", ctx.Err())
		case <-ticker.C:
			foundPod, err = p.ClientSet.CoreV1().Pods(p.Namespace).Get(ctx, podName, metav1.GetOptions{})
			if err == nil {
				slog.Info("Found pod", "name", podName, "status", foundPod.Status.Phase)
				return foundPod, nil
			}
			// Continue polling only if pod not found; fail fast on other errors
			if !errors.IsNotFound(err) {
				return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "error getting pod", err)
			}
		}
	}
}

// WaitForPodSuccess waits for a pod to reach Succeeded phase.
// Delegates to the shared pod.WaitForPodSucceeded utility.
func (p *PodLifecycle) WaitForPodSuccess(ctx context.Context, v1Pod *v1.Pod, timeout time.Duration) error {
	return podutil.WaitForPodSucceeded(ctx, p.ClientSet, v1Pod.Namespace, v1Pod.Name, timeout)
}

// GetPodLogs retrieves logs from a pod.
// Delegates to the shared podutil.GetPodLogs utility, targeting the first container.
//
//nolint:unparam // string return used by callers in performance and deployment packages
func (p *PodLifecycle) GetPodLogs(ctx context.Context, pod *v1.Pod) (string, error) {
	if len(pod.Spec.Containers) == 0 {
		return "", aicrErrors.New(aicrErrors.ErrCodeInternal, "pod has no containers")
	}
	return podutil.GetPodLogs(ctx, p.ClientSet, pod.Namespace, pod.Name, pod.Spec.Containers[0].Name)
}

// CleanupPod deletes a pod
func (p *PodLifecycle) CleanupPod(ctx context.Context, pod *v1.Pod) error {
	cleanupCtx, cancel := context.WithTimeout(ctx, defaults.K8sJobCompletionTimeout)
	defer cancel()

	slog.Info("Cleaning up pod", "namespace", pod.Namespace, "name", pod.Name)
	return p.ClientSet.CoreV1().Pods(p.Namespace).Delete(cleanupCtx, pod.Name, metav1.DeleteOptions{})
}

// ExecCommandInPod executes a command in a pod and returns stdout, stderr, and any error
func (p *PodLifecycle) ExecCommandInPod(ctx context.Context, pod *v1.Pod, command []string) (string, string, error) {
	// Add a reasonable timeout for exec commands to prevent hanging
	execCtx, cancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
	defer cancel()

	req := p.ClientSet.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec").
		VersionedParams(&v1.PodExecOptions{
			Command: command,
			Stdin:   false,
			Stdout:  true,
			Stderr:  true,
			TTY:     false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(p.RESTConfig, "POST", req.URL())
	if err != nil {
		return "", "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to create executor", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(execCtx, remotecommand.StreamOptions{
		Stdin:  nil, // No stdin needed since we set Stdin: false in PodExecOptions
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})

	if err != nil {
		return stdout.String(), stderr.String(), aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "command execution failed", err)
	}

	return stdout.String(), stderr.String(), nil
}

// WaitForPodRunning waits for a pod to reach Running phase
func (p *PodLifecycle) WaitForPodRunning(ctx context.Context, pod *v1.Pod, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	slog.Info("Waiting for pod to reach Running state", "name", pod.Name)

	ticker := time.NewTicker(defaults.PodPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-waitCtx.Done():
			return aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "timeout waiting for pod to reach Running state", waitCtx.Err())
		case <-ticker.C:
			foundPod, err := p.ClientSet.CoreV1().Pods(pod.Namespace).Get(waitCtx, pod.Name, metav1.GetOptions{})
			if err != nil {
				return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to get pod", err)
			}

			switch foundPod.Status.Phase {
			case v1.PodRunning:
				slog.Info("Pod is now in Running state", "name", pod.Name)
				return nil
			case v1.PodFailed:
				return aicrErrors.New(aicrErrors.ErrCodeInternal, "pod entered Failed phase while waiting for Running")
			case v1.PodPending, v1.PodSucceeded, v1.PodUnknown:
				// continue polling
			}
		}
	}
}

// LoadPodFromTemplate reads and processes a pod template file with variable substitution
// It takes a template path and a map of variables to replace in the format ${KEY}
func loadPodFromTemplate(templatePath string, data map[string]string) (*v1.Pod, error) {
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
