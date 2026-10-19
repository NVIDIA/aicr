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
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestGKEGB200RDMAHealthCheckClusterStates drives the shipped health check
// through the in-process executor against synthetic cluster states, covering
// a missing or misbound GKENetworkParamSet/Network on any of the 5 objects.
//
// Each case pins wantOutput, not just the pass/fail verdict, so a case
// cannot pass for the wrong reason (see TestK8sAIBOMHealthCheckClusterStates
// for the same rationale).
func TestGKEGB200RDMAHealthCheckClusterStates(t *testing.T) {
	t.Parallel()

	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	data, err := provider.ReadFile(context.Background(), "checks/gke-gb200-rdma/health-check.yaml")
	if err != nil {
		t.Fatalf("read health check: %v", err)
	}

	gkeNetworkParamSet := func(name, deviceMode string) map[string]any {
		return map[string]any{
			"apiVersion": "networking.gke.io/v1",
			"kind":       "GKENetworkParamSet",
			"metadata":   map[string]any{"name": name},
			"spec": map[string]any{
				"vpc":        "prefix-net",
				"vpcSubnet":  "prefix-sub",
				"deviceMode": deviceMode,
			},
			"status": map[string]any{
				"conditions": []map[string]any{
					{"type": "Ready", "status": "True"},
				},
			},
		}
	}
	gkeNetwork := func(name string) map[string]any {
		return map[string]any{
			"apiVersion": "networking.gke.io/v1",
			"kind":       "Network",
			"metadata":   map[string]any{"name": name},
			"spec": map[string]any{
				"type": "Device",
				"parametersRef": map[string]any{
					"group": "networking.gke.io",
					"kind":  "GKENetworkParamSet",
					"name":  name,
				},
			},
			"status": map[string]any{
				"conditions": []map[string]any{
					{"type": "Ready", "status": "True"},
					{"type": "ParamsReady", "status": "True"},
				},
			},
		}
	}
	// gb200Node returns a Node with the given networks attached, formatted the
	// way GKE actually annotates a node whose additionalNodeNetworkConfigs
	// includes them (networking.gke.io/network-status, a JSON array of
	// {"name": ...} entries). See docs/integrator/gke-gb200-networking.md.
	gb200Node := func(name string, networks ...string) map[string]any {
		entries := make([]string, 0, len(networks)+1)
		entries = append(entries, `{"name":"default"}`)
		for _, n := range networks {
			entries = append(entries, fmt.Sprintf(`{"name":%q}`, n))
		}
		return map[string]any{
			"apiVersion": "v1",
			"kind":       "Node",
			"metadata": map[string]any{
				"name":   name,
				"labels": map[string]any{"cloud.google.com/gke-accelerator": "nvidia-gb200"},
				"annotations": map[string]any{
					"networking.gke.io/network-status": "[" + strings.Join(entries, ",") + "]",
				},
			},
		}
	}
	allFiveNetworks := []string{"gvnic-1", "rdma-0", "rdma-1", "rdma-2", "rdma-3"}

	tests := []struct {
		name string
		// mutate lets a case delete or corrupt one fixture from the healthy
		// baseline before the check runs.
		mutate     func(f *fakeFetcher)
		wantPass   bool
		wantOutput string
	}{
		{
			name:     "fully healthy cluster",
			wantPass: true,
		},
		{
			name: "missing rdma-1 GKENetworkParamSet fails closed",
			mutate: func(f *fakeFetcher) {
				delete(f.gets, "networking.gke.io/v1/GKENetworkParamSet//rdma-1")
			},
			wantOutput: "rdma-1",
		},
		{
			name: "rdma-2 GKENetworkParamSet has the wrong deviceMode fails closed",
			mutate: func(f *fakeFetcher) {
				f.gets["networking.gke.io/v1/GKENetworkParamSet//rdma-2"] = gkeNetworkParamSet("rdma-2", "NetDevice")
			},
			wantOutput: "rdma-2",
		},
		{
			name: "missing gvnic-1 Network fails closed",
			mutate: func(f *fakeFetcher) {
				delete(f.gets, "networking.gke.io/v1/Network//gvnic-1")
			},
			wantOutput: "gvnic-1",
		},
		{
			name: "gvnic-1 GKENetworkParamSet bound as RDMA instead of NetDevice fails closed",
			mutate: func(f *fakeFetcher) {
				f.gets["networking.gke.io/v1/GKENetworkParamSet//gvnic-1"] = gkeNetworkParamSet("gvnic-1", "RDMA")
			},
			wantOutput: "gvnic-1",
		},
		{
			name: "rdma-3 Network parametersRef points at the wrong GKENetworkParamSet fails closed",
			mutate: func(f *fakeFetcher) {
				n := gkeNetwork("rdma-3")
				n["spec"].(map[string]any)["parametersRef"].(map[string]any)["name"] = "rdma-0"
				f.gets["networking.gke.io/v1/Network//rdma-3"] = n
			},
			wantOutput: "rdma-3",
		},
		{
			name: "gvnic-1 GKENetworkParamSet not Ready fails closed",
			mutate: func(f *fakeFetcher) {
				n := gkeNetworkParamSet("gvnic-1", "NetDevice")
				n["status"].(map[string]any)["conditions"] = []map[string]any{
					{"type": "Ready", "status": "False"},
				}
				f.gets["networking.gke.io/v1/GKENetworkParamSet//gvnic-1"] = n
			},
			wantOutput: "gvnic-1",
		},
		{
			name: "rdma-0 Network not Ready fails closed",
			mutate: func(f *fakeFetcher) {
				n := gkeNetwork("rdma-0")
				n["status"].(map[string]any)["conditions"] = []map[string]any{
					{"type": "Ready", "status": "False"},
					{"type": "ParamsReady", "status": "True"},
				}
				f.gets["networking.gke.io/v1/Network//rdma-0"] = n
			},
			wantOutput: "rdma-0",
		},
		{
			name: "rdma-1 Network ParamsReady false fails closed",
			mutate: func(f *fakeFetcher) {
				n := gkeNetwork("rdma-1")
				n["status"].(map[string]any)["conditions"] = []map[string]any{
					{"type": "Ready", "status": "True"},
					{"type": "ParamsReady", "status": "False"},
				}
				f.gets["networking.gke.io/v1/Network//rdma-1"] = n
			},
			wantOutput: "rdma-1",
		},
		{
			name: "nccl-rdma-installer DaemonSet not fully rolled out fails closed",
			mutate: func(f *fakeFetcher) {
				f.gets["apps/v1/DaemonSet/kube-system/nccl-rdma-installer"] = map[string]any{
					"apiVersion": "apps/v1",
					"kind":       "DaemonSet",
					"metadata":   map[string]any{"name": "nccl-rdma-installer", "namespace": "kube-system", "generation": 2},
					"status":     map[string]any{"desiredNumberScheduled": 2, "numberReady": 1, "updatedNumberScheduled": 1, "observedGeneration": 2},
				}
			},
			wantOutput: "DaemonSet",
		},
		{
			// numberReady alone can't tell a fully-current rollout from one
			// where a node still runs the previous revision's pod: that pod
			// reports Ready too, so desired/ready both read 2/2 while only 1
			// node has the new revision. updatedNumberScheduled is the field
			// that catches it.
			name: "nccl-rdma-installer DaemonSet with a stale-revision node fails closed",
			mutate: func(f *fakeFetcher) {
				f.gets["apps/v1/DaemonSet/kube-system/nccl-rdma-installer"] = map[string]any{
					"apiVersion": "apps/v1",
					"kind":       "DaemonSet",
					"metadata":   map[string]any{"name": "nccl-rdma-installer", "namespace": "kube-system", "generation": 2},
					"status":     map[string]any{"desiredNumberScheduled": 2, "numberReady": 2, "updatedNumberScheduled": 1, "observedGeneration": 2},
				}
			},
			wantOutput: "DaemonSet",
		},
		{
			name: "nccl-rdma-installer DaemonSet status not yet observed at current generation fails closed",
			mutate: func(f *fakeFetcher) {
				f.gets["apps/v1/DaemonSet/kube-system/nccl-rdma-installer"] = map[string]any{
					"apiVersion": "apps/v1",
					"kind":       "DaemonSet",
					"metadata":   map[string]any{"name": "nccl-rdma-installer", "namespace": "kube-system", "generation": 3},
					"status":     map[string]any{"desiredNumberScheduled": 2, "numberReady": 2, "updatedNumberScheduled": 2, "observedGeneration": 2},
				}
			},
			wantOutput: "DaemonSet",
		},
		{
			name: "nccl-rdma-installer pod pending fails closed",
			mutate: func(f *fakeFetcher) {
				f.addList("v1", "Pod", "kube-system", []map[string]any{
					{
						"apiVersion": "v1",
						"kind":       "Pod",
						"metadata": map[string]any{
							"name":      "nccl-rdma-installer-x7f2p",
							"namespace": "kube-system",
							"labels":    map[string]any{"k8s-app": "nccl-rdma-installer"},
						},
						"status": map[string]any{"phase": "Pending"},
					},
				})
			},
			wantOutput: "nccl-rdma-installer-x7f2p",
		},
		{
			name: "gb200 node missing the rdma-2 attachment fails closed",
			mutate: func(f *fakeFetcher) {
				f.addList("v1", "Node", "", []map[string]any{
					gb200Node("node-a", allFiveNetworks...),
					gb200Node("node-b", "gvnic-1", "rdma-0", "rdma-1", "rdma-3"), // missing rdma-2
				})
			},
			wantOutput: "node-b",
		},
		{
			name: "gb200 node with no additional network attachments fails closed",
			mutate: func(f *fakeFetcher) {
				f.addList("v1", "Node", "", []map[string]any{
					gb200Node("node-a", allFiveNetworks...),
					gb200Node("node-b"), // only "default", no GB200 networks at all
				})
			},
			wantOutput: "node-b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fetcher := newFakeFetcher()
			fetcher.addGet("networking.gke.io/v1", "GKENetworkParamSet", "", "gvnic-1", gkeNetworkParamSet("gvnic-1", "NetDevice"))
			fetcher.addGet("networking.gke.io/v1", "Network", "", "gvnic-1", gkeNetwork("gvnic-1"))
			for _, n := range []string{"rdma-0", "rdma-1", "rdma-2", "rdma-3"} {
				fetcher.addGet("networking.gke.io/v1", "GKENetworkParamSet", "", n, gkeNetworkParamSet(n, "RDMA"))
				fetcher.addGet("networking.gke.io/v1", "Network", "", n, gkeNetwork(n))
			}
			fetcher.addGet("apps/v1", "DaemonSet", "kube-system", "nccl-rdma-installer", map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "DaemonSet",
				"metadata":   map[string]any{"name": "nccl-rdma-installer", "namespace": "kube-system", "generation": 2},
				"status":     map[string]any{"desiredNumberScheduled": 2, "numberReady": 2, "updatedNumberScheduled": 2, "observedGeneration": 2},
			})
			fetcher.addList("v1", "Pod", "kube-system", nil)
			fetcher.addList("v1", "Node", "", []map[string]any{
				gb200Node("node-a", allFiveNetworks...),
				gb200Node("node-b", allFiveNetworks...),
			})

			if tt.mutate != nil {
				tt.mutate(fetcher)
			}

			result := runChainsawTestInProcess(
				context.Background(), "gke-gb200-rdma", string(data), 2*time.Second, fetcher,
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
