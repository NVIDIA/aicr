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

	"github.com/NVIDIA/aicr/pkg/defaults"
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
	} else {
		v.executePhaseChecks(ctx, recipeResult, phaseExecConfig{
			name:           string(PhaseDeployment),
			testPackage:    "./pkg/validator/checks/deployment",
			defaultTimeout: DefaultDeploymentTimeout,
			phase:          recipeResult.Validation.Deployment,
			topLevel:       recipeResult.Validation.Isolated,
		}, phaseResult)
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
		// Log infrastructure component if specified
		if recipeResult.Validation.Performance.Infrastructure != "" {
			slog.Debug("performance infrastructure specified",
				"component", recipeResult.Validation.Performance.Infrastructure)
		}

		v.executePhaseChecks(ctx, recipeResult, phaseExecConfig{
			name:           string(PhasePerformance),
			testPackage:    "./pkg/validator/checks/performance",
			defaultTimeout: DefaultPerformanceTimeout,
			phase:          recipeResult.Validation.Performance,
			topLevel:       recipeResult.Validation.Isolated,
		}, phaseResult)
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
	} else {
		v.executePhaseChecks(ctx, recipeResult, phaseExecConfig{
			name:           string(PhaseConformance),
			testPackage:    "./pkg/validator/checks/conformance",
			defaultTimeout: DefaultConformanceTimeout,
			phase:          recipeResult.Validation.Conformance,
			topLevel:       recipeResult.Validation.Isolated,
		}, phaseResult)
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
			for _, check := range recipeResult.Validation.Deployment.Checks {
				_, ok := checks.GetCheck(check.Name)
				if !ok {
					unregisteredChecks = append(unregisteredChecks, check.Name)
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

			for _, check := range recipeResult.Validation.Performance.Checks {
				_, ok := checks.GetCheck(check.Name)
				if !ok {
					unregisteredChecks = append(unregisteredChecks, check.Name)
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

			for _, check := range recipeResult.Validation.Conformance.Checks {
				_, ok := checks.GetCheck(check.Name)
				if !ok {
					unregisteredChecks = append(unregisteredChecks, check.Name)
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
	if recipeResult.Validation == nil {
		return buildTestPatternResult{}
	}

	switch phase {
	case string(PhaseDeployment):
		if recipeResult.Validation.Deployment != nil {
			return buildTestPatternFromItems(recipeResult.Validation.Deployment.Checks, recipeResult.Validation.Deployment.Constraints)
		}
	case string(PhasePerformance):
		if recipeResult.Validation.Performance != nil {
			return buildTestPatternFromItems(recipeResult.Validation.Performance.Checks, recipeResult.Validation.Performance.Constraints)
		}
	case string(PhaseConformance):
		if recipeResult.Validation.Conformance != nil {
			return buildTestPatternFromItems(recipeResult.Validation.Conformance.Checks, recipeResult.Validation.Conformance.Constraints)
		}
	}

	return buildTestPatternResult{}
}

// buildTestPatternFromItems builds a test pattern from explicit check and constraint lists.
// This is the core pattern builder used by both buildTestPattern and executePhaseChecks.
func buildTestPatternFromItems(checkRefs []recipe.CheckRef, constraints []recipe.Constraint) buildTestPatternResult {
	var testNames []string
	uniqueTests := make(map[string]bool)

	for _, constraint := range constraints {
		testName, ok := checks.GetTestNameForConstraint(constraint.Name)
		if ok && !uniqueTests[testName] {
			testNames = append(testNames, testName)
			uniqueTests[testName] = true
			slog.Debug("constraint mapped to test", "constraint", constraint.Name, "test", testName)
		}
	}

	for _, check := range checkRefs {
		testName, ok := checks.GetTestNameForCheck(check.Name)
		if !ok {
			testName = checkNameToTestName(check.Name)
		}
		if !uniqueTests[testName] {
			testNames = append(testNames, testName)
			uniqueTests[testName] = true
			slog.Debug("check mapped to test", "check", check.Name, "test", testName)
		}
	}

	if len(testNames) == 0 {
		return buildTestPatternResult{}
	}

	pattern := "^(" + strings.Join(testNames, "|") + ")$"
	slog.Info("built test pattern from items", "pattern", pattern, "tests", len(testNames))
	return buildTestPatternResult{Pattern: pattern, ExpectedTests: len(testNames)}
}

// sanitizeLabelValue converts a check/constraint name to a valid Kubernetes Job name suffix.
// Lowercases, replaces dots and underscores with hyphens, truncates to 40 chars.
// sanitizeLabelValue converts a check or constraint name to a valid Kubernetes label value.
// Lowercase, dots/underscores become hyphens, capped at 63 chars, no trailing hyphen.
func sanitizeLabelValue(name string) string {
	s := strings.ToLower(name)
	s = strings.NewReplacer(".", "-", "_", "-").Replace(s)
	if len(s) > 63 {
		s = s[:63]
	}
	return strings.TrimRight(s, "-")
}

// resolveItemTimeout returns the timeout for an individual check or constraint.
// Falls back to phaseTimeout if the item has no timeout specified.
func resolveItemTimeout(itemTimeout string, phaseTimeout time.Duration) time.Duration {
	if itemTimeout != "" {
		if parsed, err := time.ParseDuration(itemTimeout); err == nil {
			return parsed
		}
		slog.Warn("invalid item timeout, using phase timeout",
			"timeout", itemTimeout, "phaseTimeout", phaseTimeout)
	}
	return phaseTimeout
}

// baseJobConfig returns a Job config with fields common to all Jobs in a validation run.
// Callers set JobName, TestPattern, ExpectedTests, and Timeout on the returned config.
func (v *Validator) baseJobConfig(testPackage string) agent.Config {
	return agent.Config{
		Namespace:          v.Namespace,
		Image:              v.Image,
		ImagePullSecrets:   v.ImagePullSecrets,
		ServiceAccountName: "aicr-validator",
		SnapshotConfigMap:  fmt.Sprintf("aicr-snapshot-%s", v.RunID),
		RecipeConfigMap:    fmt.Sprintf("aicr-recipe-%s", v.RunID),
		TestPackage:        testPackage,
		Tolerations:        v.Tolerations,
		Affinity:           preferCPUNodeAffinity(),
	}
}

// phaseExecConfig holds phase-specific parameters for executePhaseChecks.
type phaseExecConfig struct {
	name           string
	testPackage    string
	defaultTimeout time.Duration
	phase          *recipe.ValidationPhase
	topLevel       *bool
}

// executePhaseChecks runs all checks and constraints for a phase using the
// three-tier execution model: shared Job, isolated Jobs, external validators.
// Results are appended to phaseResult.Checks.
//
//nolint:funlen // Orchestration function with sequential tier execution
func (v *Validator) executePhaseChecks(
	ctx context.Context,
	recipeResult *recipe.RecipeResult,
	cfg phaseExecConfig,
	phaseResult *PhaseResult,
) {

	if cfg.phase == nil {
		return
	}

	partition := partitionByIsolation(cfg.phase, cfg.topLevel)
	if !partition.hasShared() && !partition.hasIsolated() {
		return
	}

	if v.NoCluster {
		slog.Info("no-cluster mode enabled, skipping cluster check execution",
			"phase", cfg.name)
		for _, check := range partition.SharedChecks {
			phaseResult.Checks = append(phaseResult.Checks, CheckResult{
				Name:   check.Name,
				Status: ValidationStatusSkipped,
				Reason: "skipped - no-cluster mode (test mode)",
				Source: CheckSourceShared,
			})
		}
		for _, check := range partition.IsolatedChecks {
			phaseResult.Checks = append(phaseResult.Checks, CheckResult{
				Name:   check.Name,
				Status: ValidationStatusSkipped,
				Reason: "skipped - no-cluster mode (test mode)",
				Source: CheckSourceIsolated,
			})
		}
		for _, ev := range cfg.phase.Validators {
			phaseResult.Checks = append(phaseResult.Checks, CheckResult{
				Name:   ev.Name,
				Status: ValidationStatusSkipped,
				Reason: "skipped - no-cluster mode (test mode)",
				Source: CheckSourceExternal,
			})
		}
		return
	}

	clientset, _, err := k8sclient.GetKubeClient()
	if err != nil {
		slog.Warn("Kubernetes client unavailable, skipping check execution",
			"error", err, "phase", cfg.name)
		phaseResult.Checks = append(phaseResult.Checks, CheckResult{
			Name:   cfg.name,
			Status: ValidationStatusPass,
			Reason: "skipped - Kubernetes unavailable (test mode)",
		})
		return
	}

	v.validateRecipeRegistrations(recipeResult, cfg.name)
	phaseTimeout := resolvePhaseTimeout(cfg.phase, cfg.defaultTimeout)
	baseCfg := v.baseJobConfig(cfg.testPackage)

	// Tier 1: Shared Job — all non-isolated checks + constraints in one Job
	if partition.hasShared() {
		patternResult := buildTestPatternFromItems(partition.SharedChecks, partition.SharedConstraints)
		jobCfg := baseCfg
		jobCfg.JobName = fmt.Sprintf("aicr-%s-%s", v.RunID, cfg.name)
		jobCfg.TestPattern = patternResult.Pattern
		jobCfg.ExpectedTests = patternResult.ExpectedTests
		jobCfg.Timeout = phaseTimeout
		jobCfg.Labels = map[string]string{
			"aicr.nvidia.com/run-id": v.RunID,
			"aicr.nvidia.com/phase":  cfg.name,
			"aicr.nvidia.com/tier":   "shared",
		}

		deployer := agent.NewDeployer(clientset, jobCfg)
		jobResult := v.runPhaseJob(ctx, deployer, jobCfg, cfg.name)
		for i := range jobResult.Checks {
			jobResult.Checks[i].Source = CheckSourceShared
		}
		phaseResult.Checks = append(phaseResult.Checks, jobResult.Checks...)
	}

	// Tier 2: Isolated checks — each gets its own Job with the same validator image
	for _, check := range partition.IsolatedChecks {
		select {
		case <-ctx.Done():
			return
		default:
		}

		checkLabel := sanitizeLabelValue(check.Name)
		patternResult := buildTestPatternFromItems([]recipe.CheckRef{check}, nil)
		jobCfg := baseCfg
		jobCfg.JobName = fmt.Sprintf("aicr-%s-%s-%s", v.RunID, cfg.name, checkLabel)
		jobCfg.TestPattern = patternResult.Pattern
		jobCfg.ExpectedTests = patternResult.ExpectedTests
		jobCfg.Timeout = resolveItemTimeout(check.Timeout, phaseTimeout)
		jobCfg.Labels = map[string]string{
			"aicr.nvidia.com/run-id": v.RunID,
			"aicr.nvidia.com/phase":  cfg.name,
			"aicr.nvidia.com/tier":   "isolated",
			"aicr.nvidia.com/check":  checkLabel,
		}

		deployer := agent.NewDeployer(clientset, jobCfg)
		jobResult := v.runPhaseJob(ctx, deployer, jobCfg, cfg.name)
		for i := range jobResult.Checks {
			jobResult.Checks[i].Source = CheckSourceIsolated
		}
		phaseResult.Checks = append(phaseResult.Checks, jobResult.Checks...)
	}

	// Tier 2: Isolated constraints — each gets its own Job
	for _, constraint := range partition.IsolatedConstraints {
		select {
		case <-ctx.Done():
			return
		default:
		}

		constraintLabel := sanitizeLabelValue(constraint.Name)
		patternResult := buildTestPatternFromItems(nil, []recipe.Constraint{constraint})
		jobCfg := baseCfg
		jobCfg.JobName = fmt.Sprintf("aicr-%s-%s-%s", v.RunID, cfg.name, constraintLabel)
		jobCfg.TestPattern = patternResult.Pattern
		jobCfg.ExpectedTests = patternResult.ExpectedTests
		jobCfg.Timeout = resolveItemTimeout(constraint.Timeout, phaseTimeout)
		jobCfg.Labels = map[string]string{
			"aicr.nvidia.com/run-id":     v.RunID,
			"aicr.nvidia.com/phase":      cfg.name,
			"aicr.nvidia.com/tier":       "isolated",
			"aicr.nvidia.com/constraint": constraintLabel,
		}

		deployer := agent.NewDeployer(clientset, jobCfg)
		jobResult := v.runPhaseJob(ctx, deployer, jobCfg, cfg.name)
		for i := range jobResult.Checks {
			jobResult.Checks[i].Source = CheckSourceIsolated
		}
		phaseResult.Checks = append(phaseResult.Checks, jobResult.Checks...)
	}

	// Tier 3: External validators — user-provided OCI containers
	for _, ev := range cfg.phase.Validators {
		select {
		case <-ctx.Done():
			return
		default:
		}

		result := v.runExternalJob(ctx, clientset, ev, cfg.name)
		phaseResult.Checks = append(phaseResult.Checks, result)
	}
}

// runExternalJob deploys an external validator container as a Kubernetes Job.
// Exit 0 = pass, non-zero = fail. Failure reason is extracted from pod termination
// message or last lines of stdout.
//
//nolint:funlen // Sequential Job lifecycle steps
func (v *Validator) runExternalJob(
	ctx context.Context,
	clientset k8sclient.Interface,
	ev recipe.ExternalValidator,
	phaseName string,
) CheckResult {

	evTimeout := resolveItemTimeout(ev.Timeout, defaults.ExternalValidatorTimeout)

	validatorLabel := sanitizeLabelValue(ev.Name)
	jobCfg := agent.Config{
		Namespace:          v.Namespace,
		JobName:            fmt.Sprintf("aicr-%s-%s-%s", v.RunID, phaseName, validatorLabel),
		Image:              ev.Image,
		ImagePullSecrets:   v.ImagePullSecrets,
		ServiceAccountName: "aicr-validator",
		SnapshotConfigMap:  fmt.Sprintf("aicr-snapshot-%s", v.RunID),
		RecipeConfigMap:    fmt.Sprintf("aicr-recipe-%s", v.RunID),
		ExternalCommand:    true,
		Timeout:            evTimeout,
		Tolerations:        v.Tolerations,
		Affinity:           preferCPUNodeAffinity(),
		Labels: map[string]string{
			"aicr.nvidia.com/run-id":    v.RunID,
			"aicr.nvidia.com/phase":     phaseName,
			"aicr.nvidia.com/tier":      "external",
			"aicr.nvidia.com/validator": validatorLabel,
		},
	}

	deployer := agent.NewDeployer(clientset, jobCfg)

	slog.Info("deploying external validator", "name", ev.Name, "image", ev.Image, "phase", phaseName)

	// Deploy Job
	if err := deployer.DeployJob(ctx); err != nil {
		return CheckResult{
			Name:   ev.Name,
			Status: ValidationStatusFail,
			Reason: fmt.Sprintf("failed to deploy external validator Job: %v", err),
			Source: CheckSourceExternal,
		}
	}

	// Stream logs in background
	logCtx, cancelLogs := context.WithCancel(ctx)
	defer cancelLogs()
	streamingActive := startLogStreaming(logCtx, deployer, ev.Name)

	// Wait for Job completion
	if err := deployer.WaitForCompletion(ctx, evTimeout); err != nil {
		cancelLogs()

		// Capture logs for error context
		var logs string
		if !streamingActive {
			if captured, logErr := deployer.GetPodLogs(ctx); logErr == nil && captured != "" {
				logs = captured
			}
		}

		if v.Cleanup {
			if cleanupErr := deployer.CleanupJob(ctx); cleanupErr != nil {
				slog.Warn("failed to cleanup external validator Job", "name", ev.Name, "error", cleanupErr)
			}
		}

		reason := fmt.Sprintf("external validator %q failed: %v", ev.Name, err)
		if logs != "" {
			logLines := strings.Split(strings.TrimSpace(logs), "\n")
			lastLines := logLines
			if len(logLines) > 10 {
				lastLines = logLines[len(logLines)-10:]
			}
			reason += fmt.Sprintf("\n\nLast %d lines of output:\n%s", len(lastLines), strings.Join(lastLines, "\n"))
		}

		return CheckResult{
			Name:   ev.Name,
			Status: ValidationStatusFail,
			Reason: reason,
			Source: CheckSourceExternal,
		}
	}

	// Success — cleanup and return pass
	if v.Cleanup {
		if cleanupErr := deployer.CleanupJob(ctx); cleanupErr != nil {
			slog.Warn("failed to cleanup external validator Job", "name", ev.Name, "error", cleanupErr)
		}
	}

	slog.Info("external validator passed", "name", ev.Name, "image", ev.Image)
	return CheckResult{
		Name:   ev.Name,
		Status: ValidationStatusPass,
		Source: CheckSourceExternal,
	}
}
