// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
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

	// K8sPodListPageSize is the number of pods per List API page when paginating
	// cluster-wide pod listings (e.g., container image collection). Mirrors
	// TopologyListPageSize to bound memory on large clusters.
	K8sPodListPageSize = int64(500)

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

// Library facade timeouts for the top-level aicr.Client entry points.
// Each Client method context.WithTimeouts against these defaults so a
// caller passing context.Background() can't hang a controller reconcile
// on stuck I/O. Callers passing a tighter context deadline keep theirs
// (context.WithTimeout honors the smaller of the two).
const (
	// RecipeOperationTimeout is the upper bound for a single
	// Client.ResolveRecipe or Client.BundleComponents call when the
	// caller's context has no deadline. Sized for embedded + on-disk
	// recipe reads with cache misses; not appropriate for OCI fetches
	// (those will need a separate network-bound timeout once OCI sources
	// are implemented).
	RecipeOperationTimeout = 30 * time.Second

	// SnapshotOperationTimeout is the facade-level upper bound for
	// Client.CollectSnapshot when neither the caller's context nor
	// AgentConfig.Timeout supplies one. Matches CLISnapshotTimeout so
	// library and CLI consumers see the same ceiling. Callers driving
	// long-running custom collectors should pass an explicit
	// AgentConfig.Timeout — that wins so long as it's smaller than any
	// deadline already on the parent context.
	SnapshotOperationTimeout = 5 * time.Minute

	// ValidationOperationTimeout is the facade-level upper bound for
	// Client.ValidateState when the caller's context has no deadline
	// (controller/library callers; the CLI runs uncapped). It must exceed the
	// LARGEST per-check Job timeout so that inner timeout fires first and the
	// run surfaces a structured per-check error rather than the wrapping
	// context's bare deadline-exceeded. The largest is the inference-perf
	// catalog timeout (65m, which covers the model-cache populate + cold-start
	// benchmark phases), not CheckExecutionTimeout (55m, the fallback when no
	// catalog timeout is set). 75m keeps margin above 65m for orchestration
	// overhead (snapshot agent, RBAC, namespace setup, cleanup). The
	// catalog-vs-facade relationship is asserted in
	// pkg/validator/catalog/catalog_test.go.
	ValidationOperationTimeout = 75 * time.Minute
)

// Server timeouts for HTTP server configuration.
const (
	// ServerReadTimeout is the maximum duration for reading request headers.
	ServerReadTimeout = 10 * time.Second

	// ServerReadHeaderTimeout prevents slow header attacks.
	ServerReadHeaderTimeout = 5 * time.Second

	// ServerWriteTimeout is the maximum duration for writing a response.
	// Must be ≥ ServerHandlerTimeout (and therefore ≥ the longest
	// per-handler timeout, currently BundleHandlerTimeout = 60s) so a
	// handler's deadline can actually run to completion before the
	// net/http server force-closes the connection.
	ServerWriteTimeout = 90 * time.Second

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

	// PodAffinitySelectorLookupTimeout bounds the per-namespace List call
	// the deployer makes to verify a dependencyAffinity selector matches at
	// least one running pod. Short because the lookup is a best-effort
	// diagnostic — a slow apiserver shouldn't delay Job deploy, and the
	// scheduler itself will eventually surface any mismatch as Pending.
	PodAffinitySelectorLookupTimeout = 5 * time.Second

	// GPUNodeDetectionTimeout bounds the pre-deployment node List call that
	// checks for nvidia.com/gpu.present=true nodes. A single paginated List
	// with limit=1 against a local apiserver is fast; 5s matches
	// PodAffinitySelectorLookupTimeout (same best-effort preflight category).
	GPUNodeDetectionTimeout = 5 * time.Second
)

// Local filesystem timeouts.
const (
	// FileReadTimeout bounds blocking reads against the local filesystem,
	// including paths that may resolve through symlinks, FUSE mounts, or
	// network filesystems (NFS, SMB). Generous because legitimate local
	// reads are sub-second; the timeout exists to protect against pathological
	// mounts and attacker-influenced paths.
	FileReadTimeout = 30 * time.Second
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

// Trust / TUF timeouts for Sigstore trust-root refresh.
const (
	// TUFUpdateTimeout bounds the total time for Sigstore TUF metadata refresh.
	// TUF downloads several metadata files (root, timestamp, snapshot, targets)
	// from a CDN; allow more headroom than a single HTTP request.
	TUFUpdateTimeout = 2 * time.Minute
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

	// OIDCAuthTimeout is the maximum time to wait for a user to complete
	// any interactive OIDC authentication flow — browser callback or
	// device-code (RFC 8628). Prevents indefinite blocking if the flow is
	// started but never completed. Same budget for both flows today; split
	// per-flow if a future tuning need (e.g., longer device-code window
	// for typing the user code) makes the shared ceiling cramped.
	OIDCAuthTimeout = 5 * time.Minute

	// SigstoreSignTimeout bounds the non-interactive Sigstore signing flow:
	// Fulcio certificate issuance (token-exchange + cert mint) plus Rekor
	// transparency-log submission. Two HTTP round-trips against public-good
	// infrastructure; 2 minutes leaves comfortable headroom over typical
	// p99 latency without letting a hung peer block a CLI invocation
	// indefinitely. Distinct from OIDCAuthTimeout, which covers the
	// interactive user-driven step that precedes this flow.
	SigstoreSignTimeout = 2 * time.Minute
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

	// EvidenceRenderTimeout is the timeout for rendering conformance evidence.
	EvidenceRenderTimeout = 30 * time.Second

	// EvidenceBundleBuildTimeout bounds local bundle assembly. Local I/O
	// only; 60s is headroom over the typical few-second pipeline.
	EvidenceBundleBuildTimeout = 60 * time.Second

	// EvidenceBundleSignTimeout bounds Sigstore signing. Aliased to
	// SigstoreSignTimeout but kept distinct so the validate-time pipeline
	// can adjust independently of the bundle-attest path.
	EvidenceBundleSignTimeout = SigstoreSignTimeout

	// EvidenceBundlePushTimeout: multi-blob ORAS upload; 2 minutes covers
	// typical p99 against ghcr / Quay.
	EvidenceBundlePushTimeout = 2 * time.Minute
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
	// inside a K8s Job. Must be long enough for the slowest behavioral check
	// and shorter than the catalog-level Job timeout (activeDeadlineSeconds).
	//
	// The ceiling is set by the cold-start inference benchmark, which runs
	// the following phases serially under the parent ctx:
	//   InferenceNamespaceTerminationWait ( 5m, prior run's namespace drain)
	// + InferenceWorkloadReadyTimeout     (10m, model-cache populate download —
	//                                           the cache is on by default)
	// + InferenceWorkloadReadyTimeout     (10m, image pull + worker model load)
	// + InferenceHealthTimeout            ( 5m, endpoint readiness probe)
	// + InferencePerfPodTimeout           ( 5m, AIPerf pod scheduling)
	// + InferencePerfJobTimeout           (15m, AIPerf benchmark runtime)
	// ──────────────────────────────────────
	// = 50m worst-case phase sum; 55m ceiling gives 5m headroom for slow
	//   image registries and slog/K8s API round-trips between phases.
	// This is the fallback for a standalone validator invocation; normal runs
	// use the larger inference-perf catalog `timeout` (AICR_CHECK_TIMEOUT, 65m),
	// which also accounts for the cache-populate phase. Deferred cleanup
	// (K8sCleanupTimeout, ~30s) runs under a fresh context.Background and does
	// not consume this budget.
	CheckExecutionTimeout = 55 * time.Minute

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

// Inference performance validation timeouts.
const (
	// InferenceHealthTimeout is the maximum time to wait for the inference
	// endpoint to start serving real requests before running the benchmark.
	// Readiness is determined by a real /v1/chat/completions probe — a /health
	// 200 is insufficient because the frontend serves /health before backend
	// workers register.
	InferenceHealthTimeout = 5 * time.Minute

	// InferenceHealthPollInterval is the polling interval for the readiness
	// probe described on InferenceHealthTimeout.
	InferenceHealthPollInterval = 10 * time.Second

	// InferenceEndpointProbeTimeout is the per-request timeout for the readiness
	// probe's chat-completion against the inference endpoint. It must exceed the
	// cold-start first-token latency: a fresh worker captures CUDA graphs / JIT-
	// warms kernels on its first inference, which was measured at ~40s and can
	// reach 60-90s on some GPUs (e.g. RTX PRO 6000). The generic 30s
	// HTTPClientTimeout canceled that legitimate first request, so the probe
	// never saw a success and the phase failed before AIPerf (which has its own
	// warmup) could start. 120s clears observed cold-start with margin while
	// still fitting several polls inside InferenceHealthTimeout.
	InferenceEndpointProbeTimeout = 120 * time.Second

	// InferencePerfJobTimeout is the maximum time for the AIPerf benchmark Job
	// to complete. AIPerf with 100 requests at concurrency 16 typically finishes
	// in a few minutes; this provides headroom for model loading and warmup.
	InferencePerfJobTimeout = 15 * time.Minute

	// InferencePerfPodTimeout is the maximum time to wait for the AIPerf pod
	// to be created and scheduled.
	InferencePerfPodTimeout = 5 * time.Minute

	// InferenceWorkloadReadyTimeout is the maximum time to wait for the
	// DynamoGraphDeployment to reach the "successful" state. Includes image
	// pull, model loading, and health check readiness for all workers.
	InferenceWorkloadReadyTimeout = 10 * time.Minute

	// InferenceNamespaceTerminationWait is the maximum time to wait for a
	// prior run's benchmark namespace to finish terminating before a new run
	// re-creates it. Dynamo CRs with finalizers can hold the namespace in
	// Terminating state for 2-3 minutes while cascade deletion propagates;
	// waiting avoids the "... forbidden: ... because it is being terminated"
	// race on subsequent resource creates.
	InferenceNamespaceTerminationWait = 5 * time.Minute
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

	// PreflightCleanupTimeout bounds the best-effort probe-pod delete in
	// deferred validator preflight cleanup paths, which run with
	// context.Background() so they still fire after the parent context
	// has been canceled.
	PreflightCleanupTimeout = 30 * time.Second
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

	// InferenceProbeBodyLimit caps the response read by the inference-perf
	// readiness probe (POST /v1/chat/completions before launching AIPerf).
	// A successful probe with max_tokens=4 is well under 1 KiB; the cap is
	// generous enough for any reasonable OpenAI-compatible frontend yet small
	// enough that a runaway/streaming frontend can't blow memory before the
	// probe gives up.
	InferenceProbeBodyLimit = 8 * 1024 // 8 KiB
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

	// MaxBundlePOSTBytes is the maximum size in bytes for a bundle POST
	// request body. Bundle bodies carry a fully resolved RecipeResult which
	// can include component values; 8 MiB provides generous headroom while
	// preventing unbounded memory allocation by malicious or buggy clients.
	MaxBundlePOSTBytes int64 = 8 * 1024 * 1024 // 8 MiB

	// MaxRecipePOSTBytes is the maximum size in bytes for recipe / query POST
	// request bodies. Recipe criteria and query selectors are small structured
	// inputs; 1 MiB is well above any legitimate payload while bounding
	// per-request memory.
	MaxRecipePOSTBytes int64 = 1 * 1024 * 1024 // 1 MiB

	// ServerMaxBodyBytes is the default per-request body cap applied as a
	// fallback when a handler does not configure its own MaxBytesReader.
	// Derived from MaxBundlePOSTBytes (the largest legitimate body) so the
	// fallback cannot drift if the bundle limit is ever retuned.
	ServerMaxBodyBytes = MaxBundlePOSTBytes

	// MaxBOMBytes caps the size of an operator-supplied CycloneDX BOM file
	// (the --bom path on `aicr validate --emit-attestation`). Real BOMs for
	// the typical cluster are a few hundred KiB; 8 MiB covers the largest
	// observed surfaces with headroom while bounding an attacker-influenced
	// path (e.g., /proc symlink, NFS mount) before os.ReadFile would
	// allocate the whole file into memory.
	MaxBOMBytes int64 = 8 * 1024 * 1024 // 8 MiB

	// MaxConfigBytes caps the size of a user-supplied --config file. Real
	// configs are well under 100 KiB; 1 MiB is generous headroom while
	// preventing a hostile symlink (/proc, FUSE, NFS) from forcing the CLI
	// or server to allocate an unbounded buffer.
	MaxConfigBytes int64 = 1 * 1024 * 1024 // 1 MiB

	// MaxChartYAMLBytes caps the size of a Chart.yaml file that
	// pkg/oci.PackageAndPushHelmChart reads from a caller-supplied
	// SourceDir before pushing as an OCI artifact. Real Chart.yaml
	// files are well under 4 KiB (apiVersion + name + version +
	// maybe dependencies); 1 MiB is generous headroom while bounding
	// an attacker-influenced SourceDir (symlink to /proc, NFS mount,
	// FUSE filesystem) before os.ReadFile would OOM the process.
	MaxChartYAMLBytes int64 = 1 * 1024 * 1024 // 1 MiB

	// MaxChecksumFileBytes caps the size of a bundle checksums.txt file.
	// One entry is ~80 bytes; 1 MiB allows ~12k entries — well above any
	// realistic bundle while bounding attacker-influenced inputs at the
	// verifier/checksum read paths.
	MaxChecksumFileBytes int64 = 1 * 1024 * 1024 // 1 MiB

	// MaxAttestationFileBytes caps the size of in-bundle attestation
	// artifacts (binary attestation, intoto statements) that are copied
	// into the output and signed. Real attestations are tens of KiB;
	// 10 MiB matches MaxSigstoreBundleSize for parity across signed
	// supply-chain artifacts.
	MaxAttestationFileBytes int64 = 10 * 1024 * 1024 // 10 MiB

	// MaxManifestFileBytes caps the size of an in-bundle manifest.json
	// file read by the verifier. A manifest entry is ~150 bytes (path +
	// size + sha256); 1 MiB allows ~6k entries — well above any realistic
	// bundle while bounding an attacker-influenced bundle root (extracted
	// from an untrusted archive, symlink-rich tarball) before os.ReadFile
	// would allocate the whole file into memory.
	MaxManifestFileBytes int64 = 1 * 1024 * 1024 // 1 MiB

	// MaxExternalDataFileBytes caps the size of recipe/registry data files
	// read from the external data directory by LayeredDataProvider. This is
	// the single source of truth for the external-data size limit:
	// LayeredProviderConfig.MaxFileSize falls back here when zero, and
	// readExternalFile uses it when its caller passes a non-positive
	// bound. Bounds attacker-controlled file content when a network mount
	// swaps a file between walk-time validation and the read at
	// consumption time.
	MaxExternalDataFileBytes int64 = 10 * 1024 * 1024 // 10 MiB
)

// Server-wide handler defaults.
const (
	// ServerHandlerTimeout is the default per-request handler timeout used
	// by the timeout middleware. Acts as the server-wide upper bound:
	// per-handler context.WithTimeout calls (RecipeHandlerTimeout,
	// BundleHandlerTimeout, ...) must be ≤ this value, otherwise
	// context.WithTimeout's smaller-of-two semantic silently clamps them.
	// Sized for the longest handler (BundleHandlerTimeout = 60s) with
	// headroom for error-path response writing.
	ServerHandlerTimeout = 90 * time.Second

	// ServerRateLimitWindow is the rate-limit window length advertised to
	// clients via X-RateLimit-Reset. Mirrors the limiter's per-second model.
	ServerRateLimitWindow = 1 * time.Second
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

// Server listen address and env-var override names.
const (
	// ServerDefaultPort is the default TCP port the API server binds on.
	// Override via the PORT environment variable. Matches the convention
	// used by Cloud Run, App Engine, Heroku, and the project's published
	// K8s deployment manifests, so renaming would be a breaking surface
	// change for documented operators.
	ServerDefaultPort = 8080

	// EnvServerPort is the environment variable that overrides
	// ServerDefaultPort.
	EnvServerPort = "PORT"

	// EnvServerShutdownTimeoutSeconds is the environment variable that
	// overrides ServerShutdownTimeout (value parsed as seconds).
	EnvServerShutdownTimeoutSeconds = "SHUTDOWN_TIMEOUT_SECONDS"
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

	// EvidenceSectionTimeout is the per-section timeout for the bash
	// subprocess that collects behavioral evidence for a single feature.
	// A single section may deploy a workload, wait for readiness, and run
	// kubectl probes; 5 minutes provides headroom while still bounding
	// runaway shell processes.
	EvidenceSectionTimeout = 5 * time.Minute

	// EvidenceMaxOutputBytes caps captured stdout/stderr per evidence
	// section to prevent unbounded memory growth from chatty kubectl
	// commands or runaway loops in collection scripts.
	EvidenceMaxOutputBytes = 10 * 1024 * 1024 // 10 MiB
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

// Mirror discovery timeouts and defaults.
const (
	// MirrorHelmTemplateTimeout is the per-component timeout for helm
	// template rendering during mirror list discovery. Matches the
	// defaultHelmTimeout used by tools/bom (90s).
	MirrorHelmTemplateTimeout = 90 * time.Second

	// MirrorDefaultKubeVersion is the Kubernetes version passed to
	// `helm template --kube-version` when no version can be inferred from
	// recipe constraints. Without this flag Helm uses its compiled-in
	// default (currently v1.27.0 in Helm 3.x), which is too old for
	// charts that declare a kubeVersion constraint (e.g., >=1.32.0-0).
	// The value tracks the project's minimum supported Kubernetes version
	// declared in recipes/overlays/base.yaml.
	MirrorDefaultKubeVersion = "1.33.0"

	// MirrorDiscoveryConcurrency caps the number of components rendered in
	// parallel during mirror discovery. Each render forks a `helm template`
	// subprocess and reads YAML output; unbounded fan-out across recipes
	// with many components can saturate CPU and exhaust file descriptors
	// in CI runners. The bound trades wall-clock for predictable resource
	// use; 8 is a balance that keeps a typical 30-component recipe under
	// a minute on a 4-vCPU runner.
	MirrorDiscoveryConcurrency = 8
)

// MirrorExtraAPIVersions lists API group/versions passed to
// `helm template --api-versions` so that offline rendering succeeds for
// charts that gate templates on `.Capabilities.APIVersions`.
//
// Helm's offline `template` command has no cluster to query, so
// `.Capabilities.APIVersions` is empty by default. Charts that validate
// the presence of specific APIs (e.g., nvidia-dra-driver-gpu checking
// for resource.k8s.io) fail at template time unless we declare them.
//
// This list covers APIs checked by charts in recipes/registry.yaml.
// Update it when a new chart adds an APIVersion gate.
var MirrorExtraAPIVersions = []string{
	// Dynamic Resource Allocation (DRA) — checked by nvidia-dra-driver-gpu.
	// v1beta1 shipped in K8s 1.32, v1 went GA in K8s 1.34.
	"resource.k8s.io/v1",
	"resource.k8s.io/v1beta1",
}

// Shared Helm template rendering timeout. Used as the default deadline
// fallback in pkg/helm.RenderChart when the caller's context carries no
// deadline (or a deadline that exceeds this cap). Callers that need a
// tighter or looser budget (e.g., MirrorHelmTemplateTimeout) should set
// their own context.WithTimeout before calling RenderChart; this constant
// serves as a safety net so the subprocess is never unbounded.
const HelmTemplateTimeout = 90 * time.Second

// HelmTemplateOutputLimit caps the bytes written to the stdout buffer of
// a helm-template subprocess. --recipe accepts user-provided chart sources
// with no allowlist, so the subprocess is not a trusted source. The 90s
// context deadline bounds time but not memory; this limit bounds memory.
// 100 MiB is generous — real charts are single-digit MB — while still
// preventing a malicious or buggy chart from exhausting memory.
const HelmTemplateOutputLimit int64 = 100 * 1024 * 1024 // 100 MiB

// Helm chart-pull timeouts for the bundle-time --vendor-charts path.
// Sized for one chart pull from a remote Helm or OCI registry, including
// repo index fetch (HTTPS) or registry resolution (OCI), tarball download,
// and SHA256 hashing. Applies to whichever puller implementation backs
// localformat.ChartPuller — today the CLI shim, later the in-process
// Helm SDK if/when licensing clears.
const (
	// HelmChartPullTimeout bounds a single upstream chart fetch. Charts are
	// typically 0.5–5 MB, but slow or geographically distant registries plus
	// repo-index downloads on cold starts can extend the wall time well
	// beyond the default HTTPClientTimeout.
	HelmChartPullTimeout = 5 * time.Minute
)

// OCI registry push tuning. Bounds individual oras.Copy attempts and the
// retry policy applied around them. Push attempts can take minutes for
// large bundles over slow links, so the per-attempt timeout is generous.
const (
	// RegistryPushTimeout is the per-attempt timeout for a single oras.Copy
	// invocation against a remote registry. Each retry receives a fresh
	// budget of this size.
	RegistryPushTimeout = 10 * time.Minute

	// RegistryPushRetries is the maximum number of oras.Copy attempts
	// (initial attempt plus retries) for transient registry failures.
	RegistryPushRetries = 3

	// RegistryPushBackoff is the initial backoff between retry attempts.
	// The backoff is doubled per attempt and jittered by +/-25%.
	RegistryPushBackoff = 1 * time.Second

	// OCIPushConcurrency is the maximum number of concurrent blob copy
	// tasks within a single oras.Copy invocation.
	OCIPushConcurrency = 3
)
