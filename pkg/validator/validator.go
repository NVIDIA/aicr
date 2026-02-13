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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/NVIDIA/eidos/pkg/errors"
	"github.com/NVIDIA/eidos/pkg/header"
	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/snapshotter"
)

const (
	// APIVersion is the API version for validation results.
	APIVersion = "eidos.nvidia.com/v1alpha1"
)

// ConstraintEvalResult represents the result of evaluating a single constraint.
type ConstraintEvalResult struct {
	// Passed indicates if the constraint was satisfied.
	Passed bool

	// Actual is the actual value extracted from the snapshot.
	Actual string

	// Error contains the error if evaluation failed (e.g., value not found).
	Error error
}

// EvaluateConstraint evaluates a single constraint against a snapshot.
// This is a standalone function that can be used by other packages without
// creating a full Validator instance. Used by the recipe package to filter
// overlays based on constraint evaluation during snapshot-based recipe generation.
func EvaluateConstraint(constraint recipe.Constraint, snap *snapshotter.Snapshot) ConstraintEvalResult {
	result := ConstraintEvalResult{}

	// Parse the constraint path
	path, err := ParseConstraintPath(constraint.Name)
	if err != nil {
		result.Error = errors.Wrap(errors.ErrCodeInvalidRequest, "invalid constraint path", err)
		return result
	}

	// Extract the actual value from snapshot
	actual, err := path.ExtractValue(snap)
	if err != nil {
		result.Error = errors.Wrap(errors.ErrCodeNotFound, "value not found in snapshot", err)
		return result
	}
	result.Actual = actual

	// Parse the constraint expression
	parsed, err := ParseConstraintExpression(constraint.Value)
	if err != nil {
		result.Error = errors.Wrap(errors.ErrCodeInvalidRequest, "invalid constraint expression", err)
		return result
	}

	// Evaluate the constraint
	passed, err := parsed.Evaluate(actual)
	if err != nil {
		result.Error = errors.Wrap(errors.ErrCodeInternal, "evaluation failed", err)
		return result
	}

	result.Passed = passed
	return result
}

// Validator evaluates recipe constraints against snapshot measurements.
type Validator struct {
	// Version is the validator version (typically the CLI version).
	Version string

	// Namespace is the Kubernetes namespace where validation jobs will run.
	// Defaults to "eidos-validation" if not specified.
	Namespace string

	// Image is the container image to use for validation Jobs.
	// Must include Go toolchain for running tests.
	// Defaults to "ghcr.io/nvidia/eidos-validator:latest".
	Image string

	// RunID is a unique identifier for this validation run.
	// Used to scope all resources (ConfigMaps, Jobs) and enable resumability.
	// Format: YYYYMMDD-HHMMSS-RANDOM (e.g., "20260206-140523-a3f9")
	RunID string

	// Cleanup controls whether to delete Jobs, ConfigMaps, and RBAC resources after validation.
	// Defaults to true. Set to false to keep resources for debugging.
	Cleanup bool

	// ImagePullSecrets are secret names for pulling images from private registries.
	ImagePullSecrets []string
}

// Option is a functional option for configuring Validator instances.
type Option func(*Validator)

// WithVersion returns an Option that sets the Validator version string.
func WithVersion(version string) Option {
	return func(v *Validator) {
		v.Version = version
	}
}

// WithNamespace returns an Option that sets the namespace for validation jobs.
func WithNamespace(namespace string) Option {
	return func(v *Validator) {
		v.Namespace = namespace
	}
}

// WithImage returns an Option that sets the container image for validation Jobs.
func WithImage(image string) Option {
	return func(v *Validator) {
		v.Image = image
	}
}

// WithRunID returns an Option that sets the RunID for this validation run.
// Used when resuming a previous validation run.
func WithRunID(runID string) Option {
	return func(v *Validator) {
		v.RunID = runID
	}
}

// WithCleanup returns an Option that controls cleanup of validation resources.
// When false, Jobs, ConfigMaps, and RBAC resources are kept for debugging.
func WithCleanup(cleanup bool) Option {
	return func(v *Validator) {
		v.Cleanup = cleanup
	}
}

// WithImagePullSecrets returns an Option that sets image pull secrets for validation Jobs.
func WithImagePullSecrets(secrets []string) Option {
	return func(v *Validator) {
		v.ImagePullSecrets = secrets
	}
}

// generateRunID creates a unique identifier for a validation run.
// Format: YYYYMMDD-HHMMSS-RANDOM (e.g., "20260206-140523-a3f9b2c1e7d04a68")
func generateRunID() string {
	// Generate timestamp
	timestamp := time.Now().Format("20060102-150405")

	// Generate 16 random hex characters (8 bytes)
	randomBytes := make([]byte, 8)
	if _, err := rand.Read(randomBytes); err != nil {
		// Fallback to timestamp only if random generation fails
		return timestamp
	}
	randomHex := hex.EncodeToString(randomBytes)

	return fmt.Sprintf("%s-%s", timestamp, randomHex)
}

// New creates a new Validator with the provided options.
func New(opts ...Option) *Validator {
	// Default validator image (can be overridden by EIDOS_VALIDATOR_IMAGE env var for CI)
	defaultImage := "ghcr.io/nvidia/eidos-validator:latest"
	if envImage := os.Getenv("EIDOS_VALIDATOR_IMAGE"); envImage != "" {
		defaultImage = envImage
	}

	v := &Validator{
		Namespace: "eidos-validation", // Default namespace for validation jobs
		Image:     defaultImage,       // Default validator image
		RunID:     generateRunID(),    // Generate unique RunID for this validation run
		Cleanup:   true,               // Default to cleanup resources after validation
	}
	for _, opt := range opts {
		opt(v)
	}
	return v
}

// Validate evaluates all constraints from the recipe against the snapshot.
// Returns a ValidationResult containing per-constraint results and summary.
func (v *Validator) Validate(ctx context.Context, recipeResult *recipe.RecipeResult, snap *snapshotter.Snapshot) (*ValidationResult, error) {
	start := time.Now()

	if recipeResult == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "recipe cannot be nil")
	}
	if snap == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "snapshot cannot be nil")
	}

	result := NewValidationResult()
	result.Init(header.KindValidationResult, APIVersion, v.Version)

	// Evaluate each constraint
	for _, constraint := range recipeResult.Constraints {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		cv := v.evaluateConstraint(constraint, snap)
		result.Results = append(result.Results, cv)

		// Update summary counts
		switch cv.Status {
		case ConstraintStatusPassed:
			result.Summary.Passed++
		case ConstraintStatusFailed:
			result.Summary.Failed++
		case ConstraintStatusSkipped:
			result.Summary.Skipped++
		}
	}

	// Calculate summary
	result.Summary.Total = len(recipeResult.Constraints)
	result.Summary.Duration = time.Since(start)

	// Determine overall status
	switch {
	case result.Summary.Failed > 0:
		result.Summary.Status = ValidationStatusFail
	case result.Summary.Skipped > 0:
		result.Summary.Status = ValidationStatusPartial
	default:
		result.Summary.Status = ValidationStatusPass
	}

	slog.Debug("validation completed",
		"passed", result.Summary.Passed,
		"failed", result.Summary.Failed,
		"skipped", result.Summary.Skipped,
		"status", result.Summary.Status,
		"duration", result.Summary.Duration)

	return result, nil
}

// evaluateConstraint evaluates a single constraint against the snapshot.
func (v *Validator) evaluateConstraint(constraint recipe.Constraint, snap *snapshotter.Snapshot) ConstraintValidation {
	cv := ConstraintValidation{
		Name:     constraint.Name,
		Expected: constraint.Value,
	}

	// Parse the constraint path
	path, err := ParseConstraintPath(constraint.Name)
	if err != nil {
		cv.Status = ConstraintStatusSkipped
		cv.Message = fmt.Sprintf("invalid constraint path: %v", err)
		slog.Warn("skipping constraint with invalid path",
			"name", constraint.Name,
			"error", err)
		return cv
	}

	// Extract the actual value from snapshot
	actual, err := path.ExtractValue(snap)
	if err != nil {
		cv.Status = ConstraintStatusSkipped
		cv.Message = fmt.Sprintf("value not found in snapshot: %v", err)
		slog.Warn("skipping constraint - value not found",
			"name", constraint.Name,
			"path", path.String(),
			"error", err)
		return cv
	}
	cv.Actual = actual

	// Print detected criteria based on the path and value found
	printDetectedCriteria(path.String(), actual)

	// Parse the constraint expression
	parsed, err := ParseConstraintExpression(constraint.Value)
	if err != nil {
		cv.Status = ConstraintStatusSkipped
		cv.Message = fmt.Sprintf("invalid constraint expression: %v", err)
		slog.Warn("skipping constraint with invalid expression",
			"name", constraint.Name,
			"expression", constraint.Value,
			"error", err)
		return cv
	}

	// Evaluate the constraint
	passed, err := parsed.Evaluate(actual)
	if err != nil {
		cv.Status = ConstraintStatusFailed
		cv.Message = fmt.Sprintf("evaluation failed: %v", err)
		slog.Debug("constraint evaluation failed",
			"name", constraint.Name,
			"expected", constraint.Value,
			"actual", actual,
			"error", err)
		return cv
	}

	if passed {
		cv.Status = ConstraintStatusPassed
		slog.Debug("constraint passed",
			"name", constraint.Name,
			"expected", constraint.Value,
			"actual", actual)
	} else {
		cv.Status = ConstraintStatusFailed
		cv.Message = fmt.Sprintf("expected %s, got %s", constraint.Value, actual)
		slog.Debug("constraint failed",
			"name", constraint.Name,
			"expected", constraint.Value,
			"actual", actual)
	}

	return cv
}

// printDetectedCriteria prints detected criteria based on the constraint path and value.
func printDetectedCriteria(path, value string) {
	switch path {
	case "K8s.server.version":
		slog.Info("detected criteria", "service", value)
	case "GPU.smi.gpu.model":
		slog.Info("detected criteria", "accelerator", value)
	case "OS.release.ID":
		slog.Info("detected criteria", "os", value)
	case "OS.release.VERSION_ID":
		slog.Info("detected criteria", "os_version", value)
	}
}
