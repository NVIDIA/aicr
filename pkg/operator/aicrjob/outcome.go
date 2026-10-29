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

package aicrjob

import (
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/NVIDIA/aicr/api/operator/v1alpha1"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// maxMessageBytes bounds the termination message carried into errors, so a
// noisy container cannot bloat conditions or Events. It leaves room for the
// error's own prefix within a condition message.
const maxMessageBytes = 768

// State is where an AICR Job stands.
type State string

// Job states.
const (
	// StateRunning includes suspended and pending Jobs.
	StateRunning   State = "Running"
	StateSucceeded State = "Succeeded"
	StateFailed    State = "Failed"
)

// Outcome is the observed result of one AICR Job run.
type Outcome struct {
	State State
	// Message is the container's termination message: on success the
	// machine-readable result (for example the --image-refs digest), on
	// failure AICR's error text. Bounded to maxMessageBytes.
	Message string
	// Err is set for StateFailed and classifies the failure.
	Err error
	// Waiting, while a run is in progress, says why its pod is not running,
	// with the Job's name: not scheduled, a container that cannot start yet,
	// or no pod at all. Empty while it runs normally.
	Waiting string
	// cannotStart reports a container the cluster can never start as
	// configured; a new run would fail the same way.
	cannotStart bool
	// exited reports a failed run whose container exited with exitCode;
	// a run without one hit its deadline, was evicted or never started.
	exited   bool
	exitCode int32
}

// Retry is how a failed run is retried.
type Retry int

// Retry classes.
const (
	// RetryBounded: internal and unclassified exits, and runs that failed
	// without an exit (deadline, eviction), get new runs up to a bound per
	// stage, then stall.
	RetryBounded Retry = iota
	// RetryTransient: AICR exited timeout, unavailable, rate limited or
	// canceled; new runs follow with backoff, without a bound.
	RetryTransient
	// RetryNever: invalid input or not found; the stage stalls until its
	// input changes.
	RetryNever
	// RetryAuthorization: unauthorized. A credential can be rejected for a
	// moment, such as a ServiceAccount recreated seconds before; new runs
	// follow with backoff, up to the stage's bound, then the stage stalls.
	RetryAuthorization
)

// Retry classifies a failed run.
func (o Outcome) Retry() Retry {
	if o.cannotStart {
		return RetryNever
	}
	if !o.exited {
		return RetryBounded
	}
	switch o.exitCode {
	case ExitTimeout, ExitUnavailable, ExitRateLimited, ExitCanceled:
		return RetryTransient
	case ExitInvalidInput, ExitNotFound:
		return RetryNever
	case ExitUnauthorized:
		return RetryAuthorization
	default:
		return RetryBounded
	}
}

// ObserveOutcome reads a Job and its pod. Results from a run other than
// wantRunID are ignored by the caller; ObserveOutcome rejects them so a stale
// Job can never complete the current stage.
func ObserveOutcome(stage Stage, wantRunID string, job *batchv1.Job, pod *corev1.Pod) (Outcome, error) {
	if got := job.Labels[v1alpha1.LabelRunID]; got != wantRunID {
		return Outcome{}, operrors.New(operrors.ErrCodeConflict,
			fmt.Sprintf("Job %s belongs to run %q, not %q", job.Name, got, wantRunID))
	}
	switch {
	case hasCondition(job, batchv1.JobComplete):
		return Outcome{State: StateSucceeded, Message: terminationMessage(pod)}, nil
	case hasCondition(job, batchv1.JobFailed):
		code, ok := exitCode(pod)
		msg := terminationMessage(pod)
		if !ok {
			// No terminated container: deadline exceeded, eviction or a pod
			// that never started. A new run may succeed, within the bound.
			reason := failedReason(job)
			if last := job.Annotations[v1alpha1.AnnotationWaiting]; last != "" {
				reason += "; last: " + last
			}
			return Outcome{State: StateFailed, Message: msg, Err: operrors.New(operrors.ErrCodeTimeout,
				fmt.Sprintf("aicr %s Job failed without a container exit (%s)", stage, reason)).
				WithContext(map[string]any{"stage": string(stage), "jobReason": reason})}, nil
		}
		return Outcome{State: StateFailed, Message: msg, Err: ErrorFromExitCode(stage, code, msg), exited: true, exitCode: code}, nil
	default:
		waiting, cannotStart := podWaiting(job, pod)
		if cannotStart {
			return Outcome{State: StateFailed, Waiting: waiting, cannotStart: true, Err: operrors.New(operrors.ErrCodeInvalidRequest,
				fmt.Sprintf("aicr %s cannot start: %s", stage, waiting))}, nil
		}
		return Outcome{State: StateRunning, Waiting: waiting}, nil
	}
}

// unstartable are container waiting reasons no retry of the same pod spec
// changes: a malformed image reference, or a container configuration the
// cluster refuses.
var unstartable = map[string]bool{"InvalidImageName": true, "CreateContainerConfigError": true}

// podWaiting says why an unfinished Job's pod is not running, if it is not,
// and whether it never can.
func podWaiting(job *batchv1.Job, pod *corev1.Pod) (string, bool) {
	if pod == nil {
		if job.Spec.Suspend != nil && *job.Spec.Suspend {
			return "", false
		}
		return "Job " + job.Name + " has no pod; see the Job's Events", false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return bound("Job " + job.Name + " is not scheduled: " + c.Message), false
		}
	}
	for _, s := range pod.Status.ContainerStatuses {
		if w := s.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" && w.Reason != "PodInitializing" {
			return bound("Job " + job.Name + ": " + w.Reason + ": " + w.Message), unstartable[w.Reason]
		}
	}
	return "", false
}

// bound keeps a status-bound message within maxMessageBytes.
func bound(msg string) string {
	if len(msg) <= maxMessageBytes {
		return msg
	}
	return strings.ToValidUTF8(msg[:maxMessageBytes], "")
}

func hasCondition(job *batchv1.Job, t batchv1.JobConditionType) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func failedReason(job *batchv1.Job) string {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return c.Reason
		}
	}
	return "Unknown"
}

func terminated(pod *corev1.Pod) *corev1.ContainerStateTerminated {
	if pod == nil {
		return nil
	}
	for i := range pod.Status.ContainerStatuses {
		if s := pod.Status.ContainerStatuses[i]; s.Name == "aicr" && s.State.Terminated != nil {
			return s.State.Terminated
		}
	}
	return nil
}

func exitCode(pod *corev1.Pod) (int32, bool) {
	if t := terminated(pod); t != nil {
		return t.ExitCode, true
	}
	return 0, false
}

func terminationMessage(pod *corev1.Pod) string {
	t := terminated(pod)
	if t == nil {
		return ""
	}
	// A failed run's message is the log tail, and AICR's error line is last,
	// so keep the end.
	msg := strings.TrimSpace(t.Message)
	if len(msg) > maxMessageBytes {
		msg = msg[len(msg)-maxMessageBytes:]
		// Start at a whole line.
		if i := strings.IndexByte(msg, '\n'); i >= 0 && i < len(msg)-1 {
			msg = msg[i+1:]
		}
	}
	return msg
}
