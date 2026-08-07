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

// TestEvaluate_SystemDUnitPath closes the loop on the SystemD split rule at
// the level the defect actually bites.
//
// pkg/measurement proves parse, validate, and extract agree. This proves the
// consequence: a correctly spelled SystemD constraint now EVALUATES, rather
// than returning ErrCodeNotFound. That distinction is the whole issue —
// evaluateOverlayConstraints reads NotFound as "reading absent from this
// snapshot" and silently excludes the overlay, so a path that cannot resolve
// produces a recipe that quietly skipped its gate (#1783).
//
// Both directions are asserted: a satisfied constraint passes with the actual
// reading, and an unsatisfied one fails CLEANLY (Passed=false, Error=nil)
// rather than erroring — a clean failure is a real signal, a NotFound is not.
func TestEvaluate_SystemDUnitPath(t *testing.T) {
	t.Parallel()

	snap := &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeSystemD,
				Subtypes: []measurement.Subtype{
					{
						Name: "containerd.service",
						Data: map[string]measurement.Reading{
							"ActiveState": measurement.Str("active"),
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name       string
		constraint recipe.Constraint
		wantPassed bool
		wantActual string
	}{
		{
			name:       "satisfied unit property passes",
			constraint: recipe.Constraint{Name: "SystemD.containerd.service.ActiveState", Value: "active"},
			wantPassed: true,
			wantActual: "active",
		},
		{
			name:       "unsatisfied unit property fails cleanly, not NotFound",
			constraint: recipe.Constraint{Name: "SystemD.containerd.service.ActiveState", Value: "inactive"},
			wantPassed: false,
			wantActual: "active",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := Evaluate(tt.constraint, snap)
			if result.Error != nil {
				t.Fatalf("Evaluate() error = %v; a resolvable SystemD path must not degrade to an error",
					result.Error)
			}
			if result.Passed != tt.wantPassed {
				t.Errorf("Passed = %v, want %v", result.Passed, tt.wantPassed)
			}
			if result.Actual != tt.wantActual {
				t.Errorf("Actual = %q, want %q", result.Actual, tt.wantActual)
			}
		})
	}
}

// TestEvaluate_NilSnapshot pins both halves of the nil-snapshot contract.
//
// Path.Extract takes []*measurement.Measurement rather than a *Snapshot
// (issue #1783), so the guard that used to live inside ExtractValue now sits in
// Evaluate — without it, snap.Measurements panics.
//
// The two codes below are deliberately different and must stay that way. The
// node-set form handles a nil snapshot in findLabelSubtype and reports
// NotFound; the scalar path reports InvalidRequest. pkg/recipe's
// evaluateOverlayConstraints branches on exactly that distinction — NotFound is
// graceful exclusion, anything else fails resolution closed — so hoisting a
// single guard to the top of Evaluate would silently convert an excluded
// overlay into a failed resolve.
func TestEvaluate_NilSnapshot(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		constraint recipe.Constraint
		wantCode   errors.ErrorCode
	}{
		{
			name:       "scalar path yields InvalidRequest",
			constraint: recipe.Constraint{Name: "K8s.server.version", Value: ">= 1.30"},
			wantCode:   errors.ErrCodeInvalidRequest,
		},
		{
			name:       "node-set path yields NotFound",
			constraint: recipe.Constraint{Name: GPUNodesLabelConstraintName, Value: "!nvidia.com/gpu.present"},
			wantCode:   errors.ErrCodeNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// Must not panic.
			result := Evaluate(tt.constraint, nil)
			if result.Error == nil {
				t.Fatal("expected error, got nil")
			}
			if !stderrors.Is(result.Error, errors.New(tt.wantCode, "")) {
				t.Errorf("error code mismatch: want %s, got %v", tt.wantCode, result.Error)
			}
			if result.Passed {
				t.Error("Passed = true on a nil snapshot, want false")
			}
		})
	}
}
