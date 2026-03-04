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
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/aicr/pkg/defaults"
	k8sclient "github.com/NVIDIA/aicr/pkg/k8s/client"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator/agent"
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

// Phase timeout aliases — defined in pkg/defaults/timeouts.go.
const (
	DefaultReadinessTimeout   = defaults.ValidateReadinessTimeout
	DefaultDeploymentTimeout  = defaults.ValidateDeploymentTimeout
	DefaultPerformanceTimeout = defaults.ValidatePerformanceTimeout
	DefaultConformanceTimeout = defaults.ValidateConformanceTimeout
)

// gpuPresentLabelKey is the node label set by GPU Feature Discovery when a GPU is present.
// Used in soft anti-affinity to prefer scheduling validator Jobs on CPU-only nodes.
const gpuPresentLabelKey = "nvidia.com/gpu.present"

// PhaseOrder defines the canonical execution order for validation phases.
// Readiness and deployment must run before performance or conformance.
var PhaseOrder = []ValidationPhaseName{
	PhaseReadiness,
	PhaseDeployment,
	PhasePerformance,
	PhaseConformance,
}

// resolvePhaseTimeout returns the timeout for a validation phase.
// If the recipe specifies a timeout for the phase, it is used; otherwise the default is used.
func resolvePhaseTimeout(phase *recipe.ValidationPhase, defaultTimeout time.Duration) time.Duration {
	if phase != nil && phase.Timeout != "" {
		parsed, err := time.ParseDuration(phase.Timeout)
		if err == nil {
			return parsed
		}
		slog.Warn("invalid phase timeout in recipe, using default",
			"timeout", phase.Timeout, "default", defaultTimeout, "error", err)
	}
	return defaultTimeout
}

// preferCPUNodeAffinity returns a soft node affinity that prefers nodes
// without nvidia.com/gpu.present labels. Validator Jobs only need K8s API
// access and should avoid consuming GPU resources in heterogeneous clusters.
// The preference is soft: if no CPU-only nodes exist, Jobs still schedule.
func preferCPUNodeAffinity() *corev1.Affinity {
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
				{
					Weight: 100,
					Preference: corev1.NodeSelectorTerm{
						MatchExpressions: []corev1.NodeSelectorRequirement{
							{
								Key:      gpuPresentLabelKey,
								Operator: corev1.NodeSelectorOpDoesNotExist,
							},
						},
					},
				},
			},
		},
	}
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
	if err == nil && !v.NoCluster {
		// Create RBAC resources for validation Jobs
		sharedConfig := agent.Config{
			Namespace:          v.Namespace,
			JobName:            "aicr-validator", // Shared ServiceAccount name
			ServiceAccountName: "aicr-validator",
		}
		deployer := agent.NewDeployer(clientset, sharedConfig)

		if rbacErr := deployer.EnsureRBAC(ctx); rbacErr != nil {
			slog.Debug("failed to create RBAC resources", "phase", phase, "error", rbacErr)
		} else if v.Cleanup {
			// Cleanup RBAC after phase completes (only if cleanup enabled)
			//nolint:contextcheck // Using separate context for cleanup to avoid cancellation
			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
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
			// Always cleanup data ConfigMaps (recipe/snapshot) - these are internal
			//nolint:contextcheck // Using separate context for cleanup to avoid cancellation
			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
				defer cancel()
				v.cleanupDataConfigMaps(cleanupCtx, clientset)
			}()
		}
	}

	// Run the requested phase (PhaseAll is handled by early return above)
	switch phase { //nolint:exhaustive // PhaseAll handled above
	case PhaseReadiness:
		return v.validateReadiness(ctx, recipeResult, snap)
	case PhaseDeployment:
		return v.validateDeployment(ctx, recipeResult, snap)
	case PhasePerformance:
		return v.validatePerformance(ctx, recipeResult, snap)
	case PhaseConformance:
		return v.validateConformance(ctx, recipeResult, snap)
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

	slog.Info("specified phases validation completed",
		"status", overallStatus,
		"phases", len(result.Phases),
		"passed", totalPassed,
		"failed", totalFailed,
		"skipped", totalSkipped,
		"duration", result.Summary.Duration)

	return result, nil
}
