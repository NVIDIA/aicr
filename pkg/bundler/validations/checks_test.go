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
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestCheckWorkloadSelectorMissing(t *testing.T) {
	tests := []struct {
		name           string
		componentName  string
		recipeResult   *recipe.RecipeResult
		bundlerConfig  *config.Config
		conditions     map[string][]string
		wantWarnings   int
		wantErrors     int
		wantWarningMsg string
	}{
		{
			name:          "component not in recipe",
			componentName: "nodewright-customizations",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "gpu-operator"},
				},
			},
			bundlerConfig: config.NewConfig(),
			conditions:    map[string][]string{"intent": {"training"}},
			wantWarnings:  0,
			wantErrors:    0,
		},
		{
			name:          "condition not met",
			componentName: "nodewright-customizations",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "nodewright-customizations"},
				},
				Criteria: &recipe.Criteria{
					Intent: recipe.CriteriaIntentInference,
				},
			},
			bundlerConfig: config.NewConfig(),
			conditions:    map[string][]string{"intent": {"training"}},
			wantWarnings:  0,
			wantErrors:    0,
		},
		{
			name:          "workload selector missing with training intent",
			componentName: "nodewright-customizations",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "nodewright-customizations"},
				},
				Criteria: &recipe.Criteria{
					Intent: recipe.CriteriaIntentTraining,
				},
			},
			bundlerConfig:  config.NewConfig(),
			conditions:     map[string][]string{"intent": {"training"}},
			wantWarnings:   1,
			wantErrors:     0,
			wantWarningMsg: "nodewright-customizations is enabled but --workload-selector is not set",
		},
		{
			name:          "workload selector set",
			componentName: "nodewright-customizations",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "nodewright-customizations"},
				},
				Criteria: &recipe.Criteria{
					Intent: recipe.CriteriaIntentTraining,
				},
			},
			bundlerConfig: config.NewConfig(
				config.WithWorkloadSelector(map[string]string{"workload-type": "training"}),
			),
			conditions:   map[string][]string{"intent": {"training"}},
			wantWarnings: 0,
			wantErrors:   0,
		},
		{
			name:          "nil config",
			componentName: "nodewright-customizations",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "nodewright-customizations"},
				},
				Criteria: &recipe.Criteria{
					Intent: recipe.CriteriaIntentTraining,
				},
			},
			bundlerConfig: nil,
			conditions:    map[string][]string{"intent": {"training"}},
			wantWarnings:  0,
			wantErrors:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			warnings, errors := CheckWorkloadSelectorMissing(ctx, tt.componentName, tt.recipeResult, tt.bundlerConfig, tt.conditions)

			if len(warnings) != tt.wantWarnings {
				t.Errorf("CheckWorkloadSelectorMissing() warnings = %d, want %d", len(warnings), tt.wantWarnings)
			}
			if len(errors) != tt.wantErrors {
				t.Errorf("CheckWorkloadSelectorMissing() errors = %d, want %d", len(errors), tt.wantErrors)
			}

			if tt.wantWarningMsg != "" && len(warnings) > 0 {
				if !slices.Contains(warnings, tt.wantWarningMsg) {
					t.Errorf("CheckWorkloadSelectorMissing() warning message = %v, want to contain %q", warnings, tt.wantWarningMsg)
				}
			}
		})
	}
}

func TestCheckAcceleratedSelectorMissing(t *testing.T) {
	tests := []struct {
		name           string
		componentName  string
		recipeResult   *recipe.RecipeResult
		bundlerConfig  *config.Config
		conditions     map[string][]string
		wantWarnings   int
		wantErrors     int
		wantWarningMsg string
	}{
		{
			name:          "component not in recipe",
			componentName: "nodewright-customizations",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "gpu-operator"},
				},
			},
			bundlerConfig: config.NewConfig(),
			conditions:    map[string][]string{"intent": {"training", "inference"}},
			wantWarnings:  0,
			wantErrors:    0,
		},
		{
			name:          "condition not met",
			componentName: "nodewright-customizations",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "nodewright-customizations"},
				},
				Criteria: &recipe.Criteria{
					Intent: "other",
				},
			},
			bundlerConfig: config.NewConfig(),
			conditions:    map[string][]string{"intent": {"training", "inference"}},
			wantWarnings:  0,
			wantErrors:    0,
		},
		{
			name:          "accelerated selector missing with training intent",
			componentName: "nodewright-customizations",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "nodewright-customizations"},
				},
				Criteria: &recipe.Criteria{
					Intent: recipe.CriteriaIntentTraining,
				},
			},
			bundlerConfig:  config.NewConfig(),
			conditions:     map[string][]string{"intent": {"training", "inference"}},
			wantWarnings:   1,
			wantErrors:     0,
			wantWarningMsg: "nodewright-customizations is enabled but --accelerated-node-selector is not set",
		},
		{
			name:          "accelerated selector missing with inference intent",
			componentName: "nodewright-customizations",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "nodewright-customizations"},
				},
				Criteria: &recipe.Criteria{
					Intent: recipe.CriteriaIntentInference,
				},
			},
			bundlerConfig:  config.NewConfig(),
			conditions:     map[string][]string{"intent": {"training", "inference"}},
			wantWarnings:   1,
			wantErrors:     0,
			wantWarningMsg: "nodewright-customizations is enabled but --accelerated-node-selector is not set",
		},
		{
			name:          "accelerated selector set",
			componentName: "nodewright-customizations",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "nodewright-customizations"},
				},
				Criteria: &recipe.Criteria{
					Intent: recipe.CriteriaIntentTraining,
				},
			},
			bundlerConfig: config.NewConfig(
				config.WithAcceleratedNodeSelector(map[string]string{"nodeGroup": "gpu-worker"}),
			),
			conditions:   map[string][]string{"intent": {"training", "inference"}},
			wantWarnings: 0,
			wantErrors:   0,
		},
		{
			name:          "nil config",
			componentName: "nodewright-customizations",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "nodewright-customizations"},
				},
				Criteria: &recipe.Criteria{
					Intent: recipe.CriteriaIntentTraining,
				},
			},
			bundlerConfig: nil,
			conditions:    map[string][]string{"intent": {"training", "inference"}},
			wantWarnings:  0,
			wantErrors:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			warnings, errors := CheckAcceleratedSelectorMissing(ctx, tt.componentName, tt.recipeResult, tt.bundlerConfig, tt.conditions)

			if len(warnings) != tt.wantWarnings {
				t.Errorf("CheckAcceleratedSelectorMissing() warnings = %d, want %d", len(warnings), tt.wantWarnings)
			}
			if len(errors) != tt.wantErrors {
				t.Errorf("CheckAcceleratedSelectorMissing() errors = %d, want %d", len(errors), tt.wantErrors)
			}

			if tt.wantWarningMsg != "" && len(warnings) > 0 {
				if !slices.Contains(warnings, tt.wantWarningMsg) {
					t.Errorf("CheckAcceleratedSelectorMissing() warning message = %v, want to contain %q", warnings, tt.wantWarningMsg)
				}
			}
		})
	}
}

func TestCheckWildcardAcceleratedToleration(t *testing.T) {
	aksRecipe := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{Name: "nodewright-customizations"},
		},
		Criteria: &recipe.Criteria{
			Service: recipe.CriteriaServiceAKS,
		},
	}
	aksConditions := map[string][]string{"service": {"aks"}}

	tests := []struct {
		name           string
		componentName  string
		recipeResult   *recipe.RecipeResult
		bundlerConfig  *config.Config
		conditions     map[string][]string
		wantWarnings   int
		wantErrors     int
		wantWarningMsg string
	}{
		{
			name:          "component not in recipe",
			componentName: "nodewright-customizations",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "gpu-operator"},
				},
				Criteria: &recipe.Criteria{
					Service: recipe.CriteriaServiceAKS,
				},
			},
			bundlerConfig: config.NewConfig(),
			conditions:    aksConditions,
			wantWarnings:  0,
			wantErrors:    0,
		},
		{
			name:          "condition not met (eks)",
			componentName: "nodewright-customizations",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "nodewright-customizations"},
				},
				Criteria: &recipe.Criteria{
					Service: recipe.CriteriaServiceEKS,
				},
			},
			bundlerConfig: config.NewConfig(
				config.WithAcceleratedNodeTolerations([]corev1.Toleration{
					{Operator: corev1.TolerationOpExists},
				}),
			),
			conditions:   aksConditions,
			wantWarnings: 0,
			wantErrors:   0,
		},
		{
			name:           "no tolerations set (template wildcard fallback)",
			componentName:  "nodewright-customizations",
			recipeResult:   aksRecipe,
			bundlerConfig:  config.NewConfig(),
			conditions:     aksConditions,
			wantWarnings:   1,
			wantErrors:     0,
			wantWarningMsg: "wildcard (keyless) accelerated-node toleration",
		},
		{
			name:          "default wildcard toleration (CLI fallback)",
			componentName: "nodewright-customizations",
			recipeResult:  aksRecipe,
			bundlerConfig: config.NewConfig(
				config.WithAcceleratedNodeTolerations([]corev1.Toleration{
					{Operator: corev1.TolerationOpExists},
				}),
			),
			conditions:     aksConditions,
			wantWarnings:   1,
			wantErrors:     0,
			wantWarningMsg: "wildcard (keyless) accelerated-node toleration",
		},
		{
			name:          "keyed toleration only",
			componentName: "nodewright-customizations",
			recipeResult:  aksRecipe,
			bundlerConfig: config.NewConfig(
				config.WithAcceleratedNodeTolerations([]corev1.Toleration{
					{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
				}),
			),
			conditions:   aksConditions,
			wantWarnings: 0,
			wantErrors:   0,
		},
		{
			name:          "component disabled via --set (RDMA opt-out) skips wildcard check",
			componentName: "nodewright-customizations",
			recipeResult:  aksRecipe,
			bundlerConfig: config.NewConfig(
				config.WithValueOverrides(map[string]map[string]string{
					"nodewrightcustomizations": {"enabled": "false"},
				}),
			),
			conditions:   aksConditions,
			wantWarnings: 0,
			wantErrors:   0,
		},
		{
			name:          "component disabled via skyhook alias skips wildcard check",
			componentName: "nodewright-customizations",
			recipeResult:  aksRecipe,
			bundlerConfig: config.NewConfig(
				config.WithValueOverrides(map[string]map[string]string{
					"skyhookcustomizations": {"enabled": "false"},
				}),
			),
			conditions:   aksConditions,
			wantWarnings: 0,
			wantErrors:   0,
		},
		{
			name:          "component disabled via exact hyphenated name skips wildcard check",
			componentName: "nodewright-customizations",
			recipeResult:  aksRecipe,
			bundlerConfig: config.NewConfig(
				config.WithValueOverrides(map[string]map[string]string{
					"nodewright-customizations": {"enabled": "false"},
				}),
			),
			conditions:   aksConditions,
			wantWarnings: 0,
			wantErrors:   0,
		},
		{
			name:          "keyed plus wildcard still warns",
			componentName: "nodewright-customizations",
			recipeResult:  aksRecipe,
			bundlerConfig: config.NewConfig(
				config.WithAcceleratedNodeTolerations([]corev1.Toleration{
					{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
					{Operator: corev1.TolerationOpExists},
				}),
			),
			conditions:     aksConditions,
			wantWarnings:   1,
			wantErrors:     0,
			wantWarningMsg: "wildcard (keyless) accelerated-node toleration",
		},
		{
			name:          "nil config",
			componentName: "nodewright-customizations",
			recipeResult:  aksRecipe,
			bundlerConfig: nil,
			conditions:    aksConditions,
			wantWarnings:  0,
			wantErrors:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			warnings, errors := CheckWildcardAcceleratedToleration(ctx, tt.componentName, tt.recipeResult, tt.bundlerConfig, tt.conditions)

			if len(warnings) != tt.wantWarnings {
				t.Errorf("CheckWildcardAcceleratedToleration() warnings = %d, want %d", len(warnings), tt.wantWarnings)
			}
			if len(errors) != tt.wantErrors {
				t.Errorf("CheckWildcardAcceleratedToleration() errors = %d, want %d", len(errors), tt.wantErrors)
			}

			if tt.wantWarningMsg != "" && len(warnings) > 0 {
				found := false
				for _, w := range warnings {
					if strings.Contains(w, tt.wantWarningMsg) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("CheckWildcardAcceleratedToleration() warnings = %v, want to contain %q", warnings, tt.wantWarningMsg)
				}
			}
		})
	}
}

func TestCheckHostMofedWithoutNetworkOperator(t *testing.T) {
	tests := []struct {
		name           string
		componentName  string
		recipeResult   *recipe.RecipeResult
		bundlerConfig  *config.Config
		conditions     map[string][]string
		wantWarnings   int
		wantErrors     int
		wantWarningMsg string
	}{
		{
			name:          "network-operator disabled without useHostMofed override",
			componentName: "gpu-operator",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "gpu-operator"},
					{Name: "network-operator"},
				},
				Criteria: &recipe.Criteria{
					Service: recipe.CriteriaServiceAKS,
				},
			},
			bundlerConfig: config.NewConfig(
				config.WithValueOverrides(map[string]map[string]string{
					"networkoperator": {"enabled": "false"},
				}),
			),
			conditions:     map[string][]string{"service": {"aks"}},
			wantWarnings:   1,
			wantErrors:     0,
			wantWarningMsg: "network-operator is disabled but driver.rdma.useHostMofed is not set to false",
		},
		{
			name:          "network-operator disabled with useHostMofed=false",
			componentName: "gpu-operator",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "gpu-operator"},
					{Name: "network-operator"},
				},
				Criteria: &recipe.Criteria{
					Service: recipe.CriteriaServiceAKS,
				},
			},
			bundlerConfig: config.NewConfig(
				config.WithValueOverrides(map[string]map[string]string{
					"networkoperator": {"enabled": "false"},
					"gpuoperator":     {"driver.rdma.useHostMofed": "false"},
				}),
			),
			conditions:   map[string][]string{"service": {"aks"}},
			wantWarnings: 0,
			wantErrors:   0,
		},
		{
			name:          "network-operator enabled (default)",
			componentName: "gpu-operator",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "gpu-operator"},
					{Name: "network-operator"},
				},
				Criteria: &recipe.Criteria{
					Service: recipe.CriteriaServiceAKS,
				},
			},
			bundlerConfig: config.NewConfig(),
			conditions:    map[string][]string{"service": {"aks"}},
			wantWarnings:  0,
			wantErrors:    0,
		},
		{
			name:          "non-AKS service",
			componentName: "gpu-operator",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "gpu-operator"},
				},
				Criteria: &recipe.Criteria{
					Service: recipe.CriteriaServiceEKS,
				},
			},
			bundlerConfig: config.NewConfig(
				config.WithValueOverrides(map[string]map[string]string{
					"networkoperator": {"enabled": "false"},
				}),
			),
			conditions:   map[string][]string{"service": {"aks"}},
			wantWarnings: 0,
			wantErrors:   0,
		},
		{
			name:          "nil config",
			componentName: "gpu-operator",
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "gpu-operator"},
				},
				Criteria: &recipe.Criteria{
					Service: recipe.CriteriaServiceAKS,
				},
			},
			bundlerConfig: nil,
			conditions:    map[string][]string{"service": {"aks"}},
			wantWarnings:  0,
			wantErrors:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			warnings, errors := CheckHostMofedWithoutNetworkOperator(ctx, tt.componentName, tt.recipeResult, tt.bundlerConfig, tt.conditions)

			if len(warnings) != tt.wantWarnings {
				t.Errorf("CheckHostMofedWithoutNetworkOperator() warnings = %d, want %d", len(warnings), tt.wantWarnings)
			}
			if len(errors) != tt.wantErrors {
				t.Errorf("CheckHostMofedWithoutNetworkOperator() errors = %d, want %d", len(errors), tt.wantErrors)
			}

			if tt.wantWarningMsg != "" && len(warnings) > 0 {
				found := false
				for _, w := range warnings {
					if strings.Contains(w, tt.wantWarningMsg) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("CheckHostMofedWithoutNetworkOperator() warnings = %v, want to contain %q", warnings, tt.wantWarningMsg)
				}
			}
		})
	}
}

func TestCheckConditions(t *testing.T) {
	tests := []struct {
		name         string
		recipeResult *recipe.RecipeResult
		conditions   map[string][]string
		want         bool
	}{
		{
			name: "no conditions",
			recipeResult: &recipe.RecipeResult{
				Criteria: &recipe.Criteria{
					Intent: recipe.CriteriaIntentTraining,
				},
			},
			conditions: nil,
			want:       true,
		},
		{
			name: "empty conditions",
			recipeResult: &recipe.RecipeResult{
				Criteria: &recipe.Criteria{
					Intent: recipe.CriteriaIntentTraining,
				},
			},
			conditions: map[string][]string{},
			want:       true,
		},
		{
			name: "intent matches",
			recipeResult: &recipe.RecipeResult{
				Criteria: &recipe.Criteria{
					Intent: recipe.CriteriaIntentTraining,
				},
			},
			conditions: map[string][]string{"intent": {"training"}},
			want:       true,
		},
		{
			name: "intent does not match",
			recipeResult: &recipe.RecipeResult{
				Criteria: &recipe.Criteria{
					Intent: recipe.CriteriaIntentInference,
				},
			},
			conditions: map[string][]string{"intent": {"training"}},
			want:       false,
		},
		{
			name: "intent in array matches",
			recipeResult: &recipe.RecipeResult{
				Criteria: &recipe.Criteria{
					Intent: recipe.CriteriaIntentTraining,
				},
			},
			conditions: map[string][]string{"intent": {"training", "inference"}},
			want:       true,
		},
		{
			name: "intent in array does not match",
			recipeResult: &recipe.RecipeResult{
				Criteria: &recipe.Criteria{
					Intent: "other",
				},
			},
			conditions: map[string][]string{"intent": {"training", "inference"}},
			want:       false,
		},
		{
			name: "nil criteria",
			recipeResult: &recipe.RecipeResult{
				Criteria: nil,
			},
			conditions: map[string][]string{"intent": {"training"}},
			want:       false,
		},
		{
			name: "multiple conditions all match",
			recipeResult: &recipe.RecipeResult{
				Criteria: &recipe.Criteria{
					Intent:      recipe.CriteriaIntentTraining,
					Service:     recipe.CriteriaServiceEKS,
					Accelerator: recipe.CriteriaAcceleratorH100,
				},
			},
			conditions: map[string][]string{
				"intent":      {"training"},
				"service":     {"eks"},
				"accelerator": {"h100"},
			},
			want: true,
		},
		{
			name: "multiple conditions one does not match",
			recipeResult: &recipe.RecipeResult{
				Criteria: &recipe.Criteria{
					Intent:      recipe.CriteriaIntentTraining,
					Service:     recipe.CriteriaServiceEKS,
					Accelerator: recipe.CriteriaAcceleratorH100,
				},
			},
			conditions: map[string][]string{
				"intent":      {"training"},
				"service":     {"gke"},
				"accelerator": {"h100"},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkConditions(tt.recipeResult, tt.conditions)
			if got != tt.want {
				t.Errorf("checkConditions() = %v, want %v", got, tt.want)
			}
		})
	}
}
