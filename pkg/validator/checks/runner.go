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
	"testing"
	"time"

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
//	    runner.RunCheck("gpu-hardware-detection")
//	}
type TestRunner struct {
	t   testingT
	ctx *ValidationContext
}

// NewTestRunner creates a test runner by loading ValidationContext from the Job environment.
// Expected environment variables:
//   - EIDOS_SNAPSHOT_PATH: Path to mounted snapshot file (default: /data/snapshot/snapshot.yaml)
//   - EIDOS_RECIPE_DATA: Optional JSON-encoded recipe metadata
func NewTestRunner(t *testing.T) (*TestRunner, error) {
	ctx, err := LoadValidationContext()
	if err != nil {
		return nil, fmt.Errorf("failed to load validation context: %w", err)
	}

	return &TestRunner{
		t:   t,
		ctx: ctx,
	}, nil
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
func LoadValidationContext() (*ValidationContext, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Create in-cluster Kubernetes client
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to create in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes clientset: %w", err)
	}

	// Load snapshot from mounted file using serializer (auto-detects YAML/JSON format)
	snapshotPath := os.Getenv("EIDOS_SNAPSHOT_PATH")
	if snapshotPath == "" {
		snapshotPath = "/data/snapshot/snapshot.yaml"
	}

	snapshot, err := serializer.FromFile[snapshotter.Snapshot](snapshotPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load snapshot from %s: %w", snapshotPath, err)
	}

	// Load optional recipe data
	var recipeData map[string]interface{}
	if recipeJSON := os.Getenv("EIDOS_RECIPE_DATA"); recipeJSON != "" {
		if err := json.Unmarshal([]byte(recipeJSON), &recipeData); err != nil {
			return nil, fmt.Errorf("failed to unmarshal recipe data JSON: %w", err)
		}
	}

	return &ValidationContext{
		Context:    ctx,
		Snapshot:   snapshot,
		Clientset:  clientset,
		RecipeData: recipeData,
	}, nil
}
