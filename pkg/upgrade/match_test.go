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
	"strings"
	"testing"
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

// TestMatchCrossingIgnoresFromMembership is the regression this file exists
// for. Both record pairs describe the same two boundaries; only the `from`
// grammar differs. A matcher that requires the source to satisfy `from` before
// a record can apply skips the intermediate block in the bounded-below pair,
// leaves exactly one record applying, and hands over its safe verdict, so a
// jump straight over a recorded block reports safe and exits zero.
func TestMatchCrossingIgnoresFromMembership(t *testing.T) {
	tests := []struct {
		name string
		set  Set
	}{
		{
			name: "from ranges bounded below",
			set: oneComponent(
				trans(">=1.0.0 <2.0.0", ">=2.0.0 <2.1.0", VerdictSafe, "S"),
				trans(">=2.0.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictBlocked, "B"),
			),
		},
		{
			name: "from ranges open below",
			set: oneComponent(
				trans("<2.0.0", ">=2.0.0 <2.1.0", VerdictSafe, "S"),
				trans("<3.0.0", ">=3.0.0 <=3.0.0", VerdictBlocked, "B"),
			),
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
			if r.Reason != ReasonRecordBlocks {
				t.Errorf("reason = %q, want %q", r.Reason, ReasonRecordBlocks)
			}
			if r.StoppedAt != ">=3.0.0 <=3.0.0" {
				t.Errorf("stoppedAt = %q, want the blocked record's to range", r.StoppedAt)
			}
			if r.Transition != nil {
				t.Errorf("transition = %q; two records are crossed, so neither describes the jump",
					r.Transition.Summary)
			}
			if !r.FailsRun() {
				t.Error("FailsRun() = false, want true")
			}
		})
	}
}

// TestMatchVerdictSelection covers the five branches of the selection order and
// every Reason they can produce, including the invariants a blocked result must
// hold: no Transition to render another record's steps from, and a StoppedAt
// the report can name.
// A safe boundary carries no steps, so crossing one composes nothing and skips
// nothing. It must not stop a jump whose only substantive boundary describes
// the whole move -- the nodewright shape, where a manual rename is followed by
// a safe release and a jump spanning both should land on the rename's steps.
func TestMatchSafeBoundaryDoesNotBlockComposition(t *testing.T) {
	rename := trans("<0.18.0", ">=0.18.0 <=0.19.0", VerdictManual, "rename")
	drain := trans(">=0.18.0 <0.19.0", ">=0.19.0 <=0.19.0", VerdictSafe, "drain")
	secondManual := trans(">=0.18.0 <0.19.0", ">=0.19.0 <=0.19.0", VerdictManual, "second")
	firstSafe := trans("<0.18.0", ">=0.18.0 <=0.18.0", VerdictSafe, "first-safe")
	boundedA := trans(">=1.0.0 <1.1.0", ">=1.1.0 <=1.1.0", VerdictSafe, "bounded-A")
	boundedB := trans(">=1.1.0 <1.2.0", ">=1.2.0 <=1.2.0", VerdictSafe, "bounded-B")

	tests := []struct {
		name        string
		set         Set
		from, to    string
		wantVerdict Verdict
		wantReason  Reason
		wantMatched string
	}{
		{
			// Crosses both boundaries. The safe one asks nothing, so the
			// rename's own verdict and steps carry the jump.
			name:        "manual plus safe defers to the manual record",
			set:         oneComponent(rename, drain),
			from:        "0.16.0",
			to:          "0.19.0",
			wantVerdict: VerdictManual,
			wantReason:  ReasonRecorded,
			wantMatched: "rename",
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
			// vouches. The floors are bounded below, which is the ADR's
			// ordinary shape and the only one that exercises this.
			name:        "an unassessed origin is not rescued by crossing more boundaries",
			set:         oneComponent(boundedA, boundedB),
			from:        "0.9.0",
			to:          "1.2.0",
			wantVerdict: VerdictBlocked,
			wantReason:  ReasonMultipleBoundaries,
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
			// The control: two boundaries that both ask something still block,
			// because composing them is the failure the rule exists to prevent.
			name:        "two substantive boundaries still block",
			set:         oneComponent(rename, secondManual),
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

func TestMatchVerdictSelection(t *testing.T) {
	safeLow := trans("<2.0.0", ">=2.0.0 <2.1.0", VerdictSafe, "S")
	blockedHigh := trans("<3.0.0", ">=3.0.0 <=3.0.0", VerdictBlocked, "B")
	blockedMid := trans("<2.5.0", ">=2.5.0 <2.6.0", VerdictBlocked, "M")
	boundedOrigin := trans(">=2.0.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictManual, "O")

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
			name:          "an authored block outranks a lower non-blocking boundary",
			set:           oneComponent(safeLow, blockedHigh),
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
				trans("<3.0.0", ">=3.0.0 <=3.0.0", VerdictBlocked, "B"),
			),
			from: "1.5.0",
			to:   "3.0.0",
			want: "blocked by the record covering 3.0.0: do not move from 1.5.0 into >=3.0.0 <=3.0.0 " +
				"in one step. Upgrade to 3.0.0 first, then re-run this check",
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
				"Upgrade to 2.0.0 first, then re-run this check",
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

// crosses only fires when the source sits below the blocking floor, so advice
// to stop *below* that floor always already held. The floor itself is the
// actionable landing point.
func TestMatchRecordBlocksNamesTheFloorToLandOn(t *testing.T) {
	set := oneComponent(
		trans("<2.0.0", ">=2.0.0 <2.1.0", VerdictSafe, "S"),
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
		if !strings.Contains(got[0].Explanation, "Upgrade to 3.0.0 first") {
			t.Errorf("explanation does not name the floor to land on: %q", got[0].Explanation)
		}
		if strings.Contains(got[0].Explanation, "below 3.0.0") {
			t.Errorf("explanation still advises stopping below the floor, which always already held: %q",
				got[0].Explanation)
		}
		explanations[src] = got[0].Explanation
	}
	if explanations["1.5.0"] == explanations["2.4.9"] {
		t.Errorf("two different sources produced byte-identical advice: %q", explanations["1.5.0"])
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := relocation("nodewright-operator", "v0.19.0", tt.moved).Explanation
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
