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

package validations

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// init auto-registers validation functions in this package.
// This allows the registry to discover validation functions automatically.
func init() {
	// Register all validation functions in this package
	// This is called automatically when the package is imported
	registerCheck("CheckWorkloadSelectorMissing", CheckWorkloadSelectorMissing)
	registerCheck("CheckAcceleratedSelectorMissing", CheckAcceleratedSelectorMissing)
}

// registerCheck is a helper to register validation functions from checks.go.
// It's called from init() to auto-register functions.
func registerCheck(name string, fn ValidationFunc) {
	// Use Register which will initialize the registry if needed
	Register(name, fn)
}

// CheckWorkloadSelectorMissing checks if workload-selector is missing when conditions are met.
// This is a generic check that can be used by any component.
func CheckWorkloadSelectorMissing(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
	if bundlerConfig == nil {
		return nil, nil
	}

	// Check if component exists in recipe
	hasComponent := false
	for _, ref := range recipeResult.ComponentRefs {
		if ref.Name == componentName {
			hasComponent = true
			break
		}
	}

	if !hasComponent {
		return nil, nil
	}

	// Check conditions (e.g., intent: training)
	if !checkConditions(recipeResult, conditions) {
		return nil, nil
	}

	// Check if workload-selector is not set
	selector := bundlerConfig.WorkloadSelector()
	if len(selector) == 0 {
		baseMsg := fmt.Sprintf("%s is enabled but --workload-selector is not set", componentName)
		slog.Warn(baseMsg,
			"component", componentName,
			"conditions", conditions,
		)
		return []string{baseMsg}, nil
	}

	return nil, nil
}

// CheckAcceleratedSelectorMissing checks if accelerated-node-selector is missing when conditions are met.
// This is a generic check that can be used by any component.
func CheckAcceleratedSelectorMissing(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
	if bundlerConfig == nil {
		return nil, nil
	}

	// Check if component exists in recipe
	hasComponent := false
	for _, ref := range recipeResult.ComponentRefs {
		if ref.Name == componentName {
			hasComponent = true
			break
		}
	}

	if !hasComponent {
		return nil, nil
	}

	// Check conditions (e.g., intent: [training, inference])
	if !checkConditions(recipeResult, conditions) {
		return nil, nil
	}

	// Check if accelerated-node-selector is not set
	selector := bundlerConfig.AcceleratedNodeSelector()
	if len(selector) == 0 {
		baseMsg := fmt.Sprintf("%s is enabled but --accelerated-node-selector is not set", componentName)
		slog.Warn(baseMsg,
			"component", componentName,
			"conditions", conditions,
		)
		return []string{baseMsg}, nil
	}

	return nil, nil
}

// checkConditions verifies that the recipe result meets the specified conditions.
// Conditions are arrays of strings for OR matching (single element arrays are equivalent to single values).
// Reuses matching logic from recipe/criteria.go.
func checkConditions(recipeResult *recipe.RecipeResult, conditions map[string][]string) bool {
	if len(conditions) == 0 {
		return true
	}

	if recipeResult.Criteria == nil {
		return false
	}

	for key, expectedValues := range conditions {
		var actualValue string

		// Get actual value from criteria
		switch key {
		case "intent":
			actualValue = string(recipeResult.Criteria.Intent)
		case "service":
			actualValue = string(recipeResult.Criteria.Service)
		case "accelerator":
			actualValue = string(recipeResult.Criteria.Accelerator)
		case "os":
			actualValue = string(recipeResult.Criteria.OS)
		case "platform":
			actualValue = string(recipeResult.Criteria.Platform)
		default:
			// Unknown condition key, skip
			continue
		}

		// Check if actualValue matches any of the expected values (OR matching)
		found := false
		for _, expectedStr := range expectedValues {
			// Use matchesCriteriaField for consistent matching logic
			if matchesCriteriaField(actualValue, expectedStr) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}

	return true
}

// matchesCriteriaField implements matching for a single criteria field.
// Reuses the logic from recipe/criteria.go for consistency.
// Returns true if the actual value matches the expected value.
// For validation conditions, we use simple equality matching (not asymmetric like recipe matching).
func matchesCriteriaField(actualValue, expectedValue string) bool {
	// Treat empty as "any" for consistency with criteria matching
	expectedIsAny := expectedValue == "" || expectedValue == "any"

	// If expected is "any", it matches any actual value
	if expectedIsAny {
		return true
	}

	// Expected has a specific value - actual must match exactly
	return actualValue == expectedValue
}
