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
	"strings"
	"testing"
	"time"
)

// tp returns a pointer to a time.Time wrapping t.
func tp(t time.Time) *time.Time { return &t }

func TestComputeReadyState(t *testing.T) {
	now := time.Now()
	window := 60 * time.Second
	poll := 5 * time.Minute

	pass2 := map[string]ComponentResult{
		"comp-a": {Result: ResultPass},
		"comp-b": {Result: ResultPass},
	}
	fail1 := map[string]ComponentResult{
		"comp-a": {Result: ResultPass},
		"comp-b": {Result: ResultFail, Message: "timeout"},
	}

	tests := []struct {
		name          string
		allPass       bool
		components    map[string]ComponentResult
		firstPassTime *time.Time
		wantStatus    string
		wantReason    string
		wantFPTNil    bool
		wantFPTNow    bool
		wantRequeueIn time.Duration
	}{
		{
			name:          "all-pass no prior firstPassTime: start stabilizing",
			allPass:       true,
			components:    pass2,
			firstPassTime: nil,
			wantStatus:    StatusFalse,
			wantReason:    ReasonStabilizing,
			wantFPTNow:    true,
			wantRequeueIn: min(poll, window+time.Second),
		},
		{
			name:          "all-pass within window: still stabilizing",
			allPass:       true,
			components:    pass2,
			firstPassTime: tp(now.Add(-2 * time.Second)),
			wantStatus:    StatusFalse,
			wantReason:    ReasonStabilizing,
			wantRequeueIn: min(poll, (window-2*time.Second)+time.Second),
		},
		{
			name:          "all-pass exactly at window boundary: AllPass",
			allPass:       true,
			components:    pass2,
			firstPassTime: tp(now.Add(-window)),
			wantStatus:    StatusTrue,
			wantReason:    ReasonAllPass,
			wantRequeueIn: poll,
		},
		{
			name:          "all-pass past window: AllPass",
			allPass:       true,
			components:    pass2,
			firstPassTime: tp(now.Add(-2 * time.Minute)),
			wantStatus:    StatusTrue,
			wantReason:    ReasonAllPass,
			wantRequeueIn: poll,
		},
		{
			name:          "failure resets firstPassTime",
			allPass:       false,
			components:    fail1,
			firstPassTime: tp(now.Add(-2 * time.Second)),
			wantStatus:    StatusFalse,
			wantReason:    ReasonComponentsFailing,
			wantFPTNil:    true,
			wantRequeueIn: poll,
		},
		{
			name:          "failure with no prior firstPassTime",
			allPass:       false,
			components:    fail1,
			firstPassTime: nil,
			wantStatus:    StatusFalse,
			wantReason:    ReasonComponentsFailing,
			wantFPTNil:    true,
			wantRequeueIn: poll,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rs := ComputeReadyState(now, tc.allPass, tc.components, tc.firstPassTime, window, poll)

			if rs.Status != tc.wantStatus {
				t.Errorf("Status: got %q, want %q", rs.Status, tc.wantStatus)
			}
			if rs.Reason != tc.wantReason {
				t.Errorf("Reason: got %q, want %q", rs.Reason, tc.wantReason)
			}
			if tc.wantFPTNil && rs.FirstPassTime != nil {
				t.Errorf("FirstPassTime: want nil, got %v", rs.FirstPassTime)
			}
			if tc.wantFPTNow {
				if rs.FirstPassTime == nil || !rs.FirstPassTime.Equal(now) {
					t.Errorf("FirstPassTime: want %v, got %v", now, rs.FirstPassTime)
				}
			}
			if rs.RequeueIn != tc.wantRequeueIn {
				t.Errorf("RequeueIn: got %v, want %v", rs.RequeueIn, tc.wantRequeueIn)
			}
		})
	}
}

func TestApplyDeadline(t *testing.T) {
	now := time.Now()
	maxWait := time.Hour

	tests := []struct {
		name          string
		gateStart     *time.Time
		maxWait       time.Duration
		in            ReadyState
		wantStatus    string
		wantReason    string
		wantRequeueIn time.Duration
	}{
		{
			name:          "Ready=True is never overridden",
			gateStart:     tp(now.Add(-2 * time.Hour)),
			maxWait:       maxWait,
			in:            ReadyState{Status: StatusTrue, Reason: ReasonAllPass, RequeueIn: 5 * time.Minute},
			wantStatus:    StatusTrue,
			wantReason:    ReasonAllPass,
			wantRequeueIn: 5 * time.Minute,
		},
		{
			name:          "maxWait=0 disables ceiling",
			gateStart:     tp(now.Add(-10 * time.Hour)),
			maxWait:       0,
			in:            ReadyState{Status: StatusFalse, Reason: ReasonComponentsFailing, RequeueIn: 5 * time.Minute},
			wantStatus:    StatusFalse,
			wantReason:    ReasonComponentsFailing,
			wantRequeueIn: 5 * time.Minute,
		},
		{
			name:          "negative maxWait disables ceiling",
			gateStart:     tp(now.Add(-10 * time.Hour)),
			maxWait:       -1,
			in:            ReadyState{Status: StatusFalse, Reason: ReasonComponentsFailing, RequeueIn: 5 * time.Minute},
			wantStatus:    StatusFalse,
			wantReason:    ReasonComponentsFailing,
			wantRequeueIn: 5 * time.Minute,
		},
		{
			name:          "nil gateStartTime is a no-op",
			gateStart:     nil,
			maxWait:       maxWait,
			in:            ReadyState{Status: StatusFalse, Reason: ReasonComponentsFailing, RequeueIn: 5 * time.Minute},
			wantStatus:    StatusFalse,
			wantReason:    ReasonComponentsFailing,
			wantRequeueIn: 5 * time.Minute,
		},
		{
			name:          "within budget: unchanged",
			gateStart:     tp(now.Add(-30 * time.Minute)),
			maxWait:       maxWait,
			in:            ReadyState{Status: StatusFalse, Reason: ReasonComponentsFailing, RequeueIn: 5 * time.Minute},
			wantStatus:    StatusFalse,
			wantReason:    ReasonComponentsFailing,
			wantRequeueIn: 5 * time.Minute,
		},
		{
			name:          "budget exhausted: DeadlineExceeded, no requeue",
			gateStart:     tp(now.Add(-2 * time.Hour)),
			maxWait:       maxWait,
			in:            ReadyState{Status: StatusFalse, Reason: ReasonComponentsFailing, RequeueIn: 5 * time.Minute},
			wantStatus:    StatusFalse,
			wantReason:    ReasonDeadlineExceeded,
			wantRequeueIn: 0,
		},
		{
			name:          "stabilizing past budget also expires",
			gateStart:     tp(now.Add(-90 * time.Minute)),
			maxWait:       maxWait,
			in:            ReadyState{Status: StatusFalse, Reason: ReasonStabilizing, RequeueIn: 30 * time.Second},
			wantStatus:    StatusFalse,
			wantReason:    ReasonDeadlineExceeded,
			wantRequeueIn: 0,
		},
		{
			name:          "BundleMissing past budget also expires",
			gateStart:     tp(now.Add(-2 * time.Hour)),
			maxWait:       maxWait,
			in:            ReadyState{Status: StatusFalse, Reason: "BundleMissing", RequeueIn: 5 * time.Minute},
			wantStatus:    StatusFalse,
			wantReason:    ReasonDeadlineExceeded,
			wantRequeueIn: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ApplyDeadline(now, tc.gateStart, tc.maxWait, tc.in)
			if got.Status != tc.wantStatus {
				t.Errorf("Status: got %q, want %q", got.Status, tc.wantStatus)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason: got %q, want %q", got.Reason, tc.wantReason)
			}
			if got.RequeueIn != tc.wantRequeueIn {
				t.Errorf("RequeueIn: got %v, want %v", got.RequeueIn, tc.wantRequeueIn)
			}
		})
	}
}

func TestFailingSummary(t *testing.T) {
	t.Run("deterministic across repeated calls", func(t *testing.T) {
		components := map[string]ComponentResult{
			"zzz-comp": {Result: ResultFail, Message: "timeout"},
			"aaa-comp": {Result: ResultFail},
			"mmm-comp": {Result: ResultFail, Message: "crash"},
		}
		first := FailingSummary(components)
		for i := range 50 {
			if got := FailingSummary(components); got != first {
				t.Fatalf("non-deterministic at iteration %d:\n got  %q\n want %q", i+1, got, first)
			}
		}
		if !strings.HasPrefix(first, "3 component(s) failing: aaa-comp") {
			t.Errorf("unexpected order: %q", first)
		}
	})

	t.Run("passing components excluded from message", func(t *testing.T) {
		components := map[string]ComponentResult{
			"ok":  {Result: ResultPass},
			"bad": {Result: ResultFail, Message: "crash"},
		}
		got := FailingSummary(components)
		if !strings.HasPrefix(got, "1 component(s) failing:") {
			t.Errorf("wrong count: %q", got)
		}
		if strings.Contains(got, "ok") {
			t.Errorf("passing component leaked into summary: %q", got)
		}
	})

	t.Run("long output truncated to fit condition message limit", func(t *testing.T) {
		components := map[string]ComponentResult{
			"comp": {Result: ResultFail, Message: strings.Repeat("x", 300)},
		}
		got := FailingSummary(components)
		if len(got) > 256+len("...") {
			t.Errorf("summary too long (%d chars): %q", len(got), got)
		}
	})
}
