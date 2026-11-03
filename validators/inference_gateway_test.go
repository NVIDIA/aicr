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

package validators

import (
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
)

func gatewayTestContext(refs ...recipe.ComponentRef) *Context {
	return &Context{ValidationInput: &v1.ValidationInput{ComponentRefs: refs}}
}

func declareInferenceGateway(fields map[string]any) map[string]any {
	return map[string]any{AICROverrideKey: map[string]any{InferenceGatewayOverrideKey: fields}}
}

func TestResolveInferenceGatewayProfile(t *testing.T) {
	agentgateway := InferenceGatewayProfile{
		Component: "agentgateway", GatewayClass: "agentgateway", GatewayName: "inference-gateway",
		Namespace: "agentgateway-system", Probe: GatewayProbeServiceEndpoints, Source: "built-in profile",
	}
	bnkDeclared := recipe.ComponentRef{
		Name: "bnk", Namespace: "f5-bnk",
		Overrides: declareInferenceGateway(map[string]any{"gatewayClass": "f5-gateway-class", "dataPlane": "gateway-address"}),
	}

	tests := []struct {
		name    string
		ctx     *Context
		want    *InferenceGatewayProfile
		wantErr string
	}{
		{name: "nil context", ctx: nil},
		{name: "no validation input", ctx: &Context{}},
		{name: "no gateway component", ctx: gatewayTestContext(recipe.ComponentRef{Name: "gpu-operator"})},
		{name: "built-in agentgateway", ctx: gatewayTestContext(recipe.ComponentRef{Name: "agentgateway"}), want: &agentgateway},
		{
			name: "built-in takes the resolved ref namespace",
			ctx:  gatewayTestContext(recipe.ComponentRef{Name: "agentgateway", Namespace: "gw-system"}),
			want: func() *InferenceGatewayProfile { p := agentgateway; p.Namespace = "gw-system"; return &p }(),
		},
		{
			name: "disabled built-in is not a gateway",
			ctx:  gatewayTestContext(recipe.ComponentRef{Name: "agentgateway", Overrides: map[string]any{"enabled": false}}),
		},
		{
			name: "declared gateway with the address probe",
			ctx:  gatewayTestContext(bnkDeclared),
			want: &InferenceGatewayProfile{
				Component: "bnk", GatewayClass: "f5-gateway-class", GatewayName: "inference-gateway",
				Namespace: "f5-bnk", Probe: GatewayProbeGatewayAddress, Source: "componentRef overrides.aicr.inferenceGateway",
			},
		},
		{
			name: "declared gateway renames the Gateway object",
			ctx: gatewayTestContext(recipe.ComponentRef{
				Name: "edge", Namespace: "edge-system",
				Overrides: declareInferenceGateway(map[string]any{"gatewayClass": "edge-class", "gateway": "edge-gateway"}),
			}),
			want: &InferenceGatewayProfile{
				Component: "edge", GatewayClass: "edge-class", GatewayName: "edge-gateway",
				Namespace: "edge-system", Probe: GatewayProbeServiceEndpoints, Source: "componentRef overrides.aicr.inferenceGateway",
			},
		},
		{
			name: "override on a built-in changes its class",
			ctx: gatewayTestContext(recipe.ComponentRef{
				Name: "agentgateway", Overrides: declareInferenceGateway(map[string]any{"gatewayClass": "agentgateway-v2"}),
			}),
			want: func() *InferenceGatewayProfile {
				p := agentgateway
				p.GatewayClass = "agentgateway-v2"
				p.Source = "built-in profile with componentRef overrides.aicr.inferenceGateway"
				return &p
			}(),
		},
		{
			name: "declared without gatewayClass",
			ctx: gatewayTestContext(recipe.ComponentRef{
				Name: "bnk", Namespace: "f5-bnk", Overrides: declareInferenceGateway(map[string]any{"dataPlane": "gateway-address"}),
			}),
			wantErr: "without gatewayClass",
		},
		{
			name: "declared without a resolved namespace",
			ctx: gatewayTestContext(recipe.ComponentRef{
				Name: "bnk", Overrides: declareInferenceGateway(map[string]any{"gatewayClass": "f5-gateway-class"}),
			}),
			wantErr: "no resolved namespace",
		},
		{
			name: "empty gateway name",
			ctx: gatewayTestContext(recipe.ComponentRef{
				Name: "bnk", Namespace: "f5-bnk",
				Overrides: declareInferenceGateway(map[string]any{"gatewayClass": "f5-gateway-class", "gateway": ""}),
			}),
			wantErr: "may not be empty",
		},
		{
			name: "unknown dataPlane probe",
			ctx: gatewayTestContext(recipe.ComponentRef{
				Name: "bnk", Namespace: "f5-bnk",
				Overrides: declareInferenceGateway(map[string]any{"gatewayClass": "f5-gateway-class", "dataPlane": "tcp-dial"}),
			}),
			wantErr: "is not one of",
		},
		{
			name: "unknown key",
			ctx: gatewayTestContext(recipe.ComponentRef{
				Name: "bnk", Namespace: "f5-bnk",
				Overrides: declareInferenceGateway(map[string]any{"gatewayClass": "f5-gateway-class", "port": "80"}),
			}),
			wantErr: "unknown overrides.aicr.inferenceGateway key",
		},
		{
			name: "declaration is not a mapping",
			ctx: gatewayTestContext(recipe.ComponentRef{
				Name: "bnk", Namespace: "f5-bnk",
				Overrides: map[string]any{AICROverrideKey: map[string]any{InferenceGatewayOverrideKey: "yes"}},
			}),
			wantErr: "must be a mapping",
		},
		{
			name: "field is not a string",
			ctx: gatewayTestContext(recipe.ComponentRef{
				Name: "bnk", Namespace: "f5-bnk",
				Overrides: declareInferenceGateway(map[string]any{"gatewayClass": 3}),
			}),
			wantErr: "must be a string",
		},
		{
			name:    "two enabled gateways",
			ctx:     gatewayTestContext(recipe.ComponentRef{Name: "agentgateway"}, bnkDeclared),
			wantErr: "more than one inference gateway component [agentgateway, bnk]",
		},
		{
			name: "a disabled second gateway does not collide",
			ctx: gatewayTestContext(recipe.ComponentRef{Name: "agentgateway"}, recipe.ComponentRef{
				Name: "bnk", Namespace: "f5-bnk",
				Overrides: map[string]any{
					"enabled":       false,
					AICROverrideKey: map[string]any{InferenceGatewayOverrideKey: map[string]any{"gatewayClass": "f5-gateway-class"}},
				},
			}),
			want: &agentgateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found, err := ResolveInferenceGatewayProfile(tt.ctx)
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
		{"unrelated name without the prefix", "inference-gateway", "agentgateway-controller-manager", false},
		{"substring inside another word is not a match", "gw", "legacy-gw-ingress", false},
		{"short name matches only itself or its own suffixes", "gw", "gw-proxy", true},
		{"short name does not claim a longer unrelated name", "api", "api-legacy-ingress", true},
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
