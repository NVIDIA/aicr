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

func TestWaitForPodSucceeded(t *testing.T) {
	tests := []struct {
		name    string
		pod     corev1.Pod
		cancel  bool
		timeout time.Duration
		wantErr bool
	}{
		{
			name: "already succeeded",
			pod: corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
				Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
			},
			timeout: 5 * time.Second,
			wantErr: false,
		},
		{
			name: "pod failed",
			pod: corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
				Status: corev1.PodStatus{
					Phase:   corev1.PodFailed,
					Reason:  "OOMKilled",
					Message: "container ran out of memory",
				},
			},
			timeout: 2 * time.Second,
			wantErr: true,
		},
		{
			name: "context cancelled",
			pod: corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
				Status:     corev1.PodStatus{Phase: corev1.PodPending},
			},
			cancel:  true,
			timeout: 5 * time.Second,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
			client := fake.NewSimpleClientset(&tt.pod)

			ctx := context.Background()
			if tt.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			err := pod.WaitForPodSucceeded(ctx, client, "default", "test-pod", tt.timeout)
			if (err != nil) != tt.wantErr {
				t.Errorf("WaitForPodSucceeded() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestWaitForPodReady(t *testing.T) {
	tests := []struct {
		name    string
		pod     corev1.Pod
		cancel  bool
		timeout time.Duration
		wantErr bool
	}{
		{
			name: "already ready",
			pod: corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{Type: corev1.PodReady, Status: corev1.ConditionTrue},
					},
				},
			},
			timeout: 5 * time.Second,
			wantErr: false,
		},
		{
			name: "pod failed",
			pod: corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
				Status: corev1.PodStatus{
					Phase:   corev1.PodFailed,
					Reason:  "OOMKilled",
					Message: "container ran out of memory",
				},
			},
			timeout: 2 * time.Second,
			wantErr: true,
		},
		{
			name: "timeout on pending",
			pod: corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
				Status:     corev1.PodStatus{Phase: corev1.PodPending},
			},
			timeout: 500 * time.Millisecond,
			wantErr: true,
		},
		{
			name: "context cancelled",
			pod: corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
				Status:     corev1.PodStatus{Phase: corev1.PodPending},
			},
			cancel:  true,
			timeout: 5 * time.Second,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
			client := fake.NewSimpleClientset(&tt.pod)

			ctx := context.Background()
			if tt.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			err := pod.WaitForPodReady(ctx, client, "default", "test-pod", tt.timeout)
			if (err != nil) != tt.wantErr {
				t.Errorf("WaitForPodReady() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
