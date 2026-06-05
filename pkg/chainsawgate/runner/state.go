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

package runner

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Condition status values. These mirror metav1.ConditionStatus strings so
// callers that surface them on a Kubernetes condition need no translation.
const (
	StatusTrue  = "True"
	StatusFalse = "False"
)

// Well-known reasons surfaced on the Ready condition.
const (
	ReasonAllPass           = "AllPass"
	ReasonStabilizing       = "Stabilizing"
	ReasonComponentsFailing = "ComponentsFailing"
	ReasonDeadlineExceeded  = "DeadlineExceeded"
)

// ReadyState is the decision produced by ComputeReadyState, optionally
// overridden by ApplyDeadline.
type ReadyState struct {
	// Status is one of StatusTrue / StatusFalse.
	Status string
	// Reason is a short machine-readable hint (see Reason* constants above).
	Reason string
	// Message is human-readable detail.
	Message string
	// FirstPassTime is the start of the current continuous-pass streak.
	// Reset to nil on any failure.
	FirstPassTime *time.Time
	// RequeueIn is the suggested wait before the caller should re-evaluate.
	// 0 means "do not requeue" (terminal).
	RequeueIn time.Duration
}

// ComputeReadyState is pure: given the current time, test results, and prior
// continuous-pass start time, return the new condition state and the suggested
// requeue interval. The caller is responsible for persisting FirstPassTime
// between calls.
func ComputeReadyState(
	now time.Time,
	allPass bool,
	components map[string]ComponentResult,
	firstPassTime *time.Time,
	stabilityWindow, pollInterval time.Duration,
) ReadyState {

	if !allPass {
		return ReadyState{
			Status:        StatusFalse,
			Reason:        ReasonComponentsFailing,
			Message:       FailingSummary(components),
			FirstPassTime: nil,
			RequeueIn:     pollInterval,
		}
	}

	fpt := firstPassTime
	if fpt == nil {
		t := now
		fpt = &t
	}

	elapsed := now.Sub(*fpt)
	if elapsed >= stabilityWindow {
		return ReadyState{
			Status:        StatusTrue,
			Reason:        ReasonAllPass,
			Message:       fmt.Sprintf("All %d component(s) passing (stable for %s)", len(components), elapsed.Round(time.Second)),
			FirstPassTime: fpt,
			RequeueIn:     pollInterval,
		}
	}

	remaining := stabilityWindow - elapsed
	return ReadyState{
		Status:        StatusFalse,
		Reason:        ReasonStabilizing,
		Message:       fmt.Sprintf("All components passing; stabilizing for %s", remaining.Round(time.Second)),
		FirstPassTime: fpt,
		RequeueIn:     min(pollInterval, remaining+time.Second),
	}
}

// ApplyDeadline overrides a non-Ready candidate state with a terminal
// DeadlineExceeded once now - gateStartTime exceeds maxWait. Inputs pass
// through unchanged when maxWait is disabled (<=0), gateStartTime is nil,
// the candidate is already Ready, or the budget is not yet exhausted.
func ApplyDeadline(now time.Time, gateStartTime *time.Time, maxWait time.Duration, in ReadyState) ReadyState {
	if in.Status == StatusTrue || maxWait <= 0 || gateStartTime == nil {
		return in
	}
	if now.Sub(*gateStartTime) < maxWait {
		return in
	}
	return ReadyState{
		Status:        StatusFalse,
		Reason:        ReasonDeadlineExceeded,
		Message:       fmt.Sprintf("Gate did not become Ready within %s", maxWait),
		FirstPassTime: in.FirstPassTime,
		RequeueIn:     0,
	}
}

// FailingSummary renders a deterministic, truncated summary of the failing
// components for use in a condition Message.
func FailingSummary(components map[string]ComponentResult) string {
	var failing []string
	for name, r := range components {
		if r.Result == ResultPass {
			continue
		}
		entry := name + ": " + r.Result
		if r.Message != "" {
			entry += " (" + truncate(r.Message, 80) + ")"
		}
		failing = append(failing, entry)
	}
	sort.Strings(failing)
	return truncate(fmt.Sprintf("%d component(s) failing: %s", len(failing), strings.Join(failing, "; ")), 256)
}

func truncate(s string, n int) string {
	return TruncHead(s, n)
}
