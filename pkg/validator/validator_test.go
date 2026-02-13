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
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/NVIDIA/eidos/pkg/measurement"
	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/snapshotter"
)

func TestValidator_Validate(t *testing.T) {
	// Create a test snapshot
	snapshot := &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeK8s,
				Subtypes: []measurement.Subtype{
					{
						Name: "server",
						Data: map[string]measurement.Reading{
							"version": measurement.Str("v1.33.5-eks-3025e55"),
						},
					},
				},
			},
			{
				Type: measurement.TypeOS,
				Subtypes: []measurement.Subtype{
					{
						Name: "release",
						Data: map[string]measurement.Reading{
							"ID":         measurement.Str("ubuntu"),
							"VERSION_ID": measurement.Str("24.04"),
						},
					},
					{
						Name: "sysctl",
						Data: map[string]measurement.Reading{
							"/proc/sys/kernel/osrelease": measurement.Str("6.8.0-1028-aws"),
						},
					},
				},
			},
			{
				Type: measurement.TypeGPU,
				Subtypes: []measurement.Subtype{
					{
						Name: "info",
						Data: map[string]measurement.Reading{
							"type":   measurement.Str("H100"),
							"driver": measurement.Str("550.107.02"),
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name            string
		constraints     []recipe.Constraint
		wantStatus      ValidationStatus
		wantPassed      int
		wantFailed      int
		wantSkipped     int
		expectError     bool
		snapshotNil     bool
		recipeResultNil bool
	}{
		{
			name: "all constraints pass",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: ">= 1.32.4"},
				{Name: "OS.release.ID", Value: "ubuntu"},
				{Name: "OS.release.VERSION_ID", Value: "24.04"},
			},
			wantStatus:  ValidationStatusPass,
			wantPassed:  3,
			wantFailed:  0,
			wantSkipped: 0,
		},
		{
			name: "one constraint fails",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: ">= 1.32.4"},
				{Name: "OS.release.ID", Value: "rhel"}, // This should fail
				{Name: "OS.release.VERSION_ID", Value: "24.04"},
			},
			wantStatus:  ValidationStatusFail,
			wantPassed:  2,
			wantFailed:  1,
			wantSkipped: 0,
		},
		{
			name: "all constraints fail",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: ">= 2.0.0"}, // Too high
				{Name: "OS.release.ID", Value: "rhel"},          // Wrong OS
				{Name: "OS.release.VERSION_ID", Value: "22.04"}, // Wrong version
			},
			wantStatus:  ValidationStatusFail,
			wantPassed:  0,
			wantFailed:  3,
			wantSkipped: 0,
		},
		{
			name: "one constraint skipped",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: ">= 1.32.4"},
				{Name: "NonExistent.subtype.key", Value: "value"}, // This should be skipped
				{Name: "OS.release.ID", Value: "ubuntu"},
			},
			wantStatus:  ValidationStatusPartial,
			wantPassed:  2,
			wantFailed:  0,
			wantSkipped: 1,
		},
		{
			name: "mixed results",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: ">= 1.32.4"},  // Pass
				{Name: "OS.release.ID", Value: "rhel"},            // Fail
				{Name: "NonExistent.subtype.key", Value: "value"}, // Skip
			},
			wantStatus:  ValidationStatusFail, // Failed takes precedence
			wantPassed:  1,
			wantFailed:  1,
			wantSkipped: 1,
		},
		{
			name: "value not found in snapshot",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.nonexistent", Value: "test"}, // Valid type but missing key
			},
			wantStatus:  ValidationStatusPartial,
			wantPassed:  0,
			wantFailed:  0,
			wantSkipped: 1,
		},
		{
			name: "invalid constraint expression",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: ""}, // Empty expression
			},
			wantStatus:  ValidationStatusPartial,
			wantPassed:  0,
			wantFailed:  0,
			wantSkipped: 1,
		},
		{
			name: "evaluation failure on version parse",
			constraints: []recipe.Constraint{
				{Name: "OS.release.ID", Value: ">= 1.0.0"}, // Version comparison on non-version "ubuntu"
			},
			wantStatus:  ValidationStatusFail,
			wantPassed:  0,
			wantFailed:  1,
			wantSkipped: 0,
		},
		{
			name:        "empty constraints",
			constraints: []recipe.Constraint{},
			wantStatus:  ValidationStatusPass,
			wantPassed:  0,
			wantFailed:  0,
			wantSkipped: 0,
		},
		{
			name: "version comparison operators",
			constraints: []recipe.Constraint{
				{Name: "K8s.server.version", Value: ">= 1.30"},
				{Name: "K8s.server.version", Value: "<= 2.0"},
				{Name: "K8s.server.version", Value: "> 1.29"},
				{Name: "K8s.server.version", Value: "< 2.1"},
				{Name: "K8s.server.version", Value: "!= 1.30.0"},
			},
			wantStatus:  ValidationStatusPass,
			wantPassed:  5,
			wantFailed:  0,
			wantSkipped: 0,
		},
		{
			name: "kernel version constraint",
			constraints: []recipe.Constraint{
				{Name: "OS.sysctl./proc/sys/kernel/osrelease", Value: ">= 6.8"},
			},
			wantStatus:  ValidationStatusPass,
			wantPassed:  1,
			wantFailed:  0,
			wantSkipped: 0,
		},
		{
			name: "kernel version fails",
			constraints: []recipe.Constraint{
				{Name: "OS.sysctl./proc/sys/kernel/osrelease", Value: ">= 6.9"},
			},
			wantStatus:  ValidationStatusFail,
			wantPassed:  0,
			wantFailed:  1,
			wantSkipped: 0,
		},
		{
			name:        "nil snapshot",
			constraints: []recipe.Constraint{},
			snapshotNil: true,
			expectError: true,
		},
		{
			name:            "nil recipe result",
			constraints:     []recipe.Constraint{},
			recipeResultNil: true,
			expectError:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := New(WithVersion("test"))

			var testRecipe *recipe.RecipeResult
			if !tt.recipeResultNil {
				testRecipe = &recipe.RecipeResult{
					Constraints: tt.constraints,
				}
			}

			var testSnapshot *snapshotter.Snapshot
			if !tt.snapshotNil {
				testSnapshot = snapshot
			}

			result, err := v.Validate(context.Background(), testRecipe, testSnapshot)
			if tt.expectError {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result.Summary.Status != tt.wantStatus {
				t.Errorf("Status = %v, want %v", result.Summary.Status, tt.wantStatus)
			}
			if result.Summary.Passed != tt.wantPassed {
				t.Errorf("Passed = %d, want %d", result.Summary.Passed, tt.wantPassed)
			}
			if result.Summary.Failed != tt.wantFailed {
				t.Errorf("Failed = %d, want %d", result.Summary.Failed, tt.wantFailed)
			}
			if result.Summary.Skipped != tt.wantSkipped {
				t.Errorf("Skipped = %d, want %d", result.Summary.Skipped, tt.wantSkipped)
			}
			if result.Summary.Total != len(tt.constraints) {
				t.Errorf("Total = %d, want %d", result.Summary.Total, len(tt.constraints))
			}
		})
	}
}

func TestValidator_Validate_ConstraintDetails(t *testing.T) {
	snapshot := &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeK8s,
				Subtypes: []measurement.Subtype{
					{
						Name: "server",
						Data: map[string]measurement.Reading{
							"version": measurement.Str("v1.33.5-eks-3025e55"),
						},
					},
				},
			},
		},
	}

	recipeResult := &recipe.RecipeResult{
		Constraints: []recipe.Constraint{
			{Name: "K8s.server.version", Value: ">= 1.32.4"},
		},
	}

	v := New(WithVersion("test"))
	result, err := v.Validate(context.Background(), recipeResult, snapshot)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result.Results))
	}

	cv := result.Results[0]
	if cv.Name != "K8s.server.version" {
		t.Errorf("Name = %q, want %q", cv.Name, "K8s.server.version")
	}
	if cv.Expected != ">= 1.32.4" {
		t.Errorf("Expected = %q, want %q", cv.Expected, ">= 1.32.4")
	}
	if cv.Actual != "v1.33.5-eks-3025e55" {
		t.Errorf("Actual = %q, want %q", cv.Actual, "v1.33.5-eks-3025e55")
	}
	if cv.Status != ConstraintStatusPassed {
		t.Errorf("Status = %v, want %v", cv.Status, ConstraintStatusPassed)
	}
}

func TestNew(t *testing.T) {
	t.Run("default validator", func(t *testing.T) {
		v := New()
		if v == nil {
			t.Fatal("expected non-nil validator")
		}
		if v.Version != "" {
			t.Errorf("Version = %q, want empty string", v.Version)
		}
	})

	t.Run("with version", func(t *testing.T) {
		v := New(WithVersion("v1.2.3"))
		if v == nil {
			t.Fatal("expected non-nil validator")
		}
		if v.Version != "v1.2.3" {
			t.Errorf("Version = %q, want %q", v.Version, "v1.2.3")
		}
	})
}

func TestPrintDetectedCriteria(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		value    string
		wantLog  string
		wantSkip bool
	}{
		{
			name:    "K8s version logs service",
			path:    "K8s.server.version",
			value:   "v1.33.5-eks-3025e55",
			wantLog: "service",
		},
		{
			name:    "GPU model logs accelerator",
			path:    "GPU.smi.gpu.model",
			value:   "NVIDIA H100 80GB HBM3",
			wantLog: "accelerator",
		},
		{
			name:    "OS release logs os",
			path:    "OS.release.ID",
			value:   "ubuntu",
			wantLog: "os",
		},
		{
			name:    "OS version logs os_version",
			path:    "OS.release.VERSION_ID",
			value:   "24.04",
			wantLog: "os_version",
		},
		{
			name:     "unrecognized path does not log",
			path:     "Other.subtype.key",
			value:    "somevalue",
			wantSkip: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			handler := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})
			oldLogger := slog.Default()
			slog.SetDefault(slog.New(handler))
			defer slog.SetDefault(oldLogger)

			printDetectedCriteria(tt.path, tt.value)

			output := buf.String()
			if tt.wantSkip {
				if output != "" {
					t.Errorf("expected no log output for path %q, got %q", tt.path, output)
				}
				return
			}

			if output == "" {
				t.Errorf("expected log output for path %q, got none", tt.path)
				return
			}

			if !bytes.Contains(buf.Bytes(), []byte(tt.wantLog)) {
				t.Errorf("expected log to contain %q, got %q", tt.wantLog, output)
			}
			if !bytes.Contains(buf.Bytes(), []byte(tt.value)) {
				t.Errorf("expected log to contain value %q, got %q", tt.value, output)
			}
		})
	}
}

// TestValidator_Validate_ContextCancellation tests that validation respects context cancellation.
func TestValidator_Validate_ContextCancellation(t *testing.T) {
	snapshot := &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeK8s,
				Subtypes: []measurement.Subtype{
					{
						Name: "server",
						Data: map[string]measurement.Reading{
							"version": measurement.Str("v1.33.5-eks-3025e55"),
						},
					},
				},
			},
		},
	}

	// Create many constraints to ensure the loop runs multiple times
	constraints := make([]recipe.Constraint, 100)
	for i := range constraints {
		constraints[i] = recipe.Constraint{Name: "K8s.server.version", Value: ">= 1.32.4"}
	}

	recipeResult := &recipe.RecipeResult{
		Constraints: constraints,
	}

	// Create an already canceled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	v := New(WithVersion("test"))
	_, err := v.Validate(ctx, recipeResult, snapshot)

	if err == nil {
		t.Error("expected error from canceled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled error, got %v", err)
	}
}

// TestEvaluateConstraint tests the standalone EvaluateConstraint function.
func TestEvaluateConstraint(t *testing.T) {
	snapshot := &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeK8s,
				Subtypes: []measurement.Subtype{
					{
						Name: "server",
						Data: map[string]measurement.Reading{
							"version": measurement.Str("v1.33.5-eks-3025e55"),
						},
					},
				},
			},
			{
				Type: measurement.TypeOS,
				Subtypes: []measurement.Subtype{
					{
						Name: "release",
						Data: map[string]measurement.Reading{
							"ID":         measurement.Str("ubuntu"),
							"VERSION_ID": measurement.Str("24.04"),
						},
					},
				},
			},
		},
	}

	tests := []struct {
		name       string
		constraint recipe.Constraint
		wantPassed bool
		wantActual string
		wantError  bool
	}{
		{
			name:       "version constraint passes",
			constraint: recipe.Constraint{Name: "K8s.server.version", Value: ">= 1.32.4"},
			wantPassed: true,
			wantActual: "v1.33.5-eks-3025e55",
			wantError:  false,
		},
		{
			name:       "version constraint fails",
			constraint: recipe.Constraint{Name: "K8s.server.version", Value: ">= 1.35.0"},
			wantPassed: false,
			wantActual: "v1.33.5-eks-3025e55",
			wantError:  false,
		},
		{
			name:       "exact match passes",
			constraint: recipe.Constraint{Name: "OS.release.ID", Value: "ubuntu"},
			wantPassed: true,
			wantActual: "ubuntu",
			wantError:  false,
		},
		{
			name:       "exact match fails",
			constraint: recipe.Constraint{Name: "OS.release.ID", Value: "rhel"},
			wantPassed: false,
			wantActual: "ubuntu",
			wantError:  false,
		},
		{
			name:       "invalid path format",
			constraint: recipe.Constraint{Name: "invalid.path", Value: "test"},
			wantPassed: false,
			wantActual: "",
			wantError:  true,
		},
		{
			name:       "value not found",
			constraint: recipe.Constraint{Name: "K8s.server.nonexistent", Value: "test"},
			wantPassed: false,
			wantActual: "",
			wantError:  true,
		},
		{
			name:       "measurement type not found",
			constraint: recipe.Constraint{Name: "GPU.info.driver", Value: "test"},
			wantPassed: false,
			wantActual: "",
			wantError:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := EvaluateConstraint(tt.constraint, snapshot)

			if result.Passed != tt.wantPassed {
				t.Errorf("Passed = %v, want %v", result.Passed, tt.wantPassed)
			}
			if result.Actual != tt.wantActual {
				t.Errorf("Actual = %q, want %q", result.Actual, tt.wantActual)
			}
			if (result.Error != nil) != tt.wantError {
				t.Errorf("Error = %v, wantError = %v", result.Error, tt.wantError)
			}
		})
	}
}

func TestGenerateRunID(t *testing.T) {
	// Test that RunID generation produces a valid format
	runID := generateRunID()

	// Expected format: YYYYMMDD-HHMMSS-XXXXXXXXXXXXXXXX (16 hex chars)
	parts := strings.Split(runID, "-")
	if len(parts) != 3 {
		t.Errorf("RunID format incorrect, expected 3 parts, got %d: %s", len(parts), runID)
	}

	// Check timestamp part (YYYYMMDD)
	if len(parts[0]) != 8 {
		t.Errorf("Date part should be 8 characters (YYYYMMDD), got %d: %s", len(parts[0]), parts[0])
	}

	// Check time part (HHMMSS)
	if len(parts[1]) != 6 {
		t.Errorf("Time part should be 6 characters (HHMMSS), got %d: %s", len(parts[1]), parts[1])
	}

	// Check random part (16 hex characters)
	if len(parts[2]) != 16 {
		t.Errorf("Random part should be 16 characters, got %d: %s", len(parts[2]), parts[2])
	}
}

func TestGenerateRunID_Uniqueness(t *testing.T) {
	// Generate multiple RunIDs and verify they're unique
	runIDs := make(map[string]bool)
	for i := 0; i < 100; i++ {
		runID := generateRunID()
		if runIDs[runID] {
			t.Errorf("Generated duplicate RunID: %s", runID)
		}
		runIDs[runID] = true
	}
}

func TestNew_CustomImageFromEnv(t *testing.T) {
	t.Setenv("EIDOS_VALIDATOR_IMAGE", "custom-image:latest")
	v := New()
	if v.Image != "custom-image:latest" {
		t.Errorf("Image = %q, want custom-image:latest", v.Image)
	}
}

func TestNew_GeneratesRunID(t *testing.T) {
	// Verify that New() automatically generates a RunID
	v := New()

	if v.RunID == "" {
		t.Error("New() should generate a RunID, but got empty string")
	}

	// Verify format
	parts := strings.Split(v.RunID, "-")
	if len(parts) != 3 {
		t.Errorf("Generated RunID has incorrect format: %s", v.RunID)
	}
}

func TestNew_WithRunID(t *testing.T) {
	// Verify that WithRunID option sets the RunID
	customRunID := "20260101-120000-test"
	v := New(WithRunID(customRunID))

	if v.RunID != customRunID {
		t.Errorf("WithRunID() should set RunID to %s, got %s", customRunID, v.RunID)
	}
}

func TestNew_UniqueRunIDs(t *testing.T) {
	// Verify that multiple New() calls generate unique RunIDs
	v1 := New()
	v2 := New()

	if v1.RunID == v2.RunID {
		t.Errorf("New() should generate unique RunIDs, but both are: %s", v1.RunID)
	}
}

func TestNew_DefaultNamespace(t *testing.T) {
	v := New()

	expectedNamespace := "eidos-validation"
	if v.Namespace != expectedNamespace {
		t.Errorf("Expected default namespace %s, got %s", expectedNamespace, v.Namespace)
	}
}

func TestNew_WithNamespace(t *testing.T) {
	customNamespace := "custom-validation"
	v := New(WithNamespace(customNamespace))

	if v.Namespace != customNamespace {
		t.Errorf("Expected namespace %s, got %s", customNamespace, v.Namespace)
	}
}

func TestNew_MultipleOptions(t *testing.T) {
	version := "v1.0.0"
	namespace := "custom-ns"
	runID := "20260101-120000-test"
	secrets := []string{"secret-a", "secret-b"}

	v := New(
		WithVersion(version),
		WithNamespace(namespace),
		WithRunID(runID),
		WithCleanup(false),
		WithImagePullSecrets(secrets),
	)

	if v.Version != version {
		t.Errorf("Expected version %s, got %s", version, v.Version)
	}
	if v.Namespace != namespace {
		t.Errorf("Expected namespace %s, got %s", namespace, v.Namespace)
	}
	if v.RunID != runID {
		t.Errorf("Expected runID %s, got %s", runID, v.RunID)
	}
	if v.Cleanup {
		t.Error("Expected Cleanup to be false")
	}
	if len(v.ImagePullSecrets) != 2 || v.ImagePullSecrets[0] != "secret-a" {
		t.Errorf("Expected ImagePullSecrets [secret-a secret-b], got %v", v.ImagePullSecrets)
	}
}

func TestNew_WithImage(t *testing.T) {
	customImage := "localhost:5001/eidos-validator:test"
	v := New(WithImage(customImage))

	if v.Image != customImage {
		t.Errorf("Expected image %s, got %s", customImage, v.Image)
	}
}

func TestNew_WithImage_MultipleOptions(t *testing.T) {
	version := "v1.0.0"
	image := "ghcr.io/nvidia/eidos-validator:v1.0.0"

	v := New(
		WithVersion(version),
		WithImage(image),
	)

	if v.Version != version {
		t.Errorf("Expected version %s, got %s", version, v.Version)
	}
	if v.Image != image {
		t.Errorf("Expected image %s, got %s", image, v.Image)
	}
}

// TestEvaluateConstraint_EvaluationFailure tests when Evaluate() returns an error.
func TestEvaluateConstraint_EvaluationFailure(t *testing.T) {
	// Create a snapshot with non-version-like values that will fail version comparison
	snapshot := &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeOS,
				Subtypes: []measurement.Subtype{
					{
						Name: "release",
						Data: map[string]measurement.Reading{
							"ID": measurement.Str("not-a-version-string"),
						},
					},
				},
			},
		},
	}

	// Use a version comparison operator on a non-version value
	// This should cause Evaluate() to fail when trying to parse versions
	constraint := recipe.Constraint{
		Name:  "OS.release.ID",
		Value: ">= 1.0.0", // Version comparison on non-version string
	}

	result := EvaluateConstraint(constraint, snapshot)

	// Should fail due to version parse error
	if result.Passed {
		t.Errorf("Expected constraint to fail, but it passed")
	}
	if result.Error == nil {
		t.Errorf("Expected error from version comparison failure")
	}
}

// TestValidator_evaluateConstraint_InvalidExpression tests invalid constraint expressions.
func TestValidator_evaluateConstraint_InvalidExpression(t *testing.T) {
	snapshot := &snapshotter.Snapshot{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeK8s,
				Subtypes: []measurement.Subtype{
					{
						Name: "server",
						Data: map[string]measurement.Reading{
							"version": measurement.Str("v1.33.5"),
						},
					},
				},
			},
		},
	}

	// Empty expression should cause parse failure
	constraint := recipe.Constraint{
		Name:  "K8s.server.version",
		Value: "", // Empty expression is invalid
	}

	result := EvaluateConstraint(constraint, snapshot)

	// Should be skipped due to invalid expression
	if result.Passed {
		t.Errorf("Expected constraint to not pass with empty expression")
	}
	if result.Error == nil {
		t.Errorf("Expected error from invalid expression")
	}
}
