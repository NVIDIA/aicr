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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/validators"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Probe logs as gatewayTrafficProbeScript prints them for the verdicts the
// check must reach.
const (
	probeLogsRouted = "HEADER model-v1=0 model-v2=20 other=0\nWEIGHTED model-v1=352 model-v2=148 other=0\n"
	// probeLogsHeaderRuleRemoved: the header requests fall through to the
	// default split, so some reach the primary.
	probeLogsHeaderRuleRemoved = "HEADER model-v1=13 model-v2=7 other=0\nWEIGHTED model-v1=352 model-v2=148 other=0\n"
	// probeLogsWeightsSwapped: the default rule weights the alternate 70.
	probeLogsWeightsSwapped = "HEADER model-v1=0 model-v2=20 other=0\n" +
		"WEIGHTED model-v1=151 model-v2=349 other=0\nWEIGHTED model-v1=149 model-v2=351 other=0\n"
	// probeLogsForeignBackend: an existing route on the Gateway answers some
	// requests.
	probeLogsForeignBackend = "HEADER model-v1=0 model-v2=20 other=0\n" +
		"WEIGHTED model-v1=300 model-v2=120 other=80\nSAMPLE_ERROR hello from some-other-service\n"
	probeLogsNotReady = "NOT_READY after 120s header='error: <urlopen error timed out>' default='error: <urlopen error timed out>' seen=['error: <urlopen error timed out>']\n"
)

func TestParseGatewayProbeLogs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		logs        string
		wantHeader  gatewayTrafficCounts
		wantRounds  int
		wantSamples int
		wantErr     string
	}{
		{
			name: "routed", logs: probeLogsRouted,
			wantHeader: gatewayTrafficCounts{"model-v1": 0, "model-v2": 20, "other": 0}, wantRounds: 1,
		},
		{
			// The probe retries header rounds with errors; the last line is
			// the one that counts.
			name:       "last HEADER line wins, every WEIGHTED line kept",
			logs:       "HEADER model-v1=0 model-v2=18 other=2\nSAMPLE_ERROR error: boom\n" + probeLogsWeightsSwapped,
			wantHeader: gatewayTrafficCounts{"model-v1": 0, "model-v2": 20, "other": 0}, wantRounds: 2, wantSamples: 1,
		},
		{name: "no rounds", logs: probeLogsNotReady, wantErr: "no HEADER or no WEIGHTED round"},
		{name: "header only", logs: "HEADER model-v1=0 model-v2=20 other=0\n", wantErr: "no HEADER or no WEIGHTED round"},
		{name: "malformed count", logs: "HEADER model-v1=x model-v2=20 other=0\n", wantErr: "malformed probe line"},
		{name: "unknown backend key", logs: "HEADER model-v3=20\n", wantErr: "malformed probe line"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			header, rounds, samples, err := parseGatewayProbeLogs(tt.logs)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if header.String() != tt.wantHeader.String() {
				t.Errorf("header = %s, want %s", header, tt.wantHeader)
			}
			if len(rounds) != tt.wantRounds {
				t.Errorf("weighted rounds = %d, want %d", len(rounds), tt.wantRounds)
			}
			if len(samples) != tt.wantSamples {
				t.Errorf("sample errors = %d, want %d", len(samples), tt.wantSamples)
			}
		})
	}
}

func TestEvaluateGatewayHeaderRouting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		header  gatewayTrafficCounts
		wantErr string
	}{
		{name: "all reach the canary", header: gatewayTrafficCounts{"model-v2": 20}},
		{name: "header rule removed", header: gatewayTrafficCounts{"model-v1": 13, "model-v2": 7}, wantErr: "must reach model-v2, got model-v1=13 model-v2=7"},
		{name: "a request failed", header: gatewayTrafficCounts{"model-v2": 19, "other": 1}, wantErr: "must reach model-v2"},
		{name: "no header round", header: nil, wantErr: "no HEADER round"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := evaluateGatewayHeaderRouting(tt.header, 20)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
		})
	}
}

func TestEvaluateGatewayWeightedRounds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		rounds  []gatewayTrafficCounts
		wantErr string
	}{
		{name: "within tolerance", rounds: []gatewayTrafficCounts{{"model-v1": 352, "model-v2": 148}}},
		{name: "just inside tolerance", rounds: []gatewayTrafficCounts{{"model-v1": 374, "model-v2": 126}}},
		{name: "later round within tolerance", rounds: []gatewayTrafficCounts{{"model-v1": 300, "model-v2": 200}, {"model-v1": 340, "model-v2": 160}}},
		{
			name:    "weights swapped",
			rounds:  []gatewayTrafficCounts{{"model-v1": 151, "model-v2": 349}, {"model-v1": 149, "model-v2": 351}},
			wantErr: "round 1: model-v1 got 30.2%, want 70%",
		},
		{name: "outside tolerance", rounds: []gatewayTrafficCounts{{"model-v1": 380, "model-v2": 120}}, wantErr: "model-v1 got 76.0%, want 70%"},
		{name: "non-test backend answered", rounds: []gatewayTrafficCounts{{"model-v1": 300, "model-v2": 120, "other": 80}}, wantErr: "80 request(s) failed or reached a non-test backend"},
		{name: "nothing answered", rounds: []gatewayTrafficCounts{{}}, wantErr: "no request reached a test backend"},
		{name: "no rounds", rounds: nil, wantErr: "no WEIGHTED round"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := evaluateGatewayWeightedRounds(tt.rounds, 0.7, 0.05)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
		})
	}
}

// TestBuildGatewayTrafficHTTPRoute pins the route shape the verdicts depend
// on: a cross-namespace parentRef to the profile's Gateway, an exact header
// match to the alternate backend, and the weighted default rule.
func TestBuildGatewayTrafficHTTPRoute(t *testing.T) {
	t.Parallel()
	profile := agentgatewayProfile()
	const path = "/ai-conformance-abc"
	route := buildGatewayTrafficHTTPRoute("gateway-traffic-abc", profile, path, 70, 30)
	if route.GetName() != gatewayTrafficRouteName || route.GetNamespace() != "gateway-traffic-abc" || route.GetKind() != "HTTPRoute" {
		t.Fatalf("route identity = %s/%s %s", route.GetNamespace(), route.GetName(), route.GetKind())
	}
	parents, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	if len(parents) != 1 {
		t.Fatalf("parentRefs = %v, want one", parents)
	}
	parent := parents[0].(map[string]any)
	if parent["name"] != profile.GatewayName || parent["namespace"] != profile.Namespace {
		t.Errorf("parentRef = %v, want %s/%s", parent, profile.Namespace, profile.GatewayName)
	}
	rules, _, _ := unstructured.NestedSlice(route.Object, "spec", "rules")
	if len(rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(rules))
	}
	// Both rules match the per-run path, so a pre-existing route with a
	// shorter path on the same listener never outranks them.
	for i, r := range rules {
		matches, _, _ := unstructured.NestedSlice(r.(map[string]any), "matches")
		if len(matches) != 1 {
			t.Fatalf("rule %d matches = %v, want one", i, matches)
		}
		pathMatch, _, _ := unstructured.NestedMap(matches[0].(map[string]any), "path")
		if pathMatch["type"] != "PathPrefix" || pathMatch["value"] != path {
			t.Errorf("rule %d path match = %v, want PathPrefix %s", i, pathMatch, path)
		}
	}
	headerRule := rules[0].(map[string]any)
	matches, _, _ := unstructured.NestedSlice(headerRule, "matches")
	headers, _, _ := unstructured.NestedSlice(matches[0].(map[string]any), "headers")
	h := headers[0].(map[string]any)
	if h["type"] != "Exact" || h["name"] != gatewayTrafficHeaderName || h["value"] != gatewayTrafficHeaderValue {
		t.Errorf("header match = %v", h)
	}
	refs, _, _ := unstructured.NestedSlice(headerRule, "backendRefs")
	if len(refs) != 1 || refs[0].(map[string]any)["name"] != gatewayTrafficAlternate {
		t.Errorf("header rule backendRefs = %v, want only %s", refs, gatewayTrafficAlternate)
	}
	if _, weighted := refs[0].(map[string]any)["weight"]; weighted {
		t.Errorf("header rule backendRef carries a weight: %v", refs[0])
	}
	defaultRule := rules[1].(map[string]any)
	defaultMatches, _, _ := unstructured.NestedSlice(defaultRule, "matches")
	if _, hasHeaders := defaultMatches[0].(map[string]any)["headers"]; hasHeaders {
		t.Errorf("default rule carries a header match: %v", defaultRule)
	}
	refs, _, _ = unstructured.NestedSlice(defaultRule, "backendRefs")
	if len(refs) != 2 {
		t.Fatalf("default rule backendRefs = %v, want two", refs)
	}
	got := map[string]int64{}
	for _, r := range refs {
		m := r.(map[string]any)
		got[m["name"].(string)] = m["weight"].(int64)
		if m["port"] != int64(gatewayTrafficBackendPort) {
			t.Errorf("backendRef %v port, want %d", m, gatewayTrafficBackendPort)
		}
	}
	if got[gatewayTrafficPrimary] != 70 || got[gatewayTrafficAlternate] != 30 {
		t.Errorf("default rule weights = %v, want %s=70 %s=30", got, gatewayTrafficPrimary, gatewayTrafficAlternate)
	}
}

// TestGatewayTrafficPodsHonorTolerations pins that the context tolerations
// reach the backend pod template and the probe, and that nil means
// tolerate-all.
func TestGatewayTrafficPodsHonorTolerations(t *testing.T) {
	t.Parallel()
	custom := []corev1.Toleration{{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}}
	tolerateAll := []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
	for _, tt := range []struct {
		name string
		in   []corev1.Toleration
		want []corev1.Toleration
	}{
		{"custom", custom, custom},
		{"nil defaults to tolerate-all", nil, tolerateAll},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			deploy, svc := buildGatewayTrafficBackend("ns", gatewayTrafficPrimary, tt.in)
			if got := deploy.Spec.Template.Spec.Tolerations; fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("backend tolerations = %v, want %v", got, tt.want)
			}
			if svc.Spec.Selector[hpaAppLabelKey] != gatewayTrafficPrimary || deploy.Spec.Selector.MatchLabels[hpaAppLabelKey] != gatewayTrafficPrimary {
				t.Errorf("Service selector %v and Deployment selector %v must both select the backend", svc.Spec.Selector, deploy.Spec.Selector.MatchLabels)
			}
			s := &gatewayTrafficScenario{tolerations: tt.in, endpoint: "http://gw/v1/chat/completions", primaryWeight: 70, alternateWeight: 30,
				probeTimeout: time.Minute, routeReadyWindow: 30 * time.Second}
			pod := buildGatewayTrafficProbe("ns", s)
			if got := pod.Spec.Tolerations; fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("probe tolerations = %v, want %v", got, tt.want)
			}
			if pod.Spec.RestartPolicy != corev1.RestartPolicyNever || pod.Spec.ActiveDeadlineSeconds == nil {
				t.Errorf("probe must never restart and carry an activeDeadlineSeconds: %+v", pod.Spec)
			}
			env := map[string]string{}
			for _, e := range pod.Spec.Containers[0].Env {
				env[e.Name] = e.Value
			}
			if env["URL"] != s.endpoint || env["PRIMARY_SHARE"] != "0.7" || env["READY_SECONDS"] != "30" {
				t.Errorf("probe env = %v", env)
			}
		})
	}
}

// trafficTestGateway is the profile's Gateway with the listeners and
// addresses given in spec and status.
func trafficTestGateway(profile *validators.InferenceGatewayProfile, specListeners []any, addresses []any) *unstructured.Unstructured {
	gw := programmedGateway(profile, "True", []any{programmedListener("http", "True")}, addresses)
	gw.Object["spec"].(map[string]any)["listeners"] = specListeners
	return gw
}

func httpListener(port int64, hostname string) map[string]any {
	l := map[string]any{"name": "http", "protocol": "HTTP", "port": port}
	if hostname != "" {
		l["hostname"] = hostname
	}
	return l
}

func clusterIPService(ns, name string, ports ...int32) *corev1.Service {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	for _, p := range ports {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: fmt.Sprintf("p%d", p), Port: p})
	}
	return svc
}

func TestGatewayTrafficResolveEndpoint(t *testing.T) {
	t.Parallel()
	svcProfile := agentgatewayProfile()
	addrProfile := addressGatewayProfile()
	tests := []struct {
		name      string
		profile   *validators.InferenceGatewayProfile
		listeners []any
		addresses []any
		services  []runtime.Object
		wantURL   string
		wantPort  int64
		wantErr   string
		wantCode  errors.ErrorCode
	}{
		{
			name: "proxy Service named after the Gateway on the listener port", profile: svcProfile,
			listeners: []any{httpListener(80, "")},
			services:  []runtime.Object{clusterIPService(svcProfile.Namespace, "inference-gateway", 443, 80)},
			wantURL:   "http://inference-gateway.agentgateway-system.svc:80", wantPort: 80,
		},
		{
			name: "proxy Service with a suffix, first port when none matches the listener", profile: svcProfile,
			listeners: []any{httpListener(80, "")},
			services: []runtime.Object{
				clusterIPService(svcProfile.Namespace, "inference-gateway-controller-manager", 9443),
				clusterIPService(svcProfile.Namespace, "inference-gateway-proxy", 8080),
			},
			wantURL: "http://inference-gateway-proxy.agentgateway-system.svc:8080", wantPort: 80,
		},
		{
			name: "HTTPS listener skipped for the HTTP one", profile: svcProfile,
			listeners: []any{map[string]any{"name": "https", "protocol": "HTTPS", "port": int64(443)}, httpListener(8080, "")},
			services:  []runtime.Object{clusterIPService(svcProfile.Namespace, "inference-gateway", 8080)},
			wantURL:   "http://inference-gateway.agentgateway-system.svc:8080", wantPort: 8080,
		},
		{
			name: "proxy Service not found", profile: svcProfile,
			listeners: []any{httpListener(80, "")},
			services:  []runtime.Object{clusterIPService(svcProfile.Namespace, "inference-gateway-webhook", 9443)},
			wantErr:   "proxy Service the traffic probe would reach was not found", wantCode: errors.ErrCodeNotFound,
		},
		{
			name: "address-backed data plane uses the status address", profile: addrProfile,
			listeners: []any{httpListener(80, "")},
			addresses: []any{gatewayAddress("Hostname", "gw.example.internal")},
			wantURL:   "http://gw.example.internal:80", wantPort: 80,
		},
		{
			name: "IPv6 address is bracketed", profile: addrProfile,
			listeners: []any{httpListener(80, "")},
			addresses: []any{gatewayAddress("IPAddress", "fd00::10")},
			wantURL:   "http://[fd00::10]:80", wantPort: 80,
		},
		{
			name: "address-backed data plane with no reachable address", profile: addrProfile,
			listeners: []any{httpListener(80, "")},
			addresses: []any{gatewayAddress("NamedAddress", "pool-a")},
			wantErr:   "reports no IPAddress or Hostname status address", wantCode: errors.ErrCodeInternal,
		},
		{
			name: "hostname-bound HTTP listener is refused", profile: svcProfile,
			listeners: []any{httpListener(80, "models.example.com")},
			wantErr:   `HTTP listener(s) set a hostname (http=models.example.com)`, wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name: "no HTTP listener is refused", profile: svcProfile,
			listeners: []any{map[string]any{"name": "tls", "protocol": "TLS", "port": int64(443)}},
			wantErr:   "has no HTTP listener (listeners: tls=TLS)", wantCode: errors.ErrCodeInvalidRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := k8sfake.NewClientset(tt.services...)
			s, err := newGatewayTrafficScenario(c, nil, tt.profile, trafficTestGateway(tt.profile, tt.listeners, tt.addresses), nil)
			if err != nil {
				t.Fatalf("newGatewayTrafficScenario: %v", err)
			}
			err = s.resolveEndpoint(context.Background())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				errorstest.WantReportedCode(t, err, tt.wantCode)
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if s.routePath != gatewayTrafficPathPrefix+s.testRun.token {
				t.Errorf("routePath = %s, want the per-run prefix", s.routePath)
			}
			if want := tt.wantURL + s.routePath + gatewayTrafficRequestPath; s.endpoint != want || s.listenerPort != tt.wantPort {
				t.Errorf("endpoint = %s port %d, want %s port %d", s.endpoint, s.listenerPort, want, tt.wantPort)
			}
		})
	}
}

// TestGatewayTrafficResolveEndpoint_ServiceListDenied pins that a Service
// list the check cannot complete is a read failure, not a missing proxy.
func TestGatewayTrafficResolveEndpoint_ServiceListDenied(t *testing.T) {
	t.Parallel()
	profile := agentgatewayProfile()
	c := k8sfake.NewClientset()
	c.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "services"}, "", fmt.Errorf("denied"))
	})
	s, err := newGatewayTrafficScenario(c, nil, profile, trafficTestGateway(profile, []any{httpListener(80, "")}, nil), nil)
	if err != nil {
		t.Fatalf("newGatewayTrafficScenario: %v", err)
	}
	err = s.resolveEndpoint(context.Background())
	if err == nil || strings.Contains(err.Error(), "not found") {
		t.Fatalf("error = %v, want a read failure that is not reported as absence", err)
	}
}

// Fake controllers for the traffic scenario: the kubelet and the Deployment
// controller make every backend available on create; the gateway controller
// accepts the HTTPRoute on create; the probe pod reaches the given phase.
func availableOnCreate(c *k8sfake.Clientset) {
	c.PrependReactor("create", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		d := action.(k8stesting.CreateAction).GetObject().(*appsv1.Deployment)
		d.Status.AvailableReplicas = 1
		d.Status.ReadyReplicas = 1
		return false, nil, nil
	})
}

func probePhaseOnCreate(c *k8sfake.Clientset, phase corev1.PodPhase) {
	c.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		exit := int32(0)
		if phase == corev1.PodFailed {
			exit = 1
		}
		pod.Status = corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{{
			Name: gatewayTrafficProbeContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit}},
		}}}
		return false, nil, nil
	})
}

// routeStatus is a status.parents entry for the given Gateway.
func routeStatus(profile *validators.InferenceGatewayProfile, accepted, reason string) map[string]any {
	return map[string]any{
		"parentRef":      map[string]any{"name": profile.GatewayName, "namespace": profile.Namespace},
		"controllerName": "example.com/edge",
		"conditions": []any{
			map[string]any{"type": "Accepted", "status": accepted, "reason": reason},
			map[string]any{"type": "ResolvedRefs", "status": "True", "reason": "ResolvedRefs"},
		},
	}
}

func acceptRouteOnCreate(dyn *dynamicfake.FakeDynamicClient, parents ...map[string]any) {
	dyn.PrependReactor("create", "httproutes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		route := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		entries := make([]any, 0, len(parents))
		for _, p := range parents {
			entries = append(entries, p)
		}
		route.Object["status"] = map[string]any{"parents": entries}
		return false, nil, nil
	})
}

func newRouteFakeDynamicClient() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{httpRouteGVR: "HTTPRouteList"})
}

// TestGatewayTrafficScenarioRun drives the cycle against fake controllers
// and pins which step produces each verdict. The fail-closed cases are the
// acceptance of #3428: a route whose header rule is gone and one whose
// weights are swapped both fail, as does traffic reaching a non-test backend.
func TestGatewayTrafficScenarioRun(t *testing.T) {
	t.Parallel()
	profile := addressGatewayProfile()
	tests := []struct {
		name       string
		logs       string
		probePhase corev1.PodPhase
		noAccept   bool
		rejected   bool
		noBackend  bool
		// foreignRoute seeds a tenant HTTPRoute already attached to the
		// Gateway; a verdict failure must name it.
		foreignRoute bool
		wantPhase    gatewayTrafficPhase
		wantCode     errors.ErrorCode
		wantMsg      string
	}{
		{name: "routed traffic passes", logs: probeLogsRouted, probePhase: corev1.PodSucceeded, wantPhase: gatewayTrafficPhaseVerdict},
		{name: "header rule removed fails", logs: probeLogsHeaderRuleRemoved, probePhase: corev1.PodSucceeded, wantPhase: gatewayTrafficPhaseVerdict,
			wantCode: errors.ErrCodeInternal, wantMsg: "must reach model-v2, got model-v1=13 model-v2=7"},
		{name: "weights swapped fails", logs: probeLogsWeightsSwapped, probePhase: corev1.PodSucceeded, wantPhase: gatewayTrafficPhaseVerdict,
			wantCode: errors.ErrCodeInternal, wantMsg: "did not split 70/30 within 5 points in 2 round(s)"},
		{name: "non-test backend answered fails", logs: probeLogsForeignBackend, probePhase: corev1.PodSucceeded, wantPhase: gatewayTrafficPhaseVerdict,
			wantCode: errors.ErrCodeInternal, wantMsg: "80 request(s) failed or reached a non-test backend"},
		{name: "non-test backend answered names the attached routes", logs: probeLogsForeignBackend, probePhase: corev1.PodSucceeded, foreignRoute: true, wantPhase: gatewayTrafficPhaseVerdict,
			wantCode: errors.ErrCodeInternal, wantMsg: "HTTPRoutes already attached to Gateway edge-system/inference-gateway when the probe ran: [tenant/models]"},
		{name: "probe never saw the route live fails at probe", logs: probeLogsNotReady, probePhase: corev1.PodFailed, wantPhase: gatewayTrafficPhaseProbe,
			wantCode: errors.ErrCodeUnavailable, wantMsg: "did not reach the test backends as routed"},
		{name: "probe finished without counting fails at probe", logs: "", probePhase: corev1.PodSucceeded, wantPhase: gatewayTrafficPhaseProbe,
			wantCode: errors.ErrCodeInternal, wantMsg: "no HEADER or no WEIGHTED round"},
		{name: "route never accepted fails at route", logs: probeLogsRouted, probePhase: corev1.PodSucceeded, noAccept: true, wantPhase: gatewayTrafficPhaseRoute,
			wantCode: errors.ErrCodeTimeout, wantMsg: "has not reported status on the route"},
		{name: "route rejected by the listener fails at route", logs: probeLogsRouted, probePhase: corev1.PodSucceeded, rejected: true, wantPhase: gatewayTrafficPhaseRoute,
			wantCode: errors.ErrCodeTimeout, wantMsg: "reason=NotAllowedByListeners"},
		// waitForDeploymentAvailable's own verdict code is kept (not
		// double-wrapped): the bound elapsing on a 0/1 Deployment is Internal.
		{name: "backend never available fails at deploy", logs: probeLogsRouted, probePhase: corev1.PodSucceeded, noBackend: true, wantPhase: gatewayTrafficPhaseDeploy,
			wantCode: errors.ErrCodeInternal, wantMsg: "backend model-v1 did not become available"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := k8sfake.NewClientset()
			if !tt.noBackend {
				availableOnCreate(c)
			}
			probePhaseOnCreate(c, tt.probePhase)
			dyn := newRouteFakeDynamicClient()
			switch {
			case tt.noAccept:
			case tt.rejected:
				acceptRouteOnCreate(dyn, routeStatus(profile, "False", "NotAllowedByListeners"))
			default:
				acceptRouteOnCreate(dyn, routeStatus(profile, "True", "Accepted"))
			}
			if tt.foreignRoute {
				if _, seedErr := dyn.Resource(httpRouteGVR).Namespace("tenant").Create(context.Background(),
					httpRouteTo("tenant", "models", profile.Namespace), metav1.CreateOptions{}); seedErr != nil {
					t.Fatalf("seed tenant route: %v", seedErr)
				}
			}
			gw := trafficTestGateway(profile, []any{httpListener(80, "")}, []any{gatewayAddress("IPAddress", "172.16.100.12")})
			s, err := newGatewayTrafficScenario(c, dyn, profile, gw, nil)
			if err != nil {
				t.Fatalf("newGatewayTrafficScenario: %v", err)
			}
			s.readProbeLogs = func(context.Context) (string, error) { return tt.logs, nil }
			s.backendReadyTimeout = 3 * defaults.PodPollInterval
			s.routeAcceptTimeout = 3 * defaults.PodPollInterval
			s.probeTimeout = time.Second
			if resolveErr := s.resolveEndpoint(context.Background()); resolveErr != nil {
				t.Fatalf("resolveEndpoint: %v", resolveErr)
			}

			ctx := &validators.Context{Ctx: context.Background(), Clientset: c, DynamicClient: dyn}
			err = s.runWithCleanup(ctx)
			if s.phase != tt.wantPhase {
				t.Errorf("verdict step = %s, want %s (err=%v)", s.phase, tt.wantPhase, err)
			}
			if tt.wantCode == "" {
				if err != nil {
					t.Fatalf("want pass, got %v", err)
				}
				if s.header.String() != "model-v1=0 model-v2=20 other=0" || len(s.weighted) != 1 {
					t.Errorf("counts not recorded: header=%v weighted=%v", s.header, s.weighted)
				}
			} else {
				errorstest.WantReportedCode(t, err, tt.wantCode)
				if !strings.Contains(err.Error(), tt.wantMsg) || !strings.Contains(err.Error(), "step "+string(tt.wantPhase)) {
					t.Errorf("error %q lacks %q or the step name", err.Error(), tt.wantMsg)
				}
			}
			if _, getErr := c.CoreV1().Namespaces().Get(context.Background(), s.testRun.namespace, metav1.GetOptions{}); !k8serrors.IsNotFound(getErr) {
				t.Errorf("namespace %s not deleted after the run: %v", s.testRun.namespace, getErr)
			}
			if _, getErr := dyn.Resource(httpRouteGVR).Namespace(s.testRun.namespace).Get(context.Background(), gatewayTrafficRouteName, metav1.GetOptions{}); !k8serrors.IsNotFound(getErr) {
				t.Errorf("HTTPRoute not deleted after the run: %v", getErr)
			}
		})
	}
}

// TestGatewayTrafficVerdictSummaryCarriesCounts pins that the counts the
// evidence page needs are in the verdict artifact on pass and on failure.
func TestGatewayTrafficVerdictSummaryCarriesCounts(t *testing.T) {
	t.Parallel()
	profile := agentgatewayProfile()
	s, err := newGatewayTrafficScenario(k8sfake.NewClientset(), nil, profile, trafficTestGateway(profile, nil, nil), nil)
	if err != nil {
		t.Fatalf("newGatewayTrafficScenario: %v", err)
	}
	s.endpoint, s.listenerPort = "http://inference-gateway.agentgateway-system.svc:80/v1/chat/completions", 80
	s.header, s.weighted, _, err = parseGatewayProbeLogs(probeLogsForeignBackend)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s.sampleErrors = []string{"hello from some-other-service"}
	s.phase = gatewayTrafficPhaseVerdict
	out := s.verdictSummary(fmt.Errorf("boom"))
	for _, want := range []string{
		"Verdict:            FAIL in step verdict",
		"Header requests:    model-v1=0 model-v2=20 other=0 (want model-v2=20)",
		"Weighted round 1    model-v1=300 model-v2=120 other=80 (model-v1 share 71.4%, want 70%)",
		"Sample error:       hello from some-other-service",
		"Default rule:       model-v1 70 / model-v2 30 (tolerance 5 points)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("verdict summary lacks %q:\n%s", want, out)
		}
	}
	s.header, s.weighted, s.sampleErrors = nil, nil, nil
	out = s.verdictSummary(fmt.Errorf("boom"))
	if !strings.Contains(out, "Header requests:    not measured") || !strings.Contains(out, "Weighted rounds:    not measured") {
		t.Errorf("verdict summary must say the counts were not measured:\n%s", out)
	}
}

// TestWaitForHTTPRouteAccepted_Watch pins that an acceptance the controller
// writes after the first read is seen through the watch, and that a parent
// entry for another Gateway of the same name does not count.
func TestWaitForHTTPRouteAccepted_Watch(t *testing.T) {
	t.Parallel()
	profile := agentgatewayProfile()
	dyn := newRouteFakeDynamicClient()
	route := buildGatewayTrafficHTTPRoute("gateway-traffic-x", profile, "/ai-conformance-x", 70, 30)
	// Accepted by a same-named Gateway in the route's own namespace: not ours.
	foreign := routeStatus(profile, "True", "Accepted")
	foreign["parentRef"] = map[string]any{"name": profile.GatewayName}
	route.Object["status"] = map[string]any{"parents": []any{foreign}}
	if _, err := dyn.Resource(httpRouteGVR).Namespace("gateway-traffic-x").Create(context.Background(), route, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed route: %v", err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		accepted := route.DeepCopy()
		accepted.Object["status"] = map[string]any{"parents": []any{foreign, routeStatus(profile, "True", "Accepted")}}
		if _, err := dyn.Resource(httpRouteGVR).Namespace("gateway-traffic-x").Update(context.Background(), accepted, metav1.UpdateOptions{}); err != nil {
			t.Errorf("update route: %v", err)
		}
	}()
	got, err := waitForHTTPRouteAccepted(context.Background(), dyn, "gateway-traffic-x", gatewayTrafficRouteName, profile, 5*time.Second)
	if err != nil {
		t.Fatalf("waitForHTTPRouteAccepted: %v", err)
	}
	if len(httpRouteParentsFor(got, profile)) != 1 {
		t.Errorf("parents for the profile's Gateway = %d, want 1", len(httpRouteParentsFor(got, profile)))
	}
	text := httpRouteParentStatusText(got, profile)
	if !strings.Contains(text, "Accepted      True") || !strings.Contains(text, "ResolvedRefs  True") {
		t.Errorf("status text = %q", text)
	}
}

// TestWaitForHTTPRouteAccepted_ReadDenied pins that a denied read ends the
// wait as a read failure rather than a timeout.
func TestWaitForHTTPRouteAccepted_ReadDenied(t *testing.T) {
	t.Parallel()
	dyn := newRouteFakeDynamicClient()
	dyn.PrependReactor("get", "httproutes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewForbidden(schema.GroupResource{Resource: "httproutes"}, "", fmt.Errorf("denied"))
	})
	_, err := waitForHTTPRouteAccepted(context.Background(), dyn, "ns", gatewayTrafficRouteName, agentgatewayProfile(), time.Second)
	if err == nil || strings.Contains(err.Error(), "not Accepted") {
		t.Fatalf("error = %v, want the denied read, not a timeout", err)
	}
}

// TestGatewayTrafficRunWithCleanup_NamespaceCreateFailure pins that the
// traffic phase's teardown runs even when createGPUTestNamespace fails and
// never starts the cycle on that path; the cases are shared with the HPA and
// AI service metrics checks. Sequential: captureStdout swaps os.Stdout.
func TestGatewayTrafficRunWithCleanup_NamespaceCreateFailure(t *testing.T) {
	shortenCleanupTimeout(t)
	profile := agentgatewayProfile()
	for _, tt := range namespaceCreateFailureCases {
		t.Run(tt.name, func(t *testing.T) {
			c := k8sfake.NewClientset()
			failNamespaceCreate(c, tt.persists, tt.surfacesLate, tt.createErr)
			if tt.deleteErr != nil {
				failNamespaceDelete(c, tt.deleteErr)
			}
			s, err := newGatewayTrafficScenario(c, newRouteFakeDynamicClient(), profile, trafficTestGateway(profile, nil, nil), nil)
			if err != nil {
				t.Fatalf("newGatewayTrafficScenario: %v", err)
			}
			out := captureStdout(t, func() {
				err = s.runWithCleanup(&validators.Context{Ctx: context.Background(), Clientset: c})
			})
			assertNamespaceCreateFailureCleanup(t, c, s.testRun.namespace, out, err, tt.persists || tt.surfacesLate, tt.deleteErr, tt.wantDeleted, tt.wantStatus)
			if s.phase != "" {
				t.Errorf("phase = %q, want none — the cycle must not start without a namespace", s.phase)
			}
		})
	}
}

// TestRunGatewayTrafficPhase_FailsBeforeCreatingOnBadGateway pins that a
// Gateway the probe cannot target (no HTTP listener) fails before any
// resource is created.
func TestRunGatewayTrafficPhase_FailsBeforeCreatingOnBadGateway(t *testing.T) {
	t.Parallel()
	profile := addressGatewayProfile()
	c := k8sfake.NewClientset()
	gw := trafficTestGateway(profile, []any{map[string]any{"name": "tls", "protocol": "TLS", "port": int64(443)}}, nil)
	ctx := &validators.Context{Ctx: context.Background(), Clientset: c, DynamicClient: newRouteFakeDynamicClient()}
	err := runGatewayTrafficPhase(ctx, profile, gw)
	if err == nil || !strings.Contains(err.Error(), "has no HTTP listener") {
		t.Fatalf("error = %v, want the listener finding", err)
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	for _, action := range c.Actions() {
		if action.GetVerb() == "create" {
			t.Errorf("resource created before the endpoint resolved: %s %s", action.GetVerb(), action.GetResource().Resource)
		}
	}
}

// TestRunGatewayTrafficPhase_TightDeadline pins the budget guard: a check
// deadline that cannot fit the cleanup reserve fails before any create.
func TestRunGatewayTrafficPhase_TightDeadline(t *testing.T) {
	t.Parallel()
	profile := addressGatewayProfile()
	c := k8sfake.NewClientset()
	tight, cancel := context.WithTimeout(context.Background(), gpuCheckMinWorkBudget)
	defer cancel()
	ctx := &validators.Context{Ctx: tight, Clientset: c, DynamicClient: newRouteFakeDynamicClient()}
	err := runGatewayTrafficPhase(ctx, profile, trafficTestGateway(profile, []any{httpListener(80, "")}, []any{gatewayAddress("IPAddress", "10.0.0.1")}))
	errorstest.WantReportedCode(t, err, errors.ErrCodeTimeout)
	if len(c.Actions()) != 0 {
		t.Errorf("actions = %v, want none before the budget guard", c.Actions())
	}
}
