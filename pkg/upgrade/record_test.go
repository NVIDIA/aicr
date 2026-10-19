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
	"testing"

	"gopkg.in/yaml.v3"
)

func TestVerdictAuthorable(t *testing.T) {
	tests := []struct {
		name    string
		verdict Verdict
		want    bool
	}{
		{"safe", VerdictSafe, true},
		{"manual", VerdictManual, true},
		{"blocked", VerdictBlocked, true},
		{"unknown is computed, never authored", VerdictUnknown, false},
		{"unversioned is computed, never authored", VerdictUnversioned, false},
		{"empty", Verdict(""), false},
		{"garbage", Verdict("probably-fine"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.verdict.Authorable(); got != tt.want {
				t.Errorf("Authorable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestComponentUpgradesDecode(t *testing.T) {
	const doc = `
apiVersion: aicr.run/v1beta1
kind: ComponentUpgrades
component: widget-operator
transitions:
  - from: "<0.18.0"
    to: ">=0.18.0 <=0.18.0"
    verdict: manual
    reversible: true
    reversibleNotes: only until legacy cleanup runs
    summary: widget.example.invalid is renamed to gadget.example.invalid
    precondition: no Widget is mid-rollout
    stepsByDeployer:
      - deployers: [argocd, flux]
        steps:
          - id: rename-crs
            description: rewrite apiVersion and kind in one commit
            reason: splitting it lets auto-sync recreate what you deleted
      - steps:
          - id: rename-crs
            description: rewrite apiVersion and kind, then apply
    affectedResources:
      - group: widget.example.invalid
        kinds: [Widget, DeploymentPolicy]
    references:
      - https://example.invalid/migration
`
	var u ComponentUpgrades
	if err := yaml.Unmarshal([]byte(doc), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if u.Kind != ComponentUpgradesKind {
		t.Errorf("Kind = %q, want %q", u.Kind, ComponentUpgradesKind)
	}
	if len(u.Transitions) != 1 {
		t.Fatalf("transitions = %d, want 1", len(u.Transitions))
	}
	tr := u.Transitions[0]
	if tr.Verdict != VerdictManual {
		t.Errorf("Verdict = %q, want %q", tr.Verdict, VerdictManual)
	}
	if tr.Reversible == nil || !*tr.Reversible {
		t.Errorf("Reversible = %v, want pointer to true", tr.Reversible)
	}
	if len(tr.StepsByDeployer) != 2 {
		t.Fatalf("stepsByDeployer groups = %d, want 2", len(tr.StepsByDeployer))
	}
	if tr.StepsByDeployer[1].Deployers != nil {
		t.Errorf("second group Deployers = %v, want nil (the remainder group)",
			tr.StepsByDeployer[1].Deployers)
	}
	if tr.StepsByDeployer[0].Steps[0].Reason == "" {
		t.Error("Reason did not decode")
	}
}

// Absent `reversible` must stay nil — absent means no claim, not false.
func TestReversibleAbsentIsNil(t *testing.T) {
	const doc = `
apiVersion: aicr.run/v1beta1
kind: ComponentUpgrades
component: widget-operator
transitions:
  - from: "<1.0.0"
    to: ">=1.0.0 <=1.0.0"
    verdict: safe
    verifiedBy: uat-lane-eks-h100
    summary: no operator action required
`
	var u ComponentUpgrades
	if err := yaml.Unmarshal([]byte(doc), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if u.Transitions[0].Reversible != nil {
		t.Errorf("Reversible = %v, want nil when the key is absent", u.Transitions[0].Reversible)
	}
}

func TestComponentUpgradesCovers(t *testing.T) {
	u := &ComponentUpgrades{
		Component: "widget-operator",
		Transitions: []Transition{
			{From: "<0.18.0", To: ">=0.18.0 <=0.19.1", Verdict: VerdictManual},
			{From: ">=0.19.1 <0.20.0", To: "=0.20.0", Verdict: VerdictSafe},
			{From: "<1.0.0", To: "1.0.0-alpha.3", Verdict: VerdictManual},
			{From: "<0.30.0", To: "not a range", Verdict: VerdictSafe},
		},
	}
	tests := []struct {
		name    string
		u       *ComponentUpgrades
		version string
		want    bool
	}{
		{"floor of a closed range", u, "0.18.0", true},
		{"ceiling of a closed range", u, "0.19.1", true},
		{"v prefix", u, "v0.19.0", true},
		{"exact to", u, "0.20.0", true},
		{"prerelease named exactly", u, "1.0.0-alpha.3", true},
		{"between two to ranges", u, "0.19.2", false},
		{"below every to", u, "0.17.9", false},
		{"above every to", u, "0.20.1", false},
		{"a to that does not parse covers nothing", u, "0.30.0", false},
		{"not semver", u, "main", false},
		{"empty version", u, "", false},
		{"no transitions", &ComponentUpgrades{Component: "widget-operator"}, "0.18.0", false},
		{"nil record", nil, "0.18.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.u.Covers(tt.version); got != tt.want {
				t.Errorf("Covers(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

func TestComponentUpgradesAheadOf(t *testing.T) {
	ahead := &ComponentUpgrades{
		Component: "widget-operator",
		Transitions: []Transition{
			{From: "<1.26.0", To: "=1.26.0", Verdict: VerdictManual},
			{From: ">=1.26.0 <1.27.0", To: ">1.27.0 <=1.28.0", Verdict: VerdictManual},
		},
	}
	tests := []struct {
		name    string
		u       *ComponentUpgrades
		version string
		want    bool
	}{
		{"below every floor", ahead, "1.25.3", true},
		{"v prefix", ahead, "v1.25.3", true},
		{"at an inclusive floor", ahead, "1.26.0", false},
		{"between two to ranges", ahead, "1.27.0", false},
		{"above every floor", ahead, "1.29.0", false},
		{"exclusive floor reached only above it", &ComponentUpgrades{Transitions: []Transition{
			{To: ">1.0.0 <=1.1.0"},
		}}, "1.0.0", true},
		{"a to with no floor reaches everything", &ComponentUpgrades{Transitions: []Transition{
			{To: "=2.0.0"}, {To: "<=1.5.0"},
		}}, "0.1.0", false},
		{"a to that does not parse is never ahead", &ComponentUpgrades{Transitions: []Transition{
			{To: "=2.0.0"}, {To: "not a range"},
		}}, "1.0.0", false},
		{"not semver", ahead, "main", false},
		{"no transitions", &ComponentUpgrades{Component: "widget-operator"}, "1.0.0", false},
		{"nil record", nil, "1.0.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.u.AheadOf(tt.version); got != tt.want {
				t.Errorf("AheadOf(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}
