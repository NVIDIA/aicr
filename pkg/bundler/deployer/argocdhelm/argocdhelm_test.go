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

package argocdhelm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func newRecipeResult(version string, refs []recipe.ComponentRef) *recipe.RecipeResult {
	r := &recipe.RecipeResult{
		ComponentRefs: refs,
	}
	r.Metadata.Version = version
	return r
}

func TestGenerate(t *testing.T) {
	tests := []struct {
		name    string
		input   *Generator
		assert  func(t *testing.T, outputDir string, output *deployer.Output)
		wantErr bool
	}{
		{
			name: "produces Chart.yaml and templates directory",
			input: &Generator{
				RecipeResult: newRecipeResult("1.0.0", []recipe.ComponentRef{
					{Name: "gpu-operator", Namespace: "gpu-operator", Source: "https://helm.ngc.nvidia.com/nvidia", Chart: "gpu-operator", Version: "v24.9.0"},
				}),
				ComponentValues: map[string]map[string]any{
					"gpu-operator": {"driver": map[string]any{"version": "580"}},
				},
				Version: "test",
				RepoURL: "https://github.com/example/repo.git",
				DynamicValues: map[string][]string{
					"gpu-operator": {"driver.version"},
				},
			},
			assert: func(t *testing.T, outputDir string, _ *deployer.Output) {
				t.Helper()
				for _, f := range []string{"Chart.yaml", "values.yaml", "README.md"} {
					if _, err := os.Stat(filepath.Join(outputDir, f)); os.IsNotExist(err) {
						t.Errorf("%s should exist", f)
					}
				}
				if _, err := os.Stat(filepath.Join(outputDir, "templates", "gpu-operator.yaml")); os.IsNotExist(err) {
					t.Error("templates/gpu-operator.yaml should exist")
				}
				// Should NOT have flat ArgoCD artifacts
				if _, err := os.Stat(filepath.Join(outputDir, "app-of-apps.yaml")); !os.IsNotExist(err) {
					t.Error("app-of-apps.yaml should NOT exist in Helm chart output")
				}
			},
		},
		{
			name: "dynamic paths stubbed in root values.yaml",
			input: &Generator{
				RecipeResult: newRecipeResult("1.0.0", []recipe.ComponentRef{
					{Name: "gpu-operator", Namespace: "gpu-operator", Source: "https://helm.ngc.nvidia.com/nvidia", Chart: "gpu-operator", Version: "v24.9.0"},
				}),
				ComponentValues: map[string]map[string]any{
					"gpu-operator": {"driver": map[string]any{"version": "580", "registry": "nvcr.io"}},
				},
				Version: "test",
				RepoURL: "https://github.com/example/repo.git",
				DynamicValues: map[string][]string{
					"gpu-operator": {"driver.version"},
				},
			},
			assert: func(t *testing.T, outputDir string, _ *deployer.Output) {
				t.Helper()
				content, err := os.ReadFile(filepath.Join(outputDir, "values.yaml"))
				if err != nil {
					t.Fatalf("failed to read values.yaml: %v", err)
				}
				var values map[string]any
				if unmarshalErr := yaml.Unmarshal(content, &values); unmarshalErr != nil {
					t.Fatalf("failed to parse values.yaml: %v", unmarshalErr)
				}

				// Root values.yaml should ONLY have dynamic stubs
				key, keyErr := resolveOverrideKey("gpu-operator")
				if keyErr != nil {
					t.Fatalf("resolveOverrideKey failed: %v", keyErr)
				}
				compValues, ok := values[key].(map[string]any)
				if !ok {
					t.Fatalf("expected dynamic stubs under key %q", key)
				}
				driver, ok := compValues["driver"].(map[string]any)
				if !ok {
					t.Fatal("expected driver map in dynamic stubs")
				}
				// Dynamic path should have the resolved default value (not empty —
				// the ArgoCD Helm chart preserves defaults so users see what to override)
				if driver["version"] == nil {
					t.Error("dynamic path driver.version should be present in root values.yaml")
				}
				// Static values should NOT be in root values.yaml
				if _, hasRegistry := driver["registry"]; hasRegistry {
					t.Error("static path driver.registry should NOT be in root values.yaml (it's in static/)")
				}

				// Static values should be in static/ directory
				staticContent, staticErr := os.ReadFile(filepath.Join(outputDir, "static", "gpu-operator.yaml"))
				if staticErr != nil {
					t.Fatalf("failed to read static/gpu-operator.yaml: %v", staticErr)
				}
				if !strings.Contains(string(staticContent), "nvcr.io") {
					t.Error("static/gpu-operator.yaml should contain static values like registry")
				}
			},
		},
		{
			name: "transformed template uses valuesObject",
			input: &Generator{
				RecipeResult: newRecipeResult("1.0.0", []recipe.ComponentRef{
					{Name: "gpu-operator", Namespace: "gpu-operator", Source: "https://helm.ngc.nvidia.com/nvidia", Chart: "gpu-operator", Version: "v24.9.0"},
				}),
				ComponentValues: map[string]map[string]any{
					"gpu-operator": {"driver": map[string]any{"version": "580"}},
				},
				Version: "test",
				RepoURL: "https://github.com/example/repo.git",
				DynamicValues: map[string][]string{
					"gpu-operator": {"driver.version"},
				},
			},
			assert: func(t *testing.T, outputDir string, _ *deployer.Output) {
				t.Helper()
				tmplContent, err := os.ReadFile(filepath.Join(outputDir, "templates", "gpu-operator.yaml"))
				if err != nil {
					t.Fatalf("failed to read template: %v", err)
				}
				tmplStr := string(tmplContent)

				if !strings.Contains(tmplStr, "valuesObject") {
					t.Error("template should contain valuesObject")
				}
				if !strings.Contains(tmplStr, "static/gpu-operator.yaml") {
					t.Error("template should load static values via .Files.Get")
				}
				if !strings.Contains(tmplStr, "mustMergeOverwrite") {
					t.Error("template should merge static + dynamic values")
				}
				// Should be single-source, not multi-source
				if strings.Contains(tmplStr, "sources:") {
					t.Error("template should use single 'source:', not multi-source 'sources:'")
				}
				if strings.Contains(tmplStr, "$values") {
					t.Error("template should not reference $values (flat ArgoCD pattern)")
				}
			},
		},
		{
			name: "deployment steps reference helm install",
			input: &Generator{
				RecipeResult: newRecipeResult("1.0.0", []recipe.ComponentRef{
					{Name: "gpu-operator", Namespace: "gpu-operator", Source: "https://charts.example.com", Chart: "gpu-operator", Version: "v1.0.0"},
				}),
				ComponentValues: map[string]map[string]any{"gpu-operator": {}},
				Version:         "test",
				RepoURL:         "https://github.com/example/repo.git",
				DynamicValues:   map[string][]string{"gpu-operator": {"driver.version"}},
			},
			assert: func(t *testing.T, _ string, output *deployer.Output) {
				t.Helper()
				found := false
				for _, step := range output.DeploymentSteps {
					if strings.Contains(step, "helm install") {
						found = true
						break
					}
				}
				if !found {
					t.Error("deployment steps should reference 'helm install'")
				}
			},
		},
		{
			name: "Chart.yaml has correct version from recipe",
			input: &Generator{
				RecipeResult: newRecipeResult("2.5.0", []recipe.ComponentRef{
					{Name: "gpu-operator", Namespace: "gpu-operator", Source: "https://charts.example.com", Chart: "gpu-operator", Version: "v1.0.0"},
				}),
				ComponentValues: map[string]map[string]any{"gpu-operator": {}},
				Version:         "test",
				RepoURL:         "https://github.com/example/repo.git",
				DynamicValues:   map[string][]string{"gpu-operator": {"driver.version"}},
			},
			assert: func(t *testing.T, outputDir string, _ *deployer.Output) {
				t.Helper()
				content, err := os.ReadFile(filepath.Join(outputDir, "Chart.yaml"))
				if err != nil {
					t.Fatalf("failed to read Chart.yaml: %v", err)
				}
				if !strings.Contains(string(content), "version: 2.5.0") {
					t.Error("Chart.yaml should contain version: 2.5.0")
				}
			},
		},
		{
			name:    "nil input returns error",
			input:   nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outputDir := t.TempDir()
			var output *deployer.Output
			var err error
			if tt.input != nil {
				output, err = tt.input.Generate(context.Background(), outputDir)
			} else {
				gen := &Generator{}
				output, err = gen.Generate(context.Background(), outputDir)
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("Generate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if tt.assert != nil {
				tt.assert(t, outputDir, output)
			}
		})
	}
}

func TestTransformMultiSourceToValuesObject(t *testing.T) {
	input := `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: gpu-operator
spec:
  project: default
  sources:
    - repoURL: https://helm.ngc.nvidia.com/nvidia
      chart: gpu-operator
      targetRevision: v24.9.0
      helm:
        valueFiles:
          - $values/gpu-operator/values.yaml
    - repoURL: 'https://github.com/example/repo.git'
      targetRevision: main
      ref: values
  destination:
    server: https://kubernetes.default.svc
    namespace: gpu-operator
`

	result, err := transformMultiSourceToValuesObject(input, "gpu-operator", "gpuOperator", true)
	if err != nil {
		t.Fatalf("transformMultiSourceToValuesObject() error = %v", err)
	}

	if strings.Contains(result, "sources:") {
		t.Error("should not contain multi-source 'sources:'")
	}
	if !strings.Contains(result, "source:") {
		t.Error("should contain single 'source:'")
	}
	if !strings.Contains(result, "valuesObject") {
		t.Error("should contain valuesObject")
	}
	if !strings.Contains(result, "static/gpu-operator.yaml") {
		t.Error("should reference static values file")
	}
	if !strings.Contains(result, "mustMergeOverwrite") {
		t.Error("should merge static + dynamic values")
	}
	if !strings.Contains(result, `"gpuOperator"`) {
		t.Error("should reference .Values with override key")
	}
	if !strings.Contains(result, "chart: gpu-operator") {
		t.Error("should preserve chart name")
	}
	if !strings.Contains(result, "repoURL: https://helm.ngc.nvidia.com/nvidia") {
		t.Error("should preserve chart repoURL")
	}
}

func TestTransformMultiSource_StaticOnly(t *testing.T) {
	input := `apiVersion: argoproj.io/v1alpha1
kind: Application
spec:
  sources:
    - repoURL: https://charts.example.com
      chart: cert-manager
      targetRevision: v1.14.0
      helm:
        valueFiles:
          - $values/cert-manager/values.yaml
    - repoURL: 'https://github.com/example/repo.git'
      targetRevision: main
      ref: values
  destination:
    server: https://kubernetes.default.svc
`

	result, err := transformMultiSourceToValuesObject(input, "cert-manager", "certmanager", false)
	if err != nil {
		t.Fatalf("error = %v", err)
	}

	if !strings.Contains(result, "static/cert-manager.yaml") {
		t.Error("static-only should reference .Files.Get")
	}
	if strings.Contains(result, "mustMergeOverwrite") {
		t.Error("static-only should NOT use merge (no dynamic values)")
	}
}

func TestTransformMultiSource_MissingFields(t *testing.T) {
	// Application with no sources block — regex won't match
	noSources := `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: gpu-operator
spec:
  source:
    repoURL: https://example.com
  destination:
    server: https://kubernetes.default.svc
`
	_, err := transformMultiSourceToValuesObject(noSources, "gpu-operator", "gpuOperator", true)
	if err == nil {
		t.Error("expected error when sources block is missing")
	}

	// Application with sources but missing chart field
	noChart := `apiVersion: argoproj.io/v1alpha1
kind: Application
spec:
  sources:
    - repoURL: https://example.com
      targetRevision: v1.0.0
      helm:
        valueFiles:
          - $values/test/values.yaml
    - repoURL: 'https://github.com/example/repo.git'
      targetRevision: main
      ref: values
  destination:
    server: https://kubernetes.default.svc
`
	_, err = transformMultiSourceToValuesObject(noChart, "test", "test", true)
	if err == nil {
		t.Error("expected error when chart field is missing from source")
	}
}

func TestSetValueByPath_StubBehavior(t *testing.T) {
	m := map[string]any{
		"driver": map[string]any{"version": "580", "registry": "nvcr.io"},
	}
	component.SetValueByPath(m, "driver.version", "")

	driver := m["driver"].(map[string]any)
	if driver["version"] != "" {
		t.Errorf("expected empty stub, got %v", driver["version"])
	}
	if driver["registry"] != "nvcr.io" {
		t.Error("should not affect sibling keys")
	}
}

func TestDeepCopyMap(t *testing.T) {
	original := map[string]any{
		"driver": map[string]any{"version": "580"},
	}
	copied := deepCopyMap(original)

	if inner, ok := copied["driver"].(map[string]any); ok {
		inner["version"] = "changed"
	}
	if original["driver"].(map[string]any)["version"] != "580" {
		t.Error("deepCopyMap should produce independent copy")
	}
}
