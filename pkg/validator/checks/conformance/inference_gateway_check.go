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

type gatewayDataPlaneReport struct {
	ListenerAttachedRoutes []string
	AttachedHTTPRoutes     int
	MatchingEndpointSlices int
	ReadyEndpoints         int
}

func init() {
	checks.RegisterCheck(&checks.Check{
		Name:                  "inference-gateway",
		Description:           "Verify Gateway API for AI/ML inference routing (GatewayClass, Gateway, CRDs)",
		Phase:                 phaseConformance,
		Func:                  CheckInferenceGateway,
		TestName:              "TestInferenceGateway",
		RequirementID:         "ai_inference",
		EvidenceTitle:         "Inference API Gateway (kgateway)",
		EvidenceDescription:   "Demonstrates that the cluster supports Kubernetes Gateway API for AI/ML inference routing with an operational GatewayClass and Gateway.",
		EvidenceFile:          "inference-gateway.md",
		SubmissionRequirement: true,
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
	gcCond, condErr := getConditionObservation(gc, "Accepted")
	if condErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "GatewayClass not accepted", condErr)
	}
	if gcCond.Status != "True" {
		return errors.Wrap(errors.ErrCodeInternal, "GatewayClass not accepted",
			errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("condition Accepted=%s (want True)", gcCond.Status)))
	}
	recordArtifact(ctx, "GatewayClass Status",
		fmt.Sprintf("Name:      %s\nAccepted:  %s\nReason:    %s\nMessage:   %s",
			gc.GetName(), gcCond.Status, gcCond.Reason, gcCond.Message))

	// 2. Gateway "inference-gateway" programmed
	gwGVR := schema.GroupVersionResource{
		Group: "gateway.networking.k8s.io", Version: "v1", Resource: "gateways",
	}
	gw, err := dynClient.Resource(gwGVR).Namespace("kgateway-system").Get(
		ctx.Context, "inference-gateway", metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "Gateway 'inference-gateway' not found", err)
	}
	gwCond, condErr := getConditionObservation(gw, "Programmed")
	if condErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "Gateway not programmed", condErr)
	}
	if gwCond.Status != "True" {
		return errors.Wrap(errors.ErrCodeInternal, "Gateway not programmed",
			errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("condition Programmed=%s (want True)", gwCond.Status)))
	}
	recordArtifact(ctx, "Gateway Status",
		fmt.Sprintf("Name:       %s\nNamespace:  %s\nProgrammed: %s\nReason:     %s\nMessage:    %s",
			gw.GetName(), gw.GetNamespace(), gwCond.Status, gwCond.Reason, gwCond.Message))

	// 3. Required CRDs exist
	crdGVR := schema.GroupVersionResource{
		Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions",
	}
	requiredCRDs := []string{
		"gateways.gateway.networking.k8s.io",
		"httproutes.gateway.networking.k8s.io",
		"inferencepools.inference.networking.x-k8s.io",
	}
	var crdSummary strings.Builder
	for _, crdName := range requiredCRDs {
		_, crdErr := dynClient.Resource(crdGVR).Get(ctx.Context, crdName, metav1.GetOptions{})
		if crdErr != nil {
			return errors.Wrap(errors.ErrCodeNotFound,
				fmt.Sprintf("CRD %s not found", crdName), crdErr)
		}
		fmt.Fprintf(&crdSummary, "  %s: present\n", crdName)
	}
	recordArtifact(ctx, "Required CRDs", crdSummary.String())

	// 4. Gateway data-plane readiness (behavioral validation).
	dpReport, err := validateGatewayDataPlane(ctx)
	if err != nil {
		return err
	}

	listenerSummary := "none"
	if len(dpReport.ListenerAttachedRoutes) > 0 {
		listenerSummary = strings.Join(dpReport.ListenerAttachedRoutes, ", ")
	}
	recordArtifact(ctx, "Gateway Data Plane",
		fmt.Sprintf("Listeners: %s\nAttached HTTPRoutes: %d\nMatching EndpointSlices: %d\nReady Endpoints: %d",
			listenerSummary, dpReport.AttachedHTTPRoutes, dpReport.MatchingEndpointSlices, dpReport.ReadyEndpoints))
	return nil
}

// validateGatewayDataPlane verifies the gateway data plane is operational by checking
// listener status, discovering attached HTTPRoutes, and confirming ready proxy endpoints.
func validateGatewayDataPlane(ctx *checks.ValidationContext) (*gatewayDataPlaneReport, error) {
	report := &gatewayDataPlaneReport{}

	if ctx.Clientset == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"kubernetes client is not available for endpoint validation")
	}

	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return nil, err
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
					report.ListenerAttachedRoutes = append(report.ListenerAttachedRoutes,
						fmt.Sprintf("%s=%d", name, attached))
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
		report.AttachedHTTPRoutes = attached
		slog.Info("HTTPRoutes attached to inference-gateway", "count", attached)
	}

	// 3. Endpoint readiness (hard requirement): verify inference-gateway proxy has ready endpoints.
	// Filter by kubernetes.io/service-name containing "inference-gateway" to avoid matching
	// unrelated services in the namespace (e.g. controller manager, webhooks).
	slices, err := ctx.Clientset.DiscoveryV1().EndpointSlices("kgateway-system").List(
		ctx.Context, metav1.ListOptions{})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to list EndpointSlices in kgateway-system", err)
	}

	for _, slice := range slices.Items {
		svcName := slice.Labels["kubernetes.io/service-name"]
		if !strings.Contains(svcName, "inference-gateway") {
			continue
		}
		report.MatchingEndpointSlices++
		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready != nil && *ep.Conditions.Ready {
				report.ReadyEndpoints++
			}
		}
	}

	if report.ReadyEndpoints == 0 {
		return nil, errors.New(errors.ErrCodeInternal,
			"no ready endpoints for inference-gateway proxy in kgateway-system")
	}

	return report, nil
}
