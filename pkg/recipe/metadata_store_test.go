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
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/manifest"
	"golang.org/x/sync/errgroup"
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
		wantErr      bool
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
			// Models a measurement absent from the snapshot — the evaluator's
			// designed NotFound degradation signal (design 5.2) — so it must
			// still exclude gracefully with a warning, not fail the build.
			name: "evaluator returns NotFound error",
			constraints: []Constraint{
				{Name: "k8s", Value: ">= 1.30"},
			},
			evaluator: func(_ Constraint) ConstraintEvalResult {
				return ConstraintEvalResult{
					Passed: false,
					Actual: "unknown",
					Error:  aicrerrors.New(aicrerrors.ErrCodeNotFound, "value not found"),
				}
			},
			wantPassed:   false,
			wantWarnings: 1,
		},
		{
			// The default branch models a measurement absent from the
			// snapshot (NotFound); mixed with an ordinary constraint failure
			// (os) to confirm both still surface as warnings, not an error.
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
					return ConstraintEvalResult{Error: aicrerrors.New(aicrerrors.ErrCodeNotFound, "not found")}
				}
			},
			wantPassed:   false,
			wantWarnings: 2,
		},
		{
			// Models a malformed constraint / internal evaluator failure —
			// must fail closed (propagated error), never degrade gracefully.
			name: "evaluator returns non-NotFound error fails closed",
			constraints: []Constraint{
				{Name: "k8s", Value: ">= 1.30"},
			},
			evaluator: func(_ Constraint) ConstraintEvalResult {
				return ConstraintEvalResult{
					Passed: false,
					Error:  aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "invalid constraint expression"),
				}
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			overlay := &RecipeMetadata{}
			overlay.Metadata.Name = "test-overlay"
			overlay.Spec.Constraints = tt.constraints

			store := &MetadataStore{}
			passed, warnings, err := store.evaluateOverlayConstraints(overlay, tt.evaluator)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
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

// TestMetadataStore_EvaluateOverlayConstraints_WrapsUnstructuredError covers
// CodeRabbit's finding: ConstraintEvaluatorFunc accepts a plain error, so a
// non-StructuredError evaluator failure must be wrapped with a code (rather
// than propagated bare) or it reaches the server layer as an uncoded 500.
func TestMetadataStore_EvaluateOverlayConstraints_WrapsUnstructuredError(t *testing.T) {
	overlay := &RecipeMetadata{}
	overlay.Metadata.Name = "test-overlay"
	overlay.Spec.Constraints = []Constraint{
		{Name: "k8s", Value: ">= 1.30"},
	}

	store := &MetadataStore{}
	_, _, err := store.evaluateOverlayConstraints(overlay, func(_ Constraint) ConstraintEvalResult {
		return ConstraintEvalResult{Passed: false, Error: errors.New("boom")}
	})

	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInternal, "")) {
		t.Fatalf("expected ErrCodeInternal, got %v", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected cause %q preserved in error chain, got %v", "boom", err)
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

// TestSlurmLeavesClearInheritedPerformancePhase is a regression guard for
// issue #1000: leaves like h100-eks-ubuntu-training-slurm and
// h100-gke-cos-training-slurm declare `performance.checks: []` /
// `constraints: []` to drop the K8s-native nccl-all-reduce-bw check, which
// bypasses slurmd on a Slurm-managed cluster. The fix in mergeValidationPhase
// distinguishes an omitted overlay list (nil → inherit) from an explicit
// empty list (`[]` → clear), so these recipes resolve with no performance
// checks and the phase is skipped by FilterEntriesByValidation.
func TestSlurmLeavesClearInheritedPerformancePhase(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("failed to load metadata store: %v", err)
	}

	for _, name := range []string{
		"gb200-eks-ubuntu-training-slurm",
		"h100-aks-ubuntu-training-slurm",
		"h100-eks-ubuntu-training-slurm",
		"h100-gke-cos-training-slurm",
	} {
		t.Run(name, func(t *testing.T) {
			leaf, ok := store.GetRecipeByName(name)
			if !ok {
				t.Fatalf("overlay %q not found in store", name)
			}
			result, err := store.BuildRecipeResult(ctx, leaf.Spec.Criteria)
			if err != nil {
				t.Fatalf("BuildRecipeResult failed: %v", err)
			}
			if result.Validation == nil || result.Validation.Performance == nil {
				t.Fatalf("performance phase missing from resolved recipe")
			}
			if got := result.Validation.Performance.Checks; len(got) != 0 {
				t.Errorf("performance.checks = %v, want empty — Slurm leaf must drop inherited K8s-native checks", got)
			}
			if got := result.Validation.Performance.Constraints; len(got) != 0 {
				t.Errorf("performance.constraints = %v, want empty — Slurm leaf must drop inherited K8s-native constraints", got)
			}
		})
	}
}

func TestGPUSlurmLeavesConfigureGRESAndTaskCgroup(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("failed to load metadata store: %v", err)
	}

	tests := []struct {
		name     string
		wantGres string
		wantGPUs int
	}{
		{name: "gb200-eks-ubuntu-training-slurm", wantGres: "gpu:gb200:4", wantGPUs: 4},
		{name: "h100-aks-ubuntu-training-slurm", wantGres: "gpu:h100:8", wantGPUs: 8},
		{name: "h100-eks-ubuntu-training-slurm", wantGres: "gpu:h100:8", wantGPUs: 8},
		{name: "h100-gke-cos-training-slurm", wantGres: "gpu:h100:8", wantGPUs: 8},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			leaf, ok := store.GetRecipeByName(tt.name)
			if !ok {
				t.Fatalf("overlay %q not found in store", tt.name)
			}
			result, err := store.BuildRecipeResult(ctx, leaf.Spec.Criteria)
			if err != nil {
				t.Fatalf("BuildRecipeResult failed: %v", err)
			}
			values, err := result.GetValuesForComponentWithContext(ctx, "slinky-slurm")
			if err != nil {
				t.Fatalf("GetValuesForComponentWithContext(slinky-slurm) failed: %v", err)
			}
			if got := valueAtPath[string](t, values, "controller", "extraConfMap", "GresTypes"); got != "gpu" {
				t.Errorf("controller.extraConfMap.GresTypes = %q, want gpu", got)
			}
			if got := valueAtPath[string](t, values, "controller", "extraConfMap", "TaskPlugin"); got != "task/cgroup,task/affinity" {
				t.Errorf("controller.extraConfMap.TaskPlugin = %q, want task/cgroup,task/affinity", got)
			}
			if got := valueAtPath[string](t, values, "nodesets", "slinky", "extraConfMap", "Gres"); got != tt.wantGres {
				t.Errorf("nodesets.slinky.extraConfMap.Gres = %q, want %q", got, tt.wantGres)
			}
			if got := valueAtPath[int](t, values, "nodesets", "slinky", "slurmd", "resources", "limits", "nvidia.com/gpu"); got != tt.wantGPUs {
				t.Errorf("nodesets.slinky.slurmd.resources.limits.nvidia.com/gpu = %d, want %d", got, tt.wantGPUs)
			}
		})
	}
}

func TestKindSlurmLeafOmitsTaskPlugin(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("failed to load metadata store: %v", err)
	}

	leaf, ok := store.GetRecipeByName("h100-kind-training-slurm")
	if !ok {
		t.Fatal("overlay h100-kind-training-slurm not found in store")
	}
	result, err := store.BuildRecipeResult(ctx, leaf.Spec.Criteria)
	if err != nil {
		t.Fatalf("BuildRecipeResult failed: %v", err)
	}
	values, err := result.GetValuesForComponentWithContext(ctx, "slinky-slurm")
	if err != nil {
		t.Fatalf("GetValuesForComponentWithContext(slinky-slurm) failed: %v", err)
	}
	extraConfMap := valueAtPath[map[string]any](t, values, "controller", "extraConfMap")
	if got, exists := extraConfMap["TaskPlugin"]; exists {
		t.Errorf("controller.extraConfMap.TaskPlugin = %v, want omitted for Kind", got)
	}
}

func TestSlurmLeavesAppendConformanceHealthCheck(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("failed to load metadata store: %v", err)
	}

	conformanceChecks := []string{
		"platform-health",
		"gpu-operator-health",
		"dra-support",
		"accelerator-metrics",
		"ai-service-metrics",
		"gang-scheduling",
		"pod-autoscaling",
		"cluster-autoscaling",
		"robust-controller",
		"secure-accelerator-access",
		"slinky-slurm-health",
	}
	gb200ConformanceChecks := []string{
		"platform-health",
		"gpu-operator-health",
		"dra-support",
		"accelerator-metrics",
		"ai-service-metrics",
		"gang-scheduling",
		"pod-autoscaling",
		"cluster-autoscaling",
		"slinky-slurm-health",
		"slinky-slurm-imex-channel",
	}
	kindConformanceChecks := []string{
		"platform-health",
		"gpu-operator-health",
		"dra-support",
		"accelerator-metrics",
		"ai-service-metrics",
		"gang-scheduling",
		"secure-accelerator-access",
		"pod-autoscaling",
		"cluster-autoscaling",
		"slinky-slurm-health",
	}

	tests := []struct {
		name string
		want []string
	}{
		{name: "gb200-eks-ubuntu-training-slurm", want: gb200ConformanceChecks},
		{name: "h100-aks-ubuntu-training-slurm", want: conformanceChecks},
		{name: "h100-eks-ubuntu-training-slurm", want: conformanceChecks},
		{name: "h100-gke-cos-training-slurm", want: conformanceChecks},
		{name: "h100-kind-training-slurm", want: kindConformanceChecks},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			leaf, ok := store.GetRecipeByName(tt.name)
			if !ok {
				t.Fatalf("overlay %q not found in store", tt.name)
			}
			result, err := store.BuildRecipeResult(ctx, leaf.Spec.Criteria)
			if err != nil {
				t.Fatalf("BuildRecipeResult failed: %v", err)
			}
			if result.Validation == nil || result.Validation.Conformance == nil {
				t.Fatalf("conformance phase missing from resolved recipe")
			}
			if got := result.Validation.Conformance.Checks; !slices.Equal(got, tt.want) {
				t.Errorf("conformance.checks = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGB200EKSSlurmWiresIMEXComputeDomain(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("failed to load metadata store: %v", err)
	}

	leaf, ok := store.GetRecipeByName("gb200-eks-ubuntu-training-slurm")
	if !ok {
		t.Fatal("overlay gb200-eks-ubuntu-training-slurm not found in store")
	}
	result, err := store.BuildRecipeResult(ctx, leaf.Spec.Criteria)
	if err != nil {
		t.Fatalf("BuildRecipeResult failed: %v", err)
	}
	if !slices.ContainsFunc(
		result.Constraints,
		func(c Constraint) bool {
			return c.Name == "K8s.server.version" && c.Value == ">= 1.34"
		},
	) {

		t.Errorf("constraints = %v, want K8s.server.version >= 1.34 for DRA v1", result.Constraints)
	}

	if computeDomain := result.GetComponentRef("slinky-slurm-imex-compute-domain"); computeDomain != nil {
		t.Errorf("standalone IMEX ComputeDomain component = %+v, want absent", computeDomain)
	}
	slurm := result.GetComponentRef("slinky-slurm")
	if slurm == nil {
		t.Fatal("slinky-slurm component missing")
	}
	const manifestPath = "components/slinky-slurm/manifests/compute-domain.yaml"
	if !slices.Contains(slurm.PreManifestFiles, manifestPath) {
		t.Errorf("slinky-slurm preManifestFiles = %v, want %q", slurm.PreManifestFiles, manifestPath)
	}
	if !slices.Contains(slurm.DependencyRefs, "nvidia-dra-driver-gpu") {
		t.Errorf("slinky-slurm dependencyRefs = %v, want nvidia-dra-driver-gpu", slurm.DependencyRefs)
	}

	values, err := result.GetValuesForComponent("slinky-slurm")
	if err != nil {
		t.Fatalf("GetValuesForComponent(slinky-slurm) failed: %v", err)
	}
	if got := valueAtPath[string](t, values, "controller", "extraConfMap", "SwitchType"); got != "switch/nvidia_imex" {
		t.Errorf("controller.extraConfMap.SwitchType = %q, want switch/nvidia_imex", got)
	}
	if got := valueAtPath[string](t, values, "nodesets", "slinky", "extraConfMap", "Gres"); got != "gpu:gb200:4" {
		t.Errorf("nodesets.slinky.extraConfMap.Gres = %q, want gpu:gb200:4", got)
	}

	podClaims := valueAtPath[[]any](t, values, "nodesets", "slinky", "podSpec", "resourceClaims")
	assertSingleNameField(t, podClaims, "name", "imex-channels")
	assertSingleNameField(t, podClaims, "resourceClaimTemplateName", "slinky-slurm-imex-channels")
	nodeSetClaim, ok := podClaims[0].(map[string]any)
	if !ok {
		t.Fatalf("podClaims[0] = %T, want map[string]any", podClaims[0])
	}
	nodeSetRCTName, ok := nodeSetClaim["resourceClaimTemplateName"].(string)
	if !ok {
		t.Fatalf("podClaims[0].resourceClaimTemplateName = %T, want string", nodeSetClaim["resourceClaimTemplateName"])
	}
	containerClaims := valueAtPath[[]any](t, values, "nodesets", "slinky", "slurmd", "resources", "claims")
	assertSingleNameField(t, containerClaims, "name", "imex-channels")
	slurmd := valueAtPath[map[string]any](t, values, "nodesets", "slinky", "slurmd")
	if got, ok := slurmd["securityContext"]; ok {
		t.Errorf("nodesets.slinky.slurmd.securityContext = %v, want omitted to use chart default", got)
	}
	// Pyxis images are pinned in components/slinky-slurm/values.yaml and must
	// survive leaf slurmd.resources overrides (deep merge, not replace).
	if got := valueAtPath[string](t, values, "nodesets", "slinky", "slurmd", "image", "repository"); got != "ghcr.io/slinkyproject/slurmd-pyxis" {
		t.Errorf("nodesets.slinky.slurmd.image.repository = %q, want ghcr.io/slinkyproject/slurmd-pyxis", got)
	}
	if got := valueAtPath[string](t, values, "loginsets", "slinky", "login", "image", "repository"); got != "ghcr.io/slinkyproject/login-pyxis" {
		t.Errorf("loginsets.slinky.login.image.repository = %q, want ghcr.io/slinkyproject/login-pyxis", got)
	}
	// Operator-managed /etc/slurm hides the image plugstack.conf.d symlink;
	// configFiles.plugstack.conf is what actually loads spank_pyxis.so.
	// Under configless, login/workers pick it up in /run/slurm/conf/ after
	// pod start (default relative PlugStackConfig); no absolute pin needed.
	if got := valueAtPath[string](t, values, "configFiles", "plugstack.conf"); !strings.Contains(got, "include /usr/share/pyxis/*") {
		t.Errorf("configFiles.plugstack.conf = %q, want include /usr/share/pyxis/*", got)
	}

	content, err := GetManifestContentWithContext(ctx, result.DataProvider(), manifestPath)
	if err != nil {
		t.Fatalf("GetManifestContentWithContext(%q) failed: %v", manifestPath, err)
	}
	rendered, err := manifest.Render(content, manifest.RenderInput{
		ComponentName: slurm.Name,
		Namespace:     slurm.Namespace,
		ChartName:     slurm.Chart,
		ChartVersion:  slurm.Version,
		Values:        values,
	})
	if err != nil {
		t.Fatalf("render ComputeDomain manifest: %v", err)
	}
	var computeDomain map[string]any
	if err := yaml.Unmarshal(rendered, &computeDomain); err != nil {
		t.Fatalf("unmarshal rendered ComputeDomain: %v", err)
	}
	computeDomainRCTName := valueAtPath[string](t, computeDomain, "spec", "channel", "resourceClaimTemplate", "name")
	if computeDomainRCTName != nodeSetRCTName {
		t.Errorf("ComputeDomain RCT name = %q, NodeSet RCT name = %q", computeDomainRCTName, nodeSetRCTName)
	}
}

func TestSlinkySlurmIMEXComputeDomainFixedIdentityCannotBeOverridden(t *testing.T) {
	ctx := context.Background()
	const manifestPath = "components/slinky-slurm/manifests/compute-domain.yaml"
	content, err := GetManifestContentWithContext(ctx, nil, manifestPath)
	if err != nil {
		t.Fatalf("GetManifestContentWithContext(%q) failed: %v", manifestPath, err)
	}

	// Scalar --set and typed --set-json/--set-file converge on this final
	// values map before local manifests are rendered. None may change the
	// immutable ComputeDomain integration contract.
	tests := []struct {
		name   string
		values map[string]any
	}{
		{
			name: "scalar --set",
			values: map[string]any{
				"name":                      "from-set",
				"allocationMode":            "Immediate",
				"resourceClaimTemplateName": "from-set",
			},
		},
		{
			name: "typed --set-json or --set-file",
			values: map[string]any{
				"name":                      "from-typed-set",
				"allocationMode":            "Immediate",
				"resourceClaimTemplateName": "from-typed-set",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rendered, renderErr := manifest.Render(content, manifest.RenderInput{
				ComponentName: "slinky-slurm",
				Namespace:     "slurm",
				ChartName:     "slurm",
				ChartVersion:  "1.1.0",
				Values:        tt.values,
			})
			if renderErr != nil {
				t.Fatalf("render ComputeDomain manifest: %v", renderErr)
			}

			for _, want := range []string{
				"name: slinky-slurm-imex",
				"allocationMode: All",
				"name: slinky-slurm-imex-channels",
			} {
				if !strings.Contains(string(rendered), want) {
					t.Errorf("rendered ComputeDomain manifest missing fixed value %q:\n%s", want, rendered)
				}
			}
			for _, unwanted := range []string{"from-set", "from-typed-set", "allocationMode: Immediate"} {
				if strings.Contains(string(rendered), unwanted) {
					t.Errorf("rendered ComputeDomain manifest contains override value %q:\n%s", unwanted, rendered)
				}
			}
		})
	}
}

func valueAtPath[T any](t *testing.T, root map[string]any, path ...string) T {
	t.Helper()

	if len(path) == 0 {
		t.Fatal("value path must not be empty")
	}

	var current any = root
	for _, key := range path {
		m, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("%q parent is %T, want map[string]any", key, current)
		}
		current, ok = m[key]
		if !ok {
			t.Fatalf("missing nested key path %v", path)
		}
	}
	value, ok := current.(T)
	if !ok {
		var expected T
		t.Fatalf("nested path %v = %T, want %T", path, current, expected)
	}
	return value
}

func assertSingleNameField(t *testing.T, items []any, field, want string) {
	t.Helper()

	if len(items) != 1 {
		t.Fatalf("items length = %d, want 1: %v", len(items), items)
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("items[0] = %T, want map[string]any", items[0])
	}
	if got, ok := item[field].(string); !ok || got != want {
		t.Fatalf("items[0].%s = %v, want %q", field, item[field], want)
	}
}

// TestEvaluatorFailingLeafExcludesCandidate pins issue #1542: when
// constraint evaluation excludes the only overlay covering the full
// stated criteria (service+accelerator+intent+os), the surviving
// overlays (base, plus the independent non-ancestor monitoring-hpa) no
// longer satisfy the query's stated dimensions. Before task 6 wired the
// evaluator path into verifyCriteriaCoverage, this silently shipped a
// base-only result; now it must fail the build instead of masking the
// dropped coverage. The excludedOverlays context on the returned error
// still pins the underlying exclusion-isolation invariant: only the
// matched leaf is excluded, never its ancestors.
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

	_, err = store.BuildRecipeResultWithEvaluator(ctx, criteria, failAllEvaluator)
	if err == nil {
		t.Fatal("expected coverage error: excluding the only full-match leaf uncovers stated dimensions")
	}
	if !errors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("expected ErrCodeInvalidRequest, got %v", err)
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) {
		t.Fatalf("expected StructuredError, got %v", err)
	}

	excludedOverlays, ok := se.Context["excludedOverlays"].([]ExcludedOverlay)
	if !ok || len(excludedOverlays) == 0 {
		t.Fatalf("expected non-empty excludedOverlays context, got %v", se.Context["excludedOverlays"])
	}
	excluded := make(map[string]ExcludedOverlayReason)
	for _, overlay := range excludedOverlays {
		excluded[overlay.Name] = overlay.Reason
	}

	// The leaf should be excluded
	if _, ok := excluded["h100-eks-ubuntu-training"]; !ok {
		t.Errorf("expected h100-eks-ubuntu-training in excludedOverlays, got %v", excludedOverlays)
	}
	if excluded["h100-eks-ubuntu-training"] != ExcludedOverlayReasonConstraintFailed {
		t.Errorf("expected constraint-failed reason, got %q", excluded["h100-eks-ubuntu-training"])
	}

	// Ancestors should NOT appear in excludedOverlays (they were never candidates)
	for _, ancestor := range []string{"eks", testOverlayEKSTraning, "h100-eks-training"} {
		if _, ok := excluded[ancestor]; ok {
			t.Errorf("ancestor %q should not appear in excludedOverlays (not a candidate)", ancestor)
		}
	}
}

// TestMixinOSTalos_AppliesPrivilegedNamespacesAndPreManifests is an e2e
// check that the os-talos mixin (shipping in recipes/mixins/os-talos.yaml)
// correctly redirects each affected component to its privileged-<name>
// namespace and attaches the per-component manifests/talos-namespace.yaml under
// PreManifestFiles when applied to a recipe whose inheritance chain
// already declares those components.
//
// Exercises the ADR-005 carve-out for additive-field mixin merges
// (Namespace + PreManifestFiles only); identity/sourcing fields are
// covered by TestMixinComponentRefSafeForMerge.
func TestMixinOSTalos_AppliesPrivilegedNamespacesAndPreManifests(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}

	if _, ok := store.Mixins["os-talos"]; !ok {
		t.Fatalf("os-talos mixin not present in metadata store; check recipes/mixins/os-talos.yaml")
	}

	// Components the os-talos mixin overrides. Each maps to its expected
	// privileged namespace and the per-component namespace manifest path.
	type want struct {
		namespace    string
		manifestPath string
	}
	wants := map[string]want{
		"gpu-operator":          {"privileged-gpu-operator", "components/gpu-operator/manifests/talos-namespace.yaml"},
		"network-operator":      {"privileged-network-operator", "components/network-operator/manifests/talos-namespace.yaml"},
		"nvsentinel":            {"privileged-nvsentinel", "components/nvsentinel/manifests/talos-namespace.yaml"},
		"nvidia-dra-driver-gpu": {"privileged-nvidia-dra-driver-gpu", "components/nvidia-dra-driver-gpu/manifests/talos-namespace.yaml"},
		"nodewright-operator":   {"privileged-nodewright-operator", "components/nodewright-operator/manifests/talos-namespace.yaml"},
	}

	// Simulate an inheritance chain that already declared each of the five
	// components with concrete identity fields. The mixin must merge into
	// these (additive-field-only) without conflicting.
	spec := RecipeMetadataSpec{
		Mixins: []string{"os-talos"},
		ComponentRefs: []ComponentRef{
			{Name: "gpu-operator", Chart: "gpu-operator", Version: "v25.3.3", Source: "https://helm.ngc.nvidia.com/nvidia", Type: ComponentTypeHelm, Namespace: "gpu-operator"},
			{Name: "network-operator", Chart: "network-operator", Version: "v24.7.0", Source: "https://helm.ngc.nvidia.com/nvidia", Type: ComponentTypeHelm, Namespace: "nvidia-network-operator"},
			{Name: "nvsentinel", Chart: "nvsentinel", Version: "v0.1.0", Source: "oci://ghcr.io/nvidia", Type: ComponentTypeHelm, Namespace: "nvsentinel"},
			{Name: "nvidia-dra-driver-gpu", Chart: "nvidia-dra-driver-gpu", Version: "v25.3.0", Source: "https://helm.ngc.nvidia.com/nvidia", Type: ComponentTypeHelm, Namespace: "nvidia-dra-driver-gpu"},
			{Name: "nodewright-operator", Chart: "nodewright-operator", Version: "v0.1.0", Source: "oci://ghcr.io/nvidia", Type: ComponentTypeHelm, Namespace: "nodewright"},
		},
	}

	if _, err := store.mergeMixins(&spec); err != nil {
		t.Fatalf("mergeMixins: %v", err)
	}

	got := make(map[string]ComponentRef, len(spec.ComponentRefs))
	for _, c := range spec.ComponentRefs {
		got[c.Name] = c
	}

	for name, w := range wants {
		c, ok := got[name]
		if !ok {
			t.Errorf("component %q missing from merged spec", name)
			continue
		}
		if c.Namespace != w.namespace {
			t.Errorf("%s: Namespace = %q, want %q", name, c.Namespace, w.namespace)
		}
		if !slices.Contains(c.PreManifestFiles, w.manifestPath) {
			t.Errorf("%s: PreManifestFiles = %v, want to contain %q", name, c.PreManifestFiles, w.manifestPath)
		}
	}

	// Mixins field must be stripped from the materialized spec.
	if len(spec.Mixins) != 0 {
		t.Errorf("Mixins not stripped after merge: %v", spec.Mixins)
	}
}

// TestMixinComponentRefSafeForMerge pins the field-scoped relaxation of
// ADR-005's "no duplicate component names" rule: a mixin componentRef whose
// name collides with the inheritance chain is allowed if and only if the
// mixin sets only the safe additive fields (Name, Namespace, ManifestFiles,
// PreManifestFiles). Identity / sourcing fields must still trigger a
// conflict so a mixin cannot silently re-point a chart's chart, version,
// source, values, etc.
func TestMixinComponentRefSafeForMerge(t *testing.T) {
	tests := []struct {
		name          string
		ref           ComponentRef
		wantSafe      bool
		wantOffending string
	}{
		{
			name:     "empty ref (Name only) is safe",
			ref:      ComponentRef{Name: "gpu-operator"},
			wantSafe: true,
		},
		{
			name:     "namespace-only override is safe",
			ref:      ComponentRef{Name: "gpu-operator", Namespace: "privileged-gpu-operator"},
			wantSafe: true,
		},
		{
			name: "preManifestFiles-only is safe",
			ref: ComponentRef{
				Name:             "gpu-operator",
				PreManifestFiles: []string{"components/gpu-operator/manifests/talos-namespace.yaml"},
			},
			wantSafe: true,
		},
		{
			name: "manifestFiles-only is safe",
			ref: ComponentRef{
				Name:          "gpu-operator",
				ManifestFiles: []string{"components/gpu-operator/manifests/extra.yaml"},
			},
			wantSafe: true,
		},
		{
			name: "namespace + pre + post combined is safe",
			ref: ComponentRef{
				Name:             "gpu-operator",
				Namespace:        "privileged-gpu-operator",
				PreManifestFiles: []string{"components/gpu-operator/manifests/talos-namespace.yaml"},
				ManifestFiles:    []string{"components/gpu-operator/manifests/extra.yaml"},
			},
			wantSafe: true,
		},
		{
			name:          "chart set -> conflict",
			ref:           ComponentRef{Name: "gpu-operator", Chart: "something-else"},
			wantSafe:      false,
			wantOffending: "chart",
		},
		{
			name:          "source set -> conflict",
			ref:           ComponentRef{Name: "gpu-operator", Source: "https://example.com/charts"},
			wantSafe:      false,
			wantOffending: "source",
		},
		{
			name:          "version set -> conflict",
			ref:           ComponentRef{Name: "gpu-operator", Version: "v99"},
			wantSafe:      false,
			wantOffending: "version",
		},
		{
			name:          "type set -> conflict",
			ref:           ComponentRef{Name: "gpu-operator", Type: ComponentTypeHelm},
			wantSafe:      false,
			wantOffending: "type",
		},
		{
			name:          "valuesFile set -> conflict",
			ref:           ComponentRef{Name: "gpu-operator", ValuesFile: "components/gpu-operator/values.yaml"},
			wantSafe:      false,
			wantOffending: "valuesFile",
		},
		{
			name: "overrides set -> conflict",
			ref: ComponentRef{
				Name:      "gpu-operator",
				Overrides: map[string]any{"driver": map[string]any{"enabled": false}},
			},
			wantSafe:      false,
			wantOffending: "overrides",
		},
		{
			name: "dependencyRefs set -> conflict",
			ref: ComponentRef{
				Name:           "gpu-operator",
				DependencyRefs: []string{"cert-manager"},
			},
			wantSafe:      false,
			wantOffending: "dependencyRefs",
		},
		{
			name:          "cleanup=true -> conflict",
			ref:           ComponentRef{Name: "gpu-operator", Cleanup: true},
			wantSafe:      false,
			wantOffending: "cleanup",
		},
		{
			name:          "healthCheckAsserts set -> conflict",
			ref:           ComponentRef{Name: "gpu-operator", HealthCheckAsserts: "apiVersion: v1"},
			wantSafe:      false,
			wantOffending: "healthCheckAsserts",
		},
		{
			name:          "tag set -> conflict (Kustomize identity)",
			ref:           ComponentRef{Name: "kustomize-app", Tag: "v1.0.0"},
			wantSafe:      false,
			wantOffending: "tag",
		},
		{
			name:          "path set -> conflict (Kustomize identity)",
			ref:           ComponentRef{Name: "kustomize-app", Path: "deploy/production"},
			wantSafe:      false,
			wantOffending: "path",
		},
		{
			name: "patches set -> conflict",
			ref: ComponentRef{
				Name:    "kustomize-app",
				Patches: []string{"patches/namespace.yaml"},
			},
			wantSafe:      false,
			wantOffending: "patches",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			offending, ok := mixinComponentRefSafeForMerge(tt.ref)
			if ok != tt.wantSafe {
				t.Errorf("safe = %v, want %v (offending=%q)", ok, tt.wantSafe, offending)
			}
			if offending != tt.wantOffending {
				t.Errorf("offending = %q, want %q", offending, tt.wantOffending)
			}
		})
	}
}

// TestMixinConstraintFailureExcludesCandidate pins issue #1542 for the
// mixin post-compose path: excluding the only overlay covering the full
// stated criteria (service+accelerator+intent+os) via a failed mixin
// constraint leaves the stated os/service/intent dimensions uncovered by
// the surviving base+monitoring-hpa set. That must now fail the build via
// verifyCriteriaCoverage (task 6) rather than silently degrading to
// base-only output. The excludedOverlays/constraintWarnings context on the
// returned error still pins the underlying mixin-exclusion behavior.
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

	_, err = store.BuildRecipeResultWithEvaluator(ctx, criteria, selectiveEvaluator)
	if err == nil {
		t.Fatal("expected coverage error: mixin exclusion uncovers the stated os/service/intent dimensions")
	}
	if !errors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("expected ErrCodeInvalidRequest, got %v", err)
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) {
		t.Fatalf("expected StructuredError, got %v", err)
	}

	// The mixin constraint (kernel >= 6.8) should have failed post-compose
	excludedOverlays, ok := se.Context["excludedOverlays"].([]ExcludedOverlay)
	if !ok || len(excludedOverlays) == 0 {
		t.Fatalf("expected non-empty excludedOverlays context, got %v", se.Context["excludedOverlays"])
	}
	excluded := make(map[string]ExcludedOverlayReason)
	for _, overlay := range excludedOverlays {
		excluded[overlay.Name] = overlay.Reason
	}
	if excluded["h100-eks-ubuntu-training"] != ExcludedOverlayReasonMixinConstraintFailed {
		t.Fatalf("expected mixin-constraint-failed reason, got %q", excluded["h100-eks-ubuntu-training"])
	}

	// Constraint warnings should include the failing mixin constraint
	warnings, ok := se.Context["constraintWarnings"].([]ConstraintWarning)
	if !ok {
		t.Fatalf("expected constraintWarnings context, got %v", se.Context["constraintWarnings"])
	}
	foundKernelWarning := false
	for _, w := range warnings {
		if w.Constraint == "OS.sysctl./proc/sys/kernel/osrelease" {
			foundKernelWarning = true
		}
	}
	if !foundKernelWarning {
		t.Error("expected constraint warning for OS.sysctl./proc/sys/kernel/osrelease from mixin")
	}
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
			Name:    "dcgm-exporter",
			Type:    ComponentTypeHelm,
			Source:  "https://example.com/charts",
			Chart:   "dcgm-exporter",
			Version: "1.0.0",
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
							Name:    "nvidia-dcgm-exporter",
							Type:    ComponentTypeHelm,
							Source:  "https://example.com/charts",
							Chart:   "nvidia-dcgm-exporter",
							Version: "1.0.0",
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

	// Query states accelerator=h100, and h100-shared-training is the only
	// overlay carrying it. Once the mixin constraint excludes that leaf,
	// nothing in the surviving set (base, monitoring) covers accelerator=h100
	// exactly, so verifyCriteriaCoverage (task 6) now fails the build instead
	// of silently degrading to an accelerator-agnostic result (issue #1542).
	// The excludedOverlays/constraintWarnings context on the error still pins
	// the underlying chain-isolation invariant: only the failed leaf and its
	// own ancestor are excluded/removed, never the independent "monitoring"
	// chain.
	_, err := store.BuildRecipeResultWithEvaluator(ctx, criteria, evaluator)
	if err == nil {
		t.Fatal("expected coverage error: mixin exclusion uncovers the stated accelerator dimension")
	}
	if !errors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("expected ErrCodeInvalidRequest, got %v", err)
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) {
		t.Fatalf("expected StructuredError, got %v", err)
	}

	excludedOverlays, ok := se.Context["excludedOverlays"].([]ExcludedOverlay)
	if !ok || len(excludedOverlays) == 0 {
		t.Fatalf("expected non-empty excludedOverlays context, got %v", se.Context["excludedOverlays"])
	}
	excluded := make(map[string]ExcludedOverlayReason)
	for _, overlay := range excludedOverlays {
		excluded[overlay.Name] = overlay.Reason
	}
	if _, present := excluded["h100-shared-training"]; !present {
		t.Fatalf("expected failed leaf in excludedOverlays, got %v", excludedOverlays)
	}
	if excluded["h100-shared-training"] != ExcludedOverlayReasonMixinConstraintFailed {
		t.Fatalf("failed leaf reason = %q, want %q",
			excluded["h100-shared-training"], ExcludedOverlayReasonMixinConstraintFailed)
	}
	if _, present := excluded[testOverlaySharedTrain]; present {
		t.Fatalf("ancestor should not appear in excludedOverlays, got %v", excludedOverlays)
	}
	if _, present := excluded["monitoring"]; present {
		t.Fatalf("independent passing leaf should not be excluded, got %v", excludedOverlays)
	}

	warnings, ok := se.Context["constraintWarnings"].([]ConstraintWarning)
	if !ok {
		t.Fatalf("expected constraintWarnings context, got %v", se.Context["constraintWarnings"])
	}
	foundWarning := false
	for _, warning := range warnings {
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
}

// TestMixinConstraintFailurePreservesSharedAncestorsForSurvivingLeaf pins
// issue #1542: the query states accelerator=h100, and leaf-a is the only
// overlay carrying it. Excluding leaf-a via a failed mixin constraint
// leaves nothing covering the stated accelerator dimension (leaf-b is
// accelerator=any), so verifyCriteriaCoverage (task 6) must fail the build
// rather than silently falling back to leaf-b's fully-composed but
// dimension-mismatched result. The error's excludedOverlays/
// constraintWarnings context still pins the original fallback-rebuild
// isolation invariant this test was named for.
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
			Name:    "shared-component",
			Type:    ComponentTypeHelm,
			Source:  "https://example.com/charts",
			Chart:   "shared-component",
			Version: "1.0.0",
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
						Name:    "surviving-component",
						Type:    ComponentTypeHelm,
						Source:  "https://example.com/charts",
						Chart:   "surviving-component",
						Version: "1.0.0",
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

	// Query states accelerator=h100, and leaf-a is the only overlay carrying
	// it (leaf-b is accelerator=any). Once the mixin constraint excludes
	// leaf-a, nothing in the surviving set covers accelerator=h100 exactly,
	// so verifyCriteriaCoverage (task 6) now fails the build instead of
	// silently degrading to leaf-b's accelerator-agnostic result (issue
	// #1542). The excludedOverlays/constraintWarnings context on the error
	// still pins the underlying fallback-rebuild invariant: only leaf-a is
	// excluded, never the shared ancestor or the surviving leaf-b chain.
	_, err := store.BuildRecipeResultWithEvaluator(ctx, criteria, evaluator)
	if err == nil {
		t.Fatal("expected coverage error: mixin exclusion uncovers the stated accelerator dimension")
	}
	if !errors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("expected ErrCodeInvalidRequest, got %v", err)
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) {
		t.Fatalf("expected StructuredError, got %v", err)
	}

	excludedOverlays, ok := se.Context["excludedOverlays"].([]ExcludedOverlay)
	if !ok || len(excludedOverlays) == 0 {
		t.Fatalf("expected non-empty excludedOverlays context, got %v", se.Context["excludedOverlays"])
	}
	excluded := make(map[string]ExcludedOverlayReason)
	for _, overlay := range excludedOverlays {
		excluded[overlay.Name] = overlay.Reason
	}
	if excluded["leaf-a"] != ExcludedOverlayReasonMixinConstraintFailed {
		t.Fatalf("expected leaf-a excluded with mixin-constraint-failed, got %v", excludedOverlays)
	}
	if _, present := excluded[testOverlaySharedTrain]; present {
		t.Fatalf("shared ancestor should not appear in excludedOverlays, got %v", excludedOverlays)
	}
	if _, present := excluded["leaf-b"]; present {
		t.Fatalf("surviving leaf-b should not appear in excludedOverlays, got %v", excludedOverlays)
	}

	warnings, ok := se.Context["constraintWarnings"].([]ConstraintWarning)
	if !ok {
		t.Fatalf("expected constraintWarnings context, got %v", se.Context["constraintWarnings"])
	}
	foundWarning := false
	for _, warning := range warnings {
		if warning.Constraint == "GPU.ready" && warning.Overlay == "leaf-a" {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Fatalf("expected constraint warning for GPU.ready on leaf-a, got %v", warnings)
	}
}

// TestMixinConstraintFailureDegradesForUnstatedDimension pins the SUCCESS
// path through the mixin-failure fallback rebuild after the task-6 coverage
// post-condition: when a mixin constraint excludes a candidate chain whose
// stated-dimension coverage is fully duplicated by a surviving overlay, the
// build must still succeed (graceful degradation), with the excluded
// candidate reported in metadata and the survivor applied. This is the
// mixin-path sibling of the per-overlay "clean mismatch on
// unstated-dimension overlay still degrades" subtest in
// TestBuildRecipeResultWithEvaluator_FailClosed.
func TestMixinConstraintFailureDegradesForUnstatedDimension(t *testing.T) {
	ctx := context.Background()

	baseMeta := &RecipeMetadata{}
	baseMeta.Metadata.Name = testRecipeBase

	// Survivor: fully covers every dimension the query states, no mixins.
	survivor := &RecipeMetadata{}
	survivor.Metadata.Name = testOverlayEKSTraning
	survivor.Spec.Criteria = &Criteria{
		Service: CriteriaServiceEKS,
		Intent:  CriteriaIntentTraining,
	}
	survivor.Spec.ComponentRefs = []ComponentRef{
		{
			Name:    "training-component",
			Type:    ComponentTypeHelm,
			Source:  "https://example.com/charts",
			Chart:   "training-component",
			Version: "1.0.0",
		},
	}

	// Candidate excluded via mixin constraint. Its criteria state the same
	// dimensions the survivor covers, so its exclusion uncovers nothing.
	monitoring := &RecipeMetadata{}
	monitoring.Metadata.Name = "monitoring"
	monitoring.Spec.Criteria = &Criteria{
		Service: CriteriaServiceEKS,
		Intent:  CriteriaIntentTraining,
	}
	monitoring.Spec.Mixins = []string{"monitoring-gate"}

	store := &MetadataStore{
		Base: baseMeta,
		Overlays: map[string]*RecipeMetadata{
			testOverlayEKSTraning: survivor,
			"monitoring":          monitoring,
		},
		Mixins: map[string]*RecipeMixin{
			"monitoring-gate": {
				Kind:       RecipeMixinKind,
				APIVersion: RecipeAPIVersion,
				Metadata: struct {
					Name string `json:"name" yaml:"name"`
				}{Name: "monitoring-gate"},
				Spec: struct {
					Constraints   []Constraint   `json:"constraints,omitempty" yaml:"constraints,omitempty"`
					ComponentRefs []ComponentRef `json:"componentRefs,omitempty" yaml:"componentRefs,omitempty"`
				}{
					Constraints: []Constraint{{Name: "Monitoring.enabled", Value: "true"}},
				},
			},
		},
	}

	criteria := &Criteria{
		Service: CriteriaServiceEKS,
		Intent:  CriteriaIntentTraining,
	}

	evaluator := func(c Constraint) ConstraintEvalResult {
		if c.Name == "Monitoring.enabled" {
			return ConstraintEvalResult{Passed: false, Actual: "false"}
		}
		return ConstraintEvalResult{Passed: true, Actual: "ok"}
	}

	result, err := store.BuildRecipeResultWithEvaluator(ctx, criteria, evaluator)
	if err != nil {
		t.Fatalf("expected graceful degradation (survivor covers all stated dimensions), got %v", err)
	}

	excluded := make(map[string]ExcludedOverlayReason)
	for _, overlay := range result.Metadata.ExcludedOverlays {
		excluded[overlay.Name] = overlay.Reason
	}
	if excluded["monitoring"] != ExcludedOverlayReasonMixinConstraintFailed {
		t.Fatalf("expected monitoring excluded with mixin-constraint-failed, got %v", result.Metadata.ExcludedOverlays)
	}
	if _, present := excluded[testOverlayEKSTraning]; present {
		t.Fatalf("survivor should not be excluded, got %v", result.Metadata.ExcludedOverlays)
	}

	applied := make(map[string]bool)
	for _, name := range result.Metadata.AppliedOverlays {
		applied[name] = true
	}
	if !applied[baseRecipeName] {
		t.Fatal("base should always remain applied")
	}
	if !applied[testOverlayEKSTraning] {
		t.Fatalf("surviving overlay should remain applied after fallback rebuild, got %v", result.Metadata.AppliedOverlays)
	}
	if applied["monitoring"] {
		t.Fatalf("mixin-excluded candidate should not be applied, got %v", result.Metadata.AppliedOverlays)
	}

	componentNames := make(map[string]bool)
	for _, ref := range result.ComponentRefs {
		componentNames[ref.Name] = true
	}
	if !componentNames["training-component"] {
		t.Fatalf("survivor component was lost after fallback rebuild: %v", result.ComponentRefs)
	}
}

// TestMixinConstraintFailClosedOnInternalEvaluatorError pins issue #1542
// design 5.2 for the mixin post-compose path (evaluateMixinConstraints):
// a non-NotFound evaluator error on a mixin-contributed constraint must fail
// the build immediately with its own preserved code, exactly like the
// per-overlay fail-closed behavior in evaluateOverlayConstraints
// (TestBuildRecipeResultWithEvaluator_FailClosed). NotFound remains the only
// graceful-degradation signal; every other code propagates as-is instead of
// silently excluding the candidate chain.
func TestMixinConstraintFailClosedOnInternalEvaluatorError(t *testing.T) {
	ctx := context.Background()

	baseMeta := &RecipeMetadata{}
	baseMeta.Metadata.Name = testRecipeBase

	overlay := &RecipeMetadata{}
	overlay.Metadata.Name = testOverlayEKSTraning
	overlay.Spec.Criteria = &Criteria{
		Service: CriteriaServiceEKS,
		Intent:  CriteriaIntentTraining,
	}
	// Per-overlay constraint the evaluator must pass cleanly, so the only
	// failure the test observes comes from the mixin-contributed constraint.
	overlay.Spec.Constraints = []Constraint{
		{Name: testK8sVersionConstant, Value: ">= 1.28"},
	}
	overlay.Spec.Mixins = []string{"os-gate"}

	store := &MetadataStore{
		Base: baseMeta,
		Overlays: map[string]*RecipeMetadata{
			testOverlayEKSTraning: overlay,
		},
		Mixins: map[string]*RecipeMixin{
			"os-gate": {
				Kind:       RecipeMixinKind,
				APIVersion: RecipeAPIVersion,
				Metadata: struct {
					Name string `json:"name" yaml:"name"`
				}{Name: "os-gate"},
				Spec: struct {
					Constraints   []Constraint   `json:"constraints,omitempty" yaml:"constraints,omitempty"`
					ComponentRefs []ComponentRef `json:"componentRefs,omitempty" yaml:"componentRefs,omitempty"`
				}{
					Constraints: []Constraint{{Name: "OS.kernel.version", Value: ">= 6.8"}},
				},
			},
		},
	}

	criteria := &Criteria{
		Service: CriteriaServiceEKS,
		Intent:  CriteriaIntentTraining,
	}

	evaluator := func(c Constraint) ConstraintEvalResult {
		if c.Name == "OS.kernel.version" {
			return ConstraintEvalResult{Passed: false, Error: aicrerrors.New(aicrerrors.ErrCodeInternal, "evaluation failed")}
		}
		// Any per-overlay constraint (K8s.server.version here) passes cleanly.
		return ConstraintEvalResult{Passed: true, Actual: "ok"}
	}

	_, err := store.BuildRecipeResultWithEvaluator(ctx, criteria, evaluator)
	if err == nil {
		t.Fatal("expected fail-closed error from mixin constraint evaluation")
	}
	if !errors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInternal, "")) {
		t.Fatalf("expected ErrCodeInternal preserved from mixin evaluator error, got %v", err)
	}
}

// TestMixinConstraintFailClosedOnUnstructuredEvaluatorError covers
// CodeRabbit's finding: a ConstraintEvaluatorFunc returning a plain
// (non-StructuredError) error on the mixin-constraint path must still be
// coded before it reaches the caller, not propagated bare.
func TestMixinConstraintFailClosedOnUnstructuredEvaluatorError(t *testing.T) {
	ctx := context.Background()

	baseMeta := &RecipeMetadata{}
	baseMeta.Metadata.Name = testRecipeBase

	overlay := &RecipeMetadata{}
	overlay.Metadata.Name = testOverlayEKSTraning
	overlay.Spec.Criteria = &Criteria{
		Service: CriteriaServiceEKS,
		Intent:  CriteriaIntentTraining,
	}
	overlay.Spec.Constraints = []Constraint{
		{Name: testK8sVersionConstant, Value: ">= 1.28"},
	}
	overlay.Spec.Mixins = []string{"os-gate"}

	store := &MetadataStore{
		Base: baseMeta,
		Overlays: map[string]*RecipeMetadata{
			testOverlayEKSTraning: overlay,
		},
		Mixins: map[string]*RecipeMixin{
			"os-gate": {
				Kind:       RecipeMixinKind,
				APIVersion: RecipeAPIVersion,
				Metadata: struct {
					Name string `json:"name" yaml:"name"`
				}{Name: "os-gate"},
				Spec: struct {
					Constraints   []Constraint   `json:"constraints,omitempty" yaml:"constraints,omitempty"`
					ComponentRefs []ComponentRef `json:"componentRefs,omitempty" yaml:"componentRefs,omitempty"`
				}{
					Constraints: []Constraint{{Name: "OS.kernel.version", Value: ">= 6.8"}},
				},
			},
		},
	}

	criteria := &Criteria{
		Service: CriteriaServiceEKS,
		Intent:  CriteriaIntentTraining,
	}

	evaluator := func(c Constraint) ConstraintEvalResult {
		if c.Name == "OS.kernel.version" {
			return ConstraintEvalResult{Passed: false, Error: errors.New("boom")}
		}
		return ConstraintEvalResult{Passed: true, Actual: "ok"}
	}

	_, err := store.BuildRecipeResultWithEvaluator(ctx, criteria, evaluator)
	if err == nil {
		t.Fatal("expected fail-closed error from mixin constraint evaluation")
	}
	if !errors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInternal, "")) {
		t.Fatalf("expected unstructured mixin evaluator error wrapped as ErrCodeInternal, got %v", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected cause %q preserved in error chain, got %v", "boom", err)
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
apiVersion: aicr.run/v1alpha2
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
apiVersion: aicr.run/v1alpha2
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
apiVersion: aicr.run/v1alpha2
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

// inMemoryDataProvider is a minimal DataProvider backed by an in-memory
// map[path]content. Used to construct distinct DataProvider identities in
// isolation tests without touching the filesystem or the embedded FS.
type inMemoryDataProvider struct {
	files map[string][]byte
	tag   string
}

func newInMemoryProvider(tag string, files map[string][]byte) *inMemoryDataProvider {
	return &inMemoryDataProvider{files: files, tag: tag}
}

func (p *inMemoryDataProvider) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	content, ok := p.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return content, nil
}

func (p *inMemoryDataProvider) WalkDir(ctx context.Context, _ string, fn fs.WalkDirFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for path := range p.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := fn(path, inMemoryDirEntry{name: path}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (p *inMemoryDataProvider) Source(path string) string {
	return p.tag + ":" + path
}

// inMemoryDirEntry is a minimal fs.DirEntry for in-memory files.
// All entries are files (IsDir returns false).
type inMemoryDirEntry struct{ name string }

func (e inMemoryDirEntry) Name() string               { return e.name }
func (e inMemoryDirEntry) IsDir() bool                { return false }
func (e inMemoryDirEntry) Type() fs.FileMode          { return 0 }
func (e inMemoryDirEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrNotExist }

// buildProviderWithOverlays returns an inMemoryDataProvider that contains a
// minimal base recipe plus one overlay whose metadata name is derived from the
// supplied filename (e.g., "alpha-only.yaml" → overlay metadata name
// "alpha-only"). This lets isolation tests verify that the metadata store
// cache keyed by DataProvider populates distinct entries.
func buildProviderWithOverlays(t *testing.T, overlayFileName string) DataProvider {
	t.Helper()
	overlayName := overlayFileName
	if len(overlayName) > len(".yaml") && overlayName[len(overlayName)-len(".yaml"):] == ".yaml" {
		overlayName = overlayName[:len(overlayName)-len(".yaml")]
	}

	baseYAML := []byte(`kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: base
spec:
  componentRefs: []
`)

	overlayYAML := fmt.Appendf(nil, `kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: %s
spec:
  criteria:
    service: eks
  componentRefs: []
`, overlayName)

	files := map[string][]byte{
		"overlays/base.yaml":                baseYAML,
		"overlays/" + overlayName + ".yaml": overlayYAML,
	}
	return newInMemoryProvider(overlayName, files)
}

// TestLoadMetadataStore_PerProviderIsolation verifies that distinct
// DataProviders populate distinct cache entries. Two Builders against
// different providers must never share metadata state.
func TestLoadMetadataStore_PerProviderIsolation(t *testing.T) {
	t.Cleanup(ResetMetadataStoreForTesting)

	dpA := buildProviderWithOverlays(t, "alpha-only.yaml")
	dpB := buildProviderWithOverlays(t, "beta-only.yaml")

	ctx := context.Background()
	storeA, err := LoadMetadataStoreFor(ctx, dpA)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor(A): %v", err)
	}
	storeB, err := LoadMetadataStoreFor(ctx, dpB)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor(B): %v", err)
	}

	if storeA == storeB {
		t.Fatal("expected distinct stores for distinct providers")
	}
	if _, ok := storeA.GetRecipeByName("alpha-only"); !ok {
		t.Errorf("store A missing alpha-only")
	}
	if _, ok := storeB.GetRecipeByName("beta-only"); !ok {
		t.Errorf("store B missing beta-only")
	}
	if _, ok := storeA.GetRecipeByName("beta-only"); ok {
		t.Errorf("store A leaked beta-only")
	}
}

// TestEvictCachedStore_Refetches verifies that EvictCachedStore drops the
// cached entry so the next LoadMetadataStoreFor call rebuilds a fresh store
// (distinct pointer) for the same provider.
func TestEvictCachedStore_Refetches(t *testing.T) {
	t.Cleanup(ResetMetadataStoreForTesting)

	dp := buildProviderWithOverlays(t, "evict-store.yaml")
	ctx := context.Background()
	first, err := LoadMetadataStoreFor(ctx, dp)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	EvictCachedStore(dp)
	second, err := LoadMetadataStoreFor(ctx, dp)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first == second {
		t.Errorf("expected fresh store after evict")
	}
}

// TestLoadMetadataStoreFor_TransientErrorIsNotCached locks in that a
// context cancellation during the first load does NOT permanently
// poison the cache. With sync.Once semantics, the first caller's
// canceled ctx would otherwise propagate to every later caller for
// the same DataProvider — a transient reconcile timeout turning into
// a permanently-broken Client. The fix in LoadMetadataStoreFor drops
// the cache entry when entry.err is a context cancellation, so a
// follow-up call with a healthy ctx loads from scratch.
//
// Acceptance criteria: the second call succeeds without a manual
// EvictCachedStore. The error from the first call carries the
// structured ErrCodeTimeout code (preserved by PropagateOrWrap in
// builder.go callers).
func TestLoadMetadataStoreFor_TransientErrorIsNotCached(t *testing.T) {
	t.Cleanup(ResetMetadataStoreForTesting)

	dp := buildProviderWithOverlays(t, "transient-cancel.yaml")

	// First call with a pre-canceled context. The WalkDir guard
	// inside buildMetadataStore returns aicrerrors.Wrap(ErrCodeTimeout,
	// ctx.Err()); LoadMetadataStoreFor then CompareAndDeletes the
	// poisoned entry so subsequent calls don't see the canceled state.
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, firstErr := LoadMetadataStoreFor(canceledCtx, dp)
	if firstErr == nil {
		t.Fatal("first call: expected error from canceled context, got nil")
	}
	// Pin the structured code so a regression to a bare context.Canceled
	// or a wrong fallback (e.g., ErrCodeInternal) is caught. The
	// transient-eviction logic in LoadMetadataStoreFor keys on
	// stderrors.Is(err, context.Canceled), which still works for any
	// wrap chain that keeps the cancellation, but downstream callers
	// (builder.BuildFromCriteria) depend on the ErrCodeTimeout shape
	// for their own retry/timeout signaling.
	var firstSE *aicrerrors.StructuredError
	if !errors.As(firstErr, &firstSE) {
		t.Fatalf("first call: expected structured error, got %T: %v", firstErr, firstErr)
	}
	if firstSE.Code != aicrerrors.ErrCodeTimeout {
		t.Fatalf("first call: expected ErrCodeTimeout, got %s", firstSE.Code)
	}

	// Second call with a healthy context must succeed — if the
	// poisoned entry hadn't been evicted, sync.Once would lock all
	// subsequent calls into the same cancellation error.
	store, err := LoadMetadataStoreFor(context.Background(), dp)
	if err != nil {
		t.Fatalf("second call (healthy ctx) after transient cancel: %v", err)
	}
	if store == nil {
		t.Fatal("second call: expected non-nil store after retry")
	}
}

// TestEvictCachedStore_NilIsNoOp verifies that EvictCachedStore(nil) is a
// no-op: it must not panic and must not clobber any existing cache entries
// for other providers.
func TestEvictCachedStore_NilIsNoOp(t *testing.T) {
	t.Cleanup(ResetMetadataStoreForTesting)

	dp := buildProviderWithOverlays(t, "noop-evict.yaml")
	ctx := context.Background()
	before, err := LoadMetadataStoreFor(ctx, dp)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Evicting nil must not panic, must not clobber the seeded entry.
	EvictCachedStore(nil)

	after, err := LoadMetadataStoreFor(ctx, dp)
	if err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	if before != after {
		t.Errorf("EvictCachedStore(nil) clobbered an unrelated entry")
	}
}

// TestLoadMetadataStoreFor_NilProviderFallsBack verifies that passing a nil
// DataProvider routes through GetDataProvider() instead of panicking, and
// returns a non-nil store via the global fallback path.
func TestLoadMetadataStoreFor_NilProviderFallsBack(t *testing.T) {
	// No identity assertion on the returned store — the global may have been
	// populated by other tests. Only verify nil dp does not panic and returns
	// a non-nil store via the global fallback.
	store, err := LoadMetadataStoreFor(context.Background(), nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor(nil): %v", err)
	}
	if store == nil {
		t.Error("expected non-nil store via global fallback")
	}
}

// TestLoadMetadataStore_ConcurrentSameProviderIsCached verifies the
// sync.Once-per-entry guarantee: concurrent LoadMetadataStoreFor calls for
// the same provider all receive the same singleton pointer.
func TestLoadMetadataStore_ConcurrentSameProviderIsCached(t *testing.T) {
	t.Cleanup(ResetMetadataStoreForTesting)

	dp := buildProviderWithOverlays(t, "concurrent.yaml")
	ctx := context.Background()

	g, gctx := errgroup.WithContext(ctx)
	results := make([]*MetadataStore, 8)
	for i := range results {
		g.Go(func() error {
			s, err := LoadMetadataStoreFor(gctx, dp)
			results[i] = s
			return err
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("concurrent load: %v", err)
	}
	for i := 1; i < len(results); i++ {
		if results[i] != results[0] {
			t.Errorf("result[%d] is not the cached singleton (got %p, want %p)", i, results[i], results[0])
		}
	}
}

// TestGB200EKSTrainingFloor verifies that the fully resolved GB200 EKS training
// recipes require Kubernetes 1.34 for the GA resource.k8s.io/v1 API used by the
// NVLS performance validation path.
func TestGB200EKSTrainingFloor(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("failed to load metadata store: %v", err)
	}

	tests := []struct {
		name     string
		criteria *Criteria
	}{
		{
			name: "gb200 eks training",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorGB200,
				Intent:      CriteriaIntentTraining,
			},
		},
		{
			name: "gb200 eks ubuntu training",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorGB200,
				Intent:      CriteriaIntentTraining,
				OS:          CriteriaOSUbuntu,
			},
		},
		{
			name: "gb200 eks ubuntu kubeflow training",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorGB200,
				Intent:      CriteriaIntentTraining,
				OS:          CriteriaOSUbuntu,
				Platform:    CriteriaPlatformKubeflow,
			},
		},
		{
			name: "gb200 eks ubuntu slurm training",
			criteria: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorGB200,
				Intent:      CriteriaIntentTraining,
				OS:          CriteriaOSUbuntu,
				Platform:    CriteriaPlatformSlurm,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := store.BuildRecipeResult(ctx, tt.criteria)
			if err != nil {
				t.Fatalf("BuildRecipeResult failed: %v", err)
			}
			var k8sFloor string
			for _, constraint := range result.Constraints {
				if constraint.Name == testK8sVersionConstant {
					k8sFloor = constraint.Value
					break
				}
			}
			if k8sFloor != ">= 1.34" {
				t.Errorf("K8s.server.version floor = %q, want %q", k8sFloor, ">= 1.34")
			}
		})
	}
}

// TestGB200OKEFloorNotClobbered is a regression test for the GB200 OKE K8s floor
// clobber: when oke-ol-training (>= 1.30, accelerator-generic) co-matched
// gb200-oke-training (>= 1.34) as a sibling leaf, the last-writer-wins constraint
// merge silently dropped the GB200 DRA floor. Verifies the resolved recipe carries
// the GB200-specific floor (>= 1.34) regardless of OS supplied.
func TestGB200OKEFloorNotClobbered(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("failed to load metadata store: %v", err)
	}

	tests := []struct {
		name         string
		criteria     *Criteria
		wantK8sFloor string
	}{
		{
			name: "gb200 oke training with os ol preserves >= 1.34 floor",
			criteria: &Criteria{
				Service:     CriteriaServiceOKE,
				Accelerator: CriteriaAcceleratorGB200,
				Intent:      CriteriaIntentTraining,
				OS:          CriteriaOSOracleLinux,
			},
			wantK8sFloor: ">= 1.34",
		},
		{
			name: "gb200 oke training with os ubuntu preserves >= 1.34 floor",
			criteria: &Criteria{
				Service:     CriteriaServiceOKE,
				Accelerator: CriteriaAcceleratorGB200,
				Intent:      CriteriaIntentTraining,
				OS:          CriteriaOSUbuntu,
			},
			wantK8sFloor: ">= 1.34",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := store.BuildRecipeResult(ctx, tt.criteria)
			if err != nil {
				t.Fatalf("BuildRecipeResult failed: %v", err)
			}
			var k8sFloor string
			for _, c := range result.Constraints {
				if c.Name == testK8sVersionConstant {
					k8sFloor = c.Value
					break
				}
			}
			if k8sFloor != tt.wantK8sFloor {
				t.Errorf("K8s.server.version floor = %q, want %q (possible floor clobber regression)",
					k8sFloor, tt.wantK8sFloor)
			}
		})
	}
}

// TestH100AKSUbuntuTrainingSlurmFloorNotClobbered is a regression test for the
// H100 AKS Slurm K8s floor. The leaf sets >= 1.34 (DRA GA in Kubernetes 1.34,
// per the aks.yaml rationale), but resolves under last-writer-wins merge with
// lower-valued ancestors (aks-training >= 1.30, h100-aks-ubuntu-training
// >= 1.32.4). A future constraint edit on an ancestor could silently regress
// the leaf floor — the same shape TestGB200OKEFloorNotClobbered guards against.
// This locks the resolved floor and gives the AKS-family follow-up a place to
// add sibling leaves as they are bumped.
func TestH100AKSUbuntuTrainingSlurmFloorNotClobbered(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("failed to load metadata store: %v", err)
	}

	tests := []struct {
		name         string
		criteria     *Criteria
		wantK8sFloor string
	}{
		{
			name: "h100 aks ubuntu slurm training preserves >= 1.34 floor",
			criteria: &Criteria{
				Service:     CriteriaServiceAKS,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentTraining,
				OS:          CriteriaOSUbuntu,
				Platform:    CriteriaPlatformSlurm,
			},
			wantK8sFloor: ">= 1.34",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := store.BuildRecipeResult(ctx, tt.criteria)
			if err != nil {
				t.Fatalf("BuildRecipeResult failed: %v", err)
			}
			var k8sFloor string
			for _, c := range result.Constraints {
				if c.Name == testK8sVersionConstant {
					k8sFloor = c.Value
					break
				}
			}
			if k8sFloor != tt.wantK8sFloor {
				t.Errorf("K8s.server.version floor = %q, want %q (possible floor clobber regression)",
					k8sFloor, tt.wantK8sFloor)
			}
		})
	}
}

func TestBuildRecipeResult_OSRequired(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("failed to load metadata store: %v", err)
	}
	tests := []struct {
		name        string
		criteria    *Criteria
		wantErrCode aicrerrors.ErrorCode
		wantInMsg   string
	}{
		{
			name:        "gke without os returns error",
			criteria:    &Criteria{Service: CriteriaServiceGKE, Accelerator: CriteriaAcceleratorH100, Intent: CriteriaIntentTraining},
			wantErrCode: aicrerrors.ErrCodeInvalidRequest,
			wantInMsg:   "cos",
		},
		{
			name:     "gke with os cos succeeds",
			criteria: &Criteria{Service: CriteriaServiceGKE, Accelerator: CriteriaAcceleratorH100, Intent: CriteriaIntentTraining, OS: CriteriaOSCOS},
		},
		{
			name:        "oke l40s without os returns error (only os-specific overlays exist for l40s-oke)",
			criteria:    &Criteria{Service: CriteriaServiceOKE, Accelerator: CriteriaAcceleratorL40S, Intent: CriteriaIntentTraining},
			wantErrCode: aicrerrors.ErrCodeInvalidRequest,
			wantInMsg:   "ol",
		},
		{
			name:     "oke l40s with os ol succeeds",
			criteria: &Criteria{Service: CriteriaServiceOKE, Accelerator: CriteriaAcceleratorL40S, Intent: CriteriaIntentTraining, OS: CriteriaOSOracleLinux},
		},
		{
			name:        "oke gb200 without os returns error (no os-agnostic oke overlay)",
			criteria:    &Criteria{Service: CriteriaServiceOKE, Accelerator: CriteriaAcceleratorGB200, Intent: CriteriaIntentTraining},
			wantErrCode: aicrerrors.ErrCodeInvalidRequest,
			wantInMsg:   "ol",
		},
		{
			name:     "eks without os succeeds (has os-agnostic overlays)",
			criteria: &Criteria{Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100, Intent: CriteriaIntentTraining},
		},
		{
			// The OS guard itself does not fire for an unrecognized service (no
			// OS-gated overlay exists to name it in the guard message), but the
			// criteria coverage post-condition (issue #1542) now catches it: the
			// stated service dimension is honored by no overlay at all.
			name:        "unknown service without os returns coverage error",
			criteria:    &Criteria{Service: CriteriaServiceType("xyz"), Accelerator: CriteriaAcceleratorH100},
			wantErrCode: aicrerrors.ErrCodeInvalidRequest,
			wantInMsg:   "xyz",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := store.BuildRecipeResult(ctx, tt.criteria)
			if tt.wantErrCode != "" {
				if !errors.Is(err, aicrerrors.New(tt.wantErrCode, "")) {
					t.Errorf("got err %v, want code %s", err, tt.wantErrCode)
				}
				if tt.wantInMsg != "" && (err == nil || !strings.Contains(err.Error(), tt.wantInMsg)) {
					t.Errorf("error %q does not contain %q", err, tt.wantInMsg)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if result == nil {
					t.Error("expected non-nil result")
				}
			}
		})
	}
}

func TestBuildRecipeResultWithEvaluator_OSRequired(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("failed to load metadata store: %v", err)
	}
	passAll := func(_ Constraint) ConstraintEvalResult {
		return ConstraintEvalResult{Passed: true, Actual: "test"}
	}
	tests := []struct {
		name        string
		criteria    *Criteria
		wantErrCode aicrerrors.ErrorCode
		wantInMsg   string
	}{
		{
			name:        "gke without os returns error",
			criteria:    &Criteria{Service: CriteriaServiceGKE, Accelerator: CriteriaAcceleratorH100, Intent: CriteriaIntentTraining},
			wantErrCode: aicrerrors.ErrCodeInvalidRequest,
			wantInMsg:   "cos",
		},
		{
			name:     "gke with os cos succeeds",
			criteria: &Criteria{Service: CriteriaServiceGKE, Accelerator: CriteriaAcceleratorH100, Intent: CriteriaIntentTraining, OS: CriteriaOSCOS},
		},
		{
			name:        "oke l40s without os returns error",
			criteria:    &Criteria{Service: CriteriaServiceOKE, Accelerator: CriteriaAcceleratorL40S, Intent: CriteriaIntentTraining},
			wantErrCode: aicrerrors.ErrCodeInvalidRequest,
			wantInMsg:   "ol",
		},
		{
			name:        "oke gb200 without os returns error (no os-agnostic oke overlay)",
			criteria:    &Criteria{Service: CriteriaServiceOKE, Accelerator: CriteriaAcceleratorGB200, Intent: CriteriaIntentTraining},
			wantErrCode: aicrerrors.ErrCodeInvalidRequest,
			wantInMsg:   "ol",
		},
		{
			name:     "eks without os succeeds (has os-agnostic overlays)",
			criteria: &Criteria{Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100, Intent: CriteriaIntentTraining},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := store.BuildRecipeResultWithEvaluator(ctx, tt.criteria, passAll)
			if tt.wantErrCode != "" {
				if !errors.Is(err, aicrerrors.New(tt.wantErrCode, "")) {
					t.Errorf("got err %v, want code %s", err, tt.wantErrCode)
				}
				if tt.wantInMsg != "" && (err == nil || !strings.Contains(err.Error(), tt.wantInMsg)) {
					t.Errorf("error %q does not contain %q", err, tt.wantInMsg)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if result == nil {
					t.Error("expected non-nil result")
				}
			}
		})
	}
}

func TestBuildRecipeResult_CriteriaCoverage(t *testing.T) {
	ctx := context.Background()

	// Catalog mirrors the live repro shape: OS-agnostic service tier,
	// platform content only on an OS-gated leaf, plus one OS-agnostic
	// platform leaf on another service (the Kind shape).
	base := &RecipeMetadata{}
	base.Metadata.Name = testRecipeBase
	mk := func(name string, c *Criteria, baseRef string) *RecipeMetadata {
		o := &RecipeMetadata{}
		o.Metadata.Name = name
		o.Spec.Criteria = c
		o.Spec.Base = baseRef
		return o
	}
	eks := mk("eks", &Criteria{Service: CriteriaServiceEKS}, "")
	h100Any := mk("h100-any", &Criteria{Accelerator: CriteriaAcceleratorH100}, "")
	eksTr := mk("eks-training", &Criteria{Service: CriteriaServiceEKS,
		Intent: CriteriaIntentTraining}, "eks")
	// Joint OS-agnostic mid-tier mirrors the real embedded data shape: the
	// requireOSIfNeeded guard finds its joint service+accelerator match here
	// and passes, so verifyCriteriaCoverage is what rejects an uncovered
	// platform in the repro subtest below.
	h100EksTr := mk("h100-eks-training", &Criteria{
		Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100,
		Intent: CriteriaIntentTraining}, "eks-training")
	eksUbuKf := mk("h100-eks-ubuntu-training-kubeflow", &Criteria{
		Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100,
		Intent: CriteriaIntentTraining, OS: CriteriaOSUbuntu,
		Platform: CriteriaPlatformKubeflow}, "eks-training")
	kindKf := mk("h100-kind-training-kubeflow", &Criteria{
		Service: CriteriaServiceKind, Accelerator: CriteriaAcceleratorH100,
		Intent: CriteriaIntentTraining, Platform: CriteriaPlatformKubeflow}, "")
	store := &MetadataStore{Base: base, Overlays: map[string]*RecipeMetadata{
		"eks": eks, "h100-any": h100Any, "eks-training": eksTr,
		"h100-eks-training":                 h100EksTr,
		"h100-eks-ubuntu-training-kubeflow": eksUbuKf,
		"h100-kind-training-kubeflow":       kindKf,
	}}

	t.Run("issue 1542 repro: stated platform uncovered errors", func(t *testing.T) {
		_, err := store.BuildRecipeResult(ctx, &Criteria{
			Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100,
			Intent: CriteriaIntentTraining, Platform: CriteriaPlatformKubeflow})
		if !errors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
			t.Fatalf("expected ErrCodeInvalidRequest, got %v", err)
		}
		// A guard-origin error can never satisfy this: only the coverage
		// post-condition names the uncovered platform dimension.
		if !strings.Contains(err.Error(), "platform 'kubeflow'") {
			t.Fatalf("expected coverage error naming platform 'kubeflow', got: %v", err)
		}
	})

	t.Run("generic tier still succeeds", func(t *testing.T) {
		result, err := store.BuildRecipeResult(ctx, &Criteria{
			Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100,
			Intent: CriteriaIntentTraining})
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if result == nil {
			t.Fatal("nil result")
		}
	})

	t.Run("OS-agnostic platform leaf succeeds without OS (Kind shape)", func(t *testing.T) {
		_, err := store.BuildRecipeResult(ctx, &Criteria{
			Service: CriteriaServiceKind, Accelerator: CriteriaAcceleratorH100,
			Intent: CriteriaIntentTraining, Platform: CriteriaPlatformKubeflow})
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
	})

	t.Run("fully stated query resolves through OS-gated leaf", func(t *testing.T) {
		_, err := store.BuildRecipeResult(ctx, &Criteria{
			Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100,
			Intent: CriteriaIntentTraining, OS: CriteriaOSUbuntu,
			Platform: CriteriaPlatformKubeflow})
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
	})
}

// TestBuildRecipeResult_GuardAndCoverageComposition pins design 4.1: the
// joint service+accelerator OS guard fires where per-dimension coverage
// alone would pass (service and accelerator covered by SEPARATE overlays).
func TestBuildRecipeResult_GuardAndCoverageComposition(t *testing.T) {
	ctx := context.Background()
	base := &RecipeMetadata{}
	base.Metadata.Name = testRecipeBase
	a := &RecipeMetadata{}
	a.Metadata.Name = "svc-foo"
	a.Spec.Criteria = &Criteria{Service: CriteriaServiceEKS}
	b := &RecipeMetadata{}
	b.Metadata.Name = "accel-gpu"
	b.Spec.Criteria = &Criteria{Accelerator: CriteriaAcceleratorH100}
	c := &RecipeMetadata{}
	c.Metadata.Name = "svc-accel-ubuntu"
	c.Spec.Criteria = &Criteria{Service: CriteriaServiceEKS,
		Accelerator: CriteriaAcceleratorH100, OS: CriteriaOSUbuntu}
	store := &MetadataStore{Base: base, Overlays: map[string]*RecipeMetadata{
		"svc-foo": a, "accel-gpu": b, "svc-accel-ubuntu": c,
	}}

	_, err := store.BuildRecipeResult(ctx, &Criteria{
		Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100})
	if !errors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("expected guard ErrCodeInvalidRequest, got %v", err)
	}
	if !strings.Contains(err.Error(), "specify an OS") {
		t.Fatalf("expected the OS-guard message (guard runs before coverage), got: %v", err)
	}
}

// TestBuildRecipeResultWithEvaluator_FailClosed pins design 5.2: any
// constraint-evaluation error whose code is NOT ErrCodeNotFound must fail
// the build immediately with its own preserved code. ErrCodeNotFound
// ("value not found in snapshot") remains the evaluator's designed
// graceful-degradation signal (exclusion), distinct from every other code
// which indicates a malformed constraint or internal evaluator failure.
func TestBuildRecipeResultWithEvaluator_FailClosed(t *testing.T) {
	ctx := context.Background()
	base := &RecipeMetadata{}
	base.Metadata.Name = testRecipeBase
	o := &RecipeMetadata{}
	o.Metadata.Name = "eks"
	o.Spec.Criteria = &Criteria{Service: CriteriaServiceEKS}
	o.Spec.Constraints = []Constraint{{Name: "K8s.server.version", Value: ">= 1.28"}}
	store := &MetadataStore{Base: base, Overlays: map[string]*RecipeMetadata{"eks": o}}
	criteria := &Criteria{Service: CriteriaServiceEKS}

	tests := []struct {
		name     string
		evalErr  error
		wantCode aicrerrors.ErrorCode
	}{
		{
			name:     "InvalidRequest (malformed constraint) fails closed with own code",
			evalErr:  aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "invalid constraint expression"),
			wantCode: aicrerrors.ErrCodeInvalidRequest,
		},
		{
			name:     "Internal fails closed with own code",
			evalErr:  aicrerrors.New(aicrerrors.ErrCodeInternal, "evaluation failed"),
			wantCode: aicrerrors.ErrCodeInternal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			evaluator := func(_ Constraint) ConstraintEvalResult {
				return ConstraintEvalResult{Passed: false, Error: tt.evalErr}
			}
			_, err := store.BuildRecipeResultWithEvaluator(ctx, criteria, evaluator)
			if err == nil {
				t.Fatal("expected error")
			}
			if !errors.Is(err, aicrerrors.New(tt.wantCode, "")) {
				t.Fatalf("expected code %s, got %v", tt.wantCode, err)
			}
		})
	}

	// NotFound is still a graceful *exclusion* at the constraint-evaluation
	// layer (evaluateOverlayConstraints does not fail closed for it), but
	// the store here has exactly one overlay, and it is the only coverage
	// for the stated service=eks dimension. Now that
	// BuildRecipeResultWithEvaluator wires the evaluator path into
	// verifyCriteriaCoverage (task 6), excluding that overlay leaves
	// service=eks uncovered, so the build must fail with ErrCodeInvalidRequest
	// instead of silently shipping a base-only result.
	t.Run("NotFound exclusion strips only coverage: build fails", func(t *testing.T) {
		evaluator := func(_ Constraint) ConstraintEvalResult {
			return ConstraintEvalResult{Passed: false, Error: aicrerrors.New(aicrerrors.ErrCodeNotFound, "value not found in snapshot")}
		}
		_, err := store.BuildRecipeResultWithEvaluator(ctx, criteria, evaluator)
		if err == nil {
			t.Fatal("expected coverage error: stated service dimension's only coverage was constraint-excluded")
		}
		if !errors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
			t.Fatalf("expected ErrCodeInvalidRequest, got %v", err)
		}
		var se *aicrerrors.StructuredError
		if !errors.As(err, &se) {
			t.Fatalf("expected StructuredError, got %v", err)
		}
		if se.Context["excludedOverlays"] == nil {
			t.Error("coverage error should carry excludedOverlays context")
		}
	})

	t.Run("clean mismatch on unstated-dimension overlay still degrades", func(t *testing.T) {
		// Query states nothing the excluded overlay uniquely covers beyond
		// service... use an overlay the query did NOT state a dimension for:
		monitoring := &RecipeMetadata{}
		monitoring.Metadata.Name = "monitoring"
		monitoring.Spec.Criteria = &Criteria{} // matches everything
		monitoring.Spec.Constraints = []Constraint{{Name: "K8s.server.version", Value: ">= 99"}}
		s2 := &MetadataStore{Base: base, Overlays: map[string]*RecipeMetadata{
			"eks": o, "monitoring": monitoring}}
		evaluator := func(constraint Constraint) ConstraintEvalResult {
			if constraint.Value == ">= 99" {
				return ConstraintEvalResult{Passed: false, Actual: "1.33"}
			}
			return ConstraintEvalResult{Passed: true, Actual: "1.33"}
		}
		result, err := s2.BuildRecipeResultWithEvaluator(ctx, criteria, evaluator)
		if err != nil {
			t.Fatalf("expected graceful degradation, got %v", err)
		}
		if len(result.Metadata.ExcludedOverlays) != 1 {
			t.Fatalf("expected 1 excluded overlay, got %+v", result.Metadata.ExcludedOverlays)
		}
	})

	// Mixed-cause (design §8): an overlay with two constraints where the
	// evaluator returns NotFound for the first and Internal for the second.
	// evaluateOverlayConstraints processes constraints in declaration order
	// and returns immediately on the first non-NotFound error, so ordering
	// the NotFound constraint first is what makes this test meaningful — it
	// proves the fail-closed Internal cause wins over the graceful NotFound
	// exclusion rather than being masked by it.
	t.Run("mixed-cause: NotFound then Internal fails closed with Internal", func(t *testing.T) {
		mixed := &RecipeMetadata{}
		mixed.Metadata.Name = "eks-mixed"
		mixed.Spec.Criteria = &Criteria{Service: CriteriaServiceEKS}
		mixed.Spec.Constraints = []Constraint{
			{Name: "GPU.driver.version", Value: ">= 570"},    // evaluated first: NotFound
			{Name: testK8sVersionConstant, Value: ">= 1.28"}, // evaluated second: Internal
		}
		s3 := &MetadataStore{Base: base, Overlays: map[string]*RecipeMetadata{"eks-mixed": mixed}}
		evaluator := func(c Constraint) ConstraintEvalResult {
			switch c.Name {
			case "GPU.driver.version":
				return ConstraintEvalResult{Passed: false, Error: aicrerrors.New(aicrerrors.ErrCodeNotFound, "value not found in snapshot")}
			case testK8sVersionConstant:
				return ConstraintEvalResult{Passed: false, Error: aicrerrors.New(aicrerrors.ErrCodeInternal, "evaluation failed")}
			}
			return ConstraintEvalResult{Passed: true}
		}
		_, err := s3.BuildRecipeResultWithEvaluator(ctx, criteria, evaluator)
		if err == nil {
			t.Fatal("expected fail-closed error: Internal cause must win over NotFound graceful exclusion")
		}
		if !errors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInternal, "")) {
			t.Fatalf("expected ErrCodeInternal (fail-closed wins over NotFound), got %v", err)
		}
	})
}

// TestBuildRecipeResultWithEvaluator_CoverageAfterExclusion pins issue #1542:
// a stated dimension whose only coverage is stripped by constraint
// evaluation must surface as a coverage error, not a silent partial build.
func TestBuildRecipeResultWithEvaluator_CoverageAfterExclusion(t *testing.T) {
	ctx := context.Background()
	base := &RecipeMetadata{}
	base.Metadata.Name = testRecipeBase
	mk := func(name string, c *Criteria, constraints []Constraint) *RecipeMetadata {
		o := &RecipeMetadata{}
		o.Metadata.Name = name
		o.Spec.Criteria = c
		o.Spec.Constraints = constraints
		return o
	}
	eks := mk("eks", &Criteria{Service: CriteriaServiceEKS}, nil)
	// The ONLY platform coverage, gated by a constraint that will cleanly fail.
	kf := mk("eks-ubuntu-kubeflow", &Criteria{Service: CriteriaServiceEKS,
		OS: CriteriaOSUbuntu, Platform: CriteriaPlatformKubeflow},
		[]Constraint{{Name: "OS.kernel.version", Value: ">= 6.8"}})
	store := &MetadataStore{Base: base, Overlays: map[string]*RecipeMetadata{
		"eks": eks, "eks-ubuntu-kubeflow": kf}}

	evaluator := func(constraint Constraint) ConstraintEvalResult {
		if constraint.Name == "OS.kernel.version" {
			return ConstraintEvalResult{Passed: false, Actual: "6.5"}
		}
		return ConstraintEvalResult{Passed: true}
	}

	_, err := store.BuildRecipeResultWithEvaluator(ctx, &Criteria{
		Service: CriteriaServiceEKS, OS: CriteriaOSUbuntu,
		Platform: CriteriaPlatformKubeflow}, evaluator)
	if err == nil {
		t.Fatal("expected coverage error: stated platform's only coverage was constraint-excluded")
	}
	if !errors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
		t.Fatalf("expected ErrCodeInvalidRequest, got %v", err)
	}
	var se *aicrerrors.StructuredError
	if !errors.As(err, &se) {
		t.Fatalf("expected StructuredError, got %v", err)
	}
	if se.Context["excludedOverlays"] == nil {
		t.Error("coverage error should carry excludedOverlays context")
	}
	if se.Context["constraintWarnings"] == nil {
		t.Error("coverage error should carry constraintWarnings context")
	}
}
