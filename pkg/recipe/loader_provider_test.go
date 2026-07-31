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
	stderrors "errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// newTestLayeredProvider builds a LayeredDataProvider over a temp dir holding a
// minimal external registry.yaml, layered on top of the embedded data so that
// embedded base overlays remain resolvable.
func newTestLayeredProvider(t *testing.T) *LayeredDataProvider {
	t.Helper()
	tmp := t.TempDir()
	registry := `apiVersion: aicr.run/v1alpha2
kind: ComponentRegistry
components: []
`
	if err := os.WriteFile(filepath.Join(tmp, "registry.yaml"), []byte(registry), 0o600); err != nil {
		t.Fatalf("write registry.yaml: %v", err)
	}
	layered, err := NewLayeredDataProvider(
		NewEmbeddedDataProvider(GetEmbeddedFS(), "."),
		LayeredProviderConfig{ExternalDir: tmp},
	)
	if err != nil {
		t.Fatalf("NewLayeredDataProvider: %v", err)
	}
	return layered
}

// writeOverlayFile writes a leaf RecipeMetadata overlay (with criteria) to a
// temp path and returns the file path.
func writeOverlayFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "overlay.yaml")
	overlay := `apiVersion: aicr.run/v1alpha2
kind: RecipeMetadata
metadata:
  name: provider-bound-overlay
spec:
  criteria:
    service: eks
    accelerator: h100
    intent: training
`
	if err := os.WriteFile(path, []byte(overlay), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}
	return path
}

func TestLoadFromFileWithProvider(t *testing.T) {
	t.Setenv(strictModeEnvVar, "")

	t.Run("overlay hydrates and binds provider", func(t *testing.T) {
		layered := newTestLayeredProvider(t)
		overlayPath := writeOverlayFile(t)

		rec, err := LoadFromFileWithProvider(t.Context(), overlayPath, "", "vtest", layered)
		if err != nil {
			t.Fatalf("LoadFromFileWithProvider() error: %v", err)
		}
		if rec == nil {
			t.Fatal("expected non-nil result")
		}
		if rec.Kind != RecipeResultKind {
			t.Errorf("kind = %q, want %q", rec.Kind, RecipeResultKind)
		}
		if len(rec.ComponentRefs) == 0 {
			t.Error("expected hydrated recipe with component refs")
		}
		if rec.DataProvider() != layered {
			t.Errorf("DataProvider() = %v, want bound layered provider", rec.DataProvider())
		}
	})

	t.Run("already-hydrated RecipeResult binds provider", func(t *testing.T) {
		layered := newTestLayeredProvider(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "recipe.yaml")
		content := "kind: RecipeResult\napiVersion: aicr.run/v1alpha2\ncriteria:\n  service: eks\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write recipe: %v", err)
		}

		rec, err := LoadFromFileWithProvider(t.Context(), path, "", "vtest", layered)
		if err != nil {
			t.Fatalf("LoadFromFileWithProvider() error: %v", err)
		}
		if rec.DataProvider() != layered {
			t.Errorf("DataProvider() = %v, want bound layered provider", rec.DataProvider())
		}
	})

	t.Run("profile overlay in active catalog applies its default", func(t *testing.T) {
		externalDir := t.TempDir()
		registry := `apiVersion: aicr.run/v1alpha2
kind: ComponentRegistry
components: []
`
		if err := os.WriteFile(filepath.Join(externalDir, "registry.yaml"), []byte(registry), 0o600); err != nil {
			t.Fatalf("write registry.yaml: %v", err)
		}
		overlaysDir := filepath.Join(externalDir, "overlays")
		if err := os.MkdirAll(overlaysDir, 0o755); err != nil {
			t.Fatalf("create overlays directory: %v", err)
		}
		path := filepath.Join(overlaysDir, "direct-profile.yaml")
		content := `kind: RecipeMetadata
apiVersion: aicr.run/v1alpha3
metadata:
  name: direct-profile
spec:
  criteria:
    service: oke
    accelerator: h100
    intent: training
  profile:
    name: gpuStack
    default: operator-managed
    values:
      operator-managed:
        componentRefs:
          - name: gpu-operator
            overrides:
              driver:
                enabled: true
              replicas: 1
`
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write profile overlay: %v", err)
		}
		alternate := `kind: RecipeMetadata
apiVersion: aicr.run/v1alpha3
metadata:
  name: alternate-profile
spec:
  criteria:
    service: eks
    accelerator: h100
    intent: training
  profile:
    name: gpuStack
    default: operator-managed
    values:
      operator-managed:
        componentRefs:
          - name: gpu-operator
            overrides:
              driver:
                enabled: false
              replicas: 2
`
		if err := os.WriteFile(
			filepath.Join(overlaysDir, "alternate-profile.yaml"),
			[]byte(alternate),
			0o600,
		); err != nil {
			t.Fatalf("write alternate profile overlay: %v", err)
		}
		layered, err := NewLayeredDataProvider(
			NewEmbeddedDataProvider(GetEmbeddedFS(), "."),
			LayeredProviderConfig{ExternalDir: externalDir},
		)
		if err != nil {
			t.Fatalf("NewLayeredDataProvider: %v", err)
		}
		t.Cleanup(func() {
			EvictCachedStore(layered)
			EvictCachedRegistry(layered)
			EvictCachedCriteriaRegistry(layered)
		})

		rec, err := LoadFromFileWithProvider(t.Context(), path, "", "vtest", layered)
		if err != nil {
			t.Fatalf("LoadFromFileWithProvider() error: %v", err)
		}
		if rec.Metadata.SelectedProfile == nil {
			t.Fatal("SelectedProfile = nil, want applied default")
		}
		if got := rec.Metadata.SelectedProfile.Name + "=" + rec.Metadata.SelectedProfile.Value; got != "gpuStack=operator-managed" {
			t.Errorf("SelectedProfile = %q, want %q", got, "gpuStack=operator-managed")
		}

		renamedPath := filepath.Join(t.TempDir(), "renamed-profile.yaml")
		renamed := strings.Replace(content, "name: direct-profile", "name: renamed-profile", 1)
		if err := os.WriteFile(renamedPath, []byte(renamed), 0o600); err != nil {
			t.Fatalf("write renamed profile overlay: %v", err)
		}
		if _, err := LoadFromFileWithProvider(t.Context(), renamedPath, "", "vtest", layered); err != nil {
			t.Fatalf("renamed equivalent overlay error: %v", err)
		}

		stalePath := filepath.Join(t.TempDir(), "stale-profile.yaml")
		stale := strings.Replace(content, "service: oke", "service: eks", 1)
		if err := os.WriteFile(stalePath, []byte(stale), 0o600); err != nil {
			t.Fatalf("write stale profile overlay: %v", err)
		}
		if _, err := LoadFromFileWithProvider(t.Context(), stalePath, "", "vtest", layered); err == nil ||
			!strings.Contains(err.Error(), "was not applied") {

			t.Fatalf("stale overlay error = %v, want declaration mismatch", err)
		}
	})

	t.Run("nil provider behaves like LoadFromFile", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "recipe.yaml")
		content := "kind: RecipeResult\napiVersion: aicr.run/v1alpha2\ncriteria:\n  service: eks\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write recipe: %v", err)
		}

		rec, err := LoadFromFileWithProvider(t.Context(), path, "", "vtest", nil)
		if err != nil {
			t.Fatalf("LoadFromFileWithProvider() error: %v", err)
		}
		if rec == nil {
			t.Fatal("expected non-nil result")
		}
		// Nil dp must not bind a provider, matching LoadFromFile.
		if rec.DataProvider() != nil {
			t.Errorf("DataProvider() = %v, want nil for nil-dp path", rec.DataProvider())
		}
	})

	t.Run("hand-authored duplicate componentRef names are rejected", func(t *testing.T) {
		// Exercises the most-used boundary — a hand-authored, already-
		// hydrated RecipeResult file loaded via LoadFromFileWithProvider,
		// which is the shape `aicr bundle -r`/`aicr validate -r` and
		// POST /v1/bundle load. See #1874.
		layered := newTestLayeredProvider(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "recipe.yaml")
		content := "kind: RecipeResult\n" +
			"apiVersion: aicr.run/v1alpha2\n" +
			"criteria:\n  service: eks\n" +
			"componentRefs:\n" +
			"  - name: gpu-operator\n" +
			"    type: Helm\n" +
			"    source: https://charts.example.com\n" +
			"    version: \"1.0.0\"\n" +
			"  - name: gpu-operator\n" +
			"    type: Helm\n" +
			"    source: https://charts.example.com\n" +
			"    version: \"1.0.0\"\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write recipe: %v", err)
		}

		_, err := LoadFromFileWithProvider(t.Context(), path, "", "vtest", layered)
		if err == nil {
			t.Fatal("expected an error for duplicate componentRef names, got nil")
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Errorf("expected ErrCodeInvalidRequest, got: %v", err)
		}
		if !strings.Contains(err.Error(), "duplicate") {
			t.Errorf("error should mention the duplicate, got: %v", err)
		}
	})
}

func TestLoadFromFileWithProviderProfile(t *testing.T) {
	t.Setenv(strictModeEnvVar, "")

	// External catalog with a two-value profile declaration on the leaf
	// overlay, so an explicit non-default selection is observable.
	newProfileCatalog := func(t *testing.T) (*LayeredDataProvider, string) {
		t.Helper()
		externalDir := t.TempDir()
		registry := `apiVersion: aicr.run/v1alpha2
kind: ComponentRegistry
components: []
`
		if err := os.WriteFile(filepath.Join(externalDir, "registry.yaml"), []byte(registry), 0o600); err != nil {
			t.Fatalf("write registry.yaml: %v", err)
		}
		overlaysDir := filepath.Join(externalDir, "overlays")
		if err := os.MkdirAll(overlaysDir, 0o755); err != nil {
			t.Fatalf("create overlays directory: %v", err)
		}
		overlay := `kind: RecipeMetadata
apiVersion: aicr.run/v1alpha3
metadata:
  name: two-value-profile
spec:
  criteria:
    service: oke
    accelerator: h100
    intent: training
  profile:
    name: gpuStack
    default: azure-managed
    values:
      azure-managed:
        componentRefs:
          - name: gpu-operator
            overrides:
              driver:
                enabled: false
      operator-managed:
        componentRefs:
          - name: gpu-operator
            overrides:
              driver:
                enabled: true
`
		path := filepath.Join(overlaysDir, "two-value-profile.yaml")
		if err := os.WriteFile(path, []byte(overlay), 0o600); err != nil {
			t.Fatalf("write profile overlay: %v", err)
		}
		layered, err := NewLayeredDataProvider(
			NewEmbeddedDataProvider(GetEmbeddedFS(), "."),
			LayeredProviderConfig{ExternalDir: externalDir},
		)
		if err != nil {
			t.Fatalf("NewLayeredDataProvider: %v", err)
		}
		t.Cleanup(func() {
			EvictCachedStore(layered)
			EvictCachedRegistry(layered)
			EvictCachedCriteriaRegistry(layered)
		})
		return layered, path
	}

	t.Run("explicit non-default selection is applied", func(t *testing.T) {
		layered, path := newProfileCatalog(t)
		rec, err := LoadFromFileWithProviderProfile(t.Context(), path, "", "vtest", layered, "gpuStack=operator-managed")
		if err != nil {
			t.Fatalf("LoadFromFileWithProviderProfile() error: %v", err)
		}
		if rec.Metadata.SelectedProfile == nil {
			t.Fatal("SelectedProfile = nil, want explicit selection applied")
		}
		if got := rec.Metadata.SelectedProfile.Name + "=" + rec.Metadata.SelectedProfile.Value; got != "gpuStack=operator-managed" {
			t.Errorf("SelectedProfile = %q, want %q", got, "gpuStack=operator-managed")
		}
	})

	t.Run("empty selection keeps declaration default", func(t *testing.T) {
		layered, path := newProfileCatalog(t)
		rec, err := LoadFromFileWithProviderProfile(t.Context(), path, "", "vtest", layered, "")
		if err != nil {
			t.Fatalf("LoadFromFileWithProviderProfile() error: %v", err)
		}
		if rec.Metadata.SelectedProfile == nil || rec.Metadata.SelectedProfile.Value != "azure-managed" {
			t.Errorf("SelectedProfile = %+v, want default value azure-managed", rec.Metadata.SelectedProfile)
		}
	})

	t.Run("malformed selection is rejected before I/O", func(t *testing.T) {
		layered, path := newProfileCatalog(t)
		_, err := LoadFromFileWithProviderProfile(t.Context(), path, "", "vtest", layered, "not-a-selection")
		if err == nil || !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Fatalf("error = %v, want ErrCodeInvalidRequest for malformed selection", err)
		}
	})

	t.Run("hydrated RecipeResult input rejects an explicit selection", func(t *testing.T) {
		layered := newTestLayeredProvider(t)
		dir := t.TempDir()
		path := filepath.Join(dir, "recipe.yaml")
		content := "kind: RecipeResult\napiVersion: aicr.run/v1alpha2\ncriteria:\n  service: eks\n"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write recipe: %v", err)
		}
		_, err := LoadFromFileWithProviderProfile(t.Context(), path, "", "vtest", layered, "gpuStack=operator-managed")
		if err == nil || !strings.Contains(err.Error(), "already baked") {
			t.Fatalf("error = %v, want baked-in selection rejection", err)
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
			t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
		}
	})
}
