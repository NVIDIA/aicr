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

package upgrade

import (
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Masterminds/semver/v3"
)

// Every record in this file is synthetic. ADR-021's Testing Strategy forbids
// asserting a verdict for a real component: verdicts are validated empirically
// by KWOK and UAT, and pinning them here would churn on every registry pin
// bump and turn "keep the suite green" into pressure to weaken a record.

// trans builds a synthetic transition. Tests identify a matched record by its
// summary rather than by pointer, so every summary in a table is unique.
func trans(from, to string, v Verdict, summary string) Transition {
	return Transition{From: from, To: to, Verdict: v, Summary: summary}
}

// skipping returns t with Skips set to floors.
func skipping(t Transition, floors ...string) Transition {
	t.Skips = floors
	return t
}

// oneComponent wraps transitions into a single-component Set keyed "c", the
// component name every version-matching table below uses.
func oneComponent(trs ...Transition) Set {
	return Set{"c": &ComponentUpgrades{Component: "c", Transitions: trs}}
}

// ADR-021 Decision 2's worked pair: two blocks whose from domains overlap, so
// a jump reaching past both is the case that must resolve to blocked.
var (
	blockA = trans("<0.18.0", ">=0.18.0 <0.20.0", VerdictManual, "A")
	blockB = trans("<0.20.0", ">=0.20.0 <=0.20.5", VerdictSafe, "B")
)

func TestMatchVerdicts(t *testing.T) {
	tests := []struct {
		name      string
		set       Set
		from      string
		to        string
		wantRows  int
		verdict   Verdict
		matched   string // Transition.Summary, "" when no single record matched
		stoppedAt string
		reason    Reason
	}{
		{
			name:     "identical version emits no row",
			set:      oneComponent(blockA),
			from:     "0.17.2",
			to:       "0.17.2",
			wantRows: 0,
		},
		{
			name:     "source in from and target at the to floor",
			set:      oneComponent(blockA),
			from:     "0.17.2",
			to:       "0.18.1",
			wantRows: 1,
			verdict:  VerdictManual,
			matched:  "A",
			reason:   ReasonRecorded,
		},
		{
			name:     "target below the to floor crosses no boundary",
			set:      oneComponent(blockB),
			from:     "0.17.2",
			to:       "0.18.1",
			wantRows: 1,
			verdict:  VerdictUnknown,
			reason:   ReasonNoBoundaryCrossed,
		},
		{
			name:      "a jump spanning two blocks is blocked at the first",
			set:       oneComponent(blockA, blockB),
			from:      "0.17.2",
			to:        "0.20.1",
			wantRows:  1,
			verdict:   VerdictBlocked,
			stoppedAt: ">=0.18.0 <0.20.0",
			reason:    ReasonMultipleBoundaries,
		},
		{
			// The stopping point is the lowest to floor, not whichever
			// transition the file happens to list first.
			name:      "the stopping point ignores declaration order",
			set:       oneComponent(blockB, blockA),
			from:      "0.17.2",
			to:        "0.20.1",
			wantRows:  1,
			verdict:   VerdictBlocked,
			stoppedAt: ">=0.18.0 <0.20.0",
			reason:    ReasonMultipleBoundaries,
		},
		{
			name:     "a downgrade from above every from block has no reverse record",
			set:      oneComponent(blockA, blockB),
			from:     "0.20.1",
			to:       "0.17.2",
			wantRows: 1,
			verdict:  VerdictUnknown,
			reason:   ReasonDowngrade,
		},
		{
			// The source sits inside both from domains, so only the target
			// test keeps a forward record from supplying a verdict backwards.
			name:     "a forward record does not match in reverse",
			set:      oneComponent(blockA, blockB),
			from:     "0.17.9",
			to:       "0.16.0",
			wantRows: 1,
			verdict:  VerdictUnknown,
			reason:   ReasonDowngrade,
		},
		{
			// Rule 7 already forbids authoring this shape, and crossing is now
			// a property of the jump: a downgrade never rises past a floor, so
			// no record is crossed and none can lend a verdict backwards.
			name: "a rule-7-violating reverse record still supplies no verdict",
			set: oneComponent(
				trans(">=0.20.0 <0.21.0", ">=0.18.0 <=0.19.9", VerdictManual, "R"),
			),
			from:     "0.20.1",
			to:       "0.18.1",
			wantRows: 1,
			verdict:  VerdictUnknown,
			reason:   ReasonDowngrade,
		},
		{
			name:     "an unparseable source is unversioned",
			set:      oneComponent(blockA),
			from:     "main",
			to:       "0.18.1",
			wantRows: 1,
			verdict:  VerdictUnversioned,
			reason:   ReasonNotComparable,
		},
		{
			name:     "an unparseable target is unversioned",
			set:      oneComponent(blockA),
			from:     "0.17.2",
			to:       "release-1.4",
			wantRows: 1,
			verdict:  VerdictUnversioned,
			reason:   ReasonNotComparable,
		},
		{
			name:     "no record for the component at all",
			set:      Set{},
			from:     "0.17.2",
			to:       "0.18.1",
			wantRows: 1,
			verdict:  VerdictUnknown,
			reason:   ReasonNoRecord,
		},
		{
			name:     "a nil record is not a match",
			set:      Set{"c": nil},
			from:     "0.17.2",
			to:       "0.18.1",
			wantRows: 1,
			verdict:  VerdictUnknown,
			reason:   ReasonNoRecord,
		},
		{
			name: "prerelease bounds compare numerically",
			set: oneComponent(
				trans("<0.1.0-alpha.12", ">=0.1.0-alpha.12 <=0.1.0-alpha.12", VerdictSafe, "P"),
			),
			from:     "v0.1.0-alpha.8",
			to:       "v0.1.0-alpha.12",
			wantRows: 1,
			verdict:  VerdictSafe,
			matched:  "P",
			reason:   ReasonRecorded,
		},
		{
			// A to range with no floor would otherwise apply to every target,
			// including a downgrade, handing a safe verdict to a direction the
			// record says nothing about.
			name:     "a to range with no lower bound never applies",
			set:      oneComponent(trans("<0.18.0", "<0.20.0", VerdictSafe, "NOFLOOR")),
			from:     "0.17.2",
			to:       "0.16.0",
			wantRows: 1,
			verdict:  VerdictUnknown,
			reason:   ReasonDowngrade,
		},
		{
			name:     "an unparseable from range is skipped rather than panicking",
			set:      oneComponent(trans("^0.17", ">=0.18.0 <=0.18.9", VerdictSafe, "BADFROM")),
			from:     "0.17.2",
			to:       "0.18.1",
			wantRows: 1,
			verdict:  VerdictUnknown,
			reason:   ReasonNoBoundaryCrossed,
		},
		{
			name:     "an unparseable to range is skipped rather than panicking",
			set:      oneComponent(trans("<0.18.0", "~0.18", VerdictSafe, "BADTO")),
			from:     "0.17.2",
			to:       "0.18.1",
			wantRows: 1,
			verdict:  VerdictUnknown,
			reason:   ReasonNoBoundaryCrossed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Match(tt.set, map[string]string{"c": tt.from}, map[string]string{"c": tt.to})
			if len(got) != tt.wantRows {
				t.Fatalf("Match() returned %d rows, want %d: %+v", len(got), tt.wantRows, got)
			}
			if tt.wantRows == 0 {
				return
			}
			r := got[0]
			if r.Component != "c" || r.Change != ChangeVersion {
				t.Errorf("component/change = %q/%q, want \"c\"/%q", r.Component, r.Change, ChangeVersion)
			}
			if r.From != tt.from || r.To != tt.to {
				t.Errorf("from/to = %q/%q, want %q/%q", r.From, r.To, tt.from, tt.to)
			}
			if r.Verdict != tt.verdict {
				t.Errorf("verdict = %q, want %q", r.Verdict, tt.verdict)
			}
			switch {
			case tt.matched == "" && r.Transition != nil:
				t.Errorf("transition = %q, want none", r.Transition.Summary)
			case tt.matched != "" && r.Transition == nil:
				t.Errorf("transition = none, want %q", tt.matched)
			case tt.matched != "" && r.Transition.Summary != tt.matched:
				t.Errorf("transition = %q, want %q", r.Transition.Summary, tt.matched)
			}
			if r.StoppedAt != tt.stoppedAt {
				t.Errorf("stoppedAt = %q, want %q", r.StoppedAt, tt.stoppedAt)
			}
			if r.Reason != tt.reason {
				t.Errorf("reason = %q, want %q", r.Reason, tt.reason)
			}
			if r.Explanation == "" {
				t.Error("explanation is empty; every computed verdict states one")
			}
		})
	}
}

func TestMatchComponentSetChanges(t *testing.T) {
	replacement := Set{"b-arrives": &ComponentUpgrades{
		Component: "b-arrives",
		Replaces: &Replaces{
			Component: "a-departs",
			Verdict:   VerdictManual,
			Summary:   "a-departs is superseded by b-arrives",
		},
	}}

	t.Run("added", func(t *testing.T) {
		got := Match(Set{}, map[string]string{}, map[string]string{"c": "1.0.0"})
		if len(got) != 1 {
			t.Fatalf("Match() returned %d rows, want 1: %+v", len(got), got)
		}
		r := got[0]
		if r.Change != ChangeAdded || r.From != "" || r.To != "1.0.0" {
			t.Errorf("got %+v, want an added row with no from version", r)
		}
		if r.Verdict != "" {
			t.Errorf("verdict = %q, want empty: a set change is not a version transition", r.Verdict)
		}
		if r.FailsRun() {
			t.Error("FailsRun() = true, want false: a new component is simply installed")
		}
	})

	t.Run("removed", func(t *testing.T) {
		got := Match(Set{}, map[string]string{"c": "1.0.0"}, map[string]string{})
		if len(got) != 1 {
			t.Fatalf("Match() returned %d rows, want 1: %+v", len(got), got)
		}
		r := got[0]
		if r.Change != ChangeRemoved || r.From != "1.0.0" || r.To != "" {
			t.Errorf("got %+v, want a removed row with no to version", r)
		}
		if r.Verdict != "" {
			t.Errorf("verdict = %q, want empty: a set change is not a version transition", r.Verdict)
		}
		if r.FailsRun() {
			t.Error("FailsRun() = true, want false: the component stays installed and AICR does not uninstall it")
		}
	})

	t.Run("replaced joins both sides into one row", func(t *testing.T) {
		got := Match(replacement,
			map[string]string{"a-departs": "1.2.0"},
			map[string]string{"b-arrives": "1.3.1"})
		if len(got) != 1 {
			t.Fatalf("Match() returned %d rows, want 1: %+v", len(got), got)
		}
		r := got[0]
		if r.Component != "b-arrives" || r.Change != ChangeReplaced {
			t.Errorf("component/change = %q/%q, want \"b-arrives\"/%q", r.Component, r.Change, ChangeReplaced)
		}
		if r.ReplacedComponent != "a-departs" {
			t.Errorf("replacedComponent = %q, want \"a-departs\"", r.ReplacedComponent)
		}
		if r.From != "" {
			t.Errorf("from = %q, want empty: the outgoing side is a component, not a version", r.From)
		}
		if r.To != "1.3.1" {
			t.Errorf("to = %q, want \"1.3.1\"", r.To)
		}
		if r.Verdict != VerdictManual || r.Replaces == nil {
			t.Errorf("got verdict %q replaces %v, want manual with the replaces block attached", r.Verdict, r.Replaces)
		}
		if !r.FailsRun() {
			t.Error("FailsRun() = false, want true: a replacement is migration work, not an upgrade")
		}
	})

	t.Run("no join when the superseded component is still present", func(t *testing.T) {
		got := Match(replacement,
			map[string]string{"a-departs": "1.2.0"},
			map[string]string{"b-arrives": "1.3.1", "a-departs": "1.2.0"})
		if len(got) != 1 {
			t.Fatalf("Match() returned %d rows, want 1: %+v", len(got), got)
		}
		if got[0].Change != ChangeAdded {
			t.Errorf("change = %q, want %q: a-departs did not depart, so nothing was replaced",
				got[0].Change, ChangeAdded)
		}
	})

	t.Run("no join when the superseded component was never present", func(t *testing.T) {
		got := Match(replacement, map[string]string{}, map[string]string{"b-arrives": "1.3.1"})
		if len(got) != 1 {
			t.Fatalf("Match() returned %d rows, want 1: %+v", len(got), got)
		}
		if got[0].Change != ChangeAdded {
			t.Errorf("change = %q, want %q", got[0].Change, ChangeAdded)
		}
	})
}

func TestMatchSortsByComponentName(t *testing.T) {
	from := map[string]string{"zebra": "1.0.0", "alpha": "1.0.0", "mango": "1.0.0"}
	to := map[string]string{"zebra": "2.0.0", "alpha": "2.0.0", "mango": "2.0.0"}
	got := Match(Set{}, from, to)
	want := []string{"alpha", "mango", "zebra"}
	if len(got) != len(want) {
		t.Fatalf("Match() returned %d rows, want %d", len(got), len(want))
	}
	for i, name := range want {
		if got[i].Component != name {
			t.Errorf("row %d = %q, want %q", i, got[i].Component, name)
		}
	}
}

func TestMatchSpan(t *testing.T) {
	tests := []struct {
		name     string
		to       string // the matched record's to range
		from     string
		target   string
		wantSpan Span
		wantJump Span
	}{
		{
			// ADR-021 calls this range "eleven minors", counting the to
			// interval's own width. Span is measured from the source instead,
			// which is the figure Decision 10 wants: it is a wide `from` that
			// makes one verdict cover more ground, and 0.17.2 to 0.29.0 is
			// twelve minors of it.
			name:     "a record's claim is measured from the source, not across its to interval",
			to:       ">=0.18.0 <=0.29.0",
			from:     "0.17.2",
			target:   "0.18.1",
			wantSpan: Span{Minors: 12},
			wantJump: Span{Minors: 1},
		},
		{
			name:     "a patch-only record",
			to:       ">=0.17.3 <=0.17.5",
			from:     "0.17.2",
			target:   "0.17.4",
			wantSpan: Span{Patches: 3},
			wantJump: Span{Patches: 2},
		},
		{
			name:     "a major boundary",
			to:       ">=2.0.0 <=3.0.0",
			from:     "1.9.0",
			target:   "2.1.0",
			wantSpan: Span{Majors: 2},
			wantJump: Span{Majors: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := oneComponent(trans("<"+tt.target, tt.to, VerdictSafe, "S"))
			got := Match(set, map[string]string{"c": tt.from}, map[string]string{"c": tt.target})
			if len(got) != 1 {
				t.Fatalf("Match() returned %d rows, want 1: %+v", len(got), got)
			}
			if got[0].Transition == nil {
				t.Fatalf("no record matched; verdict = %q", got[0].Verdict)
			}
			if got[0].Span != tt.wantSpan {
				t.Errorf("span = %+v, want %+v", got[0].Span, tt.wantSpan)
			}
			if got[0].Jump != tt.wantJump {
				t.Errorf("jump = %+v, want %+v", got[0].Jump, tt.wantJump)
			}
		})
	}
}

func TestMatchBreakingBoundary(t *testing.T) {
	tests := []struct {
		name          string
		from          string
		to            string
		wantBreaking  bool
		wantDowngrade bool
	}{
		{"a minor bump below 1.0 is breaking", "0.17.2", "0.18.1", true, false},
		{"a minor bump at or above 1.0 is not", "1.2.0", "1.3.0", false, false},
		{"a major bump is breaking", "1.2.0", "2.0.0", true, false},
		{"a patch bump below 1.0 is not breaking", "0.18.1", "0.18.5", false, false},
		{"a patch bump above 1.0 is not breaking", "1.2.0", "1.2.5", false, false},
		{"a minor downgrade below 1.0 is breaking", "0.18.1", "0.17.2", true, true},
		{"a major downgrade is breaking", "2.0.0", "1.9.0", true, true},
		{"crossing 1.0 is breaking", "0.19.0", "1.0.0", true, false},
		{"prerelease to prerelease on one triple is breaking", "0.1.0-alpha.8", "0.1.0-alpha.12", true, false},
		{"prerelease to its release is breaking", "1.0.0-rc.1", "1.0.0", true, false},
		{"release to a prerelease of itself is breaking", "1.0.0", "1.0.0-rc.1", true, true},
		{"a prerelease across a patch bump is not breaking", "1.0.0-rc.1", "1.0.1-rc.1", false, false},
		{"equal prereleases across a patch bump are not breaking", "1.2.0-rc.1", "1.2.5-rc.1", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Match(Set{}, map[string]string{"c": tt.from}, map[string]string{"c": tt.to})
			if len(got) != 1 {
				t.Fatalf("Match() returned %d rows, want 1: %+v", len(got), got)
			}
			r := got[0]
			if r.Verdict != VerdictUnknown {
				t.Fatalf("verdict = %q, want %q; the table asserts the unassessed case", r.Verdict, VerdictUnknown)
			}
			if r.Breaking != tt.wantBreaking {
				t.Errorf("breaking = %v, want %v", r.Breaking, tt.wantBreaking)
			}
			// Independent of wantBreaking: every row here is unknown, and
			// unknown never passes.
			if !r.FailsRun() {
				t.Error("FailsRun() = false, want true: unknown never passes")
			}
			if r.Downgrade != tt.wantDowngrade {
				t.Errorf("downgrade = %v, want %v", r.Downgrade, tt.wantDowngrade)
			}
		})
	}
}

func TestComponentResultFailsRun(t *testing.T) {
	tests := []struct {
		name string
		res  ComponentResult
		want bool
	}{
		{"safe", ComponentResult{Change: ChangeVersion, Verdict: VerdictSafe}, false},
		{"manual", ComponentResult{Change: ChangeVersion, Verdict: VerdictManual}, true},
		{"blocked", ComponentResult{Change: ChangeVersion, Verdict: VerdictBlocked}, true},
		{
			"unversioned fails even with no boundary to classify",
			ComponentResult{Change: ChangeVersion, Verdict: VerdictUnversioned},
			true,
		},
		{
			// Breaking is descriptive now: an unassessed transition is
			// unassessed at any distance, so both shapes fail.
			"unknown within a non-breaking boundary",
			ComponentResult{Change: ChangeVersion, Verdict: VerdictUnknown},
			true,
		},
		{
			"unknown across a breaking boundary",
			ComponentResult{Change: ChangeVersion, Verdict: VerdictUnknown, Breaking: true},
			true,
		},
		{
			// 1.9.0 -> 1.8.0's shape: same major, non-zero, so Breaking is
			// false. It used to exit 0, which is a silently passing rollback.
			"a non-breaking downgrade still fails",
			ComponentResult{
				Change: ChangeVersion, Verdict: VerdictUnknown,
				Reason: ReasonDowngrade, Downgrade: true,
			},
			true,
		},
		{"added", ComponentResult{Change: ChangeAdded}, false},
		{"removed", ComponentResult{Change: ChangeRemoved}, false},
		{"replaced carries the authored verdict", ComponentResult{Change: ChangeReplaced, Verdict: VerdictManual}, true},
		{"a safe replacement does not stop the run", ComponentResult{Change: ChangeReplaced, Verdict: VerdictSafe}, false},
		{
			"an unrecognized verdict fails closed",
			ComponentResult{Change: ChangeVersion, Verdict: Verdict("dubious")},
			true,
		},
		{
			"a relocation stops the run",
			ComponentResult{Change: ChangeIdentity, Verdict: VerdictUnknown, IdentityChanges: nsMove},
			true,
		},
		{
			// The matcher withdraws this verdict, so the pairing only reaches
			// FailsRun on a hand-built result. It fails there too, so the
			// guarantee does not rest on the matcher remembering.
			"a safe verdict paired with a relocation fails anyway",
			ComponentResult{Change: ChangeVersion, Verdict: VerdictSafe, IdentityChanges: nsMove},
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.res.FailsRun(); got != tt.want {
				t.Errorf("FailsRun() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchReplacementIsConsumedOnce(t *testing.T) {
	supersedes := func(name string) *ComponentUpgrades {
		return &ComponentUpgrades{
			Component: name,
			Replaces:  &Replaces{Component: "a-departs", Verdict: VerdictManual, Summary: "supersedes a-departs"},
		}
	}
	set := Set{"b-arrives": supersedes("b-arrives"), "c-arrives": supersedes("c-arrives")}
	got := Match(set,
		map[string]string{"a-departs": "1.2.0"},
		map[string]string{"b-arrives": "1.3.1", "c-arrives": "1.0.0"})
	if len(got) != 2 {
		t.Fatalf("Match() returned %d rows, want 2: %+v", len(got), got)
	}
	if got[0].Component != "b-arrives" || got[0].Change != ChangeReplaced {
		t.Errorf("row 0 = %q/%q, want the first claimant to join", got[0].Component, got[0].Change)
	}
	if got[1].Component != "c-arrives" || got[1].Change != ChangeAdded {
		t.Errorf("row 1 = %q/%q, want the second claimant to report as added",
			got[1].Component, got[1].Change)
	}
}

func TestMatchSpanIsZeroWithoutACeiling(t *testing.T) {
	// Rule 2 rejects an unbounded `to`, so this shape only reaches Match on a
	// Set built without Validate. The verdict still stands; only the width the
	// report would quote is unavailable.
	set := oneComponent(trans("<0.18.0", ">=0.18.0", VerdictSafe, "OPEN"))
	got := Match(set, map[string]string{"c": "0.17.2"}, map[string]string{"c": "0.18.1"})
	if len(got) != 1 {
		t.Fatalf("Match() returned %d rows, want 1: %+v", len(got), got)
	}
	if got[0].Verdict != VerdictSafe {
		t.Errorf("verdict = %q, want %q", got[0].Verdict, VerdictSafe)
	}
	if (got[0].Span != Span{}) {
		t.Errorf("span = %+v, want zero", got[0].Span)
	}
}

func TestLevelDiffClampsRatherThanWrapping(t *testing.T) {
	// Semver levels are uint64; an unclamped conversion turns the widest
	// difference into a negative count.
	if got := levelDiff(math.MaxUint64, 0); got != math.MaxInt {
		t.Errorf("levelDiff(MaxUint64, 0) = %d, want %d", got, math.MaxInt)
	}
	if got := levelDiff(0, math.MaxUint64); got != math.MaxInt {
		t.Errorf("levelDiff(0, MaxUint64) = %d, want %d", got, math.MaxInt)
	}
}

// TestMatchCrossingIgnoresFromMembership checks that a jump over a blocked
// record is blocked however its `from` is written, whether or not the safe
// record's `to` reaches the target. When the blocked record's `from` holds the
// source, that record describes the jump and its steps render.
func TestMatchCrossingIgnoresFromMembership(t *testing.T) {
	tests := []struct {
		name        string
		set         Set
		wantReason  Reason
		wantMatched string
	}{
		{
			name: "from ranges bounded below",
			set: oneComponent(
				trans(">=1.0.0 <2.0.0", ">=2.0.0 <2.1.0", VerdictSafe, "S"),
				trans(">=2.0.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictBlocked, "B"),
			),
			wantReason: ReasonRecordBlocks,
		},
		{
			name: "from ranges open below",
			set: oneComponent(
				trans("<2.0.0", ">=2.0.0 <2.1.0", VerdictSafe, "S"),
				trans("<3.0.0", ">=3.0.0 <=3.0.0", VerdictBlocked, "B"),
			),
			wantReason:  ReasonRecorded,
			wantMatched: "B",
		},
		{
			name: "from ranges bounded below, safe to holding the target",
			set: oneComponent(
				trans(">=1.0.0 <2.0.0", ">=2.0.0 <=3.0.0", VerdictSafe, "S"),
				trans(">=2.0.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictBlocked, "B"),
			),
			wantReason: ReasonRecordBlocks,
		},
		{
			name: "from ranges open below, safe to holding the target",
			set: oneComponent(
				trans("<2.0.0", ">=2.0.0 <=3.0.0", VerdictSafe, "S"),
				trans("<3.0.0", ">=3.0.0 <=3.0.0", VerdictBlocked, "B"),
			),
			wantReason:  ReasonRecorded,
			wantMatched: "B",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Match(tt.set, map[string]string{"c": "1.5.0"}, map[string]string{"c": "3.0.0"})
			if len(got) != 1 {
				t.Fatalf("Match() returned %d rows, want 1: %+v", len(got), got)
			}
			r := got[0]
			if r.Verdict != VerdictBlocked {
				t.Errorf("verdict = %q, want %q: the jump flies over a recorded block", r.Verdict, VerdictBlocked)
			}
			if r.Reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", r.Reason, tt.wantReason)
			}
			if r.StoppedAt != ">=3.0.0 <=3.0.0" {
				t.Errorf("stoppedAt = %q, want the blocked record's to range", r.StoppedAt)
			}
			matched := ""
			if r.Transition != nil {
				matched = r.Transition.Summary
			}
			if matched != tt.wantMatched {
				t.Errorf("matched record = %q, want %q", matched, tt.wantMatched)
			}
			if !r.FailsRun() {
				t.Error("FailsRun() = false, want true")
			}
		})
	}
}

// A safe boundary carries no steps, so crossing one composes nothing and skips
// nothing. It must not stop a jump whose only substantive boundary describes
// the whole move -- the nodewright shape, where a manual rename is followed by
// a safe release and a jump spanning both should land on the rename's steps.
func TestMatchSafeBoundaryDoesNotBlockComposition(t *testing.T) {
	rename := trans("<0.18.0", ">=0.18.0 <=0.19.0", VerdictManual, "rename")
	renameSkipsDrain := skipping(rename, "0.19.0")
	renameShort := trans("<0.18.0", ">=0.18.0 <0.19.0", VerdictManual, "rename-short")
	drain := trans(">=0.18.0 <0.19.0", ">=0.19.0 <=0.19.0", VerdictSafe, "drain")
	secondManual := trans(">=0.18.0 <0.19.0", ">=0.19.0 <=0.19.0", VerdictManual, "second")
	firstSafe := trans("<0.18.0", ">=0.18.0 <=0.18.0", VerdictSafe, "first-safe")
	boundedA := trans(">=1.0.0 <1.1.0", ">=1.1.0 <=1.1.0", VerdictSafe, "bounded-A")
	boundedB := trans(">=1.1.0 <1.2.0", ">=1.2.0 <=1.2.0", VerdictSafe, "bounded-B")
	boundedManualB := trans(">=1.1.0 <1.2.0", ">=1.2.0 <=1.2.0", VerdictManual, "bounded-manual-B")
	openA := trans("<1.1.0", ">=1.1.0 <=1.1.0", VerdictSafe, "open-A")
	openB := trans("<1.2.0", ">=1.2.0 <=1.2.0", VerdictSafe, "open-B")

	tests := []struct {
		name        string
		set         Set
		from, to    string
		wantVerdict Verdict
		wantReason  Reason
		wantMatched string
	}{
		{
			// Crosses both boundaries. The rename lists the safe one, so its
			// own verdict and steps carry the jump.
			name:        "manual plus a listed safe boundary defers to the manual record",
			set:         oneComponent(renameSkipsDrain, drain),
			from:        "0.16.0",
			to:          "0.19.0",
			wantVerdict: VerdictManual,
			wantReason:  ReasonRecorded,
			wantMatched: "rename",
		},
		{
			// The rename's `to` holds the drain's floor without listing it,
			// so it describes no move that far and the route stops between.
			name:        "manual plus an unlisted safe boundary inside its to stops between them",
			set:         oneComponent(rename, drain),
			from:        "0.16.0",
			to:          "0.19.0",
			wantVerdict: VerdictBlocked,
			wantReason:  ReasonMultipleBoundaries,
		},
		{
			// Crosses only the safe boundary.
			name:        "the safe boundary alone still reads safe",
			set:         oneComponent(rename, drain),
			from:        "0.18.0",
			to:          "0.19.0",
			wantVerdict: VerdictSafe,
			wantReason:  ReasonRecorded,
			wantMatched: "drain",
		},
		{
			// N safe boundaries compose exactly as one does. Blocking here
			// would tell an operator to stop at a version where nothing
			// happens, which is the outcome this reduction exists to prevent.
			name:        "a crossing that is entirely safe reads safe",
			set:         oneComponent(firstSafe, drain),
			from:        "0.17.0",
			to:          "0.19.0",
			wantVerdict: VerdictSafe,
			wantReason:  ReasonRecorded,
			wantMatched: "drain",
		},
		{
			// An origin no record assessed stays refused however many
			// boundaries the jump crosses. Before the fromCovers guard this
			// came back safe on two crossings while the identical origin was
			// refused on one, so asking to go further bought a verdict that
			// vouches. No record covers 0.9.0, so the undefined-origin check
			// refuses it before that guard is reached; the two cases after the
			// next one reach the guards.
			name:        "an unassessed origin is not rescued by crossing more boundaries",
			set:         oneComponent(boundedA, boundedB),
			from:        "0.9.0",
			to:          "1.2.0",
			wantVerdict: VerdictBlocked,
			wantReason:  ReasonUndefinedOrigin,
		},
		{
			// The same origin across a single boundary, for the comparison the
			// case above exists to hold: one crossing already refused it.
			name:        "an unassessed origin across one boundary is refused",
			set:         oneComponent(boundedA, boundedB),
			from:        "0.9.0",
			to:          "1.1.0",
			wantVerdict: VerdictBlocked,
			wantReason:  ReasonUndefinedOrigin,
		},
		{
			// bounded-A is off the path, written for 1.0.x.
			name:        "a record covering the whole jump is honored over a lower one written for another origin",
			set:         oneComponent(boundedA, openB),
			from:        "0.9.0",
			to:          "1.2.0",
			wantVerdict: VerdictSafe,
			wantReason:  ReasonRecorded,
			wantMatched: "open-B",
		},
		{
			// The lone-substantive path's counterpart: open-A covers 0.9.0,
			// but the one boundary that asks something was written for 1.1.x,
			// so its steps do not describe this jump.
			name:        "a lone substantive boundary written for another origin is not lent",
			set:         oneComponent(openA, boundedManualB),
			from:        "0.9.0",
			to:          "1.2.0",
			wantVerdict: VerdictBlocked,
			wantReason:  ReasonMultipleBoundaries,
		},
		{
			// rename's `to` holds the target, so the walk never stands in
			// second's `from`, but the jump still passes through it.
			name:        "a manual record the jump flies over is a skipped boundary",
			set:         oneComponent(rename, secondManual),
			from:        "0.16.0",
			to:          "0.19.0",
			wantVerdict: VerdictBlocked,
			wantReason:  ReasonMultipleBoundaries,
		},
		{
			name: "a safe record covering the whole jump does not hide a manual one it flies over",
			set: oneComponent(
				trans("<0.18.0", ">=0.18.0 <=0.19.0", VerdictSafe, "wide-safe"),
				secondManual,
			),
			from:        "0.16.0",
			to:          "0.19.0",
			wantVerdict: VerdictBlocked,
			wantReason:  ReasonMultipleBoundaries,
		},
		{
			// renameShort lands on 0.18.0, where second was written for.
			name:        "two substantive boundaries on the path still block",
			set:         oneComponent(renameShort, secondManual),
			from:        "0.16.0",
			to:          "0.19.0",
			wantVerdict: VerdictBlocked,
			wantReason:  ReasonMultipleBoundaries,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchVersions(tt.set["c"], "c", tt.from, tt.to)
			if got.Verdict != tt.wantVerdict {
				t.Errorf("verdict = %q, want %q", got.Verdict, tt.wantVerdict)
			}
			if got.Reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tt.wantReason)
			}
			matched := ""
			if got.Transition != nil {
				matched = got.Transition.Summary
			}
			if matched != tt.wantMatched {
				t.Errorf("matched record = %q, want %q", matched, tt.wantMatched)
			}
		})
	}
}

// TestMatchVerdictSelection covers the branches of the selection order,
// including the invariants a blocked result must hold: no Transition to render
// another record's steps from, and a StoppedAt the report can name.
func TestMatchVerdictSelection(t *testing.T) {
	safeLow := trans("<2.0.0", ">=2.0.0 <2.1.0", VerdictSafe, "S")
	blockedHigh := trans("<3.0.0", ">=3.0.0 <=3.0.0", VerdictBlocked, "B")
	blockedMid := trans("<2.5.0", ">=2.5.0 <2.6.0", VerdictBlocked, "M")
	boundedOrigin := trans(">=2.0.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictManual, "O")
	boundedManual := trans(">=2.0.0 <3.0.0", ">=3.0.0 <3.1.0", VerdictManual, "BM")
	boundedNextManual := trans(">=3.0.0 <4.0.0", ">=4.0.0 <=4.0.0", VerdictManual, "BN")
	boundedNextBlocked := trans(">=3.0.0 <4.0.0", ">=4.0.0 <=4.0.0", VerdictBlocked, "BB")
	far := trans(">=1.0.0 <2.0.0", ">=5.0.0 <=5.0.0", VerdictManual, "far")
	blockedOrigin := trans(">=2.0.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictBlocked, "BO")

	tests := []struct {
		name          string
		set           Set
		from, to      string
		wantVerdict   Verdict
		wantReason    Reason
		wantStoppedAt string
		wantMatched   string
	}{
		{
			// The block's `from` holds the source and passes only a safe
			// floor, so the block describes the jump.
			name:          "an authored block outranks a lower non-blocking boundary",
			set:           oneComponent(safeLow, blockedHigh),
			from:          "1.5.0",
			to:            "3.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonRecorded,
			wantStoppedAt: ">=3.0.0 <=3.0.0",
			wantMatched:   "B",
		},
		{
			name: "a block reached past a lower non-blocking boundary is a record block",
			set: oneComponent(
				safeLow,
				trans(">=2.0.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictBlocked, "B"),
			),
			from:          "1.5.0",
			to:            "3.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonRecordBlocks,
			wantStoppedAt: ">=3.0.0 <=3.0.0",
		},
		{
			// One record, authored for this starting point, describing this
			// exact move. Its blocked verdict is that author saying "not in one
			// step", and the result carries the record so its steps can render.
			name:          "a lone blocked record covering the source keeps its record",
			set:           oneComponent(blockedHigh),
			from:          "1.5.0",
			to:            "3.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonRecorded,
			wantStoppedAt: ">=3.0.0 <=3.0.0",
			wantMatched:   "B",
		},
		{
			// Declaration order is reversed and the lower block is listed last,
			// so only floor ordering can pick it.
			name:          "the lowest-floor block wins when several are crossed",
			set:           oneComponent(blockedHigh, safeLow, blockedMid),
			from:          "1.5.0",
			to:            "3.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonRecordBlocks,
			wantStoppedAt: ">=2.5.0 <2.6.0",
		},
		{
			// Reversed in #2829: this used to expect blocked /
			// multiple-boundaries. That rule exists because an intermediate
			// record's steps never run on a jump straight past it, and a safe
			// record has no steps by construction, so crossing two of them
			// skips nothing. Blocking named 2.0.0 as somewhere to stop when
			// landing there asks nothing of anyone, which is the
			// false-confidence direction rather than the cautious one. The
			// furthest crossed record still has to reach the target, which is
			// what keeps this from vouching past anyone's assessment.
			name:        "two non-blocking boundaries do not block the jump",
			set:         oneComponent(safeLow, trans("<2.5.0", ">=2.5.0 <2.6.0", VerdictSafe, "S2")),
			from:        "1.5.0",
			to:          "2.5.1",
			wantVerdict: VerdictSafe,
			wantReason:  ReasonRecorded,
			wantMatched: "S2",
		},
		{
			name:        "one crossed record whose from covers the source lends its verdict",
			set:         oneComponent(safeLow),
			from:        "1.5.0",
			to:          "2.0.5",
			wantVerdict: VerdictSafe,
			wantReason:  ReasonRecorded,
			wantMatched: "S",
		},
		{
			name:          "one crossed record whose from excludes the source blocks",
			set:           oneComponent(boundedOrigin),
			from:          "1.5.0",
			to:            "3.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonUndefinedOrigin,
			wantStoppedAt: ">=3.0.0 <=3.0.0",
		},
		{
			// Naming the lowest floor as the place to stop would send the
			// operator on a first hop, 1.5.0 to 3.0.0, that no record
			// describes either: checked on its own, it is undefined-origin.
			name:          "several crossed records none of which covers the source",
			set:           oneComponent(boundedManual, boundedNextManual),
			from:          "1.5.0",
			to:            "4.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonUndefinedOrigin,
			wantStoppedAt: ">=3.0.0 <3.1.0",
		},
		{
			// The block's floor is the move it forbids, so offering it as the
			// stop from an origin no record reaches would be the worst advice
			// available.
			name:          "an authored block does not outrank a source no record covers",
			set:           oneComponent(boundedManual, boundedNextBlocked),
			from:          "1.5.0",
			to:            "4.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonUndefinedOrigin,
			wantStoppedAt: ">=3.0.0 <3.1.0",
		},
		{
			// The previous case in its simplest form: the block's own floor is
			// the move it forbids, and nothing describes a hop from 1.5.0.
			name:          "a lone crossed block does not outrank a source no record covers",
			set:           oneComponent(blockedOrigin),
			from:          "1.5.0",
			to:            "3.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonUndefinedOrigin,
			wantStoppedAt: ">=3.0.0 <=3.0.0",
		},
		{
			// far covers 1.5.0 but is not crossed, so the path is empty.
			name:          "a lone crossed block written for another origin is undefined-origin beside a record outside the jump",
			set:           oneComponent(far, blockedOrigin),
			from:          "1.5.0",
			to:            "3.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonUndefinedOrigin,
			wantStoppedAt: ">=3.0.0 <=3.0.0",
		},
		{
			name:          "several crossed records written for other origins are undefined-origin beside a record outside the jump",
			set:           oneComponent(far, boundedManual, boundedNextManual),
			from:          "1.5.0",
			to:            "4.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonUndefinedOrigin,
			wantStoppedAt: ">=3.0.0 <3.1.0",
		},
		{
			name:          "a crossed block written for another origin is undefined-origin beside a record outside the jump",
			set:           oneComponent(far, boundedManual, boundedNextBlocked),
			from:          "1.5.0",
			to:            "4.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonUndefinedOrigin,
			wantStoppedAt: ">=3.0.0 <3.1.0",
		},
		{
			// The block is the first hop.
			name: "a block taken as the first hop names its own to",
			set: oneComponent(
				trans("<3.0.0", ">=3.0.0 <3.1.0", VerdictBlocked, "B"),
				trans(">=3.0.0 <3.5.0", ">=3.5.0 <=3.5.0", VerdictSafe, "X"),
			),
			from:          "1.5.0",
			to:            "3.5.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonRecordBlocks,
			wantStoppedAt: ">=3.0.0 <3.1.0",
		},
		{
			// Two safe hops reach 2.5.x, and nothing describes going on.
			name: "a path that stops short of the target is blocked at its last ceiling",
			set: oneComponent(
				safeLow,
				trans(">=2.0.0 <2.5.0", ">=2.5.0 <2.6.0", VerdictSafe, "S2"),
			),
			from:          "1.5.0",
			to:            "2.7.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonBeyondRecordCeiling,
			wantStoppedAt: ">=2.5.0 <2.6.0",
		},
		{
			name:          "a lone blocked record below the target is beyond its ceiling",
			set:           oneComponent(trans("<3.0.0", ">=3.0.0 <3.1.0", VerdictBlocked, "B")),
			from:          "1.5.0",
			to:            "5.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonBeyondRecordCeiling,
			wantStoppedAt: ">=3.0.0 <3.1.0",
		},
		{
			// The walk takes S1 and stalls at 2.0.0, but S2 covers the source
			// and holds the target.
			name: "an all-safe path honors a safe record covering the whole jump",
			set: oneComponent(
				trans("<2.0.0", ">=2.0.0 <2.5.0", VerdictSafe, "S1"),
				trans(">=1.0.0 <2.0.0", ">=2.8.0 <=3.0.0", VerdictSafe, "S2"),
				trans(">=2.0.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictManual, "S3"),
			),
			from:        "1.5.0",
			to:          "2.9.0",
			wantVerdict: VerdictSafe,
			wantReason:  ReasonRecorded,
			wantMatched: "S2",
		},
		{
			// The walk lands on 2.0.0. The next record starts at 2.2.0.
			name: "a path that lands where no record continues is undefined-origin",
			set: oneComponent(
				safeLow,
				trans(">=2.2.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictManual, "G"),
			),
			from:          "1.5.0",
			to:            "3.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonUndefinedOrigin,
			wantStoppedAt: ">=3.0.0 <=3.0.0",
		},
		{
			// Landing on >2.0.0 stands just above it, which N's `from` admits.
			name: "a walk continues from an exclusive to floor",
			set: oneComponent(
				trans("<2.0.0", ">2.0.0 <2.1.0", VerdictSafe, "S"),
				trans(">2.0.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictManual, "N"),
			),
			from:          "1.5.0",
			to:            "3.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonMultipleBoundaries,
			wantStoppedAt: ">2.0.0 <2.1.0",
		},
		{
			// A record covers the source but is not in the way, so the one
			// record that is was still written for a different starting point.
			name:          "one crossed record whose from excludes a source another record covers",
			set:           oneComponent(far, boundedOrigin),
			from:          "1.5.0",
			to:            "3.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonUndefinedOrigin,
			wantStoppedAt: ">=3.0.0 <=3.0.0",
		},
		{
			name:        "nothing crossed and a record exists",
			set:         oneComponent(safeLow),
			from:        "1.0.0",
			to:          "1.0.1",
			wantVerdict: VerdictUnknown,
			wantReason:  ReasonNoBoundaryCrossed,
		},
		{
			name:        "nothing crossed and no record exists",
			set:         Set{},
			from:        "1.0.0",
			to:          "1.0.1",
			wantVerdict: VerdictUnknown,
			wantReason:  ReasonNoRecord,
		},
		{
			name:        "a downgrade is never lent a forward verdict",
			set:         oneComponent(safeLow, blockedHigh),
			from:        "3.0.0",
			to:          "1.5.0",
			wantVerdict: VerdictUnknown,
			wantReason:  ReasonDowngrade,
		},
		{
			name:        "an incomparable side is a gap in the inputs",
			set:         oneComponent(safeLow),
			from:        "main",
			to:          "1.2.3",
			wantVerdict: VerdictUnversioned,
			wantReason:  ReasonNotComparable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Match(tt.set, map[string]string{"c": tt.from}, map[string]string{"c": tt.to})
			if len(got) != 1 {
				t.Fatalf("Match() returned %d rows, want 1: %+v", len(got), got)
			}
			r := got[0]
			if r.Verdict != tt.wantVerdict {
				t.Errorf("verdict = %q, want %q", r.Verdict, tt.wantVerdict)
			}
			if r.Reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", r.Reason, tt.wantReason)
			}
			if r.StoppedAt != tt.wantStoppedAt {
				t.Errorf("stoppedAt = %q, want %q", r.StoppedAt, tt.wantStoppedAt)
			}
			if r.Verdict == VerdictBlocked {
				if r.StoppedAt == "" {
					t.Error("stoppedAt is empty on a blocked result; the report would print a hole")
				}
				if r.Reason != ReasonRecorded && r.Transition != nil {
					t.Errorf("transition = %q on a blocked result no single record describes; "+
						"its steps are for a different jump", r.Transition.Summary)
				}
			}
			switch {
			case tt.wantMatched == "" && r.Transition != nil:
				t.Errorf("transition = %q, want none", r.Transition.Summary)
			case tt.wantMatched != "" && r.Transition == nil:
				t.Errorf("transition = none, want %q", tt.wantMatched)
			case tt.wantMatched != "" && r.Transition.Summary != tt.wantMatched:
				t.Errorf("transition = %q, want %q", r.Transition.Summary, tt.wantMatched)
			}
			// Wording is pinned by TestMatchExplanationsAreConcrete; here the
			// point is only that no branch leaves the sentence unset.
			if r.Explanation == "" {
				t.Error("explanation is empty; every computed verdict states one")
			}
		})
	}
}

// TestMatchExplanationsAreConcrete pins the operator-facing sentence for each
// reason, because a code alone says what happened rather than what to do.
func TestMatchExplanationsAreConcrete(t *testing.T) {
	tests := []struct {
		name     string
		set      Set
		from, to string
		want     string
	}{
		{
			name: "record-blocks",
			set: oneComponent(
				trans("<2.0.0", ">=2.0.0 <2.1.0", VerdictSafe, "S"),
				trans(">=2.0.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictBlocked, "B"),
			),
			from: "1.5.0",
			to:   "3.0.0",
			want: "blocked by the record covering 3.0.0: do not move from 1.5.0 into >=3.0.0 <=3.0.0 " +
				"in one step. Upgrade to 2.0.0 first, then re-run this check",
		},
		{
			name: "record-blocks with no hop before the block",
			set: oneComponent(
				trans("<3.0.0", ">=3.0.0 <3.1.0", VerdictBlocked, "B"),
				trans(">=3.0.0 <3.5.0", ">=3.5.0 <=3.5.0", VerdictSafe, "X"),
			),
			from: "1.5.0",
			to:   "3.5.0",
			want: "blocked by the record covering 3.0.0: do not move from 1.5.0 into >=3.0.0 <3.1.0 " +
				"in one step. Check the move from 1.5.0 to 3.0.0 on its own for that record's steps, " +
				"then re-run this check",
		},
		{
			name: "record-blocks with no hop before a block on an exclusive floor",
			set: oneComponent(
				trans("<3.0.0", ">3.0.0 <3.1.0", VerdictBlocked, "B"),
				trans(">3.0.0 <3.5.0", ">=3.5.0 <=3.5.0", VerdictSafe, "X"),
			),
			from: "1.5.0",
			to:   "3.5.0",
			want: "blocked by the record covering 3.0.0: do not move from 1.5.0 into >3.0.0 <3.1.0 " +
				"in one step. Check the move from 1.5.0 to a version in >3.0.0 <3.1.0 on its own for " +
				"that record's steps, then re-run this check",
		},
		{
			name: "undefined-origin names a block the source's record passes without listing it",
			set: oneComponent(
				trans("<2.0.0", ">=2.0.0 <=3.0.0", VerdictSafe, "S"),
				trans(">=1.8.0 <1.9.0", ">=1.9.0 <=1.9.0", VerdictBlocked, "B"),
			),
			from: "1.5.0",
			to:   "2.5.0",
			want: "crosses 2 recorded boundaries (1.9.0, 2.0.0), but the record covering 2.0.0 whose from " +
				"holds 1.5.0 passes the boundary at 1.9.0 described for >=1.8.0 <1.9.0 without listing it " +
				"in skips, so it describes no move that far. Author a record for this move, or list that " +
				"boundary in skips if a cluster may pass it, then re-run this check",
		},
		{
			name: "multiple-boundaries landing on an exclusive floor names the range",
			set: oneComponent(
				trans("<2.0.0", ">2.0.0 <2.1.0", VerdictSafe, "S"),
				trans(">2.0.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictManual, "N"),
			),
			from: "1.5.0",
			to:   "3.0.0",
			want: "crosses 2 recorded boundaries (2.0.0, 3.0.0); no single record describes the whole jump. " +
				"Upgrade to a version in >2.0.0 <2.1.0 first, then re-run this check. The whole route " +
				"takes 2 upgrades: to >2.0.0 <2.1.0 (safe), then to 3.0.0 (manual)",
		},
		{
			name: "undefined-origin where the source's records lead past the target",
			set: oneComponent(
				trans(">=1.0.0 <2.0.0", ">=4.0.0 <=4.0.0", VerdictManual, "C"),
				trans(">=2.0.0 <3.0.0", ">=3.0.0 <3.1.0", VerdictManual, "B"),
			),
			from: "1.5.0",
			to:   "3.0.0",
			want: "crosses the boundary at 3.0.0, but the records written for 1.5.0 lead only into " +
				">=4.0.0 <=4.0.0, so none describes this move. Author a record for this move, then " +
				"re-run this check",
		},
		{
			name: "beyond-record-ceiling on a path that stalls below the component's highest ceiling",
			set: oneComponent(
				trans("<2.0.0", ">=2.0.0 <2.1.0", VerdictSafe, "S1"),
				trans(">=2.0.0 <2.5.0", ">=2.5.0 <2.6.0", VerdictSafe, "S2"),
				trans(">=2.5.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictSafe, "S3"),
			),
			from: "1.5.0",
			to:   "2.7.0",
			want: "the records along the upgrade path from 1.5.0 assess only as far as the last version " +
				"below 2.6.0, and 2.7.0 lands past that, so nothing assesses this move. Upgrade no further " +
				"than the last version below 2.6.0 and re-run this check, or widen a record's `to` range " +
				"to cover 2.7.0",
		},
		{
			name: "undefined-origin names a shared floor once",
			set: oneComponent(
				trans(">=2.0.0 <2.5.0", ">=3.0.0 <=3.0.0", VerdictManual, "A"),
				trans(">=2.5.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictManual, "B"),
			),
			from: "1.5.0",
			to:   "3.0.0",
			want: "crosses the boundary at 3.0.0, but no record describes an upgrade starting from 1.5.0; " +
				"the earliest recorded starting point is 2.0.0. Upgrade to a recorded version first, " +
				"then re-run this check",
		},
		{
			name: "undefined-origin where the path lands below the next record's origin",
			set: oneComponent(
				trans("<2.0.0", ">=2.0.0 <2.1.0", VerdictSafe, "S"),
				trans(">=2.2.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictManual, "G"),
			),
			from: "1.5.0",
			to:   "3.0.0",
			want: "crosses the boundary at 3.0.0, but no record describes an upgrade starting from 2.0.0, " +
				"where the records written for 1.5.0 lead. Author a record for that starting point, " +
				"then re-run this check",
		},
		{
			name: "multiple-boundaries",
			set: oneComponent(
				trans("<2.0.0", ">=2.0.0 <2.1.0", VerdictManual, "S"),
				trans("<3.0.0", ">=3.0.0 <=3.0.0", VerdictManual, "T"),
			),
			from: "1.5.0",
			to:   "3.0.0",
			want: "crosses 2 recorded boundaries (2.0.0, 3.0.0); no single record describes the whole jump. " +
				"Upgrade to 2.0.0 first, then re-run this check. The whole route takes 2 upgrades: " +
				"to >=2.0.0 <2.1.0 (manual), then to 3.0.0 (manual)",
		},
		{
			name: "multiple-boundaries lands below a manual record the jump flies over",
			set: oneComponent(
				trans("<0.18.0", ">=0.18.0 <=0.19.0", VerdictSafe, "W"),
				trans(">=0.18.0 <0.19.0", ">=0.19.0 <=0.19.0", VerdictManual, "M"),
			),
			from: "0.16.0",
			to:   "0.19.0",
			want: "crosses 2 recorded boundaries (0.18.0, 0.19.0); no single record describes the whole jump. " +
				"Upgrade to 0.18.0 first, then re-run this check. The whole route takes 2 upgrades: " +
				"to >=0.18.0 <0.19.0 (safe), then to 0.19.0 (manual)",
		},
		{
			name: "multiple-boundaries names a required stop",
			set: oneComponent(
				Transition{From: "<2.0.0", To: ">=2.0.0 <2.1.0", Verdict: VerdictSafe, RequiredStop: true, Summary: "S"},
				trans(">=2.0.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictSafe, "T"),
			),
			from: "1.5.0",
			to:   "3.0.0",
			want: "crosses 2 recorded boundaries (2.0.0, 3.0.0); no single record describes the whole jump. " +
				"Upgrade to 2.0.0 first, then re-run this check. The record covering 2.0.0 is a required " +
				"stop: a version in >=2.0.0 <2.1.0 has to run before the component moves further. The " +
				"whole route takes 2 upgrades: to >=2.0.0 <2.1.0 (safe), then to 3.0.0 (safe)",
		},
		{
			name: "multiple-boundaries names the step limit",
			set: Set{"c": &ComponentUpgrades{Component: "c", StepLimit: StepLimitMinor, Transitions: []Transition{
				trans(">=1.4.0 <1.5.0", ">=1.5.0", VerdictSafe, "S"),
				trans(">=1.5.0 <1.6.0", ">=1.6.0", VerdictSafe, "T"),
			}}},
			from: "1.4.2",
			to:   "1.6.1",
			want: "crosses 2 recorded boundaries (1.5.0, 1.6.0); no single record describes the whole jump. " +
				"Upgrade to 1.5.0 first, then re-run this check. This component moves at most one minor " +
				"version per upgrade. The whole route takes 2 upgrades: to >=1.5.0 <1.6.0 (safe), then " +
				"to 1.6.1 (safe)",
		},
		{
			name: "undefined-origin names a manual boundary a covering record passes without listing it",
			set: oneComponent(
				trans(">=1.0.0 <2.0.0", ">=4.0.0 <=4.0.0", VerdictManual, "C"),
				trans(">=2.0.0 <3.0.0", ">=3.0.0 <3.1.0", VerdictManual, "B"),
			),
			from: "1.5.0",
			to:   "4.0.0",
			want: "crosses 2 recorded boundaries (3.0.0, 4.0.0), but the record covering 4.0.0 whose from " +
				"holds 1.5.0 passes the boundary at 3.0.0 described for >=2.0.0 <3.0.0 without listing it " +
				"in skips, so it describes no move that far. Author a record for this move, or list that " +
				"boundary in skips if a cluster may pass it, then re-run this check",
		},
		{
			name: "undefined-origin",
			set:  oneComponent(trans(">=2.0.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictManual, "O")),
			from: "1.5.0",
			to:   "3.0.0",
			want: "crosses the boundary at 3.0.0, but no record describes an upgrade starting from 1.5.0; " +
				"the earliest recorded starting point is 2.0.0. Upgrade to a recorded version first, " +
				"then re-run this check",
		},
		{
			name: "undefined-origin above every from ceiling",
			set:  oneComponent(trans("<2.0.0", ">=3.0.0 <=3.0.0", VerdictManual, "O")),
			from: "2.5.0",
			to:   "3.0.0",
			want: "crosses the boundary at 3.0.0, but no record describes an upgrade starting from 2.5.0, " +
				"so nothing covers this move. Author a record for this starting point, then re-run this check",
		},
		{
			name: "undefined-origin across several boundaries names the earliest recorded starting point",
			set: oneComponent(
				trans(">=2.0.0 <3.0.0", ">=3.0.0 <3.1.0", VerdictManual, "M"),
				trans(">=3.0.0 <4.0.0", ">=4.0.0 <=4.0.0", VerdictBlocked, "B"),
			),
			from: "1.5.0",
			to:   "4.0.0",
			want: "crosses 2 recorded boundaries (3.0.0, 4.0.0), but no record describes an upgrade " +
				"starting from 1.5.0; the earliest recorded starting point is 2.0.0. Upgrade to a " +
				"recorded version first, then re-run this check",
		},
		{
			name: "undefined-origin across several boundaries from a gap between from domains",
			set: oneComponent(
				trans(">=1.0.0 <2.0.0", ">=3.0.0 <3.1.0", VerdictManual, "M"),
				trans(">=3.0.0 <4.0.0", ">=4.0.0 <=4.0.0", VerdictManual, "N"),
			),
			from: "2.5.0",
			to:   "4.0.0",
			want: "crosses 2 recorded boundaries (3.0.0, 4.0.0), but no record describes an upgrade " +
				"starting from 2.5.0, so nothing covers this move. Author a record for this starting " +
				"point, then re-run this check",
		},
		{
			name: "beyond-record-ceiling",
			set:  oneComponent(trans("<0.18.0", ">=0.18.0 <0.20.0", VerdictSafe, "S")),
			from: "0.17.2",
			to:   "0.25.0",
			want: "transition records for this component assess only as far as the last version below " +
				"0.20.0, and 0.25.0 lands past that, so nothing assesses this move or anything above " +
				"the last version below 0.20.0. Upgrade no further than the last version below 0.20.0 " +
				"and re-run this check, or widen a record's `to` range to cover 0.25.0",
		},
		{
			name: "beyond-record-ceiling names an inclusive ceiling as the version itself",
			set:  oneComponent(trans("<0.18.0", ">=0.18.0 <=0.20.0", VerdictSafe, "S")),
			from: "0.17.2",
			to:   "0.25.0",
			want: "transition records for this component assess only as far as 0.20.0, and 0.25.0 lands " +
				"past that, so nothing assesses this move or anything above 0.20.0. Upgrade no further " +
				"than 0.20.0 and re-run this check, or widen a record's `to` range to cover 0.25.0",
		},
		{
			name: "beyond-record-ceiling on a lone blocked hop points at its steps",
			set:  oneComponent(trans("<3.0.0", ">=3.0.0 <3.1.0", VerdictBlocked, "B")),
			from: "1.5.0",
			to:   "5.0.0",
			want: "the record covering 3.0.0 assesses only as far as the last version below 3.1.0, and 5.0.0 " +
				"lands past that. It also blocks moving from 1.5.0 into >=3.0.0 <3.1.0 in one step. Check " +
				"the move from 1.5.0 to 3.0.0 on its own for that record's steps, then re-run this check",
		},
		{
			name: "no-record",
			set:  Set{},
			from: "1.0.0",
			to:   "1.0.1",
			want: "no transition record exists for this component, so nothing assesses the move from " +
				"1.0.0 to 1.0.1. This is not a pass: read the component's own upstream release notes " +
				"and decide, then consider authoring the first record so the next operator does not " +
				"repeat the work",
		},
		{
			name: "no-boundary-crossed",
			set:  oneComponent(trans("<2.0.0", ">=2.0.0 <2.1.0", VerdictSafe, "S")),
			from: "1.0.0",
			to:   "1.0.1",
			want: "a record exists for this component but says nothing about the range between 1.0.0 " +
				"and 1.0.1, so no author flagged this move. This is not a pass: read the component's " +
				"own upstream release notes and decide whether this range needs a boundary, then " +
				"widen the record if it does",
		},
		{
			name: "downgrade",
			set:  oneComponent(trans("<2.0.0", ">=2.0.0 <2.1.0", VerdictSafe, "S")),
			from: "2.0.0",
			to:   "1.5.0",
			want: "downgrade from 2.0.0 to 1.5.0. Transition records describe forward moves only, so " +
				"none describes this one and none ever can: this is unassessable rather than merely " +
				"unassessed, and there is no intermediate version to land on. Review the component's " +
				"own downgrade guidance before proceeding",
		},
		{
			name: "not-comparable",
			set:  Set{},
			from: "main",
			to:   "1.2.3",
			want: `version "main" is not comparable to "1.2.3"; pin a semver version on both sides`,
		},
		{
			name: "recorded safe names its evidence",
			set: oneComponent(Transition{
				From: "<2.0.0", To: ">=2.0.0 <2.1.0", Verdict: VerdictSafe,
				VerifiedBy: "uat: synthetic lane", Summary: "S",
			}),
			from: "1.5.0",
			to:   "2.0.5",
			want: `recorded safe: the record covering 2.0.0 describes the move from 1.5.0 to 2.0.5, ` +
				`verified by "uat: synthetic lane"`,
		},
		{
			name: "recorded blocked says not in one step, and points at the steps",
			set:  oneComponent(trans("<3.0.0", ">=3.0.0 <=3.0.0", VerdictBlocked, "B")),
			from: "1.5.0",
			to:   "3.0.0",
			want: "recorded blocked: the record covering 3.0.0 describes the move from 1.5.0 to 3.0.0 " +
				"and blocks it in one step. Follow the steps below to get there safely",
		},
		{
			name: "recorded manual points at the steps",
			set:  oneComponent(trans("<2.0.0", ">=2.0.0 <2.1.0", VerdictManual, "S")),
			from: "1.5.0",
			to:   "2.0.5",
			want: "recorded manual: the record covering 2.0.0 describes the move from 1.5.0 to 2.0.5. " +
				"Follow the steps below, then apply",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Match(tt.set, map[string]string{"c": tt.from}, map[string]string{"c": tt.to})
			if len(got) != 1 {
				t.Fatalf("Match() returned %d rows, want 1: %+v", len(got), got)
			}
			if got[0].Explanation != tt.want {
				t.Errorf("explanation =\n  %q\nwant\n  %q", got[0].Explanation, tt.want)
			}
		})
	}
}

// A prerelease-only bump is the shape that shipped a CRD migration: it clears
// every major/minor rule, so without the prerelease arm an unassessed one
// crosses no boundary and passes a strict run.
func TestMatchPrereleaseBumpFailsAnUnassessedRun(t *testing.T) {
	tests := []struct {
		name        string
		set         Set
		from, to    string
		wantVerdict Verdict
		wantReason  Reason
	}{
		{
			name: "no record at all", set: Set{},
			from: "0.1.0-alpha.8", to: "0.1.0-alpha.12",
			wantVerdict: VerdictUnknown, wantReason: ReasonNoRecord,
		},
		{
			// The pin moved past the only record's ceiling, so the next bump
			// crosses no boundary. Assessment plainly ended at alpha.12.
			name: "a record exists but the pin moved past its ceiling",
			set:  oneComponent(trans("<0.1.0-alpha.12", "0.1.0-alpha.12", VerdictManual, "M")),
			from: "0.1.0-alpha.12", to: "0.1.0-alpha.20",
			wantVerdict: VerdictBlocked, wantReason: ReasonBeyondRecordCeiling,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Match(tt.set, map[string]string{"c": tt.from}, map[string]string{"c": tt.to})
			if len(got) != 1 {
				t.Fatalf("Match() returned %d rows, want 1: %+v", len(got), got)
			}
			if got[0].Verdict != tt.wantVerdict {
				t.Errorf("verdict = %q, want %q", got[0].Verdict, tt.wantVerdict)
			}
			if got[0].Reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", got[0].Reason, tt.wantReason)
			}
			if !got[0].Breaking {
				t.Error("breaking = false, want true: a differing prerelease is a boundary")
			}
			if !got[0].FailsRun() {
				t.Error("FailsRun() = false, want true: an unassessed prerelease bump must not pass")
			}
		})
	}
}

// A recorded verdict is unaffected by the prerelease arm: Breaking is consulted
// only where the verdict is unknown.
func TestMatchPrereleaseBreakingDoesNotDisturbARecordedSafe(t *testing.T) {
	set := oneComponent(Transition{
		From: "<0.1.0-alpha.12", To: ">=0.1.0-alpha.12 <=0.1.0-alpha.12",
		Verdict: VerdictSafe, VerifiedBy: "uat: synthetic lane", Summary: "S",
	})
	got := Match(set, map[string]string{"c": "0.1.0-alpha.8"}, map[string]string{"c": "0.1.0-alpha.12"})
	if len(got) != 1 {
		t.Fatalf("Match() returned %d rows, want 1: %+v", len(got), got)
	}
	if got[0].Verdict != VerdictSafe {
		t.Fatalf("verdict = %q, want %q", got[0].Verdict, VerdictSafe)
	}
	if !got[0].Breaking {
		t.Error("breaking = false, want true: the boundary is still classified")
	}
	if got[0].FailsRun() {
		t.Error("FailsRun() = true, want false: a recorded safe does not consult Breaking")
	}
}

// TestMatchRecordBlocksNamesALandingShortOfTheBlock checks that record-blocks
// advice never names the block's own floor, which sits inside the interval it
// forbids.
func TestMatchRecordBlocksNamesALandingShortOfTheBlock(t *testing.T) {
	// C closes the hole below B's from, so both sources start somewhere a
	// record describes; one that does not is undefined-origin instead.
	set := oneComponent(
		trans("<2.0.0", ">=2.0.0 <2.1.0", VerdictSafe, "S"),
		trans(">=2.0.0 <2.5.0", ">=2.5.0 <2.6.0", VerdictSafe, "C"),
		trans(">=2.5.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictBlocked, "B"),
	)
	explanations := make(map[string]string, 2)
	for _, src := range []string{"1.5.0", "2.4.9"} {
		got := Match(set, map[string]string{"c": src}, map[string]string{"c": "3.0.0"})
		if len(got) != 1 {
			t.Fatalf("Match(%s) returned %d rows, want 1: %+v", src, len(got), got)
		}
		if got[0].Reason != ReasonRecordBlocks {
			t.Fatalf("reason = %q, want %q", got[0].Reason, ReasonRecordBlocks)
		}
		if !strings.Contains(got[0].Explanation, "Upgrade to 2.5.0 first") {
			t.Errorf("explanation does not name where C lands: %q", got[0].Explanation)
		}
		if strings.Contains(got[0].Explanation, "Upgrade to 3.0.0") {
			t.Errorf("explanation names a stop inside the interval it forbids: %q", got[0].Explanation)
		}
		explanations[src] = got[0].Explanation
	}
	if explanations["1.5.0"] == explanations["2.4.9"] {
		t.Errorf("two different sources produced byte-identical advice: %q", explanations["1.5.0"])
	}
}

// TestMatchFollowsTheSourcesPath checks that neither records off the source's
// path nor declaration order decides the verdict.
func TestMatchFollowsTheSourcesPath(t *testing.T) {
	sharedA := func(v Verdict) Transition { return trans(">=1.0.0 <2.0.0", ">=3.0.0 <=3.0.0", v, "A") }
	sharedB := func(v Verdict) Transition { return trans(">=2.0.0 <3.0.0", ">=3.0.0 <=3.0.0", v, "B") }
	wide := trans(">=1.0.0 <2.0.0", ">=4.0.0 <=4.0.0", VerdictManual, "C")
	between := trans(">=2.0.0 <3.0.0", ">=3.0.0 <3.1.0", VerdictManual, "B")
	safeEntry := trans("<2.0.0", ">=2.0.0 <2.5.0", VerdictSafe, "A")
	blockedExit := trans(">=2.0.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictBlocked, "B")

	tests := []struct {
		name          string
		set           Set
		from, to      string
		wantVerdict   Verdict
		wantReason    Reason
		wantMatched   string
		wantStoppedAt string
	}{
		{
			name:        "one record on a shared floor",
			set:         oneComponent(sharedA(VerdictManual)),
			from:        "1.5.0",
			to:          "3.0.0",
			wantVerdict: VerdictManual,
			wantReason:  ReasonRecorded,
			wantMatched: "A",
		},
		{
			name:        "a shared floor resolves to the record written for the source",
			set:         oneComponent(sharedA(VerdictManual), sharedB(VerdictManual)),
			from:        "1.5.0",
			to:          "3.0.0",
			wantVerdict: VerdictManual,
			wantReason:  ReasonRecorded,
			wantMatched: "A",
		},
		{
			name:        "a shared safe floor reads safe with the source's record listed first",
			set:         oneComponent(sharedA(VerdictSafe), sharedB(VerdictSafe)),
			from:        "1.5.0",
			to:          "3.0.0",
			wantVerdict: VerdictSafe,
			wantReason:  ReasonRecorded,
			wantMatched: "A",
		},
		{
			name:        "a shared safe floor reads safe with the source's record listed last",
			set:         oneComponent(sharedB(VerdictSafe), sharedA(VerdictSafe)),
			from:        "1.5.0",
			to:          "3.0.0",
			wantVerdict: VerdictSafe,
			wantReason:  ReasonRecorded,
			wantMatched: "A",
		},
		{
			// The jump passes through 2.x, which B forbids leaving for 3.0.0
			// in one step, and A does not list B's floor.
			name:          "a block on a shared floor the jump flies over blocks",
			set:           oneComponent(sharedB(VerdictBlocked), sharedA(VerdictManual)),
			from:          "1.5.0",
			to:            "3.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonUndefinedOrigin,
			wantStoppedAt: ">=3.0.0 <=3.0.0",
		},
		{
			name:        "a record listing a block on its shared floor describes the jump",
			set:         oneComponent(sharedB(VerdictBlocked), skipping(sharedA(VerdictManual), "3.0.0")),
			from:        "1.5.0",
			to:          "3.0.0",
			wantVerdict: VerdictManual,
			wantReason:  ReasonRecorded,
			wantMatched: "A",
		},
		{
			name:          "the other origin on a shared floor still gets its own record",
			set:           oneComponent(sharedA(VerdictManual), sharedB(VerdictBlocked)),
			from:          "2.5.0",
			to:            "3.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonRecorded,
			wantMatched:   "B",
			wantStoppedAt: ">=3.0.0 <=3.0.0",
		},
		{
			name:          "a record covering the whole jump does not hide a manual one it flies over",
			set:           oneComponent(wide, between),
			from:          "1.5.0",
			to:            "4.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonUndefinedOrigin,
			wantStoppedAt: ">=3.0.0 <3.1.0",
		},
		{
			name:        "a record covering the whole jump that lists the manual one describes it",
			set:         oneComponent(skipping(wide, "3.0.0"), between),
			from:        "1.5.0",
			to:          "4.0.0",
			wantVerdict: VerdictManual,
			wantReason:  ReasonRecorded,
			wantMatched: "C",
		},
		{
			name: "a blocked record on the path does not hide a second block it flies over",
			set: oneComponent(
				trans("<1.0.0", ">=3.0.0", VerdictBlocked, "B1"),
				trans(">=1.0.0 <4.0.0", ">=4.0.0", VerdictBlocked, "B2"),
			),
			from:          "0.5.0",
			to:            "4.5.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonRecordBlocks,
			wantStoppedAt: ">=3.0.0",
		},
		{
			name:          "a stop short of the whole jump is undefined-origin",
			set:           oneComponent(wide, between),
			from:          "1.5.0",
			to:            "3.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonUndefinedOrigin,
			wantStoppedAt: ">=3.0.0 <3.1.0",
		},
		{
			name:          "a block reached through a lower hop names that hop's landing",
			set:           oneComponent(safeEntry, blockedExit),
			from:          "1.5.0",
			to:            "3.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonRecordBlocks,
			wantStoppedAt: ">=3.0.0 <=3.0.0",
		},
		{
			name:        "following that advice reaches the landing",
			set:         oneComponent(safeEntry, blockedExit),
			from:        "1.5.0",
			to:          "2.0.0",
			wantVerdict: VerdictSafe,
			wantReason:  ReasonRecorded,
			wantMatched: "A",
		},
		{
			name:          "and from the landing the block renders its own steps",
			set:           oneComponent(safeEntry, blockedExit),
			from:          "2.0.0",
			to:            "3.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonRecorded,
			wantMatched:   "B",
			wantStoppedAt: ">=3.0.0 <=3.0.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Match(tt.set, map[string]string{"c": tt.from}, map[string]string{"c": tt.to})
			if len(got) != 1 {
				t.Fatalf("Match() returned %d rows, want 1: %+v", len(got), got)
			}
			r := got[0]
			if r.Verdict != tt.wantVerdict {
				t.Errorf("verdict = %q, want %q", r.Verdict, tt.wantVerdict)
			}
			if r.Reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", r.Reason, tt.wantReason)
			}
			if r.StoppedAt != tt.wantStoppedAt {
				t.Errorf("stoppedAt = %q, want %q", r.StoppedAt, tt.wantStoppedAt)
			}
			matched := ""
			if r.Transition != nil {
				matched = r.Transition.Summary
			}
			if matched != tt.wantMatched {
				t.Errorf("matched record = %q, want %q", matched, tt.wantMatched)
			}
		})
	}
}

// TestMatchMigrationWindow checks that a jump past 0.18.0 through 0.20.x, the
// only versions that run a rename's mirror before 0.21.0 removes the legacy
// objects, stops inside them. Each case runs in both declaration orders.
func TestMatchMigrationWindow(t *testing.T) {
	rename := skipping(trans("<0.18.0", ">=0.18.0 <=0.20.0", VerdictManual, "rename"), "0.20.0")
	wideRename := skipping(trans("<0.18.0", ">=0.18.0 <=0.21.0", VerdictManual, "rename"), "0.20.0")
	wideRenameSkipsRemoval := skipping(wideRename, "0.20.0", "0.21.0")
	drain := trans(">=0.18.0 <0.20.0", "=0.20.0", VerdictSafe, "drain")
	removal := func(v Verdict) Transition { return trans(">=0.18.0 <0.21.0", "=0.21.0", v, "removal") }
	passThrough := trans("<0.18.0", "=0.21.0", VerdictBlocked, "pass-through")

	tests := []struct {
		name          string
		trs           []Transition
		from, to      string
		wantVerdict   Verdict
		wantReason    Reason
		wantMatched   string
		wantStoppedAt string
		wantInExpl    string
	}{
		{
			name:          "a jump past the window stops at its start",
			trs:           []Transition{rename, drain, removal(VerdictManual)},
			from:          "0.17.2",
			to:            "0.21.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonMultipleBoundaries,
			wantStoppedAt: ">=0.18.0 <=0.20.0",
			wantInExpl:    "Upgrade to 0.18.0 first",
		},
		{
			// The rename's ceiling, not the removal's verdict, ends the
			// window.
			name:          "a safe removal still stops the jump at the window",
			trs:           []Transition{rename, drain, removal(VerdictSafe)},
			from:          "0.17.2",
			to:            "0.21.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonMultipleBoundaries,
			wantStoppedAt: ">=0.18.0 <=0.20.0",
			wantInExpl:    "Upgrade to 0.18.0 first",
		},
		{
			// The removal's floor sits inside the rename's `to`, so the
			// landing before it ends below that floor.
			name:          "a rename ceiling past the window does not hide a manual removal",
			trs:           []Transition{wideRename, drain, removal(VerdictManual)},
			from:          "0.17.2",
			to:            "0.21.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonMultipleBoundaries,
			wantStoppedAt: ">=0.18.0 <0.21.0",
			wantInExpl:    "Upgrade to 0.18.0 first",
		},
		{
			// A safe removal still needs listing when the rename's `to`
			// holds its floor, so widening the ceiling vouches for nothing on
			// its own.
			name:          "a rename ceiling past the window with an unlisted safe removal stops at the window",
			trs:           []Transition{wideRename, drain, removal(VerdictSafe)},
			from:          "0.17.2",
			to:            "0.21.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonMultipleBoundaries,
			wantStoppedAt: ">=0.18.0 <0.21.0",
			wantInExpl:    "Upgrade to 0.18.0 first",
		},
		{
			name:        "a rename that lists the safe removal vouches for the jump",
			trs:         []Transition{wideRenameSkipsRemoval, drain, removal(VerdictSafe)},
			from:        "0.17.2",
			to:          "0.21.0",
			wantVerdict: VerdictManual,
			wantReason:  ReasonRecorded,
			wantMatched: "rename",
		},
		{
			name:          "a pass-through block that does not list the window describes no move",
			trs:           []Transition{rename, drain, removal(VerdictManual), passThrough},
			from:          "0.17.2",
			to:            "0.21.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonMultipleBoundaries,
			wantStoppedAt: ">=0.18.0 <=0.20.0",
			wantInExpl:    "Upgrade to 0.18.0 first",
		},
		{
			name:          "a pass-through block that lists the window describes the jump it forbids",
			trs:           []Transition{rename, drain, removal(VerdictManual), skipping(passThrough, "0.18.0", "0.20.0")},
			from:          "0.17.2",
			to:            "0.21.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonRecorded,
			wantMatched:   "pass-through",
			wantStoppedAt: "=0.21.0",
			wantInExpl:    "Follow the steps below",
		},
		{
			name:          "a removal written for every source still stops the jump at the window",
			trs:           []Transition{rename, drain, trans("<0.21.0", "=0.21.0", VerdictManual, "removal")},
			from:          "0.17.2",
			to:            "0.21.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonMultipleBoundaries,
			wantStoppedAt: ">=0.18.0 <=0.20.0",
			wantInExpl:    "Upgrade to 0.18.0 first",
		},
		{
			name:        "the move into the window carries the rename's steps",
			trs:         []Transition{rename, drain, removal(VerdictManual), passThrough},
			from:        "0.17.2",
			to:          "0.18.0",
			wantVerdict: VerdictManual,
			wantReason:  ReasonRecorded,
			wantMatched: "rename",
		},
		{
			name:        "a move to the window's end carries the rename's steps",
			trs:         []Transition{rename, drain, removal(VerdictManual), passThrough},
			from:        "0.17.2",
			to:          "0.20.0",
			wantVerdict: VerdictManual,
			wantReason:  ReasonRecorded,
			wantMatched: "rename",
		},
		{
			name:        "within the window the drain reads safe",
			trs:         []Transition{rename, drain, removal(VerdictManual), passThrough},
			from:        "0.18.0",
			to:          "0.20.0",
			wantVerdict: VerdictSafe,
			wantReason:  ReasonRecorded,
			wantMatched: "drain",
		},
		{
			name:        "from inside the window the removal carries the jump",
			trs:         []Transition{rename, drain, removal(VerdictManual), passThrough},
			from:        "0.18.0",
			to:          "0.21.0",
			wantVerdict: VerdictManual,
			wantReason:  ReasonRecorded,
			wantMatched: "removal",
		},
		{
			name:        "from the window's end the removal carries the jump",
			trs:         []Transition{rename, drain, removal(VerdictManual), passThrough},
			from:        "0.20.0",
			to:          "0.21.0",
			wantVerdict: VerdictManual,
			wantReason:  ReasonRecorded,
			wantMatched: "removal",
		},
		{
			name:          "a target past the removal's ceiling stops at that ceiling",
			trs:           []Transition{rename, drain, removal(VerdictManual), passThrough},
			from:          "0.20.0",
			to:            "0.22.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonBeyondRecordCeiling,
			wantStoppedAt: "=0.21.0",
			wantInExpl:    "0.21.0",
		},
	}
	for _, tt := range tests {
		for _, order := range []string{"declared", "reversed"} {
			t.Run(tt.name+"/"+order, func(t *testing.T) {
				trs := slices.Clone(tt.trs)
				if order == "reversed" {
					slices.Reverse(trs)
				}
				got := Match(oneComponent(trs...), map[string]string{"c": tt.from}, map[string]string{"c": tt.to})
				if len(got) != 1 {
					t.Fatalf("Match() returned %d rows, want 1: %+v", len(got), got)
				}
				r := got[0]
				if r.Verdict != tt.wantVerdict {
					t.Errorf("verdict = %q, want %q", r.Verdict, tt.wantVerdict)
				}
				if r.Reason != tt.wantReason {
					t.Errorf("reason = %q, want %q", r.Reason, tt.wantReason)
				}
				if r.StoppedAt != tt.wantStoppedAt {
					t.Errorf("stoppedAt = %q, want %q", r.StoppedAt, tt.wantStoppedAt)
				}
				matched := ""
				if r.Transition != nil {
					matched = r.Transition.Summary
				}
				if matched != tt.wantMatched {
					t.Errorf("matched record = %q, want %q", matched, tt.wantMatched)
				}
				if !strings.Contains(r.Explanation, tt.wantInExpl) {
					t.Errorf("explanation = %q, want it to contain %q", r.Explanation, tt.wantInExpl)
				}
			})
		}
	}
}

// TestMatchRoutes checks route planning over skips, required stops and the
// minor step limit. Each case runs in both declaration orders.
func TestMatchRoutes(t *testing.T) {
	patch := trans(">=1.23.0 <1.23.2", "=1.23.2", VerdictManual, "patch")
	minor := trans(">=1.23.0 <1.24.0", ">=1.24.0", VerdictManual, "minor")
	minorSkipsPatch := skipping(minor, "1.23.2")
	lower := skipping(trans(">=1.22.0 <1.23.0", ">=1.23.0", VerdictSafe, "lower"), "1.23.2")
	major := trans(">=1.9.0 <1.10.0", ">=2.0.0", VerdictManual, "major")
	stop := Transition{From: ">=1.0.0 <2.0.0", To: ">=2.0.0 <2.1.0", Verdict: VerdictSafe, RequiredStop: true, Summary: "stop"}
	sharing := trans("<1.0.0", ">=2.0.0 <2.1.0", VerdictSafe, "sharing")
	after := trans(">=2.0.0 <3.0.0", "=3.0.0", VerdictSafe, "after")
	// A cluster that ran 1.2 must take the manual move to 1.3. One that never
	// did may jump straight to 2.0.
	enter := trans("<1.2.0", ">=1.2.0 <1.3.0", VerdictSafe, "enter")
	leave := trans(">=1.2.0 <1.3.0", ">=1.3.0 <2.0.0", VerdictManual, "leave")
	onward := trans(">=1.3.0 <2.0.0", "=2.0.0", VerdictSafe, "onward")
	direct := skipping(trans("<1.2.0", "=2.0.0", VerdictSafe, "direct"), "1.3.0")
	// Two two-hop routes from 1.x to 3.0.0, through 2.0.x or through 2.1.x.
	lowEntry := trans(">=1.0.0 <2.0.0", ">=2.0.0 <2.1.0", VerdictSafe, "lowEntry")
	highEntry := trans(">=1.0.0 <2.0.0", ">=2.1.0 <2.2.0", VerdictSafe, "highEntry")
	lowExit := func(v Verdict) Transition { return trans(">=2.0.0 <2.1.0", "=3.0.0", v, "lowExit") }
	highExit := trans(">=2.1.0 <2.2.0", "=3.0.0", VerdictSafe, "highExit")
	// A safe run from 1.0.0 whose landing before a manual record reaches past
	// 1.2.0.
	patchStep := trans(">=1.0.0 <1.1.0", ">=1.1.0 <1.1.5", VerdictSafe, "patchStep")
	wideStep := trans(">=1.1.0 <1.1.5", ">=1.1.5 <1.2.5", VerdictSafe, "wideStep")
	lateManual := trans(">=1.1.5 <1.2.5", ">=1.2.5", VerdictManual, "lateManual")

	tests := []struct {
		name          string
		trs           []Transition
		stepLimit     string
		from, to      string
		wantVerdict   Verdict
		wantReason    Reason
		wantMatched   string
		wantStoppedAt string
		wantRoute     []RouteStop
	}{
		{
			// The safe route still splits, because joining its hops would pass
			// through 2.0.x, which lowExit says needs work.
			name:          "of two routes with as many hops, the one with fewer manual hops wins",
			trs:           []Transition{lowEntry, highEntry, lowExit(VerdictManual), highExit},
			from:          "1.0.0",
			to:            "3.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonMultipleBoundaries,
			wantStoppedAt: ">=2.1.0 <2.2.0",
			wantRoute: []RouteStop{
				{Versions: ">=2.1.0 <2.2.0", Verdict: VerdictSafe},
				{Versions: "3.0.0", Verdict: VerdictSafe},
			},
		},
		{
			name:        "of two equal routes, the one through the lower floor wins",
			trs:         []Transition{lowEntry, highEntry, lowExit(VerdictSafe), highExit},
			from:        "1.0.0",
			to:          "3.0.0",
			wantVerdict: VerdictSafe,
			wantReason:  ReasonRecorded,
			wantMatched: "lowExit",
		},
		{
			name:          "a stop under the step limit lands within one minor of where the upgrade starts",
			trs:           []Transition{patchStep, wideStep, lateManual},
			stepLimit:     StepLimitMinor,
			from:          "1.0.0",
			to:            "1.2.5",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonMultipleBoundaries,
			wantStoppedAt: ">=1.1.5 <1.2.0",
			wantRoute: []RouteStop{
				{Versions: ">=1.1.5 <1.2.0", Verdict: VerdictSafe},
				{Versions: "1.2.5", Verdict: VerdictManual},
			},
		},
		{
			name: "a landing on one version names it",
			trs: []Transition{
				trans("<2.0.0", ">=2.0.0 <=2.5.0", VerdictManual, "into"),
				trans(">=2.5.0 <3.0.0", "=3.0.0", VerdictManual, "out"),
			},
			from:          "1.5.0",
			to:            "3.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonMultipleBoundaries,
			wantStoppedAt: "=2.5.0",
			wantRoute: []RouteStop{
				{Versions: "=2.5.0", Verdict: VerdictManual},
				{Versions: "3.0.0", Verdict: VerdictManual},
			},
		},
		{
			name: "a landing that ends on an inclusive ceiling names it",
			trs: []Transition{
				trans("<2.0.0", ">=2.0.0 <=2.5.0", VerdictManual, "into"),
				trans(">=2.2.0 <=2.8.0", "=3.0.0", VerdictManual, "out"),
			},
			from:          "1.5.0",
			to:            "3.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonMultipleBoundaries,
			wantStoppedAt: ">=2.2.0 <=2.5.0",
			wantRoute: []RouteStop{
				{Versions: ">=2.2.0 <=2.5.0", Verdict: VerdictManual},
				{Versions: "3.0.0", Verdict: VerdictManual},
			},
		},
		{
			name:        "a jump that never ran the window skips its manual exit",
			trs:         []Transition{enter, leave, onward, direct},
			from:        "1.0.0",
			to:          "2.0.0",
			wantVerdict: VerdictSafe,
			wantReason:  ReasonRecorded,
			wantMatched: "direct",
		},
		{
			name:          "a jump from inside the window takes its manual exit first",
			trs:           []Transition{enter, leave, onward, direct},
			from:          "1.2.5",
			to:            "2.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonMultipleBoundaries,
			wantStoppedAt: ">=1.3.0 <2.0.0",
			wantRoute: []RouteStop{
				{Versions: ">=1.3.0 <2.0.0", Verdict: VerdictManual},
				{Versions: "2.0.0", Verdict: VerdictSafe},
			},
		},
		{
			name:        "a record that lists a patch boundary describes the jump past it",
			trs:         []Transition{patch, minorSkipsPatch},
			stepLimit:   StepLimitMinor,
			from:        "1.23.0",
			to:          "1.24.1",
			wantVerdict: VerdictManual,
			wantReason:  ReasonRecorded,
			wantMatched: "minor",
		},
		{
			name:          "an unlisted patch boundary makes the jump two upgrades",
			trs:           []Transition{patch, minor},
			stepLimit:     StepLimitMinor,
			from:          "1.23.0",
			to:            "1.24.1",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonMultipleBoundaries,
			wantStoppedAt: "=1.23.2",
			wantRoute: []RouteStop{
				{Versions: "=1.23.2", Verdict: VerdictManual},
				{Versions: "1.24.1", Verdict: VerdictManual},
			},
		},
		{
			name:        "the patch boundary alone carries its own steps",
			trs:         []Transition{patch, minorSkipsPatch},
			stepLimit:   StepLimitMinor,
			from:        "1.23.0",
			to:          "1.23.2",
			wantVerdict: VerdictManual,
			wantReason:  ReasonRecorded,
			wantMatched: "patch",
		},
		{
			name:          "a derived ceiling ends at the next minor",
			trs:           []Transition{patch, minorSkipsPatch},
			stepLimit:     StepLimitMinor,
			from:          "1.23.2",
			to:            "1.25.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonBeyondRecordCeiling,
			wantStoppedAt: ">=1.24.0 <1.25.0",
		},
		{
			name:          "a move reaches the next minor at most",
			trs:           []Transition{lower, patch, minorSkipsPatch},
			stepLimit:     StepLimitMinor,
			from:          "1.22.4",
			to:            "1.24.1",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonMultipleBoundaries,
			wantStoppedAt: ">=1.23.0 <1.24.0",
			wantRoute: []RouteStop{
				{Versions: ">=1.23.0 <1.24.0", Verdict: VerdictSafe},
				{Versions: "1.24.1", Verdict: VerdictManual},
			},
		},
		{
			name:        "a major bump from the last minor follows its record",
			trs:         []Transition{major},
			stepLimit:   StepLimitMinor,
			from:        "1.9.3",
			to:          "2.0.1",
			wantVerdict: VerdictManual,
			wantReason:  ReasonRecorded,
			wantMatched: "major",
		},
		{
			name:          "a major bump from an earlier minor is undefined",
			trs:           []Transition{major},
			stepLimit:     StepLimitMinor,
			from:          "1.8.0",
			to:            "2.0.1",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonUndefinedOrigin,
			wantStoppedAt: ">=2.0.0 <2.1.0",
		},
		{
			name:          "a required stop holds its own sources",
			trs:           []Transition{stop, sharing, after},
			from:          "1.5.0",
			to:            "3.0.0",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonMultipleBoundaries,
			wantStoppedAt: ">=2.0.0 <2.1.0",
			wantRoute: []RouteStop{
				{Versions: ">=2.0.0 <2.1.0", Verdict: VerdictSafe},
				{Versions: "3.0.0", Verdict: VerdictSafe},
			},
		},
		{
			name: "a to with no ceiling that passes an unlisted floor is no partial route",
			trs: []Transition{
				trans("<0.18.0", ">=0.18.0", VerdictManual, "open"),
				trans(">=0.18.0 <0.19.0", "=0.19.0", VerdictManual, "next"),
			},
			from:          "0.17.0",
			to:            "0.19.5",
			wantVerdict:   VerdictBlocked,
			wantReason:    ReasonMultipleBoundaries,
			wantStoppedAt: ">=0.18.0 <0.19.0",
		},
		{
			name:        "a required stop does not hold another record's sources",
			trs:         []Transition{stop, sharing, after},
			from:        "0.5.0",
			to:          "3.0.0",
			wantVerdict: VerdictSafe,
			wantReason:  ReasonRecorded,
			wantMatched: "after",
		},
	}
	for _, tt := range tests {
		for _, order := range []string{"declared", "reversed"} {
			t.Run(tt.name+"/"+order, func(t *testing.T) {
				trs := slices.Clone(tt.trs)
				if order == "reversed" {
					slices.Reverse(trs)
				}
				set := Set{"c": &ComponentUpgrades{Component: "c", StepLimit: tt.stepLimit, Transitions: trs}}
				got := Match(set, map[string]string{"c": tt.from}, map[string]string{"c": tt.to})
				if len(got) != 1 {
					t.Fatalf("Match() returned %d rows, want 1: %+v", len(got), got)
				}
				r := got[0]
				if r.Verdict != tt.wantVerdict {
					t.Errorf("verdict = %q, want %q", r.Verdict, tt.wantVerdict)
				}
				if r.Reason != tt.wantReason {
					t.Errorf("reason = %q, want %q", r.Reason, tt.wantReason)
				}
				if r.StoppedAt != tt.wantStoppedAt {
					t.Errorf("stoppedAt = %q, want %q", r.StoppedAt, tt.wantStoppedAt)
				}
				matched := ""
				if r.Transition != nil {
					matched = r.Transition.Summary
				}
				if matched != tt.wantMatched {
					t.Errorf("matched record = %q, want %q", matched, tt.wantMatched)
				}
				if !slices.Equal(r.Route, tt.wantRoute) {
					t.Errorf("route = %+v, want %+v", r.Route, tt.wantRoute)
				}
			})
		}
	}
}

// TestMatchTiesIgnoreDeclarationOrder checks that records tied on a floor or a
// ceiling give the same result in either declaration order.
func TestMatchTiesIgnoreDeclarationOrder(t *testing.T) {
	tests := []struct {
		name          string
		trs           []Transition
		from, to      string
		wantReason    Reason
		wantMatched   string
		wantStoppedAt string
		wantInExpl    string
	}{
		{
			name: "two blocked records on one floor resolve to the lower from",
			trs: []Transition{
				trans("<1.0.0", ">=3.0.0 <3.1.0", VerdictBlocked, "B1"),
				trans(">=1.0.0 <2.0.0", ">=3.0.0 <=3.0.0", VerdictBlocked, "B2"),
			},
			from:          "0.5.0",
			to:            "3.0.0",
			wantReason:    ReasonRecorded,
			wantMatched:   "B1",
			wantStoppedAt: ">=3.0.0 <3.1.0",
		},
		{
			name: "two records on one ceiling resolve to the lower floor",
			trs: []Transition{
				trans("<1.0.0", ">=1.0.0 <2.0.0", VerdictSafe, "S1"),
				trans(">=1.0.0 <1.5.0", ">=1.5.0 <2.0.0", VerdictSafe, "S2"),
			},
			from:          "1.6.0",
			to:            "2.5.0",
			wantReason:    ReasonBeyondRecordCeiling,
			wantStoppedAt: ">=1.0.0 <2.0.0",
		},
		{
			name: "the ranges the source's records lead into are listed by floor",
			trs: []Transition{
				trans("<1.0.0", ">=5.0.0", VerdictSafe, "L1"),
				trans("<1.0.0", ">=4.0.0 <5.0.0", VerdictSafe, "L2"),
				trans(">=2.0.0 <3.0.0", ">=3.0.0 <4.0.0", VerdictSafe, "X"),
			},
			from:          "0.5.0",
			to:            "3.5.0",
			wantReason:    ReasonUndefinedOrigin,
			wantStoppedAt: ">=3.0.0 <4.0.0",
			wantInExpl:    "lead only into >=4.0.0 <5.0.0 and >=5.0.0,",
		},
		{
			name: "two records on one floor and one from start resolve by to",
			trs: []Transition{
				trans(">=1.0.0 <1.5.0", ">=3.0.0 <=3.0.0", VerdictBlocked, "B1"),
				trans(">=1.0.0 <2.0.0", ">=3.0.0 <3.1.0", VerdictBlocked, "B2"),
			},
			from:          "1.2.0",
			to:            "3.0.0",
			wantReason:    ReasonRecorded,
			wantMatched:   "B2",
			wantStoppedAt: ">=3.0.0 <3.1.0",
		},
		{
			name: "two records with one floor, from start and to resolve by from",
			trs: []Transition{
				trans(">=1.0.0 <2.0.0", ">=3.0.0 <3.1.0", VerdictBlocked, "B1"),
				trans(">=1.0.0 <1.5.0", ">=3.0.0 <3.1.0", VerdictBlocked, "B2"),
			},
			from:          "1.2.0",
			to:            "3.0.0",
			wantReason:    ReasonRecorded,
			wantMatched:   "B2",
			wantStoppedAt: ">=3.0.0 <3.1.0",
		},
	}
	for _, tt := range tests {
		for _, order := range []string{"declared", "reversed"} {
			t.Run(tt.name+"/"+order, func(t *testing.T) {
				trs := slices.Clone(tt.trs)
				if order == "reversed" {
					slices.Reverse(trs)
				}
				got := Match(oneComponent(trs...), map[string]string{"c": tt.from}, map[string]string{"c": tt.to})
				if len(got) != 1 {
					t.Fatalf("Match() returned %d rows, want 1: %+v", len(got), got)
				}
				r := got[0]
				if r.Reason != tt.wantReason {
					t.Errorf("reason = %q, want %q", r.Reason, tt.wantReason)
				}
				if r.StoppedAt != tt.wantStoppedAt {
					t.Errorf("stoppedAt = %q, want %q", r.StoppedAt, tt.wantStoppedAt)
				}
				matched := ""
				if r.Transition != nil {
					matched = r.Transition.Summary
				}
				if matched != tt.wantMatched {
					t.Errorf("matched record = %q, want %q", matched, tt.wantMatched)
				}
				if !strings.Contains(r.Explanation, tt.wantInExpl) {
					t.Errorf("explanation = %q, want it to contain %q", r.Explanation, tt.wantInExpl)
				}
			})
		}
	}
}

func TestAdmits(t *testing.T) {
	v := semver.MustParse
	tests := []struct {
		name string
		rng  string
		at   bound
		want bool
	}{
		{"on an inclusive floor", ">=2.0.0 <3.0.0", bound{ver: v("2.0.0"), inclusive: true}, true},
		{"on an exclusive floor", ">2.0.0 <3.0.0", bound{ver: v("2.0.0"), inclusive: true}, false},
		{"just above an exclusive floor", ">2.0.0 <3.0.0", bound{ver: v("2.0.0")}, true},
		{"just above an inclusive floor", ">=2.0.0 <3.0.0", bound{ver: v("2.0.0")}, true},
		{"just above a version below the floor", ">=2.0.0 <3.0.0", bound{ver: v("1.9.0")}, false},
		{"just above an inclusive ceiling", ">=1.0.0 <=2.0.0", bound{ver: v("2.0.0")}, false},
		{"just above a version below the ceiling", ">=1.0.0 <2.0.0", bound{ver: v("1.9.0")}, true},
		{"just above anything in an open range", "<3.0.0", bound{ver: v("2.0.0")}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := parseBounds(tt.rng)
			if err != nil {
				t.Fatalf("parseBounds(%q): %v", tt.rng, err)
			}
			if got := admits(b, tt.at); got != tt.want {
				t.Errorf("admits(%q, %+v) = %v, want %v", tt.rng, tt.at, got, tt.want)
			}
		})
	}
}

// A record vouches only as far as its own `to` ceiling. Reaching past it is the
// same forward reach checkPinCeiling rejects at authoring time.
func TestMatchTargetPastTheRecordCeiling(t *testing.T) {
	tests := []struct {
		name        string
		to          string
		target      string
		wantVerdict Verdict
		wantReason  Reason
	}{
		{"past an exclusive ceiling", ">=1.18.0 <1.19.0", "1.25.0", VerdictBlocked, ReasonBeyondRecordCeiling},
		{"exactly at an exclusive ceiling", ">=1.18.0 <1.19.0", "1.19.0", VerdictBlocked, ReasonBeyondRecordCeiling},
		{"exactly at an inclusive ceiling", ">=1.18.0 <=1.19.0", "1.19.0", VerdictSafe, ReasonRecorded},
		{"inside the range", ">=1.18.0 <1.19.0", "1.18.5", VerdictSafe, ReasonRecorded},
		{"a ceiling-less to reaches everywhere", ">=1.18.0", "1.25.0", VerdictSafe, ReasonRecorded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := oneComponent(Transition{
				From: "<1.18.0", To: tt.to, Verdict: VerdictSafe,
				VerifiedBy: "uat: synthetic lane", Summary: "S",
			})
			got := Match(set, map[string]string{"c": "1.17.0"}, map[string]string{"c": tt.target})
			if len(got) != 1 {
				t.Fatalf("Match() returned %d rows, want 1: %+v", len(got), got)
			}
			r := got[0]
			if r.Verdict != tt.wantVerdict {
				t.Errorf("verdict = %q, want %q", r.Verdict, tt.wantVerdict)
			}
			if r.Reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", r.Reason, tt.wantReason)
			}
			if tt.wantReason != ReasonBeyondRecordCeiling {
				return
			}
			if !r.FailsRun() {
				t.Error("FailsRun() = false, want true")
			}
			// The 1.x minors keep Breaking false throughout, so a passing
			// FailsRun above is the blocked verdict rather than the semver
			// calibration standing in for it.
			if r.Breaking {
				t.Error("breaking = true, want false")
			}
			if r.Transition != nil {
				t.Error("Transition is set; a record that does not cover the jump must render no steps")
			}
			if r.StoppedAt != tt.to {
				t.Errorf("StoppedAt = %q, want %q", r.StoppedAt, tt.to)
			}
			if (r.Span != Span{}) {
				t.Errorf("span = %+v, want zero: no record's claim covers this move", r.Span)
			}
		})
	}
}

// nsMove is the one identity move these tables exercise, named once so a row
// states its expectation without restating the struct.
var nsMove = []IdentityChange{{Field: "namespace", From: "gpu-operator", To: "nvidia"}}

func TestMatchIdentitiesIdentityAxis(t *testing.T) {
	// Under safeHop the version move 1.0.0 -> 1.1.0 is recorded safe, so a row
	// that does not come back safe came back that way for the other axis.
	safeHop := oneComponent(trans("<1.1.0", ">=1.1.0 <=1.1.9", VerdictSafe, "S"))
	manualHop := oneComponent(trans("<1.1.0", ">=1.1.0 <=1.1.9", VerdictManual, "M"))

	tests := []struct {
		name         string
		set          Set
		from         Identity
		to           Identity
		wantRows     int
		change       ChangeKind
		wantFrom     string
		wantTo       string
		verdict      Verdict
		reason       Reason
		moved        []IdentityChange
		explainNames []string
	}{
		{
			name:         "a namespace move while the version holds is its own row",
			set:          safeHop,
			from:         Identity{Version: "1.0.0", Namespace: "gpu-operator"},
			to:           Identity{Version: "1.0.0", Namespace: "nvidia"},
			wantRows:     1,
			change:       ChangeIdentity,
			wantFrom:     "1.0.0",
			wantTo:       "1.0.0",
			verdict:      VerdictUnknown,
			reason:       ReasonIdentityChanged,
			moved:        nsMove,
			explainNames: []string{"gpu-operator", "nvidia", "1.0.0"},
		},
		{
			name:         "both axes moving in one hop produce one row carrying both",
			set:          safeHop,
			from:         Identity{Version: "1.0.0", Namespace: "gpu-operator"},
			to:           Identity{Version: "1.1.0", Namespace: "nvidia"},
			wantRows:     1,
			change:       ChangeVersion,
			wantFrom:     "1.0.0",
			wantTo:       "1.1.0",
			verdict:      VerdictUnknown,
			reason:       ReasonIdentityChanged,
			moved:        nsMove,
			explainNames: []string{"gpu-operator", "nvidia", "1.0.0", "1.1.0"},
		},
		{
			name:         "a non-safe verdict keeps its own reason and still carries the move",
			set:          manualHop,
			from:         Identity{Version: "1.0.0", Namespace: "gpu-operator"},
			to:           Identity{Version: "1.1.0", Namespace: "nvidia"},
			wantRows:     1,
			change:       ChangeVersion,
			wantFrom:     "1.0.0",
			wantTo:       "1.1.0",
			verdict:      VerdictManual,
			reason:       ReasonRecorded,
			moved:        nsMove,
			explainNames: []string{"1.0.0", "1.1.0"},
		},
		{
			name:         "an unchanged namespace records no identity change",
			set:          safeHop,
			from:         Identity{Version: "1.0.0", Namespace: "gpu-operator"},
			to:           Identity{Version: "1.1.0", Namespace: "gpu-operator"},
			wantRows:     1,
			change:       ChangeVersion,
			wantFrom:     "1.0.0",
			wantTo:       "1.1.0",
			verdict:      VerdictSafe,
			reason:       ReasonRecorded,
			explainNames: []string{"1.0.0", "1.1.0"},
		},
		{
			name:     "an identical identity emits no row",
			set:      safeHop,
			from:     Identity{Version: "1.0.0", Namespace: "gpu-operator"},
			to:       Identity{Version: "1.0.0", Namespace: "gpu-operator"},
			wantRows: 0,
		},
		{
			// An artifact that does not carry the field states no namespace,
			// which is not the same as moving to one.
			name:         "an unstated namespace on one side is not a move",
			set:          safeHop,
			from:         Identity{Version: "1.0.0", Namespace: "gpu-operator"},
			to:           Identity{Version: "1.1.0"},
			wantRows:     1,
			change:       ChangeVersion,
			wantFrom:     "1.0.0",
			wantTo:       "1.1.0",
			verdict:      VerdictSafe,
			reason:       ReasonRecorded,
			explainNames: []string{"1.0.0", "1.1.0"},
		},
		{
			name:     "an unstated namespace on both sides of a held version emits no row",
			set:      safeHop,
			from:     Identity{Version: "1.0.0"},
			to:       Identity{Version: "1.0.0"},
			wantRows: 0,
		},
		{
			// The version axis ignores a leading "v", so this hop held its
			// version however the two sides spelled it. The row has to be the
			// relocation it is: a version row here would print the two
			// spellings in FROM and TO and read as a bump nobody made.
			name:         "a respelled version that relocates is still only a relocation",
			set:          safeHop,
			from:         Identity{Version: "v1.0.0", Namespace: "gpu-operator"},
			to:           Identity{Version: "1.0.0", Namespace: "nvidia"},
			wantRows:     1,
			change:       ChangeIdentity,
			wantFrom:     "v1.0.0",
			wantTo:       "1.0.0",
			verdict:      VerdictUnknown,
			reason:       ReasonIdentityChanged,
			moved:        nsMove,
			explainNames: []string{"v1.0.0", "gpu-operator", "nvidia"},
		},
		{
			name:     "a respelled version that stays put emits no row",
			set:      safeHop,
			from:     Identity{Version: "v1.0.0", Namespace: "gpu-operator"},
			to:       Identity{Version: "1.0.0", Namespace: "gpu-operator"},
			wantRows: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchIdentities(tt.set,
				map[string]Identity{"c": tt.from},
				map[string]Identity{"c": tt.to})
			if len(got) != tt.wantRows {
				t.Fatalf("MatchIdentities() returned %d rows, want %d: %+v", len(got), tt.wantRows, got)
			}
			if tt.wantRows == 0 {
				return
			}
			r := got[0]
			if r.Change != tt.change {
				t.Errorf("change = %q, want %q", r.Change, tt.change)
			}
			if r.From != tt.wantFrom || r.To != tt.wantTo {
				t.Errorf("from/to = %q/%q, want %q/%q", r.From, r.To, tt.wantFrom, tt.wantTo)
			}
			if r.Verdict != tt.verdict {
				t.Errorf("verdict = %q, want %q", r.Verdict, tt.verdict)
			}
			if r.Reason != tt.reason {
				t.Errorf("reason = %q, want %q", r.Reason, tt.reason)
			}
			if !reflect.DeepEqual(r.IdentityChanges, tt.moved) {
				t.Errorf("identityChanges = %+v, want %+v", r.IdentityChanges, tt.moved)
			}
			for _, want := range tt.explainNames {
				if !strings.Contains(r.Explanation, want) {
					t.Errorf("explanation %q does not name %q", r.Explanation, want)
				}
			}
			if len(tt.moved) == 0 {
				return
			}
			if r.Verdict == VerdictSafe {
				t.Error("verdict = safe on a row whose identity moved; the record assessed a version hop only")
			}
			if !r.FailsRun() {
				t.Error("FailsRun() = false on a row whose identity moved, want true")
			}
		})
	}
}

func TestMatchIdentitiesRegistryDerivedFields(t *testing.T) {
	base := Identity{
		Version: "1.0.0", Namespace: "ns", Chart: "old-chart", Source: "https://old.example",
		Path: "deploy/a", Type: "Helm", ManifestFiles: []string{"a.yaml", "b.yaml"},
		PreManifestFiles: []string{"pre-a.yaml", "pre-b.yaml"},
	}
	with := func(mutate func(*Identity)) Identity {
		id := base
		id.ManifestFiles = append([]string(nil), base.ManifestFiles...)
		id.PreManifestFiles = append([]string(nil), base.PreManifestFiles...)
		mutate(&id)
		return id
	}

	tests := []struct {
		name string
		to   Identity
		want []IdentityChange
	}{
		{"chart", with(func(i *Identity) { i.Chart = "new-chart" }),
			[]IdentityChange{{Field: "chart", From: "old-chart", To: "new-chart"}}},
		{"source", with(func(i *Identity) { i.Source = "https://new.example" }),
			[]IdentityChange{{Field: "source", From: "https://old.example", To: "https://new.example"}}},
		{"path", with(func(i *Identity) { i.Path = "deploy/b" }),
			[]IdentityChange{{Field: "path", From: "deploy/a", To: "deploy/b"}}},
		{"type", with(func(i *Identity) { i.Type = "Kustomize" }),
			[]IdentityChange{{Field: "type", From: "Helm", To: "Kustomize"}}},
		{"a dropped manifest file", with(func(i *Identity) { i.ManifestFiles = []string{"a.yaml"} }),
			[]IdentityChange{{Field: "manifestFiles", From: "a.yaml,b.yaml", To: "a.yaml", Removed: []string{"b.yaml"}}}},
		{"an added manifest file", with(func(i *Identity) { i.ManifestFiles = []string{"a.yaml", "b.yaml", "c.yaml"} }),
			[]IdentityChange{{Field: "manifestFiles", From: "a.yaml,b.yaml", To: "a.yaml,b.yaml,c.yaml", Added: []string{"c.yaml"}}}},
		{"an emptied manifest set", with(func(i *Identity) { i.ManifestFiles = nil }),
			[]IdentityChange{{Field: "manifestFiles", From: "a.yaml,b.yaml", To: "", Removed: []string{"a.yaml", "b.yaml"}}}},
		{"a dropped pre-manifest file", with(func(i *Identity) { i.PreManifestFiles = []string{"pre-a.yaml"} }),
			[]IdentityChange{{Field: "preManifestFiles", From: "pre-a.yaml,pre-b.yaml", To: "pre-a.yaml",
				Removed: []string{"pre-b.yaml"}}}},
		{"an added pre-manifest file", with(func(i *Identity) {
			i.PreManifestFiles = []string{"pre-a.yaml", "pre-b.yaml", "pre-c.yaml"}
		}),
			[]IdentityChange{{Field: "preManifestFiles", From: "pre-a.yaml,pre-b.yaml",
				To: "pre-a.yaml,pre-b.yaml,pre-c.yaml", Added: []string{"pre-c.yaml"}}}},
		{"an emptied pre-manifest set", with(func(i *Identity) { i.PreManifestFiles = nil }),
			[]IdentityChange{{Field: "preManifestFiles", From: "pre-a.yaml,pre-b.yaml", To: "",
				Removed: []string{"pre-a.yaml", "pre-b.yaml"}}}},
		{"a reordered pre-manifest set is not a move",
			with(func(i *Identity) { i.PreManifestFiles = []string{"pre-b.yaml", "pre-a.yaml"} }), nil},
		{"a reordered manifest set is not a move", with(func(i *Identity) { i.ManifestFiles = []string{"b.yaml", "a.yaml"} }), nil},
		{"an unstated chart is not a move", with(func(i *Identity) { i.Chart = "" }), nil},
		{"an unstated source is not a move", with(func(i *Identity) { i.Source = "" }), nil},
		{"an unstated path is not a move", with(func(i *Identity) { i.Path = "" }), nil},
		{"an unstated type is not a move", with(func(i *Identity) { i.Type = "" }), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchIdentities(Set{}, map[string]Identity{"c": base}, map[string]Identity{"c": tt.to})
			if tt.want == nil {
				if len(got) != 0 {
					t.Fatalf("MatchIdentities() = %+v, want no rows", got)
				}
				return
			}
			if len(got) != 1 || got[0].Change != ChangeIdentity {
				t.Fatalf("MatchIdentities() = %+v, want one identity row", got)
			}
			if !reflect.DeepEqual(got[0].IdentityChanges, tt.want) {
				t.Errorf("identityChanges = %+v, want %+v", got[0].IdentityChanges, tt.want)
			}
			if got[0].Verdict != VerdictUnknown || !got[0].FailsRun() {
				t.Errorf("verdict = %q, FailsRun = %v, want unknown and failing", got[0].Verdict, got[0].FailsRun())
			}
		})
	}
}

func TestMatchIdentitiesManifestMoveIsNamedInExplanation(t *testing.T) {
	got := MatchIdentities(Set{},
		map[string]Identity{"c": {Version: "1.0.0", ManifestFiles: []string{"a.yaml", "b.yaml"}}},
		map[string]Identity{"c": {Version: "1.0.0", ManifestFiles: []string{"a.yaml", "c.yaml"}}})
	if len(got) != 1 {
		t.Fatalf("MatchIdentities() returned %d rows, want 1", len(got))
	}
	if want := "its manifestFiles drop b.yaml and add c.yaml"; !strings.Contains(got[0].Explanation, want) {
		t.Errorf("explanation %q does not contain %q", got[0].Explanation, want)
	}
}

func TestMatchIdentitiesPreManifestMoveIsNamedInExplanation(t *testing.T) {
	got := MatchIdentities(Set{},
		map[string]Identity{"c": {Version: "1.0.0", PreManifestFiles: []string{"rbac.yaml"}}},
		map[string]Identity{"c": {Version: "1.0.0", PreManifestFiles: []string{"rbac.yaml", "scc.yaml"}}})
	if len(got) != 1 {
		t.Fatalf("MatchIdentities() returned %d rows, want 1", len(got))
	}
	if want := "its preManifestFiles add scc.yaml"; !strings.Contains(got[0].Explanation, want) {
		t.Errorf("explanation %q does not contain %q", got[0].Explanation, want)
	}
}

func TestMatchIdentitiesDoesNotSplitAComponentAcrossAxes(t *testing.T) {
	got := MatchIdentities(Set{},
		map[string]Identity{"c": {Version: "1.0.0", Namespace: "gpu-operator"}},
		map[string]Identity{"c": {Version: "2.0.0", Namespace: "nvidia"}})
	if len(got) != 1 {
		t.Fatalf("MatchIdentities() returned %d rows, want 1: %+v", len(got), got)
	}
	if got[0].Change != ChangeVersion || !reflect.DeepEqual(got[0].IdentityChanges, nsMove) {
		t.Errorf("got %+v, want a single version row carrying the namespace move", got[0])
	}
}

func TestMatchIsMatchIdentitiesWithoutTheIdentityAxis(t *testing.T) {
	set := oneComponent(blockA, blockB)
	from := map[string]string{"c": "0.17.2", "gone": "1.0.0"}
	to := map[string]string{"c": "0.20.1", "new": "1.0.0"}

	want := MatchIdentities(set, versionsOnly(from), versionsOnly(to))
	if got := Match(set, from, to); !reflect.DeepEqual(got, want) {
		t.Errorf("Match() = %+v, want %+v", got, want)
	}
	for _, r := range want {
		if r.IdentityChanges != nil {
			t.Errorf("%s carries identity changes, but a version table states no identity to move", r.Component)
		}
	}
}

// TestMatchLeadingVPrefixIsNotAChange pins the one normalization the
// same-version check applies, and its two limits.
//
// The prefix is not cosmetic in practice: the Argo CD deployer writes
// targetRevision through deployer.NormalizeVersion for an HTTPS chart repo, so
// a recipe pinning "v26.7.0" produces a cluster that reads back "26.7.0". A
// string comparison there reports every such component as changed and then
// resolves it to unknown, failing a run against a cluster already at the
// target.
func TestMatchLeadingVPrefixIsNotAChange(t *testing.T) {
	tests := []struct {
		name     string
		from     string
		to       string
		wantRows int
	}{
		{
			name:     "a v prefix on the target alone is not a change",
			from:     "26.7.0",
			to:       "v26.7.0",
			wantRows: 0,
		},
		{
			name:     "a v prefix on the source alone is not a change",
			from:     "v26.7.0",
			to:       "26.7.0",
			wantRows: 0,
		},
		{
			name:     "a real version change still reports",
			from:     "26.7.0",
			to:       "v26.8.0",
			wantRows: 1,
		},
		{
			// semver orders build metadata as equal, so semver.Equal would
			// silence this pair. Silence reads as safe, and a pin that moved
			// is not a pin that did not.
			name:     "a build-metadata-only difference still reports",
			from:     "1.2.3+build.1",
			to:       "1.2.3+build.2",
			wantRows: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Match(oneComponent(blockA), map[string]string{"c": tt.from}, map[string]string{"c": tt.to})
			if len(got) != tt.wantRows {
				t.Fatalf("Match() returned %d rows, want %d: %+v", len(got), tt.wantRows, got)
			}
			if tt.wantRows == 0 {
				return
			}
			// The versions are reported as each side wrote them, so an
			// operator reads the real strings rather than a normalized form
			// neither artifact contains.
			if got[0].From != tt.from || got[0].To != tt.to {
				t.Errorf("From/To = %q/%q, want %q/%q as written", got[0].From, got[0].To, tt.from, tt.to)
			}
		})
	}
}

// TestIdentityChangesObjectNames covers the object-name axis directly rather
// than through MatchIdentities, because its asymmetry with namespace is the
// point and it lives here: an absent namespace is a fact the artifact did not
// record, while an absent object name is a fact it did.
func TestIdentityChangesObjectNames(t *testing.T) {
	tests := []struct {
		name string
		from Identity
		to   Identity
		want []IdentityChange
	}{
		{
			name: "object name dropped",
			from: Identity{Version: "v0.19.0", ObjectNames: map[string]string{"fullnameOverride": "op"}},
			to:   Identity{Version: "v0.19.0", ObjectNames: map[string]string{}},
			want: []IdentityChange{{Field: "fullnameOverride", From: "op", To: ""}},
		},
		{
			name: "object name added",
			from: Identity{Version: "v0.19.0", ObjectNames: map[string]string{}},
			to:   Identity{Version: "v0.19.0", ObjectNames: map[string]string{"fullnameOverride": "op"}},
			want: []IdentityChange{{Field: "fullnameOverride", From: "", To: "op"}},
		},
		{
			name: "object name changed",
			from: Identity{ObjectNames: map[string]string{"fullnameOverride": "old"}},
			to:   Identity{ObjectNames: map[string]string{"fullnameOverride": "new"}},
			want: []IdentityChange{{Field: "fullnameOverride", From: "old", To: "new"}},
		},
		{
			name: "an unchanged object name records nothing",
			from: Identity{ObjectNames: map[string]string{"fullnameOverride": "same"}},
			to:   Identity{ObjectNames: map[string]string{"fullnameOverride": "same"}},
			want: nil,
		},
		{
			name: "nil on both sides records nothing",
			from: Identity{Version: "v1"},
			to:   Identity{Version: "v1"},
			want: nil,
		},
		{
			name: "namespace leads, object names follow in path order",
			from: Identity{
				Namespace:   "skyhook",
				ObjectNames: map[string]string{"grafana.fullnameOverride": "g1", "fullnameOverride": "f1"},
			},
			to: Identity{
				Namespace:   "nodewright",
				ObjectNames: map[string]string{"grafana.fullnameOverride": "g2", "fullnameOverride": "f2"},
			},
			want: []IdentityChange{
				{Field: "namespace", From: "skyhook", To: "nodewright"},
				{Field: "fullnameOverride", From: "f1", To: "f2"},
				{Field: "grafana.fullnameOverride", From: "g1", To: "g2"},
			},
		},
		{
			name: "only the paths that moved are reported",
			from: Identity{ObjectNames: map[string]string{"a.fullnameOverride": "x", "b.nameOverride": "y"}},
			to:   Identity{ObjectNames: map[string]string{"a.fullnameOverride": "x", "b.nameOverride": "z"}},
			want: []IdentityChange{{Field: "b.nameOverride", From: "y", To: "z"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := identityChanges(tt.from, tt.to)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("identityChanges() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestIdentityAdviceMatchesAxis pins the advice to the axis that earned it.
// The two consequences differ, and this prose is what an operator reads at the
// moment they decide whether to upgrade: a namespace move leaves a second copy
// running, a rename is applied as delete-and-recreate.
func TestIdentityAdviceMatchesAxis(t *testing.T) {
	nsOnly := []IdentityChange{{Field: "namespace", From: "skyhook", To: "nodewright"}}
	nameOnly := []IdentityChange{{Field: "fullnameOverride", From: "skyhook-operator", To: ""}}
	both := []IdentityChange{nsOnly[0], nameOnly[0]}

	tests := []struct {
		name       string
		moved      []IdentityChange
		wantSubstr []string
		wantAbsent []string
	}{
		{
			name:       "namespace only names the duplicate install",
			moved:      nsOnly,
			wantSubstr: []string{"second copy"},
			wantAbsent: []string{"delete-and-recreate", "spec.selector"},
		},
		{
			name:       "object name only names the recreate",
			moved:      nameOnly,
			wantSubstr: []string{"delete-and-recreate", "spec.selector"},
			wantAbsent: []string{"second copy"},
		},
		{
			name:       "both axes name both",
			moved:      both,
			wantSubstr: []string{"second copy", "delete-and-recreate"},
		},
		{
			// Every Identity field but namespace used to read as an object
			// name, so a chart swap was told it was a rename.
			name:       "a chart move names different objects, not a rename",
			moved:      []IdentityChange{{Field: "chart", From: "skyhook", To: "nodewright"}},
			wantSubstr: []string{"different objects"},
			wantAbsent: []string{"delete-and-recreate", "spec.selector", "second copy", "GitOps prune"},
		},
		{
			name: "a dropped manifest names the prune, not a rename",
			moved: []IdentityChange{{
				Field: "manifestFiles", From: "a.yaml,b.yaml", To: "a.yaml", Removed: []string{"b.yaml"},
			}},
			wantSubstr: []string{"GitOps prune"},
			wantAbsent: []string{"delete-and-recreate", "different objects"},
		},
		{
			name: "an added manifest names new objects, not a prune",
			moved: []IdentityChange{{
				Field: "manifestFiles", From: "a.yaml", To: "a.yaml,b.yaml", Added: []string{"b.yaml"},
			}},
			wantSubstr: []string{"different objects"},
			wantAbsent: []string{"GitOps prune", "delete-and-recreate"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := relocation("nodewright-operator", "v0.19.0", "v0.19.0", tt.moved).Explanation
			for _, want := range tt.wantSubstr {
				if !strings.Contains(got, want) {
					t.Errorf("explanation %q is missing %q", got, want)
				}
			}
			for _, absent := range tt.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("explanation %q should not mention %q", got, absent)
				}
			}
		})
	}
}

// TestMovedPhraseNamesAnAppearingOrDisappearingName keeps the prose readable
// where one side is empty. "moves from skyhook-operator to" names nothing, on
// the row where the rename is the whole finding.
func TestMovedPhraseNamesAnAppearingOrDisappearingName(t *testing.T) {
	tests := []struct {
		name  string
		moved []IdentityChange
		want  string
	}{
		{
			name:  "dropped",
			moved: []IdentityChange{{Field: "fullnameOverride", From: "skyhook-operator", To: ""}},
			want:  "its fullnameOverride is no longer set, dropping skyhook-operator",
		},
		{
			name:  "added",
			moved: []IdentityChange{{Field: "fullnameOverride", From: "", To: "nodewright"}},
			want:  "its fullnameOverride is now set to nodewright",
		},
		{
			name:  "changed",
			moved: []IdentityChange{{Field: "namespace", From: "skyhook", To: "nodewright"}},
			want:  "its namespace moves from skyhook to nodewright",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := movedPhrase(tt.moved); got != tt.want {
				t.Errorf("movedPhrase() = %q, want %q", got, tt.want)
			}
		})
	}
}
