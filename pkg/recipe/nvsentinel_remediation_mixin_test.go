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
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	observeMixin     = "nvsentinel-observe"
	quarantineMixin  = "nvsentinel-quarantine"
	remediationMixin = "nvsentinel-remediation"
)

// remediationSteps is the adoption ladder, in order. Each entry lists every
// global toggle the step sets; any toggle not listed must be absent.
var remediationSteps = []struct {
	mixin   string
	dryRun  bool
	enables []string
}{
	{observeMixin, true, []string{"faultQuarantine", "nodeDrainer", "faultRemediation"}},
	{quarantineMixin, false, []string{"faultQuarantine", "nodeDrainer"}},
	{remediationMixin, false, []string{"faultQuarantine", "nodeDrainer", "faultRemediation", "janitor", "janitorProvider"}},
}

// remediationToggles is every global key a remediation step may set.
var remediationToggles = []string{
	"dryRun", "mongodbStore", "faultQuarantine", "nodeDrainer", "faultRemediation", "janitor", "janitorProvider",
}

// remediationDatastoreKeys are the global keys that wire NVSentinel to the
// Percona datastore the steps bundle.
var remediationDatastoreKeys = []string{"certificateRotationEnabled", "datastore"}

func remediationStore(t *testing.T) (context.Context, *MetadataStore) {
	t.Helper()
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}
	for _, step := range remediationSteps {
		if _, ok := store.Mixins[step.mixin]; !ok {
			t.Fatalf("%s mixin not present; check recipes/mixins/%s.yaml", step.mixin, step.mixin)
		}
	}
	return ctx, store
}

func remediationLeaf(mixins ...string) RecipeMetadataSpec {
	return RecipeMetadataSpec{
		Mixins: mixins,
		ComponentRefs: []ComponentRef{{
			Name: "nvsentinel", Chart: "nvsentinel", Version: "v1.25.0",
			Source: "oci://ghcr.io/nvidia", Type: ComponentTypeHelm, Namespace: "nvsentinel",
		}},
	}
}

// TestMixinNVSentinelRemediationSteps_Compose pins what each step turns on.
// The quarantine step leaves fault-remediation off on purpose: without janitor
// nothing acts on the repair requests it creates, yet it labels each node
// remediation-succeeded as soon as the request exists.
func TestMixinNVSentinelRemediationSteps_Compose(t *testing.T) {
	ctx, store := remediationStore(t)

	for _, step := range remediationSteps {
		t.Run(step.mixin, func(t *testing.T) {
			spec := remediationLeaf(step.mixin)
			if _, err := store.mergeMixins(ctx, &spec); err != nil {
				t.Fatalf("mergeMixins: %v", err)
			}
			nvsentinel, ok := findComponentRefByName(spec.ComponentRefs, "nvsentinel")
			if !ok {
				t.Fatal("nvsentinel missing from merged spec")
			}
			global, ok := nvsentinel.Overrides["global"].(map[string]any)
			if !ok {
				t.Fatal("mixin set no global overrides")
			}

			if dryRun, isBool := global["dryRun"].(bool); !isBool || dryRun != step.dryRun {
				t.Errorf("global.dryRun = %v, want %v -- every step pins it explicitly", global["dryRun"], step.dryRun)
			}
			for _, key := range remediationToggles[1:] {
				want := slices.Contains(step.enables, key)
				section, present := global[key].(map[string]any)
				switch {
				case want && (!present || section["enabled"] != true):
					t.Errorf("global.%s.enabled = %v, want true", key, global[key])
				case !want && present:
					t.Errorf("global.%s is set (%v); this step must leave it to the chart default (off)", key, section)
				}
			}
			for key := range global {
				if !slices.Contains(remediationToggles, key) && !slices.Contains(remediationDatastoreKeys, key) {
					t.Errorf("global.%s set by %s; a remediation step sets only the pipeline toggles and datastore", key, step.mixin)
				}
			}
			assertPerconaDatastore(t, step.mixin, global)
			assertPerconaComponents(t, spec, nvsentinel)
			// The event sources every step acts on, pinned explicitly.
			for _, monitor := range []string{"gpu-health-monitor", "syslog-health-monitor"} {
				section, _ := nvsentinel.Overrides[monitor].(map[string]any)
				if section["processingStrategy"] != "EXECUTE_REMEDIATION" {
					t.Errorf("%s.processingStrategy = %v, want EXECUTE_REMEDIATION set explicitly", monitor, section["processingStrategy"])
				}
				if len(section) != 1 {
					t.Errorf("%s overrides = %v, want only processingStrategy", monitor, section)
				}
			}
			wantKeys := []string{"global", "gpu-health-monitor", "syslog-health-monitor"}
			if slices.Contains(step.enables, "faultRemediation") {
				wantKeys = append(wantKeys, "fault-remediation")
				assertComponentResetIsGPUReset(t, step.mixin, nvsentinel.Overrides)
			}
			if slices.Contains(step.enables, "janitorProvider") {
				wantKeys = append(wantKeys, "janitor-provider")
			}
			gotKeys := slices.Sorted(maps.Keys(nvsentinel.Overrides))
			slices.Sort(wantKeys)
			if !slices.Equal(gotKeys, wantKeys) {
				t.Errorf("top-level overrides = %v, want %v", gotKeys, wantKeys)
			}
		})
	}
}

// TestMixinNVSentinelRemediationSteps_MutuallyExclusive pins that a leaf can
// be on only one step. Nothing enforces it except that every pair sets a
// shared path, which the resolver rejects as a collision.
func TestMixinNVSentinelRemediationSteps_MutuallyExclusive(t *testing.T) {
	ctx, store := remediationStore(t)

	for _, a := range remediationSteps {
		for _, b := range remediationSteps {
			if a.mixin == b.mixin {
				continue
			}
			t.Run(a.mixin+"+"+b.mixin, func(t *testing.T) {
				spec := remediationLeaf(a.mixin, b.mixin)
				_, err := store.mergeMixins(ctx, &spec)
				if err == nil {
					t.Fatalf("composing %s with %s succeeded; the steps must be mutually exclusive", a.mixin, b.mixin)
				}
				if !strings.Contains(err.Error(), "collides") {
					t.Fatalf("error = %v, want a path collision", err)
				}
			})
		}
	}
}

// TestMixinNVSentinelRemediation_ComposesOntoEveryLeaf resolves every shipped
// leaf with the widest step composed. The other two set a subset of its paths,
// so a leaf that sets any of them fails here first.
func TestMixinNVSentinelRemediation_ComposesOntoEveryLeaf(t *testing.T) {
	ctx := context.Background()
	store, err := buildMetadataStore(ctx, defaultEmbeddedProvider)
	if err != nil {
		t.Fatalf("buildMetadataStore: %v", err)
	}

	resolved := 0
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		overlay.Spec.Mixins = append(overlay.Spec.Mixins, remediationMixin)
		t.Run(name, func(t *testing.T) {
			result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
			if err != nil {
				t.Fatalf("resolving %s with %s composed: %v", name, remediationMixin, err)
			}
			if !slices.Contains(result.Metadata.AppliedOverlays, name) {
				t.Fatalf("%s: criteria resolved to %v, which does not include it", name, result.Metadata.AppliedOverlays)
			}
			nvsentinel, ok := findComponentRefByName(result.ComponentRefs, "nvsentinel")
			if !ok {
				return
			}
			global, _ := nvsentinel.Overrides["global"].(map[string]any)
			if section, _ := global["janitor"].(map[string]any); section["enabled"] != true {
				t.Errorf("%s: mixin composed but global.janitor.enabled = %v", name, global["janitor"])
			}
			// The map must survive into the resolved values on every leaf:
			// a leaf that overrode it would turn a recoverable GPU fault
			// into a node reboot.
			values, err := result.GetValuesForComponentWithContext(ctx, "nvsentinel")
			if err != nil {
				t.Fatalf("%s: GetValuesForComponentWithContext: %v", name, err)
			}
			assertComponentResetIsGPUReset(t, name, values)
		})
		resolved++
	}
	if resolved == 0 {
		t.Fatal("no leaves were resolved -- the overlay walker is broken")
	}
}

// TestNoShippedRecipeAdoptsRemediationMixins keeps every step opt-in: existing
// recipes stay monitoring-only unless an operator selects a capability.
// Checked by declaration (the base included, since mixins accumulate down the
// chain), by resolved overrides, and by resolved values, since a values file
// can turn a toggle on without any override.
func TestNoShippedRecipeAdoptsRemediationMixins(t *testing.T) {
	ctx, store := remediationStore(t)

	adopts := func(where string, mixins []string) {
		for _, step := range remediationSteps {
			if slices.Contains(mixins, step.mixin) {
				t.Errorf("%s adopts %s; the remediation steps stay opt-in", where, step.mixin)
			}
		}
	}
	adopts("recipes/overlays/base.yaml", store.Base.Spec.Mixins)
	for name, overlay := range store.Overlays {
		adopts(fmt.Sprintf("overlay %q", name), overlay.Spec.Mixins)
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
		for _, key := range remediationToggles {
			if _, present := global[key]; present {
				t.Errorf("%s resolves with global.%s on nvsentinel; no shipped recipe may set a remediation toggle", name, key)
			}
		}

		values, err := result.GetValuesForComponentWithContext(ctx, "nvsentinel")
		if err != nil {
			t.Errorf("%s: GetValuesForComponentWithContext: %v", name, err)
			continue
		}
		assertRemediationOff(t, name, values)
	}
	if resolved == 0 {
		t.Fatal("no shipped recipes were resolved -- the walker is broken")
	}
}

// assertRemediationOff fails unless every remediation subchart toggle in the
// resolved values is absent or literally false, global.dryRun is absent or
// true, and no fault-remediation settings are present.
func assertRemediationOff(t *testing.T, leaf string, values map[string]any) {
	t.Helper()
	raw, present := values["global"]
	if !present {
		return
	}
	global, ok := raw.(map[string]any)
	if !ok {
		t.Errorf("%s: resolved global is %T, want a map", leaf, raw)
		return
	}
	if _, present := values["fault-remediation"]; present {
		t.Errorf("%s: resolved values carry fault-remediation settings; they belong to the remediation mixins", leaf)
	}
	if dryRun, present := global["dryRun"]; present && dryRun != true {
		t.Errorf("%s: resolved global.dryRun = %v, want absent or true", leaf, dryRun)
	}
	for _, key := range remediationToggles[1:] {
		raw, present := global[key]
		if !present {
			continue
		}
		section, ok := raw.(map[string]any)
		if !ok {
			t.Errorf("%s: resolved global.%s is %T, want a map", leaf, key, raw)
			continue
		}
		if enabled, present := section["enabled"]; present && enabled != false {
			t.Errorf("%s: resolved global.%s.enabled = %v, want absent or false", leaf, key, enabled)
		}
	}
}

func assertComponentResetIsGPUReset(t *testing.T, leaf string, values map[string]any) {
	t.Helper()
	remediation, _ := values["fault-remediation"].(map[string]any)
	maintenance, _ := remediation["maintenance"].(map[string]any)
	actions, _ := maintenance["actions"].(map[string]any)
	action, ok := actions["COMPONENT_RESET"].(map[string]any)
	if !ok {
		t.Errorf("%s: fault-remediation.maintenance.actions.COMPONENT_RESET missing; the chart default reboots the node", leaf)
		return
	}
	want := map[string]string{
		"apiGroup":              "janitor.dgxc.nvidia.com",
		"version":               "v1alpha1",
		"kind":                  "GPUReset",
		"scope":                 "Cluster",
		"completeConditionType": "Complete",
		"templateFileName":      "gpureset",
		"equivalenceGroup":      "reset",
		"impactedEntityScope":   "GPU_UUID",
	}
	for key, value := range want {
		if action[key] != value {
			t.Errorf("%s: COMPONENT_RESET.%s = %v, want %q", leaf, key, action[key], value)
		}
	}
	if superseding, _ := action["supersedingEquivalenceGroups"].([]any); len(superseding) != 1 || superseding[0] != "restart" {
		t.Errorf("%s: COMPONENT_RESET.supersedingEquivalenceGroups = %v, want [restart]", leaf, action["supersedingEquivalenceGroups"])
	}

	templates, _ := maintenance["templates"].(map[string]any)
	tmpl, _ := templates["gpureset"].(string)
	for _, fragment := range []string{"kind: GPUReset", "{{ .ImpactedEntityScopeValue }}", "{{ .HealthEvent.NodeName }}"} {
		if !strings.Contains(tmpl, fragment) {
			t.Errorf("%s: gpureset template missing %q; fault-remediation fails to start without the template the action names", leaf, fragment)
		}
	}
}

// TestRemediationImagesPinnedEverywhere keeps every hand-maintained copy of
// the images the remediation steps add in step with what the chart renders.
// The static BOM cannot see mixin-gated images, so the scan, mirror and docs
// surfaces are their only coverage; dropping one leaves every other test green.
// NVIDIA images move with nvsentinel's defaultVersion. The Percona images are
// AICR's digest pins in the psmdb-operator and nvsentinel-mongodb values, and
// the render test fails when a chart bump renders another one.
func TestRemediationImagesPinnedEverywhere(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	nvsentinel := registry.Get("nvsentinel")
	if nvsentinel == nil || nvsentinel.Helm.DefaultVersion == "" {
		t.Fatal("nvsentinel registry entry has no defaultVersion")
	}
	version := nvsentinel.Helm.DefaultVersion

	images := map[string]string{}
	for _, name := range []string{"fault-quarantine", "node-drainer", "fault-remediation", "janitor", "janitor-provider", "gpu-reset"} {
		images["ghcr.io/nvidia/nvsentinel/"+name] = version
	}
	for image, tag := range remediationThirdPartyImages {
		images[image] = tag
	}

	surfaces := []struct{ path, why string }{
		{"../../tools/mirror-e2e", "the mirror job must cover this image"},
		{"../../docs/user/container-images.md", "the image must stay documented"},
		{"../../pkg/bundler/validations/nvsentinel_remediation_render_test.go", "the render test asserts this image reaches the chart"},
	}
	bodies := make(map[string]string, len(surfaces))
	for _, s := range surfaces {
		data, err := os.ReadFile(s.path)
		if err != nil {
			t.Fatalf("reading %s: %v", s.path, err)
		}
		bodies[s.path] = string(data)
	}

	for image, tag := range images {
		assertScanned(t, image, tag)
		for _, s := range surfaces {
			if !strings.Contains(bodies[s.path], image+":"+tag) {
				t.Errorf("%s does not reference %s:%s -- %s", s.path, image, tag, s.why)
			}
		}
	}
}

func assertScanned(t *testing.T, image, tag string) {
	t.Helper()
	const scanWorkflow = "../../.github/workflows/vuln-scan-images.yaml"
	got, found := scanMatrixTagFor(t, image)
	switch {
	case !found:
		t.Errorf("%s has no scan matrix entry for %s", scanWorkflow, image)
	case got != tag:
		t.Errorf("%s pins tag %q for %s, want %q", scanWorkflow, got, image, tag)
	}
}

// remediationThirdPartyImages are the Percona datastore images the steps
// add, pinned by AICR in recipes/components/{psmdb-operator,nvsentinel-mongodb}.
var remediationThirdPartyImages = map[string]string{
	"docker.io/percona/percona-server-mongodb-operator": "1.23.0@sha256:feaff989e25346716d85be9ea918593f89ad7481d30e033df21e0a764a6a484e",
	"docker.io/percona/percona-server-mongodb":          "8.0.26-11@sha256:53f89c001997627554e6afc0feb5906209ba109f4f98c62f2ca8456c214af60c",
	"docker.io/percona/mongodb_exporter":                "0.40.0@sha256:d66daa6aff0513860d1577cee3b55ab82fde43394f8319d7b4674411b9153cce",
	"docker.io/percona/percona-backup-mongodb":          "2.15.0@sha256:12dcba7f1b55e00eb1b49dac51427d942c221b826d621d6bfad926a9d959a7c5",
	"docker.io/percona/fluentbit":                       "5.0.9-1@sha256:030e3faf454e93c19d9cfa1ec85ba6943fe293f5bd29fb04533a1b3de8c78bd6",
	"docker.io/percona/pmm-client":                      "3.8.1@sha256:ea4061a6d9bd59d9c7aecbe75b91989bbddad55208b898d75064ac436014ca16",
}

// TestRemediationRebootImageIsInitContainerImage pins the generic reboot Job's
// image to the digest-pinned busybox already used for init containers. aicr
// mirror cannot discover it (janitor-provider receives it as an env var), so
// the scan matrix and the image docs are its only coverage.
func TestRemediationRebootImageIsInitContainerImage(t *testing.T) {
	const valuesPath = "components/nvsentinel/values.yaml"
	data, err := defaultEmbeddedProvider.ReadFile(context.Background(), valuesPath)
	if err != nil {
		t.Fatalf("reading %s: %v", valuesPath, err)
	}
	var values struct {
		Global struct {
			InitContainerImage struct {
				Repository string `yaml:"repository"`
				Tag        string `yaml:"tag"`
			} `yaml:"initContainerImage"`
		} `yaml:"global"`
	}
	if err = yaml.Unmarshal(data, &values); err != nil {
		t.Fatalf("parsing %s: %v", valuesPath, err)
	}
	initImage := values.Global.InitContainerImage
	if initImage.Repository == "" || initImage.Tag == "" {
		t.Fatalf("%s: global.initContainerImage has no repository or tag", valuesPath)
	}
	image := initImage.Repository + ":" + initImage.Tag

	_, store := remediationStore(t)
	var rebootImage any
	for _, c := range store.Mixins[remediationMixin].Spec.ComponentRefs {
		if c.Name != "nvsentinel" {
			continue
		}
		provider, _ := c.Overrides["janitor-provider"].(map[string]any)
		csp, _ := provider["csp"].(map[string]any)
		generic, _ := csp["generic"].(map[string]any)
		rebootImage = generic["rebootImage"]
	}
	if rebootImage != image {
		t.Errorf("%s: janitor-provider.csp.generic.rebootImage = %v, want %q (global.initContainerImage)", remediationMixin, rebootImage, image)
	}

	assertScanned(t, initImage.Repository, initImage.Tag)

	const docs = "../../docs/user/container-images.md"
	body, err := os.ReadFile(docs)
	if err != nil {
		t.Fatalf("reading %s: %v", docs, err)
	}
	// The image already appears elsewhere in the page, so match its own
	// table row rather than any mention.
	row := "| `" + image + "` | the `generic` provider's reboot Job"
	if !strings.Contains(string(body), row) {
		t.Errorf("%s has no remediation table row for %s -- the reboot image must stay documented", docs, image)
	}
}
