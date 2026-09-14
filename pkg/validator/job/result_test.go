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
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
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
	if !strings.Contains(result.TerminationMsg, "pod not found for Job") {
		t.Errorf("TerminationMsg = %q, want the original pod-not-found wording when the Job carries no Failed condition", result.TerminationMsg)
	}
}

// setJobFailedCondition stamps a terminal Failed condition on the deployed Job,
// standing in for the Job controller (envtest runs no controller-manager).
func setJobFailedCondition(t *testing.T, ns, jobName, reason, message string) {
	t.Helper()
	job, err := testClientset.BatchV1().Jobs(ns).Get(context.Background(), jobName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get Job %s: %v", jobName, err)
	}
	// The apiserver rejects Failed=True without a preceding FailureTarget=True
	// and without startTime, mirroring the real controller's two-step
	// termination.
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.Conditions = append(job.Status.Conditions,
		batchv1.JobCondition{
			Type:    batchv1.JobFailureTarget,
			Status:  corev1.ConditionTrue,
			Reason:  reason,
			Message: message,
		},
		batchv1.JobCondition{
			Type:    batchv1.JobFailed,
			Status:  corev1.ConditionTrue,
			Reason:  reason,
			Message: message,
		},
	)
	if _, err := testClientset.BatchV1().Jobs(ns).UpdateStatus(context.Background(), job, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("failed to update Job status: %v", err)
	}
}

// TestExtractResultPodNotFoundDeadlineExceeded covers issue #2186: a validator
// Job killed on activeDeadlineSeconds has its pod deleted by the Job
// controller, so the pod lookup finds nothing. Every case must be reported as a
// failed check naming the deadline, not an inconclusive "pod not found".
//
// Each subtest creates its own namespace and Job rather than sharing a fixture:
// the catalog timeout is a per-case input baked into the deployed Job, and the
// condition is stamped on that specific Job's status.
func TestExtractResultPodNotFoundDeadlineExceeded(t *testing.T) {
	tests := []struct {
		name            string
		timeout         time.Duration // 0 exercises the runPhase default fallback
		condMessage     string
		wantContains    []string
		wantNotContains []string
	}{
		{
			name:        "names the deadline, reason and controller message",
			timeout:     2 * time.Minute,
			condMessage: "Job was active longer than specified deadline",
			wantContains: []string{
				"Job condition Failed/DeadlineExceeded: Job was active longer than specified deadline",
				// Pins the neutral phrasing: ErrCodeNotFound does not establish
				// WHY the pod is gone, so the message must not name a cause.
				"no pod remains for it",
			},
			wantNotContains: []string{"pod not found for Job", "the Job controller deleted the pod"},
		},
		{
			name:         "unpinned catalog timeout reports the default, not 0s",
			timeout:      0,
			condMessage:  "Job was active longer than specified deadline",
			wantContains: []string{"Job condition Failed/DeadlineExceeded: Job was active longer than specified deadline"},
		},
		{
			// The reason substitutes for an empty message, so the rendered
			// detail repeats it. Asserting the full "reason: reason" phrase is
			// what makes this case meaningful — a bare "DeadlineExceeded" is
			// already present in the Failed/<reason> prefix, so the fallback
			// could be deleted without failing the test.
			name:         "empty controller message falls back to the reason",
			timeout:      2 * time.Minute,
			condMessage:  "",
			wantContains: []string{"Job condition Failed/DeadlineExceeded: DeadlineExceeded)"},
		},
		{
			// Whitespace-only is what makes the TrimSpace load-bearing: a bare
			// `cond.Message == ""` check would render "Failed/DeadlineExceeded:
			// (   )" here instead of falling back to the reason.
			name:         "whitespace-only controller message falls back to the reason",
			timeout:      2 * time.Minute,
			condMessage:  "   ",
			wantContains: []string{"Job condition Failed/DeadlineExceeded: DeadlineExceeded)"},
		},
		{
			// BuildJobPlan truncates the catalog timeout with
			// int64(timeout.Seconds()), so the Job's activeDeadlineSeconds is
			// 90s here. Reporting the raw 1m30.5s would name a deadline
			// Kubernetes never enforced.
			name:            "sub-second precision reports the truncated deadline the Job enforced",
			timeout:         90500 * time.Millisecond,
			condMessage:     "Job was active longer than specified deadline",
			wantContains:    []string{"1m30s"},
			wantNotContains: []string{"1m30.5s"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := createUniqueNamespace(t)
			entry := testEntry()
			entry.Timeout = tt.timeout
			d := deployTestJob(t, ns, entry)
			// No pod created — the Job controller deleted it on deadline expiry.
			setJobFailedCondition(t, ns, d.JobName(), batchv1.JobReasonDeadlineExceeded, tt.condMessage)

			result := d.ExtractResult(context.Background())

			if result.ExitCode != 1 {
				t.Errorf("ExitCode = %d, want 1", result.ExitCode)
			}
			if result.CTRFStatus() != ctrf.StatusFailed {
				t.Errorf("CTRFStatus = %q, want %q", result.CTRFStatus(), ctrf.StatusFailed)
			}
			// Asserted for every case, including the unpinned one, which must
			// render the same default runPhase applies rather than a bare "0s".
			// The deadline the Job actually enforced: BuildJobPlan truncates
			// to whole seconds before setting activeDeadlineSeconds.
			want := tt.timeout
			if want == 0 {
				want = defaults.ValidatorDefaultTimeout
			}
			wantTimeout := (time.Duration(int64(want.Seconds())) * time.Second).String()
			if !strings.Contains(result.TerminationMsg, wantTimeout) {
				t.Errorf("TerminationMsg = %q, want it to name the deadline %s", result.TerminationMsg, wantTimeout)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(result.TerminationMsg, want) {
					t.Errorf("TerminationMsg = %q, want it to contain %q", result.TerminationMsg, want)
				}
			}
			for _, notWant := range tt.wantNotContains {
				if strings.Contains(result.TerminationMsg, notWant) {
					t.Errorf("TerminationMsg = %q, should not contain %q", result.TerminationMsg, notWant)
				}
			}
		})
	}
}

// TestExtractResultPodNotFoundJobFailedConditionFalse pins the
// Status == ConditionTrue guard in jobFailedCondition. A JobFailed condition
// stamped False is not a terminal failure, so it must not license a
// DeadlineExceeded verdict — the original inconclusive pod-not-found wording
// stands. Without the status check this presence-only match would flip an
// exit -1 / other into a product exit 1 / failed.
func TestExtractResultPodNotFoundJobFailedConditionFalse(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())
	// No pod created, and the Job carries a non-terminal Failed=False condition.
	job, err := testClientset.BatchV1().Jobs(ns).Get(context.Background(), d.JobName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get Job %s: %v", d.JobName(), err)
	}
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{
		Type:    batchv1.JobFailed,
		Status:  corev1.ConditionFalse,
		Reason:  batchv1.JobReasonDeadlineExceeded,
		Message: "Job was active longer than specified deadline",
	})
	if _, err := testClientset.BatchV1().Jobs(ns).UpdateStatus(context.Background(), job, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("failed to update Job status: %v", err)
	}

	result := d.ExtractResult(context.Background())

	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", result.ExitCode)
	}
	if result.CTRFStatus() != ctrf.StatusOther {
		t.Errorf("CTRFStatus = %q, want %q", result.CTRFStatus(), ctrf.StatusOther)
	}
	if !strings.Contains(result.TerminationMsg, "pod not found for Job") {
		t.Errorf("TerminationMsg = %q, want the original pod-not-found wording for a Failed=False condition", result.TerminationMsg)
	}
}

// TestExtractResultPodNotFoundJobAlreadyReaped proves diagnosis is never
// blocked on the extra Job Get succeeding: with the Job itself gone, the
// original pod-not-found wording and StatusOther are preserved.
func TestExtractResultPodNotFoundJobAlreadyReaped(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())
	d.jobName = "no-such-job"

	result := d.ExtractResult(context.Background())

	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", result.ExitCode)
	}
	if result.CTRFStatus() != ctrf.StatusOther {
		t.Errorf("CTRFStatus = %q, want %q", result.CTRFStatus(), ctrf.StatusOther)
	}
	if !strings.Contains(result.TerminationMsg, "pod not found for Job no-such-job") {
		t.Errorf("TerminationMsg = %q, want the original pod-not-found wording", result.TerminationMsg)
	}
}

// TestExtractResultPodNotFoundBoundsConditionText proves an oversized Job
// condition message cannot bypass ValidatorMaxTerminationMsgBytes on its way
// into the CTRF result and the ConfigMap that carries it — the pod-status
// branch already bounds its own message, and this path must not be the gap.
func TestExtractResultPodNotFoundBoundsConditionText(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())
	huge := strings.Repeat("x", defaults.ValidatorMaxTerminationMsgBytes*2)
	setJobFailedCondition(t, ns, d.JobName(), batchv1.JobReasonDeadlineExceeded, huge)

	result := d.ExtractResult(context.Background())

	if len(result.TerminationMsg) > defaults.ValidatorMaxTerminationMsgBytes+truncationSuffixSlack {
		t.Errorf("TerminationMsg is %d bytes, want it bounded near %d",
			len(result.TerminationMsg), defaults.ValidatorMaxTerminationMsgBytes)
	}
	if !strings.Contains(result.TerminationMsg, "truncated") {
		// Bounded preview: this branch runs precisely when the message is not
		// what was expected, so it must not assume a minimum length — an
		// unguarded slice would panic on a short message and hide the very
		// regression the assertion exists to report.
		preview := result.TerminationMsg
		if len(preview) > 80 {
			preview = preview[:80]
		}
		t.Errorf("TerminationMsg = %q..., want a truncation suffix", preview)
	}
}

// truncationSuffixSlack allows for the "... [truncated N bytes]" suffix
// boundTerminationMsg appends past the cap, matching how the pod-status branch
// behaves.
const truncationSuffixSlack = 64

// TestExtractResultPodListFailsStaysInconclusive is the control for the
// fail-closed lookup rule: when the pod List itself fails, absence was never
// established, so even a terminal Failed/DeadlineExceeded Job must NOT be
// promoted to a definitive verdict. An apiserver hiccup stays StatusOther.
func TestExtractResultPodListFailsStaysInconclusive(t *testing.T) {
	deadlineJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "default"},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{
				Type:    batchv1.JobFailed,
				Status:  corev1.ConditionTrue,
				Reason:  batchv1.JobReasonDeadlineExceeded,
				Message: "Job was active longer than specified deadline",
			}},
		},
	}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	client := fake.NewSimpleClientset(deadlineJob)
	client.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(stderrors.New("apiserver hiccup"))
	})

	d := NewDeployer(Config{Clientset: client, Namespace: "default", Entry: testEntry()})
	d.jobName = "j"

	result := d.ExtractResult(context.Background())

	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1 — a failed List establishes no absence", result.ExitCode)
	}
	if result.CTRFStatus() != ctrf.StatusOther {
		t.Errorf("CTRFStatus = %q, want %q", result.CTRFStatus(), ctrf.StatusOther)
	}
	// Anchored on the Job-condition diagnosis itself, not on any one phrase
	// inside it: the message wording is free to change, but a failed List must
	// never reach the deadline verdict at all.
	if strings.Contains(result.TerminationMsg, "Job condition Failed/") {
		t.Errorf("TerminationMsg = %q, must not render the deadline verdict for a pod whose absence was never confirmed", result.TerminationMsg)
	}
}

// TestExtractResultPodNotFoundOtherJobFailure proves a non-deadline Job
// failure surfaces its reason for diagnosis while staying StatusOther — with
// the pod gone, the container's own verdict is genuinely unknown.
func TestExtractResultPodNotFoundOtherJobFailure(t *testing.T) {
	ns := createUniqueNamespace(t)
	d := deployTestJob(t, ns, testEntry())
	setJobFailedCondition(t, ns, d.JobName(), batchv1.JobReasonBackoffLimitExceeded, "Job has reached the specified backoff limit")

	result := d.ExtractResult(context.Background())

	if result.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", result.ExitCode)
	}
	if result.CTRFStatus() != ctrf.StatusOther {
		t.Errorf("CTRFStatus = %q, want %q", result.CTRFStatus(), ctrf.StatusOther)
	}
	for _, want := range []string{"BackoffLimitExceeded", "Job has reached the specified backoff limit"} {
		if !strings.Contains(result.TerminationMsg, want) {
			t.Errorf("TerminationMsg = %q, want it to contain %q", result.TerminationMsg, want)
		}
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

// TestHandleTimeoutContainerNotFoundReflectsCause verifies the validator-
// container-absent branch routes through the actual wait cause (issue #1976
// item 3): a genuine deadline still reports the configured timeout, while an
// infra/unavailable cause is surfaced verbatim rather than misreported as the
// catalog timeout. The container-contract detail is retained as a suffix in
// both cases.
func TestHandleTimeoutContainerNotFoundReflectsCause(t *testing.T) {
	tests := []struct {
		name          string
		cause         error
		wantContains  []string
		wantExclusion string
	}{
		{
			name:         "genuine deadline reports configured timeout",
			cause:        context.DeadlineExceeded,
			wantContains: []string{"timeout: validator did not complete within", "not found - validator package contract"},
		},
		{
			name:          "infra/unavailable cause surfaced verbatim",
			cause:         errors.New(errors.ErrCodeUnavailable, "apiserver watch closed: connection refused"),
			wantContains:  []string{"validation failed:", "apiserver watch closed: connection refused", "not found - validator package contract"},
			wantExclusion: "timeout: validator did not complete within",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := createUniqueNamespace(t)
			d := deployTestJob(t, ns, testEntry())

			// Pod exists but carries only a non-validator container status, so
			// findContainerStatus reports the validator container as absent.
			createPodForJob(t, ns, d.JobName(), corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  "sidecar",
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
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

func TestParseExtraSentinels(t *testing.T) {
	const p = ctrf.ExtraLinePrefix
	tests := []struct {
		name        string
		logs        string
		wantCleaned string
		wantExtra   map[string]string
	}{
		{
			name:        "no sentinel leaves logs intact and extra nil",
			logs:        "Found 2 GPU node(s):\nAll OK",
			wantCleaned: "Found 2 GPU node(s):\nAll OK",
			wantExtra:   nil,
		},
		{
			name:        "valid sentinel populates extra and is stripped",
			logs:        "Found 2 GPU node(s):\n" + p + `{"nodesValidated":"1","nodesTotal":"2"}` + "\nSuccessfully verified",
			wantCleaned: "Found 2 GPU node(s):\nSuccessfully verified",
			wantExtra:   map[string]string{"nodesValidated": "1", "nodesTotal": "2"},
		},
		{
			name:        "malformed sentinel drops extra but is still stripped",
			logs:        "line before\n" + p + `{"nodesValidated":` + "\nline after",
			wantCleaned: "line before\nline after",
			wantExtra:   nil,
		},
		{
			name:        "multiple sentinels: last wins",
			logs:        p + `{"skipReason":"stale"}` + "\nwork\n" + p + `{"skipReason":"nodes-busy"}`,
			wantCleaned: "work",
			wantExtra:   map[string]string{"skipReason": "nodes-busy"},
		},
		{
			name:        "valid then malformed: last valid payload is kept",
			logs:        p + `{"nodesValidated":"1","nodesTotal":"2"}` + "\nwork\n" + p + `{"nodesValidated":`,
			wantCleaned: "work",
			wantExtra:   map[string]string{"nodesValidated": "1", "nodesTotal": "2"},
		},
		{
			name:        "empty-object sentinel yields nil extra",
			logs:        "work\n" + p + `{}`,
			wantCleaned: "work",
			wantExtra:   nil,
		},
		{
			// Prefix mid-line (not line-initial) is human stdout, never parsed.
			name:        "prefix mid-line is kept, not stripped",
			logs:        "note: " + p + "appears mid-line\nreal",
			wantCleaned: "note: " + p + "appears mid-line\nreal",
			wantExtra:   nil,
		},
		{
			// The prefix requires its trailing space: a line-initial "##AICR-EXTRA##"
			// without the space is not a sentinel and stays in stdout.
			name:        "prefix without trailing space is not a sentinel",
			logs:        p + `{"skipReason":"nodes-busy"}` + "\n" + `##AICR-EXTRA##nospace`,
			wantCleaned: `##AICR-EXTRA##nospace`,
			wantExtra:   map[string]string{"skipReason": "nodes-busy"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleaned, extra := parseExtraSentinels(tt.logs)
			if cleaned != tt.wantCleaned {
				t.Errorf("cleaned = %q, want %q", cleaned, tt.wantCleaned)
			}
			if len(extra) != len(tt.wantExtra) {
				t.Fatalf("extra = %v, want %v", extra, tt.wantExtra)
			}
			for k, v := range tt.wantExtra {
				if extra[k] != v {
					t.Errorf("extra[%q] = %q, want %q", k, extra[k], v)
				}
			}
		})
	}
}

func TestProcessValidatorLogs(t *testing.T) {
	const p = ctrf.ExtraLinePrefix
	// Emit the sentinel AFTER more than ValidatorMaxStdoutLines of output: the
	// invariant is that Extra is parsed from the full logs before truncation, so
	// a parse-after-truncate regression would silently drop this evidence.
	var b strings.Builder
	for i := range defaults.ValidatorMaxStdoutLines + 100 {
		fmt.Fprintf(&b, "log line %d\n", i)
	}
	b.WriteString(p + `{"nodesValidated":"1","nodesTotal":"2"}`)

	extra, stdout := processValidatorLogs(b.String())

	if extra["nodesValidated"] != "1" || extra["nodesTotal"] != "2" {
		t.Fatalf("late sentinel beyond truncation window was lost: extra=%v", extra)
	}
	if len(stdout) > defaults.ValidatorMaxStdoutLines {
		t.Errorf("stdout not truncated: %d lines > max %d", len(stdout), defaults.ValidatorMaxStdoutLines)
	}
	for _, line := range stdout {
		if strings.Contains(line, p) {
			t.Errorf("sentinel transport line leaked into stdout: %q", line)
		}
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
		name          string
		msg           string
		maxBytes      int
		wantContains  string // substring that must appear (empty = none)
		wantPrefixLen int    // exact retained prefix length, 0 = only bounded check
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
			// Only the 1-byte partial is dropped (10 → 9), nothing more.
			name:          "cut mid multibyte rune trims to boundary",
			msg:           strings.Repeat("€", 10), // 30 bytes
			maxBytes:      10,                      // splits the 4th rune (bytes 9-11)
			wantContains:  "... [truncated",
			wantPrefixLen: 9,
		},
		{
			// Regression for #1976: an invalid UTF-8 byte (0xFF) sits well before
			// maxBytes. The old whole-prefix validation trimmed head to empty and
			// kept only the suffix; the readable prefix — including the earlier
			// invalid byte — must survive.
			name:          "invalid byte before cut preserves prefix",
			msg:           "ok\xffmore" + strings.Repeat("z", 5000),
			maxBytes:      100,
			wantContains:  "... [truncated",
			wantPrefixLen: 100,
		},
		{
			// A complete-but-invalid byte (0xFF) landing exactly at maxBytes-1 is
			// NOT a cut-split partial rune — it is a full width-1 error rune that
			// was in the original message, so it must be preserved, not trimmed.
			// DecodeLastRuneInString alone cannot tell it apart from an incomplete
			// sequence; the FullRuneInString guard is what keeps the full prefix.
			name:          "invalid byte at boundary is preserved",
			msg:           strings.Repeat("a", 99) + "\xff" + strings.Repeat("z", 50),
			maxBytes:      100,
			wantContains:  "... [truncated",
			wantPrefixLen: 100,
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
			// A valid-UTF-8 input must stay valid after trimming only a trailing
			// partial rune; an input already carrying invalid bytes is preserved
			// verbatim, so validity is not required there.
			if utf8.ValidString(tt.msg) && !utf8.ValidString(got) {
				t.Errorf("truncated output is not valid UTF-8: %q", got)
			}
			if tt.wantContains != "" && !strings.Contains(got, tt.wantContains) {
				t.Errorf("output missing suffix %q, got %q", tt.wantContains, got)
			}
			// The readable prefix (everything before the truncation suffix) must
			// survive. A regression that validates the whole prefix and trims it
			// to empty would leave only the suffix — assert the prefix is
			// non-empty and that at most an incomplete trailing rune (< UTFMax
			// bytes) was trimmed back from maxBytes, not the entire message.
			suffixIdx := strings.LastIndex(got, "... [truncated")
			if suffixIdx <= 0 {
				t.Fatalf("readable prefix was discarded, got only the suffix: %q", got)
			}
			if tt.wantPrefixLen != 0 && suffixIdx != tt.wantPrefixLen {
				t.Errorf("retained prefix = %d bytes, want %d", suffixIdx, tt.wantPrefixLen)
			}
			if trimmed := tt.maxBytes - suffixIdx; trimmed < 0 || trimmed >= utf8.UTFMax {
				t.Errorf("trimmed %d bytes back from maxBytes=%d; want an incomplete-rune trim (0..%d)",
					trimmed, tt.maxBytes, utf8.UTFMax-1)
			}
		})
	}
}
