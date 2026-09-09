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
			"reversible set without notes fails",
			tr(func(x *Transition) { b := true; x.Reversible = &b }),
			true, "reversibleNotes",
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
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate error = %v, wantErr %v", err, tt.wantErr)
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
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate error = %v, wantErr %v", err, tt.wantErr)
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
	// Each case supplies its own `to` so the other rules stay satisfied and
	// only rule 3 can fail.
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
		{"single transition has no interior", []string{"<0.18.0"}, ">=0.20.0 <=0.20.0", "v0.20.0", false},
		{"contiguous halves", []string{"<0.18.0", ">=0.18.0 <0.20.0"}, ">=0.20.0 <=0.20.0", "v0.20.0", false},
		{"wholly contained range is not a hole", []string{"<0.20.0", "<0.18.0"}, ">=0.20.0 <=0.20.0", "v0.20.0", false},
		{"touching at an inclusive boundary", []string{"<0.18.0", ">=0.18.0 <0.20.0"}, ">=0.20.0 <=0.20.0", "v0.20.0", false},
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
