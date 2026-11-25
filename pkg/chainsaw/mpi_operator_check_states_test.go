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

	"aicr.run/pkg/recipe"
)

// TestMPIOperatorHealthCheckClusterStates runs the mpi-operator health check
// against the states it must tell apart: nothing installed, Training Operator
// v1's CRD standing in under the same name, the operator missing, not rolled
// out or stalled mid-upgrade, and the leader-election Lease absent or unheld.
func TestMPIOperatorHealthCheckClusterStates(t *testing.T) {
	t.Parallel()

	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	data, err := provider.ReadFile(context.Background(), "checks/mpi-operator/health-check.yaml")
	if err != nil {
		t.Fatalf("read health check: %v", err)
	}
	content := string(data)

	crd := func(established string, versions ...map[string]any) map[string]any {
		served := make([]any, 0, len(versions))
		for _, v := range versions {
			served = append(served, v)
		}
		return map[string]any{
			"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
			"metadata": map[string]any{"name": "mpijobs.kubeflow.org"},
			"spec":     map[string]any{"group": "kubeflow.org", "versions": served},
			"status": map[string]any{"conditions": []any{
				map[string]any{"type": "Established", "status": established},
			}},
		}
	}
	version := func(name string, served bool) map[string]any {
		return map[string]any{"name": name, "served": served, "storage": served}
	}
	v2 := crd("True", version("v2beta1", true))

	type rollout struct{ spec, replicas, updated, available, generation, observed int64 }
	deployment := func(r rollout) map[string]any {
		return map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": "mpi-operator", "namespace": "mpi-operator", "generation": r.generation},
			"spec":     map[string]any{"replicas": r.spec},
			"status": map[string]any{
				"replicas": r.replicas, "updatedReplicas": r.updated,
				"availableReplicas": r.available, "observedGeneration": r.observed,
			},
		}
	}
	healthy := deployment(rollout{spec: 1, replicas: 1, updated: 1, available: 1, generation: 2, observed: 2})

	lease := func(spec map[string]any) map[string]any {
		return map[string]any{
			"apiVersion": "coordination.k8s.io/v1", "kind": "Lease",
			"metadata": map[string]any{"name": "mpi-operator", "namespace": "mpi-operator"},
			"spec":     spec,
		}
	}
	held := lease(map[string]any{"holderIdentity": "mpi-operator-7c9f_0d1e"})

	tests := []struct {
		name       string
		crd        map[string]any
		deployment map[string]any
		lease      map[string]any
		wantPass   bool
	}{
		{name: "CRD serves v2beta1, operator rolled out, Lease held → pass", crd: v2, deployment: healthy, lease: held, wantPass: true},
		{name: "nothing installed → fail"},
		{name: "CRD missing → fail", deployment: healthy, lease: held},
		{name: "CRD not established → fail", crd: crd("False", version("v2beta1", true)), deployment: healthy, lease: held},
		{
			// What an upstream Training Operator v1 install leaves behind.
			name: "same-named CRD serving v1 only → fail", crd: crd("True", version("v1", true)),
			deployment: healthy, lease: held,
		},
		{name: "v2beta1 present but not served → fail", crd: crd("True", version("v2beta1", false)),
			deployment: healthy, lease: held},
		{name: "operator missing → fail", crd: v2, lease: held},
		{name: "no replica available → fail", crd: v2, lease: held,
			deployment: deployment(rollout{spec: 1, replicas: 1, updated: 1, available: 0, generation: 2, observed: 2})},
		{
			// The new pod is unready and the old one keeps availability satisfied.
			name: "stalled upgrade (old pod still serving) → fail", crd: v2, lease: held,
			deployment: deployment(rollout{spec: 1, replicas: 2, updated: 1, available: 1, generation: 2, observed: 2}),
		},
		{name: "new spec not yet observed → fail", crd: v2, lease: held,
			deployment: deployment(rollout{spec: 1, replicas: 1, updated: 1, available: 1, generation: 3, observed: 2})},
		{name: "Lease missing → fail", crd: v2, deployment: healthy},
		{name: "Lease without a holder → fail", crd: v2, deployment: healthy, lease: lease(map[string]any{})},
		{name: "Lease with an empty holder → fail", crd: v2, deployment: healthy,
			lease: lease(map[string]any{"holderIdentity": ""})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeFetcher()
			if tt.crd != nil {
				f.addGet("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "mpijobs.kubeflow.org", tt.crd)
			}
			if tt.deployment != nil {
				f.addGet("apps/v1", "Deployment", "mpi-operator", "mpi-operator", tt.deployment)
			}
			if tt.lease != nil {
				f.addGet("coordination.k8s.io/v1", "Lease", "mpi-operator", "mpi-operator", tt.lease)
			}

			res := runChainsawTestInProcess(context.Background(), "mpi-operator", content, 2*time.Second, f)
			if res.Passed != tt.wantPass {
				t.Fatalf("passed = %v, want %v (output: %s)", res.Passed, tt.wantPass, res.Output)
			}
		})
	}
}
