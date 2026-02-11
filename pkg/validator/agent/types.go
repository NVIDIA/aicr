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

package agent

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// Config holds the configuration for deploying a validation agent Job.
type Config struct {
	// Namespace is where the validation Job will be deployed
	Namespace string

	// JobName is the name of the Kubernetes Job
	JobName string

	// Image is the container image to use (should contain eidos CLI)
	Image string

	// ImagePullSecrets for pulling the image from private registries
	ImagePullSecrets []string

	// ServiceAccountName for the Job pods
	ServiceAccountName string

	// NodeSelector for targeting specific nodes (e.g., GPU nodes for performance tests)
	NodeSelector map[string]string

	// Tolerations for scheduling on tainted nodes
	Tolerations []corev1.Toleration

	// SnapshotConfigMap is the ConfigMap containing the snapshot data
	SnapshotConfigMap string

	// RecipeConfigMap is the ConfigMap containing the recipe data
	RecipeConfigMap string

	// TestPackage is the Go package path containing tests to run
	// Example: "./pkg/validator/checks/readiness"
	TestPackage string

	// TestPattern is the test name pattern to run (passed to -run flag)
	// Example: "TestGpuHardwareDetection"
	TestPattern string

	// ExpectedTests is the number of tests expected to run.
	// If set and actual tests differ, validation fails.
	ExpectedTests int

	// Timeout for the Job to complete
	Timeout time.Duration

	// Cleanup determines whether to remove Job and RBAC on completion
	Cleanup bool

	// Debug enables debug logging
	Debug bool
}

// Deployer manages the deployment and lifecycle of validation agent Jobs.
type Deployer struct {
	clientset kubernetes.Interface
	config    Config
}

// NewDeployer creates a new validation agent Deployer.
func NewDeployer(clientset kubernetes.Interface, config Config) *Deployer {
	return &Deployer{
		clientset: clientset,
		config:    config,
	}
}

// CleanupOptions controls what resources to remove during cleanup.
type CleanupOptions struct {
	// Enabled determines whether to cleanup resources
	Enabled bool
}

// ValidationResult represents the result of running validation checks.
type ValidationResult struct {
	// CheckName is the name of the check that was run
	CheckName string

	// Phase is the validation phase
	Phase string

	// Status is the result status (pass/fail/skip)
	Status string

	// Message provides details about the result
	Message string

	// Duration is how long the check took
	Duration time.Duration

	// Details contains structured data about the result
	Details map[string]interface{}

	// Tests contains individual test results when parsing go test JSON output
	// Each entry represents a single test function that was executed
	Tests []TestResult
}

// TestResult represents the result of a single test function.
type TestResult struct {
	// Name is the test function name (e.g., "TestGpuHardwareDetection")
	Name string

	// Status is the test result (pass/fail/skip)
	Status string

	// Duration is how long the test took
	Duration time.Duration

	// Output contains the test output lines
	Output []string
}
