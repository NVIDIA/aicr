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

package recipe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"gopkg.in/yaml.v3"
)

const (
	testRecipeBase         = "base"
	testOverlayEKS         = "eks"
	testK8sVersionConstant = "K8s.server.version"
	testOverlayEKSTraning  = "eks-training"
	testOverlaySharedTrain = "shared-training"
)

func TestMetadataStore_GetValuesFile(t *testing.T) {
	store := &MetadataStore{
		ValuesFiles: map[string][]byte{
			"components/gpu-operator/values.yaml": []byte("driver:\n  enabled: true"),
		},
	}

	tests := []struct {
		name     string
		filename string
		wantErr  bool
	}{
		{"existing file", "components/gpu-operator/values.yaml", false},
		{"missing file", "components/missing/values.yaml", true},
		{"empty filename", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := store.GetValuesFile(tt.filename)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetValuesFile() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && len(content) == 0 {
				t.Error("expected non-empty content")
			}
		})
	}
}

func TestMetadataStore_GetRecipeByName(t *testing.T) {
	baseMeta := &RecipeMetadata{}
	baseMeta.Metadata.Name = testRecipeBase

	overlayMeta := &RecipeMetadata{}
	overlayMeta.Metadata.Name = "h100-eks"

	store := &MetadataStore{
		Base: baseMeta,
		Overlays: map[string]*RecipeMetadata{
			"h100-eks": overlayMeta,
		},
	}

	tests := []struct {
		name      string
		input     string
		wantName  string
		wantFound bool
	}{
		{"empty returns base", "", testRecipeBase, true},
		{"base returns base", testRecipeBase, testRecipeBase, true},
		{"existing overlay", "h100-eks", "h100-eks", true},
		{"missing overlay", "nonexistent", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta, found := store.GetRecipeByName(tt.input)
			if found != tt.wantFound {
				t.Errorf("found = %v, want %v", found, tt.wantFound)
				return
			}
			if found && meta.Metadata.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", meta.Metadata.Name, tt.wantName)
			}
		})
	}

	// Test with nil base
	t.Run("nil base", func(t *testing.T) {
		nilStore := &MetadataStore{Overlays: map[string]*RecipeMetadata{}}
		meta, found := nilStore.GetRecipeByName("")
		if found {
			t.Error("expected found=false for nil base")
		}
		if meta != nil {
			t.Error("expected nil meta for nil base")
		}
	})
}

func TestMetadataStore_ResolveInheritanceChain(t *testing.T) {
	baseMeta := &RecipeMetadata{}
	baseMeta.Metadata.Name = testRecipeBase

	eksMeta := &RecipeMetadata{}
	eksMeta.Metadata.Name = testOverlayEKS

	eksTraining := &RecipeMetadata{}
	eksTraining.Metadata.Name = testOverlayEKSTraning
	eksTraining.Spec.Base = testOverlayEKS

	t.Run("single overlay", func(t *testing.T) {
		store := &MetadataStore{
			Base: baseMeta,
			Overlays: map[string]*RecipeMetadata{
				testOverlayEKS: eksMeta,
			},
		}
		chain, err := store.resolveInheritanceChain(testOverlayEKS)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(chain) != 2 {
			t.Fatalf("chain length = %d, want 2", len(chain))
		}
	})

	t.Run("two-level chain", func(t *testing.T) {
		store := &MetadataStore{
			Base: baseMeta,
			Overlays: map[string]*RecipeMetadata{
				testOverlayEKS:        eksMeta,
				testOverlayEKSTraning: eksTraining,
			},
		}
		chain, err := store.resolveInheritanceChain(testOverlayEKSTraning)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(chain) != 3 {
			t.Fatalf("chain length = %d, want 3", len(chain))
		}
	})

	t.Run("missing recipe", func(t *testing.T) {
		store := &MetadataStore{
			Base:     baseMeta,
			Overlays: map[string]*RecipeMetadata{},
		}
		_, err := store.resolveInheritanceChain("nonexistent")
		if err == nil {
			t.Error("expected error for missing recipe")
		}
	})

	t.Run("cycle detection", func(t *testing.T) {
		cycleA := &RecipeMetadata{}
		cycleA.Metadata.Name = "a"
		cycleA.Spec.Base = "b"

		cycleB := &RecipeMetadata{}
		cycleB.Metadata.Name = "b"
		cycleB.Spec.Base = "a"

		store := &MetadataStore{
			Base: baseMeta,
			Overlays: map[string]*RecipeMetadata{
				"a": cycleA,
				"b": cycleB,
			},
		}
		_, err := store.resolveInheritanceChain("a")
		if err == nil {
			t.Error("expected error for circular inheritance")
		}
	})

	t.Run("nil base in store", func(t *testing.T) {
		store := &MetadataStore{
			Overlays: map[string]*RecipeMetadata{
				testOverlayEKS: eksMeta,
			},
		}
		chain, err := store.resolveInheritanceChain(testOverlayEKS)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(chain) != 1 {
			t.Fatalf("chain length = %d, want 1", len(chain))
		}
	})
}

func TestMetadataStore_EvaluateOverlayConstraints(t *testing.T) {
	tests := []struct {
		name         string
		constraints  []Constraint
		evaluator    ConstraintEvaluatorFunc
		wantPassed   bool
		wantWarnings int
	}{
		{
			name:        "no constraints passes",
			constraints: nil,
			evaluator: func(_ Constraint) ConstraintEvalResult {
				return ConstraintEvalResult{Passed: true}
			},
			wantPassed:   true,
			wantWarnings: 0,
		},
		{
			name: "all constraints pass",
			constraints: []Constraint{
				{Name: "k8s", Value: ">= 1.30"},
				{Name: "os", Value: "ubuntu"},
			},
			evaluator: func(_ Constraint) ConstraintEvalResult {
				return ConstraintEvalResult{Passed: true, Actual: "matched"}
			},
			wantPassed:   true,
			wantWarnings: 0,
		},
		{
			name: "one constraint fails",
			constraints: []Constraint{
				{Name: "k8s", Value: ">= 1.30"},
				{Name: "os", Value: "ubuntu"},
			},
			evaluator: func(c Constraint) ConstraintEvalResult {
				if c.Name == "os" {
					return ConstraintEvalResult{Passed: false, Actual: "rhel"}
				}
				return ConstraintEvalResult{Passed: true, Actual: "1.31"}
			},
			wantPassed:   false,
			wantWarnings: 1,
		},
		{
			name: "evaluator returns error",
			constraints: []Constraint{
				{Name: "k8s", Value: ">= 1.30"},
			},
			evaluator: func(_ Constraint) ConstraintEvalResult {
				return ConstraintEvalResult{
					Passed: false,
					Actual: "unknown",
					Error:  fmt.Errorf("value not found"),
				}
			},
			wantPassed:   false,
			wantWarnings: 1,
		},
		{
			name: "mixed pass fail error",
			constraints: []Constraint{
				{Name: "k8s", Value: ">= 1.30"},
				{Name: "os", Value: "ubuntu"},
				{Name: "gpu", Value: "h100"},
			},
			evaluator: func(c Constraint) ConstraintEvalResult {
				switch c.Name {
				case "k8s":
					return ConstraintEvalResult{Passed: true, Actual: "1.31"}
				case "os":
					return ConstraintEvalResult{Passed: false, Actual: "rhel"}
				default:
					return ConstraintEvalResult{Error: fmt.Errorf("not found")}
				}
			},
			wantPassed:   false,
			wantWarnings: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			overlay := &RecipeMetadata{}
			overlay.Metadata.Name = "test-overlay"
			overlay.Spec.Constraints = tt.constraints

			store := &MetadataStore{}
			passed, warnings := store.evaluateOverlayConstraints(overlay, tt.evaluator)

			if passed != tt.wantPassed {
				t.Errorf("passed = %v, want %v", passed, tt.wantPassed)
			}
			if len(warnings) != tt.wantWarnings {
				t.Errorf("warnings count = %d, want %d", len(warnings), tt.wantWarnings)
			}
			for _, w := range warnings {
				if w.Overlay != "test-overlay" {
					t.Errorf("warning Overlay = %q, want %q", w.Overlay, "test-overlay")
				}
			}
		})
	}
}

func TestMetadataStore_FindMatchingOverlays(t *testing.T) {
	baseMeta := &RecipeMetadata{}
	baseMeta.Metadata.Name = testRecipeBase

	eksOverlay := &RecipeMetadata{}
	eksOverlay.Metadata.Name = "eks-overlay"
	eksOverlay.Spec.Criteria = &Criteria{
		Service: CriteriaServiceEKS,
	}

	gkeOverlay := &RecipeMetadata{}
	gkeOverlay.Metadata.Name = "gke-overlay"
	gkeOverlay.Spec.Criteria = &Criteria{
		Service: CriteriaServiceGKE,
	}

	noCriteriaOverlay := &RecipeMetadata{}
	noCriteriaOverlay.Metadata.Name = "no-criteria"

	store := &MetadataStore{
		Base: baseMeta,
		Overlays: map[string]*RecipeMetadata{
			"eks-overlay": eksOverlay,
			"gke-overlay": gkeOverlay,
			"no-criteria": noCriteriaOverlay,
		},
	}

	t.Run("matching criteria", func(t *testing.T) {
		criteria := &Criteria{Service: CriteriaServiceEKS}
		matches := store.FindMatchingOverlays(criteria)
		found := false
		for _, m := range matches {
			if m.Metadata.Name == "eks-overlay" {
				found = true
			}
		}
		if !found {
			t.Error("expected eks-overlay to match")
		}
	})

	t.Run("no matches", func(t *testing.T) {
		criteria := &Criteria{Service: CriteriaServiceAKS}
		matches := store.FindMatchingOverlays(criteria)
		if len(matches) != 0 {
			t.Errorf("expected 0 matches, got %d", len(matches))
		}
	})

	t.Run("empty store returns empty", func(t *testing.T) {
		emptyStore := &MetadataStore{
			Base:     baseMeta,
			Overlays: map[string]*RecipeMetadata{},
		}
		criteria := &Criteria{Service: CriteriaServiceEKS}
		matches := emptyStore.FindMatchingOverlays(criteria)
		if len(matches) != 0 {
			t.Errorf("expected 0 matches for empty store, got %d", len(matches))
		}
	})
}

func TestMetadataStore_FindMatchingOverlays_MaximalLeafSelection(t *testing.T) {
	baseMeta := &RecipeMetadata{}
	baseMeta.Metadata.Name = testRecipeBase

	// Build a chain: eks → eks-training → h100-eks-training
	eksOverlay := &RecipeMetadata{}
	eksOverlay.Metadata.Name = "eks"
	eksOverlay.Spec.Criteria = &Criteria{Service: CriteriaServiceEKS}

	eksTraining := &RecipeMetadata{}
	eksTraining.Metadata.Name = testOverlayEKSTraning
	eksTraining.Spec.Base = "eks"
	eksTraining.Spec.Criteria = &Criteria{
		Service: CriteriaServiceEKS,
		Intent:  CriteriaIntentTraining,
	}

	h100EksTraining := &RecipeMetadata{}
	h100EksTraining.Metadata.Name = "h100-eks-training"
	h100EksTraining.Spec.Base = testOverlayEKSTraning
	h100EksTraining.Spec.Criteria = &Criteria{
		Service:     CriteriaServiceEKS,
		Accelerator: CriteriaAcceleratorH100,
		Intent:      CriteriaIntentTraining,
	}

	store := &MetadataStore{
		Base: baseMeta,
		Overlays: map[string]*RecipeMetadata{
			"eks":                 eksOverlay,
			testOverlayEKSTraning: eksTraining,
			"h100-eks-training":   h100EksTraining,
		},
	}

	t.Run("filters ancestors when leaf matches", func(t *testing.T) {
		criteria := &Criteria{
			Service:     CriteriaServiceEKS,
			Accelerator: CriteriaAcceleratorH100,
			Intent:      CriteriaIntentTraining,
		}
		matches := store.FindMatchingOverlays(criteria)

		// Only h100-eks-training should survive — eks and eks-training are ancestors
		if len(matches) != 1 {
			names := make([]string, len(matches))
			for i, m := range matches {
				names[i] = m.Metadata.Name
			}
			t.Fatalf("expected 1 maximal leaf, got %d: %v", len(matches), names)
		}
		if matches[0].Metadata.Name != "h100-eks-training" {
			t.Errorf("expected h100-eks-training, got %s", matches[0].Metadata.Name)
		}
	})

	t.Run("keeps multiple leaves from different branches", func(t *testing.T) {
		// Add a sibling leaf on a different branch
		gb200EksTraining := &RecipeMetadata{}
		gb200EksTraining.Metadata.Name = "gb200-eks-training"
		gb200EksTraining.Spec.Base = testOverlayEKSTraning
		gb200EksTraining.Spec.Criteria = &Criteria{
			Service:     CriteriaServiceEKS,
			Accelerator: CriteriaAcceleratorGB200,
			Intent:      CriteriaIntentTraining,
		}
		store.Overlays["gb200-eks-training"] = gb200EksTraining
		t.Cleanup(func() { delete(store.Overlays, "gb200-eks-training") })

		// Query with all fields specified so both leaves match
		criteria := &Criteria{
			Service:     CriteriaServiceEKS,
			Accelerator: CriteriaAcceleratorH100,
			Intent:      CriteriaIntentTraining,
		}
		matches := store.FindMatchingOverlays(criteria)

		// h100-eks-training matches directly. gb200-eks-training does NOT match
		// because its accelerator (gb200) != query accelerator (h100).
		// eks and eks-training are ancestors of h100-eks-training, so filtered out.
		names := make(map[string]bool)
		for _, m := range matches {
			names[m.Metadata.Name] = true
		}
		if !names["h100-eks-training"] {
			t.Error("expected h100-eks-training in matches")
		}
		if names["gb200-eks-training"] {
			t.Error("gb200-eks-training should not match (wrong accelerator)")
		}
		if names[testOverlayEKSTraning] {
			t.Error("eks-training should be filtered as ancestor")
		}
		if names["eks"] {
			t.Error("eks should be filtered as ancestor")
		}

		// Now test with GB200 query — gb200-eks-training should be the only leaf
		criteriaGB200 := &Criteria{
			Service:     CriteriaServiceEKS,
			Accelerator: CriteriaAcceleratorGB200,
			Intent:      CriteriaIntentTraining,
		}
		matchesGB200 := store.FindMatchingOverlays(criteriaGB200)
		namesGB200 := make(map[string]bool)
		for _, m := range matchesGB200 {
			namesGB200[m.Metadata.Name] = true
		}
		if !namesGB200["gb200-eks-training"] {
			t.Error("expected gb200-eks-training in GB200 matches")
		}
		if namesGB200["h100-eks-training"] {
			t.Error("h100-eks-training should not match GB200 query")
		}
	})

	t.Run("no filtering when single match", func(t *testing.T) {
		criteria := &Criteria{
			Service: CriteriaServiceGKE,
			Intent:  CriteriaIntentTraining,
		}
		matches := store.FindMatchingOverlays(criteria)
		if len(matches) != 0 {
			t.Errorf("expected 0 matches for GKE, got %d", len(matches))
		}
	})
}

// TestBothBuildPathsProduceIdenticalContent verifies that BuildRecipeResult and
// BuildRecipeResultWithEvaluator (with a pass-all evaluator) produce identical
// hydrated recipe content for all leaf overlays discovered from recipes/overlays/.
// This is a characterization test for the maximal leaf candidate selection change.
func TestBothBuildPathsProduceIdenticalContent(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("failed to load metadata store: %v", err)
	}

	// Discover all leaf overlays: overlays not referenced as spec.base by any other overlay
	referencedAsBases := make(map[string]bool)
	for _, overlay := range store.Overlays {
		if overlay.Spec.Base != "" {
			referencedAsBases[overlay.Spec.Base] = true
		}
	}

	passAllEvaluator := func(_ Constraint) ConstraintEvalResult {
		return ConstraintEvalResult{Passed: true, Actual: "test"}
	}

	leafCount := 0
	for name, overlay := range store.Overlays {
		if referencedAsBases[name] {
			continue // not a leaf
		}
		if overlay.Spec.Criteria == nil {
			continue // no criteria
		}

		leafCount++
		t.Run(name, func(t *testing.T) {
			criteria := overlay.Spec.Criteria

			resultA, errA := store.BuildRecipeResult(ctx, criteria)
			if errA != nil {
				t.Fatalf("BuildRecipeResult failed: %v", errA)
			}

			resultB, errB := store.BuildRecipeResultWithEvaluator(ctx, criteria, passAllEvaluator)
			if errB != nil {
				t.Fatalf("BuildRecipeResultWithEvaluator failed: %v", errB)
			}

			// Compare constraints
			if len(resultA.Constraints) != len(resultB.Constraints) {
				t.Errorf("constraint count mismatch: %d vs %d", len(resultA.Constraints), len(resultB.Constraints))
			}
			for i := range resultA.Constraints {
				if i >= len(resultB.Constraints) {
					break
				}
				if resultA.Constraints[i].Name != resultB.Constraints[i].Name ||
					resultA.Constraints[i].Value != resultB.Constraints[i].Value {

					t.Errorf("constraint mismatch at %d: %v vs %v", i, resultA.Constraints[i], resultB.Constraints[i])
				}
			}

			// Compare full component refs (not just names — catch value-level drift)
			if !reflect.DeepEqual(resultA.ComponentRefs, resultB.ComponentRefs) {
				t.Errorf("component refs differ between build paths")
				if len(resultA.ComponentRefs) != len(resultB.ComponentRefs) {
					t.Errorf("  count: %d vs %d", len(resultA.ComponentRefs), len(resultB.ComponentRefs))
				}
				for i := range resultA.ComponentRefs {
					if i >= len(resultB.ComponentRefs) {
						break
					}
					if !reflect.DeepEqual(resultA.ComponentRefs[i], resultB.ComponentRefs[i]) {
						t.Errorf("  diff at %d: %s", i, resultA.ComponentRefs[i].Name)
					}
				}
			}

			// Compare deployment order
			if len(resultA.DeploymentOrder) != len(resultB.DeploymentOrder) {
				t.Errorf("deployment order count mismatch: %d vs %d", len(resultA.DeploymentOrder), len(resultB.DeploymentOrder))
			}
			for i := range resultA.DeploymentOrder {
				if i >= len(resultB.DeploymentOrder) {
					break
				}
				if resultA.DeploymentOrder[i] != resultB.DeploymentOrder[i] {
					t.Errorf("deployment order mismatch at %d: %s vs %s", i, resultA.DeploymentOrder[i], resultB.DeploymentOrder[i])
				}
			}

			// Compare applied overlays
			if len(resultA.Metadata.AppliedOverlays) != len(resultB.Metadata.AppliedOverlays) {
				t.Errorf("applied overlay count mismatch: %d vs %d",
					len(resultA.Metadata.AppliedOverlays), len(resultB.Metadata.AppliedOverlays))
			}
		})
	}

	if leafCount == 0 {
		t.Fatal("no leaf overlays discovered — test is not exercising any overlays")
	}
	t.Logf("verified %d leaf overlays through both build paths", leafCount)
}

// TestEvaluatorFailingLeafExcludesCandidate verifies that when a leaf overlay's
// constraints fail evaluation, no ancestor overlay is used as a fallback
// candidate. With maximal leaf selection, ancestors are not independent
// candidates — only non-excluded leaf candidates and non-chain overlays
// (like monitoring-hpa) remain applied.
func TestEvaluatorFailingLeafExcludesCandidate(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("failed to load metadata store: %v", err)
	}

	// Use criteria that match a specific leaf overlay
	criteria := &Criteria{
		Service:     CriteriaServiceEKS,
		Accelerator: CriteriaAcceleratorH100,
		Intent:      CriteriaIntentTraining,
		OS:          CriteriaOSUbuntu,
	}

	// Evaluator that fails all constraints
	failAllEvaluator := func(_ Constraint) ConstraintEvalResult {
		return ConstraintEvalResult{Passed: false, Actual: "fail"}
	}

	result, err := store.BuildRecipeResultWithEvaluator(ctx, criteria, failAllEvaluator)
	if err != nil {
		t.Fatalf("BuildRecipeResultWithEvaluator failed: %v", err)
	}

	// The leaf candidate (h100-eks-ubuntu-training) should be excluded
	if len(result.Metadata.ExcludedOverlays) == 0 {
		t.Fatal("expected at least one excluded overlay")
	}

	excluded := make(map[string]ExcludedOverlayReason)
	for _, overlay := range result.Metadata.ExcludedOverlays {
		excluded[overlay.Name] = overlay.Reason
	}

	// The leaf should be excluded
	if _, ok := excluded["h100-eks-ubuntu-training"]; !ok {
		t.Errorf("expected h100-eks-ubuntu-training in ExcludedOverlays, got %v", result.Metadata.ExcludedOverlays)
	}
	if excluded["h100-eks-ubuntu-training"] != ExcludedOverlayReasonConstraintFailed {
		t.Errorf("expected constraint-failed reason, got %q", excluded["h100-eks-ubuntu-training"])
	}

	// Ancestors should NOT appear in ExcludedOverlays (they were never candidates)
	for _, ancestor := range []string{"eks", testOverlayEKSTraning, "h100-eks-training"} {
		if _, ok := excluded[ancestor]; ok {
			t.Errorf("ancestor %q should not appear in ExcludedOverlays (not a candidate)", ancestor)
		}
	}

	// Applied overlays should not contain any ancestor of the excluded leaf.
	// Only base and non-chain overlays (like monitoring-hpa) should remain.
	applied := make(map[string]bool)
	for _, name := range result.Metadata.AppliedOverlays {
		applied[name] = true
	}
	for _, ancestor := range []string{"eks", testOverlayEKSTraning, "h100-eks-training"} {
		if applied[ancestor] {
			t.Errorf("ancestor %q should not be applied as fallback when leaf is excluded", ancestor)
		}
	}

	// base is always applied; monitoring-hpa matches intent:any and is not
	// an ancestor of h100-eks-ubuntu-training, so it remains as an independent leaf.
	if !applied["base"] {
		t.Error("base should always be applied")
	}
	if !applied["monitoring-hpa"] {
		t.Error("monitoring-hpa should remain applied (independent non-ancestor leaf)")
	}
}

// TestMixinConstraintFailureExcludesCandidate verifies that when a mixin-contributed
// constraint fails evaluation (e.g., os-ubuntu kernel constraint against a snapshot
// with kernel < 6.8), the composed candidate is excluded and the result falls back
// to base-only output. This tests the post-compose evaluation path in
// evaluateMixinConstraints.
func TestMixinConstraintFailureExcludesCandidate(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("failed to load metadata store: %v", err)
	}

	// Query that resolves to a leaf using the os-ubuntu mixin
	criteria := &Criteria{
		Service:     CriteriaServiceEKS,
		Accelerator: CriteriaAcceleratorH100,
		Intent:      CriteriaIntentTraining,
		OS:          CriteriaOSUbuntu,
	}

	// Evaluator that passes K8s constraint but fails OS/kernel constraints
	// (simulates a snapshot where OS matches but kernel is too old)
	selectiveEvaluator := func(c Constraint) ConstraintEvalResult {
		if c.Name == testK8sVersionConstant {
			return ConstraintEvalResult{Passed: true, Actual: "v1.35.0"}
		}
		// Fail OS-related constraints (these come from the os-ubuntu mixin)
		if c.Name == "OS.sysctl./proc/sys/kernel/osrelease" {
			return ConstraintEvalResult{Passed: false, Actual: "5.15.0"}
		}
		// Pass everything else
		return ConstraintEvalResult{Passed: true, Actual: "ok"}
	}

	result, err := store.BuildRecipeResultWithEvaluator(ctx, criteria, selectiveEvaluator)
	if err != nil {
		t.Fatalf("BuildRecipeResultWithEvaluator failed: %v", err)
	}

	// The mixin constraint (kernel >= 6.8) should have failed post-compose,
	// causing a fallback to base-only output
	if len(result.Metadata.ExcludedOverlays) == 0 {
		t.Fatal("expected excluded overlays from mixin constraint failure")
	}
	excluded := make(map[string]ExcludedOverlayReason)
	for _, overlay := range result.Metadata.ExcludedOverlays {
		excluded[overlay.Name] = overlay.Reason
	}
	if excluded["h100-eks-ubuntu-training"] != ExcludedOverlayReasonMixinConstraintFailed {
		t.Fatalf("expected mixin-constraint-failed reason, got %q", excluded["h100-eks-ubuntu-training"])
	}

	// Applied overlays should be base-only (plus monitoring-hpa which has no
	// mixin constraints and passes evaluation independently)
	applied := make(map[string]bool)
	for _, name := range result.Metadata.AppliedOverlays {
		applied[name] = true
	}
	if !applied[baseRecipeName] {
		t.Error("base should always be applied")
	}

	// The EKS chain overlays should NOT be in applied (they were part of the
	// composed candidate that failed post-compose evaluation)
	for _, name := range []string{"h100-eks-ubuntu-training", "h100-eks-training", "eks-training", "eks"} {
		if applied[name] {
			t.Errorf("%q should not be applied after mixin constraint failure", name)
		}
	}

	// Constraint warnings should include the failing mixin constraint
	foundKernelWarning := false
	for _, w := range result.Metadata.ConstraintWarnings {
		if w.Constraint == "OS.sysctl./proc/sys/kernel/osrelease" {
			foundKernelWarning = true
		}
	}
	if !foundKernelWarning {
		t.Error("expected constraint warning for OS.sysctl./proc/sys/kernel/osrelease from mixin")
	}

	t.Logf("excluded: %v", result.Metadata.ExcludedOverlays)
	t.Logf("applied: %v", result.Metadata.AppliedOverlays)
	t.Logf("warnings: %d", len(result.Metadata.ConstraintWarnings))
}

func TestMixinConstraintFailureExcludesOnlyAffectedCandidateChain(t *testing.T) {
	ctx := context.Background()

	baseMeta := &RecipeMetadata{}
	baseMeta.Metadata.Name = testRecipeBase

	sharedTraining := &RecipeMetadata{}
	sharedTraining.Metadata.Name = testOverlaySharedTrain
	sharedTraining.Spec.Criteria = &Criteria{
		Service: CriteriaServiceEKS,
		Intent:  CriteriaIntentTraining,
	}
	sharedTraining.Spec.Mixins = []string{"kernel-gate"}

	failingLeaf := &RecipeMetadata{}
	failingLeaf.Metadata.Name = "h100-shared-training"
	failingLeaf.Spec.Base = testOverlaySharedTrain
	failingLeaf.Spec.Criteria = &Criteria{
		Service:     CriteriaServiceEKS,
		Accelerator: CriteriaAcceleratorH100,
		Intent:      CriteriaIntentTraining,
	}

	independentLeaf := &RecipeMetadata{}
	independentLeaf.Metadata.Name = "monitoring"
	independentLeaf.Spec.Criteria = &Criteria{
		Service: CriteriaServiceEKS,
		Intent:  CriteriaIntentTraining,
	}
	independentLeaf.Spec.Mixins = []string{"monitoring-gate"}
	independentLeaf.Spec.ComponentRefs = []ComponentRef{
		{
			Name:   "dcgm-exporter",
			Type:   ComponentTypeHelm,
			Source: "https://example.com/charts",
			Chart:  "dcgm-exporter",
		},
	}

	store := &MetadataStore{
		Base: baseMeta,
		Overlays: map[string]*RecipeMetadata{
			testOverlaySharedTrain: sharedTraining,
			"h100-shared-training": failingLeaf,
			"monitoring":           independentLeaf,
		},
		Mixins: map[string]*RecipeMixin{
			"kernel-gate": {
				Kind:       RecipeMixinKind,
				APIVersion: RecipeAPIVersion,
				Metadata: struct {
					Name string `json:"name" yaml:"name"`
				}{
					Name: "kernel-gate",
				},
				Spec: struct {
					Constraints   []Constraint   `json:"constraints,omitempty" yaml:"constraints,omitempty"`
					ComponentRefs []ComponentRef `json:"componentRefs,omitempty" yaml:"componentRefs,omitempty"`
				}{
					Constraints: []Constraint{
						{Name: "OS.kernel", Value: ">= 6.8"},
					},
				},
			},
			"monitoring-gate": {
				Kind:       RecipeMixinKind,
				APIVersion: RecipeAPIVersion,
				Metadata: struct {
					Name string `json:"name" yaml:"name"`
				}{
					Name: "monitoring-gate",
				},
				Spec: struct {
					Constraints   []Constraint   `json:"constraints,omitempty" yaml:"constraints,omitempty"`
					ComponentRefs []ComponentRef `json:"componentRefs,omitempty" yaml:"componentRefs,omitempty"`
				}{
					Constraints: []Constraint{
						{Name: "Monitoring.enabled", Value: "true"},
					},
					ComponentRefs: []ComponentRef{
						{
							Name:   "nvidia-dcgm-exporter",
							Type:   ComponentTypeHelm,
							Source: "https://example.com/charts",
							Chart:  "nvidia-dcgm-exporter",
						},
					},
				},
			},
		},
	}

	criteria := &Criteria{
		Service:     CriteriaServiceEKS,
		Accelerator: CriteriaAcceleratorH100,
		Intent:      CriteriaIntentTraining,
	}

	evaluator := func(c Constraint) ConstraintEvalResult {
		switch c.Name {
		case "OS.kernel":
			return ConstraintEvalResult{Passed: false, Actual: "5.15"}
		case "Monitoring.enabled":
			return ConstraintEvalResult{Passed: true, Actual: "true"}
		default:
			return ConstraintEvalResult{Passed: true, Actual: "ok"}
		}
	}

	result, err := store.BuildRecipeResultWithEvaluator(ctx, criteria, evaluator)
	if err != nil {
		t.Fatalf("BuildRecipeResultWithEvaluator failed: %v", err)
	}

	excluded := make(map[string]ExcludedOverlayReason)
	for _, overlay := range result.Metadata.ExcludedOverlays {
		excluded[overlay.Name] = overlay.Reason
	}
	if _, ok := excluded["h100-shared-training"]; !ok {
		t.Fatalf("expected failed leaf in ExcludedOverlays, got %v", result.Metadata.ExcludedOverlays)
	}
	if excluded["h100-shared-training"] != ExcludedOverlayReasonMixinConstraintFailed {
		t.Fatalf("failed leaf reason = %q, want %q",
			excluded["h100-shared-training"], ExcludedOverlayReasonMixinConstraintFailed)
	}
	if _, ok := excluded[testOverlaySharedTrain]; ok {
		t.Fatalf("ancestor should not appear in ExcludedOverlays, got %v", result.Metadata.ExcludedOverlays)
	}
	if _, ok := excluded["monitoring"]; ok {
		t.Fatalf("independent passing leaf should not be excluded, got %v", result.Metadata.ExcludedOverlays)
	}

	applied := make(map[string]bool)
	for _, name := range result.Metadata.AppliedOverlays {
		applied[name] = true
	}
	if !applied[baseRecipeName] {
		t.Fatal("base should always remain applied")
	}
	if applied[testOverlaySharedTrain] || applied["h100-shared-training"] {
		t.Fatalf("failed candidate chain should be removed from applied overlays, got %v", result.Metadata.AppliedOverlays)
	}
	if !applied["monitoring"] {
		t.Fatalf("independent passing leaf should remain applied, got %v", result.Metadata.AppliedOverlays)
	}

	foundWarning := false
	for _, warning := range result.Metadata.ConstraintWarnings {
		if warning.Constraint != "OS.kernel" {
			continue
		}
		foundWarning = true
		if warning.Overlay != "h100-shared-training" {
			t.Fatalf("warning overlay = %q, want failed leaf candidate", warning.Overlay)
		}
		if warning.Reason != "mixin-constraint-failed: expected >= 6.8, got 5.15" {
			t.Fatalf("warning reason = %q", warning.Reason)
		}
	}
	if !foundWarning {
		t.Fatal("expected warning for failed mixin constraint")
	}

	componentNames := make(map[string]bool)
	for _, ref := range result.ComponentRefs {
		componentNames[ref.Name] = true
	}
	if !componentNames["nvidia-dcgm-exporter"] {
		t.Fatalf("surviving mixin component was dropped: %v", result.ComponentRefs)
	}
}

func TestMixinConstraintFailurePreservesSharedAncestorsForSurvivingLeaf(t *testing.T) {
	ctx := context.Background()

	baseMeta := &RecipeMetadata{}
	baseMeta.Metadata.Name = testRecipeBase

	sharedTraining := &RecipeMetadata{}
	sharedTraining.Metadata.Name = testOverlaySharedTrain
	sharedTraining.Spec.Criteria = &Criteria{
		Service: CriteriaServiceEKS,
		Intent:  CriteriaIntentTraining,
	}
	sharedTraining.Spec.ComponentRefs = []ComponentRef{
		{
			Name:   "shared-component",
			Type:   ComponentTypeHelm,
			Source: "https://example.com/charts",
			Chart:  "shared-component",
		},
	}

	failingLeaf := &RecipeMetadata{}
	failingLeaf.Metadata.Name = "leaf-a"
	failingLeaf.Spec.Base = testOverlaySharedTrain
	failingLeaf.Spec.Criteria = &Criteria{
		Service:     CriteriaServiceEKS,
		Accelerator: CriteriaAcceleratorH100,
		Intent:      CriteriaIntentTraining,
	}
	failingLeaf.Spec.Mixins = []string{"failing-mixin"}

	survivingLeaf := &RecipeMetadata{}
	survivingLeaf.Metadata.Name = "leaf-b"
	survivingLeaf.Spec.Base = testOverlaySharedTrain
	survivingLeaf.Spec.Criteria = &Criteria{
		Service:     CriteriaServiceEKS,
		Accelerator: CriteriaAcceleratorAny,
		Intent:      CriteriaIntentTraining,
	}
	survivingLeaf.Spec.Mixins = []string{"passing-mixin"}

	store := &MetadataStore{
		Base: baseMeta,
		Overlays: map[string]*RecipeMetadata{
			testOverlaySharedTrain: sharedTraining,
			"leaf-a":               failingLeaf,
			"leaf-b":               survivingLeaf,
		},
		Mixins: map[string]*RecipeMixin{
			"failing-mixin": {
				Kind:       RecipeMixinKind,
				APIVersion: RecipeAPIVersion,
				Metadata: struct {
					Name string `json:"name" yaml:"name"`
				}{Name: "failing-mixin"},
				Spec: struct {
					Constraints   []Constraint   `json:"constraints,omitempty" yaml:"constraints,omitempty"`
					ComponentRefs []ComponentRef `json:"componentRefs,omitempty" yaml:"componentRefs,omitempty"`
				}{
					Constraints: []Constraint{{Name: "GPU.ready", Value: "true"}},
				},
			},
			"passing-mixin": {
				Kind:       RecipeMixinKind,
				APIVersion: RecipeAPIVersion,
				Metadata: struct {
					Name string `json:"name" yaml:"name"`
				}{Name: "passing-mixin"},
				Spec: struct {
					Constraints   []Constraint   `json:"constraints,omitempty" yaml:"constraints,omitempty"`
					ComponentRefs []ComponentRef `json:"componentRefs,omitempty" yaml:"componentRefs,omitempty"`
				}{
					Constraints: []Constraint{{Name: "Monitoring.enabled", Value: "true"}},
					ComponentRefs: []ComponentRef{{
						Name:   "surviving-component",
						Type:   ComponentTypeHelm,
						Source: "https://example.com/charts",
						Chart:  "surviving-component",
					}},
				},
			},
		},
	}

	criteria := &Criteria{
		Service:     CriteriaServiceEKS,
		Accelerator: CriteriaAcceleratorH100,
		Intent:      CriteriaIntentTraining,
	}

	evaluator := func(c Constraint) ConstraintEvalResult {
		switch c.Name {
		case "GPU.ready":
			return ConstraintEvalResult{Passed: false, Actual: "false"}
		case "Monitoring.enabled":
			return ConstraintEvalResult{Passed: true, Actual: "true"}
		default:
			return ConstraintEvalResult{Passed: true, Actual: "ok"}
		}
	}

	result, err := store.BuildRecipeResultWithEvaluator(ctx, criteria, evaluator)
	if err != nil {
		t.Fatalf("BuildRecipeResultWithEvaluator failed: %v", err)
	}

	applied := make(map[string]bool)
	for _, name := range result.Metadata.AppliedOverlays {
		applied[name] = true
	}
	if !applied[testOverlaySharedTrain] {
		t.Fatalf("shared ancestor should remain applied for surviving leaf, got %v", result.Metadata.AppliedOverlays)
	}
	if !applied["leaf-b"] {
		t.Fatalf("surviving leaf should remain applied, got %v", result.Metadata.AppliedOverlays)
	}
	if applied["leaf-a"] {
		t.Fatalf("failed leaf should be excluded, got %v", result.Metadata.AppliedOverlays)
	}

	componentNames := make(map[string]bool)
	for _, ref := range result.ComponentRefs {
		componentNames[ref.Name] = true
	}
	if !componentNames["shared-component"] {
		t.Fatalf("shared ancestor component was lost after fallback rebuild: %v", result.ComponentRefs)
	}
	if !componentNames["surviving-component"] {
		t.Fatalf("surviving leaf mixin component was lost after fallback rebuild: %v", result.ComponentRefs)
	}
}

func TestEvaluateMixinConstraintsReturnsErrorWhenConstraintCannotBeMappedToCandidate(t *testing.T) {
	store := &MetadataStore{
		Overlays: map[string]*RecipeMetadata{
			"candidate": {
				RecipeMetadataHeader: RecipeMetadataHeader{
					Metadata: struct {
						Name string `json:"name" yaml:"name"`
					}{Name: "candidate"},
				},
			},
		},
	}

	result, err := store.evaluateMixinConstraints(
		&RecipeMetadataSpec{
			Constraints: []Constraint{
				{Name: "OS.kernel", Value: ">= 6.8"},
			},
		},
		func(_ Constraint) ConstraintEvalResult {
			return ConstraintEvalResult{Passed: false, Actual: "5.15"}
		},
		map[string]bool{"OS.kernel": true},
		[]string{"candidate"},
	)
	if err == nil {
		t.Fatal("expected error when mixin constraint cannot be mapped to any candidate")
	}
	if result.Failed {
		t.Fatal("expected zero-value result when mapping error occurs")
	}
	var structuredErr *aicrerrors.StructuredError
	if !errors.As(err, &structuredErr) {
		t.Fatalf("expected structured error, got %T", err)
	}
	if structuredErr.Code != aicrerrors.ErrCodeInternal {
		t.Fatalf("expected INTERNAL error code, got %s", structuredErr.Code)
	}
}

// TestMalformedMixinRejected verifies that mixin files with forbidden fields
// (base, criteria, mixins, validation) are rejected at load time by
// KnownFields(true) strict parsing.
func TestMalformedMixinRejected(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name: "mixin with forbidden base field",
			content: `kind: RecipeMixin
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: bad-mixin
spec:
  base: eks
  constraints:
    - name: test
      value: "1.0"
`,
		},
		{
			name: "mixin with forbidden criteria field",
			content: `kind: RecipeMixin
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: bad-mixin
spec:
  criteria:
    service: eks
  constraints:
    - name: test
      value: "1.0"
`,
		},
		{
			name: "mixin with forbidden validation field",
			content: `kind: RecipeMixin
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: bad-mixin
spec:
  validation:
    deployment:
      checks:
        - operator-health
  constraints:
    - name: test
      value: "1.0"
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mixin RecipeMixin
			decoder := yaml.NewDecoder(bytes.NewReader([]byte(tt.content)))
			decoder.KnownFields(true)
			err := decoder.Decode(&mixin)
			if err == nil {
				t.Error("expected error for mixin with forbidden fields, got nil")
			}
		})
	}
}

func TestExcludedOverlayUnmarshalYAML(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []ExcludedOverlay
		wantErr  bool
	}{
		{
			name:  "legacy string form",
			input: "- overlay-a\n- overlay-b\n",
			expected: []ExcludedOverlay{
				{Name: "overlay-a"},
				{Name: "overlay-b"},
			},
		},
		{
			name:  "object form",
			input: "- name: overlay-a\n  reason: constraint-failed\n- name: overlay-b\n  reason: mixin-constraint-failed\n",
			expected: []ExcludedOverlay{
				{Name: "overlay-a", Reason: ExcludedOverlayReasonConstraintFailed},
				{Name: "overlay-b", Reason: ExcludedOverlayReasonMixinConstraintFailed},
			},
		},
		{
			name:  "object form without reason",
			input: "- name: overlay-a\n",
			expected: []ExcludedOverlay{
				{Name: "overlay-a"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []ExcludedOverlay
			err := yaml.Unmarshal([]byte(tt.input), &got)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("got %+v, want %+v", got, tt.expected)
			}
		})
	}
}

func TestExcludedOverlayUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []ExcludedOverlay
		wantErr  bool
	}{
		{
			name:  "legacy string form",
			input: `["overlay-a","overlay-b"]`,
			expected: []ExcludedOverlay{
				{Name: "overlay-a"},
				{Name: "overlay-b"},
			},
		},
		{
			name:  "object form",
			input: `[{"name":"overlay-a","reason":"constraint-failed"},{"name":"overlay-b","reason":"mixin-constraint-failed"}]`,
			expected: []ExcludedOverlay{
				{Name: "overlay-a", Reason: ExcludedOverlayReasonConstraintFailed},
				{Name: "overlay-b", Reason: ExcludedOverlayReasonMixinConstraintFailed},
			},
		},
		{
			name:  "object form without reason",
			input: `[{"name":"overlay-a"}]`,
			expected: []ExcludedOverlay{
				{Name: "overlay-a"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []ExcludedOverlay
			err := json.Unmarshal([]byte(tt.input), &got)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("got %+v, want %+v", got, tt.expected)
			}
		})
	}
}

func TestBuildMixinConstraintWarningReason(t *testing.T) {
	tests := []struct {
		name       string
		constraint Constraint
		result     ConstraintEvalResult
		expected   string
	}{
		{
			name:       "with error",
			constraint: Constraint{Name: "kernel.version", Value: ">= 6.8"},
			result:     ConstraintEvalResult{Passed: false, Error: errors.New("parse error")},
			expected:   "mixin-constraint-failed: parse error",
		},
		{
			name:       "without error",
			constraint: Constraint{Name: "kernel.version", Value: ">= 6.8"},
			result:     ConstraintEvalResult{Passed: false, Actual: "5.15"},
			expected:   "mixin-constraint-failed: expected >= 6.8, got 5.15",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildMixinConstraintWarningReason(tt.constraint, tt.result)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}
