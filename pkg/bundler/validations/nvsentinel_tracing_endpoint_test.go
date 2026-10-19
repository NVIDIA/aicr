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

// setJSONOverride runs a spec through the real --set-json decode path
// (config.ParseValueOverridesJSON) so numeric literals arrive as the
// genuine json.Unmarshal type (float64), not a hand-constructed Go int --
// the two are not interchangeable inputs to nvsentinelTracingEnabled's
// helmTruthy check.
func setJSONOverride(t *testing.T, spec string) *config.Config {
	t.Helper()
	paths, err := config.ParseValueOverridesJSON([]string{spec})
	if err != nil {
		t.Fatalf("ParseValueOverridesJSON(%q) = %v", spec, err)
	}
	return config.NewConfig(config.WithValueOverridesTypedPaths(paths))
}

// TestCheckNVSentinelTracingEndpointRequired covers the gate's decision
// table: tracing off (recipe default and explicit), tracing on with an
// endpoint (via recipe override, plain --set, and --set-json), tracing on
// with a missing/blank endpoint, and the non-bool-but-Helm-truthy spellings
// (--set ...=1, --set-json numeric/string) that ConvertMapValue leaves as
// non-bool Go values -- nvsentinelTracingEnabled must still treat these as
// enabled the way the chart's raw `{{ if }}` does (helmTruthy).
func TestCheckNVSentinelTracingEndpointRequired(t *testing.T) {
	t.Parallel()

	sentinelRef := func(overrides map[string]any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: nvsentinelComponent, Overrides: overrides}
	}
	tracing := func(enabled any, endpoint any) map[string]any {
		t := map[string]any{}
		if enabled != nil {
			t["enabled"] = enabled
		}
		if endpoint != nil {
			t["endpoint"] = endpoint
		}
		return map[string]any{"global": map[string]any{"tracing": t}}
	}
	result := func(refs ...recipe.ComponentRef) *recipe.RecipeResult {
		return &recipe.RecipeResult{ComponentRefs: refs}
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
			name:         "tracing untouched (chart default off) → passes",
			recipeResult: result(sentinelRef(nil)),
		},
		{
			name:         "tracing explicitly disabled → passes",
			recipeResult: result(sentinelRef(tracing(false, nil))),
		},
		{
			name:         "tracing enabled, endpoint absent → blocked",
			recipeResult: result(sentinelRef(tracing(true, nil))),
			wantBlocked:  true,
		},
		{
			name:         "tracing enabled, endpoint empty string → blocked",
			recipeResult: result(sentinelRef(tracing(true, ""))),
			wantBlocked:  true,
		},
		{
			name:         "tracing enabled, endpoint whitespace-only → blocked",
			recipeResult: result(sentinelRef(tracing(true, "   "))),
			wantBlocked:  true,
		},
		{
			name:         "tracing enabled, endpoint set on the recipe → passes",
			recipeResult: result(sentinelRef(tracing(true, "otel-collector.example:4317"))),
		},
		{
			name:         "tracing enabled via plain --set string, endpoint absent → blocked",
			recipeResult: result(sentinelRef(nil)),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"nv-sentinel": {"global.tracing.enabled": "true"},
			})),
			wantBlocked: true,
		},
		{
			name:         "tracing enabled and endpoint both via plain --set string → passes",
			recipeResult: result(sentinelRef(nil)),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"nv-sentinel": {
					"global.tracing.enabled":  "true",
					"global.tracing.endpoint": "otel-collector.example:4317",
				},
			})),
		},
		{
			name:         "endpoint supplied via --set clears a recipe-level enabled-with-no-endpoint → passes",
			recipeResult: result(sentinelRef(tracing(true, nil))),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"nv-sentinel": {"global.tracing.endpoint": "otel-collector.example:4317"},
			})),
		},
		{
			// ConvertMapValue (pkg/component/overrides.go) only bool-converts
			// the exact strings "true"/"false"; "1" is left as int64(1). The
			// chart's raw `{{- if .Values.global.tracing.enabled }}` still
			// treats int64(1) as truthy, so this must still block.
			name:         "tracing enabled via plain --set numeric spelling (not bool after ConvertMapValue), endpoint absent → blocked",
			recipeResult: result(sentinelRef(nil)),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"nv-sentinel": {"global.tracing.enabled": "1"},
			})),
			wantBlocked: true,
		},
		{
			name:          "tracing enabled via real --set-json true (bool), endpoint absent → blocked",
			recipeResult:  result(sentinelRef(nil)),
			bundlerConfig: setJSONOverride(t, "nv-sentinel:global.tracing.enabled=true"),
			wantBlocked:   true,
		},
		{
			// json.Unmarshal decodes a bare JSON number as float64, never
			// int -- this is the type nvsentinelTracingEnabled must handle
			// on the real --set-json path, not a hand-constructed Go int.
			name:          "tracing enabled via real --set-json non-zero number (float64), endpoint absent → blocked",
			recipeResult:  result(sentinelRef(nil)),
			bundlerConfig: setJSONOverride(t, "nv-sentinel:global.tracing.enabled=1"),
			wantBlocked:   true,
		},
		{
			name:         "tracing enabled via --set-json, endpoint also via --set-json → passes",
			recipeResult: result(sentinelRef(nil)),
			bundlerConfig: config.NewConfig(config.WithValueOverridesTypedPaths([]config.TypedComponentPath{
				{Component: "nv-sentinel", Path: "global.tracing.enabled", Value: true},
				{Component: "nv-sentinel", Path: "global.tracing.endpoint", Value: "otel-collector.example:4317"},
			})),
		},
		{
			name:          "tracing.enabled set to zero via real --set-json (falsy under Helm if) → passes",
			recipeResult:  result(sentinelRef(nil)),
			bundlerConfig: setJSONOverride(t, "nv-sentinel:global.tracing.enabled=0"),
		},
		{
			// --dynamic defers the path to the operator-editable
			// cluster-values.yaml, loaded after this gate runs, so a
			// declaration on the enabled path must block even though the
			// statically-resolved value (chart default, off) looks fine.
			name:         "tracing.enabled declared --dynamic, no static value → blocked",
			recipeResult: result(sentinelRef(nil)),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.tracing.enabled"},
			})),
			wantBlocked: true,
		},
		{
			name:         "tracing.endpoint declared --dynamic on top of a static enabled+endpoint → blocked",
			recipeResult: result(sentinelRef(tracing(true, "otel-collector.example:4317"))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.tracing.endpoint"},
			})),
			wantBlocked: true,
		},
		{
			name:         "unrelated --dynamic path does not trip the tracing guard → passes",
			recipeResult: result(sentinelRef(tracing(true, "otel-collector.example:4317"))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.auditLogging.maxSizeMB"},
			})),
		},
		{
			// Relation-aware: dynamic enabled alone can never produce
			// "enabled + no endpoint" when the endpoint is statically real
			// and not itself dynamic -- nothing dynamic touches it.
			name:         "tracing.enabled dynamic, endpoint statically real and not dynamic → passes",
			recipeResult: result(sentinelRef(tracing(nil, "otel-collector.example:4317"))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.tracing.enabled"},
			})),
		},
		{
			// Relation-aware: dynamic endpoint alone can never produce
			// "enabled + no endpoint" when enabled is statically false and
			// not itself dynamic -- tracing never activates regardless of
			// what the operator sets the endpoint to.
			name:         "tracing.endpoint dynamic, enabled statically false and not dynamic → passes",
			recipeResult: result(sentinelRef(tracing(false, nil))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.tracing.endpoint"},
			})),
		},
		{
			name:         "tracing.enabled dynamic, endpoint statically empty → blocked",
			recipeResult: result(sentinelRef(nil)),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.tracing.enabled"},
			})),
			wantBlocked: true,
		},
		{
			name:         "tracing.endpoint dynamic, enabled statically true → blocked",
			recipeResult: result(sentinelRef(tracing(true, nil))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.tracing.endpoint"},
			})),
			wantBlocked: true,
		},
		{
			name:         "both tracing.enabled and tracing.endpoint dynamic → always blocked",
			recipeResult: result(sentinelRef(tracing(true, "otel-collector.example:4317"))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.tracing.enabled", "global.tracing.endpoint"},
			})),
			wantBlocked: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.bundlerConfig
			if cfg == nil {
				cfg = config.NewConfig()
			}
			warnings, errs := CheckNVSentinelTracingEndpointRequired(
				t.Context(), nvsentinelComponent, tt.recipeResult, cfg, nil)
			// The dynamic-guard rows block via the first return slot
			// (registry severity converts it to a hard error only through
			// RunComponentValidations); the static endpoint-missing rows
			// block via the second slot directly. Either non-empty means
			// blocked at this raw function-call layer.
			gotBlocked := len(warnings) > 0 || len(errs) > 0
			if gotBlocked != tt.wantBlocked {
				t.Errorf("blocked = %v (warnings=%v errs=%v), want %v", gotBlocked, warnings, errs, tt.wantBlocked)
			}
		})
	}
}
