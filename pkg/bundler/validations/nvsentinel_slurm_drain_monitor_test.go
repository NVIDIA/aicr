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
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestCheckNVSentinelSlurmDrainMonitorRequiresSlinky covers the gate's
// decision table: an enabled monitor needs slinky-slurm present, enabled, and
// deployed to the namespace the monitor watches.
func TestCheckNVSentinelSlurmDrainMonitorRequiresSlinky(t *testing.T) {
	t.Parallel()

	// sentinel builds an nvsentinel ref. A nil toggle or namespace leaves the
	// key absent so the chart default applies.
	sentinel := func(enabled, namespace any) recipe.ComponentRef {
		overrides := map[string]any{}
		if enabled != nil {
			overrides["global"] = map[string]any{"slurmDrainMonitor": map[string]any{"enabled": enabled}}
		}
		if namespace != nil {
			overrides["slurm-drain-monitor"] = map[string]any{"namespace": namespace}
		}
		return recipe.ComponentRef{Name: nvsentinelComponent, Overrides: overrides}
	}
	slinky := func(namespace string) recipe.ComponentRef {
		return recipe.ComponentRef{Name: slinkySlurmComponent, Namespace: namespace}
	}
	slinkyDisabled := recipe.ComponentRef{Name: slinkySlurmComponent, Namespace: "slurm",
		Overrides: map[string]any{"enabled": false}}
	result := func(refs ...recipe.ComponentRef) *recipe.RecipeResult {
		return &recipe.RecipeResult{ComponentRefs: refs}
	}
	// A `bundlers=nvsentinel` subset: slinky-slurm is declared by the recipe
	// but filtered out of what is being bundled. The gate must find it through
	// the declared union, or it rejects a valid partial bundle.
	subset := func(rendered, declared []recipe.ComponentRef) *recipe.RecipeResult {
		return (&recipe.RecipeResult{ComponentRefs: rendered}).WithDeclaredComponents(declared)
	}
	dynamic := func(key string, paths ...string) *config.Config {
		return config.NewConfig(config.WithDynamicValues(map[string][]string{key: paths}))
	}

	tests := []struct {
		name          string
		recipeResult  *recipe.RecipeResult
		bundlerConfig *config.Config
		wantBlocked   bool
	}{
		{name: "nil recipe result → skipped", recipeResult: nil},
		{name: "no nvsentinel ref → skipped", recipeResult: result()},
		{
			name: "nvsentinel disabled by recipe → skipped",
			recipeResult: result(recipe.ComponentRef{Name: nvsentinelComponent,
				Overrides: map[string]any{"enabled": false, "global": map[string]any{"slurmDrainMonitor": map[string]any{"enabled": true}}}}),
		},
		{
			// Every non-Slurm recipe: the mixin is not composed.
			name:         "monitor untouched (chart default off), no slinky → passes",
			recipeResult: result(sentinel(nil, nil)),
		},
		{
			name:         "monitor explicitly off, no slinky → passes",
			recipeResult: result(sentinel(false, nil)),
		},
		{
			// The shape the mixin produces on a platform: slurm leaf.
			name:         "monitor on, slinky in the watched namespace → passes",
			recipeResult: result(sentinel(true, "slurm"), slinky("slurm")),
		},
		{
			name:         "monitor on at chart-default namespace, slinky namespace unset → passes",
			recipeResult: result(sentinel(true, nil), slinky("")),
		},
		{
			name:         "monitor on, slinky absent → blocked",
			recipeResult: result(sentinel(true, "slurm")),
			wantBlocked:  true,
		},
		{
			name:         "monitor on, slinky disabled → blocked",
			recipeResult: result(sentinel(true, "slurm"), slinkyDisabled),
			wantBlocked:  true,
		},
		{
			name:         "monitor on, slinky deployed elsewhere → blocked",
			recipeResult: result(sentinel(true, "slurm"), slinky("hpc")),
			wantBlocked:  true,
		},
		{
			name:         "monitor watching another namespace than slinky → blocked",
			recipeResult: result(sentinel(true, "hpc"), slinky("slurm")),
			wantBlocked:  true,
		},
		{
			name: "bundlers= subset excluding slinky-slurm → passes",
			recipeResult: subset(
				[]recipe.ComponentRef{sentinel(true, "slurm")},
				[]recipe.ComponentRef{sentinel(true, "slurm"), slinky("slurm")},
			),
		},
		{
			name: "bundlers= subset, declared slinky-slurm disabled → blocked",
			recipeResult: subset(
				[]recipe.ComponentRef{sentinel(true, "slurm")},
				[]recipe.ComponentRef{sentinel(true, "slurm"), slinkyDisabled},
			),
			wantBlocked: true,
		},
		{
			// The monitor restricts its watch only to a non-empty namespace,
			// so "" watches every namespace, the slinky one included.
			name:         "monitor watching every namespace (\"\") → passes",
			recipeResult: result(sentinel(true, ""), slinky("hpc")),
		},
		{
			name:         "monitor namespace whitespace-only → blocked",
			recipeResult: result(sentinel(true, "  "), slinky("slurm")),
			wantBlocked:  true,
		},
		{
			name:         "monitor on, namespace non-string → blocked",
			recipeResult: result(sentinel(true, 42), slinky("slurm")),
			wantBlocked:  true,
		},
		{
			// Helm renders a subchart whose condition is not a bool, so a
			// non-bool toggle is an enabled monitor.
			name:         "monitor toggle non-bool, slinky absent → blocked",
			recipeResult: result(sentinel("yes", nil)),
			wantBlocked:  true,
		},
		{
			name:          "monitor enabled via --set-json, slinky absent → blocked",
			recipeResult:  result(sentinel(nil, nil)),
			bundlerConfig: setJSONOverride(t, "nv-sentinel:global.slurmDrainMonitor.enabled=true"),
			wantBlocked:   true,
		},
		{
			name:         "slinky disabled via --set, monitor on → blocked",
			recipeResult: result(sentinel(true, "slurm"), slinky("slurm")),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"slinkyslurm": {"enabled": "false"},
			})),
			wantBlocked: true,
		},
		{
			// A statically-off monitor is still guarded: the toggle could
			// flip at install time after this gate passed.
			name:          "monitor toggle dynamic, statically off → blocked",
			recipeResult:  result(sentinel(nil, nil)),
			bundlerConfig: dynamic("nv-sentinel", "global.slurmDrainMonitor.enabled"),
			wantBlocked:   true,
		},
		{
			name:          "monitor namespace dynamic, monitor on → blocked",
			recipeResult:  result(sentinel(true, "slurm"), slinky("slurm")),
			bundlerConfig: dynamic("nv-sentinel", "slurm-drain-monitor.namespace"),
			wantBlocked:   true,
		},
		{
			name:          "slinky enabled dynamic, monitor on → blocked",
			recipeResult:  result(sentinel(true, "slurm"), slinky("slurm")),
			bundlerConfig: dynamic("slinkyslurm", "enabled"),
			wantBlocked:   true,
		},
		{
			name:          "slinky enabled dynamic, monitor statically off → passes",
			recipeResult:  result(sentinel(false, nil), slinky("slurm")),
			bundlerConfig: dynamic("slinkyslurm", "enabled"),
		},
		{
			name:          "unrelated --dynamic path does not trip the guard → passes",
			recipeResult:  result(sentinel(true, "slurm"), slinky("slurm")),
			bundlerConfig: dynamic("nv-sentinel", "global.auditLogging.maxSizeMB"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.bundlerConfig
			if cfg == nil {
				cfg = config.NewConfig()
			}
			warnings, errs := CheckNVSentinelSlurmDrainMonitorRequiresSlinky(
				t.Context(), nvsentinelComponent, tt.recipeResult, cfg, nil)
			// Dynamic-guard rows block via the first return slot, the static
			// rows via the second.
			gotBlocked := len(warnings) > 0 || len(errs) > 0
			if gotBlocked != tt.wantBlocked {
				t.Errorf("blocked = %v (warnings=%v errs=%v), want %v", gotBlocked, warnings, errs, tt.wantBlocked)
			}
		})
	}
}
