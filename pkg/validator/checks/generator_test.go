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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConstraintToFuncName(t *testing.T) {
	tests := []struct {
		name       string
		constraint string
		want       string
	}{
		{
			name:       "deployment gpu operator version",
			constraint: "Deployment.gpu-operator.version",
			want:       "DeploymentGpuOperatorVersion",
		},
		{
			name:       "simple constraint",
			constraint: "K8s.server.version",
			want:       "K8sServerVersion",
		},
		{
			name:       "single part",
			constraint: "version",
			want:       "Version",
		},
		{
			name:       "with dashes",
			constraint: "my-app.some-value",
			want:       "MyAppSomeValue",
		},
		{
			name:       "empty string",
			constraint: "",
			want:       "",
		},
		{
			name:       "uppercase parts",
			constraint: "OS.release.ID",
			want:       "OSReleaseID",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := constraintToFuncName(tt.constraint)
			if got != tt.want {
				t.Errorf("constraintToFuncName(%q) = %q, want %q", tt.constraint, got, tt.want)
			}
		})
	}
}

func TestToSnakeCase(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "camel case",
			input: "GPUOperatorVersion",
			want:  "g_p_u_operator_version",
		},
		{
			name:  "simple camel",
			input: "FooBar",
			want:  "foo_bar",
		},
		{
			name:  "already lowercase",
			input: "foobar",
			want:  "foobar",
		},
		{
			name:  "single uppercase",
			input: "A",
			want:  "a",
		},
		{
			name:  "empty string",
			input: "",
			want:  "",
		},
		{
			name:  "mixed case",
			input: "DeploymentGpuOperatorVersion",
			want:  "deployment_gpu_operator_version",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toSnakeCase(tt.input)
			if got != tt.want {
				t.Errorf("toSnakeCase(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGenerateConstraintValidator(t *testing.T) {
	// Create a temporary directory for test output
	tmpDir, err := os.MkdirTemp("", "generator-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create subdirectory named "deployment" so package name is correct
	outputDir := filepath.Join(tmpDir, "deployment")
	if mkdirErr := os.MkdirAll(outputDir, 0755); mkdirErr != nil {
		t.Fatalf("failed to create output dir: %v", mkdirErr)
	}

	cfg := GeneratorConfig{
		ConstraintName: "Deployment.test-app.version",
		Phase:          "deployment",
		Description:    "Test validator for test-app version",
		OutputDir:      outputDir,
	}

	err = GenerateConstraintValidator(cfg)
	if err != nil {
		t.Fatalf("GenerateConstraintValidator() error = %v", err)
	}

	// Verify files were created
	// Generator creates: _constraint.go (source), _constraint_test.go (test), _recipe.yaml, _README.md
	expectedFiles := []string{
		"deployment_test_app_version_constraint.go",
		"deployment_test_app_version_constraint_test.go",
		"deployment_test_app_version_recipe.yaml",
		"deployment_test_app_version_README.md",
	}

	for _, filename := range expectedFiles {
		path := filepath.Join(outputDir, filename)
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			t.Errorf("expected file %s was not created", filename)
		}
	}

	// Verify constraint source file has expected content
	constraintContent, err := os.ReadFile(filepath.Join(outputDir, "deployment_test_app_version_constraint.go"))
	if err != nil {
		t.Fatalf("failed to read constraint file: %v", err)
	}
	contentStr := string(constraintContent)

	// Verify package and registration
	if !strings.Contains(contentStr, "package deployment") {
		t.Error("constraint file missing package declaration")
	}
	if !strings.Contains(contentStr, "RegisterConstraintValidator") {
		t.Error("constraint file missing RegisterConstraintValidator")
	}
	if !strings.Contains(contentStr, "Deployment.test-app.version") {
		t.Error("constraint file missing constraint pattern")
	}
	if !strings.Contains(contentStr, "TODO") {
		t.Error("constraint file missing TODO comments")
	}

	// Verify recipe file
	recipeContent, err := os.ReadFile(filepath.Join(outputDir, "deployment_test_app_version_recipe.yaml"))
	if err != nil {
		t.Fatalf("failed to read recipe file: %v", err)
	}
	if !strings.Contains(string(recipeContent), "Deployment.test-app.version") {
		t.Error("recipe file missing constraint name")
	}

	// Verify README file
	readmeContent, err := os.ReadFile(filepath.Join(outputDir, "deployment_test_app_version_README.md"))
	if err != nil {
		t.Fatalf("failed to read README file: %v", err)
	}
	if !strings.Contains(string(readmeContent), "Deployment.test-app.version") {
		t.Error("README file missing constraint name")
	}
}

func TestGenerateConstraintValidator_DifferentPhases(t *testing.T) {
	// Test that the generator works with different valid phases
	phases := []string{"deployment", "performance", "conformance", "readiness"}

	for _, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "generator-test-*")
			if err != nil {
				t.Fatalf("failed to create temp dir: %v", err)
			}
			defer os.RemoveAll(tmpDir)

			cfg := GeneratorConfig{
				ConstraintName: "Test.constraint",
				Phase:          phase,
				Description:    "Test",
				OutputDir:      tmpDir,
			}

			err = GenerateConstraintValidator(cfg)
			if err != nil {
				t.Errorf("GenerateConstraintValidator() error = %v for phase %s", err, phase)
			}
		})
	}
}

func TestGenerateConstraintValidator_EmptyConstraint(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "generator-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := GeneratorConfig{
		ConstraintName: "",
		Phase:          "deployment",
		Description:    "Test",
		OutputDir:      tmpDir,
	}

	err = GenerateConstraintValidator(cfg)
	if err == nil {
		t.Error("expected error for empty constraint, got nil")
	}
}

func TestGenerateCheck(t *testing.T) {
	// Create a temporary directory for test output
	tmpDir, err := os.MkdirTemp("", "generator-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Create subdirectory named "deployment" so package name is correct
	outputDir := filepath.Join(tmpDir, "deployment")
	if mkdirErr := os.MkdirAll(outputDir, 0755); mkdirErr != nil {
		t.Fatalf("failed to create output dir: %v", mkdirErr)
	}

	cfg := GeneratorConfig{
		CheckName:   "test-check",
		Phase:       "deployment",
		Description: "Test check for testing",
		OutputDir:   outputDir,
	}

	err = GenerateCheck(cfg)
	if err != nil {
		t.Fatalf("GenerateCheck() error = %v", err)
	}

	// Verify files were created
	expectedFiles := []string{
		"test_check_check.go",
		"test_check_check_test.go",
		"test_check_recipe.yaml",
		"test_check_README.md",
	}

	for _, filename := range expectedFiles {
		path := filepath.Join(outputDir, filename)
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			t.Errorf("expected file %s was not created", filename)
		}
	}

	// Verify source file has expected content
	sourceContent, err := os.ReadFile(filepath.Join(outputDir, "test_check_check.go"))
	if err != nil {
		t.Fatalf("failed to read source file: %v", err)
	}
	contentStr := string(sourceContent)

	if !strings.Contains(contentStr, "package deployment") {
		t.Error("source file missing package declaration")
	}
	if !strings.Contains(contentStr, "RegisterCheck") {
		t.Error("source file missing RegisterCheck")
	}
	if !strings.Contains(contentStr, "test-check") {
		t.Error("source file missing check name")
	}

	// Verify test file has expected content
	testContent, err := os.ReadFile(filepath.Join(outputDir, "test_check_check_test.go"))
	if err != nil {
		t.Fatalf("failed to read test file: %v", err)
	}
	if !strings.Contains(string(testContent), "TestCheckTestCheck") {
		t.Error("test file missing test function")
	}
}

func TestGenerateCheck_EmptyName(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "generator-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := GeneratorConfig{
		CheckName:   "",
		Phase:       "deployment",
		Description: "Test",
		OutputDir:   tmpDir,
	}

	err = GenerateCheck(cfg)
	if err == nil {
		t.Error("expected error for empty check name, got nil")
	}
}

func TestGenerateCheck_EmptyPhase(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "generator-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	cfg := GeneratorConfig{
		CheckName:   "test-check",
		Phase:       "",
		Description: "Test",
		OutputDir:   tmpDir,
	}

	err = GenerateCheck(cfg)
	if err == nil {
		t.Error("expected error for empty phase, got nil")
	}
}
