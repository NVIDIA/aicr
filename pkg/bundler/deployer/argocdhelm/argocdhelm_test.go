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

package argocdhelm

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// update regenerates goldens under testdata/ when set via `go test -update`.
// Same convention as helm and localformat deployer test suites.
var update = flag.Bool("update", false, "update golden files")

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
		// Coverage of "produces Chart.yaml + templates/ directory and does NOT
		// emit a flat app-of-apps.yaml" lives in TestBundleGolden_*; that
		// freezes the full bundle layout (file existence + content). A
		// separate substring case here would be redundant.
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

func TestGenerate_DataFiles(t *testing.T) {
	makeGenerator := func(dataFiles []string, includeChecksums bool) *Generator {
		return &Generator{
			RecipeResult: newRecipeResult("1.0.0", []recipe.ComponentRef{
				{Name: "gpu-operator", Namespace: "gpu-operator", Source: "https://helm.ngc.nvidia.com/nvidia", Chart: "gpu-operator", Version: "v24.9.0"},
			}),
			ComponentValues:  map[string]map[string]any{"gpu-operator": {}},
			Version:          "test",
			RepoURL:          "https://github.com/example/repo.git",
			IncludeChecksums: includeChecksums,
			DataFiles:        dataFiles,
		}
	}

	tests := []struct {
		name             string
		stageDataFile    string
		includeChecksums bool
		dataFiles        []string
		wantErr          bool
		wantErrMsg       string
	}{
		{
			name:             "valid data file included in checksums",
			stageDataFile:    "data/overrides.yaml",
			includeChecksums: true,
			dataFiles:        []string{"data/overrides.yaml"},
		},
		{
			name:       "path traversal rejected",
			dataFiles:  []string{"../../../etc/passwd"},
			wantErr:    true,
			wantErrMsg: "escapes base directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			outputDir := t.TempDir()

			if tt.stageDataFile != "" {
				full := filepath.Join(outputDir, tt.stageDataFile)
				if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
					t.Fatalf("stage dir: %v", err)
					return
				}
				if err := os.WriteFile(full, []byte("key: value"), 0600); err != nil {
					t.Fatalf("stage file: %v", err)
					return
				}
			}

			g := makeGenerator(tt.dataFiles, tt.includeChecksums)
			output, err := g.Generate(ctx, outputDir)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
					return
				}
				if !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Errorf("expected error containing %q, got: %v", tt.wantErrMsg, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("Generate() unexpected error = %v", err)
				return
			}

			found := false
			for _, f := range output.Files {
				if strings.HasSuffix(f, tt.stageDataFile) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("data file %q not included in output.Files", tt.stageDataFile)
			}

			if tt.includeChecksums {
				content, readErr := os.ReadFile(filepath.Join(outputDir, "checksums.txt"))
				if readErr != nil {
					t.Fatalf("read checksums.txt: %v", readErr)
					return
				}
				if !strings.Contains(string(content), tt.stageDataFile) {
					t.Errorf("checksums.txt should contain %q entry", tt.stageDataFile)
				}
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

	// Verify helm.values contains template expressions. It's a *yaml.Node with
	// LiteralStyle so yaml.Marshal emits it as a block scalar (not a quoted string).
	helm, ok := source["helm"].(map[string]any)
	if !ok {
		t.Fatal("source should have 'helm' map")
	}
	valuesNode, ok := helm["values"].(*yaml.Node)
	if !ok {
		t.Fatalf("helm.values should be *yaml.Node, got %T", helm["values"])
	}
	if valuesNode.Style != yaml.LiteralStyle {
		t.Errorf("helm.values should use LiteralStyle to render as block scalar, got %v", valuesNode.Style)
	}
	valuesStr := valuesNode.Value
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

// TestValuesBlockScalarMarshal verifies that the raw Helm template expression
// in helm.values survives yaml.Marshal as a block scalar (not a quoted string).
// Argo CD needs the raw template text so Helm evaluates it at render time.
func TestValuesBlockScalarMarshal(t *testing.T) {
	app := map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"spec": map[string]any{
			"sources": []any{
				map[string]any{
					"repoURL":        "https://helm.ngc.nvidia.com/nvidia",
					"chart":          "gpu-operator",
					"targetRevision": "v24.9.0",
				},
			},
		},
	}

	if err := convertToSingleSourceWithValues(app, "gpu-operator", "gpuOperator"); err != nil {
		t.Fatalf("convertToSingleSourceWithValues error: %v", err)
	}

	marshaled, err := yaml.Marshal(app)
	if err != nil {
		t.Fatalf("yaml.Marshal error: %v", err)
	}

	out := string(marshaled)
	// Block scalar indicator must be present so Helm sees raw template text.
	if !strings.Contains(out, "values: |") {
		t.Errorf("marshaled output should use block scalar (|) for values, got:\n%s", out)
	}
	// Helm template expressions must NOT be quoted.
	if strings.Contains(out, `values: "{{-`) || strings.Contains(out, `values: '{{-`) {
		t.Errorf("marshaled output must not quote the template, got:\n%s", out)
	}
	if !strings.Contains(out, "{{- $static") {
		t.Error("marshaled output should contain raw template expression")
	}
	if !strings.Contains(out, "mustMergeOverwrite") {
		t.Error("marshaled output should contain mustMergeOverwrite")
	}

	// Regression: nindent inside the template must exceed the column of the
	// `values:` key, otherwise Helm renders the merged values OUTSIDE the
	// literal block as siblings of `helm:`, breaking Application schema
	// validation. Locate the `values:` key and the nindent directive, parse
	// both columns, and assert the nindent argument is greater.
	valuesKeyCol, nindentArg := -1, -1
	for line := range strings.SplitSeq(out, "\n") {
		if valuesKeyCol == -1 && strings.Contains(line, "values:") {
			valuesKeyCol = strings.Index(line, "values:")
		}
		if nindentArg == -1 {
			if _, rest, ok := strings.Cut(line, "nindent "); ok {
				_, _ = fmt.Sscanf(strings.TrimSpace(rest), "%d", &nindentArg)
			}
		}
	}
	if valuesKeyCol == -1 {
		t.Fatal("could not locate `values:` column in marshaled output")
	}
	if nindentArg == -1 {
		t.Fatal("could not locate `nindent <N>` directive in marshaled output")
	}
	if nindentArg <= valuesKeyCol {
		t.Errorf("nindent argument %d must exceed values: column %d, otherwise merged "+
			"content lands outside the literal block scalar", nindentArg, valuesKeyCol)
	}
}

// injectValuesIntoSingleSource handles the path-based input shape produced
// by argocd's KindLocalHelm folders (manifest-only, kustomize-wrapped, mixed
// -post). It must add a helm.values block with dynamic stubs, leave the
// existing source.path / source.repoURL intact, and emit a literal-block
// scalar at indent that stays inside `values: |-`.
func TestInjectValuesIntoSingleSource(t *testing.T) {
	app := map[string]any{
		"apiVersion": "argoproj.io/v1alpha1",
		"kind":       "Application",
		"spec": map[string]any{
			"source": map[string]any{
				"repoURL":        "https://github.com/myorg/myrepo.git",
				"targetRevision": "main",
				"path":           "003-nodewright-customizations",
			},
		},
	}

	if err := injectValuesIntoSingleSource(app, "nodewrightcustomizations"); err != nil {
		t.Fatalf("injectValuesIntoSingleSource error: %v", err)
	}

	out, err := yaml.Marshal(app)
	if err != nil {
		t.Fatalf("yaml.Marshal error: %v", err)
	}
	str := string(out)
	// Path and repo preserved.
	if !strings.Contains(str, "path: 003-nodewright-customizations") {
		t.Errorf("source.path should be preserved, got:\n%s", str)
	}
	// helm.values block scalar present.
	if !strings.Contains(str, "values: |") {
		t.Errorf("helm.values should be a block scalar, got:\n%s", str)
	}
	// Dynamic-only template (no .Files.Get static merge).
	if !strings.Contains(str, `index .Values "nodewrightcustomizations"`) {
		t.Errorf("template should reference dynamic .Values key, got:\n%s", str)
	}
	if strings.Contains(str, ".Files.Get") {
		t.Errorf("path-based shape should NOT use .Files.Get static merge, got:\n%s", str)
	}
	// Same column-math regression as TestValuesBlockScalarMarshal.
	valuesKeyCol, nindentArg := -1, -1
	for line := range strings.SplitSeq(str, "\n") {
		if valuesKeyCol == -1 && strings.Contains(line, "values:") {
			valuesKeyCol = strings.Index(line, "values:")
		}
		if nindentArg == -1 {
			if _, rest, ok := strings.Cut(line, "nindent "); ok {
				_, _ = fmt.Sscanf(strings.TrimSpace(rest), "%d", &nindentArg)
			}
		}
	}
	if nindentArg <= valuesKeyCol {
		t.Errorf("nindent %d must exceed values: column %d", nindentArg, valuesKeyCol)
	}

	// URL-portability invariants — the input source had baked-in URL/tag,
	// the output should have rewritten them to Helm template directives so
	// the rendered child App picks up .Values.repoURL/targetRevision at
	// install time. The single-quoted YAML scalar form is required so the
	// embedded `"..."` inside `required` survives intact (double-quoted
	// YAML would force escape sequences that break Helm's parser).
	if !strings.Contains(str, `repoURL: '{{ required `) {
		t.Errorf("source.repoURL should be rewritten to single-quoted Helm directive, got:\n%s", str)
	}
	if !strings.Contains(str, `.Values.repoURL }}`) {
		t.Errorf("source.repoURL Helm directive should reference .Values.repoURL, got:\n%s", str)
	}
	if !strings.Contains(str, `targetRevision: '{{ .Values.targetRevision | default .Chart.Version }}'`) {
		t.Errorf("source.targetRevision should be rewritten to Helm directive with .Chart.Version fallback, got:\n%s", str)
	}
	// The original baked URL must be gone (otherwise the bundle remains
	// non-portable).
	if strings.Contains(str, "https://github.com/myorg/myrepo.git") {
		t.Errorf("baked-in input repoURL leaked into output, got:\n%s", str)
	}
}

// TestInjectValuesIntoSingleSource_MissingFields mirrors the multi-source
// validation regression: if the upstream argocd template emits an empty path
// or repoURL, argocdhelm should fail at bundle time with a clear message
// rather than silently produce a broken Application.
func TestInjectValuesIntoSingleSource_MissingFields(t *testing.T) {
	tests := []struct {
		name    string
		source  map[string]any
		wantMsg string
	}{
		{
			name: "empty repoURL",
			source: map[string]any{
				"repoURL":        "",
				"targetRevision": "main",
				"path":           "001-component",
			},
			wantMsg: "repoURL",
		},
		{
			name: "empty path",
			source: map[string]any{
				"repoURL":        "https://github.com/example/repo.git",
				"targetRevision": "main",
				"path":           "",
			},
			wantMsg: "path",
		},
		{
			name: "both empty",
			source: map[string]any{
				"repoURL":        "",
				"targetRevision": "main",
				"path":           "",
			},
			wantMsg: "repoURL, path",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := map[string]any{
				"spec": map[string]any{"source": tt.source},
			}
			err := injectValuesIntoSingleSource(app, "anything")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %q should contain %q", err, tt.wantMsg)
			}
		})
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

// (TestDeriveParentChartSource removed: the parent Application is now a
// chart template that consumes .Values.repoURL at install time, so the
// bundler no longer needs URL-splitting logic — the URL never enters the
// generated chart bytes.)

// TestBundleGolden_HelmAndManifestOnly freezes the argocd-helm bundle output
// for a recipe containing both Application input shapes the transformation
// must handle:
//
//   - cert-manager: pure Helm → multi-source input → flipped to single-source
//     with helm.values that merges static (.Files.Get) and dynamic (.Values.<key>)
//     via mustMergeOverwrite + nindent 16.
//   - nodewright-customizations: manifest-only → path-based single-source
//     input → helm.values injected with dynamic-only override (no .Files.Get).
//
// To regenerate after intentional output changes:
//
//	go test ./pkg/bundler/deployer/argocdhelm/... -run TestBundleGolden -args -update
//
// Substring assertions miss indentation drift (which Bug A was — nindent 8
// silently rendered values OUTSIDE the literal block); byte-comparing against
// checked-in goldens catches that class of regression.
func TestBundleGolden_HelmAndManifestOnly(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	rr := newRecipeResult("v1.0.0", []recipe.ComponentRef{
		{
			Name:      "cert-manager",
			Namespace: "cert-manager",
			Chart:     "cert-manager",
			Version:   "v1.20.2",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://charts.jetstack.io",
		},
		{
			Name:      "nodewright-customizations",
			Namespace: "skyhook",
			Type:      recipe.ComponentTypeHelm,
			Source:    "", // manifest-only
		},
	})
	rr.DeploymentOrder = []string{"cert-manager", "nodewright-customizations"}

	g := &Generator{
		RecipeResult: rr,
		ComponentValues: map[string]map[string]any{
			"cert-manager":              {"replicaCount": 1, "prometheus": map[string]any{"enabled": true}},
			"nodewright-customizations": {"enabled": true},
		},
		Version:        "v0.0.0-golden",
		RepoURL:        "https://github.com/example/aicr-bundles.git",
		TargetRevision: "main",
		DynamicValues: map[string][]string{
			"cert-manager": {"replicaCount"},
		},
		ComponentManifests: map[string]map[string][]byte{
			"nodewright-customizations": {
				"tuning.yaml": []byte("apiVersion: skyhook.nvidia.com/v1alpha1\n" +
					"kind: Skyhook\n" +
					"metadata:\n" +
					"  name: tuning\n" +
					"  namespace: {{ .Release.Namespace }}\n" +
					"spec:\n" +
					"  packages:\n" +
					"    - tuning\n"),
			},
		},
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	for _, rel := range []string{
		"Chart.yaml",
		"values.yaml",
		"templates/aicr-stack.yaml", // parent App, Helm-templated
		"templates/cert-manager.yaml",
		"templates/nodewright-customizations.yaml",
		"static/cert-manager.yaml",
		"001-cert-manager/values.yaml",
		"002-nodewright-customizations/Chart.yaml",
		"002-nodewright-customizations/templates/tuning.yaml",
	} {
		assertGolden(t, outputDir, "testdata/helm_and_manifest_only", rel)
	}
}

// TestBundleGolden_MixedComponent freezes the argocd-helm bundle output for
// a mixed component (Helm + raw manifests). The localformat layer emits a
// primary 001-<name>/ folder plus an injected 002-<name>-post/ folder. The
// argocdhelm transformation must:
//
//   - Generate templates/<name>.yaml (multi-source flip) for the primary.
//   - Generate templates/<name>-post.yaml (path-based inject) for the -post.
//   - Route -post override-key lookups through the parent component (relies
//     on findComponentByName + the localformat collision check).
func TestBundleGolden_MixedComponent(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	rr := newRecipeResult("v1.0.0", []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	})
	rr.DeploymentOrder = []string{"gpu-operator"}

	g := &Generator{
		RecipeResult: rr,
		ComponentValues: map[string]map[string]any{
			"gpu-operator": {"driver": map[string]any{"version": "580", "enabled": true}},
		},
		Version:        "v0.0.0-golden",
		RepoURL:        "https://github.com/example/aicr-bundles.git",
		TargetRevision: "main",
		DynamicValues: map[string][]string{
			"gpu-operator": {"driver.version"},
		},
		ComponentManifests: map[string]map[string][]byte{
			"gpu-operator": {
				"dcgm-exporter.yaml": []byte("apiVersion: v1\n" +
					"kind: ConfigMap\n" +
					"metadata:\n" +
					"  name: dcgm-exporter-config\n" +
					"  namespace: {{ .Release.Namespace }}\n" +
					"data:\n" +
					"  config.yaml: |\n" +
					"    metrics: enabled\n"),
			},
		},
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	for _, rel := range []string{
		"Chart.yaml",
		"values.yaml",
		"templates/aicr-stack.yaml",        // parent App, Helm-templated
		"templates/gpu-operator.yaml",      // multi-source primary
		"templates/gpu-operator-post.yaml", // path-based -post (parent override key)
		"static/gpu-operator.yaml",
		"002-gpu-operator-post/templates/dcgm-exporter.yaml",
	} {
		assertGolden(t, outputDir, "testdata/mixed_component", rel)
	}
}

// TestHelmTemplate_RendersWithSetRepoURL is the live-render counterpart to
// the golden tests: goldens freeze the pre-render template bytes, this
// test verifies that running `helm template` against the generated bundle
// with `--set repoURL=...` actually produces a valid Argo Application
// manifest with the user-supplied URL substituted in.
//
// Catches regressions that goldens miss: a typo in the parent template's
// `required`/`default` directives, a missing `}}`, or the wrong YAML
// scalar style on the injected child source fields would all silently
// freeze into goldens but blow up at install time. This test runs the
// chart through Helm and asserts the rendered output is correct.
//
// Skipped when helm is not on PATH so unit-test environments without
// helm aren't broken by it.
func TestHelmTemplate_RendersWithSetRepoURL(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not available; skipping live-render test")
	}

	outputDir := t.TempDir()
	rr := newRecipeResult("v1.0.0", []recipe.ComponentRef{
		{
			Name:      "cert-manager",
			Namespace: "cert-manager",
			Chart:     "cert-manager",
			Version:   "v1.20.2",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://charts.jetstack.io",
		},
		{
			Name:      "nodewright-customizations",
			Namespace: "skyhook",
			Type:      recipe.ComponentTypeHelm,
			Source:    "", // manifest-only → path-based child
		},
	})
	rr.DeploymentOrder = []string{"cert-manager", "nodewright-customizations"}

	g := &Generator{
		RecipeResult: rr,
		ComponentValues: map[string]map[string]any{
			"cert-manager":              {"replicaCount": 1},
			"nodewright-customizations": {"enabled": true},
		},
		Version: "v0.0.0-test",
		ComponentManifests: map[string]map[string][]byte{
			"nodewright-customizations": {
				"tuning.yaml": []byte("apiVersion: skyhook.nvidia.com/v1alpha1\n" +
					"kind: Skyhook\n" +
					"metadata:\n" +
					"  name: tuning\n" +
					"  namespace: skyhook\n" +
					"spec:\n" +
					"  packages:\n" +
					"    - tuning\n"),
			},
		},
	}
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	const wantRepoURL = "oci://example.test/myorg/aicr-bundle"
	const wantTagName = "v9.9.9-render-test"
	cmdCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "helm", "template", "test-release", outputDir, //nolint:gosec // controlled args
		"--set", "repoURL="+wantRepoURL,
		"--set", "targetRevision="+wantTagName,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\noutput:\n%s", err, out)
	}

	// Walk the rendered multi-doc YAML, find the parent and a path-based
	// child by name, assert their source.repoURL / source.targetRevision.
	dec := yaml.NewDecoder(strings.NewReader(string(out)))
	type appLite struct {
		Metadata struct{ Name string } `yaml:"metadata"`
		Spec     struct {
			Source struct {
				RepoURL        string `yaml:"repoURL"`
				Chart          string `yaml:"chart"`
				TargetRevision string `yaml:"targetRevision"`
				Path           string `yaml:"path"`
			} `yaml:"source"`
		} `yaml:"spec"`
	}
	found := map[string]appLite{}
	for {
		var a appLite
		decErr := dec.Decode(&a)
		if errors.Is(decErr, io.EOF) {
			break
		}
		if decErr != nil {
			t.Fatalf("failed to decode rendered YAML: %v\noutput:\n%s", decErr, out)
		}
		if a.Metadata.Name != "" {
			found[a.Metadata.Name] = a
		}
	}

	parent, ok := found["aicr-stack"]
	if !ok {
		t.Fatalf("rendered output missing parent Application 'aicr-stack'\noutput:\n%s", out)
	}
	if parent.Spec.Source.RepoURL != wantRepoURL {
		t.Errorf("parent App repoURL: got %q, want %q", parent.Spec.Source.RepoURL, wantRepoURL)
	}
	if parent.Spec.Source.Chart != "aicr-bundle" {
		t.Errorf("parent App chart: got %q, want \"aicr-bundle\"", parent.Spec.Source.Chart)
	}
	if parent.Spec.Source.TargetRevision != wantTagName {
		t.Errorf("parent App targetRevision: got %q, want %q", parent.Spec.Source.TargetRevision, wantTagName)
	}

	child, ok := found["nodewright-customizations"]
	if !ok {
		t.Fatalf("rendered output missing path-based child 'nodewright-customizations'")
	}
	if child.Spec.Source.RepoURL != wantRepoURL {
		t.Errorf("child path-based repoURL: got %q, want %q", child.Spec.Source.RepoURL, wantRepoURL)
	}
	if child.Spec.Source.TargetRevision != wantTagName {
		t.Errorf("child path-based targetRevision: got %q, want %q", child.Spec.Source.TargetRevision, wantTagName)
	}
	if child.Spec.Source.Path != "002-nodewright-customizations" {
		t.Errorf("child path: got %q, want \"002-nodewright-customizations\" (NNN-folder name is structural, must not be templated)", child.Spec.Source.Path)
	}

	// And a multi-source upstream-helm child should NOT have its repoURL
	// templated — its repoURL is the upstream chart registry, not the
	// bundle URL.
	upstream, ok := found["cert-manager"]
	if !ok {
		t.Fatalf("rendered output missing upstream-helm child 'cert-manager'")
	}
	if upstream.Spec.Source.RepoURL != "https://charts.jetstack.io" {
		t.Errorf("upstream-helm child repoURL should be the upstream chart registry, got %q", upstream.Spec.Source.RepoURL)
	}
}

// TestHelmTemplate_FailsWithoutRepoURL verifies the `required` directive
// in the parent App template fires when the user omits --set repoURL. This
// is the safety net that prevents users from accidentally publishing a
// chart whose Application would point at an empty URL.
func TestHelmTemplate_FailsWithoutRepoURL(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not available; skipping live-render test")
	}

	outputDir := t.TempDir()
	rr := newRecipeResult("v1.0.0", []recipe.ComponentRef{
		{
			Name: "cert-manager", Namespace: "cert-manager", Chart: "cert-manager",
			Version: "v1.20.2", Type: recipe.ComponentTypeHelm,
			Source: "https://charts.jetstack.io",
		},
	})
	rr.DeploymentOrder = []string{"cert-manager"}
	g := &Generator{
		RecipeResult:    rr,
		ComponentValues: map[string]map[string]any{"cert-manager": {}},
		Version:         "v0.0.0-test",
	}
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	cmdCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "helm", "template", "test-release", outputDir) //nolint:gosec // controlled args
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected helm template to fail without --set repoURL, but it succeeded:\n%s", out)
	}
	if !strings.Contains(string(out), "repoURL is required") {
		t.Errorf("expected error message to mention 'repoURL is required', got:\n%s", out)
	}
}

// assertGolden reads outDir/relPath and diffs it against goldenDir/relPath.
// With -update, writes the actual content to the golden path.
func assertGolden(t *testing.T, outDir, goldenDir, relPath string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(outDir, relPath))
	if err != nil {
		t.Fatalf("read actual %s: %v", relPath, err)
	}
	goldenPath := filepath.Join(goldenDir, relPath)
	if *update {
		if err = os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir golden: %v", err)
		}
		if err = os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v (run with -update to regenerate)", goldenPath, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s differs from golden:\n--- got ---\n%s\n--- want ---\n%s", relPath, got, want)
	}
}
