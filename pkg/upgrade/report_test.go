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
	"testing"
)

// Every fixture here is synthetic. ADR-021's testing strategy forbids asserting
// a verdict for a real component: pinning one makes every registry pin bump
// churn the suite, and "keep the tests green" then becomes pressure to weaken
// the record rather than to fix the code.

func TestStepsFor(t *testing.T) {
	gitops := StepGroup{Deployers: []string{"argocd", "flux"}, Steps: []Step{{ID: "gitops"}}}
	imperative := StepGroup{Deployers: []string{"helm"}, Steps: []Step{{ID: "imperative"}}}
	remainder := StepGroup{Steps: []Step{{ID: "remainder"}}}
	emptyList := StepGroup{Deployers: []string{}, Steps: []Step{{ID: "claims-nothing"}}}

	tests := []struct {
		name     string
		groups   []StepGroup
		deployer string
		want     []string
	}{
		{"named group wins", []StepGroup{gitops, remainder}, "argocd", []string{"gitops"}},
		{"second name in a group", []StepGroup{gitops, remainder}, "flux", []string{"gitops"}},
		{"remainder covers what no group claims", []StepGroup{gitops, remainder}, "helm", []string{"remainder"}},
		{"explicit group beats remainder order", []StepGroup{remainder, imperative}, "helm", []string{"imperative"}},
		{"no remainder and no match yields nothing", []StepGroup{gitops}, "helm", nil},
		{"an empty deployers list is not the remainder", []StepGroup{emptyList}, "helm", nil},
		{"an unnamed deployer selects nothing", []StepGroup{remainder}, "", nil},
		{"no groups at all", nil, "helm", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, s := range stepsFor(tt.groups, tt.deployer) {
				got = append(got, s.ID)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("stepsFor(...) step ids = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRequiresDeployer(t *testing.T) {
	tests := []struct {
		name    string
		results []ComponentResult
		want    bool
	}{
		{"nothing changed", nil, false},
		{"safe alone", []ComponentResult{{Verdict: VerdictSafe}}, false},
		{"unknown alone", []ComponentResult{{Verdict: VerdictUnknown}}, false},
		{"unversioned alone", []ComponentResult{{Verdict: VerdictUnversioned}}, false},
		{"added and removed carry no verdict", []ComponentResult{
			{Change: ChangeAdded}, {Change: ChangeRemoved},
		}, false},
		{"one manual among many", []ComponentResult{
			{Verdict: VerdictSafe}, {Verdict: VerdictManual},
		}, true},
		{"a blocked-only report renders no steps, so it needs no deployer", []ComponentResult{
			{Verdict: VerdictBlocked},
		}, false},
		{"a blocked row whose own record describes the jump does render steps", []ComponentResult{
			{Verdict: VerdictBlocked, Transition: &Transition{Verdict: VerdictBlocked}},
		}, true},
		{"a manual row beside a blocked one still renders steps", []ComponentResult{
			{Verdict: VerdictBlocked}, {Verdict: VerdictManual},
		}, true},
		{"a replacement is manual too", []ComponentResult{
			{Change: ChangeReplaced, Verdict: VerdictManual},
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RequiresDeployer(tt.results); got != tt.want {
				t.Errorf("RequiresDeployer() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSpanPhrase(t *testing.T) {
	tests := []struct {
		name string
		span Span
		want string
	}{
		{"nothing", Span{}, ""},
		{"one patch", Span{Patches: 1}, "1 patch"},
		{"several patches", Span{Patches: 4}, "4 patches"},
		{"one minor", Span{Minors: 1}, "1 minor"},
		{"eleven minors", Span{Minors: 11}, "11 minors"},
		{"one major", Span{Majors: 1}, "1 major"},
		{"two majors", Span{Majors: 2}, "2 majors"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := spanPhrase(tt.span); got != tt.want {
				t.Errorf("spanPhrase(%+v) = %q, want %q", tt.span, got, tt.want)
			}
		})
	}
}

func TestNewReportNotes(t *testing.T) {
	verified := &Transition{Verdict: VerdictSafe, VerifiedBy: "uat: synthetic lane"}
	stepped := &Transition{
		Verdict:         VerdictManual,
		StepsByDeployer: []StepGroup{{Steps: []Step{{ID: "one"}, {ID: "two"}}}},
	}

	tests := []struct {
		name   string
		result ComponentResult
		want   string
	}{
		{
			name:   "added says there is nothing to do",
			result: ComponentResult{Component: "a", Change: ChangeAdded, To: "1.0.0"},
			want:   "new component, nothing to do",
		},
		{
			name:   "removed says the component stays installed",
			result: ComponentResult{Component: "a", Change: ChangeRemoved, From: "1.0.0"},
			want:   "stays installed; AICR does not uninstall it",
		},
		{
			name: "replaced names the outgoing component and its work",
			result: ComponentResult{
				Component: "b", Change: ChangeReplaced, ReplacedComponent: "a", To: "1.0.0",
				Verdict: VerdictManual,
				Replaces: &Replaces{
					Component:       "a",
					Verdict:         VerdictManual,
					StepsByDeployer: []StepGroup{{Steps: []Step{{ID: "one"}, {ID: "two"}}}},
				},
			},
			want: "replaces a, 2 steps",
		},
		{
			name: "safe names its evidence",
			result: ComponentResult{
				Component: "a", Change: ChangeVersion, From: "1.2.0", To: "1.2.3",
				Verdict: VerdictSafe, Transition: verified, Jump: Span{Patches: 3},
			},
			want: "3 patches, verified",
		},
		{
			name: "safe states a claim wider than the jump",
			result: ComponentResult{
				Component: "a", Change: ChangeVersion, From: "1.2.0", To: "1.2.3",
				Verdict: VerdictSafe, Transition: verified,
				Jump: Span{Patches: 3}, Span: Span{Minors: 11},
			},
			want: "3 patches, verified, safe across 11 minors",
		},
		{
			name: "manual counts the selected deployer's steps",
			result: ComponentResult{
				Component: "a", Change: ChangeVersion, From: "0.17.2", To: "0.18.1",
				Verdict: VerdictManual, Transition: stepped, Jump: Span{Minors: 1},
			},
			want: "1 minor, 2 steps",
		},
		{
			name: "blocked names where to stop",
			result: ComponentResult{
				Component: "a", Change: ChangeVersion, From: "1.0.0", To: "3.0.0",
				Verdict: VerdictBlocked, StoppedAt: ">=2.0.0 <3.0.0",
				Jump: Span{Majors: 2}, Breaking: true,
			},
			want: "2 majors, stops at >=2.0.0 <3.0.0",
		},
		{
			name: "blocked with its own record counts the steps it will render",
			result: ComponentResult{
				Component: "a", Change: ChangeVersion, From: "3.2.0", To: "4.0.0",
				Verdict: VerdictBlocked, StoppedAt: ">=4.0.0 <=4.0.0", Transition: stepped,
				Jump: Span{Majors: 1}, Breaking: true,
			},
			want: "1 major, stops at >=4.0.0 <=4.0.0, 2 steps",
		},
		{
			name: "unknown across a breaking boundary says so",
			result: ComponentResult{
				Component: "a", Change: ChangeVersion, From: "0.18.0", To: "0.19.0",
				Verdict: VerdictUnknown, Jump: Span{Minors: 1}, Breaking: true,
			},
			want: "1 minor, no record, breaking boundary",
		},
		{
			name: "unknown within a non-breaking boundary",
			result: ComponentResult{
				Component: "a", Change: ChangeVersion, From: "1.18.0", To: "1.19.0",
				Verdict: VerdictUnknown, Jump: Span{Minors: 1},
			},
			want: "1 minor, no record",
		},
		{
			name: "a downgrade has no forward record to read backwards",
			result: ComponentResult{
				Component: "a", Change: ChangeVersion, From: "0.13.0", To: "0.11.0",
				Verdict: VerdictUnknown, Downgrade: true, Jump: Span{Minors: 2}, Breaking: true,
			},
			want: "downgrade, no reverse record, breaking boundary",
		},
		{
			name: "unversioned is a gap in the inputs",
			result: ComponentResult{
				Component: "a", Change: ChangeVersion, From: "main", To: "release-1",
				Verdict: VerdictUnversioned,
			},
			want: "versions are not comparable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep := NewReport([]ComponentResult{tt.result}, ReportOptions{Deployer: "helm"})
			if len(rep.Components) != 1 {
				t.Fatalf("NewReport produced %d rows, want 1", len(rep.Components))
			}
			if got := rep.Components[0].Notes; got != tt.want {
				t.Errorf("notes = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewReportSummary(t *testing.T) {
	results := []ComponentResult{
		{Component: "a", Change: ChangeVersion, Verdict: VerdictSafe},
		{Component: "b", Change: ChangeAdded},
		{Component: "c", Change: ChangeVersion, Verdict: VerdictManual},
		{Component: "d", Change: ChangeVersion, Verdict: VerdictUnknown, Breaking: true},
	}
	rep := NewReport(results, ReportOptions{From: "old", To: "new", Deployer: "flux"})

	if rep.From != "old" || rep.To != "new" || rep.Deployer != "flux" {
		t.Errorf("report labels = %q/%q/%q, want old/new/flux", rep.From, rep.To, rep.Deployer)
	}
	if rep.Summary.Components != 4 {
		t.Errorf("Summary.Components = %d, want 4", rep.Summary.Components)
	}
	// FailsRun is the matcher's classification, not a second copy of it here:
	// manual fails, unknown fails only across a breaking boundary.
	if rep.Summary.Failing != 2 {
		t.Errorf("Summary.Failing = %d, want 2", rep.Summary.Failing)
	}
	if !rep.FailsRun() {
		t.Error("FailsRun() = false, want true")
	}

	clean := NewReport(results[:2], ReportOptions{})
	if clean.FailsRun() {
		t.Error("FailsRun() = true for a safe-and-added report, want false")
	}
	if (&Report{}).FailsRun() {
		t.Error("FailsRun() = true for an empty report, want false")
	}
	var nilReport *Report
	if nilReport.FailsRun() {
		t.Error("FailsRun() = true on a nil report, want false")
	}
}

// TestNewReportDoesNotAliasRecords proves a report survives the Set it came
// from: every consumer holds the same record pointers under a read-only
// contract, so a report that aliased them would hand that contract to a caller
// who never agreed to it.
func TestNewReportDoesNotAliasRecords(t *testing.T) {
	tr := &Transition{
		Verdict:         VerdictManual,
		Summary:         "original summary",
		StepsByDeployer: []StepGroup{{Steps: []Step{{ID: "one", Description: "original description"}}}},
	}
	rep := NewReport([]ComponentResult{{
		Component: "a", Change: ChangeVersion, From: "1.0.0", To: "2.0.0",
		Verdict: VerdictManual, Transition: tr,
	}}, ReportOptions{Deployer: "helm"})

	tr.Summary = "mutated"
	tr.StepsByDeployer[0].Steps[0].Description = "mutated"

	if got := rep.Components[0].Summary; got != "original summary" {
		t.Errorf("report summary = %q, want the value captured at build time", got)
	}
	if got := rep.Components[0].Steps[0].Description; got != "original description" {
		t.Errorf("report step description = %q, want the value captured at build time", got)
	}
}

// TestReportBlockedRendersStepsOnlyWhenOneRecordDescribesTheJump draws the line
// the blocked verdict actually needs: not "blocked hides steps", but "steps
// render only where a single record describes this exact move". Composing two
// records' instructions, or handing over instructions authored for a different
// starting point, is what the verdict exists to prevent; withholding an
// author's own guidance for the move they wrote it for is not.
func TestReportBlockedRendersStepsOnlyWhenOneRecordDescribesTheJump(t *testing.T) {
	steps := []StepGroup{{Steps: []Step{{ID: "do-this", Description: "Do this first."}}}}
	blocked := func(from, to, summary string) Transition {
		return Transition{
			From: from, To: to, Verdict: VerdictBlocked,
			Summary: summary, StepsByDeployer: steps,
		}
	}

	tests := []struct {
		name        string
		set         Set
		from, to    string
		wantReason  Reason
		wantSteps   int
		wantSummary string
	}{
		{
			name:        "one record covering the source renders its summary and steps",
			set:         oneComponent(blocked("<3.0.0", ">=3.0.0 <=3.0.0", "the one record")),
			from:        "1.5.0",
			to:          "3.0.0",
			wantReason:  ReasonRecorded,
			wantSteps:   1,
			wantSummary: "the one record",
		},
		{
			name: "a block flown past renders neither",
			set: oneComponent(
				trans("<2.0.0", ">=2.0.0 <2.1.0", VerdictSafe, "lower"),
				blocked(">=2.0.0 <3.0.0", ">=3.0.0 <=3.0.0", "the block"),
			),
			from:       "1.5.0",
			to:         "3.0.0",
			wantReason: ReasonRecordBlocks,
		},
		{
			name: "two crossed boundaries render neither",
			set: oneComponent(
				trans("<2.0.0", ">=2.0.0 <2.1.0", VerdictManual, "lower"),
				trans("<3.0.0", ">=3.0.0 <=3.0.0", VerdictManual, "upper"),
			),
			from:       "1.5.0",
			to:         "3.0.0",
			wantReason: ReasonMultipleBoundaries,
		},
		{
			name:       "a record that does not cover the source renders neither",
			set:        oneComponent(trans(">=2.0.0 <3.0.0", ">=3.0.0 <=3.0.0", VerdictManual, "elsewhere")),
			from:       "1.5.0",
			to:         "3.0.0",
			wantReason: ReasonUndefinedOrigin,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			results := Match(tt.set, map[string]string{"c": tt.from}, map[string]string{"c": tt.to})
			rep := NewReport(results, ReportOptions{Deployer: "helm"})
			if len(rep.Components) != 1 {
				t.Fatalf("NewReport produced %d rows, want 1", len(rep.Components))
			}
			row := rep.Components[0]
			if row.Verdict != VerdictBlocked {
				t.Fatalf("verdict = %q, want %q", row.Verdict, VerdictBlocked)
			}
			if row.Reason != tt.wantReason {
				t.Errorf("reason = %q, want %q", row.Reason, tt.wantReason)
			}
			if row.StoppedAt == "" {
				t.Error("stoppedAt is empty; every blocked row names the boundary it stops at")
			}
			if len(row.Steps) != tt.wantSteps {
				t.Errorf("steps = %d, want %d", len(row.Steps), tt.wantSteps)
			}
			if row.Summary != tt.wantSummary {
				t.Errorf("summary = %q, want %q", row.Summary, tt.wantSummary)
			}
			if got := RequiresDeployer(results); got != (tt.wantSteps > 0) {
				t.Errorf("RequiresDeployer() = %v, want %v", got, tt.wantSteps > 0)
			}
		})
	}
}
