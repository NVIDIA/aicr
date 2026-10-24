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

package recipe

import (
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
)

const (
	eventExporterMixin = "nvsentinel-event-exporter"
	eventExporterImage = "ghcr.io/nvidia/nvsentinel/event-exporter"
)

// eventExporterMixinSection returns the mixin's event-exporter overrides.
func eventExporterMixinSection(t *testing.T, store *MetadataStore) map[string]any {
	t.Helper()
	m, ok := store.Mixins[eventExporterMixin]
	if !ok {
		t.Fatalf("%s mixin not present; check recipes/mixins/%s.yaml", eventExporterMixin, eventExporterMixin)
	}
	ref, ok := findComponentRefByName(m.Spec.ComponentRefs, "nvsentinel")
	if !ok {
		t.Fatalf("%s has no nvsentinel componentRef", eventExporterMixin)
	}
	section, _ := ref.Overrides["event-exporter"].(map[string]any)
	return section
}

// TestMixinNVSentinelEventExporter_Composes pins what the mixin sets, alone
// and beside each remediation step: the exporter on, the Secret it mounts,
// certificate checks on and the filter, and none of the per-deployment
// values, which the bundle command supplies.
func TestMixinNVSentinelEventExporter_Composes(t *testing.T) {
	ctx, store := remediationStore(t)
	eventExporterMixinSection(t, store)

	combos := make([][]string, 0, 1+len(remediationSteps))
	combos = append(combos, []string{eventExporterMixin})
	for _, step := range remediationSteps {
		combos = append(combos, []string{step.mixin, eventExporterMixin})
	}
	for _, mixins := range combos {
		t.Run(strings.Join(mixins, "+"), func(t *testing.T) {
			spec := remediationLeaf(mixins...)
			if _, err := store.mergeMixins(ctx, &spec); err != nil {
				t.Fatalf("mergeMixins: %v", err)
			}
			nvsentinel, ok := findComponentRefByName(spec.ComponentRefs, "nvsentinel")
			if !ok {
				t.Fatal("nvsentinel missing from merged spec")
			}
			global, _ := nvsentinel.Overrides["global"].(map[string]any)
			if section, _ := global["eventExporter"].(map[string]any); len(section) != 1 || section["enabled"] != true {
				t.Errorf("global.eventExporter = %v, want exactly {enabled: true}", global["eventExporter"])
			}
			ee, _ := nvsentinel.Overrides["event-exporter"].(map[string]any)
			if got := slices.Sorted(maps.Keys(ee)); !slices.Equal(got, []string{"exporter", "oidcSecretName"}) {
				t.Errorf("event-exporter overrides = %v, want exactly [exporter oidcSecretName]", got)
			}
			if ee["oidcSecretName"] != "event-exporter-oidc-secret" {
				t.Errorf("event-exporter.oidcSecretName = %v, want event-exporter-oidc-secret", ee["oidcSecretName"])
			}
			exporter, _ := ee["exporter"].(map[string]any)
			want := map[string]map[string]any{
				"sink":   {"insecureSkipVerify": false},
				"oidc":   {"insecureSkipVerify": false},
				"filter": {"expression": "event.recommendedAction != 'NONE' || event.isHealthy == true"},
			}
			if got := slices.Sorted(maps.Keys(exporter)); !slices.Equal(got, slices.Sorted(maps.Keys(want))) {
				t.Errorf("event-exporter.exporter keys = %v, want exactly %v; the sink endpoint, OIDC client and "+
					"metadata are per deployment and come from the bundle command", got, slices.Sorted(maps.Keys(want)))
			}
			for key, w := range want {
				if got, _ := exporter[key].(map[string]any); !maps.Equal(got, w) {
					t.Errorf("event-exporter.exporter.%s = %v, want exactly %v", key, got, w)
				}
			}
		})
	}

	ref, _ := findComponentRefByName(store.Mixins[eventExporterMixin].Spec.ComponentRefs, "nvsentinel")
	if got := slices.Sorted(maps.Keys(ref.Overrides)); !slices.Equal(got, []string{"event-exporter", "global"}) {
		t.Errorf("mixin top-level overrides = %v, want [event-exporter global]", got)
	}
	if n := len(store.Mixins[eventExporterMixin].Spec.ComponentRefs); n != 1 {
		t.Errorf("mixin has %d componentRefs, want only nvsentinel: the datastore comes from a remediation step", n)
	}
}

// TestNVSentinelEventExporterFilter evaluates the mixin's filter as the
// exporter does, with event bound as a dynamically typed map, and pins what it
// drops: unhealthy events that recommend no action. Actionable faults of
// every kind and recoveries, which also recommend NONE, are exported.
func TestNVSentinelEventExporterFilter(t *testing.T) {
	_, store := remediationStore(t)
	filter, _ := eventExporterMixinSection(t, store)["exporter"].(map[string]any)["filter"].(map[string]any)
	expr, _ := filter["expression"].(string)

	env, err := cel.NewEnv(cel.Variable("event", cel.MapType(cel.StringType, cel.DynType)))
	if err != nil {
		t.Fatalf("cel.NewEnv: %v", err)
	}
	ast, issues := env.Compile(expr)
	if issues.Err() != nil {
		t.Fatalf("compiling %q: %v", expr, issues.Err())
	}
	if ast.OutputType() != cel.BoolType {
		t.Fatalf("%q evaluates to %v; the exporter refuses a filter that is not boolean", expr, ast.OutputType())
	}
	prg, err := env.Program(ast)
	if err != nil {
		t.Fatalf("cel.Program: %v", err)
	}

	event := func(healthy, fatal bool, action string) map[string]any {
		return map[string]any{
			"agent": "gpu-health-monitor", "checkName": "GpuXidError", "componentClass": "GPU",
			"errorCode": []string{"79"}, "isFatal": fatal, "isHealthy": healthy, "recommendedAction": action,
			"nodeName": "gpu-node-1", "metadata": map[string]string{}, "message": "synthetic",
		}
	}
	tests := []struct {
		name   string
		event  map[string]any
		export bool
	}{
		{"fatal fault recommending a reset", event(false, true, "COMPONENT_RESET"), true},
		{"fatal fault recommending a reboot", event(false, true, "RESTART_VM"), true},
		{"non-fatal fault recommending support", event(false, false, "CONTACT_SUPPORT"), true},
		{"recovery", event(true, false, "NONE"), true},
		{"informational event recommending no action", event(false, false, "NONE"), false},
		{"fatal event recommending no action", event(false, true, "NONE"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, _, err := prg.Eval(map[string]any{"event": tt.event})
			if err != nil {
				t.Fatalf("evaluating %q: %v; the exporter fails open and would export it", expr, err)
			}
			if got := out.Value(); got != tt.export {
				t.Errorf("filter on %v = %v, want %v", tt.event, got, tt.export)
			}
		})
	}
}

// TestNoShippedRecipeAdoptsEventExporter keeps the exporter an operator's
// choice: no overlay composes the mixin, and no resolved recipe turns it on.
func TestNoShippedRecipeAdoptsEventExporter(t *testing.T) {
	ctx, store := remediationStore(t)

	if slices.Contains(store.Base.Spec.Mixins, eventExporterMixin) {
		t.Errorf("recipes/overlays/base.yaml adopts %s; the exporter stays opt-in", eventExporterMixin)
	}
	for name, overlay := range store.Overlays {
		if slices.Contains(overlay.Spec.Mixins, eventExporterMixin) {
			t.Errorf("overlay %q adopts %s; the exporter stays opt-in", name, eventExporterMixin)
		}
	}

	resolved := 0
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
		if err != nil {
			t.Errorf("resolving %s: %v", name, err)
			continue
		}
		resolved++
		if _, ok := findComponentRefByName(result.ComponentRefs, "nvsentinel"); !ok {
			continue
		}
		values, err := result.GetValuesForComponentWithContext(ctx, "nvsentinel")
		if err != nil {
			t.Errorf("%s: GetValuesForComponentWithContext: %v", name, err)
			continue
		}
		global, _ := values["global"].(map[string]any)
		if raw, present := global["eventExporter"]; present {
			section, isMap := raw.(map[string]any)
			if !isMap || section["enabled"] != false {
				t.Errorf("%s resolves with global.eventExporter = %v; it must be absent or off", name, raw)
			}
		}
	}
	if resolved == 0 {
		t.Fatal("no shipped recipes were resolved -- the walker is broken")
	}
}

// TestEventExporterImagePinnedEverywhere keeps every hand-maintained copy of
// the exporter image in step with nvsentinel's registry pin. It follows the
// chart's version, so a bump moves the deployed image while these copies keep
// naming the old one.
func TestEventExporterImagePinnedEverywhere(t *testing.T) {
	_, store := remediationStore(t)
	registry, err := GetComponentRegistryFor(store.provider)
	if err != nil {
		t.Fatalf("GetComponentRegistryFor: %v", err)
	}
	nvsentinel := registry.Get("nvsentinel")
	if nvsentinel == nil {
		t.Fatal("nvsentinel not found in registry")
	}
	version := nvsentinel.Helm.DefaultVersion
	if version == "" {
		t.Fatal("nvsentinel registry entry has no defaultVersion")
	}

	assertScanned(t, eventExporterImage, version)
	for _, s := range []struct{ path, why string }{
		{"../../tools/mirror-e2e", "the mirror lane asserts every mixin-gated image"},
		{"../../docs/user/container-images.md", "the static BOM cannot see a mixin-gated image, so it is documented by hand"},
		{"../../pkg/bundler/validations/nvsentinel_event_exporter_render_test.go", "the render test asserts this image reaches the chart"},
	} {
		data, err := os.ReadFile(s.path)
		if err != nil {
			t.Errorf("reading %s: %v", s.path, err)
			continue
		}
		if ref := fmt.Sprintf("%s:%s", eventExporterImage, version); !strings.Contains(string(data), ref) {
			t.Errorf("%s does not pin %s -- %s; bump it alongside nvsentinel's defaultVersion", s.path, ref, s.why)
		}
	}
}
