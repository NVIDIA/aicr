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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"github.com/NVIDIA/aicr/validators"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func addressGatewayRef(fields map[string]any) recipe.ComponentRef {
	return recipe.ComponentRef{
		Name: "edge", Namespace: "edge-system",
		Overrides: map[string]any{validators.AICROverrideKey: map[string]any{validators.InferenceGatewayOverrideKey: fields}},
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

func TestResolveGatewayEndpoint_AddressProbe(t *testing.T) {
	declared := addressGatewayRef(map[string]any{"gatewayClass": "edge-class", "dataPlane": "gateway-address"})
	tests := []struct {
		name    string
		refs    []recipe.ComponentRef
		gateway *unstructured.Unstructured
		want    string
		wantErr string
	}{
		{
			name: "address and HTTP listener port",
			refs: []recipe.ComponentRef{declared},
			gateway: addressGateway(
				[]any{map[string]any{"type": "IPAddress", "value": "172.16.100.12"}},
				[]any{map[string]any{"name": "http", "protocol": "HTTP", "port": int64(8080)}}),
			want: "http://172.16.100.12:8080",
		},
		{
			name: "HTTPS-first Gateway targets the HTTP listener, not the TLS port",
			refs: []recipe.ComponentRef{declared},
			gateway: addressGateway(
				[]any{map[string]any{"type": "IPAddress", "value": "10.0.0.5"}},
				[]any{
					map[string]any{"name": "https", "protocol": "HTTPS", "port": int64(443)},
					map[string]any{"name": "http", "protocol": "HTTP", "port": int64(8080)},
				}),
			want: "http://10.0.0.5:8080",
		},
		{
			name: "no HTTP listener falls back to the default port",
			refs: []recipe.ComponentRef{declared},
			gateway: addressGateway(
				[]any{map[string]any{"type": "IPAddress", "value": "10.0.0.5"}},
				[]any{map[string]any{"name": "https", "protocol": "HTTPS", "port": int64(443)}}),
			want: "http://10.0.0.5:80",
		},
		{
			name:    "no listener port falls back to the default port",
			refs:    []recipe.ComponentRef{declared},
			gateway: addressGateway([]any{map[string]any{"type": "IPAddress", "value": "172.16.100.12"}}, nil),
			want:    "http://172.16.100.12:80",
		},
		{
			name:    "IPv6 address is bracketed",
			refs:    []recipe.ComponentRef{declared},
			gateway: addressGateway([]any{map[string]any{"type": "IPAddress", "value": "fd00:10::12"}}, nil),
			want:    "http://[fd00:10::12]:80",
		},
		{
			name:    "no addresses",
			refs:    []recipe.ComponentRef{declared},
			gateway: addressGateway(nil, nil),
			wantErr: "no status.addresses",
		},
		{
			name:    "gateway missing",
			refs:    []recipe.ComponentRef{declared},
			wantErr: "failed to read inference Gateway",
		},
		{
			name:    "malformed declaration is an error, not a fallback",
			refs:    []recipe.ComponentRef{addressGatewayRef(map[string]any{"dataPlane": "gateway-address"})},
			wantErr: "without gatewayClass",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Seeded through the tracker under the real resource name: the
			// fake's constructor pluralises "Gateway" as "gatewaies".
			dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
				map[schema.GroupVersionResource]string{inferenceGatewayGVR: "GatewayList"})
			if tt.gateway != nil {
				if err := dyn.Tracker().Create(inferenceGatewayGVR, tt.gateway, tt.gateway.GetNamespace()); err != nil {
					t.Fatalf("seed Gateway: %v", err)
				}
			}
			ctx := &validators.Context{
				Ctx: context.Background(), Clientset: fake.NewClientset(), DynamicClient: dyn,
				ValidationInput: &validatorv1.ValidationInput{ComponentRefs: tt.refs},
			}
			got, err := resolveInferenceEndpoint(ctx, &inferenceWorkloadConfig{routingMode: inferenceRoutingModeGatewayEPP})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
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

// TestResolveGatewayEndpoint_ServiceProbeFollowsProfileNamespace pins that the
// Service-backed lookup uses the recipe's namespace, not a fixed one.
func TestResolveGatewayEndpoint_ServiceProbeFollowsProfileNamespace(t *testing.T) {
	ref := recipe.ComponentRef{Name: "agentgateway", Namespace: "gw-system"}
	ctx := &validators.Context{
		Ctx: context.Background(), Clientset: fake.NewClientset(),
		ValidationInput: &validatorv1.ValidationInput{ComponentRefs: []recipe.ComponentRef{ref}},
	}
	got, err := resolveInferenceEndpoint(ctx, &inferenceWorkloadConfig{routingMode: inferenceRoutingModeGatewayEPP})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := "http://inference-gateway.gw-system.svc:80"; got != want {
		t.Errorf("resolveInferenceEndpoint() = %q, want %q", got, want)
	}
}
