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

package helm

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/shared"
	"github.com/NVIDIA/aicr/pkg/recipe"
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
				"crds": map[string]any{"enabled": true},
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

	// Verify cert-manager values contain crds.enabled
	cmValues, err := os.ReadFile(filepath.Join(outputDir, "cert-manager", "values.yaml"))
	if err != nil {
		t.Fatalf("failed to read cert-manager values: %v", err)
	}
	if !strings.Contains(string(cmValues), "crds") {
		t.Error("cert-manager/values.yaml missing crds section")
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
			"cert-manager": {"crds": map[string]any{"enabled": true}},
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
	if !strings.Contains(string(content), "MAX_RETRIES=5") {
		t.Error("deploy.sh missing default MAX_RETRIES")
	}
	if !strings.Contains(string(content), "backoff_seconds()") {
		t.Error("deploy.sh missing backoff_seconds function")
	}
	if !strings.Contains(string(content), "retry()") {
		t.Error("deploy.sh missing retry function")
	}
	if !strings.Contains(string(content), "helm_retry()") {
		t.Error("deploy.sh missing helm_retry function")
	}
	if !strings.Contains(string(content), "cleanup_helm_hooks()") {
		t.Error("deploy.sh missing cleanup_helm_hooks function")
	}
	if !strings.Contains(string(content), "HELM_TIMEOUT=") {
		t.Error("deploy.sh missing HELM_TIMEOUT variable")
	}
	if !strings.Contains(string(content), "NO_WAIT=") {
		t.Error("deploy.sh missing NO_WAIT variable")
	}
	if !strings.Contains(string(content), "--retries") {
		t.Error("deploy.sh missing --retries flag handling")
	}
}

func TestGenerate_DeployScriptKaiSchedulerTimeout(t *testing.T) {
	g := NewGenerator()
	ctx := context.Background()
	outputDir := t.TempDir()

	input := &GeneratorInput{
		RecipeResult: &recipe.RecipeResult{
			Kind:       "RecipeResult",
			APIVersion: "aicr.nvidia.com/v1alpha1",
			ComponentRefs: []recipe.ComponentRef{
				{
					Name:      "kai-scheduler",
					Namespace: "kai-scheduler",
					Chart:     "kai-scheduler",
					Version:   "v0.13.0",
					Type:      recipe.ComponentTypeHelm,
					Source:    "oci://ghcr.io/nvidia/kai-scheduler",
				},
			},
			DeploymentOrder: []string{"kai-scheduler"},
		},
		ComponentValues: map[string]map[string]any{
			"kai-scheduler": {},
		},
		Version: "v1.0.0",
	}

	_, err := g.Generate(ctx, input, outputDir)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(outputDir, "deploy.sh"))
	if err != nil {
		t.Fatalf("failed to read deploy.sh: %v", err)
	}
	script := string(content)

	// kai-scheduler should get a custom 20m timeout override
	if !strings.Contains(script, `COMPONENT_HELM_TIMEOUT="20m"`) {
		t.Error("deploy.sh missing kai-scheduler 20m timeout override")
	}
	// Other components should use the default HELM_TIMEOUT
	if !strings.Contains(script, `COMPONENT_HELM_TIMEOUT="${HELM_TIMEOUT}"`) {
		t.Error("deploy.sh missing default COMPONENT_HELM_TIMEOUT")
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

	// Verify --delete-pvcs flag defaults to off
	if !strings.Contains(script, "DELETE_PVCS=false") {
		t.Error("undeploy.sh missing DELETE_PVCS=false default")
	}
	if !strings.Contains(script, "--delete-pvcs") {
		t.Error("undeploy.sh missing --delete-pvcs flag handling")
	}

	// Verify PVC deletion is guarded by the flag
	if !strings.Contains(script, `"${DELETE_PVCS}" == "true"`) {
		t.Error("undeploy.sh PVC deletion not guarded by DELETE_PVCS flag")
	}

	// Verify no unconditional PVC deletion inside per-component loop
	// PVC deletion should only appear in the namespace cleanup section
	lines := strings.Split(script, "\n")
	inComponentLoop := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "Uninstalling") && strings.Contains(trimmed, "echo") {
			inComponentLoop = true
		}
		if strings.Contains(trimmed, "Clean up namespaces") {
			inComponentLoop = false
		}
		if inComponentLoop && strings.Contains(trimmed, "kubectl delete pvc") {
			t.Error("undeploy.sh has unconditional PVC deletion inside per-component loop")
		}
	}

	// Verify webhook cleanup runs both before and after namespace deletion
	nsCleanupIdx := strings.Index(script, "Clean up namespaces")
	nsTermIdx := strings.Index(script, "Waiting for namespaces to terminate")
	finalWebhookIdx := strings.Index(script, "Final webhook cleanup")
	if nsCleanupIdx < 0 || nsTermIdx < 0 || finalWebhookIdx < 0 {
		t.Fatal("undeploy.sh missing expected section markers")
	}

	// Webhook cleanup should appear in namespace cleanup section (before delete_namespace)
	betweenCleanupAndTerm := script[nsCleanupIdx:nsTermIdx]
	if !strings.Contains(betweenCleanupAndTerm, "delete_orphaned_webhooks_for_ns") {
		t.Error("undeploy.sh missing pre-namespace-deletion webhook cleanup")
	}

	// Final webhook cleanup should appear after namespace termination wait
	if finalWebhookIdx < nsTermIdx {
		t.Error("undeploy.sh final webhook cleanup should run after namespace termination wait")
	}
	afterTermWait := script[nsTermIdx:]
	if !strings.Contains(afterTermWait, "delete_orphaned_webhooks_for_ns") {
		t.Error("undeploy.sh missing post-namespace-deletion webhook cleanup")
	}
}

func TestUniqueNamespaces(t *testing.T) {
	tests := []struct {
		name       string
		components []ComponentData
		expected   []string
	}{
		{
			name: "deduplicates shared namespaces",
			components: []ComponentData{
				{Name: "prometheus-adapter", Namespace: "monitoring", HasChart: true},
				{Name: "k8s-ephemeral", Namespace: "monitoring", HasChart: true},
				{Name: "kube-prometheus", Namespace: "monitoring", HasChart: true},
				{Name: "gpu-operator", Namespace: "gpu-operator", HasChart: true},
			},
			expected: []string{"monitoring", "gpu-operator"},
		},
		{
			name: "excludes manifest-only components",
			components: []ComponentData{
				{Name: "my-manifests", Namespace: "custom-ns", HasManifests: true},
				{Name: "gpu-operator", Namespace: "gpu-operator", HasChart: true},
			},
			expected: []string{"gpu-operator"},
		},
		{
			name: "includes kustomize components",
			components: []ComponentData{
				{Name: "my-kustomize", Namespace: "kustomize-ns", IsKustomize: true},
				{Name: "gpu-operator", Namespace: "gpu-operator", HasChart: true},
			},
			expected: []string{"kustomize-ns", "gpu-operator"},
		},
		{
			name:       "empty input",
			components: []ComponentData{},
			expected:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := uniqueNamespaces(tt.components)
			if len(result) != len(tt.expected) {
				t.Fatalf("got %v, want %v", result, tt.expected)
			}
			for i, ns := range result {
				if ns != tt.expected[i] {
					t.Errorf("index %d: got %q, want %q", i, ns, tt.expected[i])
				}
			}
		})
	}
}

func TestNormalizeVersionWithDefault(t *testing.T) {
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
			result := shared.NormalizeVersionWithDefault(tt.input)
			if result != tt.expected {
				t.Errorf("NormalizeVersionWithDefault(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestSortComponentNamesByDeploymentOrder(t *testing.T) {
	const (
		certManager     = "cert-manager"
		gpuOperator     = "gpu-operator"
		networkOperator = "network-operator"
	)

	t.Run("all in order map", func(t *testing.T) {
		components := []string{gpuOperator, certManager, networkOperator}
		deploymentOrder := []string{certManager, gpuOperator, networkOperator}

		sorted := shared.SortComponentNamesByDeploymentOrder(components, deploymentOrder)

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

		sorted := shared.SortComponentNamesByDeploymentOrder(components, deploymentOrder)
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

		sorted := shared.SortComponentNamesByDeploymentOrder(components, deploymentOrder)
		if sorted[0] != certManager {
			t.Errorf("expected ordered component first, got %s", sorted[0])
		}
	})

	t.Run("neither in order map", func(t *testing.T) {
		// Both unknown — should fall back to alphabetical.
		components := []string{"zebra", "alpha"}
		deploymentOrder := []string{gpuOperator}

		sorted := shared.SortComponentNamesByDeploymentOrder(components, deploymentOrder)
		if sorted[0] != "alpha" {
			t.Errorf("expected alphabetical first, got %s", sorted[0])
		}
		if sorted[1] != "zebra" {
			t.Errorf("expected alphabetical second, got %s", sorted[1])
		}
	})

	t.Run("empty deployment order", func(t *testing.T) {
		components := []string{"b", "a"}
		sorted := shared.SortComponentNamesByDeploymentOrder(components, nil)
		if sorted[0] != "b" {
			t.Errorf("expected original order preserved with empty order, got %s", sorted[0])
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

func TestGenerate_KustomizeOnly(t *testing.T) {
	g := NewGenerator()
	ctx := context.Background()
	outputDir := t.TempDir()

	input := &GeneratorInput{
		RecipeResult: createKustomizeRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"my-kustomize-app": {},
		},
		Version: "v1.0.0",
	}

	output, err := g.Generate(ctx, input, outputDir)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	// Verify root files exist
	for _, f := range []string{"README.md", "deploy.sh", "undeploy.sh"} {
		path := filepath.Join(outputDir, f)
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			t.Errorf("expected root file %s does not exist", f)
		}
	}

	// Verify component directory exists with README
	readmePath := filepath.Join(outputDir, "my-kustomize-app", "README.md")
	if _, statErr := os.Stat(readmePath); os.IsNotExist(statErr) {
		t.Error("expected my-kustomize-app/README.md does not exist")
	}

	// deploy.sh should contain kustomize build, NOT helm upgrade
	deployContent, err := os.ReadFile(filepath.Join(outputDir, "deploy.sh"))
	if err != nil {
		t.Fatalf("failed to read deploy.sh: %v", err)
	}
	deployScript := string(deployContent)

	if !strings.Contains(deployScript, "kustomize build") {
		t.Error("deploy.sh missing kustomize build command")
	}
	if strings.Contains(deployScript, "helm upgrade") {
		t.Error("deploy.sh should not contain helm upgrade for kustomize-only bundle")
	}
	if !strings.Contains(deployScript, "via kustomize") {
		t.Error("deploy.sh should indicate kustomize deployment")
	}
	if !strings.Contains(deployScript, "ref=v1.0.0") {
		t.Error("deploy.sh should contain kustomize tag ref")
	}
	if !strings.Contains(deployScript, "deploy/production") {
		t.Error("deploy.sh should contain kustomize path")
	}

	// undeploy.sh should contain kustomize build for deletion
	undeployContent, err := os.ReadFile(filepath.Join(outputDir, "undeploy.sh"))
	if err != nil {
		t.Fatalf("failed to read undeploy.sh: %v", err)
	}
	undeployScript := string(undeployContent)

	if !strings.Contains(undeployScript, "kustomize build") {
		t.Error("undeploy.sh missing kustomize build command")
	}
	if strings.Contains(undeployScript, "helm_force_uninstall \"my-kustomize-app\"") {
		t.Error("undeploy.sh should not call helm_force_uninstall for kustomize-only bundle")
	}

	// Component README should show kustomize instructions
	compReadme, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("failed to read component README: %v", err)
	}
	compReadmeStr := string(compReadme)
	if !strings.Contains(compReadmeStr, "kustomize build") {
		t.Error("component README should contain kustomize build instructions")
	}
	if strings.Contains(compReadmeStr, "helm upgrade") {
		t.Error("component README should not contain helm commands for kustomize component")
	}

	if len(output.Files) < 4 {
		t.Errorf("expected at least 4 files, got %d", len(output.Files))
	}
}

func TestGenerate_MixedHelmAndKustomize(t *testing.T) {
	g := NewGenerator()
	ctx := context.Background()
	outputDir := t.TempDir()

	input := &GeneratorInput{
		RecipeResult: createMixedRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager":     {"crds": map[string]any{"enabled": true}},
			"my-kustomize-app": {},
		},
		Version: "v1.0.0",
	}

	output, err := g.Generate(ctx, input, outputDir)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	// Verify both component directories exist
	for _, comp := range []string{"cert-manager", "my-kustomize-app"} {
		readmePath := filepath.Join(outputDir, comp, "README.md")
		if _, statErr := os.Stat(readmePath); os.IsNotExist(statErr) {
			t.Errorf("expected %s/README.md does not exist", comp)
		}
	}

	// deploy.sh should contain BOTH helm and kustomize commands
	deployContent, err := os.ReadFile(filepath.Join(outputDir, "deploy.sh"))
	if err != nil {
		t.Fatalf("failed to read deploy.sh: %v", err)
	}
	deployScript := string(deployContent)

	if !strings.Contains(deployScript, "helm upgrade") {
		t.Error("deploy.sh missing helm upgrade for Helm component")
	}
	if !strings.Contains(deployScript, "kustomize build") {
		t.Error("deploy.sh missing kustomize build for Kustomize component")
	}

	// undeploy.sh should contain BOTH helm and kustomize commands
	undeployContent, err := os.ReadFile(filepath.Join(outputDir, "undeploy.sh"))
	if err != nil {
		t.Fatalf("failed to read undeploy.sh: %v", err)
	}
	undeployScript := string(undeployContent)

	if !strings.Contains(undeployScript, "helm uninstall") {
		t.Error("undeploy.sh missing helm uninstall for Helm component")
	}
	if !strings.Contains(undeployScript, "kustomize build") {
		t.Error("undeploy.sh missing kustomize build for Kustomize component")
	}

	// Root README should show both types
	rootReadme, err := os.ReadFile(filepath.Join(outputDir, "README.md"))
	if err != nil {
		t.Fatalf("failed to read README.md: %v", err)
	}
	rootReadmeStr := string(rootReadme)
	if !strings.Contains(rootReadmeStr, "Helm") {
		t.Error("root README should indicate Helm type")
	}
	if !strings.Contains(rootReadmeStr, "Kustomize") {
		t.Error("root README should indicate Kustomize type")
	}

	if len(output.Files) < 7 {
		t.Errorf("expected at least 7 files, got %d", len(output.Files))
	}
}

func TestBuildComponentDataList_Kustomize(t *testing.T) {
	g := NewGenerator()

	input := &GeneratorInput{
		RecipeResult: &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{
				{
					Name:      "my-kustomize-app",
					Namespace: "my-app",
					Type:      recipe.ComponentTypeKustomize,
					Source:    "https://github.com/example/repo",
					Tag:       "v2.0.0",
					Path:      "deploy/production",
				},
			},
		},
	}

	components, err := g.buildComponentDataList(input)
	if err != nil {
		t.Fatalf("buildComponentDataList failed: %v", err)
	}

	if len(components) != 1 {
		t.Fatalf("expected 1 component, got %d", len(components))
	}

	comp := components[0]
	if !comp.IsKustomize {
		t.Error("expected IsKustomize to be true")
	}
	if comp.HasChart {
		t.Error("expected HasChart to be false for kustomize component")
	}
	if comp.Tag != "v2.0.0" {
		t.Errorf("expected Tag v2.0.0, got %s", comp.Tag)
	}
	if comp.Path != "deploy/production" {
		t.Errorf("expected Path deploy/production, got %s", comp.Path)
	}
	if comp.Repository != "https://github.com/example/repo" {
		t.Errorf("expected Repository https://github.com/example/repo, got %s", comp.Repository)
	}
}

func TestBuildComponentDataList_MixedTypes(t *testing.T) {
	g := NewGenerator()

	input := &GeneratorInput{
		RecipeResult: &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{
				{
					Name:      "cert-manager",
					Namespace: "cert-manager",
					Chart:     "cert-manager",
					Type:      recipe.ComponentTypeHelm,
					Version:   "v1.17.2",
					Source:    "https://charts.jetstack.io",
				},
				{
					Name:      "my-kustomize-app",
					Namespace: "my-app",
					Type:      recipe.ComponentTypeKustomize,
					Source:    "https://github.com/example/repo",
					Tag:       "v2.0.0",
					Path:      "deploy/production",
				},
			},
		},
	}

	components, err := g.buildComponentDataList(input)
	if err != nil {
		t.Fatalf("buildComponentDataList failed: %v", err)
	}

	if len(components) != 2 {
		t.Fatalf("expected 2 components, got %d", len(components))
	}

	for _, comp := range components {
		switch comp.Name {
		case "cert-manager":
			if comp.IsKustomize {
				t.Error("cert-manager should not be kustomize")
			}
			if !comp.HasChart {
				t.Error("cert-manager should have HasChart=true")
			}
		case "my-kustomize-app":
			if !comp.IsKustomize {
				t.Error("my-kustomize-app should be kustomize")
			}
			if comp.HasChart {
				t.Error("my-kustomize-app should have HasChart=false")
			}
		}
	}
}

// Helper functions

func createKustomizeRecipeResult() *recipe.RecipeResult {
	return &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Metadata: struct {
			Version            string                     `json:"version,omitempty" yaml:"version,omitempty"`
			AppliedOverlays    []string                   `json:"appliedOverlays,omitempty" yaml:"appliedOverlays,omitempty"`
			ExcludedOverlays   []recipe.ExcludedOverlay   `json:"excludedOverlays,omitempty" yaml:"excludedOverlays,omitempty"`
			ConstraintWarnings []recipe.ConstraintWarning `json:"constraintWarnings,omitempty" yaml:"constraintWarnings,omitempty"`
		}{
			Version: "v0.1.0",
		},
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:      "my-kustomize-app",
				Namespace: "my-app",
				Type:      recipe.ComponentTypeKustomize,
				Source:    "https://github.com/example/repo",
				Tag:       "v1.0.0",
				Path:      "deploy/production",
			},
		},
		DeploymentOrder: []string{"my-kustomize-app"},
	}
}

func createMixedRecipeResult() *recipe.RecipeResult {
	return &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Metadata: struct {
			Version            string                     `json:"version,omitempty" yaml:"version,omitempty"`
			AppliedOverlays    []string                   `json:"appliedOverlays,omitempty" yaml:"appliedOverlays,omitempty"`
			ExcludedOverlays   []recipe.ExcludedOverlay   `json:"excludedOverlays,omitempty" yaml:"excludedOverlays,omitempty"`
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
				Type:      recipe.ComponentTypeHelm,
				Version:   "v1.17.2",
				Source:    "https://charts.jetstack.io",
			},
			{
				Name:      "my-kustomize-app",
				Namespace: "my-app",
				Type:      recipe.ComponentTypeKustomize,
				Source:    "https://github.com/example/repo",
				Tag:       "v1.0.0",
				Path:      "deploy/production",
			},
		},
		DeploymentOrder: []string{"cert-manager", "my-kustomize-app"},
	}
}

func createTestRecipeResult() *recipe.RecipeResult {
	return &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Metadata: struct {
			Version            string                     `json:"version,omitempty" yaml:"version,omitempty"`
			AppliedOverlays    []string                   `json:"appliedOverlays,omitempty" yaml:"appliedOverlays,omitempty"`
			ExcludedOverlays   []recipe.ExcludedOverlay   `json:"excludedOverlays,omitempty" yaml:"excludedOverlays,omitempty"`
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
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Metadata: struct {
			Version            string                     `json:"version,omitempty" yaml:"version,omitempty"`
			AppliedOverlays    []string                   `json:"appliedOverlays,omitempty" yaml:"appliedOverlays,omitempty"`
			ExcludedOverlays   []recipe.ExcludedOverlay   `json:"excludedOverlays,omitempty" yaml:"excludedOverlays,omitempty"`
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
				"crds": map[string]any{"enabled": true},
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

func TestGenerateDeployScript(t *testing.T) {
	tests := []struct {
		name       string
		cancelCtx  bool
		outputDir  string
		components []ComponentData
		wantErr    bool
	}{
		{
			name:      "success",
			outputDir: "", // filled per-test with t.TempDir()
			components: []ComponentData{
				{Name: "cert-manager", Namespace: "cert-manager", Repository: "https://charts.jetstack.io", ChartName: "cert-manager", Version: "v1.17.2", ChartVersion: "1.17.2"},
				{Name: "gpu-operator", Namespace: "gpu-operator", Repository: "https://helm.ngc.nvidia.com/nvidia", ChartName: "gpu-operator", Version: "v25.3.3", ChartVersion: "25.3.3"},
			},
		},
		{
			name:      "cancelled context",
			cancelCtx: true,
			outputDir: "", // filled per-test
			components: []ComponentData{
				{Name: "cert-manager"},
			},
			wantErr: true,
		},
		{
			name:      "invalid output directory",
			outputDir: "/nonexistent/path/that/does/not/exist",
			components: []ComponentData{
				{Name: "cert-manager"},
			},
			wantErr: true,
		},
		{
			name:       "empty components",
			outputDir:  "",
			components: []ComponentData{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGenerator()
			ctx := context.Background()
			if tt.cancelCtx {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			dir := tt.outputDir
			if dir == "" {
				dir = t.TempDir()
			}

			input := &GeneratorInput{
				Version: "v1.0.0",
			}

			path, size, err := g.generateDeployScript(ctx, input, tt.components, dir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			if path == "" {
				t.Fatal("expected non-empty path")
			}
			if size <= 0 {
				t.Fatal("expected positive file size")
			}

			info, statErr := os.Stat(path)
			if statErr != nil {
				t.Fatalf("stat(%s): %v", path, statErr)
			}
			if info.Mode()&0111 == 0 {
				t.Errorf("deploy.sh not executable, mode: %o", info.Mode())
			}
		})
	}
}

func TestGenerateDeployScript_EmptyVersionOmitsFlag(t *testing.T) {
	g := NewGenerator()
	ctx := context.Background()
	dir := t.TempDir()

	components := []ComponentData{
		{
			Name:       "gpu-operator",
			Namespace:  "gpu-operator",
			Repository: "https://helm.ngc.nvidia.com/nvidia",
			ChartName:  "gpu-operator",
			Version:    "", // empty version — should not produce --version flag
			HasChart:   true,
		},
	}

	input := &GeneratorInput{Version: "v1.0.0"}
	path, _, err := g.generateDeployScript(ctx, input, components, dir)
	if err != nil {
		t.Fatalf("generateDeployScript failed: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading deploy.sh: %v", err)
	}

	script := string(content)
	if strings.Contains(script, "--version") {
		t.Errorf("deploy.sh should not contain --version when Version is empty, got:\n%s", script)
	}
	if !strings.Contains(script, "helm upgrade --install gpu-operator gpu-operator") {
		t.Errorf("deploy.sh should contain helm install command for gpu-operator")
	}
}

func TestGenerateDeployScript_WithVersionIncludesFlag(t *testing.T) {
	g := NewGenerator()
	ctx := context.Background()
	dir := t.TempDir()

	components := []ComponentData{
		{
			Name:       "cert-manager",
			Namespace:  "cert-manager",
			Repository: "https://charts.jetstack.io",
			ChartName:  "cert-manager",
			Version:    "v1.17.2",
			HasChart:   true,
		},
	}

	input := &GeneratorInput{Version: "v1.0.0"}
	path, _, err := g.generateDeployScript(ctx, input, components, dir)
	if err != nil {
		t.Fatalf("generateDeployScript failed: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading deploy.sh: %v", err)
	}

	script := string(content)
	if !strings.Contains(script, "--version v1.17.2") {
		t.Errorf("deploy.sh should contain --version v1.17.2, got:\n%s", script)
	}
}

func TestGenerateUndeployScript(t *testing.T) {
	tests := []struct {
		name       string
		cancelCtx  bool
		outputDir  string
		components []ComponentData
		wantErr    bool
	}{
		{
			name:      "success",
			outputDir: "",
			components: []ComponentData{
				{Name: "cert-manager", Namespace: "cert-manager", Repository: "https://charts.jetstack.io", ChartName: "cert-manager", Version: "v1.17.2", ChartVersion: "1.17.2"},
				{Name: "gpu-operator", Namespace: "gpu-operator", Repository: "https://helm.ngc.nvidia.com/nvidia", ChartName: "gpu-operator", Version: "v25.3.3", ChartVersion: "25.3.3"},
			},
		},
		{
			name:      "cancelled context",
			cancelCtx: true,
			outputDir: "",
			components: []ComponentData{
				{Name: "cert-manager"},
			},
			wantErr: true,
		},
		{
			name:      "invalid output directory",
			outputDir: "/nonexistent/path/that/does/not/exist",
			components: []ComponentData{
				{Name: "cert-manager"},
			},
			wantErr: true,
		},
		{
			name:       "empty components",
			outputDir:  "",
			components: []ComponentData{},
		},
		{
			name:      "reverses component order",
			outputDir: "",
			components: []ComponentData{
				{Name: "alpha", Namespace: "alpha", ChartName: "alpha", Version: "v1.0.0", ChartVersion: "1.0.0", HasChart: true},
				{Name: "beta", Namespace: "beta", ChartName: "beta", Version: "v2.0.0", ChartVersion: "2.0.0", HasChart: true},
				{Name: "gamma", Namespace: "gamma", ChartName: "gamma", Version: "v3.0.0", ChartVersion: "3.0.0", HasChart: true},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGenerator()
			ctx := context.Background()
			if tt.cancelCtx {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			dir := tt.outputDir
			if dir == "" {
				dir = t.TempDir()
			}

			input := &GeneratorInput{
				Version: "v1.0.0",
			}

			path, size, err := g.generateUndeployScript(ctx, input, tt.components, dir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}

			if path == "" {
				t.Fatal("expected non-empty path")
			}
			if size <= 0 {
				t.Fatal("expected positive file size")
			}

			info, statErr := os.Stat(path)
			if statErr != nil {
				t.Fatalf("stat(%s): %v", path, statErr)
			}
			if info.Mode()&0111 == 0 {
				t.Errorf("undeploy.sh not executable, mode: %o", info.Mode())
			}

			if tt.name == "reverses component order" {
				content, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Fatalf("read undeploy.sh: %v", readErr)
				}
				script := string(content)
				gammaIdx := strings.Index(script, "Uninstalling gamma")
				alphaIdx := strings.Index(script, "Uninstalling alpha")
				if gammaIdx < 0 || alphaIdx < 0 {
					t.Fatal("expected both gamma and alpha in undeploy.sh")
				}
				if gammaIdx > alphaIdx {
					t.Error("undeploy.sh should have gamma before alpha (reverse order)")
				}
			}
		})
	}
}

func TestReverseComponents(t *testing.T) {
	tests := []struct {
		name     string
		input    []ComponentData
		wantLen  int
		wantName string // expected first element name after reverse
	}{
		{
			name:    "empty",
			input:   []ComponentData{},
			wantLen: 0,
		},
		{
			name:     "single",
			input:    []ComponentData{{Name: "a"}},
			wantLen:  1,
			wantName: "a",
		},
		{
			name: "multiple",
			input: []ComponentData{
				{Name: "a"},
				{Name: "b"},
				{Name: "c"},
			},
			wantLen:  3,
			wantName: "c",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Keep a copy of original order to verify non-mutation
			original := make([]ComponentData, len(tt.input))
			copy(original, tt.input)

			result := reverseComponents(tt.input)

			if len(result) != tt.wantLen {
				t.Fatalf("len = %d, want %d", len(result), tt.wantLen)
			}
			if tt.wantLen > 0 && result[0].Name != tt.wantName {
				t.Errorf("first element = %q, want %q", result[0].Name, tt.wantName)
			}
			// Verify original is unchanged
			for i, comp := range tt.input {
				if comp.Name != original[i].Name {
					t.Errorf("original[%d] mutated: got %q, want %q", i, comp.Name, original[i].Name)
				}
			}
		})
	}
}
