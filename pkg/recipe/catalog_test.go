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
	"strings"
	"testing"
)

// catalogStore builds a MetadataStore from an in-memory provider that has:
//   - base.yaml (required)
//   - overlays/eks-training.yaml           criteria: service=eks, intent=training
//   - overlays/h100-eks-training.yaml      criteria: service=eks, accel=h100, intent=training  base: eks-training
//   - overlays/h100-eks-ubuntu-training.yaml criteria: service=eks, accel=h100, os=ubuntu, intent=training  base: h100-eks-training
//   - overlays/gb200-eks-ubuntu-training.yaml criteria: service=eks, accel=gb200, os=ubuntu, intent=training  base: eks-training
func catalogStore(t *testing.T) *MetadataStore {
	t.Helper()

	files := map[string][]byte{
		"overlays/base.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: base
spec:
  componentRefs: []
`),
		"overlays/eks-training.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: eks-training
spec:
  criteria:
    service: eks
    intent: training
  componentRefs: []
`),
		"overlays/h100-eks-training.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: h100-eks-training
spec:
  base: eks-training
  criteria:
    service: eks
    accelerator: h100
    intent: training
  componentRefs: []
`),
		"overlays/h100-eks-ubuntu-training.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: h100-eks-ubuntu-training
spec:
  base: h100-eks-training
  criteria:
    service: eks
    accelerator: h100
    os: ubuntu
    intent: training
  componentRefs: []
`),
		"overlays/gb200-eks-ubuntu-training.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: gb200-eks-ubuntu-training
spec:
  base: eks-training
  criteria:
    service: eks
    accelerator: gb200
    os: ubuntu
    intent: training
  componentRefs: []
`),
	}

	dp := newInMemoryProvider("catalog-test", files)
	t.Cleanup(func() { EvictCachedStore(dp) })

	store, err := LoadMetadataStoreFor(context.Background(), dp)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	return store
}

func TestListCatalog_AllOverlays(t *testing.T) {
	store := catalogStore(t)

	entries := store.ListCatalog(nil)

	// We have 4 overlays (base is not in Overlays).
	if len(entries) != 4 {
		t.Fatalf("expected 4 entries, got %d: %v", len(entries), entryNames(entries))
	}

	// Entries must be sorted by name.
	if !isSortedByName(entries) {
		t.Errorf("entries not sorted by name: %v", entryNames(entries))
	}

	// IsLeaf: leaves are those whose name does not appear as any overlay's spec.base.
	//   ancestors: eks-training (base of h100-eks-training and gb200-eks-ubuntu-training)
	//              h100-eks-training (base of h100-eks-ubuntu-training)
	// IsLeaf=true:  gb200-eks-ubuntu-training, h100-eks-ubuntu-training
	// IsLeaf=false: eks-training, h100-eks-training
	wantLeaf := map[string]bool{
		"eks-training":              false,
		"h100-eks-training":         false,
		"h100-eks-ubuntu-training":  true,
		"gb200-eks-ubuntu-training": true,
	}
	for _, e := range entries {
		want, ok := wantLeaf[e.Name]
		if !ok {
			t.Errorf("unexpected entry %q", e.Name)
			continue
		}
		if e.IsLeaf != want {
			t.Errorf("entry %q: IsLeaf=%v want %v", e.Name, e.IsLeaf, want)
		}
	}
}

func TestListCatalog_SourcePropagated(t *testing.T) {
	store := catalogStore(t)

	entries := store.ListCatalog(nil)
	for _, e := range entries {
		// The inMemoryDataProvider returns "catalog-test:<path>" for every file.
		if !strings.HasPrefix(e.Source, "catalog-test:") {
			t.Errorf("entry %q: Source = %q, want prefix %q", e.Name, e.Source, "catalog-test:")
		}
	}
}

func TestListCatalog_Filter(t *testing.T) {
	store := catalogStore(t)

	tests := []struct {
		name      string
		filter    *Criteria
		wantNames []string // nil means "expect all 4"
		wantLen   int
	}{
		{
			name:    "nil filter returns all",
			filter:  nil,
			wantLen: 4,
		},
		{
			name:    "sorted by name",
			filter:  nil,
			wantLen: 4,
		},
		{
			name:    "service=eks matches all",
			filter:  &Criteria{Service: CriteriaServiceEKS},
			wantLen: 4,
		},
		{
			name:      "accelerator=h100",
			filter:    &Criteria{Accelerator: CriteriaAcceleratorH100},
			wantNames: []string{"h100-eks-training", "h100-eks-ubuntu-training"},
		},
		{
			name:    "service=eks and intent=training matches all",
			filter:  &Criteria{Service: CriteriaServiceEKS, Intent: CriteriaIntentTraining},
			wantLen: 4,
		},
		{
			name:      "os=ubuntu",
			filter:    &Criteria{OS: CriteriaOSUbuntu},
			wantNames: []string{"gb200-eks-ubuntu-training", "h100-eks-ubuntu-training"},
		},
		{
			name:    "service=gke no match",
			filter:  &Criteria{Service: CriteriaServiceGKE},
			wantLen: 0,
		},
		{
			name: "all-any filter same as nil",
			filter: &Criteria{
				Service:     CriteriaServiceAny,
				Accelerator: CriteriaAcceleratorAny,
				Intent:      CriteriaIntentAny,
				OS:          CriteriaOSAny,
				Platform:    CriteriaPlatformAny,
			},
			wantLen: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := store.ListCatalog(tt.filter)
			if tt.wantNames != nil {
				if !namesMatch(entries, tt.wantNames) {
					t.Errorf("got %v, want %v", entryNames(entries), tt.wantNames)
				}
			} else if len(entries) != tt.wantLen {
				t.Errorf("got %d entries, want %d: %v", len(entries), tt.wantLen, entryNames(entries))
			}
			if !isSortedByName(entries) {
				t.Errorf("entries not sorted: %v", entryNames(entries))
			}
		})
	}
}

func TestMatchesCatalogFilter(t *testing.T) {
	tests := []struct {
		name    string
		overlay *Criteria
		filter  *Criteria
		want    bool
	}{
		{
			name:    "nil filter matches everything",
			overlay: &Criteria{Service: CriteriaServiceEKS},
			filter:  nil,
			want:    true,
		},
		{
			name: "exact match on subset of dimensions",
			overlay: &Criteria{
				Service:     CriteriaServiceEKS,
				Accelerator: CriteriaAcceleratorH100,
				Intent:      CriteriaIntentTraining,
				OS:          CriteriaOSUbuntu,
			},
			filter: &Criteria{Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100},
			want:   true,
		},
		{
			name:    "mismatched dimension",
			overlay: &Criteria{Service: CriteriaServiceGKE},
			filter:  &Criteria{Service: CriteriaServiceEKS},
			want:    false,
		},
		{
			// Unlike Criteria.Matches for recipe resolution, catalog filter with
			// accelerator=h100 must NOT match an overlay whose accelerator is unset/any.
			name:    "wildcard overlay does not match specific filter",
			overlay: &Criteria{Service: CriteriaServiceEKS},
			filter:  &Criteria{Accelerator: CriteriaAcceleratorH100},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesCatalogFilter(tt.overlay, tt.filter)
			if got != tt.want {
				t.Errorf("matchesCatalogFilter() = %v, want %v", got, tt.want)
			}
		})
	}
}

// entryNames extracts the names from a slice for readable error messages.
func entryNames(entries []CatalogEntry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name
	}
	return names
}

// isSortedByName checks that entries are in ascending name order.
func isSortedByName(entries []CatalogEntry) bool {
	for i := 1; i < len(entries); i++ {
		if entries[i].Name < entries[i-1].Name {
			return false
		}
	}
	return true
}

// namesMatch checks that the entry names match wantNames exactly (order-insensitive).
func namesMatch(entries []CatalogEntry, wantNames []string) bool {
	if len(entries) != len(wantNames) {
		return false
	}
	got := make(map[string]bool, len(entries))
	for _, e := range entries {
		got[e.Name] = true
	}
	for _, n := range wantNames {
		if !got[n] {
			return false
		}
	}
	return true
}
