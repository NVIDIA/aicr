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

package recipe

import (
	"bytes"
	stderrors "errors"
	"io"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/manifest"
)

const agentgatewayGatewayManifest = "components/agentgateway/manifests/inference-gateway.yaml"

// renderAgentgatewayParameters renders the inference-gateway manifest with
// values and returns the AgentgatewayParameters document, whose
// spec.service.spec is merged onto the Service the controller generates.
func renderAgentgatewayParameters(t *testing.T, values map[string]any) map[string]any {
	t.Helper()
	content, err := GetEmbeddedFS().ReadFile(agentgatewayGatewayManifest)
	if err != nil {
		t.Fatalf("read %s: %v", agentgatewayGatewayManifest, err)
	}
	rendered, err := manifest.Render(content, manifest.RenderInput{
		ComponentName: "agentgateway",
		Namespace:     "agentgateway-system",
		ChartName:     "agentgateway",
		ChartVersion:  "v1.5.0",
		Values:        values,
	})
	if err != nil {
		t.Fatalf("render %s: %v", agentgatewayGatewayManifest, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(rendered))
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if stderrors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode rendered manifest: %v\n%s", err, rendered)
		}
		if doc["kind"] == "AgentgatewayParameters" {
			return doc
		}
	}
	t.Fatalf("no AgentgatewayParameters document rendered:\n%s", rendered)
	return nil
}

// TestAgentgatewayGatewayManifest_ServiceType pins the Service block of the
// AgentgatewayParameters CR. The bundler injects allowedSourceRanges only for
// a LoadBalancer serviceType, and the rendered Service type decides whether
// they may appear:
// loadBalancerSourceRanges is accepted by the apiserver only on a
// LoadBalancer Service.
func TestAgentgatewayGatewayManifest_ServiceType(t *testing.T) {
	ranges := []any{"10.0.0.0/8", "172.16.0.0/12"}
	tests := []struct {
		name       string
		values     map[string]any
		wantType   string // "" = no type line rendered
		wantRanges bool
	}{
		{
			name:       "default keeps the controller type and renders the ranges",
			values:     map[string]any{"allowedSourceRanges": ranges, "serviceType": ""},
			wantRanges: true,
		},
		{
			name:       "explicit LoadBalancer renders type and ranges",
			values:     map[string]any{"allowedSourceRanges": ranges, "serviceType": "LoadBalancer"},
			wantType:   "LoadBalancer",
			wantRanges: true,
		},
		{
			name:     "ClusterIP renders type and drops the LoadBalancer-only ranges",
			values:   map[string]any{"allowedSourceRanges": ranges, "serviceType": "ClusterIP"},
			wantType: "ClusterIP",
		},
		{
			name:   "no ranges and no type renders no service block",
			values: map[string]any{"allowedSourceRanges": []any{}, "serviceType": ""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr := renderAgentgatewayParameters(t, tt.values)
			spec, _ := cr["spec"].(map[string]any)
			service, hasService := spec["service"].(map[string]any)
			if !hasService {
				if tt.wantType != "" || tt.wantRanges {
					t.Fatalf("spec.service missing; want type %q, ranges %v", tt.wantType, tt.wantRanges)
				}
				return
			}
			if tt.wantType == "" && !tt.wantRanges {
				t.Fatalf("spec.service = %v, want none", service)
			}
			svcSpec := valueAtPath[map[string]any](t, service, "spec")
			gotType, _ := svcSpec["type"].(string)
			if gotType != tt.wantType {
				t.Errorf("spec.service.spec.type = %q, want %q", gotType, tt.wantType)
			}
			gotRanges, hasRanges := svcSpec["loadBalancerSourceRanges"]
			if hasRanges != tt.wantRanges {
				t.Fatalf("loadBalancerSourceRanges present = %v (%v), want %v", hasRanges, gotRanges, tt.wantRanges)
			}
			if tt.wantRanges {
				got, _ := gotRanges.([]any)
				if len(got) != len(ranges) {
					t.Errorf("loadBalancerSourceRanges = %v, want %v", got, ranges)
				}
			}
		})
	}
}
