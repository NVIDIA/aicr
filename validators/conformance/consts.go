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
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Kubernetes API groups referenced across conformance checks.
const (
	apiGroupAPIExtensions = "apiextensions.k8s.io"
	apiGroupResourceK8sIO = "resource.k8s.io"
	apiGroupGateway       = "gateway.networking.k8s.io"
	apiGroupNVIDIA        = "nvidia.com"

	// resourceNVIDIAGPU is the canonical Kubernetes extended-resource name for NVIDIA GPUs.
	resourceNVIDIAGPU = "nvidia.com/gpu"
	// resourceCRDs is the Kubernetes API resource name for CustomResourceDefinitions.
	resourceCRDs = "customresourcedefinitions"
	// versionV1alpha1 is the API version used by legacy NVIDIA and TrainJob CRDs.
	versionV1alpha1 = "v1alpha1"
	// versionV1beta1 is the API version used by DynamoGraphDeployment in
	// Dynamo 1.2 and NVIDIA ComputeDomain.
	versionV1beta1 = "v1beta1"
	// labelNVIDIAGPUPresent is the "key=value" selector for GPU-bearing nodes
	// when scaled-up via the cluster autoscaler.
	labelNVIDIAGPUPresent = "nvidia.com/gpu.present=true"
)

// clusterPolicyGVR and clusterPolicyName locate the GPU Operator's singleton
// ClusterPolicy, read by the operator-health check and the GPU allocation
// checks.
var clusterPolicyGVR = schema.GroupVersionResource{Group: apiGroupNVIDIA, Version: "v1", Resource: "clusterpolicies"}

const (
	clusterPolicyName = "cluster-policy"
	// clusterPolicyStateReady is the GPU Operator's status.state once every
	// operand it manages is deployed and ready.
	clusterPolicyStateReady = "ready"
)

// Prometheus Operator scrape-configuration resources the metrics checks
// create for their probe workloads; both ship with the recipe's
// kube-prometheus-stack (through prometheus-operator-crds).
var (
	podMonitorGVR     = schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1", Resource: "podmonitors"}
	serviceMonitorGVR = schema.GroupVersionResource{Group: "monitoring.coreos.com", Version: "v1", Resource: "servicemonitors"}
)

// Keys used when constructing unstructured Kubernetes manifests as map[string]any.
const (
	keyAPIVersion = "apiVersion"
	keyKind       = "kind"
	keyMetadata   = "metadata"
	keyName       = "name"
	keyNamespace  = "namespace"
	keySection    = "sectionName"
	keyPath       = "path"
	keyPort       = "port"
	keySpec       = "spec"
)

// NVIDIA DRA driver deployment topology and driver identifiers, shared by the
// dra-support and secure-accelerator-access checks and the GPU allocation
// mode probe.
const (
	// draAPIGroupVersion is the GA Dynamic Resource Allocation API
	// (resource.k8s.io/v1, K8s 1.34+), used as the default group-version in
	// test fixtures. Production checks discover the served version at runtime
	// (v1, v1beta2, or v1beta1 — see allocmode.APIVersionPreference and
	// allocmode.DiscoverServedVersion in validators/internal/allocmode,
	// aliased locally in allocmode_bridge.go).
	draAPIGroupVersion = "resource.k8s.io/v1"
	// draDriverComponentName is the recipe component that deploys the NVIDIA
	// DRA driver. The dra-support check scopes itself from the resolved
	// recipe's componentRefs: when the recipe excludes or disables this
	// component (e.g. recipes/overlays/ocp.yaml), DRA is out of scope.
	draDriverComponentName = "nvidia-dra-driver-gpu"
	// draDriverNamespace is the namespace the NVIDIA DRA driver deploys into.
	draDriverNamespace = "nvidia-dra-driver"
	// draControllerDeployment is the NVIDIA DRA driver controller Deployment name.
	draControllerDeployment = "nvidia-dra-driver-gpu-controller"
	// draKubeletPluginDaemonSet is the NVIDIA DRA driver kubelet plugin DaemonSet name.
	draKubeletPluginDaemonSet = "nvidia-dra-driver-gpu-kubelet-plugin"
	// draDriverGPU is the DRA driver (and DeviceClass) name for whole-GPU
	// allocation. The NVIDIA DRA driver's supported configuration is
	// ComputeDomain-only, so this DeviceClass is frequently absent (#1327).
	draDriverGPU = "gpu.nvidia.com"
	// draDriverComputeDomain is the DRA driver name for ComputeDomain (IMEX)
	// channel allocation — the supported NVIDIA DRA driver configuration.
	draDriverComputeDomain = "compute-domain.nvidia.com"
	// nvidiaDriverSuffix matches ResourceSlices published by any NVIDIA DRA
	// driver (gpu.nvidia.com, compute-domain.nvidia.com, ...).
	nvidiaDriverSuffix = ".nvidia.com"
)

// Common workload-related string constants used in multiple checks.
const (
	labelApp = "app"
	// keyAllocationMode is the DRA DeviceRequest allocation-mode field.
	keyAllocationMode = "allocationMode"
	// waitingReasonCrashLoopBackOff is the kubelet's container waiting reason
	// for a container that keeps exiting.
	waitingReasonCrashLoopBackOff = "CrashLoopBackOff"
	// eventReasonFailedCreatePodSandBox is the kubelet's event reason for a
	// pod whose sandbox could not be created; the pod itself stays Pending
	// with its containers ContainerCreating and no status change, so the
	// event is the only signal (see sandboxCreateFailure).
	eventReasonFailedCreatePodSandBox = "FailedCreatePodSandBox"
	// involvedObjectKindPod is the Event.involvedObject.kind of a pod event.
	involvedObjectKindPod       = "Pod"
	containerNameSleep          = "sleep"
	gpuClaimName                = "gpu"
	statusUnknown               = "unknown"
	valueNone                   = "none"
	deploymentClusterAutoscaler = "cluster-autoscaler"
	namespaceDynamoSystem       = "dynamo-system"
)

// Per-check WORK budgets for the conformance checks that create cluster
// resources needing reconciliation (the two GPU allocation checks, the HPA
// autoscaling cycle, the AI service metrics stub, the robust-controller
// TrainJob reconciliation on the Kubeflow path and the inference-gateway
// traffic phase). Each
// check bounds all of its resource-creating work — allocation-mode discovery,
// namespace/claim/pod creation, pod waits, log collection, and validation —
// to a budget, so that inside the catalog timeout (which becomes the check
// context's deadline via AICR_CHECK_TIMEOUT — the Job's activeDeadlineSeconds
// is this value plus defaults.ValidatorJobDeadlineHeadroom) there is always
// room left for ONE bounded namespace cleanup plus scheduling-skew/result-
// flush margin.
// secure-accelerator-access applies the budget at entry; dra-support first
// settles scoping (pure recipe check, or the standalone read-only presence
// probe bounded separately by draProbeTimeout) so skip semantics survive
// short deadlines, then budgets everything after.
//
// The budget is derived DYNAMICALLY from the check context's deadline (see
// gpuCheckWorkBudget below): work budget = remaining-until-deadline minus
// gpuCheckCleanupReserve. This keeps the cleanup reserve intact when a --data
// catalog override shortens or lengthens the check timeout, and it charges
// any time already spent before the check entered against the WORK budget,
// never against the reserve. The fixed budgets below apply only as fallbacks
// when the parent context carries no deadline (standalone/library use); they
// mirror the embedded catalog's timeouts:
//
//	dra-support:               3m30s work + 30s cleanup + 60s margin =  5m catalog timeout
//	secure-accelerator-access: 8m30s work + 30s cleanup + 60s margin = 10m catalog timeout
//	driver-runtime-management: 8m30s work + 30s cleanup + 60s margin = 10m catalog timeout
//	pod-autoscaling:          13m30s work + 30s cleanup + 60s margin = 15m catalog timeout
//	ai-service-metrics:        8m30s work + 30s cleanup + 60s margin = 10m catalog timeout
//	robust-controller:         6m30s work + 30s cleanup + 60s margin =  8m catalog timeout
//	inference-gateway:        13m30s work + 30s cleanup + 60s margin = 15m catalog timeout
//	workload-sandboxing:       8m30s work + 30s cleanup + 60s margin = 10m catalog timeout
//
// dra-support can run TWO namespace-creating behavioral subtests in sequence
// (the MNNVL IMEX channel probe, then the full-GPU DRA probe). The reserve
// covers only the LAST subtest's cleanup; the IMEX subtest's own bounded
// cleanup is charged against the work budget, and both probes share that
// budget. In the supported ComputeDomain-only configuration only one of the
// two applies on any given cluster; a cluster that enables full-GPU DRA on an
// MNNVL fabric runs both and shares the 3m30s between them (#1649).
//
// This containment is deliberately LOCAL to the checks that create cluster
// resources needing reconciliation — not a validator-wide lifecycle change.
// It is not an absolute guarantee: startup skew beyond the margin, or a
// SIGKILL, can still cut cleanup off. Leaked per-run namespaces carry the
// gpuTestRunLabel ownership label for external sweeping; Job-deadline
// headroom (defaults.ValidatorJobDeadlineHeadroom, #2473) now covers the
// validator-wide lifecycle hardening this note used to defer — the
// remaining gap is signal-aware cancellation. The fallback-budget-vs-
// catalog-timeout arithmetic is asserted in
// TestGPUCheckWorkBudgetsFitCatalogTimeouts.
const (
	// gpuCheckSkewAndFlushMargin absorbs pod scheduling/startup skew (the
	// Job's activeDeadlineSeconds clock starts at Job creation; the check
	// context's deadline clock starts at process boot) and result flushing
	// after cleanup. defaults.ValidatorJobDeadlineHeadroom now sizes the
	// Job's deadline well past this margin, so the Job reliably outlives
	// the check context rather than the reverse.
	gpuCheckSkewAndFlushMargin = time.Minute

	// gpuCheckCleanupReserve is the slice of the check's remaining deadline
	// that must stay untouched by work so the deferred namespace cleanup can
	// finish and its evidence can flush before the Job is killed:
	//
	//	reserve = defaults.K8sCleanupTimeout (30s, ONE bounded namespace cleanup)
	//	        + gpuCheckSkewAndFlushMargin (60s, Job-vs-process clock skew + result flush)
	//	        = 90s
	gpuCheckCleanupReserve = defaults.K8sCleanupTimeout + gpuCheckSkewAndFlushMargin

	// gpuCheckMinWorkBudget is the floor below which a derived work budget is
	// useless: creating a namespace, scheduling a test pod, and collecting its
	// logs cannot plausibly complete faster. A deadline that leaves less work
	// time than this fails fast BEFORE any cluster resource is created.
	gpuCheckMinWorkBudget = 30 * time.Second

	// Fixed FALLBACK work budgets, used only when the parent context has no
	// deadline (standalone/library use — LoadContext always sets one). The
	// inference gateway budget covers the traffic phase that follows the
	// status checks: backends available (3m), HTTPRoute accepted (2m) and
	// the probe pod's run (6m) are individual caps under it, not a sum. The
	// workload-sandboxing budget covers two CPU-only probe pods in sequence,
	// each bounded by defaults.DRATestPodTimeout; a sandbox runtime (Kata
	// boots a guest VM) starts slower than runc, so it gets the GPU checks'
	// budget.
	draSupportWorkBudget         = 3*time.Minute + 30*time.Second
	secureAccessWorkBudget       = 8*time.Minute + 30*time.Second
	driverRuntimeWorkBudget      = 8*time.Minute + 30*time.Second
	podAutoscalingWorkBudget     = 13*time.Minute + 30*time.Second
	aiServiceMetricsWorkBudget   = 8*time.Minute + 30*time.Second
	robustControllerWorkBudget   = 6*time.Minute + 30*time.Second
	inferenceGatewayWorkBudget   = 13*time.Minute + 30*time.Second
	workloadSandboxingWorkBudget = 8*time.Minute + 30*time.Second
)

// gpuCheckWorkBudget derives the work budget for a GPU allocation conformance
// check from the parent context's deadline: remaining-until-deadline minus
// gpuCheckCleanupReserve. When the context carries no deadline, the fixed
// fallback budget is returned unchanged.
//
// When the remaining time cannot fit the reserve plus gpuCheckMinWorkBudget
// of useful work, an ErrCodeTimeout error is returned — callers MUST return
// it before creating any cluster resources, because a check that starts work
// it cannot clean up would trade a clear configuration error for a leaked
// namespace holding an allocated GPU.
func gpuCheckWorkBudget(ctx context.Context, fallback time.Duration) (time.Duration, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return fallback, nil
	}
	remaining := time.Until(deadline)
	budget := remaining - gpuCheckCleanupReserve
	if budget < gpuCheckMinWorkBudget {
		return 0, errors.New(errors.ErrCodeTimeout, fmt.Sprintf(
			"check timeout too short to guarantee cleanup: %s remain until the deadline but at least %s is needed (%s minimum work + %s cleanup reserve) — no test resources were created; raise the check's catalog timeout",
			remaining.Round(time.Second), gpuCheckMinWorkBudget+gpuCheckCleanupReserve,
			gpuCheckMinWorkBudget, gpuCheckCleanupReserve))
	}
	return budget, nil
}
