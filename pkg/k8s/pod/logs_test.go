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

func TestStreamLogs(t *testing.T) {
	tests := []struct {
		name    string
		pod     *corev1.Pod
		cancel  bool
		wantErr bool
		wantOut bool
	}{
		{
			name:    "cancelled context",
			pod:     nil,
			cancel:  true,
			wantErr: true,
		},
		{
			name: "success",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			},
			wantErr: false,
			wantOut: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var client *fake.Clientset //nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
			if tt.pod != nil {
				client = fake.NewSimpleClientset(tt.pod) //nolint:staticcheck
			} else {
				client = fake.NewSimpleClientset() //nolint:staticcheck
			}

			ctx := context.Background()
			if tt.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			var buf bytes.Buffer
			err := pod.StreamLogs(ctx, client, "default", "test-pod", "", &buf)
			if (err != nil) != tt.wantErr {
				t.Errorf("StreamLogs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantOut && buf.Len() == 0 {
				t.Error("expected non-empty buffer from fake client")
			}
		})
	}
}

func TestGetPodLogs(t *testing.T) {
	tests := []struct {
		name    string
		pod     *corev1.Pod
		cancel  bool
		wantErr bool
		wantOut bool
	}{
		{
			name:    "cancelled context",
			pod:     nil,
			cancel:  true,
			wantErr: true,
		},
		{
			name: "success",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			},
			wantErr: false,
			wantOut: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var client *fake.Clientset //nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
			if tt.pod != nil {
				client = fake.NewSimpleClientset(tt.pod) //nolint:staticcheck
			} else {
				client = fake.NewSimpleClientset() //nolint:staticcheck
			}

			ctx := context.Background()
			if tt.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			result, err := pod.GetPodLogs(ctx, client, "default", "test-pod", "")
			if (err != nil) != tt.wantErr {
				t.Errorf("GetPodLogs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantOut && result == "" {
				t.Error("expected non-empty result from fake client")
			}
		})
	}
}
