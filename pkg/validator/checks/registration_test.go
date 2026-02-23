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

package checks_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/validator/checks"
	_ "github.com/NVIDIA/aicr/pkg/validator/checks/conformance" // Import for init() registration
	_ "github.com/NVIDIA/aicr/pkg/validator/checks/deployment"  // Import for init() registration
)

// TestConstraintRegistrationCompleteness ensures that every registered constraint
// has a corresponding integration test.
func TestConstraintRegistrationCompleteness(t *testing.T) {
	// Get all registered constraint tests
	constraintTests := checks.ListConstraintTests("")
	if len(constraintTests) == 0 {
		t.Skip("No constraint tests registered")
	}

	// Map of test names that exist
	existingTests := findTestFunctions(t)

	// Verify each registered constraint has a test
	var missing []string
	for _, ct := range constraintTests {
		if !existingTests[ct.TestName] {
			missing = append(missing, ct.TestName+" (pattern: "+ct.Pattern+")")
		}
	}

	if len(missing) > 0 {
		t.Errorf("Registered constraints missing test implementations:\n%s", strings.Join(missing, "\n"))
	}
}

// TestCheckRegistrationCompleteness ensures that every registered check
// has a corresponding integration test.
func TestCheckRegistrationCompleteness(t *testing.T) {
	// Get all registered checks
	allChecks := checks.ListChecks("")
	if len(allChecks) == 0 {
		t.Skip("No checks registered")
	}

	// Map of test names that exist
	existingTests := findTestFunctions(t)

	// Verify each registered check has a test
	var missing []string
	for _, check := range allChecks {
		// Use registered TestName if available, otherwise derive it
		expectedTestName := check.TestName
		if expectedTestName == "" {
			expectedTestName = checkNameToTestName(check.Name)
		}

		if !existingTests[expectedTestName] {
			missing = append(missing, expectedTestName+" (check: "+check.Name+")")
		}
	}

	if len(missing) > 0 {
		t.Errorf("Registered checks missing test implementations:\n%s", strings.Join(missing, "\n"))
	}
}

// TestIntegrationTestsAreRegistered ensures that every integration test
// has a corresponding registration.
func TestIntegrationTestsAreRegistered(t *testing.T) {
	// Find all integration test functions
	integrationTests := findIntegrationTestFunctions(t)
	if len(integrationTests) == 0 {
		t.Skip("No integration tests found")
	}

	// Get registered tests and checks
	constraintTests := checks.ListConstraintTests("")
	registeredTests := make(map[string]bool)
	for _, ct := range constraintTests {
		registeredTests[ct.TestName] = true
	}

	allChecks := checks.ListChecks("")
	for _, check := range allChecks {
		testName := check.TestName
		if testName == "" {
			testName = checkNameToTestName(check.Name)
		}
		registeredTests[testName] = true
	}

	// Verify each integration test is registered
	var unregistered []string
	for testName := range integrationTests {
		if !registeredTests[testName] {
			unregistered = append(unregistered, testName)
		}
	}

	if len(unregistered) > 0 {
		t.Errorf("Integration tests without registration (add RegisterConstraintTest or RegisterCheck):\n%s",
			strings.Join(unregistered, "\n"))
	}
}

// findTestFunctions finds all Test* functions in the validator checks packages.
func findTestFunctions(t *testing.T) map[string]bool {
	tests := make(map[string]bool)

	// Check all phase directories
	phaseDirs := []string{"deployment", "performance", "conformance"}

	for _, phaseDir := range phaseDirs {
		checksDir := filepath.Join(".", phaseDir)
		if _, err := os.Stat(checksDir); os.IsNotExist(err) {
			// Running from different directory, try relative path
			checksDir = filepath.Join("..", phaseDir)
		}
		if _, err := os.Stat(checksDir); os.IsNotExist(err) {
			// Directory doesn't exist, skip it
			continue
		}

		err := filepath.Walk(checksDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			// Only process *_test.go files
			if !strings.HasSuffix(path, "_test.go") {
				return nil
			}

			fset := token.NewFileSet()
			node, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Logf("Warning: failed to parse %s: %v", path, err)
				return nil
			}

			// Find all Test* functions
			for _, decl := range node.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok {
					if strings.HasPrefix(fn.Name.Name, "Test") {
						tests[fn.Name.Name] = true
					}
				}
			}

			return nil
		})

		if err != nil {
			t.Logf("Warning: error walking %s test files: %v", phaseDir, err)
		}
	}

	return tests
}

// findIntegrationTestFunctions finds all integration test functions
// (tests in *_integration_test.go files).
func findIntegrationTestFunctions(t *testing.T) map[string]bool {
	tests := make(map[string]bool)

	// Check all phase directories
	phaseDirs := []string{"deployment", "performance", "conformance"}

	for _, phaseDir := range phaseDirs {
		checksDir := filepath.Join(".", phaseDir)
		if _, err := os.Stat(checksDir); os.IsNotExist(err) {
			// Running from different directory, try relative path
			checksDir = filepath.Join("..", phaseDir)
		}
		if _, err := os.Stat(checksDir); os.IsNotExist(err) {
			// Directory doesn't exist, skip it
			continue
		}

		err := filepath.Walk(checksDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			// Only process *_integration_test.go files
			if !strings.HasSuffix(path, "_integration_test.go") {
				return nil
			}

			fset := token.NewFileSet()
			node, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Logf("Warning: failed to parse %s: %v", path, err)
				return nil
			}

			// Find all Test* functions
			for _, decl := range node.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok {
					if strings.HasPrefix(fn.Name.Name, "Test") {
						tests[fn.Name.Name] = true
					}
				}
			}

			return nil
		})

		if err != nil {
			t.Logf("Warning: error walking %s test files: %v", phaseDir, err)
		}
	}

	return tests
}

// TestEvidenceMetadataCompleteness ensures that every conformance check with
// SubmissionRequirement=true has complete evidence metadata.
func TestEvidenceMetadataCompleteness(t *testing.T) {
	conformanceChecks := checks.ListChecks("conformance")
	if len(conformanceChecks) == 0 {
		t.Skip("No conformance checks registered")
	}

	for _, check := range conformanceChecks {
		if !check.SubmissionRequirement {
			continue
		}

		t.Run(check.Name, func(t *testing.T) {
			if check.RequirementID == "" {
				t.Errorf("check %q has SubmissionRequirement=true but empty RequirementID", check.Name)
			}
			if check.EvidenceTitle == "" {
				t.Errorf("check %q has SubmissionRequirement=true but empty EvidenceTitle", check.Name)
			}
			if check.EvidenceDescription == "" {
				t.Errorf("check %q has SubmissionRequirement=true but empty EvidenceDescription", check.Name)
			}
			if check.EvidenceFile == "" {
				t.Errorf("check %q has SubmissionRequirement=true but empty EvidenceFile", check.Name)
			}
		})
	}
}

// TestResolveCheckDualLookup verifies that ResolveCheck finds checks by both
// check name and Go test name (handles the identity mismatch).
func TestResolveCheckDualLookup(t *testing.T) {
	conformanceChecks := checks.ListChecks("conformance")
	if len(conformanceChecks) == 0 {
		t.Skip("No conformance checks registered")
	}

	for _, check := range conformanceChecks {
		t.Run("by_name/"+check.Name, func(t *testing.T) {
			resolved, ok := checks.ResolveCheck(check.Name)
			if !ok {
				t.Errorf("ResolveCheck(%q) not found by check name", check.Name)
				return
			}
			if resolved.Name != check.Name {
				t.Errorf("ResolveCheck(%q) returned check %q", check.Name, resolved.Name)
			}
		})

		t.Run("by_test/"+check.TestName, func(t *testing.T) {
			resolved, ok := checks.ResolveCheck(check.TestName)
			if !ok {
				t.Errorf("ResolveCheck(%q) not found by test name", check.TestName)
				return
			}
			if resolved.Name != check.Name {
				t.Errorf("ResolveCheck(%q) returned check %q, want %q", check.TestName, resolved.Name, check.Name)
			}
		})
	}
}

// TestGetCheckByTestNameReverseLookup verifies that GetCheckByTestName
// can find all registered checks by their TestName.
func TestGetCheckByTestNameReverseLookup(t *testing.T) {
	allChecks := checks.ListChecks("")
	if len(allChecks) == 0 {
		t.Skip("No checks registered")
	}

	for _, check := range allChecks {
		if check.TestName == "" {
			continue
		}

		t.Run(check.TestName, func(t *testing.T) {
			found, ok := checks.GetCheckByTestName(check.TestName)
			if !ok {
				t.Errorf("GetCheckByTestName(%q) not found", check.TestName)
				return
			}
			if found.Name != check.Name {
				t.Errorf("GetCheckByTestName(%q) returned %q, want %q", check.TestName, found.Name, check.Name)
			}
		})
	}
}

// TestSharedEvidenceFileIntentional verifies that only known pairs of checks
// share an EvidenceFile (accelerator-metrics + ai-service-metrics).
func TestSharedEvidenceFileIntentional(t *testing.T) {
	conformanceChecks := checks.ListChecks("conformance")
	if len(conformanceChecks) == 0 {
		t.Skip("No conformance checks registered")
	}

	// Known intentional shared files: accelerator-metrics.md
	allowedShared := map[string]bool{
		"accelerator-metrics.md": true,
	}

	fileToChecks := make(map[string][]string)
	for _, check := range conformanceChecks {
		if check.EvidenceFile == "" {
			continue
		}
		fileToChecks[check.EvidenceFile] = append(fileToChecks[check.EvidenceFile], check.Name)
	}

	for file, checkNames := range fileToChecks {
		if len(checkNames) > 1 && !allowedShared[file] {
			t.Errorf("unexpected shared EvidenceFile %q used by checks: %v", file, checkNames)
		}
	}
}

// checkNameToTestName converts a check name to test function name.
// Example: "operator-health" -> "TestOperatorHealth"
func checkNameToTestName(checkName string) string {
	// Common acronyms that should be all-caps
	acronyms := map[string]string{
		"gpu":  "GPU",
		"api":  "API",
		"url":  "URL",
		"http": "HTTP",
		"nccl": "NCCL",
		"dcgm": "DCGM",
	}

	parts := strings.Split(checkName, "-")
	for i, part := range parts {
		if len(part) > 0 {
			// Check if it's a known acronym
			if acronym, ok := acronyms[strings.ToLower(part)]; ok {
				parts[i] = acronym
			} else {
				// Capitalize first letter
				parts[i] = strings.ToUpper(string(part[0])) + part[1:]
			}
		}
	}
	return "Test" + strings.Join(parts, "")
}
