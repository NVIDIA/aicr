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

	// Verify root files exist
	rootFiles := []string{"README.md", "deploy.sh"}
	for _, f := range rootFiles {
		path := filepath.Join(outputDir, f)
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			t.Errorf("expected root file %s does not exist", f)
		}
	}

	// Verify per-component directories
	for _, comp := range []string{"cert-manager", "gpu-operator"} {
		valuesPath := filepath.Join(outputDir, comp, "values.yaml")
		if _, statErr := os.Stat(valuesPath); os.IsNotExist(statErr) {
			t.Errorf("expected %s/values.yaml does not exist", comp)
		}
		readmePath := filepath.Join(outputDir, comp, "README.md")
		if _, statErr := os.Stat(readmePath); os.IsNotExist(statErr) {
			t.Errorf("expected %s/README.md does not exist", comp)
		}
	}

	// Verify cert-manager values contain installCRDs
	cmValues, err := os.ReadFile(filepath.Join(outputDir, "cert-manager", "values.yaml"))
	if err != nil {
		t.Fatalf("failed to read cert-manager values: %v", err)
	}
	if !strings.Contains(string(cmValues), "installCRDs") {
		t.Error("cert-manager/values.yaml missing installCRDs")
	}

	// Verify gpu-operator values contain driver
	gpuValues, err := os.ReadFile(filepath.Join(outputDir, "gpu-operator", "values.yaml"))
	if err != nil {
		t.Fatalf("failed to read gpu-operator values: %v", err)
	}
	if !strings.Contains(string(gpuValues), "driver") {
		t.Error("gpu-operator/values.yaml missing driver")
	}

	// No Chart.yaml should exist
	chartPath := filepath.Join(outputDir, "Chart.yaml")
	if _, statErr := os.Stat(chartPath); !os.IsNotExist(statErr) {
		t.Error("Chart.yaml should not exist in per-component bundle")
	}

	// Verify output has reasonable file count (2 root files + 2 component dirs × 2 files each = 6)
	if len(output.Files) < 6 {
		t.Errorf("expected at least 6 files, got %d", len(output.Files))
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

	// Check checksums.txt exists
	checksumPath := filepath.Join(outputDir, "checksums.txt")
	if _, statErr := os.Stat(checksumPath); os.IsNotExist(statErr) {
		t.Error("checksums.txt does not exist")
	}

	// Verify checksums.txt references per-component paths
	checksumContent, err := os.ReadFile(checksumPath)
	if err != nil {
		t.Fatalf("failed to read checksums.txt: %v", err)
	}
	content := string(checksumContent)

	if !strings.Contains(content, "README.md") {
		t.Error("checksums.txt missing README.md")
	}
	if !strings.Contains(content, "deploy.sh") {
		t.Error("checksums.txt missing deploy.sh")
	}
	if !strings.Contains(content, filepath.Join("cert-manager", "values.yaml")) {
		t.Error("checksums.txt missing cert-manager/values.yaml")
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

	// Verify checksums.txt is the last file (appended after generation)
	lastFile := output.Files[len(output.Files)-1]
	if !strings.HasSuffix(lastFile, "checksums.txt") {
		t.Errorf("expected last file to be checksums.txt, got %s", lastFile)
	}
}

func TestGenerate_WithManifests(t *testing.T) {
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
		ComponentManifests: map[string]map[string][]byte{
			"gpu-operator": {
				"components/gpu-operator/manifests/dcgm-exporter.yaml": []byte("apiVersion: v1\nkind: ConfigMap\n"),
			},
		},
	}

	_, err := g.Generate(ctx, input, outputDir)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	// Verify manifest was placed in component directory
	manifestPath := filepath.Join(outputDir, "gpu-operator", "manifests", "dcgm-exporter.yaml")
	if _, statErr := os.Stat(manifestPath); os.IsNotExist(statErr) {
		t.Error("gpu-operator/manifests/dcgm-exporter.yaml does not exist")
	}

	// Verify manifest content
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("failed to read manifest: %v", err)
	}
	if !strings.Contains(string(content), "ConfigMap") {
		t.Error("manifest content incorrect")
	}
}

func TestGenerate_DeployScriptExecutable(t *testing.T) {
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
		t.Fatalf("Generate failed: %v", err)
	}

	deployPath := filepath.Join(outputDir, "deploy.sh")
	info, statErr := os.Stat(deployPath)
	if os.IsNotExist(statErr) {
		t.Fatal("deploy.sh does not exist")
	}

	// Check executable permission (0755)
	mode := info.Mode()
	if mode&0111 == 0 {
		t.Errorf("deploy.sh is not executable, mode: %o", mode)
	}

	// Verify shebang
	content, err := os.ReadFile(deployPath)
	if err != nil {
		t.Fatalf("failed to read deploy.sh: %v", err)
	}
	if !strings.HasPrefix(string(content), "#!/usr/bin/env bash") {
		t.Error("deploy.sh missing shebang")
	}
	if !strings.Contains(string(content), "set -euo pipefail") {
		t.Error("deploy.sh missing strict mode")
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
	const (
		certManager     = "cert-manager"
		gpuOperator     = "gpu-operator"
		networkOperator = "network-operator"
	)

	components := []string{gpuOperator, certManager, networkOperator}
	deploymentOrder := []string{certManager, gpuOperator, networkOperator}

	sorted := SortComponentsByDeploymentOrder(components, deploymentOrder)

	if sorted[0] != certManager {
		t.Errorf("expected first component to be %s, got %s", certManager, sorted[0])
	}
	if sorted[1] != gpuOperator {
		t.Errorf("expected second component to be %s, got %s", gpuOperator, sorted[1])
	}
	if sorted[2] != networkOperator {
		t.Errorf("expected third component to be %s, got %s", networkOperator, sorted[2])
	}
}

func TestIsSafePathComponent(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"valid component name", "gpu-operator", true},
		{"valid with dots", "cert-manager", true},
		{"empty string", "", false},
		{"path traversal", "../etc/passwd", false},
		{"double dot", "..", false},
		{"forward slash", "gpu/operator", false},
		{"backslash", "gpu\\operator", false},
		{"embedded double dot", "foo..bar", false},
		{"leading dot dot slash", "../foo", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isSafePathComponent(tt.input)
			if result != tt.expected {
				t.Errorf("isSafePathComponent(%q) = %v, want %v", tt.input, result, tt.expected)
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
		{"absolute path is joined safely", baseDir, "/etc/passwd", false}, // Go's filepath.Join strips leading /
		{"empty name", baseDir, "", false},                                // empty joins to baseDir itself
		{"relative base", ".", "gpu-operator", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := safeJoin(tt.dir, tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("safeJoin(%q, %q) error = %v, wantErr %v", tt.dir, tt.input, err, tt.wantErr)
				return
			}
			if err == nil && result == "" {
				t.Errorf("safeJoin(%q, %q) returned empty path", tt.dir, tt.input)
			}
		})
	}
}

func TestBuildComponentDataListRejectsUnsafeNames(t *testing.T) {
	g := NewGenerator()
	input := &GeneratorInput{
		RecipeResult: &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{
				{Name: "../etc/passwd", Version: "v1.0.0", Source: "https://evil.com"},
			},
		},
	}

	_, err := g.buildComponentDataList(input)
	if err == nil {
		t.Error("expected error for unsafe component name, got nil")
	}
}

func TestGetNamespace(t *testing.T) {
	tests := []struct {
		name      string
		component string
		expected  string
	}{
		{"gpu-operator", "gpu-operator", "gpu-operator"},
		{"network-operator", "network-operator", "nvidia-network-operator"},
		{"cert-manager", "cert-manager", "cert-manager"},
		{"unknown", "some-component", defaultNamespace},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref := recipe.ComponentRef{Name: tt.component}
			result := getNamespace(ref)
			if result != tt.expected {
				t.Errorf("getNamespace(%q) = %q, want %q", tt.component, result, tt.expected)
			}
		})
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
