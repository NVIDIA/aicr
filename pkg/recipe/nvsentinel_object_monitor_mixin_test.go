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
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const objectMonitorMixin = "nvsentinel-object-monitor"

// objectMonitorStore loads the real embedded catalog, so every test below
// exercises the shipped mixin rather than a fixture.
func objectMonitorStore(t *testing.T) (context.Context, *MetadataStore) {
	t.Helper()
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}
	if _, ok := store.Mixins[objectMonitorMixin]; !ok {
		t.Fatalf("%s mixin not present; check recipes/mixins/%s.yaml", objectMonitorMixin, objectMonitorMixin)
	}
	return ctx, store
}

// nvsentinelLeaf builds a leaf whose chain already carries nvsentinel, which
// is what makes this mixin a composition onto an existing component.
func nvsentinelLeaf(mixins []string, overrides map[string]any) RecipeMetadataSpec {
	return RecipeMetadataSpec{
		Mixins: mixins,
		ComponentRefs: []ComponentRef{{
			Name: "nvsentinel", Chart: "nvsentinel", Version: "v1.20.0",
			Source: "oci://ghcr.io/nvidia", Type: ComponentTypeHelm,
			Namespace: "nvsentinel", Overrides: overrides,
		}},
	}
}

func objectMonitorPolicies(t *testing.T, ref ComponentRef) []any {
	t.Helper()
	kom, _ := ref.Overrides["kubernetes-object-monitor"].(map[string]any)
	if kom == nil {
		t.Fatal("overrides.kubernetes-object-monitor missing")
	}
	policies, _ := kom["policies"].([]any)
	return policies
}

func policyByName(policies []any, name string) map[string]any {
	for _, p := range policies {
		policy, _ := p.(map[string]any)
		if policy["name"] == name {
			return policy
		}
	}
	return nil
}

// TestMixinNVSentinelObjectMonitor_ComposesCleanly proves the shipped mixin
// composes onto an already-nvsentinel-chained leaf and produces exactly the
// documented values.
func TestMixinNVSentinelObjectMonitor_ComposesCleanly(t *testing.T) {
	ctx, store := objectMonitorStore(t)

	spec := nvsentinelLeaf([]string{objectMonitorMixin}, nil)
	if _, err := store.mergeMixins(ctx, &spec); err != nil {
		t.Fatalf("mergeMixins: %v", err)
	}

	nvsentinel, ok := findComponentRefByName(spec.ComponentRefs, "nvsentinel")
	if !ok {
		t.Fatal("nvsentinel component missing from merged spec")
	}

	global, _ := nvsentinel.Overrides["global"].(map[string]any)
	kom, _ := global["kubernetesObjectMonitor"].(map[string]any)
	if kom == nil || kom["enabled"] != true {
		t.Fatalf("global.kubernetesObjectMonitor.enabled = %v, want true", kom)
	}

	// Exactly two, and the loop below requires both to be ours -- so no chart
	// default survives. Asserting the count rather than a specific default's
	// name keeps this meaningful when upstream renames it (node-not-ready
	// becomes ReplaceNotReadyNode in the version #2596 bumps to).
	policies := objectMonitorPolicies(t, nvsentinel)
	if len(policies) != 2 {
		t.Fatalf("policies has %d entries, want exactly the mixin's 2 -- a chart default may no longer be replaced", len(policies))
	}

	for _, name := range []string{"gpu-operator-pods-health", "network-operator-pod-health"} {
		policy := policyByName(policies, name)
		if policy == nil {
			t.Errorf("policy %q missing from rendered policies", name)
			continue
		}

		// enabled and nodeAssociation are load-bearing and otherwise
		// unasserted: flipping enabled turns the policy off, and breaking
		// nodeAssociation detaches every event from its node. Both fail
		// silently at runtime.
		if policy["enabled"] != true {
			t.Errorf("policy %q enabled = %v, want true", name, policy["enabled"])
		}
		nodeAssociation, _ := policy["nodeAssociation"].(map[string]any)
		if got, _ := nodeAssociation["expression"].(string); got != "resource.spec.nodeName" {
			t.Errorf("policy %q nodeAssociation.expression = %q, want resource.spec.nodeName", name, got)
		}

		resource, _ := policy["resource"].(map[string]any)
		if got, _ := resource["kind"].(string); got != "Pod" {
			t.Errorf("policy %q resource.kind = %q, want Pod", name, got)
		}
		// resource.namespace takes a single namespace, but each policy has to
		// match two (see the mixin header), so it must stay unset -- setting
		// it would scope the informer to one and silence the other.
		if _, present := resource["namespace"]; present {
			t.Errorf("policy %q sets resource.namespace, which cannot express both namespaces it must match", name)
		}

		predicate, _ := policy["predicate"].(map[string]any)
		expr, _ := predicate["expression"].(string)
		if !strings.Contains(expr, "duration('30m')") {
			t.Errorf("policy %q predicate does not enforce the 30-minute grace period: %s", name, expr)
		}

		healthEvent, _ := policy["healthEvent"].(map[string]any)
		if healthEvent["isFatal"] != true {
			t.Errorf("policy %q healthEvent.isFatal = %v, want true", name, healthEvent["isFatal"])
		}
		for _, key := range []string{"quarantineOverrides", "drainOverrides"} {
			if _, present := healthEvent[key]; present {
				t.Errorf("policy %q sets %s, want unset", name, key)
			}
		}
	}
}

// TestMixinNVSentinelObjectMonitor_PolicyNamespacesMatchCatalog asserts each
// policy matches every namespace this repo can actually deploy the watched
// component into: the registry's defaultNamespace, and the relocated
// namespace os-talos assigns. Both are read from the catalog rather than
// hardcoded, so renaming either one fails here instead of leaving a policy
// watching a namespace nothing runs in -- the silent no-op #2612 exists to
// prevent, which upstream's hardcoded `network-operator` would have caused.
func TestMixinNVSentinelObjectMonitor_PolicyNamespacesMatchCatalog(t *testing.T) {
	_, store := objectMonitorStore(t)

	registry, err := GetComponentRegistryFor(store.provider)
	if err != nil {
		t.Fatalf("GetComponentRegistryFor: %v", err)
	}
	talos, ok := store.Mixins["os-talos"]
	if !ok {
		t.Fatal("os-talos mixin not present; namespace relocation can no longer be checked")
	}

	ref, ok := findComponentRefByName(store.Mixins[objectMonitorMixin].Spec.ComponentRefs, "nvsentinel")
	if !ok {
		t.Fatal("mixin has no nvsentinel componentRef")
	}
	policies := objectMonitorPolicies(t, ref)

	for component, policyName := range map[string]string{
		"gpu-operator":     "gpu-operator-pods-health",
		"network-operator": "network-operator-pod-health",
	} {
		comp := registry.Get(component)
		if comp == nil {
			t.Errorf("%s not found in registry", component)
			continue
		}
		want := []string{comp.Helm.DefaultNamespace}
		if talosRef, found := findComponentRefByName(talos.Spec.ComponentRefs, component); found && talosRef.Namespace != "" {
			want = append(want, talosRef.Namespace)
		}

		policy := policyByName(policies, policyName)
		if policy == nil {
			t.Errorf("policy %q not found in mixin", policyName)
			continue
		}
		predicate, _ := policy["predicate"].(map[string]any)
		expr, _ := predicate["expression"].(string)

		for _, namespace := range want {
			if namespace == "" {
				t.Errorf("%s has an empty namespace in the catalog", component)
				continue
			}
			if !strings.Contains(expr, "resource.metadata.namespace == '"+namespace+"'") {
				t.Errorf("policy %q does not match namespace %q that %s deploys into; expression: %s",
					policyName, namespace, component, expr)
			}
		}
	}
}

// scanMatrixTagFor returns the tag the vulnerability-scan workflow pins for
// image, read from the scan job's matrix rather than searched for in the file.
// A substring check cannot tell whose tag it found: once a second override
// exists, an unrelated image carrying the expected version would satisfy it
// while this one kept a stale tag.
func scanMatrixTagFor(t *testing.T, workflowPath, image string) (string, bool) {
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
	for _, job := range wf.Jobs {
		for _, entry := range job.Strategy.Matrix.Include {
			if entry["image"] == image {
				return entry["tag"], true
			}
		}
	}
	return "", false
}

// TestObjectMonitorImagePinnedEverywhere keeps every hand-maintained copy of
// the object-monitor image in step with nvsentinel's registry pin, and
// requires each surface to carry it at all.
//
// Two failures this guards, both silent: the subchart image inherits the
// parent chart's version, so bumping defaultVersion moves the deployed image
// while these copies keep naming the old one. And dropping the image from the
// scan, mirror or documentation surfaces would violate #2612's coverage
// criteria while leaving every other test green, so absence is an error
// rather than a skip.
func TestObjectMonitorImagePinnedEverywhere(t *testing.T) {
	_, store := objectMonitorStore(t)
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
	const image = "ghcr.io/nvidia/nvsentinel/kubernetes-object-monitor"

	// The scan workflow splits image and tag across a matrix entry and an
	// override, so it is checked structurally; the rest carry the full
	// image:tag reference.
	const scanWorkflow = "../../.github/workflows/vuln-scan-images.yaml"
	tag, found := scanMatrixTagFor(t, scanWorkflow, image)
	switch {
	case !found:
		t.Errorf("%s has no scan matrix entry for %s -- #2612 requires the scan job to cover this image", scanWorkflow, image)
	case tag != version:
		t.Errorf("%s pins tag %q for %s, want nvsentinel's defaultVersion %q", scanWorkflow, tag, image, version)
	}

	for _, s := range []struct{ path, why string }{
		{"../../tools/mirror-e2e", "#2612 requires the mirror job to cover this image"},
		{"../../docs/user/container-images.md", "#2612 requires the image to be documented"},
		{"../../pkg/bundler/validations/nvsentinel_object_monitor_render_test.go", "the render test asserts this image reaches the chart"},
	} {
		data, err := os.ReadFile(s.path)
		if err != nil {
			t.Errorf("reading %s: %v", s.path, err)
			continue
		}
		body := string(data)
		if !strings.Contains(body, image) {
			t.Errorf("%s no longer references %s -- %s", s.path, image, s.why)
			continue
		}
		if !strings.Contains(body, image+":"+version) {
			t.Errorf("%s does not pin %s:%s; bump it alongside nvsentinel's defaultVersion", s.path, image, version)
		}
	}
}
