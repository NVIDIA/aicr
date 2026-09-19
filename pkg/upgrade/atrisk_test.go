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
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// affectedSet is a component whose three transitions each name a different
// resource kind. The kinds differ per transition on purpose: a scan that
// walked every record a component owns rather than only the ones a jump
// crosses returns the same names as a correct one whenever the fixture repeats
// them, and the bug then survives a passing test.
//
// The floors are 2.0.0, 3.0.0 and 4.0.0, so a jump's endpoints alone select
// which records it crosses.
func affectedSet() Set {
	return Set{
		"omega-operator": {
			Component: "omega-operator",
			Transitions: []Transition{
				{
					From: "<2.0.0", To: ">=2.0.0 <3.0.0", Verdict: VerdictSafe,
					VerifiedBy:        "uat: synthetic lane",
					AffectedResources: []AffectedResource{{Group: "omega.io", Kinds: []string{"AlphaThing"}}},
				},
				{
					From: "<3.0.0", To: ">=3.0.0 <4.0.0", Verdict: VerdictManual,
					Summary:           "The beta store is rebuilt.",
					AffectedResources: []AffectedResource{{Group: "omega.io", Kinds: []string{"BetaThing"}}},
					StepsByDeployer: []StepGroup{{Steps: []Step{
						{ID: "rebuild", Description: "Rebuild the store."},
					}}},
				},
				{
					From: "<4.0.0", To: ">=4.0.0 <5.0.0", Verdict: VerdictManual,
					Summary: "The gamma CRD is removed.",
					AffectedResources: []AffectedResource{
						{Group: "omega.io", Kinds: []string{"GammaThing", "DeltaThing"}},
						{Group: "", Kinds: []string{"ConfigMap"}},
					},
					StepsByDeployer: []StepGroup{{Steps: []Step{
						{ID: "drop-crd", Description: "Delete the gamma CRD."},
					}}},
				},
			},
		},
		// A second component whose crossed record repeats one of omega's
		// kinds, so the union has something to deduplicate.
		"sigma-operator": {
			Component: "sigma-operator",
			Transitions: []Transition{{
				From: "<1.0.0", To: ">=1.0.0 <2.0.0", Verdict: VerdictSafe,
				VerifiedBy: "uat: synthetic lane",
				AffectedResources: []AffectedResource{
					{Group: "omega.io", Kinds: []string{"AlphaThing"}},
					{Group: "sigma.io", Kinds: []string{"SigmaThing"}},
				},
			}},
		},
	}
}

// TestAffectedKindsOnlyCrossedTransitions is the mutation guard the phase
// exists for: an upgrade that is not being made cannot put anything at risk,
// so a record whose boundary the jump never reaches contributes nothing.
func TestAffectedKindsOnlyCrossedTransitions(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		from string
		to   string
		want []ResourceKind
	}{
		{
			name: "crosses only the first boundary",
			from: "1.5.0", to: "2.1.0",
			want: []ResourceKind{{Group: "omega.io", Kind: "AlphaThing", Components: []string{"omega-operator"}}},
		},
		{
			name: "crosses only the middle boundary",
			from: "2.5.0", to: "3.1.0",
			want: []ResourceKind{{Group: "omega.io", Kind: "BetaThing", Components: []string{"omega-operator"}}},
		},
		{
			name: "crosses only the last boundary",
			from: "3.5.0", to: "4.1.0",
			want: []ResourceKind{
				{Group: "", Kind: "ConfigMap", Components: []string{"omega-operator"}},
				{Group: "omega.io", Kind: "DeltaThing", Components: []string{"omega-operator"}},
				{Group: "omega.io", Kind: "GammaThing", Components: []string{"omega-operator"}},
			},
		},
		{
			// Blocked on multiple boundaries, so ComponentResult.Transition is
			// nil. The kinds still have to come out: keying the scan on the
			// single describing record would report nothing for exactly the
			// jump most likely to destroy something.
			name: "crosses every boundary",
			from: "1.5.0", to: "4.1.0",
			want: []ResourceKind{
				{Group: "", Kind: "ConfigMap", Components: []string{"omega-operator"}},
				{Group: "omega.io", Kind: "AlphaThing", Components: []string{"omega-operator"}},
				{Group: "omega.io", Kind: "BetaThing", Components: []string{"omega-operator"}},
				{Group: "omega.io", Kind: "DeltaThing", Components: []string{"omega-operator"}},
				{Group: "omega.io", Kind: "GammaThing", Components: []string{"omega-operator"}},
			},
		},
		{
			// Entirely inside one record's covered range: no floor is passed,
			// so nothing is disturbed.
			name: "crosses nothing",
			from: "2.1.0", to: "2.5.0",
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			results := Match(affectedSet(),
				map[string]string{"omega-operator": tt.from},
				map[string]string{"omega-operator": tt.to})
			got := AffectedKinds(results)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("AffectedKinds() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestAffectedKindsBlockedMultipleBoundariesHasNoDescribingRecord pins the
// premise the case above rests on, so a matcher change that starts setting
// Transition there cannot quietly turn that case into a weaker one.
func TestAffectedKindsBlockedMultipleBoundariesHasNoDescribingRecord(t *testing.T) {
	t.Parallel()

	results := Match(affectedSet(),
		map[string]string{"omega-operator": "1.5.0"},
		map[string]string{"omega-operator": "4.1.0"})
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if results[0].Verdict != VerdictBlocked {
		t.Fatalf("verdict = %q, want %q", results[0].Verdict, VerdictBlocked)
	}
	if results[0].Transition != nil {
		t.Fatal("Transition is set; the multi-boundary fixture no longer proves Crossed is what drives the scan")
	}
	if len(results[0].Crossed) != 3 {
		t.Errorf("Crossed = %d records, want 3", len(results[0].Crossed))
	}
}

// TestAffectedKindsUnionsComponents proves the union is across components and
// deduplicates, and that the order does not depend on map iteration.
func TestAffectedKindsUnionsComponents(t *testing.T) {
	t.Parallel()

	results := Match(affectedSet(),
		map[string]string{"omega-operator": "1.5.0", "sigma-operator": "0.9.0"},
		map[string]string{"omega-operator": "2.1.0", "sigma-operator": "1.1.0"})
	// AlphaThing is one entry naming both owners rather than two entries: the
	// scan Lists a kind once however many records reached it, and an operator
	// reading the finding needs both names.
	want := []ResourceKind{
		{Group: "omega.io", Kind: "AlphaThing", Components: []string{"omega-operator", "sigma-operator"}},
		{Group: "sigma.io", Kind: "SigmaThing", Components: []string{"sigma-operator"}},
	}
	for range 10 {
		if got := AffectedKinds(results); !reflect.DeepEqual(got, want) {
			t.Fatalf("AffectedKinds() = %#v, want %#v", got, want)
		}
	}
}

// TestAffectedKindsSkipsEmptyKinds covers a record naming a group with no
// kinds, which would otherwise scan the empty kind cluster-wide.
func TestAffectedKindsSkipsEmptyKinds(t *testing.T) {
	t.Parallel()

	set := Set{"tau-operator": {
		Component: "tau-operator",
		Transitions: []Transition{{
			From: "<1.0.0", To: ">=1.0.0 <2.0.0", Verdict: VerdictSafe,
			VerifiedBy: "uat: synthetic lane",
			AffectedResources: []AffectedResource{
				{Group: "tau.io", Kinds: []string{"", "  ", "RealThing"}},
				{Group: "tau.io"},
			},
		}},
	}}
	results := Match(set,
		map[string]string{"tau-operator": "0.9.0"},
		map[string]string{"tau-operator": "1.1.0"})
	want := []ResourceKind{{Group: "tau.io", Kind: "RealThing", Components: []string{"tau-operator"}}}
	if got := AffectedKinds(results); !reflect.DeepEqual(got, want) {
		t.Errorf("AffectedKinds() = %#v, want %#v", got, want)
	}
}

// TestAtRiskNeverChangesTheExitCode is ADR-021 Decision 3: AICR blocking an
// upgrade over resources it does not own is a claim it has not earned.
func TestAtRiskNeverChangesTheExitCode(t *testing.T) {
	t.Parallel()

	results := Match(affectedSet(),
		map[string]string{"omega-operator": "1.5.0"},
		map[string]string{"omega-operator": "2.1.0"})
	for _, r := range results {
		if r.Verdict != VerdictSafe {
			t.Fatalf("fixture verdict = %q, want safe so the at-risk findings are the only thing in play", r.Verdict)
		}
	}
	rep := NewReport(results, ReportOptions{AtRisk: &AtRiskReport{
		Scanned: true,
		Kinds: []AtRiskKind{
			{Group: "omega.io", Kind: "AlphaThing", Components: []string{"omega-operator"}, Present: true, Examined: 4},
		},
		Findings: []AtRiskFinding{
			{Group: "omega.io", Kind: "AlphaThing", Components: []string{"omega-operator"},
				Namespace: "tenant-a", Name: "thing-1"},
			{Group: "omega.io", Kind: "AlphaThing", Components: []string{"omega-operator"},
				Namespace: "tenant-b", Name: "thing-2"},
		},
	}})
	if len(rep.AtRisk.Findings) != 2 {
		t.Fatalf("findings = %d, want 2", len(rep.AtRisk.Findings))
	}
	if rep.Summary.Failing != 0 {
		t.Errorf("Summary.Failing = %d, want 0: an advisory finding is not a failing row", rep.Summary.Failing)
	}
	if rep.FailsRun() {
		t.Error("FailsRun() = true; the at-risk scan must never change the exit code")
	}
}

// TestAtRiskSectionIsAlwaysSerialized pins the absent omitempty. An absent
// warning reads as an all-clear, and the resources this section covers are
// exactly the ones AICR cannot fix if it is wrong.
//
// Both encodings are checked because only one of them can enforce the rule.
// encoding/json ignores omitempty on a struct field, so the JSON cases pin the
// field names a pipeline reads; yaml.v3 honors it and drops a zero struct, so
// the zero-report case below is what an omitempty would actually break.
func TestAtRiskSectionIsAlwaysSerialized(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		report     *Report
		wantJSON   []string
		rejectJSON []string
		wantYAML   []string
	}{
		{
			name:     "no scan requested",
			report:   NewReport(nil, ReportOptions{}),
			wantJSON: []string{`"atRisk"`, `"scanned": false`, `"reason": "` + NotScannedOffline + `"`},
			wantYAML: []string{"atRisk:", "scanned: false", "reason: " + NotScannedOffline},
		},
		{
			name: "scanned and clean",
			report: NewReport(nil, ReportOptions{AtRisk: &AtRiskReport{
				Scanned: true,
				Kinds: []AtRiskKind{
					{Group: "omega.io", Kind: "AlphaThing", Components: []string{"omega-operator"},
						Present: true, Examined: 3},
				},
			}}),
			wantJSON:   []string{`"atRisk"`, `"scanned": true`, `"examined": 3`},
			rejectJSON: []string{`"reason"`, `"findings"`},
			wantYAML:   []string{"atRisk:", "scanned: true", "examined: 3"},
		},
		{
			// Constructed rather than produced by NewReport, which never
			// leaves the section zero. The type's own contract is what is
			// being pinned: a Report says whether it scanned however it was
			// built, and a zero struct is the one shape an omitempty erases.
			name:     "a zero section still renders",
			report:   &Report{},
			wantJSON: []string{`"atRisk"`, `"scanned": false`},
			wantYAML: []string{"atRisk:", "scanned: false"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotJSON, err := json.MarshalIndent(tt.report, "", "  ")
			if err != nil {
				t.Fatalf("marshal JSON: %v", err)
			}
			for _, want := range tt.wantJSON {
				if !strings.Contains(string(gotJSON), want) {
					t.Errorf("report JSON does not contain %s:\n%s", want, gotJSON)
				}
			}
			for _, reject := range tt.rejectJSON {
				if strings.Contains(string(gotJSON), reject) {
					t.Errorf("report JSON unexpectedly contains %s:\n%s", reject, gotJSON)
				}
			}

			gotYAML, err := yaml.Marshal(tt.report)
			if err != nil {
				t.Fatalf("marshal YAML: %v", err)
			}
			for _, want := range tt.wantYAML {
				if !strings.Contains(string(gotYAML), want) {
					t.Errorf("report YAML does not contain %q:\n%s", want, gotYAML)
				}
			}
		})
	}
}

// TestNewReportCopiesAtRisk holds the report to the ownership contract the
// rest of its fields hold: it must outlive everything it was built from.
func TestNewReportCopiesAtRisk(t *testing.T) {
	t.Parallel()

	opts := ReportOptions{AtRisk: &AtRiskReport{
		Scanned: true,
		Kinds: []AtRiskKind{
			{Group: "omega.io", Kind: "AlphaThing", Components: []string{"omega-operator"}, Present: true, Examined: 1},
		},
		Findings: []AtRiskFinding{
			{Group: "omega.io", Kind: "AlphaThing", Components: []string{"omega-operator"}, Name: "thing-1"},
		},
	}}
	rep := NewReport(nil, opts)

	opts.AtRisk.Scanned = false
	opts.AtRisk.Findings[0].Name = "mutated"
	opts.AtRisk.Kinds[0].Examined = 99
	// Through the slice a struct copy would still share.
	opts.AtRisk.Findings[0].Components[0] = "mutated-operator"
	opts.AtRisk.Kinds[0].Components[0] = "mutated-operator"

	if !rep.AtRisk.Scanned {
		t.Error("report tracked a later write to the option's Scanned")
	}
	if rep.AtRisk.Findings[0].Name != "thing-1" {
		t.Errorf("finding name = %q, want %q: the findings slice is shared with the caller",
			rep.AtRisk.Findings[0].Name, "thing-1")
	}
	if rep.AtRisk.Kinds[0].Examined != 1 {
		t.Errorf("kind examined = %d, want 1: the kinds slice is shared with the caller", rep.AtRisk.Kinds[0].Examined)
	}
	if rep.AtRisk.Findings[0].Components[0] != "omega-operator" {
		t.Errorf("finding component = %q, want %q: the Components backing array is shared with the caller",
			rep.AtRisk.Findings[0].Components[0], "omega-operator")
	}
	if rep.AtRisk.Kinds[0].Components[0] != "omega-operator" {
		t.Errorf("kind component = %q, want %q: the Components backing array is shared with the caller",
			rep.AtRisk.Kinds[0].Components[0], "omega-operator")
	}
}
