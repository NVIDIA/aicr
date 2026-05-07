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

package job

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// createPodForJob creates a pod that matches the Job's label selector.
func createPodForJob(t *testing.T, ns, jobName string, status corev1.PodStatus) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: jobName + "-",
			Namespace:    ns,
			Labels: map[string]string{
				"batch.kubernetes.io/job-name": jobName,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "validator",
				Image: "busybox",
			}},
		},
		Status: status,
	}
	created, err := testClientset.CoreV1().Pods(ns).Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("failed to create test pod: %v", err)
	}
	// Status must be set via UpdateStatus — the create call ignores .Status.
	created.Status = status
	_, err = testClientset.CoreV1().Pods(ns).UpdateStatus(context.Background(), created, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("failed to update pod status: %v", err)
	}
}

// deployTestJob deploys a Job via envtest and returns the Deployer.
func deployTestJob(t *testing.T, ns string, entry catalog.ValidatorEntry) *Deployer {
	t.Helper()
	d := NewDeployer(testClientset, testFactory(t, ns), ns, "run1", "", "", entry, nil, nil, nil)
	if err := d.DeployJob(context.Background()); err != nil {
		t.Fatalf("DeployJob() failed: %v", err)
	}
	return d
}

func TestExtractResultTerminatedPass(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())

	now := metav1.Now()
	start := metav1.NewTime(now.Add(-15 * time.Second))
	createPodForJob(t, ns, d.JobName(), corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode:   0,
					Message:    "all checks passed",
					StartedAt:  start,
					FinishedAt: now,
				},
			},
		}},
	})

	result := d.ExtractResult(context.Background())

	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.TerminationMsg != "all checks passed" {
		t.Errorf("TerminationMsg = %q, want %q", result.TerminationMsg, "all checks passed")
	}
	if result.CTRFStatus() != ctrf.StatusPassed {
		t.Errorf("CTRFStatus = %q, want %q", result.CTRFStatus(), ctrf.StatusPassed)
	}
	if result.Duration < 14*time.Second || result.Duration > 16*time.Second {
		t.Errorf("Duration = %v, want ~15s", result.Duration)
	}
}

func TestExtractResultTerminatedFail(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())

	createPodForJob(t, ns, d.JobName(), corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1,
					Message:  "DaemonSet check failed",
				},
			},
		}},
	})

	result := d.ExtractResult(context.Background())

	if result.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", result.ExitCode)
	}
	if result.CTRFStatus() != ctrf.StatusFailed {
		t.Errorf("CTRFStatus = %q, want %q", result.CTRFStatus(), ctrf.StatusFailed)
	}
	if result.TerminationMsg != "DaemonSet check failed" {
		t.Errorf("TerminationMsg = %q", result.TerminationMsg)
	}
}

func TestExtractResultTerminatedSkip(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())

	createPodForJob(t, ns, d.JobName(), corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 2,
				},
			},
		}},
	})

	result := d.ExtractResult(context.Background())

	if result.ExitCode != 2 {
		t.Errorf("ExitCode = %d, want 2", result.ExitCode)
	}
	if result.CTRFStatus() != ctrf.StatusSkipped {
		t.Errorf("CTRFStatus = %q, want %q", result.CTRFStatus(), ctrf.StatusSkipped)
	}
}

func TestExtractResultOOMKilled(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())

	createPodForJob(t, ns, d.JobName(), corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 137,
					Reason:   "OOMKilled",
				},
			},
		}},
	})

	result := d.ExtractResult(context.Background())

	if result.ExitCode != 137 {
		t.Errorf("ExitCode = %d, want 137", result.ExitCode)
	}
	if result.CTRFStatus() != ctrf.StatusOther {
		t.Errorf("CTRFStatus = %q, want %q", result.CTRFStatus(), ctrf.StatusOther)
	}
	if result.TerminationMsg != "Container OOMKilled" {
		t.Errorf("TerminationMsg = %q, want %q", result.TerminationMsg, "Container OOMKilled")
	}
}

func TestExtractResultWaiting(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())

	createPodForJob(t, ns, d.JobName(), corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{
				Waiting: &corev1.ContainerStateWaiting{
					Reason:  "ImagePullBackOff",
					Message: "Back-off pulling image",
				},
			},
		}},
	})

	result := d.ExtractResult(context.Background())

	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", result.ExitCode)
	}
	if result.CTRFStatus() != ctrf.StatusOther {
		t.Errorf("CTRFStatus = %q, want %q", result.CTRFStatus(), ctrf.StatusOther)
	}
	if result.TerminationMsg != "ImagePullBackOff: Back-off pulling image" {
		t.Errorf("TerminationMsg = %q", result.TerminationMsg)
	}
}

func TestExtractResultRunning(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())

	createPodForJob(t, ns, d.JobName(), corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{},
			},
		}},
	})

	result := d.ExtractResult(context.Background())

	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", result.ExitCode)
	}
	if result.TerminationMsg != "container still running after wait completed" {
		t.Errorf("TerminationMsg = %q", result.TerminationMsg)
	}
}

func TestExtractResultNoContainerStatus(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())

	createPodForJob(t, ns, d.JobName(), corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{},
	})

	result := d.ExtractResult(context.Background())

	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", result.ExitCode)
	}
	if result.TerminationMsg != "no container status available" {
		t.Errorf("TerminationMsg = %q", result.TerminationMsg)
	}
}

func TestExtractResultPodNotFound(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())
	// No pod created — simulates external deletion

	result := d.ExtractResult(context.Background())

	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", result.ExitCode)
	}
	if result.CTRFStatus() != ctrf.StatusOther {
		t.Errorf("CTRFStatus = %q, want %q", result.CTRFStatus(), ctrf.StatusOther)
	}
	if result.TerminationMsg == "" {
		t.Error("TerminationMsg should contain pod not found message")
	}
}

func TestExtractResultPreservesNameAndPhase(t *testing.T) {
	ns := createUniqueNamespace(t)
	entry := testEntry()
	d := deployTestJob(t, ns, entry)

	createPodForJob(t, ns, d.JobName(), corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
			},
		}},
	})

	result := d.ExtractResult(context.Background())

	if result.Name != entry.Name {
		t.Errorf("Name = %q, want %q", result.Name, entry.Name)
	}
	if result.Phase != entry.Phase {
		t.Errorf("Phase = %q, want %q", result.Phase, entry.Phase)
	}
}

func TestHandleTimeoutPodNotFound(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())

	result := d.HandleTimeout(context.Background())

	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", result.ExitCode)
	}
	if result.TerminationMsg != "pod never reached running state" {
		t.Errorf("TerminationMsg = %q", result.TerminationMsg)
	}
}

func TestHandleTimeoutContainerNotTerminated(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())

	createPodForJob(t, ns, d.JobName(), corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{},
			},
		}},
	})

	result := d.HandleTimeout(context.Background())

	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", result.ExitCode)
	}
	if result.TerminationMsg == "" {
		t.Error("TerminationMsg should contain timeout message")
	}
}

func TestHandleTimeoutContainerTerminated(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())

	now := metav1.Now()
	start := metav1.NewTime(now.Add(-120 * time.Second))
	createPodForJob(t, ns, d.JobName(), corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode:   137,
					Message:    "killed by deadline",
					StartedAt:  start,
					FinishedAt: now,
				},
			},
		}},
	})

	result := d.HandleTimeout(context.Background())

	if result.ExitCode != 137 {
		t.Errorf("ExitCode = %d, want 137", result.ExitCode)
	}
	if result.TerminationMsg != "killed by deadline" {
		t.Errorf("TerminationMsg = %q", result.TerminationMsg)
	}
}

func TestFilterStdoutLines(t *testing.T) {
	tests := []struct {
		name       string
		lines      []string
		maxLineLen int
		want       []string
	}{
		{
			name:       "empty input",
			lines:      []string{},
			maxLineLen: 100,
			want:       []string{},
		},
		{
			name:       "nil input",
			lines:      nil,
			maxLineLen: 100,
			want:       nil,
		},
		{
			name: "lines below max length pass through",
			lines: []string{
				`time=2026-03-10T10:00:00Z level=INFO msg="check started"`,
				`time=2026-03-10T10:00:01Z level=INFO msg="check completed"`,
			},
			maxLineLen: 512,
			want: []string{
				`time=2026-03-10T10:00:00Z level=INFO msg="check started"`,
				`time=2026-03-10T10:00:01Z level=INFO msg="check completed"`,
			},
		},
		{
			name: "long line gets truncated with suffix",
			lines: []string{
				"short line",
				strings.Repeat("x", 600),
			},
			maxLineLen: 100,
			want: []string{
				"short line",
				strings.Repeat("x", 100) + "... [truncated 500 chars]",
			},
		},
		{
			name: "line exactly at max length not truncated",
			lines: []string{
				strings.Repeat("a", 100),
			},
			maxLineLen: 100,
			want: []string{
				strings.Repeat("a", 100),
			},
		},
		{
			name: "line one over max length truncated",
			lines: []string{
				strings.Repeat("b", 101),
			},
			maxLineLen: 100,
			want: []string{
				strings.Repeat("b", 100) + "... [truncated 1 chars]",
			},
		},
		{
			name: "multiple long lines all truncated",
			lines: []string{
				strings.Repeat("a", 200),
				"ok",
				strings.Repeat("b", 300),
			},
			maxLineLen: 50,
			want: []string{
				strings.Repeat("a", 50) + "... [truncated 150 chars]",
				"ok",
				strings.Repeat("b", 50) + "... [truncated 250 chars]",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterStdoutLines(tt.lines, tt.maxLineLen)

			if len(got) != len(tt.want) {
				t.Fatalf("filterStdoutLines() returned %d lines, want %d\ngot:  %v\nwant: %v",
					len(got), len(tt.want), got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("line[%d]:\n  got:  %q\n  want: %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
