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
		"aicr_yaml_value cluster-values.yaml 'controlPlane.bootstrapToken'",
		"controlPlane.bootstrapToken is required but has no value",
	} {
		if !strings.Contains(string(script), want) {
			t.Errorf("install.sh lacks %q", want)
		}
	}
	if strings.Contains(string(script), "'global.tolerations'") {
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
	start := strings.Index(script, "aicr_yaml_value() {")
	if start < 0 {
		t.Fatal("guard block not found in install.sh")
	}
	end := strings.Index(script[start:], "\nfi\n")
	if end < 0 {
		t.Fatal("guard block has no closing fi")
	}
	return script[start : start+end+4]
}

// TestInstallScript_RequiredDynamicGuard_SharedLeafAndStubShapes covers two
// required paths that end in the same key, which a leaf-only match would
// confuse, and the stub shapes an operator can leave behind: a bare ” stub,
// an empty string followed by a comment, and a value that merely contains a
// hash sign, which is not a comment.
func TestInstallScript_RequiredDynamicGuard_SharedLeafAndStubShapes(t *testing.T) {
	c := requiredDynamicComponent(true)
	c.Values = map[string]any{
		"controlPlane": map[string]any{"url": "", "bootstrapToken": ""},
		"cluster":      map[string]any{"url": "", "uid": "u-1"},
	}
	c.DynamicPaths = []string{"controlPlane.url", "cluster.url", "controlPlane.bootstrapToken"}
	c.RequiredDynamicPaths = []string{"controlPlane.url", "cluster.url", "controlPlane.bootstrapToken"}
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
	for _, want := range []string{"'controlPlane.url'", "'cluster.url'", "'controlPlane.bootstrapToken'"} {
		if !strings.Contains(string(script), "aicr_yaml_value cluster-values.yaml "+want) {
			t.Errorf("install.sh lacks a full-path check for %s", want)
		}
	}
	guard := extractGuardBlock(t, string(script))
	cv := filepath.Join(dir, "cluster-values.yaml")
	filled := "cluster:\n  url: https://c.example\ncontrolPlane:\n  bootstrapToken: FILLED\n  url: https://cp.example\n"
	cases := []struct {
		name    string
		content string
		wantRC  int
	}{
		{"all three filled", filled, 0},
		{"only cluster.url filled, controlPlane.url empty", "cluster:\n  url: https://c.example\ncontrolPlane:\n  bootstrapToken: FILLED\n  url: \"\"\n", 1},
		{"only controlPlane.url filled, cluster.url empty", "cluster:\n  url: \"\"\ncontrolPlane:\n  bootstrapToken: FILLED\n  url: https://cp.example\n", 1},
		{"single-quoted empty stub", "cluster:\n  url: ''\ncontrolPlane:\n  bootstrapToken: FILLED\n  url: https://cp.example\n", 1},
		{"empty string with a trailing comment", "cluster:\n  url: https://c.example\ncontrolPlane:\n  bootstrapToken: \"\" # paste here\n  url: https://cp.example\n", 1},
		{"tilde", "cluster:\n  url: https://c.example\ncontrolPlane:\n  bootstrapToken: ~\n  url: https://cp.example\n", 1},
		{"value containing a hash is not a comment", "cluster:\n  url: https://c.example\ncontrolPlane:\n  bootstrapToken: abc#def\n  url: https://cp.example\n", 0},
		{"required key missing entirely", "cluster:\n  url: https://c.example\ncontrolPlane:\n  url: https://cp.example\n", 1},
		{"comment and blank lines are ignored", "# header\n---\ncluster:\n  # the FQDN\n  url: https://c.example\n\ncontrolPlane:\n  bootstrapToken: FILLED\n  url: https://cp.example\n", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(cv, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write cluster-values.yaml: %v", err)
			}
			if rc := runGuard(t, guard, dir); rc != tc.wantRC {
				t.Errorf("guard exited %d, want %d for:\n%s", rc, tc.wantRC, tc.content)
			}
		})
	}
}
