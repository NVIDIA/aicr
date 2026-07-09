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
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
	k8spod "github.com/NVIDIA/aicr/pkg/k8s/pod"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

const (
	testType       = "all_reduce_perf"
	minMessageSize = "1K"
	maxMessageSize = "16G"

	// maxMessageSizeTCP is a reduced upper bound for clusters without
	// high-bandwidth interconnect (e.g. EFA). Multi-GB all_reduce over TCP
	// can hang or take unreasonably long with 16+ ranks.
	maxMessageSizeTCP = "4G"

	// ncclTrainJobName is the name used for both the TrainJob resource and the label
	// selector when waiting for the launcher pod. Must stay in sync with trainjob.yaml.
	ncclTrainJobName = "nccl-all-reduce-tj"

	// ncclTrainingRuntimeName is the name of the TrainingRuntime resource.
	// Must stay in sync with runtime.yaml.
	ncclTrainingRuntimeName = "nccl-all-reduce-runtime"
)

// Package-level GVR definitions for Kubeflow Trainer CRDs used by both
// applyNCCLResources and cleanupNCCLResources.
var (
	trainJobGVR = schema.GroupVersionResource{
		Group:    "trainer.kubeflow.org",
		Version:  versionV1alpha1,
		Resource: "trainjobs",
	}

	trainingRuntimeGVR = schema.GroupVersionResource{
		Group:    "trainer.kubeflow.org",
		Version:  versionV1alpha1,
		Resource: "trainingruntimes",
	}

	// computeDomainGVR is the NVIDIA DRA driver's ComputeDomain CR, used only
	// by the NVLS variant to provision an IMEX domain across worker nodes.
	// The CR causes the DRA driver to auto-generate a ResourceClaimTemplate
	// (name matching channel.resourceClaimTemplate.name below) that worker
	// pods reference via resourceClaims to get /dev/nvidia-caps-imex-channels
	// mounted. Without this, MNNVL ("NVLS") fabric is detected but unusable.
	computeDomainGVR = schema.GroupVersionResource{
		Group:    "resource.nvidia.com",
		Version:  "v1beta1",
		Resource: "computedomains",
	}
)

const (
	ncclComputeDomainName = "nccl-all-reduce-cd"

	// ncclIMEXClaimTemplateName must match the resourceClaimTemplateName
	// field in runtime-nvls.yaml templates — the DRA driver uses
	// this name when auto-generating the RCT from the ComputeDomain CR.
	ncclIMEXClaimTemplateName = "nccl-all-reduce-imex"
)

// ncclBandwidthRe matches any data row in NCCL all-reduce output and captures the
// out-of-place busbw column. parseBandwidthFromLogs uses the last match (largest message size).
// EKS max is 16G (17179869184), GKE max is 8G (8589934592) — this regex handles both.
var ncclBandwidthRe = regexp.MustCompile(`\s+(\d+)\s+\d+\s+\w+\s+\w+\s+-?\d+\s+[\d.]+\s+[\d.]+\s+([\d.]+)`)

// ncclVariant selects an NCCL transport-class template for the all-reduce check.
// Variant names follow NCCL's own vocabulary: NET (network transport — EFA on EKS,
// TCPXO on GKE, IB on-prem) and NVLS (NVLink SHARP / MNNVL). The zero value runs
// the provider default template and asserts nothing about transport.
type ncclVariant string

const (
	variantDefault ncclVariant = ""
	variantNET     ncclVariant = "net"
	variantNVLS    ncclVariant = "nvls"
)

// ncclFabricType selects the inter-node fabric for the NET variant. Default EFA
// preserves all existing behavior; roce (AICR_NCCL_FABRIC=roce) selects the
// ConnectX RoCE NET path — NCCL's built-in IB/verbs transport over
// roce.networking.k8s.aws DRA devices. Fabric is keyed independently of the
// accelerator: the RoCE NET template is shared across EKS RoCE nodes
// (testdata/roce/{service}/...), not per-accelerator. Snapshot-based fabric
// auto-detection (so this env knob becomes an override, not the selector) is
// tracked in NVIDIA/aicr#1413.
type ncclFabricType string

const (
	fabricEFA  ncclFabricType = "efa"
	fabricRoCE ncclFabricType = "roce"
	// ncclFabricEnv is the validator-pod (reading) end of the fabric selector.
	// The orchestrator (forwarding) end defines the same literal as ncclFabricEnv
	// in pkg/validator/v1/job_plan_internal.go — keep the two in sync. The pod
	// binary is a separate package and does not import the orchestrator package,
	// matching how the other forwarded validator envs are split.
	ncclFabricEnv = "AICR_NCCL_FABRIC"

	// ncclRoceClaimName is the RoCE DRA ResourceClaimTemplate name. Must match
	// metadata.name in testdata/roce/{service}/roce-claim.yaml; used by cleanup
	// to delete the claim (the validator namespace is persistent/reused).
	ncclRoceClaimName = "nccl-roce-rct"
)

// roceNETSupportedServices lists services with a testdata/roce/{service} NET
// template. RoCE NET is accelerator-agnostic, so support is keyed by service.
var roceNETSupportedServices = map[recipe.CriteriaServiceType]bool{
	recipe.CriteriaServiceEKS: true,
}

// ncclFabric returns the configured NET fabric (default EFA when unset). Read
// from the validator pod's environment, forwarded by the CLI/orchestrator
// (buildEnv). A non-empty but unrecognized value (e.g. a typo "roc") is
// rejected rather than silently falling back to EFA, so an operator who
// intended RoCE never passes the EFA validator by accident.
func ncclFabric() (ncclFabricType, error) {
	v := strings.TrimSpace(os.Getenv(ncclFabricEnv))
	switch {
	case v == "":
		return fabricEFA, nil
	case strings.EqualFold(v, string(fabricEFA)):
		return fabricEFA, nil
	case strings.EqualFold(v, string(fabricRoCE)):
		return fabricRoCE, nil
	default:
		return "", aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported %s=%q (expected %q or %q)", ncclFabricEnv, v, fabricEFA, fabricRoCE))
	}
}

// Transport markers emitted by NCCL when NCCL_DEBUG=INFO. Used by
// verifyTransportFromLogs to assert the intended fabric actually carried
// traffic. Earlier NCCL releases emitted per-channel "[send] via NET/<plugin>"
// lines; from NCCL 2.27 onward the per-channel banner is gone and the
// authoritative signals are the "Using network <plugin>" bootstrap selection
// (NET) and the "NVLS comm 0x<addr>" communicator-init line (NVLS). NVLS
// comm init is only logged when NCCL actually builds an NVLS communicator,
// so matching it is proof of use rather than mere hardware availability.
var (
	ncclUsingNetRe      = regexp.MustCompile(`NCCL INFO Using network (\S+)`)
	ncclNVLSCommInitRe  = regexp.MustCompile(`NVLS comm 0x[0-9a-fA-F]+`)
	ncclNVLSAvailableRe = regexp.MustCompile(`NVLS multicast support is available`)
)

// templatePath returns the path to a testdata template file for the given
// accelerator, service, and variant:
//
//	variantDefault → testdata/{accelerator}/{service}/{filename}
//	other variants → testdata/{accelerator}/{service}/{stem}-{variant}{ext}
func templatePath(accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType, variant ncclVariant, fabric ncclFabricType, filename string) string {
	if variant != variantDefault {
		ext := filepath.Ext(filename)
		stem := strings.TrimSuffix(filename, ext)
		filename = stem + "-" + string(variant) + ext
	}
	// RoCE NET templates are fabric-keyed and accelerator-agnostic: any EKS RoCE
	// node uses testdata/roce/{service}/..., not a per-accelerator directory.
	if fabric == fabricRoCE {
		return filepath.Join("testdata", string(fabricRoCE), string(service), filename)
	}
	return filepath.Join("testdata", string(accelerator), string(service), filename)
}

// supportedNCCLCombinations lists, per variant, which (service, accelerator)
// tuples have a corresponding testdata template. All platforms use Kubeflow
// TrainJob + MPI with per-platform TrainingRuntimes and a shared TrainJob.
// variantDefault preserves the pre-variant behavior; named variants opt in
// targeted transport-class coverage.
var supportedNCCLCombinations = map[ncclVariant]map[recipe.CriteriaServiceType][]recipe.CriteriaAcceleratorType{
	variantDefault: {
		// H200 is Hopper on EFA, electrically identical to H100 for NCCL
		// (NVLink4 intra-node, EFA inter-node), so it reuses the EKS H100
		// runtime template and the same calibrated >= 300 GB/s floor.
		recipe.CriteriaServiceEKS: {recipe.CriteriaAcceleratorH100, recipe.CriteriaAcceleratorH200},
		recipe.CriteriaServiceGKE: {recipe.CriteriaAcceleratorH100},
		recipe.CriteriaServiceAny: {recipe.CriteriaAcceleratorB200, recipe.CriteriaAcceleratorGB200},
	},
	variantNET: {
		recipe.CriteriaServiceEKS: {recipe.CriteriaAcceleratorGB200},
	},
	variantNVLS: {
		recipe.CriteriaServiceEKS: {recipe.CriteriaAcceleratorGB200},
		recipe.CriteriaServiceOKE: {recipe.CriteriaAcceleratorGB200},
	},
}

// validateNcclAllReduceBw validates NCCL All Reduce bandwidth using Kubeflow TrainJob + MPI.
// Each platform has its own TrainingRuntime; the TrainJob is shared (just runtimeRef + numNodes).
// The variant selects a transport-class template (NET, NVLS) when the recipe needs per-fabric
// coverage on clusters that expose multiple inter-node fabrics (e.g. GB200/EKS).
// Returns actual bandwidth value, whether it passed the threshold, and any error.
func validateNcclAllReduceBw(ctx *validators.Context, constraint recipe.Constraint, variant ncclVariant) (string, bool, error) {
	slog.Info("Starting NCCL All Reduce bandwidth validation", "variant", string(variant))

	// Skip unless the validation targets a supported service + accelerator combination.
	if ctx.ValidationInput == nil {
		slog.Info("Skipping NCCL All Reduce bandwidth validation: no validation")
		return "skipped - requires Service + Accelerator", true, nil
	}

	service := ctx.ValidationInput.Criteria.Service
	accelerator := ctx.ValidationInput.Criteria.Accelerator

	fabric, err := ncclFabric()
	if err != nil {
		return "", false, err
	}

	supported := false
	if fabric == fabricRoCE && variant == variantNET {
		// RoCE NET is fabric-keyed and accelerator-agnostic — supported on any
		// service with a testdata/roce/{service} template. Only NET has a RoCE
		// path; NVLS (NVLink/IMEX) is fabric-independent and uses the normal
		// accelerator-keyed combinations below.
		supported = roceNETSupportedServices[service]
	} else if byService, ok := supportedNCCLCombinations[variant]; ok {
		if supportedAccelerators, ok := byService[service]; ok {
			for _, a := range supportedAccelerators {
				if accelerator == a {
					supported = true
					break
				}
			}
		}
	}

	if !supported {
		slog.Info("Skipping NCCL All Reduce bandwidth validation: unsupported variant/service/accelerator combination",
			"variant", string(variant), "service", service, "accelerator", accelerator, "fabric", string(fabric))
		return "skipped - requires Service + Accelerator to be implemented", true, nil
	}

	// Extract threshold from constraint
	threshold, err := parseThreshold(constraint.Value)
	if err != nil {
		return "", false, aicrErrors.Wrap(aicrErrors.ErrCodeInvalidRequest, "invalid threshold", err)
	}
	slog.Info("Target bandwidth threshold", "threshold", threshold, "tolerance", "10%")

	// Determine GPU configuration from cluster.
	gpuConfig, err := determineGPUConfig(ctx, service, accelerator)
	if err != nil {
		return "", false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to determine GPU configuration", err)
	}
	slog.Info("GPU Configuration", "nodes", gpuConfig.WorkerCount, ", GPUs/node", gpuConfig.GPUCountPerNode, ", total GPUs", gpuConfig.TotalGPUCount)

	// NCCL all-reduce tests EW (East-West) fabric between nodes and requires at least
	// two GPU nodes. Skip gracefully rather than fail when only one node is available.
	if gpuConfig.WorkerCount < 2 {
		slog.Info("Skipping NCCL All Reduce bandwidth validation: requires at least 2 GPU nodes for EW fabric test",
			"nodes", gpuConfig.WorkerCount)
		return "skipped - requires at least 2 GPU nodes for EW fabric test", true, nil
	}

	// Preflight cluster-side prerequisites before spending TrainJob time.
	// On GB200/EKS the NET variant needs NVreg_GrdmaPciTopoCheckOverride=1
	// on the NVIDIA driver; without it, EFA can't attach dma-buf to GPU HBM
	// and NCCL silently falls back to Socket.
	if fabric == fabricEFA && gb200NetPreflightApplies(variant, accelerator, service) {
		if pfErr := preflightGB200NetNVregFlag(ctx, gpuConfig.Nodes); pfErr != nil {
			return "", false, pfErr
		}
	}

	// On GKE H100, the worker pods depend on GPUDirect-TCPXO host artifacts
	// (nccl-env-profile.sh + FastRak libraries) laid down by the
	// nccl-tcpxo-installer DaemonSet. On freshly provisioned nodes that
	// DaemonSet may not have finished when this check runs; without the
	// artifacts the workers never start sshd and the launcher mpirun fails
	// with an opaque "pod failed" minutes later. Fail fast with an actionable
	// error naming the unready nodes instead.
	if gkeTCPXOPreflightApplies(variant, accelerator, service) {
		if pfErr := preflightGKETCPXOReady(ctx, gpuConfig.Nodes); pfErr != nil {
			return "", false, pfErr
		}
	}

	// Run the NCCL all-reduce benchmark using Kubeflow TrainJob + MPI.
	// Each platform has a per-platform TrainingRuntime with all platform-specific
	// configuration (image, mpirun args, resources, sidecars). The TrainJob is shared.
	logs, err := runNCCLTrainJob(ctx, gpuConfig, accelerator, service, variant, fabric)
	if err != nil {
		return "", false, err
	}

	// Parse bandwidth from logs (shared across all service types).
	bandwidth, err := parseBandwidthFromLogs(logs)
	if err != nil {
		// The launcher pod succeeded but its log yielded no parseable bandwidth
		// row. Surface the retrieved log into report.json the way the pod-failed
		// path does via emitDiagnosticBlock — without it, a succeeded-but-
		// unparseable run is a dead end: we cannot tell an empty/truncated log
		// capture from a benchmark that exited 0 without emitting the results
		// table. (The caller discards the returned logs string on error, so
		// logging is the only way this reaches the check's captured stdout.)
		slog.Error("NCCL launcher succeeded but bandwidth could not be parsed; dumping launcher log",
			"logBytes", len(logs))
		emitDiagnosticBlock("launcher log (bandwidth parse failed)", tailLines(strings.TrimSpace(logs), maxDiagLogLines))
		return logs, false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to parse bandwidth from logs", err)
	}

	// For named variants, assert the expected transport actually carried traffic.
	// This turns the variant label into a hard guarantee — a GB200 cluster
	// with a broken IMEX domain will fail loudly on the NVLS variant instead of
	// silently falling back to EFA.
	if err := verifyTransportFromLogs(logs, variant); err != nil {
		return logs, false, err
	}

	slog.Info("Measured bandwidth", "bandwidth", bandwidth)

	// Check if bandwidth meets threshold (within 10% tolerance)
	passed := bandwidth >= (threshold * 0.9)
	actualValue := fmt.Sprintf("%.2f GB/s", bandwidth)

	if passed {
		slog.Info("Bandwidth validation passed", "bandwidth", bandwidth, "threshold", threshold*0.9, "tolerance", "10%")
	} else {
		slog.Info("Bandwidth validation failed", "bandwidth", bandwidth, "threshold", threshold*0.9, "tolerance", "10%")
	}

	return actualValue, passed, nil
}

// runNCCLTrainJob runs the NCCL all-reduce benchmark using Kubeflow TrainJob + MPI.
// It applies the per-platform TrainingRuntime and shared TrainJob, waits for the launcher
// pod to complete, and returns the benchmark logs.
func runNCCLTrainJob(ctx *validators.Context, gpuConfig *gpuConfiguration,
	accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType, variant ncclVariant, fabric ncclFabricType) (string, error) {

	dynamicClient := ctx.DynamicClient

	// Ensure Kubeflow Trainer is installed. If it is already present we leave it
	// alone; if we install it we clean it up after the test completes.
	trainerInstalled, err := isTrainerInstalled(ctx.Ctx, dynamicClient)
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to check Kubeflow Trainer installation", err)
	}
	if !trainerInstalled {
		slog.Info("Kubeflow Trainer not found, installing...")
		var installedResources []trainerResourceRef
		installedResources, err = installTrainer(ctx.Ctx, dynamicClient, ctx.Clientset.Discovery())
		if err != nil {
			return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to install Kubeflow Trainer", err)
		}
		defer deleteTrainer(dynamicClient, installedResources)
		slog.Info("Kubeflow Trainer installed", "resources", len(installedResources))
	} else {
		slog.Info("Kubeflow Trainer already installed, proceeding")
	}

	// Clean up NCCL resources on every exit path. Registered after the trainer
	// install block but before the apply: defers run LIFO, so this runs *before*
	// the conditional deleteTrainer above — the NCCL TrainJob/TrainingRuntime CRs
	// are deleted while their CRDs still exist, rather than relying on CRD-delete
	// cascade GC. Registering it before applyNCCLResources still guarantees a
	// partial-apply failure (e.g. the RoCE claim is created, then the runtime or
	// TrainJob apply fails) doesn't leak nccl-roce-rct into the persistent, reused
	// validation namespace. cleanupNCCLResources is NotFound-tolerant for every
	// resource it deletes, so running it after an early failure is safe.
	defer cleanupNCCLResources(dynamicClient, gpuConfig.Namespace)

	// Apply runtime and trainjob resources.
	if applyErr := applyNCCLResources(ctx, dynamicClient, gpuConfig, accelerator, service, variant, fabric); applyErr != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to apply NCCL resources", applyErr)
	}

	podHelper := &helper.PodLifecycle{
		ClientSet: ctx.Clientset,
		Namespace: ctx.Namespace,
	}

	// Wait for launcher pod and get logs.
	logs, err := waitForLauncherPodAndGetLogs(ctx, podHelper)
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to get launcher logs", err)
	}

	return logs, nil
}

// gpuConfiguration holds GPU node and count information
type gpuConfiguration struct {
	WorkerCount     int
	GPUCountPerNode int
	TotalGPUCount   int
	Namespace       string
	Nodes           []v1.Node
}

// parseThreshold extracts the numeric threshold value from a constraint value.
// Handles formats like "450", "450 GB/s", ">= 400", ">= 100 GB/s".
func parseThreshold(value string) (float64, error) {
	numStr := strings.TrimSpace(value)
	// Strip comparison operator prefix (>=, >, <=, <, ==, =)
	numStr = strings.TrimLeft(numStr, "><=! ")
	numStr = strings.TrimSpace(numStr)
	// Strip units suffix (e.g., "GB/s")
	numStr = strings.Split(numStr, " ")[0]

	if numStr == "" {
		return 0, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid threshold: no numeric value found in %q", value))
	}

	threshold, err := strconv.ParseFloat(numStr, 64)
	if err != nil {
		return 0, aicrErrors.Wrap(aicrErrors.ErrCodeInvalidRequest, "invalid threshold format", err)
	}

	return threshold, nil
}

// Node label keys consulted by resolveTargetGPUNodes. gpuProductLabel is set
// by NVIDIA GPU Feature Discovery (part of the GPU Operator deployed by AICR
// recipes); instanceTypeLabel is the standard Kubernetes instance-type label
// that platformWorkerScheduling also keys off on EKS.
const (
	gpuProductLabel   = "nvidia.com/gpu.product"
	instanceTypeLabel = "node.kubernetes.io/instance-type"
)

// acceleratorProductMatchers maps a recipe Criteria.Accelerator to a predicate
// that reports whether a given nvidia.com/gpu.product label value belongs to
// that accelerator family. Exact matches are used where GFD emits a single
// product string (GB200, B200, L40 family, RTX Pro 6000); prefix matches cover
// accelerators with multiple concrete SKUs (H100 SXM/PCIe/NVL, H200, A100 SXM/PCIe).
// No entry for CriteriaAcceleratorAny — "any" deliberately skips the filter.
var acceleratorProductMatchers = map[recipe.CriteriaAcceleratorType]func(string) bool{
	recipe.CriteriaAcceleratorGB200:      func(s string) bool { return s == "NVIDIA-GB200" },
	recipe.CriteriaAcceleratorB200:       func(s string) bool { return s == "NVIDIA-B200" },
	recipe.CriteriaAcceleratorH100:       func(s string) bool { return strings.HasPrefix(s, "NVIDIA-H100-") },
	recipe.CriteriaAcceleratorH200:       func(s string) bool { return strings.HasPrefix(s, "NVIDIA-H200-") },
	recipe.CriteriaAcceleratorA100:       func(s string) bool { return strings.HasPrefix(s, "NVIDIA-A100-") },
	recipe.CriteriaAcceleratorL40:        func(s string) bool { return s == "NVIDIA-L40" || s == "NVIDIA-L40S" },
	recipe.CriteriaAcceleratorRTXPro6000: func(s string) bool { return s == "NVIDIA-RTX-PRO-6000" },
}

// resolveTargetGPUNodes narrows the full schedulable GPU set to the subset
// the NCCL TrainJob will actually schedule workers onto. Shared by every
// downstream consumer (WorkerCount / GPU_COUNT sizing, NVreg preflight,
// EFA discovery, worker podSpec placement) so the TrainJob cannot request
// more workers than the worker podSpec's nodeSelector can match.
//
// Precedence:
//  1. override (ctx.NodeSelector — user --node-selector) if non-empty.
//     Zero-match → hard error naming the override.
//  2. Filter by nvidia.com/gpu.product ↔ recipe accelerator when a matcher
//     exists and at least one input node carries the label. Makes
//     accelerator selection deterministic on heterogeneous clusters
//     (e.g. 2× GB200 + 3× H100 under one EKS control plane) instead of
//     depending on node list order. Zero-match → hard error naming the
//     expected accelerator and the products actually seen, so the
//     operator isn't pointed at a misleading secondary error like the
//     NVreg preflight failing on H100 nodes.
//  3. On EKS, further narrow to the first (possibly accelerator-filtered)
//     node's instance-type — same key platformWorkerScheduling stamps
//     into the worker podSpec. Also applies as the sole narrow when the
//     cluster lacks GFD labels, preserving behavior on non-GFD installs.
//  4. No filter — non-EKS services without a discoverable default
//     selector key return the accelerator-filtered set as-is.
//
// Heterogeneous-cluster contract: the GFD-based accelerator filter makes
// the auto-path correct on mixed accelerator pools. For finer-grained
// control (e.g. forcing a specific subnet or a single instance-type
// within a family), the operator should still pass --node-selector.
func resolveTargetGPUNodes(nodes []v1.Node, override map[string]string, service recipe.CriteriaServiceType, accelerator recipe.CriteriaAcceleratorType) ([]v1.Node, error) {
	if len(override) > 0 {
		out := nodesMatchingSelector(nodes, override)
		if len(out) == 0 {
			return nil, aicrErrors.New(aicrErrors.ErrCodeInternal,
				fmt.Sprintf("--node-selector %v matches zero of %d GPU nodes", override, len(nodes)))
		}
		slog.Info("Narrowed GPU nodes via --node-selector", "selector", override, "matched", len(out), "total", len(nodes))
		return out, nil
	}

	working, err := narrowByAccelerator(nodes, accelerator)
	if err != nil {
		return nil, err
	}

	if out, narrowed := narrowByInstanceType(working, service); narrowed {
		return out, nil
	}
	return working, nil
}

// narrowByAccelerator filters nodes by the recipe's accelerator → gpu.product
// mapping when both a matcher exists and the cluster carries GFD labels.
// Returns the input unchanged when either condition fails (e.g. accelerator=any,
// or a non-GFD install) so the caller can fall back to a later narrowing step.
// Zero matches after an attempted filter is an error, not a fallback — the
// recipe explicitly asked for an accelerator the cluster doesn't provide.
func narrowByAccelerator(nodes []v1.Node, accelerator recipe.CriteriaAcceleratorType) ([]v1.Node, error) {
	matcher, ok := acceleratorProductMatchers[accelerator]
	if !ok || !anyNodeHasLabel(nodes, gpuProductLabel) {
		return nodes, nil
	}
	matched := make([]v1.Node, 0, len(nodes))
	for _, n := range nodes {
		if matcher(n.Labels[gpuProductLabel]) {
			matched = append(matched, n)
		}
	}
	if len(matched) == 0 {
		return nil, aicrErrors.New(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("no schedulable GPU nodes match recipe accelerator %q (products seen: %v)", accelerator, uniqueGPUProducts(nodes)))
	}
	if len(matched) < len(nodes) {
		slog.Info("Narrowed GPU nodes by accelerator via nvidia.com/gpu.product",
			"accelerator", accelerator, "matched", len(matched), "total", len(nodes))
	}
	return matched, nil
}

// narrowByInstanceType applies the EKS-only instance-type narrow so the
// worker podSpec's platformWorkerScheduling selector matches every returned
// node. Returns (nodes, false) when not applicable (non-EKS, empty input, or
// first node missing the label — e.g. a non-AWS control plane mis-tagged as
// EKS in the recipe), letting the caller fall through.
func narrowByInstanceType(nodes []v1.Node, service recipe.CriteriaServiceType) ([]v1.Node, bool) {
	if service != recipe.CriteriaServiceEKS || len(nodes) == 0 {
		return nodes, false
	}
	it := nodes[0].Labels[instanceTypeLabel]
	if it == "" {
		return nodes, false
	}
	selector := map[string]string{instanceTypeLabel: it}
	out := nodesMatchingSelector(nodes, selector)
	// out is guaranteed non-empty: nodes[0] matches itself.
	if len(out) < len(nodes) {
		slog.Info("Narrowed GPU nodes by instance-type", "selector", selector, "matched", len(out), "total", len(nodes))
	}
	return out, true
}

// uniqueGPUProducts returns the sorted set of non-empty gpu.product label
// values observed across nodes. Used only for the zero-match diagnostic in
// narrowByAccelerator; happy paths skip the sort.
func uniqueGPUProducts(nodes []v1.Node) []string {
	seen := map[string]struct{}{}
	for _, n := range nodes {
		if p := n.Labels[gpuProductLabel]; p != "" {
			seen[p] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// anyNodeHasLabel reports whether at least one node carries a non-empty value
// for the given label key. Used to detect whether the cluster has been
// labeled by NVIDIA GFD before relying on the label as a filter.
func anyNodeHasLabel(nodes []v1.Node, key string) bool {
	for _, n := range nodes {
		if n.Labels[key] != "" {
			return true
		}
	}
	return false
}

// determineGPUConfig analyzes the snapshot to determine GPU node configuration.
// The returned Nodes slice is already narrowed to the TrainJob's target set
// via resolveTargetGPUNodes so WorkerCount, GPUCountPerNode, and TotalGPUCount
// agree with what the worker podSpec will later schedule onto.
func determineGPUConfig(ctx *validators.Context, service recipe.CriteriaServiceType, accelerator recipe.CriteriaAcceleratorType) (*gpuConfiguration, error) {
	slog.Info("Analyzing GPU node configuration...")

	gpuNodes, err := helper.FindSchedulableGpuNodes(ctx.Ctx, ctx.Clientset)
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to find GPU nodes", err)
	}
	if len(gpuNodes) == 0 {
		return nil, aicrErrors.New(aicrErrors.ErrCodeInternal, "no schedulable GPU nodes found")
	}
	slog.Info("Found GPU nodes", "count", len(gpuNodes))

	targetNodes, err := resolveTargetGPUNodes(gpuNodes, ctx.NodeSelector, service, accelerator)
	if err != nil {
		return nil, err
	}

	// Get GPU count from first target node (assuming homogeneous target set)
	firstNode := targetNodes[0]
	gpuResource := v1.ResourceName("nvidia.com/gpu")
	gpuQuantity := firstNode.Status.Allocatable[gpuResource]
	gpuCountPerNode := int(gpuQuantity.Value())

	if gpuCountPerNode == 0 {
		return nil, aicrErrors.New(aicrErrors.ErrCodeInternal, "no GPUs found on nodes")
	}

	totalGPUs := len(targetNodes) * gpuCountPerNode

	return &gpuConfiguration{
		WorkerCount:     len(targetNodes),
		GPUCountPerNode: gpuCountPerNode,
		TotalGPUCount:   totalGPUs,
		Namespace:       ctx.Namespace,
		Nodes:           targetNodes,
	}, nil
}

// applyNCCLResources applies the per-platform TrainingRuntime and shared TrainJob
// YAML files with template substitution using the dynamic client.
// Runtime: testdata/{accelerator}/{service}/runtime[-{variant}].yaml (per-platform+variant)
// TrainJob: testdata/trainjob.yaml (shared, just runtimeRef + numNodes)
func applyNCCLResources(ctx *validators.Context, dynamicClient dynamic.Interface, config *gpuConfiguration, accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType, variant ncclVariant, fabric ncclFabricType) error {
	slog.Info("Applying NCCL test resources...", "accelerator", accelerator, "service", service, "variant", string(variant), "fabric", string(fabric))

	templateData := map[string]string{
		"NAMESPACE":          config.Namespace,
		"WORKER_COUNT":       strconv.Itoa(config.WorkerCount),
		"GPU_COUNT_PER_NODE": strconv.Itoa(config.GPUCountPerNode),
		"GPU_COUNT":          strconv.Itoa(config.TotalGPUCount),
		"TEST_TYPE":          testType,
		"MIN_MESSAGE_SIZE":   minMessageSize,
		"MAX_MESSAGE_SIZE":   maxMessageSize,
	}

	var instanceType string

	// For GKE, discover GPU NIC network names (cluster-specific prefixes).
	if service == recipe.CriteriaServiceGKE {
		gpuNICs, err := discoverGKEGPUNICNetworks(ctx.Ctx, dynamicClient)
		if err != nil {
			return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to discover GKE GPU NIC networks", err)
		}
		if len(gpuNICs) < 8 {
			return aicrErrors.New(aicrErrors.ErrCodeInternal,
				fmt.Sprintf("expected 8 GPU NIC networks, found %d — cluster may not have multi-NIC networking configured", len(gpuNICs)))
		}
		templateData["GKE_NETWORK_INTERFACES"] = buildGKENetworkInterfacesAnnotation(gpuNICs)
		templateData["NRI_DEVICE_ANNOTATION"] = buildNRIDeviceAnnotation(config.GPUCountPerNode)
		slog.Info("Discovered GKE GPU NIC networks", "count", len(gpuNICs), "networks", gpuNICs)
	}

	// For EKS, discover instance type and EFA adapter count from GPU nodes.
	// EFA count of 0 is valid — NCCL falls back to TCP (slower but functional).
	if service == recipe.CriteriaServiceEKS {
		warnIfHeterogeneousNodes(config.Nodes)
		it, efaCount, err := discoverEKSNodeConfig(config.Nodes[0])
		if err != nil {
			return err
		}
		instanceType = it
		// EFA resource wiring is fabric-specific; the RoCE path claims NICs via a
		// DRA ResourceClaimTemplate below (keyed by fabric, not service) and
		// leaves these EFA template vars unset.
		if fabric != fabricRoCE {
			// Indentation matches the resource block position in runtime.yaml.
			const efaIndent = "                      "
			templateData["EFA_RESOURCE_LIMITS"] = buildEFAResourceLine(efaCount, efaIndent)
			templateData["EFA_RESOURCE_REQUESTS"] = buildEFAResourceLine(efaCount, efaIndent)
			if efaCount == 0 {
				templateData["MAX_MESSAGE_SIZE"] = maxMessageSizeTCP
				slog.Warn("No EFA adapters found — NCCL will use TCP (reduced bandwidth)",
					"instanceType", instanceType, "maxMessageSize", maxMessageSizeTCP)
			} else {
				slog.Info("Discovered EKS node configuration", "instanceType", instanceType, "efaCount", efaCount)
			}
		}
	}

	// Build effective worker scheduling: user override takes precedence over platform default.
	defaultNodeSelector, defaultTolerations, err := platformWorkerScheduling(service, instanceType, config.Nodes)
	if err != nil {
		return err
	}
	effectiveNodeSelector := defaultNodeSelector
	// Gate on len() rather than != nil so an explicit but empty selector does
	// not silently clear the platform default for scheduling while
	// resolveTargetGPUNodes (which gates on len > 0) still narrows the counted
	// set — that asymmetry would let workers schedule outside the cohort the
	// job was sized for.
	if len(ctx.NodeSelector) > 0 {
		effectiveNodeSelector = ctx.NodeSelector
		slog.Info("Using user-provided node selector override for NCCL workers", "selector", ctx.NodeSelector)
	}
	effectiveTolerations := defaultTolerations
	if ctx.Tolerations != nil {
		effectiveTolerations = ctx.Tolerations
		slog.Info("Using user-provided toleration override for NCCL workers", "count", len(ctx.Tolerations))
	}

	if service == recipe.CriteriaServiceAny && len(effectiveNodeSelector) == 0 {
		return aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			"self-managed clusters (service=any) require --node-selector to identify GPU nodes "+
				"(e.g., --node-selector nvidia.com/gpu.present=true)")
	}

	// RoCE NET: the worker pod references a RoCE DRA ResourceClaimTemplate
	// (nccl-roce-rct). parseYAMLTemplate is single-document, so apply the claim
	// as a standalone object before the runtime (it must exist when the TrainJob
	// later creates the worker pods that reference it).
	if fabric == fabricRoCE {
		// Claim one ConnectX RoCE device per GPU via DRA (NCCL maps GPU->NIC);
		// the per-node device pool (e.g. 8 on p6e-gb300r) is >= GPUs/node. Set
		// here — keyed by fabric, not service — so adding a non-EKS RoCE service
		// to roceNETSupportedServices still renders ${ROCE_DEVICE_COUNT}.
		templateData["ROCE_DEVICE_COUNT"] = strconv.Itoa(config.GPUCountPerNode)
		slog.Info("RoCE NET: claiming RoCE DRA devices", "count", config.GPUCountPerNode)

		// Create-or-update (not plain Create) so a stale claim left by a prior
		// run that was hard-killed before its deferred cleanup ran is reclaimed
		// rather than failing the apply with AlreadyExists. The RoCE NET path
		// legitimately still deploys a DRA ResourceClaimTemplate (per-GPU NIC
		// claims via the shared resourceClaimTemplateGVR in dra_gvr.go).
		claimPath := filepath.Join("testdata", string(fabricRoCE), string(service), "roce-claim.yaml")
		if cerr := createOrUpdateFromTemplate(ctx, resourceClaimTemplateGVR, config.Namespace, claimPath, templateData, nil); cerr != nil {
			return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to apply RoCE ResourceClaimTemplate", cerr)
		}
		slog.Info("Applied RoCE ResourceClaimTemplate", "name", ncclRoceClaimName, "count", templateData["ROCE_DEVICE_COUNT"])
	}

	runtimeObj, err := parseYAMLTemplate(templatePath(accelerator, service, variant, fabric, "runtime.yaml"), templateData)
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to parse training runtime template", err)
	}
	if err := applyNCCLWorkerScheduling(runtimeObj, effectiveNodeSelector, effectiveTolerations); err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to apply NCCL worker scheduling", err)
	}
	if err := createUnstructured(ctx.Ctx, dynamicClient, trainingRuntimeGVR, config.Namespace, runtimeObj); err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to apply training runtime", err)
	}
	slog.Info("Applied TrainingRuntime", "service", service)

	// Wait for the runtime to be visible to the Trainer admission webhook.
	// The webhook validates that the referenced runtime exists before allowing
	// TrainJob creation; without this wait we hit a race condition.
	if err := waitForTrainingRuntime(ctx.Ctx, dynamicClient, config.Namespace); err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "TrainingRuntime not ready", err)
	}

	// NVLS variant only: provision an IMEX domain via the NVIDIA DRA driver
	// before the TrainJob fires. The ComputeDomain CR causes the driver to
	// auto-create a ResourceClaimTemplate that runtime-nvls.yaml references;
	// without this, the NVL72 fabric is visible to NCCL but /dev/nvidia-caps-
	// imex-channels isn't mounted into the workers and MNNVL aborts with
	// "Cuda failure 800 'operation not permitted'".
	if variant == variantNVLS {
		if err := applyNCCLComputeDomain(ctx.Ctx, dynamicClient, config.Namespace); err != nil {
			return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to apply ComputeDomain", err)
		}
		if err := waitForIMEXClaimTemplate(ctx.Ctx, dynamicClient, config.Namespace); err != nil {
			return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "IMEX ResourceClaimTemplate not ready", err)
		}
	}

	// Apply shared trainjob: testdata/trainjob.yaml.
	// waitForTrainingRuntime above proved the runtime is visible at the API
	// server, but the Trainer validating webhook resolves runtimeRef against its
	// own informer cache, which lags that read — so the create can still be
	// rejected with "TrainingRuntime not found". applyTrainJobWithRetry retries
	// on exactly that denial until the webhook cache catches up.
	trainjobPath := filepath.Join("testdata", "trainjob.yaml")
	if err := applyTrainJobWithRetry(ctx.Ctx, dynamicClient, config.Namespace, trainjobPath, templateData); err != nil {
		return err
	}
	slog.Info("Applied TrainJob")

	return nil
}

// applyTrainJobWithRetry creates the shared NCCL TrainJob, retrying on the one
// transient failure we cannot eliminate from the client side: the Kubeflow
// Trainer validating webhook (validator.trainjob.trainer.kubeflow.org) rejects
// the TrainJob because its own controller-runtime informer cache has not yet
// observed the TrainingRuntime we just created and confirmed visible via
// waitForTrainingRuntime. The webhook's lister is eventually consistent with the
// API server's strongly-consistent read, and that freshness is not observable
// from here — so a bounded retry (letting the cache catch up) is the only robust
// remedy. Any non-race error is returned immediately.
func applyTrainJobWithRetry(ctx context.Context, dynamicClient dynamic.Interface, namespace, path string, data map[string]string) error {
	obj, err := parseYAMLTemplate(path, data)
	if err != nil {
		return err
	}

	retryCtx, cancel := context.WithTimeout(ctx, defaults.TrainJobAdmissionRetryTimeout)
	defer cancel()

	attempt := 0
	for {
		attempt++
		createErr := createUnstructured(retryCtx, dynamicClient, trainJobGVR, namespace, obj)
		if createErr == nil {
			if attempt > 1 {
				slog.Info("TrainJob created after Trainer webhook cache caught up to the TrainingRuntime",
					"attempts", attempt)
			}
			return nil
		}
		// If the retry budget expired — including while createUnstructured was in
		// flight — classify as timeout rather than leaking whatever error the
		// aborted create returned (which is not the webhook race and would
		// otherwise fall through to the non-race return below with ErrCodeInternal).
		if retryCtx.Err() != nil {
			return aicrErrors.WrapWithContext(aicrErrors.ErrCodeTimeout,
				"timed out applying NCCL TrainJob: Trainer webhook did not admit it within the retry budget",
				createErr, map[string]interface{}{"attempts": attempt})
		}
		if !isTrainingRuntimeNotYetVisible(createErr) {
			// A real failure (or a genuinely missing runtime) — do not mask it.
			return createErr
		}
		slog.Warn("TrainJob rejected: Trainer webhook has not yet observed the TrainingRuntime; retrying",
			"attempt", attempt, "error", createErr)
		select {
		case <-retryCtx.Done():
			return aicrErrors.WrapWithContext(aicrErrors.ErrCodeTimeout,
				"timed out applying NCCL TrainJob: Trainer webhook did not admit it within the retry budget",
				createErr, map[string]interface{}{"attempts": attempt})
		case <-time.After(defaults.TrainJobAdmissionRetryInterval):
		}
	}
}

// isTrainingRuntimeNotYetVisible reports whether err is the Kubeflow Trainer
// webhook's "the referenced TrainingRuntime does not exist yet" denial. On a
// runtime we just created and confirmed present at the API server
// (waitForTrainingRuntime), this denial is a webhook-cache-lag race rather than
// a genuinely missing runtime, so it is safe to retry. Matched primarily by the
// webhook's stable denial phrasing; the fallback is guarded to admission
// rejection reasons so a genuine NotFound / timeout is never mistaken for the
// race. StructuredError implements Unwrap, so the apierrors checks see through
// createUnstructured's wrap.
func isTrainingRuntimeNotYetVisible(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "must be created before the TrainJob is created") {
		return true
	}
	return strings.Contains(msg, ncclTrainingRuntimeName) && strings.Contains(msg, "not found") &&
		(apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) || apierrors.IsForbidden(err))
}

// buildComputeDomain builds the resource.nvidia.com/v1beta1 ComputeDomain CR
// that the NVLS variant applies. Extracted so tests can inspect the shape
// without needing a dynamic client.
//
// Fields:
//   - numNodes=0 because IMEXDaemonsWithDNSNames=true is the default in
//     DRA driver v25.12.0; each IMEX daemon starts immediately rather than
//     waiting for a quorum, and the validator's workers don't gate on it.
//   - channel.allocationMode=Single (one IMEX channel per pod — plenty for
//     a single TrainJob's rank/worker layout).
//   - channel.resourceClaimTemplate.name is stable and matches what
//     runtime-nvls.yaml expects to reference.
func buildComputeDomain(namespace string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "resource.nvidia.com/v1beta1",
		"kind":       "ComputeDomain",
		"metadata": map[string]interface{}{
			keyName:     ncclComputeDomainName,
			"namespace": namespace,
		},
		"spec": map[string]interface{}{
			"numNodes": int64(0),
			"channel": map[string]interface{}{
				"allocationMode": "Single",
				"resourceClaimTemplate": map[string]interface{}{
					"name": ncclIMEXClaimTemplateName,
				},
			},
		},
	}}
}

// applyNCCLComputeDomain creates (or updates) the ComputeDomain CR that
// backs the NVLS variant's IMEX channel access. The DRA driver reconciles
// it into a ResourceClaimTemplate with the same name as
// spec.channel.resourceClaimTemplate.name. Idempotent across reruns: if a
// ComputeDomain with the fixed name already exists (e.g., prior run
// SIGKILL'd before cleanup ran), the spec is updated in place rather than
// failing with AlreadyExists.
func applyNCCLComputeDomain(ctx context.Context, dynamicClient dynamic.Interface, namespace string) error {
	slog.Info("Applying ComputeDomain for NVLS/IMEX access", "namespace", namespace, "name", ncclComputeDomainName)

	applyCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	client := dynamicClient.Resource(computeDomainGVR).Namespace(namespace)
	desired := buildComputeDomain(namespace)

	if _, err := client.Create(applyCtx, desired, metav1.CreateOptions{}); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to create ComputeDomain", err)
	}

	// AlreadyExists: fetch the current resourceVersion and Update in place.
	// Required because Update rejects an empty resourceVersion to prevent
	// lost updates.
	existing, err := client.Get(applyCtx, ncclComputeDomainName, metav1.GetOptions{})
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to get existing ComputeDomain", err)
	}
	desired.SetResourceVersion(existing.GetResourceVersion())
	if _, err := client.Update(applyCtx, desired, metav1.UpdateOptions{}); err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to update ComputeDomain", err)
	}
	slog.Info("Updated existing ComputeDomain in place", "name", ncclComputeDomainName)
	return nil
}

// waitForIMEXClaimTemplate waits until the DRA driver has reconciled the
// ComputeDomain into a ResourceClaimTemplate. Applied TrainJob worker pods
// reference this template by name; if it doesn't exist yet, kubelet rejects
// the pods. Uses the watch API per CLAUDE.md "Kubernetes Patterns".
func waitForIMEXClaimTemplate(ctx context.Context, dynamicClient dynamic.Interface, namespace string) error {
	waitCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	rctClient := dynamicClient.Resource(resourceClaimTemplateGVR).Namespace(namespace)

	// Fast path: the DRA controller may have reconciled the RCT before we
	// reach this wait (common on re-runs, warm clusters).
	if _, err := rctClient.Get(waitCtx, ncclIMEXClaimTemplateName, metav1.GetOptions{}); err == nil {
		slog.Info("IMEX ResourceClaimTemplate ready", "name", ncclIMEXClaimTemplateName)
		return nil
	} else if !apierrors.IsNotFound(err) {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			"failed to get IMEX ResourceClaimTemplate", err)
	}

	watcher, err := rctClient.Watch(waitCtx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + ncclIMEXClaimTemplateName,
	})
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			"failed to watch IMEX ResourceClaimTemplate", err)
	}
	defer watcher.Stop()

	// Re-check after the watch is established: the DRA driver may have
	// reconciled the RCT between the first Get and the Watch call, in which
	// case the watch will not replay the Added event.
	if _, err := rctClient.Get(waitCtx, ncclIMEXClaimTemplateName, metav1.GetOptions{}); err == nil {
		slog.Info("IMEX ResourceClaimTemplate ready", "name", ncclIMEXClaimTemplateName)
		return nil
	} else if !apierrors.IsNotFound(err) {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			"failed to get IMEX ResourceClaimTemplate", err)
	}

	for {
		select {
		case <-waitCtx.Done():
			return aicrErrors.Wrap(aicrErrors.ErrCodeTimeout,
				"timed out waiting for DRA driver to reconcile ComputeDomain into a ResourceClaimTemplate", waitCtx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				if ctxErr := waitCtx.Err(); ctxErr != nil {
					return aicrErrors.Wrap(aicrErrors.ErrCodeTimeout,
						"timed out waiting for DRA driver to reconcile ComputeDomain into a ResourceClaimTemplate", ctxErr)
				}
				// Watch closed without cancellation — re-Get before failing a
				// healthy run, in case the RCT was reconciled during the
				// closure window (apiserver hiccup, LB drop).
				_, getErr := rctClient.Get(waitCtx, ncclIMEXClaimTemplateName, metav1.GetOptions{})
				switch {
				case getErr == nil:
					slog.Info("IMEX ResourceClaimTemplate ready", "name", ncclIMEXClaimTemplateName)
					return nil
				case apierrors.IsNotFound(getErr):
					return aicrErrors.New(aicrErrors.ErrCodeUnavailable,
						"IMEX ResourceClaimTemplate watch channel closed before reconciliation observed")
				case aicrErrors.IsTransient(getErr):
					return aicrErrors.Wrap(aicrErrors.ErrCodeTimeout,
						"IMEX ResourceClaimTemplate watch closed and re-check timed out", getErr)
				default:
					return aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
						"IMEX ResourceClaimTemplate watch closed and re-check failed", getErr)
				}
			}
			if event.Type == watch.Added || event.Type == watch.Modified {
				slog.Info("IMEX ResourceClaimTemplate ready", "name", ncclIMEXClaimTemplateName)
				return nil
			}
		}
	}
}

// parseYAMLTemplate reads a YAML template file, performs ${KEY} substitution,
// and unmarshals it into an unstructured object.
func parseYAMLTemplate(path string, data map[string]string) (*unstructured.Unstructured, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to read template", err)
	}
	yamlContent := string(content)
	for key, value := range data {
		yamlContent = strings.ReplaceAll(yamlContent, "${"+key+"}", value)
	}
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(yamlContent), obj); err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to parse YAML", err)
	}
	return obj, nil
}

// createUnstructured creates a namespaced resource from an unstructured object with a timeout.
func createUnstructured(ctx context.Context, dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) error {
	applyCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()
	_, err := dynamicClient.Resource(gvr).Namespace(namespace).Create(applyCtx, obj, metav1.CreateOptions{})
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to create resource", err)
	}
	return nil
}

// platformWorkerScheduling returns the default nodeSelector and tolerations
// for NCCL worker pods on the given service. instanceType is only used for EKS;
// nodes (the accelerator-narrowed target set from resolveTargetGPUNodes) is
// used for GKE (the gke-accelerator label) and OKE (the shared
// nvidia.com/gpu.product label).
func platformWorkerScheduling(service recipe.CriteriaServiceType, instanceType string, nodes []v1.Node) (map[string]string, []v1.Toleration, error) {
	switch service {
	case recipe.CriteriaServiceEKS:
		return map[string]string{
			"node.kubernetes.io/instance-type": instanceType,
		}, []v1.Toleration{{Operator: v1.TolerationOpExists}}, nil
	case recipe.CriteriaServiceGKE:
		// GKE accelerator node pools carry cloud.google.com/gke-accelerator
		// (set by the node pool spec). Pin workers to the SKU shared by every
		// target node so the selector matches the cohort WorkerCount was sized
		// against — narrowByAccelerator only filters by the H100 product
		// prefix, so a mixed pool (e.g. a3-megagpu-8g + a3-highgpu-1g) can
		// reach this branch. Fail if accelerator labels are missing or mixed
		// to prevent WorkerCount divergence (sizing for N nodes but scheduling
		// to a subset).
		acc := commonGKEAccelerator(nodes)
		if acc == "" {
			return nil, nil, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
				fmt.Sprintf("GKE nodes have missing or mixed %s labels — cannot derive a nodeSelector that matches the full WorkerCount cohort", gkeAcceleratorLabel))
		}
		return map[string]string{gkeAcceleratorLabel: acc}, []v1.Toleration{
			{Operator: v1.TolerationOpExists},
			{Key: "nvidia.com/gpu", Operator: v1.TolerationOpEqual, Value: "present", Effect: v1.TaintEffectNoSchedule},
		}, nil
	case recipe.CriteriaServiceOKE:
		// OKE bare-metal GB200 pools are commonly tainted and may coexist
		// with other GPU shapes under one control plane. Tolerate the pool
		// taint (mirroring EKS/GKE) and pin workers to the same cohort the
		// node count was sized against by reusing the GFD gpu.product label
		// that resolveTargetGPUNodes -> narrowByAccelerator already filtered
		// on. On non-GFD installs no shared product label exists, so emit no
		// selector — matching the counting path's unfiltered fallback so the
		// two stay aligned.
		var nodeSelector map[string]string
		if product := commonGPUProduct(nodes); product != "" {
			nodeSelector = map[string]string{gpuProductLabel: product}
		}
		return nodeSelector, []v1.Toleration{{Operator: v1.TolerationOpExists}}, nil
	case recipe.CriteriaServiceAny, recipe.CriteriaServiceAKS, recipe.CriteriaServiceOCP, recipe.CriteriaServiceKind, recipe.CriteriaServiceLKE, recipe.CriteriaServiceBCM, recipe.CriteriaServiceMetal3:
		return nil, nil, nil
	default:
		return nil, nil, nil
	}
}

// commonGPUProduct returns the nvidia.com/gpu.product label shared by every
// node, or "" when the nodes disagree or any node lacks the label (e.g. a
// non-GFD install). Used to stamp an OKE worker nodeSelector that matches
// exactly the accelerator-narrowed target set resolveTargetGPUNodes counted,
// so worker placement cannot diverge from the sizing. Returning "" on non-GFD
// clusters keeps scheduling aligned with the counting fallback, which also
// returns the unfiltered set when GFD labels are absent.
func commonGPUProduct(nodes []v1.Node) string {
	product := ""
	for _, n := range nodes {
		p := n.Labels[gpuProductLabel]
		if p == "" {
			return ""
		}
		if product == "" {
			product = p
		} else if p != product {
			return ""
		}
	}
	return product
}

// commonGKEAccelerator returns the cloud.google.com/gke-accelerator label
// shared by every node, or "" when the nodes disagree or any node lacks the
// label. Mirrors commonGPUProduct, applied to the GKE-specific label NCCL
// workers must pin to so the nodeSelector matches the same cohort
// WorkerCount was sized against.
func commonGKEAccelerator(nodes []v1.Node) string {
	accelerator := ""
	for _, n := range nodes {
		a := n.Labels[gkeAcceleratorLabel]
		if a == "" {
			return ""
		}
		if accelerator == "" {
			accelerator = a
		} else if a != accelerator {
			return ""
		}
	}
	return accelerator
}

// applyNCCLWorkerScheduling sets the nodeSelector and tolerations on the "node"
// (worker) replicatedJob within a TrainingRuntime unstructured object.
func applyNCCLWorkerScheduling(obj *unstructured.Unstructured, nodeSelector map[string]string, tolerations []v1.Toleration) error {
	replicatedJobs, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "replicatedJobs")
	if err != nil || !found {
		return aicrErrors.New(aicrErrors.ErrCodeInternal, "replicatedJobs not found in TrainingRuntime")
	}

	nodeJobFound := false
	for i, jobRaw := range replicatedJobs {
		jobMap, ok := jobRaw.(map[string]interface{})
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(jobMap, "name")
		if name != nodeJobName {
			continue
		}
		nodeJobFound = true

		// Navigate deep into the worker pod spec.
		workerPodSpec, found := nestedMap(jobMap, "template", "spec", "template", "spec")
		if !found {
			return aicrErrors.New(aicrErrors.ErrCodeInternal, "worker pod spec not found in TrainingRuntime node job")
		}

		if len(nodeSelector) > 0 {
			ns := make(map[string]interface{}, len(nodeSelector))
			for k, v := range nodeSelector {
				ns[k] = v
			}
			workerPodSpec["nodeSelector"] = ns
			slog.Info("Applying NCCL worker nodeSelector", "selector", nodeSelector)
		}

		if len(tolerations) > 0 {
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
			workerPodSpec["tolerations"] = tolList
			slog.Info("Applying NCCL worker tolerations", "count", len(tolerations))
		}

		replicatedJobs[i] = jobMap
		break
	}

	if !nodeJobFound {
		return aicrErrors.New(aicrErrors.ErrCodeInternal, `replicatedJob "node" not found in TrainingRuntime`)
	}

	return unstructured.SetNestedSlice(obj.Object, replicatedJobs, "spec", "template", "spec", "replicatedJobs")
}

// nestedMap navigates a chain of string keys through nested map[string]interface{} values.
// Returns the target map and true if found, nil and false otherwise.
func nestedMap(m map[string]interface{}, keys ...string) (map[string]interface{}, bool) {
	current := m
	for _, key := range keys {
		next, ok := current[key]
		if !ok {
			return nil, false
		}
		nextMap, ok := next.(map[string]interface{})
		if !ok {
			return nil, false
		}
		current = nextMap
	}
	return current, true
}

// waitForLauncherPodAndGetLogs waits for the launcher pod to be created and retrieves logs
func waitForLauncherPodAndGetLogs(ctx *validators.Context, podHelper *helper.PodLifecycle) (string, error) {
	slog.Info("Waiting for launcher pod to be created...")

	// Wait for launcher pod to be created (pattern: nccl-all-reduce-tj-launcher-*)
	launcherPod, err := waitForPodByLabelSelector(
		ctx.Ctx,
		ctx.Clientset,
		ctx.Namespace,
		fmt.Sprintf("jobset.sigs.k8s.io/jobset-name=%s,jobset.sigs.k8s.io/replicatedjob-name=launcher", ncclTrainJobName),
		defaults.NCCLLauncherPodTimeout,
	)
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "failed to find launcher pod", err)
	}

	slog.Info("Found launcher pod", "name", launcherPod.Name)

	// Wait for pod to complete using helper method
	err = podHelper.WaitForPodSuccess(ctx.Ctx, launcherPod, defaults.NCCLTrainJobTimeout)
	if err != nil {
		// Get logs even if pod failed for debugging. Surface (not discard) a
		// log-fetch error: an empty launcher log with no explanation is exactly
		// what makes a launcher "pod failed" impossible to root-cause after the
		// fact. When the launcher log itself is empty/unreadable (mpirun aborted
		// before writing, or the pod was torn down), the true cause is usually on
		// the worker side (sshd never came up, TCPXO sidecar crashed), so pull
		// worker diagnostics too and fold everything into the returned output.
		slog.Info("Pod did not succeed, retrieving logs for debugging...")
		launcherLogs, logErr := podHelper.GetPodLogs(ctx.Ctx, launcherPod)
		// fetchNote records why the direct fetch was unusable so it survives into
		// the emitted diagnostic payload (not just slog) when the termination-tail
		// fallback is also empty — otherwise the reader sees no reason at all.
		var fetchNote string
		switch {
		case logErr != nil:
			slog.Warn("failed to retrieve launcher pod logs", "pod", launcherPod.Name, "error", logErr)
			fetchNote = fmt.Sprintf("direct log fetch failed: %v", logErr)
			launcherLogs = ""
		case launcherLogsUnavailable(launcherLogs):
			// kubelet returned its placeholder ("unable to retrieve container
			// logs ...") as a 200 body, not an error: the container was GC'd
			// before this post-mortem fetch — the JobSet tears the launcher down
			// within ~150ms of failure. Treat as unavailable and fall back below.
			slog.Warn("launcher container logs already GC'd; falling back to termination message", "pod", launcherPod.Name)
			fetchNote = "direct logs unavailable (container GC'd before fetch)"
			launcherLogs = ""
		default:
			// Tail to the same cap as worker diagnostics — a verbose launcher
			// (mpirun + NCCL debug) would otherwise balloon the failure payload.
			launcherLogs = tailLines(strings.TrimSpace(launcherLogs), maxDiagLogLines)
		}

		// When the direct log fetch raced container GC, fall back to the
		// launcher container's termination message. The launcher container sets
		// terminationMessagePolicy: FallbackToLogsOnError, so kubelet captures
		// the tail of its output into pod status on non-zero exit — that lives in
		// the pod object and survives the container GC that GetPodLogs loses to.
		// Either way the fetchNote reason is preserved in the payload.
		if launcherLogs == "" {
			if term := launcherTerminationTail(ctx.Ctx, ctx.Clientset, ctx.Namespace, launcherPod.Name); term != "" {
				launcherLogs = fmt.Sprintf("<%s; container termination-message tail follows>\n%s",
					fetchNote, tailLines(term, maxDiagLogLines))
			} else {
				launcherLogs = fmt.Sprintf("<%s; no termination message captured>", fetchNote)
			}
		}
		workerDiag := collectNCCLWorkerDiagnostics(ctx.Ctx, ctx.Clientset, ctx.Namespace)

		// Surface the diagnostics via slog, not just the return value: every
		// caller on this error path (runNCCLTrainJob, validateNcclAllReduceBw,
		// checkNCCLAllReduceBWVariant) discards the returned logs string, so
		// logging is the only way the launcher/worker failure detail reaches the
		// check's captured stdout (report.json). emitDiagnosticBlock logs each
		// line individually so multi-line output stays readable there instead of
		// collapsing into a single logfmt value.
		slog.Error("NCCL launcher pod failed; dumping diagnostics", "launcherPod", launcherPod.Name)
		emitDiagnosticBlock("launcher "+launcherPod.Name+" logs", launcherLogs)
		emitDiagnosticBlock("worker diagnostics", workerDiag)

		logs := launcherLogs + "\n" + workerDiag
		return logs, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "pod failed to complete successfully", err)
	}

	// Get logs from the completed pod. A pod that has just reached Succeeded can
	// briefly serve an empty or truncated log if its container is being torn down
	// mid-read, and the NCCL results table (which parseBandwidthFromLogs keys on)
	// prints last — so re-read until the results are present before returning.
	slog.Info("Retrieving logs from successful pod...")
	logs, err := getCompleteLauncherLogs(ctx.Ctx, podHelper, launcherPod)
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to get pod logs", err)
	}

	return logs, nil
}

// ncclLauncherLogComplete reports whether a launcher log contains the NCCL
// results parseBandwidthFromLogs needs. all_reduce_perf prints its "Avg bus
// bandwidth" summary line only after the full size sweep finishes, so its
// presence guarantees the trailing largest-message-size row — the row the parser
// keys on (last regexp match) — is already in the log. We deliberately do NOT
// accept a bare data-row match here: an early row can appear while the log is
// still streaming, and gating on it would let the retry loop short-circuit
// before the largest row lands, defeating the purpose (parseBandwidthFromLogs
// would then read a smaller-size row).
func ncclLauncherLogComplete(logs string) bool {
	return strings.Contains(logs, "Avg bus bandwidth")
}

// getCompleteLauncherLogs retrieves the launcher pod's logs, re-reading until the
// NCCL results are present or the attempt budget is exhausted. A pod that has
// just reached Succeeded can serve an empty or truncated log if its container is
// torn down while we read; because the parser keys on the trailing
// largest-message-size row, a truncated read loses exactly that row and yields
// "could not find bandwidth value in logs".
func getCompleteLauncherLogs(ctx context.Context, podHelper *helper.PodLifecycle, pod *v1.Pod) (string, error) {
	return readLauncherLogsUntilComplete(ctx,
		func(c context.Context) (string, error) { return podHelper.GetPodLogs(c, pod) },
		defaults.NCCLLauncherLogReadAttempts, defaults.NCCLLauncherLogReadInterval)
}

// readLauncherLogsUntilComplete re-reads via fetch until ncclLauncherLogComplete
// is satisfied or attempts is exhausted, sleeping interval between tries. It
// returns the last read even when still incomplete, so the caller's parse-failure
// path can surface it for diagnosis rather than discarding it. Split from
// getCompleteLauncherLogs so the retry logic is unit-testable without a cluster.
func readLauncherLogsUntilComplete(ctx context.Context, fetch func(context.Context) (string, error), attempts int, interval time.Duration) (string, error) {
	var logs string
	for attempt := 1; ; attempt++ {
		var err error
		logs, err = fetch(ctx)
		if err != nil {
			return "", err
		}
		if ncclLauncherLogComplete(logs) {
			if attempt > 1 {
				slog.Info("launcher log complete after re-read", "attempts", attempt, "logBytes", len(logs))
			}
			return logs, nil
		}
		if attempt >= attempts {
			slog.Warn("launcher log still lacks NCCL results after re-reads; returning last read for diagnosis",
				"attempts", attempt, "logBytes", len(logs))
			return logs, nil
		}
		slog.Info("launcher log has no NCCL results yet; re-reading", "attempt", attempt, "logBytes", len(logs))
		select {
		case <-ctx.Done():
			// Return what we have; the caller's parse path will surface it.
			return logs, nil
		case <-time.After(interval):
		}
	}
}

// maxDiagLogLines bounds how many trailing log lines are kept per worker
// container in the failure diagnostics. The fatal error is almost always near
// the end, so the tail is what matters; the cap keeps a verbose worker
// (apt-get + NCCL debug output) from ballooning the returned failure payload.
const maxDiagLogLines = 100

// emitDiagnosticBlock writes a labeled, multi-line diagnostic blob to the log
// one line at a time. The check's stdout (captured into report.json) is a
// stream of slog lines, so logging the blob as a single attribute would
// collapse it into one unreadable logfmt value; emitting per line keeps it
// greppable alongside the other progress lines. A blank/whitespace-only block
// is logged as "(empty)" so the absence of output is itself visible.
func emitDiagnosticBlock(label, block string) {
	trimmed := strings.TrimSpace(block)
	if trimmed == "" {
		slog.Error("diagnostics", "section", label, "line", "(empty)")
		return
	}
	for _, line := range strings.Split(trimmed, "\n") {
		slog.Error("diagnostics", "section", label, "line", line)
	}
}

// launcherLogsUnavailable reports whether a GetPodLogs body is really the
// kubelet placeholder for a container whose logs can no longer be served (the
// container was garbage-collected), rather than genuine log output. kubelet
// returns this as a 200 response body, so it arrives as content with no error.
func launcherLogsUnavailable(logs string) bool {
	t := strings.TrimSpace(logs)
	return t == "" || strings.Contains(t, "unable to retrieve container logs")
}

// launcherTerminationTail re-Gets the pod and returns the first terminated
// container's State.Terminated.Message — the tail of that container's own
// output, captured into pod status by kubelet because the launcher container
// sets terminationMessagePolicy: FallbackToLogsOnError. Unlike GetPodLogs, this
// survives the container GC that races a post-mortem log fetch. Best-effort:
// returns "" (never errors) so it can't mask the original failure.
func launcherTerminationTail(ctx context.Context, clientset kubernetes.Interface, namespace, podName string) string {
	getCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	pod, err := clientset.CoreV1().Pods(namespace).Get(getCtx, podName, metav1.GetOptions{})
	if err != nil {
		slog.Warn("failed to re-get launcher pod for termination message", "pod", podName, "error", err)
		return ""
	}
	// Match the launcher's main container by name (nodeJobName). The pod also
	// has a fix-ssh-perms init container; keying by name avoids picking up an
	// unrelated container's message if the status ordering ever changes.
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != nodeJobName {
			continue
		}
		if cs.State.Terminated != nil {
			return strings.TrimSpace(cs.State.Terminated.Message)
		}
	}
	return ""
}

// tailLines returns the last n lines of s (or all of s when it has n or fewer).
func tailLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// collectNCCLWorkerDiagnostics gathers a compact, best-effort summary of the
// NCCL worker pods to explain a launcher failure. Called only on the failure
// path; every step is best-effort and never returns an error (a diagnostic
// helper must not mask the original failure). It reports, per worker pod:
// phase, each container's terminal state (reason/exitCode/message) or waiting
// reason, and the tail of each container's logs. The
// most common root cause — worker sshd never started (slow apt-get, missing
// TCPXO env profile) or the tcpxo-daemon sidecar crashing — shows up here even
// when the launcher's own log is empty.
func collectNCCLWorkerDiagnostics(ctx context.Context, clientset kubernetes.Interface, namespace string) string {
	diagCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	selector := fmt.Sprintf("jobset.sigs.k8s.io/jobset-name=%s,jobset.sigs.k8s.io/replicatedjob-name=%s", ncclTrainJobName, nodeJobName)
	pods, err := clientset.CoreV1().Pods(namespace).List(diagCtx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		slog.Warn("failed to list NCCL worker pods for diagnostics", "error", err)
		return fmt.Sprintf("\n--- worker diagnostics unavailable: %v ---\n", err)
	}
	if len(pods.Items) == 0 {
		return "\n--- no NCCL worker pods found (workers may never have been scheduled) ---\n"
	}

	// Fetch each worker's diagnostics concurrently with bounded concurrency:
	// sequential per-pod log fetches would share the single DiagnosticTimeout,
	// so on a large job later workers could exhaust it and lose diagnostics.
	// Order is preserved via an indexed result slice; workerPodDiagnostics is
	// best-effort and never errors, so g.Wait never cancels the group early.
	sections := make([]string, len(pods.Items))
	g, gctx := errgroup.WithContext(diagCtx)
	g.SetLimit(perNodeFanoutConcurrency)
enqueue:
	for i := range pods.Items {
		// Stop scheduling once the diagnostic budget (diagCtx) expires so a
		// large failed job returns promptly instead of queuing work that would
		// only run against an already-canceled context; note the shortfall in
		// the remaining sections so the truncation is visible, not silent.
		select {
		case <-gctx.Done():
			for j := i; j < len(pods.Items); j++ {
				sections[j] = fmt.Sprintf("worker %s: diagnostics skipped: %v\n", pods.Items[j].Name, gctx.Err())
			}
			break enqueue
		default:
		}
		p := &pods.Items[i]
		g.Go(func() error {
			sections[i] = workerPodDiagnostics(gctx, clientset, namespace, p)
			return nil
		})
	}
	_ = g.Wait()

	var b strings.Builder
	b.WriteString("\n--- NCCL worker pod diagnostics ---\n")
	for _, s := range sections {
		b.WriteString(s)
	}
	return b.String()
}

// workerPodDiagnostics renders the diagnostic section for a single worker pod:
// phase, each container's terminal/waiting/running state, and the tail of each
// container's logs. Best-effort — never errors — so it
// is safe to run under an errgroup that must not cancel on a single pod's log
// fetch failing.
func workerPodDiagnostics(ctx context.Context, clientset kubernetes.Interface, namespace string, p *v1.Pod) string {
	var b strings.Builder
	fmt.Fprintf(&b, "worker %s: phase=%s\n", p.Name, p.Status.Phase)
	// Combine init (native sidecars like tcpxo-daemon) and main container
	// statuses into a fresh slice — appending into p.Status.InitContainerStatuses
	// directly could mutate the pod's backing array.
	statuses := make([]v1.ContainerStatus, 0, len(p.Status.InitContainerStatuses)+len(p.Status.ContainerStatuses))
	statuses = append(statuses, p.Status.InitContainerStatuses...)
	statuses = append(statuses, p.Status.ContainerStatuses...)
	for _, cs := range statuses {
		switch {
		case cs.State.Terminated != nil:
			t := cs.State.Terminated
			fmt.Fprintf(&b, "  container %s: terminated reason=%s exitCode=%d %s\n",
				cs.Name, t.Reason, t.ExitCode, strings.TrimSpace(t.Message))
		case cs.State.Waiting != nil:
			w := cs.State.Waiting
			fmt.Fprintf(&b, "  container %s: waiting reason=%s %s\n",
				cs.Name, w.Reason, strings.TrimSpace(w.Message))
		case cs.State.Running != nil:
			fmt.Fprintf(&b, "  container %s: running (ready=%t)\n", cs.Name, cs.Ready)
		}
	}
	// Best-effort container logs for every container in the pod spec (init
	// sidecars like GKE's tcpxo-daemon plus the main "node" worker). Deriving
	// the names from the spec — rather than hardcoding "node"/"tcpxo-daemon" —
	// keeps this correct on every platform's launcher-failure path: a non-GKE
	// worker has no tcpxo-daemon, so a hardcoded list would emit a spurious
	// "container not found" line, and a template that renames its sidecar would
	// silently lose that log. GetPodLogs streams the full log — a verbose
	// NCCL/apt-get worker can emit thousands of lines — so tail each container
	// to the last maxDiagLogLines. The tail (not the head) is kept because the
	// fatal error is almost always the last output.
	containers := make([]string, 0, len(p.Spec.InitContainers)+len(p.Spec.Containers))
	for _, c := range p.Spec.InitContainers {
		containers = append(containers, c.Name)
	}
	for _, c := range p.Spec.Containers {
		containers = append(containers, c.Name)
	}
	for _, container := range containers {
		logs, logErr := k8spod.GetPodLogs(ctx, clientset, namespace, p.Name, container)
		if logErr != nil {
			fmt.Fprintf(&b, "  [%s logs unavailable: %v]\n", container, logErr)
			continue
		}
		if trimmed := strings.TrimSpace(logs); trimmed != "" {
			fmt.Fprintf(&b, "  --- %s/%s logs (last %d lines) ---\n%s\n",
				p.Name, container, maxDiagLogLines, tailLines(trimmed, maxDiagLogLines))
		}
	}
	return b.String()
}

// waitForTrainingRuntime waits until the TrainingRuntime is visible via the
// Trainer admission webhook. The webhook validates that the referenced
// runtime exists before allowing TrainJob creation; a brief propagation
// delay can cause a race. Uses the watch API per CLAUDE.md "Kubernetes
// Patterns" and mirrors waitForIMEXClaimTemplate above.
func waitForTrainingRuntime(ctx context.Context, dynamicClient dynamic.Interface, namespace string) error {
	waitCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	runtimeClient := dynamicClient.Resource(trainingRuntimeGVR).Namespace(namespace)

	// Fast path: runtime may already be visible (warm cluster / re-run).
	if _, err := runtimeClient.Get(waitCtx, ncclTrainingRuntimeName, metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to get TrainingRuntime", err)
	}

	watcher, err := runtimeClient.Watch(waitCtx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + ncclTrainingRuntimeName,
	})
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to watch TrainingRuntime", err)
	}
	defer watcher.Stop()

	// Re-check after the watch is established: the runtime may have become
	// visible between the first Get and the Watch call, in which case the
	// watch will not replay the Added event.
	if _, err := runtimeClient.Get(waitCtx, ncclTrainingRuntimeName, metav1.GetOptions{}); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to get TrainingRuntime", err)
	}

	for {
		select {
		case <-waitCtx.Done():
			return aicrErrors.Wrap(aicrErrors.ErrCodeTimeout,
				"timed out waiting for TrainingRuntime to be visible", waitCtx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				if ctxErr := waitCtx.Err(); ctxErr != nil {
					return aicrErrors.Wrap(aicrErrors.ErrCodeTimeout,
						"timed out waiting for TrainingRuntime to be visible", ctxErr)
				}
				// Watch closed without cancellation — re-Get before failing,
				// in case the runtime became visible during the closure window.
				if _, getErr := runtimeClient.Get(waitCtx, ncclTrainingRuntimeName, metav1.GetOptions{}); getErr == nil {
					return nil
				}
				return aicrErrors.New(aicrErrors.ErrCodeUnavailable,
					"TrainingRuntime watch channel closed before it became visible")
			}
			if event.Type == watch.Added || event.Type == watch.Modified {
				return nil
			}
		}
	}
}

// waitForPodByLabelSelector waits for a pod matching the label selector to be created.
// Uses the Watch API for efficiency instead of polling.
func waitForPodByLabelSelector(ctx context.Context, clientset kubernetes.Interface, namespace, labelSelector string, timeout time.Duration) (*v1.Pod, error) {
	slog.Info("Watching for pod with selector", "selector", labelSelector)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	watcher, err := clientset.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to watch pods", err)
	}
	defer watcher.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "timeout waiting for pod", ctx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "timeout waiting for pod", ctxErr)
				}
				// Watch closed without cancellation — re-List before failing,
				// in case the pod was created during the closure window.
				if pods, listErr := clientset.CoreV1().Pods(namespace).List(ctx,
					metav1.ListOptions{LabelSelector: labelSelector}); listErr == nil {
					if p := newestRunnablePod(pods.Items); p != nil {
						slog.Info("Found launcher pod", "name", p.Name)
						return p, nil
					}
				}
				return nil, aicrErrors.New(aicrErrors.ErrCodeUnavailable,
					"pod watch channel closed before a matching pod appeared")
			}
			pod, ok := event.Object.(*v1.Pod)
			if !ok {
				continue
			}
			slog.Info("Found launcher pod", "name", pod.Name)
			return pod, nil
		}
	}
}

// newestRunnablePod returns the youngest non-terminating pod from a label
// selector List, skipping pods being deleted or in a terminal phase
// (Succeeded/Failed). Used to recover after a watch channel closes without
// handing back a completed pod from a prior run. Returns nil when no viable
// pod is present.
func newestRunnablePod(pods []v1.Pod) *v1.Pod {
	var best *v1.Pod
	for i := range pods {
		p := &pods[i]
		phase := p.Status.Phase
		if p.DeletionTimestamp != nil || phase == v1.PodFailed || phase == v1.PodSucceeded {
			continue
		}
		if best == nil || p.CreationTimestamp.After(best.CreationTimestamp.Time) {
			best = p
		}
	}
	return best
}

// parseBandwidthFromLogs extracts the bus bandwidth value from NCCL test logs.
// It finds all data rows and returns the out-of-place busbw from the last row
// (largest message size). This works regardless of max message size:
// EKS uses 16G (17179869184), GKE uses 8G (8589934592).
func parseBandwidthFromLogs(logs string) (float64, error) {
	// NCCL test output format example:
	// #       size         count      type   redop    root     time   algbw   busbw #wrong     time   algbw   busbw #wrong
	// #        (B)    (elements)                               (us)  (GB/s)  (GB/s)            (us)  (GB/s)  (GB/s)
	//  8589934592    2147483648     float     sum      -1   48298   177.85  333.47      0   48292   177.87  333.51      0

	allMatches := ncclBandwidthRe.FindAllStringSubmatch(logs, -1)
	if len(allMatches) == 0 {
		return 0, aicrErrors.New(aicrErrors.ErrCodeInternal, "could not find bandwidth value in logs")
	}

	// Last match = largest message size row.
	lastMatch := allMatches[len(allMatches)-1]
	slog.Info("Parsing bandwidth from largest message size row", "bytes", lastMatch[1])

	bandwidth, err := strconv.ParseFloat(lastMatch[2], 64)
	if err != nil {
		return 0, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to parse bandwidth value", err)
	}

	return bandwidth, nil
}

// verifyTransportFromLogs asserts that the fabric implied by the variant actually
// carried NCCL traffic, based on the channel-assignment lines NCCL emits when
// NCCL_DEBUG=INFO. Returns nil for variantDefault (no assertion, legacy behavior).
//
// Without this check, a misconfigured cluster would silently report a passing
// bandwidth number from whatever transport NCCL happened to select.
func verifyTransportFromLogs(logs string, variant ncclVariant) error {
	switch variant {
	case variantDefault:
		return nil
	case variantNET:
		m := ncclUsingNetRe.FindStringSubmatch(logs)
		if len(m) < 2 {
			return aicrErrors.New(aicrErrors.ErrCodeInternal,
				"NET variant selected but no 'NCCL INFO Using network <plugin>' banner found — "+
					"the network transport plugin did not initialize (check NCCL_NET_PLUGIN and that the provider "+
					"plugin .so is on LD_LIBRARY_PATH)")
		}
		// Anything other than Socket implies a real NET transport engaged
		// (AWS Libfabric on EKS, IB/RoCE on-prem, TCPXO on GKE, etc.).
		// Socket-only is the catch-all slow path and usually means the
		// intended plugin failed to load — fail loudly so a misconfigured
		// EFA stack doesn't silently report sub-NET-grade bandwidth.
		if m[1] == "Socket" {
			return aicrErrors.New(aicrErrors.ErrCodeInternal,
				"NET variant selected but NCCL fell back to 'Using network Socket' — "+
					"provider plugin (e.g. AWS Libfabric) did not load")
		}
		return nil
	case variantNVLS:
		if !ncclNVLSAvailableRe.MatchString(logs) {
			return aicrErrors.New(aicrErrors.ErrCodeInternal,
				"NVLS variant selected but 'NVLS multicast support is available' banner not found in NCCL logs — "+
					"MNNVL did not initialize (check DRA IMEX channel claim and NCCL_NVLS_ENABLE=1)")
		}
		// NCCL 2.27+ no longer emits per-channel "via NVLS" lines. The
		// authoritative post-init signal is the NVLS communicator log
		// ("NVLS comm 0x<addr> headRank N nHeads M ...") which is only
		// emitted when NCCL actually constructs an NVLS communicator for
		// collective ops. If the availability banner appears but the comm
		// init doesn't, NVLS was detected but not used — fail loudly.
		if !ncclNVLSCommInitRe.MatchString(logs) {
			return aicrErrors.New(aicrErrors.ErrCodeInternal,
				"NVLS variant selected but no 'NVLS comm 0x<addr>' init line found in NCCL logs — "+
					"NVLS was available but NCCL did not build an NVLS communicator (check for 'NVLS_NCHANNELS' > 0 and no NVLS-disabling env overrides)")
		}
		return nil
	default:
		return nil
	}
}

// cleanupNCCLResources removes the trainjob, runtime, and (if present) the
// ComputeDomain CR using the dynamic client. Deleting the ComputeDomain
// cascades to its auto-generated ResourceClaimTemplate via the DRA driver;
// NotFound on the ComputeDomain is expected for the default/NET variants
// and is logged at debug rather than error.
func cleanupNCCLResources(dynamicClient dynamic.Interface, namespace string) {
	slog.Info("Cleaning up NCCL test resources...")

	cleanupCtx, cancel := context.WithTimeout(context.Background(), defaults.DiagnosticTimeout)
	defer cancel()

	// Delete trainjob. NotFound is expected and logged at debug: this runs as a
	// deferred cleanup registered before the apply, so an early/partial-apply
	// failure (or the install-trainer path, where deleteTrainer may already have
	// cascade-removed the CRs) legitimately leaves no TrainJob to delete.
	err := dynamicClient.Resource(trainJobGVR).Namespace(namespace).Delete(cleanupCtx, ncclTrainJobName, metav1.DeleteOptions{})
	switch {
	case err == nil:
		slog.Info("Deleted TrainJob")
	case apierrors.IsNotFound(err):
		slog.Debug("TrainJob not present, skipping", "name", ncclTrainJobName)
	default:
		slog.Warn("failed to delete TrainJob", "error", err)
	}

	// Delete runtime. NotFound is expected and logged at debug (see TrainJob above).
	err = dynamicClient.Resource(trainingRuntimeGVR).Namespace(namespace).Delete(cleanupCtx, ncclTrainingRuntimeName, metav1.DeleteOptions{})
	switch {
	case err == nil:
		slog.Info("Deleted TrainingRuntime")
	case apierrors.IsNotFound(err):
		slog.Debug("TrainingRuntime not present, skipping", "name", ncclTrainingRuntimeName)
	default:
		slog.Warn("failed to delete TrainingRuntime", "error", err)
	}

	// Delete ComputeDomain if this was the NVLS variant. NotFound is the
	// expected path for default/NET and is ignored here; other errors bubble
	// up as a warning because the RCT and IMEX daemons otherwise leak.
	err = dynamicClient.Resource(computeDomainGVR).Namespace(namespace).Delete(cleanupCtx, ncclComputeDomainName, metav1.DeleteOptions{})
	switch {
	case err == nil:
		slog.Info("Deleted ComputeDomain")
	case apierrors.IsNotFound(err):
		slog.Debug("ComputeDomain not present (non-NVLS variant), skipping", "name", ncclComputeDomainName)
	default:
		slog.Warn("failed to delete ComputeDomain", "error", err, "name", ncclComputeDomainName)
	}

	// Delete the RoCE ResourceClaimTemplate (RoCE NET variant only). The
	// validator namespace is persistent and reused across runs, so leaving it
	// behind makes the next RoCE run fail with AlreadyExists when
	// applyNCCLResources re-creates it. NotFound is expected for EFA/NVLS runs.
	err = dynamicClient.Resource(resourceClaimTemplateGVR).Namespace(namespace).Delete(cleanupCtx, ncclRoceClaimName, metav1.DeleteOptions{})
	switch {
	case err == nil:
		slog.Info("Deleted RoCE ResourceClaimTemplate", "name", ncclRoceClaimName)
	case apierrors.IsNotFound(err):
		slog.Debug("RoCE ResourceClaimTemplate not present (non-RoCE variant), skipping", "name", ncclRoceClaimName)
	default:
		slog.Warn("failed to delete RoCE ResourceClaimTemplate", "error", err, "name", ncclRoceClaimName)
	}
}
