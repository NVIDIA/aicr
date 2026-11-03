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

	"github.com/NVIDIA/aicr/pkg/recipe"
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
)

// TestResolveInferenceGatewayProfile covers the Context plumbing; the
// declaration grammar is tested in pkg/validator/v1.
func TestResolveInferenceGatewayProfile(t *testing.T) {
	declared := recipe.ComponentRef{
		Name: "example-gateway", Namespace: "example-gateway-system",
		Overrides: map[string]any{recipe.InferenceGatewayOverrideKey: map[string]any{"gatewayClass": "example-gateway-class"}},
	}
	tests := []struct {
		name      string
		ctx       *Context
		wantFound bool
		wantName  string
		wantErr   string
	}{
		{name: "nil context"},
		{name: "no validation input", ctx: &Context{}},
		{name: "no gateway component", ctx: &Context{ValidationInput: &v1.ValidationInput{ComponentRefs: []recipe.ComponentRef{{Name: "gpu-operator"}}}}},
		{
			name:      "built-in agentgateway",
			ctx:       &Context{ValidationInput: &v1.ValidationInput{ComponentRefs: []recipe.ComponentRef{{Name: "agentgateway"}}}},
			wantFound: true, wantName: "agentgateway",
		},
		{
			name:      "declared gateway",
			ctx:       &Context{ValidationInput: &v1.ValidationInput{ComponentRefs: []recipe.ComponentRef{declared}}},
			wantFound: true, wantName: "example-gateway",
		},
		{
			name:    "two gateways",
			ctx:     &Context{ValidationInput: &v1.ValidationInput{ComponentRefs: []recipe.ComponentRef{{Name: "agentgateway"}, declared}}},
			wantErr: "more than one inference gateway",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found, err := ResolveInferenceGatewayProfile(tt.ctx)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if found != tt.wantFound || (found && got.Component != tt.wantName) {
				t.Errorf("found=%v profile=%+v, want found=%v component=%q", found, got, tt.wantFound, tt.wantName)
			}
		})
	}
	if DefaultInferenceGatewayProfile().Component != "agentgateway" {
		t.Error("DefaultInferenceGatewayProfile is not agentgateway")
	}
}
