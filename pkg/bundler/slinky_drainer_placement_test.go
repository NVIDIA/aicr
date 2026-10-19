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

package bundler

import (
	"fmt"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/validations"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"k8s.io/apimachinery/pkg/labels"
)

// slinkyControlSelectorPaths are slinky-slurm's system nodeSelector paths;
// accounting is off in these scenarios, so its path is not rendered.
var slinkyControlSelectorPaths = []string{
	"controller.podSpec.nodeSelector",
	"restapi.podSpec.nodeSelector",
	"loginsets.slinky.podSpec.nodeSelector",
}

// TestSlinkyDrainerPlacementGateMatchesRenderedBundle keeps
// CheckNVSentinelSlurmCustomDrainCoherent's placement rule in step with what
// extractComponentValues renders. The gate cannot import this package, so it
// models the selector pipeline (--set, injection unless the overlay opts out,
// then --set-json) itself; each scenario renders the real bundle values,
// decides from them whether a Slurm control pod can share slurmd's nodes, and
// requires the gate to agree.
func TestSlinkyDrainerPlacementGateMatchesRenderedBundle(t *testing.T) {
	system := map[string]string{"nodeGroup": "system"}
	gpu := map[string]string{"nodeGroup": "gpu"}
	podSpec := func(selector any) map[string]any {
		return map[string]any{"podSpec": map[string]any{"nodeSelector": selector}}
	}
	setJSON := func(t *testing.T, specs ...string) config.Option {
		t.Helper()
		paths, err := config.ParseValueOverridesJSON(specs)
		if err != nil {
			t.Fatalf("ParseValueOverridesJSON: %v", err)
		}
		return config.WithValueOverridesTypedPaths(paths)
	}

	tests := []struct {
		name     string
		overlay  map[string]any
		selector [2]map[string]string // system, accelerated
		opts     func(t *testing.T) []config.Option
	}{
		{name: "injected selectors only", selector: [2]map[string]string{system, gpu}},
		{name: "no system selector", selector: [2]map[string]string{nil, gpu}},
		{name: "overlay control selector replaced by injection",
			overlay: map[string]any{"controller": podSpec(map[string]any{"nodeGroup": "gpu"})}, selector: [2]map[string]string{system, gpu}},
		{name: "overlay worker selector replaced by injection",
			overlay:  map[string]any{"nodesets": map[string]any{"slinky": podSpec(map[string]any{"nodeGroup": "system"})}},
			selector: [2]map[string]string{system, gpu}},
		{name: "overlay null opts out",
			overlay: map[string]any{"controller": podSpec(nil)}, selector: [2]map[string]string{system, gpu}},
		{name: "overlay {} opts out",
			overlay: map[string]any{"restapi": podSpec(map[string]any{})}, selector: [2]map[string]string{system, gpu}},
		{name: "overlay {} then --set-json pins every control path",
			overlay: map[string]any{"controller": podSpec(map[string]any{}), "restapi": podSpec(map[string]any{}),
				"loginsets": map[string]any{"slinky": podSpec(map[string]any{})}},
			selector: [2]map[string]string{system, gpu},
			opts: func(t *testing.T) []config.Option {
				return []config.Option{setJSON(t,
					`slinkyslurm:controller.podSpec.nodeSelector={"nodeGroup":"system"}`,
					`slinkyslurm:restapi.podSpec.nodeSelector={"nodeGroup":"system"}`,
					`slinkyslurm:loginsets.slinky.podSpec.nodeSelector={"nodeGroup":"system"}`)}
			}},
		{name: "overlay {} then --set-json null",
			overlay: map[string]any{"controller": podSpec(map[string]any{})}, selector: [2]map[string]string{system, gpu},
			opts: func(t *testing.T) []config.Option {
				return []config.Option{setJSON(t, `slinkyslurm:controller.podSpec.nodeSelector=null`)}
			}},
		{name: "--set-json {} merges into the injected selector", selector: [2]map[string]string{system, gpu},
			opts: func(t *testing.T) []config.Option {
				return []config.Option{setJSON(t, `slinkyslurm:controller.podSpec.nodeSelector={}`)}
			}},
		{name: "--set control selector before injection, system == accelerated",
			selector: [2]map[string]string{gpu, gpu},
			opts: func(*testing.T) []config.Option {
				return []config.Option{config.WithValueOverrides(map[string]map[string]string{
					"slinkyslurm": {"controller.podSpec.nodeSelector.pool": "cpu"}})}
			}},
		{name: "system selector a superset of the accelerated one",
			selector: [2]map[string]string{{"nodeGroup": "gpu", "zone": "a"}, gpu}},
		{name: "selectors on different keys", selector: [2]map[string]string{{"role": "system"}, gpu}},
		{name: "selectors conflict on one key and differ on another",
			selector: [2]map[string]string{{"nodeGroup": "system", "role": "system"}, gpu}},
		{name: "worker selector cleared by --set-json null", selector: [2]map[string]string{system, gpu},
			opts: func(t *testing.T) []config.Option {
				return []config.Option{setJSON(t, `slinkyslurm:nodesets.slinky.podSpec.nodeSelector=null`)}
			}},
	}

	compared := map[bool]int{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := []config.Option{}
			if tt.selector[0] != nil {
				opts = append(opts, config.WithSystemNodeSelector(tt.selector[0]))
			}
			opts = append(opts, config.WithAcceleratedNodeSelector(tt.selector[1]))
			if tt.opts != nil {
				opts = append(opts, tt.opts(t)...)
			}
			cfg := config.NewConfig(opts...)
			result := &recipe.RecipeResult{ComponentRefs: []recipe.ComponentRef{
				placementSentinel(),
				{Name: "slinky-slurm", Namespace: "slurm", Type: recipe.ComponentTypeHelm, Overrides: tt.overlay},
				{Name: "slinky-drainer", Namespace: "nvsentinel", Type: recipe.ComponentTypeHelm,
					Overrides: map[string]any{"slinkyNamespace": "slurm"}},
			}}

			b, err := New(WithConfig(cfg))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			rendered := map[string]map[string]any{}
			values, extractErr := b.extractComponentValues(t.Context(), result)
			if extractErr == nil {
				rendered = values
			}
			wantBlocked := extractErr == nil && controlPodCanShareSlurmdNode(rendered["slinky-slurm"])

			_, errs := validations.CheckNVSentinelSlurmCustomDrainCoherent(t.Context(), "nvsentinel", result, cfg, nil)
			var placement []string
			for _, e := range errs {
				if msg := e.Error(); strings.Contains(msg, "renders no node selector") || strings.Contains(msg, "overlap") {
					placement = append(placement, msg)
				}
			}
			if extractErr != nil {
				// The bundler rejects the shape itself (requireNodeSelector);
				// the gate need not agree on a bundle that cannot be built.
				return
			}
			compared[wantBlocked]++
			if gotBlocked := len(placement) > 0; gotBlocked != wantBlocked {
				t.Errorf("gate blocked = %v, rendered bundle lets a control pod share slurmd's nodes = %v\nrendered: %s\ngate: %q",
					gotBlocked, wantBlocked, renderedSelectors(rendered["slinky-slurm"]), placement)
			}
		})
	}
	if compared[true] == 0 || compared[false] == 0 {
		t.Fatalf("compared %d blocking and %d passing renders; the scenarios no longer exercise both verdicts",
			compared[true], compared[false])
	}
}

func placementSentinel() recipe.ComponentRef {
	return recipe.ComponentRef{Name: "nvsentinel", Namespace: "nvsentinel", Type: recipe.ComponentTypeHelm,
		Overrides: map[string]any{
			"global": map[string]any{"nodeDrainer": map[string]any{"enabled": true}, "dryRun": false},
			"node-drainer": map[string]any{"userNamespaces": []any{}, "customDrain": map[string]any{
				"enabled": true, "templateConfigMapName": "slinky-drain-template",
				"templateMountPath": "/etc/drain-template", "templateFileName": "drain-template.yaml",
				"namespace": "nvsentinel", "apiGroup": "nvsentinel.nvidia.com", "version": "v1alpha1",
				"kind": "DrainRequest", "resource": "drainrequests", "statusConditionType": "DrainComplete",
				"statusConditionStatus": "True", "timeout": "1800"}},
		}}
}

// controlPodCanShareSlurmdNode decides from rendered values whether some node
// could match both slurmd's selector and a Slurm control pod's: one could
// unless the two require different values of a shared key.
func controlPodCanShareSlurmdNode(values map[string]any) bool {
	worker := selectorAt(values, "nodesets.slinky.podSpec.nodeSelector")
	for _, path := range slinkyControlSelectorPaths {
		if !selectorsDisjoint(selectorAt(values, path), worker) {
			return true
		}
	}
	return false
}

func selectorAt(values map[string]any, path string) map[string]string {
	raw, _ := component.GetValueByPath(values, path)
	m, _ := raw.(map[string]any)
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = fmt.Sprint(v)
	}
	return out
}

// selectorsDisjoint reports whether no label set can satisfy both a and b.
func selectorsDisjoint(a, b map[string]string) bool {
	merged := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		merged[k] = v
	}
	for k, v := range b {
		merged[k] = v
	}
	return !labels.SelectorFromSet(a).Matches(labels.Set(merged)) ||
		!labels.SelectorFromSet(b).Matches(labels.Set(merged))
}

func renderedSelectors(values map[string]any) string {
	var b strings.Builder
	for _, p := range append([]string{"nodesets.slinky.podSpec.nodeSelector"}, slinkyControlSelectorPaths...) {
		fmt.Fprintf(&b, "%s=%v ", p, selectorAt(values, p))
	}
	return b.String()
}
