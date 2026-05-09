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

package attestation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBuildPredicate_FillsConstantFields(t *testing.T) {
	now := time.Date(2026, 5, 8, 10, 23, 11, 0, time.UTC)
	p := BuildPredicate(PredicateInputs{AttestedAt: now})
	if p.SchemaVersion != PredicateSchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", p.SchemaVersion, PredicateSchemaVersion)
	}
	if !p.AttestedAt.Equal(now) {
		t.Errorf("AttestedAt = %v, want %v", p.AttestedAt, now)
	}
}

func TestBuildPredicate_SortsValidatorImages(t *testing.T) {
	p := BuildPredicate(PredicateInputs{
		ValidatorImages: []ValidatorImage{
			{Image: "z-image", Digest: "sha256:zz"},
			{Image: "a-image", Digest: "sha256:aa"},
		},
	})
	if p.ValidatorImages[0].Image != "a-image" {
		t.Errorf("expected a-image first, got %q", p.ValidatorImages[0].Image)
	}
}

func TestBuildPredicate_OnlyKnownPhasesEmitted(t *testing.T) {
	p := BuildPredicate(PredicateInputs{
		Phases: map[Phase]PhaseSummary{
			PhaseDeployment:  {Passed: 1},
			PhasePerformance: {Passed: 0},
			Phase("rogue"):   {Passed: 99},
		},
	})
	if _, ok := p.Phases["rogue"]; ok {
		t.Errorf("predicate must not carry unknown phase keys")
	}
	if _, ok := p.Phases[PhaseDeployment]; !ok {
		t.Errorf("expected deployment phase to be present")
	}
}

func TestBuildStatement_RejectsBadInputs(t *testing.T) {
	pred := BuildPredicate(PredicateInputs{})
	cases := []struct {
		name      string
		recipe    string
		digest    string
		predicate *Predicate
	}{
		{"empty recipe name", "", strings.Repeat("a", 64), pred},
		{"empty digest", "h100-eks", "", pred},
		{"short digest", "h100-eks", "abcd", pred},
		{"nil predicate", "h100-eks", strings.Repeat("a", 64), nil},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := BuildStatement(tt.recipe, tt.digest, tt.predicate); err == nil {
				t.Errorf("expected error for %s", tt.name)
			}
		})
	}
}

func TestBuildStatement_ReturnsValidIntotoJSON(t *testing.T) {
	pred := BuildPredicate(PredicateInputs{
		AttestedAt:  time.Date(2026, 5, 8, 10, 23, 11, 0, time.UTC),
		AICRVersion: "v0.13.0",
	})
	stmt, err := BuildStatement("h100-eks-ubuntu-training", strings.Repeat("a", 64), pred)
	if err != nil {
		t.Fatalf("BuildStatement: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(stmt, &parsed); err != nil {
		t.Fatalf("statement is not JSON: %v", err)
	}
	if parsed["predicateType"] != PredicateTypeV1 {
		t.Errorf("predicateType = %v, want %v", parsed["predicateType"], PredicateTypeV1)
	}
	subj, ok := parsed["subject"].([]any)
	if !ok || len(subj) != 1 {
		t.Fatalf("expected single subject, got %v", parsed["subject"])
	}
	first, ok := subj[0].(map[string]any)
	if !ok {
		t.Fatalf("subject[0] not an object")
	}
	if first["name"] != "recipe:h100-eks-ubuntu-training" {
		t.Errorf("subject.name = %v, want recipe:<name> form", first["name"])
	}
}

func TestSubjectName_PrefixesRecipe(t *testing.T) {
	if got := SubjectName("foo"); got != "recipe:foo" {
		t.Errorf("SubjectName = %q, want recipe:foo", got)
	}
}
