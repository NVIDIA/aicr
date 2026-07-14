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

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	"github.com/NVIDIA/aicr/validators/internal/allocmode"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	resourcev1beta1 "k8s.io/api/resource/v1beta1"
	resourcev1beta2 "k8s.io/api/resource/v1beta2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/utils/ptr"
)

const (
	// aiperfBaseImage is the pre-built AIPerf benchmark image published by the
	// aicr release workflow (.github/workflows/on-tag.yaml; Dockerfile at
	// validators/performance/aiperf-bench.Dockerfile). The aiperf pip package
	// is baked in at build time — benchmark pods need no PyPI access at
	// runtime and every run uses an identical version, making the check
	// air-gap friendly. The :latest tag is rewritten to the CLI version by
	// catalog.Load for release builds.
	aiperfBaseImage = "ghcr.io/nvidia/aicr-validators/aiperf-bench:latest"

	// aiperfJobNamePrefix is the prefix for the per-run AIPerf benchmark Job
	// name. The Job lives in the shared validator namespace (ctx.Namespace),
	// so the run ID suffix prevents two concurrent validate invocations from
	// deleting each other's Job, waiting on the wrong pod, or collecting the
	// wrong logs. The full name is built in buildInferenceConfig and stored
	// on inferenceWorkloadConfig.aiperfJobName.
	aiperfJobNamePrefix = "aicr-aiperf"

	// aiperfResultSentinel delimits the AIPerf JSON output in the pod log
	// stream. Everything before the first sentinel is treated as noise
	// (pip output, warnings, progress); parseAIPerfOutput slices between
	// two sentinels to isolate the benchmark JSON.
	aiperfResultSentinel = "===AIPERF-RESULT==="

	// inferenceModel is the default model for performance validation —
	// representative of a real serving workload (not a token smoke test), so the
	// default run exercises GPU compute rather than serving overhead. Override
	// per accelerator via the `inference-model` recipe constraint, globally via
	// AICR_INFERENCE_PERF_MODEL, or fall through to this default
	// (see resolveModel / resolveInferenceModel). An 8B model is large enough to
	// benefit from the model-weights cache — enable it (MODEL_CACHE_SIZE) so the
	// workers skip the per-worker Hugging Face download.
	inferenceModel = "Qwen/Qwen3-8B"

	// aiperfConcurrencyPerGPU is the default number of concurrent requests per
	// GPU. 256/GPU is the empirical throughput sweet spot across RTX PRO 6000,
	// GB200, and H100 (EKS + GKE) on an 8B model: at or essentially at peak
	// throughput on every platform while staying below the over-saturation knee
	// (≥512/GPU degrades goodput and explodes TTFT on GB200 and EKS H100).
	// Override per accelerator via the `inference-concurrency-per-gpu` recipe
	// constraint, or globally via AICR_INFERENCE_PERF_CONCURRENCY_PER_GPU.
	aiperfConcurrencyPerGPU = 256

	// aiperfWarmupPerConcurrency is the number of warmup requests sent per
	// in-flight concurrency slot before measurement begins. vLLM compiles CUDA
	// graphs / JIT-warms kernels on its first requests; without warmup that
	// one-time cost lands inside the measured window and dominates p99 TTFT
	// (observed ~42s p99 on a cold run). One full concurrency wave primes every
	// in-flight slot so steady-state latency/throughput are what get measured.
	// Warmup requests are excluded from AIPerf's reported statistics.
	aiperfWarmupPerConcurrency = 1

	// aiperfMinRequests is the minimum total number of MEASURED requests to send
	// (warmup is separate and excluded). Actual request count is
	// max(aiperfMinRequests, concurrency * aiperfRequestsPerConcurrency) to keep
	// the steady-state window long enough for a stable throughput/TTFT estimate
	// while satisfying AIPerf's request_count >= concurrency requirement.
	aiperfMinRequests = 1000

	// aiperfRequestsPerConcurrency scales the measured request count with the
	// concurrency level (and therefore GPU count) so larger nodes still run a
	// multi-wave steady-state measurement rather than a handful of requests.
	aiperfRequestsPerConcurrency = 8

	// aiperfInputTokensMean is the mean number of input tokens per request.
	aiperfInputTokensMean = 128

	// aiperfOutputTokensMean is the mean number of output tokens per request.
	aiperfOutputTokensMean = 128

	// Determinism knobs — make the benchmark reproducible run-to-run so the
	// verdict reflects the deployment, not RNG. The benchmark is driven with a
	// fixed random seed, fixed input/output token counts (stddev 0), a fixed
	// dataset-entry pool, and greedy decoding (temperature 0 via --extra-inputs,
	// AIPerf's recommended way to get deterministic output without ignore_eos).
	// aiperfRandomSeed seeds prompt selection, ordering, and sampling.
	aiperfRandomSeed = 100
	// aiperfNumDatasetEntries pins the synthetic-prompt pool size (AIPerf's
	// default is 100; pinned here so it's explicit and reproducible).
	aiperfNumDatasetEntries = 100

	// aiperfArtifactDir is where AIPerf writes benchmark result files.
	aiperfArtifactDir = "/tmp/aiperf"

	// AICR_INFERENCE_PERF_* env vars let operators tune the benchmark without
	// rebuilding the validator image. Each overrides the like-named constant
	// above; set them on the inference-perf catalog entry's `env` (editable
	// in-tree or via `aicr ... --data`). An unset knob uses the constant
	// default; a value that is not a positive integer aborts the check with
	// ErrCodeInvalidRequest (see validatePerfTuningEnvs) rather than silently
	// defaulting. These are validation *methodology* knobs and live with the
	// validator/catalog; the per-accelerator pass/fail thresholds stay in the
	// recipe overlays.
	envConcurrencyPerGPU      = "AICR_INFERENCE_PERF_CONCURRENCY_PER_GPU"
	envWarmupPerConcurrency   = "AICR_INFERENCE_PERF_WARMUP_PER_CONCURRENCY"
	envMinRequests            = "AICR_INFERENCE_PERF_MIN_REQUESTS"
	envRequestsPerConcurrency = "AICR_INFERENCE_PERF_REQUESTS_PER_CONCURRENCY"
	envInputTokensMean        = "AICR_INFERENCE_PERF_INPUT_TOKENS_MEAN"  //nolint:gosec // G101: env var name for a token-count knob, not a credential
	envOutputTokensMean       = "AICR_INFERENCE_PERF_OUTPUT_TOKENS_MEAN" //nolint:gosec // G101: env var name for a token-count knob, not a credential
	envModel                  = "AICR_INFERENCE_PERF_MODEL"
	// envWorkloadReadyTimeout overrides how long to wait for the
	// DynamoGraphDeployment to become ready (image pull + model load + worker
	// health). Default is defaults.InferenceWorkloadReadyTimeout (tuned for the
	// small smoke-test model). Large models load far slower, so raise this for
	// characterization — but it is bounded by the parent check deadline
	// (AICR_CHECK_TIMEOUT, from the catalog entry's `timeout`), which must be
	// raised in tandem for a large value to take full effect.
	envWorkloadReadyTimeout = "AICR_INFERENCE_PERF_WORKLOAD_READY_TIMEOUT"
	// envHealthTimeout overrides how long the endpoint-readiness probe
	// (waitForEndpointReady) waits for the frontend to serve a real
	// chat-completion after the workload reports Ready. Default is
	// defaults.InferenceHealthTimeout (5m). This window covers Dynamo
	// worker→frontend registration and the worker's first model-load read;
	// when many workers load a large model concurrently from a single RWO
	// cache PVC, that read contends on the volume and can run past 5m, so raise
	// this for large-model / high-worker-count runs. Like the workload-ready
	// knob it is bounded by the parent check deadline (AICR_CHECK_TIMEOUT, from
	// the catalog entry's `timeout`), which must be raised in tandem.
	envHealthTimeout = "AICR_INFERENCE_PERF_HEALTH_TIMEOUT"
	// envRouterMode overrides the Dynamo frontend routing strategy
	// (DYN_ROUTER_MODE) the benchmark workload runs with, for characterizing
	// alternative strategies without rebuilding the validator image (issue
	// #1374). Default dynRouterModeDefault (least-loaded, see issue #1197);
	// validated against the Dynamo frontend enum by resolveRouterMode, failing
	// closed with ErrCodeInvalidRequest on a typo. Only CONSUMED on the
	// `dynamo-router` routing path — in `gateway-epp` mode the EPP performs
	// endpoint selection and the worker frontend sidecars run in direct mode.
	// A set value is still VALIDATED in either mode (validatePerfTuningEnvs):
	// the routing mode comes from the recipe, not from whoever set this env,
	// so mode-gated validation would let the same typo pass silently under one
	// recipe and abort under another. Set-but-invalid always fails closed.
	envRouterMode = "AICR_INFERENCE_PERF_ROUTER_MODE"

	// perfConstraintModel / perfConstraintConcurrency / perfConstraintRoutingMode
	// name recipe performance.constraints entries that configure the benchmark
	// per accelerator — symmetric with how inference-throughput / inference-ttft-p99
	// thresholds already live in the recipe. Resolution precedence is recipe >
	// catalog env > compiled default for model/concurrency, and recipe > compiled
	// default for routing mode. Unlike the throughput/TTFT entries these carry
	// bare values, not comparator expressions.
	perfConstraintModel       = "inference-model"
	perfConstraintConcurrency = "inference-concurrency-per-gpu"
	perfConstraintRoutingMode = "inference-routing-mode"

	// inferenceDeploymentName is the DynamoGraphDeployment name for the benchmark
	// workload. Passed to the template via ${DEPLOYMENT_NAME}.
	inferenceDeploymentName = "aicr-inference-perf"

	// inferenceQueueName is the KAI Queue name for the benchmark workload.
	// Passed to the template via ${QUEUE_NAME}.
	inferenceQueueName = "aicr-inference-perf"

	// hfTokenSecretName / hfTokenSecretKey name the optional Secret that carries
	// a Hugging Face token. The deploy template references it via an optional
	// secretKeyRef on each container, so when the Secret is absent (no token)
	// workers fall back to anonymous downloads (unchanged default). A token is
	// only needed to lift HF rate limits when many workers pull a large model
	// concurrently (small smoke-test models never hit the limit).
	hfTokenSecretName = "aicr-hf-token" //nolint:gosec // G101: Kubernetes Secret resource name, not a credential
	hfTokenSecretKey  = "token"         //nolint:gosec // G101: Secret data key, not a credential

	// envHFToken is the validator's own env var carrying the HF token, forwarded
	// from the CLI process by the orchestrator (never sourced from the in-repo
	// catalog). When set, deployInferenceWorkload provisions hfTokenSecretName.
	envHFToken = "HF_TOKEN" //nolint:gosec // G101: env var name, not a hardcoded credential

	// inferenceFrontendPort is the port exposed by the Dynamo frontend service.
	// Used in the deploy path to construct the benchmark endpoint before the
	// Service object exists.
	inferenceFrontendPort int32 = 8000

	// inferenceGatewayNamespace / inferenceGatewayName identify the AICR-managed
	// agentgateway Gateway used by the gateway-epp routing mode.
	inferenceGatewayNamespace = "agentgateway-system"
	inferenceGatewayName      = "inference-gateway"
	inferenceGatewayPort      = int32(80)

	// inferenceWorkloadNamespacePrefix is the base for the per-run benchmark
	// namespace. Each run suffixes this with its unique run ID (from the
	// AICR_RUN_ID env var the Deployer injects, or a random suffix for
	// standalone invocations) so concurrent validate invocations never share
	// a namespace and can't tear down each other's resources mid-benchmark.
	inferenceWorkloadNamespacePrefix = "aicr-inference-perf"

	// gpuDRADriverName is the NVIDIA GPU DRA driver identifier. Used to filter
	// ResourceClaim allocation results when computing in-use GPU count per
	// node (matching the `driver` field the NVIDIA DRA driver stamps on every
	// DeviceRequestAllocationResult), and as the DeviceClass the worker
	// ResourceClaimTemplate binds to when the DRA wiring mode is selected.
	gpuDRADriverName = "gpu.nvidia.com"

	// gpuResourceName is the device-plugin GPU resource name. Worker GPU
	// wiring is MODE-DISPATCHED (see allocmode.Detect in
	// validateInferencePerf): when full-GPU DRA is usable the workers bind a
	// DRA ResourceClaimTemplate (required on clusters whose kai-scheduler
	// treats nodes bearing raw node-local GPU ResourceSlices as DRA-only and rejects scalar GPU
	// requests — kai v0.14.1 node_info.go (the scalar-GPU-on-DRA-node rejection, upstream-marked "temporary fix")); otherwise they request GPUs
	// through resources.limits[gpuResourceName], which works on clusters
	// without the gpu.nvidia.com DeviceClass (issue #1327).
	gpuResourceName = "nvidia.com/gpu"

	// inferenceClaimTemplateName is the DRA ResourceClaimTemplate name used to
	// allocate one GPU per worker pod in DRA wiring mode. Passed to the
	// template via ${CLAIM_TEMPLATE_NAME}.
	inferenceClaimTemplateName = "aicr-inference-gpu-claim"

	// inferenceWorkerGPUCount is the number of GPUs each worker pod requests.
	// Worker replicas scale with the free GPU count (one data-parallel worker
	// per GPU), so each pod requests exactly one device — in both wiring
	// modes (device-plugin limit of 1, or ExactCount=1 on the claim).
	inferenceWorkerGPUCount = 1

	// mainContainerName is the v1beta1 Dynamo container name that receives
	// operator defaults and the injected nvidia.com/gpu limit.
	mainContainerName = "main"
)

type inferenceRoutingMode string

const (
	inferenceRoutingModeDynamoRouter inferenceRoutingMode = "dynamo-router"
	inferenceRoutingModeGatewayEPP   inferenceRoutingMode = "gateway-epp"
)

// dynRouterModeDefault is the Dynamo frontend routing strategy interpolated
// into the deployment template as ${ROUTER_MODE}. Least-loaded balances by
// each worker's active in-flight load rather than KV-overlap, so a
// transiently-slow worker stops receiving its full share — mitigating the
// stochastic EKS H100 worker-stall at the saturation knee (issue #1197).
// Override via AICR_INFERENCE_PERF_ROUTER_MODE (issue #1374).
const dynRouterModeDefault = "least-loaded"

// dynRouterModes is the curated subset of the upstream DYN_ROUTER_MODE
// choices (Dynamo v1.2.1 router_args.py) that the benchmark supports; the
// order here is the order shown in the resolveRouterMode error message.
// Upstream's `direct` mode is deliberately excluded: it requires an
// externally supplied worker ID per request, and AICR's AIPerf invocation
// sends no routing hints — the workload would deploy and then fail opaquely
// mid-run. resolveRouterMode fails closed on anything outside this list so a
// catalog/env typo surfaces up front instead of minutes into the run.
var dynRouterModes = []string{
	"kv",
	"round-robin",
	"random",
	"power-of-two",
	"least-loaded",
	"device-aware-weighted",
}

// dynRouterModeSet is the lookup form of dynRouterModes.
var dynRouterModeSet = func() map[string]bool {
	s := make(map[string]bool, len(dynRouterModes))
	for _, m := range dynRouterModes {
		s[m] = true
	}
	return s
}()

// GVRs for Dynamo, KAI Scheduler, and Gateway API resources.
var (
	dynamoDeploymentGVR = schema.GroupVersionResource{
		Group:    "nvidia.com",
		Version:  versionV1beta1,
		Resource: "dynamographdeployments",
	}

	kaiQueueGVR = schema.GroupVersionResource{
		Group:    "scheduling.run.ai",
		Version:  "v2",
		Resource: "queues",
	}

	httpRouteGVR = schema.GroupVersionResource{
		Group:    "gateway.networking.k8s.io",
		Version:  "v1",
		Resource: "httproutes",
	}
)

// inferenceResult holds parsed benchmark results from AIPerf output.
type inferenceResult struct {
	throughput float64 // output tokens/sec
	ttftP99Ms  float64 // time to first token p99, milliseconds
	status     string  // "ok" or "skipped - reason"
	// GPU counts for scaling the (full-node) throughput gate to what was
	// actually benchmarked: gpuCount = free GPUs the workload was sized to,
	// gpuCountPerNode = the chosen node's full allocatable count.
	gpuCount        int
	gpuCountPerNode int
}

// aiperfOutput represents the JSON output structure from AIPerf.
// Matches the schema of profile_export_aiperf.json.
type aiperfOutput struct {
	OutputTokenThroughput aiperfMetricAvg         `json:"output_token_throughput"`
	TimeToFirstToken      aiperfMetricPercentiles `json:"time_to_first_token"`
}

// aiperfMetricAvg holds a metric with an average value.
type aiperfMetricAvg struct {
	Unit string  `json:"unit"`
	Avg  float64 `json:"avg"`
}

// aiperfMetricPercentiles holds a metric with percentile breakdowns.
type aiperfMetricPercentiles struct {
	Unit string  `json:"unit"`
	Avg  float64 `json:"avg"`
	P99  float64 `json:"p99"`
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
}

// inferenceWorkloadConfig holds the configuration for deploying and benchmarking
// the inference workload, derived from cluster state. Pod-scheduling fields
// are typed (not YAML strings) so they can be injected into the unstructured
// DynamoGraphDeployment object safely, without string templating.
type inferenceWorkloadConfig struct {
	runID                  string // unique per run; suffix for namespace + aiperfJobName
	gpuCount               int
	gpuCountPerNode        int
	concurrency            int
	gpuNodeSelector        map[string]string
	gpuTolerations         []v1.Toleration
	namespace              string // per-run; derived from runID
	aiperfJobName          string // per-run; derived from runID
	model                  string // HF model ID; resolved recipe > env > default (see resolveModel)
	deployedByUs           bool   // true if we (or a prior run we own) created the workload
	modelCacheSize         string // PVC size (e.g. "100Gi") enabling the model-weights cache; empty = disabled
	modelCacheStorageClass string // StorageClass for the cache PVC; empty = cluster default
	routingMode            inferenceRoutingMode
	routerMode             string // Dynamo frontend DYN_ROUTER_MODE (dynamo-router path only); env > default (see resolveRouterMode)

	// gpuAllocMode is the cluster's detected GPU allocation capability
	// (allocmode.Detect), probed once per run. Carried for evidence output
	// and for the served resource.k8s.io API version the DRA wiring creates
	// its ResourceClaimTemplate at.
	gpuAllocMode *allocmode.Mode
	// draWorkerWiring selects the worker GPU wiring, decided NODE-LOCALLY in
	// buildInferenceConfig from the chosen node's membership in the probe's
	// DRA node set (not from the cluster-global DRAUsable flag): DRA
	// ResourceClaimTemplate when true, device-plugin limits otherwise.
	draWorkerWiring bool
}

// useDRAWorkerClaims reports whether worker GPUs are wired via the DRA
// ResourceClaimTemplate path. Decided node-locally in buildInferenceConfig:
// true when the CHOSEN worker node advertises usable full-GPU DRA devices —
// on such nodes kai-scheduler (v0.14.1) rejects scalar device-plugin GPU
// requests outright (pkg/scheduler/api/node_info/node_info.go (the scalar-GPU-on-DRA-node rejection, upstream-marked "temporary fix")), so the
// claim path is the only schedulable wiring there. On device-plugin-only
// nodes the limits path applies — it needs no gpu.nvidia.com DeviceClass
// (issue #1327).
func (c *inferenceWorkloadConfig) useDRAWorkerClaims() bool {
	return c.draWorkerWiring
}

// validateInferencePerf orchestrates the full inference performance pipeline:
// discover or deploy workload → benchmark → cleanup.
func validateInferencePerf(ctx *validators.Context) (*inferenceResult, error) {
	slog.Info("Starting inference performance validation")

	// Guard B: dynamo-platform must be declared in the recipe's componentRefs.
	// Presence of a Criteria block is independent and not consulted here.
	if !hasDynamoPlatform(ctx) {
		return &inferenceResult{status: "skipped - dynamo-platform not in recipe components"}, nil
	}

	// Guard C: the Dynamo operator CRD must actually be installed on the
	// cluster. The recipe can list dynamo-platform before the operator has
	// been deployed (e.g., mid-bootstrap, or a staged rollout where the
	// component is declared but `aicr bundle` hasn't run yet). Without this
	// check the validator would fail later with a less-actionable
	// "no matches for kind DynamoGraphDeployment" from the dynamic client.
	//
	// Only IsNotFound is treated as "not installed" → skip. Any other error
	// (Forbidden, auth failure, apiserver timeout, transient connection) is
	// a real problem with the check and must surface as a failure rather
	// than masquerading as a benign skip.
	installed, crdErr := dynamoCRDInstalled(ctx)
	if crdErr != nil {
		return nil, crdErr
	}
	if !installed {
		return &inferenceResult{status: "skipped - DynamoGraphDeployment CRD not installed on cluster (dynamo-platform component declared but operator not deployed yet)"}, nil
	}

	// Build workload configuration from cluster state. Callees already
	// return pkg/errors StructuredError values with meaningful codes
	// (ErrCodeInternal for infra/config problems, ErrCodeTimeout for deadline
	// exhaustion, etc.); propagate as-is to preserve the classification.
	// Probe the cluster's GPU allocation capability ONCE — the same detector
	// the conformance checks use (validators/internal/allocmode) — BEFORE
	// sizing: buildInferenceConfig decides the worker node, wiring, and GPU
	// count node-locally from this probe. DRA wiring is mandatory on nodes
	// advertising full-GPU DRA: kai-scheduler treats nodes bearing raw
	// node-local GPU ResourceSlices as DRA-only and rejects scalar
	// nvidia.com/gpu requests, so
	// device-plugin wiring would leave every worker Pending (observed live on
	// dual-mode GB200/EKS; kai v0.14.1 node_info.go (the scalar-GPU-on-DRA-node rejection, upstream-marked "temporary fix")).
	mode, err := allocmode.Detect(ctx.Ctx, ctx.Clientset, ctx.DynamicClient)
	if err != nil {
		return nil, err
	}

	// Fail fast on GPU DRA configurations outside AICR's supported
	// validation matrix (#1652) — before any sizing or resource creation.
	if rejErr := rejectUnsupportedGPUTopology(mode); rejErr != nil {
		return nil, rejErr
	}

	// VERIFY (#1327): a recipe-configured policy pins the worker GPU wiring
	// — the cluster must actually serve the configured mechanism (fail
	// closed on drift, no silent fallback). Unspecified verifies nothing
	// and keeps the node-local capability dispatch. Evidence BEFORE the
	// verdict (parity with the conformance gates): a Verify failure must
	// leave the configured policy and inspected mode in the check output.
	policy := ctx.ValidationInput.GetGPUAllocationPolicy()
	fmt.Printf("--- GPU allocation policy ---\nconfigured policy: %s\n%s\n",
		policy, mode.Summary())
	if verifyErr := allocmode.Verify(policy, mode); verifyErr != nil {
		return nil, verifyErr
	}

	config, err := buildInferenceConfig(ctx, mode)
	if err != nil {
		return nil, err
	}
	wiring := "device plugin (resources.limits nvidia.com/gpu)"
	if config.useDRAWorkerClaims() {
		wiring = "DRA (gpu.nvidia.com ResourceClaimTemplate)"
	}
	slog.Info("GPU allocation mode detected",
		"draUsable", mode.DRAUsable, "devicePluginUsable", mode.DevicePluginUsable,
		"workerGPUWiring", wiring, "chosenNode", config.gpuNodeSelector["kubernetes.io/hostname"])
	fmt.Printf("--- GPU allocation mode (worker GPU wiring) ---\n%s\nworker GPU wiring: %s (node-local decision for %s)\n",
		mode.Summary(), wiring, config.gpuNodeSelector["kubernetes.io/hostname"])

	// Always defer cleanup — covers both successful deploy and partial failure.
	// cleanupInferenceWorkload is a no-op if deployedByUs is false.
	defer cleanupInferenceWorkload(ctx, config)

	// Always deploy fresh in this run's private namespace. We deliberately do
	// not adopt any pre-existing workload in the namespace: a prior run's
	// leftovers could have been deployed with different scheduling (different
	// --node-selector / --toleration settings, different GPU count), and two
	// concurrent runs that share a namespace can tear down each other's
	// resources mid-benchmark. Per-run namespace isolation makes both classes
	// of cross-run interference impossible.
	slog.Info("Deploying benchmark workload",
		"gpus", config.gpuCount, "namespace", config.namespace)

	if deployErr := deployInferenceWorkload(ctx, config); deployErr != nil {
		return nil, deployErr
	}

	// Look up the serving endpoint after the workload is ready. In Dynamo
	// router mode this is the frontend Service; in gateway-epp mode it is the
	// AICR-managed inference gateway that routes to the generated InferencePool.
	endpoint, err := resolveInferenceEndpoint(ctx, config)
	if err != nil {
		return nil, err
	}

	slog.Info("Using inference endpoint", "endpoint", endpoint, "concurrency", config.concurrency)

	// Wait for the endpoint to be ready to serve requests. Callee returns
	// ErrCodeTimeout on deadline exhaustion, ErrCodeInternal on
	// request-construction errors; both classifications are lost if we rewrap.
	if readyErr := waitForEndpointReady(ctx.Ctx, endpoint, config.model); readyErr != nil {
		return nil, readyErr
	}

	// Run AIPerf benchmark. On failure, surface the captured pod logs so the
	// CTRF report contains enough signal to diagnose (pip install errors,
	// aiperf CLI failures, connection refused, etc.). Without this, the
	// error chain alone ("pod failed") is unactionable.
	logs, err := runAIPerfJob(ctx, endpoint, config.model, config.concurrency, config.aiperfJobName)
	if err != nil {
		if logs != "" {
			fmt.Printf("AIPerf pod logs:\n%s\n", logs)
		}
		// Preserve the underlying code (ErrCodeTimeout for pod-wait deadline,
		// ErrCodeInternal for Create/log-fetch failures).
		return nil, err
	}

	// Parse results. parseAIPerfOutput already returns ErrCodeInternal on
	// missing/malformed JSON; no rewrap needed.
	result, err := parseAIPerfOutput(logs)
	if err != nil {
		return nil, err
	}
	// Carry the benchmarked GPU counts up so the throughput gate can be scaled
	// to the free-GPU sizing buildInferenceConfig chose (see checkInferencePerf).
	result.gpuCount = config.gpuCount
	result.gpuCountPerNode = config.gpuCountPerNode

	slog.Info("Inference benchmark results",
		"throughput_tok/s", result.throughput,
		"ttft_p99_ms", result.ttftP99Ms,
		"gpus", result.gpuCount, "nodeGPUs", result.gpuCountPerNode)

	return result, nil
}

// hasDynamoPlatform checks if dynamo-platform is in the validation ComponentRefs.
func hasDynamoPlatform(ctx *validators.Context) bool {
	if ctx.ValidationInput == nil {
		return false
	}
	for _, ref := range ctx.ValidationInput.ComponentRefs {
		if ref.Name == "dynamo-platform" {
			return true
		}
	}
	return false
}

// dynamoCRDInstalled reports whether the DynamoGraphDeployment CRD is
// registered on the cluster. This is a pre-flight check so the validator
// produces an explicit "CRD not installed" skip instead of later failing
// deep in the deploy path with an opaque "no matches for kind" error when
// the recipe declares dynamo-platform but the operator has not been
// deployed yet.
//
// Mirrors the signature of isTrainerInstalled: only IsNotFound returns
// (false, nil) — any other error (Forbidden, auth failure, apiserver
// timeout, transient connection) surfaces as a real validator failure
// rather than being collapsed into a benign "not installed" skip.
func dynamoCRDInstalled(ctx *validators.Context) (bool, error) {
	crdGVR := schema.GroupVersionResource{
		Group:    apiGroupAPIExtensions,
		Version:  "v1",
		Resource: resourceCRDs,
	}
	getCtx, cancel := context.WithTimeout(ctx.Ctx, defaults.DiagnosticTimeout)
	defer cancel()
	_, err := ctx.DynamicClient.Resource(crdGVR).Get(getCtx, "dynamographdeployments.nvidia.com", metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, errors.Wrap(errors.ErrCodeInternal, "failed to check for DynamoGraphDeployment CRD", err)
	}
	return true, nil
}

// buildInferenceConfig determines the benchmark workload's sizing and
// scheduling from the cluster, honoring any CLI overrides.
//
// Ordering matters: the user's --node-selector override must be applied BEFORE
// sizing the workload. Otherwise gpuCount (and therefore concurrency) is
// computed from gpuNodes[0], which on a heterogeneous cluster can be a
// different GPU family than the one --node-selector actually targets — so the
// workload would be sized for the wrong pool. We instead restrict the
// candidate node pool to nodes matching the user selector, then derive all
// sizing and scheduling details from within that pool.
func buildInferenceConfig(ctx *validators.Context, mode *allocmode.Mode) (*inferenceWorkloadConfig, error) {
	slog.Info("Analyzing GPU node configuration...")

	// Node discovery is policy-dispatched (#1327): under the configured
	// dra-resource-claim policy candidates come from the probe's validated
	// DRA facts (Mode.DRANodes) — scalar allocatable nvidia.com/gpu is
	// absent on DRA-only clusters, so the scalar-only helper would find
	// nothing there. Under device-plugin-extended-resource and unspecified,
	// scalar discovery applies as before.
	policy := ctx.ValidationInput.GetGPUAllocationPolicy()
	var gpuNodes []v1.Node
	var err error
	if policy == validatorv1.GPUAllocationPolicyDRAResourceClaim {
		gpuNodes, err = findDRACapableNodes(ctx, mode)
		if err != nil {
			return nil, err
		}
	} else {
		gpuNodes, err = helper.FindSchedulableGpuNodes(ctx.Ctx, ctx.Clientset)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to find GPU nodes", err)
		}
		if len(gpuNodes) == 0 {
			// DRA-only surface (#1620 review): a cluster whose GPU nodes
			// carry usable full-GPU DRA devices but no scalar allocatable
			// must be named — the generic message hides the actual state.
			if mode != nil && len(mode.DRANodes) > 0 {
				return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
					"no schedulable GPU node advertises scalar %s, but %d node(s) carry usable full-GPU DRA devices (%s) — a DRA-only cluster is not supported by AICR validation without a recipe-configured dra-resource-claim policy (experimental; see issue #1327)",
					gpuResourceName, len(mode.DRANodes), strings.Join(mode.DRANodes, ",")))
			}
			return nil, errors.New(errors.ErrCodeInternal, "no schedulable GPU nodes found")
		}
	}
	slog.Info("Found GPU nodes", "count", len(gpuNodes), "policy", policy)

	// Restrict the candidate pool to nodes matching the user's selector, if any.
	// This keeps gpuCount, nodeSelector, and tolerations all derived from a
	// consistent node cohort.
	candidates := gpuNodes
	if ctx.NodeSelector != nil {
		candidates = nodesMatchingSelector(gpuNodes, ctx.NodeSelector)
		if len(candidates) == 0 {
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("no schedulable GPU nodes match --node-selector %v", ctx.NodeSelector))
		}
		slog.Info("Filtered GPU nodes to match --node-selector",
			"selector", ctx.NodeSelector, "matched", len(candidates))
	}

	// Enumerate existing GPU allocations — device-plugin pod requests AND DRA
	// ResourceClaim allocations — so we can subtract per-node in-use GPUs from
	// the allocatable count. Status.Allocatable ("nvidia.com/gpu") reflects
	// device capacity and does NOT shrink when devices are allocated to other
	// workloads — so on a shared cluster the "first candidate with full
	// allocatable count" can already be saturated, leaving the benchmark
	// pending until timeout. We pick the candidate with the most free GPUs and
	// size the workload to that count.
	var gpuPoolNodes map[string]string
	if mode != nil {
		gpuPoolNodes = mode.GPUPoolNodes
	}
	scalarUsed, draUsed, err := countUsedGPUsByNode(ctx.Ctx, ctx.Clientset, gpuPoolNodes)
	if err != nil {
		return nil, err
	}

	chosen, draWiring, gpuCountPerNode, freeGPUs, ok := selectWorkerNode(candidates, mode, policy, scalarUsed, draUsed)
	if !ok {
		return nil, errors.New(errors.ErrCodeInternal, describeNoEligibleWorkerNode(candidates, mode, policy, scalarUsed))
	}
	if freeGPUs <= 0 {
		if policy == validatorv1.GPUAllocationPolicyDRAResourceClaim {
			// Policy-aware framing (#1327): under the configured DRA policy
			// only the DRA ledger applies — a device-plugin-shaped
			// explanation would misdirect the operator.
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("configured GPU allocation policy %q: no eligible DRA-capable candidate node has free GPUs (%d matched; existing gpu.nvidia.com ResourceClaim allocations saturate every eligible candidate's DRA ledger — DRA candidates carrying scalar nvidia.com/gpu occupancy are excluded entirely); free GPU claims or pass --node-selector kubernetes.io/hostname=<empty-node>",
					policy, len(candidates)))
		}
		return nil, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("no eligible candidate GPU node has free GPUs (%d matched; eligible candidates are saturated by existing workloads in the selected mechanism's ledger — nodes excluded as NotReady, probe-ineligible, kai-blocked, or DRA-wired-but-scalar-occupied are not counted); free GPUs or pass --node-selector kubernetes.io/hostname=<empty-node>",
				len(candidates)))
	}
	if gpuCountPerNode == 0 {
		return nil, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("candidate GPU node %q reports 0 usable GPUs for the selected wiring (draWiring=%t)", chosen.Name, draWiring))
	}
	gpuCount := freeGPUs
	if freeGPUs < gpuCountPerNode {
		slog.Info("Candidate node has GPUs already in use by existing workloads; sizing benchmark to free GPUs only",
			"node", chosen.Name, "allocatable", gpuCountPerNode, "inUse", gpuCountPerNode-freeGPUs, "free", freeGPUs)
	}

	concurrencyPerGPU, err := resolveConcurrencyPerGPU(ctx)
	if err != nil {
		return nil, err
	}

	// Cache is on by default (100Gi); operator can resize or disable via env.
	cacheSize, _, err := parseModelCacheSize(os.Getenv(envModelCacheSize))
	if err != nil {
		return nil, err
	}

	// Validate the resolved model ID up front. It is interpolated into the
	// DynamoGraphDeployment YAML template (value: ${MODEL}, --model ${MODEL}),
	// so a value with YAML/shell metacharacters would either break the parse
	// with an opaque error or be unsafe; fail closed with a clear message
	// instead. Symmetric with the AIPerf path, which passes the model via env.
	model := resolveModel(ctx)
	if modelErr := validateModelID(model); modelErr != nil {
		return nil, modelErr
	}

	routingMode, err := resolveRoutingMode(ctx)
	if err != nil {
		return nil, err
	}

	routerMode, err := resolveRouterMode()
	if err != nil {
		return nil, err
	}

	runID := deriveRunID()
	config := &inferenceWorkloadConfig{
		runID:                  runID,
		gpuCount:               gpuCount,
		gpuCountPerNode:        gpuCountPerNode,
		concurrency:            concurrencyPerGPU * gpuCount,
		namespace:              fmt.Sprintf("%s-%s", inferenceWorkloadNamespacePrefix, runID),
		aiperfJobName:          fmt.Sprintf("%s-%s", aiperfJobNamePrefix, runID),
		model:                  model,
		modelCacheSize:         cacheSize,
		modelCacheStorageClass: strings.TrimSpace(os.Getenv(envModelCacheStorageClass)),
		routingMode:            routingMode,
		routerMode:             routerMode,
		gpuAllocMode:           mode,
		draWorkerWiring:        draWiring,
	}

	// Pin every worker to the specific chosen node via kubernetes.io/hostname
	// unconditionally — even when the user provided --node-selector.
	// --node-selector narrowed the candidate pool already (via
	// nodesMatchingSelector above), so `chosen` is guaranteed to satisfy it.
	// But applying the user's label-only selector to the worker pods
	// directly would let the scheduler spread workers across every matching
	// node in the pool, turning the measurement into a pool-level average
	// rather than the intended single-node baseline. The hostname selector
	// uniquely identifies the node so all workers co-locate.
	config.gpuNodeSelector = map[string]string{
		"kubernetes.io/hostname": chosen.Name,
	}
	if ctx.NodeSelector != nil {
		slog.Info("--node-selector narrowed candidate pool; workers pinned to single node via kubernetes.io/hostname",
			"selector", ctx.NodeSelector, "node", chosen.Name, "freeGPUs", freeGPUs)
	} else {
		slog.Info("Pinning inference workers to single GPU node for stable per-node baseline",
			"node", chosen.Name, "freeGPUs", freeGPUs)
	}

	// Tolerations: ctx.Tolerations == nil means the operator didn't pass
	// any --toleration flag, so mirror the chosen GPU node's own taints to
	// keep workers pinned to that node and nothing else. A non-nil slice
	// means the operator explicitly opted into a toleration set — honor it
	// verbatim, including the valid `--toleration '*'` tolerate-all form.
	//
	// This relies on pkg/cli/validate.go not calling ParseTolerations when
	// no flag was provided; otherwise the implicit default and explicit
	// wildcard would collapse to the same in-memory value and this branch
	// couldn't tell them apart.
	if ctx.Tolerations != nil {
		config.gpuTolerations = ctx.Tolerations
		slog.Info("Using user-provided toleration override for inference workers",
			"count", len(ctx.Tolerations))
	} else {
		config.gpuTolerations = buildTolerations(chosen)
	}

	return config, nil
}

// countUsedGPUsByNode returns two maps of nodeName → in-use GPU count, one
// per allocation source a shared cluster can mix (issue #1327). The counts
// are kept SEPARATE because the two allocators keep independent ledgers:
// scalar occupancy shrinks a DRA candidate's usable device COUNT, but DRA
// claim placement cannot avoid the specific physical devices the device
// plugin assigned — so a DRA-wired benchmark sized "around" scalar usage
// could still double-book a plugin-held GPU. selectWorkerNode therefore
// rejects DRA candidates carrying any scalar occupancy instead of
// subtracting across ledgers. The two sources:
//
//  1. Device-plugin allocations: the effective nvidia.com/gpu request of every
//     scheduled, non-terminal pod, summed per pod.Spec.NodeName. Terminal pods
//     (Succeeded/Failed) have released their devices and are skipped; a
//     terminating-but-running pod still holds its devices, so it is counted —
//     under-counting would size the benchmark onto a saturated node.
//  2. DRA allocations: existing ResourceClaim allocations with driver ==
//     gpuDRADriverName ("gpu.nvidia.com"), attributed to nodes through the
//     slice-derived gpuPoolNodes map (pool name → the spec.nodeName of the
//     node-local slices publishing that pool). The K8s API documents that a
//     pool name "is often the node name, but this is not required", so name
//     equality would mis-attribute a pool named after a DIFFERENT existing
//     node; an allocation from a pool no node-local gpu.nvidia.com slice
//     publishes fails fast (#1652) rather than being silently guessed. Kept
//     even though the benchmark itself requests GPUs via the device plugin —
//     a shared cluster may still run full-GPU DRA workloads that shrink the
//     free pool. Allocated claims whose device requests name a "gpu"-named
//     DeviceClass other than gpu.nvidia.com also fail fast (#1652):
//     kai-scheduler v0.14.1 classifies claim demand by the Exactly
//     request's DeviceClass name (IsGPUDeviceClass via
//     countGPUDevicesFromClaim, dra.go:164-184), so such claims consume
//     kai's GPU capacity vector through a driver this accounting would
//     otherwise skip. AICR also screens firstAvailable subrequest classes,
//     which kai skips — a deliberately conservative AICR policy, not kai
//     behavior. The screen checks class NAMES only: inference validation
//     assumes the driver-provided gpu.nvidia.com DeviceClass retains its
//     NVIDIA selector (device.driver == "gpu.nvidia.com", per the shipped
//     chart template) — an admin-replaced class with foreign selectors is
//     outside the supported matrix (known limitation, #1652).
//
// AdminAccess divergence (known, accepted — see #1652): kai-scheduler counts
// AdminAccess claim requests in its GPU demand vector, while this accounting
// correctly excludes them from physical occupancy (they hold no capacity) —
// a cluster running admin-access monitoring claims can therefore look busier
// to kai than to this count; a legitimate monitoring claim must not fail
// validation, so this is documented rather than rejected.
//
// Invariant: every allocated device is counted exactly once. A pod allocated
// via an explicit DRA claim carries no nvidia.com/gpu container request, and a
// device-plugin pod holds no gpu.nvidia.com ResourceClaim allocation — but
// under KEP-5004 (DRAExtendedResource) the scheduler can satisfy a plain
// nvidia.com/gpu extended-resource request from DRA capacity, in which case
// the same devices appear in BOTH sources: the pod-request sum and the
// scheduler-generated ResourceClaim's allocation. Such a pod advertises the
// generated claim in Status.ExtendedResourceClaimStatus. A single generated
// claim can back MULTIPLE extended resources, so the dedup is per claim
// REQUEST, not per claim: Status.ExtendedResourceClaimStatus.RequestMappings
// names, for each backed extended resource, the request in the generated
// claim that serves it. Only allocation results whose request is mapped to
// nvidia.com/gpu are excluded (already attributed on the pod-request side);
// results for other DRA-backed resources in the same claim still count.
//
// AdminAccess allocations (DRAAdminAccess feature gate) grant administrative
// or monitoring access to devices that remain allocatable to real workloads —
// they coexist with normal allocations and hold no capacity, so they are
// never counted as occupancy.
//
// Fails closed: a pod List failure propagates as an error rather than
// treating the cluster's GPUs as free (which would size the benchmark onto a
// possibly-saturated node and burn the check deadline pending). The DRA
// ResourceClaim list walks the served resource.k8s.io versions newest-first
// (v1, then v1beta2, then v1beta1 — beta-only K8s 1.32/1.33 clusters carry
// real DRA allocations too) and tolerates exactly one condition — NO version
// of the API being served — because that deterministically means "no DRA
// allocations"; every other failure (RBAC denied, timeout) propagates.
func countUsedGPUsByNode(ctx context.Context, clientset kubernetes.Interface, gpuPoolNodes map[string]string) (scalarUsed, draUsed map[string]int, err error) {
	listCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	scalarUsed = make(map[string]int)
	draUsed = make(map[string]int)

	// Per-claim sets (keyed namespace/name) of generated-claim REQUEST names
	// whose allocations are already accounted for by a counted pod's
	// nvidia.com/gpu extended-resource request (KEP-5004) — those requests'
	// allocation results are skipped in the DRA loop below to keep the
	// count-once invariant. Other requests in the same claim (backing other
	// extended resources) still count.
	skipClaimRequests := make(map[string]map[string]struct{})

	// Server-side filter sheds terminal pods before transfer. Fake clients
	// ignore field selectors, and older apiservers could too, so the in-code
	// terminal-phase filter below stays authoritative — the selector is an
	// optimization only.
	pods, err := clientset.CoreV1().Pods("").List(listCtx, metav1.ListOptions{
		FieldSelector: "status.phase!=" + string(v1.PodSucceeded) + ",status.phase!=" + string(v1.PodFailed),
	})
	if err != nil {
		return nil, nil, errors.Wrap(occupancyListErrCode(err),
			"failed to list pods for GPU occupancy accounting", err)
	}
	for i := range pods.Items {
		if ctxErr := listCtx.Err(); ctxErr != nil {
			return nil, nil, errors.Wrap(errors.ErrCodeTimeout,
				"GPU occupancy pod scan canceled", ctxErr)
		}
		pod := &pods.Items[i]
		if pod.Spec.NodeName == "" {
			// Not bound to a node — holds no device yet.
			continue
		}
		if pod.Status.Phase == v1.PodSucceeded || pod.Status.Phase == v1.PodFailed {
			// Terminal — devices already released back to the device plugin.
			continue
		}
		if n := podEffectiveGPURequest(pod); n > 0 {
			scalarUsed[pod.Spec.NodeName] += n
			// KEP-5004: this pod's extended-resource request may be satisfied
			// by DRA via a scheduler-generated ResourceClaim. Record which of
			// that claim's requests map to nvidia.com/gpu so the DRA loop can
			// exclude exactly those allocation results — their devices are
			// already covered by the pod-request sum just added.
			if ercs := pod.Status.ExtendedResourceClaimStatus; ercs != nil && ercs.ResourceClaimName != "" {
				key := pod.Namespace + "/" + ercs.ResourceClaimName
				for _, m := range ercs.RequestMappings {
					if m.ResourceName != gpuResourceName {
						continue
					}
					if skipClaimRequests[key] == nil {
						skipClaimRequests[key] = make(map[string]struct{})
					}
					skipClaimRequests[key][m.RequestName] = struct{}{}
				}
			}
		}
	}

	claims, err := listGPUClaimAllocations(listCtx, clientset)
	if err != nil {
		return nil, nil, errors.Wrap(occupancyListErrCode(err),
			"failed to list DRA ResourceClaims for GPU occupancy accounting", err)
	}
	for i := range claims {
		if ctxErr := listCtx.Err(); ctxErr != nil {
			return nil, nil, errors.Wrap(errors.ErrCodeTimeout,
				"GPU occupancy ResourceClaim scan canceled", ctxErr)
		}
		claim := &claims[i]
		// kai-scheduler v0.14.1 classifies claim demand by the Exactly
		// request's DeviceClass name (lowercase-contains "gpu"),
		// independent of the backing driver — so an allocated claim for a
		// foreign "gpu"-named class consumes kai's GPU capacity vector even
		// when its allocation results carry a non-"gpu" driver name that
		// the loop below would skip. AICR screens firstAvailable subrequest
		// classes too, which kai skips (countGPUDevicesFromClaim,
		// dra.go:164-184) — a deliberately conservative AICR policy, not
		// kai behavior. Outside AICR's supported matrix: fail fast.
		for _, class := range claim.deviceClassNames {
			if strings.Contains(strings.ToLower(class), "gpu") && class != gpuDRADriverName {
				return nil, nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
					"unsupported GPU DRA configuration: allocated ResourceClaim %s/%s requests DeviceClass %q — kai-scheduler classifies any \"gpu\"-named DeviceClass as GPU demand, but AICR's supported validation matrix covers only %s; see #1327, generalization tracked in #1652",
					claim.namespace, claim.name, class, gpuDRADriverName))
			}
		}
		skipRequests := skipClaimRequests[claim.namespace+"/"+claim.name]
		for _, r := range claim.results {
			if r.driver != gpuDRADriverName {
				continue
			}
			if r.adminAccess {
				// Administrative/monitoring access — coexists with real
				// allocations and holds no capacity, so it is not occupancy.
				continue
			}
			// For a firstAvailable subrequest the result's Request is
			// "<main request>/<subrequest>"; the KEP-5004 mapping names the
			// main request, so compare against the main-request prefix.
			mainRequest, _, _ := strings.Cut(r.request, "/")
			if _, counted := skipRequests[mainRequest]; counted {
				// Already attributed via the owning pod's nvidia.com/gpu
				// request (KEP-5004 extended-resource claim) — counting the
				// allocation too would double-count the same devices.
				continue
			}
			node, mapped := gpuPoolNodes[r.pool]
			if !mapped {
				return nil, nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
					"unsupported GPU DRA configuration: allocated ResourceClaim %s/%s has a %s allocation from pool %q, which no node-local %s ResourceSlice publishes — AICR's occupancy accounting attributes pools to nodes through the slices' spec.nodeName (the K8s API does not require pool names to be node names); see #1327, generalization tracked in #1652",
					claim.namespace, claim.name, gpuDRADriverName, r.pool, gpuDRADriverName))
			}
			draUsed[node]++
		}
	}
	return scalarUsed, draUsed, nil
}

// gpuClaimAllocation is a version-normalized view of an ALLOCATED
// ResourceClaim, carrying only the fields GPU occupancy accounting reads.
// The resource.k8s.io allocation-result shape is field-compatible across
// v1beta1, v1beta2, and v1, so one struct serves all served versions.
type gpuClaimAllocation struct {
	namespace string
	name      string
	// deviceClassNames lists every DeviceClass the claim's spec device
	// requests reference (v1/v1beta2 exactly + firstAvailable shapes;
	// v1beta1 direct field + firstAvailable). kai-scheduler v0.14.1
	// classifies claim demand by the Exactly request's class name only
	// (countGPUDevicesFromClaim skips request.Exactly == nil,
	// dra.go:164-184); occupancy accounting screens firstAvailable
	// classes too — a deliberately conservative AICR policy, not kai
	// behavior — for unsupported "gpu"-named classes (#1652).
	deviceClassNames []string
	results          []gpuClaimAllocationResult
}

type gpuClaimAllocationResult struct {
	request     string
	driver      string
	pool        string
	adminAccess bool
}

// claimClassNamesV1 collects every DeviceClass name the claim's spec device
// requests reference at resource.k8s.io/v1 (exactly + firstAvailable forms
// — kai v0.14.1 reads only the exactly form; including firstAvailable is
// AICR's deliberately conservative screen policy). Occupancy accounting
// screens these for unsupported "gpu"-named classes (#1652).
func claimClassNamesV1(c *resourcev1.ResourceClaim) []string {
	var classes []string
	for _, req := range c.Spec.Devices.Requests {
		if req.Exactly != nil {
			classes = append(classes, req.Exactly.DeviceClassName)
		}
		for _, sub := range req.FirstAvailable {
			classes = append(classes, sub.DeviceClassName)
		}
	}
	return classes
}

// claimClassNamesV1beta2 is claimClassNamesV1 at resource.k8s.io/v1beta2
// (identical exactly + firstAvailable shapes).
func claimClassNamesV1beta2(c *resourcev1beta2.ResourceClaim) []string {
	var classes []string
	for _, req := range c.Spec.Devices.Requests {
		if req.Exactly != nil {
			classes = append(classes, req.Exactly.DeviceClassName)
		}
		for _, sub := range req.FirstAvailable {
			classes = append(classes, sub.DeviceClassName)
		}
	}
	return classes
}

// claimClassNamesV1beta1 is the v1beta1 form: the class sits on the request
// itself (empty when the prioritized-list form is used) plus firstAvailable
// subrequests.
func claimClassNamesV1beta1(c *resourcev1beta1.ResourceClaim) []string {
	var classes []string
	for _, req := range c.Spec.Devices.Requests {
		if req.DeviceClassName != "" {
			classes = append(classes, req.DeviceClassName)
		}
		for _, sub := range req.FirstAvailable {
			classes = append(classes, sub.DeviceClassName)
		}
	}
	return classes
}

// listGPUClaimAllocations lists cluster-wide ResourceClaims at the newest
// SERVED resource.k8s.io version — v1, then v1beta2, then v1beta1 — and
// returns the allocated claims in version-normalized form. Only NotFound
// advances the fallback (that version is not served); any other error
// propagates (fail closed). Each normalization loop checks ctx so a
// cancellation that lands after List returns still aborts the scan instead
// of reporting a (possibly empty) result as success. When NO version is
// served it returns (nil, nil): "no DRA API" deterministically means "no DRA
// allocations".
func listGPUClaimAllocations(ctx context.Context, clientset kubernetes.Interface) ([]gpuClaimAllocation, error) {
	v1Claims, err := clientset.ResourceV1().ResourceClaims("").List(ctx, metav1.ListOptions{})
	if err == nil {
		// Check ctx immediately after the successful List too: with an empty
		// result the per-item checks below never run, and a canceled scan
		// must not masquerade as success.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		out := make([]gpuClaimAllocation, 0, len(v1Claims.Items))
		for i := range v1Claims.Items {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			c := &v1Claims.Items[i]
			if c.Status.Allocation == nil {
				continue
			}
			alloc := gpuClaimAllocation{
				namespace: c.Namespace, name: c.Name,
				deviceClassNames: claimClassNamesV1(c),
			}
			for _, r := range c.Status.Allocation.Devices.Results {
				alloc.results = append(alloc.results, gpuClaimAllocationResult{
					request: r.Request, driver: r.Driver, pool: r.Pool,
					adminAccess: r.AdminAccess != nil && *r.AdminAccess,
				})
			}
			out = append(out, alloc)
		}
		return out, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	b2Claims, err := clientset.ResourceV1beta2().ResourceClaims("").List(ctx, metav1.ListOptions{})
	if err == nil {
		// Check ctx immediately after the successful List too: with an empty
		// result the per-item checks below never run, and a canceled scan
		// must not masquerade as success.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		out := make([]gpuClaimAllocation, 0, len(b2Claims.Items))
		for i := range b2Claims.Items {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			c := &b2Claims.Items[i]
			if c.Status.Allocation == nil {
				continue
			}
			alloc := gpuClaimAllocation{
				namespace: c.Namespace, name: c.Name,
				deviceClassNames: claimClassNamesV1beta2(c),
			}
			for _, r := range c.Status.Allocation.Devices.Results {
				alloc.results = append(alloc.results, gpuClaimAllocationResult{
					request: r.Request, driver: r.Driver, pool: r.Pool,
					adminAccess: r.AdminAccess != nil && *r.AdminAccess,
				})
			}
			out = append(out, alloc)
		}
		return out, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	b1Claims, err := clientset.ResourceV1beta1().ResourceClaims("").List(ctx, metav1.ListOptions{})
	if err == nil {
		// Check ctx immediately after the successful List too: with an empty
		// result the per-item checks below never run, and a canceled scan
		// must not masquerade as success.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		out := make([]gpuClaimAllocation, 0, len(b1Claims.Items))
		for i := range b1Claims.Items {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			c := &b1Claims.Items[i]
			if c.Status.Allocation == nil {
				continue
			}
			alloc := gpuClaimAllocation{
				namespace: c.Namespace, name: c.Name,
				deviceClassNames: claimClassNamesV1beta1(c),
			}
			for _, r := range c.Status.Allocation.Devices.Results {
				alloc.results = append(alloc.results, gpuClaimAllocationResult{
					request: r.Request, driver: r.Driver, pool: r.Pool,
					adminAccess: r.AdminAccess != nil && *r.AdminAccess,
				})
			}
			out = append(out, alloc)
		}
		return out, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	slog.Debug("no resource.k8s.io ResourceClaim API version served; counting device-plugin GPU requests only")
	return nil, nil
}

// occupancyListErrCode classifies a K8s List failure during GPU occupancy
// accounting: every timeout form the shared allocmode classifier recognizes
// (context sentinels, apiserver Timeout/ServerTimeout, transport timeouts,
// the client-go rate-limiter's deadline message) is a deadline condition
// (ErrCodeTimeout), not an infrastructure fault — anything else is
// ErrCodeInternal. Keeping the codes distinct lets callers report "the
// occupancy scan ran out of time" separately from "the apiserver refused the
// scan".
func occupancyListErrCode(err error) errors.ErrorCode {
	if allocmode.IsK8sTimeoutErr(err) {
		return errors.ErrCodeTimeout
	}
	return errors.ErrCodeInternal
}

// podEffectiveGPURequest returns the pod's effective nvidia.com/gpu request,
// following Kubernetes' pod resource-accounting rules (mirrors the
// k8s.io/component-helpers resource helpers): init containers are walked in
// declaration order while a cumulative sum of restartable-init (sidecar)
// requests is maintained — a sidecar started earlier keeps running while
// later init containers run, so a one-shot init's effective value is that
// cumulative sum plus its own request, and a sidecar raises the running init
// maximum by the new cumulative sum itself. The pod's effective request is
// max(steady-state sum over regular containers plus all sidecars, init
// maximum), plus any pod spec.overhead (RuntimeClass overhead) — Kubernetes
// adds overhead AFTER taking the max, so it always increases the scheduled
// demand. Extended resources require requests == limits when both are set,
// so reading requests (with a limits fallback for specs that set only limits
// — the API server defaults requests from limits) is exact.
func podEffectiveGPURequest(pod *v1.Pod) int {
	gpu := v1.ResourceName(gpuResourceName)
	containerGPUs := func(c *v1.Container) int64 {
		if q, ok := c.Resources.Requests[gpu]; ok {
			return q.Value()
		}
		q := c.Resources.Limits[gpu]
		return q.Value()
	}
	var restartableSum, initMax int64
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		if c.RestartPolicy != nil && *c.RestartPolicy == v1.ContainerRestartPolicyAlways {
			// Sidecar init container: runs for the pod's lifetime, alongside
			// every init container declared after it.
			restartableSum += containerGPUs(c)
			if restartableSum > initMax {
				initMax = restartableSum
			}
			continue
		}
		// One-shot init container: runs to completion while all sidecars
		// declared before it are already running, so its effective demand is
		// the cumulative sidecar sum plus its own request.
		if n := restartableSum + containerGPUs(c); n > initMax {
			initMax = n
		}
	}
	sum := restartableSum
	for i := range pod.Spec.Containers {
		sum += containerGPUs(&pod.Spec.Containers[i])
	}
	effective := sum
	if initMax > effective {
		effective = initMax
	}
	// Pod overhead (RuntimeClass) is added on top of max(steadyState, initMax),
	// mirroring k8s.io/component-helpers resource accounting.
	if q, ok := pod.Spec.Overhead[gpu]; ok {
		effective += q.Value()
	}
	return int(effective)
}

// selectWorkerNode picks the (node, mechanism) pair with the most FREE GPUs —
// matching the documented contract (docs/user/validation.md: the validator
// "picks the candidate with the most free GPUs") while keeping the wiring
// NODE-LOCAL:
//
//   - Eligibility (TOCTOU-guarded): a candidate must be Ready on the FRESH
//     node object — the allocmode probe's node sets were captured earlier,
//     so a node Ready at probe time but NotReady now is excluded — AND, when
//     the probe ran, appear in the probe's Ready/schedulable sets.
//   - Mechanism per node: DRA where the node advertises usable full-GPU DRA
//     devices (kai-scheduler rejects scalar device-plugin GPU requests on
//     nodes bearing raw node-local GPU ResourceSlices — node_info.go (the scalar-GPU-on-DRA-node rejection, upstream-marked "temporary fix") — so plugin wiring is
//     not eligible there); device plugin otherwise.
//   - Capacity: Mode.DRANodeDevices for DRA pairs (scalar allocatable can
//     differ on dual-advertised nodes and would oversize); scalar
//     allocatable for plugin pairs.
//   - Ledger isolation: a DRA candidate carrying ANY scalar nvidia.com/gpu
//     occupancy is skipped outright — the device plugin's physical device
//     assignments are invisible to the DRA allocator, so claims sized
//     "around" the scalar count can still double-book a plugin-held GPU
//     (VRAM contention corrupts the benchmark). Subtraction across ledgers
//     is never safe; only same-ledger occupancy is subtracted.
//   - Score: free = capacity − same-ledger occupancy (clamped at 0; a stale
//     or mismatched allocation must not go negative). Max free wins; ties
//     prefer DRA, then the lexicographically smaller node name — fully
//     deterministic for equal inputs.
//
// A configured (non-unspecified) policy FORCES the mechanism (#1327): under
// dra-resource-claim only DRA-capable candidates are eligible; under
// device-plugin-extended-resource only device-plugin candidates are — a
// DRA-capable node is not (kai-scheduler rejects scalar GPU pods on nodes
// bearing gpu.nvidia.com ResourceSlices, node_info.go (the scalar-GPU-on-DRA-node rejection, upstream-marked "temporary fix")). The cross-ledger
// scalar-occupancy skip and the kai raw-slice guard stay in force in every
// mode.
//
// ok is false when no candidate is eligible at all.
func selectWorkerNode(candidates []v1.Node, mode *allocmode.Mode, policy string, scalarUsed, draUsed map[string]int) (chosen v1.Node, draWiring bool, allocatable, free int, ok bool) {
	draCapable := map[string]bool{}
	pluginBacked := map[string]bool{}
	if mode != nil {
		for _, n := range mode.DRANodes {
			draCapable[n] = true
		}
		for _, n := range mode.DevicePluginNodes {
			pluginBacked[n] = true
		}
	}
	for _, n := range candidates {
		// TOCTOU guard: current Ready condition, not just probe-time state.
		if !nodeReadyForScheduling(n) {
			continue
		}
		var capacity int
		var dra bool
		var occupancy int
		switch {
		case mode != nil && draCapable[n.Name]:
			if policy == validatorv1.GPUAllocationPolicyDevicePluginExtendedResource {
				// Configured device-plugin policy: DRA wiring is off the
				// table, and plugin wiring cannot schedule here either
				// (kai rejects scalar GPU pods on slice-bearing nodes).
				continue
			}
			// Supported full-GPU DRA state: the NVIDIA driver publishes
			// node-local per-node pools, so validated devices ARE the
			// kai-attributable devices — size from the validated count.
			// (Non-node-local gpu.nvidia.com topologies fail fast before
			// selection — see rejectUnsupportedGPUTopology / #1652.)
			if scalarUsed[n.Name] > 0 {
				// Ledger isolation: the device plugin assigned specific
				// physical devices this node's DRA allocator cannot see or
				// avoid — DRA claims here can double-book a plugin-held
				// GPU. Skip rather than subtract across ledgers.
				slog.Info("skipping DRA candidate: scalar nvidia.com/gpu occupancy is invisible to the DRA allocator",
					"node", n.Name, "scalarGPUsInUse", scalarUsed[n.Name])
				continue
			}
			dra, capacity, occupancy = true, mode.DRANodeDevices[n.Name], draUsed[n.Name]
		case mode != nil && mode.NodeLocalGPUSliceDevices[n.Name] > 0:
			// kai-compatibility guard: the node carries RAW gpu.nvidia.com
			// ResourceSlices that kai-scheduler counts (no pool/taint
			// validation) and therefore rejects scalar device-plugin GPU
			// pods on (node_info.go (the scalar-GPU-on-DRA-node rejection, upstream-marked "temporary fix")) — plugin wiring can never schedule
			// here. Not DRA-capable either (AICR could not validate the
			// slices), so the node is ineligible entirely.
			continue
		case mode == nil || pluginBacked[n.Name]:
			if policy == validatorv1.GPUAllocationPolicyDRAResourceClaim {
				// Configured DRA policy: only DRA-capable candidates are
				// eligible — never fall back to device-plugin wiring.
				continue
			}
			// Plugin-backed nodes carry no attributable gpu.nvidia.com
			// allocations (no node-local slices → no pool attribution), so
			// draUsed is normally 0 here; summing keeps the count honest if
			// that invariant ever changes.
			dra, capacity = false, nodeGPUCount(n)
			occupancy = scalarUsed[n.Name] + draUsed[n.Name]
		default:
			continue // not in the probe's Ready/schedulable sets
		}
		f := capacity - occupancy
		if f < 0 {
			f = 0
		}
		better := !ok || f > free ||
			(f == free && dra && !draWiring) ||
			(f == free && dra == draWiring && n.Name < chosen.Name)
		if better {
			chosen, draWiring, allocatable, free, ok = n, dra, capacity, f, true
		}
	}
	if ok {
		slog.Info("selected worker node by most free GPUs",
			"node", chosen.Name, "draWiring", draWiring, "allocatable", allocatable, "free", free)
	}
	return chosen, draWiring, allocatable, free, ok
}

// rejectUnsupportedGPUTopology fails fast (ErrCodeInvalidRequest) on GPU DRA
// configurations that AICR's inference validation deliberately does not
// model — the validator validates AICR's supported configurations rather
// than emulating kai-scheduler's general capacity model (#1327 scope
// decision; generalization is tracked as #1652):
//   - ResourceSlices from "gpu"-named drivers other than gpu.nvidia.com
//     (mixed GPU DRA drivers — kai counts them into its shared GPU vector);
//   - gpu.nvidia.com full-GPU slices with non-node-local topologies
//     (nodeSelector/allNodes/perDeviceNodeSelection — kai cannot
//     node-attribute them while the DRA driver can still bind them);
//   - gpu.nvidia.com pools published by node-local slices of multiple nodes
//     (pool → node attribution would be ambiguous; the NVIDIA driver
//     publishes one pool per node).
func rejectUnsupportedGPUTopology(mode *allocmode.Mode) error {
	if mode == nil {
		return nil
	}
	if len(mode.ForeignGPUSliceDrivers) > 0 {
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"unsupported GPU DRA configuration: ResourceSlices from non-NVIDIA GPU driver(s) [%s] are present — mixed GPU DRA drivers are outside AICR's supported validation matrix (see #1327; generalized driver support is tracked in #1652). Remove the foreign driver's slices or validate on a cluster with only gpu.nvidia.com full-GPU DRA",
			strings.Join(mode.ForeignGPUSliceDrivers, ", ")))
	}
	if len(mode.NonNodeLocalGPUSlices) > 0 {
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"unsupported GPU DRA configuration: gpu.nvidia.com ResourceSlice(s) [%s] use non-node-local topologies (nodeSelector/allNodes/perDeviceNodeSelection) — kai-scheduler cannot node-attribute them, and AICR's inference validation supports only the NVIDIA driver's node-local per-node pools (see #1327; generalized topology support is tracked in #1652)",
			strings.Join(mode.NonNodeLocalGPUSlices, ", ")))
	}
	if len(mode.AmbiguousGPUPools) > 0 {
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"unsupported GPU DRA configuration: gpu.nvidia.com ResourceSlice pool(s) [%s] are published by node-local slices of multiple nodes — pool → node attribution is ambiguous, and AICR's inference validation supports only the NVIDIA driver's one-pool-per-node layout (see #1327; generalized topology support is tracked in #1652)",
			strings.Join(mode.AmbiguousGPUPools, ", ")))
	}
	return nil
}

// describeNoEligibleWorkerNode builds the fail-fast message for
// selectWorkerNode returning no eligible (node, mechanism) pair, naming any
// candidates blocked by the kai-compatibility guard or by the cross-ledger
// scalar-occupancy skip so the operator can act on the actual cause instead
// of a generic "no nodes". Under the configured dra-resource-claim policy the
// message leads with the DRA-ledger framing (#1327) — a device-plugin-shaped
// explanation would misdirect the operator when only DRA candidates were ever
// eligible; unspecified/device-plugin wording is unchanged.
func describeNoEligibleWorkerNode(candidates []v1.Node, mode *allocmode.Mode, policy string, scalarUsed map[string]int) string {
	var msg string
	if policy == validatorv1.GPUAllocationPolicyDRAResourceClaim {
		msg = fmt.Sprintf("configured GPU allocation policy %q: no eligible DRA-capable GPU node among %d candidate(s) — every candidate is NotReady, absent from the allocation probe's usable full-GPU DRA node set, or DRA-capable but carrying scalar nvidia.com/gpu workloads the DRA allocator cannot see or avoid", policy, len(candidates))
	} else {
		msg = fmt.Sprintf("no eligible GPU node among %d candidate(s) — every candidate is NotReady, absent from the allocation-capability probe's Ready/schedulable node sets, blocked for device-plugin wiring by kai-visible gpu.nvidia.com ResourceSlices, or DRA-capable but carrying scalar nvidia.com/gpu workloads", len(candidates))
	}
	if mode == nil {
		return msg
	}
	draCapable := make(map[string]bool, len(mode.DRANodes))
	for _, n := range mode.DRANodes {
		draCapable[n] = true
	}
	var offenders []string
	var scalarOccupied []string
	for _, n := range candidates {
		if raw := mode.NodeLocalGPUSliceDevices[n.Name]; raw > 0 && !draCapable[n.Name] {
			offenders = append(offenders, fmt.Sprintf("%s (%d raw device(s))", n.Name, raw))
		}
		if draCapable[n.Name] && scalarUsed[n.Name] > 0 {
			scalarOccupied = append(scalarOccupied, fmt.Sprintf("%s (%d scalar GPU(s) in use)", n.Name, scalarUsed[n.Name]))
		}
	}
	if len(offenders) > 0 {
		msg += fmt.Sprintf(". Node(s) %s carry gpu.nvidia.com ResourceSlices that kai-scheduler counts but AICR cannot validate (incomplete pool, tainted devices, or unreachable node) — fix the ResourceSlices or free a different node", strings.Join(offenders, ", "))
	}
	if len(scalarOccupied) > 0 {
		msg += fmt.Sprintf(". DRA-capable node(s) %s hold GPUs through device-plugin nvidia.com/gpu requests, which the DRA allocator cannot see or avoid — free those workloads or pass --node-selector kubernetes.io/hostname=<clean-node>", strings.Join(scalarOccupied, ", "))
	}
	return msg
}

// nodeReadyForScheduling reports whether the node's Ready condition is True
// on the CURRENT node object — the freshness guard against probe-time state.
func nodeReadyForScheduling(node v1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == v1.NodeReady {
			return cond.Status == v1.ConditionTrue
		}
	}
	return false
}

// nodesMatchingSelector returns nodes whose Labels match every key=value pair
// in the selector. An empty or nil selector returns the input unchanged (no
// filter). Matching is exact: a node is included only when it has every
// key in the selector and the value for each key is an exact match.
func nodesMatchingSelector(nodes []v1.Node, selector map[string]string) []v1.Node {
	if len(selector) == 0 {
		return nodes
	}
	out := make([]v1.Node, 0, len(nodes))
	for _, n := range nodes {
		match := true
		for k, v := range selector {
			if n.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, n)
		}
	}
	return out
}

// findDRACapableNodes returns the fresh node objects for the allocation
// probe's usable full-GPU DRA node set (Mode.DRANodes) — the DRA-mode
// analog of helper.FindSchedulableGpuNodes for the configured
// dra-resource-claim policy (#1327): DRA-only nodes advertise no scalar
// allocatable nvidia.com/gpu, so scalar discovery finds nothing there.
// Capacity still comes from Mode.DRANodeDevices in selectWorkerNode.
// Currently-cordoned nodes are excluded (freshness guard, mirroring the
// scalar helper); Readiness is re-checked in selectWorkerNode.
func findDRACapableNodes(ctx *validators.Context, mode *allocmode.Mode) ([]v1.Node, error) {
	if mode == nil || len(mode.DRANodes) == 0 {
		return nil, errors.New(errors.ErrCodeInternal,
			"no Ready, schedulable node with usable full-GPU DRA devices for the configured dra-resource-claim policy")
	}
	// Bounded like the sibling occupancy List (countUsedGPUsByNode) — the
	// check-level deadline also applies, but the tighter diagnostic bound
	// keeps discovery from eating the workload budget on a slow apiserver.
	listCtx, cancel := context.WithTimeout(ctx.Ctx, defaults.DiagnosticTimeout)
	defer cancel()
	nodeList, err := ctx.Clientset.CoreV1().Nodes().List(listCtx, metav1.ListOptions{})
	if err != nil {
		return nil, allocmode.ClassifyK8sReadError(err, "nodes for DRA-mode inference node discovery")
	}
	draSet := make(map[string]bool, len(mode.DRANodes))
	for _, name := range mode.DRANodes {
		draSet[name] = true
	}
	var gpuNodes []v1.Node
	for _, node := range nodeList.Items {
		if node.Spec.Unschedulable || !draSet[node.Name] {
			continue
		}
		gpuNodes = append(gpuNodes, node)
	}
	if len(gpuNodes) == 0 {
		return nil, errors.New(errors.ErrCodeInternal,
			"no Ready, schedulable node with usable full-GPU DRA devices for the configured dra-resource-claim policy")
	}
	return gpuNodes, nil
}

// nodeGPUCount returns the node's allocatable nvidia.com/gpu count, or 0 if
// the resource is absent.
func nodeGPUCount(node v1.Node) int {
	q := node.Status.Allocatable[v1.ResourceName(gpuResourceName)]
	return int(q.Value())
}

// deriveRunID returns a short, unique suffix to isolate a single
// aicr-validate invocation's resources from any concurrent invocations.
// Output is always 8 hex chars — short enough that downstream names built
// from it (namespaces, AIPerf Job names, Grove-generated
// "<namespace>-<dgd-name>" PodClique labels) stay within Kubernetes'
// 63-char DNS-label limit. The CLI's AICR_RUN_ID (typically a 32-char
// "<date>-<time>-<hex>" string) is hashed down via SHA-256 so the runID
// is still deterministic per validator invocation; standalone CLI runs
// without AICR_RUN_ID get a random 4-byte hex suffix. Callers store the
// result once on inferenceWorkloadConfig.runID and derive both the
// namespace name and the AIPerf Job name from it — avoiding the bug of
// calling the derivation twice and getting two different values.
func deriveRunID() string {
	if runID := os.Getenv("AICR_RUN_ID"); runID != "" {
		sum := sha256.Sum256([]byte(runID))
		return hex.EncodeToString(sum[:4])
	}
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		// Extremely unlikely. Fall back to a hash of the current nanosecond
		// timestamp so the return stays 8 hex chars — downstream names
		// (namespace, Job name, Grove PodClique label) depend on a fixed
		// suffix length, so a different shape (e.g. "t1776943210000000000")
		// would silently blow the 63-char DNS-label limit.
		sum := sha256.Sum256(fmt.Appendf(nil, "%d", time.Now().UnixNano()))
		return hex.EncodeToString(sum[:4])
	}
	return hex.EncodeToString(buf)
}

// buildTolerations returns structured Toleration objects for the given GPU
// node's taints, excluding kubelet-managed node.kubernetes.io/* conditions.
// Returning typed values (not a YAML string) lets the caller inject them into
// an unstructured object without YAML templating, avoiding escape-injection
// issues when taint keys/values contain YAML-special characters.
func buildTolerations(node v1.Node) []v1.Toleration {
	if len(node.Spec.Taints) == 0 {
		return nil
	}
	tolerations := make([]v1.Toleration, 0, len(node.Spec.Taints))
	for _, taint := range node.Spec.Taints {
		if strings.HasPrefix(taint.Key, "node.kubernetes.io/") {
			continue
		}
		tolerations = append(tolerations, v1.Toleration{
			Key:      taint.Key,
			Operator: v1.TolerationOpEqual,
			Value:    taint.Value,
			Effect:   taint.Effect,
		})
	}
	return tolerations
}

// deployInferenceWorkload deploys the KAI Queue, DynamoGraphDeployment, and
// any routing-mode-specific Gateway API resources. Worker GPU wiring is
// MODE-DISPATCHED (config.useDRAWorkerClaims): in DRA mode a
// ResourceClaimTemplate is applied and workers bind it via
// podTemplate.spec.resourceClaims; in device-plugin mode no claim template is
// applied and workers carry nvidia.com/gpu limits, so the workload schedules
// on clusters without the gpu.nvidia.com DeviceClass (issue #1327). Sets
// config.deployedByUs = true as soon as any resource is created, so the
// deferred cleanup in the caller always runs — even if later steps fail.
func deployInferenceWorkload(ctx *validators.Context, config *inferenceWorkloadConfig) error {
	// Create namespace (idempotent).
	if err := ensureNamespace(ctx, config.namespace); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create namespace", err)
	}

	// Mark deployed early (namespace now exists) so cleanup tears down the
	// namespace — and everything created in it below (secret, cache PVC/Job,
	// DynamoGraphDeployment) — even on a partial failure.
	config.deployedByUs = true

	// Provision the optional Hugging Face token Secret before deploying so the
	// workers' secretKeyRef resolves. No-op when no token is set in the
	// validator env (anonymous downloads — unchanged default).
	if err := ensureHFTokenSecret(ctx, config.namespace); err != nil {
		return err
	}

	// Provision the optional model-weights cache (PVC + one-time populate Job)
	// before deploying, so workers mount a pre-populated cache and skip the
	// Hugging Face download. No-op when the cache is disabled.
	if err := ensureModelCache(ctx, config); err != nil {
		return err
	}

	templateData := map[string]string{
		"NAMESPACE":           config.namespace,
		"MODEL":               config.model,
		"ROUTER_MODE":         config.routerMode,
		"GPU_COUNT":           strconv.Itoa(config.gpuCount),
		"DEPLOYMENT_NAME":     inferenceDeploymentName,
		"QUEUE_NAME":          inferenceQueueName,
		"CLAIM_TEMPLATE_NAME": inferenceClaimTemplateName,
	}

	// Apply KAI Queue (best-effort; KAI scheduler may not be installed).
	queuePath := filepath.Join("testdata", "inference", "queue.yaml")
	if err := createOrUpdateFromTemplate(ctx, kaiQueueGVR,
		config.namespace, queuePath, templateData, nil); err != nil {
		slog.Info("Failed to apply KAI Queue (scheduler may not be installed)", "error", err)
	} else {
		slog.Info("Applied KAI Queue", "name", inferenceQueueName)
	}

	// DRA wiring mode: apply the ResourceClaimTemplate the worker pods bind
	// (one claim, ExactCount=1 GPU, per worker). Device-plugin mode skips it —
	// the template requires the gpu.nvidia.com DeviceClass, which is exactly
	// what such clusters lack.
	if config.useDRAWorkerClaims() {
		if err := applyWorkerClaimTemplate(ctx, config, templateData); err != nil {
			return err
		}
	}

	// Apply DynamoGraphDeployment with programmatic pod-scheduling injection.
	// The YAML templates have no ${GPU_*} placeholders; scheduling is applied
	// to v1beta1 component podTemplate specs via applyInferenceWorkerScheduling below.
	deployPath := filepath.Join("testdata", "inference", dynamoDeploymentTemplate(config.routingMode))
	mutator := func(obj *unstructured.Unstructured) error {
		return applyInferenceWorkerScheduling(obj, config)
	}
	if err := createOrUpdateFromTemplate(ctx, dynamoDeploymentGVR,
		config.namespace, deployPath, templateData, mutator); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to apply DynamoGraphDeployment", err)
	}
	slog.Info("Applied DynamoGraphDeployment",
		"name", inferenceDeploymentName, "gpuWorkers", config.gpuCount,
		"routingMode", config.routingMode, "routerMode", config.routerMode)

	// Wait for the deployment to become ready.
	if err := waitForDynamoDeploymentReady(ctx, config); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "DynamoGraphDeployment not ready", err)
	}

	if config.routingMode == inferenceRoutingModeGatewayEPP {
		routePath := filepath.Join("testdata", "inference", "http-route-gateway-epp.yaml")
		if err := createOrUpdateFromTemplate(ctx, httpRouteGVR,
			config.namespace, routePath, templateData, nil); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to apply HTTPRoute for gateway-epp routing", err)
		}
		slog.Info("Applied HTTPRoute for gateway-epp routing",
			"name", inferenceHTTPRouteName(), "gateway", inferenceGatewayName)
	}

	return nil
}

func dynamoDeploymentTemplate(mode inferenceRoutingMode) string {
	if mode == inferenceRoutingModeGatewayEPP {
		return "dynamo-deployment-gateway-epp.yaml"
	}
	return "dynamo-deployment.yaml"
}

func inferenceHTTPRouteName() string {
	return inferenceDeploymentName + "-route"
}

// applyWorkerClaimTemplate creates (or updates) the DRA ResourceClaimTemplate
// the workers bind, AT THE SERVED resource.k8s.io version the allocmode probe
// detected (Mode.APIVersion — v1, v1beta2, or v1beta1). v1 and v1beta2 share
// the `exactly:` request shape and use the parametrized shared template;
// v1beta1 carries the request fields inline and has its own template file.
// Built-in recipes floor K8s 1.34 (v1), so the beta paths are reachable only
// through custom validation catalogs on 1.32/1.33 clusters.
func applyWorkerClaimTemplate(ctx *validators.Context, config *inferenceWorkloadConfig, templateData map[string]string) error {
	version := "v1"
	if config.gpuAllocMode != nil && config.gpuAllocMode.APIVersion != "" {
		version = config.gpuAllocMode.APIVersion
	}
	templateFile := "resource-claim-template.yaml"
	if version == "v1beta1" {
		templateFile = "resource-claim-template-v1beta1.yaml"
	}
	data := make(map[string]string, len(templateData)+1)
	for k, v := range templateData {
		data[k] = v
	}
	data["CLAIM_API_VERSION"] = version

	claimPath := filepath.Join("testdata", "inference", templateFile)
	if err := createOrUpdateFromTemplate(ctx, allocmode.GVRAt(version, "resourceclaimtemplates"),
		config.namespace, claimPath, data, nil); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to apply ResourceClaimTemplate", err)
	}
	slog.Info("Applied ResourceClaimTemplate",
		"name", inferenceClaimTemplateName, "apiVersion", "resource.k8s.io/"+version)
	return nil
}

// applyInferenceWorkerScheduling injects nodeSelector, tolerations, and the
// mode-dispatched worker GPU wiring into each v1beta1 component's podTemplate:
// DRA resourceClaims (bound to the applied ResourceClaimTemplate) when
// config.useDRAWorkerClaims(), device-plugin GPU limits
// (resources.limits["nvidia.com/gpu"]) otherwise. Operating on the
// unstructured object (rather than text-substituting into the YAML template)
// keeps taint values safe from YAML special characters.
func applyInferenceWorkerScheduling(obj *unstructured.Unstructured,
	config *inferenceWorkloadConfig) error {

	components, found, err := unstructured.NestedSlice(obj.Object, "spec", "components")
	if err != nil || !found {
		return errors.New(errors.ErrCodeInternal, "spec.components not found in DynamoGraphDeployment")
	}

	// DRA wiring: bind the worker pod to the per-run ResourceClaimTemplate.
	claimBindings := []interface{}{map[string]interface{}{
		keyName:                     "gpu",
		"resourceClaimTemplateName": inferenceClaimTemplateName,
	}}
	containerClaimRefs := []interface{}{map[string]interface{}{
		keyName: "gpu",
	}}

	for i, compRaw := range components {
		component, ok := compRaw.(map[string]interface{})
		if !ok {
			continue
		}
		componentName, _, _ := unstructured.NestedString(component, "name")
		componentType, _, _ := unstructured.NestedString(component, "type")

		podTemplate, _, _ := unstructured.NestedMap(component, "podTemplate")
		if podTemplate == nil {
			podTemplate = map[string]interface{}{}
		}
		podSpec, _, _ := unstructured.NestedMap(podTemplate, "spec")
		if podSpec == nil {
			podSpec = map[string]interface{}{}
		}

		// Tolerations AND nodeSelector apply to every component so all pods
		// co-locate on the GPU node cohort. Co-location matters on clusters
		// whose GPU/system/CPU node groups live in separate network security
		// groups (e.g., EKS with per-nodegroup SGs) — splitting Frontend/EPP
		// onto a system node can silently break the validator→serving path via
		// cross-SG firewall drops even though every pod reports Ready.
		if len(config.gpuTolerations) > 0 {
			podSpec["tolerations"] = tolerationsToUnstructured(config.gpuTolerations)
		}
		if len(config.gpuNodeSelector) > 0 {
			ns := make(map[string]interface{}, len(config.gpuNodeSelector))
			for k, v := range config.gpuNodeSelector {
				ns[k] = v
			}
			podSpec["nodeSelector"] = ns
		}

		// Worker GPU wiring, mode-dispatched: DRA resourceClaims where
		// full-GPU DRA is usable (kai-scheduler rejects scalar GPU requests
		// on nodes bearing raw node-local GPU ResourceSlices), device-plugin limits everywhere
		// else — no gpu.nvidia.com DeviceClass required (#1327).
		if isInferenceGPUComponent(componentName, componentType) {
			if config.useDRAWorkerClaims() {
				podSpec["resourceClaims"] = claimBindings
				ensureMainContainerResourceClaims(podSpec, containerClaimRefs)
			} else {
				ensureMainContainerGPULimit(podSpec, inferenceWorkerGPUCount)
			}
		}

		// When the model-weights cache is enabled, mount the pre-populated PVC
		// read-only and point HF_HOME at it (offline) so the worker/frontend/EPP
		// load model metadata locally instead of re-downloading from Hugging Face.
		if modelCacheEnabled(config) {
			injectModelCacheMounts(podSpec)
		}

		if err := unstructured.SetNestedMap(podTemplate, podSpec, "spec"); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to set component podTemplate spec", err)
		}
		component["podTemplate"] = podTemplate
		components[i] = component
	}

	return unstructured.SetNestedSlice(obj.Object, components, "spec", "components")
}

func tolerationsToUnstructured(tolerations []v1.Toleration) []interface{} {
	tolList := make([]interface{}, 0, len(tolerations))
	for _, t := range tolerations {
		tolMap := map[string]interface{}{
			"operator": string(t.Operator),
		}
		if t.Key != "" {
			tolMap["key"] = t.Key
		}
		if t.Value != "" {
			tolMap["value"] = t.Value
		}
		if t.Effect != "" {
			tolMap["effect"] = string(t.Effect)
		}
		tolList = append(tolList, tolMap)
	}
	return tolList
}

func isInferenceGPUComponent(componentName, componentType string) bool {
	switch componentType {
	case "worker", "decode", "prefill":
		return true
	default:
		return componentName == "VllmDecodeWorker"
	}
}

// ensureMainContainerResourceClaims sets resources.claims on the pod spec's
// main container (DRA wiring mode), appending a bare named entry when the
// container is absent — the Dynamo operator merges its defaults into it.
// Sidecar/auxiliary containers are left untouched.
func ensureMainContainerResourceClaims(podSpec map[string]interface{}, claims []interface{}) {
	containers, _ := podSpec["containers"].([]interface{})
	if len(containers) == 0 {
		containers = []interface{}{map[string]interface{}{keyName: mainContainerName}}
	}
	mainIdx := -1
	for i, raw := range containers {
		container, ok := raw.(map[string]interface{})
		if ok && container[keyName] == mainContainerName {
			mainIdx = i
			break
		}
	}
	if mainIdx == -1 {
		mainIdx = len(containers)
		containers = append(containers, map[string]interface{}{keyName: mainContainerName})
	}

	container, ok := containers[mainIdx].(map[string]interface{})
	if !ok {
		container = map[string]interface{}{keyName: mainContainerName}
	}
	resources, _ := container["resources"].(map[string]interface{})
	if resources == nil {
		resources = map[string]interface{}{}
	}
	resources["claims"] = claims
	container["resources"] = resources
	containers[mainIdx] = container
	podSpec["containers"] = containers
}

// ensureMainContainerGPULimit sets resources.limits["nvidia.com/gpu"] on the
// pod spec's main container, appending a bare named entry when the container
// is absent (the Dynamo operator merges its defaults into it). Only the limit
// is set: for extended resources the API server defaults requests from limits
// and rejects requests != limits, so the limit alone is the canonical
// device-plugin GPU request. Sidecar/auxiliary containers are left untouched.
func ensureMainContainerGPULimit(podSpec map[string]interface{}, count int) {
	containers, _ := podSpec["containers"].([]interface{})
	if len(containers) == 0 {
		containers = []interface{}{map[string]interface{}{keyName: mainContainerName}}
	}
	mainIdx := -1
	for i, raw := range containers {
		container, ok := raw.(map[string]interface{})
		if ok && container[keyName] == mainContainerName {
			mainIdx = i
			break
		}
	}
	if mainIdx == -1 {
		mainIdx = len(containers)
		containers = append(containers, map[string]interface{}{keyName: mainContainerName})
	}

	container, ok := containers[mainIdx].(map[string]interface{})
	if !ok {
		container = map[string]interface{}{keyName: mainContainerName}
	}
	resources, _ := container["resources"].(map[string]interface{})
	if resources == nil {
		resources = map[string]interface{}{}
	}
	limits, _ := resources["limits"].(map[string]interface{})
	if limits == nil {
		limits = map[string]interface{}{}
	}
	// Quantity as a string — the canonical YAML/JSON form for resource
	// quantities; an int would also decode, but the string matches what a
	// hand-written manifest carries.
	limits[gpuResourceName] = strconv.Itoa(count)
	resources["limits"] = limits
	container["resources"] = resources
	containers[mainIdx] = container
	podSpec["containers"] = containers
}

// ensureHFTokenSecret provisions the optional Hugging Face token Secret in the
// benchmark namespace when the validator's environment carries HF_TOKEN (itself
// forwarded from the CLI, never the in-repo catalog). When unset it is a no-op
// and workers download anonymously — the unchanged default for smoke-test
// models. The token only matters to lift HF rate limits for large models pulled
// by many workers at once. Uses create-or-update so a re-used namespace from a
// prior run does not retain a stale token.
func ensureHFTokenSecret(ctx *validators.Context, namespace string) error {
	secrets := ctx.Clientset.CoreV1().Secrets(namespace)
	token := strings.TrimSpace(os.Getenv(envHFToken))

	// Bound the Secret API calls so a slow/wedged apiserver can't burn the
	// check's overall deadline during setup, before the workload is even deployed.
	opCtx, cancel := context.WithTimeout(ctx.Ctx, defaults.DiagnosticTimeout)
	defer cancel()

	// Read any existing secret first so an update carries the current
	// resourceVersion (an Update without it can be rejected by the apiserver)
	// and so an unset token can clear a stale one.
	existing, getErr := secrets.Get(opCtx, hfTokenSecretName, metav1.GetOptions{})
	if getErr != nil && !apierrors.IsNotFound(getErr) {
		return errors.Wrap(errors.ErrCodeInternal, "failed to read HF token secret", getErr)
	}
	exists := getErr == nil

	if token == "" {
		// Anonymous run: delete any leftover token so a reused per-run namespace
		// can't silently inject stale credentials via the workers' optional
		// secretKeyRefs.
		if exists {
			if err := secrets.Delete(opCtx, hfTokenSecretName, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return errors.Wrap(errors.ErrCodeInternal, "failed to delete stale HF token secret", err)
			}
			slog.Info("Cleared stale Hugging Face token secret (HF_TOKEN unset; anonymous downloads)", "namespace", namespace)
		}
		return nil
	}

	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: hfTokenSecretName, Namespace: namespace},
		Type:       v1.SecretTypeOpaque,
		StringData: map[string]string{hfTokenSecretKey: token},
	}
	if exists {
		secret.ResourceVersion = existing.ResourceVersion
		if _, err := secrets.Update(opCtx, secret, metav1.UpdateOptions{}); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to update HF token secret", err)
		}
		return nil
	}
	if _, err := secrets.Create(opCtx, secret, metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create HF token secret", err)
	}
	slog.Info("Provisioned Hugging Face token secret for model downloads", "namespace", namespace)
	return nil
}

// ensureNamespace creates a namespace if it doesn't exist, waiting for any
// in-flight deletion to complete first. When a prior run's cleanup deleted
// the namespace but Dynamo finalizers are still cascading through child
// resources, the namespace lingers in Terminating state — Create returns
// AlreadyExists, but subsequent resource creates inside it fail with
// "... forbidden: ... because it is being terminated". Waiting here until the
// prior Terminating instance is fully gone avoids that race.
func ensureNamespace(ctx *validators.Context, namespace string) error {
	nsCtx, cancel := context.WithTimeout(ctx.Ctx, defaults.InferenceNamespaceTerminationWait)
	defer cancel()

	clients := ctx.Clientset.CoreV1().Namespaces()

	existing, err := clients.Get(nsCtx, namespace, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		// Namespace doesn't exist — Create below will succeed.
	case err != nil:
		return errors.Wrap(errors.ErrCodeInternal, "failed to check namespace", err)
	case existing.DeletionTimestamp != nil:
		slog.Info("Namespace is terminating from a prior run; waiting for full deletion",
			"namespace", namespace)
		if waitErr := waitForNamespaceGone(nsCtx, clients, namespace); waitErr != nil {
			return waitErr
		}
	default:
		// Already exists and is usable — nothing to do.
		return nil
	}

	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	_, err = clients.Create(nsCtx, ns, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create namespace", err)
	}
	return nil
}

// waitForNamespaceGone watches the given namespace until it is removed.
// Includes a fast-path Get after the watch is established to avoid
// deadlocking if deletion completed between the caller's initial Get and
// this watch setup.
func waitForNamespaceGone(ctx context.Context, clients typedcorev1.NamespaceInterface, namespace string) error {
	watcher, err := clients.Watch(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + namespace,
	})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to watch namespace deletion", err)
	}
	defer watcher.Stop()

	if _, getErr := clients.Get(ctx, namespace, metav1.GetOptions{}); apierrors.IsNotFound(getErr) {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return errors.Wrap(errors.ErrCodeTimeout,
				"timed out waiting for namespace to be fully deleted", ctx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return errors.Wrap(errors.ErrCodeTimeout,
						"timed out waiting for namespace to be fully deleted", ctxErr)
				}
				// Watch channel closed without cancellation — apiserver
				// hiccups (rolling restart, LB drop) commonly cause this.
				// Re-Get before failing a healthy run: the namespace may have
				// been deleted during the closure window.
				if _, getErr := clients.Get(ctx, namespace, metav1.GetOptions{}); apierrors.IsNotFound(getErr) {
					return nil
				}
				return errors.New(errors.ErrCodeUnavailable,
					"namespace watch channel closed before deletion observed")
			}
			if event.Type == watch.Deleted {
				return nil
			}
		}
	}
}

// createOrUpdateFromTemplate parses a YAML template, optionally mutates the
// resulting unstructured object, and applies it via create-or-update
// semantics (Create, fall back to Update on AlreadyExists). This replaces
// the earlier delete-then-create pattern, which races against finalizers.
func createOrUpdateFromTemplate(ctx *validators.Context, gvr schema.GroupVersionResource,
	namespace, templatePath string, data map[string]string,
	mutate func(*unstructured.Unstructured) error) error {

	obj, err := parseYAMLTemplate(templatePath, data)
	if err != nil {
		return err
	}
	obj.SetNamespace(namespace)

	if mutate != nil {
		if mErr := mutate(obj); mErr != nil {
			return mErr
		}
	}

	applyCtx, cancel := context.WithTimeout(ctx.Ctx, defaults.DiagnosticTimeout)
	defer cancel()

	_, err = ctx.DynamicClient.Resource(gvr).Namespace(namespace).
		Create(applyCtx, obj, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create resource", err)
	}

	// Fetch existing to get resourceVersion, then update in-place. Using
	// Update (not Patch) matches the NCCL / shared pattern in this package.
	existing, getErr := ctx.DynamicClient.Resource(gvr).Namespace(namespace).
		Get(applyCtx, obj.GetName(), metav1.GetOptions{})
	if getErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to get existing resource for update", getErr)
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	if _, updateErr := ctx.DynamicClient.Resource(gvr).Namespace(namespace).
		Update(applyCtx, obj, metav1.UpdateOptions{}); updateErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to update resource", updateErr)
	}
	return nil
}

// waitForDynamoDeploymentReady watches the DynamoGraphDeployment until it
// reports state=successful. Fast-paths an immediate check before starting
// the watch so already-ready deployments return without allocating a watcher.
func waitForDynamoDeploymentReady(ctx *validators.Context, config *inferenceWorkloadConfig) error {
	slog.Info("Waiting for DynamoGraphDeployment to become ready...")

	readyTimeout, err := durationFromEnv(envWorkloadReadyTimeout, defaults.InferenceWorkloadReadyTimeout)
	if err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx.Ctx, readyTimeout)
	defer cancel()

	existing, err := ctx.DynamicClient.Resource(dynamoDeploymentGVR).
		Namespace(config.namespace).Get(waitCtx, inferenceDeploymentName, metav1.GetOptions{})
	switch {
	case err == nil:
		if isDynamoDeploymentReady(existing) {
			slog.Info("DynamoGraphDeployment is ready")
			return nil
		}
	case apierrors.IsNotFound(err):
		// Not yet created — fall through to watch. existing stays nil, so the
		// watch starts from an empty resourceVersion.
	default:
		// RBAC, auth, apiserver, or timeout errors: surface explicitly so the
		// real infrastructure problem isn't masked by a silent fallthrough
		// into Watch(). Without this, a Forbidden response here would send
		// the caller into the full InferenceWorkloadReadyTimeout window.
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to read DynamoGraphDeployment before watch", err)
	}

	watcher, err := ctx.DynamicClient.Resource(dynamoDeploymentGVR).
		Namespace(config.namespace).Watch(waitCtx, metav1.ListOptions{
		FieldSelector:   "metadata.name=" + inferenceDeploymentName,
		ResourceVersion: existingResourceVersion(existing),
	})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to watch DynamoGraphDeployment", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-waitCtx.Done():
			return errors.Wrap(errors.ErrCodeTimeout,
				"timed out waiting for DynamoGraphDeployment to become ready", waitCtx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				if ctxErr := waitCtx.Err(); ctxErr != nil {
					return errors.Wrap(errors.ErrCodeTimeout,
						"timed out waiting for DynamoGraphDeployment to become ready", ctxErr)
				}
				// Watch closed without cancellation — re-Get before declaring
				// failure so an apiserver hiccup doesn't fail a ready deployment.
				recheck, getErr := ctx.DynamicClient.Resource(dynamoDeploymentGVR).
					Namespace(config.namespace).Get(waitCtx, inferenceDeploymentName, metav1.GetOptions{})
				switch {
				case getErr == nil:
					if isDynamoDeploymentReady(recheck) {
						slog.Info("DynamoGraphDeployment is ready")
						return nil
					}
					return errors.New(errors.ErrCodeUnavailable,
						"DynamoGraphDeployment watch channel closed before ready state observed")
				case apierrors.IsNotFound(getErr):
					return errors.New(errors.ErrCodeUnavailable,
						"DynamoGraphDeployment watch channel closed before ready state observed")
				case errors.IsTransient(getErr):
					// The re-check itself raced the deadline — keep it transient.
					return errors.Wrap(errors.ErrCodeTimeout,
						"DynamoGraphDeployment watch closed and re-check timed out", getErr)
				default:
					// A real re-check failure (RBAC, apiserver) is deterministic —
					// surface it instead of masking it as "closed before observed".
					return errors.Wrap(errors.ErrCodeInternal,
						"DynamoGraphDeployment watch closed and re-check failed", getErr)
				}
			}
			obj, ok := event.Object.(*unstructured.Unstructured)
			if !ok {
				continue
			}
			if isDynamoDeploymentReady(obj) {
				slog.Info("DynamoGraphDeployment is ready")
				return nil
			}
			state, _, _ := unstructured.NestedString(obj.Object, "status", "state")
			slog.Debug("DynamoGraphDeployment not ready yet", "state", state)
		}
	}
}

// isDynamoDeploymentReady reports whether *every desired* component in the
// DynamoGraphDeployment has all of its replicas Ready — not just that the
// operator reported a top-level state of "successful". The benchmark pins one
// data-parallel worker per GPU; if it starts while some workers are still
// loading (common when model-cache reads stagger, e.g. 8 workers reading an 8B
// model concurrently from one RWO EBS cache), it measures an under-provisioned
// deployment and reports falsely low throughput / high TTFT. See #1181.
//
// Readiness is keyed off spec.components (the desired set), and each component's
// status.readyReplicas is compared against its spec replica count. This guards
// two failure modes that checking status.components alone misses: (1) the
// operator may populate status.components incrementally, so a desired component
// (e.g. the worker) can be entirely absent while the frontend already reports
// ready; (2) during scale-up status.replicas lags the spec count, so comparing
// ready against status.replicas can pass at, say, 6/8.
func isDynamoDeploymentReady(obj *unstructured.Unstructured) bool {
	if obj == nil {
		return false
	}
	state, found, _ := unstructured.NestedString(obj.Object, "status", "state")
	if !found || state != "successful" {
		return false
	}

	desired, found := desiredDynamoComponents(obj)
	if !found || len(desired) == 0 {
		return false
	}

	statusComponents, found, err := unstructured.NestedMap(obj.Object, "status", "components")
	if err != nil || !found {
		statusComponents, found, err = unstructured.NestedMap(obj.Object, "status", "services")
	}
	if err != nil || !found {
		return false
	}

	for name, dsvc := range desired {
		// Desired replica count from the spec. DGD components default to 1
		// replica when unset; a 0-replica component has nothing to await.
		want, wfound, werr := unstructured.NestedInt64(dsvc, "replicas")
		if werr != nil {
			// Present but wrong-typed replicas: fail closed rather than
			// silently defaulting to 1, which could pass the gate early in
			// the same under-provisioned class this guards against.
			return false
		}
		if !wfound {
			want = 1
		}
		if want < 1 {
			continue
		}

		// The desired component must be represented in status — not just the
		// subset the operator has populated so far.
		sraw, ok := statusComponents[name]
		if !ok {
			return false
		}
		ssvc, ok := sraw.(map[string]interface{})
		if !ok {
			return false
		}
		// readyReplicas is populated for Deployment/PodClique/LeaderWorkerSet;
		// PodCliqueScalingGroup reports availableReplicas instead. Compare
		// against the desired (spec) count so a scale-up window does not read
		// as ready.
		ready, rfound, err := unstructured.NestedInt64(ssvc, "readyReplicas")
		if err != nil || !rfound {
			ready, rfound, err = unstructured.NestedInt64(ssvc, "availableReplicas")
		}
		if err != nil || !rfound || ready < want {
			return false
		}
	}
	return true
}

func desiredDynamoComponents(obj *unstructured.Unstructured) (map[string]map[string]interface{}, bool) {
	components, found, err := unstructured.NestedSlice(obj.Object, "spec", "components")
	if err == nil && found {
		out := make(map[string]map[string]interface{}, len(components))
		for _, raw := range components {
			component, ok := raw.(map[string]interface{})
			if !ok {
				return nil, false
			}
			name, _, _ := unstructured.NestedString(component, "name")
			if name == "" {
				return nil, false
			}
			out[name] = component
		}
		return out, true
	}

	services, found, err := unstructured.NestedMap(obj.Object, "spec", "services")
	if err != nil || !found {
		return nil, false
	}
	out := make(map[string]map[string]interface{}, len(services))
	for name, raw := range services {
		service, ok := raw.(map[string]interface{})
		if !ok {
			return nil, false
		}
		out[name] = service
	}
	return out, true
}

// existingResourceVersion returns the ResourceVersion from an Unstructured
// object, or the empty string if the object is nil. Used to avoid re-delivery
// of events already consumed by the pre-watch Get.
func existingResourceVersion(obj *unstructured.Unstructured) string {
	if obj == nil {
		return ""
	}
	return obj.GetResourceVersion()
}

// resolveFrontendEndpoint looks up the Dynamo frontend Service in the given
// namespace and returns its cluster-internal URL. Falls back to the default
// port if the Service cannot be inspected.
func resolveFrontendEndpoint(ctx *validators.Context, namespace string) string {
	lookupCtx, cancel := context.WithTimeout(ctx.Ctx, defaults.DiagnosticTimeout)
	defer cancel()

	expectedSvc := inferenceDeploymentName + "-frontend"
	svc, err := ctx.Clientset.CoreV1().Services(namespace).Get(lookupCtx, expectedSvc, metav1.GetOptions{})
	if err != nil {
		slog.Debug("Frontend service lookup failed, using default port",
			"service", expectedSvc, "port", inferenceFrontendPort, "error", err)
		return fmt.Sprintf("http://%s.%s.svc:%d", expectedSvc, namespace, inferenceFrontendPort)
	}
	port := inferServicePort(*svc)
	return fmt.Sprintf("http://%s.%s.svc:%d", svc.Name, svc.Namespace, port)
}

func resolveInferenceEndpoint(ctx *validators.Context, config *inferenceWorkloadConfig) (string, error) {
	if config.routingMode == inferenceRoutingModeGatewayEPP {
		return resolveGatewayEndpoint(ctx)
	}
	return resolveFrontendEndpoint(ctx, config.namespace), nil
}

// resolveGatewayEndpoint returns the in-cluster URL for the AICR-managed
// inference gateway. The Service is created by the agentgateway controller from
// the Gateway resource, so fall back to the conventional name and port when it
// is not yet inspectable; waitForEndpointReady will surface a real timeout if
// the gateway never serves the route.
func resolveGatewayEndpoint(ctx *validators.Context) (string, error) {
	lookupCtx, cancel := context.WithTimeout(ctx.Ctx, defaults.DiagnosticTimeout)
	defer cancel()

	svcs, err := ctx.Clientset.CoreV1().Services(inferenceGatewayNamespace).List(lookupCtx, metav1.ListOptions{})
	if err != nil {
		slog.Debug("Inference gateway service lookup failed",
			"namespace", inferenceGatewayNamespace, "service", inferenceGatewayName, "port", inferenceGatewayPort, "error", err)
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to list inference gateway services", err)
	}

	var selected *v1.Service
	for i := range svcs.Items {
		svc := &svcs.Items[i]
		if svc.Name == inferenceGatewayName {
			selected = svc
			break
		}
		if selected == nil && isInferenceGatewayProxyServiceName(svc.Name) {
			selected = svc
		}
	}
	if selected == nil {
		slog.Debug("Inference gateway service not found, using default gateway endpoint",
			"namespace", inferenceGatewayNamespace, "service", inferenceGatewayName, "port", inferenceGatewayPort)
		return defaultGatewayEndpoint(), nil
	}

	port := inferServicePort(*selected)
	return fmt.Sprintf("http://%s.%s.svc:%d", selected.Name, selected.Namespace, port), nil
}

func defaultGatewayEndpoint() string {
	return fmt.Sprintf("http://%s.%s.svc:%d", inferenceGatewayName, inferenceGatewayNamespace, inferenceGatewayPort)
}

func isInferenceGatewayProxyServiceName(name string) bool {
	if !strings.Contains(name, inferenceGatewayName) {
		return false
	}
	for _, marker := range []string{"controller-manager", "webhook", "metrics"} {
		if strings.Contains(name, marker) {
			return false
		}
	}
	return true
}

// inferencePerfNoCleanup reports whether AICR_INFERENCE_PERF_NO_CLEANUP is set
// to a truthy value. When set, the inference-perf validator leaves the
// namespace, DGD, workers, frontend, and AIPerf Job in place after the run so a
// failed/anomalous run (e.g. serve-wait or generate hang) can be inspected
// post-mortem. Debug-only; the operator must clean up the namespace manually.
func inferencePerfNoCleanup() bool {
	b, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv("AICR_INFERENCE_PERF_NO_CLEANUP")))
	return b
}

// cleanupInferenceWorkload removes the deployed benchmark workload and its namespace.
// Safe to call even on partial failure — skips if deployedByUs is false.
func cleanupInferenceWorkload(ctx *validators.Context, config *inferenceWorkloadConfig) {
	if !config.deployedByUs {
		return
	}

	// Debug escape hatch: leave the namespace, DGD, workers, frontend, and
	// AIPerf Job in place for post-mortem inspection (e.g. serve-wait / generate
	// hangs). Set AICR_INFERENCE_PERF_NO_CLEANUP=1. Operator must delete the
	// namespace manually afterward.
	if inferencePerfNoCleanup() {
		slog.Warn("AICR_INFERENCE_PERF_NO_CLEANUP set — leaving workload in place for inspection",
			"namespace", config.namespace, "deployment", inferenceDeploymentName)
		return
	}

	slog.Info("Cleaning up inference benchmark workload...")

	cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
	defer cancel()

	// Delete DynamoGraphDeployment.
	err := ctx.DynamicClient.Resource(dynamoDeploymentGVR).
		Namespace(config.namespace).
		Delete(cleanupCtx, inferenceDeploymentName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("failed to delete DynamoGraphDeployment", "error", err)
	} else {
		slog.Info("Deleted DynamoGraphDeployment")
	}

	// Delete KAI Queue.
	err = ctx.DynamicClient.Resource(kaiQueueGVR).
		Namespace(config.namespace).
		Delete(cleanupCtx, inferenceQueueName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		slog.Debug("Failed to delete KAI Queue", "error", err)
	}

	// Delete namespace (cascades all remaining resources).
	err = ctx.Clientset.CoreV1().Namespaces().Delete(cleanupCtx, config.namespace, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("failed to delete namespace", "error", err)
	} else {
		slog.Info("Deleted namespace", "namespace", config.namespace)
	}
}

// inferServicePort returns port 8000 from the service if present, then a
// port named "http", then the first port; defaults to 8000 when the service
// exposes no ports.
func inferServicePort(svc v1.Service) int32 {
	for _, p := range svc.Spec.Ports {
		if p.Port == 8000 {
			return p.Port
		}
	}
	for _, p := range svc.Spec.Ports {
		if p.Name == "http" {
			return p.Port
		}
	}
	if len(svc.Spec.Ports) > 0 {
		return svc.Spec.Ports[0].Port
	}
	return 8000
}

// waitForEndpointReady polls the inference endpoint with a real chat-completion
// request until it produces a non-empty completion. This is stricter than a
// /health probe: Dynamo's frontend returns 200 from /health as soon as the HTTP
// server is up — before backend workers have registered or the model has
// finished loading. Hitting that window with AIPerf produces an "all requests
// completed, zero tokens" result that masquerades as a benchmark failure. A
// real inference probe is the only signal that's both necessary and sufficient
// for the endpoint to serve a benchmark workload.
func waitForEndpointReady(ctx context.Context, endpoint, model string) error {
	timeout, err := durationFromEnv(envHealthTimeout, defaults.InferenceHealthTimeout)
	if err != nil {
		return err
	}
	return waitForEndpointReadyWithInterval(ctx, endpoint, model, defaults.InferenceHealthPollInterval, timeout)
}

// waitForEndpointReadyWithInterval is the testable seam: production callers go
// through waitForEndpointReady (10 s poll, env-resolved timeout); tests pass a
// tighter interval and timeout so the success / timeout paths run in
// milliseconds.
func waitForEndpointReadyWithInterval(ctx context.Context, endpoint, model string, pollInterval, timeout time.Duration) error {
	chatURL := endpoint + "/v1/chat/completions"
	slog.Info("Waiting for inference endpoint to serve requests", "url", chatURL, "model", model, "timeout", timeout)

	client := &http.Client{Timeout: defaults.InferenceEndpointProbeTimeout}

	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bodyTmpl := `{"model":%q,"messages":[{"role":"user","content":"hi"}],"max_tokens":4}`
	body := fmt.Sprintf(bodyTmpl, model)

	for {
		req, err := http.NewRequestWithContext(pollCtx, http.MethodPost, chatURL, strings.NewReader(body))
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to create inference probe request", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req) //nolint:gosec // G107 -- URL constructed from in-cluster K8s service discovery
		if err == nil {
			respBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, defaults.InferenceProbeBodyLimit+1))
			resp.Body.Close()
			switch {
			case readErr != nil:
				slog.Debug("Inference probe read failed", "error", readErr)
			case int64(len(respBytes)) > defaults.InferenceProbeBodyLimit:
				slog.Debug("Inference probe response exceeded limit", "limit", defaults.InferenceProbeBodyLimit)
			case resp.StatusCode != http.StatusOK:
				slog.Debug("Inference probe non-200", "status", resp.StatusCode)
			default:
				var probe struct {
					Choices []struct {
						Message struct {
							Content string `json:"content"`
						} `json:"message"`
					} `json:"choices"`
				}
				ok := json.Unmarshal(respBytes, &probe) == nil &&
					len(probe.Choices) > 0 &&
					probe.Choices[0].Message.Content != ""
				if ok {
					slog.Info("Inference endpoint is serving requests")
					return nil
				}
				slog.Debug("Inference probe response missing completion content")
			}
		} else {
			slog.Debug("Inference probe request failed", "error", err)
		}

		select {
		case <-pollCtx.Done():
			return errors.Wrap(errors.ErrCodeTimeout, "timed out waiting for inference endpoint to serve requests", pollCtx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// runAIPerfJob creates and runs an AIPerf benchmark Job against the inference
// endpoint. jobName must be unique per run (see buildInferenceConfig, which
// derives it from AICR_RUN_ID) so two concurrent validate invocations can't
// delete each other's Job, wait on the wrong pod, or collect the wrong logs.
func runAIPerfJob(ctx *validators.Context, endpoint, model string, concurrency int, jobName string) (string, error) {
	// Propagate image pull secrets from the outer validator pod so the inner
	// aiperf benchmark pod can pull from the same authenticated registry.
	// Without this, setups using AICR_VALIDATOR_IMAGE_REGISTRY pointing at a
	// private mirror start the outer pod fine (the Deployer attaches pull
	// secrets via --image-pull-secret) but the inner aiperf pod hangs in
	// ImagePullBackOff.
	pullSecrets := getOwnPullSecrets(ctx)

	job, params, err := buildAIPerfJob(ctx.Namespace, jobName, endpoint, model, concurrency, pullSecrets)
	if err != nil {
		return "", err
	}

	// Log after building so the reported counts are the ones actually baked into
	// the benchmark script (honoring concurrency scaling and AICR_INFERENCE_PERF_*
	// overrides), not the bare constant defaults.
	slog.Info("Running AIPerf benchmark",
		"endpoint", endpoint,
		"model", model,
		"concurrency", concurrency,
		"requests", params.requestCount,
		"warmup", params.warmupCount,
		"job", jobName)

	// Because jobName is per-run-unique, there is no shared-state pre-clean to
	// do; only the deferred cleanup runs on exit.

	createCtx, cancel := context.WithTimeout(ctx.Ctx, defaults.K8sJobCreationTimeout)
	defer cancel()

	if _, err = ctx.Clientset.BatchV1().Jobs(ctx.Namespace).Create(createCtx, job, metav1.CreateOptions{}); err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to create AIPerf Job", err)
	}
	defer cleanupAIPerfJob(ctx, jobName)

	podHelper := &helper.PodLifecycle{
		ClientSet: ctx.Clientset,
		Namespace: ctx.Namespace,
	}

	pod, err := waitForPodByLabelSelector(
		ctx.Ctx,
		ctx.Clientset,
		ctx.Namespace,
		fmt.Sprintf("job-name=%s", jobName),
		defaults.InferencePerfPodTimeout,
	)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeTimeout, "failed to find AIPerf pod", err)
	}

	slog.Info("Found AIPerf pod", "name", pod.Name)

	err = podHelper.WaitForPodSuccess(ctx.Ctx, pod, defaults.InferencePerfJobTimeout)
	if err != nil {
		logs, _ := podHelper.GetPodLogs(ctx.Ctx, pod)
		return logs, errors.Wrap(errors.ErrCodeInternal, "AIPerf job failed", err)
	}

	logs, err := podHelper.GetPodLogs(ctx.Ctx, pod)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to get AIPerf logs", err)
	}

	return logs, nil
}

// resolveAiperfImage returns the AIPerf benchmark image, applying the same
// :latest tag pinning and registry override that catalog.Load applies to
// top-level catalog entries. The CLI forwards its version via AICR_CLI_VERSION
// and any AICR_VALIDATOR_IMAGE_REGISTRY override through the Job env, so
// mirrored/private-registry deployments and release-version pinning reach
// this inner workload too.
func resolveAiperfImage() string {
	return catalog.ResolveImage(aiperfBaseImage, os.Getenv("AICR_CLI_VERSION"), os.Getenv("AICR_CLI_COMMIT"))
}

// getOwnPullSecrets returns the imagePullSecrets attached to the pod this
// validator is running in, so they can be propagated to the inner aiperf
// benchmark Job. The pod name comes from HOSTNAME (Kubernetes sets this to
// the pod name by default). On any lookup failure the function returns nil
// — the inner Job simply runs without pull secrets, which is correct for
// public registries and a diagnostic no-op for private ones (the resulting
// ImagePullBackOff surfaces the missing secret clearly).
func getOwnPullSecrets(ctx *validators.Context) []v1.LocalObjectReference {
	podName := os.Getenv("HOSTNAME")
	if podName == "" {
		return nil
	}
	getCtx, cancel := context.WithTimeout(ctx.Ctx, defaults.DiagnosticTimeout)
	defer cancel()
	pod, err := ctx.Clientset.CoreV1().Pods(ctx.Namespace).Get(getCtx, podName, metav1.GetOptions{})
	if err != nil {
		slog.Debug("Could not look up own pod to propagate image pull secrets",
			"pod", podName, "namespace", ctx.Namespace, "error", err)
		return nil
	}
	if len(pod.Spec.ImagePullSecrets) == 0 {
		return nil
	}
	out := make([]v1.LocalObjectReference, len(pod.Spec.ImagePullSecrets))
	copy(out, pod.Spec.ImagePullSecrets)
	return out
}

// resolveRouterMode returns the Dynamo frontend routing strategy
// (DYN_ROUTER_MODE) for the benchmark workload: AICR_INFERENCE_PERF_ROUTER_MODE
// when set (trimmed), else dynRouterModeDefault. The value is validated
// against the Dynamo frontend enum (dynRouterModes) and fails closed with
// ErrCodeInvalidRequest on anything else — an unknown mode would otherwise
// surface as an opaque frontend failure minutes into the run. The value is
// CONSUMED only on the dynamo-router path — the gateway-epp template carries
// no ${ROUTER_MODE} placeholder (worker sidecars run in direct mode there) —
// but this resolver also runs in upfront validation (validatePerfTuningEnvs)
// in either mode, so a set-but-invalid value fails closed even under
// gateway-epp (see envRouterMode for why validation is not mode-gated).
func resolveRouterMode() (string, error) {
	raw := strings.TrimSpace(os.Getenv(envRouterMode))
	if raw == "" {
		return dynRouterModeDefault, nil
	}
	if !dynRouterModeSet[raw] {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid %s %q: must be one of %s", envRouterMode, raw, strings.Join(dynRouterModes, ", ")))
	}
	return raw, nil
}

// resolveInferenceModel returns the Hugging Face model ID used for the
// benchmark. Override via AICR_INFERENCE_PERF_MODEL to characterize a larger
// model (e.g., Qwen/Qwen3-32B) without rebuilding the validator image; unset or
// empty falls back to the default model (inferenceModel, Qwen/Qwen3-8B). A
// non-existent model ID surfaces as a deploy / endpoint-ready timeout
// (fail-closed), never a silent pass.
func resolveInferenceModel() string {
	if m := strings.TrimSpace(os.Getenv(envModel)); m != "" {
		return m
	}
	return inferenceModel
}

// modelIDPattern matches a Hugging Face model ID — an optional org prefix and a
// name — using only characters HF repo IDs allow. It excludes quotes, colons,
// whitespace, and other YAML/shell metacharacters so a recipe/env-supplied
// model can't break the DynamoGraphDeployment YAML (where it is interpolated as
// `${MODEL}`) or be unsafe.
var modelIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// validateModelID fails closed with ErrCodeInvalidRequest when the resolved
// model ID contains characters outside the safe HF repo-ID set, surfacing a
// clear error up front instead of an opaque YAML parse failure at deploy.
func validateModelID(model string) error {
	if !modelIDPattern.MatchString(model) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid inference model %q: must be a Hugging Face model ID (characters %s)", model, "[A-Za-z0-9._/-]"))
	}
	return nil
}

// resolveModel returns the HF model ID with precedence recipe > catalog env >
// compiled default. A per-accelerator overlay sets it via the
// `inference-model` performance constraint; absent (or blank) there, it falls
// back to resolveInferenceModel (AICR_INFERENCE_PERF_MODEL, then inferenceModel).
func resolveModel(ctx *validators.Context) string {
	if c, ok := findPerformanceConstraint(ctx, perfConstraintModel); ok {
		if m := strings.TrimSpace(c.Value); m != "" {
			return m
		}
	}
	return resolveInferenceModel()
}

// resolveRoutingMode returns where routing decisions are made for the
// benchmark workload. The default `dynamo-router` mode keeps routing in the
// Dynamo frontend, whose strategy defaults to load-aware least-loaded routing
// (DYN_ROUTER_MODE, overridable via AICR_INFERENCE_PERF_ROUTER_MODE — see
// resolveRouterMode, the deployment template, and issues #1197/#1374).
// `gateway-epp` switches to Gateway API Inference Extension: EPP
// performs KV-aware endpoint selection and worker frontend sidecars run in
// direct mode so they honor EPP's routing headers. The sidecars do not relay
// local vLLM ZMQ KV events onto NATS; that relay is handled by the worker
// runtime.
func resolveRoutingMode(ctx *validators.Context) (inferenceRoutingMode, error) {
	if c, ok := findPerformanceConstraint(ctx, perfConstraintRoutingMode); ok {
		raw := strings.TrimSpace(c.Value)
		if raw == "" {
			return inferenceRoutingModeDynamoRouter, nil
		}
		switch inferenceRoutingMode(raw) {
		case inferenceRoutingModeDynamoRouter:
			return inferenceRoutingModeDynamoRouter, nil
		case inferenceRoutingModeGatewayEPP:
			return inferenceRoutingModeGatewayEPP, nil
		default:
			return "", errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid %s=%q: must be %q or %q",
					perfConstraintRoutingMode, raw, inferenceRoutingModeDynamoRouter, inferenceRoutingModeGatewayEPP))
		}
	}
	return inferenceRoutingModeDynamoRouter, nil
}

// resolveConcurrencyPerGPU returns the per-GPU concurrency with precedence
// recipe > catalog env > compiled default. A per-accelerator overlay sets it
// via the `inference-concurrency-per-gpu` performance constraint (a bare
// positive integer); absent (or blank) there, it falls back to the
// AICR_INFERENCE_PERF_CONCURRENCY_PER_GPU env knob, then aiperfConcurrencyPerGPU.
// A non-positive / non-integer recipe value fails closed with
// ErrCodeInvalidRequest so a typo aborts the check rather than silently
// benchmarking under a value the operator never set.
func resolveConcurrencyPerGPU(ctx *validators.Context) (int, error) {
	if c, ok := findPerformanceConstraint(ctx, perfConstraintConcurrency); ok {
		if raw := strings.TrimSpace(c.Value); raw != "" {
			v, err := strconv.Atoi(raw)
			if err != nil || v <= 0 {
				return 0, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("invalid %s=%q: must be a positive integer", perfConstraintConcurrency, raw))
			}
			return v, nil
		}
	}
	return intFromEnv(envConcurrencyPerGPU, aiperfConcurrencyPerGPU)
}

// durationFromEnv reads a Go duration string (e.g. "30m") from the named env
// var, falling back to def when unset. A malformed or non-positive value aborts
// the check with ErrCodeInvalidRequest — same fail-closed contract as
// intFromEnv, so a typo can't silently run under the default.
func durationFromEnv(name string, def time.Duration) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid %s=%q: must be a positive Go duration (e.g. 30m)", name, raw))
	}
	return d, nil
}

// intFromEnv reads a positive-integer tuning knob from the named env var. It
// returns def when the var is unset, the parsed value when it is a positive
// integer, and an ErrCodeInvalidRequest error when it is set but not a positive
// integer. These knobs change the benchmark methodology and feed a pass/fail
// gate, so a typo must abort the run rather than silently fall back to a default
// and ship a result the operator never configured (see validatePerfTuningEnvs,
// which surfaces the error up front before any workload is deployed).
func intFromEnv(name string, def int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return 0, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid %s=%q: must be a positive integer", name, raw))
	}
	return v, nil
}

// validatePerfTuningEnvs fails closed if any AICR_INFERENCE_PERF_* knob is set
// to an invalid value (non-positive integer, or non-positive/malformed duration
// for the timeout knob), returning ErrCodeInvalidRequest so a catalog/env typo
// aborts the check up front — before the (multi-minute) benchmark workload is
// deployed — instead of silently benchmarking under defaults and reporting a
// pass/fail the operator did not ask for.
func validatePerfTuningEnvs() error {
	for _, name := range []string{
		envConcurrencyPerGPU, envWarmupPerConcurrency, envMinRequests,
		envRequestsPerConcurrency, envInputTokensMean, envOutputTokensMean,
	} {
		if _, err := intFromEnv(name, 1); err != nil {
			return err
		}
	}
	if _, err := durationFromEnv(envWorkloadReadyTimeout, defaults.InferenceWorkloadReadyTimeout); err != nil {
		return err
	}
	if _, err := durationFromEnv(envHealthTimeout, defaults.InferenceHealthTimeout); err != nil {
		return err
	}
	if _, _, err := parseModelCacheSize(os.Getenv(envModelCacheSize)); err != nil {
		return err
	}
	if _, err := resolveRouterMode(); err != nil {
		return err
	}
	return nil
}

// aiperfRunParams are the resolved (env-overridable) per-run AIPerf counts that
// buildAIPerfJob baked into the benchmark script. It returns them so the caller
// logs the values actually sent to aiperf rather than the bare constant
// defaults, which diverge once concurrency scaling or AICR_INFERENCE_PERF_*
// overrides take effect.
type aiperfRunParams struct {
	requestCount int
	warmupCount  int
}

// buildAIPerfJob constructs the Kubernetes Job spec for running AIPerf.
// The image (aiperfBaseImage) has aiperf pre-installed at build time — no pip
// install at runtime. The script wraps aiperf invocation in sentinel markers
// so parseAIPerfOutput can locate the JSON unambiguously. Diagnostic output
// (aiperf progress, warnings) is kept in the pod logs — silencing it made
// benchmark failures undiagnosable.
//
// Command overrides the image ENTRYPOINT (["aiperf"]) with a shell so we can
// chain aiperf + echo + cat for sentinel framing. /bin/sh is POSIX-sufficient
// for everything in the script (set -e, line continuation, echo, cat) and is
// present in the python:3.12-slim base image, avoiding a bash dependency.
func buildAIPerfJob(namespace, jobName, endpoint, model string, concurrency int, pullSecrets []v1.LocalObjectReference) (*batchv1.Job, aiperfRunParams, error) {
	// AIPerf requires request_count >= concurrency. Scale the measured request
	// count with concurrency so larger GPU counts still get a multi-wave
	// steady-state window, with a fixed floor for small nodes. Tuning-env reads
	// are pre-validated by validatePerfTuningEnvs; the error returns here are a
	// defensive fail-closed against a malformed knob.
	minRequests, err := intFromEnv(envMinRequests, aiperfMinRequests)
	if err != nil {
		return nil, aiperfRunParams{}, err
	}
	requestsPerConcurrency, err := intFromEnv(envRequestsPerConcurrency, aiperfRequestsPerConcurrency)
	if err != nil {
		return nil, aiperfRunParams{}, err
	}
	requestCount := minRequests
	if scaled := concurrency * requestsPerConcurrency; scaled > requestCount {
		requestCount = scaled
	}
	// Warmup primes vLLM (CUDA graph capture / JIT) before measurement so the
	// one-time compile cost does not inflate p99 TTFT. Excluded from stats.
	warmupPerConcurrency, err := intFromEnv(envWarmupPerConcurrency, aiperfWarmupPerConcurrency)
	if err != nil {
		return nil, aiperfRunParams{}, err
	}
	warmupCount := concurrency * warmupPerConcurrency
	inputTokensMean, err := intFromEnv(envInputTokensMean, aiperfInputTokensMean)
	if err != nil {
		return nil, aiperfRunParams{}, err
	}
	outputTokensMean, err := intFromEnv(envOutputTokensMean, aiperfOutputTokensMean)
	if err != nil {
		return nil, aiperfRunParams{}, err
	}
	// Resolve once so Image and ImagePullPolicy can't drift if env vars
	// were mutated between two calls (matters in tests; cheap in prod).
	aiperfImage := resolveAiperfImage()

	// The model is passed via the AICR_MODEL container env var and referenced as
	// "$AICR_MODEL", not interpolated into the script text. A recipe /
	// AICR_INFERENCE_PERF_MODEL value with shell metacharacters (e.g. $(...))
	// would otherwise be command-substituted by /bin/sh -c even inside double
	// quotes; "$AICR_MODEL" expands to the literal value without re-scanning it.
	script := fmt.Sprintf(`set -e
aiperf profile "$AICR_MODEL" \
  --url %s \
  --endpoint-type chat \
  --streaming \
  --concurrency %d \
  --request-count %d \
  --warmup-request-count %d \
  --prompt-input-tokens-mean %d \
  --prompt-input-tokens-stddev 0 \
  --prompt-output-tokens-mean %d \
  --prompt-output-tokens-stddev 0 \
  --num-dataset-entries %d \
  --random-seed %d \
  --extra-inputs temperature:0 \
  --output-artifact-dir %s \
  --export-level summary
echo '%s'
cat %s/profile_export_aiperf.json
echo '%s'`,
		endpoint,
		concurrency, requestCount, warmupCount,
		inputTokensMean, outputTokensMean,
		aiperfNumDatasetEntries, aiperfRandomSeed,
		aiperfArtifactDir,
		aiperfResultSentinel,
		aiperfArtifactDir,
		aiperfResultSentinel)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(0)),
			TTLSecondsAfterFinished: ptr.To(int32(3600)),
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyNever,
					// Tolerate all taints — AIPerf is a CPU-only load generator
					// that can run on any node in the cluster.
					Tolerations: []v1.Toleration{
						{Operator: v1.TolerationOpExists},
					},
					// Propagated from the outer validator pod so the inner
					// aiperf pod can pull from the same private/mirrored
					// registry when AICR_VALIDATOR_IMAGE_REGISTRY points
					// at one. Empty slice is safe: no public-registry change.
					ImagePullSecrets: pullSecrets,
					Containers: []v1.Container{
						{
							Name:  "aiperf",
							Image: aiperfImage,
							// Apply the same pull policy the outer validator
							// Job uses (validatorv1.ImagePullPolicy handles the
							// AICR_VALIDATOR_IMAGE_TAG override and digest
							// pins) so a mutable override tag on the CLI
							// doesn't let the aiperf pod serve a stale
							// cached image while the outer validator pulls
							// the current one. Leaving this unset would
							// fall back to Kubernetes' default (Always
							// only for :latest), which is insufficient for
							// `:edge`, `:main`, and similar rolling tags
							// on-push.yaml recreates on every merge.
							ImagePullPolicy: validatorv1.ImagePullPolicy(aiperfImage, os.Getenv("AICR_VALIDATOR_IMAGE_TAG")),
							// Model passed as env and referenced as "$AICR_MODEL"
							// in the script so a value with shell metacharacters
							// can't be command-substituted (see script above).
							Env:     []v1.EnvVar{{Name: "AICR_MODEL", Value: model}},
							Command: []string{shellBin, "-c"},
							Args:    []string{script},
						},
					},
				},
			},
		},
	}, aiperfRunParams{requestCount: requestCount, warmupCount: warmupCount}, nil
}

// cleanupAIPerfJob removes the AIPerf Job (if it exists) and waits for
// actual deletion. Synchronous wait prevents subsequent Create calls from
// racing against an in-flight foreground deletion and hitting AlreadyExists.
func cleanupAIPerfJob(ctx *validators.Context, jobName string) {
	if inferencePerfNoCleanup() {
		slog.Warn("AICR_INFERENCE_PERF_NO_CLEANUP set — leaving AIPerf Job in place", "job", jobName)
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
	defer cancel()

	jobs := ctx.Clientset.BatchV1().Jobs(ctx.Namespace)

	propagation := metav1.DeletePropagationForeground
	err := jobs.Delete(cleanupCtx, jobName, metav1.DeleteOptions{
		PropagationPolicy: &propagation,
	})
	if apierrors.IsNotFound(err) {
		return
	}
	if err != nil {
		slog.Debug("Failed to delete AIPerf job", "error", err)
		return
	}

	// Watch for the actual Deleted event so the next Create sees a clean slate.
	watcher, err := jobs.Watch(cleanupCtx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + jobName,
	})
	if err != nil {
		slog.Debug("failed to watch AIPerf job deletion", "error", err)
		return
	}
	defer watcher.Stop()

	// Fast-path: between Delete and Watch setup, the Job may have already been
	// fully removed. Confirm with a Get so we don't block indefinitely waiting
	// for an event that will never come.
	if _, getErr := jobs.Get(cleanupCtx, jobName, metav1.GetOptions{}); apierrors.IsNotFound(getErr) {
		return
	}

	for {
		select {
		case <-cleanupCtx.Done():
			return
		case event, ok := <-watcher.ResultChan():
			if !ok {
				return
			}
			if event.Type == watch.Deleted {
				return
			}
		}
	}
}

// parseAIPerfOutput extracts throughput and TTFT p99 from AIPerf JSON output.
// Looks for two sentinel markers (aiperfResultSentinel) emitted by the Job
// script and parses the JSON between them. Falling back on any brace-based
// slice would be fragile; the sentinel makes parsing robust to pip noise,
// aiperf progress output, and future log additions.
func parseAIPerfOutput(logs string) (*inferenceResult, error) {
	start := strings.Index(logs, aiperfResultSentinel)
	if start < 0 {
		return nil, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("AIPerf result sentinel %q not found in pod logs (length=%d); benchmark likely failed",
				aiperfResultSentinel, len(logs)))
	}
	start += len(aiperfResultSentinel)
	end := strings.Index(logs[start:], aiperfResultSentinel)
	if end < 0 {
		return nil, errors.New(errors.ErrCodeInternal,
			"AIPerf result end sentinel not found in pod logs; benchmark may have been truncated")
	}
	jsonBlob := strings.TrimSpace(logs[start : start+end])

	var output aiperfOutput
	if err := json.Unmarshal([]byte(jsonBlob), &output); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse AIPerf JSON output", err)
	}

	return &inferenceResult{
		throughput: output.OutputTokenThroughput.Avg,
		ttftP99Ms:  output.TimeToFirstToken.P99,
		status:     "ok",
	}, nil
}
