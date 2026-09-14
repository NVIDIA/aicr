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

// Network topology collector constants. Network discovery delegates to the
// k8s-launch-kit library, which boots a NIC-configuration daemon and pod-exec
// probes each east-west PF — both bounded above by l8k's own internal
// timeouts. CollectorNetworkTimeout caps the worst case as a defense in depth
// so a hung pod exec can't outlive the snapshot.
const (
	// CollectorNetworkTimeout is the upper bound for the network collector.
	// Longer than the topology timeout because l8k discovery includes
	// DaemonSet rollout + per-node pod-exec probes.
	CollectorNetworkTimeout = 10 * time.Minute

	// MaxClusterConfigBytes caps the size of an --cluster-config YAML file
	// the collector will read into memory. Used as the io.LimitReader bound
	// per the project rule against unbounded os.ReadFile.
	MaxClusterConfigBytes = int64(1 << 20) // 1 MiB

	// MaxAKSGPUPoolsBytes caps the size of an --aks-gpu-pools JSON file
	// (the `az aks nodepool list -o json` dump) read into memory. Same
	// io.LimitReader rule as MaxClusterConfigBytes; a real pool list is
	// a few KiB.
	MaxAKSGPUPoolsBytes = int64(1 << 20) // 1 MiB

	// MaxOKEAddonsBytes caps the size of an --oke-addons JSON file
	// (`oci ce cluster list-addons --cluster-id <cluster-ocid> --all --output json`); the dump is a
	// short per-cluster add-on list, so 1 MiB is generous.
	MaxOKEAddonsBytes = int64(1 << 20) // 1 MiB
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

	// SnapshotLoadTimeout is the upper bound for a single
	// Client.LoadSnapshot call, and bounds the whole load whatever the
	// source: a local file read, an HTTP(S) fetch, or a
	// cm://namespace/name ConfigMap read against the Kubernetes API.
	//
	// Matches RecipeOperationTimeout, which bounds Client.LoadRecipe over
	// the same cm:// resolution path against the Kubernetes API. Named
	// separately because a snapshot load is not a recipe operation and
	// should not silently inherit a change made for recipe resolution.
	//
	// Distinct from SnapshotOperationTimeout below, which bounds
	// CollectSnapshot — deploying an agent Job and waiting for it, an
	// operation orders of magnitude longer than reading a file.
	SnapshotLoadTimeout = 30 * time.Second

	// SnapshotOperationTimeout is the facade-level upper bound for
	// Client.CollectSnapshot when neither the caller's context nor
	// AgentConfig.Timeout supplies one. Matches CLISnapshotTimeout so
	// library and CLI consumers see the same ceiling. Callers driving
	// long-running custom collectors should pass an explicit
	// AgentConfig.Timeout — that wins so long as it's smaller than any
	// deadline already on the parent context.
	SnapshotOperationTimeout = 5 * time.Minute

	// SnapshotOperationGrace is the headroom Client.CollectSnapshot adds on
	// top of AgentConfig.Timeout when bounding the whole operation.
	//
	// AgentConfig.Timeout budgets ONE step — waiting for the agent Job to
	// complete. Deploying RBAC and the Job, projecting an --aks-gpu-pools
	// file, and retrieving the result ConfigMap all sit outside it. Capping
	// the operation at exactly AgentConfig.Timeout would silently shrink the
	// Job-completion budget by however long deployment took, so a Job that
	// legitimately needs its full timeout on a slow cluster would fail. The
	// grace keeps the operation bounded for callers that pass an unbounded
	// context without eating into the budget the caller asked for.
	SnapshotOperationGrace = 1 * time.Minute

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

	// VerifyOperationTimeout is the facade-level upper bound for a single
	// Client.VerifyBundle, Client.VerifyEvidence, Client.VerifyCatalog, or
	// Client.RecipeDigest call.
	//
	// It is an UNCONDITIONAL ceiling, not a fallback for deadline-less
	// callers: those methods always wrap the caller's context, and
	// context.WithTimeout takes the smaller of the two. A caller that
	// deliberately allows 20 minutes for a slow OCI pull is still capped
	// here. The trade is deliberate — an unbounded verify can hang a
	// controller reconcile — but it has one sharp edge worth knowing, called
	// out on Client.VerifyEvidence: a cap breach surfaces as an error, not as
	// the Incomplete verdict a CI gate uses to tell "could not check this"
	// from "checked it and it failed".
	//
	// Bundle and catalog verification are offline (locally cached or embedded
	// Sigstore trusted root), so their own work is sub-second; the budget
	// exists for the two paths that do reach the network — a KMS key URI in
	// BundleVerifyOptions.Key still makes a live GetPublicKey call, and
	// VerifyEvidence pulls an OCI artifact when its input is a pointer or a
	// registry reference.
	//
	// Deliberately NOT applied to Client.PublishEvidence or
	// Client.SignCatalog: keyless signing can block on a human completing a
	// browser or device-code OIDC flow, so a fixed cap there would cut short
	// an interactive run that works today.
	VerifyOperationTimeout = 5 * time.Minute
)

// Health computation timeouts.
const (
	// HealthComputeTimeout is the upper bound for a single health.Compute
	// run across the whole recipe catalog when the caller's context has no
	// deadline. Health resolution is hermetic and in-memory (no network, no
	// cluster), but each of the ~50 leaf combos resolves through the recipe
	// builder, so the ceiling is sized well above the expected sub-second
	// run to absorb a cold metadata-store load. Each per-combo build is
	// independently bounded by RecipeBuildTimeout.
	HealthComputeTimeout = 5 * time.Minute
)

// Tuning status computation timeouts.
const (
	// TuningComputeTimeout is the upper bound for a single tuning.Compute run
	// across the whole recipe catalog when the caller's context has no deadline.
	// Like health computation it is hermetic and in-memory (no network, no
	// cluster); the ceiling absorbs a cold metadata-store load plus a manifest
	// read per leaf.
	TuningComputeTimeout = 5 * time.Minute
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

	// DiscoveryRefreshCooldown rate-limits how often the shared cluster
	// fetcher (pkg/chainsaw) invalidates its cached discovery data after a
	// no-match. The refresh exists so a CRD installed by the component being
	// gated is picked up without a restart; the cooldown exists because an
	// assertion for a kind that genuinely does not exist retries every
	// AssertRetryInterval, and refreshing on each retry would turn one
	// missing CRD into a discovery storm.
	DiscoveryRefreshCooldown = 60 * time.Second

	// K8sClientRequestTimeout bounds a single request issued by the shared
	// read-only cluster fetcher (pkg/chainsaw). It exists because the
	// RESTMapper reaches the apiserver through the context-free
	// DiscoveryInterface: per-call contexts bound every other read, but not
	// those. Kept at or below client-go's own 32s discovery default so
	// setting it explicitly never loosens that backstop.
	K8sClientRequestTimeout = 30 * time.Second

	// K8sPodTerminationWaitTimeout is the maximum time to wait for a Job pod
	// to fully terminate after the Job is deleted. Prevents race conditions
	// where RBAC resources are cleaned up while the pod is still running
	// cleanup operations (e.g., chainsaw namespace deletion).
	// Must exceed the default Kubernetes terminationGracePeriodSeconds (30s).
	K8sPodTerminationWaitTimeout = 60 * time.Second

	// K8sJobWatchResumeBackoff paces each watch re-establishment after a Job
	// watch channel closes without the Job being terminal. Routine closures
	// (apiserver --min-request-timeout expiry) are isolated, but a flapping
	// load balancer or an apiserver rollout can close the stream immediately
	// on every reconnect; without pacing the resume loop would re-fire
	// Get+Watch back-to-back and hammer an already-degraded API server. A
	// small fixed delay bounds that call rate while staying negligible against
	// the multi-hour validator budgets the resume logic protects.
	K8sJobWatchResumeBackoff = 2 * time.Second

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
//
// Read and write budgets are kept as separate named constants — even at the
// same value today — so a future tuning of one path (e.g., raising the write
// budget to absorb rate-limiter backoff after heavy API usage) does not
// silently change the other.
const (
	// ConfigMapReadTimeout bounds a single ConfigMap GET. Applied when the
	// serializer resolves a cm:// URI so a hung apiserver cannot stall the
	// caller indefinitely.
	ConfigMapReadTimeout = 30 * time.Second

	// ConfigMapWriteTimeout bounds a single ConfigMap create/update. Sized
	// with headroom for client-side rate-limiter waits after bursty API
	// usage (e.g., during snapshot capture).
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
	// infrastructure; 2 minutes leaves comfortable headroom for
	// SigstoreRetryBudget attempts plus exponential backoff without letting
	// a hung peer block a CLI invocation indefinitely. Distinct from
	// OIDCAuthTimeout, which covers the interactive user-driven step that
	// precedes this flow.
	SigstoreSignTimeout = 2 * time.Minute

	// SigstoreAttemptTimeout bounds a single sign.Bundle invocation. The
	// AICR-side wrapper in pkg/bundler/attestation/signing.go retries up
	// to SigstoreRetryBudget attempts, each bounded by this per-attempt
	// timeout. The outer SigstoreSignTimeout ceiling caps the total
	// wall-clock so a chain of slow Rekor responses can't blow past the
	// structured deadline contract.
	SigstoreAttemptTimeout = 35 * time.Second

	// SigstoreRetryBudget is the maximum number of sign.Bundle attempts
	// the wrapper makes before returning the last error. Retries fire
	// only when the error classifies as ErrCodeUnavailable (transient
	// Fulcio/Rekor failure) — never on ErrCodeTimeout (caller deadline,
	// not worth burning more budget) or other structured failure modes.
	// Three attempts absorbs the typical Sigstore Rekor flake window
	// observed in #1244 and #1245 without inflating wall-clock for the
	// healthy path. See issue #1249.
	SigstoreRetryBudget = 3

	// SigstoreRetryInitialBackoff is the wait between attempt 1 and
	// attempt 2. Subsequent backoffs scale by SigstoreRetryBackoffFactor.
	SigstoreRetryInitialBackoff = 1 * time.Second

	// SigstoreRetryBackoffFactor scales the wait between successive
	// retries: backoff for attempt N is
	// SigstoreRetryInitialBackoff * SigstoreRetryBackoffFactor^(N-1).
	// With initial=1s and factor=5: backoffs are 1s, 5s (3-attempt
	// budget → 2 backoffs).
	SigstoreRetryBackoffFactor = 5
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

	// EvidenceIngestTimeout is the overall deadline for the evidence-project
	// ingest CLI: materialize (ORAS pull) + verify + synthesize for a single
	// bundle. Comfortably above the sum of the per-operation pull/verify
	// budgets so a hung pull is cut off well before the workflow's own
	// 20-minute job timeout.
	EvidenceIngestTimeout = 15 * time.Minute
)

// GPU deployment-readiness poll configuration. The deployment-phase Go checks
// verifyNodewrightReady (Skyhook status.status == "complete") and
// verifyDRAKubeletPluginReady (DRA kubelet-plugin DaemonSet fully rolled out)
// poll their signal until it is healthy *continuously* for the stability
// window, or the timeout elapses.
//
// Rationale: Skyhook node tuning reboots the GPU node one or more times (the
// tuning packages carry interrupt: reboot) and re-opens status=in_progress
// after each reboot and for each newly-joined GPU node. While a GPU node is
// draining/rebooting/rejoining, the DRA kubelet-plugin DaemonSet also churns:
// DesiredNumberScheduled drops to 0 (no schedulable GPU node) and NumberReady
// lags on rejoin. Both signals are therefore non-monotonic during rollout, so a
// single Get sample can land in a transient unhealthy window (e.g. mid-reboot)
// and fail the whole deployment phase even though the node converges moments
// later. The former one-shot Gets did exactly that; polling with a dwell rides
// through the reboot the way the components' chainsaw health-check siblings
// (retry-until-timeout, 5m) already do, hardened with a continuous-pass window
// so a check cannot pass on a momentary lull between reboots.
const (
	// GPUReadinessPollInterval is the sleep between readiness-signal samples.
	GPUReadinessPollInterval = 10 * time.Second

	// GPUReadinessStabilityWindow is how long a signal must report healthy
	// continuously before the check passes, absorbing the flaps a reboot
	// introduces.
	GPUReadinessStabilityWindow = 60 * time.Second

	// GPUReadinessTimeout bounds a single check's poll loop. Sized to ride
	// through a single tuning reboot (GPU node down + rejoin + kubelet ready,
	// ~5m) plus the stability window, with margin — while staying well under
	// CheckExecutionTimeout so the subsequent chainsaw asserts still fit the
	// phase budget. Convergence spanning multiple reboots is absorbed by the
	// UAT readiness gate's outer retry loop, which re-runs the phase.
	GPUReadinessTimeout = 8 * time.Minute
)

// Chainsaw assertion configuration for component health checks.
const (
	// ChainsawAssertTimeout is the fallback per-Test budget for the
	// in-process chainsaw runner when a health check YAML omits
	// spec.timeouts.assert. The runner caps each Test under a single
	// context.WithTimeout(ctx, effectiveTimeout) where effectiveTimeout
	// is the YAML's spec.timeouts.assert if set, otherwise this value.
	// Every in-tree check currently sets timeouts.assert (5m), so this
	// 6m default only kicks in for Tests that don't declare one.
	// Replaced the prior "outer timeout for the chainsaw binary
	// process" role; #1236 removed the binary entirely.
	ChainsawAssertTimeout = 6 * time.Minute

	// ChainsawMaxParallel is the maximum number of concurrent assertion
	// runs during component health checks.
	ChainsawMaxParallel = 4

	// AssertRetryInterval is the polling interval between health check
	// assertion retries. Assertions are retried at this interval until
	// they pass or the ChainsawAssertTimeout expires.
	AssertRetryInterval = 5 * time.Second

	// AbsentResourceGracePeriod bounds how long a health-check assertion
	// retries a resource that does not exist at all (the fetch returns
	// ErrCodeNotFound). A resource that is missing entirely — wrong
	// namespace, never installed — will not appear by waiting out the full
	// ChainsawAssertTimeout, and retrying it for minutes holds one of the
	// ChainsawMaxParallel worker slots and starves healthy components behind
	// it (and can push the check past the Job's activeDeadlineSeconds, which
	// surfaces as an opaque "other" status instead of a clean failure).
	//
	// This grace bounds ONLY the entirely-absent (NotFound) case. A resource
	// that EXISTS but is not yet ready returns a shape-mismatch error
	// (ErrCodeInternal), and a transient API failure returns
	// ErrCodeUnavailable — both keep the full ChainsawAssertTimeout so slow
	// but healthy rollouts are not failed prematurely. The grace allows brief
	// creation lag (a resource that appears within the window switches to the
	// full readiness budget) while failing permanently-absent resources fast.
	AbsentResourceGracePeriod = 30 * time.Second

	// JobEnvelopeMargin is the headroom added on top of ChainsawAssertTimeout
	// when computing the validator Job's outer activeDeadlineSeconds and the
	// expected-resources catalog timeout. Chainsaw needs time after the inner
	// assert deadline elapses to terminate the binary process, clean up the
	// temp test directory, and flush log output before the Job's SIGKILL
	// arrives. Without this headroom the binary is killed mid-cleanup and
	// operators see truncated output, masking the actual failure cause.
	//
	// 60s = 30s for the default Pod terminationGracePeriodSeconds (the
	// SIGTERM→SIGKILL window — see K8sPodTerminationWaitTimeout above)
	// plus 30s headroom for pre-chainsaw helper.VerifyResource iteration
	// and chainsaw startup variance. Tune upward if chainsaw output
	// truncation is observed in CI runs.
	JobEnvelopeMargin = 60 * time.Second
)

// Readiness gate (deploy-time) configuration drives the `gate` CLI Job the
// bundler emits for components that ship a readiness.yaml (see #904). The gate
// re-runs the component's chainsaw Test in a poll loop until it passes
// continuously for the stability window, or the max-wait deadline elapses.
const (
	// ReadinessGateExecTimeout bounds a single chainsaw exec inside the gate's
	// poll loop. It only needs to cover one assert pass (the test's own
	// spec.timeouts.assert), not the whole convergence — the gate owns the
	// outer retry loop via ReadinessGateMaxWait.
	ReadinessGateExecTimeout = 2 * time.Minute

	// ReadinessGatePollInterval is the sleep between gate evaluations.
	ReadinessGatePollInterval = 10 * time.Second

	// ReadinessGateStabilityWindow is the continuous-pass duration the gate
	// requires before declaring readiness, absorbing transient flaps in a
	// CRD's status during rollout.
	ReadinessGateStabilityWindow = 30 * time.Second

	// ReadinessGateMaxWait is the gate's deadline — the single knob that owns
	// how long a deploy blocks on component readiness. Sized for the slowest
	// component gated today (gpu-operator, whose operand rollout — driver,
	// toolkit, and device-plugin across every GPU node — can exceed an hour
	// on a large cluster). The gate exits non-zero if this elapses before
	// readiness.
	ReadinessGateMaxWait = 90 * time.Minute

	// ReadinessGateHelmTimeoutBuffer is added to ReadinessGateMaxWait to derive
	// the helm --timeout for the gate's install. Helm cannot wait
	// indefinitely — --wait/--wait-for-jobs is bounded by --timeout (default
	// 5m; --timeout 0 is not infinite) — so the bundler sets
	// helm --timeout = ReadinessGateMaxWait + this buffer. Large enough that
	// helm never preempts the gate, small enough to still surface a genuinely
	// hung gate process shortly after its own deadline.
	ReadinessGateHelmTimeoutBuffer = 5 * time.Minute

	// ReadinessGateBackoffLimit is the Kubernetes Job backoffLimit for the gate
	// Job. The gate CLI handles its own retry loop internally; this limit
	// absorbs transient pod disruption (drain, evict, OOM) by allowing the Job
	// controller to create a fresh pod without failing the deploy outright.
	ReadinessGateBackoffLimit = 6
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
	// + ModelCachePopulateTimeout         (13m, model-cache populate: cold image
	//                                           pull + download — on by default)
	// + InferenceWorkloadReadyTimeout     (10m, image pull + worker model load)
	// + InferenceHealthTimeout            ( 5m, endpoint readiness probe)
	// + InferencePerfPodTimeout           ( 5m, AIPerf pod scheduling)
	// + InferencePerfJobTimeout           (15m, AIPerf benchmark runtime)
	// ──────────────────────────────────────
	// = 53m worst-case phase sum; 55m ceiling gives 2m headroom for slow
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

	// SlurmAccountingRecordTimeout bounds the eventual-consistency window
	// between a completed Slurm job and its appearance in sacct via SlurmDBD.
	SlurmAccountingRecordTimeout = 2 * time.Minute

	// SlurmAccountingRecordPollInterval is the delay between sacct queries
	// while waiting for the completed job record to reach the accounting store.
	SlurmAccountingRecordPollInterval = 5 * time.Second
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

	// MetricsAPIWarmupTimeout bounds the retry while the aggregated metrics APIs
	// (custom/external.metrics.k8s.io) warm up: prometheus-adapter registers its
	// APIServices before its first Prometheus relist populates metric data, so a
	// single-shot GET right after the deployment phase can race that window. Kept
	// short so the pod-autoscaling metric-API steps plus the HPA behavioral test
	// stay within the validator's catalog timeout.
	MetricsAPIWarmupTimeout = 60 * time.Second
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
	// controller-manager Deployment to have at least one ready replica after
	// installation. Widened from 2m to 3m: on cold start the cert-controller
	// sidecar's webhook-cert get-or-create can race a not-yet-synced informer
	// cache, producing a resourceVersion conflict that the sidecar's own
	// reconcile loop retries and self-heals from unassisted. This is expected
	// behavior under cert-controller's optimistic-concurrency retry, not a
	// defect in Trainer or in this validator — but each retry adds latency
	// that could otherwise push first-ready past a tighter budget.
	TrainerControllerReadyTimeout = 3 * time.Minute

	// TrainerInstallPollInterval is the sleep between checks that a
	// recipe-declared Kubeflow Trainer installation has become complete. The
	// benchmark polls rather than failing on the first incomplete read, because a
	// CRD that is present but not yet Established — or a controller Deployment
	// that has not appeared — is an ordinary rollout state, not a failed deploy.
	TrainerInstallPollInterval = 5 * time.Second

	// NCCLTrainJobTimeout is the maximum time to wait for the NCCL all-reduce TrainJob to complete.
	NCCLTrainJobTimeout = 30 * time.Minute

	// NCCLStaleNamespacePruneAge is how old a leftover aicr-nccl-perf-* namespace
	// must be before the best-effort prune in runNCCLTrainJob deletes it. Sized
	// well past NCCLTrainJobTimeout so a namespace still owned by an in-progress
	// sibling variant, or a run that is simply slow, is never touched.
	NCCLStaleNamespacePruneAge = 2 * NCCLTrainJobTimeout

	// NCCLExecutionLockStaleAge is how long an unrenewed run execution lock
	// (see claimNCCLExecutionLock) is honored before a new caller may take
	// it over. It only needs to cover the gaps between renewals, not the
	// full run, since a live pod protects the namespace on its own via
	// verifyNCCLNamespaceNotLive regardless of lock staleness. Kept short
	// so a retry after a hard kill isn't stuck waiting out
	// NCCLStaleNamespacePruneAge instead.
	NCCLExecutionLockStaleAge = 20 * time.Minute

	// NCCLLauncherPodTimeout is the maximum time to wait for the NCCL launcher pod to be created.
	NCCLLauncherPodTimeout = 5 * time.Minute

	// NCCLTrainerArchiveDownloadTimeout is the timeout for downloading the Kubeflow Trainer
	// source archive from GitHub. The archive is several MB, so a longer timeout than the
	// standard HTTPClientTimeout is appropriate.
	NCCLTrainerArchiveDownloadTimeout = 5 * time.Minute

	// TrainJobAdmissionRetryTimeout bounds retrying the NCCL TrainJob create when the
	// Kubeflow Trainer validating webhook rejects it because the webhook's informer cache
	// has not yet observed the just-created TrainingRuntime. waitForTrainingRuntime already
	// confirms the runtime is visible at the API server, but the webhook validates runtimeRef
	// against a separate lister that lags that strongly-consistent read — a freshness the
	// client cannot observe. This bounds how long we let the webhook cache catch up.
	TrainJobAdmissionRetryTimeout = 1 * time.Minute

	// NCCLResourceRecreateWait bounds waitForResourceGone in createUnstructured's
	// same-run TrainJob reclaim. A controller-serviced finalizer (Trainer v2 /
	// JobSet ownership) can hold the delete for a while, so this gets its own
	// bound instead of the much shorter DiagnosticTimeout used for the rest of
	// createUnstructured's calls.
	NCCLResourceRecreateWait = 5 * time.Minute
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

	// ModelCachePopulateTimeout bounds the one-time model-cache populate Job (a
	// cold vLLM/cache image pull plus the first-ever Hugging Face snapshot
	// download into the PVC). It is deliberately larger than, and separate from,
	// InferenceWorkloadReadyTimeout: the populate Job pays a cold image pull *and*
	// a multi-GB anonymous download, so sharing the 10m workload-ready budget made
	// it flake on slow-pull / throttled-download nights (issue #1859). Providing
	// the optional HF token (already wired into the populate Job) removes the
	// anonymous-download throttling; this larger budget covers the pull + download
	// even without it. Sized to keep the inference sequential worst case under
	// CheckExecutionTimeout (asserted in timeouts_test.go).
	ModelCachePopulateTimeout = 13 * time.Minute

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

	// MaxOpenVEXBytes caps the size of an OpenVEX document read from disk
	// (the `.openvex.json` source that tools/openvex-bind projects onto a
	// platform manifest digest). The committed document is well under
	// 100 KiB even with per-statement impact prose; 1 MiB is generous
	// headroom while bounding an attacker-influenced path before
	// os.ReadFile would allocate the whole file into memory.
	MaxOpenVEXBytes int64 = 1 * 1024 * 1024 // 1 MiB

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

	// MaxPublicKeyPEMBytes caps the size of a local PEM public-key file passed
	// to `aicr verify --key` (#1152). A PEM-encoded ECDSA P-256 or RSA-4096
	// public key is well under 2 KiB; 64 KiB is generous headroom while
	// bounding an attacker-influenced --key path (symlink to /proc, NFS mount,
	// FUSE filesystem) before the bytes are read into memory.
	MaxPublicKeyPEMBytes int64 = 64 * 1024 // 64 KiB

	// MaxTrustedRootBytes caps the size of a user-supplied Sigstore
	// trusted_root.json passed to `aicr verify --trust-root`. A real trusted
	// root is a few KB; 1 MiB is generous headroom while bounding an
	// attacker-influenced path (a /proc symlink, an NFS mount) so it cannot
	// OOM the process the way os.ReadFile would.
	MaxTrustedRootBytes int64 = 1 * 1024 * 1024 // 1 MiB

	// MaxSigningConfigBytes caps the size of a user-supplied Sigstore
	// signing_config.json passed to `aicr bundle --signing-config` (#1650). A
	// real signing config is a few KB; 1 MiB is generous headroom while bounding
	// an attacker-influenced path (a /proc symlink, an NFS mount) so it cannot
	// OOM the process the way sigstore-go's bare os.ReadFile would.
	MaxSigningConfigBytes int64 = 1 * 1024 * 1024 // 1 MiB

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

	// ServerDefaultBindAddress is the default listen address for aicrd.
	// Empty means bind every interface — required for Kubernetes deployment
	// because kubelet probes (livenessProbe/readinessProbe httpGet) dial
	// the pod IP directly, kube-proxy routes Service traffic to the pod IP,
	// and Cloud Run / Fargate style runtimes route inbound requests to the
	// container's advertised interface. A loopback-only default would
	// CrashLoop every in-tree Deployment because probes cannot reach a
	// service bound to 127.0.0.1. Operators who need a tighter bind on a
	// bare-host or sidecar deployment set EnvServerAddress explicitly.
	// The SSRF hardening for /v1/bundle?vendor-charts=true does not rely
	// on the bind address; the opt-in gate + egress policy + index
	// pre-check + artifact cap are the acute controls (see issue #2118).
	ServerDefaultBindAddress = ""

	// EnvServerAddress overrides ServerDefaultBindAddress. Both "unset"
	// and "set to empty string" resolve to the same all-interfaces bind
	// — parseConfig uses os.LookupEnv (not os.Getenv) so the two cases
	// are distinguishable at parse time, but the resulting cfg.Address
	// is the same empty string in either case. Set to "127.0.0.1" for a
	// loopback-only bind on a sidecar/bare-host deployment, or to a
	// specific interface to constrain listener binding. Any pod-network
	// deployment should leave this unset (or empty) so kubelet probes
	// and kube-proxy can reach the container.
	EnvServerAddress = "AICR_SERVER_ADDRESS"

	// EnvAllowVendorCharts opts the server into honoring vendor-charts=true
	// on bundle requests. Off by default because vendor-charts drives
	// server-side helm pull against a caller-supplied URL — see the SSRF
	// egress-policy check in pkg/bundler/deployer/localformat.vendor.go.
	// Even with the egress-policy filter and artifact size cap, an operator
	// must explicitly acknowledge the network egress this endpoint performs.
	// Parsed by strconv.ParseBool — accepts 1/t/T/TRUE/true/True to enable
	// (and 0/f/F/FALSE/false/False to disable). Any other value (including
	// "yes", "on", or a typo) fails closed to disabled with a WARN log.
	EnvAllowVendorCharts = "AICR_ALLOW_VENDOR_CHARTS"

	// EnvHelmRepositoryHost names the single repository host the vendor
	// path is allowed to send EnvHelmRepositoryUsername /
	// EnvHelmRepositoryPassword to during the index.yaml pre-check.
	// Unset (default) suppresses credentials even when the username env
	// is set, so a caller-supplied Repository URL cannot steer operator
	// credentials at an attacker-controlled host. Credentials are also
	// only attached over HTTPS; a scheme mismatch or host mismatch
	// suppresses them silently. Go's http.Client strips Authorization on
	// cross-origin redirects automatically, so this env var is the
	// initial-URL gate.
	EnvHelmRepositoryHost = "AICR_HELM_REPOSITORY_HOST"

	// EnvHelmRepositoryUsername / EnvHelmRepositoryPassword are the
	// commonly-documented Helm repository-auth env vars. The upstream
	// helm CLI does NOT read these for `helm pull --repo <url>` — the
	// subprocess sends no Authorization header regardless. They are
	// consumed only by the vendor path's own index.yaml pre-check when
	// EnvHelmRepositoryHost gates the attachment (see attachHelmBasicAuth
	// in pkg/bundler/deployer/localformat/vendor.go). Named here as
	// constants so the raw string literals do not drift between the
	// production call site, the attachment gate, and the test coverage.
	EnvHelmRepositoryUsername = "HELM_REPOSITORY_USERNAME"
	EnvHelmRepositoryPassword = "HELM_REPOSITORY_PASSWORD"
)

// Server-side bundle-signing configuration (see docs/plans/2026-07-20-server-bundle-attestation-design.md).
const (
	// EnvSigningKey selects KMS-backed (Mode A) signing. Value is a cosign
	// KMS URI (awskms:// | gcpkms:// | azurekms:// | hashivault://).
	EnvSigningKey = "AICR_SIGNING_KEY"

	// EnvFulcioURL is the private Fulcio CA endpoint; its presence (with a
	// token source) selects keyless (Mode B) signing.
	EnvFulcioURL = "AICR_FULCIO_URL"

	// EnvRekorURL overrides the Rekor transparency-log endpoint (both modes).
	EnvRekorURL = "AICR_REKOR_URL"

	// EnvIdentityTokenFile is the path to the server's own OIDC token
	// (projected ServiceAccount token, audience "sigstore") for Mode B.
	// Read fresh per request.
	EnvIdentityTokenFile = "AICR_IDENTITY_TOKEN_FILE"

	// EnvTLogUpload=false disables the Rekor upload for KMS (Mode A) signing
	// (air-gapped). KMS-only; keyless always uploads.
	EnvTLogUpload = "AICR_TLOG_UPLOAD"

	// EnvSigningConfigPath points signing at a Sigstore SigningConfig JSON
	// (Rekor v2 targeting). Honored by both modes.
	EnvSigningConfigPath = "AICR_SIGNING_CONFIG_PATH"

	// EnvGitHubActionsIDTokenRequestURL / …RequestToken are the GitHub Actions
	// ambient OIDC endpoint env vars used for keyless (Mode B) signing when
	// aicrd itself runs in a GitHub Actions job.
	EnvGitHubActionsIDTokenRequestURL   = "ACTIONS_ID_TOKEN_REQUEST_URL"
	EnvGitHubActionsIDTokenRequestToken = "ACTIONS_ID_TOKEN_REQUEST_TOKEN"

	// EnvBinaryAttestationFile overrides the path to the server's own binary
	// attestation (tool provenance). Unset falls back to the conventional
	// <executable>-attestation.sigstore.json next to the running binary. Set it
	// when the attestation ships elsewhere in the image (e.g. a ko build's
	// KO_DATA_PATH: /var/run/ko/aicrd-attestation.sigstore.json).
	EnvBinaryAttestationFile = "AICR_BINARY_ATTESTATION_FILE"

	// EnvKoDataPath is ko's runtime data directory env var (set automatically
	// inside ko-built images). AICR ships the server's per-architecture binary
	// attestation there so a multi-arch image needs no per-deployment config.
	EnvKoDataPath = "KO_DATA_PATH"

	// BinaryAttestationKoDataNameFormat is the per-architecture filename for the
	// aicrd binary attestation shipped in ko's KO_DATA_PATH. The %s is GOARCH
	// (e.g. amd64, arm64). The .goreleaser.yaml aicrd build hook writes files
	// with this exact convention into cmd/aicrd/kodata/; keep the two in sync.
	BinaryAttestationKoDataNameFormat = "aicrd-%s-attestation.sigstore.json"

	// EnvBinaryAttestationIdentityRegexp overrides the certificate-identity
	// pattern the server pins its own binary attestation to. Unset uses the
	// release-workflow default (verifier.TrustedRepositoryPattern). A custom
	// value MUST still contain "NVIDIA/aicr" (enforced by
	// verifier.ValidateIdentityPattern) so it stays pinned to the NVIDIA org;
	// it retargets WHICH NVIDIA workflow attested the binary (e.g. an e2e
	// workflow), not the org. Mirrors the CLI's --certificate-identity-regexp.
	EnvBinaryAttestationIdentityRegexp = "AICR_BINARY_ATTESTATION_IDENTITY_REGEXP"
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

	// ValidatorMaxTerminationMsgBytes bounds the container termination message
	// copied into a validator result. kubelet caps the message at 4 KiB
	// upstream, but the result flows into ConfigMaps and rendered reports, so
	// this bounds it defensively at the source regardless of upstream behavior.
	ValidatorMaxTerminationMsgBytes = 4096

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

	// TrainJobAdmissionRetryInterval is the backoff between NCCL TrainJob create
	// attempts while the Kubeflow Trainer validating webhook's informer cache
	// catches up to a freshly-created TrainingRuntime.
	TrainJobAdmissionRetryInterval = 500 * time.Millisecond

	// NCCLLauncherLogReadInterval is the backoff between re-reads of a succeeded
	// NCCL launcher pod's log while waiting for the results table to be fully
	// captured. A pod that has just reached Succeeded can briefly serve an empty
	// or truncated log if its container is being torn down mid-read.
	NCCLLauncherLogReadInterval = 2 * time.Second

	// NCCLLauncherLogReadAttempts bounds how many times the succeeded launcher
	// pod's log is re-read before giving up and returning the last read for
	// diagnosis (the parser then fails and the log is surfaced).
	NCCLLauncherLogReadAttempts = 5
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

// Recipe component value-resolution defaults.
const (
	// HelmValueResolutionConcurrency caps concurrent values-file reads performed
	// by the public client facade. External catalogs may contain many Helm
	// components, so unbounded fan-out can exhaust file descriptors or overload a
	// remote-backed DataProvider. Eight preserves useful parallelism while
	// keeping per-request resource use predictable.
	HelmValueResolutionConcurrency = 8
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
	//
	// This is a render-safe floor, not a support floor. This constant must
	// stay at or above the strictest kubeVersion any bundled chart declares.
	// Do NOT lower it to match the ">= 1.32" recipe floor in
	// recipes/overlays/base.yaml: recipes are validated against their own
	// constraints, while mirror discovery raises lower versions to this
	// value solely for Helm rendering (see mirror.KubeVersionFromConstraints).
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

	// HelmChartArtifactLimit caps the size of a single vendored chart .tgz.
	// Real charts on the AICR registry are single-digit MB; the cap sits well
	// above headroom for future growth (multi-arch bundles, embedded CRDs)
	// while still bounding server memory. An attacker who can steer the
	// vendor path at a large blob is otherwise limited only by disk and
	// HelmChartPullTimeout — see localformat.(*CLIChartPuller).Pull.
	HelmChartArtifactLimit int64 = 64 * 1024 * 1024 // 64 MiB

	// HelmChartIndexBodyLimit caps the response body for a repository
	// index.yaml pre-fetch (used by the vendor path to validate every
	// declared chart tarball URL against the egress policy before invoking
	// helm). Real indexes range from ~50 KB (single-chart repos) to ~450 KB
	// (charts.jetstack.io); mega-repos with tens of thousands of charts can
	// hit tens of MB. 32 MiB gives multi-decade growth headroom for real
	// repos while bounding memory against a hostile index-of-the-index-of.
	HelmChartIndexBodyLimit int64 = 32 * 1024 * 1024 // 32 MiB

	// HelmChartIndexMaxRedirects bounds how many HTTP redirect hops the
	// vendor pre-check will follow when fetching a repository's index.yaml.
	// helm's own default is 10; matching it avoids rejecting legitimate
	// charts served behind CDN chains while still bounding both time and
	// the CheckRedirect callback fanout.
	HelmChartIndexMaxRedirects = 10

	// HelmChartIndexPreCheckTimeout bounds the pre-check GET of a
	// repository's index.yaml. Sized for a small YAML fetch through a
	// CDN or geographically distant registry, not a large tarball —
	// HelmChartPullTimeout (5 minutes) is the wrong ceiling here. Real
	// indexes come back in well under a second; the buffer covers cold
	// resolver caches and slow-start CDN edges without letting a stalled
	// upstream tie up an aicrd request slot for minutes.
	HelmChartIndexPreCheckTimeout = 30 * time.Second

	// HelmChartIndexRetryBudget is the maximum number of index fetch
	// attempts before failing permanently. Retryable errors are transport
	// failures, connection resets, and 5xx / 408 / 429 responses from the upstream.
	//
	// WARNING: Retry shares the parent timeout budget. On the HTTP server path,
	// the pre-check shares the 60s BundleHandlerTimeout with the subsequent helm
	// pull; a maxed-out pre-check (30s + 1s backoff + 29s) leaves ~0s for the
	// chart download. Consider capping total pre-check wall-clock if upstream
	// latency becomes a concern.
	HelmChartIndexRetryBudget = 3

	// HelmChartIndexRetryInitialBackoff is the wait between the first and
	// second index fetch attempts. Subsequent backoffs scale by
	// exponential factor 2: HelmChartIndexRetryInitialBackoff * 2^(attempt-1).
	HelmChartIndexRetryInitialBackoff = 1 * time.Second
)

// OCI publication phase budgets. The whole-publish ceiling covers two source
// stages (the CLI verification snapshot and the packageer's retained copy),
// local archive/store construction, every registry attempt and maximum-jitter
// backoff, and atomic image-reference output.
const (
	// OCISourceStageTimeout bounds one verified source-to-private-workspace
	// copy. The CLI and the OCI packager each perform one such stage.
	OCISourceStageTimeout = 2 * time.Minute

	// OCILocalPackageTimeout bounds deterministic archive generation and
	// insertion into the retained local OCI store.
	OCILocalPackageTimeout = 4 * time.Minute

	// RegistryPushTimeout is the per-attempt timeout for a single oras.Copy
	// invocation against a remote registry. Each retry receives a fresh
	// budget of this size.
	RegistryPushTimeout = 7 * time.Minute

	// RegistryPushRetries is the maximum number of oras.Copy attempts
	// (initial attempt plus retries) for transient registry failures.
	RegistryPushRetries = 3

	// RegistryPushBackoff is the initial backoff between retry attempts.
	// The backoff is doubled per attempt and jittered by +/-25%.
	RegistryPushBackoff = 1 * time.Second

	// OCIPushConcurrency is the maximum number of concurrent blob copy
	// tasks within a single oras.Copy invocation.
	OCIPushConcurrency = 3

	// OCIImageRefsWriteTimeout bounds final atomic image-reference output.
	OCIImageRefsWriteTimeout = 30 * time.Second

	// OCIBundlePublishTimeout bounds the complete verify, stage, package,
	// registry-push, and image-reference publication sequence.
	OCIBundlePublishTimeout = 35 * time.Minute
)

// OCI recipe-source pull budgets and resource limits. Recipe catalogs are
// small text trees; these ceilings leave substantial headroom while bounding
// network, memory, and filesystem use by an untrusted registry artifact.
const (
	// OCIRecipeConstructionTimeout bounds complete OCI recipe-source
	// construction: staging, digest authorization, materialization, layered
	// provider creation, and catalog validation. Eight minutes reserves more
	// than three minutes for local materialization and validation after the
	// maximum-jitter registry retry budget is exhausted.
	OCIRecipeConstructionTimeout = 8 * time.Minute

	// OCIRecipePullTimeout bounds each OCI recipe-source phase independently,
	// including staging and materialization. It remains a per-phase ceiling;
	// OCIRecipeConstructionTimeout is the separate complete-operation bound.
	OCIRecipePullTimeout = 5 * time.Minute

	// OCIRecipePullAttemptTimeout bounds one registry graph-copy attempt.
	OCIRecipePullAttemptTimeout = 90 * time.Second

	// OCIRecipePullRetries is the total attempts, including the first.
	OCIRecipePullRetries = 3

	// OCIRecipePullBackoff is the initial exponential retry backoff.
	OCIRecipePullBackoff = 1 * time.Second

	// MaxOCIRecipeManifestBytes caps one fetched OCI manifest.
	MaxOCIRecipeManifestBytes int64 = 1 * 1024 * 1024

	// MaxOCIRecipeLayerBytes caps the compressed recipe layer.
	MaxOCIRecipeLayerBytes int64 = 64 * 1024 * 1024

	// MaxOCIRecipeDownloadBytes caps compressed artifact content per attempt.
	MaxOCIRecipeDownloadBytes int64 = 64 * 1024 * 1024

	// MaxOCIRecipeRetryTrafficBytes caps response traffic across all attempts.
	MaxOCIRecipeRetryTrafficBytes int64 = OCIRecipePullRetries *
		(MaxOCIRecipeManifestBytes + MaxOCIRecipeDownloadBytes + 1)

	// MaxOCIRecipeExtractedBytes caps the complete expanded tar stream.
	MaxOCIRecipeExtractedBytes int64 = 128 * 1024 * 1024

	// MaxOCIRecipeFileBytes caps one materialized recipe file.
	MaxOCIRecipeFileBytes int64 = MaxExternalDataFileBytes

	// MaxOCIRecipeFiles caps all materialized filesystem nodes.
	MaxOCIRecipeFiles = 4096
)
