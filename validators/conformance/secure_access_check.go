// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s"
	"github.com/NVIDIA/aicr/validators"
	"github.com/NVIDIA/aicr/validators/helper"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	draTestNamespace = "dra-test"
	draTestPrefix    = "dra-gpu-test-"
	draClaimPrefix   = "gpu-claim-"
	draNoClaimPrefix = "dra-no-claim-"
)

// draTestRun holds per-invocation resource names to avoid collisions.
type draTestRun struct {
	podName        string
	claimName      string
	noClaimPodName string
}

type draPatternReport struct {
	PodName             string
	PodPhase            corev1.PodPhase
	ResourceClaimCount  int
	GPULimitsCount      int
	HostPathGPUMounts   int
	ClaimState          string
	ClaimAllocationInfo string
}

type draIsolationReport struct {
	PodName           string
	PodPhase          corev1.PodPhase
	ExitCode          int32
	ResourceClaims    int
	HostPathGPUMounts int
	Logs              string
}

func newDRATestRun() (*draTestRun, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate random suffix", err)
	}
	suffix := hex.EncodeToString(b)
	return &draTestRun{
		podName:        draTestPrefix + suffix,
		claimName:      draClaimPrefix + suffix,
		noClaimPodName: draNoClaimPrefix + suffix,
	}, nil
}

var claimGVR = schema.GroupVersionResource{
	Group: "resource.k8s.io", Version: "v1", Resource: "resourceclaims",
}

var resourceSliceGVR = schema.GroupVersionResource{
	Group: "resource.k8s.io", Version: "v1", Resource: "resourceslices",
}

// CheckSecureAcceleratorAccess validates CNCF requirement #3: Secure Accelerator Access.
// Creates a DRA-based GPU test pod with unique names, waits for completion, and verifies
// proper access patterns: resourceClaims instead of device plugin, no hostPath to GPU
// devices, and ResourceClaim is allocated.
func CheckSecureAcceleratorAccess(ctx *validators.Context) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}

	collectSecureAccessBaselineArtifacts(ctx, dynClient)

	run, err := newDRATestRun()
	if err != nil {
		return err
	}

	// Deploy DRA test resources and ensure cleanup.
	if err = deployDRATestResources(ctx.Ctx, ctx.Clientset, dynClient, run, ctx.Tolerations); err != nil {
		return err
	}
	defer func() { //nolint:contextcheck // Fresh context: parent may be canceled during cleanup
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
		defer cleanupCancel()
		cleanupDRATestResources(cleanupCtx, ctx.Clientset, dynClient, run)
	}()

	// Wait for test pod to reach terminal state.
	pod, err := waitForDRATestPod(ctx.Ctx, ctx.Clientset, run)
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "Pod status",
		"kubectl get pod isolation-test -n dra-test -o wide",
		fmt.Sprintf("Name:      %s/%s\nPhase:     %s\nNode:      %s\nClaims:    %d resource claims",
			pod.Namespace, pod.Name, pod.Status.Phase, valueOrUnknown(pod.Spec.NodeName), len(pod.Spec.ResourceClaims)))

	// Validate DRA access patterns on the completed pod.
	patternReport, err := validateDRAPatterns(ctx.Ctx, dynClient, pod, run)
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "Pod resourceClaims",
		"kubectl get pod isolation-test -n dra-test -o jsonpath='{.spec.resourceClaims}'",
		fmt.Sprintf("Pod:             %s/%s\nResourceClaims:  %d\nGPULimits:       %d",
			draTestNamespace, patternReport.PodName, patternReport.ResourceClaimCount, patternReport.GPULimitsCount))
	recordRawTextArtifact(ctx, "Pod volumes (no hostPath)",
		"kubectl get pod isolation-test -n dra-test -o jsonpath='{.spec.volumes}'",
		fmt.Sprintf("Pod:               %s/%s\nHostPathGPUMounts: %d",
			draTestNamespace, patternReport.PodName, patternReport.HostPathGPUMounts))
	recordRawTextArtifact(ctx, "ResourceClaim allocation",
		"kubectl get resourceclaim isolated-gpu -n dra-test -o wide",
		fmt.Sprintf("Name:             %s/%s\nState:            %s\nAllocationStatus: %s",
			draTestNamespace, run.claimName, patternReport.ClaimState, patternReport.ClaimAllocationInfo))

	logBytes, logErr := ctx.Clientset.CoreV1().Pods(draTestNamespace).GetLogs(
		run.podName, &corev1.PodLogOptions{}).DoRaw(ctx.Ctx)
	if logErr != nil {
		recordRawTextArtifact(ctx, "Isolation test logs",
			"kubectl logs isolation-test -n dra-test",
			fmt.Sprintf("failed to read isolation test logs: %v", logErr))
	} else {
		recordRawTextArtifact(ctx, "Isolation test logs",
			"kubectl logs isolation-test -n dra-test", string(logBytes))
	}

	// Validate isolation: a pod without DRA claims cannot access GPU devices.
	// Target the same node as the DRA test pod — isolation must be proven on the
	// GPU node, not a control-plane node that has no GPUs in the first place.
	isolationReport, err := validateDRAIsolation(ctx.Ctx, ctx.Clientset, run, pod.Spec.NodeName)
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "DRA Isolation Test",
		"kubectl logs dra-no-claim-<id> -n dra-test",
		fmt.Sprintf("Pod:               %s/%s\nPhase:             %s\nExitCode:          %d\nResourceClaims:    %d\nHostPathGPUMounts: %d",
			draTestNamespace, isolationReport.PodName, isolationReport.PodPhase,
			isolationReport.ExitCode, isolationReport.ResourceClaims, isolationReport.HostPathGPUMounts))
	recordRawTextArtifact(ctx, "No-claim pod logs",
		"kubectl logs dra-no-claim-<id> -n dra-test", isolationReport.Logs)
	return nil
}

func collectSecureAccessBaselineArtifacts(ctx *validators.Context, dynClient dynamic.Interface) {
	// ClusterPolicy status.
	clusterPolicyGVR := schema.GroupVersionResource{
		Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies",
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

	// ResourceSlices summary + details.
	slices, err := dynClient.Resource(resourceSliceGVR).List(ctx.Ctx, metav1.ListOptions{})
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
	recordObjectYAMLArtifact(ctx, "GPU devices in ResourceSlice", "kubectl get resourceslices -o yaml", slices.Object)
}

// deployDRATestResources creates the namespace, ResourceClaim, and Pod for the DRA test.
// tolerations, when non-nil, replace the default tolerate-all policy on the test pod.
func deployDRATestResources(ctx context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface, run *draTestRun, tolerations []corev1.Toleration) error {
	// 1. Create namespace (idempotent).
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: draTestNamespace},
	}
	if _, err := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); k8s.IgnoreAlreadyExists(err) != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create namespace", err)
	}

	// 2. Create ResourceClaim with unique name.
	claim := buildResourceClaim(run)
	if _, err := dynClient.Resource(claimGVR).Namespace(draTestNamespace).Create(
		ctx, claim, metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create ResourceClaim", err)
	}

	// 3. Create Pod with unique name.
	pod := buildDRATestPod(run, tolerations)
	if _, err := clientset.CoreV1().Pods(draTestNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create DRA test pod", err)
	}

	return nil
}

// waitForDRATestPod polls until the DRA test pod reaches a terminal state.
func waitForDRATestPod(ctx context.Context, clientset kubernetes.Interface, run *draTestRun) (*corev1.Pod, error) {
	var resultPod *corev1.Pod

	waitCtx, cancel := context.WithTimeout(ctx, defaults.DRATestPodTimeout)
	defer cancel()

	err := wait.PollUntilContextCancel(waitCtx, defaults.PodPollInterval, true,
		func(ctx context.Context) (bool, error) {
			pod, getErr := clientset.CoreV1().Pods(draTestNamespace).Get(
				ctx, run.podName, metav1.GetOptions{})
			if getErr != nil {
				if k8serrors.IsNotFound(getErr) {
					return false, nil // pod not yet visible after create, keep polling
				}
				// K8s client rate limiter fires near context deadline — retry gracefully.
				if strings.Contains(getErr.Error(), "rate limiter") {
					return false, nil
				}
				return false, errors.Wrap(errors.ErrCodeInternal, "failed to get DRA test pod", getErr)
			}
			// Fail fast if pod is stuck in a non-recoverable state (e.g. ImagePullBackOff).
			if reason := podStuckReason(pod); reason != "" {
				return false, errors.New(errors.ErrCodeInternal,
					fmt.Sprintf("DRA test pod stuck: %s", reason))
			}
			switch pod.Status.Phase { //nolint:exhaustive // only terminal states matter
			case corev1.PodSucceeded, corev1.PodFailed:
				resultPod = pod
				return true, nil
			default:
				return false, nil
			}
		},
	)
	if err != nil {
		// Distinguish timeout from other poll errors (RBAC, NotFound, etc).
		if ctx.Err() != nil || waitCtx.Err() != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout, "DRA test pod did not complete in time", err)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "DRA test pod polling failed", err)
	}

	return resultPod, nil
}

// validateDRAPatterns verifies the completed pod uses proper DRA access patterns.
func validateDRAPatterns(ctx context.Context, dynClient dynamic.Interface, pod *corev1.Pod, run *draTestRun) (*draPatternReport, error) {
	report := &draPatternReport{
		PodName:            pod.Name,
		PodPhase:           pod.Status.Phase,
		ResourceClaimCount: len(pod.Spec.ResourceClaims),
	}

	// 1. Pod uses resourceClaims (DRA pattern).
	if len(pod.Spec.ResourceClaims) == 0 {
		return nil, errors.New(errors.ErrCodeInternal, "pod does not use DRA resourceClaims")
	}

	// 2. No nvidia.com/gpu in resources.limits (device plugin pattern).
	for _, c := range pod.Spec.Containers {
		if c.Resources.Limits != nil {
			if _, hasGPU := c.Resources.Limits["nvidia.com/gpu"]; hasGPU {
				report.GPULimitsCount++
				return nil, errors.New(errors.ErrCodeInternal,
					"pod uses device plugin (nvidia.com/gpu in limits) instead of DRA")
			}
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

	// 4. ResourceClaim exists.
	claim, err := dynClient.Resource(claimGVR).Namespace(draTestNamespace).Get(
		ctx, run.claimName, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound,
			fmt.Sprintf("ResourceClaim %s not found", run.claimName), err)
	}
	report.ClaimState, _, _ = unstructured.NestedString(claim.Object, "status", "state")
	results, found, _ := unstructured.NestedSlice(claim.Object, "status", "allocation", "devices", "results")
	if found {
		report.ClaimAllocationInfo = fmt.Sprintf("%d allocated device result(s)", len(results))
	} else {
		report.ClaimAllocationInfo = "no allocation results reported"
	}

	// 5. Pod completed successfully — proves DRA allocation worked.
	if pod.Status.Phase != corev1.PodSucceeded {
		return nil, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("DRA test pod phase=%s (want Succeeded), GPU allocation may have failed",
				pod.Status.Phase))
	}

	return report, nil
}

// validateDRAIsolation verifies that a pod WITHOUT DRA ResourceClaims cannot see GPU devices.
// This proves GPU access is truly mediated by DRA — the scheduler does not expose devices
// to pods that lack claims. gpuNodeName pins the pod to the same GPU node where the DRA
// test ran, ensuring isolation is proven on a node that actually has GPUs and bypassing
// scheduler-level delays.
func validateDRAIsolation(ctx context.Context, clientset kubernetes.Interface, run *draTestRun, gpuNodeName string) (*draIsolationReport, error) {
	// Create no-claim pod pinned to the GPU node.
	pod := buildNoClaimTestPod(run, gpuNodeName)
	if _, err := clientset.CoreV1().Pods(draTestNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create no-claim isolation test pod", err)
	}
	defer func() { //nolint:contextcheck // Fresh context: parent may be canceled during cleanup
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), defaults.K8sCleanupTimeout)
		defer cleanupCancel()
		_ = k8s.IgnoreNotFound(clientset.CoreV1().Pods(draTestNamespace).Delete(
			cleanupCtx, run.noClaimPodName, metav1.DeleteOptions{}))
		waitForDeletion(cleanupCtx, func() error {
			_, err := clientset.CoreV1().Pods(draTestNamespace).Get(
				cleanupCtx, run.noClaimPodName, metav1.GetOptions{})
			return err
		})
	}()

	// Wait for no-claim pod to reach terminal state.
	var resultPod *corev1.Pod
	var lastPhase corev1.PodPhase
	var lastContainerStatus string
	waitCtx, cancel := context.WithTimeout(ctx, defaults.DRATestPodTimeout)
	defer cancel()

	err := wait.PollUntilContextCancel(waitCtx, defaults.PodPollInterval, true,
		func(ctx context.Context) (bool, error) {
			p, getErr := clientset.CoreV1().Pods(draTestNamespace).Get(
				ctx, run.noClaimPodName, metav1.GetOptions{})
			if getErr != nil {
				if k8serrors.IsNotFound(getErr) {
					return false, nil // pod not yet visible after create, keep polling
				}
				// K8s client rate limiter fires near context deadline — retry gracefully.
				if strings.Contains(getErr.Error(), "rate limiter") {
					return false, nil
				}
				return false, errors.Wrap(errors.ErrCodeInternal,
					"failed to get no-claim isolation test pod", getErr)
			}
			// Track last known state for diagnostics on timeout.
			lastPhase = p.Status.Phase
			lastContainerStatus = podWaitingStatus(p)
			// Fail fast if pod is stuck in a non-recoverable state (e.g. ImagePullBackOff).
			if reason := podStuckReason(p); reason != "" {
				return false, errors.New(errors.ErrCodeInternal,
					fmt.Sprintf("no-claim isolation test pod stuck: %s", reason))
			}
			switch p.Status.Phase { //nolint:exhaustive // only terminal states matter
			case corev1.PodSucceeded, corev1.PodFailed:
				resultPod = p
				return true, nil
			default:
				return false, nil
			}
		},
	)
	if err != nil {
		if ctx.Err() != nil || waitCtx.Err() != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout,
				fmt.Sprintf("no-claim isolation test pod did not complete in time (last phase=%s, status=%s, node=%s)",
					lastPhase, lastContainerStatus, gpuNodeName), err)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"no-claim isolation test pod polling failed", err)
	}

	report := &draIsolationReport{
		PodName:        resultPod.Name,
		PodPhase:       resultPod.Status.Phase,
		ExitCode:       podExitCode(resultPod),
		ResourceClaims: len(resultPod.Spec.ResourceClaims),
	}

	// Strict success criteria: require Succeeded (exit 0 = script confirmed no GPU visible).
	// Failed means either GPU was visible (exit 1) or the container failed for other reasons.
	if resultPod.Status.Phase != corev1.PodSucceeded {
		exitCode := report.ExitCode
		if exitCode == 1 {
			return nil, errors.New(errors.ErrCodeInternal,
				"GPU devices visible without DRA claim — isolation broken (container exit code 1)")
		}
		return nil, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("no-claim isolation test pod failed with exit code %d — cannot verify isolation",
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
				fmt.Sprintf("no-claim pod has hostPath volume to %s — isolation broken",
					vol.HostPath.Path))
		}
	}
	report.HostPathGPUMounts = 0

	logBytes, logErr := clientset.CoreV1().Pods(draTestNamespace).GetLogs(
		run.noClaimPodName, &corev1.PodLogOptions{}).DoRaw(ctx)
	if logErr != nil {
		report.Logs = fmt.Sprintf("failed to read logs: %v", logErr)
	} else {
		report.Logs = string(logBytes)
	}

	return report, nil
}

// cleanupDRATestResources removes test resources. Best-effort: errors are ignored
// since cleanup failures should not mask test results.
// The namespace is intentionally NOT deleted — it's harmless to leave and
// namespace deletion can hang on DRA finalizers.
func cleanupDRATestResources(ctx context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface, run *draTestRun) {
	// Delete pod first (releases claim reservation), then claim.
	_ = k8s.IgnoreNotFound(clientset.CoreV1().Pods(draTestNamespace).Delete(
		ctx, run.podName, metav1.DeleteOptions{}))
	waitForDeletion(ctx, func() error {
		_, err := clientset.CoreV1().Pods(draTestNamespace).Get(ctx, run.podName, metav1.GetOptions{})
		return err
	})
	_ = k8s.IgnoreNotFound(dynClient.Resource(claimGVR).Namespace(draTestNamespace).Delete(
		ctx, run.claimName, metav1.DeleteOptions{}))
	// Delete no-claim isolation pod (best-effort, may already be cleaned up by validateDRAIsolation).
	_ = k8s.IgnoreNotFound(clientset.CoreV1().Pods(draTestNamespace).Delete(
		ctx, run.noClaimPodName, metav1.DeleteOptions{}))
}

// buildDRATestPod returns the Pod spec for the DRA GPU allocation test.
// tolerations, when non-nil, replace the default tolerate-all policy.
func buildDRATestPod(run *draTestRun, tolerations []corev1.Toleration) *corev1.Pod {
	if tolerations == nil {
		tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      run.podName,
			Namespace: draTestNamespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations:   tolerations,
			ResourceClaims: []corev1.PodResourceClaim{
				{
					Name:              "gpu",
					ResourceClaimName: helper.StrPtr(run.claimName),
				},
			},
			Containers: []corev1.Container{
				{
					Name:    "gpu-test",
					Image:   "nvidia/cuda:12.9.0-base-ubuntu24.04",
					Command: []string{"bash", "-c", "ls /dev/nvidia* && echo 'DRA GPU allocation successful'"},
					Resources: corev1.ResourceRequirements{
						Claims: []corev1.ResourceClaim{
							{Name: "gpu"},
						},
					},
				},
			},
		},
	}
}

// buildNoClaimTestPod returns a Pod spec WITHOUT ResourceClaims.
// If the cluster properly mediates GPU access through DRA, this pod will not see GPU devices.
// Uses a lightweight image (busybox) since no CUDA libraries are needed — only checking
// whether /dev/nvidia* device files are visible.
// gpuNodeName pins the pod to the GPU node via NodeName, bypassing the scheduler to ensure
// the isolation test runs on a node that actually has GPUs and avoiding scheduler delays.
func buildNoClaimTestPod(run *draTestRun, gpuNodeName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      run.noClaimPodName,
			Namespace: draTestNamespace,
		},
		Spec: corev1.PodSpec{
			NodeName:      gpuNodeName,
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations: []corev1.Toleration{
				{Operator: corev1.TolerationOpExists},
			},
			Containers: []corev1.Container{
				{
					Name:  "isolation-test",
					Image: "busybox:1.37",
					Command: []string{
						"sh", "-c",
						"if ls /dev/nvidia* 2>/dev/null; then echo 'FAIL: GPU visible without DRA claim' && exit 1; else echo 'PASS: GPU isolated' && exit 0; fi",
					},
				},
			},
		},
	}
}

// buildResourceClaim returns the unstructured ResourceClaim for the DRA test.
func buildResourceClaim(run *draTestRun) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "resource.k8s.io/v1",
			"kind":       "ResourceClaim",
			"metadata": map[string]interface{}{
				"name":      run.claimName,
				"namespace": draTestNamespace,
			},
			"spec": map[string]interface{}{
				"devices": map[string]interface{}{
					"requests": []interface{}{
						map[string]interface{}{
							"name": "gpu",
							"exactly": map[string]interface{}{
								"deviceClassName": "gpu.nvidia.com",
								"allocationMode":  "ExactCount",
								"count":           int64(1),
							},
						},
					},
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
