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

package v1

import (
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func declareInferenceGateway(fields map[string]any) map[string]any {
	return map[string]any{recipe.InferenceGatewayOverrideKey: fields}
}

func TestResolveInferenceGatewayProfile(t *testing.T) {
	agentgateway := InferenceGatewayProfile{
		Component: "agentgateway", GatewayClass: "agentgateway", GatewayName: "inference-gateway",
		Namespace: "agentgateway-system", Probe: GatewayProbeServiceEndpoints,
	}
	exampleDeclared := recipe.ComponentRef{
		Name: "example-gateway", Namespace: "example-gateway-system",
		Overrides: declareInferenceGateway(map[string]any{"gatewayClass": "example-gateway-class", "dataPlane": "gateway-address"}),
	}

	tests := []struct {
		name    string
		refs    []recipe.ComponentRef
		want    *InferenceGatewayProfile
		wantErr string
	}{
		{name: "no refs"},
		{name: "no gateway component", refs: []recipe.ComponentRef{{Name: "gpu-operator"}}},
		{name: "built-in agentgateway", refs: []recipe.ComponentRef{{Name: "agentgateway"}}, want: &agentgateway},
		{
			name: "built-in takes the resolved ref namespace",
			refs: []recipe.ComponentRef{{Name: "agentgateway", Namespace: "gw-system"}},
			want: func() *InferenceGatewayProfile { p := agentgateway; p.Namespace = "gw-system"; return &p }(),
		},
		{
			name: "disabled built-in is not a gateway",
			refs: []recipe.ComponentRef{{Name: "agentgateway", Overrides: map[string]any{"enabled": false}}},
		},
		{
			name: "declared gateway with the address probe",
			refs: []recipe.ComponentRef{exampleDeclared},
			want: &InferenceGatewayProfile{
				Component: "example-gateway", GatewayClass: "example-gateway-class", GatewayName: "inference-gateway",
				Namespace: "example-gateway-system", Probe: GatewayProbeGatewayAddress,
			},
		},
		{
			name: "declared gateway renames the Gateway object",
			refs: []recipe.ComponentRef{{
				Name: "edge", Namespace: "edge-system",
				Overrides: declareInferenceGateway(map[string]any{"gatewayClass": "edge-class", "gateway": "edge-gateway"}),
			}},
			want: &InferenceGatewayProfile{
				Component: "edge", GatewayClass: "edge-class", GatewayName: "edge-gateway",
				Namespace: "edge-system", Probe: GatewayProbeServiceEndpoints,
			},
		},
		{
			name: "override on a built-in changes its class",
			refs: []recipe.ComponentRef{{
				Name: "agentgateway", Overrides: declareInferenceGateway(map[string]any{"gatewayClass": "agentgateway-v2"}),
			}},
			want: func() *InferenceGatewayProfile { p := agentgateway; p.GatewayClass = "agentgateway-v2"; return &p }(),
		},
		{
			name: "declared without gatewayClass",
			refs: []recipe.ComponentRef{{
				Name: "example-gateway", Namespace: "example-gateway-system",
				Overrides: declareInferenceGateway(map[string]any{"dataPlane": "gateway-address"}),
			}},
			wantErr: "without gatewayClass",
		},
		{
			name: "declared without a resolved namespace",
			refs: []recipe.ComponentRef{{
				Name: "example-gateway", Overrides: declareInferenceGateway(map[string]any{"gatewayClass": "example-gateway-class"}),
			}},
			wantErr: "no resolved namespace",
		},
		{
			name: "empty gateway name",
			refs: []recipe.ComponentRef{{
				Name: "example-gateway", Namespace: "example-gateway-system",
				Overrides: declareInferenceGateway(map[string]any{"gatewayClass": "example-gateway-class", "gateway": ""}),
			}},
			wantErr: "may not be empty",
		},
		{
			// The evidence collector passes the name to kubectl as a bare
			// argument, where a leading dash reads as a flag.
			name: "gateway name that would read as a kubectl flag",
			refs: []recipe.ComponentRef{{
				Name: "example-gateway", Namespace: "example-gateway-system",
				Overrides: declareInferenceGateway(map[string]any{"gatewayClass": "example-gateway-class", "gateway": "--server=http://attacker:8080"}),
			}},
			wantErr: `overrides.inferenceGateway.gateway "--server=http://attacker:8080" is not a valid Kubernetes name`,
		},
		{
			name: "gateway name with a newline",
			refs: []recipe.ComponentRef{{
				Name: "example-gateway", Namespace: "example-gateway-system",
				Overrides: declareInferenceGateway(map[string]any{"gatewayClass": "example-gateway-class", "gateway": "pool-vip\nother"}),
			}},
			wantErr: "is not a valid Kubernetes name",
		},
		{
			name: "gateway class that would read as a kubectl flag",
			refs: []recipe.ComponentRef{{
				Name: "example-gateway", Namespace: "example-gateway-system",
				Overrides: declareInferenceGateway(map[string]any{"gatewayClass": "-kubeconfig=/tmp/x"}),
			}},
			wantErr: `overrides.inferenceGateway.gatewayClass "-kubeconfig=/tmp/x" is not a valid Kubernetes name`,
		},
		{
			name:    "namespace that is not a DNS-1123 label",
			refs:    []recipe.ComponentRef{{Name: "agentgateway", Namespace: "-n=kube-system"}},
			wantErr: `namespace "-n=kube-system" is not a valid Kubernetes name`,
		},
		{
			name: "unknown dataPlane probe",
			refs: []recipe.ComponentRef{{
				Name: "example-gateway", Namespace: "example-gateway-system",
				Overrides: declareInferenceGateway(map[string]any{"gatewayClass": "example-gateway-class", "dataPlane": "tcp-dial"}),
			}},
			wantErr: "is not one of",
		},
		{
			name: "unknown key",
			refs: []recipe.ComponentRef{{
				Name: "example-gateway", Namespace: "example-gateway-system",
				Overrides: declareInferenceGateway(map[string]any{"gatewayClass": "example-gateway-class", "port": "80"}),
			}},
			wantErr: "unknown overrides.inferenceGateway key",
		},
		{
			name: "unknown keys are reported in a stable order",
			refs: []recipe.ComponentRef{{
				Name: "example-gateway", Namespace: "example-gateway-system",
				Overrides: map[string]any{recipe.InferenceGatewayOverrideKey: map[string]any{
					"gatewayClass": "example-gateway-class", "zeta": "x", "alpha": "y",
				}},
			}},
			wantErr: `unknown overrides.inferenceGateway key "alpha"`,
		},
		{
			name: "near-miss key is an error, not a chart value",
			refs: []recipe.ComponentRef{{
				Name: "example-gateway", Namespace: "example-gateway-system",
				Overrides: map[string]any{"inference-gateway": map[string]any{"gatewayClass": "example-gateway-class"}},
			}},
			wantErr: `override key "inference-gateway" is not recognised`,
		},
		{
			name: "snake_case near miss is an error too",
			refs: []recipe.ComponentRef{{
				Name: "example-gateway", Namespace: "example-gateway-system",
				Overrides: map[string]any{"inference_gateway": map[string]any{"gatewayClass": "example-gateway-class"}},
			}},
			wantErr: `override key "inference_gateway" is not recognised`,
		},
		{
			name: "declaration is not a mapping",
			refs: []recipe.ComponentRef{{
				Name: "example-gateway", Namespace: "example-gateway-system",
				Overrides: map[string]any{recipe.InferenceGatewayOverrideKey: "yes"},
			}},
			wantErr: "must be a mapping",
		},
		{
			name: "field is not a string",
			refs: []recipe.ComponentRef{{
				Name: "example-gateway", Namespace: "example-gateway-system",
				Overrides: declareInferenceGateway(map[string]any{"gatewayClass": 3}),
			}},
			wantErr: "must be a string",
		},
		{
			name:    "two enabled gateways",
			refs:    []recipe.ComponentRef{{Name: "agentgateway"}, exampleDeclared},
			wantErr: "more than one inference gateway component [agentgateway, example-gateway]",
		},
		{
			name: "a disabled second gateway does not collide",
			refs: []recipe.ComponentRef{{Name: "agentgateway"}, {
				Name: "example-gateway", Namespace: "example-gateway-system",
				Overrides: map[string]any{
					"enabled":                          false,
					recipe.InferenceGatewayOverrideKey: map[string]any{"gatewayClass": "example-gateway-class"},
				},
			}},
			want: &agentgateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found, err := ResolveInferenceGatewayProfile(tt.refs)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (profile %+v)", tt.wantErr, got)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tt.wantErr)
				}
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if found != (tt.want != nil) {
				t.Fatalf("found = %v, profile %+v, want %+v", found, got, tt.want)
			}
			if found && got != *tt.want {
				t.Errorf("got %+v, want %+v", got, *tt.want)
			}
		})
	}
}

func TestInferenceGatewayProfile_IsProxyName(t *testing.T) {
	tests := []struct {
		name    string
		gateway string
		in      string
		want    bool
	}{
		{"exact proxy name", "inference-gateway", "inference-gateway", true},
		{"proxy with suffix", "inference-gateway", "inference-gateway-proxy", true},
		{"controller-manager excluded", "inference-gateway", "inference-gateway-controller-manager", false},
		{"webhook excluded", "inference-gateway", "inference-gateway-webhook", false},
		{"metrics excluded", "inference-gateway", "inference-gateway-metrics", false},
		{"gateway named after a marker matches itself", "metrics-gateway", "metrics-gateway", true},
		{"gateway named after a marker matches its proxy suffix", "metrics-gateway", "metrics-gateway-proxy", true},
		{"gateway named after a marker still excludes its control plane", "metrics-gateway", "metrics-gateway-metrics", false},
		{"unrelated name without the prefix", "inference-gateway", "agentgateway-controller-manager", false},
		{"substring inside another word is not a match", "gw", "legacy-gw-ingress", false},
		{"short name matches only itself or its own suffixes", "gw", "gw-proxy", true},
		{"short name is not a prefix without the dash", "api", "apiserver", false},
		{"renamed gateway matches its own name", "edge-gateway", "edge-gateway-proxy", true},
		{"renamed gateway ignores the conventional name", "edge-gateway", "inference-gateway", false},
		{"empty name", "inference-gateway", "", false},
		{"empty gateway name matches nothing", "", "inference-gateway", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := InferenceGatewayProfile{GatewayName: tt.gateway}
			if got := p.IsProxyName(tt.in); got != tt.want {
				t.Errorf("IsProxyName(%q) with gateway %q = %v, want %v", tt.in, tt.gateway, got, tt.want)
			}
		})
	}
}

func TestDefaultInferenceGatewayProfile(t *testing.T) {
	got := DefaultInferenceGatewayProfile()
	if got.Component != "agentgateway" || got.Namespace != "agentgateway-system" ||
		got.GatewayName != DefaultInferenceGatewayName || got.Probe != GatewayProbeServiceEndpoints {

		t.Errorf("unexpected default profile %+v", got)
	}
	// The default is a copy: mutating it must not change what the next caller sees.
	got.Namespace = "elsewhere"
	if DefaultInferenceGatewayProfile().Namespace != "agentgateway-system" {
		t.Error("DefaultInferenceGatewayProfile returned shared state")
	}
}
