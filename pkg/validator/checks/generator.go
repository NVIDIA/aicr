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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

// GeneratorConfig holds configuration for generating validator code.
type GeneratorConfig struct {
	// ConstraintName is the constraint pattern (e.g., "Deployment.gpu-operator.version")
	ConstraintName string

	// CheckName is the check name (e.g., "operator-health")
	CheckName string

	// Phase is the validation phase (deployment, performance, conformance)
	Phase string

	// Description describes what this validator checks
	Description string

	// OutputDir is where to write generated files (e.g., "pkg/validator/checks/deployment")
	OutputDir string
}

// GenerateConstraintValidator generates all files needed for a new constraint validator:
// - Helper functions file (*_validator.go)
// - Unit test file (*_validator_test.go)
// - Integration test file (*_validator_integration_test.go)
func GenerateConstraintValidator(cfg GeneratorConfig) error {
	if cfg.ConstraintName == "" {
		return fmt.Errorf("constraint name is required")
	}
	if cfg.Phase == "" {
		return fmt.Errorf("phase is required")
	}
	if cfg.OutputDir == "" {
		return fmt.Errorf("output directory is required")
	}

	// Derive names from constraint
	// "Deployment.gpu-operator.version" -> "GPUOperatorVersion"
	funcName := constraintToFuncName(cfg.ConstraintName)
	testName := "Test" + funcName
	fileBaseName := toSnakeCase(funcName)

	if cfg.Description == "" {
		cfg.Description = fmt.Sprintf("Validates %s constraint", cfg.ConstraintName)
	}

	// Generate helper functions file
	helperFile := filepath.Join(cfg.OutputDir, fileBaseName+".go")
	if err := generateHelperFile(helperFile, funcName, cfg); err != nil {
		return fmt.Errorf("failed to generate helper file: %w", err)
	}

	// Generate unit test file
	unitTestFile := filepath.Join(cfg.OutputDir, fileBaseName+"_test.go")
	if err := generateUnitTestFile(unitTestFile, funcName, cfg); err != nil {
		return fmt.Errorf("failed to generate unit test file: %w", err)
	}

	// Generate integration test file
	integrationTestFile := filepath.Join(cfg.OutputDir, fileBaseName+"_integration_test.go")
	if err := generateIntegrationTestFile(integrationTestFile, funcName, testName, cfg); err != nil {
		return fmt.Errorf("failed to generate integration test file: %w", err)
	}

	fmt.Printf("✓ Generated constraint validator files:\n")
	fmt.Printf("  - %s\n", helperFile)
	fmt.Printf("  - %s\n", unitTestFile)
	fmt.Printf("  - %s\n", integrationTestFile)
	fmt.Printf("\nNext steps:\n")
	fmt.Printf("1. Implement validation logic in %s\n", helperFile)
	fmt.Printf("2. Add test cases in %s\n", unitTestFile)
	fmt.Printf("3. Run tests: make test\n")
	fmt.Printf("4. Test in Job: eidos validate --recipe <recipe> --snapshot <snapshot>\n")

	return nil
}

// constraintToFuncName converts a constraint name to a function name.
// "Deployment.gpu-operator.version" -> "GPUOperatorVersion"
func constraintToFuncName(constraint string) string {
	// Split by dots and dashes
	parts := strings.FieldsFunc(constraint, func(r rune) bool {
		return r == '.' || r == '-'
	})

	// Capitalize each part
	for i, part := range parts {
		if len(part) > 0 {
			parts[i] = strings.ToUpper(string(part[0])) + part[1:]
		}
	}

	return strings.Join(parts, "")
}

// toSnakeCase converts CamelCase to snake_case.
// "GPUOperatorVersion" -> "gpu_operator_version"
func toSnakeCase(s string) string {
	var result strings.Builder
	for i, r := range s {
		if i > 0 && r >= 'A' && r <= 'Z' {
			result.WriteRune('_')
		}
		result.WriteRune(r)
	}
	return strings.ToLower(result.String())
}

func generateHelperFile(path, funcName string, cfg GeneratorConfig) error {
	tmpl := template.Must(template.New("helper").Parse(helperTemplate))

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return tmpl.Execute(f, map[string]string{
		"Package":        filepath.Base(cfg.OutputDir),
		"FuncName":       funcName,
		"ConstraintName": cfg.ConstraintName,
		"Description":    cfg.Description,
	})
}

func generateUnitTestFile(path, funcName string, cfg GeneratorConfig) error {
	tmpl := template.Must(template.New("unittest").Parse(unitTestTemplate))

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return tmpl.Execute(f, map[string]string{
		"Package":        filepath.Base(cfg.OutputDir),
		"FuncName":       funcName,
		"ConstraintName": cfg.ConstraintName,
	})
}

func generateIntegrationTestFile(path, funcName, testName string, cfg GeneratorConfig) error {
	tmpl := template.Must(template.New("integration").Parse(integrationTestTemplate))

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return tmpl.Execute(f, map[string]string{
		"Package":        filepath.Base(cfg.OutputDir),
		"FuncName":       funcName,
		"TestName":       testName,
		"ConstraintName": cfg.ConstraintName,
		"Phase":          cfg.Phase,
		"Description":    cfg.Description,
	})
}

const helperTemplate = `// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
// Licensed under the Apache License, Version 2.0 (the "License").

package {{.Package}}

import (
	"context"
	"fmt"

	"k8s.io/client-go/kubernetes"
)

// get{{.FuncName}} retrieves the value to validate against the constraint.
// TODO: Implement the logic to query Kubernetes and extract the value.
func get{{.FuncName}}(ctx context.Context, clientset kubernetes.Interface) (string, error) {
	// TODO: Implement value retrieval logic
	// Example:
	//   deployment, err := clientset.AppsV1().Deployments("namespace").Get(ctx, "name", metav1.GetOptions{})
	//   return deployment.Labels["version"], nil

	return "", fmt.Errorf("not implemented: get{{.FuncName}}")
}

// evaluate{{.FuncName}}Constraint evaluates if the actual value satisfies the constraint.
// TODO: Implement constraint evaluation logic.
func evaluate{{.FuncName}}Constraint(actualValue, constraintValue string) (bool, error) {
	// TODO: Implement constraint evaluation
	// Example for version constraints:
	//   return evaluateVersionConstraint(actualValue, constraintValue)

	return false, fmt.Errorf("not implemented: evaluate{{.FuncName}}Constraint")
}
`

const unitTestTemplate = `// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
// Licensed under the Apache License, Version 2.0 (the "License").

package {{.Package}}

import (
	"context"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// Test{{.FuncName}}Get tests the value retrieval logic with fake Kubernetes clientset.
func Test{{.FuncName}}Get(t *testing.T) {
	tests := []struct {
		name      string
		setup     func() *fake.Clientset
		want      string
		wantErr   bool
	}{
		{
			name: "TODO: add test case",
			setup: func() *fake.Clientset {
				// TODO: Create fake Kubernetes objects
				return fake.NewSimpleClientset()
			},
			want:    "expected-value",
			wantErr: false,
		},
		// TODO: Add more test cases (not found, missing labels, etc.)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := tt.setup()
			ctx := context.Background()

			got, err := get{{.FuncName}}(ctx, fakeClient)

			if (err != nil) != tt.wantErr {
				t.Errorf("get{{.FuncName}}() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("get{{.FuncName}}() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Test{{.FuncName}}Evaluate tests the constraint evaluation logic.
func Test{{.FuncName}}Evaluate(t *testing.T) {
	tests := []struct {
		name            string
		actualValue     string
		constraintValue string
		want            bool
		wantErr         bool
	}{
		{
			name:            "TODO: add test case",
			actualValue:     "actual",
			constraintValue: "expected",
			want:            true,
			wantErr:         false,
		},
		// TODO: Add more test cases
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := evaluate{{.FuncName}}Constraint(tt.actualValue, tt.constraintValue)

			if (err != nil) != tt.wantErr {
				t.Errorf("evaluate{{.FuncName}}Constraint() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("evaluate{{.FuncName}}Constraint() = %v, want %v", got, tt.want)
			}
		})
	}
}
`

const integrationTestTemplate = `// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
// Licensed under the Apache License, Version 2.0 (the "License").

package {{.Package}}

import (
	"testing"

	"github.com/NVIDIA/eidos/pkg/validator/checks"
)

func init() {
	// Register this test for pattern matching
	checks.RegisterConstraintTest(&checks.ConstraintTest{
		TestName:    "{{.TestName}}",
		Pattern:     "{{.ConstraintName}}",
		Description: "{{.Description}}",
		Phase:       "{{.Phase}}",
	})
}

// {{.TestName}} validates the {{.ConstraintName}} constraint.
// This integration test runs inside validator Jobs and contains the actual validation logic.
// It is excluded from local test runs via the -short flag.
func {{.TestName}}(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Load Job environment
	runner, err := checks.NewTestRunner(t)
	if err != nil {
		t.Skipf("Not in Job environment: %v", err)
	}
	defer runner.Cancel() // Clean up context when test completes

	// Get constraint from recipe
	constraint := runner.GetConstraint("{{.Phase}}", "{{.ConstraintName}}")
	if constraint == nil {
		t.Skip("Constraint {{.ConstraintName}} not defined in recipe")
	}

	t.Logf("Validating constraint: %s = %s", constraint.Name, constraint.Value)

	// Get actual value from cluster
	ctx := runner.Context()
	actualValue, err := get{{.FuncName}}(ctx.Context, ctx.Clientset)
	if err != nil {
		t.Fatalf("Failed to get value for {{.ConstraintName}}: %v", err)
	}

	t.Logf("Detected value: %s", actualValue)

	// Evaluate constraint
	passed, err := evaluate{{.FuncName}}Constraint(actualValue, constraint.Value)
	if err != nil {
		t.Fatalf("Failed to evaluate constraint: %v", err)
	}

	if !passed {
		t.Errorf("Value %s does not satisfy constraint %s", actualValue, constraint.Value)
	} else {
		t.Logf("✓ Value %s satisfies constraint %s", actualValue, constraint.Value)
	}
}
`
