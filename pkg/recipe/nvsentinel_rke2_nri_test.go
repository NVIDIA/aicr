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

// TestRKE2NRISettingsPaired enforces the pairing documented at the rke2 root
// on every rke2 catalog leaf, including ones added later, and on the generic
// rke2 training and inference recipes, which resolve but are not catalog
// leaves because the VR200 overlays extend them: metadata-collector
// NRI plugin mode needs the GPU Operator's NRI plugin and the toolkit's
// namespace allowlist, the allowlist appears only with that mode (it grants
// the namespace every GPU), and under NRI the collector needs either that
// mode or a host-mounted NVML. A leaf that turns NRI off but inherits nriPlugin
// crash-loops with NVML ERROR_LIBRARY_NOT_FOUND although the `nvidia`
// RuntimeClass exists.
func TestRKE2NRISettingsPaired(t *testing.T) {
	t.Parallel()

	leaves, err := ResolveLeaves(t.Context(), ResolveLeavesOptions{
		Filter: &Criteria{Service: CriteriaServiceRKE2},
	})
	if err != nil {
		t.Fatalf("ResolveLeaves: %v", err)
	}

	type candidate struct {
		name   string
		result *RecipeResult
	}
	var candidates []candidate
	for _, leaf := range leaves {
		if leaf.Err != nil {
			t.Errorf("%s: resolve: %v", leaf.Entry.Name, leaf.Err)
			continue
		}
		candidates = append(candidates, candidate{leaf.Entry.Name, leaf.Result})
	}
	for _, intent := range []CriteriaIntentType{CriteriaIntentTraining, CriteriaIntentInference} {
		result, err := NewBuilder().BuildFromCriteriaWithProfile(t.Context(), &Criteria{Service: CriteriaServiceRKE2, Intent: intent}, "")
		if err != nil {
			t.Fatalf("rke2 %s: BuildFromCriteriaWithProfile: %v", intent, err)
		}
		candidates = append(candidates, candidate{"rke2-" + string(intent), result})
	}

	checked, nriMode := 0, 0
	for _, c := range candidates {
		name, result := c.name, c.result
		sentinelRef := result.GetComponentRef(nvsentinelComponent)
		if sentinelRef == nil || !sentinelRef.IsEnabled() || result.GetComponentRef(gpuOperatorComponentName) == nil {
			continue
		}
		sentinelValues, err := result.GetValuesForComponentWithContext(t.Context(), nvsentinelComponent)
		if err != nil {
			t.Fatalf("%s: nvsentinel values: %v", name, err)
		}
		if enabled, set := nestedMetadataCollectorEnabled(sentinelValues); set && !enabled {
			continue
		}
		gpuOpValues, err := result.GetValuesForComponentWithContext(t.Context(), gpuOperatorComponentName)
		if err != nil {
			t.Fatalf("%s: gpu-operator values: %v", name, err)
		}
		checked++

		nri, _ := nestedBool(gpuOpValues, "cdi", "nriPluginEnabled")
		collector, _ := sentinelValues["metadata-collector"].(map[string]any)
		collectorNRI, _ := nestedBool(collector, "nriPlugin", "enabled")
		runtimeClass, runtimeSet := collector["runtimeClassName"].(string)
		hostMounts, _ := collector["additionalHostVolumes"].([]any)
		hostMountedNVML := runtimeSet && runtimeClass == "" && len(hostMounts) > 0

		toolkit, _ := gpuOpValues["toolkit"].(map[string]any)
		env, _ := toolkit["env"].([]any)
		allowlisted := envListContains(env, nriManagementNamespacesEnvName, sentinelRef.Namespace)

		switch {
		case !collectorNRI && allowlisted:
			t.Errorf("%s: toolkit.env %s lists %q, but metadata-collector does not use NRI plugin mode; the entry grants that namespace every GPU for nothing (#3041)",
				name, nriManagementNamespacesEnvName, sentinelRef.Namespace)
		case collectorNRI && !nri:
			t.Errorf("%s: metadata-collector.nriPlugin.enabled is true but gpu-operator cdi.nriPluginEnabled is not; no NRI plugin injects the management device (#3041)", name)
		case collectorNRI:
			nriMode++
			if !allowlisted {
				t.Errorf("%s: metadata-collector uses NRI plugin mode but toolkit.env %s does not list %q (#3041)",
					name, nriManagementNamespacesEnvName, sentinelRef.Namespace)
			}
		case nri && !hostMountedNVML:
			t.Errorf("%s: gpu-operator runs the NRI plugin but metadata-collector has neither nriPlugin.enabled nor a cleared runtimeClassName with host-mounted NVML (#3041)", name)
		}
	}
	if checked == 0 || nriMode == 0 {
		t.Fatalf("checked %d rke2 leaves, %d in NRI plugin mode; want both > 0 or the pairing check is vacuous", checked, nriMode)
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
