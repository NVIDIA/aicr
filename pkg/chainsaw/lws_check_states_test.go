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

	"aicr.run/pkg/recipe"
)

// TestLWSHealthCheckClusterStates drives the shipped lws health check through
// the in-process executor against synthetic cluster states.
//
// Each failing case differs from the converged state in one field and pins
// wantOutput to the expression or pod that must report it, so a case cannot
// keep passing because a different assertion caught the state.
func TestLWSHealthCheckClusterStates(t *testing.T) {
	t.Parallel()

	const (
		namespace  = "lws-system"
		deployment = "lws-controller-manager"
		crd        = "leaderworkersets.leaderworkerset.x-k8s.io"
	)

	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	data, err := provider.ReadFile(context.Background(), "checks/lws/health-check.yaml")
	if err != nil {
		t.Fatalf("read health check: %v", err)
	}

	// status holds the Deployment status fields the check reads. They are all
	// omitempty, so a nil pointer models a field the API server left out.
	type status struct {
		replicas, available, updated, observedGeneration *int64
	}
	converged := status{
		replicas: new(int64(1)), available: new(int64(1)),
		updated: new(int64(1)), observedGeneration: new(int64(2)),
	}

	pod := func(name, phase string, containers, initContainers []any) map[string]any {
		st := map[string]any{"phase": phase}
		if containers != nil {
			st["containerStatuses"] = containers
		}
		if initContainers != nil {
			st["initContainerStatuses"] = initContainers
		}
		return map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": name, "namespace": namespace},
			"status":   st,
		}
	}
	running := map[string]any{"name": "manager", "state": map[string]any{"running": map[string]any{}}}
	waiting := func(reason string) map[string]any {
		return map[string]any{"name": "manager", "state": map[string]any{"waiting": map[string]any{"reason": reason}}}
	}
	healthyPod := pod("lws-controller-manager-a", "Running", []any{running}, nil)

	tests := []struct {
		name           string
		omitCRD        bool
		crdEstablished string
		omitDeployment bool
		desired        int64
		generation     int64
		status         status
		pods           []map[string]any
		wantPass       bool
		wantOutput     string
	}{
		{
			name:    "converged controller passes",
			desired: 1, generation: 2, status: converged,
			pods:     []map[string]any{healthyPod},
			wantPass: true,
		},
		{
			name:    "missing CRD fails",
			omitCRD: true,
			desired: 1, generation: 2, status: converged,
			pods:       []map[string]any{healthyPod},
			wantOutput: crd,
		},
		{
			name:           "CRD not Established fails",
			crdEstablished: "False",
			desired:        1, generation: 2, status: converged,
			pods:       []map[string]any{healthyPod},
			wantOutput: crd,
		},
		{
			// The empty-namespace case: with no pods, every pod step passes
			// vacuously, so only the named Deployment assert can fail it.
			name:           "missing Deployment fails",
			omitDeployment: true,
			wantOutput:     "Deployment",
		},
		{
			name:    "scaled to zero fails",
			desired: 0, generation: 2,
			status:     status{observedGeneration: new(int64(2))},
			wantOutput: "(spec.replicas || `0`) > `0`",
		},
		{
			// A rollout held by maxUnavailable=0: the old pod is still the
			// available one and its unready replacement already counts as
			// updated. Only status.replicas, which counts pods across every
			// ReplicaSet, shows the second pod.
			name:    "old pod still counted mid-rollout fails",
			desired: 1, generation: 2,
			status: status{
				replicas: new(int64(2)), available: new(int64(1)),
				updated: new(int64(1)), observedGeneration: new(int64(2)),
			},
			pods:       []map[string]any{healthyPod, pod("lws-controller-manager-b", "Running", []any{running}, nil)},
			wantOutput: "status.replicas",
		},
		{
			name:    "no available replica fails",
			desired: 1, generation: 2,
			status: status{
				replicas: new(int64(1)), available: new(int64(0)),
				updated: new(int64(1)), observedGeneration: new(int64(2)),
			},
			pods:       []map[string]any{healthyPod},
			wantOutput: "status.availableReplicas",
		},
		{
			// The old ReplicaSet's pod is the available one and the new
			// revision has created nothing yet.
			name:    "no updated replica fails",
			desired: 1, generation: 2,
			status: status{
				replicas: new(int64(1)), available: new(int64(1)),
				updated: new(int64(0)), observedGeneration: new(int64(2)),
			},
			pods:       []map[string]any{healthyPod},
			wantOutput: "status.updatedReplicas",
		},
		{
			// The API server has accepted a new spec that the Deployment
			// controller has not observed, so every count still describes the
			// previous generation.
			name:    "stale observedGeneration fails",
			desired: 1, generation: 3, status: converged,
			pods:       []map[string]any{healthyPod},
			wantOutput: "status.observedGeneration",
		},
		{
			// A Deployment the controller has not reconciled yet: every status
			// field is absent, which is what the `|| `0`` defaults are for.
			name:    "absent status fails",
			desired: 1, generation: 1,
			wantOutput: "Deployment",
		},
		{
			name:    "Pending pod fails",
			desired: 1, generation: 2, status: converged,
			pods:       []map[string]any{healthyPod, pod("stuck-pending", "Pending", nil, nil)},
			wantOutput: "stuck-pending",
		},
		{
			name:    "crash-looping pod fails",
			desired: 1, generation: 2, status: converged,
			pods: []map[string]any{
				healthyPod,
				pod("crash-looping", "Running", []any{waiting("CrashLoopBackOff")}, nil),
			},
			wantOutput: "crash-looping",
		},
		{
			// Two containers, so a one-element slice literal in the check
			// would not match this pod.
			name:    "ImagePullBackOff in a two-container pod fails",
			desired: 1, generation: 2, status: converged,
			pods: []map[string]any{
				healthyPod,
				pod("image-pull", "Running", []any{running, waiting("ImagePullBackOff")}, nil),
			},
			wantOutput: "image-pull",
		},
		{
			// Phase stays Running here so that only the init container step
			// can report the pod.
			name:    "crash-looping init container fails",
			desired: 1, generation: 2, status: converged,
			pods: []map[string]any{
				healthyPod,
				pod("init-crash", "Running", []any{running}, []any{waiting("CrashLoopBackOff")}),
			},
			wantOutput: "init-crash",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fetcher := newFakeFetcher()
			if !tt.omitCRD {
				established := tt.crdEstablished
				if established == "" {
					established = "True"
				}
				fetcher.addGet("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", crd, map[string]any{
					"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
					"metadata": map[string]any{"name": crd},
					"status": map[string]any{"conditions": []any{
						map[string]any{"type": "Established", "status": established},
					}},
				})
			}
			if !tt.omitDeployment {
				st := map[string]any{}
				for field, value := range map[string]*int64{
					"replicas":           tt.status.replicas,
					"availableReplicas":  tt.status.available,
					"updatedReplicas":    tt.status.updated,
					"observedGeneration": tt.status.observedGeneration,
				} {
					if value != nil {
						st[field] = *value
					}
				}
				fetcher.addGet("apps/v1", "Deployment", namespace, deployment, map[string]any{
					"apiVersion": "apps/v1", "kind": "Deployment",
					"metadata": map[string]any{"name": deployment, "namespace": namespace, "generation": tt.generation},
					"spec":     map[string]any{"replicas": tt.desired},
					"status":   st,
				})
			}
			fetcher.addList("v1", "Pod", namespace, tt.pods)

			result := runChainsawTestInProcess(context.Background(), "lws", string(data), 2*time.Second, fetcher)
			if result.Passed != tt.wantPass {
				t.Fatalf("passed = %v, want %v (output: %s)", result.Passed, tt.wantPass, result.Output)
			}
			if tt.wantOutput != "" && !strings.Contains(result.Output, tt.wantOutput) {
				t.Fatalf("output = %q, want it to name %q: the wrong assertion caught this state",
					result.Output, tt.wantOutput)
			}
		})
	}
}
