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

package main

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bom"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/helm/helmtest"
)

// writeRecipeTree lays out a minimal repo root with the shared variant-test
// registry and the given recipe sources.
func writeRecipeTree(t *testing.T, sources map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"overlays", "mixins"} {
		if err := os.MkdirAll(filepath.Join(root, "recipes", dir), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "recipes", "registry.yaml"), []byte(variantTestRegistry), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	for rel, content := range sources {
		if err := os.WriteFile(filepath.Join(root, "recipes", rel), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return root
}

const variantTestRegistry = `apiVersion: aicr.run/v1alpha2
kind: ComponentRegistry
components:
  - name: kube-prometheus-stack
    displayName: Kube Prometheus Stack
    helm:
      defaultRepository: https://prometheus-community.github.io/helm-charts
      defaultChart: prometheus-community/kube-prometheus-stack
      defaultVersion: 84.4.0
      defaultNamespace: monitoring
  - name: gpu-operator
    displayName: GPU Operator
    helm:
      defaultRepository: https://helm.ngc.nvidia.com/nvidia
      defaultChart: nvidia/gpu-operator
      defaultVersion: v25.3.0
  - name: my-kustomize
    displayName: My Kustomize
    kustomize:
      defaultSource: https://github.com/example/app
      defaultTag: v1.0.0
`

func TestDeriveVariants(t *testing.T) {
	root := writeRecipeTree(t, map[string]string{
		// Two sources pin the SAME divergent version -> one aggregated variant.
		"overlays/aks.yaml": `kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: aks
spec:
  componentRefs:
    - name: kube-prometheus-stack
      version: "83.7.0"
`,
		"mixins/platform-x.yaml": `kind: RecipeMixin
apiVersion: aicr.run/v1alpha2
metadata:
  name: platform-x
spec:
  componentRefs:
    - name: kube-prometheus-stack
      version: "83.7.0"
`,
		// Default-equal pin -> no variant.
		"overlays/base.yaml": `kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: base
spec:
  componentRefs:
    - name: gpu-operator
      version: v25.3.0
`,
		// Non-registry component and a Kustomize tag-only ref -> no variant.
		"overlays/extra.yaml": `kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: extra
spec:
  componentRefs:
    - name: not-in-registry
      version: "9.9.9"
    - name: my-kustomize
      version: "2.0.0"
`,
	})

	reg, err := loadRegistry(filepath.Join(root, "recipes", "registry.yaml"))
	if err != nil {
		t.Fatalf("loadRegistry: %v", err)
	}
	sources, err := loadRecipeSources(context.Background(), root)
	if err != nil {
		t.Fatalf("loadRecipeSources: %v", err)
	}

	variants, err := deriveVariants(reg, sources)
	if err != nil {
		t.Fatalf("deriveVariants: %v", err)
	}
	if len(variants) != 1 {
		t.Fatalf("variants = %+v, want exactly one (the aggregated kube-prometheus-stack divergence)", variants)
	}
	v := variants[0]
	if v.Name != "kube-prometheus-stack" || v.Version != "83.7.0" {
		t.Errorf("variant = %s@%s, want kube-prometheus-stack@83.7.0", v.Name, v.Version)
	}
	if want := []string{"aks", "platform-x"}; !reflect.DeepEqual(v.Sources, want) {
		t.Errorf("sources = %v, want %v (aggregated and sorted)", v.Sources, want)
	}
	if v.Repository == "" || v.Chart == "" {
		t.Errorf("variant should inherit chart coordinates from the registry, got repo=%q chart=%q",
			v.Repository, v.Chart)
	}
}

func TestDeriveVariantsMultipleVersionsSameComponent(t *testing.T) {
	root := writeRecipeTree(t, map[string]string{
		"overlays/a.yaml": `kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: a
spec:
  componentRefs:
    - name: kube-prometheus-stack
      version: "83.7.0"
`,
		"overlays/b.yaml": `kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: b
spec:
  componentRefs:
    - name: kube-prometheus-stack
      version: "82.0.0"
`,
	})

	reg, err := loadRegistry(filepath.Join(root, "recipes", "registry.yaml"))
	if err != nil {
		t.Fatalf("loadRegistry: %v", err)
	}
	sources, err := loadRecipeSources(context.Background(), root)
	if err != nil {
		t.Fatalf("loadRecipeSources: %v", err)
	}

	variants, err := deriveVariants(reg, sources)
	if err != nil {
		t.Fatalf("deriveVariants: %v", err)
	}
	if len(variants) != 2 {
		t.Fatalf("variants = %+v, want two (distinct versions are distinct variants)", variants)
	}
	// Deterministic ordering by (name, version).
	if variants[0].Version != "82.0.0" || variants[1].Version != "83.7.0" {
		t.Errorf("variant order = %s, %s; want 82.0.0 then 83.7.0",
			variants[0].Version, variants[1].Version)
	}
}

func TestLoadRecipeSourcesRejectsMalformed(t *testing.T) {
	root := writeRecipeTree(t, map[string]string{
		"overlays/broken.yaml": "not: [valid: yaml: {{",
	})
	if _, err := loadRecipeSources(context.Background(), root); err == nil {
		t.Fatal("loadRecipeSources accepted a malformed source; a skipped source could hide a divergent pin")
	}

	root2 := writeRecipeTree(t, map[string]string{
		"overlays/anon.yaml": "kind: RecipeMetadata\nspec:\n  componentRefs: []\n",
	})
	if _, err := loadRecipeSources(context.Background(), root2); err == nil {
		t.Fatal("loadRecipeSources accepted a source without metadata.name")
	}
}

func TestSurveyVariantRendersAtVariantVersion(t *testing.T) {
	root := writeRecipeTree(t, nil)
	mock := &helmtest.MockRenderer{
		Rendered: map[string][]byte{
			"kube-prometheus-stack": []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kps
spec:
  template:
    spec:
      containers:
      - name: prometheus
        image: quay.io/prometheus/prometheus:v2.83.0
`),
		},
	}

	base := component{
		Name: "kube-prometheus-stack",
		Helm: helmCfg{
			DefaultRepository: "https://prometheus-community.github.io/helm-charts",
			DefaultChart:      "prometheus-community/kube-prometheus-stack",
			DefaultVersion:    "84.4.0",
		},
	}
	v := bom.VariantResult{Name: "kube-prometheus-stack", Version: "83.7.0", Sources: []string{"aks"}}

	got := surveyVariant(context.Background(), root, v, base, mock, false)
	if len(got.Images) != 1 || got.Images[0] != "quay.io/prometheus/prometheus:v2.83.0" {
		t.Errorf("images = %v, want the rendered image", got.Images)
	}
	if len(mock.Inputs) != 1 {
		t.Fatalf("renderer calls = %d, want 1", len(mock.Inputs))
	}
	if mock.Inputs[0].Version != "83.7.0" {
		t.Errorf("rendered at version %q, want the VARIANT version 83.7.0 — variant image sets must be "+
			"rendered, not copied from the default entry", mock.Inputs[0].Version)
	}

	// skipHelm leaves the variant unrendered (no images, no renderer call).
	mock2 := &helmtest.MockRenderer{}
	got2 := surveyVariant(context.Background(), root, v, base, mock2, true)
	if len(got2.Images) != 0 || len(mock2.Inputs) != 0 {
		t.Errorf("skipHelm variant should not render, got images=%v calls=%d", got2.Images, len(mock2.Inputs))
	}
}

func TestLoadRecipeSourcesSkipsForeignKindLikeCanonicalLoader(t *testing.T) {
	// The canonical metadata store SKIPS non-RecipeMetadata files under the
	// overlay walk (e.g. a stray ValidatorCatalog); the BOM loader matches —
	// the file is not an error, and crucially it is never mined for pins.
	root := writeRecipeTree(t, map[string]string{
		"overlays/stray.yaml": "kind: ComponentRegistry\nmetadata:\n  name: stray\nspec:\n  componentRefs:\n    - name: kube-prometheus-stack\n      version: \"83.7.0\"\n",
	})
	sources, err := loadRecipeSources(context.Background(), root)
	if err != nil {
		t.Fatalf("loadRecipeSources rejected a foreign-kind file the canonical loader skips: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("sources = %+v; a skipped foreign-kind file must contribute no pins", sources)
	}
}

func TestLoadRecipeSourcesAcceptsLegacyEmptyKindOverlay(t *testing.T) {
	// The canonical store treats an overlay without a kind as legacy
	// RecipeMetadata; the BOM loader must not reject what the store loads.
	root := writeRecipeTree(t, map[string]string{
		"overlays/legacy.yaml": "apiVersion: aicr.run/v1alpha2\nmetadata:\n  name: legacy\nspec:\n  componentRefs:\n    - name: kube-prometheus-stack\n      version: \"83.7.0\"\n",
	})
	sources, err := loadRecipeSources(context.Background(), root)
	if err != nil {
		t.Fatalf("loadRecipeSources rejected a legacy empty-kind overlay: %v", err)
	}
	if len(sources) != 1 || sources[0].Name != "legacy" {
		t.Fatalf("sources = %+v, want the legacy overlay loaded", sources)
	}
}

func TestLoadRecipeSourcesDiscoversNestedOverlays(t *testing.T) {
	// Subdirectories under overlays/ are documented and supported
	// (docs/integrator/data-extension.md); a nested divergent pin must not
	// be silently omitted from the projection.
	root := writeRecipeTree(t, nil)
	nested := filepath.Join(root, "recipes", "overlays", "team-a")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	overlay := "kind: RecipeMetadata\napiVersion: aicr.run/v1alpha2\nmetadata:\n  name: team-a-leaf\nspec:\n  componentRefs:\n    - name: kube-prometheus-stack\n      version: \"83.7.0\"\n"
	if err := os.WriteFile(filepath.Join(nested, "leaf.yaml"), []byte(overlay), 0o600); err != nil {
		t.Fatalf("write nested overlay: %v", err)
	}

	reg, err := loadRegistry(filepath.Join(root, "recipes", "registry.yaml"))
	if err != nil {
		t.Fatalf("loadRegistry: %v", err)
	}
	sources, err := loadRecipeSources(context.Background(), root)
	if err != nil {
		t.Fatalf("loadRecipeSources: %v", err)
	}
	variants, err := deriveVariants(reg, sources)
	if err != nil {
		t.Fatalf("deriveVariants: %v", err)
	}
	if len(variants) != 1 || variants[0].Sources[0] != "team-a-leaf" {
		t.Fatalf("variants = %+v, want the nested overlay's divergent pin discovered", variants)
	}
}

func TestDeriveVariantsRejectsPaddedPin(t *testing.T) {
	root := writeRecipeTree(t, map[string]string{
		"overlays/padded.yaml": `kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: padded
spec:
  componentRefs:
    - name: kube-prometheus-stack
      version: " 83.7.0"
`,
	})
	reg, err := loadRegistry(filepath.Join(root, "recipes", "registry.yaml"))
	if err != nil {
		t.Fatalf("loadRegistry: %v", err)
	}
	sources, err := loadRecipeSources(context.Background(), root)
	if err != nil {
		t.Fatalf("loadRecipeSources: %v", err)
	}
	if _, err := deriveVariants(reg, sources); err == nil {
		t.Fatal("deriveVariants accepted a padded pin; the source fact must be preserved or rejected, never normalized")
	}
}

func TestDeriveVariantsSkipsExplicitNonHelmType(t *testing.T) {
	root := writeRecipeTree(t, map[string]string{
		"overlays/typed.yaml": `kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: typed
spec:
  componentRefs:
    - name: kube-prometheus-stack
      type: Kustomize
      version: "83.7.0"
`,
	})
	reg, err := loadRegistry(filepath.Join(root, "recipes", "registry.yaml"))
	if err != nil {
		t.Fatalf("loadRegistry: %v", err)
	}
	sources, err := loadRecipeSources(context.Background(), root)
	if err != nil {
		t.Fatalf("loadRecipeSources: %v", err)
	}
	variants, err := deriveVariants(reg, sources)
	if err != nil {
		t.Fatalf("deriveVariants: %v", err)
	}
	if len(variants) != 0 {
		t.Fatalf("variants = %+v; an explicitly non-Helm-typed ref is not a Helm chart pin", variants)
	}
}

func TestSurveyVariantIncludesManifestImages(t *testing.T) {
	root := writeRecipeTree(t, nil)
	manifestsDir := filepath.Join(root, "recipes", "components", "kube-prometheus-stack", "manifests")
	if err := os.MkdirAll(manifestsDir, 0o755); err != nil {
		t.Fatalf("mkdir manifests: %v", err)
	}
	manifest := `apiVersion: v1
kind: Pod
metadata:
  name: extra
spec:
  containers:
  - name: extra
    image: nvcr.io/nvidia/extra:v1.0.0
`
	if err := os.WriteFile(filepath.Join(manifestsDir, "extra.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	base := component{
		Name: "kube-prometheus-stack",
		Helm: helmCfg{
			DefaultRepository: "https://prometheus-community.github.io/helm-charts",
			DefaultChart:      "prometheus-community/kube-prometheus-stack",
			DefaultVersion:    "84.4.0",
		},
	}
	v := bom.VariantResult{Name: "kube-prometheus-stack", Version: "83.7.0", Sources: []string{"aks"}}

	// With -skip-helm the variant must still pick up manifest images, exactly
	// like the default survey path.
	got := surveyVariant(context.Background(), root, v, base, &helmtest.MockRenderer{}, true)
	if len(got.Images) != 1 || got.Images[0] != "nvcr.io/nvidia/extra:v1.0.0" {
		t.Errorf("skip-helm variant images = %v, want the embedded manifest image", got.Images)
	}
}

func TestLoadRecipeSourcesKindPerDirectory(t *testing.T) {
	// Canonical parity: a RecipeMixin under overlays/ is skipped (foreign
	// kind in the overlay walk, contributes no pins); a RecipeMetadata under
	// mixins/ is a hard error (the store rejects wrong-kind mixin files).
	root := writeRecipeTree(t, map[string]string{
		"overlays/misplaced.yaml": "kind: RecipeMixin\napiVersion: aicr.run/v1alpha2\nmetadata:\n  name: misplaced\nspec:\n  componentRefs:\n    - name: kube-prometheus-stack\n      version: \"83.7.0\"\n",
	})
	sources, err := loadRecipeSources(context.Background(), root)
	if err != nil {
		t.Fatalf("misplaced mixin under overlays should be skipped, got error: %v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("sources = %+v; a skipped misplaced mixin must contribute no pins", sources)
	}

	root2 := writeRecipeTree(t, map[string]string{
		"mixins/misplaced.yaml": "kind: RecipeMetadata\napiVersion: aicr.run/v1alpha2\nmetadata:\n  name: misplaced\nspec:\n  componentRefs: []\n",
	})
	if _, err := loadRecipeSources(context.Background(), root2); err == nil {
		t.Fatal("loadRecipeSources accepted a RecipeMetadata under mixins/ (the canonical store hard-errors)")
	}
}

func TestDeriveVariantsRendersPinWithEmptyRegistryDefault(t *testing.T) {
	// An external -repo-root registry may omit defaultVersion. The explicit
	// pin is still the version the source deploys: it must surface as a
	// variant, not vanish from the inventory.
	root := t.TempDir()
	for _, dir := range []string{"overlays", "mixins"} {
		if err := os.MkdirAll(filepath.Join(root, "recipes", dir), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	registry := `apiVersion: aicr.run/v1alpha2
kind: ComponentRegistry
components:
  - name: unpinned-helm
    displayName: Unpinned Helm
    helm:
      defaultRepository: https://charts.example.com
      defaultChart: example/unpinned-helm
`
	if err := os.WriteFile(filepath.Join(root, "recipes", "registry.yaml"), []byte(registry), 0o600); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	overlay := `kind: RecipeMetadata
apiVersion: aicr.run/v1alpha2
metadata:
  name: pinned-leaf
spec:
  componentRefs:
    - name: unpinned-helm
      version: "1.2.3"
`
	if err := os.WriteFile(filepath.Join(root, "recipes", "overlays", "pinned-leaf.yaml"), []byte(overlay), 0o600); err != nil {
		t.Fatalf("write overlay: %v", err)
	}

	reg, err := loadRegistry(filepath.Join(root, "recipes", "registry.yaml"))
	if err != nil {
		t.Fatalf("loadRegistry: %v", err)
	}
	sources, err := loadRecipeSources(context.Background(), root)
	if err != nil {
		t.Fatalf("loadRecipeSources: %v", err)
	}
	variants, err := deriveVariants(reg, sources)
	if err != nil {
		t.Fatalf("deriveVariants: %v", err)
	}
	if len(variants) != 1 || variants[0].Version != "1.2.3" {
		t.Fatalf("variants = %+v, want the explicit pin rendered as a variant", variants)
	}
}

func TestLoadRecipeSourcesRejectsEscapingSymlink(t *testing.T) {
	// A checked-in symlink that escapes the recipes root could smuggle
	// another checkout's overlay in — exactly the checkout skew the
	// same-repo-root design prevents. os.Root confines resolution.
	root := writeRecipeTree(t, nil)
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(outside, []byte("kind: RecipeMetadata\napiVersion: aicr.run/v1alpha2\nmetadata:\n  name: outside\nspec:\n  componentRefs: []\n"), 0o600); err != nil {
		t.Fatalf("write outside: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "recipes", "overlays", "escape.yaml")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	_, err := loadRecipeSources(context.Background(), root)
	if err == nil {
		t.Fatal("loadRecipeSources followed a symlink escaping the recipes root")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error code = %v, want %v (an escaping symlink is invalid operator input)",
			err, errors.ErrCodeInvalidRequest)
	}
	if !strings.Contains(err.Error(), "escapes the recipes root") {
		t.Errorf("error %q does not explain the confinement violation", err.Error())
	}
}

func TestLoadRecipeSourcesPreCanceledContext(t *testing.T) {
	root := writeRecipeTree(t, map[string]string{
		"overlays/base.yaml": "kind: RecipeMetadata\napiVersion: aicr.run/v1alpha2\nmetadata:\n  name: base\nspec:\n  componentRefs: []\n",
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loadRecipeSources(ctx, root); err == nil {
		t.Fatal("loadRecipeSources succeeded with a pre-canceled context")
	}
}

func TestLoadRecipeSourcesRejectsDuplicateNames(t *testing.T) {
	// Two recursively discovered sources with one identity: declaration-level
	// scanning cannot know which duplicate "wins" during resolution, so the
	// ambiguity fails closed (stricter than the canonical store's map-order
	// behavior for overlays, deliberately).
	root := writeRecipeTree(t, nil)
	nested := filepath.Join(root, "recipes", "overlays", "team-a")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	overlay := "kind: RecipeMetadata\napiVersion: aicr.run/v1alpha2\nmetadata:\n  name: dup\nspec:\n  componentRefs: []\n"
	if err := os.WriteFile(filepath.Join(root, "recipes", "overlays", "dup.yaml"), []byte(overlay), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "dup.yaml"), []byte(overlay), 0o600); err != nil {
		t.Fatalf("write nested: %v", err)
	}
	if _, err := loadRecipeSources(context.Background(), root); err == nil {
		t.Fatal("loadRecipeSources accepted duplicate source identities")
	}
}

func TestLoadRecipeSourcesRejectsMixinUnknownField(t *testing.T) {
	// Canonical parity: the metadata store decodes mixins with
	// KnownFields(true), so a typo'd field (which could hide a divergent
	// pin) fails at recipe load — and must fail here identically, not
	// silently parse to an empty source.
	root := writeRecipeTree(t, map[string]string{
		"mixins/typo.yaml": `kind: RecipeMixin
apiVersion: aicr.run/v1alpha2
metadata:
  name: typo
spec:
  componentRef:
    - name: kube-prometheus-stack
      version: "83.7.0"
`,
	})
	if _, err := loadRecipeSources(context.Background(), root); err == nil {
		t.Fatal("loadRecipeSources accepted a mixin with an unknown field (canonical store rejects via KnownFields)")
	}
}
