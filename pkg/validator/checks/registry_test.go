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
	"sync"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestRegisterCheck tests check registration functionality
func TestRegisterCheck(t *testing.T) {
	// Save original registry state
	originalRegistry := make(map[string]*Check)
	registryMu.Lock()
	for k, v := range checkRegistry {
		originalRegistry[k] = v
	}
	registryMu.Unlock()

	// Clean up after test
	defer func() {
		registryMu.Lock()
		checkRegistry = originalRegistry
		registryMu.Unlock()
	}()

	tests := []struct {
		name      string
		check     *Check
		wantPanic bool
	}{
		{
			name: "register valid check",
			check: &Check{
				Name:        "test-check-1",
				Description: "Test check",
				Phase:       "readiness",
				Func: func(ctx *ValidationContext) error {
					return nil
				},
			},
			wantPanic: false,
		},
		{
			name: "register check with empty description",
			check: &Check{
				Name:  "test-check-2",
				Phase: "deployment",
				Func: func(ctx *ValidationContext) error {
					return nil
				},
			},
			wantPanic: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear registry for clean test
			registryMu.Lock()
			checkRegistry = make(map[string]*Check)
			registryMu.Unlock()

			defer func() {
				r := recover()
				if (r != nil) != tt.wantPanic {
					t.Errorf("RegisterCheck() panic = %v, wantPanic %v", r, tt.wantPanic)
				}
			}()

			RegisterCheck(tt.check)

			// Verify check was registered
			if !tt.wantPanic {
				check, ok := GetCheck(tt.check.Name)
				if !ok {
					t.Errorf("Check %q not found after registration", tt.check.Name)
				}
				if check.Name != tt.check.Name {
					t.Errorf("Check Name = %v, want %v", check.Name, tt.check.Name)
				}
				if check.Phase != tt.check.Phase {
					t.Errorf("Check Phase = %v, want %v", check.Phase, tt.check.Phase)
				}
			}
		})
	}
}

func TestRegisterCheckDuplicate(t *testing.T) {
	// Save and restore registry
	originalRegistry := make(map[string]*Check)
	registryMu.Lock()
	for k, v := range checkRegistry {
		originalRegistry[k] = v
	}
	checkRegistry = make(map[string]*Check)
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		checkRegistry = originalRegistry
		registryMu.Unlock()
	}()

	check := &Check{
		Name:        "duplicate-check",
		Description: "Test duplicate",
		Phase:       "readiness",
		Func:        func(ctx *ValidationContext) error { return nil },
	}

	// Register once - should succeed
	RegisterCheck(check)

	// Register again - should panic
	defer func() {
		if r := recover(); r == nil {
			t.Error("RegisterCheck() should panic on duplicate, but did not")
		}
	}()

	RegisterCheck(check)
}

func TestGetTestNameForCheck(t *testing.T) {
	// Save and restore registry
	originalRegistry := make(map[string]*Check)
	registryMu.Lock()
	for k, v := range checkRegistry {
		originalRegistry[k] = v
	}
	checkRegistry = make(map[string]*Check)
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		checkRegistry = originalRegistry
		registryMu.Unlock()
	}()

	// Register a check with explicit TestName
	RegisterCheck(&Check{
		Name:        "test-check",
		Description: "Test check",
		Phase:       "deployment",
		TestName:    "TestMyCheck",
	})

	// Register a check without TestName (should auto-derive)
	RegisterCheck(&Check{
		Name:        "auto-test",
		Description: "Auto test",
		Phase:       "deployment",
	})

	tests := []struct {
		name         string
		checkName    string
		wantTestName string
		wantFound    bool
	}{
		{
			name:         "explicit test name",
			checkName:    "test-check",
			wantTestName: "TestMyCheck",
			wantFound:    true,
		},
		{
			name:         "auto-derived test name",
			checkName:    "auto-test",
			wantTestName: "TestCheckAutoTest",
			wantFound:    true,
		},
		{
			name:         "non-existent check",
			checkName:    "unknown-check",
			wantTestName: "",
			wantFound:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTestName, gotFound := GetTestNameForCheck(tt.checkName)
			if gotTestName != tt.wantTestName {
				t.Errorf("GetTestNameForCheck() testName = %v, want %v", gotTestName, tt.wantTestName)
			}
			if gotFound != tt.wantFound {
				t.Errorf("GetTestNameForCheck() found = %v, want %v", gotFound, tt.wantFound)
			}
		})
	}
}

func TestGetCheck(t *testing.T) {
	// Save and restore registry
	originalRegistry := make(map[string]*Check)
	registryMu.Lock()
	for k, v := range checkRegistry {
		originalRegistry[k] = v
	}
	checkRegistry = make(map[string]*Check)
	checkRegistry["existing-check"] = &Check{
		Name:  "existing-check",
		Phase: "readiness",
	}
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		checkRegistry = originalRegistry
		registryMu.Unlock()
	}()

	tests := []struct {
		name      string
		checkName string
		wantOk    bool
	}{
		{
			name:      "get existing check",
			checkName: "existing-check",
			wantOk:    true,
		},
		{
			name:      "get non-existent check",
			checkName: "non-existent",
			wantOk:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			check, ok := GetCheck(tt.checkName)
			if ok != tt.wantOk {
				t.Errorf("GetCheck() ok = %v, want %v", ok, tt.wantOk)
			}
			if tt.wantOk && check.Name != tt.checkName {
				t.Errorf("GetCheck() Name = %v, want %v", check.Name, tt.checkName)
			}
		})
	}
}

func TestListChecks(t *testing.T) {
	// Save and restore registry
	originalRegistry := make(map[string]*Check)
	registryMu.Lock()
	for k, v := range checkRegistry {
		originalRegistry[k] = v
	}
	checkRegistry = map[string]*Check{
		"deployment-1":  {Name: "deployment-1", Phase: "deployment"},
		"deployment-2":  {Name: "deployment-2", Phase: "deployment"},
		"performance-1": {Name: "performance-1", Phase: "performance"},
		"conformance-1": {Name: "conformance-1", Phase: "conformance"},
	}
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		checkRegistry = originalRegistry
		registryMu.Unlock()
	}()

	tests := []struct {
		name      string
		phase     string
		wantCount int
	}{
		{
			name:      "list all checks",
			phase:     "",
			wantCount: 4,
		},
		{
			name:      "list deployment checks",
			phase:     "deployment",
			wantCount: 2,
		},
		{
			name:      "list performance checks",
			phase:     "performance",
			wantCount: 1,
		},
		{
			name:      "list conformance checks",
			phase:     "conformance",
			wantCount: 1,
		},
		{
			name:      "list non-existent phase",
			phase:     "readiness",
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checks := ListChecks(tt.phase)
			if len(checks) != tt.wantCount {
				t.Errorf("ListChecks() count = %v, want %v", len(checks), tt.wantCount)
			}

			// Verify all returned checks match the phase filter
			if tt.phase != "" {
				for _, check := range checks {
					if check.Phase != tt.phase {
						t.Errorf("ListChecks() returned check with phase %q, want %q", check.Phase, tt.phase)
					}
				}
			}
		})
	}
}

func TestRegisterConstraintValidator(t *testing.T) {
	// Save and restore registry
	originalRegistry := make(map[string]*ConstraintValidator)
	registryMu.Lock()
	for k, v := range constraintRegistry {
		originalRegistry[k] = v
	}
	constraintRegistry = make(map[string]*ConstraintValidator)
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		constraintRegistry = originalRegistry
		registryMu.Unlock()
	}()

	validator := &ConstraintValidator{
		Pattern:     "test.pattern",
		Description: "Test validator",
		Func: func(ctx *ValidationContext, constraint recipe.Constraint) (string, bool, error) {
			return "test", true, nil
		},
	}

	// Register validator
	RegisterConstraintValidator(validator)

	// Verify registration
	retrieved, ok := GetConstraintValidator("test.pattern")
	if !ok {
		t.Fatal("ConstraintValidator not found after registration")
	}

	if retrieved.Pattern != validator.Pattern {
		t.Errorf("Pattern = %v, want %v", retrieved.Pattern, validator.Pattern)
	}
}

func TestRegisterConstraintValidatorDuplicate(t *testing.T) {
	// Save and restore registry
	originalRegistry := make(map[string]*ConstraintValidator)
	registryMu.Lock()
	for k, v := range constraintRegistry {
		originalRegistry[k] = v
	}
	constraintRegistry = make(map[string]*ConstraintValidator)
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		constraintRegistry = originalRegistry
		registryMu.Unlock()
	}()

	validator := &ConstraintValidator{
		Pattern: "duplicate.pattern",
		Func: func(ctx *ValidationContext, constraint recipe.Constraint) (string, bool, error) {
			return "", false, nil
		},
	}

	// Register once
	RegisterConstraintValidator(validator)

	// Register again - should panic
	defer func() {
		if r := recover(); r == nil {
			t.Error("RegisterConstraintValidator() should panic on duplicate, but did not")
		}
	}()

	RegisterConstraintValidator(validator)
}

func TestGetConstraintValidator(t *testing.T) {
	// Save and restore registry
	originalRegistry := make(map[string]*ConstraintValidator)
	registryMu.Lock()
	for k, v := range constraintRegistry {
		originalRegistry[k] = v
	}
	constraintRegistry = map[string]*ConstraintValidator{
		"existing.pattern": {Pattern: "existing.pattern"},
	}
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		constraintRegistry = originalRegistry
		registryMu.Unlock()
	}()

	tests := []struct {
		name    string
		pattern string
		wantOk  bool
	}{
		{
			name:    "get existing validator",
			pattern: "existing.pattern",
			wantOk:  true,
		},
		{
			name:    "get non-existent validator",
			pattern: "non.existent",
			wantOk:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validator, ok := GetConstraintValidator(tt.pattern)
			if ok != tt.wantOk {
				t.Errorf("GetConstraintValidator() ok = %v, want %v", ok, tt.wantOk)
			}
			if tt.wantOk && validator.Pattern != tt.pattern {
				t.Errorf("GetConstraintValidator() Pattern = %v, want %v", validator.Pattern, tt.pattern)
			}
		})
	}
}

func TestListConstraintValidators(t *testing.T) {
	// Save and restore registry
	originalRegistry := make(map[string]*ConstraintValidator)
	registryMu.Lock()
	for k, v := range constraintRegistry {
		originalRegistry[k] = v
	}
	constraintRegistry = map[string]*ConstraintValidator{
		"pattern1": {Pattern: "pattern1"},
		"pattern2": {Pattern: "pattern2"},
		"pattern3": {Pattern: "pattern3"},
	}
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		constraintRegistry = originalRegistry
		registryMu.Unlock()
	}()

	validators := ListConstraintValidators()

	if len(validators) != 3 {
		t.Errorf("ListConstraintValidators() count = %v, want 3", len(validators))
	}

	// Verify all validators are included
	patterns := make(map[string]bool)
	for _, v := range validators {
		patterns[v.Pattern] = true
	}

	expectedPatterns := []string{"pattern1", "pattern2", "pattern3"}
	for _, expected := range expectedPatterns {
		if !patterns[expected] {
			t.Errorf("ListConstraintValidators() missing pattern %q", expected)
		}
	}
}

func TestRegistryConcurrency(t *testing.T) {
	// Save and restore registry
	originalCheckRegistry := make(map[string]*Check)
	originalConstraintRegistry := make(map[string]*ConstraintValidator)
	registryMu.Lock()
	for k, v := range checkRegistry {
		originalCheckRegistry[k] = v
	}
	for k, v := range constraintRegistry {
		originalConstraintRegistry[k] = v
	}
	checkRegistry = make(map[string]*Check)
	constraintRegistry = make(map[string]*ConstraintValidator)
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		checkRegistry = originalCheckRegistry
		constraintRegistry = originalConstraintRegistry
		registryMu.Unlock()
	}()

	// Test concurrent registration and retrieval
	var wg sync.WaitGroup
	const goroutines = 10

	// Concurrent check registration
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			check := &Check{
				Name:  fmt.Sprintf("concurrent-check-%d", id),
				Phase: "readiness",
				Func:  func(ctx *ValidationContext) error { return nil },
			}
			RegisterCheck(check)
		}(i)
	}

	// Concurrent check retrieval
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = ListChecks("")
		}()
	}

	wg.Wait()

	// Verify all checks were registered
	checks := ListChecks("")
	if len(checks) != goroutines {
		t.Errorf("Concurrent registration: got %d checks, want %d", len(checks), goroutines)
	}
}

func TestRegisterConstraintTest(t *testing.T) {
	// Save and restore registry
	originalRegistry := make(map[string]*ConstraintTest)
	registryMu.Lock()
	for k, v := range constraintTestRegistry {
		originalRegistry[k] = v
	}
	constraintTestRegistry = make(map[string]*ConstraintTest)
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		constraintTestRegistry = originalRegistry
		registryMu.Unlock()
	}()

	tests := []struct {
		name      string
		test      *ConstraintTest
		wantPanic bool
	}{
		{
			name: "register valid constraint test",
			test: &ConstraintTest{
				TestName:    "TestMyConstraint",
				Pattern:     "Deployment.my-app.version",
				Description: "Validates my-app version",
				Phase:       "deployment",
			},
			wantPanic: false,
		},
		{
			name: "register constraint test with nil",
			test: nil,
			// Panics because it accesses test.Pattern without nil check
			// This is consistent with RegisterCheck behavior
			wantPanic: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if (r != nil) != tt.wantPanic {
					t.Errorf("RegisterConstraintTest() panic = %v, wantPanic = %v", r, tt.wantPanic)
				}
			}()

			RegisterConstraintTest(tt.test)

			if tt.test != nil && !tt.wantPanic {
				// Verify registration
				testName, found := GetTestNameForConstraint(tt.test.Pattern)
				if !found {
					t.Errorf("constraint test not found after registration")
				}
				if testName != tt.test.TestName {
					t.Errorf("GetTestNameForConstraint() = %v, want %v", testName, tt.test.TestName)
				}
			}
		})
	}
}

func TestGetTestNameForConstraint(t *testing.T) {
	// Save and restore both registries
	originalTestRegistry := make(map[string]*ConstraintTest)
	originalValidatorRegistry := make(map[string]*ConstraintValidator)
	registryMu.Lock()
	for k, v := range constraintTestRegistry {
		originalTestRegistry[k] = v
	}
	for k, v := range constraintRegistry {
		originalValidatorRegistry[k] = v
	}
	constraintTestRegistry = make(map[string]*ConstraintTest)
	constraintRegistry = make(map[string]*ConstraintValidator)
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		constraintTestRegistry = originalTestRegistry
		constraintRegistry = originalValidatorRegistry
		registryMu.Unlock()
	}()

	// Register a test constraint via ConstraintValidator (preferred single-registration)
	RegisterConstraintValidator(&ConstraintValidator{
		Pattern:     "Deployment.gpu-operator.version",
		Description: "Validates GPU operator version",
		TestName:    "TestGPUOperatorVersion",
		Phase:       "deployment",
	})

	tests := []struct {
		name           string
		constraintName string
		wantTestName   string
		wantFound      bool
	}{
		{
			name:           "existing constraint",
			constraintName: "Deployment.gpu-operator.version",
			wantTestName:   "TestGPUOperatorVersion",
			wantFound:      true,
		},
		{
			name:           "non-existent constraint",
			constraintName: "Deployment.unknown.version",
			wantTestName:   "",
			wantFound:      false,
		},
		{
			name:           "empty constraint name",
			constraintName: "",
			wantTestName:   "",
			wantFound:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTestName, gotFound := GetTestNameForConstraint(tt.constraintName)
			if gotTestName != tt.wantTestName {
				t.Errorf("GetTestNameForConstraint() testName = %v, want %v", gotTestName, tt.wantTestName)
			}
			if gotFound != tt.wantFound {
				t.Errorf("GetTestNameForConstraint() found = %v, want %v", gotFound, tt.wantFound)
			}
		})
	}
}

func TestListConstraintTests(t *testing.T) {
	// Save and restore both registries
	originalTestRegistry := make(map[string]*ConstraintTest)
	originalValidatorRegistry := make(map[string]*ConstraintValidator)
	registryMu.Lock()
	for k, v := range constraintTestRegistry {
		originalTestRegistry[k] = v
	}
	for k, v := range constraintRegistry {
		originalValidatorRegistry[k] = v
	}
	constraintTestRegistry = make(map[string]*ConstraintTest)
	constraintRegistry = make(map[string]*ConstraintValidator)
	registryMu.Unlock()

	defer func() {
		registryMu.Lock()
		constraintTestRegistry = originalTestRegistry
		constraintRegistry = originalValidatorRegistry
		registryMu.Unlock()
	}()

	// Register test constraints using preferred single-registration pattern
	RegisterConstraintValidator(&ConstraintValidator{
		Pattern:  "Deployment.test1.version",
		TestName: "TestDeploymentConstraint1",
		Phase:    "deployment",
	})
	RegisterConstraintValidator(&ConstraintValidator{
		Pattern:  "Deployment.test2.version",
		TestName: "TestDeploymentConstraint2",
		Phase:    "deployment",
	})
	RegisterConstraintValidator(&ConstraintValidator{
		Pattern:  "Readiness.test.version",
		TestName: "TestReadinessConstraint",
		Phase:    "readiness",
	})

	tests := []struct {
		name      string
		phase     string
		wantCount int
	}{
		{
			name:      "all phases",
			phase:     "",
			wantCount: 3,
		},
		{
			name:      "deployment phase only",
			phase:     "deployment",
			wantCount: 2,
		},
		{
			name:      "readiness phase only",
			phase:     "readiness",
			wantCount: 1,
		},
		{
			name:      "non-existent phase",
			phase:     "conformance",
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ListConstraintTests(tt.phase)
			if len(got) != tt.wantCount {
				t.Errorf("ListConstraintTests(%q) returned %d tests, want %d", tt.phase, len(got), tt.wantCount)
			}
		})
	}
}
