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
	"slices"
	"strings"
	"testing"
	"time"

	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
	"aicr.run/pkg/validator/catalog"
	"aicr.run/pkg/validator/ctrf"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// TestExtractResultListFailureListsOnce verifies a failed pod List is not
// retried to find a terminated pod, and the image of the last terminated pod
// observed is recorded.
func TestExtractResultListFailureListsOnce(t *testing.T) {
	const (
		planned  = "ghcr.io/x/deployment:edge"
		observed = "ghcr.io/x/deployment-observed:v2"
	)
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, stderrors.New("apiserver unavailable")
	})
	d := NewDeployer(Config{Clientset: cs, Namespace: "ns", Entry: catalog.ValidatorEntry{Name: "check", Phase: "deployment", Image: planned}})
	d.jobName = "job"
	d.lastTerminated = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: ValidatorContainerName, Image: observed}}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:    ValidatorContainerName,
			ImageID: "ghcr.io/x/deployment-observed@" + testDigestA,
			State:   corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3}},
		}}},
	}

	result := d.ExtractResult(t.Context())

	var podLists int
	for _, a := range cs.Actions() {
		if a.GetVerb() == "list" && a.GetResource().Resource == "pods" {
			podLists++
		}
	}
	if podLists != 1 {
		t.Errorf("pod lists = %d, want 1", podLists)
	}
	if want := []ctrf.PodImage{{Image: observed, Digest: testDigestA}}; !slices.Equal(result.Images, want) {
		t.Errorf("images = %+v, want %+v", result.Images, want)
	}
}

func TestWaitFailureMessageAndHandleTimeout(t *testing.T) {
	tests := []struct {
		name  string
		cause error
		code  errors.ErrorCode
		want  string
	}{
		{"deadline", errors.WrapCtxErr(context.DeadlineExceeded, errors.ErrCodeTimeout, "wait"), errors.ErrCodeTimeout, "timeout: validator did not complete within"},
		{"canceled", errors.WrapCtxErr(context.Canceled, errors.ErrCodeTimeout, "wait"), errors.ErrCodeCanceled, "canceled: validation run was interrupted"},
		{"unavailable", errors.New(errors.ErrCodeUnavailable, "connection refused"), errors.ErrCodeUnavailable, "validation failed:"},
		// WaitForJobTerminal wraps its initial Get/Watch failures as internal
		// without consulting ctx, so cancellation sits below the outer code.
		{"initial Get canceled", errors.Wrap(errors.ErrCodeInternal, "failed to get Job", context.Canceled), errors.ErrCodeInternal, "canceled: validation run was interrupted"},
		{"nested structured cancellation", errors.Wrap(errors.ErrCodeUnavailable, "infrastructure failed", errors.New(errors.ErrCodeCanceled, "inner")), errors.ErrCodeUnavailable, "canceled: validation run was interrupted"},
		{"initial Get deadline stays timeout", errors.Wrap(errors.ErrCodeInternal, "failed to get Job", context.DeadlineExceeded), errors.ErrCodeInternal, "timeout: validator did not complete within"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errorstest.WantReportedCode(t, tt.cause, tt.code)
			if msg := waitFailureMessage(tt.cause, time.Minute); !strings.HasPrefix(msg, tt.want) {
				t.Errorf("message=%q", msg)
			}
			for _, podState := range []string{"absent", "running", "no validator", "terminated"} {
				t.Run(podState, func(t *testing.T) {
					cs := fake.NewSimpleClientset()
					if podState != "absent" {
						status := corev1.ContainerStatus{Name: ValidatorContainerName, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
						if podState == "no validator" {
							status.Name = "sidecar"
						}
						if podState == "terminated" {
							status.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3, Message: "terminated by SIGTERM"}}
						}
						_, err := cs.CoreV1().Pods("ns").Create(t.Context(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", Labels: map[string]string{"batch.kubernetes.io/job-name": "job"}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{status}}}, metav1.CreateOptions{})
						if err != nil {
							t.Fatal(err)
						}
					}
					d := NewDeployer(Config{Clientset: cs, Namespace: "ns", Entry: catalog.ValidatorEntry{Name: "check", Phase: "deployment", Timeout: time.Minute}})
					d.jobName = "job"
					result := d.HandleTimeout(t.Context(), tt.cause)
					want := tt.want
					if podState == "terminated" {
						want = "terminated by SIGTERM"
					}
					if !strings.HasPrefix(result.TerminationMsg, want) {
						t.Errorf("message=%q want prefix %q", result.TerminationMsg, want)
					}
					if result.CTRFStatus() != ctrf.StatusOther {
						t.Errorf("status=%s", result.CTRFStatus())
					}
				})
			}
		})
	}
}

// TestHandleTimeoutPodLookupFailure pins that "pod never reached running
// state" is claimed only when the lookup confirmed no pod exists. A lookup
// that itself failed, as when the API connection that failed the wait is
// still down, must say so instead (issue #3709).
func TestHandleTimeoutPodLookupFailure(t *testing.T) {
	waitErr := errors.Wrap(errors.ErrCodeUnavailable, "job watch closed", stderrors.New("connection reset by peer"))
	tests := []struct {
		name      string
		listErr   error
		want      string
		wantNotIn string
	}{
		{
			name:      "no pod exists",
			want:      "(pod never reached running state)",
			wantNotIn: "could not be read",
		},
		{
			name:      "pod list fails",
			listErr:   apierrors.NewServiceUnavailable("apiserver unreachable"),
			want:      "(validator pod could not be read: ",
			wantNotIn: "never reached running state",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := fake.NewSimpleClientset()
			if tt.listErr != nil {
				cs.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, tt.listErr
				})
			}
			d := NewDeployer(Config{Clientset: cs, Namespace: "ns", Entry: catalog.ValidatorEntry{Name: "check", Phase: "performance", Timeout: time.Minute}})
			d.jobName = "job"

			result := d.HandleTimeout(t.Context(), waitErr)

			if result.ExitCode != -1 || result.CTRFStatus() != ctrf.StatusOther {
				t.Errorf("exit=%d status=%s, want -1/%s", result.ExitCode, result.CTRFStatus(), ctrf.StatusOther)
			}
			if prefix := waitFailureMessage(waitErr, time.Minute); !strings.HasPrefix(result.TerminationMsg, prefix) {
				t.Errorf("message=%q, want the wait cause %q first", result.TerminationMsg, prefix)
			}
			if !strings.Contains(result.TerminationMsg, tt.want) || strings.Contains(result.TerminationMsg, tt.wantNotIn) {
				t.Errorf("message=%q, want %q and not %q", result.TerminationMsg, tt.want, tt.wantNotIn)
			}
			if tt.listErr != nil && !strings.Contains(result.TerminationMsg, "apiserver unreachable") {
				t.Errorf("message=%q, want the lookup cause named", result.TerminationMsg)
			}
		})
	}
}

// TestExtractResultMissingPod verifies the result when the lookup finds no
// live pod for the Job. On a deadline failure the Job controller deleted the
// pod, and the check's own SIGTERM result is still read from the pod in its
// deletion grace or from the last status the deployer observed, so the
// verdict stays failed with the check's message. Without a deadline failure
// the result stays inconclusive. Either way the Job is read before the one
// terminated-pod List, which also supplies the image.
func TestExtractResultMissingPod(t *testing.T) {
	const (
		checkMsg = "terminated by SIGTERM before the check completed"
		image    = "ghcr.io/x/deployment:edge"
	)
	terminated := func(deleting bool) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", Labels: map[string]string{"batch.kubernetes.io/job-name": "job"}},
			Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: ValidatorContainerName, Image: image}}},
			Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:    ValidatorContainerName,
				ImageID: "ghcr.io/x/deployment@" + testDigestA,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 3, Message: checkMsg,
				}},
			}}},
		}
		if deleting {
			now := metav1.Now()
			p.DeletionTimestamp = &now
			p.Finalizers = []string{"test/hold"}
		}
		return p
	}
	failed := func(reason, message string) []batchv1.JobCondition {
		return []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: reason, Message: message}}
	}
	deadline := failed(batchv1.JobReasonDeadlineExceeded, "Job was active longer than specified deadline")
	withDigest := []ctrf.PodImage{{Image: image, Digest: testDigestA}}

	tests := []struct {
		name         string
		conditions   []batchv1.JobCondition
		listed       *corev1.Pod
		lastObserved *corev1.Pod
		wantExit     int32
		wantMsg      string
		wantCheckMsg bool
		wantImages   []ctrf.PodImage
	}{
		{"deadline, terminating pod is still listed", deadline, terminated(true), nil, validatorExitFailed, "Job deadline", true, withDigest},
		{"deadline, pod deleted, last observed status survives", deadline, nil, terminated(false), validatorExitFailed, "Job deadline", true, withDigest},
		{"deadline, nothing observed keeps the generic message", deadline, nil, nil, validatorExitFailed, "Job deadline", false, []ctrf.PodImage{{Image: image}}},
		{"no Failed condition", nil, terminated(true), nil, -1, "pod not found for Job job", false, withDigest},
		{"Failed without a deadline", failed(batchv1.JobReasonBackoffLimitExceeded, "Job has reached the specified backoff limit"),
			terminated(true), nil, -1, "Job job failed (BackoffLimitExceeded)", false, withDigest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "ns"}, Status: batchv1.JobStatus{Conditions: tt.conditions}}
			cs := fake.NewSimpleClientset(job)
			if tt.listed != nil {
				if _, err := cs.CoreV1().Pods("ns").Create(t.Context(), tt.listed, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			d := NewDeployer(Config{Clientset: cs, Namespace: "ns", Entry: catalog.ValidatorEntry{Name: "check", Phase: "deployment", Image: image, Timeout: time.Minute}})
			d.jobName = "job"
			d.lastTerminated = tt.lastObserved

			result := d.ExtractResult(t.Context())

			var podLists int
			jobReadFirst := false
			for _, a := range cs.Actions() {
				switch {
				case a.GetVerb() == "list" && a.GetResource().Resource == "pods":
					podLists++
				case a.GetVerb() == "get" && a.GetResource().Resource == "jobs":
					jobReadFirst = podLists == 1
				}
			}
			if podLists != 2 {
				t.Errorf("pod lists = %d, want 2", podLists)
			}
			if !jobReadFirst {
				t.Error("Job condition was not read before the terminated-pod lookup")
			}
			if !slices.Equal(result.Images, tt.wantImages) {
				t.Errorf("images=%+v, want %+v", result.Images, tt.wantImages)
			}
			if result.ExitCode != tt.wantExit || result.CTRFStatus() != ctrf.ExitCodeToCTRFStatus(tt.wantExit) {
				t.Errorf("exit=%d status=%s, want exit %d", result.ExitCode, result.CTRFStatus(), tt.wantExit)
			}
			if !strings.Contains(result.TerminationMsg, tt.wantMsg) {
				t.Errorf("message=%q, want it to contain %q", result.TerminationMsg, tt.wantMsg)
			}
			if got := strings.Contains(result.TerminationMsg, "check reported: exit code 3: "+checkMsg); got != tt.wantCheckMsg {
				t.Errorf("message=%q, check message present = %v, want %v", result.TerminationMsg, got, tt.wantCheckMsg)
			}
		})
	}
}

// TestHandleTimeoutReadsTerminatingPod pins that a wait failure still reads the
// check's own result from a pod already being deleted, which getPodForJob skips.
func TestHandleTimeoutReadsTerminatingPod(t *testing.T) {
	now := metav1.Now()
	cs := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pod", Namespace: "ns", DeletionTimestamp: &now, Finalizers: []string{"test/hold"},
			Labels: map[string]string{"batch.kubernetes.io/job-name": "job"},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  ValidatorContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 3, Message: "terminated by SIGTERM"}},
		}}},
	})
	d := NewDeployer(Config{Clientset: cs, Namespace: "ns", Entry: catalog.ValidatorEntry{Name: "check", Phase: "deployment", Timeout: time.Minute}})
	d.jobName = "job"

	result := d.HandleTimeout(t.Context(), context.DeadlineExceeded)

	if result.ExitCode != 3 || !strings.HasPrefix(result.TerminationMsg, "terminated by SIGTERM") {
		t.Errorf("exit=%d message=%q, want the check's own exit 3 result", result.ExitCode, result.TerminationMsg)
	}
}
