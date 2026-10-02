// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package recipes

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/manifest"
)

// renderGB200RDMAInstaller renders the gIB installer DaemonSet manifest with
// the given component values and returns the parsed DaemonSet pod spec.
func renderGB200RDMAInstaller(t *testing.T, values map[string]any) map[string]any {
	t.Helper()
	raw, err := FS.ReadFile("components/gke-gb200-rdma/manifests/nccl-gib-installer-arm64.yaml")
	if err != nil {
		t.Fatalf("read nccl-gib-installer-arm64.yaml: %v", err)
	}
	out, err := manifest.Render(raw, manifest.RenderInput{
		ComponentName: "gke-gb200-rdma",
		Values:        values,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rendered YAML does not parse: %v\n%s", err, out)
	}
	specNode, ok := doc["spec"].(map[string]any)
	if !ok {
		t.Fatalf("DaemonSet spec not found in rendered manifest:\n%s", out)
	}
	templateNode, ok := specNode["template"].(map[string]any)
	if !ok {
		t.Fatalf("DaemonSet spec.template not found in rendered manifest:\n%s", out)
	}
	spec, ok := templateNode["spec"].(map[string]any)
	if !ok {
		t.Fatalf("DaemonSet pod spec not found in rendered manifest:\n%s", out)
	}
	return spec
}

// TestGB200RDMAInstallerAcceleratedNodeSelector verifies
// --accelerated-node-selector (values["acceleratedNodeSelector"], see
// registry.yaml's nodeSelectorPaths) scopes the DaemonSet to the selected
// pool instead of running on every GB200/ARM64 node, and that an unset
// selector omits the nodeSelector field rather than rendering an empty one.
func TestGB200RDMAInstallerAcceleratedNodeSelector(t *testing.T) {
	tests := []struct {
		name         string
		values       map[string]any
		wantSelector map[string]any
	}{
		{
			name: "present scopes render",
			values: map[string]any{
				"acceleratedNodeSelector": map[string]any{"cloud.google.com/gke-nodepool": "a4x-pool-a"},
			},
			wantSelector: map[string]any{"cloud.google.com/gke-nodepool": "a4x-pool-a"},
		},
		{
			name:         "absent omits field",
			values:       map[string]any{},
			wantSelector: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := renderGB200RDMAInstaller(t, tt.values)
			nodeSelector, present := spec["nodeSelector"]
			if tt.wantSelector == nil {
				if present {
					t.Errorf("expected no nodeSelector field when acceleratedNodeSelector is unset, got spec: %v", spec)
				}
				return
			}
			got, ok := nodeSelector.(map[string]any)
			if !ok {
				t.Fatalf("expected rendered pod spec to carry nodeSelector, got spec: %v", spec)
			}
			for k, want := range tt.wantSelector {
				if got[k] != want {
					t.Errorf("nodeSelector[%s] = %v, want %v", k, got[k], want)
				}
			}
		})
	}
}

// TestGB200RDMARuntimeRendersFabricWiring pins the GPUDirect-RDMA settings a
// TrainJob cannot supply for itself, and the bundler-injected node scheduling
// that lets it submit without podTemplateOverrides. A job that loses NCCL_NET
// or the interfaces annotation falls back to TCP and runs slowly rather than
// failing, so neither is left to the bundle checksum alone.
func TestGB200RDMARuntimeRendersFabricWiring(t *testing.T) {
	content, err := FS.ReadFile(
		"components/kubeflow-trainer/manifests/torch-distributed-rdma-cluster-training-runtime.yaml")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	rendered, err := manifest.Render(content, manifest.RenderInput{
		ComponentName: "kubeflow-trainer",
		Namespace:     "kubeflow",
		ChartName:     "kubeflow-trainer",
		ChartVersion:  "2.2.0",
		Values: map[string]any{
			"acceleratedNodeSelector": map[string]any{"nodeGroup": "customer-gpu"},
			"acceleratedTolerations": []any{
				map[string]any{"key": "nvidia.com/gpu", "operator": "Exists", "effect": "NoSchedule"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	out := string(rendered)

	for _, want := range []string{
		`networking.gke.io/interfaces: '[{"interfaceName":"eth0","network":"default"},` +
			`{"interfaceName":"eth1","network":"gvnic-1"},{"interfaceName":"eth2","network":"rdma-0"},` +
			`{"interfaceName":"eth3","network":"rdma-1"},{"interfaceName":"eth4","network":"rdma-2"},` +
			`{"interfaceName":"eth5","network":"rdma-3"}]'`,
		"name: NCCL_NET",
		"value: gIB",
		// A misspelled key in either with block renders nothing and reports no
		// error, making the bundler's scheduling flags a silent no-op.
		"nodeGroup: customer-gpu",
		"key: nvidia.com/gpu",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered manifest missing %q", want)
		}
	}
	if strings.Contains(out, "{{") {
		t.Error("rendered manifest contains unresolved template actions")
	}
}
