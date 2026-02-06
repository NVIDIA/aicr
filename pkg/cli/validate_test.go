// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"testing"

	"github.com/NVIDIA/eidos/pkg/validator"
)

func TestParseValidationPhases(t *testing.T) {
	tests := []struct {
		name       string
		phaseStrs  []string
		wantPhases []validator.ValidationPhaseName
		wantErr    bool
		errContain string
	}{
		{
			name:       "empty defaults to readiness",
			phaseStrs:  []string{},
			wantPhases: []validator.ValidationPhaseName{validator.PhaseReadiness},
			wantErr:    false,
		},
		{
			name:       "single readiness phase",
			phaseStrs:  []string{"readiness"},
			wantPhases: []validator.ValidationPhaseName{validator.PhaseReadiness},
			wantErr:    false,
		},
		{
			name:       "single deployment phase",
			phaseStrs:  []string{"deployment"},
			wantPhases: []validator.ValidationPhaseName{validator.PhaseDeployment},
			wantErr:    false,
		},
		{
			name:       "single performance phase",
			phaseStrs:  []string{"performance"},
			wantPhases: []validator.ValidationPhaseName{validator.PhasePerformance},
			wantErr:    false,
		},
		{
			name:       "single conformance phase",
			phaseStrs:  []string{"conformance"},
			wantPhases: []validator.ValidationPhaseName{validator.PhaseConformance},
			wantErr:    false,
		},
		{
			name:       "all phases",
			phaseStrs:  []string{"all"},
			wantPhases: []validator.ValidationPhaseName{validator.PhaseAll},
			wantErr:    false,
		},
		{
			name:      "multiple phases",
			phaseStrs: []string{"readiness", "deployment", "conformance"},
			wantPhases: []validator.ValidationPhaseName{
				validator.PhaseReadiness,
				validator.PhaseDeployment,
				validator.PhaseConformance,
			},
			wantErr: false,
		},
		{
			name:      "out of order phases reordered to canonical order",
			phaseStrs: []string{"conformance", "readiness", "performance"},
			wantPhases: []validator.ValidationPhaseName{
				validator.PhaseReadiness,
				validator.PhasePerformance,
				validator.PhaseConformance,
			},
			wantErr: false,
		},
		{
			name:      "duplicate phases deduplicated",
			phaseStrs: []string{"readiness", "readiness", "deployment", "readiness"},
			wantPhases: []validator.ValidationPhaseName{
				validator.PhaseReadiness,
				validator.PhaseDeployment,
			},
			wantErr: false,
		},
		{
			name:      "duplicates with out of order",
			phaseStrs: []string{"performance", "readiness", "performance", "deployment"},
			wantPhases: []validator.ValidationPhaseName{
				validator.PhaseReadiness,
				validator.PhaseDeployment,
				validator.PhasePerformance,
			},
			wantErr: false,
		},
		{
			name:       "all with other phases returns just all",
			phaseStrs:  []string{"readiness", "all", "deployment"},
			wantPhases: []validator.ValidationPhaseName{validator.PhaseAll},
			wantErr:    false,
		},
		{
			name:       "invalid phase",
			phaseStrs:  []string{"invalid"},
			wantPhases: nil,
			wantErr:    true,
			errContain: "invalid phase",
		},
		{
			name:       "mixed valid and invalid",
			phaseStrs:  []string{"readiness", "bogus"},
			wantPhases: nil,
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
				if err == nil || !contains(err.Error(), tt.errContain) {
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

	// Verify command name
	if cmd.Name != "validate" {
		t.Errorf("command name = %q, want %q", cmd.Name, "validate")
	}

	// Verify required flags exist
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

	// Verify agent deployment flags exist
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
		"privileged",
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

// contains checks if s contains substr
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// hasFlag checks if a cli.Flag has the given name
func hasFlag(flag interface{ Names() []string }, name string) bool {
	for _, n := range flag.Names() {
		if n == name {
			return true
		}
	}
	return false
}
