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

// Package deployertest provides recipe fixtures shared by the deployer tests.
// It must not import pkg/bundler/deployer or any package that does, so that
// package's own tests can use it.
package deployertest

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/recipe/providertest"
)

// Components in DigestDataProvider's registry, each the chart
// example/<name> at DigestChartVersion from DigestChartRepository.
// DigestedComponent pins a chart digest and UndigestedComponent does not.
const (
	DigestedComponent     = "digested"
	UndigestedComponent   = "undigested"
	DigestChartVersion    = "1.2.3"
	DigestChartRepository = "https://charts.example.com"
)

// DigestDataProvider layers a registry holding DigestedComponent, pinned
// to digest, and UndigestedComponent over the embedded data, so tests do not
// depend on which real components carry a digest.
func DigestDataProvider(t testing.TB, digest string) recipe.DataProvider {
	t.Helper()
	registryYAML := `apiVersion: aicr.run/v1beta1
kind: ComponentRegistry
components:
  - name: ` + DigestedComponent + `
    displayName: Digested
    valueOverrideKeys: [` + DigestedComponent + `]
    helm:
      defaultRepository: ` + DigestChartRepository + `
      defaultChart: example/` + DigestedComponent + `
      defaultVersion: ` + DigestChartVersion + `
      defaultDigest: ` + digest + `
  - name: ` + UndigestedComponent + `
    displayName: Undigested
    helm:
      defaultRepository: ` + DigestChartRepository + `
      defaultChart: example/` + UndigestedComponent + `
      defaultVersion: ` + DigestChartVersion + `
`
	dataDir := providertest.WriteDir(t, map[string][]byte{recipe.RegistryFileName: []byte(registryYAML)})
	layered, err := recipe.NewLayeredDataProvider(
		recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), ""),
		recipe.LayeredProviderConfig{ExternalDir: dataDir})
	if err != nil {
		t.Fatalf("NewLayeredDataProvider: %v", err)
	}
	t.Cleanup(func() { recipe.EvictCachedRegistry(layered) })
	return layered
}

// DigestPinnedRecipe returns a recipe that deploys DigestedComponent, bound
// to DigestDataProvider(t, digest).
func DigestPinnedRecipe(t testing.TB, digest string) *recipe.RecipeResult {
	t.Helper()
	rr := &recipe.RecipeResult{}
	rr.Metadata.Version = "v1.0.0"
	rr.ComponentRefs = []recipe.ComponentRef{{
		Name: DigestedComponent, Namespace: DigestedComponent, Chart: DigestedComponent,
		Version: DigestChartVersion, Type: recipe.ComponentTypeHelm, Source: DigestChartRepository,
	}}
	rr.DeploymentOrder = []string{DigestedComponent}
	rr.BindDataProvider(DigestDataProvider(t, digest))
	return rr
}

// UnhashedRecordSHA256 is the VendorRecord SHA256 a RunVendorDigestCases
// puller reports. It is not the SHA-256 of any archive.
const UnhashedRecordSHA256 = "not-the-archive-sha256"

// RunVendorDigestCases checks that generate vendors DigestedComponent when the
// registry pins the digest of archive, and fails with ErrCodeConflict when it
// pins another. generate bundles rr with chart vendoring on, using a puller
// that returns archive and a VendorRecord whose SHA256 is UnhashedRecordSHA256,
// so the cases fail if the deployer checks the pin against the record.
func RunVendorDigestCases(t *testing.T, archive []byte, generate func(t *testing.T, rr *recipe.RecipeResult) error) {
	t.Helper()
	sum := sha256.Sum256(archive)
	for _, tt := range []struct {
		name    string
		digest  string
		wantErr bool
	}{
		{name: "matching archive is vendored", digest: "sha256:" + hex.EncodeToString(sum[:])},
		{name: "mismatching archive fails the bundle", digest: "sha256:" + strings.Repeat("0", 64), wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := generate(t, DigestPinnedRecipe(t, tt.digest))
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
