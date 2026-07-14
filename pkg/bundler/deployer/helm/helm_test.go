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
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
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
	recipeFile := "recipe.yaml"
	if err := os.WriteFile(filepath.Join(outputDir, recipeFile), []byte("apiVersion: aicr.run/v1alpha2\nkind: Recipe\n"), 0600); err != nil {
		t.Fatalf("write %s: %v", recipeFile, err)
	}

	g := &Generator{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {"crds": map[string]any{"enabled": true}},
			"gpu-operator": {"enabled": true},
		},
		Version:          "v1.0.0",
		IncludeChecksums: true,
		RecipeFile:       recipeFile,
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
		recipeFile,
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

func TestGenerate_MissingRecipeFile(t *testing.T) {
	g := &Generator{
		RecipeResult: createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{
			"cert-manager": {},
			"gpu-operator": {},
		},
		Version:    "v1.0.0",
		RecipeFile: "recipe.yaml",
	}

	_, err := g.Generate(context.Background(), t.TempDir())
	if err == nil {
		t.Fatal("Generate() with missing recipe file expected error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to stat data file") {
		t.Errorf("Generate() error = %v, want missing file context", err)
	}
}

// TestGenerate_RemovesStaleUndeployScript verifies that regenerating a
// bundle over an output directory that already contains a top-level
// undeploy.sh from a pre-removal bundle deletes the stale file.
// localformat.Write only prunes NNN-* folders; without the explicit removal
// in Generate, an executable, unchecksummed undeploy.sh would survive
// regeneration and contradict the new README's uninstall guidance.
func TestGenerate_RemovesStaleUndeployScript(t *testing.T) {
	ctx := context.Background()
	outputDir := t.TempDir()

	stalePath := filepath.Join(outputDir, "undeploy.sh")
	if err := os.WriteFile(stalePath, []byte("#!/bin/bash\necho stale\n"), 0o755); err != nil {
		t.Fatalf("seed stale undeploy.sh: %v", err)
	}

	g := &Generator{
		RecipeResult:    createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{"cert-manager": {}, "gpu-operator": {}},
		Version:         "v1.0.0",
	}
	if _, err := g.Generate(ctx, outputDir); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("expected stale undeploy.sh to be removed, stat err = %v", err)
	}
}

// TestGenerate_StaleUndeployRemovalErrorPropagates locks the stale-removal
// error path: if os.Remove fails with anything other than ENOENT (e.g.,
// the parent directory is not writable), Generate must surface the error
// rather than silently overwrite the rest of the bundle. Skips on
// platforms where revoking parent-dir write permission does not block
// unlink (notably when the test runs as root).
func TestGenerate_StaleUndeployRemovalErrorPropagates(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod 0o500 will not block unlink")
	}
	ctx := context.Background()
	outputDir := t.TempDir()

	stalePath := filepath.Join(outputDir, "undeploy.sh")
	if err := os.WriteFile(stalePath, []byte("#!/bin/bash\n"), 0o755); err != nil {
		t.Fatalf("seed stale undeploy.sh: %v", err)
	}
	// Drop write permission on the parent dir so os.Remove fails with
	// EACCES rather than the ENOENT case Generate intentionally swallows.
	if err := os.Chmod(outputDir, 0o500); err != nil {
		t.Fatalf("chmod outputDir read-only: %v", err)
	}
	t.Cleanup(func() {
		// Restore so t.TempDir's cleanup can recurse.
		_ = os.Chmod(outputDir, 0o755)
	})

	g := &Generator{
		RecipeResult:    createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{"cert-manager": {}, "gpu-operator": {}},
		Version:         "v1.0.0",
	}
	_, err := g.Generate(ctx, outputDir)
	if err == nil {
		t.Fatal("expected Generate to fail when stale undeploy.sh cannot be removed")
	}
	if !strings.Contains(err.Error(), "failed to remove stale undeploy.sh") {
		t.Errorf("expected error to mention stale undeploy.sh removal, got: %v", err)
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
// Property tests (helpers and data-shape preservation)
// ---------------------------------------------------------------------------

// TestReverseReleases verifies that reverseReleases projects every emitted
// localformat.Folder (including injected *-pre / *-post auxiliaries) into a
// (release, namespace) pair in reverse-install order. The bundle README
// uses this to enumerate every helm uninstall command — recipe-component
// order alone misses the auxiliary folders the deploy.sh loop installs.
func TestReverseReleases(t *testing.T) {
	tests := []struct {
		name string
		in   []localformat.Folder
		want []releaseRef
	}{
		{
			name: "empty",
			in:   nil,
			want: []releaseRef{},
		},
		{
			name: "single primary",
			in: []localformat.Folder{
				{Name: "cert-manager", Namespace: "cert-manager"},
			},
			want: []releaseRef{
				{Name: "cert-manager", Namespace: "cert-manager"},
			},
		},
		{
			name: "pre + primary + post",
			in: []localformat.Folder{
				{Name: "gpu-operator-pre", Namespace: "gpu-operator"},
				{Name: "gpu-operator", Namespace: "gpu-operator"},
				{Name: "gpu-operator-post", Namespace: "gpu-operator"},
			},
			want: []releaseRef{
				{Name: "gpu-operator-post", Namespace: "gpu-operator"},
				{Name: "gpu-operator", Namespace: "gpu-operator"},
				{Name: "gpu-operator-pre", Namespace: "gpu-operator"},
			},
		},
		{
			name: "multi-component reversal",
			in: []localformat.Folder{
				{Name: "cert-manager", Namespace: "cert-manager"},
				{Name: "gpu-operator", Namespace: "gpu-operator"},
				{Name: "nvsentinel", Namespace: "nvsentinel"},
			},
			want: []releaseRef{
				{Name: "nvsentinel", Namespace: "nvsentinel"},
				{Name: "gpu-operator", Namespace: "gpu-operator"},
				{Name: "cert-manager", Namespace: "cert-manager"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reverseReleases(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("reverseReleases() = %v, want %v", got, tt.want)
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
// Internal generators (generateDeployScript)
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
			APIVersion: "aicr.run/v1alpha2",
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
		ComponentPostManifests: map[string]map[string][]byte{
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

// TestBundleGolden_MixedGPUOperator exercises the full three-phase
// emission: pre (Namespace manifest) + primary (upstream gpu-operator
// chart) + post (dcgm-exporter manifest). Locks in the lexicographic
// ordering 001-gpu-operator-pre/, 002-gpu-operator/, 003-gpu-operator-post/
// the deploy.sh glob iterates in install order.
func TestBundleGolden_MixedGPUOperator(t *testing.T) {
	outDir := t.TempDir()
	g := &Generator{
		RecipeResult: singleComponentRecipe(
			"gpu-operator", "privileged-gpu-operator", "gpu-operator", "v25.3.3",
			"https://helm.ngc.nvidia.com/nvidia"),
		ComponentValues: map[string]map[string]any{
			"gpu-operator": {"driver": map[string]any{"enabled": true}},
		},
		ComponentPreManifests: map[string]map[string][]byte{
			"gpu-operator": {
				"components/gpu-operator/manifests/talos-namespace.yaml": []byte(`# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.

apiVersion: v1
kind: Namespace
metadata:
  name: privileged-gpu-operator
  labels:
    pod-security.kubernetes.io/enforce: privileged
`),
			},
		},
		ComponentPostManifests: map[string]map[string][]byte{
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
			APIVersion: "aicr.run/v1alpha2",
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

// TestBundleGolden_MixedWithPre exercises the pre-injection path:
// a component with ComponentPreManifests (no post manifests) and an
// upstream Helm chart emits exactly two folders in order:
//
//	001-foo-pre/   (local-helm wrapping the namespace manifest)
//	002-foo/       (upstream Helm primary)
//
// Install ordering: pre runs before primary, the opposite of post.
func TestBundleGolden_MixedWithPre(t *testing.T) {
	outDir := t.TempDir()
	g := &Generator{
		RecipeResult: singleComponentRecipe(
			"foo", "privileged-foo", "foo", "v1.0.0",
			"https://example.com/charts"),
		ComponentValues: map[string]map[string]any{"foo": {}},
		ComponentPreManifests: map[string]map[string][]byte{
			"foo": {
				"components/foo/manifests/talos-namespace.yaml": []byte(`# Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.

apiVersion: v1
kind: Namespace
metadata:
  name: privileged-foo
  labels:
    pod-security.kubernetes.io/enforce: privileged
`),
			},
		},
		Version: "v1.0.0",
	}
	if _, err := g.Generate(context.Background(), outDir); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	assertBundleGolden(t, outDir, "testdata/mixed_with_pre")
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

// singleComponentRecipe builds a RecipeResult with exactly one Helm component.
func singleComponentRecipe(name, namespace, chart, version, source string) *recipe.RecipeResult {
	return &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: "aicr.run/v1alpha2",
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
		APIVersion: "aicr.run/v1alpha2",
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
