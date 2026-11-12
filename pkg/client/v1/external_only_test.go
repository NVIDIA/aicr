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

package aicr

import (
	"strings"
	"testing"

	bundleconfig "github.com/NVIDIA/aicr/pkg/bundler/config"
	appconfig "github.com/NVIDIA/aicr/pkg/config"
	"github.com/NVIDIA/aicr/pkg/recipe/providertest"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
)

func TestExternalOnlyClient(t *testing.T) {
	files := map[string][]byte{
		"registry.yaml": []byte(`apiVersion: aicr.run/v1beta1
kind: ComponentRegistry
components:
  - name: custom
    displayName: Custom
    helm:
      defaultRepository: https://example.invalid/charts
      defaultChart: custom
      defaultVersion: 1.2.3
`),
		"overlays/base.yaml": []byte(`apiVersion: aicr.run/v1beta1
kind: RecipeMetadata
metadata:
  name: base
spec:
  componentRefs:
    - name: custom
      type: helm
      valuesFile: components/custom/values.yaml
`),
		"overlays/eks.yaml":             []byte("apiVersion: aicr.run/v1beta1\nkind: RecipeMetadata\nmetadata:\n  name: external-eks\nspec:\n  base: base\n  criteria:\n    service: eks\n"),
		"components/custom/values.yaml": []byte("externalMarker: true\n"),
		"validators/catalog.yaml":       []byte("apiVersion: validator.nvidia.com/v1alpha1\nkind: ValidatorCatalog\nvalidators: []\n"),
	}
	dir := providertest.WriteDir(t, files)
	cfg := WrapConfig(&appconfig.AICRConfig{Spec: appconfig.Spec{Recipe: &appconfig.RecipeSpec{Data: dir, ExternalOnly: true}}})
	source, ok := cfg.RecipeSource()
	if !ok {
		t.Fatal("config omitted external source")
	}
	client, err := NewClientContext(t.Context(), WithRecipeSource(source))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	result, err := client.ResolveRecipe(t.Context(), RecipeRequest{Service: "eks"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.internal.ComponentRefs) != 1 || result.internal.ComponentRefs[0].Name != "custom" {
		t.Fatalf("unexpected components: %+v", result.internal.ComponentRefs)
	}
	values, err := result.internal.GetValuesForComponentWithContext(t.Context(), "custom")
	if err != nil || values["externalMarker"] != true || len(values) != 1 {
		t.Fatalf("values = %v, error = %v", values, err)
	}
	artifact, err := client.MakeBundle(t.Context(), result, BundleOptions{
		Config:    bundleconfig.NewConfig(bundleconfig.WithDeployer(bundleconfig.DeployerHelm)),
		OutputDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if artifact.HasErrors() || artifact.TotalFiles == 0 {
		t.Fatalf("bundle failed: %+v", artifact)
	}
	cat, err := catalog.LoadWithDataProvider(t.Context(), client.dp, "dev", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Validators) != 0 {
		t.Fatalf("embedded validators leaked: %d", len(cat.Validators))
	}
}

func TestExternalOnlyClientMissingValidatorCatalog(t *testing.T) {
	dir := providertest.WriteDir(t, map[string][]byte{"registry.yaml": []byte(providertest.EmptyRegistryYAML)})
	client, err := NewClientContext(t.Context(), WithRecipeSource(ExternalFilesystemSource(dir)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_, err = catalog.LoadWithDataProvider(t.Context(), client.dp, "dev", "")
	if err == nil || !strings.Contains(err.Error(), "validators/catalog.yaml") {
		t.Fatalf("missing validator catalog error: %v", err)
	}
}

func TestExternalOnlyConfigRequiresDirectory(t *testing.T) {
	cfg := WrapConfig(&appconfig.AICRConfig{Spec: appconfig.Spec{Recipe: &appconfig.RecipeSpec{ExternalOnly: true}}})
	source, ok := cfg.RecipeSource()
	if !ok {
		t.Fatal("external-only config must not silently select embedded source")
	}
	client, err := NewClientContext(t.Context(), WithRecipeSource(source))
	if client != nil {
		_ = client.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "requires an external data directory") {
		t.Fatalf("missing data directory: %v", err)
	}
}
