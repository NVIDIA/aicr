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
	"time"

	"github.com/NVIDIA/eidos/pkg/header"
)

// ValidationStatus represents the overall validation outcome.
type ValidationStatus string

const (
	// ValidationStatusPass indicates all constraints passed.
	ValidationStatusPass ValidationStatus = "pass"

	// ValidationStatusFail indicates one or more constraints failed.
	ValidationStatusFail ValidationStatus = "fail"

	// ValidationStatusPartial indicates some constraints couldn't be evaluated.
	ValidationStatusPartial ValidationStatus = "partial"

	// ValidationStatusSkipped indicates a phase was skipped (due to dependency failure).
	ValidationStatusSkipped ValidationStatus = "skipped"

	// ValidationStatusWarning indicates warnings but no hard failures.
	ValidationStatusWarning ValidationStatus = "warning"
)

// ConstraintStatus represents the outcome of evaluating a single constraint.
type ConstraintStatus string

const (
	// ConstraintStatusPassed indicates the constraint was satisfied.
	ConstraintStatusPassed ConstraintStatus = "passed"

	// ConstraintStatusFailed indicates the constraint was not satisfied.
	ConstraintStatusFailed ConstraintStatus = "failed"

	// ConstraintStatusSkipped indicates the constraint couldn't be evaluated.
	ConstraintStatusSkipped ConstraintStatus = "skipped"
)

// ValidationResult represents the complete validation outcome.
type ValidationResult struct {
	header.Header `json:",inline" yaml:",inline"`

	// RecipeSource is the path/URI of the recipe that was validated.
	RecipeSource string `json:"recipeSource" yaml:"recipeSource"`

	// SnapshotSource is the path/URI of the snapshot used for validation.
	SnapshotSource string `json:"snapshotSource" yaml:"snapshotSource"`

	// Summary contains aggregate validation statistics.
	Summary ValidationSummary `json:"summary" yaml:"summary"`

	// Results contains per-constraint validation details (legacy, for backward compatibility).
	Results []ConstraintValidation `json:"results,omitempty" yaml:"results,omitempty"`

	// Phases contains per-phase validation results (multi-phase validation).
	Phases map[string]*PhaseResult `json:"phases,omitempty" yaml:"phases,omitempty"`
}

// ValidationSummary contains aggregate statistics about the validation.
type ValidationSummary struct {
	// Passed is the count of constraints that were satisfied.
	Passed int `json:"passed" yaml:"passed"`

	// Failed is the count of constraints that were not satisfied.
	Failed int `json:"failed" yaml:"failed"`

	// Skipped is the count of constraints that couldn't be evaluated.
	Skipped int `json:"skipped" yaml:"skipped"`

	// Total is the total number of constraints evaluated.
	Total int `json:"total" yaml:"total"`

	// Status is the overall validation status.
	Status ValidationStatus `json:"status" yaml:"status"`

	// Duration is how long the validation took.
	Duration time.Duration `json:"duration" yaml:"duration"`
}

// ConstraintValidation represents the result of evaluating a single constraint.
type ConstraintValidation struct {
	// Name is the fully qualified constraint name (e.g., "K8s.server.version").
	Name string `json:"name" yaml:"name"`

	// Expected is the constraint expression from the recipe (e.g., ">= 1.32.4").
	Expected string `json:"expected" yaml:"expected"`

	// Actual is the value found in the snapshot (e.g., "v1.33.5-eks-3025e55").
	Actual string `json:"actual" yaml:"actual"`

	// Status is the outcome of this constraint evaluation.
	Status ConstraintStatus `json:"status" yaml:"status"`

	// Message provides additional context, especially for failures or skipped constraints.
	Message string `json:"message,omitempty" yaml:"message,omitempty"`
}

// NewValidationResult creates a new ValidationResult with initialized slices.
func NewValidationResult() *ValidationResult {
	return &ValidationResult{
		Results: make([]ConstraintValidation, 0),
		Phases:  make(map[string]*PhaseResult),
	}
}

// PhaseResult represents the result of a single validation phase.
type PhaseResult struct {
	// Status is the overall status of this phase.
	Status ValidationStatus `json:"status" yaml:"status"`

	// Constraints contains per-constraint results for this phase.
	Constraints []ConstraintValidation `json:"constraints,omitempty" yaml:"constraints,omitempty"`

	// Checks contains results of named validation checks.
	Checks []CheckResult `json:"checks,omitempty" yaml:"checks,omitempty"`

	// Reason explains why the phase was skipped or failed.
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`

	// Duration is how long this phase took to run.
	Duration time.Duration `json:"duration,omitempty" yaml:"duration,omitempty"`
}

// CheckResult represents the result of a named validation check.
type CheckResult struct {
	// Name is the check identifier.
	Name string `json:"name" yaml:"name"`

	// Status is the check outcome.
	Status ValidationStatus `json:"status" yaml:"status"`

	// Reason explains why the check failed or was skipped.
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`

	// Remediation provides actionable guidance for fixing failures.
	Remediation string `json:"remediation,omitempty" yaml:"remediation,omitempty"`
}
