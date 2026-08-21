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

package recipe_test

import (
	"context"
	"slices"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

const (
	stockAdoptionComponent = "k8s-aibom"
	stockAdoptionVersion   = "k8s-aibom-stock-adoption-test"
)

// TestK8sAIBOMStockAdoption pins the stock-adoption contract from ADR-019's
// amendment: exactly one stock recipe installs k8s-aibom, its only descendant
// declines it, and the generation-time flag declines it too.
//
// Why this test rather than the parity goldens: both goldens key on *leaf*
// recipes, and `h100-gke-cos-inference` is not a leaf — `-dynamo` bases on it.
// So the target recipe of this whole amendment has no golden coverage, and the
// only golden that moves is the collateral one on the descendant. Without this
// test, flipping the target ref to `install: false` would leave every existing
// test green while silently un-shipping the component.
//
// DeploymentOrder is asserted alongside IsEnabled because the order is what
// bundlers walk. A ref that is declared-but-declined must be absent from it,
// which is the emission-level half of the claim.
func TestK8sAIBOMStockAdoption(t *testing.T) {
	target := func() *recipe.Criteria {
		return &recipe.Criteria{
			Service:     recipe.CriteriaServiceGKE,
			Accelerator: recipe.CriteriaAcceleratorH100,
			OS:          recipe.CriteriaOSCOS,
			Intent:      recipe.CriteriaIntentInference,
		}
	}

	tests := []struct {
		name         string
		criteria     *recipe.Criteria
		opts         []recipe.BuildOption
		wantDeclared bool
		wantEnabled  bool
	}{
		{
			name:         "target stock recipe declares and enables the component",
			criteria:     target(),
			wantDeclared: true,
			wantEnabled:  true,
		},
		{
			name: "dynamo descendant declares but declines it",
			criteria: func() *recipe.Criteria {
				c := target()
				c.Platform = recipe.CriteriaPlatformDynamo
				return c
			}(),
			wantDeclared: true,
			wantEnabled:  false,
		},
		{
			name:         "generation-time opt-out declines it on the target",
			criteria:     target(),
			opts:         []recipe.BuildOption{recipe.WithRuntimeInventoryMode(recipe.RuntimeInventoryDisabled)},
			wantDeclared: true,
			wantEnabled:  false,
		},
		{
			name: "a sibling stock recipe does not declare it at all",
			criteria: func() *recipe.Criteria {
				c := target()
				c.Intent = recipe.CriteriaIntentTraining
				return c
			}(),
			wantDeclared: false,
			wantEnabled:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := recipe.NewBuilder(recipe.WithVersion(stockAdoptionVersion))

			result, err := builder.BuildFromCriteria(context.Background(), tt.criteria, tt.opts...)
			if err != nil {
				t.Fatalf("BuildFromCriteria() error = %v", err)
			}

			var ref *recipe.ComponentRef
			for i := range result.ComponentRefs {
				if result.ComponentRefs[i].Name == stockAdoptionComponent {
					ref = &result.ComponentRefs[i]
					break
				}
			}

			if !tt.wantDeclared {
				if ref != nil {
					t.Fatalf("component %q is declared, want absent: adoption leaked beyond the target recipe",
						stockAdoptionComponent)
				}
				if slices.Contains(result.DeploymentOrder, stockAdoptionComponent) {
					t.Errorf("component %q is in DeploymentOrder despite not being declared",
						stockAdoptionComponent)
				}
				return
			}

			if ref == nil {
				t.Fatalf("component %q is absent, want declared: the target recipe no longer ships it",
					stockAdoptionComponent)
			}
			if got := ref.IsEnabled(); got != tt.wantEnabled {
				t.Errorf("IsEnabled() = %v, want %v", got, tt.wantEnabled)
			}
			if got := slices.Contains(result.DeploymentOrder, stockAdoptionComponent); got != tt.wantEnabled {
				t.Errorf("in DeploymentOrder = %v, want %v: enabled state and emission disagree",
					got, tt.wantEnabled)
			}
		})
	}
}
