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
	"slices"
	"testing"
)

const okeToolkitHardeningManifest = "components/nodewright-customizations/manifests/toolkit-hardening-oke.yaml"

// TestOKEToolkitHardeningManifest pins the NodeWright CR that hardens the OKE
// node-image container toolkit: nvidia-setup oci-any on every node OKE's
// bootstrap labels nvidia.com/gpu=true, tolerating every taint, with no
// interrupt, and taint-gating new nodes until it completes. Node scope stays
// fixed when the bundler injects accelerated-node selectors and tolerations.
func TestOKEToolkitHardeningManifest(t *testing.T) {
	injected := map[string]any{
		"acceleratedNodeSelector": map[string]any{"nodeGroup": "gpu-worker"},
		"acceleratedTolerations": []any{
			map[string]any{"key": "nvidia.com/gpu", "operator": "Exists", "effect": "NoSchedule"},
		},
	}
	for name, values := range map[string]map[string]any{"defaults": {}, "injected scheduling": injected} {
		t.Run(name, func(t *testing.T) {
			cr := renderToolkitHardening(t, okeToolkitHardeningManifest, values)
			if cr == nil {
				t.Fatal("manifest rendered no NodeWright CR")
			}
			if got := valueAtPath[string](t, cr, "metadata", "name"); got != "toolkit-hardening" {
				t.Errorf("metadata.name = %q, want toolkit-hardening", got)
			}
			if got := valueAtPath[int](t, cr, "spec", "priority"); got >= 200 {
				t.Errorf("spec.priority = %d, want below the default 200 so the CR precedes tuning", got)
			}
			if !valueAtPath[bool](t, cr, "spec", "runtimeRequired") {
				t.Error("spec.runtimeRequired = false, want true")
			}
			selector := valueAtPath[map[string]any](t, cr, "spec", "nodeSelectors", "matchLabels")
			if len(selector) != 1 || selector["nvidia.com/gpu"] != "true" {
				t.Errorf("nodeSelectors.matchLabels = %v, want only nvidia.com/gpu=true", selector)
			}
			tolerations := valueAtPath[[]any](t, cr, "spec", "additionalTolerations")
			if len(tolerations) != 1 {
				t.Fatalf("additionalTolerations = %v, want a single wildcard", tolerations)
			}
			if tol, _ := tolerations[0].(map[string]any); len(tol) != 1 || tol["operator"] != "Exists" {
				t.Errorf("additionalTolerations[0] = %v, want {operator: Exists}", tolerations[0])
			}

			packages := valueAtPath[map[string]any](t, cr, "spec", "packages")
			if len(packages) != 1 {
				t.Fatalf("packages = %v, want only nvidia-setup-toolkit", sortedKeys(packages))
			}
			pkg := valueAtPath[map[string]any](t, packages, "nvidia-setup-toolkit")
			if _, ok := pkg["interrupt"]; ok {
				t.Error("nvidia-setup-toolkit declares an interrupt; the toolkit re-reads its config per container start")
			}
			if got := valueAtPath[string](t, pkg, "configMap", "service"); got != "oci" {
				t.Errorf("configMap.service = %q, want oci", got)
			}
			if got := valueAtPath[string](t, pkg, "configMap", "accelerator"); got != "any" {
				t.Errorf("configMap.accelerator = %q, want any", got)
			}
		})
	}

	t.Run("enabled=false", func(t *testing.T) {
		if cr := renderToolkitHardening(t, okeToolkitHardeningManifest, map[string]any{"enabled": false}); cr != nil {
			t.Errorf("enabled=false rendered a CR: %v", cr)
		}
	})
}

// TestOKEToolkitHardeningCoverage pins that every OKE recipe carries the
// toolkit hardening, and that no other service does.
func TestOKEToolkitHardeningCoverage(t *testing.T) {
	hasHardening := func(r *RecipeResult) bool {
		ref := r.GetComponentRef("nodewright-customizations")
		return ref != nil && ref.IsEnabled() && slices.Contains(ref.ManifestFiles, okeToolkitHardeningManifest)
	}

	leaves, err := ResolveLeaves(t.Context(), ResolveLeavesOptions{})
	if err != nil {
		t.Fatalf("ResolveLeaves: %v", err)
	}
	var sawOKE bool
	for _, leaf := range leaves {
		if leaf.Entry.Criteria == nil {
			continue
		}
		isOKE := leaf.Entry.Criteria.Service == CriteriaServiceOKE
		if leaf.Err != nil || leaf.Result == nil {
			if isOKE {
				t.Errorf("%s: OKE leaf did not resolve: %v", leaf.Entry.Name, leaf.Err)
			}
			continue
		}
		sawOKE = sawOKE || isOKE
		if got := hasHardening(leaf.Result); got != isOKE {
			t.Errorf("%s: carries OKE toolkit hardening = %v, want %v", leaf.Entry.Name, got, isOKE)
		}
	}
	if !sawOKE {
		t.Fatal("no OKE leaf resolved; coverage is vacuous")
	}
}
