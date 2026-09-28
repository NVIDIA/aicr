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

package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// Component names below are synthetic and match no registry entry, so nothing
// here asserts a verdict for a real component (ADR-021 testing strategy).

func syntheticRecipeFile(t *testing.T, path string, components map[string]string) string {
	t.Helper()
	doc := "kind: RecipeResult\napiVersion: aicr.run/v1\nmetadata:\n  version: test\ncomponentRefs:\n"
	for name, version := range components {
		doc += fmt.Sprintf("  - name: %s\n    type: Helm\n    source: https://charts.invalid/synthetic\n    version: %s\n",
			name, version)
	}
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("setup: write %s: %v", path, err)
	}
	return path
}

// runUpgradeCheck invokes the command against a fresh tree and returns
// everything it wrote to the root writer, which is where CLI output belongs.
func runUpgradeCheck(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	app := &cli.Command{
		Name:     "aicr",
		Writer:   &buf,
		Commands: []*cli.Command{upgradeCheckCmd()},
	}
	err := app.Run(t.Context(), append([]string{"aicr", "upgrade-check"}, args...))
	return buf.String(), err
}

func TestUpgradeCheckCmd_CommandStructure(t *testing.T) {
	cmd := upgradeCheckCmd()

	if cmd.Name != "upgrade-check" {
		t.Errorf("command name = %q, want %q", cmd.Name, "upgrade-check")
	}
	if cmd.Category != functionalCategoryName {
		t.Errorf("category = %q, want %q", cmd.Category, functionalCategoryName)
	}

	for _, flagName := range []string{"from", "to", "deployer", "fail-on-error", "output", "format"} {
		found := false
		for _, f := range cmd.Flags {
			if hasFlag(f, flagName) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing flag: %s", flagName)
		}
	}

	// --fail-on-error defaults to true, mirroring `aicr validate`: the exit
	// code is what makes running the check worth something in a pipeline.
	for _, f := range cmd.Flags {
		bf, ok := f.(*cli.BoolFlag)
		if ok && bf.Name == "fail-on-error" && !bf.Value {
			t.Error("--fail-on-error defaults to false, want true")
		}
	}
}

func TestUpgradeCheckCmd_Validation(t *testing.T) {
	dir := t.TempDir()
	recipePath := syntheticRecipeFile(t, filepath.Join(dir, "recipe.yaml"), map[string]string{"synthetic-alpha": "1.0.0"})

	tests := []struct {
		name       string
		args       []string
		errContain string
	}{
		{"no flags", nil, "--from is required"},
		{
			name:       "unknown deployer",
			args:       []string{"--from", recipePath, "--to", recipePath, "--deployer", "nope"},
			errContain: "invalid deployer type",
		},
		{
			name:       "unknown format",
			args:       []string{"--from", recipePath, "--to", recipePath, "--format", "toml"},
			errContain: "unknown output format",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runUpgradeCheck(t, tt.args...)
			if err == nil {
				t.Fatalf("expected an error containing %q, got none", tt.errContain)
			}
			if !strings.Contains(err.Error(), tt.errContain) {
				t.Errorf("error = %v, want error containing %q", err, tt.errContain)
			}
		})
	}
}

func TestUpgradeCheckCmd_ReportsAndExits(t *testing.T) {
	dir := t.TempDir()
	from := syntheticRecipeFile(t, filepath.Join(dir, "from.yaml"), map[string]string{"synthetic-alpha": "0.18.0"})
	to := syntheticRecipeFile(t, filepath.Join(dir, "to.yaml"), map[string]string{"synthetic-alpha": "0.19.0"})

	// Unrecorded, and a 0.x minor is a breaking boundary, so this run fails.
	out, err := runUpgradeCheck(t, "--from", from, "--to", to, "--format", "table")
	if err == nil {
		t.Fatal("expected a non-zero outcome for an unrecorded breaking jump")
	}
	if code := errors.ExitCodeFromError(err); code == 0 {
		t.Errorf("exit code = %d, want non-zero", code)
	}
	// The report prints in full either way: informing and erroring are not
	// alternatives, and the exit code is orthogonal to the report.
	if !strings.Contains(out, "synthetic-alpha") {
		t.Errorf("report was not written before the failure:\n%s", out)
	}

	out, err = runUpgradeCheck(t, "--from", from, "--to", to, "--format", "table", "--fail-on-error=false")
	if err != nil {
		t.Fatalf("--fail-on-error=false still failed: %v", err)
	}
	if !strings.Contains(out, "synthetic-alpha") {
		t.Errorf("report missing under --fail-on-error=false:\n%s", out)
	}
}

func TestUpgradeCheckCmd_WritesToOutputFile(t *testing.T) {
	dir := t.TempDir()
	from := syntheticRecipeFile(t, filepath.Join(dir, "from.yaml"), map[string]string{"synthetic-alpha": "1.2.0"})
	to := syntheticRecipeFile(t, filepath.Join(dir, "to.yaml"), map[string]string{"synthetic-alpha": "1.2.3"})
	outPath := filepath.Join(dir, "report.json")

	// The bump is unrecorded, so it reports unknown and the run exits
	// non-zero. The file is still written: the exit code is orthogonal to the
	// report, which is what this asserts.
	if _, err := runUpgradeCheck(t,
		"--from", from, "--to", to, "--format", "json", "--output", outPath); err == nil {
		t.Fatal("expected a non-zero outcome for an unrecorded jump")
	}
	data, err := os.ReadFile(outPath) //nolint:gosec // test-local temp path
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	if !strings.Contains(string(data), `"component": "synthetic-alpha"`) {
		t.Errorf("JSON report does not name the changed component:\n%s", data)
	}
}

func TestUpgradeCheckCmd_TableRejectsConfigMapOutput(t *testing.T) {
	dir := t.TempDir()
	from := syntheticRecipeFile(t, filepath.Join(dir, "from.yaml"), map[string]string{"synthetic-alpha": "1.2.0"})
	to := syntheticRecipeFile(t, filepath.Join(dir, "to.yaml"), map[string]string{"synthetic-alpha": "1.2.3"})

	_, err := runUpgradeCheck(t,
		"--from", from, "--to", to, "--format", "table", "--output", "cm://default/report")
	if err == nil {
		t.Fatal("table output to a ConfigMap destination was accepted")
	}
	if !strings.Contains(err.Error(), "ConfigMap") {
		t.Errorf("error = %v, want it to name the ConfigMap destination", err)
	}
}
