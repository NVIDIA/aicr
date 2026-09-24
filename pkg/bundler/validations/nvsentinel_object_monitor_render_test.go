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

package validations

import (
	"context"
	"strings"
	"testing"

	aicrhelm "github.com/NVIDIA/aicr/pkg/helm"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// nvsentinelObjectMonitorImage is the subchart image the Object Monitor runs,
// as disclosed in docs/user/container-images.md. The static BOM cannot see a
// mixin-gated subchart, so this is the only check keeping that note honest.
const nvsentinelObjectMonitorImage = "ghcr.io/nvidia/nvsentinel/kubernetes-object-monitor:v1.22.0"

// objectMonitorWatchedNamespaces returns every namespace each policy must
// match, derived from the catalog rather than hardcoded: the registry's
// defaultNamespace for the component, plus the namespace os-talos relocates
// it to. A policy that misses one watches a namespace nothing runs in.
func objectMonitorWatchedNamespaces(t *testing.T, store *recipe.MetadataStore, component string) []string {
	t.Helper()
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	comp := registry.Get(component)
	if comp == nil {
		t.Fatalf("%s not found in registry", component)
	}
	namespaces := []string{comp.Helm.DefaultNamespace}

	talos, ok := store.Mixins["os-talos"]
	if !ok {
		t.Fatal("os-talos mixin not present; namespace relocation can no longer be checked")
	}
	for _, ref := range talos.Spec.ComponentRefs {
		if ref.Name == component && ref.Namespace != "" {
			namespaces = append(namespaces, ref.Namespace)
		}
	}
	return namespaces
}

// objectMonitorOperandIdentity is the label each operator stamps on the
// DaemonSet pods it owns, and the two are not the same.
//
// What this pins is narrow and worth stating: that the rendered policy still
// ASKS for the label. It cannot tell whether the operator still SETS it --
// this renders the nvsentinel chart, and the labels come from the GPU/Network
// Operator controllers. Bumping either operator is caught by
// pkg/recipe's TestObjectMonitorOperandIdentityPinnedToVerifiedVersion instead,
// which binds each label to the version it was read at.
//
// Quoted as they appear in the CEL expression, not bare: Helm stamps
// `app.kubernetes.io/managed-by: Helm` onto every object it renders (29 times
// in this chart), and renderedPolicyBlock's last block runs to end-of-document,
// so a bare-key search would match those instead of the predicate.
var objectMonitorOperandIdentity = map[string]string{
	"gpu-operator":     "'app.kubernetes.io/managed-by'",
	"network-operator": "'ds-owner'",
}

// renderedPolicyBlock returns the chart-rendered TOML for one named policy:
// everything from its `[[policies]]` header up to the next one. Assertions
// scoped to a single policy need this -- the rendered document contains both,
// so a whole-document search cannot tell them apart.
func renderedPolicyBlock(rendered, policy string) (string, bool) {
	blocks := strings.Split(rendered, "[[policies]]")
	for _, block := range blocks[1:] {
		if strings.Contains(block, `name = "`+policy+`"`) {
			return block, true
		}
	}
	return "", false
}

// otherComponent names the operator the given policy is NOT about, so each
// policy can be checked for the other's namespaces leaking into it.
func otherComponent(component string) string {
	if component == "gpu-operator" {
		return "network-operator"
	}
	return "gpu-operator"
}

// nvsentinelObjectMonitorResolvedValues composes the values a real adopting
// bundle produces: the base chain's nvsentinel componentRef resolved through
// its values file, with the shipped mixin's overrides merged on top.
func nvsentinelObjectMonitorResolvedValues(t *testing.T, store *recipe.MetadataStore) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), nvsentinelObservabilityChartTimeout)
	defer cancel()

	mixin, ok := store.Mixins["nvsentinel-object-monitor"]
	if !ok {
		t.Fatal("nvsentinel-object-monitor mixin not present; check recipes/mixins/")
	}
	var overrides map[string]any
	for _, c := range mixin.Spec.ComponentRefs {
		if c.Name == "nvsentinel" {
			overrides = c.Overrides
		}
	}
	if overrides == nil {
		t.Fatal("mixin has no nvsentinel componentRef")
	}

	var baseRef recipe.ComponentRef
	found := false
	for _, c := range store.Base.Spec.ComponentRefs {
		if c.Name == "nvsentinel" {
			baseRef = c
			found = true
		}
	}
	if !found {
		t.Fatal("nvsentinel not in the base chain; check recipes/overlays/base.yaml")
	}

	// Fresh map so nothing aliases the cached metadata store across runs.
	composed := map[string]any{}
	mergeValuesForRender(composed, baseRef.Overrides)
	mergeValuesForRender(composed, overrides)

	ref := baseRef
	ref.Overrides = composed
	values, err := recipe.GetComponentValuesWithContext(ctx, nil, &ref)
	if err != nil {
		t.Fatalf("GetComponentValuesWithContext: %v", err)
	}
	return values
}

// TestNVSentinelObjectMonitorChartRender renders the pinned nvsentinel chart
// with the values a real adopting bundle produces and asserts the Object
// Monitor's policies materialize in the chart's generated config, with every
// namespace the catalog can deploy the watched components into.
//
// Everything this guards fails silently at runtime: a renamed values key
// drops the mixin's policies and restores the chart's own node-not-ready
// default, and a namespace the policy doesn't match is a monitor that runs,
// reports healthy, and watches nothing.
//
// Pulls the chart over the network by mutable tag, so it is excluded from
// `make test`/`make qualify` (always -short), same gating as
// TestNVSentinelObservabilityChartRender. Run on demand:
// `go test ./pkg/bundler/validations/... -run TestNVSentinelObjectMonitorChartRender`.
func TestNVSentinelObjectMonitorChartRender(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live chart-render integration test in short mode")
	}
	requireHelmForObservabilityRender(t)

	ctx, cancel := context.WithTimeout(context.Background(), nvsentinelObservabilityChartTimeout)
	defer cancel()

	store, err := recipe.LoadMetadataStoreFor(ctx, nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	comp := registry.Get("nvsentinel")
	if comp == nil {
		t.Fatal("nvsentinel not found in component registry")
	}

	rendered, err := aicrhelm.RenderChart(ctx, aicrhelm.ChartInput{
		Name:       "nvsentinel",
		Chart:      comp.Helm.DefaultChart,
		Repository: comp.Helm.DefaultRepository,
		Version:    comp.Helm.DefaultVersion,
		Namespace:  comp.Helm.DefaultNamespace,
		Values:     nvsentinelObjectMonitorResolvedValues(t, store),
	})
	if err != nil {
		t.Fatalf("helm template failed: %v\noutput:\n%s", err, rendered)
	}
	out := string(rendered)

	for policy, component := range map[string]string{
		"gpu-operator-pods-health":    "gpu-operator",
		"network-operator-pod-health": "network-operator",
	} {
		block, ok := renderedPolicyBlock(out, policy)
		if !ok {
			t.Errorf("policy %q missing from the rendered config; the values key the mixin sets may have been renamed upstream", policy)
			continue
		}
		// Scoped to this policy's own block: searching the whole render would
		// pass even if the two policies had their namespaces swapped, which is
		// two dead policies rather than one.
		for _, namespace := range objectMonitorWatchedNamespaces(t, store, component) {
			if !strings.Contains(block, "'"+namespace+"'") {
				t.Errorf("rendered policy %q does not match namespace %q that %s deploys into", policy, namespace, component)
			}
		}
		for _, other := range objectMonitorWatchedNamespaces(t, store, otherComponent(component)) {
			if strings.Contains(block, "'"+other+"'") {
				t.Errorf("rendered policy %q matches %q, which belongs to the other operator's policy", policy, other)
			}
		}
		// The operand-identity label. Without it the predicate matches any
		// DaemonSet pod in the namespace, so an unrelated workload raises a
		// fatal event blaming the operator. Asserted against the rendered
		// output because an NVSentinel chart bump that mangles the expression
		// would otherwise leave the policy quietly over-broad.
		identity := objectMonitorOperandIdentity[component]
		if !strings.Contains(block, identity) {
			t.Errorf("rendered policy %q does not require the %s operand identity %q; it would match any DaemonSet pod in the namespace", policy, component, identity)
		}
		if otherIdentity := objectMonitorOperandIdentity[otherComponent(component)]; strings.Contains(block, otherIdentity) {
			t.Errorf("rendered policy %q requires %q, which is the other operator's identity", policy, otherIdentity)
		}
	}

	// Setting `policies` replaces the chart's default list wholesale rather
	// than merging, so the rendered config must carry the mixin's own entries
	// and nothing else. The count is derived from the mixin rather than
	// hardcoded, so appending a policy there does not silently weaken this
	// into a check that a chart default survived -- which is what it exists
	// to catch, including across the rename upstream has already made
	// (node-not-ready -> ReplaceNotReadyNode, landing with #2596).
	mixinPolicies := objectMonitorMixinPolicyNames(t, nvsentinelObjectMonitorResolvedValues(t, store))
	if got := strings.Count(out, "[[policies]]"); got != len(mixinPolicies) {
		t.Errorf("rendered config has %d policy blocks, want the mixin's %d -- a chart default may no longer be replaced", got, len(mixinPolicies))
	}
	for _, name := range mixinPolicies {
		if !strings.Contains(out, name) {
			t.Errorf("rendered config is missing policy %q", name)
		}
	}

	if !strings.Contains(out, nvsentinelObjectMonitorImage) {
		t.Errorf("rendered chart does not carry %s; docs/user/container-images.md has drifted from the chart", nvsentinelObjectMonitorImage)
	}
}

// objectMonitorMixinPolicyNames returns the policy names the mixin declares,
// read through the same resolved values the render above uses, so the
// assertions track the mixin instead of a literal that drifts the moment a
// policy is added.
func objectMonitorMixinPolicyNames(t *testing.T, values map[string]any) []string {
	t.Helper()
	kom, _ := values["kubernetes-object-monitor"].(map[string]any)
	policies, _ := kom["policies"].([]any)
	names := make([]string, 0, len(policies))
	for _, entry := range policies {
		policy, _ := entry.(map[string]any)
		if name, _ := policy["name"].(string); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		t.Fatal("mixin declares no policies; the render assertions would be vacuous")
	}

	return names
}
