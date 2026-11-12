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

package bundler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/artifact"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/recipe/providertest"
	"github.com/NVIDIA/aicr/recipes"
)

func TestArtifactBundleCaptureRetainsOriginalInputs(t *testing.T) {
	original := []byte("message: original\n")
	dir := providertest.WriteDir(t, map[string][]byte{
		"registry.yaml":                  []byte(providertest.EmptyRegistryYAML),
		"components/example/values.yaml": original,
		"artifacts/bindings.yaml":        []byte("apiVersion: aicr.run/v1beta1\nkind: ArtifactBindings\ncharts: {}\nimages: {}\nignoredArtifacts:\n  notes: {reason: release notes}\n"),
		"artifacts/inventory.yaml":       []byte("apiVersion: aicr.run/v1beta1\nkind: ArtifactInventory\nartifacts:\n- name: notes\n  type: other\n  reference: oci://example.test/notes\n  version: v1\n  manifestDigest: sha256:" + strings.Repeat("a", 64) + "\n"),
	})
	layered, err := recipe.NewLayeredDataProvider(recipe.NewEmbeddedDataProvider(recipes.FS, ""), recipe.LayeredProviderConfig{ExternalDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	captured, err := recipe.ComposeArtifacts(t.Context(), layered, artifact.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if writeErr := os.WriteFile(filepath.Join(dir, "components/example/values.yaml"), []byte("message: changed\n"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	dest := t.TempDir()
	b := &DefaultBundler{}
	files, err := b.copyDataFiles(t.Context(), dest, captured)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("captured files: %v", files)
	}
	got, err := os.ReadFile(filepath.Join(dest, "data/components/example/values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("bundle re-read mutable input: %s", got)
	}
	replay, err := recipe.NewLayeredDataProvider(recipe.NewEmbeddedDataProvider(recipes.FS, ""), recipe.LayeredProviderConfig{ExternalDir: filepath.Join(dest, "data")})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := recipe.ComposeArtifacts(t.Context(), replay, artifact.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := recipe.CheckArtifactInputs(captured.(*recipe.ArtifactProvider).ArtifactProvenance(), rebuilt); err != nil {
		t.Fatalf("captured inputs cannot replay: %v", err)
	}
}
