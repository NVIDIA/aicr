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
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/gatemanifest"
	"github.com/NVIDIA/aicr/pkg/component"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// update regenerates goldens under testdata/ when set via `go test -update`.
// Same convention as helm and localformat deployer test suites.
var update = flag.Bool("update", false, "update golden files")

// requireHelm gates the live-render tests on a helm binary. In CI (the
// standard CI=true environment variable set by GitHub Actions) a missing
// helm is a hard failure: the go-test action installs the version pinned
// in .settings.yaml, so an absent binary means the pipeline silently
// stopped exercising the live-render coverage. Locally the tests keep
// skipping so dev environments without helm are not broken.
func requireHelm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("helm is required in CI but not on PATH; the go-test action must install the pinned version from .settings.yaml (testing_tools.helm)")
		}
		t.Skip("helm not available; skipping live-render test")
	}
}

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
				key, keyErr := resolveOverrideKey("gpu-operator", nil)
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
				// writeChartYAML quotes the version scalar so YAML
				// reserved scalars (e.g. "1.0", "null") round-trip as
				// strings; see issue #1034.
				if !strings.Contains(string(content), `version: "2.5.0"`) {
					t.Errorf("Chart.yaml should contain version: \"2.5.0\", got:\n%s", string(content))
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

func TestGenerate_ChartVersion(t *testing.T) {
	tests := []struct {
		name               string
		recipeVersion      string
		bundleChartVersion string
		want               string
	}{
		{
			name:          "empty override uses normalized recipe version",
			recipeVersion: "v2.5.0",
			want:          "2.5.0",
		},
		{
			name:               "semantic build metadata is preserved exactly",
			recipeVersion:      "v9.9.9",
			bundleChartVersion: "1.2.3+build.5",
			want:               "1.2.3+build.5",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outputDir := t.TempDir()
			g := &Generator{
				RecipeResult:       newRecipeResult(tt.recipeVersion, nil),
				Version:            "test",
				BundleChartVersion: tt.bundleChartVersion,
			}
			if _, err := g.Generate(context.Background(), outputDir); err != nil {
				t.Fatalf("Generate() error = %v", err)
			}
			chartYAML, err := os.ReadFile(filepath.Join(outputDir, "Chart.yaml"))
			if err != nil {
				t.Fatalf("ReadFile(Chart.yaml) error = %v", err)
			}
			if !strings.Contains(string(chartYAML), `version: "`+tt.want+`"`) {
				t.Errorf("Chart.yaml =\n%s\nwant exact quoted version %q", chartYAML, tt.want)
			}
		})
	}
}

func TestGeneratorTargetRevision(t *testing.T) {
	tests := []struct {
		name               string
		recipeVersion      string
		bundleChartVersion string
		targetRevision     string
		want               string
	}{
		{
			name:               "bundle chart version is the default",
			recipeVersion:      "v9.9.9",
			bundleChartVersion: "1.2.3+build.5",
			want:               "1.2.3+build.5",
		},
		{
			name:          "recipe version is the fallback",
			recipeVersion: "v2.5.0",
			want:          "2.5.0",
		},
		{
			name:               "explicit target revision wins",
			recipeVersion:      "v9.9.9",
			bundleChartVersion: "1.2.3",
			targetRevision:     "release-candidate",
			want:               "release-candidate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &Generator{
				RecipeResult:       newRecipeResult(tt.recipeVersion, nil),
				BundleChartVersion: tt.bundleChartVersion,
				TargetRevision:     tt.targetRevision,
			}
			if got := g.targetRevision(); got != tt.want {
				t.Errorf("targetRevision() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGenerate_RejectsInvalidBundleChartVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
	}{
		{name: "non semantic version", version: "latest"},
		{name: "leading v", version: "v1.2.3"},
		{name: "missing patch", version: "1.2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outputDir := t.TempDir()
			g := &Generator{
				RecipeResult:       newRecipeResult("v9.9.9", nil),
				Version:            "test",
				BundleChartVersion: tt.version,
			}

			_, err := g.Generate(context.Background(), outputDir)
			if !errors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
				t.Fatalf("Generate() error = %v, want ErrCodeInvalidRequest", err)
			}
			entries, readErr := os.ReadDir(outputDir)
			if readErr != nil {
				t.Fatalf("ReadDir() error = %v", readErr)
			}
			if len(entries) != 0 {
				t.Errorf("Generate() wrote %d entries before rejecting BundleChartVersion", len(entries))
			}
		})
	}
}

// TestGenerate_PreManifestParentResolution exercises the synthetic
// `-pre` folder path that recipes with ComponentPreManifests emit
// (e.g. the gke-cos OS overlay, which injects a Talos-style driver
// prerequisite Namespace ahead of gpu-operator). The folder is named
// `NNN-gpu-operator-pre`; the bundler must strip the `-pre` suffix
// when resolving the override key, otherwise resolveOverrideKey is
// called with "gpu-operator-pre" and fails `component %q not found
// in registry` at bundle time. Caught in the KWOK gke-cos-training
// CI lane after the pre-fix; before this commit the bug only
// surfaced for recipes that combined Helm + pre-manifest overlays.
func TestGenerate_PreManifestParentResolution(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	g := &Generator{
		RecipeResult: newRecipeResult("1.0.0", []recipe.ComponentRef{
			{Name: "gpu-operator", Namespace: "gpu-operator", Source: "https://helm.ngc.nvidia.com/nvidia", Chart: "gpu-operator", Version: "v24.9.0"},
		}),
		ComponentValues: map[string]map[string]any{
			"gpu-operator": {"driver": map[string]any{"version": "580"}},
		},
		Version: "test",
		RepoURL: "https://github.com/example/repo.git",
		ComponentPreManifests: map[string]map[string][]byte{
			"gpu-operator": {
				"components/gpu-operator/manifests/cos-namespace.yaml": []byte(
					"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: gpu-operator\n",
				),
			},
		},
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v (regression: -pre suffix not stripped before resolveOverrideKey)", err)
	}

	// The -pre folder content is copied into the bundle so Argo CD's
	// repo-server can resolve the path-based child Application's
	// `path: NNN-<name>-pre` reference. Confirm it survived the
	// argocdhelm wrapper transformation.
	preDir := filepath.Join(outputDir, "001-gpu-operator-pre")
	if _, err := os.Stat(preDir); err != nil {
		t.Errorf("expected -pre folder copied into bundle at %s: %v", preDir, err)
	}

	// The transformed template under templates/ should be keyed by the
	// PARENT component (gpu-operator), not the -pre folder name,
	// because the override key comes from the parent's registry entry.
	tmplPath := filepath.Join(outputDir, "templates", "gpu-operator-pre.yaml")
	if _, err := os.Stat(tmplPath); err != nil {
		t.Errorf("expected wrapper template for -pre folder at %s: %v", tmplPath, err)
	}
}

// TestGenerate_PreAndPostManifestParentResolution covers the actual
// gke-cos OS overlay shape: a single registered component (`gpu-operator`)
// with BOTH preManifestFiles (cos-namespace prep) and postManifestFiles
// (dcgm-exporter). The argocdhelm processFolders loop sees 3 entries
// (`<NNN>-gpu-operator-pre`, `<NNN>-gpu-operator`, `<NNN>-gpu-operator-post`)
// and runs the suffix-strip logic twice across two distinct folders.
// Both wrapper templates must resolve back to the same parent override
// key so the values applied to the chart and the manifest hooks stay
// consistent. Earlier revisions handled only `-post`; this test guards
// against re-introducing that asymmetry.
func TestGenerate_PreAndPostManifestParentResolution(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	g := &Generator{
		RecipeResult: newRecipeResult("1.0.0", []recipe.ComponentRef{
			{Name: "gpu-operator", Namespace: "gpu-operator", Source: "https://helm.ngc.nvidia.com/nvidia", Chart: "gpu-operator", Version: "v24.9.0"},
		}),
		ComponentValues: map[string]map[string]any{
			"gpu-operator": {"driver": map[string]any{"version": "580"}},
		},
		Version: "test",
		RepoURL: "https://github.com/example/repo.git",
		ComponentPreManifests: map[string]map[string][]byte{
			"gpu-operator": {
				"components/gpu-operator/manifests/cos-namespace.yaml": []byte(
					"apiVersion: v1\nkind: Namespace\nmetadata:\n  name: gpu-operator\n",
				),
			},
		},
		ComponentPostManifests: map[string]map[string][]byte{
			"gpu-operator": {
				"components/gpu-operator/manifests/dcgm-exporter.yaml": []byte(
					"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: dcgm-config\n",
				),
			},
		},
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Both synthetic folders must exist in the bundle so the path-based
	// child Applications can resolve them.
	preDir := filepath.Join(outputDir, "001-gpu-operator-pre")
	if _, err := os.Stat(preDir); err != nil {
		t.Errorf("expected -pre folder at %s: %v", preDir, err)
	}
	postDir := filepath.Join(outputDir, "003-gpu-operator-post")
	if _, err := os.Stat(postDir); err != nil {
		t.Errorf("expected -post folder at %s: %v", postDir, err)
	}

	// Both wrapper templates must be present under templates/, both
	// keyed by the PARENT component name (not the -pre / -post folder
	// names). Asymmetric handling would lose one of them.
	for _, suffix := range []string{"-pre", "-post"} {
		tmplPath := filepath.Join(outputDir, "templates", "gpu-operator"+suffix+".yaml")
		if _, err := os.Stat(tmplPath); err != nil {
			t.Errorf("expected wrapper template for %s folder at %s: %v", suffix, tmplPath, err)
		}
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
	if !strings.Contains(str, `.Values.repoURL`) {
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
		ComponentPostManifests: map[string]map[string][]byte{
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
		"values.schema.json",        // install-time deployer.* schema gate
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
		ComponentPostManifests: map[string]map[string][]byte{
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

// TestBundleGolden_OCI_BakesRepoURL verifies that when OCIParentNamespace is
// set the bundle's root values.yaml contains the parent namespace as the
// repoURL default, not an empty string. See #1342.
func TestBundleGolden_OCI_BakesRepoURL(t *testing.T) {
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
	})
	rr.DeploymentOrder = []string{"cert-manager"}

	const wantNamespace = "oci://ghcr.io/myorg"

	g := &Generator{
		RecipeResult:       rr,
		ComponentValues:    map[string]map[string]any{"cert-manager": {"replicaCount": 1}},
		Version:            "v0.0.0-golden",
		TargetRevision:     "v1.0.0",
		OCIParentNamespace: wantNamespace,
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	valuesBytes, err := os.ReadFile(filepath.Join(outputDir, "values.yaml"))
	if err != nil {
		t.Fatalf("read values.yaml: %v", err)
	}
	values := string(valuesBytes)

	if !strings.Contains(values, `repoURL: `+wantNamespace) {
		t.Errorf("values.yaml missing baked repoURL; got:\n%s", values)
	}
	if strings.Contains(values, "repoURL: \"\"") {
		t.Errorf("values.yaml should not contain empty repoURL when OCIParentNamespace is set; got:\n%s", values)
	}
}

// TestBundleGolden_ReadinessGate freezes the argocd-helm bundle output when a
// component ships a readiness gate. The delegated argocd.Generator emits a
// 002-<name>-readiness/ local-helm folder after the primary; the argocdhelm
// transform flips it into a path-based child App (templates/<name>-readiness.yaml)
// that inherits the next sync-wave, so Argo CD blocks on the gate Job via its
// built-in batch/Job health. See #904.
func TestBundleGolden_ReadinessGate(t *testing.T) {
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
			"gpu-operator": {"driver": map[string]any{"version": "580"}},
		},
		Version:        "v0.0.0-golden",
		RepoURL:        "https://github.com/example/aicr-bundles.git",
		TargetRevision: "main",
		ComponentReadiness: map[string]map[string][]byte{
			"gpu-operator": {
				"readiness.yaml": readinessGateManifest(t, config.DeployerArgoCDHelm),
			},
		},
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	for _, rel := range []string{
		"templates/gpu-operator.yaml",           // multi-source primary
		"templates/gpu-operator-readiness.yaml", // path-based readiness child
		"002-gpu-operator-readiness/templates/readiness.yaml",
	} {
		assertGolden(t, outputDir, "testdata/readiness_gate", rel)
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
	requireHelm(t)

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
		ComponentPostManifests: map[string]map[string][]byte{
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

	// --set repoURL carries the parent namespace only. Both the parent
	// App template and the path-based child template append /<chart-name>
	// themselves so Argo CD's native-OCI lookup resolves at
	// `<namespace>/<chart>:<tag>` — see parentAppTemplate and
	// injectValuesIntoSingleSource in argocdhelm.go.
	const setRepoURL = "oci://example.test/myorg"
	const wantParentRepoURL = setRepoURL + "/" + DefaultChartName
	const wantChildRepoURL = setRepoURL + "/" + DefaultChartName
	const wantTagName = "v9.9.9-render-test"
	cmdCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "helm", "template", "test-release", outputDir, //nolint:gosec // controlled args
		"--set", "repoURL="+setRepoURL,
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
	if parent.Spec.Source.RepoURL != wantParentRepoURL {
		t.Errorf("parent App repoURL: got %q, want %q (must be parent-namespace + chart-name; see parentAppTemplate)", parent.Spec.Source.RepoURL, wantParentRepoURL)
	}
	if parent.Spec.Source.Chart != "" {
		t.Errorf("parent App chart: got %q, want empty (native-OCI mode ignores source.chart; see PR #1047 regression)", parent.Spec.Source.Chart)
	}
	if parent.Spec.Source.Path != "." {
		t.Errorf("parent App path: got %q, want %q (native-OCI source renders chart from artifact root)", parent.Spec.Source.Path, ".")
	}
	if parent.Spec.Source.TargetRevision != wantTagName {
		t.Errorf("parent App targetRevision: got %q, want %q", parent.Spec.Source.TargetRevision, wantTagName)
	}

	child, ok := found["nodewright-customizations"]
	if !ok {
		t.Fatalf("rendered output missing path-based child 'nodewright-customizations'")
	}
	if child.Spec.Source.RepoURL != wantChildRepoURL {
		t.Errorf("child path-based repoURL: got %q, want %q (parent namespace + chart name; see #1034)", child.Spec.Source.RepoURL, wantChildRepoURL)
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

// TestHelmTemplate_RendersWithHelmRepoRepoURL verifies the parent App
// template renders the HTTPS Helm-repo shape (repoURL + source.chart,
// no path) when .Values.repoURL is not an oci:// URL. The bundle is
// pure-Helm (only upstream-helm children, no path-based children), so
// installation from ChartMuseum / GitHub Pages-style repos is the
// supported use case. See PR #1051's Codex P2 review.
func TestHelmTemplate_RendersWithHelmRepoRepoURL(t *testing.T) {
	requireHelm(t)

	ctx := context.Background()
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
		Version:         "v0.0.0-helm-repo-test",
	}
	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	const setRepoURL = "https://charts.example.com"
	const wantTagName = "v9.9.9-helm-repo-test"
	cmdCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "helm", "template", "test-release", outputDir, //nolint:gosec // controlled args
		"--set", "repoURL="+setRepoURL,
		"--set", "targetRevision="+wantTagName,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\noutput:\n%s", err, out)
	}

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
	var parent appLite
	var found bool
	for {
		var a appLite
		decErr := dec.Decode(&a)
		if errors.Is(decErr, io.EOF) {
			break
		}
		if decErr != nil {
			t.Fatalf("failed to decode rendered YAML: %v\noutput:\n%s", decErr, out)
		}
		if a.Metadata.Name == "aicr-stack" {
			parent = a
			found = true
		}
	}
	if !found {
		t.Fatalf("rendered output missing parent Application 'aicr-stack'\noutput:\n%s", out)
	}

	// HTTPS Helm-repo shape: repoURL is the registry as-is, chart is
	// the chart name, and path is NOT set (path is a native-OCI-only
	// field). Mirror these three assertions exactly — silently
	// regressing any one of them puts the parent App back into the
	// broken shape that #1047 / #1048 / #1051 chased.
	if parent.Spec.Source.RepoURL != setRepoURL {
		t.Errorf("parent App repoURL: got %q, want %q (HTTPS Helm-repo mode: repoURL stays as-is, chart name is in source.chart)", parent.Spec.Source.RepoURL, setRepoURL)
	}
	if parent.Spec.Source.Chart != DefaultChartName {
		t.Errorf("parent App chart: got %q, want %q (HTTPS Helm-repo mode requires source.chart)", parent.Spec.Source.Chart, DefaultChartName)
	}
	if parent.Spec.Source.Path != "" {
		t.Errorf("parent App path: got %q, want empty (path is a native-OCI-only field; emitting it on a Helm-repo source confuses Argo CD)", parent.Spec.Source.Path)
	}
	if parent.Spec.Source.TargetRevision != wantTagName {
		t.Errorf("parent App targetRevision: got %q, want %q", parent.Spec.Source.TargetRevision, wantTagName)
	}
}

// TestGenerate_CustomChartName verifies the Generator's ChartName field
// flows into both Chart.yaml's `name:` and the parent Application's
// `source.repoURL` (parent namespace + `/` + .Chart.Name) at install
// time. Regression coverage for issue #1019: when a user passes
// `--output oci://reg/path/my-bundle:tag`, the OCI repository's last
// segment (`my-bundle`) must propagate so the assembled
// `<namespace>/<chart>:<targetRevision>` resolves against the actual
// published artifact instead of the literal `aicr-bundle`.
func TestGenerate_CustomChartName(t *testing.T) {
	requireHelm(t)

	const customName = "my-custom-bundle"
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
		ChartName:       customName,
	}
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	chartBytes, err := os.ReadFile(filepath.Join(outputDir, "Chart.yaml"))
	if err != nil {
		t.Fatalf("read Chart.yaml: %v", err)
	}
	// writeChartYAML quotes the name so OCI artifact paths whose last
	// segment is a YAML reserved scalar round-trip as strings; see #1034.
	if !strings.Contains(string(chartBytes), `name: "`+customName+"\"\n") {
		t.Errorf("Chart.yaml missing quoted custom name %q; got:\n%s", customName, chartBytes)
	}

	cmdCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "helm", "template", "test-release", outputDir, //nolint:gosec // controlled args
		"--set", "repoURL=oci://example.test/myorg",
		"--set", "targetRevision=v1.0.0",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\noutput:\n%s", err, out)
	}

	dec := yaml.NewDecoder(strings.NewReader(string(out)))
	type appLite struct {
		Metadata struct{ Name string } `yaml:"metadata"`
		Spec     struct {
			Source struct {
				RepoURL string `yaml:"repoURL"`
			} `yaml:"source"`
		} `yaml:"spec"`
	}
	var foundParentRepoURL string
	for {
		var a appLite
		decErr := dec.Decode(&a)
		if errors.Is(decErr, io.EOF) {
			break
		}
		if decErr != nil {
			t.Fatalf("failed to decode rendered YAML: %v\noutput:\n%s", decErr, out)
		}
		if a.Metadata.Name == "aicr-stack" {
			foundParentRepoURL = a.Spec.Source.RepoURL
		}
	}
	wantParentRepoURL := "oci://example.test/myorg/" + customName
	if foundParentRepoURL != wantParentRepoURL {
		t.Errorf("parent App repoURL: got %q, want %q (rendered repoURL must end with the Chart.yaml chart name; see #1019)",
			foundParentRepoURL, wantParentRepoURL)
	}
}

// TestHelmTemplate_AppNameOverride verifies the parent App's
// metadata.name is templated from .Values.appName so an operator can
// run two AICR bundles in the same Argo CD namespace by passing
// --set appName=<distinct> at install time. Bundle-time --app-name
// (Generator.AppName) is the chart default; install-time --set wins.
//
// Regression coverage for issue #1011 — without templating, the parent
// Application's metadata.name was the literal "aicr-stack" and the
// second bundle silently overwrote the first.
func TestHelmTemplate_AppNameOverride(t *testing.T) {
	requireHelm(t)

	tests := []struct {
		name              string
		bundleTimeAppName string
		installTimeSet    string
		wantParentName    string
	}{
		{
			name:           "default (no override) renders DefaultAppName",
			wantParentName: DefaultAppName,
		},
		{
			name:              "bundle-time AppName flows into values.yaml as the default",
			bundleTimeAppName: "gpu-runtime",
			wantParentName:    "gpu-runtime",
		},
		{
			name:              "install-time --set appName overrides the bundle-time default",
			bundleTimeAppName: "gpu-runtime",
			installTimeSet:    "ops-runtime",
			wantParentName:    "ops-runtime",
		},
		{
			name:           "install-time --set appName works without a bundle-time default",
			installTimeSet: "tenant-a",
			wantParentName: "tenant-a",
		},
		{
			// Helm's plain --set type inference: appName=true arrives as a
			// bool. The parent template pipes through `quote` and every
			// child template's collision guard coerces via `toString`, so
			// the render must succeed with the parent named "true" —
			// previously the guard's `eq` crashed with "incompatible types
			// for comparison".
			name:           "install-time --set appName=true (type-inferred bool) renders",
			installTimeSet: "true",
			wantParentName: "true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
				AppName:         tt.bundleTimeAppName,
			}
			if _, err := g.Generate(context.Background(), outputDir); err != nil {
				t.Fatalf("Generate() error = %v", err)
			}

			helmArgs := []string{"template", "test-release", outputDir,
				"--set", "repoURL=oci://example.test/myorg",
				"--set", "targetRevision=v1.0.0",
			}
			if tt.installTimeSet != "" {
				helmArgs = append(helmArgs, "--set", "appName="+tt.installTimeSet)
			}

			cmdCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(cmdCtx, "helm", helmArgs...) //nolint:gosec // controlled args
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("helm template failed: %v\noutput:\n%s", err, out)
			}

			dec := yaml.NewDecoder(strings.NewReader(string(out)))
			type appLite struct {
				Kind     string `yaml:"kind"`
				Metadata struct {
					Name      string `yaml:"name"`
					Namespace string `yaml:"namespace"`
				} `yaml:"metadata"`
				Spec struct {
					Source struct {
						RepoURL string `yaml:"repoURL"`
						Path    string `yaml:"path"`
					} `yaml:"source"`
				} `yaml:"spec"`
			}
			var parentName string
			for {
				var a appLite
				decErr := dec.Decode(&a)
				if errors.Is(decErr, io.EOF) {
					break
				}
				if decErr != nil {
					t.Fatalf("failed to decode rendered YAML: %v\noutput:\n%s", decErr, out)
				}
				// Parent App heuristic: Kind=Application in argocd namespace
				// with path="." (parent renders chart at artifact root;
				// path-based children use path=NNN-<name>).
				if a.Kind == "Application" && a.Metadata.Namespace == "argocd" && a.Spec.Source.Path == "." && strings.HasPrefix(a.Spec.Source.RepoURL, "oci://example.test/myorg") {
					parentName = a.Metadata.Name
				}
			}
			if parentName != tt.wantParentName {
				t.Errorf("parent App metadata.name: got %q, want %q\noutput:\n%s",
					parentName, tt.wantParentName, out)
			}
		})
	}
}

// TestGenerate_AppNameValidatedAtBoundary verifies the deployer boundary
// rejects an invalid AppName even when callers bypass the CLI/API
// validation layer (e.g. direct library use). Failing here keeps the
// invalid name from reaching the rendered chart's values.yaml and the
// parent App template, where it would only surface as a cryptic
// apiserver admission error at `helm install`.
func TestGenerate_AppNameValidatedAtBoundary(t *testing.T) {
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
		AppName:         "GPU_Runtime", // uppercase + underscore both reject as DNS-1123
	}
	_, err := g.Generate(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("Generate() should reject invalid DNS-1123 AppName, got nil")
	}
	if !strings.Contains(err.Error(), "DNS-1123") {
		t.Errorf("error should mention DNS-1123 validation, got: %v", err)
	}
}

// TestGenerate_InnerParentCollisionUsesEffectiveAppName verifies the
// effective parent name is forwarded to the delegated argocd.Generator so
// its parent-collision check tests children against THIS deployer's parent
// ("aicr-stack" or --app-name), not argocd's own "nvidia-stack" default.
// Without the forward, a component legitimately named "nvidia-stack"
// (possible via an external --data registry) was falsely rejected as an
// internal error.
func TestGenerate_InnerParentCollisionUsesEffectiveAppName(t *testing.T) {
	t.Run("component named nvidia-stack bundles under the default parent", func(t *testing.T) {
		// External registry declaring a component named "nvidia-stack" —
		// the embedded registry has no such component, and the inner
		// argocd generator's own default parent name is exactly
		// "nvidia-stack".
		dataDir := t.TempDir()
		registryYAML := `apiVersion: aicr.run/v1alpha2
kind: ComponentRegistry
components:
  - name: nvidia-stack
    displayName: Nvidia Stack
    valueOverrideKeys:
      - nvidiastack
    helm:
      defaultRepository: https://charts.example.com
      defaultChart: example/nvidia-stack
`
		if err := os.WriteFile(filepath.Join(dataDir, "registry.yaml"), []byte(registryYAML), 0o600); err != nil {
			t.Fatalf("WriteFile registry.yaml: %v", err)
		}
		embedded := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
		layered, err := recipe.NewLayeredDataProvider(embedded, recipe.LayeredProviderConfig{ExternalDir: dataDir})
		if err != nil {
			t.Fatalf("NewLayeredDataProvider: %v", err)
		}

		rr := newRecipeResult("v1.0.0", []recipe.ComponentRef{
			{
				Name: "nvidia-stack", Namespace: "nvidia-stack", Chart: "nvidia-stack",
				Version: "v1.0.0", Type: recipe.ComponentTypeHelm,
				Source: "https://charts.example.com",
			},
		})
		rr.DeploymentOrder = []string{"nvidia-stack"}
		rr.BindDataProvider(layered)
		g := &Generator{
			RecipeResult:    rr,
			ComponentValues: map[string]map[string]any{"nvidia-stack": {}},
			Version:         "v0.0.0-test",
		}
		if _, genErr := g.Generate(context.Background(), t.TempDir()); genErr != nil {
			t.Fatalf("Generate() error = %v; a component named \"nvidia-stack\" must not collide with THIS deployer's parent %q", genErr, DefaultAppName)
		}
	})

	t.Run("forwarded AppName is load-bearing for the collision check", func(t *testing.T) {
		g := newTestHelmGenerator(t)
		g.AppName = "gpu-operator" // collides with the recipe's only component
		_, err := g.Generate(context.Background(), t.TempDir())
		if err == nil {
			t.Fatal("expected collision error, got nil")
		}
		if !strings.Contains(err.Error(), "collides") {
			t.Errorf("error %q does not mention the parent-name collision", err.Error())
		}
	})
}

// TestHelmTemplate_MixedComponentPreChildResolvesFromOCI is the live-render
// regression test for issue #1018: a recipe with a Helm component AND
// ComponentPreManifests for that same component (the gke-cos / EKS GB200
// shape from the bug report) produces a synthetic NNN-<name>-pre folder
// rendered into a path-based child Application. Before the post-#1035
// contract, the path-based child's `source.repoURL` did not append
// `.Chart.Name`, so when the user `--set repoURL=oci://<registry>/<org>`
// (the parent namespace) Argo CD's generic OCI source resolved
// `<registry>/<org>:<tag>` — an artifact that does not exist — and the
// child stuck at `Unknown / Failed to load target state`.
//
// This test exercises the full chain: generator → helm template render
// → rendered child Application asserts. It pins both the parent and the
// `-pre` child to the same expected `<parent-namespace>/<chart-name>:
// <tag>` OCI artifact reference. The sibling upstream-helm child
// (`gpu-operator`) is also asserted to confirm its `repoURL` stays as
// the upstream Helm chart registry (not templated from .Values.repoURL).
func TestHelmTemplate_MixedComponentPreChildResolvesFromOCI(t *testing.T) {
	requireHelm(t)

	outputDir := t.TempDir()
	rr := newRecipeResult("v1.0.0", []recipe.ComponentRef{
		{
			Name: "gpu-operator", Namespace: "gpu-operator", Chart: "gpu-operator",
			Version: "v25.3.3", Type: recipe.ComponentTypeHelm,
			Source: "https://helm.ngc.nvidia.com/nvidia",
		},
	})
	rr.DeploymentOrder = []string{"gpu-operator"}

	g := &Generator{
		RecipeResult: rr,
		ComponentValues: map[string]map[string]any{
			"gpu-operator": {"driver": map[string]any{"version": "580"}},
		},
		Version: "v0.0.0-test",
		// Pre-manifest in the gke-cos / EKS GB200 shape from #1018 — a
		// Namespace primer that must apply before the gpu-operator chart.
		ComponentPreManifests: map[string]map[string][]byte{
			"gpu-operator": {
				"components/gpu-operator/manifests/kernel-module-params.yaml": []byte(
					"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: nvidia-kernel-module-params\n  namespace: gpu-operator\n",
				),
			},
		},
	}
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	const setRepoURL = "oci://example.test/myorg"
	const wantPathChildRepoURL = setRepoURL + "/" + DefaultChartName // post-#1035 fix
	const wantTagName = "v9.9.9-issue-1018"

	cmdCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "helm", "template", "test-release", outputDir, //nolint:gosec // controlled args
		"--set", "repoURL="+setRepoURL,
		"--set", "targetRevision="+wantTagName,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\noutput:\n%s", err, out)
	}

	dec := yaml.NewDecoder(strings.NewReader(string(out)))
	type appLite struct {
		Kind     string                `yaml:"kind"`
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
		if a.Kind == "Application" && a.Metadata.Name != "" {
			found[a.Metadata.Name] = a
		}
	}

	// Parent: native-OCI source shape (repoURL = namespace + "/" + chart,
	// no source.chart, path = ".").
	parent, ok := found["aicr-stack"]
	if !ok {
		t.Fatalf("rendered output missing parent Application 'aicr-stack'\noutput:\n%s", out)
	}
	wantParentRepoURL := setRepoURL + "/" + DefaultChartName
	if parent.Spec.Source.RepoURL != wantParentRepoURL {
		t.Errorf("parent App repoURL: got %q, want %q (parent namespace + chart name; native-OCI source)", parent.Spec.Source.RepoURL, wantParentRepoURL)
	}
	if parent.Spec.Source.Chart != "" {
		t.Errorf("parent App chart: got %q, want empty (native-OCI mode ignores source.chart)", parent.Spec.Source.Chart)
	}
	if parent.Spec.Source.Path != "." {
		t.Errorf("parent App path: got %q, want %q", parent.Spec.Source.Path, ".")
	}

	// The path-based -pre child is the #1018 regression target. Argo CD's
	// generic OCI source treats `repoURL` as the full artifact, so the
	// rendered value MUST equal `<parent-namespace>/<chart-name>` for the
	// `<reg>/<org>/<chart>:<tag>` lookup to resolve.
	preChild, ok := found["gpu-operator-pre"]
	if !ok {
		t.Fatalf("rendered output missing path-based child 'gpu-operator-pre'\noutput:\n%s", out)
	}
	if preChild.Spec.Source.RepoURL != wantPathChildRepoURL {
		t.Errorf("gpu-operator-pre child repoURL: got %q, want %q (issue #1018: must be parent-namespace + chart-name so the generic OCI source resolves to the published artifact)", preChild.Spec.Source.RepoURL, wantPathChildRepoURL)
	}
	if preChild.Spec.Source.Path != "001-gpu-operator-pre" {
		t.Errorf("gpu-operator-pre path: got %q, want %q (NNN-folder name is structural; must not be templated)", preChild.Spec.Source.Path, "001-gpu-operator-pre")
	}
	if preChild.Spec.Source.TargetRevision != wantTagName {
		t.Errorf("gpu-operator-pre child targetRevision: got %q, want %q", preChild.Spec.Source.TargetRevision, wantTagName)
	}

	// The sibling upstream-helm child must keep the upstream chart
	// registry — its source.repoURL is NOT templated from .Values.repoURL.
	// Guards against accidentally widening the path-based template change.
	upstream, ok := found["gpu-operator"]
	if !ok {
		t.Fatalf("rendered output missing upstream-helm child 'gpu-operator'\noutput:\n%s", out)
	}
	if upstream.Spec.Source.RepoURL != "https://helm.ngc.nvidia.com/nvidia" {
		t.Errorf("gpu-operator upstream-helm child repoURL: got %q, want upstream Helm registry (must not be templated from .Values.repoURL)", upstream.Spec.Source.RepoURL)
	}
}

// TestHelmTemplate_FailsWithoutRepoURL verifies the `required` directive
// in the parent App template fires when the user omits --set repoURL. This
// is the safety net that prevents users from accidentally publishing a
// chart whose Application would point at an empty URL.
func TestHelmTemplate_FailsWithoutRepoURL(t *testing.T) {
	requireHelm(t)

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

func readinessGateManifest(t *testing.T, deployer config.DeployerType) []byte {
	t.Helper()
	manifest, err := gatemanifest.Render(
		"gpu-operator",
		"ghcr.io/nvidia/aicr-gate:v0.0.0-golden",
		[]byte(`apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: gpu-operator-readiness
`),
		deployer,
	)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return manifest
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

// TestInjectValuesIntoSingleSource_AppendsChartName pins the
// post-#1032 path-based-children fix from issue #1034: path-based
// child Applications have no `chart` field, so Argo CD's generic OCI
// source uses `repoURL` directly as the full artifact reference. The
// rendered template must therefore append .Chart.Name to the
// install-time --set repoURL value so the assembled URL matches the
// artifact the parent Application's `repoURL/chart:tag` triple
// resolves to. Without the append, --set repoURL=oci://reg/org (the
// contract documented elsewhere in this file) produces a child
// source pointing at `oci://reg/org:tag` — an artifact that does not
// exist — and the child Application fails to sync.
func TestInjectValuesIntoSingleSource_AppendsChartName(t *testing.T) {
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

	// The .Values.repoURL expression may be wrapped in a trimSuffix
	// pipeline (defense-in-depth against operator-supplied trailing
	// slashes); the load-bearing assertion is that .Chart.Name is
	// appended directly so the assembled URL matches the artifact the
	// parent App's repoURL/chart:tag triple resolves to.
	if !strings.Contains(str, `}}/{{ .Chart.Name }}`) {
		t.Errorf("path-based child repoURL should append /{{ .Chart.Name }} after the .Values.repoURL expression so the rendered value is the full OCI artifact reference; got:\n%s", str)
	}
	// The error message must direct callers to pass the parent
	// namespace (the same contract the parent Application uses) so
	// users don't try to bake the chart name into --set repoURL.
	if !strings.Contains(str, "do NOT include the chart name") {
		t.Errorf("required-message must instruct callers not to include the chart name in --set repoURL; got:\n%s", str)
	}
}

// TestWriteChartYAML_QuotesYAMLReservedScalarsAsName documents the
// chart-name quoting fix from issue #1034. Valid OCI artifact paths
// whose last segment is a YAML reserved scalar ("null", "true",
// "false", numeric strings, etc.) must round-trip as strings; if
// emitted unquoted, Helm's YAML parser reinterprets `name: null` as
// YAML null, chart.Metadata.Name becomes empty, and the chart is
// rejected by `helm package` / `helm push` with "chart.metadata.name
// is required".
func TestWriteChartYAML_QuotesYAMLReservedScalarsAsName(t *testing.T) {
	tests := []struct {
		name         string
		chartName    string
		chartVersion string
	}{
		{"YAML null literal name", "null", "1.0.0"},
		{"YAML true literal name", "true", "1.0.0"},
		{"YAML false literal name", "false", "1.0.0"},
		{"numeric-looking name", "123", "1.0.0"},
		{"YAML yes literal name", "yes", "1.0.0"},
		// Versions like "1.0" reparse as the YAML float 1 without quoting,
		// so the %q wrap on version is load-bearing, not cosmetic. Exercise
		// that branch with the same round-trip assertion as for name.
		{"float-looking version", "aicr-bundle", "1.0"},
		{"numeric-looking version", "aicr-bundle", "123"},
		{"hyphenated normal pair", "my-bundle", "1.2.3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outputDir := t.TempDir()
			if _, _, err := writeChartYAML(outputDir, tt.chartName, tt.chartVersion); err != nil {
				t.Fatalf("writeChartYAML: %v", err)
			}
			content, err := os.ReadFile(filepath.Join(outputDir, "Chart.yaml"))
			if err != nil {
				t.Fatalf("read Chart.yaml: %v", err)
			}
			var parsed struct {
				APIVersion string `yaml:"apiVersion"`
				Name       string `yaml:"name"`
				Version    string `yaml:"version"`
			}
			if err := yaml.Unmarshal(content, &parsed); err != nil {
				t.Fatalf("Chart.yaml does not unmarshal cleanly for name=%q version=%q:\n%s\nerror: %v",
					tt.chartName, tt.chartVersion, content, err)
			}
			if parsed.Name != tt.chartName {
				t.Errorf("Chart.yaml name = %q (after YAML unmarshal), want %q\nraw:\n%s",
					parsed.Name, tt.chartName, content)
			}
			if parsed.Version != tt.chartVersion {
				t.Errorf("Chart.yaml version = %q (after YAML unmarshal), want %q\nraw:\n%s",
					parsed.Version, tt.chartVersion, content)
			}
		})
	}
}

// newTestHelmGenerator returns a minimal single-Helm-component Generator
// fixture for deployer-option tests. Callers set the deployer option
// fields (NamePrefix, DestinationServer, Project, CascadeDelete) before
// calling Generate.
func newTestHelmGenerator(t *testing.T) *Generator {
	t.Helper()
	rr := newRecipeResult("v1.0.0", []recipe.ComponentRef{
		{
			Name: "gpu-operator", Namespace: "gpu-operator", Chart: "gpu-operator",
			Version: "v25.3.3", Type: recipe.ComponentTypeHelm,
			Source: "https://helm.ngc.nvidia.com/nvidia",
		},
	})
	rr.DeploymentOrder = []string{"gpu-operator"}
	return &Generator{
		RecipeResult: rr,
		ComponentValues: map[string]map[string]any{
			"gpu-operator": {"driver": map[string]any{"version": "580"}},
		},
		Version: "v0.0.0-test",
	}
}

// readBundleFile reads a bundle-relative file, failing the test on error.
func readBundleFile(t *testing.T, outputDir, rel string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(outputDir, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return data
}

func TestGenerate_DeployerValuesInChart(t *testing.T) {
	outputDir := t.TempDir()
	g := newTestHelmGenerator(t)
	g.NamePrefix = "tenant-a-"
	g.DestinationServer = "https://remote.example.com:6443"
	g.Project = "tenant-a"
	g.CascadeDelete = true

	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	values := readBundleFile(t, outputDir, "values.yaml")
	for _, want := range []string{"deployer:", "namePrefix: tenant-a-",
		"destinationServer: https://remote.example.com:6443", "project: tenant-a"} {
		if !strings.Contains(string(values), want) {
			t.Errorf("values.yaml missing %q\n%s", want, values)
		}
	}
	// Key form only — the doc header comment deliberately mentions
	// cascadeDelete to explain that it is bundle-time only.
	if strings.Contains(string(values), "cascadeDelete:") {
		t.Error("cascadeDelete key must not appear in values.yaml (bundle-time only)")
	}

	child := readBundleFile(t, outputDir, "templates/gpu-operator.yaml")
	for _, want := range []string{
		`{{ (.Values.deployer | default dict).namePrefix | default "" }}gpu-operator`,
		`(.Values.deployer | default dict).destinationServer`,
		`(.Values.deployer | default dict).project`,
		"resources-finalizer.argocd.argoproj.io",
	} {
		if !strings.Contains(string(child), want) {
			t.Errorf("child template missing %q\n%s", want, child)
		}
	}

	parent := readBundleFile(t, outputDir, "templates/aicr-stack.yaml")
	if !strings.Contains(string(parent), "resources-finalizer.argocd.argoproj.io") {
		t.Error("parent template missing finalizer when CascadeDelete set")
	}
	for _, reject := range []string{"tenant-a-", "remote.example.com"} {
		if strings.Contains(string(parent), reject) {
			t.Errorf("parent template unexpectedly contains %q", reject)
		}
	}
}

func TestGenerate_DeployerDefaults_NoOptions(t *testing.T) {
	outputDir := t.TempDir()
	g := newTestHelmGenerator(t)
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	values := readBundleFile(t, outputDir, "values.yaml")
	// Defaults documented in values.yaml even without overrides.
	for _, want := range []string{"destinationServer: https://kubernetes.default.svc", "project: default",
		"includeRootApp: true"} {
		if !strings.Contains(string(values), want) {
			t.Errorf("values.yaml missing default %q\n%s", want, values)
		}
	}
	parent := readBundleFile(t, outputDir, "templates/aicr-stack.yaml")
	if strings.Contains(string(parent), "finalizers") {
		t.Error("parent template must not carry finalizers by default")
	}
	if !strings.Contains(string(parent), `dig "includeRootApp" true`) {
		t.Error("parent template missing includeRootApp gate")
	}

	schema := readBundleFile(t, outputDir, "values.schema.json")
	if !strings.Contains(string(schema), `"includeRootApp"`) {
		t.Error("values.schema.json missing includeRootApp property (additionalProperties:false would reject the --set)")
	}
}

// TestHelmTemplate_DeployerNamePrefixOverride is the live-render check for
// the install-time deployer.* vocabulary: `helm template --set
// deployer.namePrefix=t-` must render the child Application's
// metadata.name as `t-gpu-operator`, and project / destinationServer
// overrides must land on the child spec. Skipped when helm is not on PATH.
func TestHelmTemplate_DeployerNamePrefixOverride(t *testing.T) {
	requireHelm(t)

	outputDir := t.TempDir()
	g := newTestHelmGenerator(t)
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	cmdCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "helm", "template", "test-release", outputDir, //nolint:gosec // controlled args
		"--set", "repoURL=oci://example.test/myorg",
		"--set", "targetRevision=v1.0.0",
		"--set", "deployer.namePrefix=t-",
		"--set", "deployer.project=tenant-a",
		"--set", "deployer.destinationServer=https://remote.example.com:6443",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\noutput:\n%s", err, out)
	}

	dec := yaml.NewDecoder(strings.NewReader(string(out)))
	type appLite struct {
		Kind     string                `yaml:"kind"`
		Metadata struct{ Name string } `yaml:"metadata"`
		Spec     struct {
			Project     string `yaml:"project"`
			Destination struct {
				Server string `yaml:"server"`
			} `yaml:"destination"`
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
		if a.Kind == "Application" && a.Metadata.Name != "" {
			found[a.Metadata.Name] = a
		}
	}

	child, ok := found["t-gpu-operator"]
	if !ok {
		t.Fatalf("rendered output missing prefixed child 't-gpu-operator'\noutput:\n%s", out)
	}
	if child.Spec.Project != "tenant-a" {
		t.Errorf("child spec.project: got %q, want %q", child.Spec.Project, "tenant-a")
	}
	if child.Spec.Destination.Server != "https://remote.example.com:6443" {
		t.Errorf("child destination.server: got %q, want %q", child.Spec.Destination.Server, "https://remote.example.com:6443")
	}

	// Parent Application stays unprefixed and on the control-plane cluster.
	if _, ok := found[DefaultAppName]; !ok {
		t.Errorf("rendered output missing unprefixed parent %q", DefaultAppName)
	}
}

// TestHelmTemplate_IncludeRootAppToggle is the live-render check for
// deployer.includeRootApp (#1723): default renders the parent app-of-apps,
// `--set deployer.includeRootApp=false` renders children-only so an
// externally managed root Application pointing at the published chart does
// not fight the bundled parent over the same children, and a string value
// fails the values schema loudly instead of rendering as truthy.
// Skipped when helm is not on PATH.
func TestHelmTemplate_IncludeRootAppToggle(t *testing.T) {
	requireHelm(t)

	outputDir := t.TempDir()
	g := newTestHelmGenerator(t)
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	render := func(t *testing.T, extraArgs ...string) (string, error) {
		t.Helper()
		cmdCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		args := append([]string{"template", "test-release", outputDir,
			"--set", "repoURL=oci://example.test/myorg",
			"--set", "targetRevision=v1.0.0",
		}, extraArgs...)
		cmd := exec.CommandContext(cmdCtx, "helm", args...) //nolint:gosec // controlled args
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	appNames := func(t *testing.T, rendered string) map[string]bool {
		t.Helper()
		dec := yaml.NewDecoder(strings.NewReader(rendered))
		type appLite struct {
			Kind     string                `yaml:"kind"`
			Metadata struct{ Name string } `yaml:"metadata"`
		}
		names := map[string]bool{}
		for {
			var a appLite
			decErr := dec.Decode(&a)
			if errors.Is(decErr, io.EOF) {
				break
			}
			if decErr != nil {
				t.Fatalf("failed to decode rendered YAML: %v\noutput:\n%s", decErr, rendered)
			}
			if a.Kind == "Application" && a.Metadata.Name != "" {
				names[a.Metadata.Name] = true
			}
		}
		return names
	}

	t.Run("default renders parent and children", func(t *testing.T) {
		out, err := render(t)
		if err != nil {
			t.Fatalf("helm template failed: %v\noutput:\n%s", err, out)
		}
		names := appNames(t, out)
		if !names[DefaultAppName] {
			t.Errorf("default render missing parent %q\noutput:\n%s", DefaultAppName, out)
		}
		if !names["gpu-operator"] {
			t.Errorf("default render missing child gpu-operator\noutput:\n%s", out)
		}
	})

	t.Run("includeRootApp=false renders children-only", func(t *testing.T) {
		out, err := render(t, "--set", "deployer.includeRootApp=false")
		if err != nil {
			t.Fatalf("helm template failed: %v\noutput:\n%s", err, out)
		}
		names := appNames(t, out)
		if names[DefaultAppName] {
			t.Errorf("parent %q rendered despite includeRootApp=false\noutput:\n%s", DefaultAppName, out)
		}
		if !names["gpu-operator"] {
			t.Errorf("children must still render with includeRootApp=false\noutput:\n%s", out)
		}
	})

	t.Run("string value fails values schema", func(t *testing.T) {
		out, err := render(t, "--set-string", "deployer.includeRootApp=false")
		if err == nil {
			t.Fatalf("expected helm template to fail schema validation for string value, but it succeeded:\n%s", out)
		}
	})
}

// TestGenerate_ChildNameLimits verifies the bundle-time guards for
// composed child Application names in the argocd-helm path: names over
// Helm's 53-character release-name cap and names colliding with the
// parent Application are rejected with ErrCodeInvalidRequest. The
// install-time equivalents are covered by
// TestHelmTemplate_ChildNameGuards below.
func TestGenerate_ChildNameLimits(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Generator)
		errSubstr string
	}{
		{
			name:      "composed name exceeds Helm release-name cap",
			mutate:    func(g *Generator) { g.NamePrefix = strings.Repeat("a", 49) + "-" },
			errSubstr: "53",
		},
		{
			name: "child name collides with parent app name",
			mutate: func(g *Generator) {
				g.AppName = "tenant-gpu-operator"
				g.NamePrefix = "tenant-"
			},
			errSubstr: "collides",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newTestHelmGenerator(t)
			tt.mutate(g)
			_, err := g.Generate(context.Background(), t.TempDir())
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), tt.errSubstr) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.errSubstr)
			}
		})
	}
}

// TestGenerate_NamePrefixValidatedUpfront verifies the boundary check
// rejects a malformed NamePrefix even when the recipe produces no
// NNN-folders (the per-folder validation in processFolders never runs).
func TestGenerate_NamePrefixValidatedUpfront(t *testing.T) {
	rr := newRecipeResult("v1.0.0", nil)
	g := &Generator{
		RecipeResult: rr,
		Version:      "v0.0.0-test",
		NamePrefix:   "-Bad_Prefix",
	}
	_, err := g.Generate(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "namePrefix") {
		t.Errorf("error %q does not mention namePrefix", err.Error())
	}
}

// TestHelmTemplate_ChildNameGuards is the install-time counterpart to
// TestGenerate_ChildNameLimits: the bundle bakes valid deployer defaults,
// but `helm template --set deployer.namePrefix=...` can still compose a
// child name over Helm's release-name cap or one that collides with the
// parent Application. The guard block prepended to every child template
// must fail the render. Skipped when helm is not on PATH.
func TestHelmTemplate_ChildNameGuards(t *testing.T) {
	requireHelm(t)

	outputDir := t.TempDir()
	g := newTestHelmGenerator(t)
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	tests := []struct {
		name      string
		extraSets []string
		errSubstr string
	}{
		{
			name:      "namePrefix over release-name cap fails render",
			extraSets: []string{"--set", "deployer.namePrefix=" + strings.Repeat("a", 49) + "-"},
			errSubstr: "53",
		},
		{
			name: "child name colliding with parent appName fails render",
			extraSets: []string{
				"--set", "deployer.namePrefix=tenant-",
				"--set", "appName=tenant-gpu-operator",
			},
			errSubstr: "collides",
		},
		{
			name:      "unknown deployer key fails schema validation",
			extraSets: []string{"--set", "deployer.destinationSever=https://x"},
			errSubstr: "destinationSever",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"template", "test-release", outputDir,
				"--set", "repoURL=oci://example.test/myorg",
				"--set", "targetRevision=v1.0.0",
			}
			args = append(args, tt.extraSets...)
			cmdCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(cmdCtx, "helm", args...) //nolint:gosec // controlled args
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("expected helm template to fail, but it succeeded:\n%s", out)
			}
			if !strings.Contains(string(out), tt.errSubstr) {
				t.Errorf("helm error output does not mention %q:\n%s", tt.errSubstr, out)
			}
		})
	}
}

// TestValuesSchemaPatterns compiles the values.schema.json regex patterns
// with Go's regexp package and verifies they align with the bundle-time Go
// validators: destinationServer must reject embedded credentials, and
// project must enforce per-label DNS-1123 subdomain rules (no empty labels,
// labels capped at 63 chars).
func TestValuesSchemaPatterns(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := writeValuesSchema(dir); err != nil {
		t.Fatalf("writeValuesSchema() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "values.schema.json"))
	if err != nil {
		t.Fatalf("read values.schema.json: %v", err)
	}
	var schema valuesSchema
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("unmarshal values.schema.json: %v", err)
	}
	props := schema.Properties.Deployer.Properties

	// Pin the fail-closed shape independent of the golden fixtures and the
	// (skippable) live helm test: without additionalProperties: false, an
	// unknown deployer.* key would silently pass install-time validation.
	// The raw-bytes check matters — a future `omitempty` on the struct field
	// would drop the key entirely (JSON Schema then defaults to allowing
	// unknown properties), and an unmarshal-based check could not see that.
	if schema.Properties.Deployer.AdditionalProperties {
		t.Error("deployer.additionalProperties must be false to fail unknown keys closed")
	}
	if !strings.Contains(string(data), `"additionalProperties": false`) {
		t.Error(`values.schema.json must emit "additionalProperties": false explicitly; omitting it fails open`)
	}

	tests := []struct {
		name    string
		pattern string
		input   string
		match   bool
	}{
		{"destinationServer accepts plain https host", props.DestinationServer.Pattern, "https://kubernetes.default.svc", true},
		{"destinationServer accepts explicit empty (reset to baked default)", props.DestinationServer.Pattern, "", true},
		{"destinationServer accepts host with port", props.DestinationServer.Pattern, "https://api.example.com:6443", true},
		{"destinationServer rejects embedded credentials", props.DestinationServer.Pattern, "https://u:p@host:6443", false},
		{"destinationServer rejects port without hostname", props.DestinationServer.Pattern, "https://:6443", false},
		{"destinationServer rejects path without hostname", props.DestinationServer.Pattern, "https:///path", false},
		{"project accepts single label", props.Project.Pattern, "default", true},
		{"project accepts dotted subdomain", props.Project.Pattern, "team-a.prod", true},
		{"project rejects empty label", props.Project.Pattern, "a..b", false},
		// Per-label caps are deliberately NOT enforced: IsDNS1123Subdomain
		// only caps the total length, and a 64+-char label is a legal
		// Kubernetes object name — an AppProject with a 70-char name can
		// exist, so rejecting the reference would be a false positive.
		{"project accepts 70-char label (legal k8s object name)", props.Project.Pattern, strings.Repeat("a", 70), true},
		{"project accepts 63-char label", props.Project.Pattern, strings.Repeat("a", 63), true},
		{"project accepts explicit empty (reset to baked default)", props.Project.Pattern, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			re, err := regexp.Compile(tt.pattern)
			if err != nil {
				t.Fatalf("pattern %q does not compile: %v", tt.pattern, err)
			}
			if got := re.MatchString(tt.input); got != tt.match {
				t.Errorf("pattern %q match(%q) = %v, want %v", tt.pattern, tt.input, got, tt.match)
			}
		})
	}
}

// TestValidationContractParity is the live contract test between the two
// independent validation gates for deployer.* options:
//
//   - bundle time: the Go validators behind config.ParseArgoDeployerOptions
//     (ValidateNamePrefix, ValidateDestinationServer, ValidateProject).
//   - install time: values.schema.json patterns enforced by
//     `helm template` (plus the template guards).
//
// The two have repeatedly drifted (see #1625/#1628 follow-ups); this test
// closes the class by running BOTH gates against the same value and
// asserting each equals an explicit wantValid — merely asserting the gates
// agree would let a case where both wrongly accept a bad value pass.
func TestValidationContractParity(t *testing.T) {
	requireHelm(t)

	outputDir := t.TempDir()
	g := newTestHelmGenerator(t)
	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// helmTemplate renders the generated bundle with one extra deployer.*
	// assignment (setFlag is --set-string for byte-identical string parity,
	// or --set for Helm's type-inference cases) and reports render output
	// and success. Bounded by an exec timeout like the other live tests.
	helmTemplate := func(t *testing.T, setFlag, key, value string) (string, bool) {
		t.Helper()
		cmdCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cmdCtx, "helm", "template", "test-release", outputDir, //nolint:gosec // controlled args
			"--set", "repoURL=oci://example.test/myorg",
			"--set", "targetRevision=v1.0.0",
			setFlag, "deployer."+key+"="+value,
		)
		out, err := cmd.CombinedOutput()
		if cmdCtx.Err() != nil {
			t.Fatalf("helm template timed out: %v\noutput:\n%s", err, out)
		}
		return string(out), err == nil
	}

	t.Run("string values", func(t *testing.T) {
		tests := []struct {
			name      string
			key       string
			value     string
			wantValid bool
		}{
			{"destinationServer valid https URL", "destinationServer", "https://api.example.com:6443", true},
			{"destinationServer embedded credentials", "destinationServer", "https://u:p@host:6443", false},
			// Port with no hostname is never a usable API endpoint; both
			// gates fail it closed (ValidateHTTPSURL checks u.Hostname(),
			// the schema pattern forbids ':' right after https://).
			{"destinationServer port without hostname", "destinationServer", "https://:6443", false},
			// url.Parse lowercases the scheme, so only an explicit prefix
			// check keeps the Go validator aligned with the case-sensitive
			// schema pattern.
			{"destinationServer uppercase scheme", "destinationServer", "HTTPS://host:6443", false},
			// '@' is rejected anywhere (not just userinfo) because the
			// schema regex cannot distinguish authority from path.
			{"destinationServer at-sign in path", "destinationServer", "https://host/path@thing", false},
			{"project valid dotted subdomain", "project", "team-a.prod", true},
			{"project empty label", "project", "a..b", false},
			// Both gates mirror IsDNS1123Subdomain exactly: a 64-char
			// label is a legal Kubernetes object name (only the 253-char
			// total is capped), so both gates must accept it.
			{"project 64-char label", "project", strings.Repeat("a", 64), true},
			{"namePrefix valid trailing hyphen", "namePrefix", "tenant-a-", true},
			{"namePrefix uppercase", "namePrefix", "Tenant-", false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				// Bundle-time gate: single-key override map through the same
				// entry point the CLI/API use for --set deployer:<key>=<value>.
				_, err := config.ParseArgoDeployerOptions(map[string]string{tt.key: tt.value})
				if goValid := err == nil; goValid != tt.wantValid {
					t.Errorf("bundle-time Go validator: valid=%v, want %v (err=%v)", goValid, tt.wantValid, err)
				}
				// Install-time gate: --set-string keeps the value a string so
				// both gates judge the identical bytes.
				out, helmValid := helmTemplate(t, "--set-string", tt.key, tt.value)
				if helmValid != tt.wantValid {
					t.Errorf("install-time helm template: valid=%v, want %v\noutput:\n%s", helmValid, tt.wantValid, out)
				}
			})
		}
	})

	// Helm's plain --set type-inference: `--set deployer.project=true`
	// delivers a bool (not the string "true") to schema validation. The
	// schema's `type: string` must reject every inferred non-string —
	// otherwise a value the bundle-time gate could never even see (its
	// overrides are always strings) slips through at install time.
	t.Run("helm type inference rejected by schema", func(t *testing.T) {
		for _, value := range []string{"true", "false", "123", "0"} {
			t.Run("project="+value, func(t *testing.T) {
				out, helmValid := helmTemplate(t, "--set", "project", value)
				if helmValid {
					t.Errorf("helm template accepted --set deployer.project=%s; schema type:string must reject the inferred non-string\noutput:\n%s", value, out)
				}
			})
		}
	})

	// `--set deployer.project=null` is different: Helm deletes the key from
	// the coalesced values instead of passing a null, so the schema never
	// sees a value and the child template's `| default "default"` fallback
	// renders the baked default. That render success is acceptable — the
	// user gets the chart default, not a malformed project — so this case
	// intentionally expects success and pins the fallback value.
	t.Run("project=null deletes key and falls back to baked default", func(t *testing.T) {
		out, helmValid := helmTemplate(t, "--set", "project", "null")
		if !helmValid {
			t.Fatalf("helm template failed for --set deployer.project=null; expected key deletion + default fallback\noutput:\n%s", out)
		}
		if !strings.Contains(out, "project: 'default'") {
			t.Errorf("rendered output should fall back to the baked default project after null deletes the key; got:\n%s", out)
		}
	})

	// An explicit-empty install-time value passes the schema's `^$|`
	// alternative and resets to the baked default via the child
	// template's `| default` fallback. The CLI cannot produce this case
	// (ParseArgoDeployerOptions rejects an empty project, and the
	// component-path parser rejects empty values), so it is install-time
	// only — pin the fallback rendering here.
	t.Run("explicit-empty project renders baked default", func(t *testing.T) {
		out, helmValid := helmTemplate(t, "--set-string", "project", "")
		if !helmValid {
			t.Fatalf("helm template failed for --set-string deployer.project=\"\"; expected schema to accept empty and template to fall back\noutput:\n%s", out)
		}
		if !strings.Contains(out, "project: 'default'") {
			t.Errorf("rendered output should fall back to the baked default project for an explicit-empty value; got:\n%s", out)
		}
	})

	t.Run("explicit-empty destinationServer renders in-cluster default", func(t *testing.T) {
		out, helmValid := helmTemplate(t, "--set-string", "destinationServer", "")
		if !helmValid {
			t.Fatalf("helm template failed for --set-string deployer.destinationServer=\"\"; expected schema to accept empty and template to fall back\noutput:\n%s", out)
		}
		if !strings.Contains(out, "server: 'https://kubernetes.default.svc'") {
			t.Errorf("rendered output should fall back to the in-cluster destination server for an explicit-empty value; got:\n%s", out)
		}
	})
}
