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
	"slices"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/NVIDIA/aicr/pkg/validator"
)

func TestParseValidationPhases(t *testing.T) {
	tests := []struct {
		name       string
		phaseStrs  []string
		wantPhases []validator.Phase
		wantErr    bool
		errContain string
	}{
		{
			name:       "empty defaults to all (nil)",
			phaseStrs:  []string{},
			wantPhases: nil,
		},
		{
			name:       "all returns nil",
			phaseStrs:  []string{"all"},
			wantPhases: nil,
		},
		{
			name:       "single deployment phase",
			phaseStrs:  []string{"deployment"},
			wantPhases: []validator.Phase{validator.PhaseDeployment},
		},
		{
			name:       "single performance phase",
			phaseStrs:  []string{"performance"},
			wantPhases: []validator.Phase{validator.PhasePerformance},
		},
		{
			name:       "single conformance phase",
			phaseStrs:  []string{"conformance"},
			wantPhases: []validator.Phase{validator.PhaseConformance},
		},
		{
			name:      "multiple phases",
			phaseStrs: []string{"deployment", "conformance"},
			wantPhases: []validator.Phase{
				validator.PhaseDeployment,
				validator.PhaseConformance,
			},
		},
		{
			name:      "duplicate phases deduplicated",
			phaseStrs: []string{"deployment", "deployment", "conformance"},
			wantPhases: []validator.Phase{
				validator.PhaseDeployment,
				validator.PhaseConformance,
			},
		},
		{
			name:       "all with other phases returns nil",
			phaseStrs:  []string{"deployment", "all", "conformance"},
			wantPhases: nil,
		},
		{
			name:       "invalid phase",
			phaseStrs:  []string{"invalid"},
			wantErr:    true,
			errContain: "invalid phase",
		},
		{
			name:       "readiness is invalid (not supported in v2)",
			phaseStrs:  []string{"readiness"},
			wantErr:    true,
			errContain: "invalid phase",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseValidationPhases(tt.phaseStrs)

			if (err != nil) != tt.wantErr {
				t.Errorf("parseValidationPhases() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && tt.errContain != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("parseValidationPhases() error = %v, want error containing %q", err, tt.errContain)
				}
				return
			}

			if len(got) != len(tt.wantPhases) {
				t.Errorf("parseValidationPhases() got %d phases, want %d", len(got), len(tt.wantPhases))
				return
			}

			for i, phase := range got {
				if phase != tt.wantPhases[i] {
					t.Errorf("parseValidationPhases() phase[%d] = %v, want %v", i, phase, tt.wantPhases[i])
				}
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
		"cleanup",
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
