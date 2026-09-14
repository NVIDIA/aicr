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

// TestGenericGB300DevicePluginMOFEDStaysOff pins MOFED_ENABLED to an
// explicit "false" in the generic GB300 recipe's effective device-plugin
// env. The network-operator's device plugin owns RDMA injection on this
// bare-metal shape; MOFED_ENABLED on the GPU Operator's plugin would inject
// every host ibverbs device into GPU pods alongside it. An ABSENT key also
// fails: the GPU Operator infers an omitted MOFED_ENABLED to true when GDS
// enablement loads nvidia_fs, so only the explicit pin is fail-closed.
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
		pinnedOff := false
		for _, e := range env {
			entry, _ := e.(map[string]any)
			if entry["name"] != "MOFED_ENABLED" {
				continue
			}
			if entry["value"] != "false" {
				t.Fatalf("gpu-operator devicePlugin.env sets MOFED_ENABLED=%v: the "+
					"network-operator's device plugin owns RDMA injection on generic "+
					"bare metal — keep it off", entry["value"])
			}
			pinnedOff = true
		}
		if !pinnedOff {
			t.Fatal("gpu-operator devicePlugin.env does not pin MOFED_ENABLED=\"false\": " +
				"an omitted key is inferred true when GDS enablement loads nvidia_fs — " +
				"the explicit pin is required")
		}
		return
	}
	t.Fatal("gpu-operator component not found in the resolved generic GB300 recipe")
}

// TestGenericGB300MaintenanceOperatorSizing pins the network-operator values
// that the generic GB300 recipe owns for the maintenance operator: the
// subchart deployment is sized above its 128Mi default, and the network
// operator keeps its own node-drain path (operator.maintenanceOperator stays
// at chart defaults). The operator is enabled for nicConfigurationOperator,
// not to take over OFED upgrades from ofedDriver.upgradePolicy.
func TestGenericGB300MaintenanceOperatorSizing(t *testing.T) {
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
		if ref.Name != "network-operator" {
			continue
		}
		values, err := recipe.GetComponentValuesWithContext(t.Context(), nil, ref)
		if err != nil {
			t.Fatalf("resolve effective network-operator values: %v", err)
		}

		mo, _ := values["maintenanceOperator"].(map[string]any)
		if mo["enabled"] != true {
			t.Fatalf("maintenanceOperator.enabled = %v, want true (nicConfigurationOperator requires it)", mo["enabled"])
		}

		sub, _ := values["maintenance-operator-chart"].(map[string]any)
		subOp, _ := sub["operator"].(map[string]any)
		res, _ := subOp["resources"].(map[string]any)
		limits, _ := res["limits"].(map[string]any)
		if limits["memory"] != "1Gi" {
			t.Fatalf("maintenance-operator-chart.operator.resources.limits.memory = %v, want 1Gi "+
				"(the subchart default 128Mi is below the operator's working set)", limits["memory"])
		}

		op, _ := values["operator"].(map[string]any)
		if opMO, ok := op["maintenanceOperator"].(map[string]any); ok {
			if _, set := opMO["useRequestor"]; set {
				t.Fatalf("operator.maintenanceOperator.useRequestor = %v: the recipe must leave "+
					"node-drain ownership at the chart default; OFED upgrades use ofedDriver.upgradePolicy",
					opMO["useRequestor"])
			}
		}
		return
	}
	t.Fatal("network-operator component not found in the resolved generic GB300 recipe")
}
