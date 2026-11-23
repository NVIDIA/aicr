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
	"fmt"
	"slices"
	"strings"
	"testing"

	bundleconfig "aicr.run/pkg/bundler/config"
	appconfig "aicr.run/pkg/config"
	"aicr.run/pkg/recipe/providertest"
)

const composedRegistryFmt = `apiVersion: aicr.run/v1beta1
kind: ComponentRegistry
metadata:
  name: %s
components:
%s`

func composedComponent(name string) string {
	return "  - name: " + name + "\n    displayName: " + name + "\n    helm:\n      defaultRepository: https://example.invalid/charts\n      defaultChart: " + name + "\n      defaultVersion: 1.2.3\n"
}

func composedRegistry(name string, components ...string) []byte {
	var body strings.Builder
	for _, c := range components {
		body.WriteString(composedComponent(c))
	}
	text := body.String()
	if text == "" {
		return []byte("apiVersion: aicr.run/v1beta1\nkind: ComponentRegistry\nmetadata:\n  name: " + name + "\ncomponents: []\n")
	}
	return []byte(fmt.Sprintf(composedRegistryFmt, name, text))
}

// composedCatalogDirs writes two directory catalogs: "alpha" owns the base,
// the eks overlay and the "custom" component; "beta" extends alpha's eks
// overlay and adds the "extra" component.
func composedCatalogDirs(t *testing.T) (alpha, beta string) {
	t.Helper()
	alpha = providertest.WriteDir(t, map[string][]byte{
		"registry.yaml": composedRegistry("alpha", "custom"),
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
		"overlays/eks.yaml":             []byte("apiVersion: aicr.run/v1beta1\nkind: RecipeMetadata\nmetadata:\n  name: alpha-eks\nspec:\n  base: base\n  criteria:\n    service: eks\n"),
		"components/custom/values.yaml": []byte("alpha: true\n"),
		"validators/catalog.yaml":       []byte("apiVersion: validator.nvidia.com/v1alpha1\nkind: ValidatorCatalog\nvalidators: []\n"),
	})
	beta = providertest.WriteDir(t, map[string][]byte{
		"registry.yaml": composedRegistry("beta", "extra"),
		"overlays/eks-training.yaml": []byte(`apiVersion: aicr.run/v1beta1
kind: RecipeMetadata
metadata:
  name: beta-eks-training
spec:
  base: alpha-eks
  criteria:
    service: eks
    intent: training
  componentRefs:
    - name: extra
      type: helm
      valuesFile: components/extra/values.yaml
`),
		"components/extra/values.yaml": []byte("beta: true\n"),
	})
	return alpha, beta
}

func resolvedComponentNames(t *testing.T, source RecipeSourceOption) []string {
	t.Helper()
	client, err := NewClientContext(t.Context(), WithRecipeSource(source))
	if err != nil {
		t.Fatalf("NewClientContext: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	result, err := client.ResolveRecipe(t.Context(), RecipeRequest{Service: "eks", Intent: "training"})
	if err != nil {
		t.Fatalf("ResolveRecipe: %v", err)
	}
	var names []string
	for _, ref := range result.internal.ComponentRefs {
		names = append(names, ref.Name)
	}
	slices.Sort(names)

	values, err := result.internal.GetValuesForComponentWithContext(t.Context(), "extra")
	if err != nil || values["beta"] != true {
		t.Fatalf("beta's values = %v, error = %v", values, err)
	}
	values, err = result.internal.GetValuesForComponentWithContext(t.Context(), "custom")
	if err != nil || values["alpha"] != true {
		t.Fatalf("alpha's values = %v, error = %v", values, err)
	}
	return names
}

// TestComposedSourceResolvesAcrossCatalogs composes two directory catalogs
// without the embedded one: beta's overlay extends alpha's, and each
// catalog's component values resolve from its own files.
func TestComposedSourceResolvesAcrossCatalogs(t *testing.T) {
	alpha, beta := composedCatalogDirs(t)

	forward := resolvedComponentNames(t, ComposedSource(ExternalFilesystemSource(alpha), ExternalFilesystemSource(beta)))
	reverse := resolvedComponentNames(t, ComposedSource(ExternalFilesystemSource(beta), ExternalFilesystemSource(alpha)))
	if want := []string{"custom", "extra"}; !slices.Equal(forward, want) {
		t.Fatalf("components = %v, want %v", forward, want)
	}
	if !slices.Equal(forward, reverse) {
		t.Errorf("components depend on catalog order: %v vs %v", forward, reverse)
	}

	client, err := NewClientContext(t.Context(), WithRecipeSource(ComposedSource(
		ExternalFilesystemSource(alpha), ExternalFilesystemSource(beta))))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	result, err := client.ResolveRecipe(t.Context(), RecipeRequest{Service: "eks", Intent: "training"})
	if err != nil {
		t.Fatal(err)
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
}

func TestComposedSourceCollisionFailsConstruction(t *testing.T) {
	alpha, _ := composedCatalogDirs(t)
	twin := providertest.WriteDir(t, map[string][]byte{
		"registry.yaml": composedRegistry("twin", "custom"), // alpha also defines "custom"
	})
	client, err := NewClientContext(t.Context(), WithRecipeSource(ComposedSource(
		ExternalFilesystemSource(alpha), ExternalFilesystemSource(twin))))
	if client != nil {
		_ = client.Close()
	}
	if err == nil {
		t.Fatal("expected a collision error")
	}
	for _, want := range []string{`component "custom"`, `"alpha"`, `"twin"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

func TestComposedSourceRejectsUnsupportedChildren(t *testing.T) {
	client, err := NewClientContext(t.Context(), WithRecipeSource(ComposedSource(
		OCISource("oci://example.invalid/catalog", "sha256:"+strings.Repeat("0", 64)))))
	if client != nil {
		_ = client.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "can be composed") {
		t.Fatalf("error = %v, want one saying only embedded and filesystem sources compose", err)
	}
}

// TestComposedSourceWithEmbeddedCatalog composes the real built-in catalog
// with a partner catalog whose overlay extends an embedded overlay.
func TestComposedSourceWithEmbeddedCatalog(t *testing.T) {
	partner := providertest.WriteDir(t, map[string][]byte{
		"registry.yaml": composedRegistry("partner"),
		"overlays/partner-leaf.yaml": []byte(`apiVersion: aicr.run/v1beta1
kind: RecipeMetadata
metadata:
  name: partner-leaf
spec:
  base: h100-eks-ubuntu-training
  criteria:
    service: eks
    accelerator: h100
    os: ubuntu
    intent: training
    platform: partner-platform
`),
	})
	client, err := NewClientContext(t.Context(), WithRecipeSource(ComposedSource(
		CatalogSource("nvidia"), CatalogSource(partner))))
	if err != nil {
		t.Fatalf("NewClientContext: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	entries, err := client.ListCatalog(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]string{}
	for _, e := range entries {
		sources[e.Name] = e.Source
	}
	if sources["partner-leaf"] != CatalogSourceExternal {
		t.Errorf("partner-leaf source = %q, want %q", sources["partner-leaf"], CatalogSourceExternal)
	}
	if sources["h100-eks-ubuntu-training"] != CatalogSourceEmbedded {
		t.Errorf("h100-eks-ubuntu-training source = %q, want %q", sources["h100-eks-ubuntu-training"], CatalogSourceEmbedded)
	}

	// The same partner catalog without nvidia names the missing base.
	client2, err := NewClientContext(t.Context(), WithRecipeSource(ComposedSource(CatalogSource(partner))))
	if client2 != nil {
		_ = client2.Close()
	}
	if err == nil || !strings.Contains(err.Error(), `"h100-eks-ubuntu-training"`) {
		t.Fatalf("error = %v, want one naming the missing base overlay", err)
	}
}

func TestCatalogSourceAndConfig(t *testing.T) {
	if got := CatalogSource("nvidia").internal.kind; got != sourceKindEmbedded {
		t.Errorf(`CatalogSource("nvidia") kind = %v, want embedded`, got)
	}
	alpha, beta := composedCatalogDirs(t)
	cfg := WrapConfig(&appconfig.AICRConfig{Spec: appconfig.Spec{Recipe: &appconfig.RecipeSpec{Catalogs: []string{alpha, beta}}}})
	source, ok := cfg.RecipeSource()
	if !ok || source.internal.kind != sourceKindComposed || source.internal.composed == nil || len(source.internal.composed.children) != 2 {
		t.Fatalf("RecipeSource = %+v, %v; want a composed source of two catalogs", source.internal, ok)
	}
	if names := resolvedComponentNames(t, source); !slices.Equal(names, []string{"custom", "extra"}) {
		t.Errorf("components = %v", names)
	}
}
