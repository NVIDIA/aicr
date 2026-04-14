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
				// Should NOT have flat Argo CD artifacts
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
				// the Argo CD Helm chart preserves defaults so users see what to override)
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
			name: "transformed template uses values",
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

				if !strings.Contains(tmplStr, "values:") {
					t.Error("template should contain values:")
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
					t.Error("template should not reference $values (flat Argo CD pattern)")
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

// TestConvertToSingleSourceWithValues verifies the structured YAML
// transformation from multi-source to single-source with helm.values.
func TestConvertToSingleSourceWithValues(t *testing.T) {
	// Build a valid multi-source Application map
	app := map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"metadata":   map[string]any{"name": "gpu-operator"},
		"spec": map[string]any{
			"project": "default",
			"sources": []any{
				map[string]any{
					"repoURL":        "https://helm.ngc.nvidia.com/nvidia",
					"chart":          "gpu-operator",
					"targetRevision": "v24.9.0",
				},
				map[string]any{
					"repoURL": "https://github.com/example/repo.git",
					"ref":     "values",
				},
			},
			"destination": map[string]any{
				"server":    "https://kubernetes.default.svc",
				"namespace": "gpu-operator",
			},
		},
	}

	err := convertToSingleSourceWithValues(app, "gpu-operator", "gpuOperator")
	if err != nil {
		t.Fatalf("convertToSingleSourceWithValues() error = %v", err)
	}

	spec := app["spec"].(map[string]any)

	// Should have single "source", not "sources"
	if _, hasSources := spec["sources"]; hasSources {
		t.Error("should remove multi-source 'sources'")
	}
	source, ok := spec["source"].(map[string]any)
	if !ok {
		t.Fatal("should have single 'source' map")
	}

	// Verify chart fields preserved
	if source["repoURL"] != "https://helm.ngc.nvidia.com/nvidia" {
		t.Errorf("repoURL = %v, want nvidia repo", source["repoURL"])
	}
	if source["chart"] != "gpu-operator" {
		t.Errorf("chart = %v, want gpu-operator", source["chart"])
	}
	if source["targetRevision"] != "v24.9.0" {
		t.Errorf("targetRevision = %v, want v24.9.0", source["targetRevision"])
	}

	// Verify helm.values contains template expressions
	helm, ok := source["helm"].(map[string]any)
	if !ok {
		t.Fatal("source should have 'helm' map")
	}
	valuesStr, ok := helm["values"].(string)
	if !ok {
		t.Fatal("helm should have 'values' string")
	}
	if !strings.Contains(valuesStr, "static/gpu-operator.yaml") {
		t.Error("values should reference static file")
	}
	if !strings.Contains(valuesStr, "mustMergeOverwrite") {
		t.Error("values should use merge pattern")
	}
	if !strings.Contains(valuesStr, `"gpuOperator"`) {
		t.Error("values should reference override key")
	}
	// Should NOT use valuesObject (that expects a YAML object, not a string)
	if _, hasValuesObject := helm["valuesObject"]; hasValuesObject {
		t.Error("should use 'values' (string), not 'valuesObject' (object)")
	}

	// Destination should be untouched
	dest := spec["destination"].(map[string]any)
	if dest["namespace"] != "gpu-operator" {
		t.Error("destination should be preserved")
	}
}

// TestConvertToSingleSource_MissingFields verifies error handling when the
// Application manifest is missing required fields.
func TestConvertToSingleSource_MissingFields(t *testing.T) {
	tests := []struct {
		name string
		app  map[string]any
	}{
		{
			name: "missing spec",
			app:  map[string]any{"apiVersion": "v1"},
		},
		{
			name: "missing sources",
			app: map[string]any{
				"spec": map[string]any{
					"source": map[string]any{"repoURL": "https://example.com"},
				},
			},
		},
		{
			name: "empty sources",
			app: map[string]any{
				"spec": map[string]any{"sources": []any{}},
			},
		},
		{
			name: "missing chart in first source",
			app: map[string]any{
				"spec": map[string]any{
					"sources": []any{
						map[string]any{
							"repoURL":        "https://example.com",
							"targetRevision": "v1.0.0",
							// chart is missing
						},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := convertToSingleSourceWithValues(tt.app, "test", "test")
			if err == nil {
				t.Error("expected error for malformed application")
			}
		})
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

// TestFixValuesTemplate verifies that the raw Helm template expression in
// helm.values survives yaml.Marshal → fixValuesTemplate without being
// quoted or escaped. This is critical: Argo CD needs the raw template text, not
// a YAML string literal.
func TestFixValuesTemplate(t *testing.T) {
	tmpl := `{{- $static := (.Files.Get "static/gpu-operator.yaml") | fromYaml -}}
{{- $dynamic := index .Values "gpuOperator" | default dict -}}
{{- mustMergeOverwrite $static $dynamic | toYaml | nindent 8 }}`

	app := map[string]any{
		"spec": map[string]any{
			"source": map[string]any{
				"helm": map[string]any{
					"values": tmpl,
				},
			},
		},
	}

	// yaml.Marshal will quote the template (it contains {{ }})
	marshaled, err := yaml.Marshal(app)
	if err != nil {
		t.Fatalf("yaml.Marshal error: %v", err)
	}

	// Apply the fix
	fixed := fixValuesTemplate(marshaled, app)

	// The fixed output should contain the raw template as a block scalar
	fixedStr := string(fixed)
	if !strings.Contains(fixedStr, "values: |-") {
		t.Error("fixed output should use block scalar (|-) for values")
	}
	if !strings.Contains(fixedStr, "{{- $static") {
		t.Error("fixed output should contain raw template expression")
	}
	if !strings.Contains(fixedStr, "mustMergeOverwrite") {
		t.Error("fixed output should contain mustMergeOverwrite")
	}
}

func TestDeepCopyMap(t *testing.T) {
	original := map[string]any{
		"driver": map[string]any{"version": "580"},
	}
	copied := component.DeepCopyMap(original)

	if inner, ok := copied["driver"].(map[string]any); ok {
		inner["version"] = "changed"
	}
	if original["driver"].(map[string]any)["version"] != "580" {
		t.Error("deepCopyMap should produce independent copy")
	}
}
