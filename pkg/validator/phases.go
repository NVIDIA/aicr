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

package validator

import (
	"context"
	"log/slog"
	"time"

	"github.com/NVIDIA/eidos/pkg/errors"
	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/snapshotter"
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

// ValidatePhase runs validation for a specific phase.
// This is the main entry point for phase-based validation.
func (v *Validator) ValidatePhase(
	ctx context.Context,
	phase ValidationPhaseName,
	recipeResult *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
) (*ValidationResult, error) {

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
		return v.validateAll(ctx, recipeResult, snap)
	default:
		return v.validateReadiness(ctx, recipeResult, snap)
	}
}

// validateReadiness validates readiness phase.
// Skeleton implementation - just passes all checks.
func (v *Validator) validateReadiness(
	ctx context.Context,
	recipeResult *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
) (*ValidationResult, error) {

	_ = ctx // Context will be used when real checks are implemented
	start := time.Now()
	slog.Info("running readiness validation phase")

	result := NewValidationResult()
	phaseResult := &PhaseResult{
		Status:      ValidationStatusPass,
		Constraints: []ConstraintValidation{},
		Checks:      []CheckResult{},
	}

	// Evaluate recipe-level constraints (spec.constraints)
	for _, constraint := range recipeResult.Constraints {
		cv := v.evaluateConstraint(constraint, snap)
		phaseResult.Constraints = append(phaseResult.Constraints, cv)
	}

	// Run named checks if defined in validation config
	if recipeResult.Validation != nil && recipeResult.Validation.PreDeployment != nil {
		for _, checkName := range recipeResult.Validation.PreDeployment.Checks {
			check := CheckResult{
				Name:   checkName,
				Status: ValidationStatusPass,
				Reason: "skeleton implementation - check not yet implemented",
			}
			phaseResult.Checks = append(phaseResult.Checks, check)
			slog.Debug("readiness check passed (skeleton)", "check", checkName)
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
// Skeleton implementation - just passes.
func (v *Validator) validateDeployment(
	ctx context.Context,
	recipeResult *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
) (*ValidationResult, error) {

	_ = ctx  // Context will be used when real checks are implemented
	_ = snap // Snapshot will be used when real checks are implemented
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
	} else {
		// Evaluate phase-level constraints
		for _, constraint := range recipeResult.Validation.Deployment.Constraints {
			cv := CheckResult{
				Name:   constraint.Name,
				Status: ValidationStatusPass,
				Reason: "skeleton implementation - always passes",
			}
			phaseResult.Checks = append(phaseResult.Checks, cv)
		}

		// Run named checks
		for _, checkName := range recipeResult.Validation.Deployment.Checks {
			check := CheckResult{
				Name:   checkName,
				Status: ValidationStatusPass,
				Reason: "skeleton implementation - check not yet implemented",
			}
			phaseResult.Checks = append(phaseResult.Checks, check)
			slog.Debug("deployment check passed (skeleton)", "check", checkName)
		}
	}

	phaseResult.Duration = time.Since(start)
	result.Phases[string(PhaseDeployment)] = phaseResult

	// Update summary
	result.Summary.Status = phaseResult.Status
	result.Summary.Total = len(phaseResult.Checks)
	result.Summary.Passed = len(phaseResult.Checks)
	result.Summary.Duration = phaseResult.Duration

	slog.Info("deployment validation completed",
		"status", phaseResult.Status,
		"checks", len(phaseResult.Checks),
		"duration", phaseResult.Duration)

	return result, nil
}

// validatePerformance validates performance phase.
// Skeleton implementation - just passes.
func (v *Validator) validatePerformance(
	ctx context.Context,
	recipeResult *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
) (*ValidationResult, error) {

	_ = ctx  // Context will be used when real checks are implemented
	_ = snap // Snapshot will be used when real checks are implemented
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
		// Run named checks
		for _, checkName := range recipeResult.Validation.Performance.Checks {
			check := CheckResult{
				Name:   checkName,
				Status: ValidationStatusPass,
				Reason: "skeleton implementation - check not yet implemented",
			}
			phaseResult.Checks = append(phaseResult.Checks, check)
			slog.Debug("performance check passed (skeleton)", "check", checkName)
		}

		// Log infrastructure component if specified
		if recipeResult.Validation.Performance.Infrastructure != "" {
			slog.Debug("performance infrastructure specified",
				"component", recipeResult.Validation.Performance.Infrastructure)
		}
	}

	phaseResult.Duration = time.Since(start)
	result.Phases[string(PhasePerformance)] = phaseResult

	// Update summary
	result.Summary.Status = phaseResult.Status
	result.Summary.Total = len(phaseResult.Checks)
	result.Summary.Passed = len(phaseResult.Checks)
	result.Summary.Duration = phaseResult.Duration

	slog.Info("performance validation completed",
		"status", phaseResult.Status,
		"checks", len(phaseResult.Checks),
		"duration", phaseResult.Duration)

	return result, nil
}

// validateConformance validates conformance phase.
// Skeleton implementation - just passes.
func (v *Validator) validateConformance(
	ctx context.Context,
	recipeResult *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
) (*ValidationResult, error) {

	_ = ctx  // Context will be used when real checks are implemented
	_ = snap // Snapshot will be used when real checks are implemented
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
	} else {
		// Run named checks
		for _, checkName := range recipeResult.Validation.Conformance.Checks {
			check := CheckResult{
				Name:   checkName,
				Status: ValidationStatusPass,
				Reason: "skeleton implementation - check not yet implemented",
			}
			phaseResult.Checks = append(phaseResult.Checks, check)
			slog.Debug("conformance check passed (skeleton)", "check", checkName)
		}
	}

	phaseResult.Duration = time.Since(start)
	result.Phases[string(PhaseConformance)] = phaseResult

	// Update summary
	result.Summary.Status = phaseResult.Status
	result.Summary.Total = len(phaseResult.Checks)
	result.Summary.Passed = len(phaseResult.Checks)
	result.Summary.Duration = phaseResult.Duration

	slog.Info("conformance validation completed",
		"status", phaseResult.Status,
		"checks", len(phaseResult.Checks),
		"duration", phaseResult.Duration)

	return result, nil
}

// validateAll runs all phases sequentially with dependency logic.
// If a phase fails, subsequent phases are skipped.
func (v *Validator) validateAll(
	ctx context.Context,
	recipeResult *recipe.RecipeResult,
	snap *snapshotter.Snapshot,
) (*ValidationResult, error) {

	start := time.Now()
	slog.Info("running all validation phases")

	result := NewValidationResult()
	overallStatus := ValidationStatusPass

	// Phase order: readiness → deployment → performance → conformance
	phases := []ValidationPhaseName{
		PhaseReadiness,
		PhaseDeployment,
		PhasePerformance,
		PhaseConformance,
	}

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
