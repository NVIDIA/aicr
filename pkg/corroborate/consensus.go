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

import "github.com/NVIDIA/aicr/pkg/validator/ctrf"

// State is a corroboration cell state. Its ordering under PhaseRollup is given
// by phasePriority (worst-first), not the declaration order here.
type State string

const (
	// StateConfirmed means >= 2 distinct allowlisted signers passed the row
	// and none failed it — the strongest positive signal.
	StateConfirmed State = "CONFIRMED"

	// StateSingle means exactly one allowlisted signer ran the row and it
	// passed: reported, not yet corroborated.
	StateSingle State = "SINGLE"

	// StateContested means allowlisted signers disagree (>= 1 pass and
	// >= 1 fail). First-class and surfaced, never averaged away.
	StateContested State = "CONTESTED"

	// StateFailing means every allowlisted signer that ran the row failed it.
	StateFailing State = "FAILING"

	// StateUntested means no allowlisted signer ran the row — a coverage gap,
	// distinct from FAILING.
	StateUntested State = "UNTESTED"
)

// Result is a single signer's bucketed outcome for one row, after mapping the
// raw CTRF status through BucketStatus.
type Result string

const (
	// ResultPass is a passing run (CTRF "passed").
	ResultPass Result = "pass"

	// ResultFail is a failing run (CTRF "failed" or "other").
	ResultFail Result = "fail"

	// ResultNotRun is a coverage gap for this signer+row (CTRF "skipped"/
	// "pending", or the CTRF Name absent from the signer's report). It is
	// never counted as a pass or a fail, so it can neither promote a row to
	// CONFIRMED nor suppress a CONTESTED. Its wire value ("not-run") is the one
	// the renderer's series cells use; the index.json grid omits not-run signers
	// entirely, so it only appears in series/<recipe>.json.
	ResultNotRun Result = "not-run"
)

// BucketStatus maps a raw CTRF test status to its corroboration Result bucket.
// The mapping is total: the five CTRF statuses are covered explicitly, and any
// unrecognized (malformed) status fails closed to ResultFail so a garbled
// report can never masquerade as a passing corroboration.
func BucketStatus(status string) Result {
	switch status {
	case ctrf.StatusPassed:
		return ResultPass
	case ctrf.StatusFailed, ctrf.StatusOther:
		return ResultFail
	case ctrf.StatusSkipped, ctrf.StatusPending:
		return ResultNotRun
	default:
		return ResultFail
	}
}

// SignerResult is one distinct signer's latest in-scope result for a single
// row. Callers must pre-reduce to latest-per-signer (one entry per distinct
// SignerID) before calling ComputeConsensus; the consensus is computed over the
// set's cardinality, never the raw run count.
type SignerResult struct {
	// SignerID is the distinct-signer counting key: the verified (issuer,
	// identity) pair (see signerIdentityKey), never a contributor-controlled
	// IDHash. Duplicate SignerIDs are collapsed by the defensive de-dup below
	// (the anti-sybil guarantee that one identity is one signer); callers should
	// still pre-reduce to latest-per-signer.
	SignerID string

	// Allowlisted reports whether this signer's verified identity is on the
	// in-tree allowlist. Only allowlisted signers carry corroboration weight;
	// an unallowlisted signer is a zero-weight "reported" dot.
	Allowlisted bool

	// Result is the bucketed outcome (see BucketStatus).
	Result Result
}

// Consensus is the computed verdict for a single row.
type Consensus struct {
	// State is the cell state.
	State State

	// PassAllow is the count of allowlisted signers in S_pass.
	PassAllow int

	// FailAllow is the count of allowlisted signers in S_fail.
	FailAllow int

	// Reported is the count of non-allowlisted signers that actually ran the
	// row (passed or failed). Shown on the board, never counted toward state.
	Reported int
}

// ComputeConsensus derives the cell state for one row from its distinct-signer
// results. The decision is driven entirely by the allowlisted (S_pass, S_fail)
// cardinalities; unallowlisted signers only increment Reported.
//
//	UNTESTED  no allowlisted signer ran the row
//	CONTESTED allowlisted S_pass and S_fail both non-empty
//	FAILING   allowlisted S_fail non-empty, S_pass empty
//	CONFIRMED allowlisted S_pass >= 2, S_fail empty
//	SINGLE    allowlisted S_pass == 1, S_fail empty
//
// not-run results are excluded from both buckets, so a skipped latest neither
// promotes a row to CONFIRMED nor suppresses a CONTESTED.
func ComputeConsensus(signers []SignerResult) Consensus {
	var passAllow, failAllow, reported int
	ranAny := false
	seen := make(map[string]struct{}, len(signers))
	for _, s := range signers {
		// Defensive de-dup: a duplicated SignerID would double-count a single
		// source. Keep the first occurrence; callers should pre-reduce.
		if _, dup := seen[s.SignerID]; dup {
			continue
		}
		seen[s.SignerID] = struct{}{}

		ran := s.Result == ResultPass || s.Result == ResultFail
		if !s.Allowlisted {
			if ran {
				reported++
			}
			continue
		}
		switch s.Result {
		case ResultPass:
			passAllow++
			ranAny = true
		case ResultFail:
			failAllow++
			ranAny = true
		case ResultNotRun:
			// coverage gap: no weight
		}
	}

	var state State
	switch {
	case !ranAny:
		state = StateUntested
	case passAllow >= 1 && failAllow >= 1:
		state = StateContested
	case failAllow >= 1: // passAllow == 0
		state = StateFailing
	case passAllow >= 2: // failAllow == 0
		state = StateConfirmed
	default: // passAllow == 1, failAllow == 0
		state = StateSingle
	}
	return Consensus{State: state, PassAllow: passAllow, FailAllow: failAllow, Reported: reported}
}

// phasePriority is the worst-first rollup precedence:
// CONTESTED > FAILING > UNTESTED > SINGLE > CONFIRMED. A lower index is worse
// and wins the rollup, so a single CONTESTED row is never masked by an
// otherwise-green phase.
var phasePriority = []State{StateContested, StateFailing, StateUntested, StateSingle, StateConfirmed}

// priorityOf returns the rollup index of st, or len(phasePriority) for an
// unknown state (treated as least-significant so it cannot win a rollup).
func priorityOf(st State) int {
	for i, p := range phasePriority {
		if p == st {
			return i
		}
	}
	return len(phasePriority)
}

// RollupPhase folds a set of row states into a single phase state using the
// worst-first precedence. An empty set (no rows in the phase) rolls up to
// UNTESTED.
func RollupPhase(states []State) State {
	if len(states) == 0 {
		return StateUntested
	}
	worst := states[0]
	for _, st := range states[1:] {
		if priorityOf(st) < priorityOf(worst) {
			worst = st
		}
	}
	return worst
}
