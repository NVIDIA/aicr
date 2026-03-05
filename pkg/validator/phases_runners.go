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

//nolint:dupl // Phase validators have similar structure by design

package validator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/agent"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
)

// validateReadiness validates the readiness phase.
// Evaluates recipe constraints inline against the snapshot — no cluster access needed.
//
//nolint:unparam // error return may be used in future implementations
func (v *Validator) validateReadiness(
	_ context.Context,
	recipeResult *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
) (*ValidationResult, error) {

	start := time.Now()
	slog.Info("running readiness validation phase")

	result := NewValidationResult()
	phaseResult := &PhaseResult{
		Status:      ValidationStatusPass,
		Constraints: []ConstraintValidation{},
	}

	// Evaluate recipe-level constraints (spec.constraints) inline
	for _, constraint := range recipeResult.Constraints {
		cv := v.evaluateConstraint(constraint, snap)
		phaseResult.Constraints = append(phaseResult.Constraints, cv)
	}

	// Determine phase status based on constraints
	failedCount := 0
	passedCount := 0
	for _, cv := range phaseResult.Constraints {
		switch cv.Status {
		case ConstraintStatusFailed:
			failedCount++
		case ConstraintStatusPassed:
			passedCount++
		case ConstraintStatusSkipped:
			// Skipped constraints don't affect pass/fail count
		}
	}

	if failedCount > 0 {
		phaseResult.Status = ValidationStatusFail
	} else if len(phaseResult.Constraints) > 0 {
		phaseResult.Status = ValidationStatusPass
	}

	phaseResult.Duration = time.Since(start)
	result.Phases[string(PhaseReadiness)] = phaseResult

	// Update summary
	result.Summary.Status = phaseResult.Status
	result.Summary.Passed = passedCount
	result.Summary.Failed = failedCount
	result.Summary.Total = len(phaseResult.Constraints)
	result.Summary.Duration = phaseResult.Duration

	slog.Info("readiness validation completed",
		"status", phaseResult.Status,
		"constraints", len(phaseResult.Constraints),
		"duration", phaseResult.Duration)

	return result, nil
}

// validateDeployment validates deployment phase.
// Runs checks as Kubernetes Jobs to verify deployment constraints.
//
//nolint:unparam,dupl // error always nil; phase validation methods have similar structure by design
func (v *Validator) validateDeployment(
	ctx context.Context,
	recipeResult *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
) (*ValidationResult, error) {
	//nolint:dupl // Phase validation methods have similar structure by design
	start := time.Now()
	slog.Info("running deployment validation phase")

	result := NewValidationResult()
	phaseResult := &PhaseResult{
		Status:      ValidationStatusPass,
		Constraints: []ConstraintValidation{},
		Checks:      []CheckResult{},
	}

	// Check if deployment phase is configured
	if recipeResult.Validation == nil || recipeResult.Validation.Deployment == nil {
		phaseResult.Status = ValidationStatusSkipped
		phaseResult.Reason = "deployment phase not configured in recipe"
	} else { //nolint:gocritic // elseif not applicable, multiple statements in else block
		// NOTE: Deployment phase constraints require live cluster access.
		// They are NOT evaluated inline like readiness constraints.
		// Instead, they should be registered as constraint validators in the checks registry
		// and will be evaluated inside the validation Job with cluster access.
		// See pkg/validator/checks/deployment/constraints.go for examples.

		// Run checks and evaluate constraints as Kubernetes Jobs
		// Note: RBAC resources must be created by the caller before invoking this function.
		// For multi-phase validation, validateAll() manages RBAC lifecycle.
		// For single-phase validation, the CLI/API should call agent.EnsureRBAC() first.
		if len(recipeResult.Validation.Deployment.Checks) > 0 || len(recipeResult.Validation.Deployment.Constraints) > 0 {
			if v.NoCluster {
				slog.Info("no-cluster mode enabled, skipping cluster check execution for deployment phase")
				// Create stub check results for each check in the recipe
				for _, checkName := range recipeResult.Validation.Deployment.Checks {
					phaseResult.Checks = append(phaseResult.Checks, CheckResult{
						Name:   checkName,
						Status: ValidationStatusSkipped,
						Reason: "skipped - no-cluster mode (test mode)",
					})
				}
			} else {
				clientset, _, err := k8sclient.GetKubeClient()
				if err != nil {
					// If Kubernetes is not available (e.g., running in test mode), skip check execution
					slog.Warn("Kubernetes client unavailable, skipping check execution",
						"error", err,
						"checks", len(recipeResult.Validation.Deployment.Checks))
					// Add skeleton check result
					phaseResult.Checks = append(phaseResult.Checks, CheckResult{
						Name:   "deployment",
						Status: ValidationStatusPass,
						Reason: "skipped - Kubernetes unavailable (test mode)",
					})
				} else {
					// ConfigMap names (created once per validation run by validateAll)
					snapshotCMName := fmt.Sprintf("aicr-snapshot-%s", v.RunID)
					recipeCMName := fmt.Sprintf("aicr-recipe-%s", v.RunID)

					// Validate that all recipe constraints/checks are registered (logs warnings for missing)
					v.validateRecipeRegistrations(recipeResult, "deployment")

					// Build test pattern from recipe (constraint names -> test names)
					patternResult := v.buildTestPattern(recipeResult, "deployment")

					// Deploy ONE Job for ALL deployment checks and constraints in this phase
					jobConfig := agent.Config{
						Namespace:          v.Namespace,
						JobName:            fmt.Sprintf("aicr-%s-deployment", v.RunID),
						Image:              v.Image,
						ImagePullSecrets:   v.ImagePullSecrets,
						ServiceAccountName: "aicr-validator",
						SnapshotConfigMap:  snapshotCMName,
						RecipeConfigMap:    recipeCMName,
						TestPackage:        "./pkg/validator/checks/deployment",
						TestPattern:        patternResult.Pattern,
						ExpectedTests:      patternResult.ExpectedTests,
						Timeout:            resolvePhaseTimeout(recipeResult.Validation.Deployment, DefaultDeploymentTimeout),
						Tolerations:        v.Tolerations,
						Affinity:           preferCPUNodeAffinity(),
					}

					deployer := agent.NewDeployer(clientset, jobConfig)

					// Run the phase Job and aggregate results
					phaseJobResult := v.runPhaseJob(ctx, deployer, jobConfig, "deployment")

					// Merge Job results into phase result
					phaseResult.Checks = phaseJobResult.Checks
				}
			}
		}
	}

	// Determine phase status based on checks
	failedCount := 0
	passedCount := 0
	for _, check := range phaseResult.Checks {
		switch check.Status {
		case ValidationStatusFail:
			failedCount++
		case ValidationStatusPass:
			passedCount++
		case ValidationStatusPartial, ValidationStatusSkipped, ValidationStatusWarning:
			// Don't count these toward pass/fail
		}
	}

	if failedCount > 0 {
		phaseResult.Status = ValidationStatusFail
	} else if passedCount > 0 {
		phaseResult.Status = ValidationStatusPass
	}

	phaseResult.Duration = time.Since(start)
	result.Phases[string(PhaseDeployment)] = phaseResult

	// Update summary
	result.Summary.Status = phaseResult.Status
	result.Summary.Passed = passedCount
	result.Summary.Failed = failedCount
	result.Summary.Total = len(phaseResult.Checks)
	result.Summary.Duration = phaseResult.Duration

	slog.Info("deployment validation completed",
		"status", phaseResult.Status,
		"checks", len(phaseResult.Checks),
		"duration", phaseResult.Duration)

	return result, nil
}

// validatePerformance validates performance phase.
// Runs checks as Kubernetes Jobs with GPU node affinity for performance tests.
//
//nolint:unparam // snap may be used in future implementations
func (v *Validator) validatePerformance(
	ctx context.Context,
	recipeResult *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
) (*ValidationResult, error) {

	start := time.Now()
	slog.Info("running performance validation phase")

	result := NewValidationResult()
	phaseResult := &PhaseResult{
		Status:      ValidationStatusPass,
		Constraints: []ConstraintValidation{},
		Checks:      []CheckResult{},
	}

	// Check if performance phase is configured
	if recipeResult.Validation == nil || recipeResult.Validation.Performance == nil {
		phaseResult.Status = ValidationStatusSkipped
		phaseResult.Reason = "performance phase not configured in recipe"
	} else {
		// NOTE: Performance phase constraints require live cluster access and measurements.
		// They are NOT evaluated inline like readiness constraints.
		// Instead, they should be registered as constraint validators in the checks registry
		// and will be evaluated inside the validation Job with cluster access.
		// See pkg/validator/checks/performance/ for examples.

		// Log infrastructure component if specified
		if recipeResult.Validation.Performance.Infrastructure != "" {
			slog.Debug("performance infrastructure specified",
				"component", recipeResult.Validation.Performance.Infrastructure)
		}

		// Run checks and evaluate constraints as Kubernetes Jobs
		// Note: RBAC resources must be created by the caller before invoking this function.
		// For multi-phase validation, validateAll() manages RBAC lifecycle.
		// For single-phase validation, the CLI/API should call agent.EnsureRBAC() first.
		if len(recipeResult.Validation.Performance.Checks) > 0 || len(recipeResult.Validation.Performance.Constraints) > 0 {
			if v.NoCluster {
				slog.Info("no-cluster mode enabled, skipping cluster check execution for performance phase")
				// Create stub check results for each check in the recipe
				for _, checkName := range recipeResult.Validation.Performance.Checks {
					phaseResult.Checks = append(phaseResult.Checks, CheckResult{
						Name:   checkName,
						Status: ValidationStatusSkipped,
						Reason: "skipped - no-cluster mode (test mode)",
					})
				}
			} else {
				clientset, _, err := k8sclient.GetKubeClient()
				if err != nil {
					// If Kubernetes is not available (e.g., running in test mode), skip check execution
					slog.Warn("Kubernetes client unavailable, skipping check execution",
						"error", err,
						"checks", len(recipeResult.Validation.Performance.Checks))
					// Add skeleton check result
					phaseResult.Checks = append(phaseResult.Checks, CheckResult{
						Name:   "performance",
						Status: ValidationStatusPass,
						Reason: "skipped - Kubernetes unavailable (test mode)",
					})
				} else {
					// ConfigMap names (created once per validation run by validateAll)
					snapshotCMName := fmt.Sprintf("aicr-snapshot-%s", v.RunID)
					recipeCMName := fmt.Sprintf("aicr-recipe-%s", v.RunID)

					// Validate that all recipe constraints/checks are registered (logs warnings for missing)
					v.validateRecipeRegistrations(recipeResult, "performance")

					// Build a test pattern so only the tests required by the recipe run,
					// not every test in the package (including unit tests).
					patternResult := v.buildTestPattern(recipeResult, "performance")

					// Deploy ONE Job for ALL performance checks and constraints in this phase
					// The Job pod is an orchestration layer (creates GPU workload Pods);
					// it only needs K8s API access, so prefer CPU nodes.
					jobConfig := agent.Config{
						Namespace:          v.Namespace,
						JobName:            fmt.Sprintf("aicr-%s-performance", v.RunID),
						Image:              v.Image,
						ImagePullSecrets:   v.ImagePullSecrets,
						ServiceAccountName: "aicr-validator",
						SnapshotConfigMap:  snapshotCMName,
						RecipeConfigMap:    recipeCMName,
						TestPackage:        "./pkg/validator/checks/performance",
						TestPattern:        patternResult.Pattern,
						Timeout:            resolvePhaseTimeout(recipeResult.Validation.Performance, DefaultPerformanceTimeout),
						Tolerations:        v.Tolerations,
						Affinity:           preferCPUNodeAffinity(),
					}

					deployer := agent.NewDeployer(clientset, jobConfig)

					// Run the phase Job and aggregate results
					phaseJobResult := v.runPhaseJob(ctx, deployer, jobConfig, "performance")

					// Merge Job results into phase result
					phaseResult.Checks = phaseJobResult.Checks
				}
			}
		}
	}

	// Determine phase status based on checks
	// NOTE: Phase constraints are evaluated inside Jobs, not inline
	failedCount := 0
	passedCount := 0
	for _, check := range phaseResult.Checks {
		switch check.Status {
		case ValidationStatusFail:
			failedCount++
		case ValidationStatusPass:
			passedCount++
		case ValidationStatusPartial, ValidationStatusSkipped, ValidationStatusWarning:
			// Don't count these toward pass/fail
		}
	}

	if failedCount > 0 {
		phaseResult.Status = ValidationStatusFail
	} else if len(phaseResult.Checks) > 0 {
		phaseResult.Status = ValidationStatusPass
	}

	phaseResult.Duration = time.Since(start)
	result.Phases[string(PhasePerformance)] = phaseResult

	// Update summary
	result.Summary.Status = phaseResult.Status
	result.Summary.Passed = passedCount
	result.Summary.Failed = failedCount
	result.Summary.Total = len(phaseResult.Checks)
	result.Summary.Duration = phaseResult.Duration

	slog.Info("performance validation completed",
		"status", phaseResult.Status,
		"checks", len(phaseResult.Checks),
		"duration", phaseResult.Duration)

	return result, nil
}

// validateConformance validates conformance phase.
// Runs checks as Kubernetes Jobs to verify Kubernetes API conformance.
//
//nolint:unparam,dupl // snap may be used in future; similar structure is intentional
func (v *Validator) validateConformance(
	ctx context.Context,
	recipeResult *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
) (*ValidationResult, error) {
	//nolint:dupl // Phase validation methods have similar structure by design
	start := time.Now()
	slog.Info("running conformance validation phase")

	result := NewValidationResult()
	phaseResult := &PhaseResult{
		Status:      ValidationStatusPass,
		Constraints: []ConstraintValidation{},
		Checks:      []CheckResult{},
	}

	// Check if conformance phase is configured
	if recipeResult.Validation == nil || recipeResult.Validation.Conformance == nil {
		phaseResult.Status = ValidationStatusSkipped
		phaseResult.Reason = "conformance phase not configured in recipe"
	} else { //nolint:gocritic // elseif not applicable, multiple statements in else block
		// NOTE: Conformance phase constraints require live cluster access.
		// They are NOT evaluated inline like readiness constraints.
		// Instead, they should be registered as constraint validators in the checks registry
		// and will be evaluated inside the validation Job with cluster access.
		// See pkg/validator/checks/conformance/ for examples.

		// Run checks and evaluate constraints as Kubernetes Jobs
		// Note: RBAC resources must be created by the caller before invoking this function.
		// For multi-phase validation, validateAll() manages RBAC lifecycle.
		// For single-phase validation, the CLI/API should call agent.EnsureRBAC() first.
		if len(recipeResult.Validation.Conformance.Checks) > 0 || len(recipeResult.Validation.Conformance.Constraints) > 0 {
			if v.NoCluster {
				slog.Info("no-cluster mode enabled, skipping cluster check execution for conformance phase")
				// Create stub check results for each check in the recipe
				for _, checkName := range recipeResult.Validation.Conformance.Checks {
					phaseResult.Checks = append(phaseResult.Checks, CheckResult{
						Name:   checkName,
						Status: ValidationStatusSkipped,
						Reason: "skipped - no-cluster mode (test mode)",
					})
				}
			} else {
				clientset, _, err := k8sclient.GetKubeClient()
				if err != nil {
					// If Kubernetes is not available (e.g., running in test mode), skip check execution
					slog.Warn("Kubernetes client unavailable, skipping check execution",
						"error", err,
						"checks", len(recipeResult.Validation.Conformance.Checks))
					// Add skeleton check result
					phaseResult.Checks = append(phaseResult.Checks, CheckResult{
						Name:   "conformance",
						Status: ValidationStatusSkipped,
						Reason: "skipped - Kubernetes unavailable (test mode)",
					})
				} else {
					// ConfigMap names (created once per validation run by validateAll)
					snapshotCMName := fmt.Sprintf("aicr-snapshot-%s", v.RunID)
					recipeCMName := fmt.Sprintf("aicr-recipe-%s", v.RunID)

					// Validate that all recipe constraints/checks are registered (logs warnings for missing)
					v.validateRecipeRegistrations(recipeResult, "conformance")

					// Build test pattern from recipe (check/constraint names -> test names)
					patternResult := v.buildTestPattern(recipeResult, "conformance")

					// Deploy ONE Job for ALL conformance checks and constraints in this phase
					jobConfig := agent.Config{
						Namespace:          v.Namespace,
						JobName:            fmt.Sprintf("aicr-%s-conformance", v.RunID),
						Image:              v.Image,
						ImagePullSecrets:   v.ImagePullSecrets,
						ServiceAccountName: "aicr-validator",
						SnapshotConfigMap:  snapshotCMName,
						RecipeConfigMap:    recipeCMName,
						TestPackage:        "./pkg/validator/checks/conformance",
						TestPattern:        patternResult.Pattern,
						ExpectedTests:      patternResult.ExpectedTests,
						Timeout:            resolvePhaseTimeout(recipeResult.Validation.Conformance, DefaultConformanceTimeout),
						Tolerations:        v.Tolerations,
						Affinity:           preferCPUNodeAffinity(),
					}

					deployer := agent.NewDeployer(clientset, jobConfig)

					// Run the phase Job and aggregate results
					phaseJobResult := v.runPhaseJob(ctx, deployer, jobConfig, "conformance")

					// Merge Job results into phase result
					phaseResult.Checks = phaseJobResult.Checks
				}
			}
		}
	}

	// Determine phase status based on checks
	// NOTE: Phase constraints are evaluated inside Jobs, not inline
	failedCount := 0
	passedCount := 0
	for _, check := range phaseResult.Checks {
		switch check.Status {
		case ValidationStatusFail:
			failedCount++
		case ValidationStatusPass:
			passedCount++
		case ValidationStatusPartial, ValidationStatusSkipped, ValidationStatusWarning:
			// Don't count these toward pass/fail
		}
	}

	if failedCount > 0 {
		phaseResult.Status = ValidationStatusFail
	} else if len(phaseResult.Checks) > 0 {
		phaseResult.Status = ValidationStatusPass
	}

	phaseResult.Duration = time.Since(start)
	result.Phases[string(PhaseConformance)] = phaseResult

	// Update summary
	result.Summary.Status = phaseResult.Status
	result.Summary.Passed = passedCount
	result.Summary.Failed = failedCount
	result.Summary.Total = len(phaseResult.Checks)
	result.Summary.Duration = phaseResult.Duration

	slog.Info("conformance validation completed",
		"status", phaseResult.Status,
		"checks", len(phaseResult.Checks),
		"duration", phaseResult.Duration)

	return result, nil
}

// validateRecipeRegistrations checks that all constraints and checks in the recipe
// are registered. Logs warnings for any that are missing (does not fail validation).
func (v *Validator) validateRecipeRegistrations(recipeResult *recipe.RecipeResult, phase string) {
	var unregisteredConstraints []string
	var unregisteredChecks []string

	switch phase {
	case string(PhaseDeployment):
		if recipeResult.Validation != nil && recipeResult.Validation.Deployment != nil {
			// Check constraints
			for _, constraint := range recipeResult.Validation.Deployment.Constraints {
				_, ok := checks.GetTestNameForConstraint(constraint.Name)
				if !ok {
					unregisteredConstraints = append(unregisteredConstraints, constraint.Name)
				}
			}

			// Check explicit checks
			for _, checkName := range recipeResult.Validation.Deployment.Checks {
				_, ok := checks.GetCheck(checkName)
				if !ok {
					unregisteredChecks = append(unregisteredChecks, checkName)
				}
			}
		}
	case string(PhasePerformance):
		if recipeResult.Validation != nil && recipeResult.Validation.Performance != nil {
			for _, constraint := range recipeResult.Validation.Performance.Constraints {
				_, ok := checks.GetTestNameForConstraint(constraint.Name)
				if !ok {
					unregisteredConstraints = append(unregisteredConstraints, constraint.Name)
				}
			}

			for _, checkName := range recipeResult.Validation.Performance.Checks {
				_, ok := checks.GetCheck(checkName)
				if !ok {
					unregisteredChecks = append(unregisteredChecks, checkName)
				}
			}
		}
	case string(PhaseConformance):
		if recipeResult.Validation != nil && recipeResult.Validation.Conformance != nil {
			for _, constraint := range recipeResult.Validation.Conformance.Constraints {
				_, ok := checks.GetTestNameForConstraint(constraint.Name)
				if !ok {
					unregisteredConstraints = append(unregisteredConstraints, constraint.Name)
				}
			}

			for _, checkName := range recipeResult.Validation.Conformance.Checks {
				_, ok := checks.GetCheck(checkName)
				if !ok {
					unregisteredChecks = append(unregisteredChecks, checkName)
				}
			}
		}
	}

	// Log warnings if anything is unregistered
	if len(unregisteredConstraints) > 0 || len(unregisteredChecks) > 0 {
		var msg strings.Builder
		fmt.Fprintf(&msg, "recipe contains unregistered validations for phase %s (will be skipped):\n", phase)

		if len(unregisteredConstraints) > 0 {
			fmt.Fprintf(&msg, "\nUnregistered constraints (%d):\n", len(unregisteredConstraints))
			for _, name := range unregisteredConstraints {
				fmt.Fprintf(&msg, "  - %s\n", name)
			}

			// Show available constraints for this phase
			available := checks.ListConstraintTests(phase)
			if len(available) > 0 {
				fmt.Fprintf(&msg, "\nAvailable constraints for phase '%s' (%d):\n", phase, len(available))
				for _, ct := range available {
					fmt.Fprintf(&msg, "  - %s: %s\n", ct.Name, ct.Description)
				}
			}
		}

		if len(unregisteredChecks) > 0 {
			fmt.Fprintf(&msg, "\nUnregistered checks (%d):\n", len(unregisteredChecks))
			for _, name := range unregisteredChecks {
				fmt.Fprintf(&msg, "  - %s\n", name)
			}

			// Show available checks for this phase
			available := checks.ListChecks(phase)
			if len(available) > 0 {
				fmt.Fprintf(&msg, "\nAvailable checks for phase '%s' (%d):\n", phase, len(available))
				for _, check := range available {
					fmt.Fprintf(&msg, "  - %s: %s\n", check.Name, check.Description)
				}
			}
		}

		msg.WriteString("\nTo add missing validations, see: pkg/validator/checks/README.md")

		// Log as warning (not error) - don't fail validation
		slog.Warn(msg.String())
	}
}

// buildTestPatternResult contains the test pattern and expected count.
type buildTestPatternResult struct {
	Pattern       string
	ExpectedTests int
}

func (v *Validator) buildTestPattern(recipeResult *recipe.RecipeResult, phase string) buildTestPatternResult {
	var testNames []string
	uniqueTests := make(map[string]bool)

	switch phase {
	case string(PhaseDeployment):
		if recipeResult.Validation != nil && recipeResult.Validation.Deployment != nil {
			// Add tests for constraints
			for _, constraint := range recipeResult.Validation.Deployment.Constraints {
				testName, ok := checks.GetTestNameForConstraint(constraint.Name)
				if ok && !uniqueTests[testName] {
					testNames = append(testNames, testName)
					uniqueTests[testName] = true
					slog.Debug("constraint mapped to test", "constraint", constraint.Name, "test", testName)
				}
				// Note: Missing registrations are caught by validateRecipeRegistrations
			}

			// Add tests for explicit checks
			for _, checkName := range recipeResult.Validation.Deployment.Checks {
				testName, ok := checks.GetTestNameForCheck(checkName)
				if !ok {
					// Fallback to generated name if not registered
					testName = checkNameToTestName(checkName)
				}
				if !uniqueTests[testName] {
					testNames = append(testNames, testName)
					uniqueTests[testName] = true
					slog.Debug("check mapped to test", "check", checkName, "test", testName)
				}
			}
		}
	case string(PhasePerformance):
		if recipeResult.Validation != nil && recipeResult.Validation.Performance != nil {
			// Add tests for constraints
			for _, constraint := range recipeResult.Validation.Performance.Constraints {
				testName, ok := checks.GetTestNameForConstraint(constraint.Name)
				if ok && !uniqueTests[testName] {
					testNames = append(testNames, testName)
					uniqueTests[testName] = true
					slog.Debug("constraint mapped to test", "constraint", constraint.Name, "test", testName)
				}
			}

			// Add tests for explicit checks
			for _, checkName := range recipeResult.Validation.Performance.Checks {
				testName, ok := checks.GetTestNameForCheck(checkName)
				if !ok {
					testName = checkNameToTestName(checkName)
				}
				if !uniqueTests[testName] {
					testNames = append(testNames, testName)
					uniqueTests[testName] = true
					slog.Debug("check mapped to test", "check", checkName, "test", testName)
				}
			}
		}
	case string(PhaseConformance):
		if recipeResult.Validation != nil && recipeResult.Validation.Conformance != nil {
			// Add tests for constraints
			for _, constraint := range recipeResult.Validation.Conformance.Constraints {
				testName, ok := checks.GetTestNameForConstraint(constraint.Name)
				if ok && !uniqueTests[testName] {
					testNames = append(testNames, testName)
					uniqueTests[testName] = true
					slog.Debug("constraint mapped to test", "constraint", constraint.Name, "test", testName)
				}
			}

			// Add tests for explicit checks
			for _, checkName := range recipeResult.Validation.Conformance.Checks {
				testName, ok := checks.GetTestNameForCheck(checkName)
				if !ok {
					testName = checkNameToTestName(checkName)
				}
				if !uniqueTests[testName] {
					testNames = append(testNames, testName)
					uniqueTests[testName] = true
					slog.Debug("check mapped to test", "check", checkName, "test", testName)
				}
			}
		}
	}

	if len(testNames) == 0 {
		// No pattern - run all tests
		slog.Debug("no pattern specified, will run all tests in package")
		return buildTestPatternResult{Pattern: "", ExpectedTests: 0}
	}

	// Build regex: ^(TestGPUOperatorVersion|TestCheckExpectedResources)$
	pattern := "^(" + strings.Join(testNames, "|") + ")$"
	slog.Info("built test pattern from recipe", "pattern", pattern, "tests", len(testNames))
	return buildTestPatternResult{Pattern: pattern, ExpectedTests: len(testNames)}
}
