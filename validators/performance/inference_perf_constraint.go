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
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/catalog"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
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

	// inferenceModel is the small model used for performance validation.
	// Qwen3-0.6B is the default in all Dynamo deploy templates — canonical smoke test model.
	inferenceModel = "Qwen/Qwen3-0.6B"

	// aiperfConcurrencyPerGPU is the number of concurrent requests per GPU.
	// 16 per GPU saturates without overwhelming, based on empirical testing.
	aiperfConcurrencyPerGPU = 16

	// aiperfMinRequests is the minimum total number of requests to send.
	// Actual request count is max(aiperfMinRequests, concurrency * 2) to ensure
	// request_count >= concurrency (AIPerf requirement).
	aiperfMinRequests = 200

	// aiperfInputTokensMean is the mean number of input tokens per request.
	aiperfInputTokensMean = 128

	// aiperfOutputTokensMean is the mean number of output tokens per request.
	aiperfOutputTokensMean = 128

	// aiperfArtifactDir is where AIPerf writes benchmark result files.
	aiperfArtifactDir = "/tmp/aiperf"

	// inferenceDeploymentName is the DynamoGraphDeployment name for the benchmark
	// workload. Passed to the template via ${DEPLOYMENT_NAME}.
	inferenceDeploymentName = "aicr-inference-perf"

	// inferenceQueueName is the KAI Queue name for the benchmark workload.
	// Passed to the template via ${QUEUE_NAME}.
	inferenceQueueName = "aicr-inference-perf"

	// inferenceClaimTemplateName is the DRA ResourceClaimTemplate name used to
	// allocate one GPU per worker pod. Passed to the template via ${CLAIM_TEMPLATE_NAME}.
	inferenceClaimTemplateName = "aicr-inference-gpu-claim"

	// inferenceFrontendPort is the port exposed by the Dynamo frontend service
	// (contract from the dynamo-frontend chart). Used in the deploy path to
	// construct the benchmark endpoint before the Service object exists.
	inferenceFrontendPort int32 = 8000

	// inferenceWorkloadNamespacePrefix is the base for the per-run benchmark
	// namespace. Each run suffixes this with its unique run ID (from the
	// AICR_RUN_ID env var the Deployer injects, or a random suffix for
	// standalone invocations) so concurrent validate invocations never share
	// a namespace and can't tear down each other's resources mid-benchmark.
	inferenceWorkloadNamespacePrefix = "aicr-inference-perf"

	// gpuDRADriverName is the NVIDIA GPU DRA driver identifier, used to filter
	// ResourceClaim allocation results when computing in-use GPU count per
	// node. Matches the `driver` field the NVIDIA DRA driver stamps on every
	// DeviceRequestAllocationResult it produces.
	gpuDRADriverName = "gpu.nvidia.com"
)

// GVRs for Dynamo and KAI Scheduler CRDs.
var (
	dynamoDeploymentGVR = schema.GroupVersionResource{
		Group:    "nvidia.com",
		Version:  "v1alpha1",
		Resource: "dynamographdeployments",
	}

	kaiQueueGVR = schema.GroupVersionResource{
		Group:    "scheduling.run.ai",
		Version:  "v2",
		Resource: "queues",
	}
)

// inferenceResult holds parsed benchmark results from AIPerf output.
type inferenceResult struct {
	throughput float64 // output tokens/sec
	ttftP99Ms  float64 // time to first token p99, milliseconds
	status     string  // "ok" or "skipped - reason"
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
	runID           string // unique per run; suffix for namespace + aiperfJobName
	gpuCount        int
	gpuCountPerNode int
	concurrency     int
	gpuNodeSelector map[string]string
	gpuTolerations  []v1.Toleration
	namespace       string // per-run; derived from runID
	aiperfJobName   string // per-run; derived from runID
	deployedByUs    bool   // true if we (or a prior run we own) created the workload
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
	config, err := buildInferenceConfig(ctx)
	if err != nil {
		return nil, err
	}

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

	// Look up the frontend service to get the actual port rather than
	// assuming 8000. The Service exists once waitForDynamoDeploymentReady
	// returns, so this is safe here.
	endpoint := resolveFrontendEndpoint(ctx, config.namespace)

	slog.Info("Using inference endpoint", "endpoint", endpoint, "concurrency", config.concurrency)

	// Wait for the endpoint to be healthy. Callee returns ErrCodeTimeout on
	// deadline exhaustion, ErrCodeInternal on request-construction errors;
	// both classifications are lost if we rewrap here.
	if healthErr := waitForEndpointHealth(ctx.Ctx, endpoint); healthErr != nil {
		return nil, healthErr
	}

	// Run AIPerf benchmark. On failure, surface the captured pod logs so the
	// CTRF report contains enough signal to diagnose (pip install errors,
	// aiperf CLI failures, connection refused, etc.). Without this, the
	// error chain alone ("pod failed") is unactionable.
	logs, err := runAIPerfJob(ctx, endpoint, config.concurrency, config.aiperfJobName)
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

	slog.Info("Inference benchmark results",
		"throughput_tok/s", result.throughput,
		"ttft_p99_ms", result.ttftP99Ms)

	return result, nil
}

// hasDynamoPlatform checks if dynamo-platform is in the recipe ComponentRefs.
func hasDynamoPlatform(ctx *validators.Context) bool {
	if ctx.Recipe == nil {
		return false
	}
	for _, ref := range ctx.Recipe.ComponentRefs {
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
		Group:    "apiextensions.k8s.io",
		Version:  "v1",
		Resource: "customresourcedefinitions",
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
func buildInferenceConfig(ctx *validators.Context) (*inferenceWorkloadConfig, error) {
	slog.Info("Analyzing GPU node configuration...")

	gpuNodes, err := helper.FindSchedulableGpuNodes(ctx.Ctx, ctx.Clientset)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to find GPU nodes", err)
	}
	if len(gpuNodes) == 0 {
		return nil, errors.New(errors.ErrCodeInternal, "no schedulable GPU nodes found")
	}
	slog.Info("Found GPU nodes", "count", len(gpuNodes))

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

	// Enumerate existing DRA ResourceClaim allocations so we can subtract
	// per-node in-use GPUs from the allocatable count. Status.Allocatable
	// ("nvidia.com/gpu") reflects the device-plugin view and does NOT shrink
	// when the DRA driver allocates devices to another workload — so on a
	// shared cluster the "first candidate with full allocatable count" can
	// already be saturated, leaving the benchmark pending until timeout.
	// We pick the candidate with the most free GPUs and size the workload
	// to that count.
	usedByNode := countUsedGPUsByNode(ctx.Ctx, ctx.Clientset)

	chosen, gpuCountPerNode, freeGPUs := pickCandidateWithMostFreeGPUs(candidates, usedByNode)
	if freeGPUs <= 0 {
		return nil, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("no candidate GPU node has free GPUs — all %d matched node(s) are saturated by existing DRA ResourceClaim allocations; free GPUs or pass --node-selector kubernetes.io/hostname=<empty-node>",
				len(candidates)))
	}
	if gpuCountPerNode == 0 {
		return nil, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("candidate GPU node %q reports 0 nvidia.com/gpu allocatable", chosen.Name))
	}
	gpuCount := freeGPUs
	if freeGPUs < gpuCountPerNode {
		slog.Info("Candidate node has GPUs already allocated via DRA; sizing benchmark to free GPUs only",
			"node", chosen.Name, "allocatable", gpuCountPerNode, "inUse", gpuCountPerNode-freeGPUs, "free", freeGPUs)
	}

	runID := deriveRunID()
	config := &inferenceWorkloadConfig{
		runID:           runID,
		gpuCount:        gpuCount,
		gpuCountPerNode: gpuCountPerNode,
		concurrency:     aiperfConcurrencyPerGPU * gpuCount,
		namespace:       fmt.Sprintf("%s-%s", inferenceWorkloadNamespacePrefix, runID),
		aiperfJobName:   fmt.Sprintf("%s-%s", aiperfJobNamePrefix, runID),
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

// countUsedGPUsByNode returns a map of nodeName → in-use GPU count derived
// from existing DRA ResourceClaim allocations with driver == gpuDRADriverName
// ("gpu.nvidia.com"). The NVIDIA DRA driver sets each DeviceRequestAllocation
// Result.Pool to the host node name, so the pool key is the node.
//
// Returns an empty map on any lookup failure (DRA API not enabled, RBAC denied,
// timeout). The caller then falls back to allocatable-only sizing — which is
// safe on clusters without DRA-allocating workloads, and is the behavior
// before this helper was introduced.
func countUsedGPUsByNode(ctx context.Context, clientset kubernetes.Interface) map[string]int {
	listCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	claims, err := clientset.ResourceV1().ResourceClaims("").List(listCtx, metav1.ListOptions{})
	if err != nil {
		slog.Debug("DRA ResourceClaim list failed; falling back to allocatable-only sizing",
			"error", err)
		return map[string]int{}
	}

	used := make(map[string]int)
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Status.Allocation == nil {
			continue
		}
		for _, r := range claim.Status.Allocation.Devices.Results {
			if r.Driver != gpuDRADriverName {
				continue
			}
			used[r.Pool]++
		}
	}
	return used
}

// pickCandidateWithMostFreeGPUs scans the candidate node list and returns the
// node with the largest (allocatable − in-use) GPU count, along with the
// node's allocatable and free counts. Ties break on the node list's original
// order (deterministic; no randomness across repeated runs on the same
// cluster state). An empty candidate list returns a zero Node and 0/0 — the
// caller is expected to have rejected the empty case earlier, but the guard
// keeps this function safe to call in isolation.
func pickCandidateWithMostFreeGPUs(candidates []v1.Node, usedByNode map[string]int) (chosen v1.Node, allocatable, free int) {
	if len(candidates) == 0 {
		return v1.Node{}, 0, 0
	}
	bestIdx := -1
	for i, n := range candidates {
		total := nodeGPUCount(n)
		nFree := total - usedByNode[n.Name]
		if nFree < 0 {
			// Possible if a claim's pool value doesn't match exactly or an
			// allocation is stale; treat as 0 free rather than negative.
			nFree = 0
		}
		if bestIdx == -1 || nFree > free {
			bestIdx = i
			chosen = n
			allocatable = total
			free = nFree
		}
	}
	return chosen, allocatable, free
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

// nodeGPUCount returns the node's allocatable nvidia.com/gpu count, or 0 if
// the resource is absent.
func nodeGPUCount(node v1.Node) int {
	q := node.Status.Allocatable[v1.ResourceName("nvidia.com/gpu")]
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

// deployInferenceWorkload deploys the ResourceClaimTemplate, KAI Queue, and
// DynamoGraphDeployment. Sets config.deployedByUs = true as soon as any
// resource is created, so the deferred cleanup in the caller always runs —
// even if later steps (e.g., readiness wait) fail.
func deployInferenceWorkload(ctx *validators.Context, config *inferenceWorkloadConfig) error {
	// Create namespace (idempotent).
	if err := ensureNamespace(ctx, config.namespace); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create namespace", err)
	}

	// Mark deployed early so cleanup runs even on partial failure below.
	config.deployedByUs = true

	templateData := map[string]string{
		"NAMESPACE":           config.namespace,
		"MODEL":               inferenceModel,
		"GPU_COUNT":           strconv.Itoa(config.gpuCount),
		"DEPLOYMENT_NAME":     inferenceDeploymentName,
		"QUEUE_NAME":          inferenceQueueName,
		"CLAIM_TEMPLATE_NAME": inferenceClaimTemplateName,
	}

	// Apply ResourceClaimTemplate (one claim per worker pod, for DRA GPU
	// allocation). Required because target overlays are DRA-only.
	claimPath := filepath.Join("testdata", "inference", "resource-claim-template.yaml")
	if err := createOrUpdateFromTemplate(ctx, resourceClaimTemplateGVR,
		config.namespace, claimPath, templateData, nil); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to apply ResourceClaimTemplate", err)
	}
	slog.Info("Applied ResourceClaimTemplate", "name", inferenceClaimTemplateName)

	// Apply KAI Queue (best-effort; KAI scheduler may not be installed).
	queuePath := filepath.Join("testdata", "inference", "queue.yaml")
	if err := createOrUpdateFromTemplate(ctx, kaiQueueGVR,
		config.namespace, queuePath, templateData, nil); err != nil {
		slog.Info("Failed to apply KAI Queue (scheduler may not be installed)", "error", err)
	} else {
		slog.Info("Applied KAI Queue", "name", inferenceQueueName)
	}

	// Apply DynamoGraphDeployment with programmatic pod-scheduling injection.
	// The YAML template has no ${GPU_*} placeholders; scheduling is applied
	// to the worker extraPodSpec via applyInferenceWorkerScheduling below.
	deployPath := filepath.Join("testdata", "inference", "dynamo-deployment.yaml")
	mutator := func(obj *unstructured.Unstructured) error {
		return applyInferenceWorkerScheduling(obj, config)
	}
	if err := createOrUpdateFromTemplate(ctx, dynamoDeploymentGVR,
		config.namespace, deployPath, templateData, mutator); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to apply DynamoGraphDeployment", err)
	}
	slog.Info("Applied DynamoGraphDeployment",
		"name", inferenceDeploymentName, "gpuWorkers", config.gpuCount)

	// Wait for the deployment to become ready.
	if err := waitForDynamoDeploymentReady(ctx, config); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "DynamoGraphDeployment not ready", err)
	}

	return nil
}

// applyInferenceWorkerScheduling injects nodeSelector, tolerations, and
// resourceClaims into the VllmDecodeWorker service's extraPodSpec. Operating
// on the unstructured object (rather than text-substituting into the YAML
// template) keeps taint values safe from YAML special characters.
func applyInferenceWorkerScheduling(obj *unstructured.Unstructured,
	config *inferenceWorkloadConfig) error {

	services, found, err := unstructured.NestedMap(obj.Object, "spec", "services")
	if err != nil || !found {
		return errors.New(errors.ErrCodeInternal, "spec.services not found in DynamoGraphDeployment")
	}

	// Bind the worker pod to the DRA ResourceClaimTemplate.
	claimBindings := []interface{}{
		map[string]interface{}{
			"name":                      "gpu",
			"resourceClaimTemplateName": inferenceClaimTemplateName,
		},
	}

	for svcName, svcRaw := range services {
		svc, ok := svcRaw.(map[string]interface{})
		if !ok {
			continue
		}
		extraPodSpec, _, _ := unstructured.NestedMap(svc, "extraPodSpec")
		if extraPodSpec == nil {
			extraPodSpec = map[string]interface{}{}
		}

		// Tolerations AND nodeSelector apply to every service so all components
		// co-locate on the GPU node cohort. Co-location matters on clusters
		// whose GPU/system/CPU node groups live in separate network security
		// groups (e.g., EKS with per-nodegroup SGs) — splitting Frontend onto a
		// system node silently breaks the validator→Frontend health-check path
		// via cross-SG firewall drops even though every pod reports Ready.
		if len(config.gpuTolerations) > 0 {
			tolList := make([]interface{}, 0, len(config.gpuTolerations))
			for _, t := range config.gpuTolerations {
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
			extraPodSpec["tolerations"] = tolList
		}
		if len(config.gpuNodeSelector) > 0 {
			ns := make(map[string]interface{}, len(config.gpuNodeSelector))
			for k, v := range config.gpuNodeSelector {
				ns[k] = v
			}
			extraPodSpec["nodeSelector"] = ns
		}

		// Only the worker needs a GPU via DRA; frontend is CPU-only and runs
		// on the same node group purely for network reachability.
		if svcName == "VllmDecodeWorker" {
			extraPodSpec["resourceClaims"] = claimBindings
		}

		svc["extraPodSpec"] = extraPodSpec
		services[svcName] = svc
	}

	return unstructured.SetNestedMap(obj.Object, services, "spec", "services")
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
				return errors.New(errors.ErrCodeInternal,
					"namespace watch channel closed unexpectedly")
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

	waitCtx, cancel := context.WithTimeout(ctx.Ctx, defaults.InferenceWorkloadReadyTimeout)
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
				return errors.New(errors.ErrCodeInternal,
					"DynamoGraphDeployment watch channel closed unexpectedly")
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

// isDynamoDeploymentReady returns true when the object's status.state equals "successful".
func isDynamoDeploymentReady(obj *unstructured.Unstructured) bool {
	if obj == nil {
		return false
	}
	state, found, _ := unstructured.NestedString(obj.Object, "status", "state")
	return found && state == "successful"
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
// port if the Service cannot be inspected, which still works when the
// dynamo-frontend chart uses its contract port (8000).
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

// cleanupInferenceWorkload removes the deployed benchmark workload and its namespace.
// Safe to call even on partial failure — skips if deployedByUs is false.
func cleanupInferenceWorkload(ctx *validators.Context, config *inferenceWorkloadConfig) {
	if !config.deployedByUs {
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

	// Delete ResourceClaimTemplate.
	err = ctx.DynamicClient.Resource(resourceClaimTemplateGVR).
		Namespace(config.namespace).
		Delete(cleanupCtx, inferenceClaimTemplateName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		slog.Debug("Failed to delete ResourceClaimTemplate", "error", err)
	}

	// Delete namespace (cascades all remaining resources).
	err = ctx.Clientset.CoreV1().Namespaces().Delete(cleanupCtx, config.namespace, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("failed to delete namespace", "error", err)
	} else {
		slog.Info("Deleted namespace", "namespace", config.namespace)
	}
}

// inferServicePort returns port 8000 from the service if present, otherwise the first port.
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

// waitForEndpointHealth polls the inference endpoint health check until it returns 200.
func waitForEndpointHealth(ctx context.Context, endpoint string) error {
	healthURL := endpoint + "/health"
	slog.Info("Waiting for inference endpoint health", "url", healthURL)

	client := &http.Client{Timeout: defaults.HTTPClientTimeout}

	pollCtx, cancel := context.WithTimeout(ctx, defaults.InferenceHealthTimeout)
	defer cancel()

	for {
		req, err := http.NewRequestWithContext(pollCtx, http.MethodGet, healthURL, nil)
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to create health request", err)
		}

		resp, err := client.Do(req) //nolint:gosec // G704 -- URL constructed from in-cluster K8s service discovery
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				slog.Info("Inference endpoint is healthy")
				return nil
			}
			slog.Debug("Health check returned non-200", "status", resp.StatusCode)
		} else {
			slog.Debug("Health check failed", "error", err)
		}

		select {
		case <-pollCtx.Done():
			return errors.Wrap(errors.ErrCodeTimeout, "timed out waiting for inference endpoint health", pollCtx.Err())
		case <-time.After(defaults.InferenceHealthPollInterval):
		}
	}
}

// runAIPerfJob creates and runs an AIPerf benchmark Job against the inference
// endpoint. jobName must be unique per run (see buildInferenceConfig, which
// derives it from AICR_RUN_ID) so two concurrent validate invocations can't
// delete each other's Job, wait on the wrong pod, or collect the wrong logs.
func runAIPerfJob(ctx *validators.Context, endpoint string, concurrency int, jobName string) (string, error) {
	// Propagate image pull secrets from the outer validator pod so the inner
	// aiperf benchmark pod can pull from the same authenticated registry.
	// Without this, setups using AICR_VALIDATOR_IMAGE_REGISTRY pointing at a
	// private mirror start the outer pod fine (the Deployer attaches pull
	// secrets via --image-pull-secret) but the inner aiperf pod hangs in
	// ImagePullBackOff.
	pullSecrets := getOwnPullSecrets(ctx)
	slog.Info("Running AIPerf benchmark",
		"endpoint", endpoint,
		"model", inferenceModel,
		"concurrency", concurrency,
		"requests", aiperfMinRequests,
		"job", jobName)

	job := buildAIPerfJob(ctx.Namespace, jobName, endpoint, concurrency, pullSecrets)

	// Because jobName is per-run-unique, there is no shared-state pre-clean to
	// do; only the deferred cleanup runs on exit.

	createCtx, cancel := context.WithTimeout(ctx.Ctx, defaults.K8sJobCreationTimeout)
	defer cancel()

	_, err := ctx.Clientset.BatchV1().Jobs(ctx.Namespace).Create(createCtx, job, metav1.CreateOptions{})
	if err != nil {
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
func buildAIPerfJob(namespace, jobName, endpoint string, concurrency int, pullSecrets []v1.LocalObjectReference) *batchv1.Job {
	// AIPerf requires request_count >= concurrency.
	requestCount := aiperfMinRequests
	if concurrency*2 > requestCount {
		requestCount = concurrency * 2
	}
	// Resolve once so Image and ImagePullPolicy can't drift if env vars
	// were mutated between two calls (matters in tests; cheap in prod).
	aiperfImage := resolveAiperfImage()

	script := fmt.Sprintf(`set -e
aiperf profile "%s" \
  --url %s \
  --endpoint-type chat \
  --streaming \
  --concurrency %d \
  --request-count %d \
  --prompt-input-tokens-mean %d \
  --prompt-output-tokens-mean %d \
  --output-artifact-dir %s \
  --export-level summary
echo '%s'
cat %s/profile_export_aiperf.json
echo '%s'`,
		inferenceModel, endpoint,
		concurrency, requestCount,
		aiperfInputTokensMean, aiperfOutputTokensMean,
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
							// Job uses (catalog.ImagePullPolicy handles the
							// AICR_VALIDATOR_IMAGE_TAG override and digest
							// pins) so a mutable override tag on the CLI
							// doesn't let the aiperf pod serve a stale
							// cached image while the outer validator pulls
							// the current one. Leaving this unset would
							// fall back to Kubernetes' default (Always
							// only for :latest), which is insufficient for
							// `:edge`, `:main`, and similar rolling tags
							// on-push.yaml recreates on every merge.
							ImagePullPolicy: catalog.ImagePullPolicy(aiperfImage),
							Command:         []string{"/bin/sh", "-c"},
							Args:            []string{script},
						},
					},
				},
			},
		},
	}
}

// cleanupAIPerfJob removes the AIPerf Job (if it exists) and waits for
// actual deletion. Synchronous wait prevents subsequent Create calls from
// racing against an in-flight foreground deletion and hitting AlreadyExists.
func cleanupAIPerfJob(ctx *validators.Context, jobName string) {
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
