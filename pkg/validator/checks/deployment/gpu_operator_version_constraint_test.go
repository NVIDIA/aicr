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

package deployment

import (
	"testing"

	"github.com/NVIDIA/eidos/pkg/validator/checks"
)

// TestGPUOperatorVersion validates the GPU operator version constraint.
// This integration test runs inside validator Jobs and invokes the validator.
func TestGPUOperatorVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Load Job environment
	runner, err := checks.NewTestRunner(t)
	if err != nil {
		t.Skipf("Not in Job environment: %v", err)
	}
	defer runner.Cancel()

	// Get constraint from recipe
	constraint := runner.GetConstraint("deployment", "Deployment.gpu-operator.version")
	if constraint == nil {
		t.Skip("Constraint Deployment.gpu-operator.version not defined in recipe")
	}

	t.Logf("Validating constraint: %s = %s", constraint.Name, constraint.Value)

	// Run the validator
	ctx := runner.Context()
	actual, passed, err := ValidateGPUOperatorVersion(ctx, *constraint)
	if err != nil {
		t.Fatalf("Validation failed: %v", err)
	}

	t.Logf("CONSTRAINT_RESULT: name=%s expected=%s actual=%s passed=%v",
		constraint.Name, constraint.Value, actual, passed)

	if !passed {
		t.Errorf("Constraint not satisfied: expected %s, got %s", constraint.Value, actual)
	} else {
		t.Logf("✓ Constraint satisfied: %s = %s", constraint.Name, actual)
	}
}
