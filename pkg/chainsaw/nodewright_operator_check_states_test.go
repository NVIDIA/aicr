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

package chainsaw

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestNodewrightOperatorHealthCheckClusterStates runs the shipped health check
// against synthetic Deployment states.
func TestNodewrightOperatorHealthCheckClusterStates(t *testing.T) {
	t.Parallel()

	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	data, err := provider.ReadFile(context.Background(), "checks/nodewright-operator/health-check.yaml")
	if err != nil {
		t.Fatalf("read health check: %v", err)
	}

	managerLabels := func() map[string]any {
		return map[string]any{"app.kubernetes.io/component": "manager", "control-plane": "controller-manager"}
	}
	deployment := func(name string, labels map[string]any, status map[string]any) map[string]any {
		return map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]any{"name": name, "namespace": "nodewright", "labels": labels},
			"status":     status,
		}
	}
	available := map[string]any{"availableReplicas": 2}

	tests := []struct {
		name        string
		deployments []map[string]any
		wantPass    bool
		wantOutput  string
	}{
		{
			name:        "one available Deployment passes under any name",
			deployments: []map[string]any{deployment("skyhook-operator-controller-manager", managerLabels(), available)},
			wantPass:    true,
		},
		{
			name: "an unlabeled Deployment that is not available is ignored",
			deployments: []map[string]any{
				deployment("nodewright-controller-manager", managerLabels(), available),
				deployment("other", map[string]any{"app": "other"}, map[string]any{}),
			},
			wantPass: true,
		},
		{
			name:        "no matching Deployment fails",
			deployments: []map[string]any{deployment("other", map[string]any{"app": "other"}, available)},
			wantOutput:  "no resources found",
		},
		{
			name: "an available Deployment does not hide a matching one with zero replicas",
			deployments: []map[string]any{
				deployment("nodewright-controller-manager", managerLabels(), available),
				deployment("skyhook-operator-controller-manager", managerLabels(), map[string]any{}),
			},
			wantOutput: "forbidden shape",
		},
		{
			name: "an available Deployment does not hide a matching one reporting zero available",
			deployments: []map[string]any{
				deployment("nodewright-controller-manager", managerLabels(), available),
				deployment("skyhook-operator-controller-manager", managerLabels(), map[string]any{"availableReplicas": 0}),
			},
			wantOutput: "forbidden shape",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fetcher := newFakeFetcher()
			fetcher.addGet("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "skyhooks.skyhook.nvidia.com", map[string]any{
				"apiVersion": "apiextensions.k8s.io/v1",
				"kind":       "CustomResourceDefinition",
				"metadata":   map[string]any{"name": "skyhooks.skyhook.nvidia.com"},
				"status": map[string]any{
					"conditions": []map[string]any{{"type": "Established", "status": "True"}},
				},
			})
			fetcher.addList("apps/v1", "Deployment", "nodewright", tt.deployments)
			fetcher.addList("v1", "Pod", "nodewright", nil)

			result := runChainsawTestInProcess(
				context.Background(), "nodewright-operator", string(data), 2*time.Second, fetcher,
			)
			if result.Passed != tt.wantPass {
				t.Fatalf("passed = %v, want %v (output: %s)", result.Passed, tt.wantPass, result.Output)
			}
			if !strings.Contains(result.Output, tt.wantOutput) {
				t.Fatalf("output = %q, want it to contain %q", result.Output, tt.wantOutput)
			}
		})
	}
}
