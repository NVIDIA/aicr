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

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

var (
	gatewayClassGVR = schema.GroupVersionResource{Group: apiGroupGateway, Version: "v1", Resource: "gatewayclasses"}
	crdGVR          = schema.GroupVersionResource{Group: apiGroupAPIExtensions, Version: "v1", Resource: resourceCRDs}
)

func lbService(name string, sourceRanges []string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "agentgateway-system"},
		Spec: corev1.ServiceSpec{
			Type:                     corev1.ServiceTypeLoadBalancer,
			LoadBalancerSourceRanges: sourceRanges,
		},
	}
}

func endpointSlice(ns, name, serviceName string, ready bool) *discoveryv1.EndpointSlice {
	return &discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"kubernetes.io/service-name": serviceName}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(ready)}}},
	}
}

func agentgatewayProfile() *validators.InferenceGatewayProfile {
	p := validators.DefaultInferenceGatewayProfile()
	return &p
}

// addressGatewayProfile is a gateway whose data plane has no Service: the
// address lives in the Gateway object (a hardware or DPU-hosted proxy).
func addressGatewayProfile() *validators.InferenceGatewayProfile {
	return &validators.InferenceGatewayProfile{
		Component: "edge-gateway", GatewayClass: "edge-class", GatewayName: "inference-gateway",
		Namespace: "edge-system", Probe: validators.GatewayProbeGatewayAddress,
	}
}

// newGatewayFakeDynamicClient builds a fake dynamic client that knows the
// Gateway API list kinds the inference-gateway check reads, seeded with
// Gateways. The objects go in through the tracker under the real resource
// name: the fake's constructor guesses the resource from the kind and
// pluralises "Gateway" as "gatewaies", so a constructor-seeded Gateway is
// never found by a Get on "gateways".
func newGatewayFakeDynamicClient(t *testing.T, gateways ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gatewayGVR: "GatewayList", httpRouteGVR: "HTTPRouteList"})
	for _, gw := range gateways {
		if gw == nil {
			continue
		}
		if err := dyn.Tracker().Create(gatewayGVR, gw, gw.GetNamespace()); err != nil {
			t.Fatalf("seed Gateway %s/%s: %v", gw.GetNamespace(), gw.GetName(), err)
		}
	}
	return dyn
}

func gatewayObject(profile *validators.InferenceGatewayProfile, listeners, addresses []any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "Gateway",
		"metadata":   map[string]any{"name": profile.GatewayName, "namespace": profile.Namespace},
		"spec":       map[string]any{"gatewayClassName": profile.GatewayClass},
		"status":     map[string]any{"listeners": listeners, "addresses": addresses},
	}}
}

// withGatewayClass returns gw programmed by another class than its profile declares.
func withGatewayClass(gw *unstructured.Unstructured, class string) *unstructured.Unstructured {
	gw.Object["spec"].(map[string]any)["gatewayClassName"] = class
	return gw
}

// programmedGateway is gatewayObject with the Gateway-level Programmed
// condition CheckInferenceGateway requires.
func programmedGateway(profile *validators.InferenceGatewayProfile, status string, listeners, addresses []any) *unstructured.Unstructured {
	gw := gatewayObject(profile, listeners, addresses)
	gw.Object["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Programmed", "status": status, "reason": "Programmed"}}
	return gw
}

// testGatewayClass is the GatewayClass the end-to-end cases declare.
const testGatewayClass = "edge-class"

func gatewayClassObject(accepted string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "GatewayClass",
		"metadata":   map[string]any{"name": testGatewayClass},
		"spec":       map[string]any{"controllerName": "example.com/edge"},
		"status":     map[string]any{"conditions": []any{map[string]any{"type": "Accepted", "status": accepted, "reason": "Accepted"}}},
	}}
}

func crdObject(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": name},
	}}
}

func programmedListener(name string, status string) map[string]any {
	return map[string]any{
		"name":           name,
		"attachedRoutes": int64(1),
		"conditions":     []any{map[string]any{"type": "Programmed", "status": status, "reason": "Programmed"}},
	}
}

func gatewayAddress(typ, value string) map[string]any {
	return map[string]any{"type": typ, "value": value}
}

func setEnforce(t *testing.T, on bool) {
	t.Helper()
	if on {
		t.Setenv(requireScopedGatewayEnv, "true")
	} else {
		t.Setenv(requireScopedGatewayEnv, "")
	}
}

func TestAssessGatewayExposure(t *testing.T) {
	tests := []struct {
		name          string
		objects       []*corev1.Service
		requireScoped bool
		wantErr       bool
	}{
		{
			name:    "open LoadBalancer, default policy → warn (pass)",
			objects: []*corev1.Service{lbService("inference-gateway", nil)},
			wantErr: false,
		},
		{
			name:          "open LoadBalancer, enforce policy → fail",
			objects:       []*corev1.Service{lbService("inference-gateway", nil)},
			requireScoped: true,
			wantErr:       true,
		},
		{
			name:          "scoped LoadBalancer, enforce policy → pass",
			objects:       []*corev1.Service{lbService("inference-gateway", []string{"10.0.0.0/8"})},
			requireScoped: true,
			wantErr:       false,
		},
		{
			name:          "explicit 0.0.0.0/0, enforce policy → fail",
			objects:       []*corev1.Service{lbService("inference-gateway", []string{"0.0.0.0/0"})},
			requireScoped: true,
			wantErr:       true,
		},
		{
			name:          "explicit ::/0 (IPv6 any), enforce policy → fail",
			objects:       []*corev1.Service{lbService("inference-gateway", []string{"::/0"})},
			requireScoped: true,
			wantErr:       true,
		},
		{
			name:          "any-source CIDR mixed with a scoped range, enforce policy → fail",
			objects:       []*corev1.Service{lbService("inference-gateway", []string{"10.0.0.0/8", "0.0.0.0/0"})},
			requireScoped: true,
			wantErr:       true,
		},
		{
			name:          "unrelated open LoadBalancer is ignored, enforce policy → pass",
			objects:       []*corev1.Service{lbService("agentgateway-controller-manager", nil)},
			requireScoped: true,
			wantErr:       false,
		},
		{
			// Shares the "inference-gateway" prefix but is a control-plane
			// component, not the data-plane proxy: must not fail enforce mode.
			name:          "inference-gateway control-plane LB is ignored, enforce policy → pass",
			objects:       []*corev1.Service{lbService("inference-gateway-controller-manager", nil)},
			requireScoped: true,
			wantErr:       false,
		},
		{
			name: "non-LoadBalancer service is ignored",
			objects: []*corev1.Service{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "inference-gateway", Namespace: "agentgateway-system"},
					Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeClusterIP},
				},
			},
			requireScoped: true,
			wantErr:       false,
		},
		{
			name:          "no services → pass",
			objects:       nil,
			requireScoped: true,
			wantErr:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := make([]runtime.Object, 0, len(tt.objects))
			for _, o := range tt.objects {
				objs = append(objs, o)
			}
			vctx := &validators.Context{
				Ctx:       context.Background(),
				Clientset: fake.NewClientset(objs...),
			}
			setEnforce(t, tt.requireScoped)

			err := assessGatewayExposure(vctx, agentgatewayProfile(), nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("assessGatewayExposure() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !strings.Contains(err.Error(), "0.0.0.0/0") {
					t.Errorf("error %q should describe the open exposure", err.Error())
				}
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			}
		})
	}
}

// TestAssessGatewayExposure_RenamedGateway verifies the proxy predicate follows
// the profile's Gateway name, so a recipe that renames the Gateway has that
// proxy Service assessed, and a Service bearing the conventional name is
// ignored.
func TestAssessGatewayExposure_RenamedGateway(t *testing.T) {
	profile := agentgatewayProfile()
	profile.GatewayName = "edge-gateway"
	svc := lbService("edge-gateway", nil)
	ignored := lbService("inference-gateway", nil)
	vctx := &validators.Context{Ctx: context.Background(), Clientset: fake.NewClientset(svc, ignored)}
	setEnforce(t, true)
	err := assessGatewayExposure(vctx, profile, nil)
	if err == nil || !strings.Contains(err.Error(), "[edge-gateway]") {
		t.Fatalf("expected the renamed proxy Service to be the open one, got %v", err)
	}
}

func TestAssessGatewayExposure_NilClientset(t *testing.T) {
	err := assessGatewayExposure(&validators.Context{Ctx: context.Background()}, agentgatewayProfile(), nil)
	if err == nil {
		t.Fatal("expected error when clientset is nil")
	}
}

// TestAssessGatewayExposure_ListError verifies that a transient apiserver error
// listing Services does not fail the (already-passed) inference-gateway check in
// the default advisory mode, but does fail closed under enforce mode. See #1160.
func TestAssessGatewayExposure_ListError(t *testing.T) {
	tests := []struct {
		name          string
		requireScoped bool
		wantErr       bool
	}{
		{name: "default advisory mode → transient list error is non-fatal", requireScoped: false, wantErr: false},
		{name: "enforce mode → transient list error fails closed", requireScoped: true, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset()
			client.PrependReactor("list", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, fmt.Errorf("apiserver unavailable")
			})
			setEnforce(t, tt.requireScoped)
			err := assessGatewayExposure(&validators.Context{Ctx: context.Background(), Clientset: client}, agentgatewayProfile(), nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("assessGatewayExposure() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
			}
		})
	}
}

func TestAssessGatewayAddressExposure(t *testing.T) {
	profile := addressGatewayProfile()
	tests := []struct {
		name          string
		gateway       *unstructured.Unstructured
		requireScoped bool
		wantErr       string
		wantCode      errors.ErrorCode
		wantAbsent    string
	}{
		{
			name:          "private address, enforce → pass",
			gateway:       gatewayObject(profile, nil, []any{gatewayAddress("IPAddress", "172.16.100.12")}),
			requireScoped: true,
		},
		{
			name:          "ULA address, enforce → pass",
			gateway:       gatewayObject(profile, nil, []any{gatewayAddress("IPAddress", "fd00:10::12")}),
			requireScoped: true,
		},
		{
			name:          "shared address space (RFC 6598), enforce → pass",
			gateway:       gatewayObject(profile, nil, []any{gatewayAddress("IPAddress", "100.64.3.7")}),
			requireScoped: true,
		},
		{
			name:    "public address, default policy → warn (pass)",
			gateway: gatewayObject(profile, nil, []any{gatewayAddress("IPAddress", "203.0.113.10")}),
		},
		{
			name:          "public address, enforce → fail",
			gateway:       gatewayObject(profile, nil, []any{gatewayAddress("IPAddress", "203.0.113.10")}),
			requireScoped: true,
			wantErr:       "public address(es) [203.0.113.10]",
			wantCode:      errors.ErrCodeInvalidRequest,
		},
		{
			name: "one public among private, enforce → fail names only the public one",
			gateway: gatewayObject(profile, nil, []any{
				gatewayAddress("IPAddress", "10.1.2.3"), gatewayAddress("IPAddress", "198.51.100.7"),
			}),
			requireScoped: true,
			wantErr:       "[198.51.100.7]",
			wantCode:      errors.ErrCodeInvalidRequest,
			wantAbsent:    "10.1.2.3",
		},
		{
			name:    "hostname address, default policy → warn (pass)",
			gateway: gatewayObject(profile, nil, []any{gatewayAddress("Hostname", "a1b2.elb.example.com")}),
		},
		{
			// A hostname the check cannot classify is not confirmed scoped,
			// so enforce mode fails closed rather than passing an ELB DNS name.
			name:          "hostname address, enforce → fail closed",
			gateway:       gatewayObject(profile, nil, []any{gatewayAddress("Hostname", "a1b2.elb.example.com")}),
			requireScoped: true,
			wantErr:       "cannot classify",
			wantCode:      errors.ErrCodeInvalidRequest,
		},
		{
			// Bound on every interface is the most exposed shape, not the
			// least; the check cannot confirm it scoped, so enforce fails.
			name:          "unspecified address, enforce → fail closed",
			gateway:       gatewayObject(profile, nil, []any{gatewayAddress("IPAddress", "0.0.0.0")}),
			requireScoped: true,
			wantErr:       "cannot classify",
			wantCode:      errors.ErrCodeInvalidRequest,
		},
		{
			name:    "no addresses, default policy → pass (nothing to assess)",
			gateway: gatewayObject(profile, nil, nil),
		},
		{
			// Nothing classified means nothing confirmed scoped.
			name:          "no addresses, enforce → fail closed",
			gateway:       gatewayObject(profile, nil, nil),
			requireScoped: true,
			wantErr:       "no status.addresses to assess",
			wantCode:      errors.ErrCodeInternal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnforce(t, tt.requireScoped)
			vctx := &validators.Context{Ctx: context.Background(), Clientset: fake.NewClientset()}
			err := assessGatewayExposure(vctx, profile, tt.gateway)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("assessGatewayExposure() unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("assessGatewayExposure() error = %v, want containing %q", err, tt.wantErr)
			}
			if tt.wantAbsent != "" && strings.Contains(err.Error(), tt.wantAbsent) {
				t.Errorf("error %q names a private address %q", err.Error(), tt.wantAbsent)
			}
			errorstest.WantReportedCode(t, err, tt.wantCode)
		})
	}
}

// TestAssessGatewayExposure_ProbeDispatch pins that the probe alone decides
// which facts are read: a Service-backed profile ignores Gateway addresses, an
// address-backed profile ignores LoadBalancer Services in its namespace.
func TestAssessGatewayExposure_ProbeDispatch(t *testing.T) {
	setEnforce(t, true)
	public := gatewayObject(agentgatewayProfile(), nil, []any{gatewayAddress("IPAddress", "203.0.113.10")})
	if err := assessGatewayExposure(&validators.Context{Ctx: context.Background(), Clientset: fake.NewClientset()}, agentgatewayProfile(), public); err != nil {
		t.Errorf("service probe must not read Gateway addresses: %v", err)
	}
	addr := addressGatewayProfile()
	openLB := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: addr.GatewayName, Namespace: addr.Namespace},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeLoadBalancer},
	}
	private := gatewayObject(addr, nil, []any{gatewayAddress("IPAddress", "10.0.0.5")})
	if err := assessGatewayExposure(&validators.Context{Ctx: context.Background(), Clientset: fake.NewClientset(openLB)}, addr, private); err != nil {
		t.Errorf("address probe must not read Services: %v", err)
	}
}

// httpRouteTo is an HTTPRoute in namespace attached to the conventionally
// named Gateway; parentNamespace empty leaves the namespace off the
// parentRef, which the Gateway API reads as the route's own.
func httpRouteTo(namespace, name, parentNamespace string) *unstructured.Unstructured {
	ref := map[string]any{"name": validators.DefaultInferenceGatewayName}
	if parentNamespace != "" {
		ref["namespace"] = parentNamespace
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "HTTPRoute",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec":       map[string]any{"parentRefs": []any{ref}},
	}}
}

func TestValidateGatewayAddressDataPlane(t *testing.T) {
	profile := addressGatewayProfile()
	ok := []any{programmedListener("http", "True")}
	addr := []any{gatewayAddress("IPAddress", "172.16.100.12")}
	tests := []struct {
		name      string
		listeners []any
		addresses []any
		wantErr   string
	}{
		{name: "programmed listener with an address", listeners: ok, addresses: addr},
		{
			name:      "listener without conditions is tolerated",
			listeners: []any{map[string]any{"name": "http", "attachedRoutes": int64(0)}},
			addresses: addr,
		},
		{
			// Unknown is a controller still reconciling; the check runs once,
			// so only an explicit False fails it.
			name:      "listener with Programmed=Unknown is tolerated",
			listeners: []any{programmedListener("http", "True"), programmedListener("https", "Unknown")},
			addresses: addr,
		},
		{name: "no listeners", addresses: addr, wantErr: "no listeners"},
		{name: "no addresses", listeners: ok, wantErr: "no status.addresses"},
		{
			name:      "listener explicitly not programmed",
			listeners: []any{programmedListener("http", "True"), programmedListener("https", "False")},
			addresses: addr,
			wantErr:   `listener "https" is not programmed`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGatewayAddressDataPlane(gatewayObject(profile, tt.listeners, tt.addresses), profile)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestValidateGatewayDataPlane runs the whole data-plane step for both probes
// against fake clusters: the address probe passes on the Gateway object alone,
// while the Service probe needs ready EndpointSlices behind the proxy Service,
// chosen by the profile's predicate.
func TestValidateGatewayDataPlane(t *testing.T) {
	addr := addressGatewayProfile()
	svc := agentgatewayProfile()
	renamed := agentgatewayProfile()
	renamed.GatewayName = "edge-gateway"
	addrGateway := gatewayObject(addr, []any{programmedListener("http", "True")}, []any{gatewayAddress("IPAddress", "172.16.100.12")})
	svcGateway := gatewayObject(svc, []any{programmedListener("http", "True")}, nil)
	ns := svc.Namespace

	tests := []struct {
		name           string
		profile        *validators.InferenceGatewayProfile
		gateway        *unstructured.Unstructured
		objects        []runtime.Object
		routes         []*unstructured.Unstructured
		listReactor    bool
		nilClientset   bool
		wantErr        string
		wantCode       errors.ErrorCode
		wantReady      int
		wantMatching   int
		wantAddresses  int
		wantProgrammed int
		wantAttached   int
	}{
		{
			name: "address probe passes on a Service-less cluster", profile: addr, gateway: addrGateway,
			wantAddresses: 1, wantProgrammed: 1,
		},
		{
			// The address probe reads the Gateway object alone; only the
			// Service probe lists EndpointSlices through the typed client.
			name: "address probe needs no typed client", profile: addr, gateway: addrGateway, nilClientset: true,
			wantAddresses: 1, wantProgrammed: 1,
		},
		{
			// Routes attach by Gateway name and namespace; a same-named
			// Gateway elsewhere must not inflate the count.
			name: "attached routes count only this Gateway's namespace", profile: svc, gateway: svcGateway,
			objects: []runtime.Object{endpointSlice(ns, "inference-gateway-abc", "inference-gateway", true)},
			routes: []*unstructured.Unstructured{
				httpRouteTo("aicr-inference-perf-1", "cross-namespace", ns),
				httpRouteTo(ns, "same-namespace", ""),
				httpRouteTo("other", "other-gateway-explicit", "other"),
				httpRouteTo("other", "other-gateway-implicit", ""),
			},
			wantReady: 1, wantMatching: 1, wantProgrammed: 1, wantAttached: 2,
		},
		{
			name: "service probe on the same Service-less cluster fails as proxy not found", profile: &validators.InferenceGatewayProfile{
				Component: addr.Component, GatewayClass: addr.GatewayClass, GatewayName: addr.GatewayName,
				Namespace: addr.Namespace, Probe: validators.GatewayProbeServiceEndpoints,
			}, gateway: addrGateway,
			wantErr: "proxy Service was not found", wantCode: errors.ErrCodeNotFound,
		},
		{
			// The report carries listener and address counts for the Service
			// probe too; a programmed listener must not read as zero here.
			name: "ready proxy EndpointSlice", profile: svc, gateway: svcGateway,
			objects:   []runtime.Object{endpointSlice(ns, "inference-gateway-abc", "inference-gateway", true)},
			wantReady: 1, wantMatching: 1, wantProgrammed: 1,
		},
		{
			name: "only the control-plane slice is present", profile: svc, gateway: svcGateway,
			objects: []runtime.Object{endpointSlice(ns, "cm-abc", "inference-gateway-controller-manager", true)},
			wantErr: "proxy Service was not found", wantCode: errors.ErrCodeNotFound,
		},
		{
			name: "matching slice with no ready endpoint", profile: svc, gateway: svcGateway,
			objects: []runtime.Object{endpointSlice(ns, "inference-gateway-abc", "inference-gateway", false)},
			wantErr: "none has a ready endpoint", wantCode: errors.ErrCodeInternal,
		},
		{
			name: "renamed gateway matches its own proxy", profile: renamed, gateway: gatewayObject(renamed, nil, nil),
			objects:   []runtime.Object{endpointSlice(ns, "edge-abc", "edge-gateway-proxy", true), endpointSlice(ns, "ig-abc", "inference-gateway", true)},
			wantReady: 1, wantMatching: 1,
		},
		{
			name: "renamed gateway ignores the conventional proxy", profile: renamed, gateway: gatewayObject(renamed, nil, nil),
			objects: []runtime.Object{endpointSlice(ns, "ig-abc", "inference-gateway", true)},
			wantErr: "proxy Service was not found", wantCode: errors.ErrCodeNotFound,
		},
		{
			name: "EndpointSlice list error", profile: svc, gateway: svcGateway, listReactor: true,
			wantErr: "failed to list EndpointSlices", wantCode: errors.ErrCodeInternal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewClientset(tt.objects...)
			if tt.listReactor {
				client.PrependReactor("list", "endpointslices", func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, fmt.Errorf("apiserver unavailable")
				})
			}
			dyn := newGatewayFakeDynamicClient(t, tt.gateway)
			for _, route := range tt.routes {
				if err := dyn.Tracker().Create(httpRouteGVR, route, route.GetNamespace()); err != nil {
					t.Fatalf("seed HTTPRoute: %v", err)
				}
			}
			vctx := &validators.Context{Ctx: context.Background(), Clientset: client, DynamicClient: dyn}
			if tt.nilClientset {
				vctx.Clientset = nil
			}
			report, err := validateGatewayDataPlane(vctx, tt.profile, tt.gateway)
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
			mismatch := report.ReadyEndpoints != tt.wantReady || report.MatchingEndpointSlice != tt.wantMatching ||
				report.StatusAddresses != tt.wantAddresses || report.ProgrammedListeners != tt.wantProgrammed ||
				report.AttachedHTTPRoutes != tt.wantAttached
			if mismatch {
				t.Errorf("report = %+v, want ready=%d matching=%d addresses=%d programmed=%d attached=%d",
					*report, tt.wantReady, tt.wantMatching, tt.wantAddresses, tt.wantProgrammed, tt.wantAttached)
			}
		})
	}
}

// TestCheckInferenceGateway runs the whole check against fake clients, from
// profile resolution through the exposure step, with the traffic phase
// stubbed: it must run only once every status check has passed, on the
// Gateway those checks read, and its verdict is the check's.
func TestCheckInferenceGateway(t *testing.T) {
	const (
		class   = testGatewayClass
		ns      = "edge-system"
		gateway = "inference-gateway"
	)
	allCRDs := []string{
		"gateways.gateway.networking.k8s.io",
		"httproutes.gateway.networking.k8s.io",
		"inferencepools.inference.networking.k8s.io",
	}
	declare := func(dataPlane string) recipe.ComponentRef {
		fields := map[string]any{"gatewayClass": class}
		if dataPlane != "" {
			fields["dataPlane"] = dataPlane
		}
		return recipe.ComponentRef{Name: "edge", Namespace: ns, Overrides: map[string]any{recipe.InferenceGatewayOverrideKey: fields}}
	}
	profile := &validators.InferenceGatewayProfile{Component: "edge", GatewayClass: class, GatewayName: gateway, Namespace: ns}
	programmed := func(status string) *unstructured.Unstructured {
		return programmedGateway(profile, status, []any{programmedListener("http", "True")}, []any{gatewayAddress("IPAddress", "172.16.100.12")})
	}

	tests := []struct {
		name         string
		refs         []recipe.ComponentRef
		gatewayClass *unstructured.Unstructured
		gateway      *unstructured.Unstructured
		crds         []string
		trafficErr   error
		wantSkip     bool
		wantErr      string
		wantCode     errors.ErrorCode
	}{
		{
			name: "no gateway component → skip",
			refs: []recipe.ComponentRef{{Name: "gpu-operator"}}, wantSkip: true,
		},
		{
			name:    "malformed declaration is an error, not a skip",
			refs:    []recipe.ComponentRef{{Name: "edge", Namespace: ns, Overrides: map[string]any{recipe.InferenceGatewayOverrideKey: map[string]any{"dataPlane": "gateway-address"}}}},
			wantErr: "without gatewayClass", wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:    "GatewayClass missing",
			refs:    []recipe.ComponentRef{declare("gateway-address")},
			wantErr: `GatewayClass "edge-class" not found`, wantCode: errors.ErrCodeNotFound,
		},
		{
			name: "GatewayClass not accepted",
			refs: []recipe.ComponentRef{declare("gateway-address")}, gatewayClass: gatewayClassObject("False"),
			wantErr: "GatewayClass not accepted", wantCode: errors.ErrCodeInternal,
		},
		{
			name: "Gateway missing in the profile namespace",
			refs: []recipe.ComponentRef{declare("gateway-address")}, gatewayClass: gatewayClassObject("True"),
			wantErr: "Gateway edge-system/inference-gateway not found", wantCode: errors.ErrCodeNotFound,
		},
		{
			// The declared class is accepted and a Gateway of the declared
			// name is programmed, but by a different implementation.
			name: "Gateway programmed by another class",
			refs: []recipe.ComponentRef{declare("gateway-address")}, gatewayClass: gatewayClassObject("True"),
			gateway: withGatewayClass(programmed("True"), "agentgateway"),
			wantErr: `has gatewayClassName "agentgateway"; the recipe declares "edge-class"`, wantCode: errors.ErrCodeInternal,
		},
		{
			name: "Gateway not programmed",
			refs: []recipe.ComponentRef{declare("gateway-address")}, gatewayClass: gatewayClassObject("True"), gateway: programmed("False"),
			wantErr: "Gateway not programmed", wantCode: errors.ErrCodeInternal,
		},
		{
			name: "required CRD missing",
			refs: []recipe.ComponentRef{declare("gateway-address")}, gatewayClass: gatewayClassObject("True"), gateway: programmed("True"),
			crds:    allCRDs[:2],
			wantErr: "CRD inferencepools.inference.networking.k8s.io not found", wantCode: errors.ErrCodeNotFound,
		},
		{
			name: "address probe passes on a Service-less cluster under enforce",
			refs: []recipe.ComponentRef{declare("gateway-address")}, gatewayClass: gatewayClassObject("True"), gateway: programmed("True"),
			crds: allCRDs,
		},
		{
			name: "service probe on the same cluster fails as proxy not found",
			refs: []recipe.ComponentRef{declare("service-endpoints")}, gatewayClass: gatewayClassObject("True"), gateway: programmed("True"),
			crds:    allCRDs,
			wantErr: "proxy Service was not found", wantCode: errors.ErrCodeNotFound,
		},
		{
			name: "traffic phase verdict is the check's",
			refs: []recipe.ComponentRef{declare("gateway-address")}, gatewayClass: gatewayClassObject("True"), gateway: programmed("True"),
			crds:       allCRDs,
			trafficErr: errors.New(errors.ErrCodeInternal, "gateway traffic phase failed in step verdict: weights swapped"),
			wantErr:    "weights swapped", wantCode: errors.ErrCodeInternal,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnforce(t, true)
			dyn := newGatewayFakeDynamicClient(t, tt.gateway)
			if tt.gatewayClass != nil {
				if err := dyn.Tracker().Create(gatewayClassGVR, tt.gatewayClass, ""); err != nil {
					t.Fatalf("seed GatewayClass: %v", err)
				}
			}
			for _, name := range tt.crds {
				if err := dyn.Tracker().Create(crdGVR, crdObject(name), ""); err != nil {
					t.Fatalf("seed CRD %s: %v", name, err)
				}
			}
			vctx := &validators.Context{
				Ctx: context.Background(), Clientset: fake.NewClientset(), DynamicClient: dyn,
				ValidationInput: &validatorv1.ValidationInput{ComponentRefs: tt.refs},
			}
			var trafficGW *unstructured.Unstructured
			traffic := func(_ *validators.Context, p *validators.InferenceGatewayProfile, gw *unstructured.Unstructured) error {
				trafficGW = gw
				if p == nil || p.GatewayName != gateway || p.Namespace != ns {
					t.Errorf("traffic phase got profile %+v, want %s/%s", p, ns, gateway)
				}
				return tt.trafficErr
			}
			err := checkInferenceGateway(vctx, traffic)
			if tt.wantSkip {
				if !validators.IsSkip(err) {
					t.Fatalf("expected a skip, got %v", err)
				}
				return
			}
			if validators.IsSkip(err) {
				t.Fatalf("unexpected skip: %v", err)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				errorstest.WantReportedCode(t, err, tt.wantCode)
				if (trafficGW != nil) != (tt.trafficErr != nil) {
					t.Errorf("traffic phase ran = %t; it must run only after every status check passed", trafficGW != nil)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if trafficGW == nil || trafficGW.GetName() != gateway {
				t.Errorf("traffic phase must run on the Gateway the status checks read, got %v", trafficGW)
			}
			gets := 0
			for _, action := range dyn.Actions() {
				if action.GetVerb() == "get" && action.GetResource() == gatewayGVR {
					gets++
				}
			}
			if gets != 1 {
				t.Errorf("Gateway read %d times, want once (handed to the data-plane and exposure steps)", gets)
			}
		})
	}
}

func TestClassifyGatewayAddress(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"10.0.0.1", gatewayAddressPrivate},
		{"172.16.100.12", gatewayAddressPrivate},
		{"192.168.1.1", gatewayAddressPrivate},
		{"100.64.3.7", gatewayAddressPrivate},
		{"100.127.255.254", gatewayAddressPrivate},
		{"100.128.0.1", gatewayAddressPublic},
		{"127.0.0.1", gatewayAddressPrivate},
		{"169.254.1.1", gatewayAddressPrivate},
		{"fd00::1", gatewayAddressPrivate},
		{"::ffff:100.64.0.1", gatewayAddressPrivate},
		{"::ffff:203.0.113.10", gatewayAddressPublic},
		{"0.0.0.0", gatewayAddressUnclassified},
		{"::", gatewayAddressUnclassified},
		{"203.0.113.10", gatewayAddressPublic},
		{"2001:db8::1", gatewayAddressPublic},
		{"gateway.example.com", gatewayAddressUnclassified},
		{"", gatewayAddressUnclassified},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := classifyGatewayAddress(tt.in); got != tt.want {
				t.Errorf("classifyGatewayAddress(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestEnvTruthy(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		{"", false},
		{"true", true},
		{"True", true},
		{"1", true},
		{"false", false},
		{"0", false},
		{"garbage", false},
	}
	for _, tt := range tests {
		t.Run(tt.val, func(t *testing.T) {
			// Set unconditionally (incl. the empty-string case) so each subtest
			// fully controls the var and can't read an ambient value — hermetic.
			t.Setenv("AICR_TEST_ENV_TRUTHY", tt.val)
			got := envTruthy("AICR_TEST_ENV_TRUTHY")
			if got != tt.want {
				t.Errorf("envTruthy(%q) = %v, want %v", tt.val, got, tt.want)
			}
		})
	}
}
