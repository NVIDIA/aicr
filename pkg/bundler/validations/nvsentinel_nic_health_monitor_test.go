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

// TestCheckNVSentinelNicHealthMonitorRequiresMetadataCollector covers the
// gate's decision table. The broken state needs all three of {monitor
// enabled, metadata-collector explicitly disabled, no inclusion-regex
// override}; any one of them absent is a pass. The --dynamic rows exercise
// the relation-aware guard, which must block only when the non-dynamic
// fields still leave the broken state reachable.
func TestCheckNVSentinelNicHealthMonitorRequiresMetadataCollector(t *testing.T) {
	t.Parallel()

	sentinelRef := func(overrides map[string]any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: nvsentinelComponent, Overrides: overrides}
	}
	// values builds an nvsentinel override tree. A nil toggle leaves the
	// key absent so the chart default applies.
	values := func(monitor, collector any, override any) map[string]any {
		global := map[string]any{}
		if monitor != nil {
			global["nicHealthMonitor"] = map[string]any{"enabled": monitor}
		}
		if collector != nil {
			global["metadataCollector"] = map[string]any{"enabled": collector}
		}
		out := map[string]any{"global": global}
		if override != nil {
			out["nic-health-monitor"] = map[string]any{"nicInclusionRegexOverride": override}
		}
		return out
	}
	result := func(refs ...recipe.ComponentRef) *recipe.RecipeResult {
		return &recipe.RecipeResult{ComponentRefs: refs}
	}
	dynamic := func(paths ...string) *config.Config {
		return config.NewConfig(config.WithDynamicValues(map[string][]string{"nv-sentinel": paths}))
	}

	tests := []struct {
		name          string
		recipeResult  *recipe.RecipeResult
		bundlerConfig *config.Config
		wantBlocked   bool
	}{
		{
			name:         "nil recipe result → skipped",
			recipeResult: nil,
		},
		{
			name:         "no nvsentinel ref → skipped",
			recipeResult: result(),
		},
		{
			name:         "nvsentinel disabled by recipe → skipped",
			recipeResult: result(sentinelRef(map[string]any{"enabled": false})),
		},
		{
			name:         "monitor untouched (chart default off) → passes",
			recipeResult: result(sentinelRef(nil)),
		},
		{
			// The shipped shape on every non-AKS/OKE recipe: the
			// collector is off but nothing enabled the monitor.
			name:         "monitor off, collector explicitly disabled → passes",
			recipeResult: result(sentinelRef(values(nil, false, nil))),
		},
		{
			// The shipped shape the mixin produces on AKS/OKE: monitor on,
			// collector left at its enabled chart default.
			name:         "monitor on, collector at chart default → passes",
			recipeResult: result(sentinelRef(values(true, nil, nil))),
		},
		{
			name:         "monitor on, collector explicitly enabled → passes",
			recipeResult: result(sentinelRef(values(true, true, nil))),
		},
		{
			name:         "monitor on, collector disabled, no override → blocked",
			recipeResult: result(sentinelRef(values(true, false, nil))),
			wantBlocked:  true,
		},
		{
			name:         "monitor on, collector disabled, override empty string → blocked",
			recipeResult: result(sentinelRef(values(true, false, ""))),
			wantBlocked:  true,
		},
		{
			name:         "monitor on, collector disabled, override whitespace-only → blocked",
			recipeResult: result(sentinelRef(values(true, false, "   "))),
			wantBlocked:  true,
		},
		{
			name:         "monitor on, collector disabled, override set → passes",
			recipeResult: result(sentinelRef(values(true, false, "^mlx5_"))),
		},
		{
			// Fail closed: a non-string override leaves the bypass
			// unverifiable, which is not the same as unset.
			name:         "monitor on, collector disabled, override non-string → blocked",
			recipeResult: result(sentinelRef(values(true, false, 42))),
			wantBlocked:  true,
		},
		{
			// ConvertMapValue (pkg/component/overrides.go) bool-converts
			// only "true"/"false"; "1" stays int64(1), which Helm cannot
			// read as a condition, so the subchart renders regardless.
			name:         "monitor enabled via --set numeric spelling, collector disabled → blocked",
			recipeResult: result(sentinelRef(values(nil, false, nil))),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"nv-sentinel": {"global.nicHealthMonitor.enabled": "1"},
			})),
			wantBlocked: true,
		},
		{
			// Same rule on the collector side, opposite consequence:
			// int64(0) does not switch metadata-collector off either, so
			// the inventory is still produced and nothing is broken.
			// Reading this as "disabled" would block a working bundle.
			name:         "monitor on, collector set to numeric zero via --set (non-bool, still renders) → passes",
			recipeResult: result(sentinelRef(values(true, nil, nil))),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"nv-sentinel": {"global.metadataCollector.enabled": "0"},
			})),
		},
		{
			name:          "monitor enabled via real --set-json bool, collector disabled → blocked",
			recipeResult:  result(sentinelRef(values(nil, false, nil))),
			bundlerConfig: setJSONOverride(t, "nv-sentinel:global.nicHealthMonitor.enabled=true"),
			wantBlocked:   true,
		},
		{
			// A dependency condition is boolean-only: Helm logs "returned
			// non-bool value" for 0 and renders the subchart anyway, so
			// this is the broken combination, not a disabled monitor.
			name:          "monitor set to zero via real --set-json (non-bool, still renders) → blocked",
			recipeResult:  result(sentinelRef(values(nil, false, nil))),
			bundlerConfig: setJSONOverride(t, "nv-sentinel:global.nicHealthMonitor.enabled=0"),
			wantBlocked:   true,
		},
		{
			name:         "override supplied via --set clears a recipe-level broken combination → passes",
			recipeResult: result(sentinelRef(values(true, false, nil))),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"nv-sentinel": {"nic-health-monitor.nicInclusionRegexOverride": "^mlx5_"},
			})),
		},
		{
			// Relation-aware: the monitor could be flipped on, and the
			// collector is statically off with no override, so the broken
			// state is one install-time edit away.
			name:          "monitor dynamic, collector statically disabled → blocked",
			recipeResult:  result(sentinelRef(values(nil, false, nil))),
			bundlerConfig: dynamic("global.nicHealthMonitor.enabled"),
			wantBlocked:   true,
		},
		{
			// Relation-aware: nothing dynamic can disable a collector that
			// is statically enabled, so the monitor toggle is harmless.
			name:          "monitor dynamic, collector statically enabled → passes",
			recipeResult:  result(sentinelRef(values(nil, true, nil))),
			bundlerConfig: dynamic("global.nicHealthMonitor.enabled"),
		},
		{
			// Relation-aware: the monitor is statically off and not itself
			// dynamic, so the collector can be disabled harmlessly.
			name:          "collector dynamic, monitor statically off → passes",
			recipeResult:  result(sentinelRef(nil)),
			bundlerConfig: dynamic("global.metadataCollector.enabled"),
		},
		{
			name:          "collector dynamic, monitor statically on → blocked",
			recipeResult:  result(sentinelRef(values(true, nil, nil))),
			bundlerConfig: dynamic("global.metadataCollector.enabled"),
			wantBlocked:   true,
		},
		{
			// A statically real override stops being a guarantee once the
			// operator can edit it to empty at install time.
			name:          "override dynamic, monitor on and collector disabled → blocked",
			recipeResult:  result(sentinelRef(values(true, false, "^mlx5_"))),
			bundlerConfig: dynamic("nic-health-monitor.nicInclusionRegexOverride"),
			wantBlocked:   true,
		},
		{
			name:          "override dynamic, collector statically enabled → passes",
			recipeResult:  result(sentinelRef(values(true, true, nil))),
			bundlerConfig: dynamic("nic-health-monitor.nicInclusionRegexOverride"),
		},
		{
			name:          "all three dynamic → blocked",
			recipeResult:  result(sentinelRef(values(nil, nil, nil))),
			bundlerConfig: dynamic("global.nicHealthMonitor.enabled", "global.metadataCollector.enabled", "nic-health-monitor.nicInclusionRegexOverride"),
			wantBlocked:   true,
		},
		{
			name:          "unrelated --dynamic path does not trip the guard → passes",
			recipeResult:  result(sentinelRef(values(true, true, nil))),
			bundlerConfig: dynamic("global.auditLogging.maxSizeMB"),
		},
		{
			// The guard is not the only thing that can block: an unrelated
			// dynamic path leaves the static rule to catch a genuinely
			// broken combination.
			name:          "unrelated --dynamic path, broken static combination → blocked",
			recipeResult:  result(sentinelRef(values(true, false, nil))),
			bundlerConfig: dynamic("global.auditLogging.maxSizeMB"),
			wantBlocked:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.bundlerConfig
			if cfg == nil {
				cfg = config.NewConfig()
			}
			warnings, errs := CheckNVSentinelNicHealthMonitorRequiresMetadataCollector(
				t.Context(), nvsentinelComponent, tt.recipeResult, cfg, nil)
			// Dynamic-guard rows block via the first return slot; the
			// static rows via the second. Either non-empty is "blocked"
			// at this raw function-call layer.
			gotBlocked := len(warnings) > 0 || len(errs) > 0
			if gotBlocked != tt.wantBlocked {
				t.Errorf("blocked = %v (warnings=%v errs=%v), want %v", gotBlocked, warnings, errs, tt.wantBlocked)
			}
		})
	}
}
