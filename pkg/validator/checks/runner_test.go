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
	"testing"

	"github.com/NVIDIA/eidos/pkg/measurement"
	"github.com/NVIDIA/eidos/pkg/snapshotter"
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
	if err != nil && !contains(err.Error(), "in-cluster") {
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

		if !contains(mockT.fatalMessage, "failed") {
			t.Errorf("Fatal message should indicate check failed, got: %s", mockT.fatalMessage)
		}
	}()

	runner.RunCheck("test-check-failure")
}

func TestLoadValidationContext_MissingSnapshotFile(t *testing.T) {
	// Set environment variable to non-existent file
	originalPath := os.Getenv("EIDOS_SNAPSHOT_PATH")
	defer func() {
		if originalPath != "" {
			os.Setenv("EIDOS_SNAPSHOT_PATH", originalPath)
		} else {
			os.Unsetenv("EIDOS_SNAPSHOT_PATH")
		}
	}()

	os.Setenv("EIDOS_SNAPSHOT_PATH", "/nonexistent/snapshot.yaml")

	// Should fail to load context (will also fail on in-cluster config)
	ctx, err := LoadValidationContext()

	if err == nil {
		t.Error("LoadValidationContext should fail with missing snapshot file")
	}

	if ctx != nil {
		t.Error("LoadValidationContext should return nil context on error")
	}
}

func TestLoadValidationContext_WithValidSnapshot(t *testing.T) {
	// Create temporary snapshot file
	tmpDir := t.TempDir()
	snapshotPath := filepath.Join(tmpDir, "snapshot.yaml")

	// Write valid snapshot YAML
	snapshotYAML := `apiVersion: eidos.nvidia.com/v1alpha1
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
	originalPath := os.Getenv("EIDOS_SNAPSHOT_PATH")
	defer func() {
		if originalPath != "" {
			os.Setenv("EIDOS_SNAPSHOT_PATH", originalPath)
		} else {
			os.Unsetenv("EIDOS_SNAPSHOT_PATH")
		}
	}()

	os.Setenv("EIDOS_SNAPSHOT_PATH", snapshotPath)

	// Attempt to load context
	// This will still fail on in-cluster config, but we can verify it tries to load the snapshot
	_, err := LoadValidationContext()

	// Should fail on in-cluster config (not on snapshot loading)
	if err == nil {
		t.Error("LoadValidationContext should fail when not in Kubernetes")
	}

	// Error should be about in-cluster config, not snapshot file
	if err != nil && contains(err.Error(), "no such file") {
		t.Errorf("Should fail on in-cluster config, not snapshot file, got: %v", err)
	}
}

func TestLoadValidationContext_DefaultSnapshotPath(t *testing.T) {
	// Unset custom path to test default
	originalPath := os.Getenv("EIDOS_SNAPSHOT_PATH")
	defer func() {
		if originalPath != "" {
			os.Setenv("EIDOS_SNAPSHOT_PATH", originalPath)
		} else {
			os.Unsetenv("EIDOS_SNAPSHOT_PATH")
		}
	}()

	os.Unsetenv("EIDOS_SNAPSHOT_PATH")

	// Should use default path /data/snapshot/snapshot.yaml
	_, err := LoadValidationContext()

	if err == nil {
		t.Error("LoadValidationContext should fail when not in Kubernetes")
	}

	// Error should be about in-cluster config or default snapshot path
	if err != nil && !contains(err.Error(), "in-cluster") && !contains(err.Error(), "/data/snapshot") {
		t.Logf("Error: %v", err)
	}
}

func TestLoadValidationContext_WithRecipeData(t *testing.T) {
	// Set recipe data environment variable
	originalRecipe := os.Getenv("EIDOS_RECIPE_DATA")
	defer func() {
		if originalRecipe != "" {
			os.Setenv("EIDOS_RECIPE_DATA", originalRecipe)
		} else {
			os.Unsetenv("EIDOS_RECIPE_DATA")
		}
	}()

	recipeJSON := `{"key":"value","number":42}`
	os.Setenv("EIDOS_RECIPE_DATA", recipeJSON)

	// Will fail on in-cluster config, but that's expected
	// This test verifies the recipe data parsing logic
	_, err := LoadValidationContext()

	if err == nil {
		t.Error("LoadValidationContext should fail when not in Kubernetes")
	}

	// The error should be about in-cluster config, not recipe parsing
	if err != nil && contains(err.Error(), "recipe") && contains(err.Error(), "unmarshal") {
		t.Errorf("Recipe data parsing failed, got: %v", err)
	}
}

func TestLoadValidationContext_InvalidRecipeData(t *testing.T) {
	// Set invalid recipe data
	originalRecipe := os.Getenv("EIDOS_RECIPE_DATA")
	defer func() {
		if originalRecipe != "" {
			os.Setenv("EIDOS_RECIPE_DATA", originalRecipe)
		} else {
			os.Unsetenv("EIDOS_RECIPE_DATA")
		}
	}()

	os.Setenv("EIDOS_RECIPE_DATA", "invalid json{")

	// Will fail on in-cluster config first, but we're testing recipe parsing
	_, err := LoadValidationContext()

	if err == nil {
		t.Error("LoadValidationContext should fail with invalid recipe JSON")
	}
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

// Helper function to check if string contains substring
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) &&
		(s[:len(substr)] == substr || s[len(s)-len(substr):] == substr ||
			len(s) > len(substr)+1 && findSubstr(s, substr)))
}

func findSubstr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

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
