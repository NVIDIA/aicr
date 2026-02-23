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
	"fmt"
	"strings"
	"sync"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// ValidationContext provides runtime context for checks and constraints.
type ValidationContext struct {
	// Context for cancellation and timeouts
	Context context.Context

	// Snapshot contains captured cluster state (hardware, OS, etc.)
	Snapshot *snapshotter.Snapshot

	// Namespace is the namespace where the validation is running
	Namespace string

	// Clientset provides Kubernetes API access for live cluster queries
	Clientset kubernetes.Interface

	// RESTConfig provides Kubernetes API access for cluster queries (used for e.g. remote command execution)
	RESTConfig *rest.Config

	// DynamicClient provides dynamic Kubernetes API access for reading custom resources (CRDs).
	// If nil, checks should create one from RESTConfig. Set this in unit tests for injection.
	DynamicClient dynamic.Interface

	// RecipeData contains recipe metadata that may be needed for validation
	RecipeData map[string]interface{}

	// Recipe contains the full recipe with validation constraints
	// Only available when running inside Jobs (not in unit tests)
	Recipe *recipe.RecipeResult
}

// CheckFunc is the function signature for a validation check.
// It validates a specific aspect of the cluster and reports results via t.
type CheckFunc func(ctx *ValidationContext) error

// ConstraintValidatorFunc is the function signature for constraint validation.
// It evaluates whether a constraint is satisfied against the cluster state.
// Returns the actual value found, whether it passed, and any error.
type ConstraintValidatorFunc func(ctx *ValidationContext, constraint recipe.Constraint) (actual string, passed bool, err error)

// Check represents a registered validation check.
type Check struct {
	// Name is the unique identifier for this check (e.g., "operator-health")
	Name string

	// Description explains what this check validates
	Description string

	// Phase indicates which validation phase this check belongs to
	Phase string // "readiness", "deployment", "performance", "conformance"

	// Func is the check implementation
	Func CheckFunc

	// TestName is the Go test function name (e.g., "TestCheckOperatorHealth")
	// If empty, derived from Name automatically
	TestName string

	// RequirementID is the CNCF conformance requirement ID (e.g., "dra_support").
	// Empty for checks that are not CNCF submission requirements.
	RequirementID string

	// EvidenceTitle is the human-readable title for evidence documents (e.g., "DRA Support").
	EvidenceTitle string

	// EvidenceDescription is a one-paragraph description for evidence documents.
	EvidenceDescription string

	// EvidenceFile is the output filename for evidence (e.g., "dra-support.md").
	// Multiple checks can share the same EvidenceFile (combined evidence).
	// Empty means this check produces no evidence file.
	EvidenceFile string

	// SubmissionRequirement indicates this check maps to a CNCF submission requirement.
	// Only checks with this set to true appear in the submission evidence index.
	SubmissionRequirement bool
}

// ConstraintValidator represents a registered constraint validator.
type ConstraintValidator struct {
	// Pattern is the constraint name pattern this validator handles
	// Examples: "GPU.*", "Deployment.gpu-operator.*", "k8s.version"
	Pattern string

	// Description explains what constraints this validator handles
	Description string

	// Func is the validator implementation
	Func ConstraintValidatorFunc

	// TestName is the Go test function name (e.g., "TestGPUOperatorVersion")
	// If empty, derived from Pattern automatically
	TestName string

	// Phase indicates which validation phase (deployment, performance, conformance)
	Phase string
}

// ConstraintTest represents a registered integration test for constraint validation.
// These tests run in Jobs and contain the actual validation logic.
type ConstraintTest struct {
	// TestName is the Go test function name (e.g., "TestGPUOperatorVersion")
	TestName string

	// Pattern is the constraint name this test validates (e.g., "Deployment.gpu-operator.version")
	Pattern string

	// Description explains what this test validates
	Description string

	// Phase indicates which validation phase (deployment, performance, conformance)
	Phase string
}

var (
	checkRegistry          = make(map[string]*Check)
	constraintRegistry     = make(map[string]*ConstraintValidator)
	constraintTestRegistry = make(map[string]*ConstraintTest)
	registryMu             sync.RWMutex
)

// RegisterCheck adds a check to the registry.
// This should be called from init() functions in check packages.
// If TestName is empty, it's derived from the Name automatically.
func RegisterCheck(check *Check) {
	registryMu.Lock()
	defer registryMu.Unlock()

	if _, exists := checkRegistry[check.Name]; exists {
		panic(fmt.Sprintf("check %q is already registered", check.Name))
	}

	// Auto-derive TestName if not provided
	if check.TestName == "" {
		check.TestName = "TestCheck" + patternToFuncName(check.Name)
	}

	checkRegistry[check.Name] = check
}

// GetTestNameForCheck looks up which test function validates a check.
// Returns the test name and true if found, empty string and false otherwise.
func GetTestNameForCheck(checkName string) (string, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	check, ok := checkRegistry[checkName]
	if !ok {
		return "", false
	}
	return check.TestName, true
}

// RegisterConstraintValidator adds a constraint validator to the registry.
// This should be called from init() functions in constraint validator packages.
// If TestName is empty, it's derived from the Pattern automatically.
func RegisterConstraintValidator(validator *ConstraintValidator) {
	registryMu.Lock()
	defer registryMu.Unlock()

	if _, exists := constraintRegistry[validator.Pattern]; exists {
		panic(fmt.Sprintf("constraint validator for pattern %q is already registered", validator.Pattern))
	}

	// Auto-derive TestName if not provided
	if validator.TestName == "" {
		validator.TestName = "Test" + patternToFuncName(validator.Pattern)
	}

	constraintRegistry[validator.Pattern] = validator
}

// GetCheck retrieves a registered check by name.
func GetCheck(name string) (*Check, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	check, ok := checkRegistry[name]
	return check, ok
}

// GetCheckByTestName does a reverse lookup: Go test name → Check.
func GetCheckByTestName(testName string) (*Check, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	for _, check := range checkRegistry {
		if check.TestName == testName {
			return check, true
		}
	}
	return nil, false
}

// ResolveCheck tries check name first, then test name.
// This handles the identity mismatch where CheckResult.Name can be either
// a check registry name (--no-cluster path) or a Go test name (normal cluster runs).
func ResolveCheck(name string) (*Check, bool) {
	if check, ok := GetCheck(name); ok {
		return check, true
	}
	return GetCheckByTestName(name)
}

// GetConstraintValidator retrieves a constraint validator by pattern.
// For now, uses exact match. Can be enhanced to support pattern matching.
func GetConstraintValidator(constraintName string) (*ConstraintValidator, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	// TODO(#142): Implement pattern matching (e.g., "GPU.*" matches "GPU.smi.count")
	// For now, use exact match or prefix match
	validator, ok := constraintRegistry[constraintName]
	return validator, ok
}

// ListChecks returns all registered checks, optionally filtered by phase.
func ListChecks(phase string) []*Check {
	registryMu.RLock()
	defer registryMu.RUnlock()

	var checks []*Check
	for _, check := range checkRegistry {
		if phase == "" || check.Phase == phase {
			checks = append(checks, check)
		}
	}
	return checks
}

// ListConstraintValidators returns all registered constraint validators.
func ListConstraintValidators() []*ConstraintValidator {
	registryMu.RLock()
	defer registryMu.RUnlock()

	validators := make([]*ConstraintValidator, 0, len(constraintRegistry))
	for _, validator := range constraintRegistry {
		validators = append(validators, validator)
	}
	return validators
}

// RegisterConstraintTest adds a constraint test to the registry.
// This maps constraint names to test function names for pattern building.
func RegisterConstraintTest(test *ConstraintTest) {
	registryMu.Lock()
	defer registryMu.Unlock()

	if _, exists := constraintTestRegistry[test.Pattern]; exists {
		panic(fmt.Sprintf("constraint test for pattern %q is already registered", test.Pattern))
	}

	constraintTestRegistry[test.Pattern] = test
}

// GetTestNameForConstraint looks up which test function validates a constraint.
// Returns the test name and true if found, empty string and false otherwise.
func GetTestNameForConstraint(constraintName string) (string, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	// First check constraint registry (preferred, single registration)
	if validator, ok := constraintRegistry[constraintName]; ok && validator.TestName != "" {
		return validator.TestName, true
	}

	// Fallback to legacy test registry for backwards compatibility
	test, ok := constraintTestRegistry[constraintName]
	if !ok {
		return "", false
	}
	return test.TestName, true
}

// patternToFuncName converts a constraint pattern to a function name.
// "Deployment.gpu-operator.version" -> "DeploymentGpuOperatorVersion"
func patternToFuncName(pattern string) string {
	var result []rune
	capitalizeNext := true

	for _, r := range pattern {
		if r == '.' || r == '-' || r == '_' {
			capitalizeNext = true
			continue
		}
		if capitalizeNext {
			result = append(result, []rune(strings.ToUpper(string(r)))...)
			capitalizeNext = false
		} else {
			result = append(result, r)
		}
	}
	return string(result)
}

// ListConstraintTests returns all registered constraint tests.
// Includes both legacy ConstraintTest registrations and new ConstraintValidator registrations with Phase.
// Deduplicates by pattern — new ConstraintValidator registrations take precedence.
func ListConstraintTests(phase string) []*ConstraintTest {
	registryMu.RLock()
	defer registryMu.RUnlock()

	var tests []*ConstraintTest
	seen := make(map[string]bool)

	// Include tests from constraint validators (new single-registration pattern)
	for _, validator := range constraintRegistry {
		if validator.Phase != "" && (phase == "" || validator.Phase == phase) {
			seen[validator.Pattern] = true
			tests = append(tests, &ConstraintTest{
				TestName:    validator.TestName,
				Pattern:     validator.Pattern,
				Description: validator.Description,
				Phase:       validator.Phase,
			})
		}
	}

	// Include legacy test registry for backwards compatibility (skip duplicates)
	for _, test := range constraintTestRegistry {
		if seen[test.Pattern] {
			continue
		}
		if phase == "" || test.Phase == phase {
			tests = append(tests, test)
		}
	}
	return tests
}
