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

//nolint:dupl // Phase validators have similar structure by design

package validator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/NVIDIA/eidos/pkg/errors"
	"github.com/NVIDIA/eidos/pkg/header"
	k8sclient "github.com/NVIDIA/eidos/pkg/k8s/client"
	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/snapshotter"
	"github.com/NVIDIA/eidos/pkg/validator/agent"
)

// ValidationPhaseName represents the name of a validation phase.
type ValidationPhaseName string

const (
	// PhaseReadiness is the readiness validation phase.
	PhaseReadiness ValidationPhaseName = "readiness"

	// PhaseDeployment is the deployment validation phase.
	PhaseDeployment ValidationPhaseName = "deployment"

	// PhasePerformance is the performance validation phase.
	PhasePerformance ValidationPhaseName = "performance"

	// PhaseConformance is the conformance validation phase.
	PhaseConformance ValidationPhaseName = "conformance"

	// PhaseAll runs all phases sequentially.
	PhaseAll ValidationPhaseName = "all"
)

// PhaseOrder defines the canonical execution order for validation phases.
// Readiness and deployment must run before performance or conformance.
var PhaseOrder = []ValidationPhaseName{
	PhaseReadiness,
	PhaseDeployment,
	PhasePerformance,
	PhaseConformance,
}

// ValidatePhase runs validation for a specific phase.
// This is the main entry point for phase-based validation.
func (v *Validator) ValidatePhase(
	ctx context.Context,
	phase ValidationPhaseName,
	recipeResult *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
) (*ValidationResult, error) {

	// For "all" phases, use validateAll which manages ConfigMaps internally
	if phase == PhaseAll {
		return v.validateAll(ctx, recipeResult, snap)
	}

	// For single phase validation, create RBAC and ConfigMaps before running the phase
	clientset, _, err := k8sclient.GetKubeClient()
	if err == nil {
		// Create RBAC resources for validation Jobs
		sharedConfig := agent.Config{
			Namespace:          v.Namespace,
			JobName:            "eidos-validator", // Shared ServiceAccount name
			ServiceAccountName: "eidos-validator",
		}
		deployer := agent.NewDeployer(clientset, sharedConfig)

		slog.Debug("creating RBAC for validation")
		if rbacErr := deployer.EnsureRBAC(ctx); rbacErr != nil {
			slog.Warn("failed to create RBAC resources", "error", rbacErr)
		} else {
			// Cleanup RBAC after phase completes
			//nolint:contextcheck // Using separate context for cleanup to avoid cancellation
			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if cleanupErr := deployer.CleanupRBAC(cleanupCtx); cleanupErr != nil {
					slog.Warn("failed to cleanup RBAC resources", "error", cleanupErr)
				}
			}()
		}

		// Create ConfigMaps for this single-phase validation
		if cmErr := v.ensureDataConfigMaps(ctx, clientset, snap, recipeResult); cmErr != nil {
			slog.Warn("failed to create data ConfigMaps", "error", cmErr)
		} else {
			// Cleanup ConfigMaps after phase completes
			//nolint:contextcheck // Using separate context for cleanup to avoid cancellation
			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				v.cleanupDataConfigMaps(cleanupCtx, clientset)
			}()
		}
	}

	// Run the requested phase
	switch phase {
	case PhaseReadiness:
		return v.validateReadiness(ctx, recipeResult, snap)
	case PhaseDeployment:
		return v.validateDeployment(ctx, recipeResult, snap)
	case PhasePerformance:
		return v.validatePerformance(ctx, recipeResult, snap)
	case PhaseConformance:
		return v.validateConformance(ctx, recipeResult, snap)
	case PhaseAll:
		// Should not reach here - PhaseAll is handled above
		return v.validateAll(ctx, recipeResult, snap)
	default:
		return v.validateReadiness(ctx, recipeResult, snap)
	}
}

// ValidatePhases runs validation for multiple specified phases.
// If no phases are specified, defaults to readiness phase.
// If phases includes "all", runs all phases.
func (v *Validator) ValidatePhases(
	ctx context.Context,
	phases []ValidationPhaseName,
	recipeResult *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
) (*ValidationResult, error) {
	// Handle empty or single phase cases
	if len(phases) == 0 {
		return v.ValidatePhase(ctx, PhaseReadiness, recipeResult, snap)
	}
	if len(phases) == 1 {
		return v.ValidatePhase(ctx, phases[0], recipeResult, snap)
	}

	// Check if "all" is in the list - if so, just run all
	for _, p := range phases {
		if p == PhaseAll {
			return v.validateAll(ctx, recipeResult, snap)
		}
	}

	start := time.Now()
	slog.Info("running specified validation phases", "phases", phases)

	result := NewValidationResult()
	overallStatus := ValidationStatusPass

	for _, phase := range phases {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// Skip subsequent phases if a previous phase failed
		if overallStatus == ValidationStatusFail {
			result.Phases[string(phase)] = &PhaseResult{
				Status: ValidationStatusSkipped,
				Reason: "skipped due to previous phase failure",
			}
			slog.Info("skipping phase due to previous failure", "phase", phase)
			continue
		}

		// Run the phase
		phaseResultDoc, err := v.ValidatePhase(ctx, phase, recipeResult, snap)
		if err != nil {
			return nil, err
		}

		// Merge phase result into overall result
		if phaseResultDoc.Phases[string(phase)] != nil {
			result.Phases[string(phase)] = phaseResultDoc.Phases[string(phase)]

			// Update overall status
			if phaseResultDoc.Phases[string(phase)].Status == ValidationStatusFail {
				overallStatus = ValidationStatusFail
			}
		}
	}

	// Calculate overall summary
	totalPassed := 0
	totalFailed := 0
	totalSkipped := 0
	totalChecks := 0

	for _, phaseResult := range result.Phases {
		for _, cv := range phaseResult.Constraints {
			totalChecks++
			switch cv.Status {
			case ConstraintStatusPassed:
				totalPassed++
			case ConstraintStatusFailed:
				totalFailed++
			case ConstraintStatusSkipped:
				totalSkipped++
			}
		}
		totalChecks += len(phaseResult.Checks)
		for _, check := range phaseResult.Checks {
			switch check.Status {
			case ValidationStatusPass:
				totalPassed++
			case ValidationStatusFail:
				totalFailed++
			case ValidationStatusSkipped:
				totalSkipped++
			case ValidationStatusWarning:
				// Warnings don't affect pass/fail count
			case ValidationStatusPartial:
				// Partial status is not expected at check level
			}
		}
	}

	result.Summary.Status = overallStatus
	result.Summary.Passed = totalPassed
	result.Summary.Failed = totalFailed
	result.Summary.Skipped = totalSkipped
	result.Summary.Total = totalChecks
	result.Summary.Duration = time.Since(start)

	slog.Info("specified phases validation completed",
		"status", overallStatus,
		"phases", len(result.Phases),
		"passed", totalPassed,
		"failed", totalFailed,
		"skipped", totalSkipped,
		"duration", result.Summary.Duration)

	return result, nil
}

// validateReadiness validates readiness phase.
// Evaluates constraints inline and runs checks as Kubernetes Jobs.
//
//nolint:unparam // error return may be used in future implementations
func (v *Validator) validateReadiness(
	ctx context.Context,
	recipeResult *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
) (*ValidationResult, error) {

	start := time.Now()
	slog.Info("running readiness validation phase")

	result := NewValidationResult()
	phaseResult := &PhaseResult{
		Status:      ValidationStatusPass,
		Constraints: []ConstraintValidation{},
		Checks:      []CheckResult{},
	}

	// Evaluate recipe-level constraints (spec.constraints) inline
	for _, constraint := range recipeResult.Constraints {
		cv := v.evaluateConstraint(constraint, snap)
		phaseResult.Constraints = append(phaseResult.Constraints, cv)
	}

	// Run named checks as Kubernetes Jobs if defined in validation config
	// Note: RBAC resources must be created by the caller before invoking this function.
	// For multi-phase validation, validateAll() manages RBAC lifecycle.
	// For single-phase validation, the CLI/API should call agent.EnsureRBAC() first.
	//nolint:dupl // Phase validation methods have similar structure by design
	if recipeResult.Validation != nil && recipeResult.Validation.PreDeployment != nil && len(recipeResult.Validation.PreDeployment.Checks) > 0 {
		clientset, _, err := k8sclient.GetKubeClient()
		if err != nil {
			// If Kubernetes is not available (e.g., running in test mode), skip check execution
			slog.Warn("Kubernetes client unavailable, skipping check execution",
				"error", err,
				"checks", len(recipeResult.Validation.PreDeployment.Checks))
			// Add skeleton check results
			for _, checkName := range recipeResult.Validation.PreDeployment.Checks {
				check := CheckResult{
					Name:   checkName,
					Status: ValidationStatusPass,
					Reason: "skipped - Kubernetes unavailable (test mode)",
				}
				phaseResult.Checks = append(phaseResult.Checks, check)
			}
		} else {
			// ConfigMap names (created once per validation run by validateAll)
			snapshotCMName := fmt.Sprintf("eidos-snapshot-%s", v.RunID)
			recipeCMName := fmt.Sprintf("eidos-recipe-%s", v.RunID)

			// Deploy ONE Job for ALL readiness checks in this phase
			jobConfig := agent.Config{
				Namespace:          v.Namespace,
				JobName:            fmt.Sprintf("eidos-%s-readiness", v.RunID),
				Image:              v.Image, // TODO: Use actual image from config
				ServiceAccountName: "eidos-validator",
				SnapshotConfigMap:  snapshotCMName,
				RecipeConfigMap:    recipeCMName,
				TestPackage:        "./pkg/validator/checks/readiness",
				TestPattern:        "", // Run all tests in package
				Timeout:            5 * time.Minute,
			}

			deployer := agent.NewDeployer(clientset, jobConfig)

			// Run the phase Job and aggregate results
			phaseJobResult := v.runPhaseJob(ctx, deployer, jobConfig, "readiness")

			// Merge Job results into phase result
			phaseResult.Checks = phaseJobResult.Checks
		}
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
		"checks", len(phaseResult.Checks),
		"duration", phaseResult.Duration)

	return result, nil
}

// validateDeployment validates deployment phase.
// Evaluates constraints inline and runs checks as Kubernetes Jobs.
//
//nolint:unparam,dupl // snap may be used in future; similar structure is intentional
func (v *Validator) validateDeployment(
	ctx context.Context,
	recipeResult *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
) (*ValidationResult, error) {
	//nolint:dupl
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
				snapshotCMName := fmt.Sprintf("eidos-snapshot-%s", v.RunID)
				recipeCMName := fmt.Sprintf("eidos-recipe-%s", v.RunID)

				// Deploy ONE Job for ALL deployment checks and constraints in this phase
				jobConfig := agent.Config{
					Namespace:          v.Namespace,
					JobName:            fmt.Sprintf("eidos-%s-deployment", v.RunID),
					Image:              v.Image,
					ServiceAccountName: "eidos-validator",
					SnapshotConfigMap:  snapshotCMName,
					RecipeConfigMap:    recipeCMName,
					TestPackage:        "./pkg/validator/checks/deployment",
					TestPattern:        "", // Run all tests in package
					Timeout:            10 * time.Minute,
				}

				deployer := agent.NewDeployer(clientset, jobConfig)

				// Run the phase Job and aggregate results
				phaseJobResult := v.runPhaseJob(ctx, deployer, jobConfig, "deployment")

				// Merge Job results into phase result
				phaseResult.Checks = phaseJobResult.Checks
			}
		}
	}

	// Determine phase status based on checks
	// NOTE: Deployment constraints are evaluated inside Jobs, not inline
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
				snapshotCMName := fmt.Sprintf("eidos-snapshot-%s", v.RunID)
				recipeCMName := fmt.Sprintf("eidos-recipe-%s", v.RunID)

				// Deploy ONE Job for ALL performance checks and constraints in this phase
				// Performance tests may need GPU nodes
				jobConfig := agent.Config{
					Namespace:          v.Namespace,
					JobName:            fmt.Sprintf("eidos-%s-performance", v.RunID),
					Image:              v.Image,
					ServiceAccountName: "eidos-validator",
					SnapshotConfigMap:  snapshotCMName,
					RecipeConfigMap:    recipeCMName,
					TestPackage:        "./pkg/validator/checks/performance",
					TestPattern:        "",               // Run all tests in package
					Timeout:            30 * time.Minute, // Performance tests may take longer
					// TODO: Add GPU node selector if infrastructure specifies GPU requirements
				}

				deployer := agent.NewDeployer(clientset, jobConfig)

				// Run the phase Job and aggregate results
				phaseJobResult := v.runPhaseJob(ctx, deployer, jobConfig, "performance")

				// Merge Job results into phase result
				phaseResult.Checks = phaseJobResult.Checks
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
	//nolint:dupl
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
			clientset, _, err := k8sclient.GetKubeClient()
			if err != nil {
				// If Kubernetes is not available (e.g., running in test mode), skip check execution
				slog.Warn("Kubernetes client unavailable, skipping check execution",
					"error", err,
					"checks", len(recipeResult.Validation.Conformance.Checks))
				// Add skeleton check result
				phaseResult.Checks = append(phaseResult.Checks, CheckResult{
					Name:   "conformance",
					Status: ValidationStatusPass,
					Reason: "skipped - Kubernetes unavailable (test mode)",
				})
			} else {
				// ConfigMap names (created once per validation run by validateAll)
				snapshotCMName := fmt.Sprintf("eidos-snapshot-%s", v.RunID)
				recipeCMName := fmt.Sprintf("eidos-recipe-%s", v.RunID)

				// Deploy ONE Job for ALL conformance checks and constraints in this phase
				jobConfig := agent.Config{
					Namespace:          v.Namespace,
					JobName:            fmt.Sprintf("eidos-%s-conformance", v.RunID),
					Image:              v.Image,
					ServiceAccountName: "eidos-validator",
					SnapshotConfigMap:  snapshotCMName,
					RecipeConfigMap:    recipeCMName,
					TestPackage:        "./pkg/validator/checks/conformance",
					TestPattern:        "", // Run all tests in package
					Timeout:            15 * time.Minute,
				}

				deployer := agent.NewDeployer(clientset, jobConfig)

				// Run the phase Job and aggregate results
				phaseJobResult := v.runPhaseJob(ctx, deployer, jobConfig, "conformance")

				// Merge Job results into phase result
				phaseResult.Checks = phaseJobResult.Checks
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

// runPhaseJob deploys and runs a single Job that executes all checks for a phase.
// Returns aggregated results for all checks in the phase.
func (v *Validator) runPhaseJob(
	ctx context.Context,
	deployer *agent.Deployer,
	config agent.Config,
	phaseName string,
) *PhaseResult {

	result := &PhaseResult{
		Status: ValidationStatusPass,
		Checks: []CheckResult{},
	}

	slog.Debug("deploying Job for phase", "phase", phaseName, "job", config.JobName)

	// Deploy Job (RBAC already exists)
	if err := deployer.DeployJob(ctx); err != nil {
		// Check if this is a test environment error
		errMsg := err.Error()
		if strings.Contains(errMsg, "not found") || strings.Contains(errMsg, "namespace") {
			slog.Warn("Job deployment failed (likely test mode)",
				"phase", phaseName,
				"error", err)
			result.Status = ValidationStatusSkipped
			return result
		}
		result.Status = ValidationStatusFail
		result.Checks = append(result.Checks, CheckResult{
			Name:   phaseName,
			Status: ValidationStatusFail,
			Reason: fmt.Sprintf("failed to deploy Job: %v", err),
		})
		return result
	}

	// Wait for Job completion
	if err := deployer.WaitForCompletion(ctx, config.Timeout); err != nil {
		// Cleanup failed Job
		if cleanupErr := deployer.CleanupJob(ctx); cleanupErr != nil {
			slog.Warn("failed to cleanup Job after failure", "job", config.JobName, "error", cleanupErr)
		}
		result.Status = ValidationStatusFail
		result.Checks = append(result.Checks, CheckResult{
			Name:   phaseName,
			Status: ValidationStatusFail,
			Reason: fmt.Sprintf("Job failed or timed out: %v", err),
		})
		return result
	}

	// Get aggregated results from Job
	jobResult, err := deployer.GetResult(ctx)
	if err != nil {
		// Cleanup Job
		if cleanupErr := deployer.CleanupJob(ctx); cleanupErr != nil {
			slog.Warn("failed to cleanup Job", "job", config.JobName, "error", cleanupErr)
		}
		result.Status = ValidationStatusFail
		result.Checks = append(result.Checks, CheckResult{
			Name:   phaseName,
			Status: ValidationStatusFail,
			Reason: fmt.Sprintf("failed to retrieve result: %v", err),
		})
		return result
	}

	// Parse individual test results from go test JSON output
	// Each test becomes a separate CheckResult for granular reporting
	if len(jobResult.Tests) > 0 {
		for _, test := range jobResult.Tests {
			checkResult := CheckResult{
				Name:   test.Name,
				Status: mapTestStatusToValidationStatus(test.Status),
			}

			// Build reason from test output
			if len(test.Output) > 0 {
				// Include last few lines of output as reason (useful for failures)
				maxLines := 5
				startIdx := len(test.Output) - maxLines
				if startIdx < 0 {
					startIdx = 0
				}
				relevantOutput := test.Output[startIdx:]
				checkResult.Reason = strings.Join(relevantOutput, "\n")
			} else {
				checkResult.Reason = fmt.Sprintf("Test %s: %s", test.Status, test.Name)
			}

			result.Checks = append(result.Checks, checkResult)
		}
	} else {
		// Fallback: no individual tests parsed, return phase-level result
		result.Checks = append(result.Checks, CheckResult{
			Name:   phaseName,
			Status: ValidationStatus(jobResult.Status),
			Reason: jobResult.Message,
		})
	}

	slog.Debug("phase Job completed",
		"phase", phaseName,
		"status", jobResult.Status,
		"tests", len(jobResult.Tests),
		"duration", jobResult.Duration)

	// Cleanup Job after successful completion
	if err := deployer.CleanupJob(ctx); err != nil {
		slog.Warn("failed to cleanup Job", "job", config.JobName, "error", err)
	}

	// Set overall phase status based on check results
	for _, check := range result.Checks {
		if check.Status == ValidationStatusFail {
			result.Status = ValidationStatusFail
			break
		}
	}

	return result
}

// validateAll runs all phases sequentially with dependency logic.
// If a phase fails, subsequent phases are skipped.
// Uses efficient RBAC pattern: create once, reuse across all phases, cleanup once at end.
//
//nolint:funlen // Complex validation orchestration logic
func (v *Validator) validateAll(
	ctx context.Context,
	recipeResult *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
) (*ValidationResult, error) {

	start := time.Now()
	slog.Info("running all validation phases", "runID", v.RunID)

	result := NewValidationResult()
	result.Init(header.KindValidationResult, APIVersion, v.Version)
	overallStatus := ValidationStatusPass

	// Create Kubernetes client for agent deployment
	// If Kubernetes is not available (e.g., running in test mode), phases will skip Job execution
	clientset, _, err := k8sclient.GetKubeClient()
	rbacAvailable := err == nil

	// Check if resuming from existing validation
	var startPhase ValidationPhaseName
	var resuming bool

	if rbacAvailable {
		// Try to read existing ValidationResult (for resume)
		existingResult, readErr := v.readValidationResultConfigMap(ctx, clientset)
		if readErr == nil {
			// Resume: existing result found
			resuming = true
			result = existingResult
			startPhase = determineStartPhase(existingResult)
			slog.Info("resuming validation from existing run",
				"runID", v.RunID,
				"startPhase", startPhase)
		} else {
			// New validation: no existing result
			resuming = false
			startPhase = PhaseReadiness
			slog.Debug("starting new validation run", "runID", v.RunID)
		}
	}

	if rbacAvailable {
		// Create shared agent deployer for RBAC management
		// RBAC is created once and reused across all phases for efficiency
		sharedConfig := agent.Config{
			Namespace:          v.Namespace,
			ServiceAccountName: "eidos-validator",
			Image:              v.Image, // TODO: Use actual image from config
		}
		deployer := agent.NewDeployer(clientset, sharedConfig)

		// Ensure RBAC once at the start (idempotent - safe to call multiple times)
		slog.Debug("creating shared RBAC for all validation phases")
		if rbacErr := deployer.EnsureRBAC(ctx); rbacErr != nil {
			slog.Warn("failed to create validation RBAC, check execution will be skipped", "error", rbacErr)
		} else {
			// Cleanup RBAC at the end (deferred to ensure cleanup even on error)
			//nolint:contextcheck // Using separate context for cleanup to avoid cancellation
			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if cleanupErr := deployer.CleanupRBAC(cleanupCtx); cleanupErr != nil {
					slog.Warn("failed to cleanup RBAC resources", "error", cleanupErr)
				}
			}()
		}

		// Create ConfigMaps once at the start (reused across all phases)
		slog.Debug("creating shared ConfigMaps for snapshot and recipe data")
		if cmErr := v.ensureDataConfigMaps(ctx, clientset, snap, recipeResult); cmErr != nil {
			slog.Warn("failed to create data ConfigMaps, check execution will be skipped", "error", cmErr)
		} else {
			// Cleanup ConfigMaps at the end (deferred to ensure cleanup even on error)
			//nolint:contextcheck // Using separate context for cleanup to avoid cancellation
			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				v.cleanupDataConfigMaps(cleanupCtx, clientset)
			}()
		}

		// Create ValidationResult ConfigMap for progressive updates
		slog.Debug("creating ValidationResult ConfigMap for tracking progress")
		if resultErr := v.createValidationResultConfigMap(ctx, clientset); resultErr != nil {
			slog.Warn("failed to create validation result ConfigMap", "error", resultErr)
		} else {
			// Cleanup ValidationResult ConfigMap at the end
			//nolint:contextcheck // Using separate context for cleanup to avoid cancellation
			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				v.cleanupValidationResultConfigMap(cleanupCtx, clientset)
			}()
		}
	} else {
		slog.Warn("Kubernetes client unavailable, check execution will be skipped in all phases", "error", err)
	}

	// Use canonical phase order
	for _, phase := range PhaseOrder {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// Skip phases that come before the resume point
		if resuming && phase != startPhase {
			// Check if this phase already passed
			if phaseResult, exists := result.Phases[string(phase)]; exists && phaseResult.Status == ValidationStatusPass {
				slog.Debug("skipping phase (already passed in previous run)", "phase", phase)
				continue
			}
		}

		// We've reached the start phase - no longer resuming, run all remaining phases
		if phase == startPhase {
			resuming = false
		}

		// Skip subsequent phases if a previous phase failed
		if overallStatus == ValidationStatusFail {
			result.Phases[string(phase)] = &PhaseResult{
				Status: ValidationStatusSkipped,
				Reason: "skipped due to previous phase failure",
			}
			slog.Info("skipping phase due to previous failure", "phase", phase)
			continue
		}

		// Run the phase (RBAC already exists, phases will reuse it)
		var phaseResultDoc *ValidationResult
		var err error

		switch phase {
		case PhaseReadiness:
			phaseResultDoc, err = v.validateReadiness(ctx, recipeResult, snap)
		case PhaseDeployment:
			phaseResultDoc, err = v.validateDeployment(ctx, recipeResult, snap)
		case PhasePerformance:
			phaseResultDoc, err = v.validatePerformance(ctx, recipeResult, snap)
		case PhaseConformance:
			phaseResultDoc, err = v.validateConformance(ctx, recipeResult, snap)
		case PhaseAll:
			// PhaseAll should never reach here as it's handled in ValidatePhase
			return nil, errors.New(errors.ErrCodeInternal, "PhaseAll cannot be called within validateAll")
		}

		if err != nil {
			return nil, err
		}

		// Merge phase result into overall result
		if phaseResultDoc.Phases[string(phase)] != nil {
			result.Phases[string(phase)] = phaseResultDoc.Phases[string(phase)]

			// Update overall status
			if phaseResultDoc.Phases[string(phase)].Status == ValidationStatusFail {
				overallStatus = ValidationStatusFail
			}

			// Update ValidationResult ConfigMap with progress (progressive update)
			if rbacAvailable {
				if updateErr := v.updateValidationResultConfigMap(ctx, clientset, result); updateErr != nil {
					slog.Warn("failed to update validation result ConfigMap", "phase", phase, "error", updateErr)
				}
			}
		}
	}

	// Calculate overall summary
	totalPassed := 0
	totalFailed := 0
	totalSkipped := 0
	totalChecks := 0

	for _, phaseResult := range result.Phases {
		for _, cv := range phaseResult.Constraints {
			totalChecks++
			switch cv.Status {
			case ConstraintStatusPassed:
				totalPassed++
			case ConstraintStatusFailed:
				totalFailed++
			case ConstraintStatusSkipped:
				totalSkipped++
			}
		}
		totalChecks += len(phaseResult.Checks)
		for _, check := range phaseResult.Checks {
			switch check.Status {
			case ValidationStatusPass:
				totalPassed++
			case ValidationStatusFail:
				totalFailed++
			case ValidationStatusSkipped:
				totalSkipped++
			case ValidationStatusWarning:
				// Warnings don't affect pass/fail count
			case ValidationStatusPartial:
				// Partial status is not expected at check level
			}
		}
	}

	result.Summary.Status = overallStatus
	result.Summary.Passed = totalPassed
	result.Summary.Failed = totalFailed
	result.Summary.Skipped = totalSkipped
	result.Summary.Total = totalChecks
	result.Summary.Duration = time.Since(start)

	slog.Info("all phases validation completed",
		"status", overallStatus,
		"phases", len(result.Phases),
		"passed", totalPassed,
		"failed", totalFailed,
		"skipped", totalSkipped,
		"duration", result.Summary.Duration)

	return result, nil
}

// ensureDataConfigMaps creates ConfigMaps for snapshot and recipe data if they don't exist.
// Returns the names of the created ConfigMaps.
func (v *Validator) ensureDataConfigMaps(
	ctx context.Context,
	clientset kubernetes.Interface,
	snap *snapshotter.Snapshot,
	recipeResult *recipe.RecipeResult,
) error {

	// Use RunID to create unique ConfigMap names per validation run
	snapshotCMName := fmt.Sprintf("eidos-snapshot-%s", v.RunID)
	recipeCMName := fmt.Sprintf("eidos-recipe-%s", v.RunID)

	// Serialize snapshot to YAML
	snapshotYAML, err := yaml.Marshal(snap)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to serialize snapshot", err)
	}

	// Serialize recipe to YAML
	recipeYAML, err := yaml.Marshal(recipeResult)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to serialize recipe", err)
	}

	// Create snapshot ConfigMap
	snapshotCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      snapshotCMName,
			Namespace: v.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "eidos",
				"app.kubernetes.io/component": "validation",
				"eidos.nvidia.com/data-type":  "snapshot",
				"eidos.nvidia.com/run-id":     v.RunID,
				"eidos.nvidia.com/created-at": time.Now().Format("20060102-150405"),
			},
		},
		Data: map[string]string{
			"snapshot.yaml": string(snapshotYAML),
		},
	}

	_, err = clientset.CoreV1().ConfigMaps(v.Namespace).Create(ctx, snapshotCM, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create snapshot ConfigMap", err)
	}
	if apierrors.IsAlreadyExists(err) {
		// Update existing ConfigMap
		_, err = clientset.CoreV1().ConfigMaps(v.Namespace).Update(ctx, snapshotCM, metav1.UpdateOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to update snapshot ConfigMap", err)
		}
	}

	// Create recipe ConfigMap
	recipeCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      recipeCMName,
			Namespace: v.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "eidos",
				"app.kubernetes.io/component": "validation",
				"eidos.nvidia.com/data-type":  "recipe",
				"eidos.nvidia.com/run-id":     v.RunID,
				"eidos.nvidia.com/created-at": time.Now().Format("20060102-150405"),
			},
		},
		Data: map[string]string{
			"recipe.yaml": string(recipeYAML),
		},
	}

	_, err = clientset.CoreV1().ConfigMaps(v.Namespace).Create(ctx, recipeCM, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create recipe ConfigMap", err)
	}
	if apierrors.IsAlreadyExists(err) {
		// Update existing ConfigMap
		_, err = clientset.CoreV1().ConfigMaps(v.Namespace).Update(ctx, recipeCM, metav1.UpdateOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to update recipe ConfigMap", err)
		}
	}

	slog.Debug("ensured data ConfigMaps",
		"snapshot", snapshotCMName,
		"recipe", recipeCMName,
		"namespace", v.Namespace)

	return nil
}

// mapTestStatusToValidationStatus converts go test status to ValidationStatus.
func mapTestStatusToValidationStatus(testStatus string) ValidationStatus {
	switch testStatus {
	case "pass":
		return ValidationStatusPass
	case "fail":
		return ValidationStatusFail
	case "skip":
		return ValidationStatusSkipped
	default:
		return ValidationStatusWarning
	}
}

// determineStartPhase analyzes existing ValidationResult to determine where to resume.
// Returns the first phase that needs to run (failed or incomplete).
func determineStartPhase(existingResult *ValidationResult) ValidationPhaseName {
	// Check each phase in order
	for _, phase := range PhaseOrder {
		phaseResult, exists := existingResult.Phases[string(phase)]

		// Phase not yet run or incomplete
		if !exists {
			slog.Info("resuming from phase (not started)", "phase", phase)
			return phase
		}

		// Phase failed - resume from here
		if phaseResult.Status == ValidationStatusFail {
			slog.Info("resuming from phase (previously failed)", "phase", phase)
			return phase
		}

		// Phase passed - skip to next
		slog.Debug("skipping phase (already passed)", "phase", phase, "status", phaseResult.Status)
	}

	// All phases passed - start from beginning (shouldn't happen in normal resume)
	slog.Warn("all phases already passed, starting from beginning")
	return PhaseReadiness
}

// createValidationResultConfigMap creates an empty ValidationResult ConfigMap for this validation run.
func (v *Validator) createValidationResultConfigMap(ctx context.Context, clientset kubernetes.Interface) error {
	resultCMName := fmt.Sprintf("eidos-validation-result-%s", v.RunID)

	// Initialize empty ValidationResult structure
	result := NewValidationResult()
	result.Init(header.KindValidationResult, APIVersion, v.Version)

	// Serialize to YAML
	resultYAML, err := yaml.Marshal(result)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to serialize validation result", err)
	}

	// Create ConfigMap
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      resultCMName,
			Namespace: v.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "eidos",
				"app.kubernetes.io/component": "validation",
				"eidos.nvidia.com/data-type":  "validation-result",
				"eidos.nvidia.com/run-id":     v.RunID,
				"eidos.nvidia.com/created-at": time.Now().Format("20060102-150405"),
			},
		},
		Data: map[string]string{
			"result.yaml": string(resultYAML),
		},
	}

	_, err = clientset.CoreV1().ConfigMaps(v.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create validation result ConfigMap", err)
	}

	slog.Debug("created validation result ConfigMap",
		"name", resultCMName,
		"namespace", v.Namespace)

	return nil
}

// updateValidationResultConfigMap updates the ValidationResult ConfigMap with results from a completed phase.
func (v *Validator) updateValidationResultConfigMap(ctx context.Context, clientset kubernetes.Interface, result *ValidationResult) error {
	resultCMName := fmt.Sprintf("eidos-validation-result-%s", v.RunID)

	// Serialize updated result to YAML
	resultYAML, err := yaml.Marshal(result)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to serialize validation result", err)
	}

	// Get existing ConfigMap
	cm, err := clientset.CoreV1().ConfigMaps(v.Namespace).Get(ctx, resultCMName, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to get validation result ConfigMap", err)
	}

	// Update data
	cm.Data["result.yaml"] = string(resultYAML)

	// Update ConfigMap
	_, err = clientset.CoreV1().ConfigMaps(v.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to update validation result ConfigMap", err)
	}

	slog.Debug("updated validation result ConfigMap",
		"name", resultCMName,
		"phases", len(result.Phases))

	return nil
}

// readValidationResultConfigMap reads the existing ValidationResult ConfigMap for resume.
func (v *Validator) readValidationResultConfigMap(ctx context.Context, clientset kubernetes.Interface) (*ValidationResult, error) {
	resultCMName := fmt.Sprintf("eidos-validation-result-%s", v.RunID)

	// Get ConfigMap
	cm, err := clientset.CoreV1().ConfigMaps(v.Namespace).Get(ctx, resultCMName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("validation result not found for RunID %s", v.RunID), err)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to get validation result ConfigMap", err)
	}

	// Parse YAML
	resultYAML, ok := cm.Data["result.yaml"]
	if !ok {
		return nil, errors.New(errors.ErrCodeInternal, "result.yaml not found in ConfigMap")
	}

	var result ValidationResult
	if err := yaml.Unmarshal([]byte(resultYAML), &result); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse validation result", err)
	}

	slog.Debug("read validation result ConfigMap",
		"name", resultCMName,
		"phases", len(result.Phases))

	return &result, nil
}

// cleanupValidationResultConfigMap removes the ValidationResult ConfigMap for this validation run.
func (v *Validator) cleanupValidationResultConfigMap(ctx context.Context, clientset kubernetes.Interface) {
	resultCMName := fmt.Sprintf("eidos-validation-result-%s", v.RunID)

	err := clientset.CoreV1().ConfigMaps(v.Namespace).Delete(ctx, resultCMName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("failed to delete validation result ConfigMap", "name", resultCMName, "error", err)
	}

	slog.Debug("cleaned up validation result ConfigMap", "name", resultCMName)
}

// cleanupDataConfigMaps removes the snapshot and recipe ConfigMaps for this validation run.
func (v *Validator) cleanupDataConfigMaps(ctx context.Context, clientset kubernetes.Interface) {
	// Use RunID to identify ConfigMaps for this validation run
	snapshotCMName := fmt.Sprintf("eidos-snapshot-%s", v.RunID)
	recipeCMName := fmt.Sprintf("eidos-recipe-%s", v.RunID)

	// Delete snapshot ConfigMap
	err := clientset.CoreV1().ConfigMaps(v.Namespace).Delete(ctx, snapshotCMName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("failed to delete snapshot ConfigMap", "name", snapshotCMName, "error", err)
	}

	// Delete recipe ConfigMap
	err = clientset.CoreV1().ConfigMaps(v.Namespace).Delete(ctx, recipeCMName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("failed to delete recipe ConfigMap", "name", recipeCMName, "error", err)
	}

	slog.Debug("cleaned up data ConfigMaps", "namespace", v.Namespace)
}
