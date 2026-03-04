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

package pod_test

import (
	"context"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestWaitForPodSucceeded_AlreadySucceeded(t *testing.T) {
	succeededPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(succeededPod)

	err := pod.WaitForPodSucceeded(context.Background(), client, "default", "test-pod", 5*time.Second)
	if err != nil {
		t.Errorf("expected no error for succeeded pod, got: %v", err)
	}
}

func TestWaitForPodSucceeded_PodFailed(t *testing.T) {
	failedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase:   corev1.PodFailed,
			Reason:  "OOMKilled",
			Message: "container ran out of memory",
		},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(failedPod)

	err := pod.WaitForPodSucceeded(context.Background(), client, "default", "test-pod", 2*time.Second)
	if err == nil {
		t.Error("expected error for failed pod")
	}
}

func TestWaitForPodSucceeded_ContextCancelled(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(p)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := pod.WaitForPodSucceeded(ctx, client, "default", "test-pod", 5*time.Second)
	if err == nil {
		t.Error("expected error for cancelled context")
	}
}

func TestWaitForPodReady_AlreadyReady(t *testing.T) {
	readyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(readyPod)

	err := pod.WaitForPodReady(context.Background(), client, "default", "test-pod", 5*time.Second)
	if err != nil {
		t.Errorf("expected no error for ready pod, got: %v", err)
	}
}

func TestWaitForPodReady_PodFailed(t *testing.T) {
	failedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase:   corev1.PodFailed,
			Reason:  "OOMKilled",
			Message: "container ran out of memory",
		},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(failedPod)

	err := pod.WaitForPodReady(context.Background(), client, "default", "test-pod", 2*time.Second)
	if err == nil {
		t.Error("expected error for failed pod")
	}
}

func TestWaitForPodReady_Timeout(t *testing.T) {
	pendingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(pendingPod)

	err := pod.WaitForPodReady(context.Background(), client, "default", "test-pod", 500*time.Millisecond)
	if err == nil {
		t.Error("expected timeout error for pending pod")
	}
}

func TestWaitForPodReady_ContextCancelled(t *testing.T) {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(p)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := pod.WaitForPodReady(ctx, client, "default", "test-pod", 5*time.Second)
	if err == nil {
		t.Error("expected error for cancelled context")
	}
}
