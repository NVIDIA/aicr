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
	stderrors "errors"
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
	"github.com/NVIDIA/aicr/pkg/validator/labels"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	"github.com/NVIDIA/aicr/validators/internal/gkenet"
	coordinationv1 "k8s.io/api/coordination/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
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

	// ncclWorkloadNamespacePrefix is the base for the per-run, per-variant
	// benchmark namespace (see ncclRunNamespace), the same isolation pattern
	// inferenceWorkloadNamespacePrefix uses so fixed resource names never
	// collide across concurrent or crashed runs, and cleanup is one
	// namespace delete instead of per-resource tracking.
	ncclWorkloadNamespacePrefix = "aicr-nccl-perf"
)

// ncclRunNamespace derives the per-run, per-variant benchmark namespace.
// All three catalog checks share one AICR_RUN_ID per invocation, so folding
// the variant into the name keeps each check's namespace, and its fixed
// resource names, unique from its siblings once they can run concurrently
// (see pkg/validator's TODO(perf)).
func ncclRunNamespace(variant ncclVariant) string {
	variantLabel := string(variant)
	if variantLabel == "" {
		variantLabel = "default"
	}
	return fmt.Sprintf("%s-%s-%s", ncclWorkloadNamespacePrefix, variantLabel, deriveRunID())
}

// skipMsg* are the constraint-result strings returned when the NCCL check cannot
// run. The "skipped " prefix is contractual: nccl_all_reduce_bw.go:78 dispatches
// on it via strings.HasPrefix(actual, "skipped") to emit CTRF Skip status.
const (
	skipMsgNCCLNoInput        = "skipped - requires Service + Accelerator"
	skipMsgNCCLNotImplemented = "skipped - requires Service + Accelerator to be implemented"
	skipMsgNCCLFewNodes       = "skipped - requires at least 2 GPU nodes for EW fabric test"
)

// skipMsgNCCLNoProfile returns the skip result when the resolved benchmark
// profile does not implement the requested NCCL variant.
func skipMsgNCCLNoProfile(target ncclBenchmarkTarget, variant ncclVariant) string {
	return fmt.Sprintf("skipped - benchmark profile %s does not implement the %s NCCL variant",
		target.String(), ncclVariantDisplayName(variant))
}

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
//
// This matrix is the criteria-derived DEFAULT applicability. A recipe whose
// criteria are not listed here (e.g. a service registered only via --data)
// can still run these benchmarks by naming one of the listed tuples through
// the nccl-benchmark-profile performance constraint — see
// nccl_benchmark_profile.go and NVIDIA/aicr#1703.
var supportedNCCLCombinations = map[ncclVariant]map[recipe.CriteriaServiceType][]recipe.CriteriaAcceleratorType{
	variantDefault: {
		// H200 is Hopper on EFA, electrically identical to H100 for NCCL
		// (NVLink4 intra-node, EFA inter-node), so it reuses the EKS H100
		// runtime template and the same calibrated >= 300 GB/s floor.
		recipe.CriteriaServiceEKS: {recipe.CriteriaAcceleratorH100, recipe.CriteriaAcceleratorH200},
		recipe.CriteriaServiceGKE: {recipe.CriteriaAcceleratorH100},
		// AKS ND-series H100 (e.g. Standard_ND96isr_H100_v5): 8x H100 SXM
		// intra-node NVLink, 8x 400Gb NDR InfiniBand inter-node via the
		// network-operator rdma-shared-device-plugin. NCCL uses its built-in
		// IB/verbs transport (see testdata/h100/aks/runtime.yaml).
		recipe.CriteriaServiceAKS: {recipe.CriteriaAcceleratorH100},
		recipe.CriteriaServiceAny: {recipe.CriteriaAcceleratorB200, recipe.CriteriaAcceleratorGB200},
	},
	variantNET: {
		recipe.CriteriaServiceEKS: {recipe.CriteriaAcceleratorGB200},
		// OKE GB200 NVL72: IB east-west (rdma0-3) via the
		// rdmaSharedDevicePlugin's nvidia.com/mlnxnics shared HCAs —
		// see testdata/gb200/oke/runtime-net.yaml.
		recipe.CriteriaServiceOKE: {recipe.CriteriaAcceleratorGB200},
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
// Applicability derives from the recipe criteria via supportedNCCLCombinations, overridable
// through the nccl-benchmark-profile constraint (see nccl_benchmark_profile.go).
// Returns actual bandwidth value, whether it passed the threshold, and any error.
func validateNcclAllReduceBw(ctx *validators.Context, constraint recipe.Constraint, variant ncclVariant) (string, bool, error) {
	slog.Info("Starting NCCL All Reduce bandwidth validation", "variant", string(variant))

	// Skip unless the validation targets a supported service + accelerator combination.
	if ctx.ValidationInput == nil {
		slog.Info("Skipping NCCL All Reduce bandwidth validation: no validation")
		return skipMsgNCCLNoInput, true, nil
	}

	service := ctx.ValidationInput.Criteria.Service
	accelerator := ctx.ValidationInput.Criteria.Accelerator

	// The benchmark target defaults to the recipe's criteria; an explicit
	// nccl-benchmark-profile constraint overrides it so recipes whose criteria
	// are absent from the compiled matrix (external --data services, new
	// accelerators) can opt into an embedded benchmark profile. The target
	// keys applicability, template selection, service-specific fabric
	// plumbing, and preflights; node identification below keeps using the
	// criteria accelerator.
	target := ncclBenchmarkTarget{accelerator: accelerator, service: service}
	profile, err := resolveNCCLBenchmarkProfile(ctx)
	if err != nil {
		return "", false, err
	}

	// A recipe may supply its own TrainingRuntime (nccl-benchmark-runtime) when
	// its criteria pair has neither a compiled matrix entry nor an embedded
	// template to borrow — completing the data-driven vision for private
	// --data-only services (NVIDIA/aicr#1792). It is mutually exclusive with the
	// borrow-an-embedded-template profile: a recipe supplies its own runtime OR
	// names an embedded one, never both.
	customRuntime, err := resolveNCCLBenchmarkRuntime(ctx)
	if err != nil {
		return "", false, err
	}
	if customRuntime != "" && profile != nil {
		return "", false, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s (or %s) and %s are mutually exclusive: supply a runtime or borrow an embedded profile, not both",
				perfConstraintNCCLBenchmarkRuntime, perfConstraintNCCLBenchmarkRuntimeRef, perfConstraintNCCLBenchmarkProfile))
	}

	// Fabric selection (AICR_NCCL_FABRIC) governs only the baked-in template path
	// — EFA vs ConnectX-RoCE NIC wiring and the transport-variant template. A
	// recipe-supplied runtime owns its own fabric end to end, so a malformed
	// AICR_NCCL_FABRIC must not fail it; validate the env only when no custom
	// runtime is in play. The default is inconsequential on the custom path
	// (every fabric-dependent branch below is gated on customRuntime == "").
	fabric := fabricEFA
	if customRuntime == "" {
		fabric, err = ncclFabric()
		if err != nil {
			return "", false, err
		}
	}

	// AICR_NCCL_RUNTIME_IMAGE governs only the baked-in template path, mirroring
	// AICR_NCCL_FABRIC above: a recipe-supplied runtime owns its own workload
	// image end to end (issue #1751), so a malformed override must not fail it.
	// Validated up front — before any cluster discovery or TrainJob spend — so
	// a typo'd image reference fails fast rather than after minutes of setup.
	runtimeImage := ""
	if customRuntime == "" {
		runtimeImage, err = resolveNCCLRuntimeImage()
		if err != nil {
			return "", false, err
		}
	}

	if profile != nil {
		target = *profile
		slog.Info("Recipe declares an NCCL benchmark profile — overriding criteria-derived applicability",
			"profile", target.String(), "criteriaService", service, "criteriaAccelerator", accelerator)
	}

	// A recipe-supplied runtime grants applicability on its own: the recipe
	// explicitly opted in with a complete TrainingRuntime, keyed on its own
	// criteria, so the compiled applicability gate (which governs only the
	// criteria/profile → embedded-template paths) is bypassed. The supplied
	// runtime owns its fabric wiring, so service-specific NIC discovery,
	// preflights, and NVLS/IMEX provisioning are skipped further below.
	if customRuntime != "" {
		slog.Info("Recipe supplies its own NCCL benchmark runtime — bypassing compiled applicability and service-specific fabric plumbing",
			"criteriaService", service, "criteriaAccelerator", accelerator, "variant", string(variant))
	} else if !ncclCombinationSupported(variant, fabric, target) {
		slog.Info("Skipping NCCL All Reduce bandwidth validation: unsupported variant/service/accelerator combination",
			"variant", string(variant), "target", target.String(), "fromProfile", target.fromProfile, "fabric", string(fabric))
		if target.fromProfile {
			// The profile itself is valid (resolveNCCLBenchmarkProfile fails
			// closed on unknown pairs); it just doesn't implement this variant
			// — e.g. gb200/eks covers net and nvls but not the default check.
			return skipMsgNCCLNoProfile(target, variant), true, nil
		}
		return skipMsgNCCLNotImplemented, true, nil
	}

	// Extract threshold from constraint
	threshold, err := parseThreshold(constraint.Value)
	if err != nil {
		return "", false, aicrErrors.Wrap(aicrErrors.ErrCodeInvalidRequest, "invalid threshold", err)
	}
	slog.Info("Target bandwidth threshold", "threshold", threshold, "tolerance", "10%")

	// Size the worker cohort against the same nodes the workers will actually
	// land on. Precedence matches applyNCCLWorkerScheduling: a user
	// --node-selector wins; otherwise a recipe-supplied runtime keeps its own
	// worker nodeSelector, so size against that too — else WorkerCount would
	// count nodes the runtime's selector then excludes, leaving workers Pending.
	//
	// Scope: only nodeSelector is factored in. Richer scheduling constraints in
	// a custom runtime (nodeAffinity, un-tolerated taints) are the scheduler's
	// job to enforce, not the validator's to simulate; a runtime that narrows
	// placement below the sized cohort fails SAFE — workers stay Pending and the
	// launcher wait times out with a diagnostic, never a false pass. Operators
	// wanting the sized and scheduled cohorts to match exactly pass
	// --node-selector.
	sizingSelector := ctx.NodeSelector
	if customRuntime != "" && len(sizingSelector) == 0 {
		rs, rsErr := customRuntimeNodeSelector(customRuntime)
		if rsErr != nil {
			return "", false, rsErr
		}
		sizingSelector = rs
	}

	// Determine GPU configuration from cluster. The service comes from the
	// benchmark target (an EKS-profiled cluster gets the EKS instance-type
	// narrowing) but the accelerator stays the recipe's own criteria value:
	// the GFD gpu.product node filter identifies the cluster's hardware, and
	// a profile naming gb200 must not filter a cluster of an unmatched newer
	// accelerator down to zero nodes.
	gpuConfig, err := determineGPUConfig(ctx, target.service, accelerator, sizingSelector)
	if err != nil {
		return "", false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to determine GPU configuration", err)
	}
	slog.Info("GPU Configuration", "nodes", gpuConfig.WorkerCount, ", GPUs/node", gpuConfig.GPUCountPerNode, ", total GPUs", gpuConfig.TotalGPUCount)

	// NCCL all-reduce tests EW (East-West) fabric between nodes and requires at least
	// two GPU nodes. Skip gracefully rather than fail when only one node is available.
	if gpuConfig.WorkerCount < 2 {
		slog.Info("Skipping NCCL All Reduce bandwidth validation: requires at least 2 GPU nodes for EW fabric test",
			"nodes", gpuConfig.WorkerCount)
		return skipMsgNCCLFewNodes, true, nil
	}

	// Preflight cluster-side prerequisites before spending TrainJob time.
	// On GB200/EKS and GB200/OKE the NET variant needs
	// NVreg_GrdmaPciTopoCheckOverride=1 on the NVIDIA driver; without it, the
	// PCIe-attached NIC (EFA on EKS, ConnectX IB on OKE) can't attach dma-buf
	// to GPU HBM and NCCL silently falls back to Socket. Preflights key off
	// the benchmark target: opting into a profile opts into that profile's
	// environment contract, preflights included. (OKE takes the default
	// fabric env here — AICR_NCCL_FABRIC's roce override is an EKS-only
	// template concern.)
	if customRuntime == "" && fabric == fabricEFA && gb200NetPreflightApplies(variant, target.accelerator, target.service) {
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
	if customRuntime == "" && gkeTCPXOPreflightApplies(variant, target.accelerator, target.service) {
		if pfErr := preflightGKETCPXOReady(ctx, gpuConfig.Nodes); pfErr != nil {
			return "", false, pfErr
		}
	}

	// Run the NCCL all-reduce benchmark using Kubeflow TrainJob + MPI.
	// Each platform has a per-platform TrainingRuntime with all platform-specific
	// configuration (image, mpirun args, resources, sidecars). The TrainJob is shared.
	logs, err := runNCCLTrainJob(ctx, gpuConfig, target.accelerator, target.service, variant, fabric, customRuntime, runtimeImage)
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
	// This turns the variant label into a hard guarantee — a GB200 cluster with a
	// broken IMEX domain fails loudly on the NVLS variant instead of silently
	// falling back to EFA. Applies to recipe-supplied runtimes too: the markers
	// (NCCL's "Using network" and "NVLS comm" log lines) are transport-internal
	// and fabric-agnostic, so a runtime paired with -net / -nvls must still prove
	// its transport rather than pass on bandwidth alone. The default check
	// (variantDefault, no marker) is a no-op here, which is the documented
	// pairing for a custom runtime.
	if err := verifyTransportFromLogs(logs, variant); err != nil {
		return logs, false, err
	}

	slog.Info("Measured bandwidth", "bandwidth", bandwidth)

	// Check if bandwidth meets threshold (within 10% tolerance)
	passed := bandwidth >= (threshold * 0.9)
	actualValue := fmt.Sprintf("%.2f GB/s", bandwidth)
	// Surface the resolved workload image in the structured evidence, not just
	// slog output — issue #1751 criterion (c) asks for both. Omitted when the
	// override wasn't set (the common case) to avoid noising every result.
	if runtimeImage != "" {
		actualValue = fmt.Sprintf("%.2f GB/s (runtime image: %s)", bandwidth, runtimeImage)
	}

	if passed {
		slog.Info("Bandwidth validation passed", "bandwidth", bandwidth, "threshold", threshold*0.9, "tolerance", "10%")
	} else {
		slog.Info("Bandwidth validation failed", "bandwidth", bandwidth, "threshold", threshold*0.9, "tolerance", "10%")
	}

	return actualValue, passed, nil
}

// verifyNCCLNamespaceNotLive fails closed if the given, already-fetched NCCL
// namespace is not terminating and still has a non-terminal
// (Pending/Running/Unknown) pod in it. A live pod means some other
// execution (a genuinely concurrent run, or a rare random-suffix collision)
// still owns this namespace, so adopting it would let this run's
// fixed-name resources collide with a namespace another run is actively
// using. A nil ns (doesn't exist, or just created fresh) is not an error.
func verifyNCCLNamespaceNotLive(ctx context.Context, clientset kubernetes.Interface, ns *v1.Namespace) error {
	if ns == nil || ns.DeletionTimestamp != nil {
		// Doesn't exist, or already terminating from a prior cleanup;
		// ensureNamespace creates it fresh / waits for it to fully
		// disappear before recreating it. Either way, nothing to check yet.
		return nil
	}

	listCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()
	pods, err := clientset.CoreV1().Pods(ns.Name).List(listCtx, metav1.ListOptions{})
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			"failed to check NCCL benchmark namespace for a live execution", err)
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase != v1.PodSucceeded && pod.Status.Phase != v1.PodFailed {
			return aicrErrors.New(aicrErrors.ErrCodeConflict,
				fmt.Sprintf("NCCL benchmark namespace %q already exists with a live pod %q from another execution; refusing to adopt it",
					ns.Name, pod.Name))
		}
	}
	return nil
}

// ncclRunLockName is the Lease that admits exactly one execution into a
// per-run NCCL namespace at a time.
const ncclRunLockName = "aicr-nccl-run-lock"

// claimNCCLExecutionLock atomically admits exactly one execution into
// namespace, closing the race where two callers (e.g. sharing one
// AICR_RUN_ID) both pass verifyNCCLNamespaceNotLive before either creates a
// pod. A stale, unrenewed lock past NCCLExecutionLockStaleAge can still be
// taken over by a new caller.
//
// The returned holder ID is fresh per call, so cleanupNCCLRun can tell
// whether it still holds the lock before deleting anything.
func claimNCCLExecutionLock(ctx context.Context, clientset kubernetes.Interface, namespace string) (string, error) {
	holderID := generateExecutionID()
	now := metav1.NewMicroTime(time.Now())
	leaseDurationSeconds := int32(defaults.NCCLExecutionLockStaleAge.Seconds())

	claimCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()
	leaseClient := clientset.CoordinationV1().Leases(namespace)

	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: ncclRunLockName, Namespace: namespace},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity: &holderID, LeaseDurationSeconds: &leaseDurationSeconds,
			AcquireTime: &now, RenewTime: &now,
		},
	}
	// A missing Lease is claimed by Create, which only one racing caller can win.
	if _, err := leaseClient.Create(claimCtx, lease, metav1.CreateOptions{}); err == nil {
		return holderID, nil
	} else if !apierrors.IsAlreadyExists(err) {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to claim NCCL benchmark execution lock", err)
	}

	existing, err := leaseClient.Get(claimCtx, ncclRunLockName, metav1.GetOptions{})
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to read NCCL benchmark execution lock", err)
	}
	if existing.Spec.RenewTime != nil && time.Since(existing.Spec.RenewTime.Time) < defaults.NCCLExecutionLockStaleAge {
		return "", aicrErrors.New(aicrErrors.ErrCodeConflict,
			fmt.Sprintf("NCCL benchmark namespace %q is claimed by another execution; refusing to proceed", namespace))
	}

	existing.Spec.HolderIdentity, existing.Spec.LeaseDurationSeconds = &holderID, &leaseDurationSeconds
	existing.Spec.AcquireTime, existing.Spec.RenewTime = &now, &now
	// An existing Lease is claimed by Update pinned to the resourceVersion
	// just read, so a racing takeover here also loses to a Conflict.
	if _, err := leaseClient.Update(claimCtx, existing, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return "", aicrErrors.New(aicrErrors.ErrCodeConflict,
				fmt.Sprintf("NCCL benchmark namespace %q was claimed by a concurrent execution; refusing to proceed", namespace))
		}
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to take over an abandoned NCCL benchmark execution lock", err)
	}
	return holderID, nil
}

// rollbackNCCLNamespace best-effort deletes namespace, pinned to uid, after
// claimNCCLExecutionLock fails before the cleanup defer is registered.
// Errors are only logged. This is a leak-avoidance nicety, not correctness
// critical, since pruneStaleNCCLNamespaces reclaims an orphaned namespace on
// a later run regardless.
func rollbackNCCLNamespace(clientset kubernetes.Interface, namespace string, uid types.UID) {
	delCtx, cancel := context.WithTimeout(context.Background(), defaults.DiagnosticTimeout)
	defer cancel()
	if err := clientset.CoreV1().Namespaces().Delete(delCtx, namespace, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
	}); err != nil && !apierrors.IsNotFound(err) {
		slog.Warn("Failed to roll back NCCL benchmark namespace after execution lock admission failed",
			"namespace", namespace, "error", err)
	}
}

// releaseNCCLExecutionLockIfHeldBy best-effort deletes namespace's
// execution lock, but only if holderID is still the current holder.
// pruneStaleNCCLNamespaces calls this when its own namespace delete fails
// transiently, since the fence Lease it just claimed would otherwise sit
// there until it ages past NCCLExecutionLockStaleAge, failing a same-run
// retry closed for that long. Checking the holder first, instead of
// deleting outright, avoids removing a legitimate claim that lands in
// between.
func releaseNCCLExecutionLockIfHeldBy(ctx context.Context, clientset kubernetes.Interface, namespace, holderID string) {
	getCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()
	leaseClient := clientset.CoordinationV1().Leases(namespace)
	lease, err := leaseClient.Get(getCtx, ncclRunLockName, metav1.GetOptions{})
	if err != nil {
		return // Gone, or unreadable; nothing this call can safely remove.
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holderID {
		return // Already taken over by someone else; not ours to remove.
	}
	if delErr := leaseClient.Delete(getCtx, ncclRunLockName, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{ResourceVersion: &lease.ResourceVersion},
	}); delErr != nil && !apierrors.IsNotFound(delErr) {
		slog.Warn("Failed to release an orphaned NCCL benchmark execution lock after a failed namespace delete",
			"namespace", namespace, "error", delErr)
	}
}

// generateExecutionID returns a fresh random identifier for one execution,
// distinct from AICR_RUN_ID so two invocations sharing a run ID still get
// different lock holder identities.
func generateExecutionID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		sum := sha256.Sum256(fmt.Appendf(nil, "%d", time.Now().UnixNano()))
		return hex.EncodeToString(sum[:8])
	}
	return hex.EncodeToString(buf)
}

// ncclExecutionLockHeldBy reports whether namespace's execution lock still
// names holderID as the current holder, renewing it as part of the same
// call. Renewing, rather than a plain read, closes the gap where a
// takeover could land between this check and a subsequent delete, letting
// the stale holder's delete remove the new holder's live namespace.
//
// A missing lock reports false, not true, since that could equally mean a
// replacement is about to claim it, and treating the ambiguous case as
// authorization to proceed risks deleting or mutating that replacement's
// live resources.
//
// Called both right before cleanup's delete and mid-run before mutating
// this namespace's fixed-name resources, where the renewal also acts as a
// heartbeat, extending the caller's own claim by another
// NCCLExecutionLockStaleAge.
func ncclExecutionLockHeldBy(ctx context.Context, clientset kubernetes.Interface, namespace, holderID string) (bool, error) {
	getCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()
	leaseClient := clientset.CoordinationV1().Leases(namespace)
	lease, err := leaseClient.Get(getCtx, ncclRunLockName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to check NCCL benchmark execution lock", err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity != holderID {
		return false, nil
	}

	now := metav1.NewMicroTime(time.Now())
	lease.Spec.RenewTime = &now
	// Pinned to the resourceVersion just read, so this either succeeds,
	// proving nothing raced it, or loses to a Conflict, proving a takeover
	// already happened.
	if _, err := leaseClient.Update(getCtx, lease, metav1.UpdateOptions{}); err != nil {
		if apierrors.IsConflict(err) {
			return false, nil
		}
		return false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to renew NCCL benchmark execution lock before cleanup", err)
	}
	return true, nil
}

// pruneStaleNCCLNamespaces best-effort deletes aicr-nccl-perf-* namespaces
// left behind by a standalone (no AICR_RUN_ID) run killed before its own
// deferred cleanup ran. The Job path doesn't need this, since its
// deterministic run ID lets ensureNamespace/verifyNCCLNamespaceNotLive
// reclaim a stale namespace on the next run, but a killed standalone run
// orphans one under a random suffix no future run will ever name again.
//
// Scoped server-side to labels.ManagedBy and labels.Component, so this only
// sees namespaces this package created, decoupled from
// ncclWorkloadNamespacePrefix staying in sync with the naming logic.
// currentNamespace, anything too young, and anything still live are left
// alone (see the checks below). Fire-and-forget: it deletes and moves on
// without waiting for termination, and failures are logged and ignored,
// since this is opportunistic cleanup a benchmark run should never fail
// for.
func pruneStaleNCCLNamespaces(ctx context.Context, clientset kubernetes.Interface, dynamicClient dynamic.Interface, currentNamespace string) {
	listCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	namespaces, err := clientset.CoreV1().Namespaces().List(listCtx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s",
			labels.ManagedBy, labels.ValueValidator, labels.Component, labels.ValueNCCLPerf),
	})
	if err != nil {
		slog.Warn("Failed to list namespaces for stale NCCL benchmark namespace prune", "error", err)
		return
	}

	// Feeds reapOrphanedTrainerInstall below. Only reap once no other
	// namespace could still depend on Trainer.
	otherNamespacesRemain := false
	for _, ns := range namespaces.Items {
		if err := ctx.Err(); err != nil {
			// Canceled mid-sweep. Stop issuing more calls for the
			// remaining namespaces instead of letting each one fail in turn.
			return
		}
		if ns.Name == currentNamespace {
			// This namespace existing at all means this run, or a peer
			// sharing the same deterministic name, may still need
			// Trainer. Reaping it out from under a live peer would fail
			// that peer's in-flight TrainJob.
			otherNamespacesRemain = true
			continue
		}
		if ns.DeletionTimestamp != nil {
			// Still terminating. Its TrainJob/TrainingRuntime CRs need the
			// Trainer controller alive to clear their finalizers.
			otherNamespacesRemain = true
			continue
		}
		if time.Since(ns.CreationTimestamp.Time) < defaults.NCCLStaleNamespacePruneAge {
			otherNamespacesRemain = true
			continue
		}
		// An aged namespace can still belong to a live execution that has
		// simply run long. Reuse the same occupancy check the adoption gate
		// uses rather than deleting on age alone.
		if liveErr := verifyNCCLNamespaceNotLive(ctx, clientset, &ns); liveErr != nil {
			slog.Info("Skipping stale NCCL benchmark namespace prune: namespace still has a live pod",
				"namespace", ns.Name, "reason", liveErr)
			otherNamespacesRemain = true
			continue
		}
		// A plain read here, checked then acted on moments later, would
		// leave a gap for a legitimate claim to land in before the delete
		// below runs. Claiming the lock atomically proves nothing currently
		// holds it and fences off a claim landing in that gap, since a live
		// or freshly-won lock fails this with ErrCodeConflict. The
		// namespace delete cascades this pruner's own claim away with
		// everything else, so it never lingers.
		holderID, claimErr := claimNCCLExecutionLock(ctx, clientset, ns.Name)
		if claimErr != nil {
			if stderrors.Is(claimErr, aicrErrors.New(aicrErrors.ErrCodeConflict, "")) {
				slog.Info("Skipping stale NCCL benchmark namespace prune: namespace has a live execution lock",
					"namespace", ns.Name)
			} else {
				slog.Warn("Skipping stale NCCL benchmark namespace prune: failed to claim its execution lock",
					"namespace", ns.Name, "error", claimErr)
			}
			otherNamespacesRemain = true
			continue
		}

		delCtx, delCancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
		uid := ns.UID
		// Pinned to the UID this List observed, so a same-named namespace
		// recreated in between is left alone rather than deleted.
		delErr := clientset.CoreV1().Namespaces().Delete(delCtx, ns.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid},
		})
		delCancel()
		if delErr != nil && !apierrors.IsNotFound(delErr) {
			slog.Warn("Failed to delete stale NCCL benchmark namespace", "namespace", ns.Name, "error", delErr)
			// The claim above just fenced this namespace off. Left in
			// place after a failed delete, that Lease would otherwise
			// block a legitimate same-run retry closed until it goes
			// stale on its own.
			releaseNCCLExecutionLockIfHeldBy(ctx, clientset, ns.Name, holderID)
			otherNamespacesRemain = true
			continue
		}
		slog.Info("Deleted stale NCCL benchmark namespace left behind by an interrupted run",
			"namespace", ns.Name, "age", time.Since(ns.CreationTimestamp.Time).Round(time.Minute))
		// The delete was accepted, but its cascade still needs the Trainer
		// controller to service the namespace's CRs' finalizers.
		otherNamespacesRemain = true
	}

	reapOrphanedTrainerInstall(ctx, clientset, dynamicClient, otherNamespacesRemain)
}

// runNCCLTrainJob runs the NCCL all-reduce benchmark using Kubeflow TrainJob + MPI.
// It applies the per-platform TrainingRuntime and shared TrainJob, waits for the launcher
// pod to complete, and returns the benchmark logs.
func runNCCLTrainJob(ctx *validators.Context, gpuConfig *gpuConfiguration,
	accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType, variant ncclVariant, fabric ncclFabricType,
	customRuntime string, runtimeImage string) (logs string, err error) {

	dynamicClient := ctx.DynamicClient

	// Isolate this run in its own namespace, the same pattern
	// inferenceWorkloadConfig uses (see deriveRunID/ensureNamespace). Fixed
	// resource names only need to be unique within it, so a crashed-then-
	// retried run reclaims its own leftovers instead of colliding. The name
	// is deterministic, so before reusing an existing, non-terminating
	// instance, verifyNCCLNamespaceNotLive confirms no other execution is
	// still live under it (running on the object ensureNamespace already
	// returned, needing no separate fetch).
	gpuConfig.Namespace = ncclRunNamespace(variant)
	pruneStaleNCCLNamespaces(ctx.Ctx, ctx.Clientset, dynamicClient, gpuConfig.Namespace)
	nsObj, nsCreated, err := ensureNamespace(ctx, gpuConfig.Namespace, labels.ValueNCCLPerf)
	if err != nil {
		return "", aicrErrors.PropagateOrWrap(err, aicrErrors.ErrCodeInternal, "failed to create NCCL benchmark namespace")
	}
	if err = verifyNCCLNamespaceNotLive(ctx.Ctx, ctx.Clientset, nsObj); err != nil {
		return "", err
	}

	// Two callers can still both pass the check above before either creates
	// a pod. Claim exclusive ownership atomically so the loser fails
	// closed instead of sharing the namespace.
	holderID, err := claimNCCLExecutionLock(ctx.Ctx, ctx.Clientset, gpuConfig.Namespace)
	if err != nil {
		// The cleanup defer below isn't registered yet, so a namespace this
		// call alone just created would otherwise leak until the next run's
		// prune sweep. Roll it back now, but only the one we created. A
		// reused or concurrently-adopted namespace (nsCreated false) may
		// hold another execution's leftovers worth reclaiming later, and a
		// concurrent caller's conflicting claim means that namespace is
		// legitimately theirs, not ours to remove.
		if nsCreated && !stderrors.Is(err, aicrErrors.New(aicrErrors.ErrCodeConflict, "")) {
			rollbackNCCLNamespace(ctx.Clientset, gpuConfig.Namespace, nsObj.UID)
		}
		return "", err
	}

	// Pin the namespace instance ensureNamespace just created or reclaimed by
	// UID so the deferred delete below can never remove a different
	// namespace object that came to exist under this same name afterward
	// (e.g. deleted and recreated by an unrelated run while this one was in
	// flight).
	namespaceUID := nsObj.UID

	// Clean up everything this run created on every exit path, via a single
	// defer registered before installedResources is even known (it's read
	// by closure once cleanupNCCLRun runs). Two separate defers would
	// instead run LIFO, tearing down the Trainer controller and its CRDs
	// before the namespace's own TrainJob/TrainingRuntime CRs, leaving
	// their finalizers stuck and the namespace hung in Terminating. See
	// cleanupNCCLRun for the enforced order.
	var installedResources []trainerResourceRef
	defer func() {
		err = cleanupNCCLRun(ctx.Clientset, dynamicClient, gpuConfig.Namespace, namespaceUID, holderID, installedResources, err)
	}()

	// Ensure a usable Kubeflow Trainer. Whether an incomplete installation is a
	// failure or something to install over is decided by the recipe, not by what
	// happens to be on the cluster: a recipe that ships the component must have a
	// working one, while a recipe that does not reuses whatever is present and
	// installs an ephemeral fixture only when nothing is. Anything we install is
	// ours to clean up after the test completes.
	recipeDeclaresTrainer := validators.RecipeDeclares(ctx, kubeflowTrainerComponent)
	installedResources, err = ensureTrainerInstalled(ctx.Ctx, dynamicClient, ctx.Clientset,
		ctx.Clientset.Discovery(), recipeDeclaresTrainer)
	if err != nil {
		return "", err
	}
	if len(installedResources) > 0 {
		persistTrainerInstallManifest(ctx.Ctx, ctx.Clientset, installedResources)
	}

	// Trainer install can run long enough for the execution lock to go stale
	// and be taken over by a same-run retry. Revalidate (and renew)
	// ownership before mutating this namespace's fixed-name resources, so a
	// resumed caller that's since been superseded fails closed instead of
	// updating or recreating the new holder's resources.
	held, err := ncclExecutionLockHeldBy(ctx.Ctx, ctx.Clientset, gpuConfig.Namespace, holderID)
	if err != nil {
		return "", err
	}
	if !held {
		return "", aicrErrors.New(aicrErrors.ErrCodeConflict,
			fmt.Sprintf("NCCL benchmark execution lock for namespace %q was taken over by another execution; refusing to proceed", gpuConfig.Namespace))
	}

	// Apply runtime and trainjob resources. Propagate an inner code rather than
	// forcing ErrCodeInternal — a recipe-supplied runtime that fails to render is
	// an ErrCodeInvalidRequest (recipe-authoring error), not an internal fault.
	if applyErr := applyNCCLResources(ctx, dynamicClient, gpuConfig, accelerator, service, variant, fabric, customRuntime, runtimeImage); applyErr != nil {
		return "", aicrErrors.PropagateOrWrap(applyErr, aicrErrors.ErrCodeInternal, "failed to apply NCCL resources")
	}

	podHelper := &helper.PodLifecycle{
		ClientSet: ctx.Clientset,
		Namespace: gpuConfig.Namespace,
	}

	// Wait for launcher pod and get logs.
	logs, err = waitForLauncherPodAndGetLogs(ctx, podHelper, holderID)
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
	// Namespace is the per-run benchmark namespace. Unset by
	// determineGPUConfig, and filled in by runNCCLTrainJob once it derives
	// one (see ncclWorkloadNamespacePrefix).
	Namespace string
	Nodes     []v1.Node
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
// agree with what the worker podSpec will later schedule onto. override is the
// node selector to size against (and the first precedence in resolveTargetGPUNodes)
// — normally ctx.NodeSelector, but a recipe-supplied runtime's own worker
// nodeSelector when it pins its own nodes, so sizing and placement use the same
// cohort.
func determineGPUConfig(ctx *validators.Context, service recipe.CriteriaServiceType, accelerator recipe.CriteriaAcceleratorType, override map[string]string) (*gpuConfiguration, error) {
	slog.Info("Analyzing GPU node configuration...")

	gpuNodes, err := helper.FindSchedulableGpuNodes(ctx.Ctx, ctx.Clientset)
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to find GPU nodes", err)
	}
	if len(gpuNodes) == 0 {
		return nil, aicrErrors.New(aicrErrors.ErrCodeInternal, "no schedulable GPU nodes found")
	}
	slog.Info("Found GPU nodes", "count", len(gpuNodes))

	targetNodes, err := resolveTargetGPUNodes(gpuNodes, override, service, accelerator)
	if err != nil {
		return nil, err
	}

	// Size the workers from a per-node GPU census that requires every target
	// node to advertise the same count — a node short of its peers fails fast
	// with a named diagnostic rather than an opaque launcher timeout (#1858).
	gpuCountPerNode, err := uniformGPUCountPerNode(targetNodes)
	if err != nil {
		return nil, err
	}

	totalGPUs := len(targetNodes) * gpuCountPerNode

	return &gpuConfiguration{
		WorkerCount:     len(targetNodes),
		GPUCountPerNode: gpuCountPerNode,
		TotalGPUCount:   totalGPUs,
		Nodes:           targetNodes,
	}, nil
}

// uniformGPUCountPerNode returns the per-node allocatable nvidia.com/gpu after
// verifying every target node advertises the same positive count, and logs the
// full per-node census on every call so the counts are visible even on the
// happy path. It is the GPU-count analog of uniformFabricResourceCount (the
// fabric-count census in nccl_eks_utils.go): both census the same allocatable
// resource across all target nodes and fail closed on disagreement, naming the
// cohort — this one flags nodes below the peer max, the fabric one flags any
// deviation from the first node.
//
// NCCL schedules whole-node workers sized from this count, so a node advertising
// fewer GPUs than its peers — a degraded GPU or driver fault, e.g. an H100 node
// enumerating 7 of 8 GPUs (issue #1858) — would leave one worker Pending and the
// launcher would eventually fail with an opaque "pod failed". Failing fast here
// with the census and the short node named turns that into an immediate,
// actionable diagnostic. A uniformly low count (every node 7) is not flagged:
// it is a coherent — if unusual — topology the run can still execute, and this
// gate cannot know the SKU's nominal count; the census still surfaces it.
func uniformGPUCountPerNode(nodes []v1.Node) (int, error) {
	if len(nodes) == 0 {
		return 0, aicrErrors.New(aicrErrors.ErrCodeInternal, "no target GPU nodes for the GPU-count census")
	}

	gpuResource := v1.ResourceName(helper.GpuResourceName)
	names := make([]string, len(nodes))
	counts := make([]int, len(nodes))
	maxCount := 0
	for i := range nodes {
		q := nodes[i].Status.Allocatable[gpuResource]
		counts[i] = int(q.Value())
		names[i] = nodes[i].Name
		if counts[i] > maxCount {
			maxCount = counts[i]
		}
	}

	census := make([]string, len(nodes))
	for i := range nodes {
		census[i] = fmt.Sprintf("%s=%d", names[i], counts[i])
	}
	slog.Info("GPU census across target nodes", "perNode", strings.Join(census, " "), "expected", maxCount)

	if maxCount == 0 {
		return 0, aicrErrors.New(aicrErrors.ErrCodeInternal, "no GPUs found on the target nodes")
	}

	var short []string
	for i := range nodes {
		if counts[i] < maxCount {
			short = append(short, fmt.Sprintf("%s=%d/%d", names[i], counts[i], maxCount))
		}
	}
	if len(short) > 0 {
		return 0, aicrErrors.New(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("GPU count non-uniform across %d target node(s): %s — NCCL whole-node workers sized to %d GPUs cannot schedule on a short node. Likely causes: the cluster is still converging / the GPU device-plugin has not finished rolling out (re-run once uniform), or a genuine degraded GPU / driver fault (check nvidia-smi -L and dmesg for NVRM/Xid on the affected node)",
				len(nodes), strings.Join(short, ", "), maxCount))
	}
	return maxCount, nil
}

// applyNCCLResources applies the per-platform TrainingRuntime and shared TrainJob
// YAML files with template substitution using the dynamic client.
// Runtime: testdata/{accelerator}/{service}/runtime[-{variant}].yaml (per-platform+variant)
// TrainJob: testdata/trainjob.yaml (shared, just runtimeRef + numNodes)
func applyNCCLResources(ctx *validators.Context, dynamicClient dynamic.Interface, config *gpuConfiguration, accelerator recipe.CriteriaAcceleratorType, service recipe.CriteriaServiceType, variant ncclVariant, fabric ncclFabricType, customRuntime string, runtimeImage string) error {
	slog.Info("Applying NCCL test resources...", "accelerator", accelerator, "service", service, "variant", string(variant), "fabric", string(fabric), "customRuntime", customRuntime != "", "runtimeImageOverride", runtimeImage != "")

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
	// Skipped for a recipe-supplied runtime, which owns its own fabric wiring.
	if customRuntime == "" && service == recipe.CriteriaServiceGKE {
		gpuNICs, err := gkenet.DiscoverGPUNICNetworks(ctx.Ctx, dynamicClient)
		if err != nil {
			return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to discover GKE GPU NIC networks", err)
		}
		if len(gpuNICs) < gkenet.RequiredGPUNICNetworks {
			return aicrErrors.New(aicrErrors.ErrCodeInternal,
				fmt.Sprintf("expected %d GPU NIC networks, found %d — cluster may not have multi-NIC networking configured",
					gkenet.RequiredGPUNICNetworks, len(gpuNICs)))
		}
		templateData["GKE_NETWORK_INTERFACES"] = buildGKENetworkInterfacesAnnotation(gpuNICs)
		templateData["NRI_DEVICE_ANNOTATION"] = buildNRIDeviceAnnotation(config.GPUCountPerNode)
		slog.Info("Discovered GKE GPU NIC networks", "count", len(gpuNICs), "networks", gpuNICs)
	}

	// For EKS, discover instance type and EFA adapter count from GPU nodes.
	// EFA count of 0 is valid — NCCL falls back to TCP (slower but functional).
	// Skipped for a recipe-supplied runtime, which owns its own fabric wiring.
	if customRuntime == "" && service == recipe.CriteriaServiceEKS {
		warnIfHeterogeneousNodes(config.Nodes)
		it, efaCount, err := discoverEKSNodeConfig(config.Nodes)
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

	// For AKS, discover the rdma-shared-device-plugin resource on the target
	// GPU nodes. ND-series InfiniBand SKUs expose the node's IB HCAs through a
	// shared pool (rdma/hca_shared_devices_a); a worker requests one unit to
	// have every /dev/infiniband device mounted. A count of 0 is valid —
	// NCCL falls back to TCP over the pod network (slower but functional),
	// mirroring the EKS zero-EFA behavior above.
	// Skipped for a recipe-supplied runtime, which owns its own fabric wiring.
	if customRuntime == "" && service == recipe.CriteriaServiceAKS {
		if err := applyAKSTemplateData(config, templateData); err != nil {
			return err
		}
	}

	effectiveNodeSelector, effectiveTolerations, err := effectiveWorkerScheduling(ctx, service, instanceType, config, customRuntime)
	if err != nil {
		return err
	}

	// RoCE NET: the worker pod references a RoCE DRA ResourceClaimTemplate
	// (nccl-roce-rct). parseYAMLTemplate is single-document, so apply the claim
	// as a standalone object before the runtime (it must exist when the TrainJob
	// later creates the worker pods that reference it). Skipped for a
	// recipe-supplied runtime: it declares any DRA claims it needs itself.
	if customRuntime == "" && fabric == fabricRoCE {
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

	runtimeObj, err := buildNCCLRuntimeObject(customRuntime, accelerator, service, variant, fabric, config.Namespace, templateData)
	if err != nil {
		return err
	}
	// Render the resolved workload image into every launcher/worker container
	// this override governs (issue #1751). No-op when runtimeImage == "";
	// applyNCCLResources is only called with a non-empty override on the
	// baked-in template path — a recipe-supplied runtime (customRuntime != "")
	// is never reachable here with runtimeImage set, since the resolve site in
	// validateNcclAllReduceBw gates resolution on customRuntime == "".
	// Defense in depth: even though the only current caller
	// (validateNcclAllReduceBw) already gates runtimeImage resolution on
	// customRuntime == "", applyNCCLResources must not silently overwrite a
	// recipe-supplied image if ever called with both non-empty — the
	// recipe-supplied runtime owns its image, full stop. This is the guard
	// TestApplyNCCLResourcesRuntimeImageOverride's custom-runtime subtest
	// (mchmarny, 691b3b3 review) asserts.
	if customRuntime == "" {
		if err := applyNCCLRuntimeImageOverride(runtimeObj, runtimeImage); err != nil {
			return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to apply NCCL runtime image override", err)
		}
	}
	if err := applyNCCLWorkerScheduling(runtimeObj, effectiveNodeSelector, effectiveTolerations); err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to apply NCCL worker scheduling", err)
	}
	if err = createUnstructured(ctx.Ctx, dynamicClient, trainingRuntimeGVR, config.Namespace, runtimeObj); err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to apply training runtime", err)
	}
	slog.Info("Applied TrainingRuntime", "service", service)

	// Wait for the runtime to be visible to the Trainer admission webhook.
	// The webhook validates that the referenced runtime exists before allowing
	// TrainJob creation; without this wait we hit a race condition.
	if err = waitForTrainingRuntime(ctx.Ctx, dynamicClient, config.Namespace); err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "TrainingRuntime not ready", err)
	}

	// NVLS variant only: provision an IMEX domain via the NVIDIA DRA driver
	// before the TrainJob fires. The ComputeDomain CR causes the driver to
	// auto-create a ResourceClaimTemplate that runtime-nvls.yaml references;
	// without this, the NVL72 fabric is visible to NCCL but /dev/nvidia-caps-
	// imex-channels isn't mounted into the workers and MNNVL aborts with
	// "Cuda failure 800 'operation not permitted'". Skipped for a recipe-supplied
	// runtime: IMEX/ComputeDomain wiring is part of the fabric contract the
	// runtime owns, so it must declare any ComputeDomain/ResourceClaim it needs.
	if customRuntime == "" && variant == variantNVLS {
		if err = applyNCCLComputeDomain(ctx.Ctx, dynamicClient, config.Namespace); err != nil {
			return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to apply ComputeDomain", err)
		}
		if err = waitForIMEXClaimTemplate(ctx.Ctx, dynamicClient, config.Namespace); err != nil {
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
	if err = applyTrainJobWithRetry(ctx.Ctx, dynamicClient, config.Namespace, trainjobPath, templateData); err != nil {
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
		// ctx, not retryCtx, so the reclaim wait inside createUnstructured
		// gets its own NCCLResourceRecreateWait budget instead of retryCtx's
		// shorter one. The retry loop itself still stays bounded below.
		createErr := createUnstructured(ctx, dynamicClient, trainJobGVR, namespace, obj)
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
				createErr, map[string]any{"attempts": attempt})
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
				createErr, map[string]any{"attempts": attempt})
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
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "resource.nvidia.com/v1beta1",
		"kind":       "ComputeDomain",
		"metadata": map[string]any{
			keyName:     ncclComputeDomainName,
			"namespace": namespace,
		},
		"spec": map[string]any{
			"numNodes": int64(0),
			"channel": map[string]any{
				"allocationMode": "Single",
				"resourceClaimTemplate": map[string]any{
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
	// Required because Update rejects an empty resourceVersion. Adopting an
	// existing ComputeDomain here is intentional, since it's how a stale
	// one left by a prior run (e.g. a retry reusing AICR_RUN_ID after a
	// hard kill before cleanup ran) gets reclaimed instead of failing with
	// AlreadyExists.
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

// effectiveWorkerScheduling resolves the nodeSelector and tolerations stamped
// onto the NCCL worker pods. A user --node-selector / --tolerations override
// (ctx.NodeSelector / ctx.Tolerations) wins; otherwise the platform default is
// used — EXCEPT for a recipe-supplied runtime, which owns its own worker
// scheduling. Applying the platform default to a custom runtime would clobber
// it: the EKS default stamps {instance-type: ""} (instanceType is never
// discovered on the custom path — a selector matching zero nodes), and
// GKE/OKE/AKS overwrite with a cloud-specific selector (GKE even hard-errors on
// missing accelerator labels). The service=any explicit-selector requirement
// likewise applies only to the baked-in path.
func effectiveWorkerScheduling(ctx *validators.Context, service recipe.CriteriaServiceType,
	instanceType string, config *gpuConfiguration, customRuntime string) (map[string]string, []v1.Toleration, error) {

	var defaultNodeSelector map[string]string
	var defaultTolerations []v1.Toleration
	if customRuntime == "" {
		ns, tol, err := platformWorkerScheduling(service, instanceType, config.Nodes)
		if err != nil {
			return nil, nil, err
		}
		defaultNodeSelector, defaultTolerations = ns, tol
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

	if customRuntime == "" && service == recipe.CriteriaServiceAny && len(effectiveNodeSelector) == 0 {
		return nil, nil, aicrErrors.New(aicrErrors.ErrCodeInvalidRequest,
			"self-managed clusters (service=any) require --node-selector to identify GPU nodes "+
				"(e.g., --node-selector nvidia.com/gpu.present=true)")
	}

	return effectiveNodeSelector, effectiveTolerations, nil
}

// buildNCCLRuntimeObject selects and renders the TrainingRuntime the NCCL
// TrainJob will reference. A recipe-supplied nccl-benchmark-runtime is rendered
// from its inline template with its identity forced to what the shared TrainJob's
// runtimeRef expects and confined to the validator namespace (the value was
// shape-checked as a Kubeflow TrainingRuntime at resolve time). Otherwise the
// baked-in per-platform testdata template is read from disk.
func buildNCCLRuntimeObject(customRuntime string, accelerator recipe.CriteriaAcceleratorType,
	service recipe.CriteriaServiceType, variant ncclVariant, fabric ncclFabricType,
	namespace string, templateData map[string]string) (*unstructured.Unstructured, error) {

	if customRuntime != "" {
		obj, err := renderYAMLTemplate(customRuntime, templateData)
		if err != nil {
			return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInvalidRequest, "failed to render recipe-supplied nccl-benchmark-runtime", err)
		}
		obj.SetName(ncclTrainingRuntimeName)
		obj.SetNamespace(namespace)
		slog.Info("Rendered recipe-supplied NCCL TrainingRuntime", "name", ncclTrainingRuntimeName, "namespace", namespace)
		return obj, nil
	}

	obj, err := parseYAMLTemplate(templatePath(accelerator, service, variant, fabric, "runtime.yaml"), templateData)
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to parse training runtime template", err)
	}
	return obj, nil
}

// parseYAMLTemplate reads a YAML template file, performs ${KEY} substitution,
// and unmarshals it into an unstructured object.
func parseYAMLTemplate(path string, data map[string]string) (*unstructured.Unstructured, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to read template", err)
	}
	return renderYAMLTemplate(string(content), data)
}

// renderYAMLTemplate performs ${KEY} substitution on an in-memory YAML template
// string and unmarshals the result into an unstructured object. It is the
// substitution core shared by parseYAMLTemplate (baked-in testdata templates,
// read from disk) and the recipe-supplied nccl-benchmark-runtime path (the
// template arrives inline in ValidationInput, never touching the filesystem).
func renderYAMLTemplate(content string, data map[string]string) (*unstructured.Unstructured, error) {
	for key, value := range data {
		content = strings.ReplaceAll(content, "${"+key+"}", value)
	}
	obj := &unstructured.Unstructured{}
	if err := yaml.Unmarshal([]byte(content), obj); err != nil {
		return nil, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to parse YAML", err)
	}
	return obj, nil
}

// createUnstructured creates obj, a namespaced resource, with a timeout. If
// a fixed-name resource with that GVR/name already exists, it reclaims it
// so a same-run retry after a hard kill can recover instead of failing on
// AlreadyExists.
//
// TrainJob reclaims by delete-then-recreate rather than update, since
// Kubeflow Trainer treats most of its spec as immutable and an update
// wouldn't make the controller recreate the underlying JobSet/pods anyway,
// which is what a stale TrainJob actually needs. Every other caller
// (TrainingRuntime) reclaims by updating in place, like
// applyNCCLComputeDomain does for ComputeDomain.
func createUnstructured(ctx context.Context, dynamicClient dynamic.Interface, gvr schema.GroupVersionResource, namespace string, obj *unstructured.Unstructured) error {
	applyCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()

	client := dynamicClient.Resource(gvr).Namespace(namespace)
	if _, err := client.Create(applyCtx, obj, metav1.CreateOptions{}); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to create resource", err)
	}

	if gvr == trainJobGVR {
		// Pin the delete to the object actually observed here, not just its
		// name. Create's AlreadyExists above only proves something existed
		// at that instant. Without a UID precondition, a delete-by-name in
		// the gap since could remove a same-named TrainJob that a distinct
		// execution created in between, rather than failing the delete.
		stale, err := client.Get(applyCtx, obj.GetName(), metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			// Already gone since the AlreadyExists above. Nothing to delete.
		case err != nil:
			return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to get stale resource for recreate", err)
		default:
			uid := stale.GetUID()
			if err := client.Delete(applyCtx, obj.GetName(), metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: &uid},
			}); err != nil && !apierrors.IsNotFound(err) {
				return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to delete stale resource for recreate", err)
			}
		}
		// Delete only stamps DeletionTimestamp while a controller-serviced
		// finalizer (Trainer v2 / JobSet ownership) is still clearing.
		// Recreating immediately would race it and hit AlreadyExists again,
		// defeating the same-run retry-after-hard-kill path this exists for.
		// A finalizer can outlast applyCtx's short DiagnosticTimeout, so the
		// wait and the recreate that depends on it get their own bound.
		waitCtx, waitCancel := context.WithTimeout(ctx, defaults.NCCLResourceRecreateWait)
		defer waitCancel()
		if err := waitForResourceGone(waitCtx, client, obj.GetName()); err != nil {
			return aicrErrors.PropagateOrWrap(err, aicrErrors.ErrCodeInternal,
				"failed waiting for stale resource to finish deleting before recreate")
		}
		obj.SetResourceVersion("")
		if _, err := client.Create(waitCtx, obj, metav1.CreateOptions{}); err != nil {
			return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to recreate resource", err)
		}
		return nil
	}

	existing, err := client.Get(applyCtx, obj.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// Deleted between the AlreadyExists above and this Get. Create
		// fresh instead of failing on a race a plain Create would win.
		if _, createErr := client.Create(applyCtx, obj, metav1.CreateOptions{}); createErr != nil {
			return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to recreate resource", createErr)
		}
		return nil
	}
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to get existing resource for update", err)
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	if _, err := client.Update(applyCtx, obj, metav1.UpdateOptions{}); err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to update resource", err)
	}
	return nil
}

// waitForResourceGone watches a namespaced resource by name until it's
// removed, mirroring waitForNamespaceGone for the dynamic client. Used by
// createUnstructured to wait out a finalizer-held delete before recreating.
func waitForResourceGone(ctx context.Context, client dynamic.ResourceInterface, name string) error {
	watcher, err := client.Watch(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + name,
	})
	if err != nil {
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to watch resource deletion", err)
	}
	defer watcher.Stop()

	if _, getErr := client.Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(getErr) {
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return aicrErrors.Wrap(aicrErrors.ErrCodeTimeout,
				"timed out waiting for resource to be fully deleted", ctx.Err())
		case event, ok := <-watcher.ResultChan():
			if !ok {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return aicrErrors.Wrap(aicrErrors.ErrCodeTimeout,
						"timed out waiting for resource to be fully deleted", ctxErr)
				}
				// Watch channel closed without cancellation. Re-Get before
				// failing, since the resource may have been deleted during
				// the closure window (apiserver hiccup, rolling restart).
				if _, getErr := client.Get(ctx, name, metav1.GetOptions{}); apierrors.IsNotFound(getErr) {
					return nil
				}
				return aicrErrors.New(aicrErrors.ErrCodeUnavailable,
					"resource watch channel closed before deletion observed")
			}
			if event.Type == watch.Deleted {
				return nil
			}
		}
	}
}

// platformWorkerScheduling returns the default nodeSelector and tolerations
// for NCCL worker pods on the given service. instanceType is only used for EKS;
// nodes (the accelerator-narrowed target set from resolveTargetGPUNodes) is
// used for GKE (the gke-accelerator label) and OKE/AKS (the shared
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
	case recipe.CriteriaServiceOKE, recipe.CriteriaServiceAKS:
		// OKE bare-metal GB200 pools are commonly tainted and may coexist
		// with other GPU shapes under one control plane. Tolerate the pool
		// taint (mirroring EKS/GKE) and pin workers to the same cohort the
		// node count was sized against by reusing the GFD gpu.product label
		// that resolveTargetGPUNodes -> narrowByAccelerator already filtered
		// on. On non-GFD installs no shared product label exists, so emit no
		// selector — matching the counting path's unfiltered fallback so the
		// two stay aligned.
		//
		// AKS shares this shape: GPU pools carry the nvidia.com/gpu=present:
		// NoSchedule taint and AICR recipes deploy the GPU Operator with GFD,
		// so gpu.product (e.g. NVIDIA-H100-80GB-HBM3) is the discriminating
		// label. The AKS-native kubernetes.azure.com/accelerator label is not
		// used because its value is just "nvidia" — it cannot pin the H100
		// cohort narrowByAccelerator sized the job against.
		var nodeSelector map[string]string
		if product := commonGPUProduct(nodes); product != "" {
			nodeSelector = map[string]string{gpuProductLabel: product}
		}
		return nodeSelector, []v1.Toleration{{Operator: v1.TolerationOpExists}}, nil
	case recipe.CriteriaServiceAny, recipe.CriteriaServiceOCP, recipe.CriteriaServiceKind, recipe.CriteriaServiceLKE, recipe.CriteriaServiceBCM, recipe.CriteriaServiceMetal3:
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
		jobMap, ok := jobRaw.(map[string]any)
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
			ns := make(map[string]any, len(nodeSelector))
			for k, v := range nodeSelector {
				ns[k] = v
			}
			workerPodSpec["nodeSelector"] = ns
			slog.Info("Applying NCCL worker nodeSelector", "selector", nodeSelector)
		}

		if len(tolerations) > 0 {
			tolList := make([]any, 0, len(tolerations))
			for _, t := range tolerations {
				tolMap := map[string]any{
					keyOperator: string(t.Operator),
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

// nestedMap navigates a chain of string keys through nested map[string]any values.
// Returns the target map and true if found, nil and false otherwise.
func nestedMap(m map[string]any, keys ...string) (map[string]any, bool) {
	current := m
	for _, key := range keys {
		next, ok := current[key]
		if !ok {
			return nil, false
		}
		nextMap, ok := next.(map[string]any)
		if !ok {
			return nil, false
		}
		current = nextMap
	}
	return current, true
}

// waitForLauncherPodAndGetLogs waits for the launcher pod to be created,
// waits for it to complete, and returns its logs. Revalidates that
// holderID still holds the namespace's execution lock right after the pod
// goes terminal, since a live pod only protects the namespace from a
// same-run retry while it's still running.
func waitForLauncherPodAndGetLogs(ctx *validators.Context, podHelper *helper.PodLifecycle, holderID string) (string, error) {
	slog.Info("Waiting for launcher pod to be created...")

	// Wait for launcher pod to be created (pattern: nccl-all-reduce-tj-launcher-*)
	launcherPod, err := waitForPodByLabelSelector(
		ctx.Ctx,
		ctx.Clientset,
		podHelper.Namespace,
		fmt.Sprintf("jobset.sigs.k8s.io/jobset-name=%s,jobset.sigs.k8s.io/replicatedjob-name=launcher", ncclTrainJobName),
		defaults.NCCLLauncherPodTimeout,
	)
	if err != nil {
		return "", aicrErrors.Wrap(aicrErrors.ErrCodeTimeout, "failed to find launcher pod", err)
	}

	slog.Info("Found launcher pod", "name", launcherPod.Name)

	// Wait for pod to complete using helper method
	err = podHelper.WaitForPodSuccess(ctx.Ctx, launcherPod, defaults.NCCLTrainJobTimeout)

	// This wait can run past NCCLExecutionLockStaleAge with no renewal. A
	// live pod protects this namespace from a same-run retry on its own,
	// but that ends the moment it goes terminal. Renew right here, before
	// the diagnostic gathering below adds more unrenewed time, so a retry
	// can't take the lock while cleanup still has work to do.
	held, lockErr := ncclExecutionLockHeldBy(ctx.Ctx, ctx.Clientset, podHelper.Namespace, holderID)
	if lockErr != nil {
		return "", lockErr
	}
	if !held {
		return "", aicrErrors.New(aicrErrors.ErrCodeConflict,
			fmt.Sprintf("NCCL benchmark execution lock for namespace %q was taken over by another execution; refusing to proceed", podHelper.Namespace))
	}

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
			if term := launcherTerminationTail(ctx.Ctx, ctx.Clientset, podHelper.Namespace, launcherPod.Name); term != "" {
				launcherLogs = fmt.Sprintf("<%s; container termination-message tail follows>\n%s",
					fetchNote, tailLines(term, maxDiagLogLines))
			} else {
				launcherLogs = fmt.Sprintf("<%s; no termination message captured>", fetchNote)
			}
		}
		workerDiag := collectNCCLWorkerDiagnostics(ctx.Ctx, ctx.Clientset, podHelper.Namespace)

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

	// Append the launcher's termination message. The GKE launcher writes the NCCL
	// results rows there explicitly, and unlike the streamed log it survives
	// kubelet container-log rotation — the massive NCCL/TCPXO teardown spam can
	// rotate the results table out of the segment GetPodLogs returns, leaving only
	// the teardown tail (issue #1712). parseBandwidthFromLogs keys on the last
	// matching row, so appending the termination message makes it the
	// rotation-proof source of truth while the streamed log remains available for
	// transport verification and diagnostics. Empty for launchers that don't write
	// results there (other platforms), leaving behavior unchanged.
	term := launcherTerminationTail(ctx.Ctx, ctx.Clientset, podHelper.Namespace, launcherPod.Name)
	if term != "" {
		slog.Info("Appending launcher termination message (rotation-proof results)", "termBytes", len(term))
	}
	return appendTerminationResults(logs, term), nil
}

// appendTerminationResults appends the launcher termination message (the
// rotation-proof NCCL results the launcher wrote to /dev/termination-log) to the
// streamed log. parseBandwidthFromLogs keys on the last matching row, so the
// appended results become the source of truth; an empty term leaves logs
// unchanged (launchers that don't write results there).
func appendTerminationResults(logs, term string) string {
	if term == "" {
		return logs
	}
	return logs + "\n" + term
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
	for line := range strings.SplitSeq(trimmed, "\n") {
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

// waitForPodByLabelSelector waits for a pod matching the label selector to be
// created. Uses the Watch API for efficiency instead of polling. Skips
// Deleted events and pods that are already terminating or terminal, so a
// retry that recreates the workload under the same label selector doesn't
// return a stale pod that no longer exists.
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
			// A Deleted event, or a pod already terminating/terminal, isn't
			// a match. A recreated workload (e.g. a TrainJob admission
			// retry) can fire a Deleted event for the stale pod before the
			// replacement's Added event arrives, so keep waiting for the
			// replacement, matching the filter newestRunnablePod uses on
			// its re-List recovery path.
			isTerminal := pod.Status.Phase == v1.PodFailed || pod.Status.Phase == v1.PodSucceeded
			if event.Type == watch.Deleted || pod.DeletionTimestamp != nil || isTerminal {
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

// cleanupNCCLResources deletes the per-run benchmark namespace, cascading
// away the trainjob, runtime, and (if present) the ComputeDomain and RoCE
// ResourceClaimTemplate CRs it created, mirroring cleanupInferenceWorkload's
// pattern. Nothing else ever shares this namespace, so there's no shared
// state to pin deletes against.
//
// Unlike cleanupInferenceWorkload, a Delete failure is itself returned, not
// just logged, so foldCleanupError can still fail an otherwise-passing
// check on it. NotFound is tolerated.
//
// uid pins the delete to the exact namespace instance runNCCLTrainJob
// created or reclaimed, so a recreated same-named namespace is left alone
// instead of silently deleted (see the check below for why it must be
// non-empty).
func cleanupNCCLResources(clientset kubernetes.Interface, namespace string, uid types.UID) error {
	if uid == "" {
		// Required, not just preferred: the fake client used in tests
		// ignores delete preconditions and would otherwise silently
		// proceed on a caller bug that drops the UID.
		return aicrErrors.New(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("refusing to delete NCCL benchmark namespace %q without an owning UID", namespace))
	}

	slog.Info("Cleaning up NCCL test resources...", "namespace", namespace)

	deleteCtx, cancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
	defer cancel()

	nsClient := clientset.CoreV1().Namespaces()
	err := nsClient.Delete(deleteCtx, namespace, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
	})
	if err != nil {
		if apierrors.IsNotFound(err) {
			slog.Info("NCCL benchmark namespace already gone", "namespace", namespace)
			return nil
		}
		if apierrors.IsConflict(err) {
			// UID precondition mismatch. The namespace we created is
			// already gone and a different one holds the name, so
			// nothing this run owns leaked.
			slog.Info("NCCL benchmark namespace was already replaced", "namespace", namespace)
			return nil
		}
		return aicrErrors.Wrap(aicrErrors.ErrCodeInternal,
			fmt.Sprintf("failed to delete NCCL benchmark namespace %q", namespace), err)
	}

	// Same bound as ensureNamespace's wait on the create side. Only logged
	// on timeout, not returned, since the Delete call above already
	// succeeded, so a slow-but-real teardown (e.g. NVLS's DRA/IMEX
	// finalizers) must not fail an otherwise-passing benchmark just because
	// this observability wait ran out first.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), defaults.InferenceNamespaceTerminationWait)
	defer waitCancel()
	if err := waitForNamespaceGone(waitCtx, nsClient, namespace); err != nil {
		slog.Warn("NCCL benchmark namespace did not finish terminating within the wait bound, "+
			"deletion was accepted and its cascading GC continues in the background",
			"namespace", namespace, "error", err)
		return nil
	}

	slog.Info("Deleted NCCL benchmark namespace", "namespace", namespace)
	return nil
}

// cleanupNCCLRun tears down everything runNCCLTrainJob created, in a fixed
// order. It skips cleanup entirely if holderID no longer holds the
// namespace's execution lock, since another execution has since taken it
// over, or if that can't be confirmed either way. Otherwise it deletes the
// per-run namespace first (cascading its TrainJob/TrainingRuntime/
// ComputeDomain/RoCE-claim CRs), then, only on the self-install fallback
// path, the Kubeflow Trainer installation, whose removal would otherwise
// strand those CRs' finalizers if it ran first. Trainer teardown only runs
// if the namespace delete succeeded.
//
// benchErr is the check's result so far. A cleanup failure only overrides
// a nil benchErr, never masking a real benchmark failure (see
// foldCleanupError).
func cleanupNCCLRun(clientset kubernetes.Interface, dynamicClient dynamic.Interface, namespace string, uid types.UID, holderID string, installedResources []trainerResourceRef, benchErr error) error {
	held, lockErr := ncclExecutionLockHeldBy(context.Background(), clientset, namespace, holderID)
	if lockErr != nil {
		// A transient read failure here (an apiserver blip, a timeout) is not
		// proof the lock was actually taken over, but it's not proof it
		// wasn't either. Treat it the same as a confirmed loss below rather
		// than fail an otherwise-passing benchmark over a flaky lease read,
		// or risk deleting a peer's live resources on a guess.
		slog.Warn("Failed to verify the NCCL benchmark execution lock is still held; skipping cleanup",
			"namespace", namespace, "error", lockErr)
		return benchErr
	}
	if !held {
		slog.Warn("NCCL benchmark execution lock was taken over by another execution; skipping cleanup", "namespace", namespace)
		return benchErr
	}

	nsErr := cleanupNCCLResources(clientset, namespace, uid)
	err := foldCleanupError(benchErr, nsErr, "NCCL benchmark succeeded but NCCL resource cleanup failed")
	if nsErr != nil {
		return err
	}
	if len(installedResources) > 0 {
		// A concurrent run with a different AICR_RUN_ID gets its own
		// namespace, but the two can still share this same self-installed
		// Trainer: ensureTrainerInstalled's reuse path returns no
		// resources for the peer, so only this run's cleanup ever sees
		// installedResources non-empty. Deleting Trainer here regardless
		// of that peer would fail its still-running TrainJob out from
		// under it. On a List failure, leave Trainer installed rather
		// than risk deleting it out from under a peer this call could
		// not rule out.
		othersRemain, listErr := otherNCCLNamespacesExist(context.Background(), clientset, namespace)
		if listErr != nil {
			slog.Warn("Failed to check for other NCCL benchmark namespaces; leaving Trainer installed for a later cleanup",
				"namespace", namespace, "error", listErr)
		} else if othersRemain {
			slog.Info("Other NCCL benchmark namespaces remain; leaving the self-installed Trainer for a later cleanup",
				"namespace", namespace)
		} else if trainerErr := deleteTrainer(dynamicClient, installedResources); trainerErr != nil {
			err = foldCleanupError(err, trainerErr, "NCCL benchmark succeeded but Kubeflow Trainer cleanup failed")
		} else {
			removeTrainerInstallManifestEntries(context.Background(), clientset, installedResources)
		}
	}
	return err
}

// otherNCCLNamespacesExist reports whether any AICR-owned NCCL benchmark
// namespace other than namespace currently exists. cleanupNCCLRun checks
// this before eagerly deleting a self-installed Trainer, since a
// concurrent run with a different run ID can share that same install and
// still be using it.
func otherNCCLNamespacesExist(ctx context.Context, clientset kubernetes.Interface, namespace string) (bool, error) {
	listCtx, cancel := context.WithTimeout(ctx, defaults.DiagnosticTimeout)
	defer cancel()
	namespaces, err := clientset.CoreV1().Namespaces().List(listCtx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s,%s=%s",
			labels.ManagedBy, labels.ValueValidator, labels.Component, labels.ValueNCCLPerf),
	})
	if err != nil {
		return false, aicrErrors.Wrap(aicrErrors.ErrCodeInternal, "failed to list NCCL benchmark namespaces", err)
	}
	for _, ns := range namespaces.Items {
		if ns.Name != namespace {
			return true, nil
		}
	}
	return false, nil
}
