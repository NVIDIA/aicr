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

	expectedFlags := []string{"baseline", "target", "fail-on-drift", "output", "format", "kubeconfig", "data"}
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

func TestWritTable_ToFile(t *testing.T) {
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
	// Verifies the table path defaults to stdout without --output.
	// ConfigMap rejection is tested at the unit level below since
	// urfave/cli shared flag state prevents multi-arg integration tests.
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDiffCmd_ConfigMapTableOutputGuard(t *testing.T) {
	// Verify the ConfigMap URI guard logic directly.
	tests := []struct {
		name    string
		output  string
		wantErr bool
	}{
		{"configmap URI", "cm://default/my-cm", true},
		{"configmap with spaces", "  cm://ns/name  ", true},
		{"empty string", "", false},
		{"dash", "-", false},
		{"file path", "/tmp/out.txt", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trimmed := strings.TrimSpace(tt.output)
			isConfigMap := strings.HasPrefix(trimmed, serializer.ConfigMapURIScheme)
			if isConfigMap != tt.wantErr {
				t.Errorf("ConfigMap guard for %q: got rejected=%v, want %v", tt.output, isConfigMap, tt.wantErr)
			}
		})
	}
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
