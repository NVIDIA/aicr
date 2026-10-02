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

// TestDRANodeLabelerHealthCheckClusterStates drives the shipped
// dra-node-labeler health check through the in-process executor against
// synthetic cluster states. The check is reachable only through a bundle
// built with --dra-eviction-node-label (#2848); this pins that, once it
// runs, it passes on a healthy labeler and fails closed on a broken one.
func TestDRANodeLabelerHealthCheckClusterStates(t *testing.T) {
	t.Parallel()

	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	data, err := provider.ReadFile(context.Background(), "checks/dra-node-labeler/health-check.yaml")
	if err != nil {
		t.Fatalf("read health check: %v", err)
	}

	daemonSet := func(desired, ready, updated int) map[string]any {
		return map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "DaemonSet",
			"metadata":   map[string]any{"name": "dra-node-labeler", "namespace": "gpu-operator"},
			"status": map[string]any{
				"desiredNumberScheduled": desired,
				"numberReady":            ready,
				"updatedNumberScheduled": updated,
			},
		}
	}
	pod := func(name, phase string, restarts int) map[string]any {
		return map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]any{
				"name":      name,
				"namespace": "gpu-operator",
				"labels":    map[string]any{"app": "dra-node-labeler"},
			},
			"status": map[string]any{
				"phase":             phase,
				"containerStatuses": []any{map[string]any{"name": "labeler", "restartCount": restarts}},
			},
		}
	}
	healthyPods := []map[string]any{pod("dra-node-labeler-a", "Running", 0), pod("dra-node-labeler-b", "Running", 0)}

	tests := []struct {
		name       string
		daemonSet  map[string]any
		pods       []map[string]any
		wantPass   bool
		wantOutput string
	}{
		{
			name:      "labeler rolled out on every GPU node",
			daemonSet: daemonSet(2, 2, 2),
			pods:      healthyPods,
			wantPass:  true,
		},
		{
			name:       "DaemonSet absent fails closed",
			pods:       healthyPods,
			wantOutput: "DaemonSet",
		},
		{
			// No gpu.present node, or the affinity never matched: the DRA
			// kubelet plugin would sit at DESIRED=0 too, but this check must
			// name the labeler as the cause.
			name:       "zero desired pods fails closed",
			daemonSet:  daemonSet(0, 0, 0),
			wantOutput: "DaemonSet",
		},
		{
			name:       "a node still waiting on its labeler pod fails closed",
			daemonSet:  daemonSet(2, 1, 2),
			pods:       []map[string]any{pod("dra-node-labeler-a", "Running", 0), pod("dra-node-labeler-b", "Pending", 0)},
			wantOutput: "DaemonSet",
		},
		{
			// The labeler retries in place; a restart means its script exited,
			// which the readiness probe cannot see once the pod is Running again.
			// One pod only: the list form passes when ANY item matches, so a
			// healthy sibling would mask the restarting pod.
			name:       "restarting labeler pod fails closed",
			daemonSet:  daemonSet(1, 1, 1),
			pods:       []map[string]any{pod("dra-node-labeler-a", "Running", 3)},
			wantOutput: "Pod",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fetcher := newFakeFetcher()
			if tt.daemonSet != nil {
				fetcher.addGet("apps/v1", "DaemonSet", "gpu-operator", "dra-node-labeler", tt.daemonSet)
			}
			fetcher.addList("v1", "Pod", "gpu-operator", tt.pods)

			result := runChainsawTestInProcess(
				context.Background(), "dra-node-labeler", string(data), 2*time.Second, fetcher,
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
