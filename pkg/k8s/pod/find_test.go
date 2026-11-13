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
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestFindContainerStatus(t *testing.T) {
	p := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
		{Name: "sidecar", ImageID: "sidecar-id"},
		{Name: "validator", ImageID: "validator-id"},
	}}}

	tests := []struct {
		name      string
		pod       *corev1.Pod
		container string
		wantID    string
		wantOK    bool
	}{
		{"by name, not position", p, "validator", "validator-id", true},
		{"no such container", p, "missing", "", false},
		{"nil pod", nil, "validator", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs, ok := pod.FindContainerStatus(tt.pod, tt.container)
			if ok != tt.wantOK || cs.ImageID != tt.wantID {
				t.Errorf("FindContainerStatus() = (%q, %v), want (%q, %v)", cs.ImageID, ok, tt.wantID, tt.wantOK)
			}
		})
	}
}

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

// TestGetPodForJob_TieredSelection exercises the active-vs-failed and
// terminating-orphan filters: a Running pod outranks a Failed orphan, the
// youngest active pod wins on ties, and a Failed pod is returned only when
// no non-Failed candidate remains so ExtractResult can still read its
// termination state.
func TestGetPodForJob_TieredSelection(t *testing.T) {
	const ns = "default"
	const jobName = "validator-job"
	label := map[string]string{"batch.kubernetes.io/job-name": jobName}
	old := metav1.NewTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	mid := metav1.NewTime(time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC))
	now := metav1.NewTime(time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC))
	deleting := metav1.NewTime(time.Date(2026, 1, 1, 0, 9, 0, 0, time.UTC))

	mkPod := func(name string, ts metav1.Time, phase corev1.PodPhase, deletion *metav1.Time) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: ns, Labels: label,
				CreationTimestamp: ts,
				DeletionTimestamp: deletion,
			},
			Status: corev1.PodStatus{Phase: phase},
		}
	}

	tests := []struct {
		name     string
		objects  []runtime.Object
		wantName string
		wantErr  bool
	}{
		{
			name: "running pod beats older failed pod",
			objects: []runtime.Object{
				mkPod("old-failed", old, corev1.PodFailed, nil),
				mkPod("live", now, corev1.PodRunning, nil),
			},
			wantName: "live",
		},
		{
			name: "youngest active wins on multiple non-failed pods",
			objects: []runtime.Object{
				mkPod("older-pending", mid, corev1.PodPending, nil),
				mkPod("newer-running", now, corev1.PodRunning, nil),
			},
			wantName: "newer-running",
		},
		{
			name: "only failed pod is returned for ExtractResult to inspect",
			objects: []runtime.Object{
				mkPod("failed", now, corev1.PodFailed, nil),
			},
			wantName: "failed",
		},
		{
			name: "youngest failed wins when all candidates failed",
			objects: []runtime.Object{
				mkPod("old-failed", old, corev1.PodFailed, nil),
				mkPod("newer-failed", now, corev1.PodFailed, nil),
			},
			wantName: "newer-failed",
		},
		{
			name: "terminating pod is skipped",
			objects: []runtime.Object{
				mkPod("terminating", now, corev1.PodRunning, &deleting),
				mkPod("live", mid, corev1.PodRunning, nil),
			},
			wantName: "live",
		},
		{
			name: "all candidates terminating returns NotFound",
			objects: []runtime.Object{
				mkPod("terminating-1", now, corev1.PodRunning, &deleting),
				mkPod("terminating-2", mid, corev1.PodFailed, &deleting),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
			client := fake.NewSimpleClientset(tt.objects...)
			got, err := pod.GetPodForJob(context.Background(), client, ns, jobName)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got pod %+v", got)
				}
				var se *errors.StructuredError
				if !stderrors.As(err, &se) {
					t.Fatalf("expected StructuredError, got %T", err)
				}
				if se.Code != errors.ErrCodeNotFound {
					t.Errorf("code = %q, want %q", se.Code, errors.ErrCodeNotFound)
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

func TestGetTerminatedPodForJob(t *testing.T) {
	const ns, jobName, container = "default", "validator-job", "validator"
	label := map[string]string{"batch.kubernetes.io/job-name": jobName}
	now := metav1.Now()
	mk := func(name string, created time.Time, deleting bool, state corev1.ContainerState) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: label, CreationTimestamp: metav1.NewTime(created)},
			Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: container, State: state}}},
		}
		if deleting {
			p.DeletionTimestamp = &now
			p.Finalizers = []string{"test/hold"}
		}
		return p
	}
	done := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}}
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	base := time.Now()

	tests := []struct {
		name     string
		objects  []runtime.Object
		listErr  bool
		wantName string
		wantCode errors.ErrorCode
	}{
		{"terminating pod with terminated container", []runtime.Object{mk("a", base, true, done)}, false, "a", ""},
		{"youngest terminated pod wins", []runtime.Object{mk("old", base, false, done), mk("new", base.Add(time.Minute), true, done)}, false, "new", ""},
		{"terminating pod still running", []runtime.Object{mk("a", base, true, running)}, false, "", errors.ErrCodeNotFound},
		{"no pods", nil, false, "", errors.ErrCodeNotFound},
		{"list error", nil, true, "", errors.ErrCodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset(tt.objects...)
			if tt.listErr {
				cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, stderrors.New("boom")
				})
			}
			got, err := pod.GetTerminatedPodForJob(t.Context(), cs, ns, jobName, container)
			if tt.wantCode != "" {
				if !stderrors.Is(err, errors.New(tt.wantCode, "")) {
					t.Fatalf("err = %v, want code %s", err, tt.wantCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Name != tt.wantName {
				t.Errorf("pod = %s, want %s", got.Name, tt.wantName)
			}
		})
	}
}
