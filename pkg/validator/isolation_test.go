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

package validator

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

func boolPtr(b bool) *bool { return &b }

func TestResolveIsolated(t *testing.T) {
	tests := []struct {
		name       string
		individual *bool
		phase      *bool
		topLevel   *bool
		want       bool
	}{
		{
			name: "all nil defaults to false",
			want: false,
		},
		{
			name:     "top-level true",
			topLevel: boolPtr(true),
			want:     true,
		},
		{
			name:     "top-level false",
			topLevel: boolPtr(false),
			want:     false,
		},
		{
			name:     "phase overrides top-level",
			phase:    boolPtr(false),
			topLevel: boolPtr(true),
			want:     false,
		},
		{
			name:  "phase true with nil top-level",
			phase: boolPtr(true),
			want:  true,
		},
		{
			name:       "individual overrides phase",
			individual: boolPtr(true),
			phase:      boolPtr(false),
			topLevel:   boolPtr(false),
			want:       true,
		},
		{
			name:       "individual false overrides phase true",
			individual: boolPtr(false),
			phase:      boolPtr(true),
			topLevel:   boolPtr(true),
			want:       false,
		},
		{
			name:       "individual overrides all",
			individual: boolPtr(true),
			phase:      boolPtr(false),
			topLevel:   boolPtr(false),
			want:       true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveIsolated(tt.individual, tt.phase, tt.topLevel)
			if got != tt.want {
				t.Errorf("resolveIsolated() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPartitionByIsolation(t *testing.T) {
	tests := []struct {
		name                    string
		phase                   *recipe.ValidationPhase
		topLevel                *bool
		wantSharedChecks        int
		wantIsolatedChecks      int
		wantSharedConstraints   int
		wantIsolatedConstraints int
	}{
		{
			name:  "nil phase returns empty partition",
			phase: nil,
		},
		{
			name: "all shared by default",
			phase: &recipe.ValidationPhase{
				Checks: []recipe.CheckRef{
					{Name: "check-a"},
					{Name: "check-b"},
				},
				Constraints: []recipe.Constraint{
					{Name: "Deployment.x.version", Value: ">= v1.0"},
				},
			},
			wantSharedChecks:      2,
			wantSharedConstraints: 1,
		},
		{
			name:     "top-level isolated makes all isolated",
			topLevel: boolPtr(true),
			phase: &recipe.ValidationPhase{
				Checks: []recipe.CheckRef{
					{Name: "check-a"},
					{Name: "check-b"},
				},
				Constraints: []recipe.Constraint{
					{Name: "Deployment.x.version", Value: ">= v1.0"},
				},
			},
			wantIsolatedChecks:      2,
			wantIsolatedConstraints: 1,
		},
		{
			name:     "phase-level isolated overrides top-level",
			topLevel: boolPtr(false),
			phase: &recipe.ValidationPhase{
				Isolated: boolPtr(true),
				Checks: []recipe.CheckRef{
					{Name: "check-a"},
				},
				Constraints: []recipe.Constraint{
					{Name: "c1", Value: "v1"},
				},
			},
			wantIsolatedChecks:      1,
			wantIsolatedConstraints: 1,
		},
		{
			name: "individual check overrides phase",
			phase: &recipe.ValidationPhase{
				Isolated: boolPtr(true),
				Checks: []recipe.CheckRef{
					{Name: "shared-check", Isolated: boolPtr(false)},
					{Name: "isolated-check"},
				},
				Constraints: []recipe.Constraint{
					{Name: "shared-constraint", Value: "v1", Isolated: boolPtr(false)},
					{Name: "isolated-constraint", Value: "v2"},
				},
			},
			wantSharedChecks:        1,
			wantIsolatedChecks:      1,
			wantSharedConstraints:   1,
			wantIsolatedConstraints: 1,
		},
		{
			name: "mixed isolation within phase",
			phase: &recipe.ValidationPhase{
				Checks: []recipe.CheckRef{
					{Name: "default-check"},
					{Name: "isolated-check", Isolated: boolPtr(true)},
					{Name: "explicit-shared", Isolated: boolPtr(false)},
				},
				Constraints: []recipe.Constraint{
					{Name: "default-constraint", Value: "v1"},
					{Name: "isolated-constraint", Value: "v2", Isolated: boolPtr(true)},
				},
			},
			wantSharedChecks:        2,
			wantIsolatedChecks:      1,
			wantSharedConstraints:   1,
			wantIsolatedConstraints: 1,
		},
		{
			name: "empty checks and constraints",
			phase: &recipe.ValidationPhase{
				Checks:      []recipe.CheckRef{},
				Constraints: []recipe.Constraint{},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := partitionByIsolation(tt.phase, tt.topLevel)
			if len(got.SharedChecks) != tt.wantSharedChecks {
				t.Errorf("SharedChecks = %d, want %d", len(got.SharedChecks), tt.wantSharedChecks)
			}
			if len(got.IsolatedChecks) != tt.wantIsolatedChecks {
				t.Errorf("IsolatedChecks = %d, want %d", len(got.IsolatedChecks), tt.wantIsolatedChecks)
			}
			if len(got.SharedConstraints) != tt.wantSharedConstraints {
				t.Errorf("SharedConstraints = %d, want %d", len(got.SharedConstraints), tt.wantSharedConstraints)
			}
			if len(got.IsolatedConstraints) != tt.wantIsolatedConstraints {
				t.Errorf("IsolatedConstraints = %d, want %d", len(got.IsolatedConstraints), tt.wantIsolatedConstraints)
			}
		})
	}
}

func TestCheckPartitionHelpers(t *testing.T) {
	tests := []struct {
		name         string
		partition    checkPartition
		wantShared   bool
		wantIsolated bool
	}{
		{
			name:      "empty partition",
			partition: checkPartition{},
		},
		{
			name: "shared checks only",
			partition: checkPartition{
				SharedChecks: []recipe.CheckRef{{Name: "a"}},
			},
			wantShared: true,
		},
		{
			name: "shared constraints only",
			partition: checkPartition{
				SharedConstraints: []recipe.Constraint{{Name: "c1", Value: "v1"}},
			},
			wantShared: true,
		},
		{
			name: "isolated checks only",
			partition: checkPartition{
				IsolatedChecks: []recipe.CheckRef{{Name: "a"}},
			},
			wantIsolated: true,
		},
		{
			name: "isolated constraints only",
			partition: checkPartition{
				IsolatedConstraints: []recipe.Constraint{{Name: "c1", Value: "v1"}},
			},
			wantIsolated: true,
		},
		{
			name: "both shared and isolated",
			partition: checkPartition{
				SharedChecks:        []recipe.CheckRef{{Name: "a"}},
				IsolatedConstraints: []recipe.Constraint{{Name: "c1", Value: "v1"}},
			},
			wantShared:   true,
			wantIsolated: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.partition.hasShared(); got != tt.wantShared {
				t.Errorf("hasShared() = %v, want %v", got, tt.wantShared)
			}
			if got := tt.partition.hasIsolated(); got != tt.wantIsolated {
				t.Errorf("hasIsolated() = %v, want %v", got, tt.wantIsolated)
			}
		})
	}
}
