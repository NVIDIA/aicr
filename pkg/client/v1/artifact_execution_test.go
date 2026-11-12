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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/helm/helmtest"
	"github.com/NVIDIA/aicr/pkg/mirror"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/recipe/ocisource"
	"github.com/NVIDIA/aicr/pkg/recipe/providertest"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
)

func artifactExecutionFixture(t *testing.T) (*Client, *recipe.RecipeResult, string) {
	t.Helper()
	dir := providertest.WriteDir(t, map[string][]byte{
		"registry.yaml": []byte(`apiVersion: aicr.run/v1beta1
kind: ComponentRegistry
components:
- name: fixture-runner
  class: ops
  helm:
    defaultRepository: https://old.example
    defaultChart: runner
    defaultVersion: v1.0.0
`),
		"components/fixture-runner/values.yaml": []byte("image: old.example/runner:old\n"),
		"artifacts/bindings.yaml": []byte(`apiVersion: aicr.run/v1beta1
kind: ArtifactBindings
charts:
  runner-chart: {component: fixture-runner}
images:
  runner-image:
    targets:
    - component: fixture-runner
      valuesFile: components/fixture-runner/values.yaml
      write: {reference: /image}
`),
		"artifacts/inventory.yaml": []byte(`apiVersion: aicr.run/v1beta1
kind: ArtifactInventory
artifacts:
- name: runner-chart
  type: chart
  reference: oci://new.example/charts/runner
  version: v2.0.0_build.1
  manifestDigest: sha256:` + strings.Repeat("a", 64) + `
  archiveDigest: sha256:` + strings.Repeat("b", 64) + `
- name: runner-image
  type: image
  reference: oci://new.example/images/runner
  version: latest
  manifestDigest: sha256:` + strings.Repeat("c", 64) + "\n"),
	})
	client, err := NewClientContext(t.Context(), WithRecipeSource(FilesystemSource(dir)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	})
	p, ok := client.dp.(*recipe.ArtifactProvider)
	if !ok {
		t.Fatal("inventory not activated")
	}
	rec := &recipe.RecipeResult{Kind: recipe.RecipeResultKind, APIVersion: "aicr.run/v1beta2", Metadata: recipe.RecipeResultMetadata{Artifacts: p.ArtifactProvenance()}, ComponentRefs: []recipe.ComponentRef{{Name: "fixture-runner", Type: recipe.ComponentTypeHelm, Source: "oci://new.example/charts", Chart: "runner", Version: "v2.0.0_build.1", Namespace: "runner", ValuesFile: "components/fixture-runner/values.yaml"}}, DeploymentOrder: []string{"fixture-runner"}}
	return client, rec, dir
}

func wantArtifactReason(t *testing.T, err error, reason string) {
	t.Helper()
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	if err == nil || !strings.Contains(err.Error(), reason+":") {
		t.Fatalf("reason %s: %v", reason, err)
	}
}

func TestArtifactExecutionConsumers(t *testing.T) {
	active, rec, dir := artifactExecutionFixture(t)
	path := filepath.Join(t.TempDir(), "recipe.json")
	writeRecipe := func(r *recipe.RecipeResult) {
		t.Helper()
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeRecipe(rec)
	otherVersion, setupErr := NewClientContext(t.Context(), WithRecipeSource(FilesystemSource(dir)), WithVersion("different-binary-version"))
	if setupErr != nil {
		t.Fatal(setupErr)
	}
	if _, err := otherVersion.LoadRecipe(t.Context(), path, ""); err != nil {
		t.Fatalf("binary version changed the input identity: %v", err)
	}
	if err := otherVersion.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := active.LoadRecipe(t.Context(), path, ""); err != nil {
		t.Fatalf("matching reload: %v", err)
	}
	if _, err := active.AdoptRecipe(t.Context(), rec); err != nil {
		t.Fatalf("matching adoption: %v", err)
	}
	plain, setupErr := NewClientContext(t.Context(), WithRecipeSource(EmbeddedSource()))
	if setupErr != nil {
		t.Fatal(setupErr)
	}
	defer func() {
		if err := plain.Close(); err != nil {
			t.Error(err)
		}
	}()
	for _, tt := range []struct {
		name   string
		run    func() error
		reason string
	}{
		{"missing inputs", func() error { _, err := plain.LoadRecipe(t.Context(), path, ""); return err }, "artifact-inputs-required"},
		{"server adoption", func() error { _, err := plain.AdoptRecipe(t.Context(), rec); return err }, "artifact-inputs-required"},
		{"inventory upgrade", func() error { _, err := active.UpgradeCheck(t.Context(), UpgradeCheckRequest{From: path}); return err }, "artifact-upgrade-unsupported"},
		{"upgrade source", func() error { _, err := plain.UpgradeCheck(t.Context(), UpgradeCheckRequest{From: path}); return err }, "artifact-upgrade-unsupported"},
	} {
		t.Run(tt.name, func(t *testing.T) { wantArtifactReason(t, tt.run(), tt.reason) })
	}
	legacy := rec.DeepCopy()
	legacy.Metadata.Artifacts = nil
	legacy.APIVersion = "aicr.run/v1"
	writeRecipe(legacy)
	_, setupErr = active.LoadRecipe(t.Context(), path, "")
	wantArtifactReason(t, setupErr, "artifact-provenance-required")
	_, setupErr = plain.UpgradeCheck(t.Context(), UpgradeCheckRequest{From: path, To: func() string {
		p := filepath.Join(t.TempDir(), "target.json")
		data, _ := json.Marshal(rec)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}()})
	wantArtifactReason(t, setupErr, "artifact-upgrade-unsupported")
	changed := rec.DeepCopy()
	changed.Metadata.Artifacts.InventoryDigest = "sha256:" + strings.Repeat("d", 64)
	writeRecipe(changed)
	_, setupErr = active.LoadRecipe(t.Context(), path, "")
	wantArtifactReason(t, setupErr, "artifact-inputs-mismatch")
	conflict := rec.DeepCopy()
	conflict.ComponentRefs[0].Version = "v3.0.0"
	_, setupErr = active.AdoptRecipe(t.Context(), conflict)
	wantArtifactReason(t, setupErr, "artifact-identity-conflict")
	// Change only the source discriminator after construction to exercise the
	// unsupported consumer without network access or a materialized OCI source.
	plain.source.kind = sourceKindOCI
	writeRecipe(rec)
	_, setupErr = plain.LoadRecipe(t.Context(), path, "")
	wantArtifactReason(t, setupErr, "artifact-source-unsupported")
	_, setupErr = plain.AdoptRecipe(t.Context(), rec)
	wantArtifactReason(t, setupErr, "artifact-source-unsupported")
}

func TestArtifactNamespaceInheritance(t *testing.T) {
	client, rec, _ := artifactExecutionFixture(t)
	old := rec.DeepCopy()
	old.Metadata.Artifacts.InventoryDigest = "sha256:" + strings.Repeat("e", 64)
	old.ComponentRefs[0].Namespace = "prior-namespace"
	old.ComponentRefs[0].ValuesFile = "missing.yaml"
	path := filepath.Join(t.TempDir(), "prior.json")
	write := func() {
		data, err := json.Marshal(old)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write()
	mode := recipe.InheritNamespace
	if err := client.inheritIdentity(t.Context(), path, &mode, client.dp, rec); err != nil {
		t.Fatal(err)
	}
	if rec.ComponentRefs[0].Namespace != "prior-namespace" || rec.ComponentRefs[0].ValuesFile != "components/fixture-runner/values.yaml" {
		t.Fatal("namespace extraction imported other identity")
	}
	mode = recipe.InheritAll
	wantArtifactReason(t, client.inheritIdentity(t.Context(), path, &mode, client.dp, rec), "artifact-inputs-mismatch")
	for _, tt := range []struct {
		name   string
		mutate func()
	}{
		{"duplicate", func() { old.ComponentRefs = append(old.ComponentRefs, old.ComponentRefs[0]) }},
		{"namespace", func() { old.ComponentRefs = old.ComponentRefs[:1]; old.ComponentRefs[0].Namespace = "INVALID SPACE" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			tt.mutate()
			write()
			_, err := readInheritanceSource(t.Context(), path, recipe.InheritNamespace)
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}
}

func TestArtifactFinalConsumerGuards(t *testing.T) {
	client, rec, _ := artifactExecutionFixture(t)
	adopted, adoptErr := client.AdoptRecipe(t.Context(), rec)
	if adoptErr != nil {
		t.Fatal(adoptErr)
	}
	bound := adopted.Resolved()
	renderer := &helmtest.MockRenderer{}
	if _, err := mirror.NewLister(mirror.WithHelmRenderer(renderer)).Discover(t.Context(), bound); err != nil {
		t.Fatal(err)
	}
	if len(renderer.Inputs) != 1 {
		t.Fatalf("render calls: %d", len(renderer.Inputs))
	}
	expected := "new.example/images/runner@sha256:" + strings.Repeat("c", 64)
	if renderer.Inputs[0].Values["image"] != expected {
		t.Fatalf("renderer received wrong identity: %v", renderer.Inputs[0].Values)
	}
	renderer.Inputs = nil
	overrideValue := "unbound.example/runner:latest"
	overrides := map[string]map[string]string{"fixture-runner": {"image": "unbound.example/runner:latest"}}
	_, adoptErr = mirror.NewLister(mirror.WithHelmRenderer(renderer), mirror.WithValueOverrides([]config.ComponentPath{{Component: "fixture-runner", Path: "image", Value: &overrideValue}})).Discover(t.Context(), bound)
	wantArtifactReason(t, adoptErr, "artifact-identity-conflict")
	if len(renderer.Inputs) != 0 {
		t.Fatal("conflicting identity reached renderer")
	}
	destination := filepath.Join(t.TempDir(), "rejected")
	_, adoptErr = client.MakeBundle(t.Context(), adopted, BundleOptions{Config: config.NewConfig(config.WithDeployer(config.DeployerHelm), config.WithValueOverrides(overrides)), OutputDir: destination})
	wantArtifactReason(t, adoptErr, "artifact-identity-conflict")
	if entries, readErr := os.ReadDir(destination); readErr == nil && len(entries) > 0 {
		t.Fatal("rejected bundle wrote output")
	}
	bound.ComponentRefs[0].Overrides = map[string]any{"image": "unbound.example/runner:latest"}
	_, adoptErr = validatorv1.ToValidationInputWithContext(t.Context(), bound)
	wantArtifactReason(t, adoptErr, "artifact-identity-conflict")
}

func TestArtifactOCICatalogInventory(t *testing.T) {
	for _, present := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary catalog", true: "inventory catalog"}[present], func(t *testing.T) {
			files := map[string][]byte{}
			if present {
				files["artifacts/inventory.yaml"] = []byte("must not be interpreted")
			}
			deps := defaultClientDependencies()
			deps.newOCIProvider = func(context.Context, *recipe.EmbeddedDataProvider, ocisource.Config) (recipe.DataProvider, error) {
				return providertest.NewMap(files), nil
			}
			client, err := newClientWithContextAndDependencies(t.Context(), deps, WithRecipeSource(OCISource("example.test/catalog", "sha256:"+strings.Repeat("a", 64))))
			if present {
				wantArtifactReason(t, err, "artifact-source-unsupported")
				if client != nil {
					t.Fatal("inventory OCI client escaped")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestArtifactBundleFacadeMutation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*RecipeResult)
	}{
		{"version", func(r *RecipeResult) { r.Components[0].Version = "unbound" }},
		{"source", func(r *RecipeResult) { r.Components[0].Source = "https://unbound.test" }},
		{"chart", func(r *RecipeResult) { r.Components[0].Chart = "unbound" }},
		{"kind", func(r *RecipeResult) { r.Components[0].Kind = "Kustomize" }},
		{"duplicate", func(r *RecipeResult) { r.Components = append(r.Components, r.Components[0]) }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client, rec, _ := artifactExecutionFixture(t)
			adopted, adoptErr := client.AdoptRecipe(t.Context(), rec)
			if adoptErr != nil {
				t.Fatal(adoptErr)
			}
			tt.mutate(adopted)
			_, bundleErr := client.BundleComponents(t.Context(), adopted)
			wantArtifactReason(t, bundleErr, "artifact-identity-conflict")
		})
	}
}
