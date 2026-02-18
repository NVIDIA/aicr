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
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/eidos/pkg/errors"
	"github.com/NVIDIA/eidos/pkg/recipe"
	"github.com/NVIDIA/eidos/pkg/serializer"
	"github.com/NVIDIA/eidos/pkg/snapshotter"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// testingT is a minimal interface that matches the testing.T methods we use.
// This allows for easier testing of the TestRunner itself.
type testingT interface {
	Fatalf(format string, args ...interface{})
	Logf(format string, args ...interface{})
	Helper()
}

// TestRunner provides infrastructure for running validation checks as Go tests inside Kubernetes Jobs.
//
// The test runner bridges the gap between Go's test framework and the Eidos validation system:
//   - Loads ValidationContext from Job environment (snapshot, K8s client, recipe)
//   - Looks up registered checks by name
//   - Executes checks and reports results via testing.T
//
// Example usage in test wrappers:
//
//	func TestGPUHardwareDetection(t *testing.T) {
//	    runner, err := checks.NewTestRunner(t)
//	    if err != nil {
//	        t.Skipf("Skipping integration test (not in Kubernetes): %v", err)
//	        return
//	    }
//	    defer runner.Cancel() // Clean up context when test completes
//	    runner.RunCheck("gpu-hardware-detection")
//	}
type TestRunner struct {
	t      testingT
	ctx    *ValidationContext
	cancel context.CancelFunc
}

// NewTestRunner creates a test runner by loading ValidationContext from the Job environment.
// Expected environment variables:
//   - EIDOS_SNAPSHOT_PATH: Path to mounted snapshot file (default: /data/snapshot/snapshot.yaml)
//   - EIDOS_RECIPE_DATA: Optional JSON-encoded recipe metadata
//
// IMPORTANT: Callers should call Cancel() when done to release resources.
func NewTestRunner(t *testing.T) (*TestRunner, error) {
	ctx, cancel, err := LoadValidationContext()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to load validation context", err)
	}

	return &TestRunner{
		t:      t,
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

// Cancel releases resources associated with the test runner.
// Should be called via defer after NewTestRunner succeeds.
func (r *TestRunner) Cancel() {
	if r.cancel != nil {
		r.cancel()
	}
}

// RunCheck executes a registered validation check by name.
// The check must be registered via RegisterCheck() (usually in an init() function).
func (r *TestRunner) RunCheck(checkName string) {
	check, ok := GetCheck(checkName)
	if !ok {
		r.t.Fatalf("Check %q not found in registry", checkName)
	}

	r.t.Logf("Running check: %s - %s", check.Name, check.Description)

	err := check.Func(r.ctx)
	if err != nil {
		r.t.Fatalf("Check failed: %v", err)
	}

	r.t.Logf("Check passed: %s", check.Name)
}

// GetConstraint retrieves a constraint by name from the recipe for the current phase.
// Returns nil if the recipe doesn't contain the constraint.
// This is used by integration tests to get constraint values to validate against.
func (r *TestRunner) GetConstraint(phase, constraintName string) *recipe.Constraint {
	if r.ctx.Recipe == nil || r.ctx.Recipe.Validation == nil {
		return nil
	}

	var constraints []recipe.Constraint
	switch phase {
	case "deployment":
		if r.ctx.Recipe.Validation.Deployment != nil {
			constraints = r.ctx.Recipe.Validation.Deployment.Constraints
		}
	case "performance":
		if r.ctx.Recipe.Validation.Performance != nil {
			constraints = r.ctx.Recipe.Validation.Performance.Constraints
		}
	case "conformance":
		if r.ctx.Recipe.Validation.Conformance != nil {
			constraints = r.ctx.Recipe.Validation.Conformance.Constraints
		}
	}

	for i := range constraints {
		if constraints[i].Name == constraintName {
			return &constraints[i]
		}
	}

	return nil
}

// Context returns the validation context for direct access.
// Use this when you need the Kubernetes client, snapshot, or other context data.
func (r *TestRunner) Context() *ValidationContext {
	return r.ctx
}

// HasCheck checks if a check is enabled in the recipe for a given phase.
// Returns true if the check is listed in the recipe's checks for that phase.
func (r *TestRunner) HasCheck(phase, checkName string) bool {
	if r.ctx.Recipe == nil || r.ctx.Recipe.Validation == nil {
		return false
	}

	var checkList []string
	switch phase {
	case "readiness", "preDeployment":
		if r.ctx.Recipe.Validation.PreDeployment != nil {
			checkList = r.ctx.Recipe.Validation.PreDeployment.Checks
		}
	case "deployment":
		if r.ctx.Recipe.Validation.Deployment != nil {
			checkList = r.ctx.Recipe.Validation.Deployment.Checks
		}
	case "performance":
		if r.ctx.Recipe.Validation.Performance != nil {
			checkList = r.ctx.Recipe.Validation.Performance.Checks
		}
	case "conformance":
		if r.ctx.Recipe.Validation.Conformance != nil {
			checkList = r.ctx.Recipe.Validation.Conformance.Checks
		}
	}

	for _, name := range checkList {
		if name == checkName {
			return true
		}
	}

	return false
}

// LoadValidationContext loads the validation context from the Job environment.
// This function is called inside Kubernetes Jobs to reconstruct the context needed for validation.
//
// Context loading process:
//  1. Creates in-cluster Kubernetes client using rest.InClusterConfig()
//  2. Loads snapshot from mounted file (auto-detects YAML/JSON format)
//  3. Parses optional recipe metadata from environment variable
//  4. Returns fully initialized ValidationContext
//
// Environment variables used:
//   - EIDOS_SNAPSHOT_PATH: Path to snapshot file (default: /data/snapshot/snapshot.yaml)
//   - EIDOS_RECIPE_DATA: Optional JSON-encoded recipe metadata
//
// Mounted volumes expected:
//   - /data/snapshot/snapshot.yaml: Snapshot ConfigMap
//   - /data/recipe/recipe.yaml: Recipe ConfigMap (not currently used)
//
// Returns error if:
//   - In-cluster config cannot be created (not running in Kubernetes)
//   - Kubernetes client creation fails
//   - Snapshot file cannot be read or parsed
//
// IMPORTANT: The caller is responsible for calling the returned cancel function
// when the validation context is no longer needed.
func LoadValidationContext() (*ValidationContext, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)

	// Create in-cluster Kubernetes client
	config, err := rest.InClusterConfig()
	if err != nil {
		cancel()
		return nil, nil, errors.Wrap(errors.ErrCodeInternal, "failed to create in-cluster config", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		cancel()
		return nil, nil, errors.Wrap(errors.ErrCodeInternal, "failed to create kubernetes clientset", err)
	}

	// Get the validation namespace
	namespace, err := getNamespaceFromServiceAccount()
	if err != nil {
		cancel()
		return nil, nil, errors.Wrap(errors.ErrCodeInternal, "failed to get namespace from service account", err)
	}

	// Load snapshot from mounted file using serializer (auto-detects YAML/JSON format)
	snapshotPath := os.Getenv("EIDOS_SNAPSHOT_PATH")
	if snapshotPath == "" {
		snapshotPath = "/data/snapshot/snapshot.yaml"
	}

	snapshot, err := serializer.FromFile[snapshotter.Snapshot](snapshotPath)
	if err != nil {
		cancel()
		return nil, nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to load snapshot from %s", snapshotPath), err)
	}

	// Load optional recipe data
	var recipeData map[string]interface{}
	if recipeJSON := os.Getenv("EIDOS_RECIPE_DATA"); recipeJSON != "" {
		if err := json.Unmarshal([]byte(recipeJSON), &recipeData); err != nil {
			cancel()
			return nil, nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to unmarshal recipe data JSON", err)
		}
	}

	// Load recipe from mounted file (contains validation constraints)
	recipePath := os.Getenv("EIDOS_RECIPE_PATH")
	if recipePath == "" {
		recipePath = "/data/recipe/recipe.yaml"
	}

	var recipeResult *recipe.RecipeResult
	if _, err := os.Stat(recipePath); err == nil {
		recipeResult, err = serializer.FromFile[recipe.RecipeResult](recipePath)
		if err != nil {
			cancel()
			return nil, nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to load recipe from %s", recipePath), err)
		}
	}

	return &ValidationContext{
		Context:    ctx,
		Namespace:  namespace,
		Snapshot:   snapshot,
		Clientset:  clientset,
		RESTConfig: config,
		RecipeData: recipeData,
		Recipe:     recipeResult,
	}, cancel, nil
}

// getNamespaceFromServiceAccount gets the namespace from the service account
func getNamespaceFromServiceAccount() (string, error) {
	namespaceBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return "", fmt.Errorf("failed to read namespace from service account: %w", err)
	}
	return strings.TrimSpace(string(namespaceBytes)), nil
}
