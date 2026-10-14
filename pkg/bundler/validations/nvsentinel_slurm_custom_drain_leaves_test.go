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
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestNVSentinelSlurmCustomDrainOnEverySlurmLeaf composes the real mixins
// onto every platform: slurm leaf and runs the gate with the node selectors a
// cloud Slurm bundle must pass, so a mixin edit that breaks the plugin's
// contract fails `make test`, not only the Kind and KWOK lanes. The Kind leaf
// opts the Slurm control pods out of pinning (it is one node), so the
// plugin's drain would stall there and the gate rejects it. Each combination is layered over the embedded catalog as an external
// --data directory, the way an operator composes it, so the cached store is
// never mutated.
func TestNVSentinelSlurmCustomDrainOnEverySlurmLeaf(t *testing.T) {
	ctx := t.Context()
	store, err := recipe.LoadMetadataStoreFor(ctx, nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	var leaves []string
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria != nil && overlay.Spec.Criteria.Platform == recipe.CriteriaPlatformSlurm {
			leaves = append(leaves, name)
		}
	}
	sort.Strings(leaves)
	if len(leaves) == 0 {
		t.Fatal("no platform: slurm leaves -- the overlay walk is broken")
	}

	combos := []struct {
		name        string
		mixins      []string
		wantBlocked string
	}{
		{name: "quarantine+slinky-drainer", mixins: []string{"nvsentinel-quarantine", slinkyDrainerMixinName}},
		{name: "remediation+slinky-drainer", mixins: []string{"nvsentinel-remediation", slinkyDrainerMixinName}},
		{name: "remediation alone", mixins: []string{"nvsentinel-remediation"},
			wantBlocked: "compose the " + slinkyDrainerMixinName + " mixin"},
		// Quarantine alone evicts no slurmd and promises no repair.
		{name: "quarantine alone", mixins: []string{"nvsentinel-quarantine"}},
		{name: "observe+slinky-drainer", mixins: []string{"nvsentinel-observe", slinkyDrainerMixinName},
			wantBlocked: "does not combine with nvsentinel-observe"},
		{name: "slinky-drainer alone", mixins: []string{slinkyDrainerMixinName},
			wantBlocked: "nothing creates DrainRequests"},
	}
	for _, combo := range combos {
		t.Run(combo.name, func(t *testing.T) {
			layered := layerSlurmLeaves(t, leaves, combo.mixins)
			composed, err := recipe.LoadMetadataStoreFor(ctx, layered)
			if err != nil {
				t.Fatalf("LoadMetadataStoreFor(layered): %v", err)
			}
			for _, leaf := range leaves {
				t.Run(leaf, func(t *testing.T) {
					overlay, ok := composed.Overlays[leaf]
					if !ok {
						t.Fatalf("%s missing from the layered store", leaf)
					}
					result, err := composed.BuildRecipeResult(ctx, overlay.Spec.Criteria)
					if err != nil {
						t.Fatalf("BuildRecipeResult: %v", err)
					}
					cfg := config.NewConfig(
						config.WithSystemNodeSelector(map[string]string{"nodeGroup": "system"}),
						config.WithAcceleratedNodeSelector(map[string]string{"nodeGroup": "gpu"}))
					_, errs := CheckNVSentinelSlurmCustomDrainCoherent(ctx, nvsentinelComponent, result, cfg, nil)
					msgs := make([]string, 0, len(errs))
					for _, e := range errs {
						msgs = append(msgs, e.Error())
					}
					joined := strings.Join(msgs, "\n")
					want := combo.wantBlocked
					if want == "" && strings.HasPrefix(leaf, "h100-kind-") && slices.Contains(combo.mixins, slinkyDrainerMixinName) {
						want = "renders no node selector"
					}
					switch {
					case want == "" && len(msgs) > 0:
						t.Errorf("want pass, got %q", msgs)
					case want != "" && !strings.Contains(joined, want):
						t.Errorf("want a message containing %q, got %q", want, msgs)
					}
				})
			}
		})
	}
}

// layerSlurmLeaves writes each leaf, with mixins appended to its own, into a
// fresh --data directory and returns a provider layering it over the
// embedded catalog.
func layerSlurmLeaves(t *testing.T, leaves, mixins []string) recipe.DataProvider {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "overlays"), 0o755); err != nil {
		t.Fatal(err)
	}
	registry := "kind: ComponentRegistry\napiVersion: aicr.run/v1beta1\nmetadata:\n  name: slurm-custom-drain-test\ncomponents: []\n"
	if err := os.WriteFile(filepath.Join(dir, recipe.RegistryFileName), []byte(registry), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, leaf := range leaves {
		data, err := fs.ReadFile(recipe.GetEmbeddedFS(), "overlays/"+leaf+".yaml")
		if err != nil {
			t.Fatalf("reading embedded overlay %s: %v", leaf, err)
		}
		var doc map[string]any
		if err = yaml.Unmarshal(data, &doc); err != nil {
			t.Fatalf("parsing %s: %v", leaf, err)
		}
		spec, _ := doc["spec"].(map[string]any)
		if spec == nil {
			t.Fatalf("%s has no spec", leaf)
		}
		existing, _ := spec["mixins"].([]any)
		for _, m := range mixins {
			existing = append(existing, m)
		}
		spec["mixins"] = existing
		out, err := yaml.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, "overlays", leaf+".yaml"), out, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	layered, err := recipe.NewLayeredDataProvider(recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "."),
		recipe.LayeredProviderConfig{ExternalDir: dir})
	if err != nil {
		t.Fatalf("NewLayeredDataProvider: %v", err)
	}
	return layered
}

// TestSlurmControlSelectorPathsMatchRegistry keeps the placement rule's paths
// equal to the registry's nodeScheduling paths, which are what the bundler
// injects into: a path added there and not here would go unchecked.
func TestSlurmControlSelectorPathsMatchRegistry(t *testing.T) {
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	for _, c := range []struct {
		component           string
		system, accelerated []string
	}{
		{slinkySlurmComponent, slinkySlurmControlSelectorPaths, []string{slinkySlurmWorkerSelectorPath}},
		{slurmAccountingMariaDBComponent, []string{slurmAccountingMariaDBSelectorPath}, nil},
	} {
		comp := registry.Get(c.component)
		if comp == nil {
			t.Fatalf("%s not in the registry", c.component)
		}
		if got := comp.GetSystemNodeSelectorPaths(); !slices.Equal(got, c.system) {
			t.Errorf("%s system nodeSelectorPaths = %v, the gate checks %v", c.component, got, c.system)
		}
		if got := comp.GetAcceleratedNodeSelectorPaths(); !slices.Equal(got, c.accelerated) {
			t.Errorf("%s accelerated nodeSelectorPaths = %v, the gate checks %v", c.component, got, c.accelerated)
		}
	}
}
