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

// TestGCPDriverInstallerHealthCheckClusterStates drives the shipped health
// check through the in-process executor against synthetic cluster states.
//
// Each case pins wantOutput, not just the pass/fail verdict, so a case
// cannot pass for the wrong reason (see TestK8sAIBOMHealthCheckClusterStates
// for the same rationale).
func TestGCPDriverInstallerHealthCheckClusterStates(t *testing.T) {
	t.Parallel()

	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	data, err := provider.ReadFile(context.Background(), "checks/gcp-driver-installer/health-check.yaml")
	if err != nil {
		t.Fatalf("read health check: %v", err)
	}

	daemonSet := func(generation int, status map[string]any) map[string]any {
		return map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "DaemonSet",
			"metadata": map[string]any{
				"name":       "nvidia-driver-installer",
				"namespace":  "kube-system",
				"generation": generation,
				"labels":     map[string]any{"app.kubernetes.io/part-of": "aicr"},
			},
			"status": status,
		}
	}

	tests := []struct {
		name       string
		daemonSet  map[string]any
		wantPass   bool
		wantOutput string
	}{
		{
			name: "fully healthy cluster",
			daemonSet: daemonSet(2, map[string]any{
				"desiredNumberScheduled": 2, "numberReady": 2,
				"updatedNumberScheduled": 2, "observedGeneration": 2,
			}),
			wantPass: true,
		},
		{
			name: "not fully rolled out fails closed",
			daemonSet: daemonSet(2, map[string]any{
				"desiredNumberScheduled": 2, "numberReady": 1,
				"updatedNumberScheduled": 2, "observedGeneration": 2,
			}),
			wantOutput: "DaemonSet",
		},
		{
			// numberReady alone can't tell a fully-current rollout from one
			// where a node still runs the previous revision's pod, that pod
			// reports Ready too, so desired/ready both read 2/2 while only
			// one node has the new revision. updatedNumberScheduled is the
			// field that catches it.
			name: "stale-revision node fails closed",
			daemonSet: daemonSet(2, map[string]any{
				"desiredNumberScheduled": 2, "numberReady": 2,
				"updatedNumberScheduled": 1, "observedGeneration": 2,
			}),
			wantOutput: "DaemonSet",
		},
		{
			name: "status not yet observed at current generation fails closed",
			daemonSet: daemonSet(3, map[string]any{
				"desiredNumberScheduled": 2, "numberReady": 2,
				"updatedNumberScheduled": 2, "observedGeneration": 2,
			}),
			wantOutput: "DaemonSet",
		},
		{
			name: "missing part-of label fails closed",
			daemonSet: func() map[string]any {
				ds := daemonSet(2, map[string]any{
					"desiredNumberScheduled": 2, "numberReady": 2,
					"updatedNumberScheduled": 2, "observedGeneration": 2,
				})
				ds["metadata"].(map[string]any)["labels"] = map[string]any{}
				return ds
			}(),
			wantOutput: "DaemonSet",
		},
		{
			name: "zero desired pods fails closed",
			daemonSet: daemonSet(2, map[string]any{
				"desiredNumberScheduled": 0, "numberReady": 0,
				"updatedNumberScheduled": 0, "observedGeneration": 2,
			}),
			wantOutput: "DaemonSet",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fetcher := newFakeFetcher()
			fetcher.addGet("apps/v1", "DaemonSet", "kube-system", "nvidia-driver-installer", tt.daemonSet)

			result := runChainsawTestInProcess(
				context.Background(), "gcp-driver-installer", string(data), 2*time.Second, fetcher,
			)
			if result.Passed != tt.wantPass {
				t.Fatalf("passed = %v, want %v (output: %s)", result.Passed, tt.wantPass, result.Output)
			}
			if tt.wantOutput != "" && !strings.Contains(result.Output, tt.wantOutput) {
				t.Fatalf("output = %q, want it to name %q (the wrong assertion caught this state)",
					result.Output, tt.wantOutput)
			}
		})
	}
}
