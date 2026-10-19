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

// TestSlinkyDrainerHealthCheckClusterStates runs the slinky-drainer health
// check against the states it must tell apart: each piece node-drainer
// depends on missing or not ready, and the plugin's rollout stalled.
func TestSlinkyDrainerHealthCheckClusterStates(t *testing.T) {
	t.Parallel()

	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	data, err := provider.ReadFile(context.Background(), "checks/slinky-drainer/health-check.yaml")
	if err != nil {
		t.Fatalf("read health check: %v", err)
	}
	content := string(data)

	crd := func(established string) map[string]any {
		return map[string]any{
			"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
			"metadata": map[string]any{"name": "drainrequests.nvsentinel.nvidia.com"},
			"status": map[string]any{"conditions": []any{
				map[string]any{"type": "Established", "status": established},
			}},
		}
	}
	configMap := func(binaryData map[string]any) map[string]any {
		cm := map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "slinky-drain-template", "namespace": "nvsentinel"},
		}
		if binaryData != nil {
			cm["binaryData"] = binaryData
		}
		return cm
	}
	template := map[string]any{"drain-template.yaml": "YXBpVmVyc2lvbjogdjE="}

	type rollout struct{ spec, replicas, updated, available, generation, observed int64 }
	deployment := func(r rollout) map[string]any {
		return map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]any{"name": "slinky-drainer", "namespace": "nvsentinel", "generation": r.generation},
			"spec":     map[string]any{"replicas": r.spec},
			"status": map[string]any{
				"replicas": r.replicas, "updatedReplicas": r.updated,
				"availableReplicas": r.available, "observedGeneration": r.observed,
			},
		}
	}
	healthy := rollout{spec: 1, replicas: 1, updated: 1, available: 1, generation: 2, observed: 2}

	tests := []struct {
		name       string
		crd        map[string]any
		configMap  map[string]any
		deployment map[string]any
		wantPass   bool
	}{
		{name: "all present and rolled out → pass", crd: crd("True"), configMap: configMap(template),
			deployment: deployment(healthy), wantPass: true},
		{name: "CRD missing → fail", configMap: configMap(template), deployment: deployment(healthy)},
		{name: "CRD not established → fail", crd: crd("False"), configMap: configMap(template),
			deployment: deployment(healthy)},
		{name: "template ConfigMap missing → fail", crd: crd("True"), deployment: deployment(healthy)},
		{name: "template key missing → fail", crd: crd("True"),
			configMap: configMap(map[string]any{"other.yaml": "eA=="}), deployment: deployment(healthy)},
		{name: "plugin missing → fail", crd: crd("True"), configMap: configMap(template)},
		{name: "no replica available → fail", crd: crd("True"), configMap: configMap(template),
			deployment: deployment(rollout{spec: 1, replicas: 1, updated: 1, available: 0, generation: 2, observed: 2})},
		{
			// The new pod is unready and the old one keeps availability satisfied.
			name: "stalled upgrade (old pod still serving) → fail", crd: crd("True"), configMap: configMap(template),
			deployment: deployment(rollout{spec: 1, replicas: 2, updated: 1, available: 1, generation: 2, observed: 2}),
		},
		{name: "controller has not observed the latest spec → fail", crd: crd("True"), configMap: configMap(template),
			deployment: deployment(rollout{spec: 1, replicas: 1, updated: 1, available: 1, generation: 3, observed: 2})},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeFetcher()
			if tt.crd != nil {
				f.addGet("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "drainrequests.nvsentinel.nvidia.com", tt.crd)
			}
			if tt.configMap != nil {
				f.addGet("v1", "ConfigMap", "nvsentinel", "slinky-drain-template", tt.configMap)
			}
			if tt.deployment != nil {
				f.addGet("apps/v1", "Deployment", "nvsentinel", "slinky-drainer", tt.deployment)
			}

			res := runChainsawTestInProcess(context.Background(), "slinky-drainer", content, 2*time.Second, f)
			if res.Passed != tt.wantPass {
				t.Fatalf("passed = %v, want %v (output: %s)", res.Passed, tt.wantPass, res.Output)
			}
		})
	}
}

// TestSlinkyDrainerCRDsHealthCheckClusterStates runs the slinky-drainer-crds
// health check against a DrainRequest CRD that is Established, not yet
// Established, and missing.
func TestSlinkyDrainerCRDsHealthCheckClusterStates(t *testing.T) {
	t.Parallel()

	provider := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	data, err := provider.ReadFile(context.Background(), "checks/slinky-drainer-crds/health-check.yaml")
	if err != nil {
		t.Fatalf("read health check: %v", err)
	}

	tests := []struct {
		name        string
		established string
		wantPass    bool
	}{
		{name: "CRD Established → pass", established: "True", wantPass: true},
		{name: "CRD not Established → fail", established: "False"},
		{name: "CRD missing → fail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeFetcher()
			if tt.established != "" {
				f.addGet("apiextensions.k8s.io/v1", "CustomResourceDefinition", "", "drainrequests.nvsentinel.nvidia.com",
					map[string]any{
						"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
						"metadata": map[string]any{"name": "drainrequests.nvsentinel.nvidia.com"},
						"status": map[string]any{"conditions": []any{
							map[string]any{"type": "Established", "status": tt.established},
						}},
					})
			}
			res := runChainsawTestInProcess(context.Background(), "slinky-drainer-crds", string(data), 2*time.Second, f)
			if res.Passed != tt.wantPass {
				t.Fatalf("passed = %v, want %v (output: %s)", res.Passed, tt.wantPass, res.Output)
			}
		})
	}
}
