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

package helm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// digestStubPuller returns the same archive bytes for every pull.
type digestStubPuller struct{}

var digestStubArchive = []byte("digest-stub-archive")

func (digestStubPuller) Pull(_ context.Context, c localformat.Component) ([]byte, localformat.VendorRecord, string, error) {
	tarball := c.ChartName + "-" + c.Version + ".tgz"
	return digestStubArchive, localformat.VendorRecord{
		Name: c.Name, Chart: c.ChartName, Version: c.Version, Repository: c.Repository,
		SHA256: "not-consulted", TarballName: tarball,
	}, tarball, nil
}

// digestPinnedRecipe returns a recipe for one registry-pinned chart, bound
// to a registry that pins digest for it.
func digestPinnedRecipe(t *testing.T, digest string) *recipe.RecipeResult {
	t.Helper()
	dataDir := t.TempDir()
	registryYAML := `apiVersion: aicr.run/v1beta1
kind: ComponentRegistry
components:
  - name: digested
    displayName: Digested
    helm:
      defaultRepository: https://charts.example.com
      defaultChart: example/digested
      defaultVersion: 1.2.3
      defaultDigest: ` + digest + "\n"
	if err := os.WriteFile(filepath.Join(dataDir, "registry.yaml"), []byte(registryYAML), 0o600); err != nil {
		t.Fatalf("WriteFile registry.yaml: %v", err)
	}
	layered, err := recipe.NewLayeredDataProvider(
		recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), ""),
		recipe.LayeredProviderConfig{ExternalDir: dataDir})
	if err != nil {
		t.Fatalf("NewLayeredDataProvider: %v", err)
	}
	t.Cleanup(func() { recipe.EvictCachedRegistry(layered) })
	rr := &recipe.RecipeResult{}
	rr.Metadata.Version = "v1.0.0"
	rr.ComponentRefs = []recipe.ComponentRef{{
		Name: "digested", Namespace: "digested", Chart: "digested", Version: "1.2.3",
		Type: recipe.ComponentTypeHelm, Source: "https://charts.example.com",
	}}
	rr.DeploymentOrder = []string{"digested"}
	rr.BindDataProvider(layered)
	return rr
}

// TestGenerate_VendorChartsVerifiesRegistryDigest proves this deployer hands
// the registry's helm.defaultDigest to the vendor step: a matching archive
// is vendored, and a mismatching one fails the bundle.
func TestGenerate_VendorChartsVerifiesRegistryDigest(t *testing.T) {
	sum := sha256.Sum256(digestStubArchive)
	for _, tt := range []struct {
		name    string
		digest  string
		wantErr bool
	}{
		{name: "matching archive is vendored", digest: "sha256:" + hex.EncodeToString(sum[:])},
		{name: "mismatching archive fails the bundle", digest: "sha256:" + strings.Repeat("0", 64), wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := &Generator{
				RecipeResult:    digestPinnedRecipe(t, tt.digest),
				ComponentValues: map[string]map[string]any{"digested": {}},
				Version:         "v0.0.0-test",
				VendorCharts:    true,
				Puller:          digestStubPuller{},
			}
			_, err := g.Generate(context.Background(), t.TempDir())
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Generate() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Generate() error = nil, want the digest mismatch")
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeConflict)
		})
	}
}
