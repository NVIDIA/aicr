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

package helm

import (
	"bytes"
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// update regenerates goldens under testdata/ when set via `go test -update`.
var update = flag.Bool("update", false, "update golden files")

// testDriverVersion is a test constant for driver version strings to satisfy goconst.
const testDriverVersion = "570.86.16"

// ---------------------------------------------------------------------------
// Smoke / basic Generate tests
// ---------------------------------------------------------------------------

func TestGenerate_NilRecipeResult(t *testing.T) {
	ctx := context.Background()

	g := &Generator{
		RecipeResult: nil,
	}

	_, err := g.Generate(ctx, t.TempDir())
	if err == nil {
		t.Error("expected error for nil recipe result")
	}
}

func TestGenerate_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling Generate

	g := &Generator{
		RecipeResult:    createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{},
		Version:         "v1.0.0",
	}

	_, err := g.Generate(ctx, t.TempDir())
	if err == nil {
		t.Error("expected error for cancelled context")
	}
}

func TestGenerate_WithChecksums(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	g := &Generator{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {"crds": map[string]any{"enabled": true}},
			"gpu-operator": {"enabled": true},
		},
		Version:          "v1.0.0",
		IncludeChecksums: true,
	}

	output, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	checksumPath := filepath.Join(outputDir, "checksums.txt")
	if _, statErr := os.Stat(checksumPath); os.IsNotExist(statErr) {
		t.Error("checksums.txt does not exist")
	}

	content, err := os.ReadFile(checksumPath)
	if err != nil {
		t.Fatalf("failed to read checksums.txt: %v", err)
	}
	str := string(content)

	for _, want := range []string{
		"README.md",
		"deploy.sh",
		"undeploy.sh",
		filepath.Join("001-cert-manager", "values.yaml"),
	} {
		if !strings.Contains(str, want) {
			t.Errorf("checksums.txt missing %s", want)
		}
	}

	// Each line should carry a 64-char SHA256 hash.
	for _, line := range strings.Split(strings.TrimSpace(str), "\n") {
		parts := strings.Split(line, "  ")
		if len(parts) != 2 {
			t.Errorf("invalid checksum format: %s", line)
			continue
		}
		if len(parts[0]) != 64 {
			t.Errorf("expected 64 char hash, got %d: %s", len(parts[0]), parts[0])
		}
	}

	// checksums.txt is appended last.
	lastFile := output.Files[len(output.Files)-1]
	if !strings.HasSuffix(lastFile, "checksums.txt") {
		t.Errorf("expected last file to be checksums.txt, got %s", lastFile)
	}
}

// ---------------------------------------------------------------------------
// Deploy-script behavior tests
// ---------------------------------------------------------------------------

func TestGenerate_DeployScriptExecutable(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	g := &Generator{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {},
			"gpu-operator": {},
		},
		Version: "v1.0.0",
	}

	_, err := g.Generate(ctx, outputDir)
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	deployPath := filepath.Join(outputDir, "deploy.sh")
	info, statErr := os.Stat(deployPath)
	if os.IsNotExist(statErr) {
		t.Fatal("deploy.sh does not exist")
	}
	if info.Mode()&0111 == 0 {
		t.Errorf("deploy.sh is not executable, mode: %o", info.Mode())
	}

	content, err := os.ReadFile(deployPath)
	if err != nil {
		t.Fatalf("failed to read deploy.sh: %v", err)
	}
	script := string(content)
	if !strings.HasPrefix(script, "#!/usr/bin/env bash") {
		t.Error("deploy.sh missing shebang")
	}
	// Assertions on the orchestration script's structural markers. The inner
	// per-component `helm upgrade --install` now lives in each folder's
	// install.sh (rendered by localformat), so the structural markers here are
	// the generic install loop, retry helpers, and flag handling.
	for _, want := range []string{
		"set -euo pipefail",
		"MAX_RETRIES=5",
		"backoff_seconds()",
		"cleanup_helm_hooks()",
		"HELM_TIMEOUT=",
		"NO_WAIT=",
		"--retries",
		"ASYNC_COMPONENTS=", // async-skip policy lives here
		"bash install.sh",   // generic install loop invokes each folder's install.sh
	} {
		if !strings.Contains(script, want) {
			t.Errorf("deploy.sh missing %q", want)
		}
	}
}

// ---------------------------------------------------------------------------
// Undeploy-script behavior tests
// ---------------------------------------------------------------------------

func TestGenerate_UndeployScriptExecutable(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	g := &Generator{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {},
			"gpu-operator": {},
		},
		Version: "v1.0.0",
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	undeployPath := filepath.Join(outputDir, "undeploy.sh")
	info, statErr := os.Stat(undeployPath)
	if os.IsNotExist(statErr) {
		t.Fatal("undeploy.sh does not exist")
	}
	if info.Mode()&0111 == 0 {
		t.Errorf("undeploy.sh is not executable, mode: %o", info.Mode())
	}

	script := readFile(t, undeployPath)

	if !strings.HasPrefix(script, "#!/usr/bin/env bash") {
		t.Error("undeploy.sh missing shebang")
	}
	if !strings.Contains(script, "set -euo pipefail") {
		t.Error("undeploy.sh missing strict mode")
	}
	if !strings.Contains(script, "helm uninstall") {
		t.Error("undeploy.sh missing helm uninstall command")
	}

	// Verify reverse uninstall order: gpu-operator before cert-manager in the
	// rendered "Uninstalling…" report lines.
	gpuIdx := strings.Index(script, "gpu-operator")
	certIdx := strings.Index(script, "cert-manager")
	if gpuIdx < 0 || certIdx < 0 {
		t.Fatal("undeploy.sh missing component names")
	}
	if gpuIdx > certIdx {
		t.Error("undeploy.sh: gpu-operator should appear before cert-manager (reverse order)")
	}

	// --delete-pvcs flag defaults to off and is guarded.
	if !strings.Contains(script, "DELETE_PVCS=false") {
		t.Error("undeploy.sh missing DELETE_PVCS=false default")
	}
	if !strings.Contains(script, "--delete-pvcs") {
		t.Error("undeploy.sh missing --delete-pvcs flag handling")
	}
	if !strings.Contains(script, `"${DELETE_PVCS}" == "true"`) {
		t.Error("undeploy.sh PVC deletion not guarded by DELETE_PVCS flag")
	}

	// jq is a hard requirement for CRD/finalizer inspection.
	if strings.Contains(script, "HAS_JQ") {
		t.Error("undeploy.sh should not use HAS_JQ soft check; jq must be a hard requirement")
	}
	if !strings.Contains(script, "command -v jq") {
		t.Error("undeploy.sh missing jq availability check")
	}

	// Pre-flight exists and runs before component uninstall.
	preflightIdx := strings.Index(script, "Pre-flight checks")
	uninstallIdx := strings.Index(script, "Uninstall components in reverse install order")
	if preflightIdx < 0 {
		t.Fatal("undeploy.sh missing pre-flight checks section")
	}
	if uninstallIdx < 0 {
		t.Fatal("undeploy.sh missing component uninstall section")
	}
	if preflightIdx > uninstallIdx {
		t.Error("undeploy.sh pre-flight must run before component uninstall")
	}

	preflightSection := script[preflightIdx:uninstallIdx]
	if !strings.Contains(preflightSection, "check_release_for_stuck_crds") {
		t.Error("undeploy.sh pre-flight should call check_release_for_stuck_crds")
	}
	if !strings.Contains(preflightSection, "PREFLIGHT_DETAILS") || !strings.Contains(preflightSection, "exit 1") {
		t.Error("undeploy.sh pre-flight should detect stuck CRs and exit on failure")
	}
	if !strings.Contains(preflightSection, `check_release_for_stuck_crds "gpu-operator" "gpu-operator"`) {
		t.Error("undeploy.sh pre-flight missing check for gpu-operator with namespace")
	}
	if !strings.Contains(preflightSection, `check_release_for_stuck_crds "cert-manager" "cert-manager"`) {
		t.Error("undeploy.sh pre-flight missing check for cert-manager with namespace")
	}

	// Helper functions and Helm manifest discovery.
	for _, want := range []string{
		"check_crd_for_stuck_resources()",
		"check_release_for_stuck_crds()",
		"helm get manifest",
		"CRDs stuck in deleting state",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("undeploy.sh missing %q", want)
		}
	}

	// Stuck CRDs: script must NOT silently force-clear finalizers.
	if strings.Contains(script, "Force-clearing finalizers on stuck CRD") {
		t.Error("undeploy.sh should warn about stuck CRDs, not silently force-clear finalizers")
	}

	// API-group based destructive cleanup is forbidden; only ownership-safe paths.
	// deploy.sh may build ORPHANED_CRD_GROUPS for read-only pre-flight warnings,
	// but undeploy.sh must never use it for destructive deletion.
	if strings.Contains(script, "ORPHANED_CRD_GROUPS=") {
		t.Error("undeploy.sh should not build group-based CRD delete lists")
	}
	if strings.Contains(script, `grep "\.${group}$"`) {
		t.Error("undeploy.sh should not match CRDs by API group for destructive cleanup or post-flight stale warnings")
	}
}

// ---------------------------------------------------------------------------
// Property tests (helpers and data-shape preservation)
// ---------------------------------------------------------------------------

func TestUniqueNamespaces(t *testing.T) {
	tests := []struct {
		name       string
		components []ComponentData
		expected   []string
	}{
		{
			name: "deduplicates shared namespaces",
			components: []ComponentData{
				{Name: "prometheus-adapter", Namespace: "monitoring"},
				{Name: "k8s-ephemeral", Namespace: "monitoring"},
				{Name: "kube-prometheus", Namespace: "monitoring"},
				{Name: "gpu-operator", Namespace: "gpu-operator"},
			},
			expected: []string{"monitoring", "gpu-operator"},
		},
		{
			name: "preserves order",
			components: []ComponentData{
				{Name: "a", Namespace: "ns-a"},
				{Name: "b", Namespace: "ns-b"},
			},
			expected: []string{"ns-a", "ns-b"},
		},
		{
			name: "drops empty namespaces",
			components: []ComponentData{
				{Name: "no-ns", Namespace: ""},
				{Name: "with-ns", Namespace: "real"},
			},
			expected: []string{"real"},
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

func TestReverseComponents(t *testing.T) {
	tests := []struct {
		name     string
		input    []ComponentData
		wantLen  int
		wantName string
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
			original := make([]ComponentData, len(tt.input))
			copy(original, tt.input)

			result := reverseComponents(tt.input)

			if len(result) != tt.wantLen {
				t.Fatalf("len = %d, want %d", len(result), tt.wantLen)
			}
			if tt.wantLen > 0 && result[0].Name != tt.wantName {
				t.Errorf("first element = %q, want %q", result[0].Name, tt.wantName)
			}
			for i, comp := range tt.input {
				if comp.Name != original[i].Name {
					t.Errorf("original[%d] mutated: got %q, want %q", i, comp.Name, original[i].Name)
				}
			}
		})
	}
}

func TestBuildComponentDataListRejectsUnsafeNames(t *testing.T) {
	g := &Generator{
		RecipeResult: &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{
				{Name: "../etc/passwd", Version: "v1.0.0", Source: "https://evil.com"},
			},
		},
	}

	_, err := g.buildComponentDataList()
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

	g := &Generator{
		RecipeResult: &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{
				{Name: gpuOp, Namespace: gpuOp, Chart: gpuOp, Version: "v1.0.0", Source: "https://example.com"},
				{Name: certMgr, Namespace: certMgr, Chart: certMgr, Version: "v1.0.0", Source: "https://example.com"},
				{Name: unknown, Version: "v1.0.0", Source: "https://example.com"},
			},
		},
	}

	components, err := g.buildComponentDataList()
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

// TestNormalizeVersionWithDefault / TestSortComponentNamesByDeploymentOrder /
// TestIsSafePathComponent / TestSafeJoin live in pkg/bundler/deployer; not
// duplicated here.

// ---------------------------------------------------------------------------
// Determinism and no-timestamp
// ---------------------------------------------------------------------------

// TestGenerate_Reproducible verifies bundle generation is deterministic.
func TestGenerate_Reproducible(t *testing.T) {
	ctx := context.Background()

	g := &Generator{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {"crds": map[string]any{"enabled": true}},
			"gpu-operator": {
				"driver": map[string]any{"enabled": true},
			},
		},
		Version: "v1.0.0",
	}

	var fileContents [2]map[string]string

	for i := 0; i < 2; i++ {
		outputDir := t.TempDir()
		if _, err := g.Generate(ctx, outputDir); err != nil {
			t.Fatalf("iteration %d: Generate() error = %v", i, err)
		}

		fileContents[i] = make(map[string]string)
		err := filepath.Walk(outputDir, func(path string, info os.FileInfo, walkErr error) error {
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

	if len(fileContents[0]) != len(fileContents[1]) {
		t.Errorf("different number of files: iteration 1 has %d, iteration 2 has %d",
			len(fileContents[0]), len(fileContents[1]))
	}
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
}

// TestGenerate_NoTimestampInOutput verifies no timestamps are embedded.
func TestGenerate_NoTimestampInOutput(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	g := &Generator{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {},
			"gpu-operator": {},
		},
		Version: "v1.0.0",
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	timestampPatterns := []string{
		"GeneratedAt:",
		"generated_at:",
		"timestamp:",
		"Timestamp:",
	}

	err := filepath.Walk(outputDir, func(path string, info os.FileInfo, walkErr error) error {
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
		s := string(content)
		relPath, _ := filepath.Rel(outputDir, path)
		for _, pattern := range timestampPatterns {
			if strings.Contains(s, pattern) {
				t.Errorf("file %s contains timestamp pattern %q", relPath, pattern)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to walk directory: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Internal generators (generateDeployScript, generateUndeployScript)
// ---------------------------------------------------------------------------

// TestGenerateDeployScript_ContextCanceled exercises the early-return
// ctx.Err() check inside generateDeployScript. Generate() short-circuits at
// localformat.Write before reaching the helpers, so the helper's own ctx
// guard requires a direct call to cover.
func TestGenerateDeployScript_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := &Generator{Version: "v1.0.0"}
	if _, _, err := g.generateDeployScript(ctx, nil, t.TempDir()); err == nil {
		t.Fatal("expected error on canceled context")
	}
}

// TestGenerateUndeployScript_ContextCanceled — counterpart for undeploy.
func TestGenerateUndeployScript_ContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g := &Generator{Version: "v1.0.0"}
	if _, _, err := g.generateUndeployScript(ctx, nil, t.TempDir()); err == nil {
		t.Fatal("expected error on canceled context")
	}
}

// TestGenerate_InvalidOutputDir verifies Generate fails cleanly when the
// supplied outputDir cannot be created (parent directory does not exist
// and isn't writable). Other Generate-level error paths (nil RecipeResult,
// canceled context) are covered by their own focused tests.
func TestGenerate_InvalidOutputDir(t *testing.T) {
	g := &Generator{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {},
			"gpu-operator": {},
		},
		Version: "v1.0.0",
	}

	// /nonexistent/path/... requires creating /nonexistent/, which is not
	// writable by an unprivileged process.
	_, err := g.Generate(context.Background(), "/nonexistent/path/that/does/not/exist")
	if err == nil {
		t.Fatal("expected error on uncreatable output directory, got nil")
	}
}

// ---------------------------------------------------------------------------
// Dynamic values
// ---------------------------------------------------------------------------

func TestGenerate_DynamicValues(t *testing.T) {
	tests := []struct {
		name                string
		dynamicValues       map[string][]string
		componentValues     map[string]map[string]any
		wantClusterContains string // substring expected in gpu-operator/cluster-values.yaml
		wantValuesLacksPath string // substring that should NOT be in gpu-operator/values.yaml
	}{
		{
			name:          "no dynamic values — cluster-values.yaml still generated (empty)",
			dynamicValues: nil,
			componentValues: map[string]map[string]any{
				"cert-manager": {"crds": map[string]any{"enabled": true}},
				"gpu-operator": {"driver": map[string]any{"version": testDriverVersion, "enabled": true}},
			},
		},
		{
			name: "dynamic values present — extracted into cluster-values.yaml",
			dynamicValues: map[string][]string{
				"gpu-operator": {"driver.version"},
			},
			componentValues: map[string]map[string]any{
				"cert-manager": {"crds": map[string]any{"enabled": true}},
				"gpu-operator": {"driver": map[string]any{"version": testDriverVersion, "enabled": true}},
			},
			wantClusterContains: "version",
			wantValuesLacksPath: `version: "570.86.16"`,
		},
		{
			name: "dynamic path not in values",
			dynamicValues: map[string][]string{
				"gpu-operator": {"nonexistent.path"},
			},
			componentValues: map[string]map[string]any{
				"cert-manager": {"crds": map[string]any{"enabled": true}},
				"gpu-operator": {"driver": map[string]any{"enabled": true}},
			},
			wantClusterContains: "nonexistent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			outputDir := t.TempDir()

			g := &Generator{
				RecipeResult:    createTestRecipeResult(),
				ComponentValues: tt.componentValues,
				Version:         "v1.0.0",
				DynamicValues:   tt.dynamicValues,
			}
			if _, err := g.Generate(ctx, outputDir); err != nil {
				t.Fatalf("Generate failed: %v", err)
			}

			gpuCluster := filepath.Join(outputDir, "002-gpu-operator", "cluster-values.yaml")
			if _, err := os.Stat(gpuCluster); os.IsNotExist(err) {
				t.Fatal("gpu-operator/cluster-values.yaml should always exist")
			}
			if tt.wantClusterContains != "" {
				content := readFile(t, gpuCluster)
				if !strings.Contains(content, tt.wantClusterContains) {
					t.Errorf("cluster-values.yaml missing %q, got:\n%s", tt.wantClusterContains, content)
				}
			}
			if tt.wantValuesLacksPath != "" {
				content := readFile(t, filepath.Join(outputDir, "002-gpu-operator", "values.yaml"))
				if strings.Contains(content, tt.wantValuesLacksPath) {
					t.Errorf("values.yaml should not contain %q after dynamic split, got:\n%s", tt.wantValuesLacksPath, content)
				}
			}

			// cert-manager also always has cluster-values.yaml (every component gets one).
			if _, err := os.Stat(filepath.Join(outputDir, "001-cert-manager", "cluster-values.yaml")); os.IsNotExist(err) {
				t.Error("cert-manager should have cluster-values.yaml")
			}
		})
	}
}

func TestGenerate_DynamicValuesContentVerification(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	g := &Generator{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {"crds": map[string]any{"enabled": true}},
			"gpu-operator": {
				"driver": map[string]any{
					"version": testDriverVersion,
					"enabled": true,
				},
				"toolkit": map[string]any{
					"version": "1.17.4",
				},
			},
		},
		Version: "v1.0.0",
		DynamicValues: map[string][]string{
			"gpu-operator": {"driver.version", "toolkit.version"},
		},
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	cluster := readFile(t, filepath.Join(outputDir, "002-gpu-operator", "cluster-values.yaml"))
	if !strings.Contains(cluster, testDriverVersion) {
		t.Errorf("cluster-values.yaml missing driver.version, got:\n%s", cluster)
	}
	if !strings.Contains(cluster, "1.17.4") {
		t.Errorf("cluster-values.yaml missing toolkit.version, got:\n%s", cluster)
	}

	values := readFile(t, filepath.Join(outputDir, "002-gpu-operator", "values.yaml"))
	if strings.Contains(values, testDriverVersion) {
		t.Errorf("values.yaml should not contain driver.version, got:\n%s", values)
	}
	if strings.Contains(values, "1.17.4") {
		t.Errorf("values.yaml should not contain toolkit.version, got:\n%s", values)
	}
	if !strings.Contains(values, "enabled") {
		t.Errorf("values.yaml should still contain driver.enabled, got:\n%s", values)
	}
}

func TestSetNestedValue(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		value    any
		wantKeys []string
	}{
		{
			name:     "single level",
			path:     "version",
			value:    "1.0.0",
			wantKeys: []string{"version"},
		},
		{
			name:     "nested path",
			path:     "driver.version",
			value:    testDriverVersion,
			wantKeys: []string{"driver"},
		},
		{
			name:     "deeply nested",
			path:     "a.b.c",
			value:    "deep",
			wantKeys: []string{"a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := make(map[string]any)
			component.SetValueByPath(m, tt.path, tt.value)
			for _, key := range tt.wantKeys {
				if _, ok := m[key]; !ok {
					t.Errorf("missing key %q in result map", key)
				}
			}
		})
	}

	t.Run("verify nested structure", func(t *testing.T) {
		m := make(map[string]any)
		component.SetValueByPath(m, "driver.version", testDriverVersion)
		driver, ok := m["driver"].(map[string]any)
		if !ok {
			t.Fatal("driver should be a map")
		}
		if driver["version"] != testDriverVersion {
			t.Errorf("driver.version = %v, want 570.86.16", driver["version"])
		}
	})

	t.Run("multiple paths same parent", func(t *testing.T) {
		m := make(map[string]any)
		component.SetValueByPath(m, "driver.version", testDriverVersion)
		component.SetValueByPath(m, "driver.enabled", true)
		driver, ok := m["driver"].(map[string]any)
		if !ok {
			t.Fatal("driver should be a map")
		}
		if driver["version"] != testDriverVersion {
			t.Errorf("driver.version = %v, want 570.86.16", driver["version"])
		}
		if driver["enabled"] != true {
			t.Errorf("driver.enabled = %v, want true", driver["enabled"])
		}
	})
}

func TestGenerate_DynamicValuesDeeplyNested(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	g := &Generator{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {"crds": map[string]any{"enabled": true}},
			"gpu-operator": {
				"a": map[string]any{
					"b": map[string]any{
						"c": map[string]any{
							"d": "deep-value",
						},
					},
				},
				"driver": map[string]any{"enabled": true},
			},
		},
		Version: "v1.0.0",
		DynamicValues: map[string][]string{
			"gpu-operator": {"a.b.c.d"},
		},
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	cluster := readFile(t, filepath.Join(outputDir, "002-gpu-operator", "cluster-values.yaml"))
	if !strings.Contains(cluster, "deep-value") {
		t.Errorf("cluster-values.yaml missing deeply nested value, got:\n%s", cluster)
	}

	var clusterMap map[string]any
	if err := yaml.Unmarshal([]byte(cluster), &clusterMap); err != nil {
		t.Fatalf("failed to parse cluster-values.yaml: %v", err)
	}

	a, ok := clusterMap["a"].(map[string]any)
	if !ok {
		t.Fatal("expected 'a' to be a map in cluster-values.yaml")
	}
	b, ok := a["b"].(map[string]any)
	if !ok {
		t.Fatal("expected 'a.b' to be a map in cluster-values.yaml")
	}
	c, ok := b["c"].(map[string]any)
	if !ok {
		t.Fatal("expected 'a.b.c' to be a map in cluster-values.yaml")
	}
	d, ok := c["d"]
	if !ok {
		t.Fatal("expected 'a.b.c.d' to exist in cluster-values.yaml")
	}
	if d != "deep-value" {
		t.Errorf("a.b.c.d = %v, want 'deep-value'", d)
	}

	values := readFile(t, filepath.Join(outputDir, "002-gpu-operator", "values.yaml"))
	if strings.Contains(values, "deep-value") {
		t.Errorf("values.yaml should not contain deep-value after split, got:\n%s", values)
	}
	if !strings.Contains(values, "enabled") {
		t.Errorf("values.yaml should still contain driver.enabled, got:\n%s", values)
	}
}

func TestGenerate_DynamicValuesWithSetOverride(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	g := &Generator{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {"crds": map[string]any{"enabled": true}},
			"gpu-operator": {
				"driver": map[string]any{
					"version": "999.99.99",
					"enabled": true,
				},
			},
		},
		Version: "v1.0.0",
		DynamicValues: map[string][]string{
			"gpu-operator": {"driver.version"},
		},
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	cluster := readFile(t, filepath.Join(outputDir, "002-gpu-operator", "cluster-values.yaml"))
	if !strings.Contains(cluster, "999.99.99") {
		t.Errorf("cluster-values.yaml should contain --set override 999.99.99, got:\n%s", cluster)
	}

	values := readFile(t, filepath.Join(outputDir, "002-gpu-operator", "values.yaml"))
	if strings.Contains(values, "999.99.99") {
		t.Errorf("values.yaml should not contain 999.99.99 after dynamic split, got:\n%s", values)
	}
	if !strings.Contains(values, "enabled") {
		t.Errorf("values.yaml should still contain driver.enabled, got:\n%s", values)
	}
}

func TestGenerate_DynamicValuesRoundTrip(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	g := &Generator{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {"crds": map[string]any{"enabled": true}},
			"gpu-operator": {
				"driver":  map[string]any{"version": testDriverVersion, "enabled": true},
				"toolkit": map[string]any{"version": "1.17.4", "enabled": true},
				"gds":     map[string]any{"enabled": false},
			},
		},
		Version: "v1.0.0",
		DynamicValues: map[string][]string{
			"gpu-operator": {"driver.version", "toolkit.version"},
		},
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}

	var staticValues map[string]any
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(outputDir, "002-gpu-operator", "values.yaml"))), &staticValues); err != nil {
		t.Fatalf("failed to parse values.yaml: %v", err)
	}

	var dynamicValues map[string]any
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(outputDir, "002-gpu-operator", "cluster-values.yaml"))), &dynamicValues); err != nil {
		t.Fatalf("failed to parse cluster-values.yaml: %v", err)
	}

	// Merge simulates `helm install -f values.yaml -f cluster-values.yaml`.
	merged := deepMerge(staticValues, dynamicValues)

	driverMerged, ok := merged["driver"].(map[string]any)
	if !ok {
		t.Fatal("merged result missing 'driver' map")
	}
	if driverMerged["version"] != testDriverVersion {
		t.Errorf("merged driver.version = %v, want 570.86.16", driverMerged["version"])
	}
	if driverMerged["enabled"] != true {
		t.Errorf("merged driver.enabled = %v, want true", driverMerged["enabled"])
	}

	toolkitMerged, ok := merged["toolkit"].(map[string]any)
	if !ok {
		t.Fatal("merged result missing 'toolkit' map")
	}
	if toolkitMerged["version"] != "1.17.4" {
		t.Errorf("merged toolkit.version = %v, want 1.17.4", toolkitMerged["version"])
	}
	if toolkitMerged["enabled"] != true {
		t.Errorf("merged toolkit.enabled = %v, want true", toolkitMerged["enabled"])
	}

	gdsMerged, ok := merged["gds"].(map[string]any)
	if !ok {
		t.Fatal("merged result missing 'gds' map")
	}
	if gdsMerged["enabled"] != false {
		t.Errorf("merged gds.enabled = %v, want false", gdsMerged["enabled"])
	}
}

// deepMerge recursively merges src into dst. src values take precedence.
func deepMerge(dst, src map[string]any) map[string]any {
	result := make(map[string]any)
	for k, v := range dst {
		result[k] = v
	}
	for k, v := range src {
		if srcMap, ok := v.(map[string]any); ok {
			if dstMap, ok := result[k].(map[string]any); ok {
				result[k] = deepMerge(dstMap, srcMap)
				continue
			}
		}
		result[k] = v
	}
	return result
}

// TestGenerate_DoesNotMutateComponentValues verifies Generate does not
// mutate the caller's ComponentValues map.
func TestGenerate_DoesNotMutateComponentValues(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	originalValues := map[string]map[string]any{
		"gpu-operator": {
			"driver": map[string]any{"version": testDriverVersion, "enabled": true},
		},
	}

	g := &Generator{
		RecipeResult:    createTestRecipeResult(),
		ComponentValues: originalValues,
		Version:         "test",
		DynamicValues: map[string][]string{
			"gpu-operator": {"driver.version"},
		},
	}

	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	driver, ok := originalValues["gpu-operator"]["driver"].(map[string]any)
	if !ok {
		t.Fatal("original driver should still be a map")
	}
	if _, hasVersion := driver["version"]; !hasVersion {
		t.Error("original driver.version was mutated (removed) — deep copy is missing")
	}
}

// TestGenerate_DataFiles verifies external data files are included in
// checksums output and path traversal is rejected.
func TestGenerate_DataFiles(t *testing.T) {
	t.Run("valid data file included in output", func(t *testing.T) {
		ctx := context.Background()
		outputDir := t.TempDir()

		// Create a data file on disk so checksum generation can read it.
		dataDir := filepath.Join(outputDir, "data")
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dataDir, "overrides.yaml"), []byte("key: value"), 0o600); err != nil {
			t.Fatal(err)
		}

		g := &Generator{
			RecipeResult: createTestRecipeResult(),
			ComponentValues: map[string]map[string]any{
				"cert-manager": {},
				"gpu-operator": {},
			},
			Version:   "v1.0.0",
			DataFiles: []string{"data/overrides.yaml"},
		}

		output, err := g.Generate(ctx, outputDir)
		if err != nil {
			t.Fatalf("Generate() error = %v", err)
		}
		found := false
		for _, f := range output.Files {
			if strings.HasSuffix(f, "data/overrides.yaml") {
				found = true
				break
			}
		}
		if !found {
			t.Error("data file not included in output.Files")
		}
	})

	t.Run("path traversal rejected", func(t *testing.T) {
		ctx := context.Background()
		outputDir := t.TempDir()

		g := &Generator{
			RecipeResult: createTestRecipeResult(),
			ComponentValues: map[string]map[string]any{
				"cert-manager": {},
				"gpu-operator": {},
			},
			Version:   "v1.0.0",
			DataFiles: []string{"../../../etc/passwd"},
		}

		_, err := g.Generate(ctx, outputDir)
		if err == nil {
			t.Fatal("Generate() should reject path traversal in DataFiles")
		}
		if !strings.Contains(err.Error(), "escapes base directory") {
			t.Errorf("expected path-escape error from SafeJoin, got: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// Shell-behavior tests — preserved from the previous helm deployer
// ---------------------------------------------------------------------------

// TestUndeployScript_TransientFailureWarnsAndContinues covers the three
// post-uninstall cleanup pipelines in undeploy.sh that must tolerate a
// transient kubectl failure instead of letting `set -euo pipefail` kill the
// script.
func TestUndeployScript_TransientFailureWarnsAndContinues(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-behavior test")
	}
	if _, err := exec.LookPath("awk"); err != nil {
		t.Skip("awk not available; skipping shell-behavior test")
	}
	if _, err := exec.LookPath("sed"); err != nil {
		t.Skip("sed not available; skipping shell-behavior test")
	}

	ctx := context.Background()
	outputDir := t.TempDir()

	g := &Generator{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {},
			"gpu-operator": {},
		},
		Version: "v1.0.0",
	}
	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	undeployPath := filepath.Join(outputDir, "undeploy.sh")

	stubDir := t.TempDir()
	stubKubectl := filepath.Join(stubDir, "kubectl")
	stubScript := "#!/bin/sh\n" +
		"if [ \"$1\" = \"api-resources\" ]; then\n" +
		"  echo configmaps\n" +
		"  exit 0\n" +
		"fi\n" +
		"echo 'simulated transient API failure' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(stubKubectl, []byte(stubScript), 0o755); err != nil {
		t.Fatalf("write kubectl stub: %v", err)
	}

	tests := []struct {
		name        string
		bashSnippet string
		wantStderr  string
	}{
		{
			name: "delete_release_cluster_resources",
			bashSnippet: `
                snippet=$(sed -n '/^delete_release_cluster_resources()/,/^}/p' "$UNDEPLOY")
                eval "$snippet"
                HELM_TIMEOUT=10
                delete_release_cluster_resources "gpu-operator" "gpu-operator"
            `,
			wantStderr: "Warning: customresourcedefinitions cleanup pipeline for release gpu-operator/gpu-operator failed",
		},
		{
			name: "force_clear_namespace_finalizers",
			bashSnippet: `
                snippet=$(sed -n '/^force_clear_namespace_finalizers()/,/^}/p' "$UNDEPLOY")
                eval "$snippet"
                force_clear_namespace_finalizers "gpu-operator"
            `,
			wantStderr: "Warning: finalizer-clear pipeline for",
		},
		{
			name: "orphan_crd_inline_loop",
			bashSnippet: `
                snippet=$(sed -n '/^# Clean up orphaned CRDs that were owned by this bundle/,/^# Intentionally skip automatic deletion of unannotated CRDs matched only by/p' "$UNDEPLOY" | sed '$d')
                eval "$snippet"
            `,
			wantStderr: "Warning: orphan-CRD cleanup for",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(subCtx, "bash", "-c", "set -euo pipefail\n"+tt.bashSnippet)
			cmd.Env = append(os.Environ(),
				"PATH="+stubDir+":"+os.Getenv("PATH"),
				"UNDEPLOY="+undeployPath,
			)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			if err != nil {
				t.Fatalf("regression: cleanup pipeline killed the script with set -e instead of warning.\nerr: %v\nstdout: %s\nstderr: %s",
					err, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("expected %q in stderr, got:\nstderr: %s",
					tt.wantStderr, stderr.String())
			}
		})
	}
}

// TestUndeployScript_PreflightDiscoversExplicitExtraCRDs proves pre-flight
// catches a small set of known operator CRDs without scanning whole API
// groups.
func TestUndeployScript_PreflightDiscoversExplicitExtraCRDs(t *testing.T) {
	skipIfMissingBins(t, "bash", "awk", "sed", "jq")

	ctx := context.Background()
	outputDir := t.TempDir()
	g := &Generator{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {},
			"gpu-operator": {},
		},
		Version: "v1.0.0",
	}
	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	undeployPath := filepath.Join(outputDir, "undeploy.sh")

	stubDir := t.TempDir()
	writeStub(t, stubDir, "helm", "#!/bin/sh\nexit 0\n")
	writeStub(t, stubDir, "kubectl", `#!/bin/sh
case "$*" in
  "get crd -o json")
    echo '{"items":[{"metadata":{"name":"clusterpolicies.nvidia.com"},"spec":{"group":"nvidia.com","names":{"plural":"clusterpolicies"},"scope":"Cluster"}}]}'
    ;;
  "get crd clusterpolicies.nvidia.com -o json")
    echo '{"spec":{"names":{"plural":"clusterpolicies"},"group":"nvidia.com","scope":"Cluster"}}'
    ;;
  "get clusterpolicies.nvidia.com -o json")
    echo '{"items":[{"metadata":{"name":"cluster-policy","finalizers":["nvidia.com/clusterpolicy"]}}]}'
    ;;
  *)
    exit 0
    ;;
esac
`)

	stdout, stderr := runPreflightSnippet(t, ctx, stubDir, undeployPath,
		`check_release_for_stuck_crds "gpu-operator" "gpu-operator"`)

	for _, want := range []string{"cluster-policy", "clusterpolicies.nvidia.com", "nvidia.com/clusterpolicy"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("expected pre-flight to surface %q; stdout=%q stderr=%q", want, stdout, stderr)
		}
	}
}

// TestUndeployScript_PreflightDiscoversPrometheusExplicitCRDs covers
// kube-prometheus-stack CRDs installed outside Helm manifest/annotation
// discovery.
func TestUndeployScript_PreflightDiscoversPrometheusExplicitCRDs(t *testing.T) {
	skipIfMissingBins(t, "bash", "awk", "sed", "jq")

	ctx := context.Background()
	outputDir := t.TempDir()
	g := &Generator{
		RecipeResult: singleComponentRecipe("kube-prometheus-stack", "monitoring",
			"prometheus-community/kube-prometheus-stack", "82.8.0",
			"https://prometheus-community.github.io/helm-charts"),
		ComponentValues: map[string]map[string]any{"kube-prometheus-stack": {}},
		Version:         "v1.0.0",
	}
	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	undeployPath := filepath.Join(outputDir, "undeploy.sh")

	stubDir := t.TempDir()
	writeStub(t, stubDir, "helm", "#!/bin/sh\nexit 0\n")
	writeStub(t, stubDir, "kubectl", `#!/bin/sh
case "$*" in
  "get crd -o json")
    echo '{"items":[{"metadata":{"name":"prometheuses.monitoring.coreos.com"},"spec":{"group":"monitoring.coreos.com","names":{"plural":"prometheuses"},"scope":"Namespaced"}}]}'
    ;;
  "get crd prometheuses.monitoring.coreos.com -o json")
    echo '{"spec":{"names":{"plural":"prometheuses"},"group":"monitoring.coreos.com","scope":"Namespaced"}}'
    ;;
  "get prometheuses.monitoring.coreos.com -A -o json")
    echo '{"items":[{"metadata":{"namespace":"monitoring","name":"aicr-prometheus","finalizers":["monitoring.coreos.com/operator"]}}]}'
    ;;
  *)
    exit 0
    ;;
esac
`)

	stdout, stderr := runPreflightSnippet(t, ctx, stubDir, undeployPath,
		`check_release_for_stuck_crds "kube-prometheus-stack" "monitoring"`)

	for _, want := range []string{"prometheuses.monitoring.coreos.com", "monitoring/aicr-prometheus", "monitoring.coreos.com/operator"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("expected pre-flight to surface %q; stdout=%q stderr=%q", want, stdout, stderr)
		}
	}
}

// TestUndeployScript_PreflightDiscoversAnnotatedCRDs proves the retained
// annotation-based discovery still catches release-owned CRDs when helm get
// manifest is empty.
func TestUndeployScript_PreflightDiscoversAnnotatedCRDs(t *testing.T) {
	skipIfMissingBins(t, "bash", "awk", "sed", "jq")

	ctx := context.Background()
	outputDir := t.TempDir()
	g := &Generator{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {},
			"gpu-operator": {},
		},
		Version: "v1.0.0",
	}
	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	undeployPath := filepath.Join(outputDir, "undeploy.sh")

	stubDir := t.TempDir()
	writeStub(t, stubDir, "helm", "#!/bin/sh\nexit 0\n")
	writeStub(t, stubDir, "kubectl", `#!/bin/sh
case "$*" in
  "get crd -o json")
    echo '{"items":[{"metadata":{"name":"challenges.acme.cert-manager.io","annotations":{"meta.helm.sh/release-name":"cert-manager","meta.helm.sh/release-namespace":"cert-manager"}},"spec":{"group":"acme.cert-manager.io","names":{"plural":"challenges"},"scope":"Namespaced"}}]}'
    ;;
  "get crd challenges.acme.cert-manager.io -o json")
    echo '{"spec":{"names":{"plural":"challenges"},"group":"acme.cert-manager.io","scope":"Namespaced"}}'
    ;;
  "get challenges.acme.cert-manager.io -A -o json")
    echo '{"items":[{"metadata":{"namespace":"cert-manager","name":"test-challenge","finalizers":["acme.cert-manager.io/finalizer"]}}]}'
    ;;
  *)
    exit 0
    ;;
esac
`)

	stdout, stderr := runPreflightSnippet(t, ctx, stubDir, undeployPath,
		`check_release_for_stuck_crds "cert-manager" "cert-manager"`)

	for _, want := range []string{"challenges.acme.cert-manager.io", "cert-manager/test-challenge", "acme.cert-manager.io/finalizer"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("expected pre-flight to surface %q; stdout=%q stderr=%q", want, stdout, stderr)
		}
	}
}

// TestUndeployScript_PreflightSkipListCoversManifestDeletedReleases keeps the
// explicit skip list for releases whose dependent CRs are deleted from
// manifests before controller uninstall.
func TestUndeployScript_PreflightSkipListCoversManifestDeletedReleases(t *testing.T) {
	skipIfMissingBins(t, "bash", "sed")

	ctx := context.Background()
	outputDir := t.TempDir()
	g := &Generator{
		RecipeResult: &recipe.RecipeResult{
			Kind:       "RecipeResult",
			APIVersion: "aicr.nvidia.com/v1alpha1",
			Metadata: struct {
				Version            string                     `json:"version,omitempty" yaml:"version,omitempty"`
				AppliedOverlays    []string                   `json:"appliedOverlays,omitempty" yaml:"appliedOverlays,omitempty"`
				ExcludedOverlays   []recipe.ExcludedOverlay   `json:"excludedOverlays,omitempty" yaml:"excludedOverlays,omitempty"`
				ConstraintWarnings []recipe.ConstraintWarning `json:"constraintWarnings,omitempty" yaml:"constraintWarnings,omitempty"`
			}{Version: "v0.1.0"},
			ComponentRefs: []recipe.ComponentRef{
				{Name: "cert-manager", Namespace: "cert-manager", Chart: "cert-manager", Version: "v1.17.2", Source: "https://charts.jetstack.io"},
				{Name: "kgateway", Namespace: "kgateway-system", Chart: "kgateway", Version: "v0.1.0", Source: "https://example.invalid/charts"},
				{Name: "nodewright-operator", Namespace: "skyhook", Chart: "nodewright-operator", Version: "v0.1.0", Source: "https://example.invalid/charts"},
			},
			DeploymentOrder: []string{"cert-manager", "kgateway", "nodewright-operator"},
		},
		ComponentValues: map[string]map[string]any{
			"cert-manager":        {},
			"kgateway":            {},
			"nodewright-operator": {},
		},
		Version: "v1.0.0",
	}
	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	undeployPath := filepath.Join(outputDir, "undeploy.sh")

	bashSnippet := `
        snippet=$(sed -n '/^skip_preflight_for_release()/,/^}/p' "$UNDEPLOY")
        eval "$snippet"
        skip_preflight_for_release "nodewright-operator" && echo "skip:nodewright-operator"
        skip_preflight_for_release "kgateway" && echo "skip:kgateway"
        if skip_preflight_for_release "cert-manager"; then
            echo "unexpected:cert-manager"
            exit 1
        fi
        echo "check:cert-manager"
    `

	subCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(subCtx, "bash", "-c", "set -euo pipefail\n"+bashSnippet)
	cmd.Env = append(os.Environ(), "UNDEPLOY="+undeployPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("script exited non-zero.\nerr: %v\nstdout: %s\nstderr: %s",
			err, stdout.String(), stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"skip:nodewright-operator", "skip:kgateway", "check:cert-manager"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output; stdout=%q stderr=%q", want, out, stderr.String())
		}
	}
}

// TestUndeployScript_PreflightSkipsForeignAnnotatedExtraCRDs preserves the
// shared-cluster safety property for explicit CRD overrides annotated to a
// different release.
func TestUndeployScript_PreflightSkipsForeignAnnotatedExtraCRDs(t *testing.T) {
	skipIfMissingBins(t, "bash", "awk", "sed", "jq")

	ctx := context.Background()
	outputDir := t.TempDir()
	g := &Generator{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {},
			"gpu-operator": {},
		},
		Version: "v1.0.0",
	}
	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	undeployPath := filepath.Join(outputDir, "undeploy.sh")

	stubDir := t.TempDir()
	writeStub(t, stubDir, "helm", "#!/bin/sh\nexit 0\n")
	writeStub(t, stubDir, "kubectl", `#!/bin/sh
case "$*" in
  "get crd -o json")
    echo '{"items":[{"metadata":{"name":"clusterpolicies.nvidia.com","annotations":{"meta.helm.sh/release-name":"other-release","meta.helm.sh/release-namespace":"other-ns"}},"spec":{"group":"nvidia.com","names":{"plural":"clusterpolicies"},"scope":"Cluster"}}]}'
    ;;
  "get crd clusterpolicies.nvidia.com -o json")
    echo '{"spec":{"names":{"plural":"clusterpolicies"},"group":"nvidia.com","scope":"Cluster"}}'
    ;;
  "get clusterpolicies.nvidia.com -o json")
    echo '{"items":[{"metadata":{"name":"foreign-policy","finalizers":["nvidia.com/clusterpolicy"]}}]}'
    ;;
  *)
    exit 0
    ;;
esac
`)

	stdout, stderr := runPreflightSnippet(t, ctx, stubDir, undeployPath,
		`check_release_for_stuck_crds "gpu-operator" "gpu-operator"`)

	leaked := strings.Contains(stdout, "clusterpolicies.nvidia.com") ||
		strings.Contains(stdout, "foreign-policy") ||
		strings.Contains(stdout, "nvidia.com/clusterpolicy")
	if leaked {
		t.Errorf("pre-flight scanned a CRD annotated to a different Helm release.\nstdout=%q stderr=%q", stdout, stderr)
	}
}

// TestUndeployScript_PreflightFailsClosedOnKubectlError asserts a transient
// `kubectl get crd` failure causes pre-flight to fail closed with a clear
// error message.
func TestUndeployScript_PreflightFailsClosedOnKubectlError(t *testing.T) {
	skipIfMissingBins(t, "bash", "awk", "sed", "jq")

	ctx := context.Background()
	outputDir := t.TempDir()
	g := &Generator{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {},
			"gpu-operator": {},
		},
		Version: "v1.0.0",
	}
	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	undeployPath := filepath.Join(outputDir, "undeploy.sh")

	stubDir := t.TempDir()
	writeStub(t, stubDir, "helm", "#!/bin/sh\nexit 0\n")
	writeStub(t, stubDir, "kubectl", `#!/bin/sh
case "$*" in
  "get crd -o json")
    echo "error: the server is currently unable to handle the request (get customresourcedefinitions.apiextensions.k8s.io)" >&2
    exit 1
    ;;
  *)
    exit 0
    ;;
esac
`)

	bashSnippet := `
        for fn in extra_crds_for_release capture_kubectl_json check_crd_for_stuck_resources check_release_for_stuck_crds; do
            snippet=$(sed -n "/^${fn}()/,/^}/p" "$UNDEPLOY")
            eval "$snippet"
        done
        PREFLIGHT_DETAILS=$(mktemp)
        check_release_for_stuck_crds "gpu-operator" "gpu-operator"
        echo "UNREACHABLE: fast-path silently passed despite API error" >&2
        rm -f "$PREFLIGHT_DETAILS"
    `

	subCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(subCtx, "bash", "-c", "set -euo pipefail\n"+bashSnippet)
	cmd.Env = append(os.Environ(),
		"PATH="+stubDir+":"+os.Getenv("PATH"),
		"UNDEPLOY="+undeployPath,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	if err == nil {
		t.Fatalf("regression: check_release_for_stuck_crds returned 0 despite kubectl error.\nstdout=%q stderr=%q",
			stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "UNREACHABLE") {
		t.Fatalf("regression: execution continued past check_release_for_stuck_crds.\nstderr=%q", stderr.String())
	}
	for _, want := range []string{
		"ERROR: Pre-flight could not list CRDs",
		"gpu-operator",
		"--skip-preflight",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("expected error text %q; stderr=%q", want, stderr.String())
		}
	}
}

// TestUndeployScript_PreflightPreservesJSONWhenKubectlWarnsOnStderr asserts
// that successful kubectl output remains parseable even when warnings are on
// stderr.
func TestUndeployScript_PreflightPreservesJSONWhenKubectlWarnsOnStderr(t *testing.T) {
	skipIfMissingBins(t, "bash", "awk", "sed", "jq")

	ctx := context.Background()
	outputDir := t.TempDir()
	g := &Generator{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {},
			"gpu-operator": {},
		},
		Version: "v1.0.0",
	}
	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	undeployPath := filepath.Join(outputDir, "undeploy.sh")

	stubDir := t.TempDir()
	writeStub(t, stubDir, "helm", "#!/bin/sh\nexit 0\n")
	writeStub(t, stubDir, "kubectl", `#!/bin/sh
case "$*" in
  "get crd -o json")
    echo "warning: cached discovery response" >&2
    echo '{"items":[{"metadata":{"name":"clusterpolicies.nvidia.com"},"spec":{"group":"nvidia.com","names":{"plural":"clusterpolicies"},"scope":"Cluster"}}]}'
    ;;
  "get crd clusterpolicies.nvidia.com -o json")
    echo "warning: cached CRD read" >&2
    echo '{"spec":{"names":{"plural":"clusterpolicies"},"group":"nvidia.com","scope":"Cluster"}}'
    ;;
  "get clusterpolicies.nvidia.com -o json")
    echo "warning: cached CR list" >&2
    echo '{"items":[{"metadata":{"name":"cluster-policy","finalizers":["nvidia.com/clusterpolicy"]}}]}'
    ;;
  *)
    exit 0
    ;;
esac
`)

	stdout, stderr := runPreflightSnippet(t, ctx, stubDir, undeployPath,
		`check_release_for_stuck_crds "gpu-operator" "gpu-operator"`)

	if !strings.Contains(stdout, "cluster-policy") {
		t.Errorf("expected pre-flight to keep parsing JSON despite kubectl warnings; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stderr, "warning: cached discovery response") {
		t.Errorf("expected kubectl stderr warning to remain visible; stderr=%q", stderr)
	}
}

// TestUndeployScript_PostflightWarnsOnExplicitExtraCRDs proves post-flight
// surfaces leftover exact-name CRDs from known installed releases.
func TestUndeployScript_PostflightWarnsOnExplicitExtraCRDs(t *testing.T) {
	skipIfMissingBins(t, "bash", "sed", "jq", "awk", "sort", "tr")

	ctx := context.Background()
	outputDir := t.TempDir()
	g := &Generator{
		RecipeResult: singleComponentRecipe("kube-prometheus-stack", "monitoring",
			"prometheus-community/kube-prometheus-stack", "82.8.0",
			"https://prometheus-community.github.io/helm-charts"),
		ComponentValues: map[string]map[string]any{"kube-prometheus-stack": {}},
		Version:         "v1.0.0",
	}
	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	undeployPath := filepath.Join(outputDir, "undeploy.sh")

	stubDir := t.TempDir()
	writeStub(t, stubDir, "kubectl", `#!/bin/sh
case "$*" in
  "get crd -o json")
    echo '{"items":[{"metadata":{"name":"prometheuses.monitoring.coreos.com"},"spec":{"group":"monitoring.coreos.com","names":{"plural":"prometheuses"},"scope":"Namespaced"}}]}'
    ;;
  *)
    exit 0
    ;;
esac
`)

	bashSnippet := `
        for fn in extra_crds_for_release capture_kubectl_json; do
            snippet=$(sed -n "/^${fn}()/,/^}/p" "$UNDEPLOY")
            eval "$snippet"
        done
        snippet=$(sed -n '/^# Check for Helm-annotated CRDs from uninstalled releases\./,/^if \[\[ "\${postflight_issues}" == "true" \]\]/p' "$UNDEPLOY" | sed '$d')
        helm_orphaned_crds=""
        explicit_orphaned_crds=""
        postflight_all_crds_json=""
        postflight_issues=false
        eval "$snippet"
        [[ "${postflight_issues}" == "true" ]]
    `

	subCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(subCtx, "bash", "-c", "set -euo pipefail\n"+bashSnippet)
	cmd.Env = append(os.Environ(),
		"PATH="+stubDir+":"+os.Getenv("PATH"),
		"UNDEPLOY="+undeployPath,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("script exited non-zero.\nerr: %v\nstdout: %s\nstderr: %s",
			err, stdout.String(), stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{
		"WARNING: explicit CRDs from this bundle still present:",
		"prometheuses.monitoring.coreos.com",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in post-flight output; stdout=%q stderr=%q", want, out, stderr.String())
		}
	}
}

// TestUndeployScript_DynamoPlatformOwnsExplicitGroveCRDs verifies the
// dynamo-platform release owns the Grove CRDs via extra_crds_for_release.
func TestUndeployScript_DynamoPlatformOwnsExplicitGroveCRDs(t *testing.T) {
	skipIfMissingBins(t, "bash", "sed")

	ctx := context.Background()
	outputDir := t.TempDir()
	g := &Generator{
		RecipeResult: singleComponentRecipe("dynamo-platform", "dynamo-platform",
			"oci://example.com/dynamo-platform", "0.9.1",
			"oci://example.com"),
		ComponentValues: map[string]map[string]any{"dynamo-platform": {}},
		Version:         "v1.0.0",
	}
	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	undeployPath := filepath.Join(outputDir, "undeploy.sh")

	bashSnippet := `
        snippet=$(sed -n '/^extra_crds_for_release()/,/^}/p' "$UNDEPLOY")
        eval "$snippet"
        platform_crds=$(extra_crds_for_release "dynamo-platform")
        printf '%s\n' "$platform_crds"
        test -n "$platform_crds"
        test -z "$(extra_crds_for_release "grove")"
    `

	subCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(subCtx, "bash", "-c", "set -euo pipefail\n"+bashSnippet)
	cmd.Env = append(os.Environ(), "UNDEPLOY="+undeployPath)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("script exited non-zero.\nerr: %v\nstdout: %s\nstderr: %s",
			err, stdout.String(), stderr.String())
	}

	for _, crd := range []string{
		"podcliques.grove.io",
		"podcliquescalinggroups.grove.io",
		"podcliquesets.grove.io",
		"podgangs.scheduler.grove.io",
	} {
		if !strings.Contains(stdout.String(), crd) {
			t.Errorf("expected dynamo-platform explicit CRD list to include %s; stdout=%q stderr=%q",
				crd, stdout.String(), stderr.String())
		}
	}
}

// ---------------------------------------------------------------------------
// Golden-file bundle tests
// ---------------------------------------------------------------------------
//
// These tests assert full-tree equivalence of the rendered bundle against
// committed goldens under testdata/<scenario>/. Regenerate with:
//
//   go test ./pkg/bundler/deployer/helm/... -run TestBundleGolden -update
//
// The goldens double as reference examples of what a rendered bundle looks
// like for each common shape: upstream-helm-only, manifest-only, mixed
// (upstream + raw manifests → primary + -post folder), kai-scheduler (async
// block), and nodewright-operator (pre-install taint cleanup block).

func TestBundleGolden_UpstreamHelmOnly(t *testing.T) {
	outDir := t.TempDir()
	g := &Generator{
		RecipeResult: singleComponentRecipe(
			"cert-manager", "cert-manager", "cert-manager", "v1.17.2",
			"https://charts.jetstack.io"),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {"crds": map[string]any{"enabled": true}},
		},
		Version: "v1.0.0",
	}
	if _, err := g.Generate(context.Background(), outDir); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	assertBundleGolden(t, outDir, "testdata/upstream_helm_only")
}

func TestBundleGolden_ManifestOnly(t *testing.T) {
	outDir := t.TempDir()
	g := &Generator{
		RecipeResult: &recipe.RecipeResult{
			Kind:       "RecipeResult",
			APIVersion: "aicr.nvidia.com/v1alpha1",
			Metadata: struct {
				Version            string                     `json:"version,omitempty" yaml:"version,omitempty"`
				AppliedOverlays    []string                   `json:"appliedOverlays,omitempty" yaml:"appliedOverlays,omitempty"`
				ExcludedOverlays   []recipe.ExcludedOverlay   `json:"excludedOverlays,omitempty" yaml:"excludedOverlays,omitempty"`
				ConstraintWarnings []recipe.ConstraintWarning `json:"constraintWarnings,omitempty" yaml:"constraintWarnings,omitempty"`
			}{Version: "v0.1.0"},
			ComponentRefs: []recipe.ComponentRef{
				{Name: "skyhook-customizations", Namespace: "skyhook"},
			},
			DeploymentOrder: []string{"skyhook-customizations"},
		},
		ComponentValues: map[string]map[string]any{"skyhook-customizations": {}},
		ComponentManifests: map[string]map[string][]byte{
			"skyhook-customizations": {
				"components/skyhook-customizations/manifests/customization.yaml": []byte(`# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.

apiVersion: v1
kind: ConfigMap
metadata:
  name: customization
`),
			},
		},
		Version: "v1.0.0",
	}
	if _, err := g.Generate(context.Background(), outDir); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	assertBundleGolden(t, outDir, "testdata/manifest_only")
}

func TestBundleGolden_MixedGPUOperator(t *testing.T) {
	outDir := t.TempDir()
	g := &Generator{
		RecipeResult: singleComponentRecipe(
			"gpu-operator", "gpu-operator", "gpu-operator", "v25.3.3",
			"https://helm.ngc.nvidia.com/nvidia"),
		ComponentValues: map[string]map[string]any{
			"gpu-operator": {"driver": map[string]any{"enabled": true}},
		},
		ComponentManifests: map[string]map[string][]byte{
			"gpu-operator": {
				"components/gpu-operator/manifests/dcgm-exporter.yaml": []byte(`# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.

apiVersion: v1
kind: Service
metadata:
  name: dcgm-exporter
`),
			},
		},
		Version: "v1.0.0",
	}
	if _, err := g.Generate(context.Background(), outDir); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	assertBundleGolden(t, outDir, "testdata/mixed_gpu_operator")
}

func TestBundleGolden_KaiSchedulerPresent(t *testing.T) {
	outDir := t.TempDir()
	g := &Generator{
		RecipeResult: &recipe.RecipeResult{
			Kind:       "RecipeResult",
			APIVersion: "aicr.nvidia.com/v1alpha1",
			Metadata: struct {
				Version            string                     `json:"version,omitempty" yaml:"version,omitempty"`
				AppliedOverlays    []string                   `json:"appliedOverlays,omitempty" yaml:"appliedOverlays,omitempty"`
				ExcludedOverlays   []recipe.ExcludedOverlay   `json:"excludedOverlays,omitempty" yaml:"excludedOverlays,omitempty"`
				ConstraintWarnings []recipe.ConstraintWarning `json:"constraintWarnings,omitempty" yaml:"constraintWarnings,omitempty"`
			}{Version: "v0.1.0"},
			ComponentRefs: []recipe.ComponentRef{
				{
					Name:      "kai-scheduler",
					Namespace: "kai-scheduler",
					Chart:     "kai-scheduler",
					Version:   "v0.14.1",
					Source:    "oci://ghcr.io/kai-scheduler/kai-scheduler",
				},
			},
			DeploymentOrder: []string{"kai-scheduler"},
		},
		ComponentValues: map[string]map[string]any{"kai-scheduler": {}},
		Version:         "v1.0.0",
	}
	if _, err := g.Generate(context.Background(), outDir); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	assertBundleGolden(t, outDir, "testdata/kai_scheduler_present")
}

func TestBundleGolden_NodewrightPresent(t *testing.T) {
	outDir := t.TempDir()
	// Mirror the production registry: component name "nodewright-operator"
	// but the upstream chart is still named "skyhook-operator". This shape
	// is what real recipes have post-rename — the registry component name
	// drives the name-matched taint cleanup blocks; the chart name drives
	// helm install.
	g := &Generator{
		RecipeResult: singleComponentRecipe(
			"nodewright-operator", "skyhook", "skyhook-operator", "v0.1.0",
			"https://example.invalid/charts"),
		ComponentValues: map[string]map[string]any{"nodewright-operator": {}},
		Version:         "v1.0.0",
	}
	if _, err := g.Generate(context.Background(), outDir); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	assertBundleGolden(t, outDir, "testdata/nodewright_present")
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// readFile reads a file or fails the test with a clear message.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// skipIfMissingBins skips the test if any of the named binaries are missing.
func skipIfMissingBins(t *testing.T, bins ...string) {
	t.Helper()
	for _, b := range bins {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not available; skipping shell-behavior test", b)
		}
	}
}

// writeStub creates an executable stub at stubDir/name.
func writeStub(t *testing.T, stubDir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(stubDir, name), []byte(content), 0o755); err != nil {
		t.Fatalf("write %s stub: %v", name, err)
	}
}

// runPreflightSnippet sources the pre-flight helpers from undeployPath and
// runs the given snippet under bash with stubDir prepended to PATH. Returns
// captured stdout and stderr.
func runPreflightSnippet(t *testing.T, ctx context.Context, stubDir, undeployPath, call string) (string, string) {
	t.Helper()
	bashSnippet := `
        for fn in extra_crds_for_release capture_kubectl_json check_crd_for_stuck_resources check_release_for_stuck_crds; do
            snippet=$(sed -n "/^${fn}()/,/^}/p" "$UNDEPLOY")
            eval "$snippet"
        done
        PREFLIGHT_DETAILS=$(mktemp)
        ` + call + `
        cat "$PREFLIGHT_DETAILS"
        rm -f "$PREFLIGHT_DETAILS"
    `

	subCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(subCtx, "bash", "-c", "set -euo pipefail\n"+bashSnippet)
	cmd.Env = append(os.Environ(),
		"PATH="+stubDir+":"+os.Getenv("PATH"),
		"UNDEPLOY="+undeployPath,
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("script exited non-zero.\nerr: %v\nstdout: %s\nstderr: %s",
			err, stdout.String(), stderr.String())
	}
	return stdout.String(), stderr.String()
}

// singleComponentRecipe builds a RecipeResult with exactly one Helm component.
func singleComponentRecipe(name, namespace, chart, version, source string) *recipe.RecipeResult {
	return &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Metadata: struct {
			Version            string                     `json:"version,omitempty" yaml:"version,omitempty"`
			AppliedOverlays    []string                   `json:"appliedOverlays,omitempty" yaml:"appliedOverlays,omitempty"`
			ExcludedOverlays   []recipe.ExcludedOverlay   `json:"excludedOverlays,omitempty" yaml:"excludedOverlays,omitempty"`
			ConstraintWarnings []recipe.ConstraintWarning `json:"constraintWarnings,omitempty" yaml:"constraintWarnings,omitempty"`
		}{Version: "v0.1.0"},
		ComponentRefs: []recipe.ComponentRef{
			{Name: name, Namespace: namespace, Chart: chart, Version: version, Source: source},
		},
		DeploymentOrder: []string{name},
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
		}{Version: "v0.1.0"},
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

// assertBundleGolden verifies outDir matches the committed bundle at goldenDir.
// With -update, overwrites goldenDir with the tree in outDir. Verifies both
// directions: every golden file exists in outDir, and vice versa.
func assertBundleGolden(t *testing.T, outDir, goldenDir string) {
	t.Helper()
	actual := listBundleFiles(t, outDir)

	if *update {
		// Remove the prior golden tree so stale files don't linger. Keep the
		// directory so the writer logic below can mkdir subdirs below it.
		if err := os.RemoveAll(goldenDir); err != nil {
			t.Fatalf("remove golden dir: %v", err)
		}
		for _, rel := range actual {
			src := filepath.Join(outDir, rel)
			dst := filepath.Join(goldenDir, rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatalf("mkdir %s: %v", filepath.Dir(dst), err)
			}
			content, err := os.ReadFile(src)
			if err != nil {
				t.Fatalf("read actual %s: %v", src, err)
			}
			if err := os.WriteFile(dst, content, 0o644); err != nil {
				t.Fatalf("write golden %s: %v", dst, err)
			}
		}
		return
	}

	// Compare file lists.
	golden := listBundleFiles(t, goldenDir)
	if !reflect.DeepEqual(actual, golden) {
		t.Fatalf("bundle file tree differs from %s:\n  actual: %v\n  golden: %v\n(run with -update to regenerate)",
			goldenDir, actual, golden)
	}

	// Byte-compare each file.
	for _, rel := range actual {
		got, err := os.ReadFile(filepath.Join(outDir, rel))
		if err != nil {
			t.Fatalf("read actual %s: %v", rel, err)
		}
		want, err := os.ReadFile(filepath.Join(goldenDir, rel))
		if err != nil {
			t.Fatalf("read golden %s: %v", rel, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from golden:\n--- got ---\n%s\n--- want ---\n%s", rel, got, want)
		}
	}
}

// listBundleFiles walks dir and returns sorted relative paths of regular files.
func listBundleFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			// Return an empty list if the root does not exist yet — in -update
			// mode the golden dir may not be present on first run.
			if os.IsNotExist(walkErr) && path == dir {
				return filepath.SkipDir
			}
			return walkErr
		}
		if info.Mode().IsRegular() {
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	sort.Strings(files)
	return files
}

// Ensure deployer package is referenced so unused-import rules are satisfied.
var _ = deployer.SortComponentRefsByDeploymentOrder
