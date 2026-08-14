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
	"encoding/json"
	stderrors "errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ValidatorContainerName is the required name for the validator container.
	// This is part of the validator package contract to ensure sidecar-safety.
	ValidatorContainerName = "validator"

	// validatorExitFailed is the container exit code that ctrf maps to
	// StatusFailed. Used when a Job-level condition is a definitive verdict
	// even though no container status survives to report it.
	validatorExitFailed int32 = 1
)

// ExtractResult reads the exit code, termination message, and stdout from the
// "validator" container in a completed validator pod.
//
// CONTRACT: The container name MUST be "validator". This is a frozen public
// contract of the validator package to ensure sidecar-safety — ExtractResult
// will only read from the "validator" container, ignoring any sidecar containers
// that may be injected by external controllers (e.g., log streaming, result
// processing).
//
// Returns a ValidatorResult regardless of how the container terminated — the
// caller maps the result to a CTRF status.
//
// This method must be called after WaitForCompletion returns, when the Job is
// in a terminal state (Complete or Failed).
func (d *Deployer) ExtractResult(ctx context.Context) *ctrf.ValidatorResult {
	result := &ctrf.ValidatorResult{
		Name:  d.config.Entry.Name,
		Phase: d.config.Entry.Phase,
	}

	// Find the pod for this Job
	jobPod, err := d.getPodForJob(ctx)
	if err != nil {
		// Pod was never created, was deleted externally, or — the common
		// case — the Job controller killed and deleted it on
		// activeDeadlineSeconds expiry. The Job object still records why,
		// so classify from its Failed condition instead of reporting a bare
		// "pod not found" (see issue #2186).
		result.ExitCode, result.TerminationMsg = d.missingPodOutcome(ctx, err)
		return result
	}

	// Extract container status from "validator" container
	cs, found := findContainerStatus(jobPod.Status.ContainerStatuses, ValidatorContainerName)
	if !found {
		result.ExitCode = -1
		result.TerminationMsg = fmt.Sprintf("container %q not found (validator package contract)", ValidatorContainerName)
		return result
	}
	switch {
	case cs.State.Terminated != nil:
		result.ExitCode = cs.State.Terminated.ExitCode
		result.TerminationMsg = boundTerminationMsg(cs.State.Terminated.Message, defaults.ValidatorMaxTerminationMsgBytes)
		if cs.State.Terminated.Reason == "OOMKilled" {
			result.TerminationMsg = "Container OOMKilled"
		}
		result.StartTime = cs.State.Terminated.StartedAt.Time
		result.CompletionTime = cs.State.Terminated.FinishedAt.Time
		result.Duration = result.CompletionTime.Sub(result.StartTime)

	case cs.State.Waiting != nil:
		// Container never started (image pull failure, etc.)
		result.ExitCode = -1
		result.TerminationMsg = fmt.Sprintf("%s: %s", cs.State.Waiting.Reason, cs.State.Waiting.Message)
		return result // No logs to capture

	case cs.State.Running != nil:
		// Should not happen after WaitForCompletion, but handle defensively
		result.ExitCode = -1
		result.TerminationMsg = "container still running after wait completed"
	}

	// Capture stdout from pod logs (explicit container name)
	logs, logErr := pod.GetPodLogs(ctx, d.config.Clientset, d.config.Namespace, jobPod.Name, ValidatorContainerName)
	if logErr != nil {
		slog.Warn("failed to capture pod logs", "pod", jobPod.Name, "error", logErr)
		// Not fatal — we still have exit code and termination message
	} else if logs != "" {
		result.Extra, result.Stdout = processValidatorLogs(logs)
	}

	return result
}

// missingPodOutcome classifies an ExtractResult run whose Job pod could not be
// found, returning the exit code and termination message to report.
//
// The dominant cause is not a vanished pod but a deadline: when a validator Job
// exceeds its activeDeadlineSeconds, the Job controller kills the pod AND
// deletes it, so the pod lookup legitimately finds nothing. Reporting the raw
// "pod not found" there mislabels a reproducible, deadline-bounded failure as
// an inconclusive infrastructure error (StatusOther), burying the fact that the
// Job never reached a passing verdict in the time it was given (issue #2186).
//
// The Job object outlives its pod and still carries the reason, so:
//
//   - Failed/DeadlineExceeded — a definitive verdict: the validator Job did not
//     succeed within its own deadline. Reported as exit code 1 (StatusFailed)
//     with the deadline named, so the operator reads "this check did not pass
//     in time", not "the infrastructure hiccuped". Note this says nothing about
//     whether the container body ever ran: a pod that stayed Pending for the
//     whole deadline (see the required dependency-affinity case documented in
//     pkg/validator/v1/affinity.go) reaches the same condition, and the two are
//     indistinguishable from Job status once the pod is deleted (issue #2189).
//     The wording below is deliberately about the Job, not the check body.
//   - Any other Failed condition — the reason and message are surfaced for
//     diagnosis, but the exit code stays -1 (StatusOther): the pod is gone and
//     the underlying container verdict is genuinely unknown.
//   - No terminal Failed condition (or the Job itself cannot be read) — the
//     original "pod not found" wording is preserved verbatim.
//
// Both branches remain blocking (ctrf.IsFailingStatus covers failed and other), so
// this changes only diagnostic fidelity, never whether the phase fails.
func (d *Deployer) missingPodOutcome(ctx context.Context, findErr error) (exitCode int32, msg string) {
	notFound := fmt.Sprintf("pod not found for Job %s: %v", d.jobName, findErr)

	// Only a CONFIRMED absence licenses a Job-condition diagnosis.
	// GetPodForJob distinguishes "every candidate is gone" (ErrCodeNotFound)
	// from "the List itself failed" (ErrCodeInternal); an apiserver hiccup is
	// the latter, and it establishes nothing about the pod. Promoting it to a
	// definitive verdict on the strength of a Job condition alone would flatten
	// an ambiguous lookup into a claim the controller deleted the pod, which is
	// exactly the failure mode the repo's fail-closed lookup rule forbids. An
	// unestablished absence stays inconclusive.
	if !stderrors.Is(findErr, errors.New(errors.ErrCodeNotFound, "")) {
		return -1, notFound
	}

	job, cond, ok := d.jobFailedCondition(ctx)
	if !ok {
		return -1, notFound
	}

	detail := strings.TrimSpace(cond.Message)
	if detail == "" {
		detail = cond.Reason
	}

	// Both messages interpolate Job condition text straight from API state, so
	// they are capped like any other termination message. The constant exists
	// precisely because the result flows into ConfigMaps and rendered reports,
	// and the pod-status branch above already bounds its own message the same
	// way — an unbounded Job condition would be the one way around it.
	if cond.Reason == batchv1.JobReasonDeadlineExceeded {
		// States the observed fact — no pod remains — rather than naming a
		// cause. ErrCodeNotFound covers a pod deleted by the Job controller on
		// deadline expiry (the dominant case), one never created at all
		// (quota or webhook rejection), and one removed externally; asserting
		// controller deletion would name a false root cause for the other two.
		return validatorExitFailed, boundTerminationMsg(fmt.Sprintf(
			"validator Job did not complete within its %s deadline; no pod remains for it, so its logs are unavailable (Job condition Failed/%s: %s)",
			enforcedDeadline(job, d.config.Entry.Timeout), cond.Reason, detail),
			defaults.ValidatorMaxTerminationMsgBytes)
	}

	return -1, boundTerminationMsg(fmt.Sprintf("Job %s failed (%s): %s; pod not found: %v",
		d.jobName, cond.Reason, detail, findErr),
		defaults.ValidatorMaxTerminationMsgBytes)
}

// jobFailedCondition returns the validator Job and its terminal Failed
// condition, if the Job can still be read and carries one. The Job itself is
// returned alongside the condition so callers can report the deadline
// Kubernetes actually enforced (spec.activeDeadlineSeconds) rather than
// re-deriving it. A Get failure (including the
// NotFound left by an already-reaped Job) is reported at DEBUG and yields
// ok=false so the caller falls back to its original message — diagnosis must
// never be blocked on a second API call succeeding.
//
// The read is explicitly bounded: `aicr validate` opts out of a facade-level
// deadline (WithValidationTimeout(0)), so the inherited context may carry none
// at all. Left unbounded, a stalled apiserver would hang result extraction
// indefinitely instead of emitting the very diagnosis this call exists to
// produce. K8sCleanupTimeout is the same short allowance the orchestrator
// already grants the sibling post-mortem capture around HandleTimeout.
func (d *Deployer) jobFailedCondition(ctx context.Context) (*batchv1.Job, batchv1.JobCondition, bool) {
	ctx, cancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
	defer cancel()

	job, err := d.config.Clientset.BatchV1().Jobs(d.config.Namespace).Get(ctx, d.jobName, metav1.GetOptions{})
	if err != nil {
		slog.Debug("failed to read Job while diagnosing missing pod", "job", d.jobName, "error", err)
		return nil, batchv1.JobCondition{}, false
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return job, c, true
		}
	}
	return nil, batchv1.JobCondition{}, false
}

// processValidatorLogs turns raw pod logs into the structured Extra map and the
// human-readable Stdout lines. Order matters and is the invariant both call
// sites rely on: the Extra sentinel is parsed from the FULL logs BEFORE any
// tail-truncation, so a sentinel emitted after more than ValidatorMaxStdoutLines
// of output still survives; only then is the sentinel-stripped remainder
// truncated and length-capped for human display.
func processValidatorLogs(logs string) (extra map[string]string, stdout []string) {
	cleaned, extra := parseExtraSentinels(logs)
	stdout = filterStdoutLines(
		truncateLogLines(cleaned, defaults.ValidatorMaxStdoutLines),
		defaults.ValidatorMaxStdoutLineLength,
	)
	return extra, stdout
}

// parseExtraSentinels scans the raw pod logs for ctrf.ExtraLinePrefix sentinel
// lines (the transport for a check's structured Extra map) and returns the logs
// with every sentinel line removed plus the LAST VALID non-empty payload.
//
// Each sentinel is parsed as it is encountered: a malformed one is logged at
// WARN and skipped WITHOUT discarding an earlier valid payload, so a valid
// coverage/skip line followed by a garbled line still yields the valid map. A
// merged/garbled line must never flip a passing check to an error — the exit
// code, not this map, is the verdict. Returns (logs, nil) when no sentinel is
// present, and extra is nil unless some line parsed to a non-empty object.
func parseExtraSentinels(logs string) (cleaned string, extra map[string]string) {
	if !strings.Contains(logs, ctrf.ExtraLinePrefix) {
		return logs, nil
	}
	lines := strings.Split(logs, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if payload, ok := strings.CutPrefix(line, ctrf.ExtraLinePrefix); ok {
			var parsed map[string]string
			if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
				slog.Warn("failed to parse validator extra sentinel; dropping", "error", err)
			} else if len(parsed) > 0 {
				extra = parsed // keep the last VALID non-empty payload
			}
			continue // transport, not human evidence — strip it
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n"), extra
}

// HandleTimeout extracts whatever result is available when the orchestrator's
// wait returned an error. Uses a fresh context since the parent may be
// canceled.
//
// waitCause is the error returned by WaitForCompletion. It classifies the
// failure so the rendered TerminationMsg reflects the ACTUAL cause: a genuine
// context-deadline expiry (stdlib DeadlineExceeded or a pkg/errors
// ErrCodeTimeout) renders the "timeout: validator did not complete within
// <configured>" wording, while any other cause (e.g. an infra/unavailable
// error) renders "validation failed: <cause>" so a mid-run infra failure is
// not misreported as the full catalog timeout (see issue #1966). A nil
// waitCause is treated as a timeout for backward compatibility.
func (d *Deployer) HandleTimeout(ctx context.Context, waitCause error) *ctrf.ValidatorResult {
	result := &ctrf.ValidatorResult{
		Name:  d.config.Entry.Name,
		Phase: d.config.Entry.Phase,
	}

	// Try to find the pod
	jobPod, err := d.getPodForJob(ctx)
	if err != nil {
		result.ExitCode = -1
		result.TerminationMsg = "pod never reached running state"
		return result
	}

	// Check container status from "validator" container first (before fetching logs)
	cs, found := findContainerStatus(jobPod.Status.ContainerStatuses, ValidatorContainerName)
	if !found {
		result.ExitCode = -1
		// Route through waitFailureMessage so an infra/unavailable waitCause is
		// not misreported as the catalog timeout (see issue #1966); keep the
		// container-contract detail as a suffix.
		result.TerminationMsg = fmt.Sprintf("%s (container %q not found - validator package contract)",
			waitFailureMessage(waitCause, effectiveTimeout(d.config.Entry.Timeout)), ValidatorContainerName)
		return result
	}

	// Try to get logs from "validator" container. Parse and strip the Extra
	// sentinel here too: a check that emits coverage counts before hitting the
	// timeout still yields structured evidence.
	if logs, logErr := pod.GetPodLogs(ctx, d.config.Clientset, d.config.Namespace, jobPod.Name, ValidatorContainerName); logErr == nil && logs != "" {
		result.Extra, result.Stdout = processValidatorLogs(logs)
	}

	if cs.State.Terminated != nil {
		result.ExitCode = cs.State.Terminated.ExitCode
		result.TerminationMsg = boundTerminationMsg(cs.State.Terminated.Message, defaults.ValidatorMaxTerminationMsgBytes)
		result.StartTime = cs.State.Terminated.StartedAt.Time
		result.CompletionTime = cs.State.Terminated.FinishedAt.Time
		result.Duration = result.CompletionTime.Sub(result.StartTime)
	} else {
		result.ExitCode = -1
		result.TerminationMsg = waitFailureMessage(waitCause, effectiveTimeout(d.config.Entry.Timeout))
	}

	return result
}

// effectiveTimeout mirrors the fallback runPhase applies before waiting: a
// catalog entry with no explicit timeout is waited on for
// defaults.ValidatorDefaultTimeout, so the rendered message must report that
// same effective value rather than a bare "0s".
func effectiveTimeout(configured time.Duration) time.Duration {
	if configured == 0 {
		return defaults.ValidatorDefaultTimeout
	}
	return configured
}

// enforcedDeadline is the deadline Kubernetes actually applied, as opposed to
// the duration the catalog asked for. The authoritative source is the live
// Job's spec.activeDeadlineSeconds — the literal value the Job controller
// enforced — so it is preferred whenever the Job could be read and carries it.
//
// The fallback re-derives it the way BuildJobPlan does, converting the catalog
// timeout with int64(timeout.Seconds()) before assigning it to
// ActiveDeadlineSeconds (pkg/validator/v1/job_plan.go), so any sub-second
// component is truncated away: a 1500ms entry yields a 1s Job deadline.
// Reporting the raw duration would name a deadline that never existed, and the
// whole point of this message is to describe the Kubernetes state that produced
// the failure.
func enforcedDeadline(job *batchv1.Job, configured time.Duration) time.Duration {
	if job != nil && job.Spec.ActiveDeadlineSeconds != nil {
		return time.Duration(*job.Spec.ActiveDeadlineSeconds) * time.Second
	}
	return time.Duration(int64(effectiveTimeout(configured).Seconds())) * time.Second
}

// waitFailureMessage renders the TerminationMsg for a validator whose container
// never terminated. Only a genuine context-deadline expiry is reported as a
// "timeout ... within <configured>" — any other cause (infra/unavailable) is
// surfaced verbatim so diagnosis is not misdirected to the catalog timeout
// (see issue #1966). A nil cause is treated as a timeout for backward
// compatibility with callers that had no error to thread through.
func waitFailureMessage(cause error, configured time.Duration) string {
	if cause == nil || isDeadlineCause(cause) {
		return fmt.Sprintf("timeout: validator did not complete within %s", configured)
	}
	return fmt.Sprintf("validation failed: %v", cause)
}

// isDeadlineCause reports whether err represents a genuine context-deadline
// expiry — either the stdlib context.DeadlineExceeded sentinel or a pkg/errors
// StructuredError carrying ErrCodeTimeout.
func isDeadlineCause(err error) bool {
	if err == nil {
		return false
	}
	if stderrors.Is(err, context.DeadlineExceeded) {
		return true
	}
	return stderrors.Is(err, errors.New(errors.ErrCodeTimeout, ""))
}

// truncateLogLines splits raw log output into lines and returns at most the
// last maxLines lines (tail behavior).
func truncateLogLines(logs string, maxLines int) []string {
	lines := strings.Split(strings.TrimRight(logs, "\n"), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return lines
}

// filterStdoutLines truncates lines that exceed maxLineLen characters.
// Lines longer than maxLineLen are cut to maxLineLen with a
// "... [truncated N chars]" suffix appended.
func filterStdoutLines(lines []string, maxLineLen int) []string {
	if len(lines) == 0 {
		return lines
	}

	for i, line := range lines {
		if len(line) > maxLineLen {
			dropped := len(line) - maxLineLen
			lines[i] = line[:maxLineLen] + fmt.Sprintf("... [truncated %d chars]", dropped)
		}
	}

	return lines
}

// boundTerminationMsg defensively caps a container termination message at
// maxBytes, appending a truncation suffix that reports the dropped byte count.
// The kubelet already caps the message upstream, but bounding it at the source
// keeps oversized messages out of ConfigMaps and rendered reports regardless.
func boundTerminationMsg(msg string, maxBytes int) string {
	if len(msg) <= maxBytes {
		return msg
	}
	// Trim only a trailing partial rune — a multi-byte rune the cut split — so it
	// is never emitted as an incomplete sequence. Scan back to the final rune's
	// start byte and drop those bytes only when they do NOT form a full rune.
	// utf8.FullRuneInString distinguishes an incomplete sequence (dropped) from a
	// complete-but-invalid byte such as a lone 0xFF (kept): the latter was in the
	// original message, not an artifact of the cut, so it is preserved just like
	// any invalid byte earlier in the readable prefix (see issue #1976).
	head := msg[:maxBytes]
	start := len(head)
	for start > 0 && start > len(head)-utf8.UTFMax {
		start--
		if utf8.RuneStart(head[start]) {
			break
		}
	}
	if start < len(head) && !utf8.FullRuneInString(head[start:]) {
		head = head[:start]
	}
	return head + fmt.Sprintf("... [truncated %d bytes]", len(msg)-len(head))
}

// getPodForJob finds the pod created by the validator Job using the shared
// pod.GetPodForJob helper. Kept as a thin wrapper so existing call sites
// inside this file remain readable.
func (d *Deployer) getPodForJob(ctx context.Context) (*corev1.Pod, error) {
	return pod.GetPodForJob(ctx, d.config.Clientset, d.config.Namespace, d.jobName)
}

// findContainerStatus finds a container status by name in the pod's container
// status list. Returns the container status and true if found, or a zero value
// and false if not found.
//
// This helper ensures sidecar-safety by allowing explicit container name lookup
// instead of assuming index 0 is the validator container.
func findContainerStatus(statuses []corev1.ContainerStatus, name string) (corev1.ContainerStatus, bool) {
	for _, cs := range statuses {
		if cs.Name == name {
			return cs, true
		}
	}
	return corev1.ContainerStatus{}, false
}
