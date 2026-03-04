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

package deployment

import (
	"os"
	"testing"

	"github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
)

// TestDeploymentConstraints is an integration test that runs inside the validator Job.
// It reads the recipe ConfigMap, extracts deployment constraints, and evaluates them
// using registered constraint validators.
func TestDeploymentConstraints(t *testing.T) {
	// Skip in short mode (unit tests)
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// This test runs inside the validator Job with mounted volumes
	// Get file paths from environment variables (set by the Job)
	recipePath := os.Getenv("AICR_RECIPE_PATH")
	snapshotPath := os.Getenv("AICR_SNAPSHOT_PATH")

	if recipePath == "" || snapshotPath == "" {
		t.Skip("Skipping: not running in validator Job environment")
	}

	// Get Kubernetes client
	clientset, _, err := client.GetKubeClient()
	if err != nil {
		t.Fatalf("Failed to get Kubernetes client: %v", err)
	}

	// Read recipe from mounted file
	recipeResult, err := serializer.FromFile[recipe.RecipeResult](recipePath)
	if err != nil {
		t.Fatalf("Failed to read recipe from %s: %v", recipePath, err)
	}

	// Read snapshot from mounted file
	snapshot, err := serializer.FromFile[snapshotter.Snapshot](snapshotPath)
	if err != nil {
		t.Fatalf("Failed to read snapshot from %s: %v", snapshotPath, err)
	}

	// Debug: log what we found
	t.Logf("Recipe loaded: Validation=%v", recipeResult.Validation != nil)
	if recipeResult.Validation != nil {
		t.Logf("Recipe Validation.Deployment=%v", recipeResult.Validation.Deployment != nil)
		if recipeResult.Validation.Deployment != nil {
			t.Logf("Recipe Validation.Deployment.Constraints count=%d", len(recipeResult.Validation.Deployment.Constraints))
		}
	}

	// Check if deployment phase has constraints
	if recipeResult.Validation == nil ||
		recipeResult.Validation.Deployment == nil ||
		len(recipeResult.Validation.Deployment.Constraints) == 0 {

		t.Log("No deployment constraints to evaluate")
		return
	}

	// Create validation context
	validationCtx := &checks.ValidationContext{
		Clientset: clientset,
		Snapshot:  snapshot,
	}

	// Evaluate each constraint
	for _, constraint := range recipeResult.Validation.Deployment.Constraints {
		t.Run(constraint.Name, func(t *testing.T) {
			// Get the registered validator for this constraint
			validator, ok := checks.GetConstraintValidator(constraint.Name)
			if !ok {
				t.Errorf("No validator registered for constraint: %s", constraint.Name)
				return
			}

			// Execute the validator
			actualValue, passed, err := validator.Func(validationCtx, constraint)
			if err != nil {
				t.Errorf("Constraint %s evaluation failed: %v", constraint.Name, err)
				return
			}

			if !passed {
				t.Errorf("Constraint %s failed: expected %q, got %q",
					constraint.Name, constraint.Value, actualValue)
			} else {
				t.Logf("Constraint %s passed: expected %q, got %q",
					constraint.Name, constraint.Value, actualValue)
			}
		})
	}
}
