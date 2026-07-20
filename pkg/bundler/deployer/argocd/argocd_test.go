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
	stderrors "errors"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	sigsyaml "sigs.k8s.io/yaml"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/bundler/gatemanifest"
	"github.com/NVIDIA/aicr/pkg/errors"
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

// TestGenerate_AppName verifies the parent App-of-Apps `metadata.name`
// and the rendered README `argocd app get/sync` examples both pick up
// Generator.AppName, with DefaultAppName ("nvidia-stack") as the fallback
// when AppName is empty. Regression coverage for issue #1011: a hardcoded
// parent name silently overwrote the first bundle when two non-overlapping
// AICR bundles shared an Argo CD namespace.
func TestGenerate_AppName(t *testing.T) {
	tests := []struct {
		name         string
		appNameField string
		wantName     string
	}{
		{
			name:         "default falls back to DefaultAppName",
			appNameField: "",
			wantName:     DefaultAppName,
		},
		{
			name:         "explicit AppName flows into manifest and README",
			appNameField: "gpu-runtime",
			wantName:     "gpu-runtime",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			outputDir := t.TempDir()

			recipeResult := &recipe.RecipeResult{}
			recipeResult.Metadata.Version = testVersion
			recipeResult.ComponentRefs = []recipe.ComponentRef{
				{
					Name: "cert-manager", Namespace: "cert-manager", Chart: "cert-manager",
					Version: "v1.17.2", Type: "helm",
					Source: "https://charts.jetstack.io",
				},
			}
			recipeResult.DeploymentOrder = []string{"cert-manager"}

			g := &Generator{
				RecipeResult:    recipeResult,
				ComponentValues: map[string]map[string]any{"cert-manager": {}},
				Version:         "v0.9.0",
				AppName:         tt.appNameField,
			}
			if _, err := g.Generate(ctx, outputDir); err != nil {
				t.Fatalf("Generate() error = %v", err)
			}

			appOfApps, err := os.ReadFile(filepath.Join(outputDir, "app-of-apps.yaml"))
			if err != nil {
				t.Fatalf("read app-of-apps.yaml: %v", err)
			}
			if !strings.Contains(string(appOfApps), `name: "`+tt.wantName+`"`+"\n") {
				t.Errorf("app-of-apps.yaml missing quoted %q metadata.name; got:\n%s", tt.wantName, appOfApps)
			}

			readme, err := os.ReadFile(filepath.Join(outputDir, "README.md"))
			if err != nil {
				t.Fatalf("read README.md: %v", err)
			}
			if !strings.Contains(string(readme), "argocd app get "+tt.wantName) {
				t.Errorf("README missing `argocd app get %s`; got snippet:\n%s",
					tt.wantName, snippetAround(string(readme), "argocd app get"))
			}
			if !strings.Contains(string(readme), "argocd app sync "+tt.wantName) {
				t.Errorf("README missing `argocd app sync %s`; got snippet:\n%s",
					tt.wantName, snippetAround(string(readme), "argocd app sync"))
			}
		})
	}
}

// TestGenerate_AppName_YAMLReservedScalars verifies the rendered
// app-of-apps.yaml survives a kubectl-equivalent decode when AppName is a
// DNS-1123-valid scalar that YAML would otherwise interpret as a non-string
// type (integer, boolean, null, float). ValidateAppName accepts these
// because DNS-1123 subdomain literally allows them, so the template — not
// the validator — is responsible for forcing a string scalar.
//
// Decode path mirrors what kubectl does: sigs.k8s.io/yaml.YAMLToJSON →
// unstructured.UnmarshalJSON → GetName(). Without the template's
// `printf "%q"`, GetName() returns "" for non-string scalars because of
// the val.(string) type assertion inside getNestedString.
func TestGenerate_AppName_YAMLReservedScalars(t *testing.T) {
	tests := []string{"123", "true", "false", "null", "1e3"}

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name: "cert-manager", Namespace: "cert-manager", Chart: "cert-manager",
			Version: "v1.17.2", Type: "helm",
			Source: "https://charts.jetstack.io",
		},
	}
	recipeResult.DeploymentOrder = []string{"cert-manager"}

	for _, appName := range tests {
		t.Run(appName, func(t *testing.T) {
			outputDir := t.TempDir()
			g := &Generator{
				RecipeResult:    recipeResult,
				ComponentValues: map[string]map[string]any{"cert-manager": {}},
				Version:         "v0.9.0",
				AppName:         appName,
			}
			if _, err := g.Generate(context.Background(), outputDir); err != nil {
				t.Fatalf("Generate() error = %v", err)
			}

			raw, err := os.ReadFile(filepath.Join(outputDir, "app-of-apps.yaml"))
			if err != nil {
				t.Fatalf("read app-of-apps.yaml: %v", err)
			}

			jsonBytes, err := sigsyaml.YAMLToJSON(raw)
			if err != nil {
				t.Fatalf("YAMLToJSON: %v\nraw:\n%s", err, raw)
			}
			u := &unstructured.Unstructured{}
			if err := u.UnmarshalJSON(jsonBytes); err != nil {
				t.Fatalf("UnmarshalJSON: %v\njson: %s", err, jsonBytes)
			}
			if got := u.GetName(); got != appName {
				t.Errorf("unstructured.GetName() = %q, want %q; rendered manifest treated metadata.name as a non-string YAML scalar.\nraw:\n%s", got, appName, raw)
			}
		})
	}
}

// TestGenerate_AppNameValidatedAtBoundary verifies the deployer boundary
// rejects an invalid AppName even when callers bypass the CLI/API
// validation layer (e.g. direct library use). Failing here keeps the
// invalid name from reaching the rendered manifest, where it would only
// surface as a cryptic apiserver admission error at apply time.
func TestGenerate_AppNameValidatedAtBoundary(t *testing.T) {
	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name: "cert-manager", Namespace: "cert-manager", Chart: "cert-manager",
			Version: "v1.17.2", Type: "helm",
			Source: "https://charts.jetstack.io",
		},
	}
	recipeResult.DeploymentOrder = []string{"cert-manager"}

	g := &Generator{
		RecipeResult:    recipeResult,
		ComponentValues: map[string]map[string]any{"cert-manager": {}},
		Version:         "v0.9.0",
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

// snippetAround returns up to 120 chars of haystack around the first occurrence
// of needle, used for compact failure messages in TestGenerate_AppName.
func snippetAround(haystack, needle string) string {
	i := strings.Index(haystack, needle)
	if i < 0 {
		return "<not found>"
	}
	end := min(i+len(needle)+80, len(haystack))
	return haystack[i:end]
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
		// "foo..bar" is a benign filename; only the standalone parent ref ".." is rejected.
		{"contains double dot is benign", "foo..bar", true},
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
			data, err := buildApplicationData(comp, folder, 0, "https://github.com/example/repo.git", "main", nil, false)
			if err != nil {
				t.Fatalf("buildApplicationData() error = %v", err)
			}
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

// TestGenerate_InlineUpstreamValues verifies that an OCI-bound bundle
// inlines per-component values under helm.valuesObject and drops the
// multi-source $values ref. Closes #960: Argo CD's $values multi-source
// pattern is Git-only — over OCI, the repo-server attempts a git ListRefs
// against the OCI URL and fails with `unsupported scheme "oci"`.
func TestGenerate_InlineUpstreamValues(t *testing.T) {
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
	}
	recipeResult.DeploymentOrder = []string{"cert-manager"}

	values := map[string]any{
		"installCRDs": true,
		"resources": map[string]any{
			"requests": map[string]any{
				"cpu":    "100m",
				"memory": "128Mi",
			},
		},
	}

	g := &Generator{
		RecipeResult:    recipeResult,
		ComponentValues: map[string]map[string]any{"cert-manager": values},
		Version:         "v0.9.0",
		RepoURL:         "oci://registry.aicr-registry.svc.cluster.local:5000/aicr/test",
		TargetRevision:  "v0.9.0",
		// Mirrors the bundler wiring at pkg/bundler/bundler.go for an
		// OCI-bound --deployer argocd target.
		InlineUpstreamValues: true,
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	appPath := filepath.Join(outputDir, "001-cert-manager", "application.yaml")
	content, err := os.ReadFile(appPath)
	if err != nil {
		t.Fatalf("failed to read application.yaml: %v", err)
	}
	assertValidYAML(t, appPath)

	var doc map[string]any
	if err := yaml.Unmarshal(content, &doc); err != nil {
		t.Fatalf("failed to parse application.yaml: %v", err)
	}
	spec, ok := doc["spec"].(map[string]any)
	if !ok {
		t.Fatal("spec is not a map")
	}

	if _, hasSources := spec["sources"]; hasSources {
		t.Error("spec.sources should NOT be set when InlineUpstreamValues is true (would re-introduce Git-only $values ref)")
	}
	source, ok := spec["source"].(map[string]any)
	if !ok {
		t.Fatal("spec.source is not a map")
	}
	if source["repoURL"] != "https://charts.jetstack.io" {
		t.Errorf("source.repoURL = %v, want https://charts.jetstack.io", source["repoURL"])
	}
	if source["chart"] != "cert-manager" {
		t.Errorf("source.chart = %v, want cert-manager", source["chart"])
	}
	// HTTPS Helm-chart-repo sources strip the `v` prefix (index.yaml
	// convention); OCI sources preserve it. cert-manager here is HTTPS.
	if source["targetRevision"] != "1.20.2" {
		t.Errorf("source.targetRevision = %v, want 1.20.2", source["targetRevision"])
	}

	helm, ok := source["helm"].(map[string]any)
	if !ok {
		t.Fatal("source.helm is not a map")
	}
	valuesObj, ok := helm["valuesObject"].(map[string]any)
	if !ok {
		t.Fatalf("helm.valuesObject is not a map: %T", helm["valuesObject"])
	}
	if valuesObj["installCRDs"] != true {
		t.Errorf("valuesObject.installCRDs = %v, want true", valuesObj["installCRDs"])
	}

	// $values literals must not appear anywhere — that token is the symptom
	// of the bug being fixed; if it leaks back in, the regression goes
	// silent until a live Argo CD parse.
	if strings.Contains(string(content), "$values") {
		t.Errorf("application.yaml still references $values:\n%s", string(content))
	}
	if strings.Contains(string(content), "ref: values") {
		t.Errorf("application.yaml still has ref: values source:\n%s", string(content))
	}
}

// TestRenderInlineValuesYAML covers the edge cases for the inline-values
// helper: empty map produces a well-formed `{}` (Argo CD's schema rejects a
// bare key with no value), and non-empty maps are 8-space indented to drop
// cleanly under `helm.valuesObject:` at column 6.
func TestRenderInlineValuesYAML(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
		want   string
	}{
		{
			name:   "empty map → explicit {} (Argo CD rejects bare valuesObject:)",
			values: nil,
			want:   "        {}\n",
		},
		{
			name:   "scalar value at top level",
			values: map[string]any{"replicaCount": 3},
			want:   "        replicaCount: 3\n",
		},
		{
			name: "nested map preserves indentation under 8-space base",
			values: map[string]any{
				"resources": map[string]any{
					"requests": map[string]any{
						"cpu": "100m",
					},
				},
			},
			// yaml.v3 uses 2-space indent for nested keys; 8-space base
			// stacks on top: col 8 = resources, col 10 = requests, col 12 = cpu.
			want: "        resources:\n          requests:\n            cpu: 100m\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := renderInlineValuesYAML(tt.values)
			if err != nil {
				t.Fatalf("renderInlineValuesYAML() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("renderInlineValuesYAML() got:\n%q\nwant:\n%q", got, tt.want)
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

// TestBundleGolden_MixedWithPre freezes the bundle output for a mixed
// component with BOTH pre and post manifests. localformat emits three
// folders — pre (NNN-<name>-pre/) before the primary, primary
// (NNN+1)-<name>/), and post ((NNN+2)-<name>-post/) after — and the
// argocd deployer assigns sync-waves 0/1/2 from the folder index, so
// pre's namespace lands before the primary chart's pods need it.
//
// Regenerate goldens after changes to argocd / helm / localformat
// (run with -update). See deployer/helm/testdata/README.md for the
// pattern.
//
//	go test ./pkg/bundler/deployer/argocd/... -run TestBundleGolden -args -update
func TestBundleGolden_MixedWithPre(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{
			Name:      "gpu-operator",
			Namespace: "privileged-gpu-operator",
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
		ComponentPreManifests: map[string]map[string][]byte{
			"gpu-operator": {
				"components/gpu-operator/manifests/talos-namespace.yaml": []byte("apiVersion: v1\n" +
					"kind: Namespace\n" +
					"metadata:\n" +
					"  name: privileged-gpu-operator\n" +
					"  labels:\n" +
					"    pod-security.kubernetes.io/enforce: privileged\n"),
			},
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
		// Pre: path-based single-source wrapping rendered Namespace
		"001-gpu-operator-pre/application.yaml",
		"001-gpu-operator-pre/Chart.yaml",
		"001-gpu-operator-pre/values.yaml",
		"001-gpu-operator-pre/templates/talos-namespace.yaml",
		// Primary: multi-source upstream-helm
		"002-gpu-operator/application.yaml",
		"002-gpu-operator/values.yaml",
		// Post: path-based single-source wrapping rendered manifests
		"003-gpu-operator-post/application.yaml",
		"003-gpu-operator-post/Chart.yaml",
		"003-gpu-operator-post/values.yaml",
		"003-gpu-operator-post/templates/dcgm-exporter.yaml",
	} {
		assertGolden(t, outputDir, "testdata/mixed_with_pre", rel)
	}
}

// TestBundleGolden_ReadinessGate freezes the bundle output when a component
// ships a readiness gate (ComponentReadiness populated, as the bundler does
// for --readiness-hooks). localformat emits the gate as a local-helm folder
// immediately after the component's primary folder, so the argocd deployer
// assigns it the next sync-wave (1 > the component's 0). Argo CD blocks that
// wave on the gate Job via its built-in batch/Job health — no custom Lua.
// See #904.
//
//	go test ./pkg/bundler/deployer/argocd/... -run TestBundleGolden -args -update
func TestBundleGolden_ReadinessGate(t *testing.T) {
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
		// Mirrors the multi-doc manifest the bundler synthesizes from
		// readiness.yaml via gatemanifest.Render.
		ComponentReadiness: map[string]map[string][]byte{
			"gpu-operator": {
				"readiness.yaml": readinessGateManifest(t, config.DeployerArgoCD),
			},
		},
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	for _, rel := range []string{
		// Primary: multi-source upstream-helm at its level's primary wave (1)
		"001-gpu-operator/application.yaml",
		"001-gpu-operator/values.yaml",
		// Readiness gate: path-based single-source at the level's gate wave (3)
		"002-gpu-operator-readiness/application.yaml",
		"002-gpu-operator-readiness/Chart.yaml",
		"002-gpu-operator-readiness/values.yaml",
		"002-gpu-operator-readiness/templates/readiness.yaml",
	} {
		assertGolden(t, outputDir, "testdata/readiness_gate", rel)
	}
}

// TestReadinessGateSyncWaveOrdering asserts the core ArgoCD gating invariant:
// the readiness gate Application carries a strictly higher sync-wave than its
// component's primary Application, so Argo CD applies — and blocks on — the
// gate Job after the component installs. See #904.
func TestReadinessGateSyncWaveOrdering(t *testing.T) {
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
		RecipeResult:    recipeResult,
		ComponentValues: map[string]map[string]any{"gpu-operator": {}},
		Version:         "v0.0.0-test",
		RepoURL:         "https://github.com/example/aicr-bundles.git",
		TargetRevision:  "main",
		ComponentReadiness: map[string]map[string][]byte{
			"gpu-operator": {
				"readiness.yaml": []byte("apiVersion: batch/v1\nkind: Job\nmetadata:\n" +
					"  name: gpu-operator-readiness-gate\n  namespace: {{ .Release.Namespace }}\n"),
			},
		},
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	primaryWave := syncWaveOf(t, filepath.Join(outputDir, "001-gpu-operator", "application.yaml"))
	readinessApp := filepath.Join(outputDir, "002-gpu-operator-readiness", "application.yaml")
	readinessWave := syncWaveOf(t, readinessApp)

	if readinessWave <= primaryWave {
		t.Errorf("readiness sync-wave (%d) must be greater than component sync-wave (%d) so Argo gates after install",
			readinessWave, primaryWave)
	}

	// The gate is wrapped as a local chart, so its Application is path-based.
	content, err := os.ReadFile(readinessApp)
	if err != nil {
		t.Fatalf("read readiness application.yaml: %v", err)
	}
	if !strings.Contains(string(content), "path: 002-gpu-operator-readiness") {
		t.Errorf("readiness Application should be path-based at its folder:\n%s", content)
	}
}

// TestWaveForFolder pins the level→sync-wave arithmetic: within a level's
// band [level*stride, level*stride+3] the per-phase offset preserves the
// -pre → primary → -post → -readiness ordering, and the band width equals the
// stride so consecutive levels never overlap.
func TestWaveForFolder(t *testing.T) {
	tests := []struct {
		name   string
		folder localformat.Folder
		level  int
		want   int
	}{
		{"pre L0", localformat.Folder{Name: "cert-manager-pre", Parent: "cert-manager"}, 0, 0},
		{"primary L0", localformat.Folder{Name: "cert-manager", Parent: "cert-manager"}, 0, 1},
		{"post L0", localformat.Folder{Name: "cert-manager-post", Parent: "cert-manager"}, 0, 2},
		{"readiness L0", localformat.Folder{Name: "cert-manager-readiness", Parent: "cert-manager"}, 0, 3},
		{"pre L1", localformat.Folder{Name: "gpu-operator-pre", Parent: "gpu-operator"}, 1, 4},
		{"primary L1", localformat.Folder{Name: "gpu-operator", Parent: "gpu-operator"}, 1, 5},
		{"post L1", localformat.Folder{Name: "gpu-operator-post", Parent: "gpu-operator"}, 1, 6},
		{"readiness L1", localformat.Folder{Name: "gpu-operator-readiness", Parent: "gpu-operator"}, 1, 7},
		{"primary L2", localformat.Folder{Name: "nvsentinel", Parent: "nvsentinel"}, 2, 9},
		// A component literally named "<x>-pre" (no sibling injects the
		// suffix, so Parent == Name) classifies as a primary, not a pre.
		{"component named like pre", localformat.Folder{Name: "foo-pre", Parent: "foo-pre"}, 0, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := waveForFolder(tt.folder, tt.level); got != tt.want {
				t.Errorf("waveForFolder(%q@L%d) = %d, want %d", tt.folder.Name, tt.level, got, tt.want)
			}
		})
	}
}

// TestGenerate_LevelBasedSyncWaves is the end-to-end proof that independent
// components deploy in parallel while genuine dependencies still gate. The
// graph: cert-manager and nfd have no dependencies (level 0); gpu-operator
// depends on both (level 1). Each carries a readiness gate. Under level-based
// scheduling cert-manager and nfd must share the level-0 primary wave (deploy
// together), and gpu-operator must land in a strictly higher band than every
// level-0 folder — including the level-0 gate Jobs — so Argo CD holds it until
// the whole prior level is Healthy.
func TestGenerate_LevelBasedSyncWaves(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{Name: "cert-manager", Namespace: "cert-manager", Chart: "cert-manager", Version: "v1.14.0",
			Type: recipe.ComponentTypeHelm, Source: "https://charts.jetstack.io"},
		{Name: "nfd", Namespace: "node-feature-discovery", Chart: "node-feature-discovery", Version: "v0.15.0",
			Type: recipe.ComponentTypeHelm, Source: "https://kubernetes-sigs.github.io/node-feature-discovery/charts"},
		{Name: "gpu-operator", Namespace: "gpu-operator", Chart: "gpu-operator", Version: "v25.3.3",
			Type: recipe.ComponentTypeHelm, Source: "https://helm.ngc.nvidia.com/nvidia",
			DependencyRefs: []string{"cert-manager", "nfd"}},
	}
	recipeResult.DeploymentOrder = []string{"cert-manager", "nfd", "gpu-operator"}

	gate := func(name string) []byte {
		return []byte("apiVersion: batch/v1\nkind: Job\nmetadata:\n" +
			"  name: " + name + "-readiness-gate\n  namespace: {{ .Release.Namespace }}\n")
	}
	g := &Generator{
		RecipeResult: recipeResult,
		ComponentValues: map[string]map[string]any{
			"cert-manager": {}, "nfd": {}, "gpu-operator": {},
		},
		Version:        "v0.0.0-test",
		RepoURL:        "https://github.com/example/aicr-bundles.git",
		TargetRevision: "main",
		ComponentReadiness: map[string]map[string][]byte{
			"cert-manager": {"readiness.yaml": gate("cert-manager")},
			"nfd":          {"readiness.yaml": gate("nfd")},
			"gpu-operator": {"readiness.yaml": gate("gpu-operator")},
		},
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// waveFor globs the NNN-prefixed folder whose name ends in the exact
	// suffix and returns its sync-wave, so the assertions don't hardcode the
	// localformat index numbering.
	certWave := waveForComponentFolder(t, outputDir, "cert-manager")
	nfdWave := waveForComponentFolder(t, outputDir, "nfd")
	certGate := waveForComponentFolder(t, outputDir, "cert-manager-readiness")
	nfdGate := waveForComponentFolder(t, outputDir, "nfd-readiness")
	gpuWave := waveForComponentFolder(t, outputDir, "gpu-operator")
	gpuGate := waveForComponentFolder(t, outputDir, "gpu-operator-readiness")

	// Level 0 independents deploy in parallel: identical primary wave.
	if certWave != nfdWave {
		t.Errorf("independent level-0 components must share a wave: cert-manager=%d nfd=%d", certWave, nfdWave)
	}
	// The dependent lands in a strictly higher band than every level-0 folder,
	// gate Jobs included, so it cannot start until level 0 is Healthy.
	for _, l0 := range []int{certWave, nfdWave, certGate, nfdGate} {
		if gpuWave <= l0 {
			t.Errorf("gpu-operator wave (%d) must exceed every level-0 wave, got level-0 wave %d", gpuWave, l0)
		}
	}
	// Exact bands: level 0 primary=1/gate=3, level 1 primary=5/gate=7.
	if certWave != 1 || certGate != 3 || gpuWave != 5 || gpuGate != 7 {
		t.Errorf("unexpected waves: cert=%d certGate=%d gpu=%d gpuGate=%d; want 1/3/5/7",
			certWave, certGate, gpuWave, gpuGate)
	}
}

// TestGenerate_SerialSyncWaves is the escape-hatch counterpart to
// TestGenerate_LevelBasedSyncWaves: with Serial set, every folder takes a
// sync-wave equal to its linear position, so even independent components
// (cert-manager, nfd) get distinct, strictly increasing waves and deploy one
// at a time — the pre-parallelism behavior restored by --serial.
func TestGenerate_SerialSyncWaves(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{Name: "cert-manager", Namespace: "cert-manager", Chart: "cert-manager", Version: "v1.14.0",
			Type: recipe.ComponentTypeHelm, Source: "https://charts.jetstack.io"},
		{Name: "nfd", Namespace: "node-feature-discovery", Chart: "node-feature-discovery", Version: "v0.15.0",
			Type: recipe.ComponentTypeHelm, Source: "https://kubernetes-sigs.github.io/node-feature-discovery/charts"},
		{Name: "gpu-operator", Namespace: "gpu-operator", Chart: "gpu-operator", Version: "v25.3.3",
			Type: recipe.ComponentTypeHelm, Source: "https://helm.ngc.nvidia.com/nvidia",
			DependencyRefs: []string{"cert-manager", "nfd"}},
	}
	recipeResult.DeploymentOrder = []string{"cert-manager", "nfd", "gpu-operator"}

	g := &Generator{
		RecipeResult:    recipeResult,
		ComponentValues: map[string]map[string]any{"cert-manager": {}, "nfd": {}, "gpu-operator": {}},
		Version:         "v0.0.0-test",
		RepoURL:         "https://github.com/example/aicr-bundles.git",
		TargetRevision:  "main",
		Serial:          true,
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	cert := waveForComponentFolder(t, outputDir, "cert-manager")
	nfd := waveForComponentFolder(t, outputDir, "nfd")
	gpu := waveForComponentFolder(t, outputDir, "gpu-operator")

	// Linear position waves: 0, 1, 2 in deployment order.
	if cert != 0 || nfd != 1 || gpu != 2 {
		t.Errorf("serial waves = cert:%d nfd:%d gpu:%d; want 0/1/2", cert, nfd, gpu)
	}
	// The defining property of serial mode: independent components do NOT
	// share a wave (contrast TestGenerate_LevelBasedSyncWaves where cert==nfd).
	if cert == nfd {
		t.Errorf("serial mode must give independent components distinct waves; cert==nfd==%d", cert)
	}
}

// TestGenerate_SerialStillValidatesTopology guards the fail-closed invariant:
// even in --serial mode (where sync-waves come from the linear index, not
// dependency levels), Generate must still reject a cyclic dependency graph.
// A direct library caller can hand-build a RecipeResult, so the graph
// validation must not be gated behind the parallel path.
func TestGenerate_SerialStillValidatesTopology(t *testing.T) {
	ctx := context.Background()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{
		{Name: "a", Namespace: "a", Chart: "a", Version: "v1", Type: recipe.ComponentTypeHelm,
			Source: "https://example.com", DependencyRefs: []string{"b"}},
		{Name: "b", Namespace: "b", Chart: "b", Version: "v1", Type: recipe.ComponentTypeHelm,
			Source: "https://example.com", DependencyRefs: []string{"a"}}, // a <-> b cycle
	}
	recipeResult.DeploymentOrder = []string{"a", "b"}

	g := &Generator{
		RecipeResult:    recipeResult,
		ComponentValues: map[string]map[string]any{"a": {}, "b": {}},
		Version:         "v0.0.0-test",
		RepoURL:         "https://github.com/example/aicr-bundles.git",
		TargetRevision:  "main",
		Serial:          true,
	}

	if _, err := g.Generate(ctx, t.TempDir()); err == nil {
		t.Fatal("expected Generate to reject a cyclic dependency graph in --serial mode, got nil error")
	}
}

// waveForComponentFolder returns the sync-wave of the single NNN-<suffix>
// folder's application.yaml under outputDir. Shared by the level-based and
// serial sync-wave tests.
func waveForComponentFolder(t *testing.T, outputDir, suffix string) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(outputDir, "*-"+suffix))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected exactly one folder ending in %q, got %v (err %v)", suffix, matches, err)
	}
	return syncWaveOf(t, filepath.Join(matches[0], "application.yaml"))
}

// syncWaveOf parses the argocd.argoproj.io/sync-wave annotation from an
// Application manifest and returns it as an int.
func syncWaveOf(t *testing.T, appPath string) int {
	t.Helper()
	raw, err := os.ReadFile(appPath)
	if err != nil {
		t.Fatalf("read %s: %v", appPath, err)
	}
	var app struct {
		Metadata struct {
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
	}
	if err = yaml.Unmarshal(raw, &app); err != nil {
		t.Fatalf("parse %s: %v", appPath, err)
	}
	wave := app.Metadata.Annotations["argocd.argoproj.io/sync-wave"]
	n, err := strconv.Atoi(wave)
	if err != nil {
		t.Fatalf("sync-wave %q not an int in %s: %v", wave, appPath, err)
	}
	return n
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

// newTestGenerator returns a minimal single-component (gpu-operator)
// Generator fixture shared by the deployer-options tests. gpu-operator is
// the sole component so its child Application lands at
// 001-gpu-operator/application.yaml.
func newTestGenerator(t *testing.T) *Generator {
	t.Helper()
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
	return &Generator{
		RecipeResult:    recipeResult,
		ComponentValues: map[string]map[string]any{"gpu-operator": {}},
		Version:         "v0.9.0",
	}
}

// readBundleFile reads relPath under outputDir, failing the test on error.
func readBundleFile(t *testing.T, outputDir, relPath string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(outputDir, relPath))
	if err != nil {
		t.Fatalf("read %s: %v", relPath, err)
	}
	return content
}

// TestGenerate_DeployerOptions verifies the four deployer: options land in
// the generated manifests: namePrefix, destinationServer, and project apply
// to child Applications only, while cascadeDelete adds the resources
// finalizer to both the parent and children. See #1625 and #1628.
func TestGenerate_DeployerOptions(t *testing.T) {
	outputDir := t.TempDir()
	g := newTestGenerator(t)
	g.NamePrefix = "tenant-a-"
	g.DestinationServer = "https://remote.example.com:6443"
	g.Project = "tenant-a"
	g.CascadeDelete = true

	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	appYAML := readBundleFile(t, outputDir, "001-gpu-operator/application.yaml")
	for _, want := range []string{
		`name: "tenant-a-gpu-operator"`,
		`server: "https://remote.example.com:6443"`,
		`project: "tenant-a"`,
		"- resources-finalizer.argocd.argoproj.io",
	} {
		if !strings.Contains(string(appYAML), want) {
			t.Errorf("child application.yaml missing %q\n%s", want, appYAML)
		}
	}

	parentYAML := readBundleFile(t, outputDir, "app-of-apps.yaml")
	// Parent: finalizer YES (cascadeDelete covers parent), but name prefix,
	// destination, and project must NOT apply — control-plane stays put.
	if !strings.Contains(string(parentYAML), "- resources-finalizer.argocd.argoproj.io") {
		t.Error("parent app-of-apps.yaml missing finalizer")
	}
	for _, reject := range []string{"tenant-a-", "remote.example.com"} {
		if strings.Contains(string(parentYAML), reject) {
			t.Errorf("parent app-of-apps.yaml unexpectedly contains %q", reject)
		}
	}
	if !strings.Contains(string(parentYAML), "project: default") {
		t.Error("parent app-of-apps.yaml project changed; must stay default")
	}
}

// TestGenerate_DeployerOptions_InvalidRejected verifies the deployer
// boundary rejects malformed option values with ErrCodeInvalidRequest even
// when callers bypass CLI/API validation (direct library use).
func TestGenerate_DeployerOptions_InvalidRejected(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Generator)
	}{
		{"combined child name too long", func(g *Generator) { g.NamePrefix = strings.Repeat("a", 254) + "-" }},
		{"http destination", func(g *Generator) { g.DestinationServer = "http://insecure" }},
		{"invalid project", func(g *Generator) { g.Project = "Not_Valid" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := newTestGenerator(t)
			tt.mutate(g)
			_, err := g.Generate(context.Background(), t.TempDir())
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

// TestGenerate_ProjectReservedScalarQuoted verifies the template quotes
// spec.project (and the other deployer-controlled scalars) so a project
// named after a YAML reserved scalar ("true", "null", "on", ...) renders
// as a string instead of being reinterpreted as a boolean/null by YAML
// consumers. "true" passes DNS-1123 validation, so quoting in the
// template is the only line of defense.
func TestGenerate_ProjectReservedScalarQuoted(t *testing.T) {
	outputDir := t.TempDir()
	g := newTestGenerator(t)
	g.Project = "true"

	if _, err := g.Generate(context.Background(), outputDir); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	appYAML := readBundleFile(t, outputDir, "001-gpu-operator/application.yaml")
	if !strings.Contains(string(appYAML), `project: "true"`) {
		t.Errorf("application.yaml missing quoted project scalar\n%s", appYAML)
	}

	var app struct {
		Spec struct {
			Project any `yaml:"project"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(appYAML, &app); err != nil {
		t.Fatalf("unmarshal application.yaml: %v", err)
	}
	proj, ok := app.Spec.Project.(string)
	if !ok {
		t.Fatalf("spec.project decoded as %T (%v), want string — reserved scalar leaked as non-string", app.Spec.Project, app.Spec.Project)
	}
	if proj != "true" {
		t.Errorf("spec.project = %q, want %q", proj, "true")
	}
}

// TestGenerate_ChildNameLimits verifies the bundle-time guards for
// composed child Application names: names over Helm's 53-character
// release-name cap and names that collide with the parent Application
// are rejected with ErrCodeInvalidRequest.
func TestGenerate_ChildNameLimits(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Generator)
		errSubstr string
	}{
		{
			// prefix (50) + "gpu-operator" (12) = 62 > 53, but each label
			// stays DNS-1123-valid so only the release-name cap fires.
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
			g := newTestGenerator(t)
			tt.mutate(g)
			_, err := g.Generate(context.Background(), t.TempDir())
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), tt.errSubstr) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.errSubstr)
			}
		})
	}
}
