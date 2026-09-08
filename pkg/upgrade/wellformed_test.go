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
	"strings"
	"testing"
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
