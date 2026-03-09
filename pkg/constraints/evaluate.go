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

// Package constraints provides constraint parsing, extraction, and evaluation
// utilities for comparing recipe constraints against snapshot measurements.
package constraints

import (
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// EvalResult represents the result of evaluating a single constraint.
type EvalResult struct {
	// Passed indicates if the constraint was satisfied.
	Passed bool

	// Actual is the actual value extracted from the snapshot.
	Actual string

	// Error contains the error if evaluation failed.
	Error error
}

// Evaluate evaluates a single constraint against a snapshot.
// Used by the recipe package to filter overlays based on constraint
// evaluation during snapshot-based recipe generation.
func Evaluate(constraint recipe.Constraint, snap *snapshotter.Snapshot) EvalResult {
	result := EvalResult{}

	path, err := ParseConstraintPath(constraint.Name)
	if err != nil {
		result.Error = errors.Wrap(errors.ErrCodeInvalidRequest, "invalid constraint path", err)
		return result
	}

	actual, err := path.ExtractValue(snap)
	if err != nil {
		result.Error = errors.Wrap(errors.ErrCodeNotFound, "value not found in snapshot", err)
		return result
	}
	result.Actual = actual

	parsed, err := ParseConstraintExpression(constraint.Value)
	if err != nil {
		result.Error = errors.Wrap(errors.ErrCodeInvalidRequest, "invalid constraint expression", err)
		return result
	}

	passed, err := parsed.Evaluate(actual)
	if err != nil {
		result.Error = errors.Wrap(errors.ErrCodeInternal, "evaluation failed", err)
		return result
	}

	result.Passed = passed
	return result
}
