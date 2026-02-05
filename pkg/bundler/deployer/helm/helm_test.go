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

package helm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/eidos/pkg/recipe"
)

func TestNewGenerator(t *testing.T) {
	g := NewGenerator()
	if g == nil {
		t.Fatal("NewGenerator returned nil")
	}
}

func TestGenerate_Success(t *testing.T) {
	g := NewGenerator()
	ctx := context.Background()
	outputDir := t.TempDir()

	input := &GeneratorInput{
		RecipeResult: createTestRecipeResult(),
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
		Version: "v1.0.0",
	}

	output, err := g.Generate(ctx, input, outputDir)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	// Verify output
	if len(output.Files) != 3 {
		t.Errorf("expected 3 files, got %d", len(output.Files))
	}

	// Check files exist
	expectedFiles := []string{"Chart.yaml", "values.yaml", "README.md"}
	for _, f := range expectedFiles {
		path := filepath.Join(outputDir, f)
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			t.Errorf("expected file %s does not exist", f)
		}
	}

	// Verify Chart.yaml content
	chartContent, err := os.ReadFile(filepath.Join(outputDir, "Chart.yaml"))
	if err != nil {
		t.Fatalf("failed to read Chart.yaml: %v", err)
	}
	if !strings.Contains(string(chartContent), "cert-manager") {
		t.Error("Chart.yaml missing cert-manager dependency")
	}
	if !strings.Contains(string(chartContent), "gpu-operator") {
		t.Error("Chart.yaml missing gpu-operator dependency")
	}

	// Verify values.yaml content
	valuesContent, err := os.ReadFile(filepath.Join(outputDir, "values.yaml"))
	if err != nil {
		t.Fatalf("failed to read values.yaml: %v", err)
	}
	if !strings.Contains(string(valuesContent), "cert-manager") {
		t.Error("values.yaml missing cert-manager values")
	}
	if !strings.Contains(string(valuesContent), "gpu-operator") {
		t.Error("values.yaml missing gpu-operator values")
	}
	if !strings.Contains(string(valuesContent), "enabled: true") {
		t.Error("values.yaml missing enabled flag")
	}
}

func TestGenerate_NilInput(t *testing.T) {
	g := NewGenerator()
	ctx := context.Background()

	_, err := g.Generate(ctx, nil, t.TempDir())
	if err == nil {
		t.Error("expected error for nil input")
	}
}

func TestGenerate_NilRecipeResult(t *testing.T) {
	g := NewGenerator()
	ctx := context.Background()

	input := &GeneratorInput{
		RecipeResult: nil,
	}

	_, err := g.Generate(ctx, input, t.TempDir())
	if err == nil {
		t.Error("expected error for nil recipe result")
	}
}

func TestGenerate_ContextCancellation(t *testing.T) {
	g := NewGenerator()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	input := &GeneratorInput{
		RecipeResult:    createEmptyRecipeResult(),
		ComponentValues: map[string]map[string]any{},
		Version:         "v1.0.0",
	}

	_, err := g.Generate(ctx, input, t.TempDir())
	if err == nil {
		t.Error("expected error for cancelled context")
	}
}

func TestGenerate_WithChecksums(t *testing.T) {
	g := NewGenerator()
	ctx := context.Background()
	outputDir := t.TempDir()

	input := &GeneratorInput{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {"installCRDs": true},
			"gpu-operator": {"enabled": true},
		},
		Version:          "v1.0.0",
		IncludeChecksums: true,
	}

	output, err := g.Generate(ctx, input, outputDir)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	// Should have 4 files: Chart.yaml, values.yaml, README.md, checksums.txt
	if len(output.Files) != 4 {
		t.Errorf("expected 4 files, got %d", len(output.Files))
	}

	// Check checksums.txt exists
	checksumPath := filepath.Join(outputDir, "checksums.txt")
	if _, statErr := os.Stat(checksumPath); os.IsNotExist(statErr) {
		t.Error("checksums.txt does not exist")
	}

	// Verify checksums.txt content
	checksumContent, err := os.ReadFile(checksumPath)
	if err != nil {
		t.Fatalf("failed to read checksums.txt: %v", err)
	}
	content := string(checksumContent)

	// Should contain hashes for the 3 main files
	if !strings.Contains(content, "Chart.yaml") {
		t.Error("checksums.txt missing Chart.yaml")
	}
	if !strings.Contains(content, "values.yaml") {
		t.Error("checksums.txt missing values.yaml")
	}
	if !strings.Contains(content, "README.md") {
		t.Error("checksums.txt missing README.md")
	}

	// Each line should have 64-char SHA256 hash
	lines := strings.Split(strings.TrimSpace(content), "\n")
	for _, line := range lines {
		parts := strings.Split(line, "  ")
		if len(parts) != 2 {
			t.Errorf("invalid checksum format: %s", line)
			continue
		}
		if len(parts[0]) != 64 {
			t.Errorf("expected 64 char hash, got %d: %s", len(parts[0]), parts[0])
		}
	}
}

func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"v1.0.0", "1.0.0"},
		{"1.0.0", "1.0.0"},
		{"v0.1.0-alpha", "0.1.0-alpha"},
		{"", "0.1.0"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := normalizeVersion(tt.input)
			if result != tt.expected {
				t.Errorf("normalizeVersion(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestResolveChartName(t *testing.T) {
	tests := []struct {
		name          string
		componentName string
		expected      string
	}{
		{
			name:          "kube-prometheus-stack resolves to kube-prometheus-stack",
			componentName: "kube-prometheus-stack",
			expected:      "kube-prometheus-stack",
		},
		{
			name:          "gpu-operator resolves to gpu-operator",
			componentName: "gpu-operator",
			expected:      "gpu-operator",
		},
		{
			name:          "cert-manager resolves to cert-manager",
			componentName: "cert-manager",
			expected:      "cert-manager",
		},
		{
			name:          "skyhook-operator resolves to skyhook-operator",
			componentName: "skyhook-operator",
			expected:      "skyhook-operator",
		},
		{
			name:          "unknown component falls back to component name",
			componentName: "unknown-component",
			expected:      "unknown-component",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := resolveChartName(tt.componentName)
			if result != tt.expected {
				t.Errorf("resolveChartName(%q) = %q, want %q", tt.componentName, result, tt.expected)
			}
		})
	}
}

func TestSortComponentsByDeploymentOrder(t *testing.T) {
	components := []string{"gpu-operator", "cert-manager", "network-operator"}
	deploymentOrder := []string{"cert-manager", "gpu-operator", "network-operator"}

	sorted := SortComponentsByDeploymentOrder(components, deploymentOrder)

	if sorted[0] != "cert-manager" {
		t.Errorf("expected first component to be cert-manager, got %s", sorted[0])
	}
	if sorted[1] != "gpu-operator" {
		t.Errorf("expected second component to be gpu-operator, got %s", sorted[1])
	}
	if sorted[2] != "network-operator" {
		t.Errorf("expected third component to be network-operator, got %s", sorted[2])
	}
}

// Helper functions

func createTestRecipeResult() *recipe.RecipeResult {
	return &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: "eidos.nvidia.com/v1alpha1",
		Metadata: struct {
			Version            string                     `json:"version,omitempty" yaml:"version,omitempty"`
			AppliedOverlays    []string                   `json:"appliedOverlays,omitempty" yaml:"appliedOverlays,omitempty"`
			ExcludedOverlays   []string                   `json:"excludedOverlays,omitempty" yaml:"excludedOverlays,omitempty"`
			ConstraintWarnings []recipe.ConstraintWarning `json:"constraintWarnings,omitempty" yaml:"constraintWarnings,omitempty"`
		}{
			Version: "v0.1.0",
		},
		Criteria: &recipe.Criteria{
			Service:     "eks",
			Accelerator: "h100",
			Intent:      "training",
		},
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "cert-manager",
				Version: "v1.17.2",
				Source:  "https://charts.jetstack.io",
			},
			{
				Name:    "gpu-operator",
				Version: "v25.3.3",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
		},
		DeploymentOrder: []string{"cert-manager", "gpu-operator"},
	}
}

func createEmptyRecipeResult() *recipe.RecipeResult {
	return &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: "eidos.nvidia.com/v1alpha1",
		Metadata: struct {
			Version            string                     `json:"version,omitempty" yaml:"version,omitempty"`
			AppliedOverlays    []string                   `json:"appliedOverlays,omitempty" yaml:"appliedOverlays,omitempty"`
			ExcludedOverlays   []string                   `json:"excludedOverlays,omitempty" yaml:"excludedOverlays,omitempty"`
			ConstraintWarnings []recipe.ConstraintWarning `json:"constraintWarnings,omitempty" yaml:"constraintWarnings,omitempty"`
		}{
			Version: "v0.1.0",
		},
		ComponentRefs:   []recipe.ComponentRef{},
		DeploymentOrder: []string{},
	}
}

// TestGenerate_Reproducible verifies that Helm bundle generation is deterministic.
// Running Generate() twice with the same input should produce identical output files.
func TestGenerate_Reproducible(t *testing.T) {
	g := NewGenerator()
	ctx := context.Background()

	input := &GeneratorInput{
		RecipeResult: createTestRecipeResult(),
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
		Version: "v1.0.0",
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

	t.Logf("Helm reproducibility verified: both iterations produced %d identical files", len(fileContents[0]))
}

// TestGenerate_NoTimestampInOutput verifies that generated files don't contain timestamps.
func TestGenerate_NoTimestampInOutput(t *testing.T) {
	g := NewGenerator()
	ctx := context.Background()
	outputDir := t.TempDir()

	input := &GeneratorInput{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {},
			"gpu-operator": {},
		},
		Version: "v1.0.0",
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

func TestIsCRDDependentResource(t *testing.T) {
	g := NewGenerator()

	tests := []struct {
		name     string
		content  string
		expected bool
	}{
		{
			name:     "standard ConfigMap",
			content:  "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test",
			expected: false,
		},
		{
			name:     "standard Deployment",
			content:  "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: test",
			expected: false,
		},
		{
			name:     "custom resource",
			content:  "apiVersion: skyhook.nvidia.com/v1alpha1\nkind: Skyhook\nmetadata:\n  name: test",
			expected: true,
		},
		{
			name: "custom resource with Helm template expressions",
			content: `{{- $cust := index .Values "skyhook-customizations" }}
---
apiVersion: skyhook.nvidia.com/v1alpha1
kind: Skyhook
metadata:
  labels:
    app.kubernetes.io/managed-by: {{ .Release.Service }}
    helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
  name: test
  namespace: {{ .Release.Namespace }}`,
			expected: true,
		},
		{
			name:     "no apiVersion",
			content:  "kind: ConfigMap\nmetadata:\n  name: test",
			expected: false,
		},
		{
			name:     "RBAC resource",
			content:  "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: test",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := g.isCRDDependentResource([]byte(tt.content))
			if result != tt.expected {
				t.Errorf("isCRDDependentResource() = %v, want %v", result, tt.expected)
			}
		})
	}
}
