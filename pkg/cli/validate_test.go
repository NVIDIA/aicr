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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

func TestValidateCmd_CommandStructure(t *testing.T) {
	cmd := validateCmd()

	if cmd.Name != "validate" {
		t.Errorf("command name = %q, want %q", cmd.Name, "validate")
	}

	requiredFlags := []string{"recipe", "phase", "namespace", "node-selector", "toleration", "timeout"}
	for _, flagName := range requiredFlags {
		found := false
		for _, flag := range cmd.Flags {
			if hasFlag(flag, flagName) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing required flag: %s", flagName)
		}
	}
}

func TestValidateCmd_AgentFlags(t *testing.T) {
	cmd := validateCmd()

	agentFlags := []string{
		"namespace",
		"image",
		"image-pull-secret",
		"job-name",
		"service-account-name",
		"node-selector",
		"toleration",
		"timeout",
		"no-cleanup",
	}

	for _, flagName := range agentFlags {
		found := false
		for _, flag := range cmd.Flags {
			if hasFlag(flag, flagName) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing agent flag: %s", flagName)
		}
	}
}

func hasFlag(flag interface{ Names() []string }, name string) bool {
	return slices.Contains(flag.Names(), name)
}

func TestValidateCmd_CNCFSubmissionFlags(t *testing.T) {
	cmd := validateCmd()

	// Verify --cncf-submission and --feature flags exist
	evidenceFlags := []string{"cncf-submission", "feature", "evidence-dir"}
	for _, flagName := range evidenceFlags {
		found := false
		for _, flag := range cmd.Flags {
			if hasFlag(flag, flagName) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing evidence flag: %s", flagName)
		}
	}

	// Verify --feature has -f alias
	for _, flag := range cmd.Flags {
		if hasFlag(flag, "feature") && !hasFlag(flag, "f") {
			t.Error("--feature flag missing -f alias")
		}
	}
}

func TestValidateCmd_CNCFSubmissionFlagValidation(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantErr    bool
		errContain string
	}{
		{
			name:       "cncf-submission without evidence-dir",
			args:       []string{"aicr", "validate", "--cncf-submission"},
			wantErr:    true,
			errContain: "--cncf-submission requires --evidence-dir",
		},
		{
			name:       "feature without cncf-submission",
			args:       []string{"aicr", "validate", "--feature", "dra", "--evidence-dir", "/tmp/test"},
			wantErr:    true,
			errContain: "--feature requires --cncf-submission",
		},
		{
			name:       "cncf-submission with invalid feature",
			args:       []string{"aicr", "validate", "--cncf-submission", "--evidence-dir", "/tmp/test", "--feature", "nonexistent"},
			wantErr:    true,
			errContain: "unknown feature",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := validateCmd()
			// Wrap in a parent app so flag parsing works correctly.
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

func TestValidateCmdFlags_FailFastDefault(t *testing.T) {
	cmd := validateCmd()
	for _, f := range cmd.Flags {
		if !hasFlag(f, "fail-fast") {
			continue
		}
		bf, ok := f.(*cli.BoolFlag)
		if !ok {
			t.Fatal("--fail-fast should be a *cli.BoolFlag")
		}
		if bf.Value {
			t.Error("--fail-fast default should be false")
		}
		return
	}
	t.Error("--fail-fast flag not found in validateCmd flags")
}

func TestValidateCmd_RecipeKindHandling(t *testing.T) {
	tests := []struct {
		name        string
		yamlContent string
		wantErr     bool
		errContain  string
		errAbsent   string
	}{
		{
			name:        "RecipeMetadata without criteria returns clear error",
			yamlContent: "kind: RecipeMetadata\napiVersion: aicr.nvidia.com/v1alpha1\nmetadata:\n  name: test\nspec: {}\n",
			wantErr:     true,
			errContain:  "has no criteria",
		},
		{
			name:        "RecipeMetadata with criteria auto-hydrates",
			yamlContent: "kind: RecipeMetadata\napiVersion: aicr.nvidia.com/v1alpha1\nmetadata:\n  name: test\nspec:\n  criteria:\n    service: eks\n    accelerator: h100\n    intent: training\n",
			wantErr:     true,
			errContain:  "--no-cluster requires --snapshot",
			errAbsent:   "has no criteria",
		},
		{
			name:        "RecipeMixin kind is rejected",
			yamlContent: "kind: RecipeMixin\napiVersion: aicr.nvidia.com/v1alpha1\nmetadata:\n  name: test\nspec: {}\n",
			wantErr:     true,
			errContain:  `kind "RecipeMixin"`,
		},
		{
			name:        "unknown kind is rejected",
			yamlContent: "kind: SomethingElse\napiVersion: aicr.nvidia.com/v1alpha1\n",
			wantErr:     true,
			errContain:  `kind "SomethingElse"`,
		},
		{
			name:        "RecipeResult kind passes kind check",
			yamlContent: "kind: RecipeResult\napiVersion: aicr.nvidia.com/v1alpha1\n",
			wantErr:     true,
			errContain:  "--no-cluster requires --snapshot",
			errAbsent:   "is required",
		},
		{
			name:        "empty kind passes kind check",
			yamlContent: "apiVersion: aicr.nvidia.com/v1alpha1\n",
			wantErr:     true,
			errContain:  "--no-cluster requires --snapshot",
			errAbsent:   "is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			recipeFile := filepath.Join(dir, "recipe.yaml")
			if err := os.WriteFile(recipeFile, []byte(tt.yamlContent), 0o600); err != nil {
				t.Fatalf("failed to write test recipe file: %v", err)
			}

			cmd := validateCmd()
			app := &cli.Command{
				Name:     "aicr",
				Commands: []*cli.Command{cmd},
			}
			err := app.Run(t.Context(), []string{"aicr", "validate", "--recipe", recipeFile, "--no-cluster"})

			if tt.wantErr && err == nil {
				t.Error("expected error but got nil")
				return
			}
			if tt.errContain != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("error = %v, want error containing %q", err, tt.errContain)
				}
			}
			if tt.errAbsent != "" && err != nil {
				if strings.Contains(err.Error(), tt.errAbsent) {
					t.Errorf("error = %v, should NOT contain %q", err, tt.errAbsent)
				}
			}
		})
	}
}
