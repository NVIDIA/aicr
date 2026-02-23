// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package conformance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/k8s"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
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

func init() {
	checks.RegisterCheck(&checks.Check{
		Name:        "secure-accelerator-access",
		Description: "Verify DRA-mediated GPU access (no device plugin, no hostPath)",
		Phase:       phaseConformance,
		Func:        CheckSecureAcceleratorAccess,
		TestName:    "TestSecureAcceleratorAccess",
	})
}

// CheckSecureAcceleratorAccess validates CNCF requirement #3: Secure Accelerator Access.
// Creates a DRA-based GPU test pod with unique names, waits for completion, and verifies
// proper access patterns: resourceClaims instead of device plugin, no hostPath to GPU
// devices, and ResourceClaim is allocated.
func CheckSecureAcceleratorAccess(ctx *checks.ValidationContext) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}

	run, err := newDRATestRun()
	if err != nil {
		return err
	}

	// Deploy DRA test resources and ensure cleanup.
	if err = deployDRATestResources(ctx.Context, ctx.Clientset, dynClient, run); err != nil {
		return err
	}
	defer cleanupDRATestResources(ctx.Context, ctx.Clientset, dynClient, run)

	// Wait for test pod to reach terminal state.
	pod, err := waitForDRATestPod(ctx.Context, ctx.Clientset, run)
	if err != nil {
		return err
	}

	// Validate DRA access patterns on the completed pod.
	if err = validateDRAPatterns(ctx.Context, dynClient, pod, run); err != nil {
		return err
	}

	// Validate isolation: a pod without DRA claims cannot access GPU devices.
	return validateDRAIsolation(ctx.Context, ctx.Clientset, run)
}

// deployDRATestResources creates the namespace, ResourceClaim, and Pod for the DRA test.
func deployDRATestResources(ctx context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface, run *draTestRun) error {
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
	pod := buildDRATestPod(run)
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
			pod, err := clientset.CoreV1().Pods(draTestNamespace).Get(
				ctx, run.podName, metav1.GetOptions{})
			if err != nil {
				return false, errors.Wrap(errors.ErrCodeInternal, "failed to get DRA test pod", err)
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
func validateDRAPatterns(ctx context.Context, dynClient dynamic.Interface, pod *corev1.Pod, run *draTestRun) error {
	// 1. Pod uses resourceClaims (DRA pattern).
	if len(pod.Spec.ResourceClaims) == 0 {
		return errors.New(errors.ErrCodeInternal,
			"pod does not use DRA resourceClaims")
	}

	// 2. No nvidia.com/gpu in resources.limits (device plugin pattern).
	for _, c := range pod.Spec.Containers {
		if c.Resources.Limits != nil {
			if _, hasGPU := c.Resources.Limits["nvidia.com/gpu"]; hasGPU {
				return errors.New(errors.ErrCodeInternal,
					"pod uses device plugin (nvidia.com/gpu in limits) instead of DRA")
			}
		}
	}

	// 3. No hostPath volumes to /dev/nvidia*.
	for _, vol := range pod.Spec.Volumes {
		if vol.HostPath != nil && strings.Contains(vol.HostPath.Path, "/dev/nvidia") {
			return errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("pod has hostPath volume to %s", vol.HostPath.Path))
		}
	}

	// 4. ResourceClaim exists.
	if _, err := dynClient.Resource(claimGVR).Namespace(draTestNamespace).Get(
		ctx, run.claimName, metav1.GetOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeNotFound,
			fmt.Sprintf("ResourceClaim %s not found", run.claimName), err)
	}

	// 5. Pod completed successfully — proves DRA allocation worked.
	if pod.Status.Phase != corev1.PodSucceeded {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("DRA test pod phase=%s (want Succeeded), GPU allocation may have failed",
				pod.Status.Phase))
	}

	return nil
}

// validateDRAIsolation verifies that a pod WITHOUT DRA ResourceClaims cannot see GPU devices.
// This proves GPU access is truly mediated by DRA — the scheduler does not expose devices
// to pods that lack claims.
func validateDRAIsolation(ctx context.Context, clientset kubernetes.Interface, run *draTestRun) error {
	// Create no-claim pod.
	pod := buildNoClaimTestPod(run)
	if _, err := clientset.CoreV1().Pods(draTestNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create no-claim isolation test pod", err)
	}
	defer func() {
		_ = k8s.IgnoreNotFound(clientset.CoreV1().Pods(draTestNamespace).Delete(
			ctx, run.noClaimPodName, metav1.DeleteOptions{}))
		waitForDeletion(ctx, func() error {
			_, err := clientset.CoreV1().Pods(draTestNamespace).Get(
				ctx, run.noClaimPodName, metav1.GetOptions{})
			return err
		})
	}()

	// Wait for no-claim pod to reach terminal state.
	var resultPod *corev1.Pod
	waitCtx, cancel := context.WithTimeout(ctx, defaults.DRATestPodTimeout)
	defer cancel()

	err := wait.PollUntilContextCancel(waitCtx, defaults.PodPollInterval, true,
		func(ctx context.Context) (bool, error) {
			p, err := clientset.CoreV1().Pods(draTestNamespace).Get(
				ctx, run.noClaimPodName, metav1.GetOptions{})
			if err != nil {
				return false, errors.Wrap(errors.ErrCodeInternal,
					"failed to get no-claim isolation test pod", err)
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
			return errors.Wrap(errors.ErrCodeTimeout,
				"no-claim isolation test pod did not complete in time", err)
		}
		return errors.Wrap(errors.ErrCodeInternal,
			"no-claim isolation test pod polling failed", err)
	}

	// Strict success criteria: require Succeeded (exit 0 = script confirmed no GPU visible).
	// Failed means either GPU was visible (exit 1) or the container failed for other reasons.
	if resultPod.Status.Phase != corev1.PodSucceeded {
		exitCode := int32(-1)
		if len(resultPod.Status.ContainerStatuses) > 0 {
			cs := resultPod.Status.ContainerStatuses[0]
			if cs.State.Terminated != nil {
				exitCode = cs.State.Terminated.ExitCode
			}
		}
		if exitCode == 1 {
			return errors.New(errors.ErrCodeInternal,
				"GPU devices visible without DRA claim — isolation broken (container exit code 1)")
		}
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("no-claim isolation test pod failed with exit code %d — cannot verify isolation",
				exitCode))
	}

	// Verify no hostPath to GPU devices on the no-claim pod.
	for _, vol := range resultPod.Spec.Volumes {
		if vol.HostPath != nil && strings.Contains(vol.HostPath.Path, "/dev/nvidia") {
			return errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("no-claim pod has hostPath volume to %s — isolation broken",
					vol.HostPath.Path))
		}
	}

	return nil
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

// waitForDeletion polls until a resource is gone (NotFound) or the context expires.
func waitForDeletion(ctx context.Context, getFunc func() error) {
	pollCtx, cancel := context.WithTimeout(ctx, defaults.K8sCleanupTimeout)
	defer cancel()
	_ = wait.PollUntilContextCancel(pollCtx, defaults.PodPollInterval, true,
		func(ctx context.Context) (bool, error) {
			err := getFunc()
			if k8serrors.IsNotFound(err) {
				return true, nil
			}
			return false, nil
		},
	)
}

// buildDRATestPod returns the Pod spec for the DRA GPU allocation test.
func buildDRATestPod(run *draTestRun) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      run.podName,
			Namespace: draTestNamespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations: []corev1.Toleration{
				{Operator: corev1.TolerationOpExists},
			},
			ResourceClaims: []corev1.PodResourceClaim{
				{
					Name:              "gpu",
					ResourceClaimName: strPtr(run.claimName),
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

// buildNoClaimTestPod returns a Pod spec identical to the DRA test pod but WITHOUT ResourceClaims.
// If the cluster properly mediates GPU access through DRA, this pod will not see GPU devices.
func buildNoClaimTestPod(run *draTestRun) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      run.noClaimPodName,
			Namespace: draTestNamespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Tolerations: []corev1.Toleration{
				{Operator: corev1.TolerationOpExists},
			},
			Containers: []corev1.Container{
				{
					Name:  "isolation-test",
					Image: "nvidia/cuda:12.9.0-base-ubuntu24.04",
					Command: []string{
						"bash", "-c",
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

func strPtr(s string) *string {
	return &s
}
