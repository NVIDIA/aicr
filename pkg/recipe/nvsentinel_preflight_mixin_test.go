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
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const preflightMixin = "nvsentinel-preflight"

// preflightStore loads the real embedded catalog, so these tests exercise the
// shipped mixin rather than a fixture.
func preflightStore(t *testing.T) (context.Context, *MetadataStore) {
	t.Helper()
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}
	if _, ok := store.Mixins[preflightMixin]; !ok {
		t.Fatalf("%s mixin not present; check recipes/mixins/%s.yaml", preflightMixin, preflightMixin)
	}
	return ctx, store
}

// preflightUncachedStore builds a store that bypasses LoadMetadataStoreFor's
// process-wide cache. Tests that compose the mixin by writing into
// store.Overlays must use this: the cached store is shared by every test in
// the package, so an in-place write would leave the mixin composed onto all
// 126 leaves for every test that runs afterwards.
func preflightUncachedStore(t *testing.T) (context.Context, *MetadataStore) {
	t.Helper()
	ctx := context.Background()
	store, err := buildMetadataStore(ctx, defaultEmbeddedProvider)
	if err != nil {
		t.Fatalf("buildMetadataStore: %v", err)
	}
	if _, ok := store.Mixins[preflightMixin]; !ok {
		t.Fatalf("%s mixin not present; check recipes/mixins/%s.yaml", preflightMixin, preflightMixin)
	}
	return ctx, store
}

// preflightLeaf builds a leaf whose chain already carries nvsentinel with the
// base chain's own dependency edges, which is what makes the mixin's
// dependencyRefs an additive merge rather than a fresh assignment.
func preflightLeaf(mixins []string, deps []string) RecipeMetadataSpec {
	return RecipeMetadataSpec{
		Mixins: mixins,
		ComponentRefs: []ComponentRef{
			{Name: "kai-scheduler", Chart: "kai-scheduler", Version: "v0.16.9", Source: "oci://ghcr.io/kai-scheduler", Type: ComponentTypeHelm},
			{
				Name: "nvsentinel", Chart: "nvsentinel", Version: "v1.22.0",
				Source: "oci://ghcr.io/nvidia", Type: ComponentTypeHelm,
				Namespace: "nvsentinel", DependencyRefs: deps,
			},
		},
	}
}

// TestMixinNVSentinelPreflight_ComposesCleanly proves the shipped mixin merges
// onto an already-nvsentinel-chained leaf and produces the documented values.
func TestMixinNVSentinelPreflight_ComposesCleanly(t *testing.T) {
	ctx, store := preflightStore(t)

	spec := preflightLeaf([]string{preflightMixin}, []string{"cert-manager"})
	if _, err := store.mergeMixins(ctx, &spec); err != nil {
		t.Fatalf("mergeMixins: %v", err)
	}

	nvsentinel, ok := findComponentRefByName(spec.ComponentRefs, "nvsentinel")
	if !ok {
		t.Fatal("nvsentinel missing from merged spec")
	}

	global, _ := nvsentinel.Overrides["global"].(map[string]any)
	gp, _ := global["preflight"].(map[string]any)
	if gp == nil || gp["enabled"] != true {
		t.Fatalf("global.preflight.enabled = %v, want true", gp)
	}

	preflight, _ := nvsentinel.Overrides["preflight"].(map[string]any)
	if preflight == nil {
		t.Fatal("overrides.preflight missing")
	}

	// The gate #2610 asks for: a failed check must strand the pod rather than
	// exit 0. STORE_ONLY would silently convert every failure to success and
	// drop the health event before it reaches Kubernetes. failurePolicy Fail
	// would make a webhook outage reject every pod in a labeled namespace.
	if preflight["processingStrategy"] != "EXECUTE_REMEDIATION" {
		t.Errorf("processingStrategy = %v, want EXECUTE_REMEDIATION -- STORE_ONLY converts check failures to exit 0 and drops the event", preflight["processingStrategy"])
	}
	webhook, _ := preflight["webhook"].(map[string]any)
	if webhook["failurePolicy"] != "Ignore" {
		t.Errorf("webhook.failurePolicy = %v, want Ignore (chart default is Fail)", webhook["failurePolicy"])
	}

	gangCoordination, _ := preflight["gangCoordination"].(map[string]any)
	if gangCoordination["enabled"] != true {
		t.Errorf("gangCoordination.enabled = %v, want true -- the chart generates the PodGroup RBAC from it", gangCoordination["enabled"])
	}

	gang, _ := preflight["gangDiscovery"].(map[string]any)
	if gang == nil {
		t.Fatal("preflight.gangDiscovery missing")
	}
	// annotationKeys is load-bearing: the controller refuses to start without
	// it, and gangDiscovery.name alone does not satisfy that requirement.
	keys, _ := gang["annotationKeys"].([]any)
	if len(keys) != 1 || keys[0] != "pod-group-name" {
		t.Errorf("gangDiscovery.annotationKeys = %v, want [pod-group-name]", gang["annotationKeys"])
	}
	gvr, _ := gang["podGroupGVR"].(map[string]any)
	for key, want := range map[string]string{
		"group":    "scheduling.run.ai",
		"version":  "v2alpha2",
		"resource": "podgroups",
	} {
		if gvr[key] != want {
			t.Errorf("gangDiscovery.podGroupGVR.%s = %v, want %q", key, gvr[key], want)
		}
	}

	// The mixin owns the whole list so it can disable the multi-node check,
	// which a plain GPU pod cannot satisfy. Asserting the shape here catches a
	// partial restatement, which would silently drop checks.
	ics, _ := preflight["initContainers"].([]any)
	if len(ics) != 3 {
		t.Fatalf("preflight.initContainers has %d entries, want 3", len(ics))
	}
	wantNames := []string{"preflight-dcgm-diag", "preflight-nccl-loopback", "preflight-nccl-allreduce"}
	for i, want := range wantNames {
		entry, _ := ics[i].(map[string]any)
		if entry["name"] != want {
			t.Errorf("initContainers[%d].name = %v, want %q (order is injection order)", i, entry["name"], want)
			continue
		}
		de, present := entry["defaultEnabled"]
		if want == "preflight-nccl-allreduce" {
			if !present || de != false {
				t.Errorf("initContainers[%d] %s: defaultEnabled = %v (present=%v), want false", i, want, de, present)
			}
			continue
		}
		if present {
			t.Errorf("initContainers[%d] %s: defaultEnabled = %v, want unset", i, want, de)
		}
	}
}

// TestMixinNVSentinelPreflight_AddsKaiDependencyAdditively is the reason
// DependencyRefs is in mixinComponentRefSafeForMerge's safe set: the preflight
// controller validates the KAI PodGroup CRD at startup and fails closed, so it
// needs kai-scheduler applied first -- but the mixin must not disturb the
// dependency edges the base chain already declared.
func TestMixinNVSentinelPreflight_AddsKaiDependencyAdditively(t *testing.T) {
	ctx, store := preflightStore(t)

	baseDeps := []string{"cert-manager", "gpu-operator", "prometheus-operator-crds"}
	spec := preflightLeaf([]string{preflightMixin}, baseDeps)
	if _, err := store.mergeMixins(ctx, &spec); err != nil {
		t.Fatalf("mergeMixins: %v", err)
	}

	nvsentinel, _ := findComponentRefByName(spec.ComponentRefs, "nvsentinel")
	got := map[string]int{}
	for _, d := range nvsentinel.DependencyRefs {
		got[d]++
	}

	for _, want := range baseDeps {
		if got[want] == 0 {
			t.Errorf("dependency %q from the base chain was dropped; the merge must be a union, not a replacement", want)
		}
	}
	if got["kai-scheduler"] == 0 {
		t.Error("kai-scheduler was not added by the mixin")
	}
	for dep, n := range got {
		if n > 1 {
			t.Errorf("dependency %q appears %d times; the merge must deduplicate", dep, n)
		}
	}
}

func TestMixinNVSentinelPreflight_DependencyAlreadyPresentStaysDeduplicated(t *testing.T) {
	ctx, store := preflightStore(t)

	spec := preflightLeaf([]string{preflightMixin}, []string{"cert-manager", "kai-scheduler"})
	if _, err := store.mergeMixins(ctx, &spec); err != nil {
		t.Fatalf("mergeMixins: %v", err)
	}

	nvsentinel, _ := findComponentRefByName(spec.ComponentRefs, "nvsentinel")
	count := 0
	for _, d := range nvsentinel.DependencyRefs {
		if d == "kai-scheduler" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("kai-scheduler appears %d times, want exactly 1", count)
	}
}

// TestMixinComponentRefSafeForMerge_DependencyRefsAccepted pins the safe-set
// change directly, alongside the fields that must stay rejected. Without the
// negative cases a blanket relaxation would pass this test.
func TestMixinComponentRefSafeForMerge_DependencyRefsAccepted(t *testing.T) {
	tests := []struct {
		name      string
		ref       ComponentRef
		wantSafe  bool
		wantField string
	}{
		{
			name:     "dependencyRefs is additive, so it is safe",
			ref:      ComponentRef{Name: "nvsentinel", DependencyRefs: []string{"kai-scheduler"}},
			wantSafe: true,
		},
		{
			name:      "valuesFile replaces wholesale, so it stays rejected",
			ref:       ComponentRef{Name: "nvsentinel", ValuesFile: "components/nvsentinel/values.yaml"},
			wantSafe:  false,
			wantField: "valuesFile",
		},
		{
			name:      "patches stay rejected",
			ref:       ComponentRef{Name: "nvsentinel", Patches: []string{"components/nvsentinel/patches/x.yaml"}},
			wantSafe:  false,
			wantField: "patches",
		},
		{
			name:      "version stays rejected",
			ref:       ComponentRef{Name: "nvsentinel", Version: "v9.9.9"},
			wantSafe:  false,
			wantField: "version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			field, safe := mixinComponentRefSafeForMerge(tt.ref)
			if safe != tt.wantSafe {
				t.Fatalf("safe = %v, want %v (field %q)", safe, tt.wantSafe, field)
			}
			if !safe && field != tt.wantField {
				t.Errorf("offending field = %q, want %q", field, tt.wantField)
			}
		})
	}
}

// TestMixinNVSentinelPreflight_RejectsNonAllowlistedPath proves the registry
// allowlist still gates this mixin's surface: a path outside
// mixinSafeOverridePaths fails at compose time rather than silently applying.
func TestMixinNVSentinelPreflight_RejectsNonAllowlistedPath(t *testing.T) {
	ctx := context.Background()
	store := newNvsentinelAllowlistStore("preflight-gate", []string{"global.preflight.enabled"}, nil)
	addTestMixin(store, "test-preflight", []ComponentRef{
		{Name: "nvsentinel", Overrides: map[string]any{
			"preflight": map[string]any{"webhook": map[string]any{"failurePolicy": "Ignore"}},
		}},
	})

	spec := RecipeMetadataSpec{
		Mixins: []string{"test-preflight"},
		ComponentRefs: []ComponentRef{
			{Name: "nvsentinel", Chart: "nvsentinel", Version: "v1.22.0", Source: "oci://ghcr.io/nvidia", Type: ComponentTypeHelm, Namespace: "nvsentinel"},
		},
	}
	if _, err := store.mergeMixins(ctx, &spec); err == nil {
		t.Fatal("expected a path outside the allowlist to be rejected, got nil")
	}
}

// TestMixinNVSentinelPreflight_ComposesOntoEveryLeaf resolves every shipped
// leaf with the mixin composed in. The mixin's kai-scheduler dependencyRef is
// only satisfiable on a chain that already carries kai-scheduler, and the
// resolver rejects a dependency on an absent component -- so a future leaf
// that drops kai-scheduler would make this mixin unusable there, and would do
// so without any other test noticing.
func TestMixinNVSentinelPreflight_ComposesOntoEveryLeaf(t *testing.T) {
	ctx, store := preflightUncachedStore(t)

	resolved := 0
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		overlay.Spec.Mixins = append(overlay.Spec.Mixins, preflightMixin)
		t.Run(name, func(t *testing.T) {
			result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
			if err != nil {
				t.Fatalf("resolving %s with the %s mixin composed: %v", name, preflightMixin, err)
			}
			// Without this the subtest would pass for a leaf whose criteria
			// resolved to some OTHER overlay entirely.
			if !slices.Contains(result.Metadata.AppliedOverlays, name) {
				t.Fatalf("%s: criteria resolved to %v, which does not include it", name, result.Metadata.AppliedOverlays)
			}
			nvsentinel, ok := findComponentRefByName(result.ComponentRefs, "nvsentinel")
			if !ok {
				t.Fatalf("%s resolved without nvsentinel", name)
			}
			if _, present := nvsentinel.Overrides["preflight"]; !present {
				t.Errorf("%s: mixin composed but preflight overrides are absent", name)
			}
			// The whole point of the dependencyRefs relaxation: the edge must
			// survive composition on every leaf, not just resolve without error.
			if !slices.Contains(nvsentinel.DependencyRefs, "kai-scheduler") {
				t.Errorf("%s: kai-scheduler edge missing from nvsentinel (%v)", name, nvsentinel.DependencyRefs)
			}
		})
		resolved++
	}
	if resolved == 0 {
		t.Fatal("no leaves were resolved -- the overlay walker is broken")
	}
}

// TestRecipesWithoutPreflightMixinAreUnchanged guards the opt-in contract: no
// shipped recipe composes this mixin, so nothing may appear in a recipe that
// does not ask for it.
func TestRecipesWithoutPreflightMixinAreUnchanged(t *testing.T) {
	ctx, store := preflightStore(t)

	spec := preflightLeaf(nil, []string{"cert-manager"})
	if _, err := store.mergeMixins(ctx, &spec); err != nil {
		t.Fatalf("mergeMixins: %v", err)
	}

	nvsentinel, _ := findComponentRefByName(spec.ComponentRefs, "nvsentinel")
	if _, present := nvsentinel.Overrides["preflight"]; present {
		t.Error("preflight values present without the mixin composed")
	}
	for _, d := range nvsentinel.DependencyRefs {
		if d == "kai-scheduler" {
			t.Error("kai-scheduler dependency added without the mixin composed")
		}
	}
}

// preflightScanMatrixTags maps each image in the vuln-scan workflow's matrix
// include block to the tag it pins. The scan job splits image and tag across a
// matrix entry and an override, so the pin is read structurally rather than by
// grepping for a version string.
func preflightScanMatrixTags(t *testing.T, workflowPath string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("reading %s: %v", workflowPath, err)
	}
	var wf struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Include []map[string]string `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &wf); err != nil {
		t.Fatalf("parsing %s: %v", workflowPath, err)
	}
	tags := map[string]string{}
	for _, job := range wf.Jobs {
		for _, entry := range job.Strategy.Matrix.Include {
			if image := entry["image"]; image != "" {
				tags[image] = entry["tag"]
			}
		}
	}
	return tags
}

// preflightImages are the four images the mixin pulls in, named without a tag
// because every surface below pins them at nvsentinel's own chart version.
var preflightImages = []string{
	"ghcr.io/nvidia/nvsentinel/preflight",
	"ghcr.io/nvidia/nvsentinel/preflight-dcgm-diag",
	"ghcr.io/nvidia/nvsentinel/preflight-nccl-loopback",
	"ghcr.io/nvidia/nvsentinel/preflight-nccl-allreduce",
}

// TestPreflightImagesPinnedEverywhere keeps every hand-maintained copy of the
// preflight images in step with nvsentinel's registry pin, and requires each
// surface to carry them at all.
//
// Two failures this guards, both silent: the subchart images inherit the
// parent chart's version, so bumping defaultVersion moves the deployed images
// while these copies keep naming the old one. And dropping an image from the
// scan, mirror or documentation surfaces would violate #2610's coverage
// criteria while leaving every other test green, so absence is an error rather
// than a skip.
func TestPreflightImagesPinnedEverywhere(t *testing.T) {
	_, store := preflightStore(t)
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
	scanTags := preflightScanMatrixTags(t, scanWorkflow)

	// Read once per surface rather than once per image: the tagged reference
	// must appear, so a surface that names the image but pins the wrong
	// version fails rather than passing on the bare name.
	surfaces := []struct{ path, why string }{
		{"../../tools/mirror-e2e", "#2610 requires the mirror job to cover these images"},
		{"../../docs/user/container-images.md", "#2610 requires the images to be documented"},
	}
	bodies := map[string]string{}
	for _, s := range surfaces {
		data, readErr := os.ReadFile(s.path)
		if readErr != nil {
			t.Fatalf("reading %s: %v", s.path, readErr)
		}
		bodies[s.path] = string(data)
	}

	for _, image := range preflightImages {
		t.Run(image, func(t *testing.T) {
			tag, found := scanTags[image]
			switch {
			case !found:
				t.Errorf("%s has no scan matrix entry for %s -- #2610 requires the scan job to cover this image", scanWorkflow, image)
			case tag != version:
				t.Errorf("%s pins tag %q for %s, want nvsentinel's defaultVersion %q", scanWorkflow, tag, image, version)
			}

			// Word-boundary match, not strings.Contains: "preflight" is a
			// prefix of "preflight-dcgm-diag", so a plain substring check
			// would report the controller image as present on a surface that
			// only names one of the three check images.
			mention := regexp.MustCompile(regexp.QuoteMeta(image) + `([^A-Za-z0-9._-]|$)`)
			for _, s := range surfaces {
				body := bodies[s.path]
				if !mention.MatchString(body) {
					t.Errorf("%s does not mention %s -- %s", s.path, image, s.why)
					continue
				}
				// docs/user/container-images.md names the images in a table
				// without tags on purpose (the version is the chart's own and
				// stated in prose), so only the executable surface is pinned.
				if s.path == "../../tools/mirror-e2e" && !strings.Contains(body, image+":"+version) {
					t.Errorf("%s names %s but not at %s -- the pinned tag drifted from nvsentinel's defaultVersion", s.path, image, version)
				}
			}
		})
	}
}

// TestGroveLeavesAreExactlyTheDynamoLeaves backs the Grove limitation
// documented for this mixin. Preflight's gang discovery assumes one flat
// PodGroup per gang, which Grove's hierarchical model does not provide, so the
// docs name the affected recipes by their `*-inference-dynamo` suffix. That
// naming shorthand is only true while the two sets coincide: a future
// Grove-scheduled leaf under another name would silently fall outside the
// documented list, and a dynamo leaf that moved off Grove would be listed as
// limited when it is not.
func TestGroveLeavesAreExactlyTheDynamoLeaves(t *testing.T) {
	ctx, store := preflightStore(t)

	checked := 0
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
		if err != nil {
			// Not skipped: a leaf that fails to resolve would silently
			// escape the invariant below rather than being checked.
			t.Errorf("resolving %s: %v", name, err)
			continue
		}
		checked++
		_, hasGrove := findComponentRefByName(result.ComponentRefs, "grove")
		isDynamo := strings.HasSuffix(name, "-inference-dynamo")
		switch {
		case hasGrove && !isDynamo:
			t.Errorf("%s schedules with grove but is not an *-inference-dynamo leaf; the Grove limitation in docs/user/component-catalog.md names only the dynamo leaves", name)
		case !hasGrove && isDynamo:
			t.Errorf("%s is an *-inference-dynamo leaf but does not carry grove; it is listed as gang-limited when it is not", name)
		}
	}
	if checked == 0 {
		t.Fatal("no leaves were resolved -- the overlay walker is broken")
	}
}

// TestNoShippedRecipeAdoptsPreflightMixin pins the opt-in-only claim that every
// doc about this mixin repeats.
//
// Checked at two levels because either alone has a hole. The declaration scan
// misses a leaf that picks the mixin up through its base chain; the resolution
// scan misses an overlay with no criteria (never resolved on its own) that a
// future leaf could inherit from. Both must stay clean.
func TestNoShippedRecipeAdoptsPreflightMixin(t *testing.T) {
	ctx, store := preflightStore(t)

	declared := 0
	for name, overlay := range store.Overlays {
		declared++
		if slices.Contains(overlay.Spec.Mixins, preflightMixin) {
			t.Errorf("overlay %s declares the %s mixin; it must stay opt-in, adopted only by an integrator's own leaf", name, preflightMixin)
		}
	}
	if declared == 0 {
		t.Fatal("no overlays were inspected -- the walker is broken")
	}

	// The load-bearing half: resolve every shipped leaf as a user would and
	// assert nothing preflight-shaped reaches the recipe.
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
		if _, present := nvsentinel.Overrides["preflight"]; present {
			t.Errorf("%s resolves with preflight overrides on nvsentinel; the mixin must not reach a shipped recipe", name)
		}
		if global, ok := nvsentinel.Overrides["global"].(map[string]any); ok {
			if _, present := global["preflight"]; present {
				t.Errorf("%s resolves with global.preflight on nvsentinel; the mixin must not reach a shipped recipe", name)
			}
		}
	}
	if resolved == 0 {
		t.Fatal("no shipped recipes were resolved -- the walker is broken")
	}
}
