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

package attestation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	cdx "github.com/CycloneDX/cyclonedx-go"

	k8scollector "github.com/NVIDIA/aicr/pkg/collector/k8s"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
)

func TestCatalogVersion(t *testing.T) {
	tests := []struct {
		name string
		cat  *catalog.ValidatorCatalog
		want string
	}{
		{"nil catalog", nil, ""},
		{"no metadata", &catalog.ValidatorCatalog{}, ""},
		{"metadata with version", &catalog.ValidatorCatalog{
			Metadata: &catalog.CatalogMetadata{Version: "v1.4.2"},
		}, "v1.4.2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CatalogVersion(tt.cat); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDedupValidatorImages(t *testing.T) {
	tests := []struct {
		name string
		cat  *catalog.ValidatorCatalog
		want []string
	}{
		{"nil catalog", nil, nil},
		{"no validators", &catalog.ValidatorCatalog{}, nil},
		{
			name: "dedupes by image preserving order",
			cat: &catalog.ValidatorCatalog{Validators: []catalog.ValidatorEntry{
				{Name: "a", Image: "ghcr.io/x/deployment:v1"},
				{Name: "b", Image: "ghcr.io/x/deployment:v1"},
				{Name: "c", Image: "ghcr.io/x/performance:v1"},
				{Name: "d", Image: "ghcr.io/x/conformance:v1"},
			}},
			want: []string{
				"ghcr.io/x/deployment:v1",
				"ghcr.io/x/performance:v1",
				"ghcr.io/x/conformance:v1",
			},
		},
		{
			name: "skips entries with empty image",
			cat: &catalog.ValidatorCatalog{Validators: []catalog.ValidatorEntry{
				{Name: "a", Image: ""},
				{Name: "b", Image: "ghcr.io/x/deployment:v1"},
			}},
			want: []string{"ghcr.io/x/deployment:v1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DedupValidatorImages(tt.cat)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d (got=%v)", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestValidatorImagesForPredicate(t *testing.T) {
	cat := &catalog.ValidatorCatalog{Validators: []catalog.ValidatorEntry{
		{Name: "a", Image: "ghcr.io/x/deployment:v1"},
		{Name: "b", Image: "ghcr.io/x/deployment:v1"},
		{Name: "c", Image: "ghcr.io/x/performance:v1"},
	}}
	got := ValidatorImagesForPredicate(cat)
	want := []ValidatorImage{
		{Image: "ghcr.io/x/deployment:v1"},
		{Image: "ghcr.io/x/performance:v1"},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (got=%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	if ValidatorImagesForPredicate(nil) != nil {
		t.Errorf("nil catalog should produce nil slice")
	}
}

// TestObservedImagesFromSnapshot is a contract test for the snapshot →
// BOM coupling. It pins the collector constants the function depends on
// (measurement.TypeK8s and k8scollector.SubtypeImage), exercises the
// dedupe + non-matching-subtype paths, and asserts that the output order
// is deterministic (sorted by name). Order matters because the auto-BOM
// is signed via the predicate's bom.digest — non-deterministic iteration
// would break reproducible-build invariants.
func TestObservedImagesFromSnapshot(t *testing.T) {
	mkSubtype := func(name string, data map[string]string) measurement.Subtype {
		readings := make(map[string]measurement.Reading, len(data))
		for k, v := range data {
			readings[k] = measurement.Str(v)
		}
		return measurement.Subtype{Name: name, Data: readings}
	}

	tests := []struct {
		name string
		snap *snapshotter.Snapshot
		want []string // expected ordered output
	}{
		{name: "nil snapshot", snap: nil, want: nil},
		{name: "no measurements", snap: &snapshotter.Snapshot{}, want: nil},
		{
			name: "non-K8s measurement ignored",
			snap: &snapshotter.Snapshot{Measurements: []*measurement.Measurement{
				{Type: measurement.TypeOS, Subtypes: []measurement.Subtype{
					mkSubtype(k8scollector.SubtypeImage, map[string]string{"alpine": "3.20"}),
				}},
			}},
			want: nil,
		},
		{
			name: "non-image subtype ignored",
			snap: &snapshotter.Snapshot{Measurements: []*measurement.Measurement{
				{Type: measurement.TypeK8s, Subtypes: []measurement.Subtype{
					mkSubtype("server", map[string]string{"version": "v1.34.0"}),
				}},
			}},
			want: nil,
		},
		{
			name: "K8s image subtype emits sorted, registry-stripped name:tag refs",
			snap: &snapshotter.Snapshot{Measurements: []*measurement.Measurement{
				{Type: measurement.TypeK8s, Subtypes: []measurement.Subtype{
					mkSubtype(k8scollector.SubtypeImage, map[string]string{
						"coredns":     "v1.11",
						"kube-proxy":  "v1.34.0",
						"aws-ebs-csi": "v1.59",
					}),
				}},
			}},
			want: []string{
				"aws-ebs-csi:v1.59",
				"coredns:v1.11",
				"kube-proxy:v1.34.0",
			},
		},
		{
			name: "duplicate refs across subtypes are deduped",
			snap: &snapshotter.Snapshot{Measurements: []*measurement.Measurement{
				{Type: measurement.TypeK8s, Subtypes: []measurement.Subtype{
					mkSubtype(k8scollector.SubtypeImage, map[string]string{"coredns": "v1.11"}),
					mkSubtype(k8scollector.SubtypeImage, map[string]string{"coredns": "v1.11"}),
				}},
			}},
			want: []string{"coredns:v1.11"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ObservedImagesFromSnapshot(tt.snap)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d (got=%v)", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q (full=%v)", i, got[i], tt.want[i], got)
				}
			}
		})
	}
}

func TestLoadOrGenerateBOM_FromPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bom.cdx.json")
	body := []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","version":1}`)
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatalf("setup write: %v", err)
	}
	got, err := LoadOrGenerateBOM(p, nil, nil, nil, "v0")
	if err != nil {
		t.Fatalf("LoadOrGenerateBOM: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("body mismatch: got=%q want=%q", got, body)
	}
}

func TestLoadOrGenerateBOM_PathNotFound(t *testing.T) {
	_, err := LoadOrGenerateBOM("/nonexistent/path/bom.json", nil, nil, nil, "v0")
	if err == nil {
		t.Fatalf("expected error reading missing path")
	}
}

// TestLoadOrGenerateBOM_RejectsOversizedFile pins the os.Open +
// io.LimitReader bound: an operator-supplied path that exceeds
// defaults.MaxBOMBytes must be rejected before the body is materialized
// in memory.
func TestLoadOrGenerateBOM_RejectsOversizedFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "huge.bom.json")
	body := make([]byte, defaults.MaxBOMBytes+1)
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatalf("setup write: %v", err)
	}
	if _, err := LoadOrGenerateBOM(p, nil, nil, nil, "v0"); err == nil {
		t.Fatalf("expected error for oversized BOM file")
	}
}

func TestLoadOrGenerateBOM_AutoFromRecipe(t *testing.T) {
	rec := &recipe.RecipeResult{
		Criteria: &recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
			Intent:      recipe.CriteriaIntentTraining,
		},
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Type: recipe.ComponentTypeHelm, Chart: "gpu-operator", Source: "https://helm.ngc.nvidia.com/nvidia", Version: "v25.10.1"},
		},
	}
	cat := &catalog.ValidatorCatalog{Validators: []catalog.ValidatorEntry{
		{Name: "operator-health", Image: "ghcr.io/x/deployment:latest"},
	}}

	body, err := LoadOrGenerateBOM("", rec, nil, cat, "v0.1.0")
	if err != nil {
		t.Fatalf("LoadOrGenerateBOM auto: %v", err)
	}
	if len(body) == 0 {
		t.Fatalf("auto-gen BOM is empty")
	}
	doc := &cdx.BOM{}
	if err := json.Unmarshal(body, doc); err != nil {
		t.Fatalf("auto-gen BOM is not valid JSON: %v", err)
	}
	if doc.BOMFormat != "CycloneDX" {
		t.Errorf("BOMFormat = %q, want CycloneDX", doc.BOMFormat)
	}
}

func TestBuildAutoBOM_NilRecipeReturnsError(t *testing.T) {
	if _, err := BuildAutoBOM(nil, nil, nil, "v0"); err == nil {
		t.Fatalf("expected error for nil recipe")
	}
}

func TestBuildAutoBOM_IncludesRecipeAndValidatorComponents(t *testing.T) {
	rec := &recipe.RecipeResult{
		Criteria: &recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
		},
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Type: recipe.ComponentTypeHelm, Chart: "gpu-operator", Source: "https://helm.ngc.nvidia.com/nvidia", Version: "v25.10.1"},
			{Name: "disabled-comp", Type: recipe.ComponentTypeHelm, Overrides: map[string]any{"enabled": false}},
		},
	}
	cat := &catalog.ValidatorCatalog{Validators: []catalog.ValidatorEntry{
		{Name: "a", Image: "ghcr.io/x/deployment:latest"},
		{Name: "b", Image: "ghcr.io/x/deployment:latest"},
		{Name: "c", Image: "ghcr.io/x/performance:latest"},
	}}

	body, err := BuildAutoBOM(rec, nil, cat, "v0.1.0")
	if err != nil {
		t.Fatalf("BuildAutoBOM: %v", err)
	}

	doc := &cdx.BOM{}
	if err := json.Unmarshal(body, doc); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var (
		sawGPUOperator bool
		sawValidators  bool
		sawDisabled    bool
	)
	if doc.Components != nil {
		for _, c := range *doc.Components {
			switch c.Name {
			case "gpu-operator":
				sawGPUOperator = true
			case "validators":
				sawValidators = true
			case "disabled-comp":
				sawDisabled = true
			}
		}
	}
	if !sawGPUOperator {
		t.Errorf("expected gpu-operator component in auto BOM")
	}
	if !sawValidators {
		t.Errorf("expected validators meta-component in auto BOM")
	}
	if sawDisabled {
		t.Errorf("disabled component must not appear in auto BOM")
	}
}
