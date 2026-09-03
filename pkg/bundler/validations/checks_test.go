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
	stderrors "errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
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

// TestCheckDriverOwnershipCoherence covers the bundle-time
// driver-ownership gate on FINAL effective values: Rule 1 (a recipe whose
// snapshot observed no NVIDIA driver on the sampled GPU node —
// metadata.gpuDriverState=absent — must not bundle with the
// preinstalled-driver assumption) and Rule 2 (the
// nvidia-dra-driver-gpu.nvidiaDriverRoot ↔ gpu-operator
// hostPaths.driverInstallDir lockstep, metadata-independent so it also
// catches legacy pre-flip recipes), plus the rule-independent rejection
// of an explicit driverInstallDir of "/" (issue #1106) and the
// fail-closed hard error when effective values cannot be resolved. See
// pkg/client/v1's gpu_driver_state.go for the recording side.
func TestCheckDriverOwnershipCoherence(t *testing.T) {
	gpuOpRef := func(overrides map[string]any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: "gpu-operator", Overrides: overrides}
	}
	draRef := func(overrides map[string]any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: "nvidia-dra-driver-gpu", Overrides: overrides}
	}
	driverOff := func() map[string]any {
		return map[string]any{"driver": map[string]any{"enabled": false}}
	}
	driverOn := func() map[string]any {
		return map[string]any{"driver": map[string]any{"enabled": true}}
	}
	rootAt := func(root string) map[string]any {
		return map[string]any{"nvidiaDriverRoot": root}
	}
	result := func(state string, service recipe.CriteriaServiceType, refs ...recipe.ComponentRef) *recipe.RecipeResult {
		r := &recipe.RecipeResult{
			ComponentRefs: refs,
			Criteria:      &recipe.Criteria{Service: service},
		}
		r.Metadata.GPUDriverState = state
		return r
	}
	resultOS := func(state string, service recipe.CriteriaServiceType, os recipe.CriteriaOSType, refs ...recipe.ComponentRef) *recipe.RecipeResult {
		r := result(state, service, refs...)
		r.Criteria.OS = os
		return r
	}
	resultProfiled := func(state string, service recipe.CriteriaServiceType, refs ...recipe.ComponentRef) *recipe.RecipeResult {
		r := result(state, service, refs...)
		r.Metadata.SelectedProfile = &recipe.SelectedProfile{Name: "gpuStack", Value: "azure-managed"}
		return r
	}
	gcpInstallerRef := func(gate any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: "gcp-driver-installer",
			Overrides: map[string]any{"installer": map[string]any{"enabled": gate}}}
	}
	// A GKE+COS bundle-installer recipe whose snapshot (correctly) saw no
	// driver: pools are created gpu-driver-version=disabled, the bundle's
	// gcp-driver-installer supplies the driver.
	resultBundleInstaller := func(gate any) *recipe.RecipeResult {
		r := resultOS(recipe.GPUDriverStateAbsent, recipe.CriteriaServiceGKE,
			recipe.CriteriaOSCOS, gpuOpRef(driverOff()), gcpInstallerRef(gate))
		r.Metadata.SelectedProfile = &recipe.SelectedProfile{Name: "gpuStack", Value: "bundle-installer"}
		return r
	}
	aks := recipe.CriteriaServiceAKS

	tests := []struct {
		name            string
		recipeResult    *recipe.RecipeResult
		bundlerConfig   *config.Config
		conditions      map[string][]string
		wantMsgs        int
		wantContains    []string
		wantErrs        int
		wantErrContains []string
	}{
		{
			name:         "nil recipe result → skipped",
			recipeResult: nil,
		},
		{
			name:         "no gpu-operator ref → skipped",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks, draRef(rootAt("/"))),
		},
		{
			name:         "conditions mismatch → skipped",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks, gpuOpRef(driverOff())),
			conditions:   map[string][]string{"service": {"eks"}},
		},
		{
			name: "gpu-operator disabled by recipe → skipped",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks, gpuOpRef(map[string]any{
				"enabled": false,
				"driver":  map[string]any{"enabled": false},
			})),
		},
		{
			name:         "gpu-operator disabled via --set alias → skipped",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks, gpuOpRef(driverOff())),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"gpuoperator": {"enabled": "false"},
			})),
		},
		{
			name:         "no recorded state, no DRA ref → not gated by Rule 1",
			recipeResult: result("", aks, gpuOpRef(driverOff())),
		},
		{
			name:         "preinstalled recorded state → not gated by Rule 1",
			recipeResult: result(recipe.GPUDriverStatePreinstalled, aks, gpuOpRef(driverOff())),
		},
		{
			// Legacy pre-profile artifact: no selectedProfile, so the
			// ownership lock does not apply and the remedy must keep
			// naming the four-flag tuple.
			name:         "Rule 1: absent + driver.enabled=false → blocked (nil bundler config)",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks, gpuOpRef(driverOff())),
			wantMsgs:     1,
			wantContains: []string{"driverless", "--gpu-driver none", "--set gpuoperator:driver.enabled=true"},
		},
		{
			// Profiled artifact: the tuple is lock-rejected, so the remedy
			// names the recapture + --profile path instead.
			name:         "Rule 1: absent + profiled recipe → profile-aware remedy",
			recipeResult: resultProfiled(recipe.GPUDriverStateAbsent, aks, gpuOpRef(driverOff())),
			wantMsgs:     1,
			wantContains: []string{"driverless", "--profile gpuStack=operator-managed", "recapture the snapshot"},
		},
		{
			name: "Rule 1: absent + toolkit.enabled=false alone → blocked",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks, gpuOpRef(map[string]any{
				"toolkit": map[string]any{"enabled": false},
			})),
			wantMsgs:     1,
			wantContains: []string{"driverless"},
		},
		{
			name: "Rule 1: absent on GKE, unknown OS → combined COS + non-COS wording",
			recipeResult: result(recipe.GPUDriverStateAbsent, recipe.CriteriaServiceGKE,
				gpuOpRef(driverOff())),
			wantMsgs: 1,
			wantContains: []string{
				"gpu-driver-version",
				"On GKE COS node images the GPU Operator cannot install the driver",
				"GKE Ubuntu node images",
				"--set gpuoperator:driver.enabled=true",
			},
		},
		{
			name: "Rule 1: absent on GKE+COS → COS-only wording, no override tuple",
			recipeResult: resultOS(recipe.GPUDriverStateAbsent, recipe.CriteriaServiceGKE,
				recipe.CriteriaOSCOS, gpuOpRef(driverOff())),
			wantMsgs: 1,
			wantContains: []string{
				"On GKE COS node images the GPU Operator cannot install the driver",
				"gpu-driver-version=default",
				"--profile gpuStack=bundle-installer",
				"gke-no-default-nvidia-gpu-device-plugin=true",
				"gpu-driver-version=disabled",
				"gcp-driver-installer",
				"do not deploy a standalone DaemonSet alongside it",
			},
		},
		{
			// The bundle-installer regression (#2360 review): a correctly
			// provisioned pool has no driver, so the snapshot records
			// absent and driver.enabled=false — but the bundle's own
			// gcp-driver-installer supplies the driver, so Rule 1 must
			// stand down and let the pool generate its own bundle.
			name:         "Rule 1: absent + enabled gcp-driver-installer → bundle supplies driver, no message",
			recipeResult: resultBundleInstaller(true),
		},
		{
			// Template parity: the manifest gate is toString == "true",
			// so a string \"true\" renders the DaemonSet and counts as a
			// driver producer too.
			name:         "Rule 1: absent + string-true installer gate → suppressed (template parity)",
			recipeResult: resultBundleInstaller("true"),
		},
		{
			name:         "Rule 1: absent + disabled installer gate → still blocked",
			recipeResult: resultBundleInstaller(false),
			wantMsgs:     1,
			wantContains: []string{"driverless", "On GKE COS node images"},
		},
		{
			// Unrecognized gate types must not disarm the driverless gate.
			name:         "Rule 1: absent + non-boolean installer gate → still blocked (fail closed)",
			recipeResult: resultBundleInstaller(map[string]any{"oops": true}),
			wantMsgs:     1,
			wantContains: []string{"driverless"},
		},
		{
			// The suppression keys off the EFFECTIVE gate, not the profile
			// name: a --set that turns the installer off re-arms Rule 1.
			name:         "Rule 1: absent + installer gate turned off via --set → still blocked",
			recipeResult: resultBundleInstaller(true),
			bundlerConfig: config.NewConfig(
				config.WithValueOverrides(map[string]map[string]string{
					"gcpdriverinstaller": {"installer.enabled": "false"},
				}),
			),
			wantMsgs:     1,
			wantContains: []string{"driverless"},
		},
		{
			// TC3: the fail-closed propagation — gpu-operator resolves but
			// the installer's effective values do not (a --set that
			// descends through the scalar gate). Must surface as a hard
			// error, never degrade to the driverless remediation.
			name:         "Rule 1: absent + installer values unresolvable → hard error, no driverless remedy",
			recipeResult: resultBundleInstaller(true),
			bundlerConfig: config.NewConfig(
				config.WithValueOverrides(map[string]map[string]string{
					"gcpdriverinstaller": {"installer.enabled.bogus": "true"},
				}),
			),
			wantErrs:        1,
			wantErrContains: []string{"bundle-supplied driver detection"},
		},
		{
			// installer present as a scalar rather than a map: not a
			// producer — Rule 1 stays armed.
			name: "Rule 1: absent + installer as non-map scalar → still blocked",
			recipeResult: func() *recipe.RecipeResult {
				r := resultBundleInstaller(true)
				for i := range r.ComponentRefs {
					if r.ComponentRefs[i].Name == "gcp-driver-installer" {
						r.ComponentRefs[i].Overrides = map[string]any{"installer": "on"}
					}
				}
				return r
			}(),
			wantMsgs:     1,
			wantContains: []string{"driverless"},
		},
		{
			name: "Rule 1: absent on GKE+ubuntu → operator-managed remedy, no COS instruction",
			recipeResult: resultOS(recipe.GPUDriverStateAbsent, recipe.CriteriaServiceGKE,
				recipe.CriteriaOSUbuntu, gpuOpRef(driverOff())),
			wantMsgs: 1,
			wantContains: []string{
				"On GKE Ubuntu node images the GPU Operator can manage",
				"--set gpuoperator:driver.enabled=true",
				"--set gpuoperator:hostPaths.driverInstallDir=/run/nvidia/driver",
			},
		},
		{
			name: "Rule 1: absent on GKE+ubuntu Google-installer profile (both roots /home/kubernetes/bin/nvidia) → remedy moves both roots",
			recipeResult: resultOS(recipe.GPUDriverStateAbsent, recipe.CriteriaServiceGKE,
				recipe.CriteriaOSUbuntu,
				gpuOpRef(map[string]any{
					"driver":    map[string]any{"enabled": false},
					"hostPaths": map[string]any{"driverInstallDir": "/home/kubernetes/bin/nvidia"},
				}),
				draRef(rootAt("/home/kubernetes/bin/nvidia"))),
			wantMsgs: 1,
			wantContains: []string{
				"--set gpuoperator:hostPaths.driverInstallDir=/run/nvidia/driver",
				"--set dradriver:nvidiaDriverRoot=/run/nvidia/driver",
			},
		},
		{
			name: "invalid driverInstallDir + mismatching DRA root → single rejection, no guessed-value lockstep remedy",
			recipeResult: result("", aks,
				gpuOpRef(map[string]any{
					"driver":    map[string]any{"enabled": true},
					"hostPaths": map[string]any{"driverInstallDir": true},
				}),
				draRef(rootAt("/home/kubernetes/bin/nvidia"))),
			wantMsgs:     1,
			wantContains: []string{"hostPaths.driverInstallDir"},
		},
		{
			name: "GKE Google-installer profile + full five-flag tuple applied → passes both rules",
			recipeResult: resultOS("", recipe.CriteriaServiceGKE,
				recipe.CriteriaOSUbuntu,
				gpuOpRef(map[string]any{
					"driver":    map[string]any{"enabled": true},
					"toolkit":   map[string]any{"enabled": true},
					"hostPaths": map[string]any{"driverInstallDir": "/run/nvidia/driver"},
				}),
				draRef(rootAt("/run/nvidia/driver"))),
			wantMsgs: 0,
		},
		{
			name: "Rule 1: absent on GKE+rhel (no such GKE node image) → combined wording, no unsupported recommendation",
			recipeResult: resultOS(recipe.GPUDriverStateAbsent, recipe.CriteriaServiceGKE,
				recipe.CriteriaOSRHEL, gpuOpRef(driverOff())),
			wantMsgs: 1,
			wantContains: []string{
				"gpu-driver-version",
				"On GKE Ubuntu node images the GPU Operator can manage",
			},
		},
		{
			name: "Rule 1: absent + full effective flip (--set tuple) → passes",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks,
				gpuOpRef(map[string]any{
					"driver":  map[string]any{"enabled": false},
					"toolkit": map[string]any{"enabled": false},
				}),
				draRef(rootAt("/"))),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"gpuoperator": {"driver.enabled": "true", "toolkit.enabled": "true"},
				"dradriver":   {"nvidiaDriverRoot": "/run/nvidia/driver"},
			})),
		},
		{
			// Finding-1 regression test: flipping driver.enabled alone must
			// not clear the gate when the DRA root is left at the
			// preinstalled-profile value — Rule 2 catches the partial flip.
			name: "Rule 2: partial flip (--set driver.enabled=true alone, DRA root=/) → blocked",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks,
				gpuOpRef(driverOff()), draRef(rootAt("/"))),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"gpuoperator": {"driver.enabled": "true"},
			})),
			wantMsgs:     1,
			wantContains: []string{"CDI spec generation", "--set dradriver:nvidiaDriverRoot=/run/nvidia/driver"},
		},
		{
			name:         "override under canonical name gpu-operator honored",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks, gpuOpRef(driverOff())),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"gpu-operator": {"driver.enabled": "true"},
			})),
		},
		{
			name:         "override under registry alias gpuoperator honored",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks, gpuOpRef(driverOff())),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"gpuoperator": {"driver.enabled": "true"},
			})),
		},
		{
			name: "typed --set-json override honored (DRA root retarget)",
			recipeResult: result("", aks,
				gpuOpRef(driverOn()), draRef(rootAt("/"))),
			bundlerConfig: config.NewConfig(config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{Component: "dradriver", Path: "nvidiaDriverRoot", Value: "/run/nvidia/driver"},
			})),
		},
		{
			// The legacy pre-flip signature: a recipe whose valuesFile now
			// resolves driver.enabled=false while its baked DRA override
			// still points at the operator container root — no recorded
			// state, caught by Rule 2 on effective values alone.
			name: "Rule 2: legacy recipe (no state, driver off, DRA root=/run/nvidia/driver) → blocked",
			recipeResult: result("", aks,
				gpuOpRef(driverOff()), draRef(rootAt("/run/nvidia/driver"))),
			wantMsgs:     1,
			wantContains: []string{"nothing", "regenerate the recipe", "--set gpuoperator:driver.enabled=true"},
		},
		{
			name: "Rule 2: driver off + DRA root=/ (preinstalled profile) → passes",
			recipeResult: result("", aks,
				gpuOpRef(driverOff()), draRef(rootAt("/"))),
		},
		{
			name: "Rule 2: operator-managed with matching root → passes",
			recipeResult: result("", aks,
				gpuOpRef(driverOn()), draRef(rootAt("/run/nvidia/driver"))),
		},
		{
			name: "Rule 2: operator-managed, DRA root undeclared → blocked",
			recipeResult: result("", aks,
				gpuOpRef(driverOn()), draRef(nil)),
			wantMsgs:     1,
			wantContains: []string{"not declared", "--set dradriver:nvidiaDriverRoot=/run/nvidia/driver"},
		},
		{
			name: "Rule 2: custom installDir mismatch → blocked",
			recipeResult: result("", aks,
				gpuOpRef(map[string]any{
					"driver":    map[string]any{"enabled": true},
					"hostPaths": map[string]any{"driverInstallDir": "/home/kubernetes/bin/nvidia"},
				}),
				draRef(rootAt("/run/nvidia/driver"))),
			wantMsgs:     1,
			wantContains: []string{"/home/kubernetes/bin/nvidia", "CDI spec generation"},
		},
		{
			name: "Rule 2: custom installDir with matching root → passes",
			recipeResult: result("", aks,
				gpuOpRef(map[string]any{
					"driver":    map[string]any{"enabled": true},
					"hostPaths": map[string]any{"driverInstallDir": "/home/kubernetes/bin/nvidia"},
				}),
				draRef(rootAt("/home/kubernetes/bin/nvidia"))),
		},
		{
			name: "Rule 2: DRA disabled via --set → lockstep not enforced",
			recipeResult: result("", aks,
				gpuOpRef(driverOn()), draRef(rootAt("/"))),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"dradriver": {"enabled": "false"},
			})),
		},
		{
			name: "Rules 1+2 both violated → two blocking messages",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks,
				gpuOpRef(driverOff()), draRef(rootAt("/run/nvidia/driver"))),
			wantMsgs:     2,
			wantContains: []string{"driverless", "regenerate the recipe"},
		},
		{
			// Fail closed within validation itself. Normal bundle value
			// extraction also rejects this input, but direct validation
			// callers must not treat an unverifiable recipe as a skip.
			name: "unresolvable gpu-operator values → blocking error, not skip",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks, recipe.ComponentRef{
				Name:       "gpu-operator",
				ValuesFile: "components/gpu-operator/does-not-exist.yaml",
			}),
			wantErrs:        1,
			wantErrContains: []string{"driver-ownership coherence", "gpu-operator"},
		},
		{
			name: "unresolvable DRA values → blocking error, Rule 2 not silently skipped",
			recipeResult: result("", aks,
				gpuOpRef(driverOn()),
				recipe.ComponentRef{
					Name:       "nvidia-dra-driver-gpu",
					ValuesFile: "components/nvidia-dra-driver-gpu/does-not-exist.yaml",
				}),
			wantErrs:        1,
			wantErrContains: []string{"driver-ownership coherence", "nvidia-dra-driver-gpu"},
		},
		{
			// Fail closed on override reapplication too. This direct
			// validation call must surface an override set whose effective
			// values it cannot reconstruct. (driver.enabled is a boolean, so
			// the deeper path fails traversal deterministically.)
			name:         "--set override that cannot be reapplied → blocking error, not skip",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks, gpuOpRef(driverOff())),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"gpuoperator": {"driver.enabled.nested": "1"},
			})),
			wantErrs:        1,
			wantErrContains: []string{"driver-ownership coherence", "failed to apply --set overrides"},
		},
		{
			name:         "--set-json override that cannot be reapplied → blocking error, not skip",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks, gpuOpRef(driverOff())),
			bundlerConfig: config.NewConfig(config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{Component: "gpuoperator", Path: "driver.enabled.nested", Value: true},
			})),
			wantErrs:        1,
			wantErrContains: []string{"driver-ownership coherence", "failed to apply --set-json/--set-file overrides"},
		},
		{
			name: "DRA --set override that cannot be reapplied → blocking error, not skip",
			recipeResult: result("", aks,
				gpuOpRef(driverOn()), draRef(rootAt("/run/nvidia/driver"))),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"dradriver": {"nvidiaDriverRoot.nested": "1"},
			})),
			wantErrs:        1,
			wantErrContains: []string{"driver-ownership coherence", "nvidia-dra-driver-gpu"},
		},
		{
			// Rule 1 keys on exact equality with "absent"; an unrecognized
			// spelling in a loaded/hand-edited recipe must be rejected, not
			// silently degraded to the empty=unknown disarm state.
			name:         "unrecognized recorded state → blocked, Rule 1 not silently disarmed",
			recipeResult: result("Absent", aks, gpuOpRef(driverOff())),
			wantMsgs:     1,
			wantContains: []string{"metadata.gpuDriverState=\"Absent\" is not a recognized value"},
		},
		{
			name: "unrecognized recorded state + legacy DRA signature → both reported",
			recipeResult: result("ABSENT", aks,
				gpuOpRef(driverOff()), draRef(rootAt("/run/nvidia/driver"))),
			wantMsgs:     2,
			wantContains: []string{"not a recognized value", "regenerate the recipe"},
		},
		{
			name: "unrecognized recorded state fires even when values resolution fails",
			recipeResult: result("preinstalled ", aks, recipe.ComponentRef{
				Name:       "gpu-operator",
				ValuesFile: "components/gpu-operator/does-not-exist.yaml",
			}),
			wantMsgs:        1,
			wantContains:    []string{"not a recognized value"},
			wantErrs:        1,
			wantErrContains: []string{"failed to resolve its effective values"},
		},
		{
			// Canonical name beats registry alias on the enabled toggle,
			// mirroring the bundler's getSetEnabledOverride merge order.
			name:         "--set canonical enabled=true beats alias enabled=false → check runs",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks, gpuOpRef(driverOff())),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"gpu-operator": {"enabled": "true"},
				"gpuoperator":  {"enabled": "false"},
			})),
			wantMsgs:     1,
			wantContains: []string{"driverless"},
		},
		{
			// strconv.ParseBool spellings disable exactly like the bundler.
			name:         "--set enabled=0 disables like the bundler → skipped",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks, gpuOpRef(driverOff())),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"gpuoperator": {"enabled": "0"},
			})),
		},
		{
			name: "Rule 2: trailing-slash DRA root equals operator install dir → passes",
			recipeResult: result("", aks,
				gpuOpRef(driverOn()), draRef(rootAt("/run/nvidia/driver/"))),
		},
		{
			name: "Rule 2: trailing-slash legacy root still matches legacy signature → blocked",
			recipeResult: result("", aks,
				gpuOpRef(driverOff()), draRef(rootAt("/run/nvidia/driver/"))),
			wantMsgs:     1,
			wantContains: []string{"regenerate the recipe"},
		},
		{
			name: "Rule 2: trailing-slash installDir equals DRA root → passes",
			recipeResult: result("", aks,
				gpuOpRef(map[string]any{
					"driver":    map[string]any{"enabled": true},
					"hostPaths": map[string]any{"driverInstallDir": "/home/kubernetes/bin/nvidia/"},
				}),
				draRef(rootAt("/home/kubernetes/bin/nvidia"))),
		},
		{
			// Typed-toggle rejection: the chart interpolates driver.enabled
			// unquoted into the ClusterPolicy, so the STRING "false" deploys
			// as boolean false — it must be rejected, not defaulted to
			// "operator manages the driver" (which would silently clear
			// Rule 1 on a driverless cluster).
			name:         "--set-json driver.enabled=\"false\" (string) → typed rejection, not silent pass",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks, gpuOpRef(driverOn())),
			bundlerConfig: config.NewConfig(config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{Component: "gpuoperator", Path: "driver.enabled", Value: "false"},
			})),
			wantMsgs:     1,
			wantContains: []string{"driver.enabled=false (string) is not a boolean", "no quotes"},
		},
		{
			// ConvertMapValue coerces only the exact spellings "true"/"false";
			// "False" survives as a string, which YAML re-types to boolean
			// false at install time.
			name:         "--set driver.enabled=False (uncoerced spelling) → typed rejection",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks, gpuOpRef(driverOn())),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"gpuoperator": {"driver.enabled": "False"},
			})),
			wantMsgs:     1,
			wantContains: []string{"driver.enabled=False (string) is not a boolean"},
		},
		{
			name:         "--set driver.enabled=1 (numeric) → typed rejection",
			recipeResult: result("", aks, gpuOpRef(driverOn())),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"gpuoperator": {"driver.enabled": "1"},
			})),
			wantMsgs:     1,
			wantContains: []string{"driver.enabled=1 (int64) is not a boolean"},
		},
		{
			name:         "--set-json driver.enabled=null → typed rejection",
			recipeResult: result("", aks, gpuOpRef(driverOn())),
			bundlerConfig: config.NewConfig(config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{Component: "gpuoperator", Path: "driver.enabled", Value: nil},
			})),
			wantMsgs:     1,
			wantContains: []string{"driver.enabled is null, not a boolean"},
		},
		{
			name:         "toolkit.enabled string \"false\" → typed rejection",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks, gpuOpRef(driverOn())),
			bundlerConfig: config.NewConfig(config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{Component: "gpuoperator", Path: "toolkit.enabled", Value: "false"},
			})),
			wantMsgs:     1,
			wantContains: []string{"toolkit.enabled=false (string) is not a boolean"},
		},
		{
			// The recipe merge itself (hand-authored values, --set-file
			// content) can carry the string, with no bundle-time override
			// in play at all.
			name: "recipe-side string driver.enabled → typed rejection (nil bundler config)",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks, gpuOpRef(map[string]any{
				"driver": map[string]any{"enabled": "false"},
			})),
			wantMsgs:     1,
			wantContains: []string{"driver.enabled=false (string) is not a boolean"},
		},
		{
			name:         "driver section replaced by a scalar → typed rejection",
			recipeResult: result("", aks, gpuOpRef(driverOn())),
			bundlerConfig: config.NewConfig(config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{Component: "gpuoperator", Path: "driver", Value: "false"},
			})),
			wantMsgs:     1,
			wantContains: []string{"driver=false (string) is not a map"},
		},
		{
			// Both toggles invalid → both rejections surface in one pass,
			// and the ownership rules are suppressed (no third message
			// derived from guessed defaults).
			name: "both toggles invalid → two rejections, rules suppressed",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks, gpuOpRef(map[string]any{
				"driver":  map[string]any{"enabled": "false"},
				"toolkit": map[string]any{"enabled": "no"},
			}), draRef(rootAt("/run/nvidia/driver"))),
			wantMsgs:     2,
			wantContains: []string{"driver.enabled=false (string)", "toolkit.enabled=no (string)"},
		},
		{
			// A null SECTION is NOT absent: Helm's null-coalescing deletes
			// the key together with its chart defaults, so .Values.driver
			// is nil at render time and the chart's unconditional field
			// accesses (_helpers.tpl .Values.driver.manager.repository,
			// v26.7.0) fail at install. Reject rather than default. The
			// reachable vector is a top-level --set-json null: the typed
			// merge assigns it verbatim (mergeTypedValueByPath), while the
			// recipe-side overlay merge drops nil-valued keys before the
			// check ever sees them.
			name:         "--set-json driver=null → rejected, ownership unverifiable",
			recipeResult: result(recipe.GPUDriverStateAbsent, aks, gpuOpRef(driverOn())),
			bundlerConfig: config.NewConfig(config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{Component: "gpuoperator", Path: "driver", Value: nil},
			})),
			wantMsgs:     1,
			wantContains: []string{"driver is explicitly null"},
		},
		{
			// Same rejection for an explicitly null toolkit section.
			name:         "--set-json toolkit=null → rejected, ownership unverifiable",
			recipeResult: result("", aks, gpuOpRef(driverOn())),
			bundlerConfig: config.NewConfig(config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{Component: "gpuoperator", Path: "toolkit", Value: nil},
			})),
			wantMsgs:     1,
			wantContains: []string{"toolkit is explicitly null"},
		},
		{
			// hostPaths: null is the same hazard as a null driver/toolkit
			// section: Helm null-coalescing deletes the chart defaults and
			// clusterpolicy.yaml's unconditional .Values.hostPaths.rootFS
			// access fails at install (v26.7.0).
			name:         "--set-json hostPaths=null → rejected",
			recipeResult: result("", aks, gpuOpRef(driverOn())),
			bundlerConfig: config.NewConfig(config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{Component: "gpuoperator", Path: "hostPaths", Value: nil},
			})),
			wantMsgs:     1,
			wantContains: []string{"hostPaths is explicitly null"},
		},
		{
			name:         "--set-json hostPaths=\"str\" (non-map) → rejected",
			recipeResult: result("", aks, gpuOpRef(driverOn())),
			bundlerConfig: config.NewConfig(config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{Component: "gpuoperator", Path: "hostPaths", Value: "str"},
			})),
			wantMsgs:     1,
			wantContains: []string{"is not a map, so hostPaths.driverInstallDir"},
		},
		{
			// A dynamic ownership path moves to the operator-editable
			// cluster-values.yaml at install time, past this gate.
			name:         "--dynamic gpuoperator:driver.enabled → rejected",
			recipeResult: result("", aks, gpuOpRef(driverOn())),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"gpuoperator": {"driver.enabled"},
			})),
			wantMsgs:     1,
			wantContains: []string{"--dynamic gpuoperator:driver.enabled targets the driver-ownership path"},
		},
		{
			// Parent-path declarations cover their children.
			name:         "--dynamic gpuoperator:driver (parent path) → rejected",
			recipeResult: result("", aks, gpuOpRef(driverOn())),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"gpuoperator": {"driver"},
			})),
			wantMsgs:     1,
			wantContains: []string{"targets the driver-ownership path driver.enabled"},
		},
		{
			name: "--dynamic dradriver:nvidiaDriverRoot → rejected",
			recipeResult: result("", aks,
				gpuOpRef(driverOn()), draRef(rootAt("/run/nvidia/driver"))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"dradriver": {"nvidiaDriverRoot"},
			})),
			wantMsgs:     1,
			wantContains: []string{"--dynamic dradriver:nvidiaDriverRoot targets the driver-ownership path"},
		},
		{
			// Benign dynamic paths (the documented toleration use case,
			// #1371) stay allowed.
			name:         "--dynamic gpuoperator:daemonsets.tolerations → allowed",
			recipeResult: result("", aks, gpuOpRef(driverOn())),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"gpuoperator": {"daemonsets.tolerations"},
			})),
		},
		{
			// A present-null DRA root previously collapsed into "not
			// declared" and, with driver.enabled=false, fired NO Rule 2
			// branch — while the emitted null fails the DRA chart's
			// trimSuffix/dir template pipeline at install.
			name: "--set-json dradriver:nvidiaDriverRoot=null → rejected",
			recipeResult: result("", aks,
				gpuOpRef(driverOff()), draRef(rootAt("/"))),
			bundlerConfig: config.NewConfig(config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{Component: "dradriver", Path: "nvidiaDriverRoot", Value: nil},
			})),
			wantMsgs:     1,
			wantContains: []string{"is not a string"},
		},
		{
			// Unlike gpu-operator's driverInstallDir (where "" is
			// default-equivalent upstream), the DRA chart renders ""
			// verbatim into host paths.
			name: "dradriver nvidiaDriverRoot=\"\" (explicit empty) → rejected",
			recipeResult: result("", aks,
				gpuOpRef(driverOff()), draRef(rootAt(""))),
			wantMsgs:     1,
			wantContains: []string{"empty string"},
		},
		{
			name: "--set-json dradriver:nvidiaDriverRoot=true (bool) → rejected",
			recipeResult: result("", aks,
				gpuOpRef(driverOn()), draRef(rootAt("/run/nvidia/driver"))),
			bundlerConfig: config.NewConfig(config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{Component: "dradriver", Path: "nvidiaDriverRoot", Value: true},
			})),
			wantMsgs:     1,
			wantContains: []string{"is not a string"},
		},
		{
			// A non-string driverInstallDir leaf previously fell back
			// silently to the default while the emitted value violates
			// the ClusterPolicy CRD's string typing.
			name:         "--set-json gpuoperator:hostPaths.driverInstallDir=true → rejected",
			recipeResult: result("", aks, gpuOpRef(driverOn())),
			bundlerConfig: config.NewConfig(config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{Component: "gpuoperator", Path: "hostPaths.driverInstallDir", Value: true},
			})),
			wantMsgs:     1,
			wantContains: []string{"driverInstallDir=true (bool) is not a string"},
		},
		{
			name:         "--set-json gpuoperator:hostPaths.driverInstallDir=null → rejected",
			recipeResult: result("", aks, gpuOpRef(driverOn())),
			bundlerConfig: config.NewConfig(config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{Component: "gpuoperator", Path: "hostPaths.driverInstallDir", Value: nil},
			})),
			wantMsgs:     1,
			wantContains: []string{"is not a string"},
		},
		{
			// A relative DRA root with driver.enabled=false previously fired
			// NO Rule 2 branch (it compares unequal to /run/nvidia/driver),
			// so the missing-leading-slash typo of the operator path slipped
			// past the legacy-signature check while rendering unmountable
			// relative host paths.
			name: "relative DRA root with driver off → rejected, not silently passed",
			recipeResult: result("", aks,
				gpuOpRef(driverOff()), draRef(rootAt("run/nvidia/driver"))),
			wantMsgs:     1,
			wantContains: []string{`nvidiaDriverRoot="run/nvidia/driver" is not an absolute path`},
		},
		{
			name: "relative driverInstallDir without DRA → rejected",
			recipeResult: result("", aks,
				gpuOpRef(map[string]any{
					"driver":    map[string]any{"enabled": true},
					"hostPaths": map[string]any{"driverInstallDir": "run/nvidia/driver"},
				})),
			wantMsgs:     1,
			wantContains: []string{`hostPaths.driverInstallDir="run/nvidia/driver" is not an absolute path`},
		},
		{
			// Matching relative pair: equality must not clear the gate —
			// both sides are rejected independently.
			name: "matching relative installDir and DRA root → both rejected",
			recipeResult: result("", aks,
				gpuOpRef(map[string]any{
					"driver":    map[string]any{"enabled": true},
					"hostPaths": map[string]any{"driverInstallDir": "run/nvidia/driver"},
				}),
				draRef(rootAt("run/nvidia/driver"))),
			wantMsgs: 2,
			wantContains: []string{
				`hostPaths.driverInstallDir="run/nvidia/driver" is not an absolute path`,
				`nvidiaDriverRoot="run/nvidia/driver" is not an absolute path`,
			},
		},
		{
			// Non-normalized absolute spellings clean to the canonical path:
			// a ..-segment spelling of the operator container root still
			// matches the legacy signature.
			name: "Rule 2: dot-dot legacy root cleans to operator path → blocked",
			recipeResult: result("", aks,
				gpuOpRef(driverOff()), draRef(rootAt("/run/../run/nvidia/driver"))),
			wantMsgs:     1,
			wantContains: []string{"regenerate the recipe"},
		},
		{
			// The legacy-recipe alternative clause is OS-aware like Rule 1's
			// remedy: on GKE+COS the operator cannot install the driver, so
			// the operator-managed tuple must NOT be recommended — the DRA
			// root retarget at the GKE-managed install path is offered
			// instead.
			name: "Rule 2: legacy signature on GKE+COS → COS wording, no operator-managed tuple",
			recipeResult: resultOS("", recipe.CriteriaServiceGKE, recipe.CriteriaOSCOS,
				gpuOpRef(driverOff()), draRef(rootAt("/run/nvidia/driver"))),
			wantMsgs: 1,
			wantContains: []string{
				"regenerate the recipe",
				"On GKE COS node images the GPU Operator cannot install the driver",
				"--set dradriver:nvidiaDriverRoot=/home/kubernetes/bin/nvidia",
			},
		},
		{
			name: "Rule 2: legacy signature on GKE+ubuntu → operator-managed tuple with fifth flag",
			recipeResult: resultOS("", recipe.CriteriaServiceGKE, recipe.CriteriaOSUbuntu,
				gpuOpRef(driverOff()), draRef(rootAt("/run/nvidia/driver"))),
			wantMsgs: 1,
			wantContains: []string{
				"regenerate the recipe",
				"--set gpuoperator:driver.enabled=true",
				"--set gpuoperator:hostPaths.driverInstallDir=/run/nvidia/driver",
			},
		},
		{
			name: "Rule 2: legacy signature on GKE, unknown OS → both paths, COS wording included",
			recipeResult: result("", recipe.CriteriaServiceGKE,
				gpuOpRef(driverOff()), draRef(rootAt("/run/nvidia/driver"))),
			wantMsgs: 1,
			wantContains: []string{
				"On GKE COS node images the GPU Operator cannot install the driver",
				"On GKE Ubuntu node images the GPU Operator can manage the driver",
				"--set gpuoperator:hostPaths.driverInstallDir=/run/nvidia/driver",
			},
		},
		{
			// driverInstallDir "/" is a bind-mount destination runc rejects
			// (issue #1106) — flagged even with no DRA component present.
			name: "driverInstallDir=/ without DRA → blocked",
			recipeResult: result("", aks,
				gpuOpRef(map[string]any{
					"driver":    map[string]any{"enabled": true},
					"hostPaths": map[string]any{"driverInstallDir": "/"},
				})),
			wantMsgs:     1,
			wantContains: []string{"hostPaths.driverInstallDir=/ is invalid", "#1106"},
		},
		{
			// ...and flagged even when the DRA root agrees — equality does
			// not make "/" a legal mount destination. The DRA root of "/"
			// itself is NOT flagged (legitimate preinstalled value).
			name: "driverInstallDir=/ with matching DRA root=/ → still blocked",
			recipeResult: result("", aks,
				gpuOpRef(map[string]any{
					"driver":    map[string]any{"enabled": true},
					"hostPaths": map[string]any{"driverInstallDir": "/"},
				}),
				draRef(rootAt("/"))),
			wantMsgs:     1,
			wantContains: []string{"hostPaths.driverInstallDir=/ is invalid"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs, errs := CheckDriverOwnershipCoherence(
				context.Background(), "gpu-operator", tt.recipeResult, tt.bundlerConfig, tt.conditions)
			if len(errs) != tt.wantErrs {
				t.Fatalf("hard errors = %d (%v), want %d", len(errs), errs, tt.wantErrs)
			}
			joinedErrs := make([]string, 0, len(errs))
			for _, err := range errs {
				joinedErrs = append(joinedErrs, err.Error())
			}
			joinedErr := strings.Join(joinedErrs, "\n")
			for _, want := range tt.wantErrContains {
				if !strings.Contains(joinedErr, want) {
					t.Errorf("hard errors missing %q:\n%s", want, joinedErr)
				}
			}
			if len(msgs) != tt.wantMsgs {
				t.Fatalf("messages = %d (%v), want %d", len(msgs), msgs, tt.wantMsgs)
			}
			joined := strings.Join(msgs, "\n")
			for _, want := range tt.wantContains {
				if !strings.Contains(joined, want) {
					t.Errorf("messages missing %q:\n%s", want, joined)
				}
			}
		})
	}
}

func TestCheckMariaDBOperatorOwnershipCoherence(t *testing.T) {
	t.Parallel()

	result := func(mode recipe.AccountingMode, state string) *recipe.RecipeResult {
		r := &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{{Name: "mariadb-operator"}},
			Configuration: &recipe.RecipeConfiguration{
				Slurm: &recipe.SlurmConfiguration{
					Accounting: &recipe.SlurmAccountingConfiguration{Mode: mode},
				},
			},
		}
		r.Metadata.MariaDBOperatorState = state
		return r
	}
	noComponent := result(recipe.AccountingModeAICRProvided, recipe.MariaDBOperatorStateCRsDetected)
	noComponent.ComponentRefs = nil
	disabledComponent := result(recipe.AccountingModeAICRProvided, recipe.MariaDBOperatorStateCRsDetected)
	disabledComponent.ComponentRefs[0].Overrides = map[string]any{"install": false}
	tests := []struct {
		name         string
		recipeResult *recipe.RecipeResult
		wantWarnings int
		wantErrors   int
		wantContains string
		wantCode     aicrerrors.ErrorCode
	}{
		{name: "nil recipe skipped"},
		{name: "missing component skipped", recipeResult: noComponent},
		{name: "disabled component skipped", recipeResult: disabledComponent},
		{
			name:         "customer-managed ignores detected CRs",
			recipeResult: result(recipe.AccountingModeCustomerManaged, recipe.MariaDBOperatorStateCRsDetected),
		},
		{
			name:         "no snapshot metadata warns and preserves criteria-only flow",
			recipeResult: result(recipe.AccountingModeAICRProvided, ""),
			wantWarnings: 1,
			wantContains: "current snapshot",
		},
		{
			name:         "conclusive absence is silent",
			recipeResult: result(recipe.AccountingModeAICRProvided, recipe.MariaDBOperatorStateAbsent),
		},
		{
			name:         "API detected warns and allows",
			recipeResult: result(recipe.AccountingModeAICRProvided, recipe.MariaDBOperatorStateAPIDetected),
			wantWarnings: 1,
			wantContains: "customer-managed",
		},
		{
			name:         "CRs detected blocks",
			recipeResult: result(recipe.AccountingModeAICRProvided, recipe.MariaDBOperatorStateCRsDetected),
			wantErrors:   1,
			wantContains: "customer-managed",
			wantCode:     aicrerrors.ErrCodeConflict,
		},
		{
			name:         "unknown blocks",
			recipeResult: result(recipe.AccountingModeAICRProvided, recipe.MariaDBOperatorStateUnknown),
			wantErrors:   1,
			wantContains: "customer-managed",
			wantCode:     aicrerrors.ErrCodeConflict,
		},
		{
			name:         "unrecognized state fails closed",
			recipeResult: result(recipe.AccountingModeAICRProvided, "typo"),
			wantErrors:   1,
			wantContains: "not recognized",
			wantCode:     aicrerrors.ErrCodeInvalidRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			warnings, errs := CheckMariaDBOperatorOwnershipCoherence(
				context.Background(), "mariadb-operator", tt.recipeResult, nil, nil)
			if len(warnings) != tt.wantWarnings {
				t.Fatalf("warnings = %d (%v), want %d", len(warnings), warnings, tt.wantWarnings)
			}
			if len(errs) != tt.wantErrors {
				t.Fatalf("errors = %d (%v), want %d", len(errs), errs, tt.wantErrors)
			}
			if tt.wantCode != "" && !stderrors.Is(errs[0], aicrerrors.New(tt.wantCode, "")) {
				t.Errorf("error = %v, want code %s", errs[0], tt.wantCode)
			}
			combined := strings.Join(warnings, "\n")
			for _, err := range errs {
				combined += "\n" + err.Error()
			}
			if tt.wantContains != "" && !strings.Contains(combined, tt.wantContains) {
				t.Errorf("result missing %q:\n%s", tt.wantContains, combined)
			}
		})
	}
}

// TestEffectiveComponentValues_PreservesResolverCode pins the
// error-classification contract of the fail-closed resolution path: when
// GetValuesForComponentWithContext returns an already-coded structured
// error, effectiveComponentValues must wrap with THAT code, not
// reclassify it as ErrCodeInvalidRequest — SDK and server consumers map
// retryability and HTTP status from the outermost code.
func TestEffectiveComponentValues_PreservesResolverCode(t *testing.T) {
	ctx := context.Background()
	rr := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{{
			Name:       "gpu-operator",
			ValuesFile: "components/gpu-operator/does-not-exist.yaml",
		}},
	}

	_, directErr := rr.GetValuesForComponentWithContext(ctx, "gpu-operator")
	if directErr == nil {
		t.Fatal("expected resolver error for missing valuesFile, got nil")
	}
	var directStructured *aicrerrors.StructuredError
	if !stderrors.As(directErr, &directStructured) {
		t.Fatalf("resolver error is not structured: %v", directErr)
	}

	_, err := effectiveComponentValues(ctx, rr, nil, "gpu-operator", []string{"gpu-operator"}, "driver-ownership coherence")
	if err == nil {
		t.Fatal("expected blocking error, got nil")
	}
	var wrapped *aicrerrors.StructuredError
	if !stderrors.As(err, &wrapped) {
		t.Fatalf("wrapped error is not structured: %v", err)
	}
	if wrapped.Code != directStructured.Code {
		t.Errorf("outermost code = %v, want resolver code %v (must not be reclassified)",
			wrapped.Code, directStructured.Code)
	}
	if !strings.Contains(err.Error(), "driver-ownership coherence") {
		t.Errorf("error %q missing coherence framing", err.Error())
	}
}
