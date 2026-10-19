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
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestNetworkOperatorReadinessRequiresEveryNicClusterPolicy exercises the
// committed readiness gates through the same in-process evaluator used by the
// gate image. An unnamed positive assertion is match-any, so the paired
// negative assertion must keep one ready policy from hiding a non-ready peer.
func TestNetworkOperatorReadinessRequiresEveryNicClusterPolicy(t *testing.T) {
	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	components := []string{"network-operator", "network-operator-ocp"}
	tests := []struct {
		name       string
		states     map[string]string
		wantPassed bool
	}{
		{
			name:       "one ready policy passes",
			states:     map[string]string{"ready": "ready"},
			wantPassed: true,
		},
		{
			name:       "every ready policy passes",
			states:     map[string]string{"ready-1": "ready", "ready-2": "ready"},
			wantPassed: true,
		},
		{
			name:       "one ready policy cannot hide a non-ready policy",
			states:     map[string]string{"ready": "ready", "not-ready": "notReady"},
			wantPassed: false,
		},
		{
			name:       "one ready policy cannot hide a policy without status",
			states:     map[string]string{"ready": "ready", "reconciling": ""},
			wantPassed: false,
		},
	}

	for _, component := range components {
		data, err := provider.ReadFile(t.Context(), "components/"+component+"/readiness.yaml")
		if err != nil {
			t.Fatalf("read %s readiness gate: %v", component, err)
		}
		for _, tt := range tests {
			t.Run(component+"/"+tt.name, func(t *testing.T) {
				fetcher := newFakeFetcher()
				policies := make([]map[string]any, 0, len(tt.states))
				for name, state := range tt.states {
					policy := map[string]any{
						"apiVersion": "mellanox.com/v1alpha1",
						"kind":       "NicClusterPolicy",
						"metadata":   map[string]any{"name": name},
					}
					if state != "" {
						policy["status"] = map[string]any{"state": state}
					}
					policies = append(policies, policy)
				}
				fetcher.addList("mellanox.com/v1alpha1", "NicClusterPolicy", "", policies)

				result := runChainsawTestInProcess(
					t.Context(), component, string(data), 100*time.Millisecond, fetcher)
				if result.Passed != tt.wantPassed {
					t.Errorf("Passed = %v, want %v (Error=%v Output=%s)",
						result.Passed, tt.wantPassed, result.Error, result.Output)
				}
			})
		}
	}
}
