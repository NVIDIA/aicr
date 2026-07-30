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
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	corev1 "k8s.io/api/core/v1"
)

const (
	// ValidatorContainerName is the required name for the validator container.
	// This is part of the validator package contract to ensure sidecar-safety.
	ValidatorContainerName = "validator"
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
		// Pod was never created or was deleted externally
		result.ExitCode = -1
		result.TerminationMsg = fmt.Sprintf("pod not found for Job %s: %v", d.jobName, err)
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
		result.Stdout = filterStdoutLines(
			truncateLogLines(logs, defaults.ValidatorMaxStdoutLines),
			defaults.ValidatorMaxStdoutLineLength,
		)
	}

	return result
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
		result.TerminationMsg = fmt.Sprintf("timeout: validator did not complete within %s (container %q not found - validator package contract)", effectiveTimeout(d.config.Entry.Timeout), ValidatorContainerName)
		return result
	}

	// Try to get logs from "validator" container
	if logs, logErr := pod.GetPodLogs(ctx, d.config.Clientset, d.config.Namespace, jobPod.Name, ValidatorContainerName); logErr == nil && logs != "" {
		result.Stdout = filterStdoutLines(
			truncateLogLines(logs, defaults.ValidatorMaxStdoutLines),
			defaults.ValidatorMaxStdoutLineLength,
		)
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
	// Trim the cut back to a valid UTF-8 rune boundary so a multi-byte rune is
	// never split into an invalid sequence in the emitted message.
	head := msg[:maxBytes]
	for len(head) > 0 && !utf8.ValidString(head) {
		head = head[:len(head)-1]
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
