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

package v1

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestToValidationInput(t *testing.T) {
	recipeResult := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1",
		Kind:       "RecipeResult",
		Metadata: struct {
			Version            string                     `json:"version,omitempty" yaml:"version,omitempty"`
			AppliedOverlays    []string                   `json:"appliedOverlays,omitempty" yaml:"appliedOverlays,omitempty"`
			ExcludedOverlays   []recipe.ExcludedOverlay   `json:"excludedOverlays,omitempty" yaml:"excludedOverlays,omitempty"`
			ConstraintWarnings []recipe.ConstraintWarning `json:"constraintWarnings,omitempty" yaml:"constraintWarnings,omitempty"`
		}{
			Version:         "1.0.0",
			AppliedOverlays: []string{"base", "eks"},
		},
		Criteria: &recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
		},
		Constraints: []recipe.Constraint{
			{Name: "test-constraint", Value: "test-value"},
		},
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator"},
		},
		Validation: &recipe.ValidationConfig{
			Deployment: &recipe.ValidationPhase{
				Timeout: "10m",
			},
		},
	}

	validation := ToValidationInput(recipeResult)

	if validation == nil {
		t.Fatal("ToValidationInput returned nil")
	}

	// Verify optional resource fields are populated
	if validation.APIVersion != "validator.nvidia.com/v1alpha1" {
		t.Errorf("APIVersion = %q, want %q", validation.APIVersion, "validator.nvidia.com/v1alpha1")
	}
	if validation.Kind != KindValidationInput {
		t.Errorf("Kind = %q, want %q", validation.Kind, KindValidationInput)
	}
	if validation.Metadata == nil {
		t.Fatal("Metadata is nil")
	}
	if validation.Metadata.Version != "1.0.0" {
		t.Errorf("Metadata.Version = %q, want %q", validation.Metadata.Version, "1.0.0")
	}

	// Verify validation fields are copied
	if validation.Criteria.Service != recipe.CriteriaServiceEKS {
		t.Errorf("Criteria.Service = %q, want %q", validation.Criteria.Service, recipe.CriteriaServiceEKS)
	}
	if len(validation.Constraints) != 1 {
		t.Errorf("len(Constraints) = %d, want 1", len(validation.Constraints))
	}
	if len(validation.ComponentRefs) != 1 {
		t.Errorf("len(ComponentRefs) = %d, want 1", len(validation.ComponentRefs))
	}
	if validation.Config.Deployment == nil {
		t.Fatal("Config.Deployment is nil")
	}
	if validation.Config.Deployment.Timeout != "10m" {
		t.Errorf("Config.Deployment.Timeout = %q, want %q", validation.Config.Deployment.Timeout, "10m")
	}
}

func TestToValidationInputNil(t *testing.T) {
	validation := ToValidationInput(nil)
	if validation != nil {
		t.Errorf("ToValidationInput(nil) = %v, want nil", validation)
	}
}
