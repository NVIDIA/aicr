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

package recipe

import (
	"slices"
	"strings"
	"testing"
)

const (
	gpuOperatorComponentName       = "gpu-operator"
	nriManagementNamespacesEnvName = "NRI_MANAGEMENT_CDI_DEVICE_NAMESPACES"
)

// TestRKE2NVSentinelNRIPluginMode pins the paired NRI wiring on the generic
// RKE2 leaves (#3041). The rke2 root runs the GPU Operator with the NRI
// plugin, which never creates the `nvidia` RuntimeClass, so
// metadata-collector must request the management CDI device instead
// (NVIDIA/NVSentinel#1949), and the toolkit must allow nvsentinel's
// namespace or it skips the injection silently and the collector
// crash-loops with NVML ERROR_LIBRARY_NOT_FOUND.
func TestRKE2NVSentinelNRIPluginMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		intent CriteriaIntentType
	}{
		{name: "rke2-training", intent: CriteriaIntentTraining},
		{name: "rke2-inference", intent: CriteriaIntentInference},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			criteria := &Criteria{Service: CriteriaServiceRKE2, Intent: tt.intent}
			result, err := NewBuilder().BuildFromCriteriaWithProfile(t.Context(), criteria, "")
			if err != nil {
				t.Fatalf("BuildFromCriteriaWithProfile: %v", err)
			}

			sentinelRef := result.GetComponentRef(nvsentinelComponent)
			if sentinelRef == nil || !sentinelRef.IsEnabled() {
				t.Fatal("nvsentinel missing or disabled — the assertions below would be vacuous")
			}
			if sentinelRef.Namespace == "" {
				t.Fatal("nvsentinel resolved without a namespace — cannot check the toolkit allowlist")
			}

			gpuOpValues, err := result.GetValuesForComponentWithContext(t.Context(), gpuOperatorComponentName)
			if err != nil {
				t.Fatalf("GetValuesForComponentWithContext(gpu-operator): %v", err)
			}
			nri, ok := nestedBool(gpuOpValues, "cdi", "nriPluginEnabled")
			if !ok || !nri {
				t.Fatalf("gpu-operator cdi.nriPluginEnabled = %v (set=%v), want true — the paired metadata-collector setting below only applies under NRI", nri, ok)
			}
			toolkit, _ := gpuOpValues["toolkit"].(map[string]any)
			env, _ := toolkit["env"].([]any)
			if !envListContains(env, nriManagementNamespacesEnvName, sentinelRef.Namespace) {
				t.Fatalf("gpu-operator toolkit.env %s does not list %q; the NRI plugin then skips metadata-collector's management device and the collector crash-loops (#3041). Got: %v",
					nriManagementNamespacesEnvName, sentinelRef.Namespace, env)
			}

			sentinelValues, err := result.GetValuesForComponentWithContext(t.Context(), nvsentinelComponent)
			if err != nil {
				t.Fatalf("GetValuesForComponentWithContext(nvsentinel): %v", err)
			}
			if enabled, set := nestedMetadataCollectorEnabled(sentinelValues); set && !enabled {
				t.Fatal("global.metadataCollector.enabled = false — the NRI assertion below would pass against no DaemonSet")
			}
			collector, ok := sentinelValues["metadata-collector"].(map[string]any)
			if !ok {
				t.Fatal("metadata-collector overrides missing, want nriPlugin.enabled=true under NRI (#3041)")
			}
			if enabled, ok := nestedBool(collector, "nriPlugin", "enabled"); !ok || !enabled {
				t.Fatalf("metadata-collector.nriPlugin.enabled = %v (set=%v), want true: under NRI there is no `nvidia` RuntimeClass (#3041)", enabled, ok)
			}
		})
	}
}

// envListContains reports whether env carries name with a comma-separated
// value that includes item, the format NRI_MANAGEMENT_CDI_DEVICE_NAMESPACES
// takes.
func envListContains(env []any, name, item string) bool {
	for _, entry := range env {
		m, ok := entry.(map[string]any)
		if !ok || m["name"] != name {
			continue
		}
		value, _ := m["value"].(string)
		if slices.Contains(strings.Split(value, ","), item) {
			return true
		}
	}
	return false
}
