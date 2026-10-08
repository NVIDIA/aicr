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
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	slurmDrainMonitorMixin = "nvsentinel-slurm-drain-monitor"
	slurmDrainMonitorImage = "ghcr.io/nvidia/nvsentinel/slurm-drain-monitor"
	// slurmDrainMonitorSelector is the worker-pod selector read off the Slinky
	// slurm-operator source at slurmDrainMonitorSelectorVerifiedAt.
	slurmDrainMonitorSelector = "app.kubernetes.io/name=slurmd,app.kubernetes.io/component=worker"
	// slurmDrainMonitorSelectorVerifiedAt is the slinky-slurm-operator version
	// the selector and the SlurmNodeStateDrain condition were verified against.
	slurmDrainMonitorSelectorVerifiedAt = "1.2.2"
)

// slurmDrainMonitorDeployment is the workload every platform: slurm leaf asks
// deployment validation to verify.
var slurmDrainMonitorDeployment = ExpectedResource{Kind: "Deployment", Namespace: "nvsentinel", Name: "slurm-drain-monitor"}

// slurmDrainMonitorStore loads the real embedded catalog, so every test below
// exercises the shipped mixin and leaves rather than a fixture.
func slurmDrainMonitorStore(t *testing.T) (context.Context, *MetadataStore) {
	t.Helper()
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}
	if _, ok := store.Mixins[slurmDrainMonitorMixin]; !ok {
		t.Fatalf("%s mixin not present; check recipes/mixins/%s.yaml", slurmDrainMonitorMixin, slurmDrainMonitorMixin)
	}
	return ctx, store
}

// slurmDrainMonitorValues returns the slurm-drain-monitor subchart overrides
// on an nvsentinel ref, failing if they are absent.
func slurmDrainMonitorValues(t *testing.T, ref ComponentRef) map[string]any {
	t.Helper()
	sdm, _ := ref.Overrides["slurm-drain-monitor"].(map[string]any)
	if sdm == nil {
		t.Fatal("overrides.slurm-drain-monitor missing")
	}
	return sdm
}

// TestMixinNVSentinelSlurmDrainMonitor_ComposesCleanly proves the shipped
// mixin composes onto an already-nvsentinel-chained leaf and sets every value
// #2611 requires explicitly, rather than inheriting a chart default.
func TestMixinNVSentinelSlurmDrainMonitor_ComposesCleanly(t *testing.T) {
	ctx, store := slurmDrainMonitorStore(t)

	spec := nvsentinelLeaf([]string{slurmDrainMonitorMixin}, nil)
	if _, err := store.mergeMixins(ctx, &spec); err != nil {
		t.Fatalf("mergeMixins: %v", err)
	}
	nvsentinel, ok := findComponentRefByName(spec.ComponentRefs, "nvsentinel")
	if !ok {
		t.Fatal("nvsentinel component missing from merged spec")
	}

	global, _ := nvsentinel.Overrides["global"].(map[string]any)
	sdmGlobal, _ := global["slurmDrainMonitor"].(map[string]any)
	if sdmGlobal == nil || sdmGlobal["enabled"] != true {
		t.Fatalf("global.slurmDrainMonitor.enabled = %v, want true", sdmGlobal)
	}

	sdm := slurmDrainMonitorValues(t, nvsentinel)
	for key, want := range map[string]string{
		"namespace":          "slurm",
		"labelSelector":      slurmDrainMonitorSelector,
		"processingStrategy": "STORE_ONLY",
	} {
		if got := sdm[key]; got != want {
			t.Errorf("slurm-drain-monitor.%s = %v, want %q", key, got, want)
		}
	}

	// The reason-to-action mapping is pinned in the mixin, not inherited.
	patterns, _ := sdm["patterns"].([]any)
	if len(patterns) != 1 {
		t.Fatalf("slurm-drain-monitor.patterns has %d entries, want exactly the [HC] pattern", len(patterns))
	}
	pattern, _ := patterns[0].(map[string]any)
	for key, want := range map[string]any{
		"regex":             `^\[HC\]`,
		"checkName":         "SlurmHealthCheck",
		"componentClass":    "NODE",
		"isFatal":           false,
		"recommendedAction": "CONTACT_SUPPORT",
	} {
		if got := pattern[key]; got != want {
			t.Errorf("slurm-drain-monitor.patterns[0].%s = %v, want %v", key, got, want)
		}
	}
}

// TestSlurmDrainMonitorScopedToSlurmLeaves pins where the monitor ships: on
// every platform: slurm leaf, and nowhere else. A non-Slurm recipe has no
// Slinky worker pods to watch, and CheckNVSentinelSlurmDrainMonitorRequiresSlinky
// would reject its bundle.
//
// Checked at two levels: the declaration scan misses a leaf that picks the
// mixin up through its base chain, and the resolution scan proves each Slurm
// leaf resolves with the monitor watching the namespace slinky-slurm deploys
// to there.
func TestSlurmDrainMonitorScopedToSlurmLeaves(t *testing.T) {
	ctx, store := slurmDrainMonitorStore(t)

	slurmLeaves := 0
	for name, overlay := range store.Overlays {
		isSlurm := overlay.Spec.Criteria != nil && overlay.Spec.Criteria.Platform == CriteriaPlatformSlurm
		declares := slices.Contains(overlay.Spec.Mixins, slurmDrainMonitorMixin)
		switch {
		case isSlurm && !declares:
			t.Errorf("platform: slurm leaf %s does not declare the %s mixin", name, slurmDrainMonitorMixin)
		case !isSlurm && declares:
			t.Errorf("overlay %s declares the %s mixin but is not a platform: slurm leaf", name, slurmDrainMonitorMixin)
		}
		if isSlurm {
			slurmLeaves++
		}
	}
	if slurmLeaves == 0 {
		t.Fatal("no platform: slurm leaves were found -- the overlay walker is broken")
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
		nvsentinel, ok := findComponentRefByName(result.ComponentRefs, "nvsentinel")
		if !ok {
			continue
		}
		global, _ := nvsentinel.Overrides["global"].(map[string]any)
		_, enabled := global["slurmDrainMonitor"]
		_, configured := nvsentinel.Overrides["slurm-drain-monitor"]

		expectsMonitor := slices.Contains(nvsentinel.ExpectedResources, slurmDrainMonitorDeployment)
		if overlay.Spec.Criteria.Platform != CriteriaPlatformSlurm {
			if enabled || configured || expectsMonitor {
				t.Errorf("%s resolves with slurm-drain-monitor settings on nvsentinel but is not a platform: slurm leaf", name)
			}
			continue
		}
		// Normal deployment validation must notice an absent monitor; the
		// registry's nvsentinel check runs on every recipe and cannot.
		if !expectsMonitor {
			t.Errorf("%s: nvsentinel expectedResources %v do not include %+v", name, nvsentinel.ExpectedResources, slurmDrainMonitorDeployment)
		}
		if !slices.Contains(result.Metadata.AppliedOverlays, name) {
			t.Errorf("%s: criteria resolved to %v, which does not include it", name, result.Metadata.AppliedOverlays)
			continue
		}
		if !enabled || !configured {
			t.Errorf("%s resolves without the slurm-drain-monitor settings", name)
			continue
		}
		slinky, ok := findComponentRefByName(result.ComponentRefs, "slinky-slurm")
		if !ok {
			t.Errorf("%s resolved without slinky-slurm", name)
			continue
		}
		slinkyNamespace := slinky.Namespace
		if slinkyNamespace == "" {
			slinkyNamespace = "slurm"
		}
		if got := slurmDrainMonitorValues(t, nvsentinel)["namespace"]; got != slinkyNamespace {
			t.Errorf("%s: slurm-drain-monitor.namespace = %v but slinky-slurm deploys to %q", name, got, slinkyNamespace)
		}
	}
	if resolved == 0 {
		t.Fatal("no shipped recipes were resolved -- the walker is broken")
	}
}

// TestSlurmDrainMonitorOptOutLeaf pins the supported opt-out: an external
// --data copy of a Slurm leaf that drops both the mixin and nvsentinel's
// expectedResources entry resolves with the monitor neither enabled nor
// expected, so bundle and deployment validation agree. A bundle-time --set
// alone cannot do this, because aicr validate resolves values from the recipe.
func TestSlurmDrainMonitorOptOutLeaf(t *testing.T) {
	const leaf = "h100-eks-ubuntu-training-slurm"
	shipped, err := os.ReadFile(filepath.Join("..", "..", "recipes", "overlays", leaf+".yaml"))
	if err != nil {
		t.Fatalf("reading shipped leaf: %v", err)
	}
	var doc map[string]any
	if err = yaml.Unmarshal(shipped, &doc); err != nil {
		t.Fatalf("parsing shipped leaf: %v", err)
	}
	spec, _ := doc["spec"].(map[string]any)
	mixins, _ := spec["mixins"].([]any)
	keptMixins := make([]any, 0, len(mixins))
	for _, m := range mixins {
		if m != slurmDrainMonitorMixin {
			keptMixins = append(keptMixins, m)
		}
	}
	if len(keptMixins) == len(mixins) {
		t.Fatalf("shipped %s does not compose %s; the opt-out has nothing to remove", leaf, slurmDrainMonitorMixin)
	}
	spec["mixins"] = keptMixins
	refs, _ := spec["componentRefs"].([]any)
	keptRefs := make([]any, 0, len(refs))
	for _, r := range refs {
		if ref, _ := r.(map[string]any); ref["name"] != "nvsentinel" {
			keptRefs = append(keptRefs, r)
		}
	}
	if len(keptRefs) == len(refs) {
		t.Fatalf("shipped %s has no nvsentinel componentRef; the opt-out has no expectedResources to remove", leaf)
	}
	spec["componentRefs"] = keptRefs
	optOut, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("marshaling opt-out leaf: %v", err)
	}

	dir := t.TempDir()
	if err = os.MkdirAll(filepath.Join(dir, "overlays"), 0o750); err != nil {
		t.Fatalf("creating overlays dir: %v", err)
	}
	registry := "apiVersion: aicr.run/v1beta1\nkind: ComponentRegistry\ncomponents: []\n"
	if err = os.WriteFile(filepath.Join(dir, "registry.yaml"), []byte(registry), 0o600); err != nil {
		t.Fatalf("writing registry.yaml: %v", err)
	}
	if err = os.WriteFile(filepath.Join(dir, "overlays", leaf+".yaml"), optOut, 0o600); err != nil {
		t.Fatalf("writing opt-out leaf: %v", err)
	}
	layered, err := NewLayeredDataProvider(NewEmbeddedDataProvider(GetEmbeddedFS(), "."), LayeredProviderConfig{ExternalDir: dir})
	if err != nil {
		t.Fatalf("NewLayeredDataProvider: %v", err)
	}
	t.Cleanup(func() {
		EvictCachedStore(layered)
		EvictCachedRegistry(layered)
		EvictCachedCriteriaRegistry(layered)
	})

	ctx := context.Background()
	store, err := LoadMetadataStoreFor(ctx, layered)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	overlay, ok := store.Overlays[leaf]
	if !ok {
		t.Fatalf("%s missing from the layered catalog", leaf)
	}
	result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
	if err != nil {
		t.Fatalf("resolving the opt-out %s: %v", leaf, err)
	}
	nvsentinel, ok := findComponentRefByName(result.ComponentRefs, "nvsentinel")
	if !ok {
		t.Fatalf("opt-out %s resolved without nvsentinel", leaf)
	}
	global, _ := nvsentinel.Overrides["global"].(map[string]any)
	if _, present := global["slurmDrainMonitor"]; present {
		t.Errorf("opt-out %s still resolves with global.slurmDrainMonitor", leaf)
	}
	if _, present := nvsentinel.Overrides["slurm-drain-monitor"]; present {
		t.Errorf("opt-out %s still resolves with slurm-drain-monitor overrides", leaf)
	}
	if slices.Contains(nvsentinel.ExpectedResources, slurmDrainMonitorDeployment) {
		t.Errorf("opt-out %s still expects %+v; deployment validation would fail a bundle that never deployed it", leaf, slurmDrainMonitorDeployment)
	}
}

// TestSlurmDrainMonitorImagePinnedEverywhere keeps every hand-maintained copy
// of the monitor image in step with nvsentinel's registry pin, and requires
// each surface to carry it at all. The subchart image inherits the parent
// chart's version, so a bump moves the deployed image while these copies keep
// naming the old one -- and dropping it from the scan or mirror would break
// #2611's coverage criterion with every other test green.
func TestSlurmDrainMonitorImagePinnedEverywhere(t *testing.T) {
	_, store := slurmDrainMonitorStore(t)
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

	const scanWorkflow = "../../.github/workflows/vuln-scan-images.yaml"
	tag, found := scanMatrixTagFor(t, scanWorkflow, slurmDrainMonitorImage)
	switch {
	case !found:
		t.Errorf("%s has no scan matrix entry for %s -- #2611 requires the scan job to cover this image", scanWorkflow, slurmDrainMonitorImage)
	case tag != version:
		t.Errorf("%s pins tag %q for %s, want nvsentinel's defaultVersion %q", scanWorkflow, tag, slurmDrainMonitorImage, version)
	}

	for _, s := range []struct{ path, why string }{
		{"../../tools/mirror-e2e", "#2611 requires the mirror job to cover this image"},
		{"../../docs/user/container-images.md", "#2611 requires the image to be documented"},
		{"../../pkg/bundler/validations/nvsentinel_slurm_drain_monitor_render_test.go", "the render test asserts this image reaches the chart"},
	} {
		data, err := os.ReadFile(s.path)
		if err != nil {
			t.Errorf("reading %s: %v", s.path, err)
			continue
		}
		body := string(data)
		if !strings.Contains(body, slurmDrainMonitorImage) {
			t.Errorf("%s no longer references %s -- %s", s.path, slurmDrainMonitorImage, s.why)
			continue
		}
		if !strings.Contains(body, slurmDrainMonitorImage+":"+version) {
			t.Errorf("%s does not pin %s:%s; bump it alongside nvsentinel's defaultVersion", s.path, slurmDrainMonitorImage, version)
		}
	}
}

// TestSlurmDrainMonitorSelectorPinnedToVerifiedSlinkyVersion is the drift
// guard nothing else in this repo can be. The worker-pod labels and the
// SlurmNodeStateDrain condition come from the Slinky slurm-operator's
// controller, not from any chart AICR renders, so a rename upstream would
// leave the monitor watching nothing with every test green. Bumping the
// operator therefore fails here until someone re-reads them.
func TestSlurmDrainMonitorSelectorPinnedToVerifiedSlinkyVersion(t *testing.T) {
	_, store := slurmDrainMonitorStore(t)
	registry, err := GetComponentRegistryFor(store.provider)
	if err != nil {
		t.Fatalf("GetComponentRegistryFor: %v", err)
	}
	operator := registry.Get("slinky-slurm-operator")
	if operator == nil {
		t.Fatal("slinky-slurm-operator not found in registry")
	}
	if got := operator.Helm.DefaultVersion; got != slurmDrainMonitorSelectorVerifiedAt {
		t.Errorf("slinky-slurm-operator is pinned at %q but the slurm-drain-monitor selector %q was only verified against %q.\n"+
			"Re-verify before shipping this bump, in the slurm-operator source at the new tag:\n"+
			"  internal/builder/labels/labels.go  WithWorkerLabels still sets app.kubernetes.io/name=slurmd and app.kubernetes.io/component=worker\n"+
			"  internal/controller/nodeset/slurmcontrol  the drain condition is still SlurmNodeStateDrain with .Message = the Slurm reason,\n"+
			"                                            and operator drains still carry the \"slurm-operator: \" prefix\n"+
			"or on a live cluster: kubectl get pods -n slurm --show-labels. Then update slurmDrainMonitorSelectorVerifiedAt,\n"+
			"or recipes/mixins/%s.yaml if anything changed.",
			got, slurmDrainMonitorSelector, slurmDrainMonitorSelectorVerifiedAt, slurmDrainMonitorMixin)
	}

	// The mixin must still set the selector explicitly. Without this the pin
	// above keeps passing after the line is deleted and the chart default,
	// which nothing here verifies, takes over.
	ref, ok := findComponentRefByName(store.Mixins[slurmDrainMonitorMixin].Spec.ComponentRefs, "nvsentinel")
	if !ok {
		t.Fatal("mixin has no nvsentinel componentRef")
	}
	if got := slurmDrainMonitorValues(t, ref)["labelSelector"]; got != slurmDrainMonitorSelector {
		t.Errorf("mixin labelSelector = %v, want the verified %q", got, slurmDrainMonitorSelector)
	}
}
