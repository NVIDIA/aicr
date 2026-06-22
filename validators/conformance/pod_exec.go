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

package main

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	k8sexec "k8s.io/client-go/util/exec"
)

type podExecOptions struct {
	DefaultContainerAnnotation string
	PreferredContainerName     string
}

type podExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// selectExecContainer chooses which container to exec into using caller-provided
// preferences. It first honors a configured default-container annotation when it
// names an existing container, then a configured preferred container name, then
// falls back to the pod's first container. Callers must ensure the pod has at
// least one container.
func selectExecContainer(pod *corev1.Pod, opts podExecOptions) string {
	containers := pod.Spec.Containers
	if annotated := pod.Annotations[opts.DefaultContainerAnnotation]; opts.DefaultContainerAnnotation != "" && annotated != "" {
		for i := range containers {
			if containers[i].Name == annotated {
				return annotated
			}
		}
	}
	if opts.PreferredContainerName != "" {
		for i := range containers {
			if containers[i].Name == opts.PreferredContainerName {
				return opts.PreferredContainerName
			}
		}
	}
	return containers[0].Name
}

type podExecFunc func(context.Context, *validators.Context, string, string, []string, podExecOptions) (podExecResult, error)

type podExecExecutorFactory func(*rest.Config, string, string) (remotecommand.Executor, error)

var newPodExecExecutor podExecExecutorFactory = func(config *rest.Config, method, requestURL string) (remotecommand.Executor, error) {
	parsedURL, err := url.Parse(requestURL)
	if err != nil {
		return nil, err
	}
	return remotecommand.NewSPDYExecutor(config, method, parsedURL)
}

func execPodCommand(
	streamCtx context.Context,
	ctx *validators.Context,
	namespace string,
	podName string,
	command []string,
	opts podExecOptions,
) (podExecResult, error) {

	pod, err := ctx.Clientset.CoreV1().Pods(namespace).Get(streamCtx, podName, metav1.GetOptions{})
	if err != nil {
		return podExecResult{}, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to get pod %s/%s before exec", namespace, podName), err)
	}
	if len(pod.Spec.Containers) == 0 {
		return podExecResult{}, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("pod %s/%s has no containers", namespace, podName))
	}

	req := ctx.Clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: selectExecContainer(pod, opts),
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := newPodExecExecutor(ctx.RESTConfig, http.MethodPost, req.URL().String())
	if err != nil {
		return podExecResult{}, errors.Wrap(errors.ErrCodeInternal, "failed to create pod exec executor", err)
	}

	var stdout, stderr bytes.Buffer
	streamErr := executor.StreamWithContext(streamCtx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	result := podExecResult{
		Stdout: stdout.String(),
		Stderr: stderr.String(),
	}
	if streamErr == nil {
		return result, nil
	}

	var exitErr k8sexec.ExitError
	if stderrors.As(streamErr, &exitErr) {
		result.ExitCode = exitErr.ExitStatus()
		return result, nil
	}
	return result, errors.Wrap(errors.ErrCodeInternal,
		fmt.Sprintf("pod exec stream failed for %s/%s", namespace, podName), streamErr)
}
