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

	"github.com/NVIDIA/eidos/pkg/bundler/deployer/shared"
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
	rootFiles := []string{"README.md", "deploy.sh", "undeploy.sh"}
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

	// Verify output has reasonable file count (3 root files + 2 component dirs × 2 files each = 7)
	if len(output.Files) < 7 {
		t.Errorf("expected at least 7 files, got %d", len(output.Files))
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
	if !strings.Contains(content, "undeploy.sh") {
		t.Error("checksums.txt missing undeploy.sh")
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

	manifestContent := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  namespace: {{ .Release.Namespace }}\n  labels:\n    helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}\n"

	input := &GeneratorInput{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {},
			"gpu-operator": {},
		},
		Version: "v1.0.0",
		ComponentManifests: map[string]map[string][]byte{
			"gpu-operator": {
				"components/gpu-operator/manifests/dcgm-exporter.yaml": []byte(manifestContent),
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

	// Verify manifest content was rendered with ComponentData
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("failed to read manifest: %v", err)
	}
	rendered := string(content)
	if !strings.Contains(rendered, "ConfigMap") {
		t.Error("manifest missing ConfigMap kind")
	}
	if !strings.Contains(rendered, "namespace: gpu-operator") {
		t.Errorf("manifest namespace not rendered, got: %s", rendered)
	}
	if !strings.Contains(rendered, "gpu-operator-25.3.3") { // normalizeVersion strips 'v' prefix for chart labels
		t.Errorf("manifest chart label not rendered, got: %s", rendered)
	}
}

func TestHasYAMLObjects(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		expected bool
	}{
		{"empty", "", false},
		{"whitespace only", "  \n  \n", false},
		{"comments only", "# comment\n# another\n", false},
		{"separator only", "---\n", false},
		{"comments and separators", "# Copyright\n# License\n---\n# more comments\n", false},
		{"valid YAML", "apiVersion: v1\nkind: ConfigMap\n", true},
		{"comments then YAML", "# header\napiVersion: v1\n", true},
		{"separator then YAML", "---\napiVersion: v1\n", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hasYAMLObjects([]byte(tt.content))
			if result != tt.expected {
				t.Errorf("hasYAMLObjects(%q) = %v, want %v", tt.content, result, tt.expected)
			}
		})
	}
}

func TestGenerate_EmptyManifestsSkipped(t *testing.T) {
	g := NewGenerator()
	ctx := context.Background()
	outputDir := t.TempDir()

	// Template that renders to empty when enabled=false
	emptyTemplate := "# Comment\n{{- $cust := index .Values \"gpu-operator\" }}\n{{- if ne (toString (index $cust \"enabled\")) \"false\" }}\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n{{- end }}\n"

	input := &GeneratorInput{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {},
			"gpu-operator": {"enabled": "false"},
		},
		Version: "v1.0.0",
		ComponentManifests: map[string]map[string][]byte{
			"gpu-operator": {
				"components/gpu-operator/manifests/test.yaml": []byte(emptyTemplate),
			},
		},
	}

	output, err := g.Generate(ctx, input, outputDir)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	// Manifest should not exist (rendered to empty)
	manifestPath := filepath.Join(outputDir, "gpu-operator", "manifests", "test.yaml")
	if _, statErr := os.Stat(manifestPath); !os.IsNotExist(statErr) {
		t.Error("expected empty manifest to be skipped, but file exists")
	}

	// Manifests dir should not exist (removed when empty)
	manifestDir := filepath.Join(outputDir, "gpu-operator", "manifests")
	if _, statErr := os.Stat(manifestDir); !os.IsNotExist(statErr) {
		t.Error("expected empty manifests directory to be removed")
	}

	// deploy.sh should NOT contain kubectl apply for gpu-operator manifests
	deployPath := filepath.Join(outputDir, "deploy.sh")
	deployContent, err := os.ReadFile(deployPath)
	if err != nil {
		t.Fatalf("failed to read deploy.sh: %v", err)
	}
	if strings.Contains(string(deployContent), "Applying manifests for gpu-operator") {
		t.Error("deploy.sh should not contain manifest apply for disabled component")
	}

	_ = output
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

func TestGenerate_UndeployScriptExecutable(t *testing.T) {
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

	undeployPath := filepath.Join(outputDir, "undeploy.sh")
	info, statErr := os.Stat(undeployPath)
	if os.IsNotExist(statErr) {
		t.Fatal("undeploy.sh does not exist")
	}

	// Check executable permission (0755)
	mode := info.Mode()
	if mode&0111 == 0 {
		t.Errorf("undeploy.sh is not executable, mode: %o", mode)
	}

	// Verify content
	content, err := os.ReadFile(undeployPath)
	if err != nil {
		t.Fatalf("failed to read undeploy.sh: %v", err)
	}
	script := string(content)

	if !strings.HasPrefix(script, "#!/usr/bin/env bash") {
		t.Error("undeploy.sh missing shebang")
	}
	if !strings.Contains(script, "set -euo pipefail") {
		t.Error("undeploy.sh missing strict mode")
	}
	if !strings.Contains(script, "helm uninstall") {
		t.Error("undeploy.sh missing helm uninstall command")
	}

	// Verify reverse order: gpu-operator should appear before cert-manager
	gpuIdx := strings.Index(script, "Uninstalling gpu-operator")
	certIdx := strings.Index(script, "Uninstalling cert-manager")
	if gpuIdx < 0 || certIdx < 0 {
		t.Fatal("undeploy.sh missing component uninstall lines")
	}
	if gpuIdx > certIdx {
		t.Error("undeploy.sh components not in reverse order: gpu-operator should come before cert-manager")
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

func TestSortComponentsByDeploymentOrder(t *testing.T) {
	const (
		certManager     = "cert-manager"
		gpuOperator     = "gpu-operator"
		networkOperator = "network-operator"
	)

	t.Run("all in order map", func(t *testing.T) {
		components := []string{gpuOperator, certManager, networkOperator}
		deploymentOrder := []string{certManager, gpuOperator, networkOperator}

		sorted := SortComponentsByDeploymentOrder(components, deploymentOrder)

		if sorted[0] != certManager {
			t.Errorf("expected first %s, got %s", certManager, sorted[0])
		}
		if sorted[1] != gpuOperator {
			t.Errorf("expected second %s, got %s", gpuOperator, sorted[1])
		}
		if sorted[2] != networkOperator {
			t.Errorf("expected third %s, got %s", networkOperator, sorted[2])
		}
	})

	t.Run("only one in order map", func(t *testing.T) {
		// "alpha" is not in the order map, gpuOperator is.
		// gpuOperator should come first (okI branch).
		components := []string{"alpha", gpuOperator}
		deploymentOrder := []string{gpuOperator}

		sorted := SortComponentsByDeploymentOrder(components, deploymentOrder)
		if sorted[0] != gpuOperator {
			t.Errorf("expected ordered component first, got %s", sorted[0])
		}
	})

	t.Run("only j in order map", func(t *testing.T) {
		// "zebra" is not in the order map, certManager is.
		// certManager should sort after "zebra" would normally, but since
		// certManager is in the map and zebra is not, certManager gets priority=false (okJ branch).
		components := []string{certManager, "zebra"}
		deploymentOrder := []string{certManager}

		sorted := SortComponentsByDeploymentOrder(components, deploymentOrder)
		if sorted[0] != certManager {
			t.Errorf("expected ordered component first, got %s", sorted[0])
		}
	})

	t.Run("neither in order map", func(t *testing.T) {
		// Both unknown — should fall back to alphabetical.
		components := []string{"zebra", "alpha"}
		deploymentOrder := []string{gpuOperator}

		sorted := SortComponentsByDeploymentOrder(components, deploymentOrder)
		if sorted[0] != "alpha" {
			t.Errorf("expected alphabetical first, got %s", sorted[0])
		}
		if sorted[1] != "zebra" {
			t.Errorf("expected alphabetical second, got %s", sorted[1])
		}
	})

	t.Run("empty deployment order", func(t *testing.T) {
		components := []string{"b", "a"}
		sorted := SortComponentsByDeploymentOrder(components, nil)
		if sorted[0] != "a" {
			t.Errorf("expected alphabetical with empty order, got %s", sorted[0])
		}
	})
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
			result := shared.IsSafePathComponent(tt.input)
			if result != tt.expected {
				t.Errorf("shared.IsSafePathComponent(%q) = %v, want %v", tt.input, result, tt.expected)
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

func TestBuildComponentDataList_NamespaceAndChart(t *testing.T) {
	const (
		gpuOp   = "gpu-operator"
		certMgr = "cert-manager"
		unknown = "unknown"
	)

	g := NewGenerator()
	input := &GeneratorInput{
		RecipeResult: &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{
				{Name: gpuOp, Namespace: gpuOp, Chart: gpuOp, Version: "v1.0.0", Source: "https://example.com"},
				{Name: certMgr, Namespace: certMgr, Chart: certMgr, Version: "v1.0.0", Source: "https://example.com"},
				{Name: unknown, Version: "v1.0.0", Source: "https://example.com"},
			},
		},
	}

	components, err := g.buildComponentDataList(input)
	if err != nil {
		t.Fatalf("buildComponentDataList failed: %v", err)
	}

	for _, comp := range components {
		switch comp.Name {
		case gpuOp:
			if comp.Namespace != gpuOp {
				t.Errorf("gpu-operator namespace = %q, want %q", comp.Namespace, gpuOp)
			}
			if comp.ChartName != gpuOp {
				t.Errorf("gpu-operator chart = %q, want %q", comp.ChartName, gpuOp)
			}
		case certMgr:
			if comp.Namespace != certMgr {
				t.Errorf("cert-manager namespace = %q, want %q", comp.Namespace, certMgr)
			}
		case unknown:
			if comp.Namespace != "" {
				t.Errorf("unknown namespace = %q, want empty", comp.Namespace)
			}
			if comp.ChartName != unknown {
				t.Errorf("unknown chart = %q, want %q (fallback to name)", comp.ChartName, unknown)
			}
		}
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
				Name:      "cert-manager",
				Namespace: "cert-manager",
				Chart:     "cert-manager",
				Version:   "v1.17.2",
				Source:    "https://charts.jetstack.io",
			},
			{
				Name:      "gpu-operator",
				Namespace: "gpu-operator",
				Chart:     "gpu-operator",
				Version:   "v25.3.3",
				Source:    "https://helm.ngc.nvidia.com/nvidia",
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

func TestHelmFuncMap(t *testing.T) {
	funcs := helmFuncMap()

	t.Run("toYaml", func(t *testing.T) {
		fn := funcs["toYaml"].(func(any) string)

		// map input
		got := fn(map[string]string{"key": "value"})
		if !strings.Contains(got, "key: value") {
			t.Errorf("toYaml(map) = %q, want to contain 'key: value'", got)
		}

		// string input
		got = fn("hello")
		if got != "hello" {
			t.Errorf("toYaml(string) = %q, want 'hello'", got)
		}

		// nil input
		got = fn(nil)
		if got != "null" {
			t.Errorf("toYaml(nil) = %q, want 'null'", got)
		}
	})

	t.Run("nindent", func(t *testing.T) {
		fn := funcs["nindent"].(func(int, string) string)
		got := fn(4, "line1\nline2")
		if !strings.Contains(got, "    line1") {
			t.Errorf("nindent should indent line1: got %q", got)
		}
		if !strings.Contains(got, "    line2") {
			t.Errorf("nindent should indent line2: got %q", got)
		}
		if got[0] != '\n' {
			t.Error("nindent should start with newline")
		}
	})

	t.Run("toString", func(t *testing.T) {
		fn := funcs["toString"].(func(any) string)
		if got := fn(42); got != "42" {
			t.Errorf("toString(42) = %q, want '42'", got)
		}
		if got := fn(nil); got != "<nil>" {
			t.Errorf("toString(nil) = %q, want '<nil>'", got)
		}
	})

	t.Run("default", func(t *testing.T) {
		fn := funcs["default"].(func(any, any) any)

		// nil value returns default
		if got := fn("fallback", nil); got != "fallback" {
			t.Errorf("default(fallback, nil) = %v, want 'fallback'", got)
		}
		// empty string returns default
		if got := fn("fallback", ""); got != "fallback" {
			t.Errorf("default(fallback, empty) = %v, want 'fallback'", got)
		}
		// non-empty returns value
		if got := fn("fallback", "actual"); got != "actual" {
			t.Errorf("default(fallback, actual) = %v, want 'actual'", got)
		}
		// non-string non-nil returns value
		if got := fn("fallback", 42); got != 42 {
			t.Errorf("default(fallback, 42) = %v, want 42", got)
		}
	})
}
