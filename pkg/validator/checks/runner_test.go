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

package checks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"k8s.io/client-go/kubernetes/fake"
)

// Mock check function for testing
var testCheckCalled bool
var testCheckError error

func mockCheckSuccess(ctx *ValidationContext) error {
	testCheckCalled = true
	return nil
}

func mockCheckFailure(ctx *ValidationContext) error {
	testCheckCalled = true
	return testCheckError
}

func TestNewTestRunner_FailsOutsideKubernetes(t *testing.T) {
	// This test verifies that NewTestRunner fails gracefully when not in Kubernetes
	// (which is the expected behavior during local testing)

	runner, err := NewTestRunner(t)

	if err == nil {
		t.Error("NewTestRunner() should fail when not in Kubernetes cluster")
	}

	if runner != nil {
		t.Error("NewTestRunner() should return nil runner on error")
	}

	// Error should mention in-cluster config
	if err != nil && !strings.Contains(err.Error(), "in-cluster") {
		t.Errorf("Error should mention in-cluster config, got: %v", err)
	}
}

func TestRunCheck_CheckNotFound(t *testing.T) {
	// Create a mock test runner with fake context and mock testing.T
	mockT := &mockTestingT{}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	runner := &TestRunner{
		t: mockT,
		ctx: &ValidationContext{
			Context:   context.Background(),
			Snapshot:  &snapshotter.Snapshot{},
			Clientset: fake.NewSimpleClientset(),
		},
	}

	// Run check with non-existent name (should panic)
	defer func() {
		if r := recover(); r == nil {
			t.Error("RunCheck should panic when check not found")
		}
	}()

	runner.RunCheck("non-existent-check")

	// Verify t.Fatalf was called (we'll only get here if no panic, which should fail)
	if !mockT.fatalCalled {
		t.Error("RunCheck should call t.Fatalf when check not found")
	}
}

func TestRunCheck_Success(t *testing.T) {
	// Register a test check
	testCheckCalled = false
	RegisterCheck(&Check{
		Name:        "test-check-success",
		Description: "Test check that succeeds",
		Phase:       "test",
		Func:        mockCheckSuccess,
	})
	defer func() {
		// Clean up registry
		registryMu.Lock()
		delete(checkRegistry, "test-check-success")
		registryMu.Unlock()
	}()

	// Create mock test runner
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	runner := &TestRunner{
		t: t,
		ctx: &ValidationContext{
			Context:   context.Background(),
			Snapshot:  &snapshotter.Snapshot{},
			Clientset: fake.NewSimpleClientset(),
		},
	}

	// Run check
	runner.RunCheck("test-check-success")

	// Verify check was called
	if !testCheckCalled {
		t.Error("Check function should have been called")
	}
}

func TestRunCheck_Failure(t *testing.T) {
	// Register a test check that fails
	testCheckCalled = false
	testCheckError = &testError{msg: "test failure"}
	RegisterCheck(&Check{
		Name:        "test-check-failure",
		Description: "Test check that fails",
		Phase:       "test",
		Func:        mockCheckFailure,
	})
	defer func() {
		// Clean up registry
		registryMu.Lock()
		delete(checkRegistry, "test-check-failure")
		registryMu.Unlock()
	}()

	// Create mock test runner
	mockT := &mockTestingT{}
	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	runner := &TestRunner{
		t: mockT,
		ctx: &ValidationContext{
			Context:   context.Background(),
			Snapshot:  &snapshotter.Snapshot{},
			Clientset: fake.NewSimpleClientset(),
		},
	}

	// Run check (should panic via t.Fatalf)
	defer func() {
		if r := recover(); r == nil {
			t.Error("RunCheck should panic when check fails")
		}

		// Verify check was called and failed
		if !testCheckCalled {
			t.Error("Check function should have been called")
		}

		if !mockT.fatalCalled {
			t.Error("t.Fatalf should have been called when check failed")
		}

		if !strings.Contains(mockT.fatalMessage, "failed") {
			t.Errorf("Fatal message should indicate check failed, got: %s", mockT.fatalMessage)
		}
	}()

	runner.RunCheck("test-check-failure")
}

func TestLoadValidationContext_MissingSnapshotFile(t *testing.T) {
	// Set environment variable to non-existent file
	originalPath := os.Getenv("AICR_SNAPSHOT_PATH")
	defer func() {
		if originalPath != "" {
			os.Setenv("AICR_SNAPSHOT_PATH", originalPath)
		} else {
			os.Unsetenv("AICR_SNAPSHOT_PATH")
		}
	}()

	os.Setenv("AICR_SNAPSHOT_PATH", "/nonexistent/snapshot.yaml")

	// Should fail to load context (will also fail on in-cluster config)
	ctx, cancel, err := LoadValidationContext()

	if err == nil {
		t.Error("LoadValidationContext should fail with missing snapshot file")
	}

	if ctx != nil {
		t.Error("LoadValidationContext should return nil context on error")
	}

	if cancel != nil {
		t.Error("LoadValidationContext should return nil cancel on error")
	}
}

func TestLoadValidationContext_WithValidSnapshot(t *testing.T) {
	// Create temporary snapshot file
	tmpDir := t.TempDir()
	snapshotPath := filepath.Join(tmpDir, "snapshot.yaml")

	// Write valid snapshot YAML
	snapshotYAML := `apiVersion: aicr.nvidia.com/v1alpha1
kind: Snapshot
metadata:
  version: test
measurements:
  - type: GPU
    subtypes:
      - name: nvidia-smi
        data:
          driver_version: "560.35.03"
          cuda_version: "12.6"
`
	if err := os.WriteFile(snapshotPath, []byte(snapshotYAML), 0644); err != nil {
		t.Fatalf("Failed to create test snapshot file: %v", err)
	}

	// Set environment variable
	originalPath := os.Getenv("AICR_SNAPSHOT_PATH")
	defer func() {
		if originalPath != "" {
			os.Setenv("AICR_SNAPSHOT_PATH", originalPath)
		} else {
			os.Unsetenv("AICR_SNAPSHOT_PATH")
		}
	}()

	os.Setenv("AICR_SNAPSHOT_PATH", snapshotPath)

	// Attempt to load context
	// This will still fail on in-cluster config, but we can verify it tries to load the snapshot
	ctx, cancel, err := LoadValidationContext()
	if cancel != nil {
		defer cancel()
	}

	// Should fail on in-cluster config (not on snapshot loading)
	if err == nil {
		t.Error("LoadValidationContext should fail when not in Kubernetes")
	}

	// Error should be about in-cluster config, not snapshot file
	if err != nil && strings.Contains(err.Error(), "no such file") {
		t.Errorf("Should fail on in-cluster config, not snapshot file, got: %v", err)
	}

	if ctx != nil {
		t.Error("LoadValidationContext should return nil context on error")
	}
}

func TestLoadValidationContext_DefaultSnapshotPath(t *testing.T) {
	// Unset custom path to test default
	originalPath := os.Getenv("AICR_SNAPSHOT_PATH")
	defer func() {
		if originalPath != "" {
			os.Setenv("AICR_SNAPSHOT_PATH", originalPath)
		} else {
			os.Unsetenv("AICR_SNAPSHOT_PATH")
		}
	}()

	os.Unsetenv("AICR_SNAPSHOT_PATH")

	// Should use default path /data/snapshot/snapshot.yaml
	ctx, cancel, err := LoadValidationContext()
	if cancel != nil {
		defer cancel()
	}

	if err == nil {
		t.Error("LoadValidationContext should fail when not in Kubernetes")
	}

	if ctx != nil {
		t.Error("LoadValidationContext should return nil context on error")
	}

	// Error should be about in-cluster config or default snapshot path
	if err != nil && !strings.Contains(err.Error(), "in-cluster") && !strings.Contains(err.Error(), "/data/snapshot") {
		t.Logf("Error: %v", err)
	}
}

func TestLoadValidationContext_WithRecipeData(t *testing.T) {
	// Set recipe data environment variable
	originalRecipe := os.Getenv("AICR_RECIPE_DATA")
	defer func() {
		if originalRecipe != "" {
			os.Setenv("AICR_RECIPE_DATA", originalRecipe)
		} else {
			os.Unsetenv("AICR_RECIPE_DATA")
		}
	}()

	recipeJSON := `{"key":"value","number":42}`
	os.Setenv("AICR_RECIPE_DATA", recipeJSON)

	// Will fail on in-cluster config, but that's expected
	// This test verifies the recipe data parsing logic
	ctx, cancel, err := LoadValidationContext()
	if cancel != nil {
		defer cancel()
	}

	if err == nil {
		t.Error("LoadValidationContext should fail when not in Kubernetes")
	}

	if ctx != nil {
		t.Error("LoadValidationContext should return nil context on error")
	}

	// The error should be about in-cluster config, not recipe parsing
	if err != nil && strings.Contains(err.Error(), "recipe") && strings.Contains(err.Error(), "unmarshal") {
		t.Errorf("Recipe data parsing failed, got: %v", err)
	}
}

func TestLoadValidationContext_InvalidRecipeData(t *testing.T) {
	// Set invalid recipe data
	originalRecipe := os.Getenv("AICR_RECIPE_DATA")
	defer func() {
		if originalRecipe != "" {
			os.Setenv("AICR_RECIPE_DATA", originalRecipe)
		} else {
			os.Unsetenv("AICR_RECIPE_DATA")
		}
	}()

	os.Setenv("AICR_RECIPE_DATA", "invalid json{")

	// Will fail on in-cluster config first, but we're testing recipe parsing
	ctx, cancel, err := LoadValidationContext()
	if cancel != nil {
		defer cancel()
	}

	if err == nil {
		t.Error("LoadValidationContext should fail with invalid recipe JSON")
	}

	if ctx != nil {
		t.Error("LoadValidationContext should return nil context on error")
	}
}

func TestTestRunner_HasCheck(t *testing.T) {
	recipeResult := &recipe.RecipeResult{
		Validation: &recipe.ValidationConfig{
			Deployment: &recipe.ValidationPhase{
				Checks: []string{"operator-health", "check-nvidia-smi"},
			},
			Performance: &recipe.ValidationPhase{
				Checks: []string{"nccl-bandwidth"},
			},
			Conformance: &recipe.ValidationPhase{
				Checks: []string{"k8s-conformance"},
			},
		},
	}

	//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
	runner := &TestRunner{
		t: t,
		ctx: &ValidationContext{
			Context:   context.Background(),
			Clientset: fake.NewSimpleClientset(),
			Recipe:    recipeResult,
		},
	}

	tests := []struct {
		name      string
		phase     string
		checkName string
		want      bool
	}{
		{"deployment phase has check", "deployment", "operator-health", true},
		{"deployment phase has second check", "deployment", "check-nvidia-smi", true},
		{"deployment phase missing check", "deployment", "nonexistent", false},
		{"performance phase has check", "performance", "nccl-bandwidth", true},
		{"performance phase missing check", "performance", "nonexistent", false},
		{"conformance phase has check", "conformance", "k8s-conformance", true},
		{"conformance phase missing check", "conformance", "nonexistent", false},
		{"unknown phase", "unknown", "operator-health", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runner.HasCheck(tt.phase, tt.checkName)
			if got != tt.want {
				t.Errorf("HasCheck(%q, %q) = %v, want %v", tt.phase, tt.checkName, got, tt.want)
			}
		})
	}

	// Test nil recipe
	t.Run("nil recipe", func(t *testing.T) {
		//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
		nilRunner := &TestRunner{
			t: t,
			ctx: &ValidationContext{
				Context:   context.Background(),
				Clientset: fake.NewSimpleClientset(),
				Recipe:    nil,
			},
		}
		if nilRunner.HasCheck("deployment", "operator-health") {
			t.Error("HasCheck() should return false with nil recipe")
		}
	})

	// Test nil validation
	t.Run("nil validation", func(t *testing.T) {
		//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
		nilValRunner := &TestRunner{
			t: t,
			ctx: &ValidationContext{
				Context:   context.Background(),
				Clientset: fake.NewSimpleClientset(),
				Recipe:    &recipe.RecipeResult{Validation: nil},
			},
		}
		if nilValRunner.HasCheck("deployment", "operator-health") {
			t.Error("HasCheck() should return false with nil validation")
		}
	})

	// Test nil phase
	t.Run("nil deployment phase", func(t *testing.T) {
		//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
		nilPhaseRunner := &TestRunner{
			t: t,
			ctx: &ValidationContext{
				Context:   context.Background(),
				Clientset: fake.NewSimpleClientset(),
				Recipe: &recipe.RecipeResult{
					Validation: &recipe.ValidationConfig{
						Deployment: nil,
					},
				},
			},
		}
		if nilPhaseRunner.HasCheck("deployment", "operator-health") {
			t.Error("HasCheck() should return false with nil deployment phase")
		}
	})
}

// Helper types for testing

type testError struct {
	msg string
}

func (e *testError) Error() string {
	return e.msg
}

// mockTestingT implements the testingT interface for testing RunCheck
type mockTestingT struct {
	fatalCalled  bool
	fatalMessage string
	logMessages  []string
}

func (m *mockTestingT) Fatalf(format string, args ...interface{}) {
	m.fatalCalled = true
	m.fatalMessage = format
	// Fatalf should stop execution, so panic like real testing.T does
	panic("test failed: " + format)
}

func (m *mockTestingT) Logf(format string, args ...interface{}) {
	m.logMessages = append(m.logMessages, format)
}

func (m *mockTestingT) Helper() {}

// Test with actual check that uses snapshot
func TestRunCheck_WithSnapshotData(t *testing.T) {
	// Register a check that uses snapshot data
	testCheckCalled = false
	RegisterCheck(&Check{
		Name:        "test-snapshot-check",
		Description: "Test check that uses snapshot",
		Phase:       "test",
		Func: func(ctx *ValidationContext) error {
			testCheckCalled = true
			if ctx.Snapshot == nil {
				return &testError{msg: "snapshot is nil"}
			}
			for _, m := range ctx.Snapshot.Measurements {
				if m.Type == measurement.TypeGPU {
					return nil
				}
			}
			return &testError{msg: "no GPU measurement found"}
		},
	})
	defer func() {
		registryMu.Lock()
		delete(checkRegistry, "test-snapshot-check")
		registryMu.Unlock()
	}()

	// Test with snapshot containing GPU data
	t.Run("with GPU data", func(t *testing.T) {
		testCheckCalled = false
		//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
		runner := &TestRunner{
			t: t,
			ctx: &ValidationContext{
				Context: context.Background(),
				Snapshot: &snapshotter.Snapshot{
					Measurements: []*measurement.Measurement{
						{
							Type: measurement.TypeGPU,
							Subtypes: []measurement.Subtype{
								{
									Name: "nvidia-smi",
									Data: map[string]measurement.Reading{
										"count": measurement.Int(8),
									},
								},
							},
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
		}

		runner.RunCheck("test-snapshot-check")

		if !testCheckCalled {
			t.Error("Check should have been called")
		}
	})

	// Test with snapshot missing GPU data
	t.Run("without GPU data", func(t *testing.T) {
		testCheckCalled = false
		mockT := &mockTestingT{}
		//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
		runner := &TestRunner{
			t: mockT,
			ctx: &ValidationContext{
				Context: context.Background(),
				Snapshot: &snapshotter.Snapshot{
					Measurements: []*measurement.Measurement{
						{
							Type: measurement.TypeOS,
							Subtypes: []measurement.Subtype{
								{
									Name: "release",
									Data: map[string]measurement.Reading{
										"ID": measurement.Str("ubuntu"),
									},
								},
							},
						},
					},
				},
				Clientset: fake.NewSimpleClientset(),
			},
		}

		// Should panic when check fails
		defer func() {
			if r := recover(); r == nil {
				t.Error("RunCheck should panic when check fails")
			}

			if !testCheckCalled {
				t.Error("Check should have been called")
			}

			if !mockT.fatalCalled {
				t.Error("Check should have failed when GPU data not found")
			}
		}()

		runner.RunCheck("test-snapshot-check")
	})
}

func TestTestRunner_Cancel(t *testing.T) {
	t.Run("cancel with nil cancel func", func(t *testing.T) {
		runner := &TestRunner{
			t:      t,
			ctx:    nil,
			cancel: nil,
		}

		// Should not panic when cancel is nil
		runner.Cancel()
	})

	t.Run("cancel with valid cancel func", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		runner := &TestRunner{
			t:      t,
			ctx:    &ValidationContext{Context: ctx},
			cancel: cancel,
		}

		// Cancel should work without panic
		runner.Cancel()

		// Verify context is actually canceled
		select {
		case <-ctx.Done():
			// Expected
		default:
			t.Error("Context should be cancelled after Cancel()")
		}
	})
}

func TestTestRunner_Context(t *testing.T) {
	t.Run("returns validation context", func(t *testing.T) {
		//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
		expectedCtx := &ValidationContext{
			Context:   context.Background(),
			Clientset: fake.NewSimpleClientset(),
		}

		runner := &TestRunner{
			t:   t,
			ctx: expectedCtx,
		}

		got := runner.Context()
		if got != expectedCtx {
			t.Errorf("Context() = %v, want %v", got, expectedCtx)
		}
	})

	t.Run("returns nil when context not set", func(t *testing.T) {
		runner := &TestRunner{
			t:   t,
			ctx: nil,
		}

		got := runner.Context()
		if got != nil {
			t.Errorf("Context() = %v, want nil", got)
		}
	})
}

func TestTestRunner_GetConstraint(t *testing.T) {
	t.Run("returns constraint when found", func(t *testing.T) {
		recipeResult := &recipe.RecipeResult{
			Validation: &recipe.ValidationConfig{
				Deployment: &recipe.ValidationPhase{
					Constraints: []recipe.Constraint{
						{Name: "Deployment.gpu-operator.version", Value: ">= v24.6.0"},
						{Name: "Deployment.other.version", Value: "== v1.0.0"},
					},
				},
			},
		}

		//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
		runner := &TestRunner{
			t: t,
			ctx: &ValidationContext{
				Context:   context.Background(),
				Clientset: fake.NewSimpleClientset(),
				Recipe:    recipeResult,
			},
		}

		got := runner.GetConstraint("deployment", "Deployment.gpu-operator.version")
		if got == nil {
			t.Fatal("GetConstraint() returned nil, want constraint")
		}
		if got.Name != "Deployment.gpu-operator.version" {
			t.Errorf("GetConstraint().Name = %v, want %v", got.Name, "Deployment.gpu-operator.version")
		}
		if got.Value != ">= v24.6.0" {
			t.Errorf("GetConstraint().Value = %v, want %v", got.Value, ">= v24.6.0")
		}
	})

	t.Run("returns nil when constraint not found", func(t *testing.T) {
		recipeResult := &recipe.RecipeResult{
			Validation: &recipe.ValidationConfig{
				Deployment: &recipe.ValidationPhase{
					Constraints: []recipe.Constraint{
						{Name: "Deployment.other.version", Value: "== v1.0.0"},
					},
				},
			},
		}

		//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
		runner := &TestRunner{
			t: t,
			ctx: &ValidationContext{
				Context:   context.Background(),
				Clientset: fake.NewSimpleClientset(),
				Recipe:    recipeResult,
			},
		}

		got := runner.GetConstraint("deployment", "Deployment.nonexistent.version")
		if got != nil {
			t.Errorf("GetConstraint() = %v, want nil", got)
		}
	})

	t.Run("returns nil when recipe is nil", func(t *testing.T) {
		//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
		runner := &TestRunner{
			t: t,
			ctx: &ValidationContext{
				Context:   context.Background(),
				Clientset: fake.NewSimpleClientset(),
				Recipe:    nil,
			},
		}

		got := runner.GetConstraint("deployment", "Deployment.test.version")
		if got != nil {
			t.Errorf("GetConstraint() = %v, want nil", got)
		}
	})

	t.Run("returns nil when validation config is nil", func(t *testing.T) {
		recipeResult := &recipe.RecipeResult{
			Validation: nil,
		}

		//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
		runner := &TestRunner{
			t: t,
			ctx: &ValidationContext{
				Context:   context.Background(),
				Clientset: fake.NewSimpleClientset(),
				Recipe:    recipeResult,
			},
		}

		got := runner.GetConstraint("deployment", "Deployment.test.version")
		if got != nil {
			t.Errorf("GetConstraint() = %v, want nil", got)
		}
	})

	t.Run("returns nil for unknown phase", func(t *testing.T) {
		recipeResult := &recipe.RecipeResult{
			Validation: &recipe.ValidationConfig{
				Deployment: &recipe.ValidationPhase{
					Constraints: []recipe.Constraint{
						{Name: "Deployment.test.version", Value: "== v1.0.0"},
					},
				},
			},
		}

		//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
		runner := &TestRunner{
			t: t,
			ctx: &ValidationContext{
				Context:   context.Background(),
				Clientset: fake.NewSimpleClientset(),
				Recipe:    recipeResult,
			},
		}

		got := runner.GetConstraint("unknown-phase", "Deployment.test.version")
		if got != nil {
			t.Errorf("GetConstraint() = %v, want nil for unknown phase", got)
		}
	})

	t.Run("returns constraint for performance phase", func(t *testing.T) {
		recipeResult := &recipe.RecipeResult{
			Validation: &recipe.ValidationConfig{
				Performance: &recipe.ValidationPhase{
					Constraints: []recipe.Constraint{
						{Name: "Performance.nccl.bandwidth", Value: ">= 100Gbps"},
					},
				},
			},
		}

		//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
		runner := &TestRunner{
			t: t,
			ctx: &ValidationContext{
				Context:   context.Background(),
				Clientset: fake.NewSimpleClientset(),
				Recipe:    recipeResult,
			},
		}

		got := runner.GetConstraint("performance", "Performance.nccl.bandwidth")
		if got == nil {
			t.Fatal("GetConstraint() returned nil, want constraint")
		}
		if got.Name != "Performance.nccl.bandwidth" {
			t.Errorf("GetConstraint().Name = %v, want %v", got.Name, "Performance.nccl.bandwidth")
		}
		if got.Value != ">= 100Gbps" {
			t.Errorf("GetConstraint().Value = %v, want %v", got.Value, ">= 100Gbps")
		}
	})

	t.Run("returns nil for performance phase when nil", func(t *testing.T) {
		recipeResult := &recipe.RecipeResult{
			Validation: &recipe.ValidationConfig{
				Performance: nil,
			},
		}

		//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
		runner := &TestRunner{
			t: t,
			ctx: &ValidationContext{
				Context:   context.Background(),
				Clientset: fake.NewSimpleClientset(),
				Recipe:    recipeResult,
			},
		}

		got := runner.GetConstraint("performance", "Performance.nccl.bandwidth")
		if got != nil {
			t.Errorf("GetConstraint() = %v, want nil for nil performance phase", got)
		}
	})

	t.Run("returns constraint for conformance phase", func(t *testing.T) {
		recipeResult := &recipe.RecipeResult{
			Validation: &recipe.ValidationConfig{
				Conformance: &recipe.ValidationPhase{
					Constraints: []recipe.Constraint{
						{Name: "Conformance.k8s.version", Value: ">= 1.30"},
					},
				},
			},
		}

		//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
		runner := &TestRunner{
			t: t,
			ctx: &ValidationContext{
				Context:   context.Background(),
				Clientset: fake.NewSimpleClientset(),
				Recipe:    recipeResult,
			},
		}

		got := runner.GetConstraint("conformance", "Conformance.k8s.version")
		if got == nil {
			t.Fatal("GetConstraint() returned nil, want constraint")
		}
		if got.Name != "Conformance.k8s.version" {
			t.Errorf("GetConstraint().Name = %v, want %v", got.Name, "Conformance.k8s.version")
		}
		if got.Value != ">= 1.30" {
			t.Errorf("GetConstraint().Value = %v, want %v", got.Value, ">= 1.30")
		}
	})

	t.Run("returns nil for conformance phase when nil", func(t *testing.T) {
		recipeResult := &recipe.RecipeResult{
			Validation: &recipe.ValidationConfig{
				Conformance: nil,
			},
		}

		//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
		runner := &TestRunner{
			t: t,
			ctx: &ValidationContext{
				Context:   context.Background(),
				Clientset: fake.NewSimpleClientset(),
				Recipe:    recipeResult,
			},
		}

		got := runner.GetConstraint("conformance", "Conformance.k8s.version")
		if got != nil {
			t.Errorf("GetConstraint() = %v, want nil for nil conformance phase", got)
		}
	})

	t.Run("returns nil for deployment phase when nil", func(t *testing.T) {
		recipeResult := &recipe.RecipeResult{
			Validation: &recipe.ValidationConfig{
				Deployment: nil,
			},
		}

		//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
		runner := &TestRunner{
			t: t,
			ctx: &ValidationContext{
				Context:   context.Background(),
				Clientset: fake.NewSimpleClientset(),
				Recipe:    recipeResult,
			},
		}

		got := runner.GetConstraint("deployment", "Deployment.gpu-operator.version")
		if got != nil {
			t.Errorf("GetConstraint() = %v, want nil for nil deployment phase", got)
		}
	})

	t.Run("performance phase constraint not found", func(t *testing.T) {
		recipeResult := &recipe.RecipeResult{
			Validation: &recipe.ValidationConfig{
				Performance: &recipe.ValidationPhase{
					Constraints: []recipe.Constraint{
						{Name: "Performance.nccl.bandwidth", Value: ">= 100Gbps"},
					},
				},
			},
		}

		//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
		runner := &TestRunner{
			t: t,
			ctx: &ValidationContext{
				Context:   context.Background(),
				Clientset: fake.NewSimpleClientset(),
				Recipe:    recipeResult,
			},
		}

		got := runner.GetConstraint("performance", "Performance.nonexistent")
		if got != nil {
			t.Errorf("GetConstraint() = %v, want nil for nonexistent constraint", got)
		}
	})

	t.Run("conformance phase constraint not found", func(t *testing.T) {
		recipeResult := &recipe.RecipeResult{
			Validation: &recipe.ValidationConfig{
				Conformance: &recipe.ValidationPhase{
					Constraints: []recipe.Constraint{
						{Name: "Conformance.k8s.version", Value: ">= 1.30"},
					},
				},
			},
		}

		//nolint:staticcheck // SA1019: fake.NewSimpleClientset is sufficient for tests
		runner := &TestRunner{
			t: t,
			ctx: &ValidationContext{
				Context:   context.Background(),
				Clientset: fake.NewSimpleClientset(),
				Recipe:    recipeResult,
			},
		}

		got := runner.GetConstraint("conformance", "Conformance.nonexistent")
		if got != nil {
			t.Errorf("GetConstraint() = %v, want nil for nonexistent constraint", got)
		}
	})
}
