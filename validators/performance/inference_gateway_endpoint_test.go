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
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func addressGatewayRef(fields map[string]any) recipe.ComponentRef {
	return recipe.ComponentRef{
		Name: "edge", Namespace: "edge-system",
		Overrides: map[string]any{recipe.InferenceGatewayOverrideKey: fields},
	}
}

func addressGateway(addresses []any, listeners []any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "gateway.networking.k8s.io/v1",
		"kind":       "Gateway",
		"metadata":   map[string]any{"name": "inference-gateway", "namespace": "edge-system"},
		"spec":       map[string]any{"listeners": listeners},
		"status":     map[string]any{"addresses": addresses},
	}}
}

// newAddressGatewayClient seeds the Gateway through the tracker under the real
// resource name: the fake's constructor pluralises "Gateway" as "gatewaies".
func newAddressGatewayClient(t *testing.T, gw *unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{inferenceGatewayGVR: "GatewayList"})
	if gw != nil {
		if err := dyn.Tracker().Create(inferenceGatewayGVR, gw, gw.GetNamespace()); err != nil {
			t.Fatalf("seed Gateway: %v", err)
		}
	}
	return dyn
}

func TestResolveGatewayEndpoint_AddressProbe(t *testing.T) {
	declared := addressGatewayRef(map[string]any{"gatewayClass": "edge-class", "dataPlane": "gateway-address"})
	ip := []any{map[string]any{"type": "IPAddress", "value": "172.16.100.12"}}
	tests := []struct {
		name     string
		refs     []recipe.ComponentRef
		gateway  *unstructured.Unstructured
		want     string
		wantErr  string
		wantCode errors.ErrorCode
	}{
		{
			name:    "address and HTTP listener port",
			refs:    []recipe.ComponentRef{declared},
			gateway: addressGateway(ip, []any{map[string]any{"name": "http", "protocol": "HTTP", "port": int64(8080)}}),
			want:    "http://172.16.100.12:8080",
		},
		{
			name: "HTTPS-first Gateway targets the HTTP listener, not the TLS port",
			refs: []recipe.ComponentRef{declared},
			gateway: addressGateway(ip, []any{
				map[string]any{"name": "https", "protocol": "HTTPS", "port": int64(443)},
				map[string]any{"name": "http", "protocol": "HTTP", "port": int64(8080)},
			}),
			want: "http://172.16.100.12:8080",
		},
		{
			name:     "no HTTP listener is an error, not a guess at port 80",
			refs:     []recipe.ComponentRef{declared},
			gateway:  addressGateway(ip, []any{map[string]any{"name": "https", "protocol": "HTTPS", "port": int64(443)}}),
			wantErr:  "has no HTTP listener (listeners: HTTPS)",
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name:     "no listeners at all is an error",
			refs:     []recipe.ComponentRef{declared},
			gateway:  addressGateway(ip, nil),
			wantErr:  "has no HTTP listener",
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			// The benchmark sends the address as Host; a hostname-bound
			// listener would answer 404 and surface as a readiness timeout.
			name:     "HTTP listener bound to a hostname is unsupported, not a timeout",
			refs:     []recipe.ComponentRef{declared},
			gateway:  addressGateway(ip, []any{map[string]any{"name": "http", "protocol": "HTTP", "port": int64(80), "hostname": "models.example.com"}}),
			wantErr:  "set a hostname (http=models.example.com)",
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			name: "hostname-free HTTP listener is preferred over a hostname-bound one",
			refs: []recipe.ComponentRef{declared},
			gateway: addressGateway(ip, []any{
				map[string]any{"name": "vhost", "protocol": "HTTP", "port": int64(80), "hostname": "models.example.com"},
				map[string]any{"name": "http", "protocol": "HTTP", "port": int64(8080)},
			}),
			want: "http://172.16.100.12:8080",
		},
		{
			name:    "IPv6 address is bracketed",
			refs:    []recipe.ComponentRef{declared},
			gateway: addressGateway([]any{map[string]any{"type": "IPAddress", "value": "fd00:10::12"}}, []any{map[string]any{"name": "http", "protocol": "HTTP", "port": int64(80)}}),
			want:    "http://[fd00:10::12]:80",
		},
		{
			name:     "no addresses",
			refs:     []recipe.ComponentRef{declared},
			gateway:  addressGateway(nil, []any{map[string]any{"name": "http", "protocol": "HTTP", "port": int64(80)}}),
			wantErr:  "no status.addresses",
			wantCode: errors.ErrCodeInternal,
		},
		{
			name:     "gateway missing",
			refs:     []recipe.ComponentRef{declared},
			wantErr:  "failed to read inference Gateway",
			wantCode: errors.ErrCodeInternal,
		},
		{
			name:     "malformed declaration is an error, not a fallback",
			refs:     []recipe.ComponentRef{addressGatewayRef(map[string]any{"dataPlane": "gateway-address"})},
			wantErr:  "without gatewayClass",
			wantCode: errors.ErrCodeInvalidRequest,
		},
		{
			// A recipe is present but ships no gateway: falling back to the
			// built-in agentgateway target could pass against a stale install.
			name:     "recipe without a gateway is an error, not the built-in target",
			refs:     []recipe.ComponentRef{{Name: "dynamo-platform", Namespace: "dynamo-system"}},
			wantErr:  "requires an enabled inference gateway component",
			wantCode: errors.ErrCodeInvalidRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &validators.Context{
				Ctx: context.Background(), Clientset: fake.NewClientset(), DynamicClient: newAddressGatewayClient(t, tt.gateway),
				ValidationInput: &validatorv1.ValidationInput{ComponentRefs: tt.refs},
			}
			got, err := resolveInferenceEndpoint(ctx, &inferenceWorkloadConfig{routingMode: inferenceRoutingModeGatewayEPP})
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
			if got != tt.want {
				t.Errorf("resolveInferenceEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveGatewayEndpoint_ServiceProbe pins the Service-backed lookup:
// Services are listed in the profile's namespace, the proxy is chosen by the
// profile's predicate so a renamed Gateway's proxy wins over its controller
// manager, and a frontend-routed run never reads the declaration at all.
func TestResolveGatewayEndpoint_ServiceProbe(t *testing.T) {
	svc := func(ns, name string, port int32) *v1.Service {
		return &v1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec:       v1.ServiceSpec{Ports: []v1.ServicePort{{Port: port}}},
		}
	}
	renamed := recipe.ComponentRef{
		Name: "agentgateway", Namespace: "gw-system",
		Overrides: map[string]any{recipe.InferenceGatewayOverrideKey: map[string]any{"gateway": "edge-gateway"}},
	}
	tests := []struct {
		name     string
		refs     []recipe.ComponentRef
		services []runtime.Object
		mode     inferenceRoutingMode
		want     string
	}{
		{
			name: "lookup lists Services in the profile's namespace",
			refs: []recipe.ComponentRef{{Name: "agentgateway", Namespace: "gw-system"}},
			mode: inferenceRoutingModeGatewayEPP,
			want: "http://inference-gateway.gw-system.svc:80",
		},
		{
			name:     "renamed gateway picks its proxy over its controller manager",
			refs:     []recipe.ComponentRef{renamed},
			services: []runtime.Object{svc("gw-system", "edge-gateway-controller-manager", 9443), svc("gw-system", "edge-gateway-proxy", 8080)},
			mode:     inferenceRoutingModeGatewayEPP,
			want:     "http://edge-gateway-proxy.gw-system.svc:8080",
		},
		{
			name:     "frontend-routed run ignores a malformed gateway declaration",
			refs:     []recipe.ComponentRef{addressGatewayRef(map[string]any{"dataPlane": "gateway-address"})},
			services: []runtime.Object{svc("bench", inferenceDeploymentName+"-frontend", 8000)},
			mode:     inferenceRoutingModeDynamoRouter,
			want:     "http://" + inferenceDeploymentName + "-frontend.bench.svc:8000",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &validators.Context{
				Ctx: context.Background(), Clientset: fake.NewClientset(tt.services...),
				ValidationInput: &validatorv1.ValidationInput{ComponentRefs: tt.refs},
			}
			got, err := resolveInferenceEndpoint(ctx, &inferenceWorkloadConfig{namespace: "bench", routingMode: tt.mode})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveInferenceEndpoint() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestHTTPRouteTemplateAttachesToProfileGateway pins that the gateway-epp
// HTTPRoute's parentRef is rendered from the profile through the same fill
// deployInferenceWorkload applies, and that no placeholder survives rendering
// in the parentRef or the name (the renderer is a plain string replacement).
func TestHTTPRouteTemplateAttachesToProfileGateway(t *testing.T) {
	cfg := &inferenceWorkloadConfig{namespace: "bench", model: "m", routerMode: "direct", gpuCount: 1}
	data := inferenceTemplateData(cfg)
	setInferenceGatewayTemplateData(data, validators.InferenceGatewayProfile{GatewayName: "edge-gateway", Namespace: "edge-system"})
	obj, err := parseYAMLTemplate(filepath.Join("testdata", "inference", "http-route-gateway-epp.yaml"), data)
	if err != nil {
		t.Fatalf("parseYAMLTemplate: %v", err)
	}
	refs, _, _ := unstructured.NestedSlice(obj.Object, "spec", "parentRefs")
	if len(refs) != 1 {
		t.Fatalf("parentRefs = %v, want one entry", refs)
	}
	ref, _ := refs[0].(map[string]any)
	if ref["name"] != "edge-gateway" || ref["namespace"] != "edge-system" {
		t.Errorf("parentRef = %v, want edge-system/edge-gateway", ref)
	}
	// The renderer is a plain string replacement, so scan the whole rendered
	// object rather than the fields named above: backendRefs and the
	// namespace read placeholders too.
	rendered, err := json.Marshal(obj.Object)
	if err != nil {
		t.Fatalf("marshal rendered HTTPRoute: %v", err)
	}
	if strings.Contains(string(rendered), "${") {
		t.Errorf("unrendered placeholder in the HTTPRoute: %s", rendered)
	}
}

// TestGatewayAddressTargetPinsTheListener pins that the route the benchmark
// applies attaches to the listener the benchmark targets. Without a
// sectionName a route is Accepted when any listener admits it, so a Gateway
// whose targeted listener keeps allowedRoutes Same beside one that allows All
// would pass the Accepted wait and fail minutes later as a readiness timeout.
func TestGatewayAddressTargetPinsTheListener(t *testing.T) {
	ref := addressGatewayRef(map[string]any{"gatewayClass": "edge-class", "dataPlane": "gateway-address"})
	gw := addressGateway(
		[]any{map[string]any{"type": "IPAddress", "value": "172.16.100.12"}},
		[]any{
			map[string]any{"name": "tls", "protocol": "HTTPS", "port": int64(443)},
			map[string]any{"name": "bound", "protocol": "HTTP", "port": int64(80), "hostname": "models.example.com"},
			map[string]any{"name": "open", "protocol": "HTTP", "port": int64(8080)},
			map[string]any{"name": "spare", "protocol": "HTTP", "port": int64(8081)},
		})
	vctx := &validators.Context{
		Ctx: context.Background(), DynamicClient: newAddressGatewayClient(t, gw),
		ValidationInput: &validatorv1.ValidationInput{ComponentRefs: []recipe.ComponentRef{ref}},
	}
	profile, err := inferenceGatewayProfile(vctx)
	if err != nil {
		t.Fatalf("inferenceGatewayProfile: %v", err)
	}
	target, err := resolveGatewayAddressTarget(vctx, profile, true)
	if err != nil {
		t.Fatalf("resolveGatewayAddressTarget: %v", err)
	}
	if target.url != "http://172.16.100.12:8080" || target.listener != "open" {
		t.Fatalf("target = %+v, want the first hostname-free HTTP listener open on :8080", target)
	}

	cfg := &inferenceWorkloadConfig{namespace: "bench", model: "m", routerMode: "direct", gpuCount: 1}
	data := inferenceTemplateData(cfg)
	setInferenceGatewayTemplateData(data, profile)
	route, err := parseYAMLTemplate(filepath.Join("testdata", "inference", "http-route-gateway-epp.yaml"), data)
	if err != nil {
		t.Fatalf("parseYAMLTemplate: %v", err)
	}
	if err := pinRouteSectionName(route, target.listener); err != nil {
		t.Fatalf("pinRouteSectionName: %v", err)
	}
	refs, _, _ := unstructured.NestedSlice(route.Object, "spec", "parentRefs")
	if len(refs) != 1 {
		t.Fatalf("parentRefs = %v, want one", refs)
	}
	if got := refs[0].(map[string]any)["sectionName"]; got != "open" {
		t.Errorf("parentRef sectionName = %v, want open", got)
	}
}

// TestDeployInferenceWorkload_GatewayAddressFailsBeforeProvisioning pins that
// an address-backed Gateway the benchmark cannot use is reported before the
// namespace, model cache and workload are created.
func TestDeployInferenceWorkload_GatewayAddressFailsBeforeProvisioning(t *testing.T) {
	ref := addressGatewayRef(map[string]any{"gatewayClass": "edge-class", "dataPlane": "gateway-address"})
	gw := addressGateway(
		[]any{map[string]any{"type": "IPAddress", "value": "172.16.100.12"}},
		[]any{map[string]any{"name": "tls", "protocol": "HTTPS", "port": int64(443)}})
	client := fake.NewClientset()
	vctx := &validators.Context{
		Ctx: context.Background(), Clientset: client, DynamicClient: newAddressGatewayClient(t, gw),
		ValidationInput: &validatorv1.ValidationInput{ComponentRefs: []recipe.ComponentRef{ref}},
	}
	config := &inferenceWorkloadConfig{namespace: "aicr-inference-perf-addr", routingMode: inferenceRoutingModeGatewayEPP}
	err := deployInferenceWorkload(vctx, config)
	if err == nil || !strings.Contains(err.Error(), "has no HTTP listener") {
		t.Fatalf("error = %v, want the no-HTTP-listener failure", err)
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	if _, getErr := client.CoreV1().Namespaces().Get(context.Background(), config.namespace, metav1.GetOptions{}); getErr == nil {
		t.Errorf("namespace %s was created before the Gateway listener was checked", config.namespace)
	}
}

// TestGatewayAddressTarget_AddressNotYetPublished pins that the check made
// before provisioning needs only the listener: a freshly applied Gateway may
// publish its address while the workload deploys, and the endpoint read after
// the workload is ready still requires it.
func TestGatewayAddressTarget_AddressNotYetPublished(t *testing.T) {
	ref := addressGatewayRef(map[string]any{"gatewayClass": "edge-class", "dataPlane": "gateway-address"})
	gw := addressGateway(nil, []any{map[string]any{"name": "open", "protocol": "HTTP", "port": int64(8080)}})
	vctx := &validators.Context{
		Ctx: context.Background(), DynamicClient: newAddressGatewayClient(t, gw),
		ValidationInput: &validatorv1.ValidationInput{ComponentRefs: []recipe.ComponentRef{ref}},
	}
	profile, err := inferenceGatewayProfile(vctx)
	if err != nil {
		t.Fatalf("inferenceGatewayProfile: %v", err)
	}
	early, err := resolveGatewayAddressTarget(vctx, profile, false)
	if err != nil {
		t.Fatalf("pre-provisioning check failed on a Gateway with no address yet: %v", err)
	}
	if early.listener != "open" || early.url != "" {
		t.Errorf("early target = %+v, want listener open and no URL", early)
	}
	if _, err := resolveGatewayAddressEndpoint(vctx, profile); err == nil || !strings.Contains(err.Error(), "reports no status.addresses") {
		t.Errorf("endpoint error = %v, want the missing-address failure", err)
	}
}
