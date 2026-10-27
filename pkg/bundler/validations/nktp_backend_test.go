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

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const nktpComponent = "nktp"

func TestCheckNKTPExportersHaveBackend(t *testing.T) {
	t.Parallel()

	const backend = "otlp.observability.corp.internal:4317"
	// lgtmOff disables otel-lgtm and sets each of paths to endpoint.
	lgtmOff := func(endpoint any, paths ...string) map[string]any {
		values := map[string]any{"otel-lgtm": map[string]any{"enabled": false}}
		for _, path := range paths {
			component.SetValueByPath(values, path, endpoint)
		}
		return values
	}
	recipeOf := func(overrides map[string]any) *recipe.RecipeResult {
		return &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{
			Name: nktpComponent, Namespace: "nktp", Overrides: overrides,
		}}}
	}
	sets := func(kv map[string]string) *config.Config {
		return config.NewConfig(config.WithValueOverrides(map[string]map[string]string{nktpComponent: kv}))
	}
	dynamic := func(paths ...string) *config.Config {
		return config.NewConfig(config.WithDynamicValues(map[string][]string{nktpComponent: paths}))
	}
	shipped := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{{
		Name: nktpComponent, Namespace: "nktp", ValuesFile: "components/nktp/values.yaml",
	}}}
	allSet := func() map[string]any { return lgtmOff(backend, nktpLGTMEndpoints...) }
	gateways, watchdog := nktpLGTMEndpoints[:3], nktpLGTMEndpoints[3:]

	tests := []struct {
		name   string
		recipe *recipe.RecipeResult
		cfg    *config.Config
		// want lists the endpoints the one error must name; empty wants no error.
		want []string
	}{
		{name: "otel-lgtm enabled by default", recipe: recipeOf(nil)},
		{name: "otel-lgtm enabled by shipped values", recipe: shipped},
		{name: "disabled, all five set", recipe: recipeOf(allSet())},
		{name: "disabled, all five set with --set", recipe: recipeOf(nil), cfg: sets(map[string]string{
			nktpLGTMEnabledPath:  "false",
			nktpLGTMEndpoints[0]: backend, nktpLGTMEndpoints[1]: backend, nktpLGTMEndpoints[2]: backend,
			nktpLGTMEndpoints[3]: backend, nktpLGTMEndpoints[4]: backend,
		})},
		{name: "disabled, none set", recipe: recipeOf(lgtmOff(backend)), want: nktpLGTMEndpoints},
		{name: "disabled, only the gateways set", recipe: recipeOf(lgtmOff(backend, gateways...)), want: watchdog},
		{name: "disabled with --set on shipped values", recipe: shipped,
			cfg: sets(map[string]string{nktpLGTMEnabledPath: "false"}), want: nktpLGTMEndpoints},
		{name: "disabled, one endpoint null",
			recipe: recipeOf(func() map[string]any {
				v := allSet()
				component.SetValueByPath(v, nktpLGTMEndpoints[4], nil)
				return v
			}()),
			want: nktpLGTMEndpoints[4:]},
		{name: "disabled, an exporter section null",
			recipe: recipeOf(func() map[string]any {
				v := allSet()
				component.SetValueByPath(v, "watchdog.otelcol.config.exporters", nil)
				return v
			}()),
			want: watchdog[:1]},
		{name: "otel-lgtm section not a map counts as disabled",
			recipe: recipeOf(map[string]any{"otel-lgtm": "off"}), want: nktpLGTMEndpoints},
		{name: "unset endpoints deferred with --dynamic", recipe: recipeOf(lgtmOff(backend, gateways...)),
			cfg: dynamic(watchdog[0], "watchdog.sidecar.otelcol.config.exporters")},
		{name: "--dynamic otel-lgtm toggle counts as disabled", recipe: recipeOf(nil),
			cfg: dynamic(nktpLGTMEnabledPath), want: nktpLGTMEndpoints},
		{name: "nktp disabled", recipe: recipeOf(map[string]any{"enabled": false, "otel-lgtm": map[string]any{"enabled": false}})},
		{name: "nktp absent from the recipe", recipe: &recipe.RecipeResult{}},
		{name: "nil recipe"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.cfg
			if cfg == nil {
				cfg = config.NewConfig()
			}
			warnings, errs := CheckNKTPExportersHaveBackend(context.Background(), nktpComponent, tt.recipe, cfg, nil)
			if len(warnings) != 0 {
				t.Errorf("warnings = %q, want none", warnings)
			}
			if len(tt.want) == 0 {
				if len(errs) != 0 {
					t.Fatalf("errors = %v, want none", errs)
				}
				return
			}
			if len(errs) != 1 {
				t.Fatalf("got %d errors %v, want one naming %q", len(errs), errs, tt.want)
			}
			msg := errs[0].Error()
			for _, path := range nktpLGTMEndpoints {
				if named, wanted := strings.Contains(msg, path), slices.Contains(tt.want, path); named != wanted {
					t.Errorf("error names %s = %v, want %v: %q", path, named, wanted, msg)
				}
			}
		})
	}
}
