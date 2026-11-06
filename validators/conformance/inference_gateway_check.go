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
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/netutil"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	httpRouteGVR = validators.HTTPRouteGVR
	gatewayGVR   = validators.GatewayGVR
)

// The metav1 condition statuses as the strings the unstructured condition
// reads compare against.
const (
	conditionStatusTrue  = string(metav1.ConditionTrue)
	conditionStatusFalse = string(metav1.ConditionFalse)
)

type gatewayDataPlaneReport struct {
	ListenerCount         int
	AttachedHTTPRoutes    int
	TotalHTTPRoutes       int
	MatchingEndpointSlice int
	ReadyEndpoints        int
	ProgrammedListeners   int
	StatusAddresses       int
	// RouteDiscoveryError is set when HTTPRoutes could not be listed, so the
	// route counts above are unknown rather than zero.
	RouteDiscoveryError string
}

// routeCountText renders the all-routes count for the data-plane artifact,
// saying "unavailable" when discovery failed instead of printing zero.
func routeCountText(report *gatewayDataPlaneReport) string {
	if report.RouteDiscoveryError != "" {
		return "unavailable (" + report.RouteDiscoveryError + ")"
	}
	return strconv.Itoa(report.TotalHTTPRoutes)
}

// CheckInferenceGateway validates CNCF requirement #6: Inference Gateway.
// The gateway under test is whichever component the recipe declares as its
// inference gateway (validators.ResolveInferenceGatewayProfile): its
// GatewayClass must be accepted, its Gateway programmed, the Gateway API and
// InferencePool CRDs present, and its data plane serving by the probe the
// profile names. The Gateway object is read once and handed to the
// data-plane, exposure and traffic steps, so every verdict comes from one
// observation. After the status checks, the traffic phase
// (runGatewayTrafficPhase) routes requests through that Gateway: a
// header-matched canary and a 70/30 weighted split over two CPU backends, as
// the upstream TestGatewayAPIInference asserts.
func CheckInferenceGateway(ctx *validators.Context) error {
	return checkInferenceGateway(ctx, runGatewayTrafficPhase)
}

func checkInferenceGateway(ctx *validators.Context, traffic gatewayTrafficRunner) error {
	resolved, found, err := validators.ResolveInferenceGatewayProfile(ctx)
	if err != nil {
		return err
	}
	// Training clusters typically don't have an inference gateway.
	if !found {
		return validators.SkipWithReason(validators.SkipReasonComponentNotInRecipe, "no inference gateway component in recipe. The inference gateway check applies to inference clusters only")
	}
	profile := &resolved

	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return err
	}

	collectGatewayControlPlaneArtifacts(ctx, profile)

	// 1. GatewayClass accepted
	gcGVR := schema.GroupVersionResource{
		Group: apiGroupGateway, Version: "v1", Resource: "gatewayclasses",
	}
	gc, err := dynClient.Resource(gcGVR).Get(ctx.Ctx, profile.GatewayClass, metav1.GetOptions{})
	if err != nil {
		return lookupError(fmt.Sprintf("GatewayClass %q", profile.GatewayClass), err)
	}
	gcCond, condErr := getConditionObservation(gc, "Accepted")
	if condErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "GatewayClass not accepted", condErr)
	}
	if gcCond.Status != conditionStatusTrue {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("GatewayClass not accepted: status=%s reason=%s message=%s",
				gcCond.Status, gcCond.Reason, gcCond.Message))
	}
	controllerName, _, _ := unstructured.NestedString(gc.Object, "spec", "controllerName")
	recordRawTextArtifact(ctx, "GatewayClass",
		"kubectl get gatewayclass "+profile.GatewayClass+" -o yaml",
		fmt.Sprintf("Name:            %s\nControllerName:  %s\nAccepted:        %s\nReason:          %s\nMessage:         %s",
			gc.GetName(), valueOrUnknown(controllerName), gcCond.Status, gcCond.Reason, gcCond.Message))

	// 2. Gateway programmed
	gw, err := dynClient.Resource(gatewayGVR).Namespace(profile.Namespace).Get(
		ctx.Ctx, profile.GatewayName, metav1.GetOptions{})
	if err != nil {
		return lookupError(fmt.Sprintf("Gateway %s/%s", profile.Namespace, profile.GatewayName), err)
	}
	// The class and the Gateway were each read by name; a Gateway programmed
	// by another implementation would otherwise pass on the strength of the
	// declared class being accepted. The evidence script fails the same state.
	gwClass, _, _ := unstructured.NestedString(gw.Object, "spec", "gatewayClassName")
	if gwClass != profile.GatewayClass {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("Gateway %s/%s has gatewayClassName %q; the recipe declares %q",
				profile.Namespace, profile.GatewayName, gwClass, profile.GatewayClass))
	}
	gwCond, condErr := getConditionObservation(gw, "Programmed")
	if condErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "Gateway not programmed", condErr)
	}
	if gwCond.Status != conditionStatusTrue {
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
		fmt.Sprintf("Name:            %s/%s\nGatewayClass:    %s\nProgrammed:      %s\nReason:          %s\nMessage:         %s\nAddressCount:    %d",
			gw.GetNamespace(), gw.GetName(), gwClass, gwCond.Status, gwCond.Reason, gwCond.Message, addressCount))
	recordObjectYAMLArtifact(ctx, "Gateway details",
		gatewayGetCommand(profile)+" -o yaml", gw.Object)

	// 3. Required CRDs exist
	crdGVR := schema.GroupVersionResource{
		Group: apiGroupAPIExtensions, Version: "v1", Resource: resourceCRDs,
	}
	requiredCRDs := []string{
		"gateways.gateway.networking.k8s.io",
		"httproutes.gateway.networking.k8s.io",
		"inferencepools.inference.networking.k8s.io",
	}
	var crdSummary strings.Builder
	for _, crdName := range requiredCRDs {
		if _, crdErr := dynClient.Resource(crdGVR).Get(ctx.Ctx, crdName, metav1.GetOptions{}); crdErr != nil {
			return lookupError("CRD "+crdName, crdErr)
		}
		fmt.Fprintf(&crdSummary, "  %s: present\n", crdName)
	}
	recordRawTextArtifact(ctx, "Required CRDs", "", crdSummary.String())

	// 4. Gateway data-plane readiness (behavioral validation).
	report, err := validateGatewayDataPlane(ctx, profile, gw)
	if err != nil {
		return err
	}
	recordRawTextArtifact(ctx, "Gateway Data Plane",
		gatewayDataPlaneCommand(profile),
		fmt.Sprintf("Probe:                   %s\nListeners:               %d\nProgrammed listeners:    %d\nAttached HTTPRoutes:     %d\nHTTPRoutes (all):        %s\nMatching EndpointSlices: %d\nReady endpoints:         %d\nStatus addresses:        %d",
			profile.Probe, report.ListenerCount, report.ProgrammedListeners, report.AttachedHTTPRoutes, routeCountText(report),
			report.MatchingEndpointSlice, report.ReadyEndpoints, report.StatusAddresses))

	// 5. Network exposure (security finding). Surfaces whether the gateway is
	// scoped to trusted source ranges / private addresses or open to the internet.
	if err := assessGatewayExposure(ctx, profile, gw); err != nil {
		return err
	}

	// 6. Traffic: header-routed canary and weighted split through the
	// Gateway, the behavioral half of the requirement.
	return traffic(ctx, profile, gw)
}

func gatewayGetCommand(profile *validators.InferenceGatewayProfile) string {
	return "kubectl get gateway " + profile.GatewayName + " -n " + profile.Namespace
}

func gatewayDataPlaneCommand(profile *validators.InferenceGatewayProfile) string {
	if profile.Probe == validators.GatewayProbeGatewayAddress {
		return gatewayGetCommand(profile) + " -o jsonpath='{.status.addresses}{.status.listeners}'"
	}
	return "kubectl get endpointslices -n " + profile.Namespace
}

// requireScopedGatewayEnv, when set truthy, escalates an open inference
// gateway (a LoadBalancer whose spec.loadBalancerSourceRanges is empty or
// includes an any-source CIDR, a Gateway programmed on a public or
// unclassifiable address, or an exposure that could not be read) from a
// non-fatal warning to a check failure. The default (unset) preserves the intentional
// open-by-default behavior (#1138): the exposure is recorded and warned, but
// the check still passes.
const requireScopedGatewayEnv = "AICR_REQUIRE_SCOPED_INFERENCE_GATEWAY"

// assessGatewayExposure records whether the inference gateway is reachable
// from any source and surfaces the result as an evidence artifact. For a
// Service-backed data plane the fact read is the proxy LoadBalancer's
// spec.loadBalancerSourceRanges (#1160); for an address-backed data plane it
// is the class of each status address of the Gateway already in hand. An open
// gateway is a non-fatal warning by default; setting
// AICR_REQUIRE_SCOPED_INFERENCE_GATEWAY=true makes it fail the check.
func assessGatewayExposure(ctx *validators.Context, profile *validators.InferenceGatewayProfile, gw *unstructured.Unstructured) error {
	if profile.Probe == validators.GatewayProbeGatewayAddress {
		return assessGatewayAddressExposure(ctx, profile, gw)
	}
	return assessGatewayServiceExposure(ctx, profile)
}

func assessGatewayServiceExposure(ctx *validators.Context, profile *validators.InferenceGatewayProfile) error {
	if ctx.Clientset == nil {
		return errors.New(errors.ErrCodeInvalidRequest,
			"kubernetes client is not available for gateway exposure assessment")
	}
	listCmd := "kubectl get svc -n " + profile.Namespace

	svcs, err := ctx.Clientset.CoreV1().Services(profile.Namespace).List(ctx.Ctx, metav1.ListOptions{})
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
		recordRawTextArtifact(ctx, "Inference Gateway Exposure", listCmd,
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
		// Only the gateway's proxy Service is in scope. The EndpointSlice
		// readiness filter uses the same predicate so a co-located
		// LoadBalancer — controller manager, webhook, metrics — is neither
		// mislabeled as the gateway nor able to fail the check under enforce
		// mode. See #1160.
		if !profile.IsProxyName(svc.Name) {
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
		recordRawTextArtifact(ctx, "Inference Gateway Exposure", listCmd,
			fmt.Sprintf("No LoadBalancer Service in %s (gateway is not internet-facing via a cloud load balancer).", profile.Namespace))
		return nil
	}

	recordRawTextArtifact(ctx, "Inference Gateway Exposure",
		listCmd+" -o custom-columns=NAME:.metadata.name,TYPE:.spec.type,SOURCERANGES:.spec.loadBalancerSourceRanges",
		summary.String())

	if len(openSvcs) == 0 {
		return nil
	}

	remedy := "Scope spec.loadBalancerSourceRanges on the gateway's proxy Service to trusted CIDRs."
	if profile.Component == validators.DefaultInferenceGatewayProfile().Component {
		remedy = "Scope to trusted CIDRs via agentgateway.allowedSourceRanges — a recipe componentRef override or " +
			"`--set-json agentgateway:allowedSourceRanges='[\"<cidr>\"]'`."
	}
	msg := fmt.Sprintf(
		"inference-gateway LoadBalancer Service(s) [%s] are open to the entire internet (0.0.0.0/0): "+
			"spec.loadBalancerSourceRanges is empty or includes an any-source CIDR. %s",
		strings.Join(openSvcs, ", "), remedy)

	return reportOpenGateway(ctx, msg, "services", openSvcs)
}

// assessGatewayAddressExposure is the exposure assessment for a data plane
// with no Service: it classifies each status address of the Gateway as
// private (RFC 1918, RFC 6598 shared space, ULA, loopback, link-local),
// public, or unclassifiable (a Hostname-type address the check does not
// resolve). Public and unclassifiable addresses are both reported; under
// enforce mode both fail, because an address the check cannot classify is
// one it cannot confirm scoped. What the step cannot see is the route domain
// or firewall policy the gateway itself applies to that address, so the
// artifact says the assessment is address-based.
func assessGatewayAddressExposure(ctx *validators.Context, profile *validators.InferenceGatewayProfile, gw *unstructured.Unstructured) error {
	getCmd := gatewayGetCommand(profile) + " -o jsonpath='{.status.addresses}'"

	var (
		summary      strings.Builder
		public       []string
		unclassified []string
		classified   int
	)
	addresses, _, _ := unstructured.NestedSlice(gw.Object, "status", "addresses")
	for _, a := range addresses {
		aMap, ok := a.(map[string]any)
		if !ok {
			continue
		}
		typ, _, _ := unstructured.NestedString(aMap, "type")
		value, _, _ := unstructured.NestedString(aMap, "value")
		class := classifyGatewayAddress(value)
		if class != gatewayAddressUnclassified {
			classified++
		}
		fmt.Fprintf(&summary, "%-40s type=%s class=%s\n", value, valueOrUnknown(typ), class)
		switch class {
		case gatewayAddressPublic:
			public = append(public, value)
		case gatewayAddressUnclassified:
			unclassified = append(unclassified, value)
		}
	}
	fmt.Fprintf(&summary, "\nAssessed from Gateway status addresses: this data plane has no Service. "+
		"Route-domain or firewall scoping applied by the gateway to these addresses is not visible here.\n")
	recordRawTextArtifact(ctx, "Inference Gateway Exposure", getCmd, summary.String())

	if classified == 0 && len(unclassified) == 0 && envTruthy(requireScopedGatewayEnv) {
		// No address at all, so nothing was confirmed scoped. An address the
		// check cannot classify is reported as a finding below instead.
		// CheckInferenceGateway never reaches this branch, since the
		// data-plane step fails a Gateway without an address in every mode
		// first; it keeps this function total for a caller that does not.
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("Gateway %s/%s reports no status.addresses to assess under %s=true",
				profile.Namespace, profile.GatewayName, requireScopedGatewayEnv))
	}

	var findings []string
	if len(public) > 0 {
		findings = append(findings, fmt.Sprintf(
			"inference gateway %s/%s is programmed on public address(es) [%s]. "+
				"Give the Gateway a private address, or confine the public one with the %s component's own network policy",
			profile.Namespace, profile.GatewayName, strings.Join(public, ", "), profile.Component))
	}
	if len(unclassified) > 0 {
		findings = append(findings, fmt.Sprintf(
			"inference gateway %s/%s is programmed on address(es) [%s] the check cannot classify "+
				"(a hostname, or an unspecified address bound on every interface); "+
				"resolve or replace them to confirm the gateway is not internet-facing",
			profile.Namespace, profile.GatewayName, strings.Join(unclassified, ", ")))
	}
	if len(findings) == 0 {
		return nil
	}
	return reportOpenGateway(ctx, strings.Join(findings, "; ")+".", "addresses", append(public, unclassified...))
}

// reportOpenGateway applies the exposure policy to a finding: fail under
// enforce mode, otherwise warn and record it as a non-fatal artifact.
func reportOpenGateway(ctx *validators.Context, msg, subjectKey string, subjects []string) error {
	if envTruthy(requireScopedGatewayEnv) {
		return errors.New(errors.ErrCodeInvalidRequest, msg)
	}
	slog.Warn("inference-gateway may be internet-facing",
		subjectKey, subjects,
		"enforce", requireScopedGatewayEnv+"=true")
	recordRawTextArtifact(ctx, "Inference Gateway Exposure WARNING", "",
		"WARNING: "+msg+"\n\nNon-fatal finding (open-by-default is intentional). "+
			"Set "+requireScopedGatewayEnv+"=true to make an open gateway fail this check.")
	return nil
}

const (
	gatewayAddressPrivate      = "private"
	gatewayAddressPublic       = "public"
	gatewayAddressUnclassified = "unclassified"
)

// sharedAddressSpace is RFC 6598 (100.64.0.0/10), which netip does not treat
// as private although it is never internet-routable; on-prem and DPU fabrics
// use it for gateway VIPs.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// classifyGatewayAddress reports whether a Gateway status address is private
// (RFC 1918, RFC 6598 shared space, ULA, loopback or link-local), public, or
// unclassifiable: a Hostname-type address the check does not resolve, or an
// unspecified address (0.0.0.0, ::), which means bound on every interface and
// so cannot be confirmed scoped.
func classifyGatewayAddress(value string) string {
	addr, err := netip.ParseAddr(value)
	if err != nil {
		return gatewayAddressUnclassified
	}
	addr = addr.Unmap()
	if addr.IsUnspecified() {
		return gatewayAddressUnclassified
	}
	if addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || sharedAddressSpace.Contains(addr) {
		return gatewayAddressPrivate
	}
	return gatewayAddressPublic
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
	return slices.ContainsFunc(ranges, netutil.IsAnySourceCIDR)
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

// validateGatewayDataPlane verifies the gateway data plane is operational by
// reading listener status off the Gateway already in hand, discovering
// attached HTTPRoutes, and confirming the data plane is serving by the
// profile's probe: ready proxy endpoints, or a programmed Gateway that carries
// an address.
func validateGatewayDataPlane(ctx *validators.Context, profile *validators.InferenceGatewayProfile, gw *unstructured.Unstructured) (*gatewayDataPlaneReport, error) {
	report := &gatewayDataPlaneReport{}

	dynClient, err := getDynamicClient(ctx)
	if err != nil {
		return nil, err
	}

	// 1. Listener and address status, reported for both probes (informational
	// for a Service-backed gateway; the address probe reads the same object
	// as a hard requirement below).
	listeners, found, _ := unstructured.NestedSlice(gw.Object, "status", "listeners")
	if found {
		report.ListenerCount = len(listeners)
		for _, l := range listeners {
			if lMap, ok := l.(map[string]any); ok {
				name, _, _ := unstructured.NestedString(lMap, "name")
				attached, _, _ := unstructured.NestedInt64(lMap, "attachedRoutes")
				report.AttachedHTTPRoutes += int(attached)
				if listenerProgrammed(lMap) {
					report.ProgrammedListeners++
				}
				slog.Info("gateway listener status", "listener", name, "attachedRoutes", attached)
			}
		}
	}
	addresses, _, _ := unstructured.NestedSlice(gw.Object, "status", "addresses")
	report.StatusAddresses = len(addresses)

	// 2. HTTPRoute discovery (informational): find routes attached to the Gateway.
	httpRouteList, listErr := dynClient.Resource(httpRouteGVR).Namespace("").List(
		ctx.Ctx, metav1.ListOptions{})
	if listErr != nil {
		// Informational step, so not fatal, but the artifact must say the
		// counts are unavailable rather than report zero routes.
		slog.Warn("HTTPRoute discovery failed; route counts unavailable",
			"gateway", profile.GatewayName, "error", listErr)
		report.RouteDiscoveryError = listErr.Error()
	}
	if listErr == nil {
		report.TotalHTTPRoutes = len(httpRouteList.Items)
		var attached int
		for i := range httpRouteList.Items {
			if httpRouteTargetsGateway(&httpRouteList.Items[i], profile) {
				attached++
			}
		}
		report.AttachedHTTPRoutes = attached
		slog.Info("HTTPRoutes attached to inference gateway", "gateway", profile.GatewayName, "count", attached)
	}

	// 3. Data-plane readiness (hard requirement).
	if profile.Probe == validators.GatewayProbeGatewayAddress {
		if addrErr := validateGatewayAddressDataPlane(gw, profile); addrErr != nil {
			return nil, addrErr
		}
		return report, nil
	}

	// Only the Service probe needs the typed client; the address probe above
	// read the Gateway object alone.
	if ctx.Clientset == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"kubernetes client is not available for endpoint validation")
	}
	// Filter by kubernetes.io/service-name via the profile's proxy predicate to
	// avoid matching unrelated services in the namespace (e.g. controller
	// manager, webhooks, metrics).
	slices, err := ctx.Clientset.DiscoveryV1().EndpointSlices(profile.Namespace).List(
		ctx.Ctx, metav1.ListOptions{})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to list EndpointSlices in "+profile.Namespace, err)
	}

	for _, slice := range slices.Items {
		svcName := slice.Labels["kubernetes.io/service-name"]
		if !profile.IsProxyName(svcName) {
			continue
		}
		report.MatchingEndpointSlice++
		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready != nil && *ep.Conditions.Ready {
				report.ReadyEndpoints++
			}
		}
	}

	if report.MatchingEndpointSlice == 0 {
		return nil, errors.New(errors.ErrCodeNotFound,
			fmt.Sprintf("no EndpointSlice in %s belongs to a Service named %q or %q-*: the %s proxy Service was not found; "+
				"check componentRef.namespace and overrides.inferenceGateway.gateway, or declare dataPlane: gateway-address when the proxy Service is not named after the Gateway in its namespace",
				profile.Namespace, profile.GatewayName, profile.GatewayName, profile.Component))
	}
	if report.ReadyEndpoints == 0 {
		return nil, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("%d EndpointSlice(s) match the %s proxy in %s but none has a ready endpoint",
				report.MatchingEndpointSlice, profile.GatewayName, profile.Namespace))
	}

	return report, nil
}

// validateGatewayAddressDataPlane is the hard requirement for a data plane
// observed through the Gateway object, the probe for one that is not a
// Kubernetes workload: at least one listener in status, at least one status
// address, and no listener with Programmed=False. A listener whose Programmed
// condition is absent or Unknown is tolerated: per-listener conditions are
// optional in the Gateway API, so their absence, or an Unknown a controller
// still reconciling reports, is not a failure of a check that runs once with
// no retry. The Gateway's own Programmed=True is the requirement, and
// CheckInferenceGateway has already gated on it before calling here. The
// counts the caller reports for both probes are taken from the same status.
func validateGatewayAddressDataPlane(gw *unstructured.Unstructured, profile *validators.InferenceGatewayProfile) error {
	listeners, _, _ := unstructured.NestedSlice(gw.Object, "status", "listeners")
	if len(listeners) == 0 {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("Gateway %s/%s reports no listeners in status", profile.Namespace, profile.GatewayName))
	}
	for _, l := range listeners {
		lMap, ok := l.(map[string]any)
		if !ok {
			continue
		}
		name, _, _ := unstructured.NestedString(lMap, "name")
		conds, _, _ := unstructured.NestedSlice(lMap, "conditions")
		for _, c := range conds {
			cMap, ok := c.(map[string]any)
			if !ok {
				continue
			}
			typ, _, _ := unstructured.NestedString(cMap, "type")
			status, _, _ := unstructured.NestedString(cMap, "status")
			if typ == "Programmed" && status == conditionStatusFalse {
				reason, _, _ := unstructured.NestedString(cMap, "reason")
				return errors.New(errors.ErrCodeInternal,
					fmt.Sprintf("Gateway %s/%s listener %q is not programmed: status=%s reason=%s",
						profile.Namespace, profile.GatewayName, name, status, reason))
			}
		}
	}
	addresses, _, _ := unstructured.NestedSlice(gw.Object, "status", "addresses")
	if len(addresses) == 0 {
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("Gateway %s/%s reports no status.addresses: the %s data plane has not been assigned an address",
				profile.Namespace, profile.GatewayName, profile.Component))
	}
	return nil
}

// listenerProgrammed reports whether a status.listeners entry carries a
// Programmed=True condition.
func listenerProgrammed(listener map[string]any) bool {
	conds, _, _ := unstructured.NestedSlice(listener, "conditions")
	for _, c := range conds {
		cMap, ok := c.(map[string]any)
		if !ok {
			continue
		}
		typ, _, _ := unstructured.NestedString(cMap, "type")
		status, _, _ := unstructured.NestedString(cMap, "status")
		if typ == "Programmed" && status == conditionStatusTrue {
			return true
		}
	}
	return false
}

// lookupError keeps a missing object apart from a failed read: RBAC denial or
// an apiserver error leaves the object's existence unknown, which must not
// read as absent.
func lookupError(what string, err error) error {
	if apierrors.IsNotFound(err) {
		return errors.Wrap(errors.ErrCodeNotFound, what+" not found", err)
	}
	return errors.Wrap(errors.ErrCodeInternal, "failed to read "+what, err)
}

func collectGatewayControlPlaneArtifacts(ctx *validators.Context, profile *validators.InferenceGatewayProfile) {
	if ctx.Clientset == nil {
		return
	}
	// A Service-backed gateway's namespace is its own, so its workloads are
	// its control plane. An address-backed data plane may sit in a namespace
	// shared with unrelated workloads, so the artifact names the namespace
	// rather than claiming the listing for the component.
	owner := profile.Component
	if profile.Probe == validators.GatewayProbeGatewayAddress {
		owner = "namespace " + profile.Namespace
	}
	deployTitle := owner + " deployments"
	deployCmd := "kubectl get deploy -n " + profile.Namespace
	podTitle := owner + " pods"
	podCmd := "kubectl get pods -n " + profile.Namespace

	deploys, deployErr := ctx.Clientset.AppsV1().Deployments(profile.Namespace).List(
		ctx.Ctx, metav1.ListOptions{})
	if deployErr != nil {
		recordRawTextArtifact(ctx, deployTitle, deployCmd,
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
		recordRawTextArtifact(ctx, deployTitle, deployCmd, deploymentSummary.String())
	}

	pods, podErr := ctx.Clientset.CoreV1().Pods(profile.Namespace).List(ctx.Ctx, metav1.ListOptions{})
	if podErr != nil {
		recordRawTextArtifact(ctx, podTitle, podCmd,
			fmt.Sprintf("failed to list pods: %v", podErr))
		return
	}
	var podSummary strings.Builder
	for _, pod := range pods.Items {
		fmt.Fprintf(&podSummary, "%-48s ready=%s phase=%s node=%s\n",
			pod.Name, podReadyCount(pod), pod.Status.Phase, valueOrUnknown(pod.Spec.NodeName))
	}
	recordRawTextArtifact(ctx, podTitle, podCmd, podSummary.String())
}
