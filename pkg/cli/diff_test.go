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

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/diff"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

func TestDiffCmd_CommandStructure(t *testing.T) {
	cmd := diffCmd()

	if cmd.Name != "diff" {
		t.Errorf("command name = %q, want %q", cmd.Name, "diff")
	}

	if cmd.Category != functionalCategoryName {
		t.Errorf("category = %q, want %q", cmd.Category, functionalCategoryName)
	}
}

func TestDiffCmd_Flags(t *testing.T) {
	cmd := diffCmd()

	expectedFlags := []string{"baseline", "target", "fail-on-drift", "output", "format", "kubeconfig"}
	for _, flagName := range expectedFlags {
		found := false
		for _, flag := range cmd.Flags {
			if hasFlag(flag, flagName) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing flag: %s", flagName)
		}
	}

	// Check baseline alias
	for _, flag := range cmd.Flags {
		if hasFlag(flag, "baseline") && !hasFlag(flag, "b") {
			t.Error("--baseline flag missing -b alias")
		}
	}
}

func TestDiffCmd_Validation(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantErr    bool
		errContain string
	}{
		{
			name:       "no flags",
			args:       []string{"aicr", "diff"},
			wantErr:    true,
			errContain: "--baseline is required",
		},
		{
			name:       "baseline without target",
			args:       []string{"aicr", "diff", "--baseline", "b.yaml"},
			wantErr:    true,
			errContain: "--target is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := diffCmd()
			app := &cli.Command{
				Name:     "aicr",
				Commands: []*cli.Command{cmd},
			}
			err := app.Run(t.Context(), tt.args)

			if (err != nil) != tt.wantErr {
				t.Errorf("error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr && tt.errContain != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("error = %v, want error containing %q", err, tt.errContain)
				}
			}
		})
	}
}

func TestWriteTable_ToFile(t *testing.T) {
	tmpDir := t.TempDir()
	outFile := filepath.Join(tmpDir, "out.txt")

	result := &diff.Result{Changes: make([]diff.Change, 0)}
	f, err := os.Create(outFile)
	if err != nil {
		t.Fatalf("failed to create output file: %v", err)
	}

	err = diff.WriteTable(f, result)
	if closeErr := f.Close(); closeErr != nil && err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("WriteTable to file failed: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}
	if !strings.Contains(string(data), "NO CHANGES") {
		t.Errorf("expected NO CHANGES in table output, got: %s", string(data))
	}
}

func TestWriteTable_ToStdout(t *testing.T) {
	result := &diff.Result{Changes: make([]diff.Change, 0)}

	// WriteTable to stdout should not error.
	err := diff.WriteTable(os.Stdout, result)
	if err != nil {
		t.Errorf("WriteTable to stdout failed: %v", err)
	}
}

func TestDiffCmd_TableDefaultStdout(t *testing.T) {
	cmd := diffCmd()
	app := &cli.Command{
		Name:     "aicr",
		Commands: []*cli.Command{cmd},
	}

	tmpDir := t.TempDir()
	snap := writeMinimalSnapshot(t, tmpDir, "snap.yaml")

	err := app.Run(t.Context(), []string{
		"aicr", "diff",
		"--baseline", snap,
		"--target", snap,
		"--format", "table",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestDiffCmd_TableConfigMapRejected exercises the ConfigMap URI guard
// through the real CLI code path (writeDiffResult), so a regression in the
// guard inside pkg/cli/diff.go would fail this test.
func TestDiffCmd_TableConfigMapRejected(t *testing.T) {
	cmd := diffCmd()
	app := &cli.Command{
		Name:     "aicr",
		Commands: []*cli.Command{cmd},
	}

	tmpDir := t.TempDir()
	snap := writeMinimalSnapshot(t, tmpDir, "snap.yaml")

	err := app.Run(t.Context(), []string{
		"aicr", "diff",
		"--baseline", snap,
		"--target", snap,
		"--format", "table",
		"--output", "cm://default/my-cm",
	})
	if err == nil {
		t.Fatal("expected ConfigMap rejection error, got nil")
	}
	if !strings.Contains(err.Error(), "ConfigMap") {
		t.Errorf("expected error mentioning ConfigMap, got: %v", err)
	}
}

// TestDiffCmd_FailOnDriftReturnsConflict exercises the core CI-gating
// contract end-to-end: when baseline and target differ and --fail-on-drift
// is set, the CLI must return a non-nil error whose code maps to the
// "drift detected" surface (ErrCodeConflict, exit code 2). Without this
// test, a regression that silently swallows the drift signal would not be
// caught.
func TestDiffCmd_FailOnDriftReturnsConflict(t *testing.T) {
	tmpDir := t.TempDir()
	baselinePath := filepath.Join(tmpDir, "baseline.yaml")
	targetPath := filepath.Join(tmpDir, "target.yaml")

	writeSnapshotWithVersion(t, baselinePath, "1.31.0")
	writeSnapshotWithVersion(t, targetPath, "1.32.4")

	cmd := diffCmd()
	app := &cli.Command{Name: "aicr", Commands: []*cli.Command{cmd}}

	err := app.Run(t.Context(), []string{
		"aicr", "diff",
		"--baseline", baselinePath,
		"--target", targetPath,
		"--format", "json",
		"--fail-on-drift",
	})
	if err == nil {
		t.Fatal("expected non-nil error from --fail-on-drift on differing snapshots, got nil")
	}
	if !strings.Contains(err.Error(), "drift detected") {
		t.Errorf("expected error mentioning 'drift detected', got: %v", err)
	}

	// Verify that the same inputs without --fail-on-drift do NOT error.
	cmd2 := diffCmd()
	app2 := &cli.Command{Name: "aicr", Commands: []*cli.Command{cmd2}}
	if err := app2.Run(t.Context(), []string{
		"aicr", "diff",
		"--baseline", baselinePath,
		"--target", targetPath,
		"--format", "json",
	}); err != nil {
		t.Errorf("--fail-on-drift unset should not error on drift, got: %v", err)
	}
}

// writeSnapshotWithVersion writes a minimal snapshot with a single K8s.server.version
// reading. Used by drift-gating tests that need two distinguishable snapshots.
func writeSnapshotWithVersion(t *testing.T, path, version string) {
	t.Helper()
	content := `kind: Snapshot
apiVersion: aicr.nvidia.com/v1alpha1
metadata: {}
measurements:
  - type: K8s
    subtypes:
      - subtype: server
        data:
          version: ` + version + `
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write snapshot: %v", err)
	}
}

// TestWriteDiffResult_TableToFile exercises writeDiffResult directly so the
// named-return Close-error promotion and os.Create paths are covered.
func TestWriteDiffResult_TableToFile(t *testing.T) {
	tmpDir := t.TempDir()
	outFile := filepath.Join(tmpDir, "out.txt")

	cmd := buildDiffCommandWithOutput(t, outFile)
	result := &diff.Result{Changes: make([]diff.Change, 0)}

	if err := writeDiffResult(t.Context(), cmd, serializer.FormatTable, result); err != nil {
		t.Fatalf("writeDiffResult failed: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("failed to read output file: %v", err)
	}
	if !strings.Contains(string(data), "NO CHANGES") {
		t.Errorf("expected NO CHANGES in table output, got: %s", string(data))
	}
}

// TestWriteDiffResult_CreateFails verifies that os.Create errors propagate
// as wrapped structured errors rather than panicking.
func TestWriteDiffResult_CreateFails(t *testing.T) {
	// A path under a non-existent directory makes os.Create fail.
	bogusPath := filepath.Join(t.TempDir(), "does-not-exist", "out.txt")

	cmd := buildDiffCommandWithOutput(t, bogusPath)
	result := &diff.Result{Changes: make([]diff.Change, 0)}

	err := writeDiffResult(t.Context(), cmd, serializer.FormatTable, result)
	if err == nil {
		t.Fatal("expected error from os.Create on missing parent dir, got nil")
	}
	if !strings.Contains(err.Error(), "failed to create output file") {
		t.Errorf("expected wrapped create error, got: %v", err)
	}
}

// buildDiffCommandWithOutput constructs a parsed *cli.Command with --output
// set to the supplied path so writeDiffResult can be exercised in isolation.
func buildDiffCommandWithOutput(t *testing.T, output string) *cli.Command {
	t.Helper()
	cmd := diffCmd()
	app := &cli.Command{Name: "aicr", Commands: []*cli.Command{cmd}}

	tmpDir := t.TempDir()
	snap := writeMinimalSnapshot(t, tmpDir, "snap.yaml")

	// Run captures the parsed flags onto cmd; intercept before any IO via the
	// Action so the function returns early but flags are populated.
	var captured *cli.Command
	cmd.Action = func(_ context.Context, c *cli.Command) error {
		captured = c
		return nil
	}
	if err := app.Run(t.Context(), []string{
		"aicr", "diff",
		"--baseline", snap,
		"--target", snap,
		"--format", "table",
		"--output", output,
	}); err != nil {
		t.Fatalf("flag parse setup failed: %v", err)
	}
	if captured == nil {
		t.Fatal("flag parse setup did not capture cmd")
	}
	return captured
}

// writeMinimalSnapshot creates a minimal valid snapshot YAML for testing.
func writeMinimalSnapshot(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	content := `kind: Snapshot
apiVersion: aicr.nvidia.com/v1alpha1
metadata: {}
measurements: []
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write snapshot: %v", err)
	}
	return path
}
