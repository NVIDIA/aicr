// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"testing"
)

const (
	testRecipeBase = "base"
	testOverlayEKS = "eks"
)

func TestMetadataStore_GetValuesFile(t *testing.T) {
	store := &MetadataStore{
		ValuesFiles: map[string][]byte{
			"components/gpu-operator/values.yaml": []byte("driver:\n  enabled: true"),
		},
	}

	tests := []struct {
		name     string
		filename string
		wantErr  bool
	}{
		{"existing file", "components/gpu-operator/values.yaml", false},
		{"missing file", "components/missing/values.yaml", true},
		{"empty filename", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, err := store.GetValuesFile(tt.filename)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetValuesFile() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && len(content) == 0 {
				t.Error("expected non-empty content")
			}
		})
	}
}

func TestMetadataStore_GetRecipeByName(t *testing.T) {
	baseMeta := &RecipeMetadata{}
	baseMeta.Metadata.Name = testRecipeBase

	overlayMeta := &RecipeMetadata{}
	overlayMeta.Metadata.Name = "h100-eks"

	store := &MetadataStore{
		Base: baseMeta,
		Overlays: map[string]*RecipeMetadata{
			"h100-eks": overlayMeta,
		},
	}

	tests := []struct {
		name      string
		input     string
		wantName  string
		wantFound bool
	}{
		{"empty returns base", "", testRecipeBase, true},
		{"base returns base", testRecipeBase, testRecipeBase, true},
		{"existing overlay", "h100-eks", "h100-eks", true},
		{"missing overlay", "nonexistent", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta, found := store.GetRecipeByName(tt.input)
			if found != tt.wantFound {
				t.Errorf("found = %v, want %v", found, tt.wantFound)
				return
			}
			if found && meta.Metadata.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", meta.Metadata.Name, tt.wantName)
			}
		})
	}

	// Test with nil base
	t.Run("nil base", func(t *testing.T) {
		nilStore := &MetadataStore{Overlays: map[string]*RecipeMetadata{}}
		meta, found := nilStore.GetRecipeByName("")
		if found {
			t.Error("expected found=false for nil base")
		}
		if meta != nil {
			t.Error("expected nil meta for nil base")
		}
	})
}

func TestMetadataStore_ResolveInheritanceChain(t *testing.T) {
	baseMeta := &RecipeMetadata{}
	baseMeta.Metadata.Name = testRecipeBase

	eksMeta := &RecipeMetadata{}
	eksMeta.Metadata.Name = testOverlayEKS

	eksTraining := &RecipeMetadata{}
	eksTraining.Metadata.Name = "eks-training"
	eksTraining.Spec.Base = testOverlayEKS

	t.Run("single overlay", func(t *testing.T) {
		store := &MetadataStore{
			Base: baseMeta,
			Overlays: map[string]*RecipeMetadata{
				testOverlayEKS: eksMeta,
			},
		}
		chain, err := store.resolveInheritanceChain(testOverlayEKS)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(chain) != 2 {
			t.Fatalf("chain length = %d, want 2", len(chain))
		}
	})

	t.Run("two-level chain", func(t *testing.T) {
		store := &MetadataStore{
			Base: baseMeta,
			Overlays: map[string]*RecipeMetadata{
				testOverlayEKS: eksMeta,
				"eks-training": eksTraining,
			},
		}
		chain, err := store.resolveInheritanceChain("eks-training")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(chain) != 3 {
			t.Fatalf("chain length = %d, want 3", len(chain))
		}
	})

	t.Run("missing recipe", func(t *testing.T) {
		store := &MetadataStore{
			Base:     baseMeta,
			Overlays: map[string]*RecipeMetadata{},
		}
		_, err := store.resolveInheritanceChain("nonexistent")
		if err == nil {
			t.Error("expected error for missing recipe")
		}
	})

	t.Run("cycle detection", func(t *testing.T) {
		cycleA := &RecipeMetadata{}
		cycleA.Metadata.Name = "a"
		cycleA.Spec.Base = "b"

		cycleB := &RecipeMetadata{}
		cycleB.Metadata.Name = "b"
		cycleB.Spec.Base = "a"

		store := &MetadataStore{
			Base: baseMeta,
			Overlays: map[string]*RecipeMetadata{
				"a": cycleA,
				"b": cycleB,
			},
		}
		_, err := store.resolveInheritanceChain("a")
		if err == nil {
			t.Error("expected error for circular inheritance")
		}
	})

	t.Run("nil base in store", func(t *testing.T) {
		store := &MetadataStore{
			Overlays: map[string]*RecipeMetadata{
				testOverlayEKS: eksMeta,
			},
		}
		chain, err := store.resolveInheritanceChain(testOverlayEKS)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(chain) != 1 {
			t.Fatalf("chain length = %d, want 1", len(chain))
		}
	})
}

func TestMetadataStore_FindMatchingOverlays(t *testing.T) {
	baseMeta := &RecipeMetadata{}
	baseMeta.Metadata.Name = testRecipeBase

	eksOverlay := &RecipeMetadata{}
	eksOverlay.Metadata.Name = "eks-overlay"
	eksOverlay.Spec.Criteria = &Criteria{
		Service: CriteriaServiceEKS,
	}

	gkeOverlay := &RecipeMetadata{}
	gkeOverlay.Metadata.Name = "gke-overlay"
	gkeOverlay.Spec.Criteria = &Criteria{
		Service: CriteriaServiceGKE,
	}

	noCriteriaOverlay := &RecipeMetadata{}
	noCriteriaOverlay.Metadata.Name = "no-criteria"

	store := &MetadataStore{
		Base: baseMeta,
		Overlays: map[string]*RecipeMetadata{
			"eks-overlay": eksOverlay,
			"gke-overlay": gkeOverlay,
			"no-criteria": noCriteriaOverlay,
		},
	}

	t.Run("matching criteria", func(t *testing.T) {
		criteria := &Criteria{Service: CriteriaServiceEKS}
		matches := store.FindMatchingOverlays(criteria)
		found := false
		for _, m := range matches {
			if m.Metadata.Name == "eks-overlay" {
				found = true
			}
		}
		if !found {
			t.Error("expected eks-overlay to match")
		}
	})

	t.Run("no matches", func(t *testing.T) {
		criteria := &Criteria{Service: CriteriaServiceAKS}
		matches := store.FindMatchingOverlays(criteria)
		if len(matches) != 0 {
			t.Errorf("expected 0 matches, got %d", len(matches))
		}
	})

	t.Run("empty store returns empty", func(t *testing.T) {
		emptyStore := &MetadataStore{
			Base:     baseMeta,
			Overlays: map[string]*RecipeMetadata{},
		}
		criteria := &Criteria{Service: CriteriaServiceEKS}
		matches := emptyStore.FindMatchingOverlays(criteria)
		if len(matches) != 0 {
			t.Errorf("expected 0 matches for empty store, got %d", len(matches))
		}
	})
}
