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
	stderrors "errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
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
				Name:  ValidatorContainerName,
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
	d := NewDeployer(Config{Clientset: testClientset, Factory: testFactory(t, ns), Namespace: ns, RunID: "run1", Entry: entry})
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
			Name: ValidatorContainerName,
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
			Name: ValidatorContainerName,
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
			Name: ValidatorContainerName,
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
			Name: ValidatorContainerName,
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
			Name: ValidatorContainerName,
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
			Name: ValidatorContainerName,
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

func TestExtractResultValidatorContainerNotFound(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())

	createPodForJob(t, ns, d.JobName(), corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{},
	})

	result := d.ExtractResult(context.Background())

	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", result.ExitCode)
	}
	if !strings.Contains(result.TerminationMsg, ValidatorContainerName) {
		t.Errorf("TerminationMsg = %q, want message containing %q", result.TerminationMsg, ValidatorContainerName)
	}
	if !strings.Contains(result.TerminationMsg, "not found") {
		t.Errorf("TerminationMsg = %q, want message containing 'not found'", result.TerminationMsg)
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
			Name: ValidatorContainerName,
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

	result := d.HandleTimeout(context.Background(), context.DeadlineExceeded)

	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", result.ExitCode)
	}
	if result.TerminationMsg != "pod never reached running state" {
		t.Errorf("TerminationMsg = %q", result.TerminationMsg)
	}
}

// TestHandleTimeoutContainerNotTerminated verifies that a validator whose
// container never reached a terminal state renders a message reflecting the
// ACTUAL wait cause: a genuine context-deadline expiry reports the configured
// timeout, while an infra/unavailable cause reports that failure verbatim and
// must NOT masquerade as the catalog timeout (issue #1966).
func TestHandleTimeoutContainerNotTerminated(t *testing.T) {
	tests := []struct {
		name          string
		cause         error
		wantContains  []string // substrings that MUST all appear
		wantExclusion string   // substring that must NOT appear
	}{
		{
			name:         "genuine deadline (stdlib sentinel)",
			cause:        context.DeadlineExceeded,
			wantContains: []string{"timeout: validator did not complete within"},
		},
		{
			name:         "genuine deadline (ErrCodeTimeout)",
			cause:        errors.New(errors.ErrCodeTimeout, "wait deadline exceeded"),
			wantContains: []string{"timeout: validator did not complete within"},
		},
		{
			name:         "nil cause falls back to timeout wording",
			cause:        nil,
			wantContains: []string{"timeout: validator did not complete within"},
		},
		{
			name:          "infra/unavailable cause is surfaced verbatim",
			cause:         errors.New(errors.ErrCodeUnavailable, "apiserver watch closed: connection refused"),
			wantContains:  []string{"validation failed:", "apiserver watch closed: connection refused"},
			wantExclusion: "timeout: validator did not complete within",
		},
		{
			// Production shape: WaitForJobTerminal wraps the context error under
			// ErrCodeTimeout — isDeadlineCause must see through the wrap chain.
			name:         "wrapped ErrCodeTimeout (production wait shape)",
			cause:        errors.Wrap(errors.ErrCodeTimeout, "job terminal wait timeout", context.DeadlineExceeded),
			wantContains: []string{"timeout: validator did not complete within"},
		},
		{
			// Production shape: a transient re-check Get failure classified as
			// ErrCodeUnavailable and wrapped by classifyReGetError — must render
			// verbatim, not as the catalog timeout.
			name:          "wrapped ErrCodeUnavailable (production resume shape)",
			cause:         errors.Wrap(errors.ErrCodeUnavailable, "job watch closed and Job re-check failed", stderrors.New("connection refused")),
			wantContains:  []string{"validation failed:", "connection refused"},
			wantExclusion: "timeout: validator did not complete within",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := createUniqueNamespace(t)
			d := deployTestJob(t, ns, testEntry())

			createPodForJob(t, ns, d.JobName(), corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: ValidatorContainerName,
					State: corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{},
					},
				}},
			})

			result := d.HandleTimeout(context.Background(), tt.cause)

			if result.ExitCode != -1 {
				t.Errorf("ExitCode = %d, want -1", result.ExitCode)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(result.TerminationMsg, want) {
					t.Errorf("TerminationMsg = %q, want substring %q", result.TerminationMsg, want)
				}
			}
			if tt.wantExclusion != "" && strings.Contains(result.TerminationMsg, tt.wantExclusion) {
				t.Errorf("TerminationMsg = %q, must NOT contain %q", result.TerminationMsg, tt.wantExclusion)
			}
		})
	}
}

// TestHandleTimeoutZeroTimeoutRendersDefault verifies that a catalog entry with
// no explicit timeout renders the effective default runPhase applies
// (ValidatorDefaultTimeout), not a misleading "within 0s".
func TestHandleTimeoutZeroTimeoutRendersDefault(t *testing.T) {
	entry := testEntry()
	entry.Timeout = 0

	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, entry)
	createPodForJob(t, ns, d.JobName(), corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  ValidatorContainerName,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	})

	result := d.HandleTimeout(context.Background(), context.DeadlineExceeded)

	wantTimeout := defaults.ValidatorDefaultTimeout.String()
	if !strings.Contains(result.TerminationMsg, wantTimeout) {
		t.Errorf("TerminationMsg = %q, want effective timeout %q", result.TerminationMsg, wantTimeout)
	}
	if strings.Contains(result.TerminationMsg, "within 0s") {
		t.Errorf("TerminationMsg = %q, must not render a bare 0s timeout", result.TerminationMsg)
	}
}

func TestHandleTimeoutContainerTerminated(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())

	now := metav1.Now()
	start := metav1.NewTime(now.Add(-120 * time.Second))
	createPodForJob(t, ns, d.JobName(), corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: ValidatorContainerName,
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

	result := d.HandleTimeout(context.Background(), context.DeadlineExceeded)

	if result.ExitCode != 137 {
		t.Errorf("ExitCode = %d, want 137", result.ExitCode)
	}
	if result.TerminationMsg != "killed by deadline" {
		t.Errorf("TerminationMsg = %q", result.TerminationMsg)
	}
}

func TestExtractResultWithSidecar(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())

	now := metav1.Now()
	start := metav1.NewTime(now.Add(-10 * time.Second))
	createPodForJob(t, ns, d.JobName(), corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{
			{
				Name: "log-sidecar",
				State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{},
				},
			},
			{
				Name: ValidatorContainerName,
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode:   0,
						Message:    "validation passed",
						StartedAt:  start,
						FinishedAt: now,
					},
				},
			},
		},
	})

	result := d.ExtractResult(context.Background())

	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if result.TerminationMsg != "validation passed" {
		t.Errorf("TerminationMsg = %q, want %q", result.TerminationMsg, "validation passed")
	}
	if result.CTRFStatus() != ctrf.StatusPassed {
		t.Errorf("CTRFStatus = %q, want %q", result.CTRFStatus(), ctrf.StatusPassed)
	}
}

func TestExtractResultSidecarOnlyNoValidator(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())

	createPodForJob(t, ns, d.JobName(), corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{
			{
				Name: "log-sidecar",
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 0,
						Message:  "sidecar terminated",
					},
				},
			},
		},
	})

	result := d.ExtractResult(context.Background())

	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", result.ExitCode)
	}
	if !strings.Contains(result.TerminationMsg, ValidatorContainerName) {
		t.Errorf("TerminationMsg = %q, want message containing %q", result.TerminationMsg, ValidatorContainerName)
	}
	if !strings.Contains(result.TerminationMsg, "not found") {
		t.Errorf("TerminationMsg = %q, want message containing 'not found'", result.TerminationMsg)
	}
	if result.CTRFStatus() != ctrf.StatusOther {
		t.Errorf("CTRFStatus = %q, want %q", result.CTRFStatus(), ctrf.StatusOther)
	}
}

func TestHandleTimeoutWithSidecar(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())

	now := metav1.Now()
	start := metav1.NewTime(now.Add(-120 * time.Second))
	createPodForJob(t, ns, d.JobName(), corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{
			{
				Name: "log-sidecar",
				State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{},
				},
			},
			{
				Name: ValidatorContainerName,
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode:   137,
						Message:    "killed by deadline",
						StartedAt:  start,
						FinishedAt: now,
					},
				},
			},
		},
	})

	result := d.HandleTimeout(context.Background(), context.DeadlineExceeded)

	if result.ExitCode != 137 {
		t.Errorf("ExitCode = %d, want 137", result.ExitCode)
	}
	if result.TerminationMsg != "killed by deadline" {
		t.Errorf("TerminationMsg = %q", result.TerminationMsg)
	}
}

func TestHandleTimeoutSidecarOnlyNoValidator(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())

	createPodForJob(t, ns, d.JobName(), corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{
			{
				Name: "log-sidecar",
				State: corev1.ContainerState{
					Running: &corev1.ContainerStateRunning{},
				},
			},
		},
	})

	result := d.HandleTimeout(context.Background(), context.DeadlineExceeded)

	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", result.ExitCode)
	}
	if !strings.Contains(result.TerminationMsg, ValidatorContainerName) {
		t.Errorf("TerminationMsg = %q, want message containing %q", result.TerminationMsg, ValidatorContainerName)
	}
	if !strings.Contains(result.TerminationMsg, "not found") {
		t.Errorf("TerminationMsg = %q, want message containing 'not found'", result.TerminationMsg)
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

func TestBoundTerminationMsg(t *testing.T) {
	tests := []struct {
		name         string
		msg          string
		maxBytes     int
		wantLen      int    // expected exact length, 0 = compute from msg
		wantContains string // substring that must appear (empty = none)
	}{
		{
			name:     "under limit passes through unchanged",
			msg:      "container exited with code 1",
			maxBytes: 4096,
		},
		{
			name:     "exactly at limit passes through unchanged",
			msg:      strings.Repeat("x", 100),
			maxBytes: 100,
		},
		{
			name:         "over limit is truncated with byte-count suffix",
			msg:          strings.Repeat("y", 5000),
			maxBytes:     4096,
			wantContains: "... [truncated 904 bytes]",
		},
		{
			// Cut lands mid-rune: "€" is 3 bytes, so a maxBytes that splits it
			// must trim back to the rune boundary and never emit an invalid rune.
			name:         "cut mid multibyte rune trims to boundary",
			msg:          strings.Repeat("€", 10), // 30 bytes
			maxBytes:     10,                      // splits the 4th rune (bytes 9-11)
			wantContains: "... [truncated",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := boundTerminationMsg(tt.msg, tt.maxBytes)
			if len(tt.msg) <= tt.maxBytes {
				if got != tt.msg {
					t.Errorf("expected passthrough, got %q", got)
				}
				return
			}
			if !utf8.ValidString(got) {
				t.Errorf("truncated output is not valid UTF-8: %q", got)
			}
			if tt.wantContains != "" && !strings.Contains(got, tt.wantContains) {
				t.Errorf("output missing suffix %q, got %q", tt.wantContains, got)
			}
		})
	}
}
