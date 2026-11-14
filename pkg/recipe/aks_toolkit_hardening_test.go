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

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/manifest"
)

const aksToolkitHardeningManifest = "components/nodewright-customizations/manifests/toolkit-hardening-aks.yaml"

// renderAKSToolkitHardening renders the AKS toolkit-hardening manifest with the
// given nodewright-customizations values and returns the parsed NodeWright CR,
// or nil when the values suppress it.
func renderAKSToolkitHardening(t *testing.T, values map[string]any) map[string]any {
	t.Helper()
	return renderToolkitHardening(t, aksToolkitHardeningManifest, values)
}

// renderToolkitHardening renders a toolkit-hardening manifest with the given
// nodewright-customizations values and returns the parsed NodeWright CR, or nil
// when the values suppress it.
func renderToolkitHardening(t *testing.T, path string, values map[string]any) map[string]any {
	t.Helper()
	content, err := GetEmbeddedFS().ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	rendered, err := manifest.Render(content, manifest.RenderInput{
		ComponentName: "nodewright-customizations",
		Namespace:     "nodewright",
		ChartName:     "nodewright-customizations",
		ChartVersion:  "0.1.0",
		Values:        values,
	})
	if err != nil {
		t.Fatalf("render %s: %v", path, err)
	}
	var cr map[string]any
	if err := yaml.Unmarshal(rendered, &cr); err != nil {
		t.Fatalf("unmarshal rendered manifest: %v\n%s", err, rendered)
	}
	return cr
}

// TestAKSToolkitHardeningManifest pins the NodeWright CR that hardens the AKS
// node-image container toolkit: nvidia-setup aks-any on every node carrying
// the AKS accelerator label, tolerating every taint, with no interrupt, and
// taint-gating new nodes until it completes. Node scope stays fixed when the
// bundler injects accelerated-node selectors and tolerations, so the hardening
// never narrows to the subset the tuning packages target.
func TestAKSToolkitHardeningManifest(t *testing.T) {
	injected := map[string]any{
		"acceleratedNodeSelector": map[string]any{"pool": "gpu-a"},
		"acceleratedTolerations": []any{
			map[string]any{"key": "nvidia.com/gpu", "operator": "Exists", "effect": "NoSchedule"},
		},
	}
	for name, values := range map[string]map[string]any{"defaults": {}, "injected scheduling": injected} {
		t.Run(name, func(t *testing.T) {
			cr := renderAKSToolkitHardening(t, values)
			if cr == nil {
				t.Fatal("manifest rendered no NodeWright CR")
			}
			if got := valueAtPath[string](t, cr, "kind"); got != "NodeWright" {
				t.Errorf("kind = %q, want NodeWright", got)
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
			if !valueAtPath[bool](t, cr, "spec", "autoTaintNewNodes") {
				t.Error("spec.autoTaintNewNodes = false, want true")
			}
			selector := valueAtPath[map[string]any](t, cr, "spec", "nodeSelectors", "matchLabels")
			if len(selector) != 1 || selector["kubernetes.azure.com/accelerator"] != "nvidia" {
				t.Errorf("nodeSelectors.matchLabels = %v, want only kubernetes.azure.com/accelerator=nvidia", selector)
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
			if got := valueAtPath[string](t, pkg, "image"); got != "ghcr.io/nvidia/nodewright-packages/nvidia-setup" {
				t.Errorf("image = %q, want nvidia-setup", got)
			}
			if _, ok := pkg["interrupt"]; ok {
				t.Error("nvidia-setup-toolkit declares an interrupt; the toolkit re-reads its config per container start")
			}
			if got := valueAtPath[string](t, pkg, "configMap", "service"); got != "aks" {
				t.Errorf("configMap.service = %q, want aks", got)
			}
			if got := valueAtPath[string](t, pkg, "configMap", "accelerator"); got != "any" {
				t.Errorf("configMap.accelerator = %q, want any", got)
			}
			for _, field := range []string{"cpuRequest", "cpuLimit", "memoryRequest", "memoryLimit"} {
				if _, ok := valueAtPath[map[string]any](t, pkg, "resources")[field]; !ok {
					t.Errorf("resources.%s unset; the package would take the LimitRange default request", field)
				}
			}
		})
	}

	t.Run("autoTaintNewNodes=false", func(t *testing.T) {
		cr := renderAKSToolkitHardening(t, map[string]any{"autoTaintNewNodes": false})
		if valueAtPath[bool](t, cr, "spec", "autoTaintNewNodes") {
			t.Error("spec.autoTaintNewNodes = true, want false")
		}
	})

	t.Run("enabled=false", func(t *testing.T) {
		if cr := renderAKSToolkitHardening(t, map[string]any{"enabled": false}); cr != nil {
			t.Errorf("enabled=false rendered a CR: %v", cr)
		}
	})
}

// TestAKSToolkitHardeningCoverage pins that every AKS recipe carries the
// toolkit hardening, under both gpuStack profiles: the catalog leaves and the
// criteria-only recipes that have no accelerator leaf. No other service
// carries it.
func TestAKSToolkitHardeningCoverage(t *testing.T) {
	hasHardening := func(r *RecipeResult) bool {
		ref := r.GetComponentRef("nodewright-customizations")
		return ref != nil && ref.IsEnabled() && slices.Contains(ref.ManifestFiles, aksToolkitHardeningManifest)
	}

	leaves, err := ResolveLeaves(t.Context(), ResolveLeavesOptions{})
	if err != nil {
		t.Fatalf("ResolveLeaves: %v", err)
	}
	var sawAKS bool
	for _, leaf := range leaves {
		if leaf.Entry.Criteria == nil {
			continue
		}
		isAKS := leaf.Entry.Criteria.Service == CriteriaServiceAKS
		if leaf.Err != nil || leaf.Result == nil {
			if isAKS {
				t.Errorf("%s: AKS leaf did not resolve: %v", leaf.Entry.Name, leaf.Err)
			}
			continue
		}
		sawAKS = sawAKS || isAKS
		if got := hasHardening(leaf.Result); got != isAKS {
			t.Errorf("%s: carries AKS toolkit hardening = %v, want %v", leaf.Entry.Name, got, isAKS)
		}
	}
	if !sawAKS {
		t.Fatal("no AKS leaf resolved; coverage is vacuous")
	}

	for _, intent := range []CriteriaIntentType{CriteriaIntentTraining, CriteriaIntentInference} {
		for _, profile := range []string{"gpuStack=azure-managed", "gpuStack=operator-managed"} {
			criteria := &Criteria{Service: CriteriaServiceAKS, Intent: intent}
			result, err := NewBuilder().BuildFromCriteriaWithProfile(t.Context(), criteria, profile)
			if err != nil {
				t.Fatalf("%s %s: BuildFromCriteriaWithProfile: %v", criteria.String(), profile, err)
			}
			if !hasHardening(result) {
				t.Errorf("%s %s: missing AKS toolkit hardening", criteria.String(), profile)
			}
		}
	}
}

// TestAKSToolkitEnvHardened asserts both AKS gpu-operator values files carry the
// exact uppercase toolkit-hardening env vars bound to their correct string
// values, so the operator-owned toolkit is hardened in the GPU-Operator-managed
// profile, where the operator's toolkit, not the node image's, owns the
// runtime. It parses the YAML and asserts the exact name→value mapping (rather
// than searching for names and values independently), so swapping the two
// security values would fail the test.
func TestAKSToolkitEnvHardened(t *testing.T) {
	// The two security-relevant keys and their required values. Both matter:
	// the env-var key must be "false" (deny the NVIDIA_VISIBLE_DEVICES path) and
	// the volume-mounts key must be "true" (allow the device plugin's allocated
	// volume-mounts). A swap would silently reopen the env-var isolation gap.
	wantPairs := map[string]string{
		"ACCEPT_NVIDIA_VISIBLE_DEVICES_ENVVAR_WHEN_UNPRIVILEGED": "false",
		"ACCEPT_NVIDIA_VISIBLE_DEVICES_AS_VOLUME_MOUNTS":         "true",
	}

	// toolkitValues models just the toolkit.env list we assert on.
	type toolkitValues struct {
		Toolkit struct {
			Env []struct {
				Name  string `yaml:"name"`
				Value string `yaml:"value"`
			} `yaml:"env"`
		} `yaml:"toolkit"`
	}

	for _, f := range []string{
		"components/gpu-operator/values-aks.yaml",
		"components/gpu-operator/values-aks-training.yaml",
	} {
		content, err := GetEmbeddedFS().ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		var tv toolkitValues
		if uerr := yaml.Unmarshal(content, &tv); uerr != nil {
			t.Fatalf("unmarshal %s: %v", f, uerr)
		}

		// Build the actual name→value map from the parsed env list.
		got := make(map[string]string, len(tv.Toolkit.Env))
		for _, e := range tv.Toolkit.Env {
			got[e.Name] = e.Value
		}

		for name, want := range wantPairs {
			val, ok := got[name]
			if !ok {
				t.Errorf("%s: toolkit.env missing %s", f, name)
				continue
			}
			if val != want {
				t.Errorf("%s: toolkit.env %s = %q, want %q", f, name, val, want)
			}
		}
	}
}

// TestTuningGenericAccelerator pins the AKS RDMA opt-out
// (--set nodewrightcustomizations:accelerator=generic): AICR's "generic"
// profile value reaches nvidia-setup as "any" (its aks-any combination, the
// toolkit hardening only) and nvidia-tuned unchanged as "generic". Any other
// accelerator passes through to both packages as is.
func TestTuningGenericAccelerator(t *testing.T) {
	content, err := GetEmbeddedFS().ReadFile(nodewrightTuningManifest)
	if err != nil {
		t.Fatalf("read %s: %v", nodewrightTuningManifest, err)
	}
	tests := []struct {
		accelerator, wantSetup, wantTuned string
	}{
		{"generic", "any", "generic"},
		{"h100", "h100", "h100"},
	}
	for _, tt := range tests {
		t.Run(tt.accelerator, func(t *testing.T) {
			packages, _ := renderNodewrightTuning(t, content, map[string]any{
				"service": "aks", "accelerator": tt.accelerator, "intent": "inference", "tuningEnabled": true,
			})
			for _, pkg := range []string{"nvidia-setup-kernel", "nvidia-setup-full"} {
				if got := valueAtPath[string](t, packages, pkg, "configMap", "accelerator"); got != tt.wantSetup {
					t.Errorf("%s configMap.accelerator = %q, want %q", pkg, got, tt.wantSetup)
				}
			}
			if got := valueAtPath[string](t, packages, "nvidia-tuned", "configMap", "accelerator"); got != tt.wantTuned {
				t.Errorf("nvidia-tuned configMap.accelerator = %q, want %q", got, tt.wantTuned)
			}
		})
	}
}
