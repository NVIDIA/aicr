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
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/header"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/agent"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
)

// checkNameToTestName converts a check name to a test function name.
// Handles '-', '.', and '_' separators for consistency with patternToFuncName.
// Example: "operator-health" -> "TestOperatorHealth"
func checkNameToTestName(checkName string) string {
	parts := strings.FieldsFunc(checkName, func(r rune) bool {
		return r == '-' || r == '.' || r == '_'
	})
	for i, part := range parts {
		if len(part) > 0 {
			parts[i] = strings.ToUpper(string(part[0])) + part[1:]
		}
	}
	return "Test" + strings.Join(parts, "")
}

// parseConstraintResult extracts constraint validation results from test output.
// It looks for lines matching the pattern:
// CONSTRAINT_RESULT: name=<name> expected=<expected> actual=<actual> passed=<bool>
// Values can contain spaces, so we parse more carefully using regexp.
func parseConstraintResult(output []string) *ConstraintValidation {
	for _, line := range output {
		if !strings.Contains(line, "CONSTRAINT_RESULT:") {
			continue
		}

		// Extract the part after "CONSTRAINT_RESULT:"
		parts := strings.SplitN(line, "CONSTRAINT_RESULT:", 2)
		if len(parts) != 2 {
			continue
		}

		fields := strings.TrimSpace(parts[1])

		// Parse key=value pairs more carefully to handle multi-word values
		// Format: name=X expected=Y actual=Z passed=B
		// We need to find the start of each key and extract until the next key
		result := &ConstraintValidation{}

		// Find each field by looking for the key patterns
		nameIdx := strings.Index(fields, "name=")
		expectedIdx := strings.Index(fields, " expected=")
		actualIdx := strings.Index(fields, " actual=")
		passedIdx := strings.Index(fields, " passed=")

		if nameIdx >= 0 && expectedIdx > nameIdx && actualIdx > expectedIdx && passedIdx > actualIdx {
			// Extract name (from "name=" to " expected=")
			result.Name = strings.TrimSpace(fields[nameIdx+5 : expectedIdx])

			// Extract expected (from " expected=" to " actual=")
			result.Expected = strings.TrimSpace(fields[expectedIdx+10 : actualIdx])

			// Extract actual (from " actual=" to " passed=")
			result.Actual = strings.TrimSpace(fields[actualIdx+8 : passedIdx])

			// Extract passed (from " passed=" to end)
			passedValue := strings.TrimSpace(fields[passedIdx+8:])
			if passedValue == "true" {
				result.Status = ConstraintStatusPassed
			} else {
				result.Status = ConstraintStatusFailed
			}

			// Only return if we found all required fields
			if result.Name != "" && result.Expected != "" && result.Actual != "" {
				return result
			}
		}
	}

	return nil
}

// extractArtifacts separates artifact lines from regular test output.
// ARTIFACT: lines are base64-encoded JSON produced by TestRunner.Cancel().
// t.Logf prefixes output with source location (e.g. "runner.go:102: ARTIFACT:..."),
// so we use Contains + SplitN (same approach as CONSTRAINT_RESULT parsing).
// Lines that contain ARTIFACT: but fail to decode are preserved in reason output
// and a warning is logged, so debugging context is never silently lost.
func extractArtifacts(output []string) ([]checks.Artifact, []string) {
	var artifacts []checks.Artifact
	var reasonLines []string
	for _, line := range output {
		if !strings.Contains(line, "ARTIFACT:") {
			reasonLines = append(reasonLines, line)
			continue
		}
		parts := strings.SplitN(line, "ARTIFACT:", 2)
		if len(parts) != 2 {
			reasonLines = append(reasonLines, line)
			continue
		}
		encoded := strings.TrimSpace(parts[1])
		a, err := checks.DecodeArtifact(encoded)
		if err != nil {
			slog.Warn("failed to decode artifact line", "error", err)
			reasonLines = append(reasonLines, line)
			continue
		}
		artifacts = append(artifacts, *a)
	}
	return artifacts, reasonLines
}

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
		// Try to capture Job logs before cleanup
		logs, logErr := deployer.GetPodLogs(ctx)
		if logErr != nil {
			slog.Warn("failed to capture Job logs", "job", config.JobName, "error", logErr)
		} else if logs != "" {
			// Output logs to stderr for debugging
			slog.Info("validation job logs", "job", config.JobName, "logs", logs)
		}

		// Cleanup failed Job (only if cleanup enabled)
		if v.Cleanup {
			if cleanupErr := deployer.CleanupJob(ctx); cleanupErr != nil {
				slog.Warn("failed to cleanup Job after failure", "job", config.JobName, "error", cleanupErr)
			}
			// Wait for pod to fully terminate before returning.
			// Prevents deferred CleanupRBAC from racing with in-progress pod
			// operations (e.g., chainsaw namespace cleanup needs ClusterRole).
			termCtx, termCancel := context.WithTimeout(context.Background(), defaults.K8sPodTerminationWaitTimeout)
			defer termCancel()
			deployer.WaitForJobPodTermination(termCtx) //nolint:contextcheck // Fresh context: parent may have timed out
		} else {
			slog.Info("cleanup disabled, keeping failed Job for debugging", "job", config.JobName)
		}

		// Build error reason with log snippet
		reason := fmt.Sprintf("Job failed or timed out: %v", err)
		if logs != "" {
			// Include last 10 lines of logs in reason for context
			logLines := strings.Split(strings.TrimSpace(logs), "\n")
			lastLines := logLines
			if len(logLines) > 10 {
				lastLines = logLines[len(logLines)-10:]
			}
			reason += fmt.Sprintf("\n\nLast %d lines of Job output:\n%s", len(lastLines), strings.Join(lastLines, "\n"))
		}

		result.Status = ValidationStatusFail
		result.Checks = append(result.Checks, CheckResult{
			Name:   phaseName,
			Status: ValidationStatusFail,
			Reason: reason,
		})
		return result
	}

	// Get aggregated results from Job
	jobResult, err := deployer.GetResult(ctx)
	if err != nil {
		// Cleanup Job (only if cleanup enabled)
		if v.Cleanup {
			if cleanupErr := deployer.CleanupJob(ctx); cleanupErr != nil {
				slog.Warn("failed to cleanup Job", "job", config.JobName, "error", cleanupErr)
			}
		} else {
			slog.Info("cleanup disabled, keeping Job for debugging", "job", config.JobName)
		}
		result.Status = ValidationStatusFail
		result.Checks = append(result.Checks, CheckResult{
			Name:   phaseName,
			Status: ValidationStatusFail,
			Reason: fmt.Sprintf("failed to retrieve result: %v", err),
		})
		return result
	}

	// Log test count for debugging (mismatch check temporarily disabled during development)
	actualTests := len(jobResult.Tests)
	if config.ExpectedTests > 0 && actualTests != config.ExpectedTests {
		slog.Warn("test count mismatch (non-fatal)",
			"expected", config.ExpectedTests,
			"actual", actualTests,
			"pattern", config.TestPattern)
	}

	// Parse individual test results from go test JSON output
	// Each test becomes a separate CheckResult for granular reporting
	if len(jobResult.Tests) > 0 {
		for _, test := range jobResult.Tests {
			checkResult := CheckResult{
				Name:     test.Name,
				Status:   mapTestStatusToValidationStatus(test.Status),
				Duration: test.Duration,
			}

			// Parse constraint results from test output
			// Look for lines like: CONSTRAINT_RESULT: name=X expected=Y actual=Z passed=true
			constraintResult := parseConstraintResult(test.Output)
			if constraintResult != nil {
				result.Constraints = append(result.Constraints, *constraintResult)
			}

			// Extract artifacts from test output and build reason from remaining lines.
			artifacts, reasonLines := extractArtifacts(test.Output)
			checkResult.Artifacts = artifacts

			// Build reason from last few non-artifact output lines
			if len(reasonLines) > 0 {
				maxLines := 5
				startIdx := len(reasonLines) - maxLines
				if startIdx < 0 {
					startIdx = 0
				}
				checkResult.Reason = strings.Join(reasonLines[startIdx:], "\n")
			} else {
				checkResult.Reason = fmt.Sprintf("Test %s: %s", test.Status, test.Name)
			}

			result.Checks = append(result.Checks, checkResult)
		}
	} else if config.ExpectedTests == 0 {
		// Fallback: no individual tests parsed and no expected tests, return phase-level result
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

	// Cleanup Job after successful completion (only if cleanup enabled)
	if v.Cleanup {
		if err := deployer.CleanupJob(ctx); err != nil {
			slog.Warn("failed to cleanup Job", "job", config.JobName, "error", err)
		}
	} else {
		slog.Info("cleanup disabled, keeping Job for debugging", "job", config.JobName)
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
	result.RunID = v.RunID
	overallStatus := ValidationStatusPass

	// Create Kubernetes client for agent deployment
	// If Kubernetes is not available (e.g., running in test mode), phases will skip Job execution
	clientset, _, err := k8sclient.GetKubeClient()
	rbacAvailable := err == nil && !v.NoCluster

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
			ServiceAccountName: "aicr-validator",
			Image:              v.Image,
			ImagePullSecrets:   v.ImagePullSecrets,
		}
		deployer := agent.NewDeployer(clientset, sharedConfig)

		// Ensure RBAC once at the start (idempotent - safe to call multiple times)
		slog.Debug("creating shared RBAC for all validation phases")
		if rbacErr := deployer.EnsureRBAC(ctx); rbacErr != nil {
			slog.Warn("failed to create validation RBAC, check execution will be skipped", "error", rbacErr)
		} else if v.Cleanup {
			// Cleanup RBAC at the end (deferred to ensure cleanup even on error, only if cleanup enabled)
			//nolint:contextcheck // Using separate context for cleanup to avoid cancellation
			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
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
			// Always cleanup data ConfigMaps (recipe/snapshot) - these are internal
			//nolint:contextcheck // Using separate context for cleanup to avoid cancellation
			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
				defer cancel()
				v.cleanupDataConfigMaps(cleanupCtx, clientset)
			}()
		}

		// Create ValidationResult ConfigMap for progressive updates
		slog.Debug("creating ValidationResult ConfigMap for tracking progress")
		if resultErr := v.createValidationResultConfigMap(ctx, clientset); resultErr != nil {
			slog.Warn("failed to create validation result ConfigMap", "error", resultErr)
		} else if v.Cleanup {
			// Cleanup ValidationResult ConfigMap at the end (only if cleanup enabled)
			//nolint:contextcheck // Using separate context for cleanup to avoid cancellation
			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
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
		phaseResultDoc, err := v.validatePhaseOnly(ctx, phase, recipeResult, snap)
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

	// Calculate overall summary by phase status
	totalPassed := 0
	totalFailed := 0
	totalSkipped := 0

	for _, phaseResult := range result.Phases {
		switch phaseResult.Status {
		case ValidationStatusPass:
			totalPassed++
		case ValidationStatusFail:
			totalFailed++
		case ValidationStatusSkipped:
			totalSkipped++
		case ValidationStatusWarning, ValidationStatusPartial:
			// Warnings and partial statuses are not expected at phase level
		}
	}

	result.Summary.Status = overallStatus
	result.Summary.Passed = totalPassed
	result.Summary.Failed = totalFailed
	result.Summary.Skipped = totalSkipped
	result.Summary.Total = len(result.Phases)
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
