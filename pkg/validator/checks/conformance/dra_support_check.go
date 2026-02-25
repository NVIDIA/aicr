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
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

func init() {
	checks.RegisterCheck(&checks.Check{
		Name:                  "dra-support",
		Description:           "Verify DRA driver controller, kubelet plugin, and ResourceSlices exist",
		Phase:                 phaseConformance,
		Func:                  CheckDRASupport,
		TestName:              "TestDRASupport",
		RequirementID:         "dra_support",
		EvidenceTitle:         "DRA Support (Dynamic Resource Allocation)",
		EvidenceDescription:   "Demonstrates that the cluster supports Dynamic Resource Allocation with a functioning DRA driver, kubelet plugin, and GPU ResourceSlices.",
		EvidenceFile:          "dra-support.md",
		SubmissionRequirement: true,
	})
}

// CheckDRASupport validates CNCF requirement #2: DRA Support.
// Verifies DRA driver controller deployment, kubelet plugin DaemonSet,
// and that ResourceSlices (resource.k8s.io/v1 GA) exist advertising GPU resources.
func CheckDRASupport(ctx *checks.ValidationContext) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "kubernetes client is not available")
	}

	// 1. DRA API resources are discoverable.
	resources, err := ctx.Clientset.Discovery().ServerResourcesForGroupVersion("resource.k8s.io/v1")
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "resource.k8s.io/v1 API resources not available", err)
	}
	var apiResources strings.Builder
	for _, r := range resources.APIResources {
		fmt.Fprintf(&apiResources, "%-26s %-22s namespaced=%t\n", r.Name, r.Kind, r.Namespaced)
	}
	recordRawTextArtifact(ctx, "DRA API resources",
		"kubectl api-resources --api-group=resource.k8s.io", apiResources.String())

	// 2. DRA driver pods inventory.
	pods, err := ctx.Clientset.CoreV1().Pods("nvidia-dra-driver").List(ctx.Context, metav1.ListOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to list DRA driver pods", err)
	}
	var driverPods strings.Builder
	for _, pod := range pods.Items {
		fmt.Fprintf(&driverPods, "%-48s ready=%s phase=%s node=%s\n",
			pod.Name, podReadyCount(pod), pod.Status.Phase, pod.Spec.NodeName)
	}
	recordRawTextArtifact(ctx, "DRA driver pods", "kubectl get pods -n nvidia-dra-driver -o wide", driverPods.String())

	// 3. DRA driver controller Deployment available.
	deploy, deployErr := getDeploymentIfAvailable(ctx, "nvidia-dra-driver", "nvidia-dra-driver-gpu-controller")
	if deployErr != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "DRA driver controller check failed", deployErr)
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
	ds, dsErr := getDaemonSetIfReady(ctx, "nvidia-dra-driver", "nvidia-dra-driver-gpu-kubelet-plugin")
	if dsErr != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "DRA kubelet plugin check failed", dsErr)
	}
	if ds != nil {
		recordRawTextArtifact(ctx, "DRA Kubelet Plugin DaemonSet", "",
			fmt.Sprintf("Name:      %s/%s\nReady:     %d/%d pods\nImage:     %s",
				ds.Namespace, ds.Name,
				ds.Status.NumberReady, ds.Status.DesiredNumberScheduled,
				firstContainerImage(ds.Spec.Template.Spec.Containers)))
	}

	// 5. ResourceSlices exist (GPU resources advertised via resource.k8s.io/v1 — GA).
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}
	gvr := schema.GroupVersionResource{
		Group: "resource.k8s.io", Version: "v1", Resource: "resourceslices",
	}
	slices, err := dynClient.Resource(gvr).List(ctx.Context, metav1.ListOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to list ResourceSlices", err)
	}
	var sliceSummary strings.Builder
	fmt.Fprintf(&sliceSummary, "Total ResourceSlices: %d\n", len(slices.Items))
	for _, item := range slices.Items {
		driver, _, _ := unstructured.NestedString(item.Object, "spec", "driver")
		nodeName, _, _ := unstructured.NestedString(item.Object, "spec", "nodeName")
		poolName, _, _ := unstructured.NestedString(item.Object, "spec", "pool", "name")
		fmt.Fprintf(&sliceSummary, "%-48s node=%s driver=%s pool=%s\n",
			item.GetName(), nodeName, driver, poolName)
	}
	recordRawTextArtifact(ctx, "ResourceSlices", "kubectl get resourceslices", sliceSummary.String())
	if len(slices.Items) == 0 {
		return errors.New(errors.ErrCodeNotFound, "no ResourceSlices found (GPU resources not advertised)")
	}

	// 6. Behavioral DRA allocation validation (create claim+pod, wait, capture observed state).
	return validateDRAAllocation(ctx, dynClient)
}

func validateDRAAllocation(ctx *checks.ValidationContext, dynClient dynamic.Interface) error {
	run, err := newDRATestRun()
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "Apply test manifest",
		"kubectl apply -f docs/conformance/cncf/manifests/dra-gpu-test.yaml",
		fmt.Sprintf("Created Namespace=%s ResourceClaim=%s Pod=%s via Kubernetes API",
			draTestNamespace, run.claimName, run.podName))

	if err = deployDRATestResources(ctx.Context, ctx.Clientset, dynClient, run); err != nil {
		return err
	}
	defer func() {
		cleanupDRATestResources(ctx.Context, ctx.Clientset, dynClient, run)
		recordRawTextArtifact(ctx, "Delete test namespace",
			"kubectl delete namespace dra-test --ignore-not-found",
			"Deleted DRA test pod and ResourceClaim; namespace retained intentionally to avoid DRA finalizer stalls.")
	}()

	pod, err := waitForDRATestPod(ctx.Context, ctx.Clientset, run)
	if err != nil {
		return err
	}

	claimObj, err := dynClient.Resource(claimGVR).Namespace(draTestNamespace).Get(
		ctx.Context, run.claimName, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to read DRA test ResourceClaim", err)
	}
	state, _, _ := unstructured.NestedString(claimObj.Object, "status", "state")
	claimLines := []string{
		fmt.Sprintf("Name:      %s/%s", draTestNamespace, run.claimName),
		fmt.Sprintf("State:     %s", valueOrUnknown(state)),
	}
	recordRawTextArtifact(ctx, "ResourceClaim status",
		"kubectl get resourceclaim -n dra-test -o wide", strings.Join(claimLines, "\n"))

	podLines := []string{
		fmt.Sprintf("Name:      %s/%s", pod.Namespace, pod.Name),
		fmt.Sprintf("Phase:     %s", pod.Status.Phase),
		fmt.Sprintf("Node:      %s", valueOrUnknown(pod.Spec.NodeName)),
		fmt.Sprintf("PodIP:     %s", valueOrUnknown(pod.Status.PodIP)),
		fmt.Sprintf("Claims:    %d", len(pod.Spec.ResourceClaims)),
	}
	recordRawTextArtifact(ctx, "Pod status",
		"kubectl get pod dra-gpu-test -n dra-test -o wide", strings.Join(podLines, "\n"))

	logBytes, logErr := ctx.Clientset.CoreV1().Pods(draTestNamespace).GetLogs(run.podName, &corev1.PodLogOptions{}).DoRaw(ctx.Context)
	if logErr != nil {
		recordRawTextArtifact(ctx, "Pod logs", "kubectl logs dra-gpu-test -n dra-test",
			fmt.Sprintf("failed to read logs: %v", logErr))
	} else {
		recordChunkedTextArtifact(ctx, "Pod logs", "kubectl logs dra-gpu-test -n dra-test", string(logBytes))
	}

	if pod.Status.Phase != corev1.PodSucceeded {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("DRA test pod phase=%s (want Succeeded), GPU allocation may have failed", pod.Status.Phase))
	}

	return nil
}
