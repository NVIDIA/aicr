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
	"maps"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/manifest"
	"gopkg.in/yaml.v3"
)

// nodewrightTuningManifest is the shared nodewright-customizations manifest
// (EKS and AKS) carrying the nvidia-setup/nvidia-tuned Skyhook packages.
const nodewrightTuningManifest = "components/nodewright-customizations/manifests/tuning.yaml"

// Single-package tuning manifests: their only Skyhook package IS the tuning
// package, so the tuningEnabled gate suppresses the whole CR instead of one
// package.
const (
	nodewrightTuningGKEManifest     = "components/nodewright-customizations/manifests/tuning-gke.yaml"
	nodewrightTuningGenericManifest = "components/nodewright-customizations/manifests/tuning-generic.yaml"
)

// renderNodewrightTuning renders the tuning manifest with the given component
// values and returns the parsed Skyhook packages map plus the dependsOn map of
// the nvidia-setup-full package.
func renderNodewrightTuning(t *testing.T, content []byte, values map[string]any) (packages, fullDependsOn map[string]any) {
	t.Helper()

	rendered, err := manifest.Render(content, manifest.RenderInput{
		ComponentName: "nodewright-customizations",
		Namespace:     "skyhook",
		ChartName:     "nodewright-customizations",
		ChartVersion:  "0.1.0",
		Values:        values,
	})
	if err != nil {
		t.Fatalf("render tuning manifest: %v", err)
	}
	var skyhook map[string]any
	if err := yaml.Unmarshal(rendered, &skyhook); err != nil {
		t.Fatalf("unmarshal rendered tuning manifest: %v\n%s", err, rendered)
	}
	packages = valueAtPath[map[string]any](t, skyhook, "spec", "packages")
	fullDependsOn = valueAtPath[map[string]any](t, skyhook, "spec", "packages", "nvidia-setup-full", "dependsOn")
	return packages, fullDependsOn
}

// TestNodewrightTuningGateAcrossCatalog renders the shared nodewright
// tuning.yaml manifest for every leaf recipe that wires it and pins the
// per-service tuningEnabled contract:
//
//   - AKS leaves ship tuningEnabled: false under the Azure-managed driver
//     profile: the rendered Skyhook omits the nvidia-tuned package (no
//     tuning-triggered node reboot) and nvidia-setup-full depends directly on
//     nvidia-setup-kernel. nvidia-setup itself stays enabled — on AKS it
//     carries the RDMA host prep (MEMLOCK limits + IB module loading).
//   - Non-AKS leaves (EKS) are unchanged: nvidia-tuned renders and
//     nvidia-setup-full depends on it.
//
// A final subtest pins the documented re-enable path: rendering an AKS leaf's
// values with tuningEnabled flipped to true restores the full package chain
// (--set nodewrightcustomizations:tuningEnabled=true).
func TestNodewrightTuningGateAcrossCatalog(t *testing.T) {
	ctx := context.Background()
	leaves, err := ResolveLeaves(ctx, ResolveLeavesOptions{})
	if err != nil {
		t.Fatalf("ResolveLeaves: %v", err)
	}

	var sawAKS, sawNonAKS bool
	var aksContent []byte
	var aksValues map[string]any
	for _, leaf := range leaves {
		if leaf.Err != nil || leaf.Result == nil || leaf.Entry.Criteria == nil {
			continue
		}
		ref := leaf.Result.GetComponentRef("nodewright-customizations")
		if ref == nil || !slices.Contains(ref.ManifestFiles, nodewrightTuningManifest) {
			continue
		}
		t.Run(leaf.Entry.Name, func(t *testing.T) {
			values, verr := leaf.Result.GetValuesForComponentWithContext(ctx, "nodewright-customizations")
			if verr != nil {
				t.Fatalf("GetValuesForComponentWithContext(nodewright-customizations): %v", verr)
			}
			content, cerr := GetManifestContentWithContext(ctx, leaf.Result.DataProvider(), nodewrightTuningManifest)
			if cerr != nil {
				t.Fatalf("GetManifestContentWithContext(%q): %v", nodewrightTuningManifest, cerr)
			}
			packages, fullDependsOn := renderNodewrightTuning(t, content, values)

			// nvidia-setup renders for every service: on AKS it carries the
			// RDMA host prep that replaced the ib-node-config DaemonSet.
			for _, pkg := range []string{"nvidia-setup-kernel", "nvidia-setup-full"} {
				if _, ok := packages[pkg]; !ok {
					t.Errorf("rendered packages missing %q: %v", pkg, sortedKeys(packages))
				}
			}

			if leaf.Entry.Criteria.Service == CriteriaServiceAKS {
				sawAKS = true
				aksContent, aksValues = content, values
				if got, ok := values["tuningEnabled"]; !ok || got != false {
					t.Errorf("AKS leaf tuningEnabled = %v (present=%v), want false", got, ok)
				}
				if _, ok := packages["nvidia-tuned"]; ok {
					t.Error("AKS rendering must omit the nvidia-tuned package (tuningEnabled: false)")
				}
				if _, ok := fullDependsOn["nvidia-setup-kernel"]; !ok {
					t.Errorf("AKS nvidia-setup-full.dependsOn = %v, want nvidia-setup-kernel", fullDependsOn)
				}
				if _, ok := fullDependsOn["nvidia-tuned"]; ok {
					t.Errorf("AKS nvidia-setup-full.dependsOn = %v, must not reference the omitted nvidia-tuned", fullDependsOn)
				}
			} else {
				sawNonAKS = true
				if _, ok := packages["nvidia-tuned"]; !ok {
					t.Errorf("non-AKS rendering must keep the nvidia-tuned package: %v", sortedKeys(packages))
				}
				if _, ok := fullDependsOn["nvidia-tuned"]; !ok {
					t.Errorf("non-AKS nvidia-setup-full.dependsOn = %v, want nvidia-tuned", fullDependsOn)
				}
			}
		})
	}
	if !sawAKS {
		t.Error("no AKS leaf wiring the nodewright tuning manifest was resolved")
	}
	if !sawNonAKS {
		t.Error("no non-AKS leaf wiring the nodewright tuning manifest was resolved")
	}

	t.Run("re-enable via tuningEnabled=true restores nvidia-tuned", func(t *testing.T) {
		if aksContent == nil {
			t.Skip("no AKS leaf resolved; covered by the catalog assertions above")
		}
		values := make(map[string]any, len(aksValues))
		maps.Copy(values, aksValues)
		values["tuningEnabled"] = true
		packages, fullDependsOn := renderNodewrightTuning(t, aksContent, values)
		if _, ok := packages["nvidia-tuned"]; !ok {
			t.Errorf("tuningEnabled=true must restore the nvidia-tuned package: %v", sortedKeys(packages))
		}
		if _, ok := fullDependsOn["nvidia-tuned"]; !ok {
			t.Errorf("tuningEnabled=true nvidia-setup-full.dependsOn = %v, want nvidia-tuned", fullDependsOn)
		}
	})
}

// renderNodewrightTuningRaw renders a nodewright tuning manifest and returns
// the raw output, so callers can assert on whole-CR presence/absence (the
// single-package manifests gate the entire Skyhook CR, not one package).
func renderNodewrightTuningRaw(t *testing.T, content []byte, values map[string]any) string {
	t.Helper()

	rendered, err := manifest.Render(content, manifest.RenderInput{
		ComponentName: "nodewright-customizations",
		Namespace:     "skyhook",
		ChartName:     "nodewright-customizations",
		ChartVersion:  "0.1.0",
		Values:        values,
	})
	if err != nil {
		t.Fatalf("render tuning manifest: %v", err)
	}
	return string(rendered)
}

// TestNodewrightTuningGateSinglePackageManifests pins the tuningEnabled
// contract on the single-package tuning manifests (tuning-gke.yaml,
// tuning-generic.yaml), for every catalog leaf that wires one:
//
//   - default (tuningEnabled absent, no leaf sets it on these manifests):
//     the Skyhook CR renders — behavior identical to before the gate;
//   - explicit tuningEnabled=false suppresses the whole CR (the tuning
//     package is the CR's only package, so an empty packages map would be
//     the alternative);
//   - explicit tuningEnabled=true renders byte-identically to the default.
func TestNodewrightTuningGateSinglePackageManifests(t *testing.T) {
	ctx := context.Background()
	leaves, err := ResolveLeaves(ctx, ResolveLeavesOptions{})
	if err != nil {
		t.Fatalf("ResolveLeaves: %v", err)
	}

	singlePackageManifests := []string{nodewrightTuningGKEManifest, nodewrightTuningGenericManifest}
	seen := map[string]bool{}
	for _, leaf := range leaves {
		if leaf.Err != nil || leaf.Result == nil || leaf.Entry.Criteria == nil {
			continue
		}
		ref := leaf.Result.GetComponentRef("nodewright-customizations")
		if ref == nil {
			continue
		}
		for _, manifestPath := range singlePackageManifests {
			if !slices.Contains(ref.ManifestFiles, manifestPath) {
				continue
			}
			seen[manifestPath] = true
			t.Run(leaf.Entry.Name+"/"+path.Base(manifestPath), func(t *testing.T) {
				values, verr := leaf.Result.GetValuesForComponentWithContext(ctx, "nodewright-customizations")
				if verr != nil {
					t.Fatalf("GetValuesForComponentWithContext(nodewright-customizations): %v", verr)
				}
				content, cerr := GetManifestContentWithContext(ctx, leaf.Result.DataProvider(), manifestPath)
				if cerr != nil {
					t.Fatalf("GetManifestContentWithContext(%q): %v", manifestPath, cerr)
				}
				if got, ok := values["tuningEnabled"]; ok {
					t.Fatalf("leaf sets tuningEnabled=%v on a single-package tuning manifest; update this test's default-render assumption", got)
				}

				defaultRender := renderNodewrightTuningRaw(t, content, values)
				if !strings.Contains(defaultRender, "kind: Skyhook") {
					t.Fatalf("default rendering must produce the Skyhook CR:\n%s", defaultRender)
				}

				disabled := make(map[string]any, len(values)+1)
				maps.Copy(disabled, values)
				disabled["tuningEnabled"] = false
				if got := renderNodewrightTuningRaw(t, content, disabled); strings.Contains(got, "kind: Skyhook") {
					t.Errorf("tuningEnabled=false must suppress the whole Skyhook CR:\n%s", got)
				}

				enabled := make(map[string]any, len(values)+1)
				maps.Copy(enabled, values)
				enabled["tuningEnabled"] = true
				if got := renderNodewrightTuningRaw(t, content, enabled); got != defaultRender {
					t.Errorf("tuningEnabled=true must render identically to the default (absent):\ngot:\n%s\nwant:\n%s", got, defaultRender)
				}
			})
		}
	}
	for _, manifestPath := range singlePackageManifests {
		if !seen[manifestPath] {
			t.Errorf("no catalog leaf wiring %s was resolved", manifestPath)
		}
	}
}
