// Copyright 2026 NVIDIA CORPORATION & AFFILIATES
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
//
// SPDX-License-Identifier: Apache-2.0

package kubeclient

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// ExecResult carries the captured streams from a pod exec. Stdout and
// stderr are returned separately so callers can decide whether to
// surface stderr in error messages or include it in the parsed output.
type ExecResult struct {
	Stdout string
	Stderr string
}

// ExecInPod runs `command` inside `container` of `pod` in `namespace`
// via the kube-apiserver's SPDY exec channel. Returns the captured
// stdout/stderr.
//
// Used by both the discovery probes (sysfs reads, nvidia-smi parse) and
// the Phase-2 connectivity ping matrix. The helper builds its own
// clientset from the supplied REST config so callers don't need to
// thread two clients around — there's a one-time TLS/transport cost per
// call that's negligible against the cost of an exec round-trip.
func ExecInPod(ctx context.Context, restConfig *rest.Config, namespace, pod, container string, command []string) (ExecResult, error) {
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return ExecResult{}, fmt.Errorf("create clientset: %w", err)
	}

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return ExecResult{}, fmt.Errorf("create SPDY executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	streamErr := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	res := ExecResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if streamErr != nil {
		return res, fmt.Errorf("exec %v in %s/%s (stderr: %q): %w",
			command, namespace, pod, res.Stderr, streamErr)
	}
	return res, nil
}

// ExecStdoutInPod is a convenience wrapper that returns only stdout —
// matches the signature of the historical execInPod helper in
// pkg/networkoperatorplugin/discovery.go so existing call sites can
// switch over without changing each line.
func ExecStdoutInPod(ctx context.Context, restConfig *rest.Config, namespace, pod, container string, command []string) (string, error) {
	res, err := ExecInPod(ctx, restConfig, namespace, pod, container, command)
	return res.Stdout, err
}
