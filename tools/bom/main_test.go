// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/helm/helmtest"
)

// writeTestRegistry writes a minimal registry YAML to path and returns the
// repo root directory (parent of recipes/).
func writeTestRegistry(t *testing.T, content string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "recipes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir recipes: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "registry.yaml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write registry.yaml: %v", err)
	}
	return root
}

const testRegistryHelm = `apiVersion: v1
kind: ComponentRegistry
components:
  - name: gpu-operator
    displayName: GPU Operator
    helm:
      defaultRepository: "oci://ghcr.io/nvidia"
      defaultChart: gpu-operator
      defaultVersion: "25.3.0"
      defaultNamespace: gpu-operator
`

const testRegistryKustomize = `apiVersion: v1
kind: ComponentRegistry
components:
  - name: my-kustomize
    displayName: My Kustomize
    kustomize:
      defaultSource: "https://github.com/example/my-app"
      defaultPath: deploy
      defaultTag: v1.0.0
`

const testRegistryMixed = `apiVersion: v1
kind: ComponentRegistry
components:
  - name: gpu-operator
    displayName: GPU Operator
    helm:
      defaultRepository: "oci://ghcr.io/nvidia"
      defaultChart: gpu-operator
      defaultVersion: "25.3.0"
      defaultNamespace: gpu-operator
  - name: my-kustomize
    displayName: My Kustomize
    kustomize:
      defaultSource: "https://github.com/example/my-app"
      defaultPath: deploy
      defaultTag: v1.0.0
`

const testRegistryHelmUnpinned = `apiVersion: v1
kind: ComponentRegistry
components:
  - name: gpu-operator
    displayName: GPU Operator
    helm:
      defaultRepository: "oci://ghcr.io/nvidia"
      defaultChart: gpu-operator
      defaultNamespace: gpu-operator
`

// renderedYAML is a minimal Kubernetes manifest returned by the mock renderer.
const renderedYAML = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-operator
spec:
  template:
    spec:
      containers:
        - name: gpu-operator
          image: nvcr.io/nvidia/gpu-operator:v25.3.0
        - name: toolkit
          image: nvcr.io/nvidia/k8s/container-toolkit:v1.17.5-ubuntu22.04
`

func TestComponentKind(t *testing.T) {
	tests := []struct {
		name string
		comp component
		want string
	}{
		{
			name: "helm component",
			comp: component{
				Helm: helmCfg{DefaultRepository: "oci://ghcr.io/nvidia", DefaultChart: "gpu-operator"},
			},
			want: "helm",
		},
		{
			name: "helm component chart only",
			comp: component{
				Helm: helmCfg{DefaultChart: "gpu-operator"},
			},
			want: "helm",
		},
		{
			name: "kustomize component",
			comp: component{
				Kustomize: kustCfg{DefaultSource: "https://github.com/example/app"},
			},
			want: "kustomize",
		},
		{
			name: "manifest component",
			comp: component{
				Name: "bare-manifests",
			},
			want: "manifest",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.comp.kind()
			if got != tt.want {
				t.Errorf("kind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadRegistry(t *testing.T) {
	root := writeTestRegistry(t, testRegistryHelm)
	regPath := filepath.Join(root, "recipes", "registry.yaml")

	reg, err := loadRegistry(regPath)
	if err != nil {
		t.Fatalf("loadRegistry() error = %v", err)
	}
	if len(reg.Components) != 1 {
		t.Fatalf("expected 1 component, got %d", len(reg.Components))
	}
	if reg.Components[0].Name != "gpu-operator" {
		t.Errorf("component name = %q, want %q", reg.Components[0].Name, "gpu-operator")
	}
	if reg.Components[0].Helm.DefaultVersion != "25.3.0" {
		t.Errorf("default version = %q, want %q", reg.Components[0].Helm.DefaultVersion, "25.3.0")
	}
}

func TestLoadRegistryNotFound(t *testing.T) {
	_, err := loadRegistry("/nonexistent/registry.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestLoadRegistryInvalidYAML(t *testing.T) {
	root := writeTestRegistry(t, "not: [valid: yaml: {{")
	regPath := filepath.Join(root, "recipes", "registry.yaml")

	_, err := loadRegistry(regPath)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

func TestSurveyComponentHelm(t *testing.T) {
	root := writeTestRegistry(t, testRegistryHelm)
	mock := &helmtest.MockRenderer{
		Rendered: map[string][]byte{
			"gpu-operator": []byte(renderedYAML),
		},
	}

	c := component{
		Name:        "gpu-operator",
		DisplayName: "GPU Operator",
		Helm: helmCfg{
			DefaultRepository: "oci://ghcr.io/nvidia",
			DefaultChart:      "gpu-operator",
			DefaultVersion:    "25.3.0",
			DefaultNamespace:  "gpu-operator",
		},
	}

	res := surveyComponent(context.Background(), root, c, mock, false)
	if res.Name != "gpu-operator" {
		t.Errorf("Name = %q, want %q", res.Name, "gpu-operator")
	}
	if res.Type != "helm" {
		t.Errorf("Type = %q, want %q", res.Type, "helm")
	}
	if !res.Pinned {
		t.Error("expected Pinned = true")
	}
	if len(res.Images) != 2 {
		t.Fatalf("expected 2 images, got %d: %v", len(res.Images), res.Images)
	}
	// Images are sorted.
	if res.Images[0] != "nvcr.io/nvidia/gpu-operator:v25.3.0" {
		t.Errorf("Images[0] = %q, want %q", res.Images[0], "nvcr.io/nvidia/gpu-operator:v25.3.0")
	}
	if res.Images[1] != "nvcr.io/nvidia/k8s/container-toolkit:v1.17.5-ubuntu22.04" {
		t.Errorf("Images[1] = %q, want %q", res.Images[1], "nvcr.io/nvidia/k8s/container-toolkit:v1.17.5-ubuntu22.04")
	}
	if len(res.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", res.Warnings)
	}
}

func TestSurveyComponentSkipHelm(t *testing.T) {
	root := writeTestRegistry(t, testRegistryHelm)
	mock := &helmtest.MockRenderer{
		Rendered: map[string][]byte{
			"gpu-operator": []byte(renderedYAML),
		},
	}

	c := component{
		Name:        "gpu-operator",
		DisplayName: "GPU Operator",
		Helm: helmCfg{
			DefaultRepository: "oci://ghcr.io/nvidia",
			DefaultChart:      "gpu-operator",
			DefaultVersion:    "25.3.0",
		},
	}

	res := surveyComponent(context.Background(), root, c, mock, true)
	// With skipHelm, no images should come from the renderer.
	if len(res.Images) != 0 {
		t.Errorf("expected 0 images with skipHelm, got %d: %v", len(res.Images), res.Images)
	}
}

func TestSurveyComponentRendererError(t *testing.T) {
	root := writeTestRegistry(t, testRegistryHelm)
	mock := &helmtest.MockRenderer{
		Errs: map[string]error{
			"gpu-operator": errors.New(errors.ErrCodeInternal, "mock render failure"),
		},
	}

	c := component{
		Name:        "gpu-operator",
		DisplayName: "GPU Operator",
		Helm: helmCfg{
			DefaultRepository: "oci://ghcr.io/nvidia",
			DefaultChart:      "gpu-operator",
			DefaultVersion:    "25.3.0",
		},
	}

	res := surveyComponent(context.Background(), root, c, mock, false)
	if len(res.Warnings) == 0 {
		t.Fatal("expected warnings from renderer error, got none")
	}
}

func TestSurveyComponentKustomize(t *testing.T) {
	root := writeTestRegistry(t, testRegistryKustomize)
	mock := &helmtest.MockRenderer{}

	c := component{
		Name:        "my-kustomize",
		DisplayName: "My Kustomize",
		Kustomize: kustCfg{
			DefaultSource: "https://github.com/example/my-app",
			DefaultPath:   "deploy",
			DefaultTag:    "v1.0.0",
		},
	}

	res := surveyComponent(context.Background(), root, c, mock, false)
	if res.Type != "kustomize" {
		t.Errorf("Type = %q, want %q", res.Type, "kustomize")
	}
	// Kustomize components don't call the helm renderer.
	if len(res.Images) != 0 {
		t.Errorf("expected 0 images for kustomize component, got %d", len(res.Images))
	}
}

func TestSurveyComponentManifestsDir(t *testing.T) {
	root := writeTestRegistry(t, testRegistryHelm)

	// Create a manifests directory with a YAML file containing an image.
	manifestsDir := filepath.Join(root, "recipes", "components", "my-comp", "manifests")
	if err := os.MkdirAll(manifestsDir, 0o755); err != nil {
		t.Fatalf("mkdir manifests: %v", err)
	}
	manifest := `apiVersion: v1
kind: Pod
metadata:
  name: test
spec:
  containers:
    - name: app
      image: docker.io/library/nginx:1.27
`
	if err := os.WriteFile(filepath.Join(manifestsDir, "pod.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	mock := &helmtest.MockRenderer{}
	c := component{
		Name:        "my-comp",
		DisplayName: "My Component",
	}

	res := surveyComponent(context.Background(), root, c, mock, false)
	if res.Type != "manifest" {
		t.Errorf("Type = %q, want %q", res.Type, "manifest")
	}
	if len(res.Images) != 1 {
		t.Fatalf("expected 1 image from manifests dir, got %d: %v", len(res.Images), res.Images)
	}
	if res.Images[0] != "docker.io/library/nginx:1.27" {
		t.Errorf("Images[0] = %q, want %q", res.Images[0], "docker.io/library/nginx:1.27")
	}
}

func TestSurveyComponentHelmPlusManifests(t *testing.T) {
	root := writeTestRegistry(t, testRegistryHelm)

	// Create a manifests directory with an additional image.
	manifestsDir := filepath.Join(root, "recipes", "components", "gpu-operator", "manifests")
	if err := os.MkdirAll(manifestsDir, 0o755); err != nil {
		t.Fatalf("mkdir manifests: %v", err)
	}
	manifest := `apiVersion: v1
kind: Pod
metadata:
  name: sidecar
spec:
  containers:
    - name: sidecar
      image: docker.io/library/busybox:1.37
`
	if err := os.WriteFile(filepath.Join(manifestsDir, "sidecar.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	mock := &helmtest.MockRenderer{
		Rendered: map[string][]byte{
			"gpu-operator": []byte(renderedYAML),
		},
	}

	c := component{
		Name:        "gpu-operator",
		DisplayName: "GPU Operator",
		Helm: helmCfg{
			DefaultRepository: "oci://ghcr.io/nvidia",
			DefaultChart:      "gpu-operator",
			DefaultVersion:    "25.3.0",
		},
	}

	res := surveyComponent(context.Background(), root, c, mock, false)
	// 2 from helm + 1 from manifests = 3 unique images.
	if len(res.Images) != 3 {
		t.Fatalf("expected 3 images (helm + manifests), got %d: %v", len(res.Images), res.Images)
	}
}

func TestRenderHelmComponent(t *testing.T) {
	root := writeTestRegistry(t, testRegistryHelm)
	mock := &helmtest.MockRenderer{
		Rendered: map[string][]byte{
			"gpu-operator": []byte(renderedYAML),
		},
	}

	c := component{
		Name: "gpu-operator",
		Helm: helmCfg{
			DefaultChart:      "gpu-operator",
			DefaultRepository: "oci://ghcr.io/nvidia",
			DefaultVersion:    "25.3.0",
			DefaultNamespace:  "gpu-operator",
		},
	}

	out, warnings := renderHelmComponent(context.Background(), root, c, mock)
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(out) == 0 {
		t.Error("expected non-empty rendered output")
	}
}

func TestRenderHelmComponentError(t *testing.T) {
	root := writeTestRegistry(t, testRegistryHelm)
	mock := &helmtest.MockRenderer{
		Errs: map[string]error{
			"gpu-operator": errors.New(errors.ErrCodeInternal, "helm template failed"),
		},
	}

	c := component{
		Name: "gpu-operator",
		Helm: helmCfg{
			DefaultChart:      "gpu-operator",
			DefaultRepository: "oci://ghcr.io/nvidia",
		},
	}

	out, warnings := renderHelmComponent(context.Background(), root, c, mock)
	if len(out) != 0 {
		t.Errorf("expected empty output on error, got %d bytes", len(out))
	}
	if len(warnings) == 0 {
		t.Fatal("expected warnings from renderer error, got none")
	}
}

func TestRenderHelmComponentWithValuesFile(t *testing.T) {
	root := writeTestRegistry(t, testRegistryHelm)

	// Create a values.yaml file for the component.
	valuesDir := filepath.Join(root, "recipes", "components", "gpu-operator")
	if err := os.MkdirAll(valuesDir, 0o755); err != nil {
		t.Fatalf("mkdir values: %v", err)
	}
	if err := os.WriteFile(filepath.Join(valuesDir, "values.yaml"), []byte("driver:\n  enabled: true\n"), 0o644); err != nil {
		t.Fatalf("write values.yaml: %v", err)
	}

	mock := &helmtest.MockRenderer{
		Rendered: map[string][]byte{
			"gpu-operator": []byte(renderedYAML),
		},
	}

	c := component{
		Name: "gpu-operator",
		Helm: helmCfg{
			DefaultChart:      "gpu-operator",
			DefaultRepository: "oci://ghcr.io/nvidia",
			DefaultVersion:    "25.3.0",
		},
	}

	out, warnings := renderHelmComponent(context.Background(), root, c, mock)
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if len(out) == 0 {
		t.Error("expected non-empty rendered output")
	}

	// Verify the values file path was passed to the renderer.
	if len(mock.Inputs) != 1 {
		t.Fatalf("expected 1 render call, got %d", len(mock.Inputs))
	}
	wantValuesPath := filepath.Join(root, "recipes", "components", "gpu-operator", "values.yaml")
	if got := mock.Inputs[0].ValuesPath; got != wantValuesPath {
		t.Errorf("ValuesPath = %q, want %q", got, wantValuesPath)
	}
}

func TestRenderHelmComponentValuesStatError(t *testing.T) {
	root := writeTestRegistry(t, testRegistryHelm)

	// Create the component directory but make it unreadable so os.Stat
	// on values.yaml returns a permission error rather than os.IsNotExist.
	valuesDir := filepath.Join(root, "recipes", "components", "gpu-operator")
	if err := os.MkdirAll(valuesDir, 0o755); err != nil {
		t.Fatalf("mkdir values: %v", err)
	}
	if err := os.WriteFile(filepath.Join(valuesDir, "values.yaml"), []byte("x: 1\n"), 0o644); err != nil {
		t.Fatalf("write values.yaml: %v", err)
	}
	// Remove read+execute on the directory so stat on the file fails with EACCES.
	if err := os.Chmod(valuesDir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(valuesDir, 0o755) }) //nolint:errcheck // best-effort restore for TempDir cleanup

	mock := &helmtest.MockRenderer{
		Rendered: map[string][]byte{
			"gpu-operator": []byte(renderedYAML),
		},
	}

	c := component{
		Name: "gpu-operator",
		Helm: helmCfg{
			DefaultChart:      "gpu-operator",
			DefaultRepository: "oci://ghcr.io/nvidia",
			DefaultVersion:    "25.3.0",
		},
	}

	_, warnings := renderHelmComponent(context.Background(), root, c, mock)
	if len(warnings) == 0 {
		t.Fatal("expected warning from values.yaml stat permission error, got none")
	}

	found := false
	for _, w := range warnings {
		if len(w) > 0 && w[:len("stat values.yaml:")] == "stat values.yaml:" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected stat warning, got: %v", warnings)
	}

	// ValuesPath should be cleared so the render still proceeds.
	if len(mock.Inputs) != 1 {
		t.Fatalf("expected 1 render call, got %d", len(mock.Inputs))
	}
	if mock.Inputs[0].ValuesPath != "" {
		t.Errorf("ValuesPath = %q, want empty after stat error", mock.Inputs[0].ValuesPath)
	}
}

func TestRunEndToEnd(t *testing.T) {
	root := writeTestRegistry(t, testRegistryHelm)
	outDir := t.TempDir()

	mock := &helmtest.MockRenderer{
		Rendered: map[string][]byte{
			"gpu-operator": []byte(renderedYAML),
		},
	}

	err := run(root, outDir, "test-v1", mock, false, false, true, true)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}

	// Verify output files exist and are non-empty.
	jsonPath := filepath.Join(outDir, "bom.cdx.json")
	jsonData, err := os.ReadFile(jsonPath)
	if err != nil {
		t.Fatalf("read bom.cdx.json: %v", err)
	}
	if len(jsonData) == 0 {
		t.Error("bom.cdx.json is empty")
	}

	mdPath := filepath.Join(outDir, "bom.md")
	mdData, err := os.ReadFile(mdPath)
	if err != nil {
		t.Fatalf("read bom.md: %v", err)
	}
	if len(mdData) == 0 {
		t.Error("bom.md is empty")
	}
}

func TestRunMissingRegistry(t *testing.T) {
	outDir := t.TempDir()
	mock := &helmtest.MockRenderer{}

	err := run("/nonexistent", outDir, "test-v1", mock, false, false, false, false)
	if err == nil {
		t.Fatal("expected error for missing registry, got nil")
	}
}

func TestRunStrictUnpinnedVersion(t *testing.T) {
	root := writeTestRegistry(t, testRegistryHelmUnpinned)
	outDir := t.TempDir()

	mock := &helmtest.MockRenderer{
		Rendered: map[string][]byte{
			"gpu-operator": []byte(renderedYAML),
		},
	}

	err := run(root, outDir, "test-v1", mock, false, true, true, true)
	if err == nil {
		t.Fatal("expected error in strict mode for unpinned version, got nil")
	}
}

func TestRunStrictWithWarnings(t *testing.T) {
	root := writeTestRegistry(t, testRegistryHelm)
	outDir := t.TempDir()

	mock := &helmtest.MockRenderer{
		Errs: map[string]error{
			"gpu-operator": errors.New(errors.ErrCodeInternal, "mock render failure"),
		},
	}

	err := run(root, outDir, "test-v1", mock, false, true, true, true)
	if err == nil {
		t.Fatal("expected error in strict mode with warnings, got nil")
	}
}

func TestRunSkipHelm(t *testing.T) {
	root := writeTestRegistry(t, testRegistryHelm)
	outDir := t.TempDir()

	mock := &helmtest.MockRenderer{
		Rendered: map[string][]byte{
			"gpu-operator": []byte(renderedYAML),
		},
	}

	err := run(root, outDir, "test-v1", mock, true, false, true, true)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}

	// Output files should still exist even with skipHelm (just no chart images).
	if _, err := os.Stat(filepath.Join(outDir, "bom.cdx.json")); err != nil {
		t.Errorf("expected bom.cdx.json to exist: %v", err)
	}
}

func TestRunMixedComponents(t *testing.T) {
	root := writeTestRegistry(t, testRegistryMixed)
	outDir := t.TempDir()

	mock := &helmtest.MockRenderer{
		Rendered: map[string][]byte{
			"gpu-operator": []byte(renderedYAML),
		},
	}

	err := run(root, outDir, "test-v1", mock, false, false, true, true)
	if err != nil {
		t.Fatalf("run() error = %v", err)
	}
}

// TestRunStrictDegenerateVersions pins the strict gate against the shared
// resolution rule (recipe.IsEffectiveChartVersion plus the padded-value
// rejection): a defaultVersion that is whitespace-only, a bare "v", or
// padded would pass an empty-string check here but fail ValidateCoherence
// at recipe resolution — the CI gate must be at least as strict as the
// resolver it guards.
func TestRunStrictDegenerateVersions(t *testing.T) {
	registryFor := func(version string) string {
		return `apiVersion: v1
kind: ComponentRegistry
components:
  - name: gpu-operator
    displayName: GPU Operator
    helm:
      defaultRepository: "oci://ghcr.io/nvidia"
      defaultChart: gpu-operator
      defaultVersion: "` + version + `"
      defaultNamespace: gpu-operator
`
	}
	tests := []struct {
		name    string
		version string
		wantErr bool
	}{
		{"whitespace-only version fails strict", "   ", true},
		{"bare v version fails strict", "v", true},
		{"padded version fails strict", " 1.0.0", true},
		{"trailing-space version fails strict", "1.0.0 ", true},
		{"pinned version passes strict", "v25.3.0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := writeTestRegistry(t, registryFor(tt.version))
			outDir := t.TempDir()
			mock := &helmtest.MockRenderer{
				Rendered: map[string][]byte{"gpu-operator": []byte(renderedYAML)},
			}
			err := run(root, outDir, "test-v1", mock, false, true, true, true)
			if (err != nil) != tt.wantErr {
				t.Errorf("run() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestSurveyComponentSourceOnlyChartFallback pins the registry-level chart
// fallback: a Helm entry with a defaultRepository but no defaultChart deploys
// the component-name chart (recipe.ComponentRef.EffectiveChart), so the BOM
// must render that chart and record it — not pass an empty chart to the
// renderer ("no helm chart configured" in strict mode) and omit the metadata.
func TestSurveyComponentSourceOnlyChartFallback(t *testing.T) {
	root := writeTestRegistry(t, testRegistryHelm)
	mock := &helmtest.MockRenderer{
		Rendered: map[string][]byte{
			"gpu-operator": []byte(renderedYAML),
		},
	}

	c := component{
		Name:        "gpu-operator",
		DisplayName: "GPU Operator",
		Helm: helmCfg{
			DefaultRepository: "oci://ghcr.io/nvidia",
			DefaultVersion:    "25.3.0",
			DefaultNamespace:  "gpu-operator",
		},
	}

	res := surveyComponent(context.Background(), root, c, mock, false)
	if len(res.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", res.Warnings)
	}
	if res.Chart != "gpu-operator" {
		t.Errorf("Chart = %q, want the component-name fallback %q", res.Chart, "gpu-operator")
	}
	if len(mock.Inputs) != 1 {
		t.Fatalf("renderer calls = %d, want 1", len(mock.Inputs))
	}
	if got := mock.Inputs[0].Chart; got != "gpu-operator" {
		t.Errorf("renderer ChartInput.Chart = %q, want fallback %q", got, "gpu-operator")
	}

	// A manifest-only entry (no repository, no chart) stays chartless.
	manifestOnly := component{Name: "nodewright-customizations", DisplayName: "nodewright"}
	if got := manifestOnly.effectiveChart(); got != "" {
		t.Errorf("manifest-only effectiveChart() = %q, want empty", got)
	}
}
