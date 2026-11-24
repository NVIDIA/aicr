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
	stderrors "errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aicr.run/pkg/bundler/config"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
	"aicr.run/pkg/recipe"
	"aicr.run/pkg/recipe/providertest"
)

const (
	composedTestRegistryFmt = "apiVersion: aicr.run/v1beta1\nkind: ComponentRegistry\nmetadata:\n  name: %s\ncomponents: []\n"
	composedTestBase        = "apiVersion: aicr.run/v1beta1\nkind: RecipeMetadata\nmetadata:\n  name: base\nspec: {}\n"
	composedTestLeaf        = "apiVersion: aicr.run/v1beta1\nkind: RecipeMetadata\nmetadata:\n  name: partner-leaf\nspec:\n  base: base\n"
)

// composedProvider returns a composition of the built-in-style catalog and,
// when withDirectory is set, a partner directory catalog. Both are in memory:
// the guard under test looks at which catalogs are composed, not at the disk.
func composedProvider(t *testing.T, withDirectory bool) *recipe.ComposedDataProvider {
	t.Helper()
	registry := func(name string) []byte {
		return []byte(strings.Replace(composedTestRegistryFmt, "%s", name, 1))
	}
	inputs := []recipe.CatalogInput{{
		Provider: providertest.NewMap(map[string][]byte{
			"registry.yaml":      registry(recipe.NvidiaCatalogName),
			"overlays/base.yaml": []byte(composedTestBase),
		}),
		Label:    "embedded",
		Embedded: true,
	}}
	if withDirectory {
		inputs = append(inputs, recipe.CatalogInput{
			Provider: providertest.NewMap(map[string][]byte{
				"registry.yaml":              registry("partner"),
				"overlays/partner-leaf.yaml": []byte(composedTestLeaf),
			}),
			Label: "./partner",
		})
	}
	dp, err := recipe.NewComposedDataProvider(t.Context(), inputs)
	if err != nil {
		t.Fatalf("NewComposedDataProvider: %v", err)
	}
	t.Cleanup(func() {
		recipe.EvictCachedStore(dp)
		recipe.EvictCachedRegistry(dp)
		recipe.EvictCachedCriteriaRegistry(dp)
	})
	return dp
}

// TestMake_AttestRejectsComposedDirectoryCatalog pins the guard that keeps
// --attest from signing a bundle whose recipe data it cannot carry. The bundle
// copies data/ only for a single --data directory, so a composition that
// includes a catalog directory would otherwise be attested as embedded-only
// and verification would trust it as such.
func TestMake_AttestRejectsComposedDirectoryCatalog(t *testing.T) {
	t.Parallel()

	t.Run("attest with a composed directory catalog is rejected before anything is written", func(t *testing.T) {
		t.Parallel()
		dest := filepath.Join(t.TempDir(), "out")
		att := &recordingAttester{}
		b, err := New(
			WithConfig(config.NewConfig(config.WithAttest(true))),
			WithAttester(att),
			WithVerifiedBinaryAttestation([]byte("stub")),
		)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		rr := closedWorldRecipeResult()
		rr.BindDataProvider(composedProvider(t, true))

		_, err = b.Make(t.Context(), rr, dest)
		if err == nil || !strings.Contains(err.Error(), "composed catalog") {
			t.Fatalf("Make() error = %v, want the composed-catalog attestation rejection", err)
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		if att.subject != nil {
			t.Error("the attester was called for a bundle the guard rejects")
		}
		if _, statErr := os.Stat(dest); !stderrors.Is(statErr, os.ErrNotExist) {
			t.Errorf("output directory exists after rejection: %v", statErr)
		}
	})

	t.Run("attest over the built-in catalog alone is not affected", func(t *testing.T) {
		t.Parallel()
		b, err := New(
			WithConfig(config.NewConfig(config.WithAttest(true))),
			WithAttester(&recordingAttester{}),
			WithVerifiedBinaryAttestation([]byte("stub")),
		)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		rr := closedWorldRecipeResult()
		rr.BindDataProvider(composedProvider(t, false))
		if _, err = b.Make(t.Context(), rr, t.TempDir()); err != nil {
			t.Fatalf("Make() error = %v: a composition of the built-in catalog alone carries no external data", err)
		}
	})

	t.Run("a composed directory catalog without attest still builds", func(t *testing.T) {
		t.Parallel()
		b, err := New(WithConfig(config.NewConfig()))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		rr := closedWorldRecipeResult()
		rr.BindDataProvider(composedProvider(t, true))
		if _, err = b.Make(t.Context(), rr, t.TempDir()); err != nil {
			t.Fatalf("Make() error = %v", err)
		}
	})
}
