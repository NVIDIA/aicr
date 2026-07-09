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
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	// gpuTestNamespacePrefix names the PER-RUN test namespace. A fresh
	// namespace per run makes cleanup a single bounded operation — delete the
	// namespace and wait — and closes the ambiguous-create leak window: once
	// the namespace is Terminating, the NamespaceLifecycle admission plugin
	// rejects subsequent Creates in it, and the namespace deletion controller
	// durably garbage-collects any content that still lands (e.g. an
	// already-admitted in-flight Create committing late) before finalizing.
	gpuTestNamespacePrefix = "aicr-gpu-test-"
	// gpuTestRunLabel is the ownership label stamped on the per-run test
	// namespace; its value is the run's 128-bit random token. Cleanup uses it
	// to verify that a namespace found at the expected name really belongs to
	// this run before deleting anything, and an external label-based sweeper
	// can use it to find leaked test namespaces — the residual paths no
	// in-process budget can cover (SIGKILL, or scheduling/image-pull skew
	// beyond the margin the work budgets reserve; see gpuCheckWorkBudget in
	// consts.go).
	gpuTestRunLabel = "aicr.run/gpu-test-run"
	// gpuTestPodPrefix names the GPU allocation test pod. Mechanism-neutral:
	// the same pod shape is used in DRA mode (ResourceClaim) and device
	// plugin mode (nvidia.com/gpu limits).
	gpuTestPodPrefix = "gpu-alloc-test-"
	gpuClaimPrefix   = "gpu-claim-"
	// noAllocProbePrefix names the standalone probe pod that is granted no
	// GPU at all (no ResourceClaims, no nvidia.com/gpu limits).
	noAllocProbePrefix = "no-alloc-probe-"

	// cudaTestImage runs the in-container GPU visibility verification.
	cudaTestImage = "nvidia/cuda:12.9.0-base-ubuntu24.04"
	// bashShell is the shell binary used by cudaTestImage containers.
	bashShell = "bash"

	// containerNameGPUTest is the granted container in the GPU test pod: the
	// only container given the GPU (DRA claim or device plugin limits).
	containerNameGPUTest = "gpu-test"
	// containerNameUnauthorized is the identical sibling container in the GPU
	// test pod that is granted nothing — it must not see any accelerator
	// (container-level isolation, kubernetes-sigs/ai-conformance#75).
	containerNameUnauthorized = "unauthorized"

	// gpuAbsenceProbeScript exits 0 only when no NVIDIA device nodes are
	// visible in the container. Shared by the unauthorized sibling container
	// and the standalone no-allocation probe pod. This NEGATIVE probe
	// deliberately checks /dev/nvidia* device files rather than nvidia-smi:
	// unauthorized containers and the busybox no-allocation probe must not
	// depend on CUDA tooling being present to prove GPU absence.
	//
	// Deliberate failure-surface note: CUDA images bake
	// NVIDIA_VISIBLE_DEVICES=all, so on clusters whose container toolkit
	// honors the legacy envvar device-list strategy the unauthorized sibling
	// WILL see GPUs and this check fails. That is intended (upstream
	// kubernetes-sigs/ai-conformance#75 semantics): such clusters genuinely
	// expose GPUs to containers that requested nothing, which is exactly the
	// isolation failure secure-accelerator-access exists to catch. AICR's
	// gpu-operator default uses the CDI device-list strategy, which ignores
	// the image env var.
	gpuAbsenceProbeScript = "if ls /dev/nvidia* 2>/dev/null; then echo 'FAIL: GPU visible without GPU allocation' && exit 1; else echo 'PASS: GPU isolated' && exit 0; fi"

	// gpuExclusiveGrantProbeScript is the POSITIVE probe for the AUTHORIZED
	// container: it must see exactly ONE usable GPU. A bare /dev/nvidia*
	// listing would also pass for a container exposed to every GPU on the
	// node (isolation broken) or to control devices only (/dev/nvidiactl,
	// /dev/nvidia-uvm — no usable GPU), so the granted container counts GPUs
	// via nvidia-smi (available in cudaTestImage) and emits the count and
	// UUIDs to its logs as evidence. The success gate is a `case` matching
	// the literal string "1" — fail closed: an empty or non-numeric count
	// (e.g. a failed pipeline stage) lands in the FAIL branch instead of
	// erroring inside a numeric `[ -ne ]` test and falling through to PASS.
	gpuExclusiveGrantProbeScript = `uuids="$(nvidia-smi --query-gpu=uuid --format=csv,noheader)" || { echo "FAIL: nvidia-smi cannot enumerate GPUs - no usable GPU granted"; exit 1; }; ` +
		`count="$(printf '%s\n' "$uuids" | grep -c .)"; ` +
		`echo "granted GPU count: ${count}"; echo "granted GPU UUIDs: ${uuids}"; ` +
		`case "${count}" in 1) echo "PASS: exactly one usable GPU visible"; exit 0;; ` +
		`*) echo "FAIL: expected exactly 1 usable GPU, saw ${count:-<no count>}"; exit 1;; esac`
)

// gpuTestRun holds per-invocation resource names to avoid collisions,
// including the per-run test namespace every resource is created in. Shared
// by the DRA and device plugin secure-access paths (claimName is unused in
// device plugin mode). The name suffix is a 128-bit random token (32 hex
// chars) so a same-name collision with a namespace this run did not create is
// practically impossible; the token doubles as the value of the
// gpuTestRunLabel ownership label on the namespace.
//
// nsObserved/nsUID/nsForeign/nsNeverCreated carry namespace-ownership state
// from createGPUTestNamespace to the deferred cleanupGPUTestNamespace:
//   - nsObserved: the namespace was positively observed (Create succeeded, or
//     an ownership-verified Get after AlreadyExists). When false at cleanup
//     time the Create was ambiguous — a NotFound is NOT confirmed absence.
//   - nsUID: the observed namespace's UID; cleanup deletes with a
//     metav1.Preconditions{UID} guard so it can never delete a recreated or
//     foreign namespace at the same name.
//   - nsForeign: the name is occupied by a namespace WITHOUT this run's
//     ownership token — cleanup must not touch it.
//   - nsNeverCreated: the Create was DEFINITIVELY rejected by the apiserver
//     (authn/authz/admission/validation verdict) and cannot have persisted
//     the namespace — cleanup has nothing to settle or delete.
type gpuTestRun struct {
	token          string
	namespace      string
	podName        string
	claimName      string
	noAllocPodName string

	nsObserved     bool
	nsUID          types.UID
	nsForeign      bool
	nsNeverCreated bool
}

type draPatternReport struct {
	PodName             string
	PodPhase            corev1.PodPhase
	ResourceClaimCount  int
	GPULimitsCount      int
	HostPathGPUMounts   int
	ClaimState          string
	ClaimAllocationInfo string
	// UnauthorizedExitCode is the exit code of the unauthorized sibling
	// container (0 = confirmed no accelerator visible).
	UnauthorizedExitCode int32
}

// isolationProbeReport captures the observed state of the standalone
// no-allocation probe pod (shared by the DRA and device plugin paths).
type isolationProbeReport struct {
	PodName           string
	PodPhase          corev1.PodPhase
	ExitCode          int32
	ResourceClaims    int
	HostPathGPUMounts int
	Logs              string
}

func newGPUTestRun() (*gpuTestRun, error) {
	// 16 bytes (128 bits) of entropy: a 32-bit suffix makes accidental
	// same-name collisions plausible across runs, and the unconditional
	// namespace cleanup would then delete a namespace some other run owns.
	// All name prefixes are short enough that prefix + 32 hex chars stays
	// within the 63-char DNS-1123 label limit for namespace and pod names.
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate random suffix", err)
	}
	token := hex.EncodeToString(b)
	return &gpuTestRun{
		token:          token,
		namespace:      gpuTestNamespacePrefix + token,
		podName:        gpuTestPodPrefix + token,
		claimName:      gpuClaimPrefix + token,
		noAllocPodName: noAllocProbePrefix + token,
	}, nil
}

// createGPUTestNamespace creates the per-run test namespace stamped with the
// run's ownership label (gpuTestRunLabel=token) and records the created UID
// so cleanup can delete with a metav1.Preconditions{UID} guard.
//
// On AlreadyExists it verifies ownership before adopting: with a 128-bit
// random name token, an existing namespace bearing our token can only be our
// own ambiguously committed create (response lost, object persisted) — adopt
// it and capture its UID. Anything else is a FOREIGN namespace: fail the
// check with ErrCodeConflict and mark the run so cleanup never deletes it.
func createGPUTestNamespace(ctx context.Context, clientset kubernetes.Interface, run *gpuTestRun) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   run.namespace,
			Labels: map[string]string{gpuTestRunLabel: run.token},
		},
	}
	created, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if err == nil {
		run.nsObserved = true
		run.nsUID = created.UID
		return nil
	}
	if !k8serrors.IsAlreadyExists(err) {
		if createDefinitelyRejected(err) {
			// A definitive apiserver rejection (Forbidden, Invalid, ...)
			// cannot have persisted the namespace — record that so cleanup
			// neither burns its budget settling nor warns that the
			// namespace "may still surface".
			run.nsNeverCreated = true
		}
		// Otherwise ambiguous (timeout, transport error, 5xx): the namespace
		// may have been persisted anyway. nsObserved stays false so cleanup
		// runs its settle loop instead of trusting a first NotFound.
		return errors.Wrap(errors.ErrCodeInternal, "failed to create test namespace", err)
	}
	existing, getErr := clientset.CoreV1().Namespaces().Get(ctx, run.namespace, metav1.GetOptions{})
	if getErr != nil {
		// Ownership unknown — fail closed: cleanup must never delete a
		// namespace this run cannot prove it owns.
		run.nsForeign = true
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf(
			"test namespace %s already exists and its ownership could not be verified", run.namespace), getErr)
	}
	if existing.Labels[gpuTestRunLabel] != run.token {
		run.nsForeign = true
		return errors.New(errors.ErrCodeConflict, fmt.Sprintf(
			"test namespace %s already exists without this run's %s ownership label — refusing to reuse or delete a foreign namespace",
			run.namespace, gpuTestRunLabel))
	}
	run.nsObserved = true
	run.nsUID = existing.UID
	return nil
}

// createDefinitelyRejected reports whether a namespace Create error is a
// DEFINITIVE apiserver rejection that cannot have persisted the object —
// an authn/authz/admission/validation verdict rendered before storage — as
// opposed to an ambiguous outcome (timeout, transport failure, 5xx) where
// the write may have committed despite the error. Unknown errors default to
// ambiguous: fail closed toward cleanup's settle loop, never toward assuming
// absence.
func createDefinitelyRejected(err error) bool {
	return k8serrors.IsForbidden(err) ||
		k8serrors.IsUnauthorized(err) ||
		k8serrors.IsInvalid(err) ||
		k8serrors.IsBadRequest(err) ||
		k8serrors.IsMethodNotSupported(err) ||
		k8serrors.IsNotAcceptable(err) ||
		k8serrors.IsUnsupportedMediaType(err) ||
		k8serrors.IsRequestEntityTooLargeError(err)
}

// CheckSecureAcceleratorAccess validates CNCF requirement #3: Secure Accelerator Access.
// The CNCF spec permits GPU allocation via either DRA or the device plugin, so the
// check probes which mechanism is live (see detectGPUAllocationMode) and exercises
// the matching isolation test: a multi-container GPU test pod whose first container
// is granted the GPU (DRA ResourceClaim, or resources.limits["nvidia.com/gpu"]) and
// whose unauthorized sibling container must not see any accelerator, plus a
// standalone no-allocation probe pod that must not see any GPU device. Neither
// mechanism usable is an environment failure.
func CheckSecureAcceleratorAccess(ctx *validators.Context) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// Bound ALL work to the check-local budget so one bounded namespace
	// cleanup plus scheduling-skew/result-flush margin always fit inside the
	// check's deadline — see gpuCheckWorkBudget in consts.go. Fail fast (no
	// resources created yet) when the deadline cannot fit the cleanup reserve.
	// The deferred cleanup runs on its own background context and is NOT
	// bounded by this.
	budget, err := gpuCheckWorkBudget(ctx.Ctx, secureAccessWorkBudget)
	if err != nil {
		return err
	}
	workCtx, cancelWork := ctx.Timeout(budget)
	defer cancelWork()
	work := *ctx
	work.Ctx = workCtx
	ctx = &work

	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}

	// Detect which GPU allocation mechanism is live (and which
	// resource.k8s.io API version the cluster serves — v1, v1beta2, or
	// v1beta1). The NVIDIA DRA driver's supported configuration is
	// ComputeDomain-only (no gpu.nvidia.com DeviceClass); whole GPUs come
	// from the device plugin (#1327).
	mode, err := detectGPUAllocationMode(ctx.Ctx, ctx.Clientset, dynClient)
	if err != nil {
		return err
	}

	collectSecureAccessBaselineArtifacts(ctx, dynClient, mode.APIVersion)

	recordRawTextArtifact(ctx, "GPU allocation mode",
		"kubectl get deviceclass gpu.nvidia.com; kubectl get resourceslices; kubectl get nodes -o wide",
		mode.Summary())

	// VERIFY, then SELECT (#1327): a recipe-configured policy pins the
	// mechanism this check exercises — the cluster must actually serve it
	// (fail closed on drift, no silent fallback to the other mechanism).
	// Only unspecified (standalone runs) keeps the capability-driven
	// dispatch below.
	policy := ctx.ValidationInput.GetGPUAllocationPolicy()
	// Evidence BEFORE the verdict: a Verify failure below must leave the
	// configured policy in the artifacts.
	recordRawTextArtifact(ctx, "GPU allocation policy", "",
		fmt.Sprintf("configured policy: %s", policy))
	if verifyErr := verifyGPUAllocationPolicy(policy, mode); verifyErr != nil {
		return verifyErr
	}

	switch {
	case policy == validatorv1.GPUAllocationPolicyDRAResourceClaim,
		policy == validatorv1.GPUAllocationPolicyUnspecified && mode.DRAUsable:
		recordRawTextArtifact(ctx, "Secure access mode", "", "mode exercised: DRA (gpu.nvidia.com ResourceClaim)")
		return runDRASecureAccessTest(ctx, dynClient, mode.APIVersion)
	case policy == validatorv1.GPUAllocationPolicyDevicePluginExtendedResource,
		policy == validatorv1.GPUAllocationPolicyUnspecified && mode.DevicePluginUsable:
		// Sound attribution: constrain the test pod (required node affinity)
		// to the probe's device-plugin nodes — every listed node advertises
		// scalar allocatable nvidia.com/gpu, and on such a node the
		// extended-resource request is device-plugin-served by definition —
		// even when a DeviceClass maps nvidia.com/gpu to DRA via
		// spec.extendedResourceName (KEP-5004), which applies only where
		// scalar allocatable is absent/zero. Record the mapping as
		// attribution evidence when present.
		if mode.ExtendedResourceDRABacked {
			recordRawTextArtifact(ctx, "Extended-resource attribution (KEP-5004)",
				"kubectl get deviceclasses -o yaml", mode.ExtendedResourceDetail)
		}
		recordRawTextArtifact(ctx, "Secure access mode", "", "mode exercised: device plugin (nvidia.com/gpu limits)")
		return runDevicePluginSecureAccessTest(ctx, mode.DevicePluginNodes)
	default:
		return errors.New(errors.ErrCodeUnavailable, fmt.Sprintf(
			"no usable GPU allocation mechanism for the secure accelerator access test: %s; %s",
			mode.DRADetail, mode.DevicePluginDetail))
	}
}

// runDRASecureAccessTest exercises secure accelerator access via DRA:
// a pod with a gpu.nvidia.com ResourceClaim, validated for DRA access
// patterns, followed by the no-allocation isolation probe. version is the
// served resource.k8s.io API version (v1, v1beta2, or v1beta1) the
// ResourceClaim is created, read, and cleaned up at.
//
// The return is NAMED so the deferred cleanup can fail an otherwise-passing
// check when cleanup terminally fails — a PASS that leaks the namespace, test
// pod, and allocated GPU claim is not a pass. A primary test error is always
// preserved (never overwritten by the cleanup error).
func runDRASecureAccessTest(ctx *validators.Context, dynClient dynamic.Interface, version string) (err error) {
	run, runErr := newGPUTestRun()
	if runErr != nil {
		return runErr
	}

	// Register cleanup BEFORE the first Create: the apiserver can persist an
	// object and then lose or time out the response, so an error return from
	// deploy does not prove nothing was created — an ambiguously created pod
	// would otherwise stay bound and hold a GPU. Deleting the per-run
	// namespace reconciles every test resource in one bounded operation (see
	// cleanupGPUTestNamespace).
	defer func() { //nolint:contextcheck // cleanup runs on its own bounded context
		status, cleanupErr := cleanupGPUTestNamespace(ctx.Clientset, run)
		recordRawTextArtifact(ctx, "Cleanup test namespace",
			cleanupInspectEquivalent(run.namespace), status)
		if cleanupErr != nil && err == nil {
			err = cleanupErr
		}
	}()
	if err = deployDRATestResources(ctx.Ctx, ctx.Clientset, dynClient, run, ctx.Tolerations, version); err != nil {
		return err
	}

	// Wait for test pod to reach terminal state.
	pod, err := waitForGPUTestPod(ctx.Ctx, ctx.Clientset, run)
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "Pod status",
		fmt.Sprintf("kubectl get pod %s -n %s -o wide", run.podName, run.namespace),
		fmt.Sprintf("Name:      %s/%s\nPhase:     %s\nNode:      %s\nClaims:    %d resource claims",
			pod.Namespace, pod.Name, pod.Status.Phase, valueOrUnknown(pod.Spec.NodeName), len(pod.Spec.ResourceClaims)))
	// Logs BEFORE the pattern verdict: a failing validation must still ship
	// the pod's container logs — they are the primary diagnostic and would
	// otherwise be missing exactly when the check fails.
	recordGPUPodContainerLogs(ctx, pod, "GPU test pod logs")

	// Validate DRA access patterns on the completed pod.
	patternReport, err := validateDRAPatterns(ctx.Ctx, dynClient, pod, run, version)
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "Pod resourceClaims",
		fmt.Sprintf("kubectl get pod %s -n %s -o jsonpath='{.spec.resourceClaims}'", run.podName, run.namespace),
		fmt.Sprintf("Pod:             %s/%s\nResourceClaims:  %d\nGPULimits:       %d",
			run.namespace, patternReport.PodName, patternReport.ResourceClaimCount, patternReport.GPULimitsCount))
	recordRawTextArtifact(ctx, "Pod volumes (no hostPath)",
		fmt.Sprintf("kubectl get pod %s -n %s -o jsonpath='{.spec.volumes}'", run.podName, run.namespace),
		fmt.Sprintf("Pod:               %s/%s\nHostPathGPUMounts: %d",
			run.namespace, patternReport.PodName, patternReport.HostPathGPUMounts))
	recordRawTextArtifact(ctx, "ResourceClaim allocation",
		fmt.Sprintf("kubectl get resourceclaim %s -n %s -o wide", run.claimName, run.namespace),
		fmt.Sprintf("Name:             %s/%s\nState:            %s\nAllocationStatus: %s",
			run.namespace, run.claimName, patternReport.ClaimState, patternReport.ClaimAllocationInfo))
	recordRawTextArtifact(ctx, "Container-level isolation", "",
		fmt.Sprintf("granted container:      %s (ResourceClaim, exactly one GPU visible via nvidia-smi, exit 0)\nunauthorized container: %s (no allocation, no GPU visible, exit %d)",
			containerNameGPUTest, containerNameUnauthorized, patternReport.UnauthorizedExitCode))

	// Validate isolation: a pod without DRA claims cannot access GPU devices.
	// Target the same node as the DRA test pod — isolation must be proven on the
	// GPU node, not a control-plane node that has no GPUs in the first place.
	// The probe records its own logs artifact before evaluating pass/fail.
	isolationReport, err := verifyNoAllocationIsolation(ctx, run, pod.Spec.NodeName)
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "DRA Isolation Test",
		fmt.Sprintf("kubectl logs %s -n %s", run.noAllocPodName, run.namespace),
		fmt.Sprintf("Pod:               %s/%s\nPhase:             %s\nExitCode:          %d\nResourceClaims:    %d\nHostPathGPUMounts: %d",
			run.namespace, isolationReport.PodName, isolationReport.PodPhase,
			isolationReport.ExitCode, isolationReport.ResourceClaims, isolationReport.HostPathGPUMounts))
	return nil
}

// devicePluginPatternReport captures the observed state of the device plugin
// isolation test pod.
type devicePluginPatternReport struct {
	PodName            string
	PodPhase           corev1.PodPhase
	GPULimitsCount     int
	ResourceClaimCount int
	HostPathGPUMounts  int
	// UnauthorizedExitCode is the exit code of the unauthorized sibling
	// container (0 = confirmed no accelerator visible).
	UnauthorizedExitCode int32
}

// runDevicePluginSecureAccessTest exercises secure accelerator access via the
// device plugin: a pod requesting resources.limits["nvidia.com/gpu"]=1 (no
// ResourceClaims), validated for device plugin access patterns, followed by
// the same no-allocation isolation probe used by the DRA path.
// pluginNodes is the probe's sorted set of Ready, schedulable nodes with
// scalar allocatable nvidia.com/gpu; the test pod is constrained to that set
// via REQUIRED node affinity so the scheduler can pick any detected node with
// a free GPU (pinning spec.nodeName to a single node false-fails when that
// node's GPUs are all in use — allocatable is capacity, not free capacity).
// Attribution stays sound: every listed node advertises scalar allocatable
// nvidia.com/gpu, so wherever the pod lands the request is
// device-plugin-served.
// The return is NAMED so the deferred cleanup can fail an otherwise-passing
// check when cleanup terminally fails (see runDRASecureAccessTest).
func runDevicePluginSecureAccessTest(ctx *validators.Context, pluginNodes []string) (err error) {
	if len(pluginNodes) == 0 {
		return errors.New(errors.ErrCodeInternal,
			"device plugin secure access test invoked without any device-plugin node")
	}
	run, runErr := newGPUTestRun()
	if runErr != nil {
		return runErr
	}

	// Register cleanup BEFORE the first Create: the apiserver can persist the
	// pod and then lose or time out the response, so an error return from
	// deploy does not prove nothing was created — an ambiguously created pod
	// would otherwise stay bound and hold a GPU. Deleting the per-run
	// namespace reconciles every test resource in one bounded operation (see
	// cleanupGPUTestNamespace).
	defer func() { //nolint:contextcheck // cleanup runs on its own bounded context
		status, cleanupErr := cleanupGPUTestNamespace(ctx.Clientset, run)
		recordRawTextArtifact(ctx, "Cleanup test namespace",
			cleanupInspectEquivalent(run.namespace), status)
		if cleanupErr != nil && err == nil {
			err = cleanupErr
		}
	}()
	if err = deployDevicePluginTestResources(ctx.Ctx, ctx.Clientset, run, ctx.Tolerations, pluginNodes); err != nil {
		return err
	}

	pod, err := waitForGPUTestPod(ctx.Ctx, ctx.Clientset, run)
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "Pod status",
		fmt.Sprintf("kubectl get pod %s -n %s -o wide", run.podName, run.namespace),
		fmt.Sprintf("Name:      %s/%s\nPhase:     %s\nNode:      %s\nGPULimits: %s: 1",
			pod.Namespace, pod.Name, pod.Status.Phase, valueOrUnknown(pod.Spec.NodeName), resourceNVIDIAGPU))
	// Logs BEFORE the pattern verdict: a failing validation must still ship
	// the pod's container logs (see the DRA path for rationale).
	recordGPUPodContainerLogs(ctx, pod, "GPU test pod logs")

	patternReport, err := validateDevicePluginPatterns(pod)
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "Pod GPU limits (device plugin)",
		fmt.Sprintf("kubectl get pod %s -n %s -o jsonpath='{.spec.containers[*].resources.limits}'", run.podName, run.namespace),
		fmt.Sprintf("Pod:             %s/%s\nGPULimits:       %d\nResourceClaims:  %d",
			run.namespace, patternReport.PodName, patternReport.GPULimitsCount, patternReport.ResourceClaimCount))
	recordRawTextArtifact(ctx, "Pod volumes (no hostPath)",
		fmt.Sprintf("kubectl get pod %s -n %s -o jsonpath='{.spec.volumes}'", run.podName, run.namespace),
		fmt.Sprintf("Pod:               %s/%s\nHostPathGPUMounts: %d",
			run.namespace, patternReport.PodName, patternReport.HostPathGPUMounts))
	recordRawTextArtifact(ctx, "Container-level isolation", "",
		fmt.Sprintf("granted container:      %s (%s limits, exactly one GPU visible via nvidia-smi, exit 0)\nunauthorized container: %s (no allocation, no GPU visible, exit %d)",
			containerNameGPUTest, resourceNVIDIAGPU, containerNameUnauthorized, patternReport.UnauthorizedExitCode))

	// Validate isolation: a pod without any GPU allocation cannot access GPU
	// devices. Same probe as the DRA path, pinned to the GPU node. The probe
	// records its own logs artifact before evaluating pass/fail.
	isolationReport, err := verifyNoAllocationIsolation(ctx, run, pod.Spec.NodeName)
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "Device Plugin Isolation Test",
		fmt.Sprintf("kubectl logs %s -n %s", run.noAllocPodName, run.namespace),
		fmt.Sprintf("Pod:               %s/%s\nPhase:             %s\nExitCode:          %d\nResourceClaims:    %d\nHostPathGPUMounts: %d",
			run.namespace, isolationReport.PodName, isolationReport.PodPhase,
			isolationReport.ExitCode, isolationReport.ResourceClaims, isolationReport.HostPathGPUMounts))
	return nil
}

// deployDevicePluginTestResources creates the per-run test namespace and the
// device plugin GPU test pod, constrained (required node affinity) to the
// detected device-plugin nodes. No ResourceClaim is involved in this mode.
func deployDevicePluginTestResources(ctx context.Context, clientset kubernetes.Interface, run *gpuTestRun, tolerations []corev1.Toleration, nodeNames []string) error {
	if err := createGPUTestNamespace(ctx, clientset, run); err != nil {
		return err
	}

	pod := buildDevicePluginTestPod(run, tolerations, nodeNames)
	if err := createPodWhenSAReady(ctx, clientset, run.namespace, pod); err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to create device plugin test pod")
	}

	return nil
}

// buildDevicePluginTestPod returns the Pod spec for the device plugin GPU
// allocation test: the granted container requests one nvidia.com/gpu via
// resources.limits (no ResourceClaims) and must observe exactly one usable
// GPU (gpuExclusiveGrantProbeScript), while an identical unauthorized sibling
// container is granted nothing and must not see any accelerator
// (container-level isolation subtest). nodeNames constrains the pod via
// REQUIRED node affinity to the detected device-plugin nodes: the scheduler
// picks any listed node with a free GPU (a single spec.nodeName pin would
// false-fail when that node's GPUs are all in use), and because every listed
// node advertises scalar allocatable nvidia.com/gpu, device-plugin mediation
// stays sound even under a KEP-5004 extendedResourceName mapping (DRA serves
// the extended resource only where scalar allocatable is absent/zero, never
// on such a node). The affinity matches metadata.name via matchFields —
// nodeNames are node OBJECT names, and the kubernetes.io/hostname label is
// not guaranteed to equal (or even exist for) the node name, which could
// leave a valid probe Pending or weaken attribution on a name/label
// collision. matchFields In accepts exactly one value per requirement, so
// the node set is expressed as OR-ed single-node terms.
// tolerations, when non-nil, replace the default tolerate-all policy.
func buildDevicePluginTestPod(run *gpuTestRun, tolerations []corev1.Toleration, nodeNames []string) *corev1.Pod {
	if tolerations == nil {
		tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	}
	terms := make([]corev1.NodeSelectorTerm, 0, len(nodeNames))
	for _, nodeName := range nodeNames {
		terms = append(terms, corev1.NodeSelectorTerm{
			MatchFields: []corev1.NodeSelectorRequirement{{
				Key:      metav1.ObjectNameField,
				Operator: corev1.NodeSelectorOpIn,
				Values:   []string{nodeName},
			}},
		})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      run.podName,
			Namespace: run.namespace,
		},
		Spec: corev1.PodSpec{
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: terms,
					},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations:   tolerations,
			Containers: []corev1.Container{
				{
					Name:    containerNameGPUTest,
					Image:   cudaTestImage,
					Command: []string{bashShell, "-c", gpuExclusiveGrantProbeScript},
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceName(resourceNVIDIAGPU): resource.MustParse("1"),
						},
					},
				},
				unauthorizedSiblingContainer(),
			},
		},
	}
}

// validateDevicePluginPatterns verifies the completed pod uses proper device
// plugin access patterns: nvidia.com/gpu limits on the granted container only
// (no DRA resourceClaims), no hostPath volumes to GPU devices, successful
// completion (the allocated GPU was visible in the granted container), and
// container-level isolation (the unauthorized sibling saw no accelerator).
func validateDevicePluginPatterns(pod *corev1.Pod) (*devicePluginPatternReport, error) {
	report := &devicePluginPatternReport{
		PodName:            pod.Name,
		PodPhase:           pod.Status.Phase,
		ResourceClaimCount: len(pod.Spec.ResourceClaims),
	}

	// 1. Pod does NOT use DRA resourceClaims in device plugin mode.
	if len(pod.Spec.ResourceClaims) != 0 {
		return nil, errors.New(errors.ErrCodeInternal,
			"device plugin test pod unexpectedly uses DRA resourceClaims")
	}

	// 2. Only the granted (first) container requests GPUs via nvidia.com/gpu
	// resource limits; the unauthorized sibling container must request none.
	for i, c := range pod.Spec.Containers {
		hasGPU := false
		if c.Resources.Limits != nil {
			_, hasGPU = c.Resources.Limits[corev1.ResourceName(resourceNVIDIAGPU)]
		}
		if i == 0 {
			if !hasGPU {
				return nil, errors.New(errors.ErrCodeInternal,
					fmt.Sprintf("device plugin test pod does not request %s in resources.limits", resourceNVIDIAGPU))
			}
			report.GPULimitsCount++
			continue
		}
		if hasGPU {
			return nil, errors.New(errors.ErrCodeInternal, fmt.Sprintf(
				"unauthorized container %s unexpectedly requests %s in resources.limits", c.Name, resourceNVIDIAGPU))
		}
	}

	// 3. No hostPath volumes to /dev/nvidia*.
	for _, vol := range pod.Spec.Volumes {
		if vol.HostPath != nil && strings.Contains(vol.HostPath.Path, "/dev/nvidia") {
			report.HostPathGPUMounts++
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("pod has hostPath volume to %s", vol.HostPath.Path))
		}
	}

	// 4. Pod completed successfully (the allocated GPU was visible in the
	// granted container) AND the unauthorized sibling container confirmed no
	// accelerator access.
	code, err := verifyCompletionAndContainerIsolation(pod, "device plugin")
	report.UnauthorizedExitCode = code
	if err != nil {
		return nil, err
	}

	return report, nil
}

// collectSecureAccessBaselineArtifacts records best-effort cluster baseline
// evidence. draAPIVersion is the served resource.k8s.io version discovered by
// the allocation-mode probe ("" when the group is not served, in which case
// the ResourceSlice artifacts record that instead of listing).
func collectSecureAccessBaselineArtifacts(ctx *validators.Context, dynClient dynamic.Interface, draAPIVersion string) {
	// ClusterPolicy status.
	clusterPolicyGVR := schema.GroupVersionResource{
		Group: apiGroupNVIDIA, Version: "v1", Resource: "clusterpolicies",
	}
	cp, err := dynClient.Resource(clusterPolicyGVR).Get(ctx.Ctx, "cluster-policy", metav1.GetOptions{})
	if err != nil {
		recordRawTextArtifact(ctx, "ClusterPolicy status", "kubectl get clusterpolicy -o wide",
			fmt.Sprintf("failed to read ClusterPolicy: %v", err))
	} else {
		state, _, _ := unstructured.NestedString(cp.Object, "status", "state")
		recordRawTextArtifact(ctx, "ClusterPolicy status", "kubectl get clusterpolicy -o wide",
			fmt.Sprintf("Name:   %s\nState:  %s", cp.GetName(), valueOrUnknown(state)))
	}

	// GPU operator pods.
	operatorPods, err := ctx.Clientset.CoreV1().Pods(defaults.GPUOperatorNamespace).List(ctx.Ctx, metav1.ListOptions{})
	if err != nil {
		recordRawTextArtifact(ctx, "GPU operator pods", "kubectl get pods -n gpu-operator -o wide",
			fmt.Sprintf("failed to list gpu-operator pods: %v", err))
	} else {
		var podSummary strings.Builder
		for _, pod := range operatorPods.Items {
			fmt.Fprintf(&podSummary, "%-46s ready=%s phase=%s node=%s\n",
				pod.Name, podReadyCount(pod), pod.Status.Phase, pod.Spec.NodeName)
		}
		recordRawTextArtifact(ctx, "GPU operator pods", "kubectl get pods -n gpu-operator -o wide", podSummary.String())
	}

	// GPU operator DaemonSets.
	daemonSets, err := ctx.Clientset.AppsV1().DaemonSets(defaults.GPUOperatorNamespace).List(ctx.Ctx, metav1.ListOptions{})
	if err != nil {
		recordRawTextArtifact(ctx, "GPU operator DaemonSets", "kubectl get ds -n gpu-operator",
			fmt.Sprintf("failed to list gpu-operator DaemonSets: %v", err))
	} else {
		var dsSummary strings.Builder
		for _, ds := range daemonSets.Items {
			fmt.Fprintf(&dsSummary, "%-38s ready=%d/%d\n",
				ds.Name, ds.Status.NumberReady, ds.Status.DesiredNumberScheduled)
		}
		recordRawTextArtifact(ctx, "GPU operator DaemonSets", "kubectl get ds -n gpu-operator", dsSummary.String())
	}

	// ResourceSlices summary + details, at the served resource.k8s.io version.
	if draAPIVersion == "" {
		notServed := fmt.Sprintf("no served %s API version — ResourceSlices unavailable", apiGroupResourceK8sIO)
		recordRawTextArtifact(ctx, "ResourceSlices", "kubectl get resourceslices -o wide", notServed)
		recordRawTextArtifact(ctx, "GPU devices in ResourceSlice", "kubectl get resourceslices -o yaml", notServed)
		return
	}
	slices, err := dynClient.Resource(draGVRAt(draAPIVersion, "resourceslices")).List(ctx.Ctx, metav1.ListOptions{})
	if err != nil {
		recordRawTextArtifact(ctx, "ResourceSlices", "kubectl get resourceslices -o wide",
			fmt.Sprintf("failed to list ResourceSlices: %v", err))
		recordRawTextArtifact(ctx, "GPU devices in ResourceSlice", "kubectl get resourceslices -o yaml",
			fmt.Sprintf("failed to list ResourceSlices: %v", err))
		return
	}
	var sliceSummary strings.Builder
	for _, s := range slices.Items {
		driver, _, _ := unstructured.NestedString(s.Object, "spec", "driver")
		nodeName, _, _ := unstructured.NestedString(s.Object, "spec", "nodeName")
		fmt.Fprintf(&sliceSummary, "%-50s node=%s driver=%s\n", s.GetName(), nodeName, driver)
	}
	recordRawTextArtifact(ctx, "ResourceSlices", "kubectl get resourceslices -o wide", sliceSummary.String())
	// UnstructuredContent(), not slices.Object: on an UnstructuredList the
	// Object map excludes the decoded Items (a separate struct field), so
	// serializing slices.Object would emit only apiVersion/kind/metadata and
	// drop every device from the evidence. UnstructuredContent materializes
	// the items back into the map.
	recordObjectYAMLArtifact(ctx, "GPU devices in ResourceSlice", "kubectl get resourceslices -o yaml", slices.UnstructuredContent())
}

// deployDRATestResources creates the per-run test namespace, ResourceClaim,
// and Pod for the DRA test.
// tolerations, when non-nil, replace the default tolerate-all policy on the test pod.
// version is the served resource.k8s.io API version the ResourceClaim is built and created at.
func deployDRATestResources(ctx context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface, run *gpuTestRun, tolerations []corev1.Toleration, version string) error {
	// 1. Create the per-run namespace (ownership-labeled, UID recorded).
	if err := createGPUTestNamespace(ctx, clientset, run); err != nil {
		return err
	}

	// 2. Create ResourceClaim with unique name, shaped for the served version.
	claim := buildResourceClaim(run, version)
	if _, err := dynClient.Resource(draGVRAt(version, "resourceclaims")).Namespace(run.namespace).Create(
		ctx, claim, metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create ResourceClaim", err)
	}

	// 3. Create Pod with unique name. No rollback here: every caller
	// registers cleanupGPUTestNamespace BEFORE invoking this function, and
	// deleting the namespace reconciles claim and pod together — including
	// after an AMBIGUOUS create (object persisted but the response lost),
	// where deleting only the claim here would strand a bound pod.
	pod := buildDRATestPod(run, tolerations)
	if err := createPodWhenSAReady(ctx, clientset, run.namespace, pod); err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to create DRA test pod")
	}

	return nil
}

// waitForGPUTestPod waits until the GPU test pod reaches a terminal state.
// Mechanism-neutral: used by both the DRA and device plugin paths.
func waitForGPUTestPod(ctx context.Context, clientset kubernetes.Interface, run *gpuTestRun) (*corev1.Pod, error) {
	return waitForTerminalPod(ctx, clientset, run.namespace, run.podName, "GPU test pod")
}

// waitForTerminalPod waits until the named pod reaches a terminal phase
// (Succeeded or Failed), bounded by defaults.DRATestPodTimeout, using the
// WATCH API rather than GET polling (repo rule: watch, don't poll — two
// 5-minute waits at the old 500ms poll interval cost ~1200 GETs per run).
//
// Structure: an initial Get anchors the watch's ResourceVersion (and serves
// the common fast path where the pod is already terminal — fake clientsets in
// tests mark pods terminal at create); podStuckReason fails fast on every
// observed state (e.g. ImagePullBackOff); a watch channel that closes WITHOUT
// the deadline firing (apiserver hiccup, LB drop, rolling restart) loops back
// to re-Get + re-watch instead of failing — see the repo anti-pattern list.
// what names the pod in error messages.
func waitForTerminalPod(ctx context.Context, clientset kubernetes.Interface, namespace, name, what string) (*corev1.Pod, error) {
	waitCtx, cancel := context.WithTimeout(ctx, defaults.DRATestPodTimeout)
	defer cancel()

	var lastPhase corev1.PodPhase
	var lastContainerStatus string
	timeoutErr := func(cause error) error {
		return errors.Wrap(errors.ErrCodeTimeout, fmt.Sprintf(
			"%s did not complete in time (last phase=%s, status=%s)",
			what, lastPhase, lastContainerStatus), cause)
	}
	evalPod := func(pod *corev1.Pod) (*corev1.Pod, bool, error) {
		lastPhase = pod.Status.Phase
		lastContainerStatus = podWaitingStatus(pod)
		// Fail fast if pod is stuck in a non-recoverable state (e.g. ImagePullBackOff).
		if reason := podStuckReason(pod); reason != "" {
			return nil, true, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("%s stuck: %s", what, reason))
		}
		switch pod.Status.Phase { //nolint:exhaustive // only terminal states matter
		case corev1.PodSucceeded, corev1.PodFailed:
			return pod, true, nil
		default:
			return nil, false, nil
		}
	}

	// Backoff between watch-restart iterations: an apiserver (or LB) that
	// accepts watches and closes them immediately must not turn the restart
	// loop into a hot GET/WATCH spin. Starts at 250ms, doubles to a 5s cap,
	// and resets whenever a watch session delivers at least one event.
	const (
		watchRestartBackoffBase = 250 * time.Millisecond
		watchRestartBackoffCap  = 5 * time.Second
	)
	backoff := watchRestartBackoffBase
	// Set after a 410 ResourceExpired watch failure: a fresh List's
	// collection resourceVersion for the next watch attempt (an unchanged
	// pod's object resourceVersion would reproduce the 410).
	refreshedRV := ""

	for {
		pod, getErr := clientset.CoreV1().Pods(namespace).Get(waitCtx, name, metav1.GetOptions{})
		switch {
		case k8serrors.IsNotFound(getErr):
			// Pod not yet visible after create — brief pause, then re-Get.
			select {
			case <-waitCtx.Done():
				return nil, timeoutErr(waitCtx.Err())
			case <-time.After(defaults.PodPollInterval):
			}
			continue
		case getErr != nil:
			if waitCtx.Err() != nil {
				return nil, timeoutErr(getErr)
			}
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to get "+what, getErr)
		}
		if p, done, err := evalPod(pod); done {
			return p, err
		}

		// Watch from the pod's object resourceVersion unless a prior 410
		// refreshed a collection resourceVersion from a List (below).
		watchFromRV := pod.ResourceVersion
		if refreshedRV != "" {
			watchFromRV = refreshedRV
			refreshedRV = ""
		}
		watcher, watchErr := clientset.CoreV1().Pods(namespace).Watch(waitCtx, metav1.ListOptions{
			FieldSelector:   "metadata.name=" + name,
			ResourceVersion: watchFromRV,
		})
		if watchErr != nil {
			if waitCtx.Err() != nil {
				return nil, timeoutErr(watchErr)
			}
			// Permanent authorization/validation failures surface
			// immediately; only transient setup failures (410, apiserver
			// timeouts, throttling, transport drops) get the bounded
			// backoff + re-Get treatment a closed watch channel gets —
			// they are the hiccup class this restart loop exists to
			// absorb. Even if 410 persists, the loop's re-Get still
			// observes terminal phases directly (bounded polling), so the
			// verdict never depends on the watch establishing.
			if !retryableWatchSetupErr(watchErr) {
				return nil, errors.Wrap(errors.ErrCodeInternal, "failed to watch "+what, watchErr)
			}
			refreshedRV = refreshExpiredWatchRV(waitCtx, clientset, namespace, name, watchErr)
			slog.Warn("pod watch setup failed; backing off before re-get and re-watch",
				"target", what, "error", watchErr)
			select {
			case <-waitCtx.Done():
				return nil, timeoutErr(watchErr)
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > watchRestartBackoffCap {
				backoff = watchRestartBackoffCap
			}
			continue
		}
		res, done, received, evErr := consumeWatchEvents(watcher, evalPod, what)
		if done {
			return res, evErr
		}
		if waitCtx.Err() != nil {
			return nil, timeoutErr(waitCtx.Err())
		}
		// Watch channel closed without the deadline firing — apiserver
		// hiccup. Back off (bounded, context-aware), then re-Get (never
		// declare failure from a closed channel alone) and re-watch from the
		// fresh ResourceVersion.
		if received {
			backoff = watchRestartBackoffBase // progress was made — reset
		}
		select {
		case <-waitCtx.Done():
			return nil, timeoutErr(waitCtx.Err())
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > watchRestartBackoffCap {
			backoff = watchRestartBackoffCap
		}
	}
}

// consumeWatchEvents drains an established watch until a terminal verdict
// (done=true: result/err are final), a deletion (done=true with error), or
// the channel closes (done=false — the caller's restart loop takes over).
// received reports whether at least one event arrived (backoff-reset signal).
// Non-Pod objects (e.g. *metav1.Status bookmark/error events) are skipped —
// the caller's re-Get recovers.
func consumeWatchEvents(watcher watch.Interface, evalPod func(*corev1.Pod) (*corev1.Pod, bool, error), what string) (result *corev1.Pod, done, received bool, err error) {
	defer watcher.Stop()
	for event := range watcher.ResultChan() {
		received = true
		if event.Type == watch.Deleted {
			return nil, true, received, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("%s was deleted while waiting for completion", what))
		}
		p, ok := event.Object.(*corev1.Pod)
		if !ok {
			continue
		}
		if res, terminal, evalErr := evalPod(p); terminal {
			return res, true, received, evalErr
		}
	}
	return nil, false, received, nil
}

// refreshExpiredWatchRV returns a fresh COLLECTION resourceVersion for the
// next watch attempt after a 410 ResourceExpired/Gone setup failure: an
// unchanged pod's OBJECT resourceVersion can sit outside the apiserver watch
// window indefinitely, so re-Getting the pod reproduces the same 410 — only
// a List supplies a current collection version. Returns "" when the failure
// was not a 410 or the List itself failed (best-effort: the caller's loop
// then degrades to bounded re-Get polling, which still observes terminal
// phases directly).
func refreshExpiredWatchRV(ctx context.Context, clientset kubernetes.Interface, namespace, name string, watchErr error) string {
	if !k8serrors.IsResourceExpired(watchErr) && !k8serrors.IsGone(watchErr) {
		return ""
	}
	list, err := clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + name,
	})
	if err != nil {
		return ""
	}
	return list.ResourceVersion
}

// retryableWatchSetupErr reports whether a watch-establishment failure is
// transient (worth the bounded backoff + re-watch: expired resourceVersion,
// apiserver timeouts, throttling, 5xx) or permanent (authorization /
// validation — surfaced immediately rather than converted into a timeout).
// Non-Status errors default to transient: they are transport-level
// (connection reset, LB drop) and retrying under the wait deadline is the
// safe direction.
func retryableWatchSetupErr(err error) bool {
	switch {
	case k8serrors.IsResourceExpired(err), k8serrors.IsGone(err),
		k8serrors.IsTimeout(err), k8serrors.IsServerTimeout(err),
		k8serrors.IsTooManyRequests(err), k8serrors.IsInternalError(err),
		k8serrors.IsServiceUnavailable(err):
		return true
	case k8serrors.IsForbidden(err), k8serrors.IsUnauthorized(err),
		k8serrors.IsInvalid(err), k8serrors.IsBadRequest(err),
		k8serrors.IsMethodNotSupported(err), k8serrors.IsNotFound(err):
		return false
	}
	return true
}

// validateDRAPatterns verifies the completed pod uses proper DRA access
// patterns. version is the served resource.k8s.io API version the test
// ResourceClaim was created at.
func validateDRAPatterns(ctx context.Context, dynClient dynamic.Interface, pod *corev1.Pod, run *gpuTestRun, version string) (*draPatternReport, error) {
	report := &draPatternReport{
		PodName:            pod.Name,
		PodPhase:           pod.Status.Phase,
		ResourceClaimCount: len(pod.Spec.ResourceClaims),
	}

	// 1. Pod uses resourceClaims (DRA pattern).
	if len(pod.Spec.ResourceClaims) == 0 {
		return nil, errors.New(errors.ErrCodeInternal, "pod does not use DRA resourceClaims")
	}

	// 2. Only the granted (first) container may reference the claim; the
	// unauthorized sibling container must reference nothing.
	for i, c := range pod.Spec.Containers {
		if i == 0 {
			if len(c.Resources.Claims) == 0 {
				return nil, errors.New(errors.ErrCodeInternal, fmt.Sprintf(
					"granted container %s does not reference the ResourceClaim", c.Name))
			}
			continue
		}
		if len(c.Resources.Claims) != 0 {
			return nil, errors.New(errors.ErrCodeInternal, fmt.Sprintf(
				"unauthorized container %s unexpectedly references a ResourceClaim", c.Name))
		}
	}

	// 3. No nvidia.com/gpu in resources.limits. This applies to the DRA-mode
	// test pod only (it must allocate exclusively through its ResourceClaim);
	// device plugin allocation in general is a valid CNCF pattern and is
	// exercised by validateDevicePluginPatterns instead.
	for _, c := range pod.Spec.Containers {
		if c.Resources.Limits != nil {
			if _, hasGPU := c.Resources.Limits[corev1.ResourceName(resourceNVIDIAGPU)]; hasGPU {
				report.GPULimitsCount++
				return nil, errors.New(errors.ErrCodeInternal,
					"DRA test pod unexpectedly requests nvidia.com/gpu in limits — must allocate via its ResourceClaim only")
			}
		}
	}

	// 4. No hostPath volumes to /dev/nvidia*.
	for _, vol := range pod.Spec.Volumes {
		if vol.HostPath != nil && strings.Contains(vol.HostPath.Path, "/dev/nvidia") {
			report.HostPathGPUMounts++
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("pod has hostPath volume to %s", vol.HostPath.Path))
		}
	}

	// 5. ResourceClaim exists. classifyK8sReadError keeps the code honest:
	// only a true NotFound is ErrCodeNotFound, timeouts are ErrCodeTimeout,
	// RBAC/transient apiserver errors stay ErrCodeInternal — none may
	// masquerade as "claim missing".
	claim, err := dynClient.Resource(draGVRAt(version, "resourceclaims")).Namespace(run.namespace).Get(
		ctx, run.claimName, metav1.GetOptions{})
	if err != nil {
		return nil, classifyK8sReadError(err, fmt.Sprintf("ResourceClaim %s", run.claimName))
	}
	report.ClaimState, _, _ = unstructured.NestedString(claim.Object, "status", "state")
	results, found, _ := unstructured.NestedSlice(claim.Object, "status", "allocation", "devices", "results")
	if found {
		report.ClaimAllocationInfo = fmt.Sprintf("%d allocated device result(s)", len(results))
	} else {
		report.ClaimAllocationInfo = "no allocation results reported"
	}

	// 6. Pod completed successfully (proves DRA allocation worked) AND the
	// unauthorized sibling container confirmed no accelerator access.
	code, err := verifyCompletionAndContainerIsolation(pod, "DRA")
	report.UnauthorizedExitCode = code
	if err != nil {
		return nil, err
	}

	return report, nil
}

// verifyNoAllocationIsolation verifies that a pod WITHOUT any GPU allocation
// (no DRA ResourceClaims and no nvidia.com/gpu limits) cannot see GPU
// devices. This proves GPU access is truly mediated by the allocation
// mechanism under test (DRA or device plugin) — devices are not exposed to
// pods that request nothing. gpuNodeName pins the pod to the same GPU node
// where the allocation test ran, ensuring isolation is proven on a node that
// actually has GPUs and bypassing scheduler-level delays.
//
// Takes the full validators.Context so it can record the probe pod's logs as
// an artifact BEFORE evaluating pass/fail: a failing probe (isolation broken,
// unexpected exit code) must still ship its logs — recording them only on the
// success path would omit the primary diagnostic exactly when the check fails.
func verifyNoAllocationIsolation(vctx *validators.Context, run *gpuTestRun, gpuNodeName string) (*isolationProbeReport, error) {
	ctx := vctx.Ctx
	clientset := vctx.Clientset
	// No local cleanup here: the caller's deferred cleanupGPUTestNamespace is
	// the single cleanup owner — deleting the per-run namespace reconciles
	// this probe pod too, including after an ambiguous create (persisted
	// despite an error return).

	// Create the no-allocation probe pod pinned to the GPU node. Usually the
	// namespace's default SA exists by now (the main test pod already ran),
	// but keep the SA-race guard uniform across all direct pod Creates.
	pod := buildNoAllocationProbePod(run, gpuNodeName)
	if err := createPodWhenSAReady(ctx, clientset, run.namespace, pod); err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to create no-allocation probe pod")
	}

	// Wait for no-claim pod to reach terminal state (watch-based; node name
	// in the description so a timeout is attributable to the pinned node).
	resultPod, err := waitForTerminalPod(ctx, clientset, run.namespace, run.noAllocPodName,
		fmt.Sprintf("no-allocation probe pod (node=%s)", gpuNodeName))
	if err != nil {
		return nil, err
	}

	report := &isolationProbeReport{
		PodName:        resultPod.Name,
		PodPhase:       resultPod.Status.Phase,
		ExitCode:       podExitCode(resultPod),
		ResourceClaims: len(resultPod.Spec.ResourceClaims),
	}

	// Fetch and record the probe's logs FIRST — before any pass/fail verdict —
	// so a failing probe still ships its diagnostics.
	logBytes, logErr := clientset.CoreV1().Pods(run.namespace).GetLogs(
		run.noAllocPodName, &corev1.PodLogOptions{}).DoRaw(ctx)
	if logErr != nil {
		report.Logs = fmt.Sprintf("failed to read logs: %v", logErr)
	} else {
		report.Logs = string(logBytes)
	}
	recordRawTextArtifact(vctx, "No-allocation probe pod logs",
		fmt.Sprintf("kubectl logs %s -n %s", run.noAllocPodName, run.namespace), report.Logs)

	// Strict success criteria: require Succeeded (exit 0 = script confirmed no GPU visible).
	// Failed means either GPU was visible (exit 1) or the container failed for other reasons.
	if resultPod.Status.Phase != corev1.PodSucceeded {
		exitCode := report.ExitCode
		if exitCode == 1 {
			return nil, errors.New(errors.ErrCodeInternal,
				"GPU devices visible without any GPU allocation — isolation broken (container exit code 1)")
		}
		return nil, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("no-allocation probe pod failed with exit code %d — cannot verify isolation",
				exitCode))
	}
	if len(resultPod.Status.ContainerStatuses) > 0 {
		cs := resultPod.Status.ContainerStatuses[0]
		if cs.State.Terminated != nil {
			report.ExitCode = cs.State.Terminated.ExitCode
		}
	}

	// Verify no hostPath to GPU devices on the no-claim pod.
	for _, vol := range resultPod.Spec.Volumes {
		if vol.HostPath != nil && strings.Contains(vol.HostPath.Path, "/dev/nvidia") {
			report.HostPathGPUMounts++
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("no-allocation probe pod has hostPath volume to %s — isolation broken",
					vol.HostPath.Path))
		}
	}
	report.HostPathGPUMounts = 0

	return report, nil
}

// saProvisionTimeout bounds the retry window for pod creation racing the
// ServiceAccount controller in a freshly created per-run namespace (a package
// variable so tests can shorten it, mirroring cleanupTimeout).
var saProvisionTimeout = 15 * time.Second

// createPodWhenSAReady creates the pod, retrying while the ServiceAccount
// admission controller rejects it because the fresh namespace's `default`
// ServiceAccount has not been provisioned yet. The per-run namespace is
// created immediately before the first pod Create; the SA controller
// populates `default` asynchronously, and unlike controller-managed workloads
// a DIRECT pod Create is not retried by anything — without this, the check
// false-fails with "serviceaccount \"default\" not found" on that race.
// Only that specific admission rejection is retried (bounded by
// saProvisionTimeout); every other error propagates immediately.
func createPodWhenSAReady(ctx context.Context, clientset kubernetes.Interface, namespace string, pod *corev1.Pod) error {
	waitCtx, cancel := context.WithTimeout(ctx, saProvisionTimeout)
	defer cancel()
	// Retry interval: PodPollInterval in production, scaled down when the
	// window itself is short (tests shrink saProvisionTimeout; an interval
	// larger than the window would allow zero retries).
	retryInterval := defaults.PodPollInterval
	if scaled := saProvisionTimeout / 10; scaled < retryInterval {
		retryInterval = scaled
	}
	saTimeoutErr := func(cause error) error {
		return errors.Wrap(errors.ErrCodeTimeout, fmt.Sprintf(
			"namespace %s default ServiceAccount was not provisioned within %s — pod creation kept failing SA admission", namespace, saProvisionTimeout), cause)
	}
	var lastErr error
	for {
		// waitCtx (not the parent ctx) bounds the Create so an in-flight API
		// call cannot outlive the SA-provisioning retry window.
		_, err := clientset.CoreV1().Pods(namespace).Create(waitCtx, pod, metav1.CreateOptions{})
		if err == nil {
			return nil
		}
		if !isMissingDefaultSAError(err) {
			if waitCtx.Err() != nil {
				// The retry window's own deadline cut the Create off. With at
				// least one observed SA admission rejection this IS the
				// SA-provisioning timeout; without one (apiserver stall,
				// parent cancellation) claiming an SA race would be
				// fabricated diagnosis — report a generic bounded-create
				// timeout wrapping the actual error instead.
				if lastErr != nil {
					return saTimeoutErr(lastErr)
				}
				return errors.Wrap(errors.ErrCodeTimeout, fmt.Sprintf(
					"pod create in namespace %s timed out waiting on namespace provisioning", namespace), err)
			}
			return err
		}
		lastErr = err
		select {
		case <-waitCtx.Done():
			return saTimeoutErr(lastErr)
		case <-time.After(retryInterval):
		}
	}
}

// isMissingDefaultSAError reports the ServiceAccount admission controller's
// missing-ServiceAccount rejection for a fresh namespace's DEFAULT
// ServiceAccount specifically (admission message shape: `error looking up
// service account <ns>/default: serviceaccount "default" not found`). A
// missing CUSTOM ServiceAccount is a configuration error, not the namespace
// provisioning race — it must NOT be retried.
func isMissingDefaultSAError(err error) bool {
	return k8serrors.IsForbidden(err) &&
		strings.Contains(err.Error(), `serviceaccount "default" not found`)
}

// cleanupTimeout bounds the ENTIRE per-run cleanup — one namespace Delete
// plus the wait for the namespace to disappear (a package variable so tests
// can shorten it). A single shared budget keeps worst-case deferred cleanup
// at one K8sCleanupTimeout regardless of how many test resources a path
// created, so cleanup cannot stack per-resource budgets past the validator
// Job's activeDeadlineSeconds and replace the real result with a generic Job
// timeout.
var cleanupTimeout = defaults.K8sCleanupTimeout

// cleanupGPUTestNamespace reconciles the per-run test namespace to absence.
// The Delete is issued as early as possible so the namespace enters
// Terminating quickly; it then re-Gets (re-issuing the best-effort Delete
// each poll) until the namespace is confirmed gone or the budget expires.
//
// Ownership safety (see gpuTestRun):
//   - a run marked nsForeign never deletes anything — the name is occupied by
//     a namespace this run does not own; a run marked nsNeverCreated has
//     nothing to delete — the Create was definitively rejected;
//   - every Delete carries a metav1.Preconditions{UID} guard (when the UID is
//     known), so a recreated or foreign namespace at the same name can never
//     be deleted. A 409 Conflict from the Delete is NOT taken as proof of
//     absence — admission webhooks and the storage layer can also return 409
//     while our namespace still exists — the follow-up Get decides: only
//     NotFound or a different UID confirms this run's namespace is gone;
//   - when the namespace was never positively observed (ambiguous Create:
//     error returned, object possibly committed), a first NotFound is NOT
//     confirmed absence — cleanup keeps re-Getting within the budget, adopts
//     the namespace if it surfaces with our ownership token, and otherwise
//     reports that it may surface later carrying gpuTestRunLabel for
//     external sweeping.
//
// Deleting the namespace reconciles every test resource inside it (GPU test
// pod, ResourceClaim, no-allocation probe pod) in one bounded operation, and
// it also closes the ambiguous-create window: once the namespace is
// Terminating, the NamespaceLifecycle admission plugin rejects subsequent
// Creates in it, and the namespace deletion controller repeatedly deletes
// remaining content before finalizing — so even an already-admitted
// in-flight Create that commits late is garbage-collected durably
// (server-side, not an atomic rejection guarantee).
//
// Runs on its own context, safe from a deferred block whose parent context
// is already canceled. Best-effort in the sense that it must not replace a
// primary test failure — but it is NOT silent: on budget exhaustion the
// terminal artifact distinguishes deletion-in-progress (accepted or observed
// Terminating; the namespace controller finishes server-side, err nil) from
// CLEANUP FAILED (never accepted, never Terminating — e.g. persistent
// Forbidden), which additionally returns a non-nil error so callers can fail
// an otherwise-passing check instead of reporting PASS while leaking the
// namespace, test pod, and allocated GPU claim. DRA ResourceClaim finalizers
// can make namespace termination outlast the validator Job; the namespace
// controller keeps working after the Job exits. The returned status string
// is recorded as cleanup evidence.
func cleanupGPUTestNamespace(clientset kubernetes.Interface, run *gpuTestRun) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	if run.nsForeign {
		return fmt.Sprintf(
			"namespace %s is owned by another party (missing %s=%s ownership label) — deliberately left untouched, nothing was created in it",
			run.namespace, gpuTestRunLabel, run.token), nil
	}
	if run.nsNeverCreated {
		return fmt.Sprintf(
			"namespace %s was never created — the create was definitively rejected by the apiserver, nothing to clean up",
			run.namespace), nil
	}

	var (
		deleteAccepted      bool  // a Delete returned nil or NotFound at least once
		observedTerminating bool  // a Get observed a non-nil deletionTimestamp
		lastErr             error // last unexpected Delete/Get error
	)
	// Deliberately a GET/DELETE reconcile loop rather than a watch (repo rule
	// exception, justified): each iteration is a multi-condition state machine
	// — resolve ambiguous-create identity, verify ownership, RE-ISSUE the
	// best-effort UID-preconditioned Delete, and interpret 409s — not a
	// passive wait for one condition. It is tightly bounded by cleanupTimeout
	// (~30s, a few dozen iterations worst case), and it must keep WRITING
	// (the Delete) per pass, which a watch cannot express.
	for {
		// Resolve identity first when the namespace was never positively
		// observed: an ambiguously committed Create may still surface, so a
		// NotFound here must not short-circuit as confirmed absence.
		if !run.nsObserved {
			existing, getErr := clientset.CoreV1().Namespaces().Get(ctx, run.namespace, metav1.GetOptions{})
			switch {
			case getErr == nil:
				if existing.Labels[gpuTestRunLabel] != run.token {
					return fmt.Sprintf(
						"namespace %s exists but is owned by another party (missing %s=%s ownership label) — deliberately left untouched, nothing was created in it",
						run.namespace, gpuTestRunLabel, run.token), nil
				}
				run.nsObserved = true
				run.nsUID = existing.UID
			case k8serrors.IsNotFound(getErr):
				// Not visible (yet) — keep settling within the budget.
			default:
				lastErr = getErr
			}
		}

		if run.nsObserved {
			// Delete only the exact object this run observed: the UID
			// precondition makes deleting a recreated or foreign namespace
			// impossible.
			opts := metav1.DeleteOptions{}
			if run.nsUID != "" {
				uid := run.nsUID
				opts.Preconditions = &metav1.Preconditions{UID: &uid}
			}
			delErr := clientset.CoreV1().Namespaces().Delete(ctx, run.namespace, opts)
			switch {
			case delErr == nil, k8serrors.IsNotFound(delErr):
				deleteAccepted = true
			default:
				// Includes 409 Conflict: a UID-precondition mismatch does
				// surface as 409, but admission webhooks and the storage
				// layer can also return 409 while OUR namespace still
				// exists — never treat the 409 itself as confirmed absence.
				// The follow-up Get decides: only NotFound or a different
				// UID proves this run's namespace is gone.
				lastErr = delErr
			}

			existing, getErr := clientset.CoreV1().Namespaces().Get(ctx, run.namespace, metav1.GetOptions{})
			switch {
			case k8serrors.IsNotFound(getErr):
				return fmt.Sprintf(
					"namespace %s deleted — confirmed absent (all test resources removed with it)", run.namespace), nil
			case getErr == nil:
				if run.nsUID != "" && existing.UID != run.nsUID {
					return fmt.Sprintf(
						"namespace %s (uid %s) deleted — confirmed absent (name since reused by a different namespace, left untouched)",
						run.namespace, run.nsUID), nil
				}
				if existing.DeletionTimestamp != nil {
					observedTerminating = true
				}
			default:
				lastErr = getErr
			}
		}

		select {
		case <-ctx.Done():
			return cleanupDeadlineStatus(run, deleteAccepted, observedTerminating, lastErr)
		case <-time.After(defaults.PodPollInterval):
		}
	}
}

// cleanupDeadlineStatus classifies the terminal cleanup artifact when the
// budget expires without confirmed absence:
//   - deletion in progress (err nil): a Delete was accepted (nil/NotFound) or
//     a Get observed Terminating — the namespace controller finishes
//     server-side;
//   - never observed (err nil): the ambiguous Create never surfaced within
//     the budget — it may still commit later, and it would carry the
//     ownership label for external sweeping (the primary error from the
//     failed Create is already the check's result);
//   - CLEANUP FAILED (err non-nil): no Delete was ever accepted and the
//     namespace was never Terminating (e.g. persistent Forbidden or transport
//     failure) — surfaced with the last error so a passing check cannot
//     report PASS while leaking its namespace, test pod, and GPU claim.
func cleanupDeadlineStatus(run *gpuTestRun, deleteAccepted, observedTerminating bool, lastErr error) (string, error) {
	switch {
	case deleteAccepted || observedTerminating:
		return fmt.Sprintf(
			"WARNING: namespace %s deletion accepted but not confirmed within the %s cleanup budget — deletion in progress, continues server-side (the namespace controller finishes the garbage collection)",
			run.namespace, cleanupTimeout), nil
	case !run.nsObserved:
		msg := fmt.Sprintf(
			"WARNING: namespace %s was never observed within the %s cleanup budget after an ambiguous create — it may still surface later; if it does, it carries the ownership label %s=%s for external sweeping",
			run.namespace, cleanupTimeout, gpuTestRunLabel, run.token)
		if lastErr != nil {
			msg += fmt.Sprintf(" (last error: %v)", lastErr)
		}
		return msg, nil
	default:
		msg := fmt.Sprintf(
			"CLEANUP FAILED: namespace %s still exists and its deletion was never accepted within the %s cleanup budget — test resources may be leaked (last error: %v)",
			run.namespace, cleanupTimeout, lastErr)
		if lastErr != nil {
			return msg, errors.Wrap(errors.ErrCodeInternal, msg, lastErr)
		}
		return msg, errors.New(errors.ErrCodeInternal, msg)
	}
}

// buildDRATestPod returns the Pod spec for the DRA GPU allocation test: the
// granted container allocates one GPU via the pod's ResourceClaim and must
// observe exactly one usable GPU (gpuExclusiveGrantProbeScript), while an
// identical unauthorized sibling container is granted nothing and must not
// see any accelerator (container-level isolation subtest).
// tolerations, when non-nil, replace the default tolerate-all policy.
func buildDRATestPod(run *gpuTestRun, tolerations []corev1.Toleration) *corev1.Pod {
	if tolerations == nil {
		tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      run.podName,
			Namespace: run.namespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations:   tolerations,
			ResourceClaims: []corev1.PodResourceClaim{
				{
					Name:              gpuClaimName,
					ResourceClaimName: helper.StrPtr(run.claimName),
				},
			},
			Containers: []corev1.Container{
				{
					Name:    containerNameGPUTest,
					Image:   cudaTestImage,
					Command: []string{bashShell, "-c", gpuExclusiveGrantProbeScript},
					Resources: corev1.ResourceRequirements{
						Claims: []corev1.ResourceClaim{
							{Name: gpuClaimName},
						},
					},
				},
				unauthorizedSiblingContainer(),
			},
		},
	}
}

// unauthorizedSiblingContainer returns a container identical to the granted
// GPU test container except that it is granted nothing — no ResourceClaim
// reference and no nvidia.com/gpu limits. Per the upstream multi-container
// isolation subtest (kubernetes-sigs/ai-conformance#75), it must not see any
// accelerator: its probe exits non-zero when a GPU device is visible.
func unauthorizedSiblingContainer() corev1.Container {
	return corev1.Container{
		Name:    containerNameUnauthorized,
		Image:   cudaTestImage,
		Command: []string{bashShell, "-c", gpuAbsenceProbeScript},
	}
}

// containerExitCode returns the terminated exit code of the named container,
// or ok=false when the container has no terminated status.
func containerExitCode(pod *corev1.Pod, name string) (int32, bool) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != name {
			continue
		}
		if cs.State.Terminated == nil {
			return 0, false
		}
		return cs.State.Terminated.ExitCode, true
	}
	return 0, false
}

// verifyCompletionAndContainerIsolation verifies that the multi-container
// GPU test pod completed successfully AND that its unauthorized sibling
// container confirmed no accelerator access (container-level isolation,
// kubernetes-sigs/ai-conformance#75). A non-zero exit from the unauthorized
// container is reported as an isolation break (it also drives the pod to
// Failed, so it is checked first for correct attribution); a missing
// terminated status on a Succeeded pod fails closed so the isolation subtest
// can never silently not run. mechanism labels the failure messages
// ("DRA" or "device plugin").
func verifyCompletionAndContainerIsolation(pod *corev1.Pod, mechanism string) (int32, error) {
	code, isoObserved := containerExitCode(pod, containerNameUnauthorized)
	if isoObserved && code != 0 {
		return code, errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"GPU devices visible in unauthorized container %s (exit code %d) — container-level isolation broken",
			containerNameUnauthorized, code))
	}
	if pod.Status.Phase != corev1.PodSucceeded {
		return code, errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"%s test pod phase=%s (want Succeeded), GPU allocation may have failed",
			mechanism, pod.Status.Phase))
	}
	if !isoObserved {
		return code, errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"unauthorized container %s has no terminated status — cannot verify container-level isolation",
			containerNameUnauthorized))
	}
	return code, nil
}

// recordGPUPodContainerLogs records per-container logs for the
// (multi-container) GPU test pod.
func recordGPUPodContainerLogs(ctx *validators.Context, pod *corev1.Pod, label string) {
	for _, c := range pod.Spec.Containers {
		artifactLabel := fmt.Sprintf("%s (%s)", label, c.Name)
		equivalent := fmt.Sprintf("kubectl logs %s -n %s -c %s", pod.Name, pod.Namespace, c.Name)
		logBytes, logErr := ctx.Clientset.CoreV1().Pods(pod.Namespace).GetLogs(
			pod.Name, &corev1.PodLogOptions{Container: c.Name}).DoRaw(ctx.Ctx)
		if logErr != nil {
			recordRawTextArtifact(ctx, artifactLabel, equivalent,
				fmt.Sprintf("failed to read logs: %v", logErr))
			continue
		}
		recordRawTextArtifact(ctx, artifactLabel, equivalent, string(logBytes))
	}
}

// buildNoAllocationProbePod returns a Pod spec WITHOUT any GPU allocation —
// no ResourceClaims and no nvidia.com/gpu limits. If the cluster properly
// mediates GPU access (via DRA or the device plugin), this pod will not see
// GPU devices. Uses a lightweight image (busybox) since no CUDA libraries are
// needed — only checking whether /dev/nvidia* device files are visible.
// gpuNodeName pins the pod to the GPU node via NodeName, bypassing the scheduler to ensure
// the isolation test runs on a node that actually has GPUs and avoiding scheduler delays.
func buildNoAllocationProbePod(run *gpuTestRun, gpuNodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      run.noAllocPodName,
			Namespace: run.namespace,
		},
		Spec: corev1.PodSpec{
			NodeName:      gpuNodeName,
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations: []corev1.Toleration{
				{Operator: corev1.TolerationOpExists},
			},
			Containers: []corev1.Container{
				{
					Name:    "isolation-test",
					Image:   defaults.ProbeImage,
					Command: []string{"sh", "-c", gpuAbsenceProbeScript},
				},
			},
		},
	}
}

// buildResourceClaim returns the unstructured ResourceClaim for the DRA test,
// shaped for the served resource.k8s.io API version. v1 and v1beta2 wrap the
// request detail in DeviceRequest.exactly (ExactDeviceRequest); v1beta1 has
// no `exactly` wrapper — deviceClassName/allocationMode/count are direct
// fields on the request (see k8s.io/api/resource/v1beta1.DeviceRequest).
func buildResourceClaim(run *gpuTestRun, version string) *unstructured.Unstructured {
	request := map[string]interface{}{
		keyName: gpuClaimName,
	}
	if version == versionV1beta1 {
		request["deviceClassName"] = draDriverGPU
		request["allocationMode"] = "ExactCount"
		request["count"] = int64(1)
	} else {
		request["exactly"] = map[string]interface{}{
			"deviceClassName": draDriverGPU,
			"allocationMode":  "ExactCount",
			"count":           int64(1),
		}
	}
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			keyAPIVersion: apiGroupResourceK8sIO + "/" + version,
			keyKind:       "ResourceClaim",
			keyMetadata: map[string]interface{}{
				keyName:      run.claimName,
				keyNamespace: run.namespace,
			},
			keySpec: map[string]interface{}{
				"devices": map[string]interface{}{
					"requests": []interface{}{request},
				},
			},
		},
	}
}

func podExitCode(pod *corev1.Pod) int32 {
	if len(pod.Status.ContainerStatuses) == 0 {
		return -1
	}
	terminated := pod.Status.ContainerStatuses[0].State.Terminated
	if terminated == nil {
		return -1
	}
	return terminated.ExitCode
}
