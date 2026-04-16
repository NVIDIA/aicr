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

package defaults

import "time"

// Collector timeouts for data collection operations.
const (
	// CollectorTimeout is the default timeout for collector operations.
	// Collectors should respect parent context deadlines when shorter.
	CollectorTimeout = 10 * time.Second

	// CollectorK8sTimeout is the timeout for Kubernetes API calls in collectors.
	// Covers 6 sequential sub-collectors (server, image, policy, node, helm, argocd).
	CollectorK8sTimeout = 60 * time.Second

	// NFDDetectionTimeout is the timeout for NFD-based hardware detection.
	// PCI enumeration and kernel module listing are fast local operations
	// reading from sysfs/procfs, so a short timeout is sufficient.
	NFDDetectionTimeout = 5 * time.Second
)

// Node topology collector constants.
const (
	// CollectorTopologyTimeout is the timeout for node topology collection.
	// Longer than standard K8s collector because of paginated node listing.
	CollectorTopologyTimeout = 90 * time.Second

	// TopologyListPageSize is the number of nodes per List API page.
	TopologyListPageSize = int64(500)
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
	// Needs headroom for image pull + scheduling in large clusters.
	K8sPodReadyTimeout = 2 * time.Minute

	// K8sJobCompletionTimeout is the default timeout for job completion.
	K8sJobCompletionTimeout = 5 * time.Minute

	// K8sCleanupTimeout is the timeout for cleanup operations.
	K8sCleanupTimeout = 30 * time.Second

	// K8sPodTerminationWaitTimeout is the maximum time to wait for a Job pod
	// to fully terminate after the Job is deleted. Prevents race conditions
	// where RBAC resources are cleaned up while the pod is still running
	// cleanup operations (e.g., chainsaw namespace deletion).
	// Must exceed the default Kubernetes terminationGracePeriodSeconds (30s).
	K8sPodTerminationWaitTimeout = 60 * time.Second
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

	// InteractiveOIDCTimeout is the maximum time to wait for a user to complete
	// browser-based OIDC authentication. Prevents indefinite blocking if the
	// browser flow is started but never completed.
	InteractiveOIDCTimeout = 5 * time.Minute
)

// Validation phase timeouts for validation phase operations.
// Validation phase timeouts.
const (
	// ResourceVerificationTimeout is the timeout for verifying individual
	// expected resources exist and are healthy during deployment validation.
	ResourceVerificationTimeout = 10 * time.Second

	// ComponentRenderTimeout is the maximum time to render a single component
	// via helm template or manifest file rendering during resource discovery.
	ComponentRenderTimeout = 60 * time.Second
)

// Chainsaw assertion configuration for component health checks.
const (
	// ChainsawAssertTimeout is the outer timeout for the chainsaw binary process.
	// Must be greater than the chainsaw-internal assert timeout (spec.timeouts.assert
	// in health check YAML files, currently 5m) to avoid killing the process early.
	ChainsawAssertTimeout = 6 * time.Minute

	// ChainsawMaxParallel is the maximum number of concurrent assertion
	// runs during component health checks.
	ChainsawMaxParallel = 4

	// AssertRetryInterval is the polling interval between health check
	// assertion retries. Assertions are retried at this interval until
	// they pass or the ChainsawAssertTimeout expires.
	AssertRetryInterval = 5 * time.Second
)

// Conformance test timeouts for DRA and gang scheduling validation.
const (
	// CheckExecutionTimeout is the parent context timeout for checks running
	// inside a K8s Job. Must be long enough for behavioral checks (DRA pod
	// creation + image pull + GPU allocation + isolation verification) and
	// shorter than the Job-level ValidateConformanceTimeout.
	CheckExecutionTimeout = 10 * time.Minute

	// DRATestPodTimeout is the timeout for the DRA test pod to complete.
	// The pod runs a simple CUDA device check but may need time for image pull.
	DRATestPodTimeout = 5 * time.Minute

	// GangTestPodTimeout is the timeout for gang scheduling test pods to complete.
	// Two pods must be co-scheduled, each pulling a CUDA image and running nvidia-smi.
	GangTestPodTimeout = 5 * time.Minute
)

// AI service metrics conformance validation.
const (
	// AIServiceMetricsWaitTimeout is the maximum time to wait for GPU metrics
	// to appear in Prometheus. DCGM exporter may not have scraped yet when
	// the validator runs, especially on fresh deployments.
	AIServiceMetricsWaitTimeout = 2 * time.Minute

	// AIServiceMetricsPollInterval is the polling interval between Prometheus
	// queries when waiting for GPU metric time series to appear.
	AIServiceMetricsPollInterval = 10 * time.Second
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

// Gang scheduling co-scheduling validation.
const (
	// CoScheduleWindow is the maximum time span between PodScheduled timestamps
	// for gang-scheduled pods. If pods are scheduled further apart than this,
	// they are not considered co-scheduled.
	CoScheduleWindow = 30 * time.Second
)

// Kubeflow Trainer install timeouts for NCCL performance validation.
const (
	// TrainerCRDEstablishedTimeout is the time to wait for Kubeflow Trainer CRDs
	// to reach the Established condition after installation.
	TrainerCRDEstablishedTimeout = 2 * time.Minute

	// TrainerControllerReadyTimeout is the time to wait for the Kubeflow Trainer
	// controller-manager Deployment to have at least one ready replica after installation.
	TrainerControllerReadyTimeout = 2 * time.Minute

	// NCCLTrainJobTimeout is the maximum time to wait for the NCCL all-reduce TrainJob to complete.
	NCCLTrainJobTimeout = 30 * time.Minute

	// NCCLLauncherPodTimeout is the maximum time to wait for the NCCL launcher pod to be created.
	NCCLLauncherPodTimeout = 5 * time.Minute

	// NCCLTrainerArchiveDownloadTimeout is the timeout for downloading the Kubeflow Trainer
	// source archive from GitHub. The archive is several MB, so a longer timeout than the
	// standard HTTPClientTimeout is appropriate.
	NCCLTrainerArchiveDownloadTimeout = 5 * time.Minute
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

// HTTP response limits for conformance checks.
const (
	// HTTPResponseBodyLimit is the maximum size in bytes for HTTP response bodies
	// read by conformance checks (e.g., Prometheus metric scrapes). Prevents
	// unbounded reads from in-cluster services.
	HTTPResponseBodyLimit = 1 * 1024 * 1024 // 1 MiB

	// MaxErrorBodySize is the maximum size in bytes for HTTP error response bodies.
	// Bounds io.ReadAll on error paths to prevent unbounded memory allocation.
	MaxErrorBodySize = 4096
)

// Job configuration constants.
const (
	// JobTTLAfterFinished is the time-to-live for completed Jobs.
	// Jobs are kept for debugging purposes before automatic cleanup.
	JobTTLAfterFinished = 1 * time.Hour

	// AgentJobActiveDeadline is the active deadline for K8s agent Jobs.
	// Prevents runaway Jobs from consuming cluster resources indefinitely.
	AgentJobActiveDeadline = 5 * time.Hour
)

// Server size limits.
const (
	// ServerMaxHeaderBytes is the maximum size of request headers (64KB).
	// Prevents header-based attacks.
	ServerMaxHeaderBytes = 1 << 16
)

// Server rate limiting constants.
const (
	// ServerDefaultRateLimit is the default requests per second for the rate limiter.
	ServerDefaultRateLimit = 100

	// ServerDefaultRateLimitBurst is the maximum burst size for the rate limiter.
	ServerDefaultRateLimitBurst = 200

	// ServerRetryAfterSeconds is the Retry-After header value when rate limited.
	ServerRetryAfterSeconds = "1"
)

// Log scanner buffer sizes.
const (
	// LogScannerBufferSize is the maximum line size for reading pod logs.
	// Larger than the default 64KB to handle container runtime line splitting
	// and long go test -json output events.
	LogScannerBufferSize = 1 << 20 // 1MB
)

// Validator constants.
const (
	// ValidatorWaitBuffer is added to the catalog timeout when waiting for Job
	// completion. Accounts for pod scheduling, image pull, and graceful termination.
	ValidatorWaitBuffer = 30 * time.Second

	// ValidatorDefaultTimeout is the default per-validator timeout if not
	// specified in the catalog. Used as fallback only.
	ValidatorDefaultTimeout = 5 * time.Minute

	// ValidatorTerminationGracePeriod is the time between SIGTERM and SIGKILL
	// for validator containers. Validators should trap SIGTERM and write partial
	// results within this window.
	ValidatorTerminationGracePeriod = 30 * time.Second

	// ValidatorMaxStdoutLines is the maximum number of stdout lines captured
	// per validator. Lines beyond this are truncated (keeping the last N lines)
	// to prevent ConfigMap overflow.
	ValidatorMaxStdoutLines = 1000

	// ValidatorMaxStdoutLineLength is the maximum length of a single stdout
	// line. Lines exceeding this are truncated with a suffix indicating the
	// number of dropped characters. Prevents oversized report output from
	// inline JSON payloads (e.g., Prometheus metric scrapes).
	ValidatorMaxStdoutLineLength = 512

	// ValidatorDefaultCPU is the default CPU request/limit for validator containers
	// when not specified in the catalog entry.
	ValidatorDefaultCPU = "1"

	// ValidatorDefaultMemory is the default memory request/limit for validator
	// containers when not specified in the catalog entry.
	ValidatorDefaultMemory = "1Gi"
)

// File parser limits.
const (
	// FileParserMaxSize is the maximum file size in bytes for the file collector parser.
	FileParserMaxSize = 1 << 20 // 1MB
)

// Validator runtime class check timeout.
const (
	// RuntimeClassCheckTimeout is the timeout for verifying RuntimeClass
	// existence in the cluster during agent deployment.
	RuntimeClassCheckTimeout = 5 * time.Second
)

// CNCF conformance submission timeout.
const (
	// CNCFSubmissionTimeout is the timeout for CNCF submission evidence
	// collection. CNCF submission deploys GPU workloads and runs HPA tests.
	CNCFSubmissionTimeout = 20 * time.Minute
)

// Retry poll intervals for validator wait loops.
const (
	// TrainerControllerPollInterval is the retry interval when waiting
	// for the Kubeflow Trainer controller-manager to become ready.
	TrainerControllerPollInterval = 2 * time.Second

	// TrainingRuntimePollInterval is the retry interval when waiting
	// for a TrainingRuntime resource to become visible via the API.
	TrainingRuntimePollInterval = 500 * time.Millisecond
)

// Termination and truncation limits for validator output.
const (
	// TerminationLogMaxSize is the maximum size in bytes of the K8s
	// termination log message written to /dev/termination-log.
	TerminationLogMaxSize = 4096

	// ConfigMapStatusTruncateLen is the maximum length for ConfigMap
	// status data before truncation in autoscaler status collection.
	ConfigMapStatusTruncateLen = 2000

	// AutoscalerMaxEvents is the maximum number of autoscaler events
	// to capture when collecting cluster autoscaler evidence.
	AutoscalerMaxEvents = 10

	// MetricsDisplayLimit is the maximum number of custom metrics
	// resources to display in AI service metrics evidence.
	MetricsDisplayLimit = 20
)

// Well-known Kubernetes resource names shared across validators.
const (
	// GPUOperatorNamespace is the default namespace for the GPU operator.
	GPUOperatorNamespace = "gpu-operator"

	// KubeSystemNamespace is the standard kube-system namespace.
	KubeSystemNamespace = "kube-system"
)

// Attestation file size limits.
const (
	// MaxSigstoreBundleSize is the maximum size in bytes for a .sigstore.json file.
	// Prevents unbounded memory allocation when reading attestation bundles.
	// A typical Sigstore bundle is under 100KB; 10 MiB provides generous headroom.
	MaxSigstoreBundleSize = 10 * 1024 * 1024 // 10 MiB
)
