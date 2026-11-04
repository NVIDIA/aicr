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

package localformat_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
)

// requiredDynamicComponent is an upstream Helm component with one dynamic
// path the registry requires and one it does not.
func requiredDynamicComponent(required bool) localformat.Component {
	c := localformat.Component{
		Name:         "runai-fixture",
		Namespace:    "runai",
		Repository:   "https://helm.example.invalid/charts",
		ChartName:    "runai-cluster",
		Version:      "3.0.20",
		Values:       map[string]any{"controlPlane": map[string]any{"url": "https://cp.example", "bootstrapToken": ""}, "global": map[string]any{"tolerations": []any{}}},
		DynamicPaths: []string{"controlPlane.bootstrapToken", "global.tolerations"},
	}
	if required {
		c.RequiredDynamicPaths = []string{"controlPlane.bootstrapToken"}
	}
	return c
}

func writeRequiredDynamicFolder(t *testing.T, required bool) string {
	t.Helper()
	outDir := t.TempDir()
	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir:  outDir,
		Components: []localformat.Component{requiredDynamicComponent(required)},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if len(res.Folders) != 1 {
		t.Fatalf("want 1 folder, got %d", len(res.Folders))
	}
	return filepath.Join(outDir, res.Folders[0].Dir)
}

func TestInstallScript_RequiredDynamicGuard(t *testing.T) {
	dir := writeRequiredDynamicFolder(t, true)
	script, err := os.ReadFile(filepath.Join(dir, "install.sh"))
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	for _, want := range []string{
		"grep -E '^[[:space:]]*bootstrapToken:[[:space:]]*[^[:space:]]' cluster-values.yaml",
		"controlPlane.bootstrapToken is required but has no value",
	} {
		if !strings.Contains(string(script), want) {
			t.Errorf("install.sh lacks %q", want)
		}
	}
	if strings.Contains(string(script), "tolerations:[[:space:]]") {
		t.Error("install.sh guards global.tolerations, which the registry does not require")
	}

	// The guard is a self-contained block; run it on its own against the
	// folder's real cluster-values.yaml in each of its three states.
	guard := extractGuardBlock(t, string(script))
	run := func(env ...string) int { return runGuard(t, guard, dir, env...) }
	if rc := run(); rc != 1 {
		t.Errorf("empty stub: guard exited %d, want 1", rc)
	}
	if rc := run("DRY_RUN_FLAG=--dry-run"); rc != 0 {
		t.Errorf("empty stub under --dry-run: guard exited %d, want 0", rc)
	}
	cv := filepath.Join(dir, "cluster-values.yaml")
	raw, err := os.ReadFile(cv)
	if err != nil {
		t.Fatalf("read cluster-values.yaml: %v", err)
	}
	filled := strings.Replace(string(raw), "bootstrapToken: \"\"", "bootstrapToken: filled-at-install", 1)
	if filled == string(raw) {
		t.Fatalf("cluster-values.yaml does not carry the empty stub this test expects:\n%s", raw)
	}
	if err := os.WriteFile(cv, []byte(filled), 0o600); err != nil {
		t.Fatalf("write cluster-values.yaml: %v", err)
	}
	if rc := run(); rc != 0 {
		t.Errorf("filled stub: guard exited %d, want 0", rc)
	}
}

func TestInstallScript_NoGuardWithoutRequiredDynamicPaths(t *testing.T) {
	dir := writeRequiredDynamicFolder(t, false)
	script, err := os.ReadFile(filepath.Join(dir, "install.sh"))
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	if strings.Contains(string(script), "has no value in cluster-values.yaml") {
		t.Error("install.sh carries the required-dynamic guard for a component with no required dynamic paths")
	}
}

// TestInstallScript_RequiredDynamicGuard_AncestorDeclaration covers
// --dynamic on the parent map. The bundler moves the whole map into
// cluster-values.yaml, leaves included, so the file as written carries the
// required leaf empty beneath the map; a map the values never defined is
// written as a bare "" stub instead. The guard must fail on both, pass once
// the required leaf has a value beneath the map, and never trip on the map
// line itself, which has no scalar value.
func TestInstallScript_RequiredDynamicGuard_AncestorDeclaration(t *testing.T) {
	c := requiredDynamicComponent(true)
	c.DynamicPaths = []string{"controlPlane"}
	c.RequiredDynamicPaths = []string{"controlPlane.bootstrapToken"}
	outDir := t.TempDir()
	res, err := localformat.Write(context.Background(), localformat.Options{
		OutputDir:  outDir,
		Components: []localformat.Component{c},
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	dir := filepath.Join(outDir, res.Folders[0].Dir)
	script, err := os.ReadFile(filepath.Join(dir, "install.sh"))
	if err != nil {
		t.Fatalf("read install.sh: %v", err)
	}
	guard := extractGuardBlock(t, string(script))
	cv := filepath.Join(dir, "cluster-values.yaml")
	raw, err := os.ReadFile(cv)
	if err != nil {
		t.Fatalf("read cluster-values.yaml: %v", err)
	}
	if !strings.Contains(string(raw), "controlPlane:") || !strings.Contains(string(raw), "bootstrapToken: \"\"") {
		t.Fatalf("cluster-values.yaml does not carry the moved map with its empty leaf:\n%s", raw)
	}
	cases := []struct {
		name    string
		content string
		wantRC  int
	}{
		{"map moved as written, leaf still empty", string(raw), 1},
		{"bare stub for a map the values never defined", "controlPlane: \"\"\n", 1},
		{"leaf filled beneath the map", "controlPlane:\n  bootstrapToken: FILLED\n  url: https://t.example\n", 0},
		{"leaf present but empty", "controlPlane:\n  bootstrapToken: \"\"\n  url: https://t.example\n", 1},
		{"leaf null", "controlPlane:\n  bootstrapToken: null\n", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(cv, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write cluster-values.yaml: %v", err)
			}
			if rc := runGuard(t, guard, dir); rc != tc.wantRC {
				t.Errorf("guard exited %d, want %d", rc, tc.wantRC)
			}
		})
	}
}

// runGuard runs the extracted guard block in dir and returns its exit code.
func runGuard(t *testing.T, guard, dir string, env ...string) int {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "bash", "-c", guard)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	if runErr := cmd.Run(); runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return exitErr.ExitCode()
		}
		t.Fatalf("run guard: %v", runErr)
	}
	return 0
}

// extractGuardBlock returns the lines from the guard's opening if to its
// closing fi, so the test runs exactly the block the template rendered.
func extractGuardBlock(t *testing.T, script string) string {
	t.Helper()
	start := strings.Index(script, "if [[ -z \"${DRY_RUN_FLAG:-}\" ]]; then\n  if [ -z \"$(grep -E")
	if start < 0 {
		t.Fatal("guard block not found in install.sh")
	}
	end := strings.Index(script[start:], "\nfi\n")
	if end < 0 {
		t.Fatal("guard block has no closing fi")
	}
	return script[start : start+end+4]
}
