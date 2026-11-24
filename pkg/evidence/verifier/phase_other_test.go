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

package verifier

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aicr.run/pkg/evidence/attestation"
)

func TestHasPhaseFailures(t *testing.T) {
	tests := []struct {
		name   string
		phases map[attestation.Phase]attestation.PhaseSummary
		want   bool
	}{
		{"passed and skipped only", map[attestation.Phase]attestation.PhaseSummary{
			attestation.PhaseDeployment: {Passed: 3, Skipped: 1},
		}, false},
		{"failed", map[attestation.Phase]attestation.PhaseSummary{
			attestation.PhaseDeployment: {Passed: 3, Failed: 1},
		}, true},
		{"other", map[attestation.Phase]attestation.PhaseSummary{
			attestation.PhaseDeployment:  {Passed: 3},
			attestation.PhasePerformance: {Other: 1},
		}, true},
		{"no phases", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasPhaseFailures(&attestation.Predicate{Phases: tt.phases}); got != tt.want {
				t.Errorf("hasPhaseFailures = %v, want %v", got, tt.want)
			}
		})
	}
	if hasPhaseFailures(nil) {
		t.Error("nil predicate reported phase failures")
	}
}

func TestWritePhasesRendersOther(t *testing.T) {
	tests := []struct {
		name    string
		summary attestation.PhaseSummary
		want    string
	}{
		{"passed", attestation.PhaseSummary{Passed: 2}, "- ✓ **performance** passed=2 failed=0 skipped=0\n"},
		{"failed", attestation.PhaseSummary{Passed: 1, Failed: 1}, "- ✗ **performance** passed=1 failed=1 skipped=0\n"},
		{"other", attestation.PhaseSummary{Passed: 1, Other: 1}, "- ✗ **performance** passed=1 failed=0 skipped=0 other=1\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			writePhases(&b, &attestation.Predicate{Phases: map[attestation.Phase]attestation.PhaseSummary{
				attestation.PhasePerformance: tt.summary,
			}})
			if !strings.Contains(b.String(), tt.want) {
				t.Errorf("rendered:\n%s\nwant line %q", b.String(), tt.want)
			}
		})
	}
}

// TestVerify_OtherResultInPredicate covers issue #3709: an attestation built
// from a CTRF report with one other result records it in the signed phase
// summary, and verify reports the informational phase-failure verdict.
func TestVerify_OtherResultInPredicate(t *testing.T) {
	summary := summaryDirOf(t, buildTestBundleOtherPhase(t))

	res, err := Verify(context.Background(), VerifyOptions{Input: summary})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Exit != ExitValidPhaseFailures {
		t.Errorf("Exit = %d, want %d", res.Exit, ExitValidPhaseFailures)
	}
	if res.Predicate == nil {
		t.Fatal("no predicate")
	}
	if got := res.Predicate.Phases[attestation.PhaseDeployment]; got.Other != 1 || got.Failed != 0 {
		t.Errorf("deployment summary = %+v, want other=1 failed=0", got)
	}
	if res.Predicate.SchemaVersion != attestation.PredicateSchemaVersion {
		t.Errorf("schemaVersion = %q, want %q", res.Predicate.SchemaVersion, attestation.PredicateSchemaVersion)
	}
	md := RenderMarkdown(res)
	if !strings.Contains(md, "✗ **deployment** passed=0 failed=0 skipped=0 other=1") {
		t.Errorf("markdown does not mark the phase:\n%s", md)
	}
}

// TestVerify_PredicateWithoutOtherStillVerifies pins that a predicate written
// before schema 1.1.0, with no other count, verifies as it did.
func TestVerify_PredicateWithoutOtherStillVerifies(t *testing.T) {
	summary := summaryDirOf(t, buildTestBundle(t))
	path := filepath.Join(summary, attestation.StatementFilename)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var stmt struct {
		Type          string         `json:"_type"`
		Subject       []any          `json:"subject"`
		PredicateType string         `json:"predicateType"`
		Predicate     map[string]any `json:"predicate"`
	}
	if err = json.Unmarshal(body, &stmt); err != nil {
		t.Fatal(err)
	}
	phases, _ := stmt.Predicate["phases"].(map[string]any)
	phase, _ := phases[string(attestation.PhaseDeployment)].(map[string]any)
	if phase == nil {
		t.Fatalf("statement records no deployment phase: %s", body)
	}
	if _, has := phase["other"]; has {
		t.Fatalf("a phase with no other result must serialize without an other count: %v", phase)
	}
	if got := stmt.Predicate["schemaVersion"]; got != attestation.PredicateSchemaVersion {
		t.Fatalf("schemaVersion = %v, want %s", got, attestation.PredicateSchemaVersion)
	}
	stmt.Predicate["schemaVersion"] = "1.0.0"
	legacy, err := json.Marshal(stmt)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := Verify(context.Background(), VerifyOptions{Input: summary})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Exit != ExitValidPassed || res.Predicate == nil || res.Predicate.SchemaVersion != "1.0.0" {
		t.Errorf("exit=%d predicate=%+v, want a passing 1.0.0 predicate", res.Exit, res.Predicate)
	}
}
