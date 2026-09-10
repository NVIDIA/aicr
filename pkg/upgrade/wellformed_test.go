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
	"reflect"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
)

// tr builds a minimally valid transition for mutation in tests.
func tr(mut func(*Transition)) Transition {
	t := Transition{
		From:    "<0.18.0",
		To:      ">=0.18.0 <=0.18.0",
		Verdict: VerdictManual,
		Summary: "the rename",
		StepsByDeployer: []StepGroup{
			{Steps: []Step{{ID: "rename", Description: "do the thing"}}},
		},
	}
	if mut != nil {
		mut(&t)
	}
	return t
}

// rec wraps transitions into a record and a matching Component.
func rec(pin string, trs ...Transition) (Set, []Component) {
	u := &ComponentUpgrades{Component: "c", Transitions: trs}
	return Set{"c": u}, []Component{{Name: "c", File: "upgrades/c.yaml", PinnedVersion: pin}}
}

// mentions reports whether err names any of the given message fragments.
//
// Rules 2, 3 and 7 constrain the same two ranges from three directions, so a
// fixture that isolates one of them is often not well-formed under another —
// a `to` ceiling below the pin is legal under rule 2 and always leaves a rule
// 3 hole, for instance. A table that asserts on its own rule's wording stays
// about that rule; asserting err != nil would make it a referendum on all
// three at once.
func mentions(err error, fragments ...string) bool {
	if err == nil {
		return false
	}
	for _, f := range fragments {
		if strings.Contains(err.Error(), f) {
			return true
		}
	}
	return false
}

// The message fragments checkPinCeiling and checkDirectional can emit.
var (
	rule2Fragments = []string{
		"reaches past", "to range with no upper bound", "to range with no lower bound",
		"not a comparable version", "which carries build metadata", "unparseable to range",
	}
	rule7Fragments = []string{
		"matches in reverse", "cannot be shown to be forward-only",
		"matches no version", "unparseable from range",
	}
)

func TestValidateVerdictFields(t *testing.T) {
	tests := []struct {
		name       string
		transition Transition
		wantErr    bool
		wantText   string
	}{
		{"manual with a step passes", tr(nil), false, ""},
		{
			"safe without verifiedBy fails",
			tr(func(x *Transition) { x.Verdict = VerdictSafe; x.StepsByDeployer = nil }),
			true, "verifiedBy",
		},
		{
			"safe with verifiedBy and no steps passes",
			tr(func(x *Transition) {
				x.Verdict = VerdictSafe
				x.VerifiedBy = "uat lane eks-h100-training"
				x.StepsByDeployer = nil
			}),
			false, "",
		},
		{
			"safe carrying steps fails",
			tr(func(x *Transition) { x.Verdict = VerdictSafe; x.VerifiedBy = "uat lane" }),
			true, "must not carry steps",
		},
		{
			"safe may carry hooks",
			tr(func(x *Transition) {
				x.Verdict = VerdictSafe
				x.VerifiedBy = "uat lane"
				x.StepsByDeployer = nil
				x.Hooks = []Hook{{File: "manifests/migrations/adopt.yaml", Phase: "pre-upgrade"}}
			}),
			false, "",
		},
		{
			"manual with no steps fails",
			tr(func(x *Transition) { x.StepsByDeployer = nil }),
			true, "at least one step",
		},
		{
			"blocked with no steps fails",
			tr(func(x *Transition) { x.Verdict = VerdictBlocked; x.StepsByDeployer = nil }),
			true, "at least one step",
		},
		{
			"reversible true without notes fails",
			tr(func(x *Transition) { b := true; x.Reversible = &b }),
			true, "reversibleNotes",
		},
		{
			// ADR-021's worked example carries reversible: false and no notes.
			// Requiring them there rejects the record first-record authors copy.
			"reversible false without notes passes",
			tr(func(x *Transition) { b := false; x.Reversible = &b }),
			false, "",
		},
		{
			"duplicate step id within a group fails",
			tr(func(x *Transition) {
				x.StepsByDeployer[0].Steps = append(x.StepsByDeployer[0].Steps,
					Step{ID: "rename", Description: "again"})
			}),
			true, "duplicate step id",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set, comps := rec("v0.18.0", tt.transition)
			err := set.Validate(comps)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.wantText)
			}
		})
	}
}

// Validate is exported on an exported map type, so a Set can be built without
// ever going through Load. Every rule keyed to a verdict reads as satisfied
// when the verdict itself is garbage, and nothing else in wellformed.go looks
// at summary — so deleting either of Load's own gates left the whole pipeline
// green on a record that asserts nothing usable.
func TestValidateIsSelfSufficientWithoutLoad(t *testing.T) {
	tests := []struct {
		name       string
		transition Transition
		wantText   string
	}{
		{
			"a verdict Load would never admit",
			Transition{From: "<0.18.0", To: ">=0.18.0 <=0.18.0", Verdict: Verdict("yolo"), Summary: "s"},
			"only safe, manual and blocked may be authored",
		},
		{
			"the computed unknown verdict",
			Transition{From: "<0.18.0", To: ">=0.18.0 <=0.18.0", Verdict: VerdictUnknown, Summary: "s"},
			"only safe, manual and blocked may be authored",
		},
		{
			"the computed unversioned verdict",
			Transition{From: "<0.18.0", To: ">=0.18.0 <=0.18.0", Verdict: VerdictUnversioned, Summary: "s"},
			"only safe, manual and blocked may be authored",
		},
		{
			"no summary",
			tr(func(x *Transition) { x.Summary = "" }),
			"is missing summary",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set, comps := rec("v0.18.0", tt.transition)
			err := set.Validate(comps)
			if err == nil {
				t.Fatal("Validate = nil, want a violation")
			}
			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.wantText)
			}
		})
	}
}

// The same holds for a replaces block, which Load gates separately.
func TestValidateReplacesIsSelfSufficientWithoutLoad(t *testing.T) {
	set := Set{"c": &ComponentUpgrades{
		Component: "c",
		Replaces:  &Replaces{Component: "old", Verdict: VerdictUnknown, Summary: "superseded"},
	}}
	comps := []Component{{Name: "c", File: "upgrades/c.yaml", PinnedVersion: "v1.0.0"}}
	err := set.Validate(comps)
	if err == nil {
		t.Fatal("Validate = nil, want a violation for a non-authorable replaces verdict")
	}
	if !strings.Contains(err.Error(), "only safe, manual and blocked may be authored") {
		t.Errorf("error %q does not name the non-authorable verdict", err.Error())
	}
}

// One violation per problem: checkReplaces used to repeat checkVerdictFields'
// summary check, and an author counting violations should not see the same
// one twice.
func TestValidateReportsAMissingReplacesSummaryOnce(t *testing.T) {
	set := Set{"c": &ComponentUpgrades{
		Component: "c",
		Replaces: &Replaces{
			Component: "old", Verdict: VerdictManual,
			StepsByDeployer: []StepGroup{{Steps: []Step{{ID: "swap", Description: "swap it"}}}},
		},
	}}
	comps := []Component{{Name: "c", File: "upgrades/c.yaml", PinnedVersion: "v1.0.0"}}
	err := set.Validate(comps)
	if err == nil {
		t.Fatal("Validate = nil, want a missing-summary violation")
	}
	if got := strings.Count(err.Error(), "is missing summary"); got != 1 {
		t.Errorf("error mentions %q %d time(s), want exactly 1: %s", "is missing summary", got, err.Error())
	}
}

// An author should see every problem in one run, not one CI cycle at a time.
func TestValidateAggregatesViolations(t *testing.T) {
	bad := tr(func(x *Transition) {
		x.Verdict = VerdictSafe // no verifiedBy (rule 4) AND carries steps (rule 5)
		b := true
		x.Reversible = &b // no reversibleNotes
	})
	set, comps := rec("v0.18.0", bad)
	err := set.Validate(comps)
	if err == nil {
		t.Fatal("Validate = nil, want violations")
	}
	for _, want := range []string{"verifiedBy", "must not carry steps", "reversibleNotes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("aggregate error %q does not mention %q", err.Error(), want)
		}
	}
}

// Violations are grouped per component, and the grouping is only stable
// because Validate sorts the names before walking the map. Every other test
// here uses a single-key Set, which cannot see the difference. The loop runs
// the walk repeatedly: a single range over a small map is a random rotation of
// its slots, so one pass can land on ascending order by luck.
func TestValidateOrdersComponentsDeterministically(t *testing.T) {
	bad := func(name string) *ComponentUpgrades {
		return &ComponentUpgrades{
			Component:   name,
			Transitions: []Transition{tr(func(x *Transition) { x.Summary = "" })},
		}
	}
	set := Set{"a": bad("a"), "b": bad("b"), "c": bad("c")}
	comps := []Component{
		{Name: "a", File: "upgrades/a.yaml", PinnedVersion: "v0.18.0"},
		{Name: "b", File: "upgrades/b.yaml", PinnedVersion: "v0.18.0"},
		{Name: "c", File: "upgrades/c.yaml", PinnedVersion: "v0.18.0"},
	}
	for i := range 50 {
		err := set.Validate(comps)
		if err == nil {
			t.Fatal("Validate = nil, want violations from all three components")
		}
		ai := strings.Index(err.Error(), `component "a"`)
		bi := strings.Index(err.Error(), `component "b"`)
		ci := strings.Index(err.Error(), `component "c"`)
		if ai < 0 || bi < 0 || ci < 0 {
			t.Fatalf("run %d: not every component is reported: %s", i, err.Error())
		}
		if ai >= bi || bi >= ci {
			t.Fatalf("run %d: components reported at offsets a=%d b=%d c=%d, want ascending: %s",
				i, ai, bi, ci, err.Error())
		}
	}
}

// The heavy import lives here, never in the package itself.
func TestCanonicalDeployersMatchBundlerConfig(t *testing.T) {
	if got, want := canonicalDeployers, config.GetDeployerTypes(); !reflect.DeepEqual(got, want) {
		t.Errorf("canonicalDeployers = %v, want %v (a new deployer must be added here too)", got, want)
	}
}

func TestValidateStepGroups(t *testing.T) {
	group := func(deployers []string) StepGroup {
		return StepGroup{Deployers: deployers, Steps: []Step{{ID: "s", Description: "d"}}}
	}
	tests := []struct {
		name     string
		groups   []StepGroup
		wantErr  bool
		wantText string
	}{
		{"single remainder group covers everything", []StepGroup{group(nil)}, false, ""},
		{
			"explicit groups covering all five pass",
			[]StepGroup{group([]string{"argocd", "argocd-helm", "flux"}), group([]string{"helm", "helmfile"})},
			false, "",
		},
		{
			"explicit group plus remainder passes",
			[]StepGroup{group([]string{"argocd"}), group(nil)},
			false, "",
		},
		{
			"two remainder groups fail",
			[]StepGroup{group(nil), group(nil)},
			true, "more than one group omits deployers",
		},
		{
			"overlapping explicit groups fail",
			[]StepGroup{group([]string{"argocd", "flux"}), group([]string{"flux", "helm"})},
			true, "claimed by more than one group",
		},
		{
			"duplicate deployer within one group fails",
			[]StepGroup{group([]string{"flux", "flux"}), group(nil)},
			true, "listed twice",
		},
		{
			"unknown deployer fails",
			[]StepGroup{group([]string{"argo"}), group(nil)},
			true, "not a selectable deployer",
		},
		{
			"localformat is not selectable",
			[]StepGroup{group([]string{"localformat"}), group(nil)},
			true, "not a selectable deployer",
		},
		{
			"manual verdict leaving a deployer uncovered fails",
			[]StepGroup{group([]string{"argocd", "flux"})},
			true, "no steps for deployer",
		},
		{
			// Rule 5 counted a per-transition total, so this passed while a
			// helm operator got a manual verdict with an empty step list.
			"covered group with an empty steps list fails",
			[]StepGroup{
				group([]string{"argocd", "argocd-helm", "flux"}),
				{Deployers: []string{"helm", "helmfile"}, Steps: nil},
			},
			true, "carries no steps",
		},
		{
			"explicitly empty deployers list is not the remainder",
			[]StepGroup{group([]string{"argocd", "argocd-helm", "flux"}),
				{Deployers: []string{}, Steps: []Step{{ID: "s", Description: "d"}}}},
			true, "omit the key entirely",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set, comps := rec("v0.18.0", tr(func(x *Transition) { x.StepsByDeployer = tt.groups }))
			err := set.Validate(comps)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.wantText)
			}
		})
	}
}

func TestValidatePinCeiling(t *testing.T) {
	tests := []struct {
		name     string
		to       string
		pin      string
		wantErr  bool
		wantText string
	}{
		{"ceiling equals the pin", ">=0.18.0 <=0.18.0", "v0.18.0", false, ""},
		{"ceiling below the pin", ">=0.17.0 <=0.17.9", "v0.18.0", false, ""},
		{"ADR ordinary idiom", ">=25.0.0 <=25.3.0", "v25.3.0", false, ""},
		{"ceiling above the pin", ">=0.18.0 <=0.20.0", "v0.18.0", true, "reaches past"},
		{"exclusive ceiling above the pin", ">=0.18.0 <0.20.0", "v0.18.0", true, "reaches past"},
		{"unbounded above fails", ">=0.18.0", "v0.18.0", true, "upper bound"},
		{"unbounded below fails", "<=0.18.0", "v0.18.0", true, "lower bound"},
		{"non-semver pin fails", ">=0.18.0 <=0.18.0", "main", true, "not a comparable version"},
		{"commit sha pin fails", ">=0.18.0 <=0.18.0", "9f8e7d6c5b4a", true, "not a comparable version"},
		{"build metadata pin fails", ">=0.18.0 <=0.18.0", "v0.18.0+build.5", true, "build metadata"},
		{"prerelease pin with matching prerelease ceiling passes",
			">=0.1.0-alpha.1 <=0.1.0-alpha.12", "v0.1.0-alpha.12", false, ""},
		{"prerelease pin with release ceiling fails",
			">=0.1.0-alpha.1 <=0.1.0", "v0.1.0-alpha.12", true, "reaches past"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set, comps := rec(tt.pin, tr(func(x *Transition) {
				x.To = tt.to
				x.From = "<0.0.1"
			}))
			err := set.Validate(comps)
			if got := mentions(err, rule2Fragments...); got != tt.wantErr {
				t.Fatalf("rule 2 violation = %v, want %v (err: %v)", got, tt.wantErr, err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.wantText)
			}
		})
	}
}

// Rule 2 fires per transition. A record carrying only a `replaces` block has no `to` to compare
// and must not be failed for lacking one.
func TestValidatePinCeilingSkipsReplacesOnlyRecord(t *testing.T) {
	u := &ComponentUpgrades{
		Component: "c",
		Replaces: &Replaces{
			Component: "old", Verdict: VerdictManual, Summary: "superseded",
			StepsByDeployer: []StepGroup{{Steps: []Step{{ID: "swap", Description: "swap it"}}}},
		},
	}
	set := Set{"c": u}
	comps := []Component{{Name: "c", File: "upgrades/c.yaml", PinnedVersion: "main"}}
	if err := set.Validate(comps); err != nil {
		t.Errorf("Validate error = %v, want nil for a replaces-only record", err)
	}
}

func TestValidateDirectional(t *testing.T) {
	tests := []struct {
		name     string
		from     string
		to       string
		wantErr  bool
		wantText string
	}{
		{"exclusive from meets inclusive to", "<0.18.0", ">=0.18.0 <=0.18.0", false, ""},
		{"clear separation", "<0.17.0", ">=0.18.0 <=0.18.0", false, ""},
		{"inclusive from against exclusive to floor", "<=0.18.0", ">0.18.0 <=0.19.0", false, ""},
		{"exclusive from against exclusive to floor", "<0.18.0", ">0.18.0 <=0.19.0", false, ""},
		{
			"both inclusive at the same version overlaps",
			"<=0.18.0", ">=0.18.0 <=0.18.0", true, "matches in reverse",
		},
		{
			"from reaching above the to floor overlaps",
			"<0.19.0", ">=0.18.0 <=0.18.0", true, "matches in reverse",
		},
		{
			"from with no upper bound fails",
			">=0.16.0", ">=0.18.0 <=0.18.0", true, "upper bound",
		},
		{
			// Pins the ferr branch: an unparseable from is reported here, by
			// checkDirectional itself, not silently skipped.
			"unparseable from is reported here",
			"^0.18.0", ">=0.18.0 <=0.18.0", true, "unparseable from range",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set, comps := rec("v0.19.0", tr(func(x *Transition) { x.From = tt.from; x.To = tt.to }))
			err := set.Validate(comps)
			if got := mentions(err, rule7Fragments...); got != tt.wantErr {
				t.Fatalf("rule 7 violation = %v, want %v (err: %v)", got, tt.wantErr, err)
			}
			if tt.wantErr && !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.wantText)
			}
		})
	}
}

// Pins the terr branch: an unparseable to must be reported exactly once, by
// checkPinCeiling alone. strings.Contains would pass even if checkDirectional
// duplicated the report, which is precisely the failure the terr != nil
// early return exists to prevent.
func TestValidateDirectionalUnparseableToReportedOnce(t *testing.T) {
	set, comps := rec("v0.19.0", tr(func(x *Transition) {
		x.From = "<0.18.0"
		x.To = "^0.20.0"
	}))
	err := set.Validate(comps)
	if err == nil {
		t.Fatal("Validate error = nil, want a violation for an unparseable to range")
	}
	if got := strings.Count(err.Error(), "unparseable to range"); got != 1 {
		t.Errorf("error mentions %q %d time(s), want exactly 1 (checkDirectional must not duplicate checkPinCeiling's report): %s",
			"unparseable to range", got, err.Error())
	}
}

// parseBounds treats an empty or contradictory `from` interval (e.g.
// ">=0.20.0 <0.18.0") as harmless: both range representations agree it
// matches nothing. checkDirectional compares upper(from) against lower(to),
// so such a from must be rejected explicitly, or a record that can never
// apply would sail through the disjointness check as spuriously "safe".
func TestValidateDirectionalRejectsEmptyFromRange(t *testing.T) {
	tests := []struct {
		name string
		from string
	}{
		{"lower above upper", ">=0.20.0 <0.18.0"},
		{"equal with inclusive lower, exclusive upper", ">=0.18.0 <0.18.0"},
		{"equal with both exclusive", ">0.18.0 <0.18.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set, comps := rec("v0.19.0", tr(func(x *Transition) {
				x.From = tt.from
				x.To = ">=0.18.0 <=0.18.0"
			}))
			err := set.Validate(comps)
			if err == nil {
				t.Fatal("Validate error = nil, want a violation for an empty from range")
			}
			if !strings.Contains(err.Error(), "matches no version") {
				t.Errorf("error %q does not mention %q", err.Error(), "matches no version")
			}
		})
	}
}

func TestValidateCoverage(t *testing.T) {
	// Every case shares one `to` across its transitions, so a multi-transition
	// case also violates rule 8. That is deliberate and harmless here: the
	// assertion greps rule 3's own wording, and pinning the `to` keeps rules 2
	// and 7 constant while only the `from` domains vary.
	froms := func(to string, fs ...string) []Transition {
		out := make([]Transition, 0, len(fs))
		for _, f := range fs {
			x := tr(nil)
			x.From = f
			x.To = to
			out = append(out, x)
		}
		return out
	}
	tests := []struct {
		name    string
		from    []string
		to      string
		pin     string
		wantErr bool
	}{
		{"single transition reaching the pin", []string{"<0.20.0"}, ">=0.20.0 <=0.20.0", "v0.20.0", false},
		{
			// A lone transition has no interior to hole, but it still has to
			// reach the pin: everything in [0.18.0, 0.20.0) matches nothing.
			"single transition stopping short of the pin",
			[]string{"<0.18.0"}, ">=0.20.0 <=0.20.0", "v0.20.0", true,
		},
		{
			// The shape a record has right after a pin bump nobody extended it
			// for: the intervals are contiguous with each other and the walk
			// alone sees no problem, but [0.19.0, 0.30.0) matches nothing.
			"contiguous intervals stopping short of the pin",
			[]string{"<0.18.0", "<0.19.0"}, ">=0.20.0 <=0.20.0", "v0.30.0", true,
		},
		{"contiguous halves", []string{"<0.18.0", ">=0.18.0 <0.20.0"}, ">=0.20.0 <=0.20.0", "v0.20.0", false},
		{"wholly contained range is not a hole", []string{"<0.20.0", "<0.18.0"}, ">=0.20.0 <=0.20.0", "v0.20.0", false},
		{
			"ADR ordinary idiom does not require coverage from zero",
			[]string{">=25.0.0 <26.0.0"}, ">=26.0.0 <=26.0.0", "v26.0.0", false,
		},
		{
			"two blocks in one major line, no interior hole",
			[]string{">=25.0.0 <25.2.0", ">=25.2.0 <26.0.0"}, ">=26.0.0 <=26.0.0", "v26.0.0", false,
		},
		{
			"gap between non-adjacent domains",
			[]string{"<0.18.0", ">=0.19.0 <0.20.0"}, ">=0.20.0 <=0.20.0", "v0.20.0", true,
		},
		{
			"one-version hole at a shared exclusive boundary",
			[]string{"<0.18.0", ">0.18.0 <0.20.0"}, ">=0.20.0 <=0.20.0", "v0.20.0", true,
		},
		{
			// A blind cur = next.upper (instead of a running maximum) would
			// drop coverage back to 0.18.0 after the second interval and
			// report a spurious hole before 0.20.0.
			"running maximum merge across three intervals, no interior hole",
			[]string{"<0.20.0", "<0.18.0", ">=0.20.0 <0.22.0"}, ">=0.22.0 <=0.22.0", "v0.22.0", false,
		},
		{
			// Both floors are exactly 0.20.0; only the running-maximum
			// upper's inclusivity tie-break tells the merge that <=0.20.0
			// (not <0.20.0) is the one that reaches the third interval.
			"upperAfter inclusivity tie-break at equal floors, no interior hole",
			[]string{"<=0.20.0", "<0.20.0", ">0.20.0 <0.22.0"}, ">=0.22.0 <=0.22.0", "v0.22.0", false,
		},
		{
			// Deliberately unsorted input (0.10-0.15, 0.20-0.25, 0.15-0.20):
			// only sort.Slice + lowerBefore's total order puts these back in
			// ascending-floor order before the walk; processed as given, the
			// walk would see the third interval's 0.15 floor arrive after
			// the second interval's 0.25 ceiling and misreport a hole.
			"unsorted intervals still resolve to no interior hole",
			[]string{">=0.10.0 <0.15.0", ">=0.20.0 <0.25.0", ">=0.15.0 <0.20.0"}, ">=0.25.0 <=0.25.0", "v0.25.0", false,
		},
		{
			// The other three cases never compare two equal concrete
			// floors, so lowerBefore's cmp==0 tie-break line is never
			// evaluated by them: distinct-version pairs return from the
			// cmp!=0 branch first, and unbounded-lower pairs return from
			// the a.unbounded branch first. Here ">=0.20.0" and ">0.20.0"
			// share floor 0.20.0 with different inclusivity: the correct
			// order sorts the inclusive one first, so it bridges "<0.20.0"
			// and ">0.20.0 <0.22.0" at the single point 0.20.0. A flipped
			// tie-break reorders them, drops the bridging interval to last,
			// and reports a spurious hole at that point.
			"lowerBefore inclusivity tie-break at equal floors, no interior hole",
			[]string{"<0.20.0", ">=0.20.0 <0.21.0", ">0.20.0 <0.22.0"}, ">=0.22.0 <=0.22.0", "v0.22.0", false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := Set{"c": &ComponentUpgrades{Component: "c", Transitions: froms(tt.to, tt.from...)}}
			comps := []Component{{Name: "c", File: "upgrades/c.yaml", PinnedVersion: tt.pin}}
			err := set.Validate(comps)
			hasHole := err != nil && strings.Contains(err.Error(), "no record describes")
			if hasHole != tt.wantErr {
				t.Fatalf("coverage violation = %v, want %v (err: %v)", hasHole, tt.wantErr, err)
			}
		})
	}
}

// Rule 3 runs "up to the pin". Comparing consecutive intervals alone stops at
// the highest from ceiling the file names, which accepts a record whose
// coverage ends below the pin and strands every version in between. The
// message must name the pin, or an author cannot tell a trailing hole from an
// interior one.
func TestValidateCoverageNamesThePinOnATrailingHole(t *testing.T) {
	set, comps := rec("v0.30.0", tr(func(x *Transition) {
		x.From = "<0.19.0"
		x.To = ">=0.20.0 <=0.20.0"
	}))
	err := set.Validate(comps)
	if err == nil {
		t.Fatal("Validate = nil, want a trailing coverage hole up to the pin")
	}
	for _, want := range []string{"0.19.0", "pinned version 0.30.0", "no record describes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestValidateReplaces(t *testing.T) {
	base := func(mut func(*Replaces)) *Replaces {
		r := &Replaces{
			Component: "old-operator",
			Verdict:   VerdictManual,
			Summary:   "superseded by this component",
			StepsByDeployer: []StepGroup{
				{Steps: []Step{{ID: "swap", Description: "uninstall old, install new"}}},
			},
		}
		if mut != nil {
			mut(r)
		}
		return r
	}
	tests := []struct {
		name     string
		replaces *Replaces
		wantErr  bool
		wantText string
	}{
		{"manual with steps passes", base(nil), false, ""},
		{
			"safe without verifiedBy fails (rule 4 applies)",
			base(func(r *Replaces) { r.Verdict = VerdictSafe; r.StepsByDeployer = nil }),
			true, "verifiedBy",
		},
		{
			"manual with no steps fails (rule 5 applies)",
			base(func(r *Replaces) { r.StepsByDeployer = nil }),
			true, "at least one step",
		},
		{
			"unknown deployer fails (rule 6 applies)",
			base(func(r *Replaces) { r.StepsByDeployer[0].Deployers = []string{"argo"} }),
			true, "not a selectable deployer",
		},
		{
			"missing summary fails",
			base(func(r *Replaces) { r.Summary = "" }),
			true, "summary",
		},
		{
			"missing component fails",
			base(func(r *Replaces) { r.Component = "" }),
			true, "names no component",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := Set{"c": &ComponentUpgrades{Component: "c", Replaces: tt.replaces}}
			comps := []Component{{Name: "c", File: "upgrades/c.yaml", PinnedVersion: "v1.0.0"}}
			err := set.Validate(comps)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.wantText)
			}
		})
	}
}

func TestValidateDistinctBoundaries(t *testing.T) {
	mk := func(from, to string) Transition {
		x := tr(nil)
		x.From, x.To = from, to
		return x
	}
	tests := []struct {
		name    string
		trs     []Transition
		wantErr bool
	}{
		{
			"ADR block-spanning pair is legitimate",
			[]Transition{
				mk("<0.18.0", ">=0.18.0 <0.20.0"),
				mk("<0.20.0", ">=0.20.0 <=0.20.0"),
			},
			false,
		},
		{
			"exact duplicate transitions",
			[]Transition{
				mk("<0.18.0", ">=0.18.0 <=0.18.0"),
				mk("<0.18.0", ">=0.18.0 <=0.18.0"),
			},
			true,
		},
		{
			"same boundary reached by different from ranges",
			[]Transition{
				mk("<0.18.0", ">=0.18.0 <=0.18.0"),
				mk("<0.17.0", ">=0.18.0 <=0.18.0"),
			},
			true,
		},
		{
			// Two blocks reaching one boundary. Under ADR:182's applies
			// predicate at most one of these can match any given source, so
			// the "would resolve to blocked" claim does not hold and rule 8
			// must not reject the pair.
			"same boundary from disjoint from ranges is not a duplicate",
			[]Transition{
				mk("<0.18.0", ">=0.20.0 <=0.20.0"),
				mk(">=0.18.0 <0.20.0", ">=0.20.0 <=0.20.0"),
			},
			false,
		},
		{
			// The from domains touch at exactly 0.18.0, so one source version
			// does match both, and the pair is a duplicate after all.
			"same boundary from ranges sharing a single version is a duplicate",
			[]Transition{
				mk("<=0.18.0", ">=0.20.0 <=0.20.0"),
				mk(">=0.18.0 <0.20.0", ">=0.20.0 <=0.20.0"),
			},
			true,
		},
		{
			// A prerelease tag that reads as the exclusivity marker must not
			// collide with it: an exclusive floor at 1.0.0 and an inclusive
			// floor at the distinct version 1.0.0-exclusive are not the same
			// boundary, even though naive string concatenation of version and
			// marker would produce the same key for both.
			"exclusive floor does not collide with a same-named prerelease floor",
			[]Transition{
				mk("<1.0.0", ">1.0.0 <2.0.0"),
				mk("<1.0.0", ">=1.0.0-exclusive <2.0.0"),
			},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set := Set{"c": &ComponentUpgrades{Component: "c", Transitions: tt.trs}}
			comps := []Component{{Name: "c", File: "upgrades/c.yaml", PinnedVersion: "v0.20.0"}}
			err := set.Validate(comps)
			got := err != nil && strings.Contains(err.Error(), "same boundary")
			if got != tt.wantErr {
				t.Fatalf("duplicate-boundary violation = %v, want %v (err: %v)", got, tt.wantErr, err)
			}
		})
	}
}

func TestValidateHooks(t *testing.T) {
	tests := []struct {
		name     string
		hooks    []Hook
		wantErr  bool
		wantText string
	}{
		{"pre-upgrade", []Hook{{File: "manifests/migrations/a.yaml", Phase: "pre-upgrade"}}, false, ""},
		{"post-upgrade", []Hook{{File: "manifests/migrations/a.yaml", Phase: "post-upgrade"}}, false, ""},
		{"empty hook", []Hook{{}}, true, "phase"},
		{"typo'd phase", []Hook{{File: "manifests/migrations/a.yaml", Phase: "pre-upgrde"}}, true, "phase"},
		{"missing file", []Hook{{Phase: "pre-upgrade"}}, true, "file"},
		{"absolute path", []Hook{{File: "/etc/passwd", Phase: "pre-upgrade"}}, true, "must be a local path"},
		{"traversal", []Hook{{File: "../../etc/passwd", Phase: "pre-upgrade"}}, true, "must be a local path"},
		{
			// Local and traversal-free, but outside the only tree tools/bom
			// walks, so its image would be unpinned and BOM-invisible.
			"local path outside the migrations tree",
			[]Hook{{File: "values.yaml", Phase: "pre-upgrade"}}, true, "must live under manifests/migrations/",
		},
		{
			"a sibling manifests directory is not the migrations tree",
			[]Hook{{File: "manifests/adopt.yaml", Phase: "pre-upgrade"}}, true, "must live under manifests/migrations/",
		},
		{
			// IsLocal accepts this: the ".." never climbs above the bundle
			// root. But a raw-string prefix test would still see the literal
			// "manifests/migrations/" prefix and accept a hook that actually
			// resolves outside the tree entirely.
			"traversal within an accepted prefix escapes the migrations tree",
			[]Hook{{File: "manifests/migrations/../../values.yaml", Phase: "pre-upgrade"}},
			true, "must live under manifests/migrations/",
		},
		{
			"traversal within an accepted prefix resolves to a sibling file",
			[]Hook{{File: "manifests/migrations/../secret.yaml", Phase: "pre-upgrade"}},
			true, "must live under manifests/migrations/",
		},
		{
			"a legitimate hook under the migrations tree still passes",
			[]Hook{{File: "manifests/migrations/adopt.yaml", Phase: "pre-upgrade"}}, false, "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set, comps := rec("v0.18.0", tr(func(x *Transition) { x.Hooks = tt.hooks }))
			err := set.Validate(comps)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.wantText)
			}
		})
	}
}
