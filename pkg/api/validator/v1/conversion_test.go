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
	"gopkg.in/yaml.v3"
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

// TestToValidationInputYAMLRoundTrip is the regression guard for the
// orchestrator/validator wire-shape mismatch that silently dropped phase
// constraints (issue #874). The validator runtime deserializes the
// ConfigMap payload into *ValidationInput and looks up phase constraints
// under ctx.ValidationInput.Config.Performance / Config.Deployment.
//
// This test exercises the exact path: recipe.RecipeResult →
// ToValidationInput → yaml.Marshal → yaml.Unmarshal → constraint lookup,
// and asserts both a performance and a deployment constraint survive the
// round trip. A regression to marshaling recipe.Validation directly (which
// inlines ValidationConfig) would land the phase fields at the YAML root
// and fail this test.
func TestToValidationInputYAMLRoundTrip(t *testing.T) {
	rec := &recipe.RecipeResult{
		APIVersion: "aicr.nvidia.com/v1",
		Kind:       "RecipeResult",
		Constraints: []recipe.Constraint{
			{Name: "K8s.server.version", Value: ">= 1.32.4"},
		},
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator"},
		},
		Validation: &recipe.ValidationConfig{
			Deployment: &recipe.ValidationPhase{
				Constraints: []recipe.Constraint{
					{Name: "Deployment.gpu-operator.version", Value: "580.126.20"},
				},
				Checks: []string{"gpu-operator-version"},
			},
			Performance: &recipe.ValidationPhase{
				Constraints: []recipe.Constraint{
					{Name: "nccl-all-reduce-bw-net", Value: ">= 300"},
					{Name: "nccl-all-reduce-bw-nvls", Value: ">= 800"},
				},
				Checks: []string{"nccl-all-reduce-bw-net", "nccl-all-reduce-bw-nvls"},
			},
		},
	}

	data, err := yaml.Marshal(ToValidationInput(rec))
	if err != nil {
		t.Fatalf("yaml.Marshal() failed: %v", err)
	}

	var got ValidationInput
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("yaml.Unmarshal() failed: %v\npayload:\n%s", err, data)
	}

	// Deployment constraint must survive the round trip — the bug surfaced
	// here as gpu-operator-version silently passing.
	deploy := findConstraint(got.Config.Deployment, "Deployment.gpu-operator.version")
	if deploy == nil {
		t.Errorf("Deployment.gpu-operator.version constraint lost after YAML round trip\npayload:\n%s", data)
	} else if deploy.Value != "580.126.20" {
		t.Errorf("Deployment constraint value = %q, want %q", deploy.Value, "580.126.20")
	}

	// Performance constraints must survive — the bug surfaced here as
	// nccl-all-reduce-bw-{net,nvls} silently skipping with "no
	// <name> constraint in recipe".
	for _, name := range []string{"nccl-all-reduce-bw-net", "nccl-all-reduce-bw-nvls"} {
		c := findConstraint(got.Config.Performance, name)
		if c == nil {
			t.Errorf("%s constraint lost after YAML round trip\npayload:\n%s", name, data)
		}
	}
}

// findConstraint mirrors how validator runtime helpers look up phase
// constraints — see validators/performance/nccl_all_reduce_bw.go and
// validators/deployment/gpu_operator_version.go.
func findConstraint(phase *ValidationPhase, name string) *recipe.Constraint {
	if phase == nil {
		return nil
	}
	for i := range phase.Constraints {
		if phase.Constraints[i].Name == name {
			return &phase.Constraints[i]
		}
	}
	return nil
}
