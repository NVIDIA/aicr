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

// External test package: pkg/recipe imports this package for the embedded
// catalog FS.
package recipes_test

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestGenericGB300DevicePluginMOFEDStaysOff pins MOFED_ENABLED off in the
// generic GB300 recipe's effective device-plugin env. The network-operator's
// device plugin owns RDMA injection on this bare-metal shape; MOFED_ENABLED
// on the GPU Operator's plugin would inject every host ibverbs device into
// GPU pods alongside it. The recipe relies on the plugin default (off), so a
// future base-values or overlay edit that sets it true must fail here, not
// on a cluster.
func TestGenericGB300DevicePluginMOFEDStaysOff(t *testing.T) {
	t.Parallel()

	criteria := &recipe.Criteria{
		Service:     recipe.CriteriaServiceGeneric,
		Accelerator: recipe.CriteriaAcceleratorGB300,
		OS:          recipe.CriteriaOSUbuntu,
		Intent:      recipe.CriteriaIntentTraining,
	}
	result, err := recipe.NewBuilder().BuildFromCriteriaWithProfile(t.Context(), criteria, "")
	if err != nil {
		t.Fatalf("BuildFromCriteriaWithProfile: %v", err)
	}

	for i := range result.ComponentRefs {
		ref := &result.ComponentRefs[i]
		if ref.Name != "gpu-operator" {
			continue
		}
		values, err := recipe.GetComponentValuesWithContext(t.Context(), nil, ref)
		if err != nil {
			t.Fatalf("resolve effective gpu-operator values: %v", err)
		}
		dp, _ := values["devicePlugin"].(map[string]any)
		env, _ := dp["env"].([]any)
		for _, e := range env {
			entry, _ := e.(map[string]any)
			if entry["name"] == "MOFED_ENABLED" && entry["value"] != "false" {
				t.Fatalf("gpu-operator devicePlugin.env sets MOFED_ENABLED=%v: the "+
					"network-operator's device plugin owns RDMA injection on generic "+
					"bare metal — keep it off", entry["value"])
			}
		}
		return
	}
	t.Fatal("gpu-operator component not found in the resolved generic GB300 recipe")
}
