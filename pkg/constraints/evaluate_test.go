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

package constraints

import (
	stderrors "errors"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

func evalSnapshot() *snapshotter.Snapshot {
	return &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeK8s,
				Subtypes: []measurement.Subtype{
					{
						Name: "server",
						Data: map[string]measurement.Reading{
							"version": measurement.Str("v1.33.5"),
						},
					},
				},
			},
			{
				Type: measurement.TypeOS,
				Subtypes: []measurement.Subtype{
					{
						Name: "release",
						Data: map[string]measurement.Reading{
							"ID": measurement.Str("ubuntu"),
						},
					},
				},
			},
		},
	}
}

// TestEvaluate exercises the package-level Evaluate entry point used by the
// recipe engine's constraint evaluator, pinning the error-code contract the
// fail-closed handling in pkg/recipe depends on (issue #1542, design 5.2):
// ErrCodeNotFound is the graceful-exclusion signal, and every other error
// must keep its own structured code — an ErrCodeInvalidRequest from the
// version parser must not be re-wrapped as ErrCodeInternal.
func TestEvaluate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		constraint recipe.Constraint
		wantPassed bool
		wantCode   errors.ErrorCode // "" means no error expected
	}{
		{
			name:       "satisfied version constraint passes",
			constraint: recipe.Constraint{Name: "K8s.server.version", Value: ">= 1.30"},
			wantPassed: true,
		},
		{
			name:       "unsatisfied version constraint fails cleanly",
			constraint: recipe.Constraint{Name: "K8s.server.version", Value: ">= 99.0"},
			wantPassed: false,
		},
		{
			name:       "missing measurement yields NotFound",
			constraint: recipe.Constraint{Name: "K8s.server.absent", Value: ">= 1.0"},
			wantCode:   errors.ErrCodeNotFound,
		},
		{
			name:       "invalid constraint path yields InvalidRequest",
			constraint: recipe.Constraint{Name: "not-a-path", Value: ">= 1.0"},
			wantCode:   errors.ErrCodeInvalidRequest,
		},
		{
			name:       "empty constraint expression yields InvalidRequest",
			constraint: recipe.Constraint{Name: "K8s.server.version", Value: ""},
			wantCode:   errors.ErrCodeInvalidRequest,
		},
		{
			name:       "unparseable actual version preserves InvalidRequest",
			constraint: recipe.Constraint{Name: "OS.release.ID", Value: ">= 1.2.3"},
			wantCode:   errors.ErrCodeInvalidRequest,
		},
	}

	snap := evalSnapshot()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := Evaluate(tt.constraint, snap)
			if tt.wantCode == "" {
				if result.Error != nil {
					t.Fatalf("unexpected error: %v", result.Error)
				}
				if result.Passed != tt.wantPassed {
					t.Errorf("Passed = %v, want %v", result.Passed, tt.wantPassed)
				}
				return
			}
			if result.Error == nil {
				t.Fatal("expected error, got nil")
			}
			if !stderrors.Is(result.Error, errors.New(tt.wantCode, "")) {
				t.Errorf("error code mismatch: want %s, got %v", tt.wantCode, result.Error)
			}
		})
	}
}
