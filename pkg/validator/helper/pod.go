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

package helper

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

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
	T          *testing.T
}

// CreatePodFromTemplate creates a pod from a YAML template file
func (p *PodLifecycle) CreatePodFromTemplate(ctx context.Context, templatePath string, data map[string]string) (*v1.Pod, error) {
	pod, err := loadPodFromTemplate(templatePath, data)
	if err != nil {
		return nil, fmt.Errorf("failed to load template: %w", err)
	}

	createCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	createdPod, err := p.ClientSet.CoreV1().Pods(p.Namespace).Create(createCtx, pod, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to create pod: %w", err)
	}

	p.T.Logf("Successfully created pod %s/%s", createdPod.Namespace, createdPod.Name)
	return createdPod, nil
}

// WaitForPodByName waits for a pod with the given name to be created in the namespace
// and returns the pod object when found or an error if the timeout is reached
func (p *PodLifecycle) WaitForPodByName(ctx context.Context, podName string, timeout time.Duration) (*v1.Pod, error) {
	p.T.Logf("Waiting for pod %s to be created...", podName)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var foundPod *v1.Pod
	var err error

	// Poll until pod is found or timeout occurs
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for pod %s to be created", podName)
		case <-ticker.C:
			foundPod, err = p.ClientSet.CoreV1().Pods(p.Namespace).Get(ctx, podName, metav1.GetOptions{})
			if err == nil {
				p.T.Logf("Found pod %s (status: %s)", podName, foundPod.Status.Phase)
				return foundPod, nil
			}
			// Continue polling only if pod not found; fail fast on other errors
			if !errors.IsNotFound(err) {
				return nil, fmt.Errorf("error getting pod %s: %w", podName, err)
			}
		}
	}
}

// WaitForPodSuccess waits for a pod to reach Succeeded phase
func (p *PodLifecycle) WaitForPodSuccess(ctx context.Context, pod *v1.Pod, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	p.T.Logf("Waiting for pod %s to reach Succeeded state...", pod.Name)

	// Poll the pod status instead of using the framework's wait condition
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			// Get current pod state for error message using a fresh, short-lived context
			diagCtx, diagCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer diagCancel()

			//nolint:contextcheck // intentionally using a fresh context for diagnostics after parent timeout
			foundPod, err := p.ClientSet.CoreV1().Pods(p.Namespace).Get(diagCtx, pod.Name, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("timed out waiting for pod %s to succeed, and failed to get current state: %w", pod.Name, err)
			}

			// Provide detailed information about why it failed
			phase := foundPod.Status.Phase
			reason := foundPod.Status.Reason
			message := foundPod.Status.Message

			var containerStatuses []string
			for _, cs := range foundPod.Status.ContainerStatuses {
				switch {
				case cs.State.Waiting != nil:
					containerStatuses = append(containerStatuses, fmt.Sprintf("%s: Waiting (%s: %s)",
						cs.Name, cs.State.Waiting.Reason, cs.State.Waiting.Message))
				case cs.State.Running != nil:
					containerStatuses = append(containerStatuses, fmt.Sprintf("%s: Running", cs.Name))
				case cs.State.Terminated != nil:
					containerStatuses = append(containerStatuses, fmt.Sprintf("%s: Terminated (exit code: %d, reason: %s)",
						cs.Name, cs.State.Terminated.ExitCode, cs.State.Terminated.Reason))
				}
			}

			errorMsg := fmt.Sprintf("timed out waiting for pod %s to succeed after %v. Current state: Phase=%s",
				pod.Name, timeout, phase)
			if reason != "" {
				errorMsg += fmt.Sprintf(", Reason=%s", reason)
			}
			if message != "" {
				errorMsg += fmt.Sprintf(", Message=%s", message)
			}
			if len(containerStatuses) > 0 {
				errorMsg += fmt.Sprintf(", Container statuses: [%s]", strings.Join(containerStatuses, "; "))
			}

			return fmt.Errorf("%s", errorMsg)

		case <-ticker.C:
			foundPod, err := p.ClientSet.CoreV1().Pods(p.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
			if err != nil {
				p.T.Logf("Failed to get pod status: %v", err)
				continue
			}

			p.T.Logf("Pod %s current phase: %s", pod.Name, foundPod.Status.Phase)

			if foundPod.Status.Phase == v1.PodSucceeded {
				p.T.Logf("Pod %s successfully completed", pod.Name)
				return nil
			}

			// If pod failed, return immediately with error
			if foundPod.Status.Phase == v1.PodFailed {
				reason := foundPod.Status.Reason
				message := foundPod.Status.Message
				errorMsg := fmt.Sprintf("pod %s failed", pod.Name)
				if reason != "" {
					errorMsg += fmt.Sprintf(" (reason: %s)", reason)
				}
				if message != "" {
					errorMsg += fmt.Sprintf(" (message: %s)", message)
				}
				return fmt.Errorf("%s", errorMsg)
			}
		}
	}
}

// GetPodLogs retrieves logs from a pod
func (p *PodLifecycle) GetPodLogs(ctx context.Context, pod *v1.Pod) (string, error) {
	// Check if pod has containers
	if len(pod.Spec.Containers) == 0 {
		return "", fmt.Errorf("pod %s has no containers", pod.Name)
	}

	logsReq := p.ClientSet.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &v1.PodLogOptions{
		Container: pod.Spec.Containers[0].Name,
	})

	logsReader, err := logsReq.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("failed to get logs stream: %w", err)
	}
	defer func() {
		if closeErr := logsReader.Close(); closeErr != nil {
			p.T.Logf("Error closing logs reader: %v", closeErr)
		}
	}()

	logBytes, err := io.ReadAll(logsReader)
	if err != nil {
		return "", fmt.Errorf("failed to read logs: %w", err)
	}

	return string(logBytes), nil
}

// CleanupPod deletes a pod
func (p *PodLifecycle) CleanupPod(ctx context.Context, pod *v1.Pod) error {
	cleanupCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	p.T.Logf("Cleaning up pod %s/%s", pod.Namespace, pod.Name)
	return p.ClientSet.CoreV1().Pods(p.Namespace).Delete(cleanupCtx, pod.Name, metav1.DeleteOptions{})
}

// ExecCommandInPod executes a command in a pod and returns stdout, stderr, and any error
func (p *PodLifecycle) ExecCommandInPod(ctx context.Context, pod *v1.Pod, command []string) (string, string, error) {
	// Add a reasonable timeout for exec commands to prevent hanging
	execCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
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
		return "", "", fmt.Errorf("failed to create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(execCtx, remotecommand.StreamOptions{
		Stdin:  nil, // No stdin needed since we set Stdin: false in PodExecOptions
		Stdout: &stdout,
		Stderr: &stderr,
		Tty:    false,
	})

	if err != nil {
		return stdout.String(), stderr.String(), fmt.Errorf("command execution failed: %w", err)
	}

	return stdout.String(), stderr.String(), nil
}

// WaitForPodRunning waits for a pod to reach Running phase
func (p *PodLifecycle) WaitForPodRunning(ctx context.Context, pod *v1.Pod, timeout time.Duration) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	p.T.Logf("Waiting for pod %s to reach Running state...", pod.Name)

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("timeout waiting for pod %s to reach Running state", pod.Name)
		case <-ticker.C:
			foundPod, err := p.ClientSet.CoreV1().Pods(pod.Namespace).Get(waitCtx, pod.Name, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("failed to get pod %s: %w", pod.Name, err)
			}

			switch foundPod.Status.Phase {
			case v1.PodRunning:
				p.T.Logf("Pod %s is now in Running state", pod.Name)
				return nil
			case v1.PodFailed:
				return fmt.Errorf("pod %s entered Failed phase while waiting for Running", pod.Name)
			case v1.PodPending, v1.PodSucceeded, v1.PodUnknown:
				// continue polling
			}
		}
	}
}

// LoadPodFromTemplate reads and processes a pod template file with variable substitution
// It takes a template path and a map of variables to replace in the format ${KEY}
func loadPodFromTemplate(templatePath string, data map[string]string) (*v1.Pod, error) {
	content, err := os.ReadFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read template: %w", err)
	}

	yamlContent := string(content)
	for key, value := range data {
		yamlContent = strings.ReplaceAll(yamlContent, "${"+key+"}", value)
	}
	pod := &v1.Pod{}
	if err := yaml.Unmarshal([]byte(yamlContent), pod); err != nil {
		return nil, fmt.Errorf("failed to parse template: %w", err)
	}

	return pod, nil
}
