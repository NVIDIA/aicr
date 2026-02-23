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
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// phaseConformance is the phase identifier for conformance checks.
const phaseConformance = "conformance"

// getDynamicClient returns the dynamic client from context, or creates one from RESTConfig.
func getDynamicClient(ctx *checks.ValidationContext) (dynamic.Interface, error) {
	if ctx.DynamicClient != nil {
		return ctx.DynamicClient, nil
	}
	if ctx.RESTConfig == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "RESTConfig is not available")
	}
	dc, err := dynamic.NewForConfig(ctx.RESTConfig)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create dynamic client", err)
	}
	return dc, nil
}

// httpGet performs an HTTP GET to an in-cluster service URL with context timeout.
func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create request", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable,
			fmt.Sprintf("failed to reach %s", url), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("HTTP %d from %s", resp.StatusCode, url))
	}
	return io.ReadAll(resp.Body)
}

// checkCondition verifies a status condition on an unstructured object.
func checkCondition(obj *unstructured.Unstructured, condType, expectedStatus string) error {
	conditions, found, err := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if err != nil || !found {
		return errors.New(errors.ErrCodeInternal, "status.conditions not found")
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if cond["type"] == condType {
			if cond["status"] == expectedStatus {
				return nil
			}
			return errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("condition %s=%v (want %s)", condType, cond["status"], expectedStatus))
		}
	}
	return errors.New(errors.ErrCodeNotFound,
		fmt.Sprintf("condition %s not found", condType))
}

// verifyDeploymentAvailable checks that a Deployment has at least one available replica.
func verifyDeploymentAvailable(ctx *checks.ValidationContext, namespace, name string) error {
	deploy, err := ctx.Clientset.AppsV1().Deployments(namespace).Get(
		ctx.Context, name, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("deployment %s/%s not found", namespace, name), err)
	}
	if deploy.Status.AvailableReplicas < 1 {
		expected := int32(1)
		if deploy.Spec.Replicas != nil {
			expected = *deploy.Spec.Replicas
		}
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("deployment %s/%s not available: %d/%d replicas",
				namespace, name, deploy.Status.AvailableReplicas, expected))
	}
	return nil
}

// verifyDaemonSetReady checks that a DaemonSet has at least one ready pod.
func verifyDaemonSetReady(ctx *checks.ValidationContext, namespace, name string) error {
	ds, err := ctx.Clientset.AppsV1().DaemonSets(namespace).Get(
		ctx.Context, name, metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("daemonset %s/%s not found", namespace, name), err)
	}
	if ds.Status.NumberReady < 1 {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("daemonset %s/%s not ready: %d/%d pods",
				namespace, name, ds.Status.NumberReady, ds.Status.DesiredNumberScheduled))
	}
	return nil
}

// int32Ptr returns a pointer to the given int32 value.
func int32Ptr(i int32) *int32 { return &i }

// containsAllMetrics checks that all required metric names appear in the given text.
// Returns the list of missing metrics.
func containsAllMetrics(text string, required []string) []string {
	var missing []string
	for _, metric := range required {
		if !strings.Contains(text, metric) {
			missing = append(missing, metric)
		}
	}
	return missing
}

// gpuDriverName is the DRA driver name for NVIDIA GPUs.
const gpuDriverName = "gpu.nvidia.com"

// countAvailableGPUs counts total GPU devices from ResourceSlices and subtracts
// allocated devices from ResourceClaims to determine how many are free.
func countAvailableGPUs(ctx context.Context, dynClient dynamic.Interface) (total, free int, err error) {
	sliceGVR := schema.GroupVersionResource{
		Group: "resource.k8s.io", Version: "v1", Resource: "resourceslices",
	}

	// Count total GPU devices from ResourceSlices.
	slices, err := dynClient.Resource(sliceGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0, errors.Wrap(errors.ErrCodeInternal, "failed to list ResourceSlices", err)
	}
	for _, slice := range slices.Items {
		driver, _, _ := unstructured.NestedString(slice.Object, "spec", "driver")
		if driver != gpuDriverName {
			continue
		}
		devices, found, _ := unstructured.NestedSlice(slice.Object, "spec", "devices")
		if found {
			total += len(devices)
		}
	}

	// Count allocated GPU devices from ResourceClaims.
	var allocated int
	claims, err := dynClient.Resource(claimGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, 0, errors.Wrap(errors.ErrCodeInternal, "failed to list ResourceClaims", err)
	}
	for _, claim := range claims.Items {
		results, found, _ := unstructured.NestedSlice(claim.Object, "status", "allocation", "devices", "results")
		if !found {
			continue
		}
		for _, r := range results {
			result, ok := r.(map[string]interface{})
			if !ok {
				continue
			}
			if result["driver"] == gpuDriverName {
				allocated++
			}
		}
	}

	return total, total - allocated, nil
}
