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

package validations

import (
	"context"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestCheckNPDNotDuplicatingProviderNPD(t *testing.T) {
	gkeConditions := map[string][]string{"service": {"gke", "aks"}}

	npdOnGKE := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{{Name: "node-problem-detector"}},
		Criteria:      &recipe.Criteria{Service: recipe.CriteriaServiceGKE},
	}
	npdOnAKS := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{{Name: "node-problem-detector"}},
		Criteria:      &recipe.Criteria{Service: recipe.CriteriaServiceAKS},
	}
	npdOnEKS := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{{Name: "node-problem-detector"}},
		Criteria:      &recipe.Criteria{Service: recipe.CriteriaServiceEKS},
	}
	npdNoCriteria := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{{Name: "node-problem-detector"}},
		Criteria:      nil,
	}
	npdOn := func(service recipe.CriteriaServiceType) *recipe.RecipeResult {
		return &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{{Name: "node-problem-detector"}},
			Criteria:      &recipe.Criteria{Service: service},
		}
	}
	npdOnTalos := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{{Name: "node-problem-detector"}},
		Criteria:      &recipe.Criteria{Service: recipe.CriteriaServiceEKS, OS: recipe.CriteriaOSTalos},
	}

	tests := []struct {
		name          string
		componentName string
		recipeResult  *recipe.RecipeResult
		bundlerConfig *config.Config
		conditions    map[string][]string
		wantErrors    int
		wantErrMsg    string
	}{
		{
			name:          "component absent from recipe",
			componentName: "node-problem-detector",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}},
				Criteria:      &recipe.Criteria{Service: recipe.CriteriaServiceGKE},
			},
			bundlerConfig: config.NewConfig(),
			conditions:    gkeConditions,
			wantErrors:    0,
		},
		{
			name:          "condition not met (eks has no provider-installed NPD)",
			componentName: "node-problem-detector",
			recipeResult:  npdOnEKS,
			bundlerConfig: config.NewConfig(),
			conditions:    gkeConditions,
			wantErrors:    0,
		},
		{
			name:          "gke blocked",
			componentName: "node-problem-detector",
			recipeResult:  npdOnGKE,
			bundlerConfig: config.NewConfig(),
			conditions:    gkeConditions,
			wantErrors:    1,
			wantErrMsg:    "already runs its own node-problem-detector",
		},
		{
			name:          "aks blocked",
			componentName: "node-problem-detector",
			recipeResult:  npdOnAKS,
			bundlerConfig: config.NewConfig(),
			conditions:    gkeConditions,
			wantErrors:    1,
			wantErrMsg:    "already runs its own node-problem-detector",
		},
		{
			// Qualified: verified on live clusters or upstream packaging.
			name:          "kind allowed",
			componentName: "node-problem-detector",
			recipeResult:  npdOn(recipe.CriteriaServiceKind),
			conditions:    gkeConditions,
		},
		{
			name:          "rke2 allowed",
			componentName: "node-problem-detector",
			recipeResult:  npdOn(recipe.CriteriaServiceRKE2),
			conditions:    gkeConditions,
		},
		{
			// Oracle ships NPD disabled behind a node label. The label is
			// operator-settable and invisible at bundle time, so a second
			// instance cannot be ruled out.
			name:          "oke -> blocked (provider NPD is label-gated, not absent)",
			componentName: "node-problem-detector",
			recipeResult:  npdOn(recipe.CriteriaServiceOKE),
			conditions:    gkeConditions,
			wantErrors:    1,
		},
		{
			// An allowlist, so unexamined platforms fail closed rather than
			// passing by omission.
			name:          "lke unverified -> blocked",
			componentName: "node-problem-detector",
			recipeResult:  npdOn(recipe.CriteriaServiceLKE),
			conditions:    gkeConditions,
			wantErrors:    1,
		},
		{
			name:          "generic unverified -> blocked",
			componentName: "node-problem-detector",
			recipeResult:  npdOn(recipe.CriteriaServiceGeneric),
			conditions:    gkeConditions,
			wantErrors:    1,
		},
		{
			name:          "bcm unverified -> blocked",
			componentName: "node-problem-detector",
			recipeResult:  npdOn(recipe.CriteriaServiceBCM),
			conditions:    gkeConditions,
			wantErrors:    1,
		},
		{
			// Privileged DaemonSet with no SCC binding shipped: bundles clean,
			// then denied at admission.
			name:          "ocp -> blocked",
			componentName: "node-problem-detector",
			recipeResult:  npdOn(recipe.CriteriaServiceOCP),
			conditions:    gkeConditions,
			wantErrors:    1,
		},
		{
			// os-talos relocates privileged components to privileged-*
			// namespaces for PSA; NPD is not among them.
			name:          "talos OS on a qualified service -> blocked",
			componentName: "node-problem-detector",
			recipeResult:  npdOnTalos,
			conditions:    gkeConditions,
			wantErrors:    1,
		},
		{
			name:          "nil recipeResult",
			componentName: "node-problem-detector",
			recipeResult:  nil,
			bundlerConfig: config.NewConfig(),
			conditions:    gkeConditions,
			wantErrors:    0,
		},
		{
			// Regression: a criteria-less RecipeResult (e.g. an
			// already-hydrated recipe loaded via pkg/client/v1's
			// LoadRecipe path -- see loadedResultFromInternal) means the
			// target platform is UNKNOWN, not "confirmed not gke/aks".
			// checkConditions' blanket nil-Criteria bypass must not be
			// used to skip this gate silently -- fail closed instead.
			name:          "no criteria at all: fails closed rather than silently passing",
			componentName: "node-problem-detector",
			recipeResult:  npdNoCriteria,
			bundlerConfig: config.NewConfig(),
			conditions:    gkeConditions,
			wantErrors:    1,
			wantErrMsg:    "carries no criteria",
		},
		{
			name:          "no criteria, but explicitly disabled via --set skips the gate",
			componentName: "node-problem-detector",
			recipeResult:  npdNoCriteria,
			bundlerConfig: config.NewConfig(
				config.WithValueOverrides(map[string]map[string]string{
					"node-problem-detector": {"enabled": "false"},
				}),
			),
			conditions: gkeConditions,
			wantErrors: 0,
		},
		{
			name:          "gke but explicitly disabled via --set skips the gate",
			componentName: "node-problem-detector",
			recipeResult:  npdOnGKE,
			bundlerConfig: config.NewConfig(
				config.WithValueOverrides(map[string]map[string]string{
					"node-problem-detector": {"enabled": "false"},
				}),
			),
			conditions: gkeConditions,
			wantErrors: 0,
		},
		{
			name:          "nil bundlerConfig still blocks on gke",
			componentName: "node-problem-detector",
			recipeResult:  npdOnGKE,
			bundlerConfig: nil,
			conditions:    gkeConditions,
			wantErrors:    1,
			wantErrMsg:    "already runs its own node-problem-detector",
		},
		{
			// Regression: the check must resolve the effective enabled
			// state via componentDisabled's priority-ordered alias merge
			// (componentName wins over its aliases), not a naive "any
			// alias says false" OR scan. componentOverrideKeys' fallback
			// path (no registry reachable from a bare test RecipeResult
			// with no bound DataProvider) still produces the
			// non-hyphenated alias "nodeproblemdetector" alongside the
			// canonical "node-problem-detector" key -- exercising the
			// exact two-key priority order the gate depends on without
			// needing a live registry. A conflicting --set
			// node-problem-detector:enabled=true alongside --set
			// nodeproblemdetector:enabled=false must NOT disarm the gate,
			// because the canonical key (checked first by
			// mergeOverridesAcrossKeys) says the component stays enabled.
			name:          "conflicting alias overrides: canonical key wins, gate still fires",
			componentName: "node-problem-detector",
			recipeResult:  npdOnGKE,
			bundlerConfig: config.NewConfig(
				config.WithValueOverrides(map[string]map[string]string{
					"node-problem-detector": {"enabled": "true"},
					"nodeproblemdetector":   {"enabled": "false"},
				}),
			),
			conditions: gkeConditions,
			wantErrors: 1,
			wantErrMsg: "already runs its own node-problem-detector",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, errs := CheckNPDNotDuplicatingProviderNPD(context.Background(), tt.componentName, tt.recipeResult, tt.bundlerConfig, tt.conditions)
			if len(errs) != tt.wantErrors {
				t.Fatalf("errors = %d, want %d (errs: %v)", len(errs), tt.wantErrors, errs)
			}
			if tt.wantErrMsg != "" {
				if len(errs) == 0 || !strings.Contains(errs[0].Error(), tt.wantErrMsg) {
					t.Errorf("error = %v, want containing %q", errs, tt.wantErrMsg)
				}
			}
		})
	}
}
