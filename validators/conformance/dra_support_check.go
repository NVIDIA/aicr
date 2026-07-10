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
	stderrors "errors"
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
)

// draProbeTimeout bounds the standalone live-driver presence probe (three
// read-only API calls in nvidiaDRADriverInstalled). The probe runs before the
// work-budget context is created — deliberately, so skip semantics survive
// short deadlines — which means a deadline-less library caller would
// otherwise hand it an unbounded context. A package variable (not a const) so
// tests can shorten it, mirroring cleanupTimeout.
var draProbeTimeout = defaults.CollectorK8sTimeout

// CheckDRASupport validates CNCF requirement #2: DRA Support.
// Verifies DRA driver controller deployment, kubelet plugin DaemonSet,
// and that ResourceSlices (at the served resource.k8s.io version) exist
// advertising NVIDIA resources. This check validates DRA itself and never
// falls back to the device plugin.
//
// Scoping comes from the resolved recipe, not from live cluster resources:
// the check is skipped ONLY when the recipe's componentRefs exclude or
// disable the nvidia-dra-driver-gpu component (e.g. OCP). When the recipe
// enables the driver, its absence is an installation FAILURE, not a skip.
// When no componentRefs are available at all (standalone runs against a bare
// validation input), the check falls back to live-driver detection for
// backward compatibility.
func CheckDRASupport(ctx *validators.Context) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// The namespace every driver probe (pods, controller Deployment, kubelet
	// plugin DaemonSet) targets: the enabled componentRef's resolved namespace
	// when the recipe provides one, the default otherwise.
	driverNamespace, namespaceSource := resolveDRADriverNamespace(ctx)

	// 0. Scoping gate: does the recipe include the NVIDIA DRA driver?
	// Deliberately runs BEFORE the work-budget gate below: scoping is a pure
	// recipe check (plus, standalone, a read-only presence probe) that creates
	// nothing needing cleanup, so a recipe that excludes/disables the driver —
	// or a standalone cluster without it — must yield its documented skip even
	// when the deadline is too short for the cleanup reserve.
	// probedPods carries the fallback probe's pod list forward so step 2 does
	// not repeat the identical List call.
	var probedPods *corev1.PodList
	if len(ctx.ValidationInput.GetComponentRefs()) == 0 {
		pods, probeErr := probeStandaloneDriverScope(ctx, driverNamespace)
		if probeErr != nil {
			return probeErr
		}
		probedPods = pods
	} else if !recipeHasComponent(ctx, draDriverComponentName) {
		return validators.Skip(fmt.Sprintf(
			"recipe excludes or disables the %s component — DRA is out of scope for this recipe",
			draDriverComponentName))
	}

	// DRA is in scope. Bound all REMAINING work to the check-local budget so
	// one bounded namespace cleanup plus scheduling-skew/result-flush margin
	// always fit inside the check's deadline — see gpuCheckWorkBudget in
	// consts.go. Fail fast (no resources created yet) when the deadline cannot
	// fit the cleanup reserve. The deferred cleanup runs on its own background
	// context and is NOT bounded by this.
	budget, err := gpuCheckWorkBudget(ctx.Ctx, draSupportWorkBudget)
	if err != nil {
		return err
	}
	workCtx, cancelWork := ctx.Timeout(budget)
	defer cancelWork()
	work := *ctx
	work.Ctx = workCtx
	ctx = &work

	recordRawTextArtifact(ctx, "DRA driver namespace", "",
		fmt.Sprintf("Namespace: %s\nSource:    %s", driverNamespace, namespaceSource))

	// 1. DRA in scope → a served version of the resource.k8s.io API group
	// must be present, discovered in preference order v1, v1beta2, v1beta1
	// (see draAPIVersionPreference). An expected driver on a cluster serving
	// NO version is a broken configuration — fail, do not skip.
	servedVersion, resources, err := discoverServedDRAAPIVersion(ctx.Ctx, ctx.Clientset)
	if err != nil {
		return err
	}
	if servedVersion == "" {
		return errors.New(errors.ErrCodeUnavailable, fmt.Sprintf(
			"NVIDIA DRA driver is in scope but no served version of the %s API group was discovered (tried %s; requires K8s 1.32+)",
			apiGroupResourceK8sIO, strings.Join(draAPIVersionPreference, ", ")))
	}
	var apiResources strings.Builder
	fmt.Fprintf(&apiResources, "Served version: %s/%s\n", apiGroupResourceK8sIO, servedVersion)
	for _, r := range resources.APIResources {
		fmt.Fprintf(&apiResources, "%-26s %-22s namespaced=%t\n", r.Name, r.Kind, r.Namespaced)
	}
	recordRawTextArtifact(ctx, "DRA API resources",
		"kubectl api-resources --api-group=resource.k8s.io", apiResources.String())

	// 2. DRA driver pods inventory. Reuse the fallback probe's list when it
	// already fetched one (avoids an identical List round-trip).
	pods := probedPods
	if pods == nil {
		pods, err = ctx.Clientset.CoreV1().Pods(driverNamespace).List(ctx.Ctx, metav1.ListOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to list DRA driver pods", err)
		}
	}
	var driverPods strings.Builder
	for _, pod := range pods.Items {
		fmt.Fprintf(&driverPods, "%-48s ready=%s phase=%s node=%s\n",
			pod.Name, podReadyCount(pod), pod.Status.Phase, pod.Spec.NodeName)
	}
	recordRawTextArtifact(ctx, "DRA driver pods",
		fmt.Sprintf("kubectl get pods -n %s -o wide", driverNamespace), driverPods.String())

	// 3. DRA driver controller Deployment available.
	deploy, deployErr := getDeploymentIfAvailable(ctx, driverNamespace, draControllerDeployment)
	if deployErr != nil {
		// The helper returns structured errors with the right code (e.g.
		// ErrCodeInternal for deployed-but-unavailable) — propagate as-is
		// rather than reclassifying everything as NotFound.
		return deployErr
	}
	if deploy != nil {
		expected := int32(1)
		if deploy.Spec.Replicas != nil {
			expected = *deploy.Spec.Replicas
		}
		recordRawTextArtifact(ctx, "DRA Controller Deployment", "",
			fmt.Sprintf("Name:      %s/%s\nReplicas:  %d/%d available\nImage:     %s",
				deploy.Namespace, deploy.Name,
				deploy.Status.AvailableReplicas, expected,
				firstContainerImage(deploy.Spec.Template.Spec.Containers)))
	}

	// 4. DRA kubelet plugin DaemonSet ready.
	ds, dsErr := getDaemonSetIfReady(ctx, driverNamespace, draKubeletPluginDaemonSet)
	if dsErr != nil {
		// Structured error from the helper — propagate without reclassifying.
		return dsErr
	}
	if ds != nil {
		recordRawTextArtifact(ctx, "DRA Kubelet Plugin DaemonSet", "",
			fmt.Sprintf("Name:      %s/%s\nReady:     %d/%d pods\nImage:     %s",
				ds.Namespace, ds.Name,
				ds.Status.NumberReady, ds.Status.DesiredNumberScheduled,
				firstContainerImage(ds.Spec.Template.Spec.Containers)))
	}

	// 5. ResourceSlices from NVIDIA DRA drivers must exist AND pass robust
	// validation. The supported NVIDIA DRA driver configuration is
	// ComputeDomain-only, so validated compute-domain slices are sufficient —
	// gpu.nvidia.com slices are NOT required (#1327).
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}
	if err = validateNVIDIAResourceSlices(ctx, dynClient, servedVersion); err != nil {
		return err
	}

	// 6. Behavioral GPU allocation subtest — only applicable when full-GPU
	// DRA (gpu.nvidia.com) is usable. The probe and the behavioral test run
	// at the served resource.k8s.io version (v1, v1beta2, or v1beta1), so
	// beta-only clusters (K8s 1.32/1.33) exercise it too. In the supported
	// ComputeDomain-only configuration there is no gpu.nvidia.com DeviceClass
	// to allocate from; record the subtest as not applicable within a passing
	// check — allowed only because the robust ResourceSlice validation above
	// already passed.
	// TODO(#1649): behaviorally exercise the MNNVL ComputeDomain →
	// ResourceClaimTemplate → IMEX channel flow instead of recording N/A for
	// ComputeDomain-only configurations.
	mode, err := detectGPUAllocationMode(ctx.Ctx, ctx.Clientset, dynClient)
	if err != nil {
		return err
	}
	// Policy gate (#1327): configuration selects the mechanism the subtest
	// may exercise. Verify runs for ANY configured (non-unspecified) policy
	// and fails closed on mismatch — including the reserved and unknown
	// policies, which allocmode.Verify rejects outright (Verify is a no-op
	// for unspecified). Under dra-resource-claim the recipe PROMISES usable
	// full-GPU DRA, so its absence is a FAILURE rather than the
	// not-applicable skip below. Under device-plugin-extended-resource the
	// full-GPU subtest is N/A even when DRA is incidentally usable (the
	// dual-advertised transition state) — the subtest must not exercise the
	// non-configured mechanism. Unspecified keeps today's capability-driven
	// dispatch.
	policy := ctx.ValidationInput.GetGPUAllocationPolicy()
	// Evidence BEFORE the verdict: record the configured policy and the
	// inspected mode so a Verify failure below is diagnosable from the
	// artifacts alone (evidence parity with secure-accelerator-access).
	recordRawTextArtifact(ctx, "GPU allocation policy", "",
		fmt.Sprintf("configured policy: %s", policy))
	recordRawTextArtifact(ctx, "GPU allocation mode", "",
		mode.Summary())
	if verifyErr := verifyGPUAllocationPolicy(policy, mode); verifyErr != nil {
		return verifyErr
	}
	if policy == validatorv1.GPUAllocationPolicyDevicePluginExtendedResource {
		recordRawTextArtifact(ctx, "Behavioral GPU allocation", "",
			"skipped (not applicable): configured GPU allocation policy is "+policy+
				" — whole GPUs are device-plugin-allocated, so the full-GPU DRA behavioral subtest does not apply; "+
				"DRA validated via driver health and validated ResourceSlices. "+mode.DRADetail)
		return nil
	}
	if !mode.DRAUsable {
		recordRawTextArtifact(ctx, "Behavioral GPU allocation", "",
			"skipped (not applicable): full-GPU DRA not enabled; behavioral allocation not applicable — "+
				"DRA validated via driver health and validated ResourceSlices. "+mode.DRADetail)
		return nil
	}
	return validateDRAAllocation(ctx, dynClient, mode.APIVersion)
}

// validateNVIDIAResourceSlices verifies that at least one NVIDIA DRA driver
// (*.nvidia.com, e.g. compute-domain.nvidia.com or gpu.nvidia.com) publishes
// ResourceSlices that pass robust validation: a slice only counts when it
// belongs to a complete, current-generation pool and advertises at least one
// untainted device that resolves to a Ready, schedulable node (see
// usableDriverSliceNodes). Slices that merely exist do NOT count.
//
// version is the served resource.k8s.io version discovered by
// discoverServedDRAAPIVersion; slices are listed via the dynamic client at
// that group-version. The inventory and validation operate on unstructured
// objects with version-agnostic field paths, and the fields validated here —
// spec.driver, spec.pool.{name,generation,resourceSliceCount}, spec.devices
// (per-device name), and node topology (spec.nodeName, nodeSelector,
// allNodes, perDeviceNodeSelection) — are structurally compatible across
// v1beta1, v1beta2, and v1 (the version bumps reshaped device
// attribute/capacity detail, which this validation does not read), so the
// same logic applies to whichever served version was discovered. Device
// taints and per-device topology are top-level device fields in v1beta2/v1
// but nested under the v1beta1 `basic` wrapper — deviceFields (in
// validators/internal/allocmode) normalizes the wrapper so v1beta1 device
// taints are honored too.
func validateNVIDIAResourceSlices(ctx *validators.Context, dynClient dynamic.Interface, version string) error {
	sliceList, err := dynClient.Resource(draGVRAt(version, "resourceslices")).List(ctx.Ctx, metav1.ListOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to list ResourceSlices", err)
	}
	nodeList, err := ctx.Clientset.CoreV1().Nodes().List(ctx.Ctx, metav1.ListOptions{})
	if err != nil {
		return classifyK8sReadError(err, "nodes for ResourceSlice validation")
	}
	eligible := eligibleReadySchedulableNodes(nodeList)

	var sliceSummary strings.Builder
	fmt.Fprintf(&sliceSummary, "Total ResourceSlices: %d\n", len(sliceList.Items))
	nvidiaDrivers := make(map[string]struct{})
	for _, item := range sliceList.Items {
		if ctxErr := ctx.Ctx.Err(); ctxErr != nil {
			return errors.Wrap(errors.ErrCodeTimeout,
				"ResourceSlice inventory scan canceled", ctxErr)
		}
		driver, _, _ := unstructured.NestedString(item.Object, "spec", "driver")
		nodeName, _, _ := unstructured.NestedString(item.Object, "spec", "nodeName")
		poolName, _, _ := unstructured.NestedString(item.Object, "spec", "pool", "name")
		if strings.HasSuffix(driver, nvidiaDriverSuffix) {
			nvidiaDrivers[driver] = struct{}{}
		}
		fmt.Fprintf(&sliceSummary, "%-48s node=%s driver=%s pool=%s\n",
			item.GetName(), nodeName, driver, poolName)
	}

	if len(nvidiaDrivers) == 0 {
		recordRawTextArtifact(ctx, "ResourceSlices", "kubectl get resourceslices", sliceSummary.String())
		return errors.New(errors.ErrCodeNotFound, fmt.Sprintf(
			"no ResourceSlices from NVIDIA DRA drivers (e.g. %s, %s) found — NVIDIA resources not advertised",
			draDriverComputeDomain, draDriverGPU))
	}

	// Per-driver robust validation: complete current-generation pools,
	// untainted devices, Ready schedulable nodes.
	usableTotal := 0
	for _, driver := range sortedNodeNames(nvidiaDrivers) {
		usable, seen, sliceErr := usableDriverSliceNodes(ctx.Ctx, sliceList.Items, driver, eligible)
		if sliceErr != nil {
			return sliceErr
		}
		usableTotal += len(usable)
		fmt.Fprintf(&sliceSummary,
			"NVIDIA driver %s: %d slice(s), usable device(s) on %d Ready, schedulable node(s) [%s]\n",
			driver, seen, len(usable), strings.Join(sortedNodeNames(usable), ","))
	}
	recordRawTextArtifact(ctx, "ResourceSlices", "kubectl get resourceslices", sliceSummary.String())

	if usableTotal == 0 {
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"NVIDIA DRA driver ResourceSlices (*%s) exist but none passed validation — a slice counts only when it is in a complete, current-generation pool and advertises an untainted device on a Ready, schedulable node",
			nvidiaDriverSuffix))
	}
	return nil
}

// resolveDRADriverNamespace returns the namespace the DRA driver probes
// (pods, controller Deployment, kubelet plugin DaemonSet) must target, plus a
// human-readable source for the evidence artifact. When the resolved recipe
// carries an ENABLED nvidia-dra-driver-gpu componentRef with a non-empty
// resolved Namespace — recipe resolution hydrates ComponentRef.Namespace from
// the registry/overlay/mixin, e.g. recipes/mixins/os-talos.yaml overrides it
// to privileged-nvidia-dra-driver-gpu — that namespace wins, matching how
// pkg/validator/v1 BuildOrchestratorAffinity resolves component namespaces
// from componentRefs. The default draDriverNamespace applies when the ref's
// namespace is empty or when no componentRefs are available at all
// (standalone runs; GetComponentRefs is nil-safe).
func resolveDRADriverNamespace(ctx *validators.Context) (namespace, source string) {
	for _, ref := range ctx.ValidationInput.GetComponentRefs() {
		if ref.Name == draDriverComponentName && ref.IsEnabled() && ref.Namespace != "" {
			return ref.Namespace, fmt.Sprintf(
				"resolved recipe componentRef %s", draDriverComponentName)
		}
	}
	return draDriverNamespace, fmt.Sprintf(
		"default (no resolved namespace on an enabled %s componentRef)", draDriverComponentName)
}

// probeStandaloneDriverScope is the backward-compat scoping fallback for
// standalone runs without a resolved recipe: it probes the live cluster for
// the driver in the given (DEFAULT) namespace. With no componentRefs there is
// nothing to resolve a custom namespace from, and the driver's workload
// labels are not pinned across chart versions, so a cluster-wide label lookup
// would be a guess — the default-namespace assumption is deliberate.
//
// The probe runs BEFORE the caller's work-budget context exists, so it bounds
// its three read-only API calls explicitly (draProbeTimeout): a deadline-less
// library caller must not hang here indefinitely. Probe time still charges
// the caller's main work budget, which derives from the ORIGINAL parent
// deadline, not from this child context.
//
// Returns the driver-namespace pod list for reuse by the caller's inventory
// step. The error is a Skip when the driver is not installed, ErrCodeTimeout
// when the probe ran out of time or was canceled, and the probe's own
// already-structured error otherwise.
func probeStandaloneDriverScope(ctx *validators.Context, driverNamespace string) (*corev1.PodList, error) {
	probeCtx, cancelProbe := ctx.Timeout(draProbeTimeout)
	probe := *ctx
	probe.Ctx = probeCtx
	installed, pods, probeErr := nvidiaDRADriverInstalled(&probe, driverNamespace)
	// Capture the probe context's state BEFORE canceling: cancel forces
	// probeCtx.Err() == context.Canceled unconditionally, which would
	// reclassify EVERY probe failure — immediate RBAC denials, transient
	// API errors — as a (retryable) timeout.
	probeCtxErr := probeCtx.Err()
	cancelProbe()
	if probeErr != nil {
		if probeCtxErr != nil ||
			stderrors.Is(probeErr, context.DeadlineExceeded) ||
			stderrors.Is(probeErr, context.Canceled) {

			return nil, errors.Wrap(errors.ErrCodeTimeout, fmt.Sprintf(
				"DRA driver presence probe did not complete within %s", draProbeTimeout), probeErr)
		}
		return nil, probeErr
	}
	if !installed {
		return nil, validators.Skip(fmt.Sprintf(
			"NVIDIA DRA driver not installed — no pods, %s Deployment, or %s DaemonSet in namespace %s (no recipe componentRefs available; live detection)",
			draControllerDeployment, draKubeletPluginDaemonSet, driverNamespace))
	}
	return pods, nil
}

// nvidiaDRADriverInstalled reports whether the NVIDIA DRA driver is deployed
// in the given namespace: any pod there, the controller Deployment, or the
// kubelet plugin DaemonSet. The second return value is the driver-namespace
// pod list (nil when the list did not succeed) so callers can reuse it for
// inventory without repeating the List call. NotFound means "nothing there"
// for every probe — a missing namespace or resource is exactly the
// not-installed signal, matching the Deployment/DaemonSet probes. All other
// API errors fail closed (returned as errors, not flattened into "not
// installed") so a flaky apiserver cannot turn a real DRA regression into a
// skip.
func nvidiaDRADriverInstalled(ctx *validators.Context, namespace string) (bool, *corev1.PodList, error) {
	pods, err := ctx.Clientset.CoreV1().Pods(namespace).List(ctx.Ctx, metav1.ListOptions{})
	switch {
	case err == nil:
		if len(pods.Items) > 0 {
			return true, pods, nil
		}
	case k8serrors.IsNotFound(err):
		// Defensive: a pod List in a missing namespace normally returns an
		// empty list, but if the apiserver does answer NotFound it
		// deterministically means nothing is there — keep probing. Return an
		// EMPTY, NON-NIL list so a caller that reuses the probe's result does
		// not repeat the List and turn the same NotFound into a hard failure.
		pods = &corev1.PodList{}
	default:
		return false, nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to list pods in namespace %s", namespace), err)
	}

	_, err = ctx.Clientset.AppsV1().Deployments(namespace).Get(
		ctx.Ctx, draControllerDeployment, metav1.GetOptions{})
	if err == nil {
		return true, pods, nil
	}
	if !k8serrors.IsNotFound(err) {
		return false, nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf(
			"failed to get deployment %s/%s", namespace, draControllerDeployment), err)
	}

	_, err = ctx.Clientset.AppsV1().DaemonSets(namespace).Get(
		ctx.Ctx, draKubeletPluginDaemonSet, metav1.GetOptions{})
	if err == nil {
		return true, pods, nil
	}
	if !k8serrors.IsNotFound(err) {
		return false, nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf(
			"failed to get daemonset %s/%s", namespace, draKubeletPluginDaemonSet), err)
	}

	return false, nil, nil
}

// validateDRAAllocation behaviorally exercises full-GPU DRA allocation via a
// ResourceClaim-backed test pod. version is the served resource.k8s.io API
// version (v1, v1beta2, or v1beta1) the ResourceClaim is created and read at.
//
// The return is NAMED so the deferred cleanup can fail an otherwise-passing
// check when cleanup terminally fails — a PASS that leaks the namespace, test
// pod, and allocated GPU claim is not a pass. A primary test error is always
// preserved (never overwritten by the cleanup error).
func validateDRAAllocation(ctx *validators.Context, dynClient dynamic.Interface, version string) (err error) {
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
		recordRawTextArtifact(ctx, "Delete test namespace",
			cleanupInspectEquivalent(run.namespace), status)
		if cleanupErr != nil && err == nil {
			err = cleanupErr
		}
	}()
	if err = deployDRATestResources(ctx.Ctx, ctx.Clientset, dynClient, run, ctx.Tolerations, version); err != nil {
		return err
	}

	// Recorded AFTER the creates succeed so the evidence reports what was
	// actually constructed, not what was merely attempted. The resources are
	// built programmatically via the Kubernetes API at the SERVED
	// resource.k8s.io version with per-run generated names — no static
	// manifest matches them — so the "equivalent" is an inspection command
	// for the created objects.
	// TODO(#1629): emit the full equivalent manifest (Namespace,
	// ResourceClaim, and test Pod at the served version) as an artifact
	// instead of this summary.
	recordRawTextArtifact(ctx, "Created test resources",
		fmt.Sprintf("kubectl get resourceclaims,pods -n %s", run.namespace),
		fmt.Sprintf(
			"Test resources created via the Kubernetes API at the served %s/%s version:\nNamespace:     %s\nResourceClaim: %s\nPod:           %s",
			apiGroupResourceK8sIO, version, run.namespace, run.claimName, run.podName))

	pod, err := waitForGPUTestPod(ctx.Ctx, ctx.Clientset, run)
	if err != nil {
		return err
	}

	// Record the pod's status and container logs FIRST — before the claim
	// read and the pass/fail verdict — so a failing run (claim read error,
	// pod phase != Succeeded) still ships its diagnostics. Logs recorded only
	// on success would be missing exactly when they are most needed.
	podLines := []string{
		fmt.Sprintf("Name:      %s/%s", pod.Namespace, pod.Name),
		fmt.Sprintf("Phase:     %s", pod.Status.Phase),
		fmt.Sprintf("Node:      %s", valueOrUnknown(pod.Spec.NodeName)),
		fmt.Sprintf("PodIP:     %s", valueOrUnknown(pod.Status.PodIP)),
		fmt.Sprintf("Claims:    %d", len(pod.Spec.ResourceClaims)),
	}
	recordRawTextArtifact(ctx, "Pod status",
		fmt.Sprintf("kubectl get pod %s -n %s -o wide", run.podName, run.namespace), strings.Join(podLines, "\n"))
	recordGPUPodContainerLogs(ctx, pod, "Pod logs")

	claimObj, err := dynClient.Resource(draGVRAt(version, "resourceclaims")).Namespace(run.namespace).Get(
		ctx.Ctx, run.claimName, metav1.GetOptions{})
	if err != nil {
		// Shared classifier — the same NotFound/Timeout/Internal mapping as
		// the secure-access claim read (classifyK8sReadError, helpers.go).
		return classifyK8sReadError(err, fmt.Sprintf("DRA test ResourceClaim %s", run.claimName))
	}
	state, _, _ := unstructured.NestedString(claimObj.Object, "status", "state")
	claimLines := []string{
		fmt.Sprintf("Name:      %s/%s", run.namespace, run.claimName),
		fmt.Sprintf("State:     %s", valueOrUnknown(state)),
	}
	recordRawTextArtifact(ctx, "ResourceClaim status",
		fmt.Sprintf("kubectl get resourceclaim -n %s -o wide", run.namespace), strings.Join(claimLines, "\n"))

	if pod.Status.Phase != corev1.PodSucceeded {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("DRA test pod phase=%s (want Succeeded), GPU allocation may have failed", pod.Status.Phase))
	}

	return nil
}
