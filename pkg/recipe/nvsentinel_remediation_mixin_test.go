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
	"regexp"
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
// read from AICR's digest pins in the psmdb-operator and nvsentinel-mongodb
// values, so a bump to those files alone fails here; the render test fails
// when a chart bump renders another one.
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
	maps.Copy(images, perconaPinnedImages(t))

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

// perconaValuesFiles hold every Percona datastore image the steps add.
var perconaValuesFiles = []string{
	"components/psmdb-operator/values.yaml",
	"components/nvsentinel-mongodb/values.yaml",
}

// perconaDigest is the immutable suffix every Percona tag must carry. Copies
// that agree with each other are not enough: a tag-only pin can move upstream.
var perconaDigest = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

// perconaPinnedImages maps each image repository pinned in perconaValuesFiles
// to its tag. Every image reference counts, so a pin added to either file is
// covered without being listed here.
func perconaPinnedImages(t *testing.T) map[string]string {
	t.Helper()
	pins := map[string]string{}
	pinnedIn := map[string]string{}
	for _, path := range perconaValuesFiles {
		var values map[string]any
		readComponentYAML(t, path, &values)
		refs := imageRefs(values)
		if len(refs) == 0 {
			t.Fatalf("%s has no image references; imageRefs no longer matches its layout", path)
		}
		for _, ref := range refs {
			repo, tag, ok := splitImageRef(ref)
			if !ok {
				t.Errorf("%s: image %q has no string tag", path, ref)
				continue
			}
			if !perconaDigest.MatchString(tag) {
				t.Errorf("%s pins %s:%s without a sha256 digest", path, repo, tag)
			}
			if prev, seen := pins[repo]; seen && prev != tag {
				t.Errorf("%s pins %s at %q, but %s pins it at %q", path, repo, tag, pinnedIn[repo], prev)
				continue
			}
			pins[repo] = tag
			pinnedIn[repo] = path
		}
	}
	return pins
}

// imageRefs collects the image references in a values tree: a map with a
// repository, joined to its tag when the tag is a string, and any string under
// an image key.
func imageRefs(node any) []string {
	var refs []string
	switch n := node.(type) {
	case map[string]any:
		if repo, ok := n["repository"].(string); ok {
			if tag, ok := n["tag"].(string); ok && tag != "" {
				repo += ":" + tag
			}
			refs = append(refs, repo)
		}
		if ref, ok := n["image"].(string); ok {
			refs = append(refs, ref)
		}
		for _, v := range n {
			refs = append(refs, imageRefs(v)...)
		}
	case []any:
		for _, v := range n {
			refs = append(refs, imageRefs(v)...)
		}
	}
	return refs
}

// splitImageRef splits repo:tag[@digest] into the repository and the tag with
// its digest. The tag separator is the last colon after the last slash, so a
// registry port is not mistaken for one.
func splitImageRef(ref string) (repo, tag string, ok bool) {
	name, digest, pinned := strings.Cut(ref, "@")
	colon := strings.LastIndex(name, ":")
	if colon <= strings.LastIndex(name, "/") || colon == len(name)-1 {
		return "", "", false
	}
	repo, tag = name[:colon], name[colon+1:]
	if pinned {
		tag += "@" + digest
	}
	return repo, tag, true
}

func TestSplitImageRef(t *testing.T) {
	tests := []struct {
		name     string
		ref      string
		wantRepo string
		wantTag  string
		wantOK   bool
	}{
		{"tag", "docker.io/percona/pmm-client:3.9.1", "docker.io/percona/pmm-client", "3.9.1", true},
		{"tag and digest", "docker.io/percona/fluentbit:5.1.2-1@sha256:abc", "docker.io/percona/fluentbit", "5.1.2-1@sha256:abc", true},
		{"registry port", "registry.local:5000/percona/pmm-client:3.9.1", "registry.local:5000/percona/pmm-client", "3.9.1", true},
		{"no tag", "docker.io/percona/pmm-client", "", "", false},
		{"registry port, no tag", "registry.local:5000/percona/pmm-client", "", "", false},
		{"digest, no tag", "docker.io/percona/pmm-client@sha256:abc", "", "", false},
		{"empty tag", "docker.io/percona/pmm-client:", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, tag, ok := splitImageRef(tt.ref)
			if repo != tt.wantRepo || tag != tt.wantTag || ok != tt.wantOK {
				t.Errorf("splitImageRef(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.ref, repo, tag, ok, tt.wantRepo, tt.wantTag, tt.wantOK)
			}
		})
	}
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

// TestMixinNVSentinelDatastoreEKS pins the storage edge ADR-018 declares for
// the datastore on EKS: composed after a datastore step, the mixin adds
// nvsentinel-mongodb -> aws-ebs-csi-driver on every EKS leaf, so the driver
// deploys first. Composed before the step it fails loudly, because it would
// introduce nvsentinel-mongodb and the step's own type for that component
// then conflicts; on a service that declares no aws-ebs-csi-driver the edge
// dangles and resolution rejects it.
func TestMixinNVSentinelDatastoreEKS(t *testing.T) {
	const storageMixin = "nvsentinel-datastore-eks"
	ctx := context.Background()
	store, err := buildMetadataStore(ctx, defaultEmbeddedProvider)
	if err != nil {
		t.Fatalf("buildMetadataStore: %v", err)
	}
	if _, ok := store.Mixins[storageMixin]; !ok {
		t.Fatalf("%s mixin not present; check recipes/mixins/%s.yaml", storageMixin, storageMixin)
	}

	eksLeaves, otherLeaf := 0, ""
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		if overlay.Spec.Criteria.Service != CriteriaServiceEKS {
			if otherLeaf == "" {
				otherLeaf = name
			}
			continue
		}
		eksLeaves++
		overlay.Spec.Mixins = append(overlay.Spec.Mixins, observeMixin, storageMixin)
		t.Run(name, func(t *testing.T) {
			result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
			if err != nil {
				t.Fatalf("resolving %s with %s and %s composed: %v", name, observeMixin, storageMixin, err)
			}
			mongodb := result.GetComponentRef("nvsentinel-mongodb")
			if mongodb == nil {
				t.Fatalf("%s: nvsentinel-mongodb missing from the resolved recipe", name)
			}
			if !slices.Contains(mongodb.DependencyRefs, "aws-ebs-csi-driver") {
				t.Fatalf("%s: nvsentinel-mongodb dependencyRefs = %v, want aws-ebs-csi-driver", name, mongodb.DependencyRefs)
			}
			driver := slices.Index(result.DeploymentOrder, "aws-ebs-csi-driver")
			datastore := slices.Index(result.DeploymentOrder, "nvsentinel-mongodb")
			if driver < 0 || datastore < 0 || driver > datastore {
				t.Errorf("%s: deploymentOrder = %v, want aws-ebs-csi-driver before nvsentinel-mongodb", name, result.DeploymentOrder)
			}
		})
	}
	if eksLeaves == 0 {
		t.Fatal("no EKS leaf overlays found; the mixin was never composed")
	}

	t.Run("before the datastore step", func(t *testing.T) {
		spec := remediationLeaf(storageMixin, observeMixin)
		_, err := store.mergeMixins(ctx, &spec)
		if err == nil {
			t.Fatal("composing the storage mixin before the datastore step succeeded; it must be listed after")
		}
		if !strings.Contains(err.Error(), "identity/sourcing field") {
			t.Fatalf("error = %v, want an identity-field conflict on nvsentinel-mongodb", err)
		}
	})

	t.Run("without aws-ebs-csi-driver", func(t *testing.T) {
		if otherLeaf == "" {
			t.Skip("no non-EKS leaf in the catalog")
		}
		overlay := store.Overlays[otherLeaf]
		overlay.Spec.Mixins = append(overlay.Spec.Mixins, observeMixin, storageMixin)
		if _, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria); err == nil {
			t.Fatalf("resolving %s with %s composed succeeded; the aws-ebs-csi-driver edge dangles there", otherLeaf, storageMixin)
		}
	})
}
