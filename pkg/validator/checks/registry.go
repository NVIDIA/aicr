// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

	// Artifacts collects diagnostic evidence during check execution.
	// Nil when artifact capture is not active (e.g., non-conformance phases).
	// Checks should nil-check before recording.
	Artifacts *ArtifactCollector
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
	// Name is the unique identifier for this check (e.g., "expected-resources")
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
	// Name is the unique identifier for this constraint (e.g., "Deployment.gpu-operator.version")
	Name string

	// Description explains what constraints this validator handles
	Description string

	// Func is the validator implementation
	Func ConstraintValidatorFunc

	// TestName is the Go test function name (e.g., "TestGPUOperatorVersion")
	// If empty, derived from Name automatically
	TestName string

	// Phase indicates which validation phase (deployment, performance, conformance)
	Phase string
}

// The check and constraint registries serve two purposes:
//
//  1. Name → test function mapping: The validator orchestrator (pkg/validator) looks up
//     registered test names to build -test.run patterns for Jobs it deploys.
//
//  2. Function dispatch: Inside those Jobs, TestRunner.RunCheck() and runtime tests
//     look up Func by name and call it directly (runner.go, deployment/runtime_test.go).
//
// Both purposes require init()-time registration, which is why Func must be populated
// even though the orchestrator never calls it.
var (
	checkRegistry      = make(map[string]*Check)
	constraintRegistry = make(map[string]*ConstraintValidator)
	registryMu         sync.RWMutex
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
		check.TestName = "TestCheck" + nameToFuncName(check.Name)
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
// If TestName is empty, it's derived from the Name automatically.
func RegisterConstraintValidator(validator *ConstraintValidator) {
	registryMu.Lock()
	defer registryMu.Unlock()

	if _, exists := constraintRegistry[validator.Name]; exists {
		panic(fmt.Sprintf("constraint validator %q is already registered", validator.Name))
	}

	// Auto-derive TestName if not provided
	if validator.TestName == "" {
		validator.TestName = "Test" + nameToFuncName(validator.Name)
	}

	constraintRegistry[validator.Name] = validator
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

// GetConstraintValidator retrieves a constraint validator by name.
func GetConstraintValidator(constraintName string) (*ConstraintValidator, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()

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

// GetTestNameForConstraint looks up which test function validates a constraint.
// Returns the test name and true if found, empty string and false otherwise.
func GetTestNameForConstraint(constraintName string) (string, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()

	if validator, ok := constraintRegistry[constraintName]; ok && validator.TestName != "" {
		return validator.TestName, true
	}

	return "", false
}

// nameToFuncName converts a dotted/dashed name to a Go function name.
// "Deployment.gpu-operator.version" -> "DeploymentGpuOperatorVersion"
func nameToFuncName(name string) string {
	var result []rune
	capitalizeNext := true

	for _, r := range name {
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

// ListConstraintTests returns all registered constraint validators, optionally filtered by phase.
func ListConstraintTests(phase string) []*ConstraintValidator {
	registryMu.RLock()
	defer registryMu.RUnlock()

	var validators []*ConstraintValidator
	for _, validator := range constraintRegistry {
		if validator.Phase != "" && (phase == "" || validator.Phase == phase) {
			validators = append(validators, validator)
		}
	}
	return validators
}
