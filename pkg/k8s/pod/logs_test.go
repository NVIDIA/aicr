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

package pod_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// NOTE: The fake K8s client does not support GetLogs().Stream() for returning
// real log data. It returns an empty body for any pod, even nonexistent ones.
// Therefore we can only test error paths that fail before or during Stream().

func TestStreamLogs_CancelledContext(t *testing.T) {
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset()
	var buf bytes.Buffer

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := pod.StreamLogs(ctx, client, "default", "test-pod", &buf)
	if err == nil {
		t.Error("expected error for cancelled context")
	}
}

func TestGetPodLogs_CancelledContext(t *testing.T) {
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := pod.GetPodLogs(ctx, client, "default", "test-pod")
	if err == nil {
		t.Error("expected error for cancelled context")
	}
}

func TestStreamLogs_Success(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(p)
	var buf bytes.Buffer

	// Fake client returns "fake logs" for any stream request.
	err := pod.StreamLogs(context.Background(), client, "default", "test-pod", &buf)
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
	if buf.Len() == 0 {
		t.Error("expected non-empty buffer from fake client")
	}
}

func TestGetPodLogs_Success(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(p)

	// Fake client returns "fake logs" for any stream request.
	result, err := pod.GetPodLogs(context.Background(), client, "default", "test-pod")
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
	if result == "" {
		t.Error("expected non-empty result from fake client")
	}
}
