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

package defaults

import "time"

// Collector timeouts for data collection operations.
const (
	// CollectorTimeout is the default timeout for collector operations.
	// Collectors should respect parent context deadlines when shorter.
	CollectorTimeout = 10 * time.Second

	// CollectorK8sTimeout is the timeout for Kubernetes API calls in collectors.
	CollectorK8sTimeout = 30 * time.Second
)

// Handler timeouts for HTTP request processing.
const (
	// RecipeHandlerTimeout is the timeout for recipe generation requests.
	RecipeHandlerTimeout = 30 * time.Second

	// RecipeBuildTimeout is the internal timeout for recipe building.
	// Should be less than RecipeHandlerTimeout to allow error handling.
	RecipeBuildTimeout = 25 * time.Second

	// BundleHandlerTimeout is the timeout for bundle generation requests.
	// Longer than recipe due to file I/O operations.
	BundleHandlerTimeout = 60 * time.Second

	// RecipeCacheTTL is the default cache duration for recipe responses.
	RecipeCacheTTL = 10 * time.Minute
)

// Server timeouts for HTTP server configuration.
const (
	// ServerReadTimeout is the maximum duration for reading request headers.
	ServerReadTimeout = 10 * time.Second

	// ServerReadHeaderTimeout prevents slow header attacks.
	ServerReadHeaderTimeout = 5 * time.Second

	// ServerWriteTimeout is the maximum duration for writing a response.
	ServerWriteTimeout = 30 * time.Second

	// ServerIdleTimeout is the maximum duration to wait for the next request.
	ServerIdleTimeout = 120 * time.Second

	// ServerShutdownTimeout is the maximum duration for graceful shutdown.
	ServerShutdownTimeout = 30 * time.Second
)

// Kubernetes timeouts for K8s API operations.
const (
	// K8sJobCreationTimeout is the timeout for creating K8s Job resources.
	K8sJobCreationTimeout = 30 * time.Second

	// K8sPodReadyTimeout is the timeout for waiting for pods to be ready.
	K8sPodReadyTimeout = 60 * time.Second

	// K8sJobCompletionTimeout is the default timeout for job completion.
	K8sJobCompletionTimeout = 5 * time.Minute

	// K8sCleanupTimeout is the timeout for cleanup operations.
	K8sCleanupTimeout = 30 * time.Second
)

// HTTP client timeouts for outbound requests.
const (
	// HTTPClientTimeout is the default total timeout for HTTP requests.
	HTTPClientTimeout = 30 * time.Second

	// HTTPConnectTimeout is the timeout for establishing connections.
	HTTPConnectTimeout = 5 * time.Second

	// HTTPTLSHandshakeTimeout is the timeout for TLS handshake.
	HTTPTLSHandshakeTimeout = 5 * time.Second

	// HTTPResponseHeaderTimeout is the timeout for reading response headers.
	HTTPResponseHeaderTimeout = 10 * time.Second

	// HTTPIdleConnTimeout is the timeout for idle connections in the pool.
	HTTPIdleConnTimeout = 90 * time.Second

	// HTTPKeepAlive is the keep-alive duration for connections.
	HTTPKeepAlive = 30 * time.Second

	// HTTPExpectContinueTimeout is the timeout for Expect: 100-continue.
	HTTPExpectContinueTimeout = 1 * time.Second
)

// ConfigMap timeouts for Kubernetes ConfigMap operations.
const (
	// ConfigMapWriteTimeout is the timeout for writing to ConfigMaps.
	ConfigMapWriteTimeout = 30 * time.Second
)

// CLI timeouts for command-line operations.
const (
	// CLISnapshotTimeout is the default timeout for snapshot operations.
	CLISnapshotTimeout = 5 * time.Minute
)

// Validation phase timeouts for validation phase operations.
// These are used when the recipe does not specify a timeout.
const (
	// ValidateReadinessTimeout is the default timeout for readiness validation.
	ValidateReadinessTimeout = 5 * time.Minute

	// ValidateDeploymentTimeout is the default timeout for deployment validation.
	ValidateDeploymentTimeout = 10 * time.Minute

	// ValidatePerformanceTimeout is the default timeout for performance validation.
	// Performance tests may take longer due to GPU benchmarks.
	ValidatePerformanceTimeout = 30 * time.Minute

	// ValidateConformanceTimeout is the default timeout for conformance validation.
	ValidateConformanceTimeout = 15 * time.Minute

	// ResourceVerificationTimeout is the timeout for verifying individual
	// expected resources exist and are healthy during deployment validation.
	ResourceVerificationTimeout = 10 * time.Second

	// ComponentRenderTimeout is the maximum time to render a single component
	// via helm template or manifest file rendering during resource discovery.
	ComponentRenderTimeout = 60 * time.Second
)

// Conformance test timeouts for DRA and gang scheduling validation.
const (
	// DRATestPodTimeout is the timeout for the DRA test pod to complete.
	// The pod runs a simple CUDA device check but may need time for image pull.
	DRATestPodTimeout = 5 * time.Minute

	// GangTestPodTimeout is the timeout for gang scheduling test pods to complete.
	// Two pods must be co-scheduled, each pulling a CUDA image and running nvidia-smi.
	GangTestPodTimeout = 5 * time.Minute
)

// HPA behavioral test timeouts for conformance validation.
const (
	// HPAScaleTimeout is the timeout for waiting for HPA to report scaling intent.
	// The HPA needs time to read metrics and compute desired replicas.
	HPAScaleTimeout = 3 * time.Minute

	// HPAPollInterval is the interval for polling HPA status during behavioral tests.
	HPAPollInterval = 10 * time.Second
)

// Karpenter behavioral test timeouts for conformance validation.
const (
	// KarpenterNodeTimeout is the timeout for Karpenter to provision KWOK nodes.
	KarpenterNodeTimeout = 3 * time.Minute

	// KarpenterPollInterval is the interval for polling Karpenter node provisioning.
	KarpenterPollInterval = 10 * time.Second
)

// Deployment and pod scheduling test timeouts for conformance validation.
const (
	// DeploymentScaleTimeout is the timeout for waiting for Deployment controller
	// to observe and act on HPA scale-up by increasing replica count.
	DeploymentScaleTimeout = 2 * time.Minute

	// PodScheduleTimeout is the timeout for waiting for test pods to be scheduled
	// on Karpenter-provisioned nodes after the HPA scales up.
	PodScheduleTimeout = 2 * time.Minute
)

// Pod operation timeouts for validation and agent operations.
const (
	// PodWaitTimeout is the maximum time to wait for pod operations to complete.
	PodWaitTimeout = 10 * time.Minute

	// PodPollInterval is the interval for polling pod status.
	// Used in legacy polling code (to be replaced with watch API in Phase 3).
	PodPollInterval = 500 * time.Millisecond

	// ValidationPodTimeout is the timeout for validation pod operations.
	ValidationPodTimeout = 10 * time.Minute

	// DiagnosticTimeout is the timeout for collecting diagnostic information.
	DiagnosticTimeout = 2 * time.Minute

	// PodReadyTimeout is the timeout for waiting for pods to become ready.
	PodReadyTimeout = 2 * time.Minute
)

// Job configuration constants.
const (
	// JobTTLAfterFinished is the time-to-live for completed Jobs.
	// Jobs are kept for debugging purposes before automatic cleanup.
	JobTTLAfterFinished = 1 * time.Hour
)

// Server size limits.
const (
	// ServerMaxHeaderBytes is the maximum size of request headers (64KB).
	// Prevents header-based attacks.
	ServerMaxHeaderBytes = 1 << 16
)
