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
	stderrors "errors"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestGetPodForJob(t *testing.T) {
	const ns = "default"
	const jobName = "validator-job"

	matchPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "validator-job-abcde",
			Namespace: ns,
			Labels:    map[string]string{"batch.kubernetes.io/job-name": jobName},
		},
	}
	otherPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-pod",
			Namespace: ns,
			Labels:    map[string]string{"batch.kubernetes.io/job-name": "other-job"},
		},
	}

	tests := []struct {
		name      string
		objects   []runtime.Object
		injectErr bool
		wantName  string
		wantErr   bool
		wantCode  errors.ErrorCode
	}{
		{
			name:     "pod found",
			objects:  []runtime.Object{matchPod, otherPod},
			wantName: matchPod.Name,
		},
		{
			name:     "pod not found returns NotFound",
			objects:  []runtime.Object{otherPod},
			wantErr:  true,
			wantCode: errors.ErrCodeNotFound,
		},
		{
			name:      "list error returns Internal",
			objects:   nil,
			injectErr: true,
			wantErr:   true,
			wantCode:  errors.ErrCodeInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
			client := fake.NewSimpleClientset(tt.objects...)
			if tt.injectErr {
				client.PrependReactor("list", "pods", func(_ k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, stderrors.New("simulated list failure")
				})
			}

			got, err := pod.GetPodForJob(context.Background(), client, ns, jobName)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got pod %+v", got)
				}
				var se *errors.StructuredError
				if !stderrors.As(err, &se) {
					t.Fatalf("error is not a StructuredError: %v", err)
				}
				if se.Code != tt.wantCode {
					t.Errorf("error code = %q, want %q", se.Code, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got == nil || got.Name != tt.wantName {
				t.Errorf("got pod %v, want name %q", got, tt.wantName)
			}
		})
	}
}
