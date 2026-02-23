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
	"log/slog"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var httpRouteGVR = schema.GroupVersionResource{
	Group: "gateway.networking.k8s.io", Version: "v1", Resource: "httproutes",
}

func init() {
	checks.RegisterCheck(&checks.Check{
		Name:        "inference-gateway",
		Description: "Verify Gateway API for AI/ML inference routing (GatewayClass, Gateway, CRDs)",
		Phase:       phaseConformance,
		Func:        CheckInferenceGateway,
		TestName:    "TestInferenceGateway",
	})
}

// CheckInferenceGateway validates CNCF requirement #6: Inference Gateway.
// Verifies GatewayClass "kgateway" is accepted, Gateway "inference-gateway" is programmed,
// and required Gateway API + InferencePool CRDs exist.
func CheckInferenceGateway(ctx *checks.ValidationContext) error {
	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}

	// 1. GatewayClass "kgateway" accepted
	gcGVR := schema.GroupVersionResource{
		Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gatewayclasses",
	}
	gc, err := dynClient.Resource(gcGVR).Get(ctx.Context, "kgateway", metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "GatewayClass 'kgateway' not found", err)
	}
	if condErr := checkCondition(gc, "Accepted", "True"); condErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "GatewayClass not accepted", condErr)
	}

	// 2. Gateway "inference-gateway" programmed
	gwGVR := schema.GroupVersionResource{
		Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways",
	}
	gw, err := dynClient.Resource(gwGVR).Namespace("kgateway-system").Get(
		ctx.Context, "inference-gateway", metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "Gateway 'inference-gateway' not found", err)
	}
	if condErr := checkCondition(gw, "Programmed", "True"); condErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "Gateway not programmed", condErr)
	}

	// 3. Required CRDs exist
	crdGVR := schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}
	requiredCRDs := []string{
		"gateways.gateway.networking.k8s.io",
		"httproutes.gateway.networking.k8s.io",
		"inferencepools.inference.networking.x-k8s.io",
	}
	for _, crdName := range requiredCRDs {
		_, err := dynClient.Resource(crdGVR).Get(ctx.Context, crdName, metav1.GetOptions{})
		if err != nil {
			return errors.Wrap(errors.ErrCodeNotFound,
				fmt.Sprintf("CRD %s not found", crdName), err)
		}
	}

	// 4. Gateway data-plane readiness (behavioral validation).
	return validateGatewayDataPlane(ctx)
}

// validateGatewayDataPlane verifies the gateway data plane is operational by checking
// listener status, discovering attached HTTPRoutes, and confirming ready proxy endpoints.
func validateGatewayDataPlane(ctx *checks.ValidationContext) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest,
			"kubernetes client is not available for endpoint validation")
	}

	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}

	// 1. Listener status (informational): log attached routes count.
	gwGVR := schema.GroupVersionResource{
		Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways",
	}
	gw, gwErr := dynClient.Resource(gwGVR).Namespace("kgateway-system").Get(
		ctx.Context, "inference-gateway", metav1.GetOptions{})
	if gwErr == nil {
		listeners, found, _ := unstructured.NestedSlice(gw.Object, "status", "listeners")
		if found {
			for _, l := range listeners {
				if lMap, ok := l.(map[string]interface{}); ok {
					name, _, _ := unstructured.NestedString(lMap, "name")
					attached, _, _ := unstructured.NestedInt64(lMap, "attachedRoutes")
					slog.Info("gateway listener status", "listener", name, "attachedRoutes", attached)
				}
			}
		}
	}

	// 2. HTTPRoute discovery (informational): find routes attached to inference-gateway.
	httpRouteList, listErr := dynClient.Resource(httpRouteGVR).Namespace("").List(
		ctx.Context, metav1.ListOptions{})
	if listErr == nil {
		var attached int
		for _, route := range httpRouteList.Items {
			parentRefs, found, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
			if !found {
				continue
			}
			for _, ref := range parentRefs {
				if refMap, ok := ref.(map[string]interface{}); ok {
					name, _, _ := unstructured.NestedString(refMap, "name")
					if name == "inference-gateway" {
						attached++
						break
					}
				}
			}
		}
		slog.Info("HTTPRoutes attached to inference-gateway", "count", attached)
	}

	// 3. Endpoint readiness (hard requirement): verify inference-gateway proxy has ready endpoints.
	// Filter by kubernetes.io/service-name containing "inference-gateway" to avoid matching
	// unrelated services in the namespace (e.g. controller manager, webhooks).
	slices, err := ctx.Clientset.DiscoveryV1().EndpointSlices("kgateway-system").List(
		ctx.Context, metav1.ListOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to list EndpointSlices in kgateway-system", err)
	}

	var hasReadyEndpoint bool
	for _, slice := range slices.Items {
		svcName := slice.Labels["kubernetes.io/service-name"]
		if !strings.Contains(svcName, "inference-gateway") {
			continue
		}
		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready != nil && *ep.Conditions.Ready {
				hasReadyEndpoint = true
				break
			}
		}
		if hasReadyEndpoint {
			break
		}
	}

	if !hasReadyEndpoint {
		return errors.New(errors.ErrCodeInternal,
			"no ready endpoints for inference-gateway proxy in kgateway-system")
	}

	return nil
}
