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

	corev1 "k8s.io/api/core/v1"
)

func TestCheckNodewrightRuntimeRequiredTaintKey(t *testing.T) {
	const component = "nodewright-operator"
	const taintPath = "controllerManager.manager.env.runtimeRequiredTaint"
	withOperator := func() *recipe.RecipeResult {
		return &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: component}}}
	}
	gate := func(key string) config.Option {
		return config.WithWorkloadGateTaint(&corev1.Taint{Key: key, Value: "runtime-required", Effect: corev1.TaintEffectNoSchedule})
	}
	set := func(kv map[string]string) config.Option {
		return config.WithValueOverrides(map[string]map[string]string{component: kv})
	}

	tests := []struct {
		name         string
		recipeResult *recipe.RecipeResult
		opts         []config.Option
		wantWarnings int
		wantContains string
	}{
		{
			name:         "component not in recipe",
			recipeResult: &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}}},
			opts:         []config.Option{gate("example.com/runtime-required")},
		},
		{name: "chart default, nothing set", recipeResult: withOperator()},
		{name: "default key via workload gate", recipeResult: withOperator(), opts: []config.Option{gate("nodewright.nvidia.com")}},
		{name: "legacy key via workload gate", recipeResult: withOperator(), opts: []config.Option{gate("skyhook.nvidia.com")}},
		{name: "53-character key", recipeResult: withOperator(), opts: []config.Option{gate(strings.Repeat("a", 53))}},
		{
			name:         "54-character key",
			recipeResult: withOperator(),
			opts:         []config.Option{gate(strings.Repeat("a", 54))},
			wantWarnings: 1,
			wantContains: "no more than 63 bytes",
		},
		{
			name:         "unprefixed key with trailing punctuation via --set",
			recipeResult: withOperator(),
			opts:         []config.Option{set(map[string]string{taintPath: "gate-=true:NoSchedule"})},
			wantWarnings: 1,
			wantContains: "must consist of alphanumeric characters",
		},
		{
			name:         "DNS-prefixed key via workload gate",
			recipeResult: withOperator(),
			opts:         []config.Option{gate("example.com/runtime-required")},
			wantWarnings: 1,
			wantContains: `"example.com/runtime-required"`,
		},
		{
			name:         "DNS-prefixed key via --set",
			recipeResult: withOperator(),
			opts:         []config.Option{set(map[string]string{taintPath: "example.com/gate=true:NoSchedule"})},
			wantWarnings: 1,
			wantContains: `"example.com/gate"`,
		},
		{
			name:         "same bad key from both sources reported once",
			recipeResult: withOperator(),
			opts: []config.Option{
				gate("example.com/gate"),
				set(map[string]string{taintPath: "example.com/gate:NoSchedule"}),
			},
			wantWarnings: 1,
		},
		{
			name:         "dynamic declaration on the taint path",
			recipeResult: withOperator(),
			opts:         []config.Option{config.WithDynamicValues(map[string][]string{component: {taintPath}})},
			wantWarnings: 1,
			wantContains: "--dynamic",
		},
		{
			name:         "disabled component skipped",
			recipeResult: withOperator(),
			opts: []config.Option{
				gate("example.com/runtime-required"),
				set(map[string]string{config.ComponentEnabledKey: "false"}),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings, errs := CheckNodewrightRuntimeRequiredTaintKey(context.Background(), component, tt.recipeResult, config.NewConfig(tt.opts...), nil)
			if len(errs) != 0 {
				t.Fatalf("errors = %v, want none", errs)
			}
			if len(warnings) != tt.wantWarnings {
				t.Fatalf("warnings = %v, want %d", warnings, tt.wantWarnings)
			}
			if tt.wantContains != "" && !strings.Contains(warnings[0], tt.wantContains) {
				t.Errorf("warning %q does not contain %q", warnings[0], tt.wantContains)
			}
		})
	}
}

// TestCheckNodewrightRuntimeRequiredTaintKeyNilConfig covers the values-only SDK
// path, which runs validations without a bundler config.
func TestCheckNodewrightRuntimeRequiredTaintKeyNilConfig(t *testing.T) {
	const component = "nodewright-operator"
	withOverrides := func(overrides map[string]any) *recipe.RecipeResult {
		return &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{Name: component, Overrides: overrides}}}
	}
	badRecipeValue := map[string]any{"controllerManager": map[string]any{"manager": map[string]any{"env": map[string]any{
		"runtimeRequiredTaint": "example.com/gate=true:NoSchedule",
	}}}}
	tests := []struct {
		name         string
		recipeResult *recipe.RecipeResult
		wantWarnings int
	}{
		{name: "nothing set", recipeResult: withOverrides(nil)},
		{name: "bad key in recipe values", recipeResult: withOverrides(badRecipeValue), wantWarnings: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings, errs := CheckNodewrightRuntimeRequiredTaintKey(context.Background(), component, tt.recipeResult, nil, nil)
			if len(errs) != 0 {
				t.Fatalf("errors = %v, want none", errs)
			}
			if len(warnings) != tt.wantWarnings {
				t.Errorf("warnings = %v, want %d", warnings, tt.wantWarnings)
			}
		})
	}
}

func TestTaintStringKey(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"nodewright.nvidia.com=runtime-required:NoSchedule", "nodewright.nvidia.com"},
		{"example.com/gate:NoSchedule", "example.com/gate"},
		{"bare", "bare"},
	}
	for _, tt := range tests {
		if got := taintStringKey(tt.in); got != tt.want {
			t.Errorf("taintStringKey(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
