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
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// preflightShells returns the bash on PATH and, when the host has an older
// one at /bin/bash (stock macOS ships 3.2), that one too: the pre-flight block
// exists for those users, and CI runs only a newer bash.
func preflightShells() []string {
	var shells []string
	if p, err := exec.LookPath("bash"); err == nil {
		shells = append(shells, p)
	}
	out, err := exec.Command("/bin/bash", "-c", `echo "${BASH_VERSINFO[0]}"`).Output()
	if err == nil && strings.TrimSpace(string(out)) < "4" && !slices.Contains(shells, "/bin/bash") {
		shells = append(shells, "/bin/bash")
	}
	return shells
}

// preflightCase is one run of deploy.sh's CRD-timeout pre-flight block.
type preflightCase struct {
	name      string
	bundleDir string
	crdFolder bool
	tools     []string
	dryRun    bool
	wantFail  bool
}

// A bundle whose components own CRDs runs apply-crds.sh, which refuses to run
// without timeout(1) or gtimeout(1). deploy.sh's pre-flight must stop on that
// before anything is installed, rather than after the components ahead of the
// first CRD owner (#3717), and must not ask for it when nothing needs it.
func TestDeployScript_PreflightRequiresTimeoutForCRDSteps(t *testing.T) {
	shells := preflightShells()
	if len(shells) == 0 {
		t.Skip("bash not available")
	}
	outputDir := t.TempDir()
	g := &Generator{
		RecipeResult:    createTestRecipeResult(),
		ComponentValues: map[string]map[string]any{"cert-manager": {}},
		Version:         "v1.0.0",
	}
	if _, genErr := g.Generate(context.Background(), outputDir); genErr != nil {
		t.Fatalf("Generate failed: %v", genErr)
	}
	script, err := os.ReadFile(filepath.Join(outputDir, "deploy.sh"))
	if err != nil {
		t.Fatalf("read deploy.sh: %v", err)
	}
	lines := strings.Split(string(script), "\n")
	start := slices.Index(lines, "preflight_failed=false")
	end := slices.Index(lines, "# Bundle namespace list (deduplicated)")
	if start < 0 || end < start {
		t.Fatalf("deploy.sh pre-flight no longer opens with preflight_failed=false before the namespace list")
	}
	block := strings.Join(lines[start:end], "\n")

	tests := []preflightCase{
		{name: "CRD step without timeout fails", crdFolder: true, wantFail: true},
		{name: "CRD step under a path with brackets fails", bundleDir: "bundles [v2]", crdFolder: true, wantFail: true},
		{name: "CRD step with timeout passes", crdFolder: true, tools: []string{"timeout"}},
		{name: "CRD step with gtimeout passes", crdFolder: true, tools: []string{"gtimeout"}},
		{name: "no CRD step needs no timeout", crdFolder: false},
		{name: "dry run needs no timeout", crdFolder: true, dryRun: true},
	}
	for _, shell := range shells {
		for _, tt := range tests {
			t.Run(shell+"/"+tt.name, func(t *testing.T) {
				runPreflightCase(t, shell, block, tt)
			})
		}
	}
}

func runPreflightCase(t *testing.T, shell, block string, tt preflightCase) {
	t.Helper()
	dir := t.TempDir()
	bundleDir := tt.bundleDir
	if bundleDir == "" {
		bundleDir = "bundle"
	}
	bundle := filepath.Join(dir, bundleDir)
	for _, folder := range []string{"001-cert-manager", "002-nvsentinel"} {
		if err := os.MkdirAll(filepath.Join(bundle, folder), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	if tt.crdFolder {
		if err := os.WriteFile(filepath.Join(bundle, "002-nvsentinel", "apply-crds.sh"), []byte("#!/usr/bin/env bash\n"), 0o700); err != nil {
			t.Fatalf("write apply-crds.sh: %v", err)
		}
	}
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, tool := range tt.tools {
		if err := os.WriteFile(filepath.Join(binDir, tool), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatalf("write %s stub: %v", tool, err)
		}
	}
	dryRun := ""
	if tt.dryRun {
		dryRun = "--dry-run"
	}
	harness := "set -euo pipefail\nSCRIPT_DIR=" + shellQuote(bundle) + "\nDRY_RUN_FLAG=" + shellQuote(dryRun) +
		"\n" + block + "\necho \"preflight_failed=${preflight_failed}\"\n"
	cmd := exec.Command(shell, "-c", harness)
	cmd.Env = []string{"PATH=" + binDir}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pre-flight block failed to run: %v\n%s", err, out)
	}
	if gotFail := strings.Contains(string(out), "preflight_failed=true"); gotFail != tt.wantFail {
		t.Errorf("preflight_failed = %t, want %t\n%s", gotFail, tt.wantFail, out)
	}
	if tt.wantFail && !strings.Contains(string(out), "brew install coreutils") {
		t.Errorf("the failure does not say how to install timeout(1):\n%s", out)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
