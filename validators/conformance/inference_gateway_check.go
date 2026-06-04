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
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/netutil"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var httpRouteGVR = schema.GroupVersionResource{
	Group: apiGroupGateway, Version: "v1", Resource: "httproutes",
}

// agentgatewayNamespace is the fixed namespace the agentgateway component
// deploys into (set via defaultNamespace in recipes/registry.yaml). The
// inference-gateway check — including the exposure assessment — assumes the
// Gateway, its LoadBalancer Service, and the controller all live here.
const agentgatewayNamespace = "agentgateway-system"

type gatewayDataPlaneReport struct {
	ListenerCount         int
	AttachedHTTPRoutes    int
	TotalHTTPRoutes       int
	MatchingEndpointSlice int
	ReadyEndpoints        int
}

// CheckInferenceGateway validates CNCF requirement #6: Inference Gateway.
// Verifies GatewayClass "agentgateway" is accepted, Gateway "inference-gateway" is programmed,
// and required Gateway API + InferencePool CRDs exist.
func CheckInferenceGateway(ctx *validators.Context) error {
	// Skip if the recipe does not include agentgateway (inference gateway component).
	// Training clusters typically don't have an inference gateway.
	if !recipeHasComponent(ctx, "agentgateway") {
		return validators.Skip("agentgateway not in recipe — inference gateway check applies to inference clusters only")
	}

	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}

	collectGatewayControlPlaneArtifacts(ctx)

	// 1. GatewayClass "agentgateway" accepted
	gcGVR := schema.GroupVersionResource{
		Group: apiGroupGateway, Version: "v1", Resource: "gatewayclasses",
	}
	gc, err := dynClient.Resource(gcGVR).Get(ctx.Ctx, "agentgateway", metav1.GetOptions{})
	if err != nil {
		return errors.Wrap(errors.ErrCodeNotFound, "GatewayClass 'agentgateway' not found", err)
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
		"kubectl get gatewayclass agentgateway -o yaml",
		fmt.Sprintf("Name:            %s\nControllerName:  %s\nAccepted:        %s\nReason:          %s\nMessage:         %s",
			gc.GetName(), valueOrUnknown(controllerName), gcCond.Status, gcCond.Reason, gcCond.Message))

	// 2. Gateway "inference-gateway" programmed
	gwGVR := schema.GroupVersionResource{
		Group: apiGroupGateway, Version: "v1", Resource: "gateways",
	}
	gw, err := dynClient.Resource(gwGVR).Namespace(agentgatewayNamespace).Get(
		ctx.Ctx, "inference-gateway", metav1.GetOptions{})
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
		"kubectl get gateway inference-gateway -n agentgateway-system -o yaml", gw.Object)

	// 3. Required CRDs exist
	crdGVR := schema.GroupVersionResource{
		Group: apiGroupAPIExtensions, Version: "v1", Resource: resourceCRDs,
	}
	requiredCRDs := []string{
		"gateways.gateway.networking.k8s.io",
		"httproutes.gateway.networking.k8s.io",
		"inferencepools.inference.networking.x-k8s.io",
	}
	var crdSummary strings.Builder
	for _, crdName := range requiredCRDs {
		if _, crdErr := dynClient.Resource(crdGVR).Get(ctx.Ctx, crdName, metav1.GetOptions{}); crdErr != nil {
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
		"kubectl get endpointslices -n agentgateway-system",
		fmt.Sprintf("Listeners:               %d\nAttached HTTPRoutes:     %d\nHTTPRoutes (all):        %d\nMatching EndpointSlices: %d\nReady endpoints:         %d",
			report.ListenerCount, report.AttachedHTTPRoutes, report.TotalHTTPRoutes,
			report.MatchingEndpointSlice, report.ReadyEndpoints))

	// 5. Network exposure (security finding). Surfaces whether the public
	// LoadBalancer is scoped to trusted source ranges or open to 0.0.0.0/0.
	if err := assessGatewayExposure(ctx); err != nil {
		return err
	}
	return nil
}

// requireScopedGatewayEnv, when set truthy, escalates an open inference-gateway
// LoadBalancer (empty spec.loadBalancerSourceRanges) from a non-fatal warning
// to a check failure. The default (unset) preserves the intentional
// open-by-default behavior (#1138): the exposure is recorded and warned, but
// the check still passes.
const requireScopedGatewayEnv = "AICR_REQUIRE_SCOPED_INFERENCE_GATEWAY"

// assessGatewayExposure records whether the inference-gateway public
// LoadBalancer Service(s) in agentgateway-system are scoped to specific source
// ranges or reachable from any source (0.0.0.0/0), and surfaces the result as
// an evidence artifact. An open gateway is a non-fatal warning by default;
// setting AICR_REQUIRE_SCOPED_INFERENCE_GATEWAY=true makes it fail the check.
// See #1160.
func assessGatewayExposure(ctx *validators.Context) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest,
			"kubernetes client is not available for gateway exposure assessment")
	}

	svcs, err := ctx.Clientset.CoreV1().Services(agentgatewayNamespace).List(ctx.Ctx, metav1.ListOptions{})
	if err != nil {
		// The exposure assessment is an advisory security finding layered on
		// top of the data-plane checks that have already passed by this point.
		// A transient apiserver error must not fail the whole inference-gateway
		// check in the default (advisory) mode — record it best-effort and
		// pass. Under enforce mode we cannot confirm the gateway is scoped, so
		// fail closed. See #1160.
		if envTruthy(requireScopedGatewayEnv) {
			return errors.Wrap(errors.ErrCodeInternal,
				"failed to list Services for gateway exposure assessment under "+requireScopedGatewayEnv+"=true", err)
		}
		slog.Warn("could not assess inference-gateway exposure; skipping advisory finding",
			"error", err, "enforce", requireScopedGatewayEnv+"=true")
		recordRawTextArtifact(ctx, "Inference Gateway Exposure",
			"kubectl get svc -n agentgateway-system",
			fmt.Sprintf("Could not list Services to assess exposure: %v\n"+
				"Advisory finding skipped (non-fatal). Set %s=true to fail closed when exposure cannot be assessed.",
				err, requireScopedGatewayEnv))
		return nil
	}

	var (
		summary  strings.Builder
		openSvcs []string
		lbCount  int
	)
	for i := range svcs.Items {
		svc := &svcs.Items[i]
		if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
			continue
		}
		// Only the inference-gateway proxy Service is in scope. Mirror the
		// EndpointSlice readiness filter (isInferenceGatewayProxyName) so a
		// co-located LoadBalancer — controller manager, webhook, metrics — is
		// neither mislabeled as the gateway nor able to fail the check under
		// enforce mode. See #1160.
		if !isInferenceGatewayProxyName(svc.Name) {
			continue
		}
		lbCount++
		ranges := svc.Spec.LoadBalancerSourceRanges
		if isOpenSourceRanges(ranges) {
			openSvcs = append(openSvcs, svc.Name)
			if len(ranges) == 0 {
				fmt.Fprintf(&summary, "%-40s type=LoadBalancer sourceRanges=<empty> (OPEN to 0.0.0.0/0)\n", svc.Name)
			} else {
				fmt.Fprintf(&summary, "%-40s type=LoadBalancer sourceRanges=%s (OPEN: includes an any-source CIDR)\n",
					svc.Name, strings.Join(ranges, ","))
			}
		} else {
			fmt.Fprintf(&summary, "%-40s type=LoadBalancer sourceRanges=%s\n",
				svc.Name, strings.Join(ranges, ","))
		}
	}

	if lbCount == 0 {
		recordRawTextArtifact(ctx, "Inference Gateway Exposure",
			"kubectl get svc -n agentgateway-system",
			"No LoadBalancer Service in agentgateway-system (gateway is not internet-facing via a cloud load balancer).")
		return nil
	}

	recordRawTextArtifact(ctx, "Inference Gateway Exposure",
		"kubectl get svc -n agentgateway-system -o custom-columns=NAME:.metadata.name,TYPE:.spec.type,SOURCERANGES:.spec.loadBalancerSourceRanges",
		summary.String())

	if len(openSvcs) == 0 {
		return nil
	}

	msg := fmt.Sprintf(
		"inference-gateway LoadBalancer Service(s) [%s] are open to the entire internet (0.0.0.0/0): "+
			"spec.loadBalancerSourceRanges is empty or includes an any-source CIDR. "+
			"Scope to trusted CIDRs via agentgateway.allowedSourceRanges — a recipe componentRef override or "+
			"`--set-json agentgateway:allowedSourceRanges='[\"<cidr>\"]'`.",
		strings.Join(openSvcs, ", "))

	if envTruthy(requireScopedGatewayEnv) {
		return errors.New(errors.ErrCodeInvalidRequest, msg)
	}

	slog.Warn("inference-gateway is internet-facing (open to 0.0.0.0/0)",
		"services", openSvcs,
		"enforce", requireScopedGatewayEnv+"=true")
	recordRawTextArtifact(ctx, "Inference Gateway Exposure WARNING", "",
		"WARNING: "+msg+"\n\nNon-fatal finding (open-by-default is intentional). "+
			"Set "+requireScopedGatewayEnv+"=true to make an open gateway fail this check.")
	return nil
}

// isOpenSourceRanges reports whether a LoadBalancer's source-range list leaves
// the Service reachable from the entire internet. An empty list is open by
// definition (the cloud LB admits all sources). A non-empty list is still open
// if any entry is an any-source CIDR (prefix length 0, e.g. 0.0.0.0/0 or ::/0):
// a length-only check would misclassify such a list as scoped and silently pass
// an internet-wide gateway under enforce mode. See #1160.
func isOpenSourceRanges(ranges []string) bool {
	if len(ranges) == 0 {
		return true
	}
	for _, r := range ranges {
		if netutil.IsAnySourceCIDR(r) {
			return true
		}
	}
	return false
}

// gatewayControlPlaneNameMarkers identify co-located agentgateway control-plane
// components that share the "inference-gateway" name prefix but are not the
// data-plane proxy. A plain substring match on "inference-gateway" would also
// catch these, so they are excluded explicitly: under enforce mode a
// control-plane LoadBalancer (e.g. inference-gateway-controller-manager) would
// otherwise fail the exposure check, and a control-plane EndpointSlice would
// inflate the proxy readiness count. See #1160.
var gatewayControlPlaneNameMarkers = []string{"controller-manager", "webhook", "metrics"}

// isInferenceGatewayProxyName reports whether name refers to the
// inference-gateway data-plane proxy (its Service or EndpointSlice) rather than
// a co-located control-plane component sharing the prefix. The Service and the
// EndpointSlice readiness filter use this same predicate so the exposure
// assessment and the data-plane check stay in lockstep. See #1160.
func isInferenceGatewayProxyName(name string) bool {
	if !strings.Contains(name, "inference-gateway") {
		return false
	}
	for _, marker := range gatewayControlPlaneNameMarkers {
		if strings.Contains(name, marker) {
			return false
		}
	}
	return true
}

// envTruthy reports whether the named environment variable is set to a truthy
// value (per strconv.ParseBool: 1/t/T/TRUE/true/True, etc.).
func envTruthy(key string) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	return err == nil && b
}

// validateGatewayDataPlane verifies the gateway data plane is operational by checking
// listener status, discovering attached HTTPRoutes, and confirming ready proxy endpoints.
func validateGatewayDataPlane(ctx *validators.Context) (*gatewayDataPlaneReport, error) {
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
		Group: apiGroupGateway, Version: "v1", Resource: "gateways",
	}
	gw, gwErr := dynClient.Resource(gwGVR).Namespace(agentgatewayNamespace).Get(
		ctx.Ctx, "inference-gateway", metav1.GetOptions{})
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
		ctx.Ctx, metav1.ListOptions{})
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
	// Filter by kubernetes.io/service-name via isInferenceGatewayProxyName to avoid matching
	// unrelated services in the namespace (e.g. controller manager, webhooks, metrics).
	slices, err := ctx.Clientset.DiscoveryV1().EndpointSlices(agentgatewayNamespace).List(
		ctx.Ctx, metav1.ListOptions{})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to list EndpointSlices in agentgateway-system", err)
	}

	for _, slice := range slices.Items {
		svcName := slice.Labels["kubernetes.io/service-name"]
		if !isInferenceGatewayProxyName(svcName) {
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
			"no ready endpoints for inference-gateway proxy in agentgateway-system")
	}

	return report, nil
}

func collectGatewayControlPlaneArtifacts(ctx *validators.Context) {
	if ctx.Clientset == nil {
		return
	}

	deploys, deployErr := ctx.Clientset.AppsV1().Deployments(agentgatewayNamespace).List(
		ctx.Ctx, metav1.ListOptions{})
	if deployErr != nil {
		recordRawTextArtifact(ctx, "agentgateway deployments", "kubectl get deploy -n agentgateway-system",
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
		recordRawTextArtifact(ctx, "agentgateway deployments", "kubectl get deploy -n agentgateway-system", deploymentSummary.String())
	}

	pods, podErr := ctx.Clientset.CoreV1().Pods(agentgatewayNamespace).List(ctx.Ctx, metav1.ListOptions{})
	if podErr != nil {
		recordRawTextArtifact(ctx, "agentgateway pods", "kubectl get pods -n agentgateway-system",
			fmt.Sprintf("failed to list pods: %v", podErr))
		return
	}
	var podSummary strings.Builder
	for _, pod := range pods.Items {
		fmt.Fprintf(&podSummary, "%-48s ready=%s phase=%s node=%s\n",
			pod.Name, podReadyCount(pod), pod.Status.Phase, valueOrUnknown(pod.Spec.NodeName))
	}
	recordRawTextArtifact(ctx, "agentgateway pods", "kubectl get pods -n agentgateway-system", podSummary.String())
}
