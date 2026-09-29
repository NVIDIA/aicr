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
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// withPatch returns testdata/profile-patch/<fixture> with the YAML merge patch
// applied on top. Maps merge recursively and any other value replaces the
// fixture's.
func withPatch(t *testing.T, fixture, patch string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "profile-patch", fixture))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var doc, overlay map[string]any
	if err = yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse fixture %s: %v", fixture, err)
	}
	if err = yaml.Unmarshal([]byte(patch), &overlay); err != nil {
		t.Fatalf("parse patch %q: %v", patch, err)
	}
	deepMergeMap(doc, overlay)
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("serialize patched %s: %v", fixture, err)
	}
	return string(out)
}

func TestPatchOverlayProfile(t *testing.T) {
	embedded, err := os.ReadFile(filepath.Join("testdata", "profile-patch", "embedded.yaml"))
	if err != nil {
		t.Fatalf("read embedded fixture: %v", err)
	}
	const addZ = "spec: {profile: {values: {z: {componentRefs: [{name: c, overrides: {x: 9}}]}}}}"

	tests := []struct {
		name        string
		patch       string
		appended    string
		wantPatched bool
		wantErr     string
		wantX       map[string]int
	}{
		{
			name:        "adds values and keeps the embedded overlay and profile settings",
			patch:       addZ,
			wantPatched: true,
			wantX:       map[string]int{"a": 1, "b": 2, "z": 9},
		},
		{
			name:  "external file with criteria is a replacement, not a patch",
			patch: "spec: {criteria: {service: gke}, profile: {default: z, values: {z: {}}}}",
		},
		{
			name:  "explicitly empty non-profile field is a replacement, not a patch",
			patch: "spec: {base: '', profile: {values: {z: {}}}}",
		},
		{
			name:     "unparsable external is left for the loader",
			patch:    addZ,
			appended: "bogus: [unclosed\n",
		},
		{
			name:  "unknown key is left for the loader",
			patch: "spec: {profile: {bogus: 1, values: {z: {}}}}",
		},
		{
			name:  "legacy apiVersion is left for the loader",
			patch: "{apiVersion: aicr.run/v1beta1, spec: {profile: {values: {z: {}}}}}",
		},
		{
			name:     "several documents are left for the loader",
			patch:    addZ,
			appended: "---\nspec: {}\n",
		},
		{
			name:    "redeclaring an embedded value is rejected",
			patch:   "spec: {profile: {values: {b: {componentRefs: [{name: c, overrides: {x: 7}}]}}}}",
			wantErr: "already declares [b]",
		},
		{
			name:    "setting the default is rejected",
			patch:   "spec: {profile: {default: z, values: {z: {}}}}",
			wantErr: "not set default or description",
		},
		{
			name:    "a different profile name is rejected",
			patch:   "spec: {profile: {name: other, values: {z: {}}}}",
			wantErr: `patches profile "other"`,
		},
		{
			name:    "a patch with no values is rejected",
			patch:   "{}",
			wantErr: "adds no values",
		},
		{
			name:    "a different overlay name is rejected",
			patch:   "{metadata: {name: other}, spec: {profile: {values: {z: {}}}}}",
			wantErr: `is named "other"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			external := withPatch(t, "patch.yaml", tt.patch) + tt.appended
			got, patched, err := patchOverlayProfile("overlays/fam.yaml", embedded, []byte(external))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if patched != tt.wantPatched {
				t.Fatalf("patched = %v, want %v", patched, tt.wantPatched)
			}
			if !patched {
				if string(got) != external {
					t.Fatalf("expected external unchanged, got:\n%s", got)
				}
				return
			}

			var merged RecipeMetadata
			if err := yaml.Unmarshal(got, &merged); err != nil {
				t.Fatalf("merged output does not parse: %v\n%s", err, got)
			}
			if merged.Spec.Criteria == nil || merged.Spec.Criteria.Service != CriteriaServiceGKE {
				t.Fatalf("merged criteria = %#v, want the embedded criteria", merged.Spec.Criteria)
			}
			profile := merged.Spec.Profile
			if profile.Default != "a" || profile.Description != "embedded description" {
				t.Fatalf("default/description = %q/%q, want the embedded ones", profile.Default, profile.Description)
			}
			gotX := make(map[string]int, len(profile.Values))
			for name, value := range profile.Values {
				gotX[name], _ = value.ComponentRefs[0].Overrides["x"].(int)
			}
			if !maps.Equal(gotX, tt.wantX) {
				t.Fatalf("value x overrides = %v, want %v", gotX, tt.wantX)
			}
		})
	}
}

// TestExternalOverlayContributesProfileValue drives the layered provider end
// to end with a profile-only gke-cos.yaml, the only file the catalog carries
// for that overlay.
func TestExternalOverlayContributesProfileValue(t *testing.T) {
	ctx := t.Context()

	tests := []struct {
		name         string
		patch        string
		wantLoadErr  string
		wantSelected map[string]string
	}{
		{
			name: "value with the required path set loads beside the embedded values",
			patch: `spec: {profile: {values: {custom: {advertiser: external, componentRefs: [
  {name: gcp-driver-installer, overrides: {installer: {enabled: false}}},
  {name: gpu-operator, overrides: {devicePlugin: {enabled: false}}},
  {name: nvsentinel, overrides: {labeler: {assumeDriverInstalled: true}}}]}}}}`,
			wantSelected: map[string]string{
				"":                          "gke-default",
				"gpuStack=custom":           "custom",
				"gpuStack=bundle-installer": "bundle-installer",
			},
		},
		{
			name: "value assigning only a DRA path fails union totality at catalog load",
			patch: `spec: {profile: {values: {dra: {componentRefs: [{name: nvidia-dra-driver-gpu,
  overrides: {gpuResourcesEnabledOverride: true, resources: {gpus: {enabled: true}}}}]}}}}`,
			wantLoadErr: "union totality",
		},
		{
			name:        "value redeclaring an embedded one fails at read",
			patch:       "spec: {profile: {values: {bundle-installer: {componentRefs: []}}}}",
			wantLoadErr: "already declares [bundle-installer]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			registry := "apiVersion: aicr.run/v1beta1\nkind: ComponentRegistry\ncomponents: []\n"
			if err := os.WriteFile(filepath.Join(dir, "registry.yaml"), []byte(registry), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(dir, "overlays"), 0o750); err != nil {
				t.Fatal(err)
			}
			patch := withPatch(t, "gke-cos.yaml", tt.patch)
			if err := os.WriteFile(filepath.Join(dir, "overlays", "gke-cos.yaml"), []byte(patch), 0o600); err != nil {
				t.Fatal(err)
			}
			layered, err := NewLayeredDataProvider(
				NewEmbeddedDataProvider(GetEmbeddedFS(), "."), LayeredProviderConfig{ExternalDir: dir})
			if err != nil {
				t.Fatalf("NewLayeredDataProvider() error = %v", err)
			}

			store, err := buildMetadataStore(ctx, layered)
			if tt.wantLoadErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantLoadErr) {
					t.Fatalf("buildMetadataStore() error = %v, want %q", err, tt.wantLoadErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("buildMetadataStore() error = %v", err)
			}
			if got := layered.Source("overlays/gke-cos.yaml"); got != CatalogSourceEmbedded {
				t.Fatalf("Source(overlays/gke-cos.yaml) = %q, want %q", got, CatalogSourceEmbedded)
			}
			for selection, want := range tt.wantSelected {
				result, buildErr := store.BuildRecipeResultWithProfile(ctx, gkeCriteria(), selection)
				if buildErr != nil {
					t.Fatalf("selection %q: %v", selection, buildErr)
				}
				if got := result.Metadata.SelectedProfile.Value; got != want {
					t.Fatalf("selection %q resolved to %q, want %q", selection, got, want)
				}
			}
		})
	}
}
