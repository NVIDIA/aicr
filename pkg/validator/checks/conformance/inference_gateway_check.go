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
	ListenerCount         int
	AttachedHTTPRoutes    int
	TotalHTTPRoutes       int
	MatchingEndpointSlice int
	ReadyEndpoints        int
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

	collectGatewayControlPlaneArtifacts(ctx)

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
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("GatewayClass not accepted: status=%s reason=%s message=%s",
				gcCond.Status, gcCond.Reason, gcCond.Message))
	}
	controllerName, _, _ := unstructured.NestedString(gc.Object, "spec", "controllerName")
	recordRawTextArtifact(ctx, "GatewayClass",
		"kubectl get gatewayclass kgateway -o yaml",
		fmt.Sprintf("Name:            %s\nControllerName:  %s\nAccepted:        %s\nReason:          %s\nMessage:         %s",
			gc.GetName(), valueOrUnknown(controllerName), gcCond.Status, gcCond.Reason, gcCond.Message))

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
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("Gateway not programmed: status=%s reason=%s message=%s",
				gwCond.Status, gwCond.Reason, gwCond.Message))
	}
	addresses, found, _ := unstructured.NestedSlice(gw.Object, "status", "addresses")
	addressCount := 0
	if found {
		addressCount = len(addresses)
	}
	recordRawTextArtifact(ctx, "Gateways",
		"kubectl get gateways -A",
		fmt.Sprintf("Name:            %s/%s\nProgrammed:      %s\nReason:          %s\nMessage:         %s\nAddressCount:    %d",
			gw.GetNamespace(), gw.GetName(), gwCond.Status, gwCond.Reason, gwCond.Message, addressCount))
	recordObjectYAMLArtifact(ctx, "Gateway details",
		"kubectl get gateway inference-gateway -n kgateway-system -o yaml", gw.Object)

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
		if _, crdErr := dynClient.Resource(crdGVR).Get(ctx.Context, crdName, metav1.GetOptions{}); crdErr != nil {
			return errors.Wrap(errors.ErrCodeNotFound,
				fmt.Sprintf("CRD %s not found", crdName), crdErr)
		}
		fmt.Fprintf(&crdSummary, "  %s: present\n", crdName)
	}
	recordRawTextArtifact(ctx, "Required CRDs", "", crdSummary.String())

	// 4. Gateway data-plane readiness (behavioral validation).
	report, err := validateGatewayDataPlane(ctx)
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "Gateway Data Plane",
		"kubectl get endpointslices -n kgateway-system",
		fmt.Sprintf("Listeners:               %d\nAttached HTTPRoutes:     %d\nHTTPRoutes (all):        %d\nMatching EndpointSlices: %d\nReady endpoints:         %d",
			report.ListenerCount, report.AttachedHTTPRoutes, report.TotalHTTPRoutes,
			report.MatchingEndpointSlice, report.ReadyEndpoints))
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
			report.ListenerCount = len(listeners)
			for _, l := range listeners {
				if lMap, ok := l.(map[string]interface{}); ok {
					name, _, _ := unstructured.NestedString(lMap, "name")
					attached, _, _ := unstructured.NestedInt64(lMap, "attachedRoutes")
					report.AttachedHTTPRoutes += int(attached)
					slog.Info("gateway listener status", "listener", name, "attachedRoutes", attached)
				}
			}
		}
	}

	// 2. HTTPRoute discovery (informational): find routes attached to inference-gateway.
	httpRouteList, listErr := dynClient.Resource(httpRouteGVR).Namespace("").List(
		ctx.Context, metav1.ListOptions{})
	if listErr == nil {
		report.TotalHTTPRoutes = len(httpRouteList.Items)
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
		report.MatchingEndpointSlice++
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

func collectGatewayControlPlaneArtifacts(ctx *checks.ValidationContext) {
	if ctx.Clientset == nil {
		return
	}

	deploys, deployErr := ctx.Clientset.AppsV1().Deployments("kgateway-system").List(
		ctx.Context, metav1.ListOptions{})
	if deployErr != nil {
		recordRawTextArtifact(ctx, "kgateway deployments", "kubectl get deploy -n kgateway-system",
			fmt.Sprintf("failed to list deployments: %v", deployErr))
	} else {
		var deploymentSummary strings.Builder
		for _, d := range deploys.Items {
			expected := int32(1)
			if d.Spec.Replicas != nil {
				expected = *d.Spec.Replicas
			}
			fmt.Fprintf(&deploymentSummary, "%-40s available=%d/%d image=%s\n",
				d.Name, d.Status.AvailableReplicas, expected, firstContainerImage(d.Spec.Template.Spec.Containers))
		}
		recordRawTextArtifact(ctx, "kgateway deployments", "kubectl get deploy -n kgateway-system", deploymentSummary.String())
	}

	pods, podErr := ctx.Clientset.CoreV1().Pods("kgateway-system").List(ctx.Context, metav1.ListOptions{})
	if podErr != nil {
		recordRawTextArtifact(ctx, "kgateway pods", "kubectl get pods -n kgateway-system",
			fmt.Sprintf("failed to list pods: %v", podErr))
		return
	}
	var podSummary strings.Builder
	for _, pod := range pods.Items {
		fmt.Fprintf(&podSummary, "%-48s ready=%s phase=%s node=%s\n",
			pod.Name, podReadyCount(pod), pod.Status.Phase, valueOrUnknown(pod.Spec.NodeName))
	}
	recordRawTextArtifact(ctx, "kgateway pods", "kubectl get pods -n kgateway-system", podSummary.String())
}
