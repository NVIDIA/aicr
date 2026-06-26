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

package corroborate

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

func pass(id string, allow bool) SignerResult {
	return SignerResult{SignerID: id, Allowlisted: allow, Result: ResultPass}
}

// fail and notRun are allowlisted-signer helpers; reported (non-allowlisted)
// cases use pass(id, false) directly, which is all the reported path needs.
func fail(id string) SignerResult {
	return SignerResult{SignerID: id, Allowlisted: true, Result: ResultFail}
}
func notRun(id string) SignerResult {
	return SignerResult{SignerID: id, Allowlisted: true, Result: ResultNotRun}
}

func TestComputeConsensus(t *testing.T) {
	tests := []struct {
		name         string
		signers      []SignerResult
		wantState    State
		wantPass     int
		wantFail     int
		wantReported int
	}{
		{
			name:      "CONFIRMED: two allowlisted pass",
			signers:   []SignerResult{pass("a", true), pass("b", true)},
			wantState: StateConfirmed, wantPass: 2,
		},
		{
			name:         "CONFIRMED survives a reported pass",
			signers:      []SignerResult{pass("a", true), pass("b", true), pass("rogue", false)},
			wantState:    StateConfirmed,
			wantPass:     2,
			wantReported: 1,
		},
		{
			name:      "SINGLE: one allowlisted pass",
			signers:   []SignerResult{pass("a", true)},
			wantState: StateSingle, wantPass: 1,
		},
		{
			name:      "SINGLE: one pass plus an allowlisted not-run",
			signers:   []SignerResult{pass("a", true), notRun("b")},
			wantState: StateSingle, wantPass: 1,
		},
		{
			name:      "CONTESTED: one pass and one fail",
			signers:   []SignerResult{pass("a", true), fail("b")},
			wantState: StateContested, wantPass: 1, wantFail: 1,
		},
		{
			name:      "CONTESTED is not suppressed by a skipped latest",
			signers:   []SignerResult{pass("a", true), fail("b"), notRun("c")},
			wantState: StateContested, wantPass: 1, wantFail: 1,
		},
		{
			name:      "FAILING: single allowlisted fail",
			signers:   []SignerResult{fail("a")},
			wantState: StateFailing, wantFail: 1,
		},
		{
			name:      "FAILING: all allowlisted fail",
			signers:   []SignerResult{fail("a"), fail("b")},
			wantState: StateFailing, wantFail: 2,
		},
		{
			name:      "UNTESTED: no allowlisted signer ran",
			signers:   []SignerResult{notRun("a"), notRun("b")},
			wantState: StateUntested,
		},
		{
			name:         "UNTESTED: only a reported signer ran",
			signers:      []SignerResult{pass("rogue", false)},
			wantState:    StateUntested,
			wantReported: 1,
		},
		// Sybil resistance.
		{
			name:         "sybil: one first-party + one unknown = SINGLE, not CONFIRMED",
			signers:      []SignerResult{pass("nvidia", true), pass("unknown", false)},
			wantState:    StateSingle,
			wantPass:     1,
			wantReported: 1,
		},
		{
			name:         "sybil: two unknown community signers never reach CONFIRMED",
			signers:      []SignerResult{pass("u1", false), pass("u2", false)},
			wantState:    StateUntested,
			wantReported: 2,
		},
		{
			name:      "sybil: N runs from one signer count once (dedup) = SINGLE",
			signers:   []SignerResult{pass("a", true), pass("a", true), pass("a", true)},
			wantState: StateSingle, wantPass: 1,
		},
		{
			name:      "skipped does not promote a row to CONFIRMED",
			signers:   []SignerResult{pass("a", true), notRun("b")},
			wantState: StateSingle, wantPass: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeConsensus(tt.signers)
			if got.State != tt.wantState {
				t.Errorf("state = %q, want %q", got.State, tt.wantState)
			}
			if got.PassAllow != tt.wantPass {
				t.Errorf("passAllow = %d, want %d", got.PassAllow, tt.wantPass)
			}
			if got.FailAllow != tt.wantFail {
				t.Errorf("failAllow = %d, want %d", got.FailAllow, tt.wantFail)
			}
			if got.Reported != tt.wantReported {
				t.Errorf("reported = %d, want %d", got.Reported, tt.wantReported)
			}
		})
	}
}

func TestBucketStatus(t *testing.T) {
	tests := []struct {
		status string
		want   Result
	}{
		{ctrf.StatusPassed, ResultPass},
		{ctrf.StatusFailed, ResultFail},
		{ctrf.StatusOther, ResultFail},
		{ctrf.StatusSkipped, ResultNotRun},
		{ctrf.StatusPending, ResultNotRun},
		{"garbled-unknown-status", ResultFail}, // fail-closed
		{"", ResultFail},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			if got := BucketStatus(tt.status); got != tt.want {
				t.Errorf("BucketStatus(%q) = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestRollupPhase(t *testing.T) {
	tests := []struct {
		name   string
		states []State
		want   State
	}{
		{"empty rolls up untested", nil, StateUntested},
		{"single confirmed", []State{StateConfirmed}, StateConfirmed},
		{"untested outranks confirmed", []State{StateConfirmed, StateConfirmed, StateUntested}, StateUntested},
		{"single outranks confirmed", []State{StateConfirmed, StateSingle}, StateSingle},
		{"failing outranks untested", []State{StateUntested, StateFailing}, StateFailing},
		{"contested outranks failing", []State{StateFailing, StateContested}, StateContested},
		{"contested wins over everything", []State{StateConfirmed, StateSingle, StateUntested, StateFailing, StateContested}, StateContested},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RollupPhase(tt.states); got != tt.want {
				t.Errorf("RollupPhase(%v) = %q, want %q", tt.states, got, tt.want)
			}
		})
	}
}

func TestPriorityOfUnknownState(t *testing.T) {
	// An unrecognized state sorts least-significant so it can never win a rollup.
	if got := priorityOf(State("MYSTERY")); got != len(phasePriority) {
		t.Errorf("priorityOf(unknown) = %d, want %d", got, len(phasePriority))
	}
	if got := RollupPhase([]State{State("MYSTERY"), StateConfirmed}); got != StateConfirmed {
		t.Errorf("rollup with unknown = %q, want CONFIRMED", got)
	}
}
