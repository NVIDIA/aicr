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

package argocd

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const testVersion = "v1.0.0"

// update regenerates goldens under testdata/ when set via `go test -update`.
// Same convention as helm and localformat deployer test suites.
var update = flag.Bool("update", false, "update golden files")

func TestGenerate_Success(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "cert-manager",
			Namespace: "cert-manager",
			Chart:     "cert-manager",
			Version:   "v1.17.2",
			Type:      "helm",
			Source:    "https://charts.jetstack.io",
		},
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      "helm",
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}
	recipeResult.DeploymentOrder = []string{"cert-manager", "gpu-operator"}

	g := &Generator{
		RecipeResult: recipeResult,
		ComponentValues: map[string]map[string]any{
			"cert-manager": {
				"crds": map[string]any{"enabled": true},
			},
			"gpu-operator": {
				"driver": map[string]any{
					"enabled": true,
				},
			},
		},
		Version: "v0.9.0",
	}

	output, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Verify output
	if output == nil {
		t.Fatal("Generate() returned nil output")
	}

	if len(output.Files) == 0 {
		t.Error("Generate() returned no files")
	}

	if output.TotalSize == 0 {
		t.Error("Generate() returned zero total size")
	}

	if output.Duration == 0 {
		t.Error("Generate() returned zero duration")
	}

	// Verify expected files exist
	expectedFiles := []string{
		"001-cert-manager/application.yaml",
		"001-cert-manager/values.yaml",
		"002-gpu-operator/application.yaml",
		"002-gpu-operator/values.yaml",
		"app-of-apps.yaml",
		"README.md",
	}

	for _, relPath := range expectedFiles {
		fullPath := filepath.Join(outputDir, relPath)
		if _, statErr := os.Stat(fullPath); os.IsNotExist(statErr) {
			t.Errorf("Expected file %s does not exist", relPath)
		}
	}

	// Verify generated application.yaml files are valid YAML. (Sync-wave
	// content and full Application shape are frozen by TestBundleGolden_*;
	// what's left here is the basic "files-exist + parseable" sanity check.)
	assertValidYAML(t, filepath.Join(outputDir, "001-cert-manager", "application.yaml"))
	assertValidYAML(t, filepath.Join(outputDir, "002-gpu-operator", "application.yaml"))

	// Verify README contains component information. README is not golden-
	// tested because it carries operator guidance with absolute paths
	// (TempDir-dependent) that wouldn't survive byte-comparison.
	readmePath := filepath.Join(outputDir, "README.md")
	content, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("Failed to read README: %v", err)
	}
	if !strings.Contains(string(content), "cert-manager") {
		t.Error("README should contain cert-manager")
	}
	if !strings.Contains(string(content), "gpu-operator") {
		t.Error("README should contain gpu-operator")
	}
}

func TestGenerate_NilRecipeResult(t *testing.T) {
	g := &Generator{
		Version: "v0.9.0",
	}
	ctx := context.Background()
	outputDir := t.TempDir()

	_, err := g.Generate(ctx, outputDir)
	if err == nil {
		t.Fatal("Generate() should return error for nil recipe result")
	}
}

func TestGenerate_EmptyComponents(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{}

	g := &Generator{
		RecipeResult: recipeResult,
		Version:      "v0.9.0",
	}

	output, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Should still generate app-of-apps and README
	expectedFiles := []string{
		"app-of-apps.yaml",
		"README.md",
	}

	for _, relPath := range expectedFiles {
		fullPath := filepath.Join(outputDir, relPath)
		if _, statErr := os.Stat(fullPath); os.IsNotExist(statErr) {
			t.Errorf("Expected file %s does not exist", relPath)
		}
	}

	// Verify file count
	if len(output.Files) != 2 {
		t.Errorf("Expected 2 files, got %d", len(output.Files))
	}
}

func TestGenerate_WithRepoURL(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	customRepoURL := "https://github.com/my-org/my-gitops-repo.git"

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      "helm",
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}

	g := &Generator{
		RecipeResult: recipeResult,
		Version:      "v0.9.0",
		RepoURL:      customRepoURL,
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Verify app-of-apps contains custom repo URL
	appOfAppsPath := filepath.Join(outputDir, "app-of-apps.yaml")
	content, err := os.ReadFile(appOfAppsPath)
	if err != nil {
		t.Fatalf("Failed to read app-of-apps.yaml: %v", err)
	}
	if !strings.Contains(string(content), customRepoURL) {
		t.Error("app-of-apps.yaml should contain custom repo URL")
	}

	// Verify child application.yaml contains custom repo URL in values source
	gpuOperatorApp := filepath.Join(outputDir, "001-gpu-operator", "application.yaml")
	appContent, err := os.ReadFile(gpuOperatorApp)
	if err != nil {
		t.Fatalf("Failed to read gpu-operator application.yaml: %v", err)
	}
	if !strings.Contains(string(appContent), customRepoURL) {
		t.Errorf("application.yaml should contain custom repo URL %s, got:\n%s", customRepoURL, string(appContent))
	}
}

func TestGenerate_WithOCIRepoURL(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	ociRepoURL := "nvcr.io/foo/aicr-bundles"
	ociTag := "v0.0.1"

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      "helm",
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}

	g := &Generator{
		RecipeResult:    recipeResult,
		ComponentValues: map[string]map[string]any{"gpu-operator": {}},
		Version:         "v0.9.0",
		RepoURL:         ociRepoURL,
		TargetRevision:  ociTag,
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Verify app-of-apps uses OCI repo URL and tag
	appOfApps, err := os.ReadFile(filepath.Join(outputDir, "app-of-apps.yaml"))
	if err != nil {
		t.Fatalf("Failed to read app-of-apps.yaml: %v", err)
	}
	if !strings.Contains(string(appOfApps), ociRepoURL) {
		t.Error("app-of-apps.yaml should contain OCI repo URL")
	}
	if !strings.Contains(string(appOfApps), ociTag) {
		t.Error("app-of-apps.yaml should contain OCI tag as targetRevision")
	}

	// Verify child application uses OCI repo URL and tag
	gpuApp, err := os.ReadFile(filepath.Join(outputDir, "001-gpu-operator", "application.yaml"))
	if err != nil {
		t.Fatalf("Failed to read gpu-operator application.yaml: %v", err)
	}
	gpuAppStr := string(gpuApp)
	if !strings.Contains(gpuAppStr, ociRepoURL) {
		t.Errorf("application.yaml should contain OCI repo URL, got:\n%s", gpuAppStr)
	}
	if !strings.Contains(gpuAppStr, ociTag) {
		t.Errorf("application.yaml should contain OCI tag as targetRevision, got:\n%s", gpuAppStr)
	}
	if strings.Contains(gpuAppStr, "{{ .RepoURL }}") {
		t.Error("application.yaml should not contain literal {{ .RepoURL }} placeholder")
	}
}

func TestGenerate_DefaultRepoURL_InChildApplications(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      "helm",
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}

	g := &Generator{
		RecipeResult:    recipeResult,
		ComponentValues: map[string]map[string]any{"gpu-operator": {}},
		Version:         "v0.9.0",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	gpuApp, err := os.ReadFile(filepath.Join(outputDir, "001-gpu-operator", "application.yaml"))
	if err != nil {
		t.Fatalf("Failed to read application.yaml: %v", err)
	}
	gpuAppStr := string(gpuApp)
	if !strings.Contains(gpuAppStr, "YOUR-ORG/YOUR-REPO") {
		t.Errorf("application.yaml should contain placeholder URL, got:\n%s", gpuAppStr)
	}
	if strings.Contains(gpuAppStr, "{{ .RepoURL }}") {
		t.Error("application.yaml should not contain literal {{ .RepoURL }} placeholder")
	}
}

func TestGenerate_WithChecksums(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "cert-manager",
			Namespace: "cert-manager",
			Chart:     "cert-manager",
			Version:   "v1.17.2",
			Type:      "helm",
			Source:    "https://charts.jetstack.io",
		},
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      "helm",
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}
	recipeResult.DeploymentOrder = []string{"cert-manager", "gpu-operator"}

	g := &Generator{
		RecipeResult:     recipeResult,
		Version:          "v0.9.0",
		IncludeChecksums: true,
	}

	output, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Verify checksums.txt was generated
	checksumPath := filepath.Join(outputDir, "checksums.txt")
	if _, statErr := os.Stat(checksumPath); os.IsNotExist(statErr) {
		t.Error("checksums.txt should exist when IncludeChecksums is true")
	}

	// Verify checksums.txt is in output files list
	foundChecksum := false
	for _, f := range output.Files {
		if strings.HasSuffix(f, "checksums.txt") {
			foundChecksum = true
			break
		}
	}
	if !foundChecksum {
		t.Error("checksums.txt should be in output files list")
	}

	// Verify checksums.txt contains entries for other files
	content, err := os.ReadFile(checksumPath)
	if err != nil {
		t.Fatalf("Failed to read checksums.txt: %v", err)
	}
	checksumContent := string(content)
	if !strings.Contains(checksumContent, "app-of-apps.yaml") {
		t.Error("checksums.txt should contain app-of-apps.yaml")
	}
	if !strings.Contains(checksumContent, "README.md") {
		t.Error("checksums.txt should contain README.md")
	}
}

func TestGenerate_DataFiles(t *testing.T) {
	recipeResult := func() *recipe.RecipeResult {
		r := &recipe.RecipeResult{}
		r.Metadata.Version = testVersion
		r.ComponentRefs = []recipe.ComponentRef{
			{Name: "gpu-operator", Namespace: "gpu-operator", Chart: "gpu-operator", Version: "v25.3.3", Type: "helm", Source: "https://helm.ngc.nvidia.com/nvidia"},
		}
		return r
	}

	tests := []struct {
		name             string
		stageDataFile    string // relative path to create under outputDir (empty = skip)
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

			g := &Generator{
				RecipeResult:     recipeResult(),
				Version:          "v0.9.0",
				IncludeChecksums: tt.includeChecksums,
				DataFiles:        tt.dataFiles,
			}

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

func TestGenerate_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      "helm",
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}

	g := &Generator{
		RecipeResult: recipeResult,
		Version:      "v0.9.0",
	}

	_, err := g.Generate(ctx, outputDir)
	if err == nil {
		t.Fatal("Generate() should return error for cancelled context")
	}
}

func TestSortComponentRefsByDeploymentOrder(t *testing.T) {
	tests := []struct {
		name     string
		refs     []recipe.ComponentRef
		order    []string
		expected []string
	}{
		{
			name: "ordered",
			refs: []recipe.ComponentRef{
				{Name: "gpu-operator"},
				{Name: "cert-manager"},
				{Name: "network-operator"},
			},
			order:    []string{"cert-manager", "gpu-operator", "network-operator"},
			expected: []string{"cert-manager", "gpu-operator", "network-operator"},
		},
		{
			name: "empty order",
			refs: []recipe.ComponentRef{
				{Name: "gpu-operator"},
				{Name: "cert-manager"},
			},
			order:    []string{},
			expected: []string{"gpu-operator", "cert-manager"},
		},
		{
			name: "partial order",
			refs: []recipe.ComponentRef{
				{Name: "gpu-operator"},
				{Name: "cert-manager"},
				{Name: "network-operator"},
			},
			order:    []string{"cert-manager"},
			expected: []string{"cert-manager", "gpu-operator", "network-operator"},
		},
		{
			name: "component not in order goes last",
			refs: []recipe.ComponentRef{
				{Name: "unknown"},
				{Name: "gpu-operator"},
			},
			order:    []string{"gpu-operator"},
			expected: []string{"gpu-operator", "unknown"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := deployer.SortComponentRefsByDeploymentOrder(tt.refs, tt.order)

			if len(result) != len(tt.expected) {
				t.Fatalf("Expected %d components, got %d", len(tt.expected), len(result))
			}

			for i, name := range tt.expected {
				if result[i].Name != name {
					t.Errorf("Position %d: expected %s, got %s", i, name, result[i].Name)
				}
			}
		})
	}
}

func TestSafeJoin(t *testing.T) {
	baseDir := t.TempDir()

	tests := []struct {
		name    string
		dir     string
		input   string
		wantErr bool
	}{
		{"valid component", baseDir, "gpu-operator", false},
		{"valid with dots", baseDir, "cert-manager", false},
		{"path traversal", baseDir, "../etc/passwd", true},
		{"double dot", baseDir, "..", true},
		{"absolute path rejected", baseDir, "/etc/passwd", true},
		{"empty name", baseDir, "", false}, // empty joins to baseDir itself
		{"relative base", ".", "gpu-operator", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := deployer.SafeJoin(tt.dir, tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("deployer.SafeJoin(%q, %q) error = %v, wantErr %v", tt.dir, tt.input, err, tt.wantErr)
				return
			}
			if err == nil && result == "" {
				t.Errorf("deployer.SafeJoin(%q, %q) returned empty path", tt.dir, tt.input)
			}
		})
	}
}

func TestApplicationData_NamespaceFromComponentRef(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      "helm",
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}
	recipeResult.DeploymentOrder = []string{"gpu-operator"}

	g := &Generator{
		RecipeResult: recipeResult,
		Version:      "v0.9.0",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Verify namespace is used in application.yaml
	appPath := filepath.Join(outputDir, "001-gpu-operator", "application.yaml")
	content, readErr := os.ReadFile(appPath)
	if readErr != nil {
		t.Fatalf("Failed to read application.yaml: %v", readErr)
	}
	if !strings.Contains(string(content), "gpu-operator") {
		t.Error("application.yaml should reference gpu-operator namespace")
	}
}

func TestIsSafePathComponent(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"valid name", "gpu-operator", true},
		{"valid with dots", "cert-manager.io", true},
		{"empty string", "", false},
		{"forward slash", "path/traversal", false},
		{"backslash", "path\\traversal", false},
		{"double dot", "..", false},
		{"contains double dot", "foo..bar", false},
		{"single dot", ".", true},
		{"dashes and numbers", "test-123", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deployer.IsSafePathComponent(tt.input); got != tt.want {
				t.Errorf("deployer.IsSafePathComponent(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"v1.0.0", "1.0.0"},
		{"1.0.0", "1.0.0"},
		{"v25.3.3", "25.3.3"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := deployer.NormalizeVersion(tt.input)
			if result != tt.expected {
				t.Errorf("NormalizeVersion(%s) = %s, want %s", tt.input, result, tt.expected)
			}
		})
	}
}

// TestGenerate_KustomizeOnly freezes the bundle output for a kustomize-only
// component. localformat wraps the kustomize build output as a local Helm
// chart (Chart.yaml + templates/manifest.yaml), and the argocd deployer
// emits a path-based single-source Application against that wrapped chart.
// Driven from a committed kustomize input under testdata/kustomize_input/
// so the kustomize build is hermetic (no git fetch).
func TestGenerate_KustomizeOnly(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	kustomizePath, err := filepath.Abs("testdata/kustomize_input")
	if err != nil {
		t.Fatalf("resolve kustomize input path: %v", err)
	}

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "my-kustomize-app",
			Namespace: "my-app",
			Type:      recipe.ComponentTypeKustomize,
			// Repository empty: buildKustomize uses Path as a local
			// filesystem path so the test does not hit the network.
			Path: kustomizePath,
		},
	}
	recipeResult.DeploymentOrder = []string{"my-kustomize-app"}

	g := &Generator{
		RecipeResult:    recipeResult,
		ComponentValues: map[string]map[string]any{},
		Version:         "v0.0.0-golden",
		RepoURL:         "https://github.com/example/aicr-bundles.git",
		TargetRevision:  "main",
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Freeze the wrapped local-chart layout and the path-based Application
	// shape. Skip Chart.yaml and README — they carry no behavior beyond
	// the templates already covered elsewhere.
	for _, rel := range []string{
		"001-my-kustomize-app/application.yaml",
		"001-my-kustomize-app/values.yaml",
		"001-my-kustomize-app/templates/manifest.yaml",
		"app-of-apps.yaml",
	} {
		assertGolden(t, outputDir, "testdata/kustomize_only", rel)
	}

	// values.yaml exists but is empty for kustomize components (no Helm
	// values to merge) — assertGolden covers content, but verify that
	// the wrapped chart kind is local-helm by confirming Chart.yaml exists.
	if _, statErr := os.Stat(filepath.Join(outputDir, "001-my-kustomize-app", "Chart.yaml")); statErr != nil {
		t.Errorf("expected Chart.yaml in wrapped kustomize folder: %v", statErr)
	}
}

// TestGenerate_MixedHelmAndKustomize freezes the bundle output for a
// recipe that mixes a pure-Helm component with a kustomize component.
// The Helm component yields a multi-source Application; the kustomize
// component yields a path-based single-source Application against its
// wrapped local chart.
func TestGenerate_MixedHelmAndKustomize(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	kustomizePath, err := filepath.Abs("testdata/kustomize_input")
	if err != nil {
		t.Fatalf("resolve kustomize input path: %v", err)
	}

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "cert-manager",
			Namespace: "cert-manager",
			Chart:     "cert-manager",
			Version:   "v1.20.2",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://charts.jetstack.io",
		},
		{
			Name:      "my-kustomize-app",
			Namespace: "my-app",
			Type:      recipe.ComponentTypeKustomize,
			Path:      kustomizePath,
		},
	}
	recipeResult.DeploymentOrder = []string{"cert-manager", "my-kustomize-app"}

	g := &Generator{
		RecipeResult: recipeResult,
		ComponentValues: map[string]map[string]any{
			"cert-manager":     {"replicaCount": 1},
			"my-kustomize-app": {},
		},
		Version:        "v0.0.0-golden",
		RepoURL:        "https://github.com/example/aicr-bundles.git",
		TargetRevision: "main",
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	for _, rel := range []string{
		"001-cert-manager/application.yaml",
		"001-cert-manager/values.yaml",
		"002-my-kustomize-app/application.yaml",
		"002-my-kustomize-app/values.yaml",
		"002-my-kustomize-app/templates/manifest.yaml",
		"app-of-apps.yaml",
	} {
		assertGolden(t, outputDir, "testdata/helm_and_kustomize", rel)
	}
}

// TestGenerate_Reproducible verifies that Argo CD bundle generation is deterministic.
// Running Generate() twice with the same input should produce identical output files.
func TestGenerate_Reproducible(t *testing.T) {
	ctx := context.Background()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "cert-manager",
			Namespace: "cert-manager",
			Chart:     "cert-manager",
			Version:   "v1.17.2",
			Type:      "helm",
			Source:    "https://charts.jetstack.io",
		},
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      "helm",
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}
	recipeResult.DeploymentOrder = []string{"cert-manager", "gpu-operator"}

	g := &Generator{
		RecipeResult: recipeResult,
		ComponentValues: map[string]map[string]any{
			"cert-manager": {
				"crds": map[string]any{"enabled": true},
			},
			"gpu-operator": {
				"driver": map[string]any{
					"enabled": true,
				},
			},
		},
		Version: "v0.9.0",
		RepoURL: "https://github.com/test/repo.git",
	}

	// Generate twice in different directories
	var fileContents [2]map[string]string

	for i := 0; i < 2; i++ {
		outputDir := t.TempDir()

		_, err := g.Generate(ctx, outputDir)
		if err != nil {
			t.Fatalf("iteration %d: Generate() error = %v", i, err)
		}

		// Read all generated files
		fileContents[i] = make(map[string]string)
		err = filepath.Walk(outputDir, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() {
				return nil
			}

			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}

			relPath, _ := filepath.Rel(outputDir, path)
			fileContents[i][relPath] = string(content)
			return nil
		})
		if err != nil {
			t.Fatalf("iteration %d: failed to walk directory: %v", i, err)
		}
	}

	// Verify same files were generated
	if len(fileContents[0]) != len(fileContents[1]) {
		t.Errorf("different number of files: iteration 1 has %d, iteration 2 has %d",
			len(fileContents[0]), len(fileContents[1]))
	}

	// Verify file contents are identical
	for filename, content1 := range fileContents[0] {
		content2, exists := fileContents[1][filename]
		if !exists {
			t.Errorf("file %s exists in iteration 1 but not iteration 2", filename)
			continue
		}
		if content1 != content2 {
			t.Errorf("file %s has different content between iterations:\n--- iteration 1 ---\n%s\n--- iteration 2 ---\n%s",
				filename, content1, content2)
		}
	}

	t.Logf("Argo CD reproducibility verified: both iterations produced %d identical files", len(fileContents[0]))
}

// assertValidYAML reads the file at path and fails the test if it is not valid YAML.
func assertValidYAML(t *testing.T, path string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read %s: %v", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(content, &doc); err != nil {
		t.Errorf("invalid YAML in %s: %v\n--- content ---\n%s", path, err, string(content))
	}
}

// TestGenerate_ApplicationYAMLStructure verifies that generated application.yaml files
// are valid YAML with the expected Argo CD Application structure (issue #410).
func TestGenerate_ApplicationYAMLStructure(t *testing.T) {
	tests := []struct {
		name       string
		refs       []recipe.ComponentRef
		assertFunc func(t *testing.T, doc map[string]any)
	}{
		{
			name: "helm component has spec.sources",
			refs: []recipe.ComponentRef{
				{
					Name:      "gpu-operator",
					Namespace: "gpu-operator",
					Chart:     "gpu-operator",
					Version:   "v25.3.3",
					Type:      recipe.ComponentTypeHelm,
					Source:    "https://helm.ngc.nvidia.com/nvidia",
				},
			},
			assertFunc: func(t *testing.T, doc map[string]any) {
				t.Helper()
				spec, ok := doc["spec"].(map[string]any)
				if !ok {
					t.Fatal("spec is not a map")
				}
				if _, hasSources := spec["sources"]; !hasSources {
					t.Error("spec.sources missing for helm component")
				}
				dest, destOK := spec["destination"].(map[string]any)
				if !destOK {
					t.Fatal("spec.destination is not a map")
				}
				if dest["server"] != "https://kubernetes.default.svc" {
					t.Errorf("unexpected destination server: %v", dest["server"])
				}
			},
		},
		// kustomize subtest pending rewrite for #664: kustomize is now wrapped
		// as a local Helm chart. spec.source.path becomes the NNN-<name>
		// directory inside the bundle, not the upstream Path. Needs new
		// fixtures and assertions; tracked under TestGenerate_KustomizeOnly.
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			outputDir := t.TempDir()

			recipeResult := &recipe.RecipeResult{}
			recipeResult.Metadata.Version = testVersion
			recipeResult.ComponentRefs = tt.refs
			recipeResult.DeploymentOrder = []string{tt.refs[0].Name}

			g := &Generator{
				RecipeResult:    recipeResult,
				ComponentValues: map[string]map[string]any{tt.refs[0].Name: {}},
				Version:         "v0.9.0",
			}

			_, err := g.Generate(ctx, outputDir)
			if err != nil {
				t.Fatalf("Generate() error = %v", err)
			}

			appPath := filepath.Join(outputDir, "001-"+tt.refs[0].Name, "application.yaml")
			assertValidYAML(t, appPath)

			content, err := os.ReadFile(appPath)
			if err != nil {
				t.Fatalf("failed to read application.yaml: %v", err)
			}
			var doc map[string]any
			if err := yaml.Unmarshal(content, &doc); err != nil {
				t.Fatalf("failed to parse application.yaml: %v", err)
			}
			tt.assertFunc(t, doc)
		})
	}
}

// TestGenerate_NoTimestampInOutput verifies that generated files don't contain timestamps.
func TestGenerate_NoTimestampInOutput(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      "helm",
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}
	recipeResult.DeploymentOrder = []string{"gpu-operator"}

	g := &Generator{
		RecipeResult: recipeResult,
		ComponentValues: map[string]map[string]any{
			"gpu-operator": {},
		},
		Version: "v0.9.0",
		RepoURL: "https://github.com/test/repo.git",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Check that no files contain obvious timestamp patterns
	timestampPatterns := []string{
		"GeneratedAt:",
		"generated_at:",
		"timestamp:",
		"Timestamp:",
	}

	err = filepath.Walk(outputDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}

		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}

		contentStr := string(content)
		relPath, _ := filepath.Rel(outputDir, path)

		for _, pattern := range timestampPatterns {
			if strings.Contains(contentStr, pattern) {
				t.Errorf("file %s contains timestamp pattern %q", relPath, pattern)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to walk directory: %v", err)
	}
}

// TestBuildApplicationData_OCIHandling exercises two OCI-specific
// concerns that diverge from HTTPS Helm-repo conventions:
//
//  1. The chart-name segment is appended to repoURL for OCI sources.
//     Recipes carry source = registry+namespace (no chart name) by
//     convention, mirroring how localformat's writeUpstreamHelmFolder
//     constructs the install URL. Argo CD's Helm pull treats repoURL
//     as the full chart path and ignores the separate chart field, so
//     we must bake the chart name in.
//
//  2. The `v` prefix is preserved on OCI versions. OCI tags are literal
//     strings — `ghcr.io/.../nvsentinel:v1.3.0` is a distinct tag from
//     `:1.3.0` (only the prefixed one exists in the registry). HTTPS
//     Helm chart repos use index.yaml where versions are conventionally
//     non-prefixed, so stripping there is correct.
//
// Without these, Argo's repo-server reports
// "manifests/<wrong-path-or-tag>: not found".
func TestBuildApplicationData_OCIHandling(t *testing.T) {
	tests := []struct {
		name           string
		source         string
		chart          string
		recipeVer      string
		wantRepository string
		wantVersion    string
	}{
		{
			name:           "OCI source: chart appended, v preserved (nvsentinel pattern)",
			source:         "oci://ghcr.io/nvidia",
			chart:          "nvsentinel",
			recipeVer:      "v1.3.0",
			wantRepository: "oci://ghcr.io/nvidia/nvsentinel",
			wantVersion:    "v1.3.0",
		},
		{
			name:           "OCI source: chart appended even when org name == chart name (kai-scheduler pattern)",
			source:         "oci://ghcr.io/kai-scheduler/kai-scheduler",
			chart:          "kai-scheduler",
			recipeVer:      "v0.14.1",
			wantRepository: "oci://ghcr.io/kai-scheduler/kai-scheduler/kai-scheduler",
			wantVersion:    "v0.14.1",
		},
		{
			name:           "OCI source with trailing slash: no double slash",
			source:         "oci://ghcr.io/nvidia/",
			chart:          "nvsentinel",
			recipeVer:      "v1.3.0",
			wantRepository: "oci://ghcr.io/nvidia/nvsentinel",
			wantVersion:    "v1.3.0",
		},
		{
			name:           "HTTPS source: repoURL untouched, v stripped (Helm repo convention)",
			source:         "https://charts.jetstack.io",
			chart:          "cert-manager",
			recipeVer:      "v1.20.2",
			wantRepository: "https://charts.jetstack.io",
			wantVersion:    "1.20.2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			comp := recipe.ComponentRef{
				Name:    "test-component",
				Source:  tt.source,
				Chart:   tt.chart,
				Version: tt.recipeVer,
			}
			folder := localformat.Folder{
				Name:   "001-test-component",
				Dir:    "001-test-component",
				Kind:   localformat.KindUpstreamHelm,
				Parent: "test-component",
			}
			data := buildApplicationData(comp, folder, 0, "https://github.com/example/repo.git", "main")
			if data.Repository != tt.wantRepository {
				t.Errorf("Repository: got %q, want %q (source=%q chart=%q)",
					data.Repository, tt.wantRepository, tt.source, tt.chart)
			}
			if data.Version != tt.wantVersion {
				t.Errorf("Version: got %q, want %q", data.Version, tt.wantVersion)
			}
		})
	}
}

// TestBundleGolden_HelmAndManifestOnly freezes the bundle output for a recipe
// containing both component shapes that this deployer must handle:
//
//   - cert-manager: pure Helm → KindUpstreamHelm folder, multi-source
//     Application
//   - nodewright-customizations: manifest-only → KindLocalHelm folder
//     (Chart.yaml + templates/), path-based single-source Application
//
// To regenerate after intentional output changes:
//
//	go test ./pkg/bundler/deployer/argocd/... -run TestBundleGolden -args -update
//
// Substring assertions miss indentation drift, field reordering, and silent
// template changes; byte-comparing against checked-in goldens catches them.
func TestBundleGolden_HelmAndManifestOnly(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
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
	}
	recipeResult.DeploymentOrder = []string{"cert-manager", "nodewright-customizations"}

	g := &Generator{
		RecipeResult: recipeResult,
		ComponentValues: map[string]map[string]any{
			"cert-manager":              {"replicaCount": 1},
			"nodewright-customizations": {"enabled": true},
		},
		Version:        "v0.0.0-golden",
		RepoURL:        "https://github.com/example/aicr-bundles.git",
		TargetRevision: "main",
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

	// Freeze both Application shapes and a sample local-chart template body.
	// Don't golden the README or Chart.yaml inside the wrapped chart — those
	// carry no behavior beyond what the templates already commit to.
	for _, rel := range []string{
		"001-cert-manager/application.yaml",
		"001-cert-manager/values.yaml",
		"002-nodewright-customizations/application.yaml",
		"002-nodewright-customizations/Chart.yaml",
		"002-nodewright-customizations/values.yaml",
		"002-nodewright-customizations/templates/tuning.yaml",
		"app-of-apps.yaml",
	} {
		assertGolden(t, outputDir, "testdata/helm_and_manifest_only", rel)
	}
}

// TestBundleGolden_MixedComponent freezes the bundle output for a mixed
// component (Helm chart + raw manifests). localformat emits the primary
// upstream-helm folder (NNN-<name>/) followed by an injected `-post`
// wrapped chart ((NNN+1)-<name>-post/) so the manifests apply after the
// chart's CRDs are registered. The argocd deployer must produce two
// Applications — multi-source for the primary, path-based for the -post —
// with sync waves preserving order.
func TestBundleGolden_MixedComponent(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "gpu-operator",
			Chart:     "gpu-operator",
			Version:   "v25.3.3",
			Type:      recipe.ComponentTypeHelm,
			Source:    "https://helm.ngc.nvidia.com/nvidia",
		},
	}
	recipeResult.DeploymentOrder = []string{"gpu-operator"}

	g := &Generator{
		RecipeResult: recipeResult,
		ComponentValues: map[string]map[string]any{
			"gpu-operator": {"driver": map[string]any{"version": "580"}},
		},
		Version:        "v0.0.0-golden",
		RepoURL:        "https://github.com/example/aicr-bundles.git",
		TargetRevision: "main",
		// gpu-operator carries an additional rendered manifest — this turns
		// it into a mixed component, triggering the primary + -post split.
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
		// Primary: multi-source upstream-helm
		"001-gpu-operator/application.yaml",
		"001-gpu-operator/values.yaml",
		// Injected -post: path-based single-source wrapping rendered manifests
		"002-gpu-operator-post/application.yaml",
		"002-gpu-operator-post/Chart.yaml",
		"002-gpu-operator-post/values.yaml",
		"002-gpu-operator-post/templates/dcgm-exporter.yaml",
	} {
		assertGolden(t, outputDir, "testdata/mixed_component", rel)
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
