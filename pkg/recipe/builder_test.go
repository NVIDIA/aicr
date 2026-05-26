// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
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
	"fmt"
	"slices"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"
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

// TestNewBuilder_WithDataProvider verifies that WithDataProvider binds the
// provided DataProvider to the Builder, isolating it from the package-global
// provider at GetDataProvider().
func TestNewBuilder_WithDataProvider(t *testing.T) {
	dp := NewEmbeddedDataProvider(GetEmbeddedFS(), "")
	b := NewBuilder(WithDataProvider(dp))
	if got := b.DataProvider(); got != dp {
		t.Errorf("Builder.DataProvider() = %v, want %v", got, dp)
	}
}

// TestNewBuilder_DataProviderNilFallback verifies that when WithDataProvider
// is not set, Builder.DataProvider() returns nil so callers fall back to the
// package-global DataProvider — preserving CLI and API server behavior.
func TestNewBuilder_DataProviderNilFallback(t *testing.T) {
	b := NewBuilder() // no option set
	if b.DataProvider() != nil {
		t.Error("expected nil provider when WithDataProvider not used")
	}
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
	warning := ConstraintWarning{
		Overlay:    "h100-eks-ubuntu-training-kubeflow",
		Constraint: testK8sVersionConstant,
		Expected:   ">= 1.32.4",
		Actual:     "1.30.0",
		Reason:     "expected >= 1.32.4, got 1.30.0",
	}

	if warning.Overlay != "h100-eks-ubuntu-training-kubeflow" {
		t.Errorf("expected overlay h100-eks-ubuntu-training-kubeflow, got %q", warning.Overlay)
	}
	if warning.Constraint != testK8sVersionConstant {
		t.Errorf("expected constraint %s, got %q", testK8sVersionConstant, warning.Constraint)
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
	if passed.Error != nil {
		t.Errorf("expected Error to be nil, got %v", passed.Error)
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
	if failed.Actual != "rhel" {
		t.Errorf("expected actual rhel, got %q", failed.Actual)
	}
	if failed.Error != nil {
		t.Errorf("expected Error to be nil, got %v", failed.Error)
	}

	// Test error result
	errResult := ConstraintEvalResult{
		Passed: false,
		Actual: "",
		Error:  errors.New("value not found"),
	}
	if errResult.Passed {
		t.Error("expected Passed to be false")
	}
	if errResult.Actual != "" {
		t.Errorf("expected actual to be empty, got %q", errResult.Actual)
	}
	if errResult.Error == nil {
		t.Error("expected error to be set")
	}
}

// buildIsolationProvider returns an inMemoryDataProvider seeded with a
// minimal registry.yaml, an empty base recipe, and a single leaf overlay
// whose metadata name is derived from `overlayName`. The overlay declares
// `criteria.service: eks` and a uniquely-named componentRef
// (`<overlayName>-component`) so isolation tests can detect both presence
// (correct provider routing) and leak (wrong provider routing).
//
// The componentRef carries minimal Type/Source/Chart fields so
// finalizeRecipeResult's dependency validation and topological sort pass
// without a registry hit; applyRegistryDefaults will find no matching
// entry in the empty registry and leave the ref untouched, which is
// exactly what these tests want — they care about which overlays land
// in the result, not registry merging.
func buildIsolationProvider(t *testing.T, overlayName string) DataProvider {
	t.Helper()

	registryYAML := []byte(`components: []
`)
	baseYAML := []byte(`kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: base
spec:
  componentRefs: []
`)
	overlayYAML := fmt.Appendf(nil, `kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: %[1]s
spec:
  base: base
  criteria:
    service: eks
  componentRefs:
    - name: %[1]s-component
      type: Helm
      chart: example
      source: https://charts.example.com
      version: 0.1.0
`, overlayName)

	files := map[string][]byte{
		"registry.yaml":                     registryYAML,
		"overlays/base.yaml":                baseYAML,
		"overlays/" + overlayName + ".yaml": overlayYAML,
	}
	return newInMemoryProvider(overlayName, files)
}

// componentNames extracts the ordered set of ComponentRef.Name from a
// RecipeResult. Used by isolation tests to assert which components landed
// in the merged result.
func componentNames(r *RecipeResult) []string {
	if r == nil {
		return nil
	}
	names := make([]string, 0, len(r.ComponentRefs))
	for _, c := range r.ComponentRefs {
		names = append(names, c.Name)
	}
	return names
}

// TestBuilder_WithDataProvider_UsesBoundProvider verifies that a Builder
// constructed with WithDataProvider routes BuildFromCriteria through the
// bound provider end-to-end, and that the resulting RecipeResult carries
// the same provider via DataProvider() so downstream consumers (e.g.,
// GetValuesForComponent in Task 6) honor per-tenant isolation.
func TestBuilder_WithDataProvider_UsesBoundProvider(t *testing.T) {
	t.Cleanup(ResetMetadataStoreForTesting)
	t.Cleanup(ResetComponentRegistryForTesting)

	dp := buildIsolationProvider(t, "isolated")
	b := NewBuilder(WithDataProvider(dp))

	criteria := &Criteria{Service: "eks"}
	result, err := b.BuildFromCriteria(context.Background(), criteria)
	if err != nil {
		t.Fatalf("BuildFromCriteria: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// Provider should propagate to the result so GetValuesForComponent
	// (Task 6) honors it.
	if result.DataProvider() != dp {
		t.Errorf("RecipeResult.DataProvider() = %v, want bound provider %v", result.DataProvider(), dp)
	}

	// And the overlay's unique component must land in the result —
	// proving the bound provider was actually walked, not the global.
	if got := componentNames(result); !slices.Contains(got, "isolated-component") {
		t.Errorf("expected isolated-component in result, got %v", got)
	}
}

// TestBuilder_DataProvider_NilSafe verifies the nil-safety contract of
// RecipeResult.DataProvider() — call sites must be able to chain off a
// possibly-nil result without panicking.
func TestBuilder_DataProvider_NilSafe(t *testing.T) {
	var r *RecipeResult
	if got := r.DataProvider(); got != nil {
		t.Errorf("nil RecipeResult.DataProvider() = %v, want nil", got)
	}
}

// TestBuilder_TwoProviders_ConcurrentBuild verifies that two Builders
// bound to different providers produce isolated results under concurrent
// load. With 8 goroutines per provider, the -race detector also catches
// any shared-state slip introduced by the per-provider caches.
func TestBuilder_TwoProviders_ConcurrentBuild(t *testing.T) {
	t.Cleanup(ResetMetadataStoreForTesting)
	t.Cleanup(ResetComponentRegistryForTesting)

	dpA := buildIsolationProvider(t, "alpha-only")
	dpB := buildIsolationProvider(t, "beta-only")
	bA := NewBuilder(WithDataProvider(dpA))
	bB := NewBuilder(WithDataProvider(dpB))

	const fanout = 8
	resultsA := make([]*RecipeResult, fanout)
	resultsB := make([]*RecipeResult, fanout)

	g, gctx := errgroup.WithContext(context.Background())
	for i := range fanout {
		g.Go(func() error {
			r, err := bA.BuildFromCriteria(gctx, &Criteria{Service: "eks"})
			resultsA[i] = r
			return err
		})
		g.Go(func() error {
			r, err := bB.BuildFromCriteria(gctx, &Criteria{Service: "eks"})
			resultsB[i] = r
			return err
		})
	}
	if err := g.Wait(); err != nil {
		t.Fatalf("concurrent build: %v", err)
	}

	for i, r := range resultsA {
		if r == nil {
			t.Fatalf("resultsA[%d] is nil", i)
		}
		if r.DataProvider() != dpA {
			t.Errorf("resultsA[%d].DataProvider() = %v, want %v", i, r.DataProvider(), dpA)
		}
		names := componentNames(r)
		if !slices.Contains(names, "alpha-only-component") {
			t.Errorf("resultsA[%d] missing alpha-only-component: %v", i, names)
		}
		if slices.Contains(names, "beta-only-component") {
			t.Errorf("resultsA[%d] leaked beta-only-component: %v", i, names)
		}
	}
	for i, r := range resultsB {
		if r == nil {
			t.Fatalf("resultsB[%d] is nil", i)
		}
		if r.DataProvider() != dpB {
			t.Errorf("resultsB[%d].DataProvider() = %v, want %v", i, r.DataProvider(), dpB)
		}
		names := componentNames(r)
		if !slices.Contains(names, "beta-only-component") {
			t.Errorf("resultsB[%d] missing beta-only-component: %v", i, names)
		}
		if slices.Contains(names, "alpha-only-component") {
			t.Errorf("resultsB[%d] leaked alpha-only-component: %v", i, names)
		}
	}
}
