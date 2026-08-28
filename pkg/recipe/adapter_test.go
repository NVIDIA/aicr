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
	stderrors "errors"
	"fmt"
	"io/fs"
	"maps"
	"reflect"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"gopkg.in/yaml.v3"
)

const testVersionV2 = "v2.0"

func TestMergeValues(t *testing.T) {
	tests := []struct {
		name     string
		base     map[string]any
		overlay  map[string]any
		expected map[string]any
	}{
		{
			name: "simple override",
			base: map[string]any{
				"enabled": true,
				"version": "1.0.0",
			},
			overlay: map[string]any{
				"version": "2.0.0",
			},
			expected: map[string]any{
				"enabled": true,
				"version": "2.0.0",
			},
		},
		{
			name: "nested map merge",
			base: map[string]any{
				"driver": map[string]any{
					"enabled":    true,
					"repository": "nvcr.io/nvidia",
					"version":    "1.0.0",
				},
			},
			overlay: map[string]any{
				"driver": map[string]any{
					"version": "2.0.0",
				},
			},
			expected: map[string]any{
				"driver": map[string]any{
					"enabled":    true,
					"repository": "nvcr.io/nvidia",
					"version":    "2.0.0",
				},
			},
		},
		{
			name: "add new key",
			base: map[string]any{
				"enabled": true,
			},
			overlay: map[string]any{
				"newFeature": true,
			},
			expected: map[string]any{
				"enabled":    true,
				"newFeature": true,
			},
		},
		{
			name: "deep nested merge",
			base: map[string]any{
				"driver": map[string]any{
					"config": map[string]any{
						"timeout": 30,
						"retry":   3,
					},
				},
			},
			overlay: map[string]any{
				"driver": map[string]any{
					"config": map[string]any{
						"timeout": 60,
					},
				},
			},
			expected: map[string]any{
				"driver": map[string]any{
					"config": map[string]any{
						"timeout": 60,
						"retry":   3,
					},
				},
			},
		},
		{
			name: "type mismatch - overlay wins",
			base: map[string]any{
				"value": map[string]any{
					"nested": "data",
				},
			},
			overlay: map[string]any{
				"value": "string",
			},
			expected: map[string]any{
				"value": "string",
			},
		},
		{
			name: "null override deletes key",
			base: map[string]any{
				"storageSpec": map[string]any{
					"emptyDir": map[string]any{
						"medium":    "",
						"sizeLimit": "10Gi",
					},
				},
			},
			overlay: map[string]any{
				"storageSpec": map[string]any{
					"emptyDir": nil,
					"volumeClaimTemplate": map[string]any{
						"spec": map[string]any{
							"storageClassName": "managed-csi",
						},
					},
				},
			},
			expected: map[string]any{
				"storageSpec": map[string]any{
					"volumeClaimTemplate": map[string]any{
						"spec": map[string]any{
							"storageClassName": "managed-csi",
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a copy of base to avoid modifying the test data
			dst := make(map[string]any)
			maps.Copy(dst, tt.base)

			// Merge overlay into dst
			mergeValues(dst, tt.overlay)

			// Compare results
			if !mapsEqual(dst, tt.expected) {
				t.Errorf("mergeValues() result mismatch\ngot:  %+v\nwant: %+v", dst, tt.expected)
			}
		})
	}
}

// TestMergeValues_NoSliceAliasing verifies that mutating a slice in the
// merged result does not leak back into the source overlay. The previous
// implementation assigned []any by reference, allowing cached overlays
// to be corrupted by downstream --set or dynamic-injection callers.
func TestMergeValues_NoSliceAliasing(t *testing.T) {
	src := map[string]any{
		"tolerations": []any{
			map[string]any{"key": "nvidia.com/gpu", "operator": "Exists"},
		},
		"env": []any{"FOO=bar"},
	}
	srcOriginalTol := src["tolerations"].([]any)[0].(map[string]any)["key"]
	srcOriginalEnv := src["env"].([]any)[0]

	dst := map[string]any{}
	mergeValues(dst, src)

	// Mutate dst's slices/elements.
	dst["tolerations"].([]any)[0].(map[string]any)["key"] = "MUTATED"
	dst["env"].([]any)[0] = "MUTATED"

	if got := src["tolerations"].([]any)[0].(map[string]any)["key"]; got != srcOriginalTol {
		t.Errorf("src tolerations corrupted via dst alias: got %v want %v", got, srcOriginalTol)
	}
	if got := src["env"].([]any)[0]; got != srcOriginalEnv {
		t.Errorf("src env corrupted via dst alias: got %v want %v", got, srcOriginalEnv)
	}
}

// mapsEqual compares two maps recursively.
func mapsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}

	for key, aVal := range a {
		bVal, exists := b[key]
		if !exists {
			return false
		}

		// If both are maps, compare recursively
		if aMap, aOK := aVal.(map[string]any); aOK {
			if bMap, bOK := bVal.(map[string]any); bOK {
				if !mapsEqual(aMap, bMap) {
					return false
				}
				continue
			}
		}

		// For non-map types, use direct comparison
		if aVal != bVal {
			return false
		}
	}

	return true
}

// TestGetValuesForComponent_InlineOverrides tests the three-way merge:
// base values → ValuesFile → inline Overrides.
func TestGetValuesForComponent_InlineOverrides(t *testing.T) {
	tests := []struct {
		name          string
		setupRecipe   func() *RecipeResult
		componentName string
		wantDriver    string
		wantGDRCopy   bool
		wantGDS       bool
		wantErr       bool
	}{
		{
			name: "inline overrides only (no valuesFile)",
			setupRecipe: func() *RecipeResult {
				return &RecipeResult{
					ComponentRefs: []ComponentRef{
						{
							Name:    "gpu-operator",
							Version: "v25.3.4",
							Overrides: map[string]any{
								"driver": map[string]any{
									"version": "570.86.16",
								},
								"gdrcopy": map[string]any{
									"enabled": true,
								},
								"gds": map[string]any{
									"enabled": true,
								},
							},
						},
					},
				}
			},
			componentName: "gpu-operator",
			wantDriver:    "570.86.16",
			wantGDRCopy:   true,
			wantGDS:       true,
			wantErr:       false,
		},
		{
			name: "valuesFile + inline overrides (hybrid)",
			setupRecipe: func() *RecipeResult {
				// This would load from components/gpu-operator/values.yaml
				// and apply overrides on top
				return &RecipeResult{
					ComponentRefs: []ComponentRef{
						{
							Name:       "gpu-operator",
							Version:    "v25.3.4",
							ValuesFile: "components/gpu-operator/values.yaml",
							Overrides: map[string]any{
								// Override just the driver version
								"driver": map[string]any{
									"version": "570.86.16",
								},
							},
						},
					},
				}
			},
			componentName: "gpu-operator",
			wantDriver:    "570.86.16",
			wantErr:       false,
		},
		{
			name: "valuesFile only (traditional)",
			setupRecipe: func() *RecipeResult {
				// Load from base values file without inline overrides
				return &RecipeResult{
					ComponentRefs: []ComponentRef{
						{
							Name:       "gpu-operator",
							Version:    "v25.3.4",
							ValuesFile: "components/gpu-operator/values.yaml",
						},
					},
				}
			},
			componentName: "gpu-operator",
			wantDriver:    "", // Base values.yaml doesn't have driver.version, skip check
			wantGDRCopy:   false,
			wantGDS:       false,
			wantErr:       false,
		},
		{
			name: "inline overrides take precedence over valuesFile",
			setupRecipe: func() *RecipeResult {
				return &RecipeResult{
					ComponentRefs: []ComponentRef{
						{
							Name:       "gpu-operator",
							Version:    "v25.3.4",
							ValuesFile: "components/gpu-operator/values.yaml", // driver: 550.54.15
							Overrides: map[string]any{
								"driver": map[string]any{
									"version": "999.99.99", // Override with different version
								},
							},
						},
					},
				}
			},
			componentName: "gpu-operator",
			wantDriver:    "999.99.99", // Inline override should win
			wantErr:       false,
		},
		{
			name: "no valuesFile and no overrides (empty)",
			setupRecipe: func() *RecipeResult {
				return &RecipeResult{
					ComponentRefs: []ComponentRef{
						{
							Name:    "test-component",
							Version: "v1.0.0",
						},
					},
				}
			},
			componentName: "test-component",
			wantErr:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recipe := tt.setupRecipe()

			values, err := recipe.GetValuesForComponent(tt.componentName)
			if (err != nil) != tt.wantErr {
				t.Fatalf("GetValuesForComponent() error = %v, wantErr %v", err, tt.wantErr)
			}

			if err != nil {
				return // Expected error, test passes
			}

			// Verify driver version if specified
			if tt.wantDriver != "" {
				driver, ok := values["driver"].(map[string]any)
				if !ok {
					t.Fatalf("driver not found or not a map")
				}
				version, ok := driver["version"].(string)
				if !ok {
					t.Fatalf("driver.version not found or not a string")
				}
				if version != tt.wantDriver {
					t.Errorf("driver.version = %q, want %q", version, tt.wantDriver)
				}
			}

			// Verify gdrcopy if specified
			if tt.wantGDRCopy {
				gdrcopy, ok := values["gdrcopy"].(map[string]any)
				if !ok {
					t.Errorf("gdrcopy not found or not a map")
				} else {
					enabled, ok := gdrcopy["enabled"].(bool)
					if !ok {
						t.Errorf("gdrcopy.enabled not found or not a bool")
					} else if !enabled {
						t.Errorf("gdrcopy.enabled = false, want true")
					}
				}
			}

			// Verify gds if specified
			if tt.wantGDS {
				gds, ok := values["gds"].(map[string]any)
				if !ok {
					t.Errorf("gds not found or not a map")
				} else {
					enabled, ok := gds["enabled"].(bool)
					if !ok {
						t.Errorf("gds.enabled not found or not a bool")
					} else if !enabled {
						t.Errorf("gds.enabled = false, want true")
					}
				}
			}

			t.Logf("Test passed - values merged correctly")
		})
	}
}

// TestGetValuesForComponent_OverridesMergeDeep tests that inline overrides
// merge deeply with existing values, not replace entire maps.
func TestGetValuesForComponent_OverridesMergeDeep(t *testing.T) {
	recipe := &RecipeResult{
		ComponentRefs: []ComponentRef{
			{
				Name:       "gpu-operator",
				Version:    "v25.3.4",
				ValuesFile: "components/gpu-operator/values.yaml",
				Overrides: map[string]any{
					"driver": map[string]any{
						// Only override version, other driver fields should remain
						"version": "999.99.99",
					},
					"newField": map[string]any{
						// Add entirely new field
						"enabled": true,
					},
				},
			},
		},
	}

	values, err := recipe.GetValuesForComponent("gpu-operator")
	if err != nil {
		t.Fatalf("GetValuesForComponent() error = %v", err)
	}

	// Verify driver.version was overridden
	driver, ok := values["driver"].(map[string]any)
	if !ok {
		t.Fatalf("driver not found or not a map")
	}
	version, ok := driver["version"].(string)
	if !ok {
		t.Fatalf("driver.version not found or not a string")
	}
	if version != "999.99.99" {
		t.Errorf("driver.version = %q, want 999.99.99", version)
	}

	// Verify other driver fields still exist (from base values)
	// The base values.yaml should have more than just version
	if len(driver) < 2 {
		t.Errorf("driver map has %d fields, expected more (deep merge should preserve other fields)", len(driver))
	}

	// Verify newField was added
	newField, ok := values["newField"].(map[string]any)
	if !ok {
		t.Errorf("newField not found or not a map")
	} else {
		enabled, ok := newField["enabled"].(bool)
		if !ok || !enabled {
			t.Errorf("newField.enabled = %v, want true", enabled)
		}
	}

	t.Logf("Deep merge works correctly - overrides merged, not replaced")
}

// TestGetComponentValues_BareRef pins the package-level GetComponentValues
// helper added for #1844: it resolves the same effective values a bare
// ComponentRef would merge (base → ValuesFile → Overrides) without a
// RecipeResult, mirroring RecipeResult.GetValuesForComponent. The deployment
// validator uses it to render a component's manifests exactly as the bundler
// would.
func TestGetComponentValues_BareRef(t *testing.T) {
	t.Run("no valuesFile and no overrides returns empty map", func(t *testing.T) {
		got, err := GetComponentValues(&ComponentRef{Name: "manifest-only", Version: "v1.0.0"})
		if err != nil {
			t.Fatalf("GetComponentValues() error = %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("GetComponentValues() = %v, want empty map", got)
		}
	})

	t.Run("inline overrides are merged", func(t *testing.T) {
		ref := &ComponentRef{
			Name: "nodewright-customizations",
			Overrides: map[string]any{
				"tuningEnabled": false,
				"accelerator":   "h100",
			},
		}
		got, err := GetComponentValues(ref)
		if err != nil {
			t.Fatalf("GetComponentValues() error = %v", err)
		}
		if v, ok := got["tuningEnabled"].(bool); !ok || v {
			t.Fatalf("tuningEnabled = %v (%T), want false", got["tuningEnabled"], got["tuningEnabled"])
		}
		if v, _ := got["accelerator"].(string); v != "h100" {
			t.Fatalf("accelerator = %q, want h100", v)
		}
	})

	t.Run("matches RecipeResult.GetValuesForComponent for the same ref", func(t *testing.T) {
		ref := ComponentRef{
			Name:       "gpu-operator",
			Version:    "v25.3.4",
			ValuesFile: "components/gpu-operator/values.yaml",
			Overrides:  map[string]any{"driver": map[string]any{"version": "999.99.99"}},
		}
		viaResult, err := (&RecipeResult{ComponentRefs: []ComponentRef{ref}}).GetValuesForComponent("gpu-operator")
		if err != nil {
			t.Fatalf("GetValuesForComponent() error = %v", err)
		}
		viaBareRef, err := GetComponentValues(&ref)
		if err != nil {
			t.Fatalf("GetComponentValues() error = %v", err)
		}
		if !reflect.DeepEqual(viaResult, viaBareRef) {
			t.Fatalf("GetComponentValues and GetValuesForComponent disagree:\n bare = %v\n result = %v", viaBareRef, viaResult)
		}
	})

	t.Run("nil ref is rejected", func(t *testing.T) {
		if _, err := GetComponentValues(nil); err == nil {
			t.Fatal("GetComponentValues(nil) error = nil, want error")
		}
	})
}

// TestGetValuesForComponent_BuilderIntegration tests inline overrides
// with real recipe building from criteria.
func TestGetValuesForComponent_BuilderIntegration(t *testing.T) {
	ctx := context.Background()
	builder := NewBuilder()

	// Build a recipe (this will load from metadata store)
	criteria := &Criteria{
		Service:     CriteriaServiceEKS,
		Accelerator: CriteriaAcceleratorGB200,
		Intent:      CriteriaIntentTraining,
	}

	result, err := builder.BuildFromCriteria(ctx, criteria)
	if err != nil {
		t.Fatalf("BuildFromCriteria() error = %v", err)
	}

	// Get gpu-operator component
	ref := result.GetComponentRef("gpu-operator")
	if ref == nil {
		t.Fatal("gpu-operator not found in recipe")
	}

	// Load values (this tests the full pipeline)
	values, err := result.GetValuesForComponent("gpu-operator")
	if err != nil {
		t.Fatalf("GetValuesForComponent() error = %v", err)
	}

	// Verify values were loaded
	if len(values) == 0 {
		t.Error("values map is empty")
	}

	t.Logf("Builder integration works - loaded %d top-level keys", len(values))

	// If the recipe has inline overrides, verify they were applied
	if len(ref.Overrides) > 0 {
		t.Logf("   Recipe has %d inline override keys", len(ref.Overrides))
	}
}

func TestGetManifestContent(t *testing.T) {
	t.Run("existing manifest", func(t *testing.T) {
		content, err := GetManifestContent("components/network-operator/manifests/nfd-network-rule.yaml")
		if err != nil {
			t.Fatalf("GetManifestContent() error = %v", err)
		}
		if len(content) == 0 {
			t.Error("expected non-empty content")
		}
	})

	t.Run("missing manifest", func(t *testing.T) {
		_, err := GetManifestContent("components/nonexistent/manifests/missing.yaml")
		if err == nil {
			t.Error("expected error for missing manifest")
		}
	})
}

func TestRecipe_Accessors(t *testing.T) {
	t.Run("GetComponentRef always nil", func(t *testing.T) {
		r := &Recipe{}
		if got := r.GetComponentRef("anything"); got != nil {
			t.Errorf("Recipe.GetComponentRef() = %v, want nil", got)
		}
	})

	t.Run("GetValuesForComponent returns empty map", func(t *testing.T) {
		r := &Recipe{}
		got, err := r.GetValuesForComponent("anything")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Errorf("expected empty map, got %v", got)
		}
	})

	t.Run("GetVersion with nil metadata", func(t *testing.T) {
		r := &Recipe{}
		if got := r.GetVersion(); got != "" {
			t.Errorf("Recipe.GetVersion() = %q, want empty", got)
		}
	})

	t.Run("GetVersion with metadata", func(t *testing.T) {
		r := &Recipe{}
		r.Metadata = map[string]string{"recipe-version": "v1.0"}
		if got := r.GetVersion(); got != "v1.0" {
			t.Errorf("Recipe.GetVersion() = %q, want v1.0", got)
		}
	})

	t.Run("GetCriteria always nil", func(t *testing.T) {
		r := &Recipe{}
		if got := r.GetCriteria(); got != nil {
			t.Errorf("Recipe.GetCriteria() = %v, want nil", got)
		}
	})
}

func TestRecipeResult_Accessors(t *testing.T) {
	t.Run("GetVersion", func(t *testing.T) {
		rr := &RecipeResult{}
		rr.Metadata.Version = testVersionV2
		if got := rr.GetVersion(); got != testVersionV2 {
			t.Errorf("RecipeResult.GetVersion() = %q, want v2.0", got)
		}
	})

	t.Run("GetCriteria", func(t *testing.T) {
		c := &Criteria{Service: "eks"}
		rr := &RecipeResult{Criteria: c}
		if got := rr.GetCriteria(); got != c {
			t.Errorf("RecipeResult.GetCriteria() != expected criteria")
		}
	})

	t.Run("GetComponentRef found", func(t *testing.T) {
		rr := &RecipeResult{
			ComponentRefs: []ComponentRef{
				{Name: "gpu-operator", Version: "v1.0"},
				{Name: "network-operator", Version: testVersionV2},
			},
		}
		got := rr.GetComponentRef("network-operator")
		if got == nil {
			t.Fatal("expected non-nil component ref")
		}
		if got.Version != testVersionV2 {
			t.Errorf("Version = %q, want v2.0", got.Version)
		}
	})

	t.Run("GetComponentRef not found", func(t *testing.T) {
		rr := &RecipeResult{ComponentRefs: []ComponentRef{{Name: "gpu-operator"}}}
		if got := rr.GetComponentRef("missing"); got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})
}

func Test_hasComponentRefs(t *testing.T) {
	t.Run("RecipeResult returns true", func(t *testing.T) {
		rr := &RecipeResult{}
		if !hasComponentRefs(rr) {
			t.Error("expected true for RecipeResult")
		}
	})

	t.Run("Recipe returns false", func(t *testing.T) {
		r := &Recipe{}
		if hasComponentRefs(r) {
			t.Error("expected false for Recipe")
		}
	})
}

// buildIsolatedCriteria returns a Criteria matching the leaf overlay seeded
// by buildProviderWithValues — keeps the test focused on the bound-provider
// routing and not on criteria matching.
func buildIsolatedCriteria(t *testing.T) *Criteria {
	t.Helper()
	return &Criteria{Service: "eks"}
}

// buildProviderWithValues returns an inMemoryDataProvider whose values for
// the gpu-operator component live only in the bound provider. It seeds:
//   - an empty registry.yaml
//   - a base overlay
//   - a leaf overlay (criteria.service=eks) whose single componentRef points
//     at gpu-operator with valuesFile = components/gpu-operator/values.yaml
//   - that values.yaml at the expected path, containing the supplied content
//
// Together these let BuildFromCriteria succeed against the bound provider and
// let RecipeResult.GetValuesForComponent("gpu-operator") read the seeded
// values exclusively from the bound provider. The valuesPath argument is
// retained for symmetry with buildProviderWithManifest (so callers can
// document the path they expect to be read) but is not load-bearing — the
// component always references components/gpu-operator/values.yaml.
func buildProviderWithValues(t *testing.T, valuesPath string, values map[string]any) DataProvider {
	t.Helper()

	valuesBytes, err := yaml.Marshal(values)
	if err != nil {
		t.Fatalf("marshal values: %v", err)
	}

	registryYAML := []byte(`apiVersion: aicr.run/v1alpha2
kind: ComponentRegistry
components: []
`)
	baseYAML := []byte(`kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: base
spec:
  componentRefs: []
`)
	overlayYAML := []byte(`kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: bound-values
spec:
  base: base
  criteria:
    service: eks
  componentRefs:
    - name: gpu-operator
      type: Helm
      chart: gpu-operator
      source: https://helm.ngc.nvidia.com/nvidia
      version: v25.3.4
      valuesFile: components/gpu-operator/values.yaml
`)

	files := map[string][]byte{
		"registry.yaml":                       registryYAML,
		"overlays/base.yaml":                  baseYAML,
		"overlays/bound-values.yaml":          overlayYAML,
		"components/gpu-operator/values.yaml": valuesBytes,
	}
	// valuesPath only documents the expected lookup; the recipe always
	// references the canonical components/gpu-operator/values.yaml. Sanity-
	// check that the helper isn't being asked for a non-canonical path
	// (which would hide a test mistake silently).
	if valuesPath != "components/gpu-operator/values.yaml" {
		t.Logf("buildProviderWithValues: note - valuesPath %q is documentation only; component reads components/gpu-operator/values.yaml", valuesPath)
	}
	return newInMemoryProvider("bound-values", files)
}

// buildProviderWithManifest returns an inMemoryDataProvider seeded with a
// single manifest file at the supplied path. Used to verify that
// GetManifestContentWithProvider reads from the supplied provider rather
// than the embedded default.
func buildProviderWithManifest(t *testing.T, path string, content []byte) DataProvider {
	t.Helper()
	files := map[string][]byte{
		path: content,
	}
	return newInMemoryProvider(fmt.Sprintf("manifest-%s", path), files)
}

// TestRecipeResult_GetValuesForComponent_HonorsBoundProvider verifies that
// when a RecipeResult was built from a Builder bound via WithDataProvider,
// GetValuesForComponent reads the component's valuesFile from that bound
// provider — not from the embedded default. This closes the
// silent-fallback path flagged in reviewer feedback on Tasks 4/5.
func TestRecipeResult_GetValuesForComponent_HonorsBoundProvider(t *testing.T) {
	t.Cleanup(ResetMetadataStoreForTesting)
	t.Cleanup(ResetComponentRegistryForTesting)

	dp := buildProviderWithValues(t, "components/gpu-operator/values.yaml", map[string]any{
		"driver": map[string]any{"version": "999.99.99"},
	})
	b := NewBuilder(WithDataProvider(dp))
	result, err := b.BuildFromCriteria(context.Background(), buildIsolatedCriteria(t))
	if err != nil {
		t.Fatalf("BuildFromCriteria: %v", err)
	}

	// values.yaml exists in the BOUND provider only; the default embedded
	// values.yaml has different content (no driver.version key at
	// all). If GetValuesForComponent reaches for the embedded default, the type
	// assertion below will fail.
	vals, err := result.GetValuesForComponent("gpu-operator")
	if err != nil {
		t.Fatalf("GetValuesForComponent: %v", err)
	}

	driver, ok := vals["driver"].(map[string]any)
	if !ok {
		t.Fatalf("driver not a map: %#v", vals["driver"])
	}
	if got := driver["version"]; got != "999.99.99" {
		t.Errorf("driver.version = %v, want from bound provider (999.99.99)", got)
	}
}

// TestGetManifestContentWithProvider verifies the explicit-provider variant
// reads from the supplied DataProvider rather than the embedded default.
func TestGetManifestContentWithProvider(t *testing.T) {
	dp := buildProviderWithManifest(t, "components/x/manifests/special.yaml", []byte("from-bound\n"))
	got, err := GetManifestContentWithProvider(dp, "components/x/manifests/special.yaml")
	if err != nil {
		t.Fatalf("GetManifestContentWithProvider: %v", err)
	}
	if string(got) != "from-bound\n" {
		t.Errorf("content = %q, want %q", got, "from-bound\n")
	}
}

// TestGetManifestContentWithProvider_NilFallback verifies that passing a nil
// DataProvider falls back to the embedded catalog (defaultEmbeddedProvider) —
// preserving back-compat for callers that don't have a RecipeResult-bound
// provider.
func TestGetManifestContentWithProvider_NilFallback(t *testing.T) {
	content, err := GetManifestContentWithProvider(nil, "components/network-operator/manifests/nfd-network-rule.yaml")
	if err != nil {
		t.Fatalf("GetManifestContentWithProvider(nil): %v", err)
	}
	if len(content) == 0 {
		t.Error("expected non-empty content from global fallback")
	}
}

// TestGetManifestContentWithProvider_NotFound verifies that missing manifest
// files surface as a structured pkg/errors error with ErrCodeNotFound while
// preserving the underlying fs.ErrNotExist in the wrap chain — bundler
// callers depend on stderrors.Is(err, fs.ErrNotExist) for distinguishing
// missing-file errors from internal read failures.
func TestGetManifestContentWithProvider_NotFound(t *testing.T) {
	dp := newInMemoryProvider("empty", map[string][]byte{})
	_, err := GetManifestContentWithProvider(dp, "components/missing/manifests/x.yaml")
	if err == nil {
		t.Fatal("expected error for missing manifest, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeNotFound, "")) {
		t.Errorf("expected ErrCodeNotFound, got %v", err)
	}
	if !stderrors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected wrap chain to preserve fs.ErrNotExist, got %v", err)
	}
}

// TestWithResolvedValues covers the read-once view that lets one operation
// pin a component's effective values so a gate and the emitted artifact
// cannot observe different results from a mutable DataProvider (#1873 A).
func TestWithResolvedValues(t *testing.T) {
	t.Parallel()

	newResult := func() *RecipeResult {
		return &RecipeResult{
			Kind:       "RecipeResult",
			APIVersion: RecipeResultAPIVersion,
			ComponentRefs: []ComponentRef{
				{Name: "pinned", Type: ComponentTypeHelm, Overrides: map[string]any{"from": "provider"}},
				{Name: "unpinned", Type: ComponentTypeHelm, Overrides: map[string]any{"from": "provider"}},
			},
		}
	}

	t.Run("nil receiver returns nil", func(t *testing.T) {
		t.Parallel()
		var r *RecipeResult
		if got := r.WithResolvedValues(map[string]map[string]any{"x": {}}); got != nil {
			t.Errorf("WithResolvedValues() on nil receiver = %v, want nil", got)
		}
	})

	t.Run("empty snapshot returns receiver unchanged", func(t *testing.T) {
		t.Parallel()
		r := newResult()
		if got := r.WithResolvedValues(nil); got != r {
			t.Error("WithResolvedValues(nil) returned a copy, want the receiver unchanged")
		}
	})

	t.Run("pinned component bypasses the provider, unpinned does not", func(t *testing.T) {
		t.Parallel()
		r := newResult()
		pinned := r.WithResolvedValues(map[string]map[string]any{
			"pinned": {"from": "snapshot", "nested": map[string]any{"k": "v"}},
		})

		got, err := pinned.GetValuesForComponentWithContext(context.Background(), "pinned")
		if err != nil {
			t.Fatalf("GetValuesForComponentWithContext(pinned): %v", err)
		}
		if got["from"] != "snapshot" {
			t.Errorf("pinned component values = %v, want the snapshot value", got)
		}

		got, err = pinned.GetValuesForComponentWithContext(context.Background(), "unpinned")
		if err != nil {
			t.Fatalf("GetValuesForComponentWithContext(unpinned): %v", err)
		}
		if got["from"] != "provider" {
			t.Errorf("unpinned component values = %v, want the provider-resolved value", got)
		}

		// The receiver is untouched: WithResolvedValues returns a view.
		got, err = r.GetValuesForComponentWithContext(context.Background(), "pinned")
		if err != nil {
			t.Fatalf("GetValuesForComponentWithContext(receiver): %v", err)
		}
		if got["from"] != "provider" {
			t.Errorf("receiver values = %v, want the provider-resolved value", got)
		}
	})

	t.Run("callers receive deep copies", func(t *testing.T) {
		t.Parallel()
		snapshot := map[string]map[string]any{
			"pinned": {"nested": map[string]any{"k": "original"}, "list": []any{"a"}},
		}
		pinned := newResult().WithResolvedValues(snapshot)

		first, err := pinned.GetValuesForComponentWithContext(context.Background(), "pinned")
		if err != nil {
			t.Fatalf("GetValuesForComponentWithContext: %v", err)
		}
		// Mutate exactly as the coherence gate does when it layers --set
		// overrides onto the values it resolved.
		first["nested"].(map[string]any)["k"] = "mutated"
		first["list"].([]any)[0] = "mutated"

		second, err := pinned.GetValuesForComponentWithContext(context.Background(), "pinned")
		if err != nil {
			t.Fatalf("GetValuesForComponentWithContext (second): %v", err)
		}
		if got := second["nested"].(map[string]any)["k"]; got != "original" {
			t.Errorf("nested map leaked a caller mutation: got %v, want %q", got, "original")
		}
		if got := second["list"].([]any)[0]; got != "a" {
			t.Errorf("slice leaked a caller mutation: got %v, want %q", got, "a")
		}
	})

	t.Run("canceled context is honored", func(t *testing.T) {
		t.Parallel()
		pinned := newResult().WithResolvedValues(map[string]map[string]any{"pinned": {"from": "snapshot"}})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := pinned.GetValuesForComponentWithContext(ctx, "pinned"); err == nil {
			t.Error("GetValuesForComponentWithContext(canceled) = nil error, want cancellation")
		}
	})

	t.Run("DeepCopy drops the pin", func(t *testing.T) {
		t.Parallel()
		pinned := newResult().WithResolvedValues(map[string]map[string]any{"pinned": {"from": "snapshot"}})
		got, err := pinned.DeepCopy().GetValuesForComponentWithContext(context.Background(), "pinned")
		if err != nil {
			t.Fatalf("GetValuesForComponentWithContext on DeepCopy: %v", err)
		}
		if got["from"] != "provider" {
			t.Errorf("DeepCopy values = %v, want the provider-resolved value (the pin scopes one operation)", got)
		}
	})
}

// TestWithDeclaredComponents pins the explicit-presence contract: the
// attachment bit is HasDeclaredComponents, never a slice-length
// inference — a union that happens to have the same length as the
// filtered refs is still an attached union.
func TestWithDeclaredComponents(t *testing.T) {
	t.Parallel()

	base := &RecipeResult{ComponentRefs: []ComponentRef{{Name: "a"}}}
	if base.HasDeclaredComponents() {
		t.Error("HasDeclaredComponents() = true on a result without an attached union")
	}
	if got := base.DeclaredComponentRefs(); len(got) != 1 || got[0].Name != "a" {
		t.Errorf("DeclaredComponentRefs() fallback = %v, want ComponentRefs", got)
	}

	union := []ComponentRef{{Name: "b"}} // same length as ComponentRefs — the inference trap
	view := base.WithDeclaredComponents(union)
	if !view.HasDeclaredComponents() {
		t.Error("HasDeclaredComponents() = false after WithDeclaredComponents")
	}
	if got := view.DeclaredComponentRefs(); len(got) != 1 || got[0].Name != "b" {
		t.Errorf("DeclaredComponentRefs() = %v, want the attached union", got)
	}
	if base.HasDeclaredComponents() {
		t.Error("WithDeclaredComponents mutated the receiver")
	}

	var nilResult *RecipeResult
	if nilResult.HasDeclaredComponents() {
		t.Error("HasDeclaredComponents() on nil = true")
	}
	if nilResult.WithDeclaredComponents(union) != nil {
		t.Error("WithDeclaredComponents on nil receiver must return nil")
	}
	if base.WithDeclaredComponents(nil) != base {
		t.Error("WithDeclaredComponents(nil) must return the receiver unchanged")
	}
}
