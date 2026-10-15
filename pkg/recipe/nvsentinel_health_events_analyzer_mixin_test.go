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

const (
	healthEventsAnalyzerMixin = "nvsentinel-health-events-analyzer"
	healthEventsAnalyzerImage = "ghcr.io/nvidia/nvsentinel/health-events-analyzer"
)

// healthEventsAnalyzerRules is the rule set #2618 asks to be chosen
// deliberately: every rule in the pinned chart, all enabled.
var healthEventsAnalyzerRules = []string{
	"MultipleRemediations",
	"RepeatedXIDErrorOnSameGPU",
	"RepeatedXID31OnSameGPU",
	"RepeatedXID31OnDifferentGPU",
	"RepeatedXID13OnSameGPCAndTPC",
	"RepeatedXID13OnDifferentGPCAndTPC",
	"XIDErrorSoloNoBurst",
	"XID74Reg0SoloNVLinkError",
	"XID74Reg0ECCParityError",
	"RepeatedXID74Reg0HardwareIssue",
	"XID74Reg0SignalIntegrityError",
	"XID74Reg0Bit27Or29Set",
	"RepeatedXID74Reg2HardwareIssue",
	"XID74Reg2Bit13Set",
	"RepeatedXID74Reg2Bit16Or19Set",
	"RepeatedXID74Reg2Bit17Or18Set",
	"XID74Reg3UnexpectedError",
	"XID74Reg3Bit18Set",
	"XID74Reg4HardwareIssue",
	"XID74Reg4ECCError",
	"RepeatedNICDriverError",
	"RepeatedNICDegradation",
}

// TestMixinNVSentinelHealthEventsAnalyzer_Composes pins what the mixin sets,
// alone and beside each remediation step: the analyzer on, STORE_ONLY, and
// every rule stated on, with nothing else.
func TestMixinNVSentinelHealthEventsAnalyzer_Composes(t *testing.T) {
	ctx, store := remediationStore(t)
	if _, ok := store.Mixins[healthEventsAnalyzerMixin]; !ok {
		t.Fatalf("%s mixin not present; check recipes/mixins/%s.yaml", healthEventsAnalyzerMixin, healthEventsAnalyzerMixin)
	}

	combos := make([][]string, 0, 1+len(remediationSteps))
	combos = append(combos, []string{healthEventsAnalyzerMixin})
	for _, step := range remediationSteps {
		combos = append(combos, []string{step.mixin, healthEventsAnalyzerMixin})
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
			section, _ := global["healthEventsAnalyzer"].(map[string]any)
			if len(section) != 1 || section["enabled"] != true {
				t.Errorf("global.healthEventsAnalyzer = %v, want exactly {enabled: true}", global["healthEventsAnalyzer"])
			}

			hea, _ := nvsentinel.Overrides["health-events-analyzer"].(map[string]any)
			if hea["processingStrategy"] != "STORE_ONLY" {
				t.Errorf("health-events-analyzer.processingStrategy = %v, want STORE_ONLY set explicitly", hea["processingStrategy"])
			}
			want := make([]string, 0, 1+len(healthEventsAnalyzerRules))
			want = append(want, "processingStrategy")
			for _, rule := range healthEventsAnalyzerRules {
				key := "enable" + rule + "Rule"
				want = append(want, key)
				if hea[key] != true {
					t.Errorf("health-events-analyzer.%s = %v, want true", key, hea[key])
				}
			}
			slices.Sort(want)
			if got := slices.Sorted(maps.Keys(hea)); !slices.Equal(got, want) {
				t.Errorf("health-events-analyzer overrides = %v, want exactly %v", got, want)
			}
		})
	}

	// Alone, the mixin touches nothing outside the analyzer.
	ref, ok := findComponentRefByName(store.Mixins[healthEventsAnalyzerMixin].Spec.ComponentRefs, "nvsentinel")
	if !ok {
		t.Fatal("mixin has no nvsentinel componentRef")
	}
	if got := slices.Sorted(maps.Keys(ref.Overrides)); !slices.Equal(got, []string{"global", "health-events-analyzer"}) {
		t.Errorf("mixin top-level overrides = %v, want [global health-events-analyzer]", got)
	}
	global, _ := ref.Overrides["global"].(map[string]any)
	if got := slices.Sorted(maps.Keys(global)); !slices.Equal(got, []string{"healthEventsAnalyzer"}) {
		t.Errorf("mixin global overrides = %v, want [healthEventsAnalyzer]", got)
	}
	if n := len(store.Mixins[healthEventsAnalyzerMixin].Spec.ComponentRefs); n != 1 {
		t.Errorf("mixin has %d componentRefs, want only nvsentinel: the datastore comes from a remediation step", n)
	}
}

// TestNoShippedRecipeAdoptsHealthEventsAnalyzer keeps the analyzer opt-in:
// no overlay composes the mixin, and no resolved recipe turns it on.
func TestNoShippedRecipeAdoptsHealthEventsAnalyzer(t *testing.T) {
	ctx, store := remediationStore(t)

	if slices.Contains(store.Base.Spec.Mixins, healthEventsAnalyzerMixin) {
		t.Errorf("recipes/overlays/base.yaml adopts %s; the analyzer stays opt-in", healthEventsAnalyzerMixin)
	}
	for name, overlay := range store.Overlays {
		if slices.Contains(overlay.Spec.Mixins, healthEventsAnalyzerMixin) {
			t.Errorf("overlay %q adopts %s; the analyzer stays opt-in", name, healthEventsAnalyzerMixin)
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
		if raw, present := global["healthEventsAnalyzer"]; present {
			section, isMap := raw.(map[string]any)
			if !isMap || section["enabled"] != false {
				t.Errorf("%s resolves with global.healthEventsAnalyzer = %v; it must be absent or off", name, raw)
			}
		}
	}
	if resolved == 0 {
		t.Fatal("no shipped recipes were resolved -- the walker is broken")
	}
}

// TestHealthEventsAnalyzerImagePinnedEverywhere keeps every hand-maintained
// copy of the analyzer image in step with nvsentinel's registry pin. The
// subchart image follows the parent chart's version, so a bump moves the
// deployed image while these copies keep naming the old one.
func TestHealthEventsAnalyzerImagePinnedEverywhere(t *testing.T) {
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

	assertScanned(t, healthEventsAnalyzerImage, version)
	for _, s := range []struct{ path, why string }{
		{"../../tools/mirror-e2e", "the mirror lane asserts every mixin-gated image"},
		{"../../docs/user/container-images.md", "the static BOM cannot see a mixin-gated image, so it is documented by hand"},
		{"../../pkg/bundler/validations/nvsentinel_health_events_analyzer_render_test.go", "the render test asserts this image reaches the chart"},
	} {
		data, err := os.ReadFile(s.path)
		if err != nil {
			t.Errorf("reading %s: %v", s.path, err)
			continue
		}
		if ref := fmt.Sprintf("%s:%s", healthEventsAnalyzerImage, version); !strings.Contains(string(data), ref) {
			t.Errorf("%s does not pin %s -- %s; bump it alongside nvsentinel's defaultVersion", s.path, ref, s.why)
		}
	}
}
