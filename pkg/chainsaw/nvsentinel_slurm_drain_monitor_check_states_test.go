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
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestNVSentinelSlurmDrainMonitorHealthCheckClusterStates runs the
// slurm-drain-monitor health check against the rollout and RBAC states it
// must distinguish. The stalled-upgrade row is the case an old ready pod hides:
// availability is satisfied while the new revision never becomes ready.
func TestNVSentinelSlurmDrainMonitorHealthCheckClusterStates(t *testing.T) {
	t.Parallel()

	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	data, err := provider.ReadFile(context.Background(), "checks/nvsentinel-slurm-drain-monitor/health-check.yaml")
	if err != nil {
		t.Fatalf("read health check: %v", err)
	}
	content := string(data)

	type rollout struct{ spec, replicas, updated, available, generation, observed int64 }
	deployment := func(r rollout) map[string]any {
		return map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": "slurm-drain-monitor", "namespace": "nvsentinel", "generation": r.generation},
			"spec":     map[string]any{"replicas": r.spec},
			"status": map[string]any{
				"replicas": r.replicas, "updatedReplicas": r.updated,
				"availableReplicas": r.available, "observedGeneration": r.observed,
			},
		}
	}
	healthy := rollout{spec: 1, replicas: 1, updated: 1, available: 1, generation: 2, observed: 2}

	role := func(rules ...map[string]any) map[string]any {
		items := make([]any, 0, len(rules))
		for _, r := range rules {
			items = append(items, r)
		}
		return map[string]any{
			"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRole",
			"metadata": map[string]any{"name": "slurm-drain-monitor"},
			"rules":    items,
		}
	}
	rule := func(resources []any, verbs ...any) map[string]any {
		return map[string]any{"apiGroups": []any{""}, "resources": resources, "verbs": verbs}
	}
	readOnly := role(rule([]any{"pods"}, "get", "list", "watch"))

	tests := []struct {
		name       string
		deployment rollout
		role       map[string]any
		wantPass   bool
	}{
		{name: "healthy rollout, read-only RBAC → pass", deployment: healthy, role: readOnly, wantPass: true},
		{
			// desired=1, updated=1, available=1, total=2: the new pod is
			// unready and the old one keeps availability satisfied.
			name:       "stalled upgrade (old pod still serving) → fail",
			deployment: rollout{spec: 1, replicas: 2, updated: 1, available: 1, generation: 2, observed: 2},
			role:       readOnly,
		},
		{
			name:       "not all replicas updated → fail",
			deployment: rollout{spec: 1, replicas: 1, updated: 0, available: 1, generation: 2, observed: 2},
			role:       readOnly,
		},
		{
			name:       "no replica available → fail",
			deployment: rollout{spec: 1, replicas: 1, updated: 1, available: 0, generation: 2, observed: 2},
			role:       readOnly,
		},
		{
			name:       "controller has not observed the latest spec → fail",
			deployment: rollout{spec: 1, replicas: 1, updated: 1, available: 1, generation: 3, observed: 2},
			role:       readOnly,
		},
		{
			name:       "RBAC grants a write verb → fail",
			deployment: healthy,
			role:       role(rule([]any{"pods"}, "get", "list", "watch", "delete")),
		},
		{
			name:       "RBAC grants a second resource → fail",
			deployment: healthy,
			role:       role(rule([]any{"pods", "nodes"}, "get", "list", "watch")),
		},
		{
			name:       "RBAC adds a second rule → fail",
			deployment: healthy,
			role:       role(rule([]any{"pods"}, "get", "list", "watch"), rule([]any{"nodes"}, "patch")),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeFetcher()
			f.addGet("apps/v1", "Deployment", "nvsentinel", "slurm-drain-monitor", deployment(tt.deployment))
			f.addGet("rbac.authorization.k8s.io/v1", "ClusterRole", "", "slurm-drain-monitor", tt.role)

			res := runChainsawTestInProcess(context.Background(), "nvsentinel-slurm-drain-monitor", content,
				2*time.Second, f)
			if res.Passed != tt.wantPass {
				t.Fatalf("passed = %v, want %v (output: %s)", res.Passed, tt.wantPass, res.Output)
			}
		})
	}
}
