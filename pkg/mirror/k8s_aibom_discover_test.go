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

package mirror

import (
	"context"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/helm/helmtest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestK8sAIBOM_MirrorDiscoversQualifiedImage covers the AICR-owned
// half of ADR-019's air-gap requirement.
//
// The requirement reads "aicr mirror list discovers exactly
// ghcr.io/googlecloudplatform/k8s-aibom@sha256:...". That claim decomposes:
// AICR is responsible for hydrating the component's values file and handing
// the renderer the qualified, digest-pinned coordinates; the upstream chart is
// responsible for turning those values into an image reference. Only the first
// half is AICR's to test, and only the first half can be tested without
// pulling the real chart over the network.
//
// The assertions therefore split in two. First, the ChartInput the Lister
// actually built: if the values file stops being hydrated, the registry
// coordinates drift, or the digest stops reaching the renderer, that fails.
// Second, the images extracted from rendered output: the renderer is stubbed
// with a manifest that references the qualified digest, standing in for what
// the upstream chart emits, so the extraction, dedup, and formatting AICR owns
// are exercised end to end and the exact reference is pinned.
//
// Asserting only the stub's own output would prove nothing, which is why the
// input assertion is not optional: together they show AICR hands over the
// right coordinates AND turns the resulting manifest into the right image
// list. That the real chart emits this image for these values is upstream's
// contract, verified against the live chart by tools/k8s-aibom-test.
func TestK8sAIBOM_MirrorDiscoversQualifiedImage(t *testing.T) {
	ctx := context.Background()

	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	component := registry.Get("k8s-aibom")
	if component == nil {
		t.Fatal("k8s-aibom is not in the component registry")
	}

	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	rec := &recipe.RecipeResult{
		Kind:       recipe.RecipeResultKind,
		APIVersion: "aicr.run/v1alpha2",
		ComponentRefs: []recipe.ComponentRef{{
			Name:       "k8s-aibom",
			Namespace:  component.Helm.DefaultNamespace,
			Type:       recipe.ComponentTypeHelm,
			Source:     component.Helm.DefaultRepository,
			Chart:      component.Helm.DefaultChart,
			Version:    component.Helm.DefaultVersion,
			ValuesFile: "components/k8s-aibom/values.yaml",
		}},
	}
	rec.BindDataProvider(provider)

	wantImage := qualifiedAIBOMImage(t, provider)
	renderer := &helmtest.MockRenderer{
		Rendered: map[string][]byte{
			"k8s-aibom": []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
  name: k8s-aibom
  namespace: k8s-aibom-system
spec:
  template:
    spec:
      containers:
        - name: manager
          image: ` + wantImage + `
`),
		},
	}
	lister := NewLister(WithHelmRenderer(renderer), WithVersion("v1.0.0"))

	list, err := lister.Discover(ctx, rec)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// Exactly one chart, at the registry's qualified coordinates. Asserting
	// the count (not just "a matching entry exists") is what makes a stray
	// second chart a failure rather than a silent pass.
	if len(list.Charts) != 1 {
		t.Fatalf("discovered %d charts, want exactly 1: %+v", len(list.Charts), list.Charts)
	}
	chart := list.Charts[0]
	if chart.Repository != component.Helm.DefaultRepository ||
		chart.Chart != component.Helm.DefaultChart ||
		chart.Version != component.Helm.DefaultVersion {

		t.Errorf("chart = %+v, want repository=%s chart=%s version=%s",
			chart, component.Helm.DefaultRepository, component.Helm.DefaultChart,
			component.Helm.DefaultVersion)
	}

	if len(renderer.Inputs) != 1 {
		t.Fatalf("renderer received %d inputs, want exactly 1", len(renderer.Inputs))
	}
	input := renderer.Inputs[0]
	if input.Chart != component.Helm.DefaultChart || input.Repository != component.Helm.DefaultRepository {
		t.Errorf("ChartInput chart/repository = %s/%s, want %s/%s",
			input.Chart, input.Repository, component.Helm.DefaultChart, component.Helm.DefaultRepository)
	}

	// Exactly one image, digest-pinned, at the qualified reference. Asserting
	// the whole slice (not "contains") is what makes a stray extra image, a
	// tag-only reference, or an empty list a failure.
	if len(list.Images) != 1 || list.Images[0] != wantImage {
		t.Errorf("discovered images = %v, want exactly [%s]", list.Images, wantImage)
	}
	if len(list.Components) != 1 {
		t.Fatalf("discovered %d components, want 1: %+v", len(list.Components), list.Components)
	}
	if got := list.Components[0].Images; len(got) != 1 || got[0] != wantImage {
		t.Errorf("component images = %v, want exactly [%s]", got, wantImage)
	}

	// The digest must survive values hydration and reach the renderer. This is
	// the load-bearing assertion: an unhydrated values file, or a values file
	// that lost its digest pin, would leave the mirror inventory referencing a
	// floating tag and silently break air-gap relocation.
	wantDigest := qualifiedAIBOMDigest(t, provider)
	image, ok := input.Values["image"].(map[string]any)
	if !ok {
		t.Fatalf("ChartInput.Values has no image mapping: %+v", input.Values)
	}
	if got := image["digest"]; got != wantDigest {
		t.Errorf("ChartInput.Values.image.digest = %v, want %s", got, wantDigest)
	}
}

// qualifiedAIBOMImage returns the full repository@digest reference the mirror
// inventory must carry, assembled from the component values file so
// requalification updates one file rather than one file plus this test.
func qualifiedAIBOMImage(t *testing.T, provider recipe.DataProvider) string {
	t.Helper()
	return qualifiedAIBOMRepository(t, provider) + "@" + qualifiedAIBOMDigest(t, provider)
}

// qualifiedAIBOMRepository reads image.repository from the component values.
func qualifiedAIBOMRepository(t *testing.T, provider recipe.DataProvider) string {
	t.Helper()
	raw, err := provider.ReadFile(context.Background(), "components/k8s-aibom/values.yaml")
	if err != nil {
		t.Fatalf("read k8s-aibom values: %v", err)
	}
	var values struct {
		Image struct {
			Repository string `yaml:"repository"`
		} `yaml:"image"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse k8s-aibom values: %v", err)
	}
	if values.Image.Repository == "" {
		t.Fatal("k8s-aibom values.yaml has no image.repository")
	}
	return values.Image.Repository
}

// qualifiedAIBOMDigest reads the pinned digest from the component values file,
// so requalification updates one file rather than one file plus this test.
func qualifiedAIBOMDigest(t *testing.T, provider recipe.DataProvider) string {
	t.Helper()
	raw, err := provider.ReadFile(context.Background(), "components/k8s-aibom/values.yaml")
	if err != nil {
		t.Fatalf("read k8s-aibom values: %v", err)
	}
	var values struct {
		Image struct {
			Digest string `yaml:"digest"`
		} `yaml:"image"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse k8s-aibom values: %v", err)
	}
	if values.Image.Digest == "" {
		t.Fatal("k8s-aibom values.yaml has no image.digest")
	}
	return values.Image.Digest
}
