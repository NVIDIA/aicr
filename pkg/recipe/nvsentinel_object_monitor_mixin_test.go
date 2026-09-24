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
	"slices"
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
			Name: "nvsentinel", Chart: "nvsentinel", Version: "v1.22.0",
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

	// The exact NAME SET, not a count. Both catch a surviving chart default,
	// but the set keeps working when the mixin legitimately grows (the NPD
	// policies below) and still fails on an upstream rename (node-not-ready
	// becomes ReplaceNotReadyNode in the version #2596 bumps to), because the
	// new name is not in this list. Anything added here has to be deliberate.
	policies := objectMonitorPolicies(t, nvsentinel)
	wantNames := []string{
		"gpu-operator-pods-health",
		"network-operator-pod-health",
		"NPDXfsShutdown",
		"NPDCperHardwareErrorFatal",
		"NPDReadonlyFilesystem",
	}
	gotNames := make([]string, 0, len(policies))
	for _, entry := range policies {
		policy, _ := entry.(map[string]any)
		name, _ := policy["name"].(string)
		gotNames = append(gotNames, name)
	}
	slices.Sort(gotNames)
	sortedWant := slices.Clone(wantNames)
	slices.Sort(sortedWant)
	if !slices.Equal(gotNames, sortedWant) {
		t.Fatalf("policy names = %v, want exactly %v -- an unexpected entry means a chart default survived the list replacement", gotNames, sortedWant)
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

	// The NPD policies read Node Conditions, so every shape assertion above is
	// wrong for them: a different resource kind, a different node association,
	// and no grace period (a latched condition is already the debounce).
	// STORE_ONLY is the load-bearing one -- upstream recommends REPLACE_VM,
	// and shipping that unvalidated would let a filesystem fault replace a VM.
	for name, wantCondition := range map[string]string{
		"NPDXfsShutdown":            "XfsShutdown",
		"NPDCperHardwareErrorFatal": "CperHardwareErrorFatal",
		"NPDReadonlyFilesystem":     "ReadonlyFilesystem",
	} {
		policy := policyByName(policies, name)
		if policy == nil {
			t.Errorf("policy %q missing from rendered policies", name)
			continue
		}
		if policy["enabled"] != true {
			t.Errorf("policy %q enabled = %v, want true", name, policy["enabled"])
		}
		resource, _ := policy["resource"].(map[string]any)
		if got, _ := resource["kind"].(string); got != "Node" {
			t.Errorf("policy %q resource.kind = %q, want Node", name, got)
		}
		nodeAssociation, _ := policy["nodeAssociation"].(map[string]any)
		if got, _ := nodeAssociation["expression"].(string); got != "resource.metadata.name" {
			t.Errorf("policy %q nodeAssociation.expression = %q, want resource.metadata.name", name, got)
		}
		predicate, _ := policy["predicate"].(map[string]any)
		if expr, _ := predicate["expression"].(string); !strings.Contains(expr, wantCondition) {
			t.Errorf("policy %q predicate does not read condition %q: %s", name, wantCondition, expr)
		}
		healthEvent, _ := policy["healthEvent"].(map[string]any)
		if got, _ := healthEvent["processingStrategy"].(string); got != "STORE_ONLY" {
			t.Errorf("policy %q processingStrategy = %q, want STORE_ONLY -- REPLACE_VM is unvalidated", name, got)
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

// objectMonitorOperandIdentities records, per operator, the label its operand
// DaemonSet pods carry and the component version that label was READ OFF A
// LIVE DEPLOYMENT at.
//
// Neither label is a documented API. Both were obtained by installing the
// operator at the version below, driving it to create its operands, and reading
// the labels off the result. They are not the same label between the two
// operators, which is exactly why neither can be assumed.
//
// The two were verified to different depths, and that difference matters:
//
//	gpu-operator     confirmed on a live EKS H100 cluster. All 9 operand
//	                 DaemonSets carry it, INCLUDING nvidia-driver-daemonset,
//	                 nvidia-container-toolkit-daemonset and nvidia-dcgm, and
//	                 the running Pods inherit it (the predicate matches Pods,
//	                 not DaemonSets). The two pods in that namespace which do
//	                 NOT carry it are correctly outside the policy anyway: the
//	                 operator's own Deployment (ReplicaSet-owned) and
//	                 nvidia-cuda-validator (a ClusterPolicy-owned Pod, not a
//	                 DaemonSet).
//	network-operator verified only on Kind, against a hand-written
//	                 NicClusterPolicy, on the mofed driver and rdma-shared-dp
//	                 operands. No AICR cluster with network-operator was
//	                 available to confirm it, and AICR ships its own
//	                 NicClusterPolicy manifests which may enable other
//	                 operands. Treat this one as the weaker of the two.
var objectMonitorOperandIdentities = []struct {
	component    string
	verifiedAt   string
	label        string
	policy       string
	notCoveredBy string
}{
	{
		component:    "gpu-operator",
		verifiedAt:   "v26.7.0",
		label:        "app.kubernetes.io/managed-by",
		policy:       "gpu-operator-pods-health",
		notCoveredBy: "the bundled node-feature-discovery subchart, which is a Helm dependency rather than a ClusterPolicy operand",
	},
	{
		component:    "network-operator",
		verifiedAt:   "26.4.1",
		label:        "ds-owner",
		policy:       "network-operator-pod-health",
		notCoveredBy: "nv-ipam-node, which omits the label upstream",
	},
}

// TestObjectMonitorOperandIdentityPinnedToVerifiedVersion is the drift guard
// the render test cannot be.
//
// The render test renders the NVSENTINEL chart and confirms the policy asks for
// a given label — it compares the mixin against itself, so it stays green if
// GPU Operator or Network Operator renames the label it stamps on its operands.
// Nothing inside this repo can observe that rename: the labels come from the
// operators' own controllers, not from any chart AICR renders.
//
// So the assumption is bound to the version it was verified against instead.
// Bumping gpu-operator or network-operator in recipes/registry.yaml fails this
// test, which forces someone to re-read the labels off the new version before
// the bump can land. That is a weaker guarantee than a live check and it is the
// honest one: it converts a silent no-op into a required revalidation step.
func TestObjectMonitorOperandIdentityPinnedToVerifiedVersion(t *testing.T) {
	_, store := objectMonitorStore(t)

	registry, err := GetComponentRegistryFor(store.provider)
	if err != nil {
		t.Fatalf("GetComponentRegistryFor: %v", err)
	}
	ref, ok := findComponentRefByName(store.Mixins[objectMonitorMixin].Spec.ComponentRefs, "nvsentinel")
	if !ok {
		t.Fatal("mixin has no nvsentinel componentRef")
	}
	policies := objectMonitorPolicies(t, ref)

	for _, identity := range objectMonitorOperandIdentities {
		t.Run(identity.component, func(t *testing.T) {
			comp := registry.Get(identity.component)
			if comp == nil {
				t.Fatalf("%s not found in registry", identity.component)
			}
			if comp.Helm.DefaultVersion != identity.verifiedAt {
				t.Errorf(
					"%s is pinned at %q but its operand identity label %q was only verified against %q.\n"+
						"Re-verify before shipping this bump: install %s %s, let it create its operands, and read\n"+
						"  kubectl get ds -n %s -o jsonpath='{range .items[*]}{.metadata.name}{\"\\t\"}{.spec.template.metadata.labels}{\"\\n\"}{end}'\n"+
						"If the label still holds, update verifiedAt here. If it changed, update the predicate in\n"+
						"recipes/mixins/%s.yaml too -- otherwise policy %q silently matches nothing.",
					identity.component, comp.Helm.DefaultVersion, identity.label, identity.verifiedAt,
					identity.component, comp.Helm.DefaultVersion, comp.Helm.DefaultNamespace,
					objectMonitorMixin, identity.policy)
			}

			// The mixin must still actually require the label. Without this the
			// version pin above would keep passing after someone deleted the
			// clause, which is the over-match this guard exists to prevent.
			policy := policyByName(policies, identity.policy)
			if policy == nil {
				t.Fatalf("policy %q not found in mixin", identity.policy)
			}
			predicate, _ := policy["predicate"].(map[string]any)
			expr, _ := predicate["expression"].(string)
			if !strings.Contains(expr, "'"+identity.label+"'") {
				t.Errorf("policy %q does not require the %s operand identity %q; it would match any DaemonSet pod in the namespace (not covered either way: %s)",
					identity.policy, identity.component, identity.label, identity.notCoveredBy)
			}
		})
	}
}
