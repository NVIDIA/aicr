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
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/errors"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
)

// TestResolveCNCFAllocationPolicy exercises the #1629 policy threading for
// --cncf-submission runs: no recipe context resolves to an empty policy
// (standalone runs keep the evidence script's capability detection), a
// recipe-backed run resolves the policy from the hydrated recipe, and a
// broken recipe path fails closed instead of silently collecting without a
// policy. The validate Action is overridden so only the resolution flow runs
// — never the collector (which would contact a live cluster).
func TestResolveCNCFAllocationPolicy(t *testing.T) {
	tests := []struct {
		name       string
		recipeYAML string // written to a temp recipe file when non-empty
		recipePath string // used verbatim when non-empty (overrides recipeYAML)
		wantPolicy string
		wantErr    bool
	}{
		{
			name:       "no recipe context resolves empty policy",
			wantPolicy: "",
		},
		{
			name: "recipe context resolves the hydrated policy",
			// Auto-hydrates from the embedded catalog; stock recipes default
			// to device-plugin allocation since the #1327/#1671 flip.
			recipeYAML: "kind: RecipeMetadata\napiVersion: aicr.run/v1alpha2\nmetadata:\n  name: test\nspec:\n  criteria:\n    service: eks\n    accelerator: h100\n    intent: training\n    os: ubuntu\n",
			wantPolicy: v1.GPUAllocationPolicyDevicePluginExtendedResource,
		},
		{
			name:       "unreadable recipe fails closed",
			recipePath: filepath.Join(t.TempDir(), "does-not-exist.yaml"),
			wantErr:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{"validate", "--no-cluster"}
			recipePath := tt.recipePath
			if tt.recipeYAML != "" {
				recipePath = filepath.Join(t.TempDir(), "recipe.yaml")
				if err := os.WriteFile(recipePath, []byte(tt.recipeYAML), 0o600); err != nil {
					t.Fatalf("failed to write test recipe file: %v", err)
				}
			}
			if recipePath != "" {
				args = append(args, "--recipe", recipePath)
			}

			var gotPolicy string
			cmd := validateCmd()
			cmd.Action = func(ctx context.Context, c *cli.Command) error {
				cfg, err := loadCmdConfig(ctx, c)
				if err != nil {
					return err
				}
				resolved, err := cfg.Validation().Resolve()
				if err != nil {
					return err
				}
				gotPolicy, err = resolveCNCFAllocationPolicy(ctx, c, cfg, resolved)
				return err
			}
			err := cmd.Run(t.Context(), args)

			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && gotPolicy != tt.wantPolicy {
				t.Errorf("policy = %q, want %q", gotPolicy, tt.wantPolicy)
			}
		})
	}
}

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
		{
			name:       "cncf-submission with no-cluster",
			args:       []string{"aicr", "validate", "--cncf-submission", "--evidence-dir", "/tmp/test", "--no-cluster"},
			wantErr:    true,
			errContain: "--cncf-submission cannot be combined with --no-cluster",
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
			if tt.wantErr {
				// Every flag-combination rejection in this command is a
				// well-formed-but-invalid request, so assert the structured
				// code (not just the message text) — a wrong code would map to
				// the wrong HTTP status / exit behavior for library callers.
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("error = %v, want code ErrCodeInvalidRequest", err)
				}
				if tt.errContain != "" && (err == nil || !strings.Contains(err.Error(), tt.errContain)) {
					t.Errorf("error = %v, want error containing %q", err, tt.errContain)
				}
			}
		})
	}
}

// TestValidateCmd_NoClusterEvidenceFlags asserts that explicitly requesting an
// attestation (--emit-attestation / --push) alongside the offline --no-cluster
// dry-run is rejected with ErrCodeInvalidRequest, rather than warn-and-ignored.
// (Config-driven suppression is a separate path — see evidenceConfigForRunMode.)
func TestValidateCmd_NoClusterEvidenceFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{
			name: "emit-attestation with no-cluster",
			args: []string{"aicr", "validate", "--no-cluster", "--emit-attestation", "/tmp/out"},
		},
		{
			name: "push with no-cluster",
			args: []string{"aicr", "validate", "--no-cluster", "--emit-attestation", "/tmp/out", "--push", "ghcr.io/x/y"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := validateCmd()
			app := &cli.Command{Name: "aicr", Commands: []*cli.Command{cmd}}
			err := app.Run(t.Context(), tt.args)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want code ErrCodeInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), "cannot be combined with --no-cluster") {
				t.Errorf("error = %v, want a combined-with-no-cluster message", err)
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
			yamlContent: "kind: RecipeMetadata\napiVersion: aicr.run/v1alpha2\nmetadata:\n  name: test\nspec: {}\n",
			wantErr:     true,
			errContain:  "has no criteria",
		},
		{
			name:        "RecipeMetadata with criteria auto-hydrates",
			yamlContent: "kind: RecipeMetadata\napiVersion: aicr.run/v1alpha2\nmetadata:\n  name: test\nspec:\n  criteria:\n    service: eks\n    accelerator: h100\n    intent: training\n",
			wantErr:     true,
			errContain:  "--no-cluster requires --snapshot",
			errAbsent:   "has no criteria",
		},
		{
			name:        "RecipeMixin kind is rejected",
			yamlContent: "kind: RecipeMixin\napiVersion: aicr.run/v1alpha2\nmetadata:\n  name: test\nspec: {}\n",
			wantErr:     true,
			errContain:  `kind "RecipeMixin"`,
		},
		{
			name:        "unknown kind is rejected",
			yamlContent: "kind: SomethingElse\napiVersion: aicr.run/v1alpha2\n",
			wantErr:     true,
			errContain:  `kind "SomethingElse"`,
		},
		{
			name:        "RecipeResult kind passes kind check",
			yamlContent: "kind: RecipeResult\napiVersion: aicr.run/v1alpha2\n",
			wantErr:     true,
			errContain:  "--no-cluster requires --snapshot",
			errAbsent:   "is required",
		},
		{
			name:        "empty kind passes kind check",
			yamlContent: "apiVersion: aicr.run/v1alpha2\n",
			wantErr:     true,
			errContain:  "--no-cluster requires --snapshot",
			errAbsent:   "is required",
		},
		{
			name:        "legacy apiVersion is rejected",
			yamlContent: "kind: RecipeResult\napiVersion: aicr.nvidia.com/v1alpha1\n",
			wantErr:     true,
			errContain:  "apiVersion",
			errAbsent:   "--no-cluster requires --snapshot",
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
