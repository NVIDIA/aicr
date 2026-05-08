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

import "testing"

func h100Fingerprint() *Fingerprint {
	return &Fingerprint{
		Service:     Dimension{Value: "eks", Source: "k8s.node.provider"},
		Accelerator: Dimension{Value: "h100", Source: "gpu.smi.gpu.model"},
		OS:          OSDimension{Value: "ubuntu", Version: "22.04", Source: "os.release"},
		K8sVersion:  Dimension{Value: "1.33.4", Source: "k8s.server.version"},
		NodeCount:   IntDimension{Value: 12, Source: "nodeTopology.summary.node-count"},
	}
}

func TestMatch_AllDimensionsMatch(t *testing.T) {
	fp := h100Fingerprint()
	c := CriteriaInput{
		Service:     "eks",
		Accelerator: "h100",
		Intent:      "training",
		OS:          "ubuntu",
		Platform:    "kubeflow",
		Nodes:       12,
	}
	got := fp.Match(c)
	if !got.Matched {
		t.Errorf("Matched = false, want true; perDimension = %+v", got.PerDimension)
	}
	// intent and platform are not in the fingerprint -> unknown
	if got.PerDimension["intent"].Match != DimensionUnknown {
		t.Errorf("intent.Match = %q, want unknown", got.PerDimension["intent"].Match)
	}
	if got.PerDimension["platform"].Match != DimensionUnknown {
		t.Errorf("platform.Match = %q, want unknown", got.PerDimension["platform"].Match)
	}
	// service / accelerator / os / nodes capturable -> matched
	for _, dim := range []string{"service", "accelerator", "os", "nodes"} {
		if got.PerDimension[dim].Match != DimensionMatched {
			t.Errorf("%s.Match = %q, want matched", dim, got.PerDimension[dim].Match)
		}
	}
}

func TestMatch_AcceleratorMismatch(t *testing.T) {
	fp := h100Fingerprint()
	c := CriteriaInput{Accelerator: "gb200"}
	got := fp.Match(c)
	if got.Matched {
		t.Error("Matched = true, want false (recipe wants gb200, fingerprint has h100)")
	}
	if got.PerDimension["accelerator"].Match != DimensionMismatched {
		t.Errorf("accelerator.Match = %q, want mismatched", got.PerDimension["accelerator"].Match)
	}
}

func TestMatch_RecipeAnyMatchesAnyFingerprint(t *testing.T) {
	fp := h100Fingerprint()
	got := fp.Match(CriteriaInput{}) // every field empty == "any"
	if !got.Matched {
		t.Error("Matched = false, want true (every recipe field is any)")
	}
	for dim, diff := range got.PerDimension {
		if diff.Match != DimensionMatched {
			t.Errorf("%s.Match = %q, want matched (recipe is generic)", dim, diff.Match)
		}
	}
}

func TestMatch_RecipeAnyLiteralMatches(t *testing.T) {
	fp := h100Fingerprint()
	got := fp.Match(CriteriaInput{Accelerator: "any"})
	if got.PerDimension["accelerator"].Match != DimensionMatched {
		t.Errorf("accelerator.Match = %q, want matched (recipe explicitly any)", got.PerDimension["accelerator"].Match)
	}
}

func TestMatch_FingerprintMissingDimensionIsUnknown(t *testing.T) {
	// fingerprint did not detect a service
	fp := &Fingerprint{Accelerator: Dimension{Value: "h100"}}
	got := fp.Match(CriteriaInput{Service: "eks"})
	if got.Matched != true {
		t.Error("Matched = false, want true (unknown does not flip overall match)")
	}
	if got.PerDimension["service"].Match != DimensionUnknown {
		t.Errorf("service.Match = %q, want unknown", got.PerDimension["service"].Match)
	}
}

func TestMatch_NilFingerprint(t *testing.T) {
	var fp *Fingerprint
	got := fp.Match(CriteriaInput{Service: "eks"})
	if got.Matched != true {
		t.Error("Matched = false, want true (nil fingerprint -> unknown, not mismatched)")
	}
	if got.PerDimension["service"].Match != DimensionUnknown {
		t.Errorf("service.Match = %q, want unknown for nil fingerprint", got.PerDimension["service"].Match)
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
			got := fp.Match(CriteriaInput{Nodes: tt.recipeNodes})
			if got.PerDimension["nodes"].Match != tt.wantMatch {
				t.Errorf("nodes.Match = %q, want %q", got.PerDimension["nodes"].Match, tt.wantMatch)
			}
			if got.Matched != tt.wantOverall {
				t.Errorf("Matched = %v, want %v", got.Matched, tt.wantOverall)
			}
		})
	}
}

func TestMatch_PerDimensionDiffPopulated(t *testing.T) {
	fp := h100Fingerprint()
	got := fp.Match(CriteriaInput{Service: "gke"})
	d := got.PerDimension["service"]
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
	got := fp.Match(CriteriaInput{Intent: "training"})
	if got.PerDimension["intent"].Match != DimensionUnknown {
		t.Errorf("intent.Match = %q, want unknown (intent is not detectable)", got.PerDimension["intent"].Match)
	}
	if !got.Matched {
		t.Error("Matched = false, want true (unknown intent should not flip overall)")
	}
}
