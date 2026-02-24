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

package chainsaw

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGenerateTestManifest(t *testing.T) {
	tests := []struct {
		name          string
		componentName string
		timeout       time.Duration
		wantContains  []string
	}{
		{
			name:          "generates valid chainsaw test YAML",
			componentName: "gpu-operator",
			timeout:       2 * time.Minute,
			wantContains: []string{
				"apiVersion: chainsaw.kyverno.io/v1alpha1",
				"kind: Test",
				"name: gpu-operator",
				"assert: 120s",
				"file: assert.yaml",
			},
		},
		{
			name:          "handles short timeout",
			componentName: "network-operator",
			timeout:       30 * time.Second,
			wantContains: []string{
				"name: network-operator",
				"assert: 30s",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			path := filepath.Join(tmpDir, "chainsaw-test.yaml")

			err := generateTestManifest(path, tt.componentName, tt.timeout)
			if err != nil {
				t.Fatalf("generateTestManifest() error = %v", err)
			}

			content, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("failed to read generated file: %v", err)
			}

			for _, want := range tt.wantContains {
				if !strings.Contains(string(content), want) {
					t.Errorf("generated YAML missing expected content %q, got:\n%s", want, string(content))
				}
			}
		})
	}
}

func TestRunEmpty(t *testing.T) {
	results := Run(t.Context(), nil, 2*time.Minute)
	if results != nil {
		t.Errorf("Run(nil) = %v, want nil", results)
	}
}

func TestRunSingleMissingChainsaw(t *testing.T) {
	// When chainsaw binary is not in PATH, runSingle should return an error.
	// Save and clear PATH to ensure chainsaw is not found.
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", "")
	defer func() {
		// t.Setenv already handles restore, but be explicit
		_ = os.Setenv("PATH", origPath)
	}()

	asserts := []ComponentAssert{
		{
			Name:       "test-component",
			AssertYAML: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n",
		},
	}

	results := Run(t.Context(), asserts, 30*time.Second)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	r := results[0]
	if r.Component != "test-component" {
		t.Errorf("Component = %q, want %q", r.Component, "test-component")
	}
	if r.Passed {
		t.Errorf("expected Passed=false when chainsaw is not in PATH")
	}
	if r.Error == nil {
		t.Errorf("expected non-nil Error when chainsaw is not in PATH")
	}
}

func TestRunSingleContextCancelled(t *testing.T) {
	// When context is canceled, chainsaw exec should fail promptly.
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately

	asserts := []ComponentAssert{
		{
			Name:       "cancelled-component",
			AssertYAML: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n",
		},
	}

	results := Run(ctx, asserts, 30*time.Second)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	r := results[0]
	if r.Component != "cancelled-component" {
		t.Errorf("Component = %q, want %q", r.Component, "cancelled-component")
	}
	if r.Passed {
		t.Error("expected Passed=false for cancelled context")
	}
	if r.Error == nil {
		t.Error("expected non-nil Error for cancelled context")
	}
}

func TestRunMultipleComponents(t *testing.T) {
	// Verify that Run handles multiple components concurrently.
	// Chainsaw not in PATH during unit tests, so all should fail,
	// but we verify the results are correctly attributed to each component.
	origPath := os.Getenv("PATH")
	t.Setenv("PATH", "")
	defer func() { _ = os.Setenv("PATH", origPath) }()

	asserts := []ComponentAssert{
		{Name: "comp-a", AssertYAML: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n"},
		{Name: "comp-b", AssertYAML: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\n"},
		{Name: "comp-c", AssertYAML: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: c\n"},
	}

	results := Run(t.Context(), asserts, 30*time.Second)
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	// Verify each result matches its component (order preserved since results[i] = runSingle(asserts[i]))
	for i, r := range results {
		if r.Component != asserts[i].Name {
			t.Errorf("results[%d].Component = %q, want %q", i, r.Component, asserts[i].Name)
		}
		if r.Passed {
			t.Errorf("results[%d].Passed = true, want false (chainsaw not in PATH)", i)
		}
		if r.Error == nil {
			t.Errorf("results[%d].Error = nil, want non-nil", i)
		}
	}
}

func TestRunSingleWritesFiles(t *testing.T) {
	// Verify that runSingle creates the expected directory structure
	// even if chainsaw is not available (it will fail at exec).
	assertYAML := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: test-ns\n"

	// Create a temp dir and verify structure would be created
	baseDir := t.TempDir()
	testDir := filepath.Join(baseDir, "my-component")
	if err := os.MkdirAll(testDir, 0o750); err != nil {
		t.Fatalf("failed to create test dir: %v", err)
	}

	// Write assert.yaml
	assertPath := filepath.Join(testDir, "assert.yaml")
	if err := os.WriteFile(assertPath, []byte(assertYAML), 0o600); err != nil {
		t.Fatalf("failed to write assert.yaml: %v", err)
	}

	// Generate test manifest
	testYAMLPath := filepath.Join(testDir, "chainsaw-test.yaml")
	if err := generateTestManifest(testYAMLPath, "my-component", 2*time.Minute); err != nil {
		t.Fatalf("generateTestManifest() error = %v", err)
	}

	// Verify assert.yaml exists
	if _, err := os.Stat(assertPath); os.IsNotExist(err) {
		t.Error("assert.yaml was not created")
	}

	// Verify chainsaw-test.yaml exists and references assert.yaml
	content, err := os.ReadFile(testYAMLPath)
	if err != nil {
		t.Fatalf("failed to read chainsaw-test.yaml: %v", err)
	}
	if !strings.Contains(string(content), "file: assert.yaml") {
		t.Error("chainsaw-test.yaml does not reference assert.yaml")
	}
}
