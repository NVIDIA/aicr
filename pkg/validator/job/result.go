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
	"fmt"
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/k8s/pod"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	corev1 "k8s.io/api/core/v1"
)

// ExtractResult reads the exit code, termination message, and stdout from a
// completed validator pod. Returns a ValidatorResult regardless of how the
// container terminated — the caller maps the result to a CTRF status.
//
// This method must be called after WaitForCompletion returns, when the Job is
// in a terminal state (Complete or Failed).
func (d *Deployer) ExtractResult(ctx context.Context) *ctrf.ValidatorResult {
	result := &ctrf.ValidatorResult{
		Name:  d.entry.Name,
		Phase: d.entry.Phase,
	}

	// Find the pod for this Job
	jobPod, err := d.getPodForJob(ctx)
	if err != nil {
		// Pod was never created or was deleted externally
		result.ExitCode = -1
		result.TerminationMsg = fmt.Sprintf("pod not found for Job %s: %v", d.jobName, err)
		return result
	}

	// Extract container status
	if len(jobPod.Status.ContainerStatuses) == 0 {
		result.ExitCode = -1
		result.TerminationMsg = "no container status available"
		return result
	}

	cs := jobPod.Status.ContainerStatuses[0]
	switch {
	case cs.State.Terminated != nil:
		result.ExitCode = cs.State.Terminated.ExitCode
		result.TerminationMsg = cs.State.Terminated.Message
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

	// Capture stdout from pod logs
	logs, logErr := pod.GetPodLogs(ctx, d.clientset, d.namespace, jobPod.Name, "")
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
// wait has timed out. Uses a fresh context since the parent may be canceled.
func (d *Deployer) HandleTimeout(ctx context.Context) *ctrf.ValidatorResult {
	result := &ctrf.ValidatorResult{
		Name:  d.entry.Name,
		Phase: d.entry.Phase,
	}

	// Try to find the pod
	jobPod, err := d.getPodForJob(ctx)
	if err != nil {
		result.ExitCode = -1
		result.TerminationMsg = "pod never reached running state"
		return result
	}

	// Try to get logs
	if logs, logErr := pod.GetPodLogs(ctx, d.clientset, d.namespace, jobPod.Name, ""); logErr == nil && logs != "" {
		result.Stdout = filterStdoutLines(
			truncateLogLines(logs, defaults.ValidatorMaxStdoutLines),
			defaults.ValidatorMaxStdoutLineLength,
		)
	}

	// Try to get container status
	if len(jobPod.Status.ContainerStatuses) > 0 {
		cs := jobPod.Status.ContainerStatuses[0]
		if cs.State.Terminated != nil {
			result.ExitCode = cs.State.Terminated.ExitCode
			result.TerminationMsg = cs.State.Terminated.Message
			result.StartTime = cs.State.Terminated.StartedAt.Time
			result.CompletionTime = cs.State.Terminated.FinishedAt.Time
			result.Duration = result.CompletionTime.Sub(result.StartTime)
		} else {
			result.ExitCode = -1
			result.TerminationMsg = fmt.Sprintf("timeout: validator did not complete within %s", d.entry.Timeout)
		}
	} else {
		result.ExitCode = -1
		result.TerminationMsg = fmt.Sprintf("timeout: validator did not complete within %s", d.entry.Timeout)
	}

	return result
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

// getPodForJob finds the pod created by the validator Job using the shared
// pod.GetPodForJob helper. Kept as a thin wrapper so existing call sites
// inside this file remain readable.
func (d *Deployer) getPodForJob(ctx context.Context) (*corev1.Pod, error) {
	return pod.GetPodForJob(ctx, d.clientset, d.namespace, d.jobName)
}
