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
)

// cspHealthMonitorMixins maps each CSP Health Monitor mixin to the cspName it
// fixes.
var cspHealthMonitorMixins = map[string]string{
	"nvsentinel-csp-health-monitor-aws": "aws",
	"nvsentinel-csp-health-monitor-gcp": "gcp",
}

// cspHealthMonitorImages follow nvsentinel's chart version.
var cspHealthMonitorImages = []string{
	"ghcr.io/nvidia/nvsentinel/csp-health-monitor",
	"ghcr.io/nvidia/nvsentinel/maintenance-notifier",
}

// TestMixinNVSentinelCSPHealthMonitor_Composes pins what each mixin sets,
// alone and beside each remediation step: the monitor on, its cloud,
// STORE_ONLY and the lead time, and none of the per-cluster values, which
// the bundle command supplies.
func TestMixinNVSentinelCSPHealthMonitor_Composes(t *testing.T) {
	ctx, store := remediationStore(t)

	for mixin, cloud := range cspHealthMonitorMixins {
		if _, ok := store.Mixins[mixin]; !ok {
			t.Fatalf("%s mixin not present; check recipes/mixins/%s.yaml", mixin, mixin)
		}
		combos := make([][]string, 0, 1+len(remediationSteps))
		combos = append(combos, []string{mixin})
		for _, step := range remediationSteps {
			combos = append(combos, []string{step.mixin, mixin})
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
				if section, _ := global["cspHealthMonitor"].(map[string]any); len(section) != 1 || section["enabled"] != true {
					t.Errorf("global.cspHealthMonitor = %v, want exactly {enabled: true}", global["cspHealthMonitor"])
				}
				csp, _ := nvsentinel.Overrides["csp-health-monitor"].(map[string]any)
				if got := slices.Sorted(maps.Keys(csp)); !slices.Equal(got, []string{"configToml", "cspName", "processingStrategy"}) {
					t.Errorf("csp-health-monitor overrides = %v, want exactly [configToml cspName processingStrategy]", got)
				}
				if csp["cspName"] != cloud {
					t.Errorf("csp-health-monitor.cspName = %v, want %s", csp["cspName"], cloud)
				}
				if csp["processingStrategy"] != "STORE_ONLY" {
					t.Errorf("csp-health-monitor.processingStrategy = %v, want STORE_ONLY set explicitly", csp["processingStrategy"])
				}
				toml, _ := csp["configToml"].(map[string]any)
				if !maps.Equal(toml, map[string]any{"triggerQuarantineWorkflowTimeLimitMinutes": 30}) {
					t.Errorf("csp-health-monitor.configToml = %v, want exactly the lead time; clusterName and "+
						"the cloud identity are per cluster and come from the bundle command", toml)
				}
			})
		}

		ref, ok := findComponentRefByName(store.Mixins[mixin].Spec.ComponentRefs, "nvsentinel")
		if !ok {
			t.Fatalf("%s has no nvsentinel componentRef", mixin)
		}
		if got := slices.Sorted(maps.Keys(ref.Overrides)); !slices.Equal(got, []string{"csp-health-monitor", "global"}) {
			t.Errorf("%s top-level overrides = %v, want [csp-health-monitor global]", mixin, got)
		}
		if n := len(store.Mixins[mixin].Spec.ComponentRefs); n != 1 {
			t.Errorf("%s has %d componentRefs, want only nvsentinel: the datastore comes from a remediation step", mixin, n)
		}
	}
}

// TestNoShippedRecipeAdoptsCSPHealthMonitor keeps the monitor an operator's
// choice: no overlay composes either mixin, and no resolved recipe turns it
// on, since each needs cloud identity AICR does not create.
func TestNoShippedRecipeAdoptsCSPHealthMonitor(t *testing.T) {
	ctx, store := remediationStore(t)

	for mixin := range cspHealthMonitorMixins {
		if slices.Contains(store.Base.Spec.Mixins, mixin) {
			t.Errorf("recipes/overlays/base.yaml adopts %s; the CSP Health Monitor stays opt-in", mixin)
		}
		for name, overlay := range store.Overlays {
			if slices.Contains(overlay.Spec.Mixins, mixin) {
				t.Errorf("overlay %q adopts %s; the CSP Health Monitor stays opt-in", name, mixin)
			}
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
		if raw, present := global["cspHealthMonitor"]; present {
			section, isMap := raw.(map[string]any)
			if !isMap || section["enabled"] != false {
				t.Errorf("%s resolves with global.cspHealthMonitor = %v; it must be absent or off", name, raw)
			}
		}
	}
	if resolved == 0 {
		t.Fatal("no shipped recipes were resolved -- the walker is broken")
	}
}

// TestCSPHealthMonitorImagesPinnedEverywhere keeps every hand-maintained copy
// of the monitor's images in step with nvsentinel's registry pin. Both follow
// the chart's version, so a bump moves the deployed images while these
// copies keep naming the old ones.
func TestCSPHealthMonitorImagesPinnedEverywhere(t *testing.T) {
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

	for _, image := range cspHealthMonitorImages {
		assertScanned(t, image, version)
	}
	for _, s := range []struct{ path, why string }{
		{"../../tools/mirror-e2e", "the mirror lane asserts every mixin-gated image"},
		{"../../docs/user/container-images.md", "the static BOM cannot see a mixin-gated image, so it is documented by hand"},
		{"../../pkg/bundler/validations/nvsentinel_csp_health_monitor_render_test.go", "the render test asserts each image reaches the chart"},
	} {
		data, err := os.ReadFile(s.path)
		if err != nil {
			t.Errorf("reading %s: %v", s.path, err)
			continue
		}
		for _, image := range cspHealthMonitorImages {
			if ref := fmt.Sprintf("%s:%s", image, version); !strings.Contains(string(data), ref) {
				t.Errorf("%s does not pin %s -- %s; bump it alongside nvsentinel's defaultVersion", s.path, ref, s.why)
			}
		}
	}
}
