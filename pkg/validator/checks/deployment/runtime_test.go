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
	"os"
	"testing"

	"github.com/NVIDIA/eidos/pkg/k8s/client"
	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/serializer"
	"github.com/NVIDIA/eidos/pkg/snapshotter"
	"github.com/NVIDIA/eidos/pkg/validator/checks"
)

// TestDeploymentConstraints is an integration test that runs inside the validator Job.
// It reads the recipe ConfigMap, extracts deployment constraints, and evaluates them
// using registered constraint validators.
func TestDeploymentConstraints(t *testing.T) {
	// Skip in short mode (unit tests)
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// This test runs inside the validator Job, so required ConfigMaps should be mounted
	// Get configuration from environment variables (set by the Job)
	recipeConfigMap := os.Getenv("RECIPE_CONFIGMAP")
	snapshotConfigMap := os.Getenv("SNAPSHOT_CONFIGMAP")
	namespace := os.Getenv("VALIDATION_NAMESPACE")

	if recipeConfigMap == "" || snapshotConfigMap == "" || namespace == "" {
		t.Skip("Skipping: not running in validator Job environment")
	}

	// Get Kubernetes client
	clientset, _, err := client.GetKubeClient()
	if err != nil {
		t.Fatalf("Failed to get Kubernetes client: %v", err)
	}

	// Read recipe from ConfigMap using cm:// URI
	recipeURI := "cm://" + namespace + "/" + recipeConfigMap
	recipeResult, err := serializer.FromFile[recipe.RecipeResult](recipeURI)
	if err != nil {
		t.Fatalf("Failed to read recipe from ConfigMap %s: %v", recipeURI, err)
	}

	// Read snapshot from ConfigMap using cm:// URI
	snapshotURI := "cm://" + namespace + "/" + snapshotConfigMap
	snapshot, err := serializer.FromFile[snapshotter.Snapshot](snapshotURI)
	if err != nil {
		t.Fatalf("Failed to read snapshot from ConfigMap %s: %v", snapshotURI, err)
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
