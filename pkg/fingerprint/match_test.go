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

package fingerprint

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

func h100Fingerprint() *Fingerprint {
	return &Fingerprint{
		Service:     Dimension{Value: "eks", Source: "k8s.node.provider"},
		Accelerator: Dimension{Value: "h100", Source: "gpu.smi.gpu.model"},
		OS:          OSDimension{Value: "ubuntu", Version: "22.04", Source: "os.release"},
		K8sVersion:  Dimension{Value: "1.33.4", Source: "k8s.server.version"},
		NodeCount:   IntDimension{Value: 12, Source: "nodeTopology.summary.node-count"},
	}
}

// requireDim looks up a dimension diff via Find and fails the test if
// it is missing, so per-dimension assertions stay terse.
func requireDim(t *testing.T, r MatchResult, name DimensionName) DimensionDiff {
	t.Helper()
	d, ok := r.Find(name)
	if !ok {
		t.Fatalf("MatchResult missing dimension %q; perDimension = %+v", name, r.PerDimension)
	}
	return d
}

func TestMatch_AllDimensionsMatch(t *testing.T) {
	fp := h100Fingerprint()
	c := &recipe.Criteria{
		Service:     recipe.CriteriaServiceEKS,
		Accelerator: recipe.CriteriaAcceleratorH100,
		Intent:      recipe.CriteriaIntentTraining,
		OS:          recipe.CriteriaOSUbuntu,
		Platform:    recipe.CriteriaPlatformKubeflow,
		Nodes:       12,
	}
	got := fp.Match(c)
	if !got.Matched {
		t.Errorf("Matched = false, want true; perDimension = %+v", got.PerDimension)
	}
	// intent and platform are not in the fingerprint -> unknown
	if requireDim(t, got, DimensionIntent).Match != DimensionUnknown {
		t.Errorf("intent.Match = %q, want unknown", requireDim(t, got, DimensionIntent).Match)
	}
	if requireDim(t, got, DimensionPlatform).Match != DimensionUnknown {
		t.Errorf("platform.Match = %q, want unknown", requireDim(t, got, DimensionPlatform).Match)
	}
	for _, dim := range []DimensionName{DimensionService, DimensionAccelerator, DimensionOS, DimensionNodes} {
		if requireDim(t, got, dim).Match != DimensionMatched {
			t.Errorf("%s.Match = %q, want matched", dim, requireDim(t, got, dim).Match)
		}
	}
}

func TestMatch_DeterministicOrder(t *testing.T) {
	got := h100Fingerprint().Match(&recipe.Criteria{})
	wantOrder := []DimensionName{
		DimensionService, DimensionAccelerator, DimensionOS,
		DimensionIntent, DimensionPlatform, DimensionNodes,
	}
	if len(got.PerDimension) != len(wantOrder) {
		t.Fatalf("PerDimension length = %d, want %d", len(got.PerDimension), len(wantOrder))
	}
	for i, want := range wantOrder {
		if got.PerDimension[i].Dimension != want {
			t.Errorf("PerDimension[%d].Dimension = %q, want %q", i, got.PerDimension[i].Dimension, want)
		}
	}
}

func TestMatch_AcceleratorMismatch(t *testing.T) {
	fp := h100Fingerprint()
	c := &recipe.Criteria{Accelerator: recipe.CriteriaAcceleratorGB200}
	got := fp.Match(c)
	if got.Matched {
		t.Error("Matched = true, want false (recipe wants gb200, fingerprint has h100)")
	}
	if requireDim(t, got, DimensionAccelerator).Match != DimensionMismatched {
		t.Errorf("accelerator.Match = %q, want mismatched", requireDim(t, got, DimensionAccelerator).Match)
	}
}

func TestMatch_RecipeAnyMatchesAnyFingerprint(t *testing.T) {
	fp := h100Fingerprint()
	got := fp.Match(recipe.NewCriteria()) // every field "any"
	if !got.Matched {
		t.Error("Matched = false, want true (every recipe field is any)")
	}
	for _, diff := range got.PerDimension {
		if diff.Match != DimensionMatched {
			t.Errorf("%s.Match = %q, want matched (recipe is generic)", diff.Dimension, diff.Match)
		}
	}
}

func TestMatch_RecipeAnyLiteralMatches(t *testing.T) {
	fp := h100Fingerprint()
	got := fp.Match(&recipe.Criteria{Accelerator: recipe.CriteriaAcceleratorAny})
	if requireDim(t, got, DimensionAccelerator).Match != DimensionMatched {
		t.Errorf("accelerator.Match = %q, want matched (recipe explicitly any)", requireDim(t, got, DimensionAccelerator).Match)
	}
}

func TestMatch_FingerprintMissingDimensionIsUnknown(t *testing.T) {
	// fingerprint did not detect a service
	fp := &Fingerprint{Accelerator: Dimension{Value: "h100"}}
	got := fp.Match(&recipe.Criteria{Service: recipe.CriteriaServiceEKS})
	if got.Matched != true {
		t.Error("Matched = false, want true (unknown does not flip overall match)")
	}
	if requireDim(t, got, DimensionService).Match != DimensionUnknown {
		t.Errorf("service.Match = %q, want unknown", requireDim(t, got, DimensionService).Match)
	}
}

func TestMatch_NilFingerprint(t *testing.T) {
	var fp *Fingerprint
	got := fp.Match(&recipe.Criteria{Service: recipe.CriteriaServiceEKS})
	if got.Matched != true {
		t.Error("Matched = false, want true (nil fingerprint -> unknown, not mismatched)")
	}
	if requireDim(t, got, DimensionService).Match != DimensionUnknown {
		t.Errorf("service.Match = %q, want unknown for nil fingerprint", requireDim(t, got, DimensionService).Match)
	}
}

func TestMatch_NilCriteria(t *testing.T) {
	fp := h100Fingerprint()
	got := fp.Match(nil)
	if !got.Matched {
		t.Error("Matched = false, want true (nil criteria = fully generic recipe)")
	}
}

func TestMatch_NodesComparison(t *testing.T) {
	tests := []struct {
		name         string
		recipeNodes  int
		fingerprintN int
		wantMatch    DimensionMatch
		wantOverall  bool
	}{
		{"both zero (any)", 0, 0, DimensionMatched, true},
		{"recipe zero, fingerprint specific", 0, 12, DimensionMatched, true},
		{"recipe specific, fingerprint zero", 12, 0, DimensionUnknown, true},
		{"both specific match", 12, 12, DimensionMatched, true},
		{"both specific mismatch", 12, 8, DimensionMismatched, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fp := &Fingerprint{NodeCount: IntDimension{Value: tt.fingerprintN}}
			got := fp.Match(&recipe.Criteria{Nodes: tt.recipeNodes})
			if requireDim(t, got, DimensionNodes).Match != tt.wantMatch {
				t.Errorf("nodes.Match = %q, want %q", requireDim(t, got, DimensionNodes).Match, tt.wantMatch)
			}
			if got.Matched != tt.wantOverall {
				t.Errorf("Matched = %v, want %v", got.Matched, tt.wantOverall)
			}
		})
	}
}

func TestMatch_PerDimensionDiffPopulated(t *testing.T) {
	fp := h100Fingerprint()
	got := fp.Match(&recipe.Criteria{Service: recipe.CriteriaServiceGKE})
	d := requireDim(t, got, DimensionService)
	if d.Dimension != DimensionService {
		t.Errorf("Dimension = %q, want service", d.Dimension)
	}
	if d.RecipeRequires != "gke" {
		t.Errorf("RecipeRequires = %q, want gke", d.RecipeRequires)
	}
	if d.FingerprintProvides != "eks" {
		t.Errorf("FingerprintProvides = %q, want eks", d.FingerprintProvides)
	}
	if d.Match != DimensionMismatched {
		t.Errorf("Match = %q, want mismatched", d.Match)
	}
}

func TestMatch_IntentSpecificIsUnknown(t *testing.T) {
	fp := h100Fingerprint()
	got := fp.Match(&recipe.Criteria{Intent: recipe.CriteriaIntentTraining})
	if requireDim(t, got, DimensionIntent).Match != DimensionUnknown {
		t.Errorf("intent.Match = %q, want unknown (intent is not detectable)", requireDim(t, got, DimensionIntent).Match)
	}
	if !got.Matched {
		t.Error("Matched = false, want true (unknown intent should not flip overall)")
	}
}

func TestMatchResult_Find_Missing(t *testing.T) {
	r := MatchResult{}
	if _, ok := r.Find(DimensionService); ok {
		t.Error("Find on empty MatchResult should return ok=false")
	}
}
