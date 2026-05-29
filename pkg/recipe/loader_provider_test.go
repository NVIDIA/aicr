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
	"os"
	"path/filepath"
	"testing"
)

// newTestLayeredProvider builds a LayeredDataProvider over a temp dir holding a
// minimal external registry.yaml, layered on top of the embedded data so that
// embedded base overlays remain resolvable.
func newTestLayeredProvider(t *testing.T) *LayeredDataProvider {
	t.Helper()
	tmp := t.TempDir()
	registry := `apiVersion: aicr.nvidia.com/v1alpha1
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
	overlay := `apiVersion: aicr.nvidia.com/v1alpha1
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
		content := "kind: RecipeResult\napiVersion: aicr.nvidia.com/v1alpha1\ncriteria:\n  service: eks\n"
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

	t.Run("nil provider behaves like LoadFromFile", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "recipe.yaml")
		content := "kind: RecipeResult\napiVersion: aicr.nvidia.com/v1alpha1\ncriteria:\n  service: eks\n"
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
}
