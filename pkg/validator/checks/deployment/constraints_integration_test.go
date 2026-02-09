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
// This integration test runs inside validator Jobs and contains the actual validation logic.
// It is excluded from local test runs via the -short flag.
func TestGPUOperatorVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Load Job environment
	runner, err := checks.NewTestRunner(t)
	if err != nil {
		t.Skipf("Not in Job environment: %v", err)
	}
	defer runner.Cancel() // Clean up context when test completes

	// Get constraint from recipe
	constraint := runner.GetConstraint("deployment", "Deployment.gpu-operator.version")
	if constraint == nil {
		t.Skip("Constraint Deployment.gpu-operator.version not defined in recipe")
	}

	t.Logf("Validating constraint: %s = %s", constraint.Name, constraint.Value)

	// Get GPU operator version from cluster
	ctx := runner.Context()
	version, err := getGPUOperatorVersion(ctx.Context, ctx.Clientset)
	if err != nil {
		t.Fatalf("Failed to get GPU operator version: %v", err)
	}

	t.Logf("Detected GPU operator version: %s", version)

	// Evaluate constraint
	passed, err := evaluateVersionConstraint(version, constraint.Value)
	if err != nil {
		t.Fatalf("Failed to evaluate version constraint: %v", err)
	}

	// Output structured constraint result for parsing
	// Format: CONSTRAINT_RESULT: name=<name> expected=<expected> actual=<actual> passed=<bool>
	t.Logf("CONSTRAINT_RESULT: name=%s expected=%s actual=%s passed=%t",
		constraint.Name, constraint.Value, version, passed)

	if !passed {
		t.Errorf("GPU operator version %s does not satisfy constraint %s", version, constraint.Value)
	} else {
		t.Logf("✓ GPU operator version %s satisfies constraint %s", version, constraint.Value)
	}
}
