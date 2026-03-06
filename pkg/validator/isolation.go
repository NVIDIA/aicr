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
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// resolveIsolated returns the effective isolated flag for a check or constraint.
// Precedence: individual > phase > top-level > default (false).
func resolveIsolated(individual, phase, topLevel *bool) bool {
	if individual != nil {
		return *individual
	}
	if phase != nil {
		return *phase
	}
	if topLevel != nil {
		return *topLevel
	}
	return false
}

// checkPartition holds checks and constraints partitioned by isolation mode.
type checkPartition struct {
	// SharedChecks are non-isolated checks to combine into one Job.
	SharedChecks []recipe.CheckRef
	// IsolatedChecks each get their own Job.
	IsolatedChecks []recipe.CheckRef
	// SharedConstraints are non-isolated constraints to combine into one Job.
	SharedConstraints []recipe.Constraint
	// IsolatedConstraints each get their own Job.
	IsolatedConstraints []recipe.Constraint
}

// partitionByIsolation splits a phase's checks and constraints into shared vs isolated groups.
func partitionByIsolation(phase *recipe.ValidationPhase, topLevel *bool) checkPartition {
	var result checkPartition
	if phase == nil {
		return result
	}

	for _, check := range phase.Checks {
		if resolveIsolated(check.Isolated, phase.Isolated, topLevel) {
			result.IsolatedChecks = append(result.IsolatedChecks, check)
		} else {
			result.SharedChecks = append(result.SharedChecks, check)
		}
	}

	for _, constraint := range phase.Constraints {
		if resolveIsolated(constraint.Isolated, phase.Isolated, topLevel) {
			result.IsolatedConstraints = append(result.IsolatedConstraints, constraint)
		} else {
			result.SharedConstraints = append(result.SharedConstraints, constraint)
		}
	}

	return result
}

// hasShared returns true if there are any shared checks or constraints.
func (p checkPartition) hasShared() bool {
	return len(p.SharedChecks) > 0 || len(p.SharedConstraints) > 0
}

// hasIsolated returns true if there are any isolated checks or constraints.
func (p checkPartition) hasIsolated() bool {
	return len(p.IsolatedChecks) > 0 || len(p.IsolatedConstraints) > 0
}
