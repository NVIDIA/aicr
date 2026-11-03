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

	"github.com/NVIDIA/aicr/validators"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
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

func agentgatewayProfile() *validators.InferenceGatewayProfile {
	p := validators.DefaultInferenceGatewayProfile()
	return &p
}

// addressGatewayProfile is a gateway whose data plane has no Service: the
// address lives in the Gateway object (a hardware or DPU-hosted proxy).
func addressGatewayProfile() *validators.InferenceGatewayProfile {
	return &validators.InferenceGatewayProfile{
		Component: "edge-gateway", GatewayClass: "edge-class", GatewayName: "inference-gateway",
		Namespace: "edge-system", Probe: validators.GatewayProbeGatewayAddress, Source: "test",
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
		"status":     map[string]any{"listeners": listeners, "addresses": addresses},
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
			if tt.requireScoped {
				t.Setenv(requireScopedGatewayEnv, "true")
			} else {
				t.Setenv(requireScopedGatewayEnv, "")
			}

			err := assessGatewayExposure(vctx, agentgatewayProfile(), nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("assessGatewayExposure() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !strings.Contains(err.Error(), "0.0.0.0/0") {
				t.Errorf("error %q should describe the open exposure", err.Error())
			}
		})
	}
}

// TestAssessGatewayExposure_RenamedGateway verifies the proxy predicate follows
// the profile's Gateway name, so a recipe that renames the Gateway still has
// its proxy Service assessed and the conventional name is no longer special.
func TestAssessGatewayExposure_RenamedGateway(t *testing.T) {
	profile := agentgatewayProfile()
	profile.GatewayName = "edge-gateway"
	svc := lbService("edge-gateway", nil)
	ignored := lbService("inference-gateway", nil)
	vctx := &validators.Context{Ctx: context.Background(), Clientset: fake.NewClientset(svc, ignored)}
	t.Setenv(requireScopedGatewayEnv, "true")
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
			if tt.requireScoped {
				t.Setenv(requireScopedGatewayEnv, "true")
			} else {
				t.Setenv(requireScopedGatewayEnv, "")
			}
			err := assessGatewayExposure(&validators.Context{Ctx: context.Background(), Clientset: client}, agentgatewayProfile(), nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("assessGatewayExposure() error = %v, wantErr %v", err, tt.wantErr)
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
		},
		{
			name: "one public among private, enforce → fail names only the public one",
			gateway: gatewayObject(profile, nil, []any{
				gatewayAddress("IPAddress", "10.1.2.3"), gatewayAddress("IPAddress", "198.51.100.7"),
			}),
			requireScoped: true,
			wantErr:       "[198.51.100.7]",
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
		},
		{
			name:          "no addresses, enforce → pass (nothing exposed)",
			gateway:       gatewayObject(profile, nil, nil),
			requireScoped: true,
		},
		{
			name:          "no gateway object, enforce → fail closed",
			requireScoped: true,
			wantErr:       "failed to read Gateway",
		},
		{
			name: "no gateway object, default policy → non-fatal",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.requireScoped {
				t.Setenv(requireScopedGatewayEnv, "true")
			} else {
				t.Setenv(requireScopedGatewayEnv, "")
			}
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
		})
	}
}

func TestValidateGatewayAddressDataPlane(t *testing.T) {
	profile := addressGatewayProfile()
	ok := []any{programmedListener("http", "True")}
	addr := []any{gatewayAddress("IPAddress", "172.16.100.12")}
	tests := []struct {
		name            string
		listeners       []any
		addresses       []any
		wantErr         string
		wantProgrammed  int
		wantStatusAddrs int
	}{
		{name: "programmed listener with an address", listeners: ok, addresses: addr, wantProgrammed: 1, wantStatusAddrs: 1},
		{
			name:            "listener without conditions is tolerated",
			listeners:       []any{map[string]any{"name": "http", "attachedRoutes": int64(0)}},
			addresses:       addr,
			wantStatusAddrs: 1,
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
			report := &gatewayDataPlaneReport{}
			err := validateGatewayAddressDataPlane(gatewayObject(profile, tt.listeners, tt.addresses), profile, report)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if report.ProgrammedListeners != tt.wantProgrammed || report.StatusAddresses != tt.wantStatusAddrs {
				t.Errorf("report = %+v, want programmed=%d addresses=%d", *report, tt.wantProgrammed, tt.wantStatusAddrs)
			}
		})
	}
}

// TestValidateGatewayDataPlane_AddressProbe runs the whole data-plane step
// against a fake cluster with no Service and no EndpointSlice: the address
// probe must pass on the Gateway object alone, and the Service probe must
// still fail the same cluster, which is the behavior this profile exists to
// change.
func TestValidateGatewayDataPlane_AddressProbe(t *testing.T) {
	profile := addressGatewayProfile()
	gw := gatewayObject(profile, []any{programmedListener("http", "True")}, []any{gatewayAddress("IPAddress", "172.16.100.12")})
	dyn := newGatewayFakeDynamicClient(t, gw)
	vctx := &validators.Context{Ctx: context.Background(), Clientset: fake.NewClientset(), DynamicClient: dyn}

	report, err := validateGatewayDataPlane(vctx, profile, gw)
	if err != nil {
		t.Fatalf("address probe: unexpected error: %v", err)
	}
	if report.StatusAddresses != 1 || report.ListenerCount != 1 {
		t.Errorf("address probe report = %+v", *report)
	}

	serviceProbe := *profile
	serviceProbe.Probe = validators.GatewayProbeServiceEndpoints
	if _, err := validateGatewayDataPlane(vctx, &serviceProbe, gw); err == nil || !strings.Contains(err.Error(), "no ready endpoints") {
		t.Fatalf("service probe on a Service-less cluster: error = %v, want no ready endpoints", err)
	}

	if _, err := validateGatewayDataPlane(vctx, profile, nil); err == nil || !strings.Contains(err.Error(), "no Gateway object") {
		t.Fatalf("nil Gateway: error = %v, want no Gateway object", err)
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
		{"0.0.0.0", gatewayAddressPrivate},
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
