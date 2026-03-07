// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package validator

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
	"github.com/NVIDIA/aicr/recipes"
	corev1 "k8s.io/api/core/v1"
)

func TestNewDefaults(t *testing.T) {
	v := New()

	if v.Namespace != "aicr-validation" {
		t.Errorf("Namespace = %q, want %q", v.Namespace, "aicr-validation")
	}
	if v.RunID == "" {
		t.Error("RunID should be generated")
	}
	if !v.Cleanup {
		t.Error("Cleanup should default to true")
	}
	if v.NoCluster {
		t.Error("NoCluster should default to false")
	}
	if len(v.Tolerations) != 1 || v.Tolerations[0].Operator != corev1.TolerationOpExists {
		t.Errorf("Tolerations should default to tolerate-all, got %v", v.Tolerations)
	}
}

func TestNewWithOptions(t *testing.T) {
	v := New(
		WithVersion("1.0.0"),
		WithNamespace("custom-ns"),
		WithRunID("test-run"),
		WithCleanup(false),
		WithNoCluster(true),
		WithImagePullSecrets([]string{"secret1"}),
		WithTolerations(nil),
	)

	if v.Version != "1.0.0" {
		t.Errorf("Version = %q, want %q", v.Version, "1.0.0")
	}
	if v.Namespace != "custom-ns" {
		t.Errorf("Namespace = %q, want %q", v.Namespace, "custom-ns")
	}
	if v.RunID != "test-run" {
		t.Errorf("RunID = %q, want %q", v.RunID, "test-run")
	}
	if v.Cleanup {
		t.Error("Cleanup should be false")
	}
	if !v.NoCluster {
		t.Error("NoCluster should be true")
	}
	if len(v.ImagePullSecrets) != 1 || v.ImagePullSecrets[0] != "secret1" {
		t.Errorf("ImagePullSecrets = %v", v.ImagePullSecrets)
	}
}

func TestGenerateRunID(t *testing.T) {
	id1 := generateRunID()
	id2 := generateRunID()

	if id1 == "" {
		t.Error("RunID should not be empty")
	}
	if id1 == id2 {
		t.Error("RunIDs should be unique")
	}
	if len(id1) < 20 {
		t.Errorf("RunID too short: %q", id1)
	}
}

func loadEmbeddedCatalog(t *testing.T) *catalog.ValidatorCatalog {
	t.Helper()
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatalf("failed to load catalog: %v", err)
	}
	return cat
}

func TestPhasesSkipped(t *testing.T) {
	v := New(WithVersion("1.0.0"))
	cat := loadEmbeddedCatalog(t)

	results := v.phasesSkipped(cat, PhaseOrder, "test reason")
	if len(results) != len(PhaseOrder) {
		t.Fatalf("expected %d results, got %d", len(PhaseOrder), len(results))
	}

	for i, pr := range results {
		if pr.Phase != PhaseOrder[i] {
			t.Errorf("results[%d].Phase = %q, want %q", i, pr.Phase, PhaseOrder[i])
		}
		if pr.Status != ctrf.StatusSkipped {
			t.Errorf("results[%d].Status = %q, want %q", i, pr.Status, ctrf.StatusSkipped)
		}
		if pr.Report == nil {
			t.Errorf("results[%d].Report should not be nil", i)
		}
	}
}

func TestPhasesSkippedSubset(t *testing.T) {
	v := New(WithVersion("1.0.0"))
	cat := loadEmbeddedCatalog(t)

	subset := []Phase{PhaseDeployment}
	results := v.phasesSkipped(cat, subset, "test reason")
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Phase != PhaseDeployment {
		t.Errorf("Phase = %q, want %q", results[0].Phase, PhaseDeployment)
	}
}

func TestPhaseSkipped(t *testing.T) {
	v := New(WithVersion("1.0.0"))
	cat := loadEmbeddedCatalog(t)

	pr := v.phaseSkipped(cat, PhaseDeployment, "no cluster")
	if pr.Phase != PhaseDeployment {
		t.Errorf("Phase = %q, want %q", pr.Phase, PhaseDeployment)
	}
	if pr.Status != ctrf.StatusSkipped {
		t.Errorf("Status = %q, want %q", pr.Status, ctrf.StatusSkipped)
	}
	if pr.Report == nil {
		t.Fatal("Report should not be nil")
	}
	if pr.Report.ReportFormat != ctrf.ReportFormatCTRF {
		t.Errorf("ReportFormat = %q, want %q", pr.Report.ReportFormat, ctrf.ReportFormatCTRF)
	}
}

func TestValidatePhasesNoClusterAll(t *testing.T) {
	v := New(
		WithVersion("1.0.0"),
		WithNoCluster(true),
	)

	results, err := v.ValidatePhases(context.Background(), nil, nil, nil)
	if err != nil {
		t.Fatalf("ValidatePhases() failed: %v", err)
	}

	if len(results) != len(PhaseOrder) {
		t.Fatalf("expected %d results, got %d", len(PhaseOrder), len(results))
	}

	for _, pr := range results {
		if pr.Status != ctrf.StatusSkipped {
			t.Errorf("phase %q status = %q, want %q", pr.Phase, pr.Status, ctrf.StatusSkipped)
		}
	}
}

func TestValidatePhasesNoClusterSubset(t *testing.T) {
	v := New(
		WithVersion("1.0.0"),
		WithNoCluster(true),
	)

	results, err := v.ValidatePhases(context.Background(), []Phase{PhaseDeployment}, nil, nil)
	if err != nil {
		t.Fatalf("ValidatePhases() failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Phase != PhaseDeployment {
		t.Errorf("Phase = %q, want %q", results[0].Phase, PhaseDeployment)
	}
}

func TestValidatePhaseNoCluster(t *testing.T) {
	v := New(
		WithVersion("1.0.0"),
		WithNoCluster(true),
	)

	pr, err := v.ValidatePhase(context.Background(), PhaseDeployment, nil, nil)
	if err != nil {
		t.Fatalf("ValidatePhase() failed: %v", err)
	}

	if pr.Status != ctrf.StatusSkipped {
		t.Errorf("status = %q, want %q", pr.Status, ctrf.StatusSkipped)
	}
	if pr.Phase != PhaseDeployment {
		t.Errorf("phase = %q, want %q", pr.Phase, PhaseDeployment)
	}
}

func TestCheckReadinessNilInputs(t *testing.T) {
	tests := []struct {
		name string
		rec  *recipe.RecipeResult
		snap *snapshotter.Snapshot
	}{
		{"nil recipe", nil, &snapshotter.Snapshot{}},
		{"nil snapshot", &recipe.RecipeResult{}, nil},
		{"both nil", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := checkReadiness(tt.rec, tt.snap); err != nil {
				t.Errorf("checkReadiness() = %v, want nil", err)
			}
		})
	}
}

func TestCheckReadinessEmptyConstraints(t *testing.T) {
	rec := &recipe.RecipeResult{
		Constraints: []recipe.Constraint{},
	}
	snap := &snapshotter.Snapshot{}
	if err := checkReadiness(rec, snap); err != nil {
		t.Errorf("checkReadiness() = %v, want nil for empty constraints", err)
	}
}

func TestCheckReadinessPassingConstraint(t *testing.T) {
	// Use a constraint path that Evaluate can resolve.
	// When the path cannot be parsed or value not found, Evaluate returns Error (skipped).
	// A constraint with an unparseable name results in Error != nil -> skipped (not failure).
	rec := &recipe.RecipeResult{
		Constraints: []recipe.Constraint{
			{Name: "invalid-path-that-will-be-skipped", Value: "anything"},
		},
	}
	snap := &snapshotter.Snapshot{}
	// Unparseable constraint path => skipped (warn), not failure.
	if err := checkReadiness(rec, snap); err != nil {
		t.Errorf("checkReadiness() = %v, want nil for skipped constraint", err)
	}
}

func TestFilterEntriesByRecipeNilRecipe(t *testing.T) {
	entries := []catalog.ValidatorEntry{
		{Name: "v1", Phase: "deployment"},
		{Name: "v2", Phase: "deployment"},
	}
	got := filterEntriesByRecipe(entries, PhaseDeployment, nil)
	if len(got) != 0 {
		t.Errorf("filterEntriesByRecipe(nil recipe) returned %d entries, want 0 (skip)", len(got))
	}
}

func TestFilterEntriesByRecipeNilValidation(t *testing.T) {
	entries := []catalog.ValidatorEntry{
		{Name: "v1", Phase: "deployment"},
	}
	rec := &recipe.RecipeResult{Validation: nil}
	got := filterEntriesByRecipe(entries, PhaseDeployment, rec)
	if len(got) != 0 {
		t.Errorf("filterEntriesByRecipe(nil validation) returned %d entries, want 0 (skip)", len(got))
	}
}

func TestFilterEntriesByRecipeNoChecks(t *testing.T) {
	entries := []catalog.ValidatorEntry{
		{Name: "v1", Phase: "deployment"},
		{Name: "v2", Phase: "deployment"},
	}
	rec := &recipe.RecipeResult{
		Validation: &recipe.ValidationConfig{
			Deployment: &recipe.ValidationPhase{},
		},
	}
	got := filterEntriesByRecipe(entries, PhaseDeployment, rec)
	if len(got) != 0 {
		t.Errorf("filterEntriesByRecipe(empty checks) returned %d entries, want 0 (skip)", len(got))
	}
}

func TestFilterEntriesByRecipeWithChecks(t *testing.T) {
	entries := []catalog.ValidatorEntry{
		{Name: "v1", Phase: "deployment"},
		{Name: "v2", Phase: "deployment"},
		{Name: "v3", Phase: "deployment"},
	}

	tests := []struct {
		name     string
		phase    Phase
		rec      *recipe.RecipeResult
		expected int
		names    []string
	}{
		{
			name:  "deployment filters to declared checks",
			phase: PhaseDeployment,
			rec: &recipe.RecipeResult{
				Validation: &recipe.ValidationConfig{
					Deployment: &recipe.ValidationPhase{
						Checks: []string{"v1", "v3"},
					},
				},
			},
			expected: 2,
			names:    []string{"v1", "v3"},
		},
		{
			name:  "performance phase with no matching entries",
			phase: PhasePerformance,
			rec: &recipe.RecipeResult{
				Validation: &recipe.ValidationConfig{
					Performance: &recipe.ValidationPhase{
						Checks: []string{"perf-only"},
					},
				},
			},
			expected: 0,
		},
		{
			name:  "conformance phase returns none when nil phase config",
			phase: PhaseConformance,
			rec: &recipe.RecipeResult{
				Validation: &recipe.ValidationConfig{
					Deployment: &recipe.ValidationPhase{
						Checks: []string{"v1"},
					},
				},
			},
			expected: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterEntriesByRecipe(entries, tt.phase, tt.rec)
			if len(got) != tt.expected {
				t.Errorf("filterEntriesByRecipe() returned %d entries, want %d", len(got), tt.expected)
			}
			if tt.names != nil {
				for i, name := range tt.names {
					if i < len(got) && got[i].Name != name {
						t.Errorf("got[%d].Name = %q, want %q", i, got[i].Name, name)
					}
				}
			}
		})
	}
}

func TestFilterEntriesByRecipeEmptyChecksList(t *testing.T) {
	entries := []catalog.ValidatorEntry{
		{Name: "v1", Phase: "deployment"},
	}
	rec := &recipe.RecipeResult{
		Validation: &recipe.ValidationConfig{
			Deployment: &recipe.ValidationPhase{
				Checks: []string{},
			},
		},
	}
	got := filterEntriesByRecipe(entries, PhaseDeployment, rec)
	if len(got) != 0 {
		t.Errorf("filterEntriesByRecipe(empty checks list) returned %d entries, want 0 (skip)", len(got))
	}
}

func TestPhaseOrder(t *testing.T) {
	expected := []Phase{PhaseDeployment, PhasePerformance, PhaseConformance}
	if len(PhaseOrder) != len(expected) {
		t.Fatalf("PhaseOrder length = %d, want %d", len(PhaseOrder), len(expected))
	}
	for i, p := range PhaseOrder {
		if p != expected[i] {
			t.Errorf("PhaseOrder[%d] = %q, want %q", i, p, expected[i])
		}
	}
}

// TestRecipeCheckNamesMatchCatalog verifies that every check name referenced
// in recipe overlays exists in the validator catalog for the correct phase.
// Catches typos and drift between recipes and catalog at PR time.
func TestRecipeCheckNamesMatchCatalog(t *testing.T) {
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatalf("failed to load catalog: %v", err)
	}

	// Build lookup: phase → set of valid check names.
	validChecks := map[string]map[string]bool{
		"deployment":  make(map[string]bool),
		"performance": make(map[string]bool),
		"conformance": make(map[string]bool),
	}
	for _, entry := range cat.Validators {
		if m, ok := validChecks[entry.Phase]; ok {
			m[entry.Name] = true
		}
	}

	// Walk all embedded overlay YAML files.
	err = fs.WalkDir(recipes.FS, "overlays", func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || filepath.Ext(path) != ".yaml" {
			return nil
		}

		data, readErr := recipes.FS.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("failed to read %s: %w", path, readErr)
		}

		var metadata recipe.RecipeMetadata
		if unmarshalErr := yaml.Unmarshal(data, &metadata); unmarshalErr != nil {
			return nil // skip non-recipe YAML
		}

		if metadata.Spec.Validation == nil {
			return nil
		}

		phases := map[string]*recipe.ValidationPhase{
			"deployment":  metadata.Spec.Validation.Deployment,
			"performance": metadata.Spec.Validation.Performance,
			"conformance": metadata.Spec.Validation.Conformance,
		}

		for phase, vp := range phases {
			if vp == nil {
				continue
			}
			for _, checkName := range vp.Checks {
				if !validChecks[phase][checkName] {
					t.Errorf("%s: check %q in %s phase does not exist in catalog (valid: %v)",
						path, checkName, phase, catalogNames(cat, phase))
				}
			}
		}

		return nil
	})
	if err != nil {
		t.Fatalf("failed to walk overlays: %v", err)
	}
}

func catalogNames(cat *catalog.ValidatorCatalog, phase string) []string {
	entries := cat.ForPhase(phase)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return names
}
