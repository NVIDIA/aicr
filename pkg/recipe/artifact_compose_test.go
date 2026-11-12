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

package recipe

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/artifact"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"gopkg.in/yaml.v3"
)

func artifactCatalogFixture() (map[string][]byte, []byte) {
	files := map[string][]byte{
		"registry.yaml": []byte(`apiVersion: aicr.run/v1beta1
kind: ComponentRegistry
components:
- name: runner
  class: ops
  ownsCRDs: true
  helm:
    defaultRepository: https://old.test
    defaultChart: old/runner
    defaultVersion: v1.0.0
- name: other
  class: ops
  ownsCRDs: true
  helm:
    defaultRepository: https://old.test
    defaultChart: other
    defaultVersion: v1.0.0
`),
		"overlays/runner.yaml": []byte(`apiVersion: aicr.run/v1beta1
kind: RecipeMetadata
metadata: {name: runner}
spec:
  componentRefs:
  - name: runner
    source: https://old.test
    chart: runner
    version: v1.0.0
    valuesFile: components/runner/values.yaml
`),
		"components/runner/values.yaml": []byte("image:\n  repository: old.test/runner\n  digest: old\n  tag: old\ncontroller:\n  image: old.test/runner:old\n"),
		artifactBindingsPath: []byte(`apiVersion: aicr.run/v1beta1
kind: ArtifactBindings
charts:
  runner-chart: {component: runner}
  absent-chart: {component: stale-component}
images:
  runner-image:
    targets:
    - component: runner
      valuesFile: components/runner/values.yaml
      write: {repository: /image/repository, digest: /image/digest, clearTag: /image/tag}
  absent-image:
    targets:
    - component: stale-component
      valuesFile: absent.yaml
      write: {reference: /not-present}
`),
	}
	inventory := []byte(`apiVersion: aicr.run/v1beta1
kind: ArtifactInventory
artifacts:
- name: runner-chart
  type: chart
  reference: oci://new.test/charts/runner
  version: v2.0.0_build.1
  manifestDigest: sha256:` + strings.Repeat("a", 64) + `
  archiveDigest: sha256:` + strings.Repeat("b", 64) + `
- name: runner-image
  type: image
  reference: oci://new.test/images/runner
  version: release-2
  manifestDigest: sha256:` + strings.Repeat("c", 64) + "\n")
	return files, inventory
}

func TestArtifactComposition(t *testing.T) {
	files, inventory := artifactCatalogFixture()
	original := string(files["components/runner/values.yaml"])
	p, composeErr := composeCapturedArtifacts(t.Context(), map[string][]byte{}, files, inventory, artifact.Options{})
	if composeErr != nil {
		t.Fatal(composeErr)
	}
	data, composeErr := p.ReadFile(t.Context(), "registry.yaml")
	if composeErr != nil {
		t.Fatal(composeErr)
	}
	var registry ComponentRegistry
	if err := yaml.Unmarshal(data, &registry); err != nil {
		t.Fatal(err)
	}
	got := registry.Components[0].Helm
	if got.DefaultRepository != "oci://new.test/charts" || got.DefaultChart != "runner" || got.DefaultVersion != "v2.0.0_build.1" || got.DefaultDigest != "sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("wrong chart: %+v", got)
	}
	for _, component := range registry.Components {
		if component.OwnsCRDs {
			t.Fatal("ownership survived changed chart")
		}
	}
	values, composeErr := p.ReadFile(t.Context(), "components/runner/values.yaml")
	if composeErr != nil {
		t.Fatal(composeErr)
	}
	if !strings.Contains(string(values), "new.test/images/runner") || !strings.Contains(string(values), `tag: ""`) {
		t.Fatalf("wrong values: %s", values)
	}
	overlay, composeErr := p.ReadFile(t.Context(), "overlays/runner.yaml")
	if composeErr != nil {
		t.Fatal(composeErr)
	}
	if !strings.Contains(string(overlay), "oci://new.test/charts") || !strings.Contains(string(overlay), "v2.0.0_build.1") {
		t.Fatalf("default restatement not rebound: %s", overlay)
	}
	if string(files["components/runner/values.yaml"]) != original {
		t.Fatal("raw snapshot mutated")
	}
	inputs := p.ArtifactInputs()
	if string(inputs["components/runner/values.yaml"]) != original || string(inputs[artifactInventoryPath]) != string(inventory) {
		t.Fatal("bundle inputs differ from retained bytes")
	}
	values[0] = 'x'
	fresh, composeErr := p.ReadFile(t.Context(), "components/runner/values.yaml")
	if composeErr != nil {
		t.Fatal(composeErr)
	}
	if fresh[0] == 'x' {
		t.Fatal("ReadFile exposed mutable storage")
	}
	if p.provenance.Verification != "metadata-only" || len(p.provenance.Applied) != 2 {
		t.Fatalf("bad provenance: %+v", p.provenance)
	}
}

func TestArtifactActiveTargets(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string][]byte, *[]byte)
		valid  bool
	}{
		{"inactive stale target", func(map[string][]byte, *[]byte) {}, true},
		{"missing active file", func(files map[string][]byte, _ *[]byte) { delete(files, "components/runner/values.yaml") }, false},
		{"nonstring leaf", func(files map[string][]byte, _ *[]byte) {
			files["components/runner/values.yaml"] = []byte("image: {repository: 12, digest: old, tag: old}")
		}, false},
		{"array traversal", func(files map[string][]byte, _ *[]byte) {
			files["components/runner/values.yaml"] = []byte("image: [a,b]")
		}, false},
		{"overlapping writes", func(files map[string][]byte, _ *[]byte) {
			files[artifactBindingsPath] = []byte(strings.Replace(string(files[artifactBindingsPath]), "clearTag: /image/tag", "clearTag: /image/digest", 1))
		}, false},
		{"missing active component", func(files map[string][]byte, _ *[]byte) {
			files[artifactBindingsPath] = []byte(strings.Replace(string(files[artifactBindingsPath]), "{component: runner}", "{component: missing}", 1))
		}, false},
		{"asserted platform missing", func(files map[string][]byte, _ *[]byte) {
			files[artifactBindingsPath] = []byte(strings.Replace(string(files[artifactBindingsPath]), "    - component: runner", "    - component: runner\n      platform: {os: linux, architecture: amd64}", 1))
		}, false},
		{"matching explicit platform", func(files map[string][]byte, inventory *[]byte) {
			files[artifactBindingsPath] = []byte(strings.Replace(string(files[artifactBindingsPath]), "    - component: runner", "    - component: runner\n      platform: {os: linux, architecture: arm64, variant: v8}", 1))
			*inventory = []byte(strings.Replace(string(*inventory), "  version: release-2", "  platform: {os: linux, architecture: arm64, variant: v8}\n  version: release-2", 1))
		}, true},
		{"platform variant mismatch", func(files map[string][]byte, inventory *[]byte) {
			files[artifactBindingsPath] = []byte(strings.Replace(string(files[artifactBindingsPath]), "    - component: runner", "    - component: runner\n      platform: {os: linux, architecture: arm64}", 1))
			*inventory = []byte(strings.Replace(string(*inventory), "  version: release-2", "  platform: {os: linux, architecture: arm64, variant: v8}\n  version: release-2", 1))
		}, false},
		{"GPU operator image layout", func(files map[string][]byte, _ *[]byte) {
			files["registry.yaml"] = []byte(strings.Replace(string(files["registry.yaml"]), "name: runner", "name: gpu-operator", 1))
			files[artifactBindingsPath] = []byte(strings.ReplaceAll(string(files[artifactBindingsPath]), "component: runner", "component: gpu-operator"))
		}, false},
		{"Network operator image layout", func(files map[string][]byte, _ *[]byte) {
			files["registry.yaml"] = []byte(strings.Replace(string(files["registry.yaml"]), "name: runner", "name: network-operator", 1))
			files[artifactBindingsPath] = []byte(strings.ReplaceAll(string(files[artifactBindingsPath]), "component: runner", "component: network-operator"))
		}, false},
		{"unbound artifact", func(_ map[string][]byte, inventory *[]byte) {
			*inventory = []byte(strings.Replace(string(*inventory), "name: runner-image", "name: misspelled-image", 1))
		}, false},
		{"chart without archive", func(_ map[string][]byte, inventory *[]byte) {
			*inventory = []byte(strings.Replace(string(*inventory), "  archiveDigest: sha256:"+strings.Repeat("b", 64)+"\n", "", 1))
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files, inventory := artifactCatalogFixture()
			tt.mutate(files, &inventory)
			_, err := composeCapturedArtifacts(t.Context(), map[string][]byte{}, files, inventory, artifact.Options{})
			if tt.valid {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			}
		})
	}
}

func writeArtifactCatalog(t *testing.T, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range files {
		filename := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestArtifactActivationAndSnapshot(t *testing.T) {
	files, inventory := artifactCatalogFixture()
	dir := writeArtifactCatalog(t, files)
	provider, providerErr := NewLayeredDataProvider(NewEmbeddedDataProvider(GetEmbeddedFS(), ""), LayeredProviderConfig{ExternalDir: dir, ExternalOnly: true})
	if providerErr != nil {
		t.Fatal(providerErr)
	}
	got, providerErr := ComposeArtifacts(t.Context(), provider, artifact.Options{})
	if providerErr != nil {
		t.Fatal(providerErr)
	}
	if got != provider {
		t.Fatal("no inventory changed provider")
	}
	// A malformed binding is ignored until an inventory activates composition.
	if err := os.WriteFile(filepath.Join(dir, artifactBindingsPath), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ComposeArtifacts(t.Context(), provider, artifact.Options{}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, artifactBindingsPath), files[artifactBindingsPath], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, artifactInventoryPath), inventory, 0o600); err != nil {
		t.Fatal(err)
	}
	composed, providerErr := ComposeArtifacts(t.Context(), provider, artifact.Options{})
	if providerErr != nil {
		t.Fatal(providerErr)
	}
	if err := os.WriteFile(filepath.Join(dir, "components/runner/values.yaml"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, providerErr := composed.ReadFile(t.Context(), "components/runner/values.yaml")
	if providerErr != nil {
		t.Fatal(providerErr)
	}
	if !strings.Contains(string(data), "new.test/images/runner") {
		t.Fatal("read live directory")
	}
	snapshot := composed.(*ArtifactProvider)
	if string(snapshot.ArtifactInputs()["components/runner/values.yaml"]) != string(files["components/runner/values.yaml"]) {
		t.Fatal("bundle capture read live directory")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := composed.ReadFile(canceled, "registry.yaml"); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestArtifactDigests(t *testing.T) {
	files, inventory := artifactCatalogFixture()
	first, err := composeCapturedArtifacts(t.Context(), map[string][]byte{}, files, inventory, artifact.Options{})
	if err != nil {
		t.Fatal(err)
	}
	files[artifactBindingsPath] = append([]byte("# formatting\n"), files[artifactBindingsPath]...)
	second, err := composeCapturedArtifacts(t.Context(), map[string][]byte{}, files, inventory, artifact.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if first.provenance.BindingsDigest != second.provenance.BindingsDigest || first.provenance.BaseCatalogDigest != second.provenance.BaseCatalogDigest {
		t.Fatal("bindings formatting affected canonical digest")
	}
	files["unused.yaml"] = []byte("test: value\n")
	third, err := composeCapturedArtifacts(t.Context(), map[string][]byte{}, files, inventory, artifact.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if first.provenance.BaseCatalogDigest == third.provenance.BaseCatalogDigest {
		t.Fatal("unselected file omitted from digest")
	}
}

func TestStaticArtifactValidation(t *testing.T) {
	tests := []struct {
		name      string
		inventory bool
		modify    func(map[string][]byte)
		wantError bool
	}{
		{"without inventory", false, func(map[string][]byte) {}, false},
		{"schema without inventory", false, func(files map[string][]byte) { files[artifactBindingsPath] = []byte("kind: invalid\n") }, true},
		{"partial inventory", true, func(map[string][]byte) {}, false},
		{"accounting", true, func(files map[string][]byte) {
			files[artifactBindingsPath] = []byte("apiVersion: aicr.run/v1beta1\nkind: ArtifactBindings\ncharts: {}\nimages: {}\n")
		}, true},
		{"active target", true, func(files map[string][]byte) { delete(files, "components/runner/values.yaml") }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files, inventory := artifactCatalogFixture()
			tt.modify(files)
			if tt.inventory {
				files[artifactInventoryPath] = inventory
			}
			dir := writeArtifactCatalog(t, files)
			provider, err := NewLayeredDataProvider(NewEmbeddedDataProvider(GetEmbeddedFS(), ""), LayeredProviderConfig{ExternalDir: dir, ExternalOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			err = validateStaticArtifacts(t.Context(), provider, artifact.Options{})
			if tt.wantError {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	files, inventory := artifactCatalogFixture()
	for name, data := range map[string][]byte{"inventory.yaml": inventory, "bindings.yaml": files[artifactBindingsPath]} {
		t.Run("standalone "+name, func(t *testing.T) {
			dir := writeArtifactCatalog(t, map[string][]byte{name: data})
			diagnostics, err := ValidateStatic(t.Context(), filepath.Join(dir, name), "")
			if err != nil {
				t.Fatal(err)
			}
			if len(diagnostics) != 1 || diagnostics[0].Severity != "info" || !strings.Contains(diagnostics[0].Message, "schema") {
				t.Fatalf("standalone validation must distinguish schema-only result: %+v", diagnostics)
			}
		})
	}
}

func TestArtifactExceptionsAndFullReference(t *testing.T) {
	files, inventory := artifactCatalogFixture()
	tests := []struct {
		name       string
		exceptions []string
		valid      bool
	}{
		{"none", nil, true}, {"known", []string{"other", "runner"}, true}, {"unknown", []string{"missing"}, false}, {"duplicate", []string{"runner", "runner"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := composeCapturedArtifacts(t.Context(), map[string][]byte{}, files, inventory, artifact.Options{AllowCatalogPins: tt.exceptions})
			if tt.valid {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			}
		})
	}
	files[artifactBindingsPath] = []byte(strings.Replace(string(files[artifactBindingsPath]), "{repository: /image/repository, digest: /image/digest, clearTag: /image/tag}", "{reference: /controller/image}", 1))
	p, err := composeCapturedArtifacts(t.Context(), map[string][]byte{}, files, inventory, artifact.Options{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := p.ReadFile(t.Context(), "components/runner/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "new.test/images/runner@sha256:"+strings.Repeat("c", 64)) {
		t.Fatalf("full reference not bound: %s", data)
	}
}

func TestArtifactCoverageAndIdentityGuards(t *testing.T) {
	files, inventory := artifactCatalogFixture()
	p, composeErr := composeCapturedArtifacts(t.Context(), map[string][]byte{}, files, inventory, artifact.Options{})
	if composeErr != nil {
		t.Fatal(composeErr)
	}
	ref := ComponentRef{Name: "runner", Type: ComponentTypeHelm, Source: "oci://new.test/charts", Chart: "runner", Version: "v2.0.0_build.1", ValuesFile: "components/runner/values.yaml"}
	tests := []struct {
		name               string
		ref                ComponentRef
		coverage, identity bool
	}{
		{"matching", ref, true, true},
		{"uncovered", ComponentRef{Name: "other", Type: ComponentTypeHelm}, false, true},
		{"chart conflict", func() ComponentRef { copyRef := ref; copyRef.Source = "https://old.test"; return copyRef }(), true, false},
		{"build metadata conflict", func() ComponentRef { copyRef := ref; copyRef.Version = "v2.0.0_build.2"; return copyRef }(), true, false},
		{"missing participation", func() ComponentRef { copyRef := ref; copyRef.ValuesFile = ""; return copyRef }(), false, false},
		{"image override", func() ComponentRef {
			copyRef := ref
			copyRef.Overrides = map[string]any{"image": map[string]any{"repository": "changed"}}
			return copyRef
		}(), true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := p.CheckSelection(t.Context(), []ComponentRef{tt.ref}); (err == nil) != tt.coverage {
				t.Fatalf("coverage: %v", err)
			}
			if err := p.CheckIdentities(t.Context(), []ComponentRef{tt.ref}, nil, nil); (err == nil) != tt.identity {
				t.Fatalf("identity: %v", err)
			}
		})
	}
	if err := p.CheckIdentities(t.Context(), []ComponentRef{ref}, nil, map[string][]string{"runner": {"image"}}); err == nil {
		t.Fatal("dynamic parent accepted")
	}
	partial := []byte(strings.Split(string(inventory), "- name: runner-image")[0])
	p, composeErr = composeCapturedArtifacts(t.Context(), map[string][]byte{}, files, partial, artifact.Options{})
	if composeErr != nil {
		t.Fatal(composeErr)
	}
	if err := p.CheckSelection(t.Context(), []ComponentRef{ref}); err == nil {
		t.Fatal("partial image coverage accepted")
	}
	p, composeErr = composeCapturedArtifacts(t.Context(), map[string][]byte{}, files, partial, artifact.Options{AllowCatalogPins: []string{"runner"}})
	if composeErr != nil {
		t.Fatal(composeErr)
	}
	if err := p.CheckSelection(t.Context(), []ComponentRef{ref}); err != nil {
		t.Fatal(err)
	}
	ref.Source = "https://old.test"
	if err := p.CheckIdentities(t.Context(), []ComponentRef{ref}, nil, nil); err == nil {
		t.Fatal("exception waived active binding")
	}
}

func TestArtifactCanonicalJSON(t *testing.T) {
	got, err := canonicalArtifactDigest(map[string]any{"\ue000": "<>&", "😀": 1})
	if err != nil {
		t.Fatal(err)
	}
	want := digestArtifactBytes([]byte("{\"😀\":1,\"\ue000\":\"<>&\"}"))
	if got != want {
		t.Fatalf("digest is not JCS UTF-16 ordering and unescaped HTML: %s != %s", got, want)
	}
}

func TestArtifactSavedInputChecks(t *testing.T) {
	files, inventory := artifactCatalogFixture()
	p, err := composeCapturedArtifacts(t.Context(), map[string][]byte{}, files, inventory, artifact.Options{})
	if err != nil {
		t.Fatal(err)
	}
	changed := p.ArtifactProvenance()
	changed.InventoryDigest = "sha256:" + strings.Repeat("f", 64)
	tests := []struct {
		name     string
		record   *artifact.Provenance
		provider DataProvider
		reason   string
	}{
		{"legacy inactive", nil, nil, ""},
		{"legacy active", nil, p, "artifact-provenance-required"},
		{"missing inputs", p.ArtifactProvenance(), nil, "artifact-inputs-required"},
		{"matching", p.ArtifactProvenance(), p, ""},
		{"mismatch", changed, p, "artifact-inputs-mismatch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckArtifactInputs(tt.record, tt.provider)
			if tt.reason == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			if !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("wrong reason: %v", err)
			}
		})
	}
	copyRecord := p.ArtifactProvenance()
	copyRecord.Applied[1].Image.Targets[0].ValuesFile = "changed.yaml"
	if p.ArtifactProvenance().Applied[1].Image.Targets[0].ValuesFile == "changed.yaml" {
		t.Fatal("provenance aliases provider")
	}
}

func TestArtifactStaticInputWithContext(t *testing.T) {
	files, inventory := artifactCatalogFixture()
	dir := writeArtifactCatalog(t, files)
	input := filepath.Join(t.TempDir(), "inventory.yaml")
	if err := os.WriteFile(input, []byte(strings.Replace(string(inventory), "runner-chart", "unbound-chart", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	findings, err := ValidateStatic(t.Context(), input, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Severity != "error" || !strings.Contains(findings[0].Message, "artifact-unbound") {
		t.Fatalf("inventory with catalog context did not check accounting: %+v", findings)
	}
}

func TestArtifactBindingLayerReplacement(t *testing.T) {
	embedded, inventory := artifactCatalogFixture()
	external := map[string][]byte{artifactBindingsPath: []byte(`apiVersion: aicr.run/v1beta1
kind: ArtifactBindings
charts: {}
images: {}
ignoredArtifacts:
  runner-image: {reason: Image remains managed by catalog policy}
`)}
	p, err := composeCapturedArtifacts(t.Context(), embedded, external, inventory, artifact.Options{AllowCatalogPins: []string{"runner"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.provenance.Ignored) != 1 || len(p.provenance.Applied) != 1 {
		t.Fatalf("not replaced by artifact name: %+v", p.provenance)
	}
	if _, ok := p.bindings.Charts["absent-chart"]; !ok {
		t.Fatal("unrelated embedded binding lost")
	}
	if _, ok := p.bindings.Images["runner-image"]; ok {
		t.Fatal("old disposition survived replacement")
	}
}

func TestArtifactSnapshotRejectsSymlink(t *testing.T) {
	files, inventory := artifactCatalogFixture()
	files[artifactInventoryPath] = inventory
	dir := writeArtifactCatalog(t, files)
	provider, providerErr := NewLayeredDataProvider(NewEmbeddedDataProvider(GetEmbeddedFS(), ""), LayeredProviderConfig{ExternalDir: dir, ExternalOnly: true})
	if providerErr != nil {
		t.Fatal(providerErr)
	}
	name := filepath.Join(dir, "components/runner/values.yaml")
	if err := os.Rename(name, name+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("values.yaml.original", name); err != nil {
		t.Fatal(err)
	}
	_, providerErr = ComposeArtifacts(t.Context(), provider, artifact.Options{})
	errorstest.WantReportedCode(t, providerErr, errors.ErrCodeInvalidRequest)
}

func TestArtifactInventoryOverrideCapture(t *testing.T) {
	files, inventory := artifactCatalogFixture()
	files["release.yaml"] = inventory
	files[artifactInventoryPath] = []byte("this default must not override the explicit choice")
	dir := writeArtifactCatalog(t, files)
	provider, err := NewLayeredDataProvider(NewEmbeddedDataProvider(GetEmbeddedFS(), ""), LayeredProviderConfig{ExternalDir: dir, ExternalOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	composed, err := ComposeArtifacts(t.Context(), provider, artifact.Options{Inventory: filepath.Join(dir, "release.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := composed.(*ArtifactProvider)
	if string(snapshot.ArtifactInputs()[artifactInventoryPath]) != string(inventory) {
		t.Fatal("explicit inventory bytes not retained")
	}
	if _, exists := snapshot.ArtifactInputs()["release.yaml"]; exists {
		t.Fatal("relocated inventory duplicated as an untracked base file")
	}
	replay, err := composeCapturedArtifacts(t.Context(), map[string][]byte{}, snapshot.ArtifactInputs(), inventory, artifact.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if replay.provenance.BaseCatalogDigest != snapshot.provenance.BaseCatalogDigest {
		t.Fatal("relocating the inventory changed base catalog identity")
	}
	hdr := &RecipeMetadataHeader{Kind: artifact.InventoryKind, APIVersion: artifact.APIVersion}
	if metadata, _, err := classifyRecipeMetadataCatalogHeader(hdr, "release.yaml"); err != nil || metadata {
		t.Fatalf("inventory treated as recipe metadata: %v", err)
	}
	excluded := artifactExcludedFiles(files, "release.yaml")
	if !excluded["release.yaml"] {
		t.Fatal("inventory-only file included in base digest")
	}
	files["overlays/uses-inventory.yaml"] = []byte("spec: {componentRefs: [{name: runner, valuesFile: release.yaml}]}")
	if artifactExcludedFiles(files, "release.yaml")["release.yaml"] {
		t.Fatal("catalog data file excluded from base digest")
	}
}

func TestArtifactRebindsCatalogDocumentsOutsideConventionalDirectories(t *testing.T) {
	files, inventory := artifactCatalogFixture()
	files["custom.yaml"] = files["overlays/runner.yaml"]
	delete(files, "overlays/runner.yaml")
	p, err := composeCapturedArtifacts(t.Context(), map[string][]byte{}, files, inventory, artifact.Options{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := p.ReadFile(t.Context(), "custom.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "oci://new.test/charts") || !strings.Contains(string(data), "v2.0.0_build.1") {
		t.Fatalf("catalog defaults were not rebound: %s", data)
	}
}

func TestArtifactCompositionPreservesJSONValues(t *testing.T) {
	files, inventory := artifactCatalogFixture()
	files["components/runner/values.json"] = []byte(`{"image":{"repository":"old.test/runner","digest":"old","tag":"old"}}`)
	files[artifactBindingsPath] = []byte(strings.ReplaceAll(string(files[artifactBindingsPath]), "components/runner/values.yaml", "components/runner/values.json"))
	p, err := composeCapturedArtifacts(t.Context(), map[string][]byte{}, files, inventory, artifact.Options{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := p.ReadFile(t.Context(), "components/runner/values.json")
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Fatalf("JSON values became a different format: %s", data)
	}
	if !strings.Contains(string(data), "new.test/images/runner") {
		t.Fatalf("binding was not applied: %s", data)
	}
}

func TestArtifactEmbeddedOnlyBindings(t *testing.T) {
	inventory := filepath.Join(t.TempDir(), "inventory.yaml")
	data := []byte("apiVersion: aicr.run/v1beta1\nkind: ArtifactInventory\nartifacts:\n- name: slurm-accounting-mariadb-image\n  type: image\n  reference: oci://example.test/mariadb\n  version: release\n  manifestDigest: sha256:" + strings.Repeat("a", 64) + "\n")
	if err := os.WriteFile(inventory, data, 0o600); err != nil {
		t.Fatal(err)
	}
	provider, err := ComposeArtifacts(t.Context(), NewEmbeddedDataProvider(GetEmbeddedFS(), ""), artifact.Options{Inventory: inventory})
	if err != nil {
		t.Fatal(err)
	}
	values, err := provider.ReadFile(t.Context(), "components/slurm-accounting-mariadb/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(values), "example.test/mariadb@sha256:"+strings.Repeat("a", 64)) {
		t.Fatalf("embedded image binding missing: %s", values)
	}
	record := provider.(*ArtifactProvider).ArtifactProvenance()
	if len(record.Applied) != 1 || record.Applied[0].Image == nil {
		t.Fatalf("wrong provenance: %+v", record)
	}
}

func TestArtifactCapturePreservesDualUseInputs(t *testing.T) {
	for _, name := range []string{artifactBindingsPath, artifactInventoryPath} {
		t.Run(name, func(t *testing.T) {
			files, inventory := artifactCatalogFixture()
			files[artifactInventoryPath] = append([]byte("# original default\n"), inventory...)
			files["selected.yaml"] = inventory
			files["overlays/dual-use.yaml"] = []byte("apiVersion: aicr.run/v1beta1\nkind: RecipeMetadata\nmetadata: {name: dual-use}\nspec:\n  componentRefs:\n  - name: other\n    valuesFile: " + name + "\n")
			original := string(files[name])
			p, err := composeCapturedArtifacts(t.Context(), map[string][]byte{}, files, inventory, artifact.Options{}, "selected.yaml")
			if err != nil {
				t.Fatal(err)
			}
			captured := p.ArtifactInputs()
			if string(captured[name]) != original {
				t.Fatalf("dual-use input bytes changed: %s", name)
			}
			replay, err := composeCapturedArtifacts(t.Context(), map[string][]byte{}, captured, captured[p.inventoryCapturePath], artifact.Options{}, p.inventoryCapturePath)
			if err != nil {
				t.Fatal(err)
			}
			if err := CheckArtifactInputs(p.ArtifactProvenance(), replay); err != nil {
				t.Fatalf("captured dual-use inputs cannot replay: %v", err)
			}
		})
	}
}

func TestArtifactExistingCapturePathReplay(t *testing.T) {
	files, inventory := artifactCatalogFixture()
	files[artifactInventoryPath] = append([]byte("# default\n"), inventory...)
	files["overlays/dual.yaml"] = []byte("spec: {componentRefs: [{name: other, valuesFile: artifacts/inventory.yaml}]}\n")
	capturePath := "artifacts/selected-inventory-" + strings.TrimPrefix(digestArtifactBytes(inventory), "sha256:") + ".yaml"
	files[capturePath] = inventory
	p, composeErr := composeCapturedArtifacts(t.Context(), map[string][]byte{}, files, inventory, artifact.Options{})
	if composeErr != nil {
		t.Fatal(composeErr)
	}
	replay, replayErr := composeCapturedArtifacts(t.Context(), map[string][]byte{}, p.ArtifactInputs(), inventory, artifact.Options{}, capturePath)
	if replayErr != nil {
		t.Fatal(replayErr)
	}
	if inputErr := CheckArtifactInputs(p.ArtifactProvenance(), replay); inputErr != nil {
		t.Fatal(inputErr)
	}
}

func TestArtifactRebindingRejectsTrailingDocuments(t *testing.T) {
	for _, tail := range []string{"---\n{}\n", "---\n", "---\ninvalid: ["} {
		t.Run(tail, func(t *testing.T) {
			files, inventory := artifactCatalogFixture()
			files["overlays/runner.yaml"] = append(files["overlays/runner.yaml"], []byte(tail)...)
			_, composeErr := composeCapturedArtifacts(t.Context(), map[string][]byte{}, files, inventory, artifact.Options{})
			errorstest.WantReportedCode(t, composeErr, errors.ErrCodeInvalidRequest)
		})
	}
}

func TestArtifactCapturedOriginsAndPaths(t *testing.T) {
	embedded, inventory := artifactCatalogFixture()
	embedded["overlays/profile.yaml"] = []byte(`apiVersion: aicr.run/v1beta2
kind: RecipeMetadata
metadata: {name: profile}
spec:
  profile:
    name: mode
    default: original
    values:
      original: {}
`)
	external := map[string][]byte{"overlays/profile.yaml": []byte(`apiVersion: aicr.run/v1beta2
kind: RecipeMetadata
metadata: {name: profile}
spec:
  profile:
    name: mode
    values:
      extra: {}
`)}
	p, composeErr := composeCapturedArtifacts(t.Context(), embedded, external, inventory, artifact.Options{})
	if composeErr != nil {
		t.Fatal(composeErr)
	}
	if p.Source("overlays/profile.yaml") != sourceEmbedded {
		t.Fatal("profile patch changed embedded origin")
	}
	for _, name := range []string{"./components/runner/values.yaml", "components/runner/../runner/values.yaml"} {
		if _, readErr := p.ReadFile(t.Context(), name); readErr != nil {
			t.Fatal(readErr)
		}
	}
}
