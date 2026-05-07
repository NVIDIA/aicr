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

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrErrors "github.com/NVIDIA/aicr/pkg/errors"
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
		Version:  "v1alpha1",
		Resource: "trainjobs",
	}

	trainingRuntimeGVR = schema.GroupVersionResource{
		Group:    "trainer.kubeflow.org",
		Version:  "v1alpha1",
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
	// field in testdata/gb200/eks/runtime-nvls.yaml — the DRA driver uses
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
func templatePath(accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType, variant ncclVariant, filename string) string {
	if variant != variantDefault {
		ext := filepath.Ext(filename)
		stem := strings.TrimSuffix(filename, ext)
		filename = stem + "-" + string(variant) + ext
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
		recipe.CriteriaServiceEKS: {recipe.CriteriaAcceleratorH100},
		recipe.CriteriaServiceGKE: {recipe.CriteriaAcceleratorH100},
		recipe.CriteriaServiceAny: {recipe.CriteriaAcceleratorB200, recipe.CriteriaAcceleratorGB200},
	},
	variantNET: {
		recipe.CriteriaServiceEKS: {recipe.CriteriaAcceleratorGB200},
	},
	variantNVLS: {
		recipe.CriteriaServiceEKS: {recipe.CriteriaAcceleratorGB200},
	},
}

// validateNcclAllReduceBw validates NCCL All Reduce bandwidth using Kubeflow TrainJob + MPI.
// Each platform has its own TrainingRuntime; the TrainJob is shared (just runtimeRef + numNodes).
// The variant selects a transport-class template (NET, NVLS) when the recipe needs per-fabric
// coverage on clusters that expose multiple inter-node fabrics (e.g. GB200/EKS).
// Returns actual bandwidth value, whether it passed the threshold, and any error.
func validateNcclAllReduceBw(ctx *validators.Context, constraint recipe.Constraint, variant ncclVariant) (string, bool, error) {
	slog.Info("Starting NCCL All Reduce bandwidth validation", "variant", string(variant))

	// Skip unless the recipe targets a supported service + accelerator combination.
	if ctx.Recipe == nil || ctx.Recipe.Criteria == nil {
		slog.Info("Skipping NCCL All Reduce bandwidth validation: no recipe criteria")
		return "skipped - requires Service + Accelerator", true, nil
	}

	service := ctx.Recipe.Criteria.Service
	accelerator := ctx.Recipe.Criteria.Accelerator

	supported := false
	if byService, ok := supportedNCCLCombinations[variant]; ok {
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
			"variant", string(variant), "service", service, "accelerator", accelerator)
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
	if gb200NetPreflightApplies(variant, accelerator, service) {
		if pfErr := preflightGB200NetNVregFlag(ctx, gpuConfig.Nodes); pfErr != nil {
			return "", false, pfErr
		}
	}

	// Run the NCCL all-reduce benchmark using Kubeflow TrainJob + MPI.
	// Each platform has a per-platform TrainingRuntime with all platform-specific
	// configuration (image, mpirun args, resources, sidecars). The TrainJob is shared.
	logs, err := runNCCLTrainJob(ctx, gpuConfig, accelerator, service, variant)
	if err != nil {
		return "", false, err
	}

	// Parse bandwidth from logs (shared across all service types).
	bandwidth, err := parseBandwidthFromLogs(logs)
	if err != nil {
		return logs, false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to parse bandwidth from logs", err)
	}

	// For named variants, assert the expected transport actually carried traffic.
	// This turns the variant label into a hard guarantee — a GB200/EKS cluster
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
	accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType, variant ncclVariant) (string, error) {

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

	// Apply runtime and trainjob resources.
	if applyErr := applyNCCLResources(ctx, dynamicClient, gpuConfig, accelerator, service, variant); applyErr != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to apply NCCL resources", applyErr)
	}
	defer cleanupNCCLResources(dynamicClient, gpuConfig.Namespace)

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
// accelerators with multiple concrete SKUs (H100 SXM/PCIe/NVL, A100 SXM/PCIe).
// No entry for CriteriaAcceleratorAny — "any" deliberately skips the filter.
var acceleratorProductMatchers = map[recipe.CriteriaAcceleratorType]func(string) bool{
	recipe.CriteriaAcceleratorGB200:      func(s string) bool { return s == "NVIDIA-GB200" },
	recipe.CriteriaAcceleratorB200:       func(s string) bool { return s == "NVIDIA-B200" },
	recipe.CriteriaAcceleratorH100:       func(s string) bool { return strings.HasPrefix(s, "NVIDIA-H100-") },
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
func applyNCCLResources(ctx *validators.Context, dynamicClient dynamic.Interface, config *gpuConfiguration, accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType, variant ncclVariant) error {
	slog.Info("Applying NCCL test resources...", "accelerator", accelerator, "service", service, "variant", string(variant))

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

	// Build effective worker scheduling: user override takes precedence over platform default.
	defaultNodeSelector, defaultTolerations := platformWorkerScheduling(service, instanceType)
	effectiveNodeSelector := defaultNodeSelector
	if ctx.NodeSelector != nil {
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

	runtimeObj, err := parseYAMLTemplate(templatePath(accelerator, service, variant, "runtime.yaml"), templateData)
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

	// Apply shared trainjob: testdata/trainjob.yaml
	trainjobPath := filepath.Join("testdata", "trainjob.yaml")
	if err := applyYAMLWithDynamicClient(ctx.Ctx, dynamicClient, trainJobGVR, config.Namespace, trainjobPath, templateData); err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to apply train job", err)
	}
	slog.Info("Applied TrainJob")

	return nil
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
			"name":      ncclComputeDomainName,
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
				return aicrErrors.New(aicrErrors.ErrCodeInternal,
					"IMEX ResourceClaimTemplate watch channel closed unexpectedly")
			}
			if event.Type == watch.Added || event.Type == watch.Modified {
				slog.Info("IMEX ResourceClaimTemplate ready", "name", ncclIMEXClaimTemplateName)
				return nil
			}
		}
	}
}

// applyYAMLWithDynamicClient reads a YAML template, performs substitution, and applies it using dynamic client
func applyYAMLWithDynamicClient(ctx context.Context, dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, namespace, path string, data map[string]string) error {
	obj, err := parseYAMLTemplate(path, data)
	if err != nil {
		return err
	}
	return createUnstructured(ctx, dynamicClient, gvr, namespace, obj)
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
// for NCCL worker pods on the given service. instanceType is only used for EKS.
func platformWorkerScheduling(service recipe.CriteriaServiceType, instanceType string) (map[string]string, []v1.Toleration) {
	switch service {
	case recipe.CriteriaServiceEKS:
		return map[string]string{
			"node.kubernetes.io/instance-type": instanceType,
		}, []v1.Toleration{{Operator: v1.TolerationOpExists}}
	case recipe.CriteriaServiceGKE:
		return map[string]string{
				"cloud.google.com/gke-accelerator": "nvidia-h100-mega-80gb",
			}, []v1.Toleration{
				{Operator: v1.TolerationOpExists},
				{Key: "nvidia.com/gpu", Operator: v1.TolerationOpEqual, Value: "present", Effect: v1.TaintEffectNoSchedule},
			}
	case recipe.CriteriaServiceAny, recipe.CriteriaServiceAKS, recipe.CriteriaServiceOKE, recipe.CriteriaServiceKind, recipe.CriteriaServiceLKE:
		return nil, nil
	default:
		return nil, nil
	}
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
		if name != "node" {
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
		// Get logs even if pod failed for debugging
		slog.Info("Pod did not succeed, retrieving logs for debugging...")
		logs, _ := podHelper.GetPodLogs(ctx.Ctx, launcherPod)
		return logs, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "pod failed to complete successfully", err)
	}

	// Get logs from completed pod using helper method
	slog.Info("Retrieving logs from successful pod...")
	logs, err := podHelper.GetPodLogs(ctx.Ctx, launcherPod)
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to get pod logs", err)
	}

	return logs, nil
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
				return aicrErrors.New(aicrErrors.ErrCodeInternal,
					"TrainingRuntime watch channel closed unexpectedly")
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
				return nil, aicrErrors.New(aicrErrors.ErrCodeInternal, "pod watch channel closed unexpectedly")
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

	// Delete trainjob
	err := dynamicClient.Resource(trainJobGVR).Namespace(namespace).Delete(cleanupCtx, ncclTrainJobName, metav1.DeleteOptions{})
	if err != nil {
		slog.Warn("failed to delete TrainJob", "error", err)
	} else {
		slog.Info("Deleted TrainJob")
	}

	// Delete runtime
	err = dynamicClient.Resource(trainingRuntimeGVR).Namespace(namespace).Delete(cleanupCtx, ncclTrainingRuntimeName, metav1.DeleteOptions{})
	if err != nil {
		slog.Warn("failed to delete TrainingRuntime", "error", err)
	} else {
		slog.Info("Deleted TrainingRuntime")
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
}
