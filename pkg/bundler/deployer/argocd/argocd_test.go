// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/eidos/pkg/bundler/deployer/shared"
	"github.com/NVIDIA/eidos/pkg/recipe"
)

const testVersion = "v1.0.0"

func TestNewGenerator(t *testing.T) {
	g := NewGenerator()
	if g == nil {
		t.Fatal("NewGenerator() returned nil")
	}
}

func TestGenerate_Success(t *testing.T) {
	g := NewGenerator()
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

	input := &GeneratorInput{
		RecipeResult: recipeResult,
		ComponentValues: map[string]map[string]any{
			"cert-manager": {
				"installCRDs": true,
			},
			"gpu-operator": {
				"driver": map[string]any{
					"enabled": true,
				},
			},
		},
		Version: "v0.9.0",
	}

	output, err := g.Generate(ctx, input, outputDir)
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
		"cert-manager/application.yaml",
		"cert-manager/values.yaml",
		"gpu-operator/application.yaml",
		"gpu-operator/values.yaml",
		"app-of-apps.yaml",
		"README.md",
	}

	for _, relPath := range expectedFiles {
		fullPath := filepath.Join(outputDir, relPath)
		if _, statErr := os.Stat(fullPath); os.IsNotExist(statErr) {
			t.Errorf("Expected file %s does not exist", relPath)
		}
	}

	// Verify cert-manager application has sync-wave 0
	certManagerApp := filepath.Join(outputDir, "cert-manager", "application.yaml")
	content, err := os.ReadFile(certManagerApp)
	if err != nil {
		t.Fatalf("Failed to read cert-manager application: %v", err)
	}
	if !strings.Contains(string(content), "sync-wave: \"0\"") {
		t.Error("cert-manager application should have sync-wave 0")
	}

	// Verify gpu-operator application has sync-wave 1
	gpuOperatorApp := filepath.Join(outputDir, "gpu-operator", "application.yaml")
	content, err = os.ReadFile(gpuOperatorApp)
	if err != nil {
		t.Fatalf("Failed to read gpu-operator application: %v", err)
	}
	if !strings.Contains(string(content), "sync-wave: \"1\"") {
		t.Error("gpu-operator application should have sync-wave 1")
	}

	// Verify README contains component information
	readmePath := filepath.Join(outputDir, "README.md")
	content, err = os.ReadFile(readmePath)
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

func TestGenerate_NilInput(t *testing.T) {
	g := NewGenerator()
	ctx := context.Background()
	outputDir := t.TempDir()

	_, err := g.Generate(ctx, nil, outputDir)
	if err == nil {
		t.Fatal("Generate() should return error for nil input")
	}
}

func TestGenerate_NilRecipeResult(t *testing.T) {
	g := NewGenerator()
	ctx := context.Background()
	outputDir := t.TempDir()

	input := &GeneratorInput{
		RecipeResult: nil,
		Version:      "v0.9.0",
	}

	_, err := g.Generate(ctx, input, outputDir)
	if err == nil {
		t.Fatal("Generate() should return error for nil recipe result")
	}
}

func TestGenerate_EmptyComponents(t *testing.T) {
	g := NewGenerator()
	ctx := context.Background()
	outputDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{}
	recipeResult.Metadata.Version = testVersion
	recipeResult.ComponentRefs = []recipe.ComponentRef{}

	input := &GeneratorInput{
		RecipeResult: recipeResult,
		Version:      "v0.9.0",
	}

	output, err := g.Generate(ctx, input, outputDir)
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
	g := NewGenerator()
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

	input := &GeneratorInput{
		RecipeResult: recipeResult,
		Version:      "v0.9.0",
		RepoURL:      customRepoURL,
	}

	_, err := g.Generate(ctx, input, outputDir)
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
}

func TestGenerate_WithChecksums(t *testing.T) {
	g := NewGenerator()
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

	input := &GeneratorInput{
		RecipeResult:     recipeResult,
		Version:          "v0.9.0",
		IncludeChecksums: true,
	}

	output, err := g.Generate(ctx, input, outputDir)
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

func TestGenerate_ContextCancellation(t *testing.T) {
	g := NewGenerator()
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

	input := &GeneratorInput{
		RecipeResult: recipeResult,
		Version:      "v0.9.0",
	}

	_, err := g.Generate(ctx, input, outputDir)
	if err == nil {
		t.Fatal("Generate() should return error for cancelled context")
	}
}

func TestSortComponentsByDeploymentOrder(t *testing.T) {
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
			result := sortComponentsByDeploymentOrder(tt.refs, tt.order)

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
			result, err := shared.SafeJoin(tt.dir, tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("shared.SafeJoin(%q, %q) error = %v, wantErr %v", tt.dir, tt.input, err, tt.wantErr)
				return
			}
			if err == nil && result == "" {
				t.Errorf("shared.SafeJoin(%q, %q) returned empty path", tt.dir, tt.input)
			}
		})
	}
}

func TestApplicationData_NamespaceFromComponentRef(t *testing.T) {
	g := NewGenerator()
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

	input := &GeneratorInput{
		RecipeResult: recipeResult,
		Version:      "v0.9.0",
	}

	_, err := g.Generate(ctx, input, outputDir)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	// Verify namespace is used in application.yaml
	appPath := filepath.Join(outputDir, "gpu-operator", "application.yaml")
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
			if got := shared.IsSafePathComponent(tt.input); got != tt.want {
				t.Errorf("shared.IsSafePathComponent(%q) = %v, want %v", tt.input, got, tt.want)
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
			result := normalizeVersion(tt.input)
			if result != tt.expected {
				t.Errorf("normalizeVersion(%s) = %s, want %s", tt.input, result, tt.expected)
			}
		})
	}
}

// TestGenerate_Reproducible verifies that ArgoCD bundle generation is deterministic.
// Running Generate() twice with the same input should produce identical output files.
func TestGenerate_Reproducible(t *testing.T) {
	g := NewGenerator()
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

	input := &GeneratorInput{
		RecipeResult: recipeResult,
		ComponentValues: map[string]map[string]any{
			"cert-manager": {
				"installCRDs": true,
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

		_, err := g.Generate(ctx, input, outputDir)
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

	t.Logf("ArgoCD reproducibility verified: both iterations produced %d identical files", len(fileContents[0]))
}

// TestGenerate_NoTimestampInOutput verifies that generated files don't contain timestamps.
func TestGenerate_NoTimestampInOutput(t *testing.T) {
	g := NewGenerator()
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

	input := &GeneratorInput{
		RecipeResult: recipeResult,
		ComponentValues: map[string]map[string]any{
			"gpu-operator": {},
		},
		Version: "v0.9.0",
		RepoURL: "https://github.com/test/repo.git",
	}

	_, err := g.Generate(ctx, input, outputDir)
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
