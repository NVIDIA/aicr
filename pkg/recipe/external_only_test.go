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
	"io/fs"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe/providertest"
)

func TestExternalOnlyProviderNeverReadsEmbedded(t *testing.T) {
	files := map[string][]byte{
		"registry.yaml":                         []byte(providertest.EmptyRegistryYAML),
		"overlays/base.yaml":                    []byte("apiVersion: aicr.run/v1beta1\nkind: RecipeMetadata\nmetadata:\n  name: base\nspec: {}\n"),
		"overlays/h100-eks-training.yaml":       []byte("apiVersion: aicr.run/v1beta1\nkind: RecipeMetadata\nmetadata:\n  name: external-leaf\nspec:\n  base: base\n  criteria:\n    service: external-service\n"),
		"mixins/os-ubuntu.yaml":                 []byte("apiVersion: aicr.run/v1beta1\nkind: RecipeMixin\nmetadata:\n  name: external-mixin\nspec: {}\n"),
		"validators/catalog.yaml":               []byte("apiVersion: validator.nvidia.com/v1alpha1\nkind: ValidatorCatalog\nvalidators: []\n"),
		"components/gpu-operator/values.yaml":   []byte("externalMarker: true\n"),
		"checks/gpu-operator/health-check.yaml": []byte("external-check\n"),
	}
	dir := providertest.WriteDir(t, files)
	// A nil embedded provider panics if any fallback, merge or walk touches it.
	dp, err := NewLayeredDataProvider(nil, LayeredProviderConfig{ExternalDir: dir, ExternalOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { EvictCachedStore(dp); EvictCachedRegistry(dp); EvictCachedCriteriaRegistry(dp) })
	for name, want := range files {
		t.Run(name, func(t *testing.T) {
			got, readErr := dp.ReadFile(t.Context(), name)
			if readErr != nil || string(got) != string(want) {
				t.Fatalf("read %s: %q, %v", name, got, readErr)
			}
			if dp.Source(name) != CatalogSourceExternal {
				t.Fatalf("source %s", dp.Source(name))
			}
		})
	}
	seen := map[string]bool{}
	if walkErr := dp.WalkDir(t.Context(), "", func(name string, entry fs.DirEntry, entryErr error) error {
		if entryErr != nil {
			return entryErr
		}
		if !entry.IsDir() {
			seen[name] = true
		}
		return nil
	}); walkErr != nil {
		t.Fatal(walkErr)
	}
	if len(seen) != len(files) {
		t.Fatalf("walk returned %v", seen)
	}
	store, err := LoadMetadataStoreFor(t.Context(), dp)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.Overlays) != 1 || len(store.Mixins) != 1 {
		t.Fatalf("unexpected catalog: %d overlays, %d mixins", len(store.Overlays), len(store.Mixins))
	}
	registry, err := GetComponentRegistryFor(dp)
	if err != nil {
		t.Fatal(err)
	}
	if len(registry.Names()) != 0 {
		t.Fatalf("embedded registry leaked: %v", registry.Names())
	}
	for _, name := range []string{"components/gpu-operator/missing.yaml", "overlays/h100-eks-ubuntu-training.yaml", "checks/missing/health-check.yaml"} {
		if _, readErr := dp.ReadFile(t.Context(), name); readErr == nil || !strings.Contains(readErr.Error(), name) {
			t.Fatalf("missing %s: %v", name, readErr)
		}
	}
}
