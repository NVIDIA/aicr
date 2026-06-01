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

package validator

import (
	"strings"
	"testing"
)

func TestParsePhaseSelection(t *testing.T) {
	tests := []struct {
		name       string
		phaseStrs  []string
		wantPhases []Phase
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
			wantPhases: []Phase{PhaseDeployment},
		},
		{
			name:       "single performance phase",
			phaseStrs:  []string{"performance"},
			wantPhases: []Phase{PhasePerformance},
		},
		{
			name:       "single conformance phase",
			phaseStrs:  []string{"conformance"},
			wantPhases: []Phase{PhaseConformance},
		},
		{
			name:       "multiple phases",
			phaseStrs:  []string{"deployment", "conformance"},
			wantPhases: []Phase{PhaseDeployment, PhaseConformance},
		},
		{
			name:       "duplicate phases deduplicated",
			phaseStrs:  []string{"deployment", "deployment", "conformance"},
			wantPhases: []Phase{PhaseDeployment, PhaseConformance},
		},
		{
			name:       "all repeated returns nil",
			phaseStrs:  []string{"all", "all"},
			wantPhases: nil,
		},
		{
			name:       "all combined with specific phase is rejected",
			phaseStrs:  []string{"deployment", "all", "conformance"},
			wantErr:    true,
			errContain: "cannot be combined",
		},
		{
			name:       "invalid phase",
			phaseStrs:  []string{"invalid"},
			wantErr:    true,
			errContain: "invalid phase",
		},
		{
			name:       "invalid phase is caught even when all is also present",
			phaseStrs:  []string{"all", "garbage"},
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
			got, err := ParsePhaseSelection(tt.phaseStrs)

			if (err != nil) != tt.wantErr {
				t.Errorf("ParsePhaseSelection() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && tt.errContain != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("ParsePhaseSelection() error = %v, want error containing %q",
						err, tt.errContain)
				}
				return
			}

			if len(got) != len(tt.wantPhases) {
				t.Errorf("ParsePhaseSelection() got %d phases, want %d",
					len(got), len(tt.wantPhases))
				return
			}

			for i, phase := range got {
				if phase != tt.wantPhases[i] {
					t.Errorf("ParsePhaseSelection() phase[%d] = %v, want %v",
						i, phase, tt.wantPhases[i])
				}
			}
		})
	}
}
