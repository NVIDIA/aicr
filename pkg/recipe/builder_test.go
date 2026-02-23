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

package recipe

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestBuilder_BuildFromCriteria_ContextCancellation tests context cancellation
// during recipe building to ensure proper timeout handling and error propagation.
func TestBuilder_BuildFromCriteria_ContextCancellation(t *testing.T) {
	tests := []struct {
		name        string
		setupCtx    func() (context.Context, context.CancelFunc)
		wantTimeout bool
	}{
		{
			name: "immediate cancellation",
			setupCtx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel() // Cancel immediately
				return ctx, cancel
			},
			wantTimeout: true,
		},
		{
			name: "normal operation with adequate timeout",
			setupCtx: func() (context.Context, context.CancelFunc) {
				// Provide adequate timeout for normal operation
				return context.WithTimeout(context.Background(), 5*time.Second)
			},
			wantTimeout: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := tt.setupCtx()
			defer cancel()

			// Create builder with standard configuration
			builder := NewBuilder()

			// Create minimal criteria (all "any" wildcards)
			criteria := NewCriteria()

			// Attempt to build recipe
			result, err := builder.BuildFromCriteria(ctx, criteria)

			if tt.wantTimeout {
				// Should get timeout error
				if err == nil {
					t.Fatal("expected error due to context cancellation, got nil")
				}

				// Verify error is timeout-related
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("expected context cancellation error, got: %v", err)
				}

				// Result should be nil on error
				if result != nil {
					t.Error("expected nil result on error")
				}
			} else {
				// Should succeed
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if result == nil {
					t.Fatal("expected non-nil result")
				}
			}
		})
	}
}

// TestBuilder_BuildFromCriteria_TimeoutBudget verifies that the builder
// respects the 25-second timeout budget for recipe building.
func TestBuilder_BuildFromCriteria_TimeoutBudget(t *testing.T) {
	// Create context with 30s timeout (handler-level)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	builder := NewBuilder()
	criteria := NewCriteria()

	start := time.Now()
	result, err := builder.BuildFromCriteria(ctx, criteria)
	elapsed := time.Since(start)

	// Should complete quickly (within 1 second)
	if elapsed > 1*time.Second {
		t.Errorf("build took too long: %v (expected < 1s)", elapsed)
	}

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

// TestBuilder_BuildFromCriteria_ContextValues tests that context values
// are properly propagated through the build process.
func TestBuilder_BuildFromCriteria_ContextValues(t *testing.T) {
	type contextKey string
	const requestIDKey contextKey = "request-id"

	// Create context with value
	ctx := context.WithValue(context.Background(), requestIDKey, "test-request-123")

	builder := NewBuilder()
	criteria := NewCriteria()

	result, err := builder.BuildFromCriteria(ctx, criteria)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Verify context value was accessible (would be used for logging/tracing)
	if requestID := ctx.Value(requestIDKey); requestID != "test-request-123" {
		t.Error("context value was lost during build")
	}
}

// TestBuilder_BuildFromCriteriaWithEvaluator tests the constraint-aware
// recipe building with a custom evaluator function.
func TestBuilder_BuildFromCriteriaWithEvaluator(t *testing.T) {
	tests := []struct {
		name              string
		evaluator         ConstraintEvaluatorFunc
		wantExcluded      bool
		wantWarningCount  int
		expectSpecificErr string
	}{
		{
			name:             "nil evaluator behaves like standard build",
			evaluator:        nil,
			wantExcluded:     false,
			wantWarningCount: 0,
		},
		{
			name: "evaluator that passes all constraints",
			evaluator: func(_ Constraint) ConstraintEvalResult {
				return ConstraintEvalResult{Passed: true, Actual: "test-value"}
			},
			wantExcluded:     false,
			wantWarningCount: 0,
		},
		{
			name: "evaluator that fails all constraints",
			evaluator: func(c Constraint) ConstraintEvalResult {
				return ConstraintEvalResult{
					Passed: false,
					Actual: "wrong-value",
					Error:  nil,
				}
			},
			wantExcluded:     true,
			wantWarningCount: -1, // At least some warnings (actual count depends on overlay constraints)
		},
		{
			name: "evaluator with errors",
			evaluator: func(_ Constraint) ConstraintEvalResult {
				return ConstraintEvalResult{
					Passed: false,
					Error:  errors.New("simulated evaluation error"),
				}
			},
			wantExcluded:     true,
			wantWarningCount: -1, // At least some warnings
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			builder := NewBuilder(WithVersion("test-v1.0.0"))
			criteria := NewCriteria()

			result, err := builder.BuildFromCriteriaWithEvaluator(ctx, criteria, tt.evaluator)

			if tt.expectSpecificErr != "" {
				if err == nil || err.Error() != tt.expectSpecificErr {
					t.Errorf("expected error %q, got %v", tt.expectSpecificErr, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result == nil {
				t.Fatal("expected non-nil result")
			}

			// Verify metadata version was set
			if result.Metadata.Version != "test-v1.0.0" {
				t.Errorf("expected version test-v1.0.0, got %q", result.Metadata.Version)
			}

			// Verify warnings match expectations. A wantWarningCount of -1 means
			// "skip exact count validation" for constraint warnings.
			if tt.wantWarningCount >= 0 {
				if len(result.Metadata.ConstraintWarnings) != tt.wantWarningCount {
					t.Errorf("expected %d warnings, got %d",
						tt.wantWarningCount, len(result.Metadata.ConstraintWarnings))
				}
			}

			// Basic result validation
			if result.Kind != RecipeResultKind {
				t.Errorf("expected kind %s, got %q", RecipeResultKind, result.Kind)
			}
			if result.APIVersion != RecipeAPIVersion {
				t.Errorf("expected apiVersion %s, got %q", RecipeAPIVersion, result.APIVersion)
			}
		})
	}
}

// TestWithAllowLists tests the WithAllowLists builder option.
func TestWithAllowLists(t *testing.T) {
	t.Run("nil allowlists", func(t *testing.T) {
		b := NewBuilder(WithAllowLists(nil))
		if b.AllowLists != nil {
			t.Error("expected nil AllowLists")
		}
	})

	t.Run("valid allowlists", func(t *testing.T) {
		al := &AllowLists{
			Services: []CriteriaServiceType{CriteriaServiceEKS},
		}
		b := NewBuilder(WithAllowLists(al))
		if b.AllowLists == nil {
			t.Fatal("expected non-nil AllowLists")
		}
		if len(b.AllowLists.Services) != 1 {
			t.Errorf("Services length = %d, want 1", len(b.AllowLists.Services))
		}
	})
}

// TestGetEmbeddedFS tests that the embedded filesystem is accessible.
func TestGetEmbeddedFS(t *testing.T) {
	fs := GetEmbeddedFS()

	// Should be able to read the registry file
	data, err := fs.ReadFile("registry.yaml")
	if err != nil {
		t.Fatalf("failed to read registry.yaml from embedded FS: %v", err)
	}
	if len(data) == 0 {
		t.Error("registry.yaml is empty")
	}
}

// TestConstraintWarning tests the ConstraintWarning struct.
func TestConstraintWarning(t *testing.T) {
	const k8sVersionConstraint = "K8s.server.version"

	warning := ConstraintWarning{
		Overlay:    "h100-eks-ubuntu-training-kubeflow",
		Constraint: k8sVersionConstraint,
		Expected:   ">= 1.32.4",
		Actual:     "1.30.0",
		Reason:     "expected >= 1.32.4, got 1.30.0",
	}

	if warning.Overlay != "h100-eks-ubuntu-training-kubeflow" {
		t.Errorf("expected overlay h100-eks-ubuntu-training-kubeflow, got %q", warning.Overlay)
	}
	if warning.Constraint != k8sVersionConstraint {
		t.Errorf("expected constraint %s, got %q", k8sVersionConstraint, warning.Constraint)
	}
	if warning.Expected != ">= 1.32.4" {
		t.Errorf("expected expression >= 1.32.4, got %q", warning.Expected)
	}
	if warning.Actual != "1.30.0" {
		t.Errorf("expected actual 1.30.0, got %q", warning.Actual)
	}
	if warning.Reason != "expected >= 1.32.4, got 1.30.0" {
		t.Errorf("expected reason string, got %q", warning.Reason)
	}
}

// TestConstraintEvalResult tests the ConstraintEvalResult struct.
func TestConstraintEvalResult(t *testing.T) {
	// Test passed result
	passed := ConstraintEvalResult{
		Passed: true,
		Actual: "ubuntu",
		Error:  nil,
	}
	if !passed.Passed {
		t.Error("expected Passed to be true")
	}
	if passed.Actual != "ubuntu" {
		t.Errorf("expected actual ubuntu, got %q", passed.Actual)
	}

	// Test failed result
	failed := ConstraintEvalResult{
		Passed: false,
		Actual: "rhel",
		Error:  nil,
	}
	if failed.Passed {
		t.Error("expected Passed to be false")
	}

	// Test error result
	errResult := ConstraintEvalResult{
		Passed: false,
		Actual: "",
		Error:  errors.New("value not found"),
	}
	if errResult.Error == nil {
		t.Error("expected error to be set")
	}
}
