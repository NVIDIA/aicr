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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
	corev1 "k8s.io/api/core/v1"
)

func TestFirstContainerImage(t *testing.T) {
	tests := []struct {
		name       string
		containers []corev1.Container
		expected   string
	}{
		{
			name:       "empty list",
			containers: nil,
			expected:   "unknown",
		},
		{
			name: "single container",
			containers: []corev1.Container{
				{Image: "nvidia/cuda:12.0"},
			},
			expected: "nvidia/cuda:12.0",
		},
		{
			name: "multiple containers returns first",
			containers: []corev1.Container{
				{Image: "nvidia/cuda:12.0"},
				{Image: "busybox:latest"},
			},
			expected: "nvidia/cuda:12.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := firstContainerImage(tt.containers)
			if got != tt.expected {
				t.Errorf("firstContainerImage() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestValueOrUnknown(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"empty string", "", "unknown"},
		{"whitespace only", "   ", "unknown"},
		{"tab and newline", "\t\n", "unknown"},
		{"non-empty string", "hello", "hello"},
		{"string with spaces", "  hello  ", "  hello  "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := valueOrUnknown(tt.input)
			if got != tt.expected {
				t.Errorf("valueOrUnknown(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestPodReadyCount(t *testing.T) {
	tests := []struct {
		name     string
		pod      corev1.Pod
		expected string
	}{
		{
			name:     "no containers",
			pod:      corev1.Pod{},
			expected: "0/0",
		},
		{
			name: "all ready",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{Ready: true},
						{Ready: true},
					},
				},
			},
			expected: "2/2",
		},
		{
			name: "partial ready",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{Ready: true},
						{Ready: false},
						{Ready: true},
					},
				},
			},
			expected: "2/3",
		},
		{
			name: "none ready",
			pod: corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{Ready: false},
					},
				},
			},
			expected: "0/1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := podReadyCount(tt.pod)
			if got != tt.expected {
				t.Errorf("podReadyCount() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestTruncateLines(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		n        int
		expected string
	}{
		{
			name:     "empty string",
			text:     "",
			n:        5,
			expected: "",
		},
		{
			name:     "fewer lines than limit",
			text:     "line1\nline2",
			n:        5,
			expected: "line1\nline2",
		},
		{
			name:     "exact limit",
			text:     "line1\nline2\nline3",
			n:        3,
			expected: "line1\nline2\nline3",
		},
		{
			name:     "more lines than limit",
			text:     "line1\nline2\nline3\nline4\nline5",
			n:        2,
			expected: "line1\nline2\n... [truncated]",
		},
		{
			name:     "single line within limit",
			text:     "only one line",
			n:        1,
			expected: "only one line",
		},
		{
			name:     "truncate to one line",
			text:     "line1\nline2\nline3",
			n:        1,
			expected: "line1\n... [truncated]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateLines(tt.text, tt.n)
			if got != tt.expected {
				t.Errorf("truncateLines(%q, %d) = %q, want %q", tt.text, tt.n, got, tt.expected)
			}
		})
	}
}

func TestContainsAllMetrics(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		required []string
		wantLen  int
	}{
		{
			name:     "all present",
			text:     "metric_a 1\nmetric_b 2\nmetric_c 3",
			required: []string{"metric_a", "metric_b", "metric_c"},
			wantLen:  0,
		},
		{
			name:     "some missing",
			text:     "metric_a 1\nmetric_c 3",
			required: []string{"metric_a", "metric_b", "metric_c"},
			wantLen:  1,
		},
		{
			name:     "empty text",
			text:     "",
			required: []string{"metric_a"},
			wantLen:  1,
		},
		{
			name:     "empty required list",
			text:     "anything here",
			required: nil,
			wantLen:  0,
		},
		{
			name:     "all missing",
			text:     "unrelated output",
			required: []string{"metric_a", "metric_b"},
			wantLen:  2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			missing := containsAllMetrics(tt.text, tt.required)
			if len(missing) != tt.wantLen {
				t.Errorf("containsAllMetrics() returned %d missing, want %d; missing=%v",
					len(missing), tt.wantLen, missing)
			}
		})
	}
}

func TestRecordArtifact(t *testing.T) {
	t.Parallel()
	// Verify recordArtifact and recordRawTextArtifact do not panic.
	recordArtifact(nil, "test-label", "test-data")
	recordRawTextArtifact(nil, "test-label", "kubectl get pods", "pod output")
	recordRawTextArtifact(nil, "test-label", "", "no equivalent")
}

func TestPodStuckReason(t *testing.T) {
	tests := []struct {
		name      string
		pod       *corev1.Pod
		wantEmpty bool
		wantSub   string
	}{
		{
			name:      "healthy pod",
			pod:       &corev1.Pod{},
			wantEmpty: true,
		},
		{
			name: "ImagePullBackOff",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Image: "bad-image:latest",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason:  "ImagePullBackOff",
									Message: "back-off pulling",
								},
							},
						},
					},
				},
			},
			wantEmpty: false,
			wantSub:   "ImagePullBackOff",
		},
		{
			name: "init container CrashLoopBackOff",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					InitContainerStatuses: []corev1.ContainerStatus{
						{
							Image: "init:latest",
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason:  "CrashLoopBackOff",
									Message: "crash loop",
								},
							},
						},
					},
				},
			},
			wantEmpty: false,
			wantSub:   "init container",
		},
		{
			name: "unschedulable",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:    corev1.PodScheduled,
							Status:  corev1.ConditionFalse,
							Reason:  string(corev1.PodReasonUnschedulable),
							Message: "insufficient gpu",
						},
					},
				},
			},
			wantEmpty: false,
			wantSub:   "Unschedulable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := podStuckReason(tt.pod)
			if tt.wantEmpty && got != "" {
				t.Errorf("podStuckReason() = %q, want empty", got)
			}
			if !tt.wantEmpty && !strings.Contains(got, tt.wantSub) {
				t.Errorf("podStuckReason() = %q, want substring %q", got, tt.wantSub)
			}
		})
	}
}

func TestPodWaitingStatus(t *testing.T) {
	tests := []struct {
		name     string
		pod      *corev1.Pod
		expected string
	}{
		{
			name:     "no waiting containers",
			pod:      &corev1.Pod{},
			expected: "none",
		},
		{
			name: "waiting container",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason:  "ContainerCreating",
									Message: "pulling image",
								},
							},
						},
					},
				},
			},
			expected: "ContainerCreating: pulling image",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := podWaitingStatus(tt.pod)
			if got != tt.expected {
				t.Errorf("podWaitingStatus() = %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestNewGangTestRun(t *testing.T) {
	run, err := newGangTestRun()
	if err != nil {
		t.Fatalf("newGangTestRun() error = %v", err)
	}

	if run.suffix == "" {
		t.Error("newGangTestRun() suffix is empty")
	}
	if len(run.suffix) != 8 {
		t.Errorf("newGangTestRun() suffix length = %d, want 8 hex chars", len(run.suffix))
	}
	if run.groupName == "" {
		t.Error("newGangTestRun() groupName is empty")
	}
	if !strings.HasPrefix(run.groupName, gangGroupPrefix) {
		t.Errorf("newGangTestRun() groupName = %q, want prefix %q", run.groupName, gangGroupPrefix)
	}
	for i := range gangMinMembers {
		if run.pods[i] == "" {
			t.Errorf("newGangTestRun() pods[%d] is empty", i)
		}
		if !strings.HasPrefix(run.pods[i], gangPodPrefix) {
			t.Errorf("newGangTestRun() pods[%d] = %q, want prefix %q", i, run.pods[i], gangPodPrefix)
		}
	}

	// Two calls should produce different suffixes.
	run2, err := newGangTestRun()
	if err != nil {
		t.Fatalf("newGangTestRun() second call error = %v", err)
	}
	if run.suffix == run2.suffix {
		t.Error("newGangTestRun() two calls produced identical suffixes")
	}
}

func TestBuildGangTestPodUsesCPUOnlyWorkload(t *testing.T) {
	run, err := newGangTestRun()
	if err != nil {
		t.Fatalf("newGangTestRun() error = %v", err)
	}

	pod := buildGangTestPod(run, 0, nil)
	if pod.Spec.SchedulerName != "kai-scheduler" {
		t.Errorf("SchedulerName = %q, want kai-scheduler", pod.Spec.SchedulerName)
	}
	if got := pod.Labels["pod-group.scheduling.run.ai/name"]; got != run.groupName {
		t.Errorf("pod group label = %q, want %q", got, run.groupName)
	}
	if len(pod.Spec.ResourceClaims) != 0 {
		t.Fatalf("ResourceClaims length = %d, want 0", len(pod.Spec.ResourceClaims))
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("containers length = %d, want 1", len(pod.Spec.Containers))
	}
	container := pod.Spec.Containers[0]
	if container.Image != defaults.ProbeImage {
		t.Errorf("container image = %q, want %q", container.Image, defaults.ProbeImage)
	}
	if len(container.Resources.Claims) != 0 {
		t.Errorf("container resource claims length = %d, want 0", len(container.Resources.Claims))
	}
}
