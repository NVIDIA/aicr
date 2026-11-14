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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"

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
	b := &bundleRun{DefaultBundler: &DefaultBundler{}}
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

func TestArtifactBundleReusePreservesInputsOnFailure(t *testing.T) {
	bindings := []byte("apiVersion: aicr.run/v1beta1\nkind: ArtifactBindings\ncharts: {}\nimages: {}\nignoredArtifacts:\n  notes: {reason: release notes}\n")
	inventory := []byte("apiVersion: aicr.run/v1beta1\nkind: ArtifactInventory\nartifacts:\n- name: notes\n  type: other\n  reference: oci://example.test/notes\n  version: v2\n  manifestDigest: sha256:" + strings.Repeat("a", 64) + "\n")
	dir := providertest.WriteDir(t, map[string][]byte{
		"registry.yaml":            []byte(providertest.EmptyRegistryYAML),
		"artifacts/bindings.yaml":  bindings,
		"artifacts/inventory.yaml": inventory,
	})
	layered, err := recipe.NewLayeredDataProvider(recipe.NewEmbeddedDataProvider(recipes.FS, ""), recipe.LayeredProviderConfig{ExternalDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	captured, err := recipe.ComposeArtifacts(t.Context(), layered, artifact.Options{AllowCatalogPins: []string{"gpu-operator"}})
	if err != nil {
		t.Fatal(err)
	}
	rr := &recipe.RecipeResult{
		APIVersion: "aicr.run/v1beta2", Kind: "Recipe",
		ComponentRefs:   []recipe.ComponentRef{{Name: "gpu-operator", Version: "v25.3.3", Type: "helm", Source: "https://helm.ngc.nvidia.com/nvidia"}},
		DeploymentOrder: []string{"gpu-operator"},
	}
	rr.Metadata.Artifacts = captured.(*recipe.ArtifactProvider).ArtifactProvenance()
	rr.BindDataProvider(captured)
	b, err := New()
	if err != nil {
		t.Fatal(err)
	}
	for _, blockDeployer := range []bool{false, true} {
		t.Run(fmt.Sprint("deployerFailure=", blockDeployer), func(t *testing.T) {
			dest := t.TempDir()
			old := map[string][]byte{
				"data/artifacts/inventory.yaml": []byte("old inventory\n"),
				"data/artifacts/bindings.yaml":  []byte("old bindings\n"),
				"checksums.txt":                 []byte("old checksums\n"),
			}
			for name, data := range old {
				path := filepath.Join(dest, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// A directory the bundle does not own makes the rerun fail
			// before it publishes.
			if blockDeployer {
				if err := os.Mkdir(filepath.Join(dest, "deploy.sh"), 0o750); err != nil {
					t.Fatal(err)
				}
			}
			_, err := b.Make(t.Context(), rr, dest)
			want := old
			switch {
			case blockDeployer && err == nil:
				t.Fatal("want failure with a stray deploy.sh directory, got nil")
			case !blockDeployer && err != nil:
				t.Fatalf("rerun over an earlier bundle: %v", err)
			case !blockDeployer:
				want = map[string][]byte{
					"data/artifacts/inventory.yaml": inventory,
					"data/artifacts/bindings.yaml":  bindings,
				}
			}
			for name, w := range want {
				got, err := os.ReadFile(filepath.Join(dest, name))
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != string(w) {
					t.Errorf("%s = %q, want %q", name, got, w)
				}
			}
		})
	}
}

func TestArtifactChartVersionHelmHandoffs(t *testing.T) {
	for _, tt := range allDeployerCases() {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New(WithConfig(tt.configFor(config.WithRepoURL("https://github.com/example/bundles.git"))))
			if err != nil {
				t.Fatal(err)
			}
			rr := closedWorldRecipeResult()
			rr.ComponentRefs[0].Source = "oci://registry.example/charts"
			rr.ComponentRefs[0].Version = "v25.3.3_build.1"
			dir := t.TempDir()
			if _, makeErr := b.Make(t.Context(), rr, dir); makeErr != nil {
				t.Fatal(makeErr)
			}
			found := false
			err = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if entry.IsDir() {
					return nil
				}
				name := entry.Name()
				eligible := name == "upstream.env"
				switch tt.name {
				case "helmfile":
					eligible = name == "helmfile.yaml"
				case "argocd", "argocd-helm", "flux":
					eligible = strings.HasSuffix(name, ".yaml") && name != RecipeFileName
				case "fleet-gitrepo", "fleet-helmop":
					eligible = strings.HasSuffix(name, ".yaml") && name != RecipeFileName
				case "terraform":
					eligible = strings.HasSuffix(name, ".tf")
				}
				if eligible {
					data, readErr := os.ReadFile(path)
					if readErr != nil {
						return readErr
					}
					found = found || strings.Contains(string(data), "v25.3.3+build.1")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !found {
				t.Fatal("Helm handoff did not decode OCI build metadata")
			}
			data, err := os.ReadFile(filepath.Join(dir, RecipeFileName))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "v25.3.3_build.1") {
				t.Fatal("recipe lost literal OCI tag")
			}
		})
	}
}
