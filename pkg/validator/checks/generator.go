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

	"github.com/NVIDIA/eidos/pkg/errors"
)

// GeneratorConfig holds configuration for generating validator code.
type GeneratorConfig struct {
	// ConstraintName is the constraint pattern (e.g., "Deployment.gpu-operator.version")
	ConstraintName string

	// CheckName is the check name (e.g., "operator-health")
	CheckName string

	// Phase is the validation phase (readiness, deployment, performance, conformance)
	Phase string

	// Description describes what this validator checks
	Description string

	// OutputDir is where to write generated files (e.g., "pkg/validator/checks/deployment")
	OutputDir string
}

// GenerateCheck generates files for a new check.
// Creates:
// - *_check.go: registration and validator function
// - *_check_test.go: integration test function
// - *_check_unit_test.go: unit test function
// - *_recipe.yaml: sample recipe
// - *_README.md: instructions
//
//nolint:dupl // Similar structure to GenerateConstraintValidator but different templates and logic
func GenerateCheck(cfg GeneratorConfig) error {
	if cfg.CheckName == "" {
		return fmt.Errorf("check name is required")
	}
	if cfg.Phase == "" {
		return fmt.Errorf("phase is required")
	}
	if cfg.OutputDir == "" {
		return fmt.Errorf("output directory is required")
	}

	// Derive names from check name
	// "operator-health" -> "OperatorHealth"
	funcName := checkNameToFuncName(cfg.CheckName)
	testName := "TestCheck" + funcName
	fileBaseName := toSnakeCase(funcName)

	if cfg.Description == "" {
		cfg.Description = fmt.Sprintf("Validates %s", cfg.CheckName)
	}

	// Generate source file with registration and validator
	checkFile := filepath.Join(cfg.OutputDir, fileBaseName+"_check.go")
	if err := generateCheckSourceFile(checkFile, funcName, testName, cfg); err != nil {
		return fmt.Errorf("failed to generate check file: %w", err)
	}

	// Generate test file (must end in _test.go for go test to find it)
	testFile := filepath.Join(cfg.OutputDir, fileBaseName+"_check_test.go")
	if err := generateCheckTestFile(testFile, funcName, testName, cfg); err != nil {
		return fmt.Errorf("failed to generate test file: %w", err)
	}

	// Generate unit test file
	unitTestFile := filepath.Join(cfg.OutputDir, fileBaseName+"_check_unit_test.go")
	if err := generateCheckUnitTestFile(unitTestFile, funcName, cfg); err != nil {
		return fmt.Errorf("failed to generate unit test file: %w", err)
	}

	// Generate sample recipe
	recipeFile := filepath.Join(cfg.OutputDir, fileBaseName+"_recipe.yaml")
	if err := generateCheckRecipeFile(recipeFile, cfg); err != nil {
		return fmt.Errorf("failed to generate recipe file: %w", err)
	}

	// Generate README
	readmeFile := filepath.Join(cfg.OutputDir, fileBaseName+"_README.md")
	if err := generateCheckReadmeFile(readmeFile, funcName, testName, checkFile, testFile, unitTestFile, recipeFile, cfg); err != nil {
		return fmt.Errorf("failed to generate README file: %w", err)
	}

	fmt.Printf("✓ Generated check:\n")
	fmt.Printf("  - %s\n", checkFile)
	fmt.Printf("  - %s\n", testFile)
	fmt.Printf("  - %s\n", unitTestFile)
	fmt.Printf("  - %s\n", recipeFile)
	fmt.Printf("  - %s\n", readmeFile)
	fmt.Printf("\nSee %s for instructions.\n", readmeFile)

	return nil
}

// checkNameToFuncName converts a check name to a function name.
// "operator-health" -> "OperatorHealth"
func checkNameToFuncName(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return r == '-' || r == '_'
	})

	for i, part := range parts {
		if len(part) > 0 {
			parts[i] = strings.ToUpper(string(part[0])) + part[1:]
		}
	}

	return strings.Join(parts, "")
}

func generateCheckRecipeFile(path string, cfg GeneratorConfig) error {
	tmpl := template.Must(template.New("checkRecipe").Parse(checkRecipeTemplate))

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return tmpl.Execute(f, map[string]string{
		"CheckName": cfg.CheckName,
		"Phase":     cfg.Phase,
	})
}

func generateCheckReadmeFile(path, funcName, testName, checkFile, testFile, unitTestFile, recipeFile string, cfg GeneratorConfig) error {
	tmpl := template.Must(template.New("checkReadme").Parse(checkReadmeTemplate))

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return tmpl.Execute(f, map[string]string{
		"CheckName":    cfg.CheckName,
		"FuncName":     funcName,
		"TestName":     testName,
		"Phase":        cfg.Phase,
		"CheckFile":    filepath.Base(checkFile),
		"TestFile":     filepath.Base(testFile),
		"UnitTestFile": filepath.Base(unitTestFile),
		"RecipeFile":   filepath.Base(recipeFile),
		"Description":  cfg.Description,
	})
}

func generateCheckUnitTestFile(path, funcName string, cfg GeneratorConfig) error {
	tmpl := template.Must(template.New("checkUnitTest").Parse(checkUnitTestTemplate))

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return tmpl.Execute(f, map[string]string{
		"Package":   sanitizePackageName(filepath.Base(cfg.OutputDir)),
		"FuncName":  funcName,
		"CheckName": cfg.CheckName,
	})
}

const checkRecipeTemplate = `# Sample recipe for testing {{.CheckName}} check
kind: recipeResult
apiVersion: eidos.nvidia.com/v1alpha1
metadata:
  version: dev
validation:
  {{.Phase}}:
    checks:
      - {{.CheckName}}
`

const checkReadmeTemplate = `# {{.CheckName}}

{{.Description}}

## Files

- ` + "`" + `{{.CheckFile}}` + "`" + ` - Check registration and validator function
- ` + "`" + `{{.TestFile}}` + "`" + ` - Integration test (runs in Kubernetes Jobs)
- ` + "`" + `{{.UnitTestFile}}` + "`" + ` - Unit test (runs locally with mocked context)
- ` + "`" + `{{.RecipeFile}}` + "`" + ` - Sample recipe for testing

## Implementation

1. Edit ` + "`" + `{{.CheckFile}}` + "`" + ` and implement ` + "`" + `validate{{.FuncName}}()` + "`" + `:

` + "```" + `go
func validate{{.FuncName}}(ctx *checks.ValidationContext) error {
    // Your validation logic here
    // Return nil if check passes, error if it fails
    return nil
}
` + "```" + `

2. Unit test locally:

` + "```" + `bash
go test -v -short -run {{.TestName}} ./pkg/validator/checks/{{.Phase}}/...
` + "```" + `

## Build and Run

Use a unique image tag (timestamp) to avoid caching issues:

` + "```" + `bash
# Generate unique tag
export IMAGE_TAG=$(date +%Y%m%d-%H%M%S)
export IMAGE=localhost:5001/eidos-validator:${IMAGE_TAG}

# Build and push
docker build -f Dockerfile.validator -t ${IMAGE} .
docker push ${IMAGE}

# Run validation
eidos validate \
  --recipe pkg/validator/checks/{{.Phase}}/{{.RecipeFile}} \
  --snapshot cm://gpu-operator/eidos-e2e-snapshot \
  --phase {{.Phase}} \
  --image ${IMAGE}
` + "```" + `

## Debugging

Verify the test is compiled into the image:

` + "```" + `bash
docker run --rm ${IMAGE} \
  go test -list ".*" ./pkg/validator/checks/{{.Phase}}/... 2>/dev/null | grep -i {{.FuncName}}
` + "```" + `

Keep resources for debugging:

` + "```" + `bash
eidos validate \
  --recipe pkg/validator/checks/{{.Phase}}/{{.RecipeFile}} \
  --snapshot snapshot.yaml \
  --phase {{.Phase}} \
  --image ${IMAGE} \
  --cleanup=false --debug

# Inspect Job logs
kubectl logs -l eidos.nvidia.com/job -n eidos-validation

# List Jobs
kubectl get jobs -n eidos-validation
` + "```" + `

## Troubleshooting

**"unregistered validations" error:**

This means the check is not found in the validator image. Use a new tag:

` + "```" + `bash
export IMAGE_TAG=$(date +%Y%m%d-%H%M%S)
export IMAGE=localhost:5001/eidos-validator:${IMAGE_TAG}
docker build -f Dockerfile.validator -t ${IMAGE} .
docker push ${IMAGE}
` + "```" + `

**"0 tests passed" or "no tests to run":**

The test function is not in the image. Verify and rebuild with a new tag:

` + "```" + `bash
# Verify test exists in image
docker run --rm ${IMAGE} \
  go test -list "{{.TestName}}" ./pkg/validator/checks/{{.Phase}}/...

# If not found, rebuild with new tag
export IMAGE_TAG=$(date +%Y%m%d-%H%M%S)
export IMAGE=localhost:5001/eidos-validator:${IMAGE_TAG}
docker build -f Dockerfile.validator -t ${IMAGE} .
docker push ${IMAGE}
` + "```" + `
`

func generateCheckSourceFile(path, funcName, testName string, cfg GeneratorConfig) error {
	tmpl := template.Must(template.New("checkSource").Parse(checkSourceTemplate))

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return tmpl.Execute(f, map[string]string{
		"Package":     sanitizePackageName(filepath.Base(cfg.OutputDir)),
		"FuncName":    funcName,
		"TestName":    testName,
		"CheckName":   cfg.CheckName,
		"Phase":       cfg.Phase,
		"Description": cfg.Description,
	})
}

func generateCheckTestFile(path, funcName, testName string, cfg GeneratorConfig) error {
	tmpl := template.Must(template.New("checkTest").Parse(checkTestTemplate))

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return tmpl.Execute(f, map[string]string{
		"Package":   sanitizePackageName(filepath.Base(cfg.OutputDir)),
		"FuncName":  funcName,
		"TestName":  testName,
		"CheckName": cfg.CheckName,
		"Phase":     cfg.Phase,
	})
}

// checkSourceTemplate is for the source file with registration and validator
const checkSourceTemplate = `// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package {{.Package}}

import (
	"github.com/NVIDIA/eidos/pkg/validator/checks"
)

func init() {
	// Register this check
	checks.RegisterCheck(&checks.Check{
		Name:        "{{.CheckName}}",
		Description: "{{.Description}}",
		Phase:       "{{.Phase}}",
		TestName:    "{{.TestName}}",
	})
}

// validate{{.FuncName}} is the validator function.
// Implement this function and unit test it separately.
// Returns nil if validation passes, error if it fails.
func validate{{.FuncName}}(ctx *checks.ValidationContext) error {
	// TODO: Implement validation logic
	return nil
}
`

// checkTestTemplate is for the test file
const checkTestTemplate = `// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package {{.Package}}

import (
	"testing"

	"github.com/NVIDIA/eidos/pkg/validator/checks"
)

// {{.TestName}} is the integration test for {{.CheckName}}.
// This runs inside validator Jobs and invokes the validator.
func {{.TestName}}(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Load Job environment
	runner, err := checks.NewTestRunner(t)
	if err != nil {
		t.Skipf("Not in Job environment: %v", err)
	}
	defer runner.Cancel()

	// Check if this check is enabled in recipe
	if !runner.HasCheck("{{.Phase}}", "{{.CheckName}}") {
		t.Skip("Check {{.CheckName}} not enabled in recipe")
	}

	t.Logf("Running check: {{.CheckName}}")

	// Run the validator
	ctx := runner.Context()
	err = validate{{.FuncName}}(ctx)

	if err != nil {
		t.Errorf("Check failed: %v", err)
	} else {
		t.Logf("✓ Check passed: {{.CheckName}}")
	}
}
`

// checkUnitTestTemplate is for the unit test file
const checkUnitTestTemplate = `// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package {{.Package}}

import (
	"context"
	"testing"

	"github.com/NVIDIA/eidos/pkg/validator/checks"
)

func TestValidate{{.FuncName}}(t *testing.T) {
	tests := []struct {
		name    string
		setup   func() *checks.ValidationContext
		wantErr bool
	}{
		{
			name: "success case",
			setup: func() *checks.ValidationContext {
				return &checks.ValidationContext{
					Context: context.Background(),
					// TODO: Add mock clientset if needed
					// Clientset: fake.NewSimpleClientset(...),
				}
			},
			wantErr: false,
		},
		// TODO: Add failure test cases when implementation is complete
		// {
		// 	name: "failure case - missing resource",
		// 	setup: func() *checks.ValidationContext {
		// 		return &checks.ValidationContext{
		// 			Context: context.Background(),
		// 			// Setup context that should cause failure
		// 		}
		// 	},
		// 	wantErr: true,
		// },
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.setup()
			err := validate{{.FuncName}}(ctx)

			if (err != nil) != tt.wantErr {
				t.Errorf("validate{{.FuncName}}() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestValidate{{.FuncName}}Registration(t *testing.T) {
	// Verify the check is registered
	check, ok := checks.GetCheck("{{.CheckName}}")
	if !ok {
		t.Fatal("{{.CheckName}} check not registered")
	}

	if check.Name != "{{.CheckName}}" {
		t.Errorf("Name = %v, want {{.CheckName}}", check.Name)
	}

	if check.Description == "" {
		t.Error("Description is empty")
	}

	if check.TestName == "" {
		t.Error("TestName is empty")
	}
}
`

// GenerateConstraintValidator generates files for a new constraint validator.
// Creates:
// - *_constraint.go: registration and validator function
// - *_constraint_test.go: integration test function
// - *_constraint_unit_test.go: unit test function
// - *_recipe.yaml: sample recipe
// - *_README.md: instructions
//
//nolint:dupl // Similar structure to GenerateCheck but different templates and logic
func GenerateConstraintValidator(cfg GeneratorConfig) error {
	if cfg.ConstraintName == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "constraint name is required")
	}
	if cfg.Phase == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "phase is required")
	}
	if cfg.OutputDir == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "output directory is required")
	}

	// Derive names from constraint
	// "Deployment.gpu-operator.version" -> "GPUOperatorVersion"
	funcName := constraintToFuncName(cfg.ConstraintName)
	testName := "Test" + funcName
	fileBaseName := toSnakeCase(funcName)

	if cfg.Description == "" {
		cfg.Description = fmt.Sprintf("Validates %s constraint", cfg.ConstraintName)
	}

	// Generate source file with registration and validator
	validatorFile := filepath.Join(cfg.OutputDir, fileBaseName+"_constraint.go")
	if err := generateConstraintSourceFile(validatorFile, funcName, testName, cfg); err != nil {
		return fmt.Errorf("failed to generate validator file: %w", err)
	}

	// Generate test file (must end in _test.go for go test to find it)
	testFile := filepath.Join(cfg.OutputDir, fileBaseName+"_constraint_test.go")
	if err := generateConstraintTestFile(testFile, funcName, testName, cfg); err != nil {
		return fmt.Errorf("failed to generate test file: %w", err)
	}

	// Generate unit test file
	unitTestFile := filepath.Join(cfg.OutputDir, fileBaseName+"_constraint_unit_test.go")
	if err := generateConstraintUnitTestFile(unitTestFile, funcName, cfg); err != nil {
		return fmt.Errorf("failed to generate unit test file: %w", err)
	}

	// Generate sample recipe
	recipeFile := filepath.Join(cfg.OutputDir, fileBaseName+"_recipe.yaml")
	if err := generateConstraintRecipeFile(recipeFile, cfg); err != nil {
		return fmt.Errorf("failed to generate recipe file: %w", err)
	}

	// Generate README
	readmeFile := filepath.Join(cfg.OutputDir, fileBaseName+"_README.md")
	if err := generateConstraintReadmeFile(readmeFile, funcName, testName, validatorFile, testFile, unitTestFile, recipeFile, cfg); err != nil {
		return fmt.Errorf("failed to generate README file: %w", err)
	}

	fmt.Printf("✓ Generated constraint validator:\n")
	fmt.Printf("  - %s\n", validatorFile)
	fmt.Printf("  - %s\n", testFile)
	fmt.Printf("  - %s\n", unitTestFile)
	fmt.Printf("  - %s\n", recipeFile)
	fmt.Printf("  - %s\n", readmeFile)
	fmt.Printf("\nSee %s for instructions.\n", readmeFile)

	return nil
}

func generateConstraintRecipeFile(path string, cfg GeneratorConfig) error {
	tmpl := template.Must(template.New("constraintRecipe").Parse(constraintRecipeTemplate))

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return tmpl.Execute(f, map[string]string{
		"ConstraintName": cfg.ConstraintName,
		"Phase":          cfg.Phase,
	})
}

func generateConstraintReadmeFile(path, funcName, testName, validatorFile, testFile, unitTestFile, recipeFile string, cfg GeneratorConfig) error {
	tmpl := template.Must(template.New("constraintReadme").Parse(constraintReadmeTemplate))

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return tmpl.Execute(f, map[string]string{
		"ConstraintName": cfg.ConstraintName,
		"FuncName":       funcName,
		"TestName":       testName,
		"Phase":          cfg.Phase,
		"ValidatorFile":  filepath.Base(validatorFile),
		"TestFile":       filepath.Base(testFile),
		"UnitTestFile":   filepath.Base(unitTestFile),
		"RecipeFile":     filepath.Base(recipeFile),
		"Description":    cfg.Description,
	})
}

func generateConstraintUnitTestFile(path, funcName string, cfg GeneratorConfig) error {
	tmpl := template.Must(template.New("constraintUnitTest").Parse(constraintUnitTestTemplate))

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return tmpl.Execute(f, map[string]string{
		"Package":        sanitizePackageName(filepath.Base(cfg.OutputDir)),
		"FuncName":       funcName,
		"ConstraintName": cfg.ConstraintName,
	})
}

func generateConstraintSourceFile(path, funcName, testName string, cfg GeneratorConfig) error {
	tmpl := template.Must(template.New("constraintSource").Parse(constraintSourceTemplate))

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return tmpl.Execute(f, map[string]string{
		"Package":        sanitizePackageName(filepath.Base(cfg.OutputDir)),
		"FuncName":       funcName,
		"TestName":       testName,
		"ConstraintName": cfg.ConstraintName,
		"Phase":          cfg.Phase,
		"Description":    cfg.Description,
	})
}

func generateConstraintTestFile(path, funcName, testName string, cfg GeneratorConfig) error {
	tmpl := template.Must(template.New("constraintTest").Parse(constraintTestTemplate))

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return tmpl.Execute(f, map[string]string{
		"Package":        sanitizePackageName(filepath.Base(cfg.OutputDir)),
		"FuncName":       funcName,
		"TestName":       testName,
		"ConstraintName": cfg.ConstraintName,
		"Phase":          cfg.Phase,
	})
}

const constraintRecipeTemplate = `# Sample recipe for testing {{.ConstraintName}} constraint
kind: recipeResult
apiVersion: eidos.nvidia.com/v1alpha1
metadata:
  version: dev
validation:
  {{.Phase}}:
    constraints:
      - name: {{.ConstraintName}}
        value: "expected-value"
`

const constraintSourceTemplate = `// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package {{.Package}}

import (
	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/validator/checks"
)

func init() {
	// Register this constraint validator
	checks.RegisterConstraintValidator(&checks.ConstraintValidator{
		Pattern:     "{{.ConstraintName}}",
		Description: "{{.Description}}",
		TestName:    "{{.TestName}}",
		Phase:       "{{.Phase}}",
	})
}

// validate{{.FuncName}} is the validator function.
// Implement this function and unit test it separately.
// Returns actual value, whether it passed, and any error.
func validate{{.FuncName}}(ctx *checks.ValidationContext, constraint recipe.Constraint) (string, bool, error) {
	// TODO: Implement validation logic
	return "not-implemented", true, nil
}
`

const constraintTestTemplate = `// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package {{.Package}}

import (
	"testing"

	"github.com/NVIDIA/eidos/pkg/validator/checks"
)

// {{.TestName}} validates the {{.ConstraintName}} constraint.
// This integration test runs inside validator Jobs and invokes the validator.
func {{.TestName}}(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Load Job environment
	runner, err := checks.NewTestRunner(t)
	if err != nil {
		t.Skipf("Not in Job environment: %v", err)
	}
	defer runner.Cancel()

	// Get constraint from recipe
	constraint := runner.GetConstraint("{{.Phase}}", "{{.ConstraintName}}")
	if constraint == nil {
		t.Skip("Constraint {{.ConstraintName}} not defined in recipe")
	}

	t.Logf("Validating constraint: %s = %s", constraint.Name, constraint.Value)

	// Run the validator
	ctx := runner.Context()
	actual, passed, err := validate{{.FuncName}}(ctx, *constraint)
	if err != nil {
		t.Fatalf("Validation failed: %v", err)
	}

	t.Logf("CONSTRAINT_RESULT: name=%s expected=%s actual=%s passed=%v",
		constraint.Name, constraint.Value, actual, passed)

	if !passed {
		t.Errorf("Constraint not satisfied: expected %s, got %s", constraint.Value, actual)
	} else {
		t.Logf("✓ Constraint satisfied: %s = %s", constraint.Name, actual)
	}
}
`

// constraintUnitTestTemplate is for the unit test file
const constraintUnitTestTemplate = `// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package {{.Package}}

import (
	"context"
	"testing"

	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/validator/checks"
)

func TestValidate{{.FuncName}}(t *testing.T) {
	tests := []struct {
		name       string
		setup      func() *checks.ValidationContext
		constraint recipe.Constraint
		wantActual string
		wantPassed bool
		wantErr    bool
	}{
		{
			name: "constraint satisfied",
			setup: func() *checks.ValidationContext {
				return &checks.ValidationContext{
					Context: context.Background(),
					// TODO: Add mock clientset if needed
					// Clientset: fake.NewSimpleClientset(...),
				}
			},
			constraint: recipe.Constraint{
				Name:  "{{.ConstraintName}}",
				Value: "expected-value",
			},
			wantActual: "not-implemented",
			wantPassed: true,
			wantErr:    false,
		},
		// TODO: Add constraint failure test cases when implementation is complete
		// {
		// 	name: "constraint not satisfied",
		// 	setup: func() *checks.ValidationContext {
		// 		return &checks.ValidationContext{
		// 			Context: context.Background(),
		// 			// Setup context that should cause constraint to fail
		// 		}
		// 	},
		// 	constraint: recipe.Constraint{
		// 		Name:  "{{.ConstraintName}}",
		// 		Value: "different-value",
		// 	},
		// 	wantActual: "actual-value",
		// 	wantPassed: false,
		// 	wantErr:    false,
		// },
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.setup()
			actual, passed, err := validate{{.FuncName}}(ctx, tt.constraint)

			if (err != nil) != tt.wantErr {
				t.Errorf("validate{{.FuncName}}() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				return
			}

			if actual != tt.wantActual {
				t.Errorf("validate{{.FuncName}}() actual = %v, want %v", actual, tt.wantActual)
			}

			if passed != tt.wantPassed {
				t.Errorf("validate{{.FuncName}}() passed = %v, want %v", passed, tt.wantPassed)
			}
		})
	}
}

func TestValidate{{.FuncName}}Registration(t *testing.T) {
	// Verify the constraint validator is registered
	validator, ok := checks.GetConstraintValidator("{{.ConstraintName}}")
	if !ok {
		t.Fatal("{{.ConstraintName}} constraint validator not registered")
	}

	if validator.Pattern != "{{.ConstraintName}}" {
		t.Errorf("Pattern = %v, want {{.ConstraintName}}", validator.Pattern)
	}

	if validator.Description == "" {
		t.Error("Description is empty")
	}

	if validator.TestName == "" {
		t.Error("TestName is empty")
	}
}
`

const constraintReadmeTemplate = `# {{.ConstraintName}}

{{.Description}}

## Files

- ` + "`" + `{{.ValidatorFile}}` + "`" + ` - Constraint registration and validator function
- ` + "`" + `{{.TestFile}}` + "`" + ` - Integration test (runs in Kubernetes Jobs)
- ` + "`" + `{{.UnitTestFile}}` + "`" + ` - Unit test (runs locally with mocked context)
- ` + "`" + `{{.RecipeFile}}` + "`" + ` - Sample recipe for testing

## Implementation

1. Edit ` + "`" + `{{.ValidatorFile}}` + "`" + ` and implement ` + "`" + `validate{{.FuncName}}()` + "`" + `:

` + "```" + `go
func validate{{.FuncName}}(ctx *checks.ValidationContext, constraint recipe.Constraint) (string, bool, error) {
    // Query cluster for actual value
    actual := "actual-value"

    // Compare against constraint.Value
    passed := actual == constraint.Value

    return actual, passed, nil
}
` + "```" + `

2. Edit ` + "`" + `{{.RecipeFile}}` + "`" + ` and set the expected value.

3. Unit test locally:

` + "```" + `bash
go test -v -short -run {{.TestName}} ./pkg/validator/checks/{{.Phase}}/...
` + "```" + `

## Build and Run

Use a unique image tag (timestamp) to avoid caching issues:

` + "```" + `bash
# Generate unique tag
export IMAGE_TAG=$(date +%Y%m%d-%H%M%S)
export IMAGE=localhost:5001/eidos-validator:${IMAGE_TAG}

# Build and push
docker build -f Dockerfile.validator -t ${IMAGE} .
docker push ${IMAGE}

# Run validation
eidos validate \
  --recipe pkg/validator/checks/{{.Phase}}/{{.RecipeFile}} \
  --snapshot cm://gpu-operator/eidos-e2e-snapshot \
  --phase {{.Phase}} \
  --image ${IMAGE}
` + "```" + `

## Debugging

Verify the test is compiled into the image:

` + "```" + `bash
docker run --rm ${IMAGE} \
  go test -list ".*" ./pkg/validator/checks/{{.Phase}}/... 2>/dev/null | grep -i {{.FuncName}}
` + "```" + `

Keep resources for debugging:

` + "```" + `bash
eidos validate \
  --recipe pkg/validator/checks/{{.Phase}}/{{.RecipeFile}} \
  --snapshot snapshot.yaml \
  --phase {{.Phase}} \
  --image ${IMAGE} \
  --cleanup=false --debug

# Inspect Job logs
kubectl logs -l eidos.nvidia.com/job -n eidos-validation

# List Jobs
kubectl get jobs -n eidos-validation
` + "```" + `

## Troubleshooting

**"unregistered validations" error:**

This means the constraint is not found in the validator image. Use a new tag:

` + "```" + `bash
export IMAGE_TAG=$(date +%Y%m%d-%H%M%S)
export IMAGE=localhost:5001/eidos-validator:${IMAGE_TAG}
docker build -f Dockerfile.validator -t ${IMAGE} .
docker push ${IMAGE}
` + "```" + `

**"0 tests passed" or "no tests to run":**

The test function is not in the image. Verify and rebuild with a new tag:

` + "```" + `bash
# Verify test exists in image
docker run --rm ${IMAGE} \
  go test -list "{{.TestName}}" ./pkg/validator/checks/{{.Phase}}/...

# If not found, rebuild with new tag
export IMAGE_TAG=$(date +%Y%m%d-%H%M%S)
export IMAGE=localhost:5001/eidos-validator:${IMAGE_TAG}
docker build -f Dockerfile.validator -t ${IMAGE} .
docker push ${IMAGE}
` + "```" + `
`

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

// sanitizePackageName converts a directory name to a valid Go package name.
// "gen-test" -> "gentest", "my_package" -> "my_package"
func sanitizePackageName(name string) string {
	// Replace dashes with empty string (Go package names can't have dashes)
	name = strings.ReplaceAll(name, "-", "")
	// Underscores are allowed but discouraged; keep them for now
	return strings.ToLower(name)
}
