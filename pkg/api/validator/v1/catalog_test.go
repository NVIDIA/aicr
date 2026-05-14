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

package v1

import (
	"testing"
)

func TestFilterEntriesByValidation_NilValidation(t *testing.T) {
	entries := []ValidatorEntry{
		{Name: "v1", Phase: "deployment"},
		{Name: "v2", Phase: "deployment"},
	}
	got := FilterEntriesByValidation(entries, PhaseDeployment, nil)
	if len(got) != 0 {
		t.Errorf("FilterEntriesByValidation(nil validation) returned %d entries, want 0", len(got))
	}
}

func TestFilterEntriesByValidation_NilPhaseConfig(t *testing.T) {
	entries := []ValidatorEntry{
		{Name: "v1", Phase: "deployment"},
	}
	validationInput := &ValidationInput{
		Config: ValidationConfig{
			Deployment: nil, // No deployment config
		},
	}
	got := FilterEntriesByValidation(entries, PhaseDeployment, validationInput)
	if len(got) != 0 {
		t.Errorf("FilterEntriesByValidation(nil phase config) returned %d entries, want 0", len(got))
	}
}

func TestFilterEntriesByValidation_EmptyChecks(t *testing.T) {
	entries := []ValidatorEntry{
		{Name: "v1", Phase: "deployment"},
	}
	validationInput := &ValidationInput{
		Config: ValidationConfig{
			Deployment: &ValidationPhase{
				Checks: []string{}, // Empty checks list
			},
		},
	}
	got := FilterEntriesByValidation(entries, PhaseDeployment, validationInput)
	if len(got) != 0 {
		t.Errorf("FilterEntriesByValidation(empty checks) returned %d entries, want 0", len(got))
	}
}

func TestFilterEntriesByValidation_SingleCheck(t *testing.T) {
	entries := []ValidatorEntry{
		{Name: "operator-health", Phase: "deployment"},
		{Name: "expected-resources", Phase: "deployment"},
		{Name: "gpu-operator-version", Phase: "deployment"},
	}
	validationInput := &ValidationInput{
		Config: ValidationConfig{
			Deployment: &ValidationPhase{
				Checks: []string{"operator-health"},
			},
		},
	}
	got := FilterEntriesByValidation(entries, PhaseDeployment, validationInput)
	if len(got) != 1 {
		t.Errorf("FilterEntriesByValidation() returned %d entries, want 1", len(got))
	}
	if len(got) > 0 && got[0].Name != "operator-health" {
		t.Errorf("FilterEntriesByValidation() returned %q, want %q", got[0].Name, "operator-health")
	}
}

func TestFilterEntriesByValidation_MultipleChecks(t *testing.T) {
	entries := []ValidatorEntry{
		{Name: "operator-health", Phase: "deployment"},
		{Name: "expected-resources", Phase: "deployment"},
		{Name: "gpu-operator-version", Phase: "deployment"},
		{Name: "check-nvidia-smi", Phase: "deployment"},
	}
	validationInput := &ValidationInput{
		Config: ValidationConfig{
			Deployment: &ValidationPhase{
				Checks: []string{"operator-health", "expected-resources"},
			},
		},
	}
	got := FilterEntriesByValidation(entries, PhaseDeployment, validationInput)
	if len(got) != 2 {
		t.Errorf("FilterEntriesByValidation() returned %d entries, want 2", len(got))
	}
	names := make(map[string]bool)
	for _, entry := range got {
		names[entry.Name] = true
	}
	if !names["operator-health"] || !names["expected-resources"] {
		t.Errorf("FilterEntriesByValidation() missing expected entries")
	}
}

func TestFilterEntriesByValidation_AllPhases(t *testing.T) {
	tests := []struct {
		name            string
		phase           Phase
		entries         []ValidatorEntry
		validationInput *ValidationInput
		expected        int
		names           []string
	}{
		{
			name:  "deployment phase filters correctly",
			phase: PhaseDeployment,
			entries: []ValidatorEntry{
				{Name: "operator-health", Phase: "deployment"},
				{Name: "expected-resources", Phase: "deployment"},
			},
			validationInput: &ValidationInput{
				Config: ValidationConfig{
					Deployment: &ValidationPhase{
						Checks: []string{"operator-health"},
					},
				},
			},
			expected: 1,
			names:    []string{"operator-health"},
		},
		{
			name:  "performance phase filters correctly",
			phase: PhasePerformance,
			entries: []ValidatorEntry{
				{Name: "nccl-all-reduce-bw", Phase: "performance"},
				{Name: "inference-perf", Phase: "performance"},
			},
			validationInput: &ValidationInput{
				Config: ValidationConfig{
					Performance: &ValidationPhase{
						Checks: []string{"nccl-all-reduce-bw"},
					},
				},
			},
			expected: 1,
			names:    []string{"nccl-all-reduce-bw"},
		},
		{
			name:  "conformance phase filters correctly",
			phase: PhaseConformance,
			entries: []ValidatorEntry{
				{Name: "dra-support", Phase: "conformance"},
				{Name: "gang-scheduling", Phase: "conformance"},
			},
			validationInput: &ValidationInput{
				Config: ValidationConfig{
					Conformance: &ValidationPhase{
						Checks: []string{"dra-support"},
					},
				},
			},
			expected: 1,
			names:    []string{"dra-support"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FilterEntriesByValidation(tt.entries, tt.phase, tt.validationInput)
			if len(got) != tt.expected {
				t.Errorf("FilterEntriesByValidation() returned %d entries, want %d", len(got), tt.expected)
			}
			if tt.names != nil {
				for i, name := range tt.names {
					if i >= len(got) {
						t.Errorf("FilterEntriesByValidation() missing entry[%d], want %q (got only %d entries)", i, name, len(got))
					} else if got[i].Name != name {
						t.Errorf("FilterEntriesByValidation() entry[%d] = %q, want %q", i, got[i].Name, name)
					}
				}
			}
		})
	}
}

func TestFilterEntriesByValidation_NonExistentCheck(t *testing.T) {
	entries := []ValidatorEntry{
		{Name: "operator-health", Phase: "deployment"},
		{Name: "expected-resources", Phase: "deployment"},
	}
	validationInput := &ValidationInput{
		Config: ValidationConfig{
			Deployment: &ValidationPhase{
				Checks: []string{"non-existent-check"},
			},
		},
	}
	got := FilterEntriesByValidation(entries, PhaseDeployment, validationInput)
	if len(got) != 0 {
		t.Errorf("FilterEntriesByValidation(non-existent check) returned %d entries, want 0", len(got))
	}
}
