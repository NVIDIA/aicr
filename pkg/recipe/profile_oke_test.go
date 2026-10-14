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
	"testing"

	"github.com/NVIDIA/aicr/pkg/allocpolicy"
)

func okeCriteria() *Criteria {
	return &Criteria{
		Service:     CriteriaServiceOKE,
		Accelerator: CriteriaAcceleratorL40S,
		OS:          CriteriaOSOracleLinux,
		Intent:      CriteriaIntentTraining,
	}
}

// TestOKENetworkOperatorAddonGate checks that an OKE recipe requires the
// NvidiaNetworkOperator add-on to be absent exactly when it deploys
// network-operator.
func TestOKENetworkOperatorAddonGate(t *testing.T) {
	t.Parallel()

	const gatePath = "K8s.oke-addons.nvidia-network-operator"
	ol, ubuntu := CriteriaOSOracleLinux, CriteriaOSUbuntu
	training, inference := CriteriaIntentTraining, CriteriaIntentInference
	tests := []struct {
		accelerator CriteriaAcceleratorType
		os          CriteriaOSType
		intent      CriteriaIntentType
		platform    CriteriaPlatformType
	}{
		{CriteriaAcceleratorA100, ol, training, ""},
		{CriteriaAcceleratorA100, ubuntu, training, ""},
		{CriteriaAcceleratorA100, ubuntu, training, CriteriaPlatformKubeflow},
		{CriteriaAcceleratorGB200, ol, inference, ""},
		{CriteriaAcceleratorGB200, ol, training, ""},
		{CriteriaAcceleratorGB200, ubuntu, inference, ""},
		{CriteriaAcceleratorGB200, ubuntu, inference, CriteriaPlatformDynamo},
		{CriteriaAcceleratorGB200, ubuntu, training, ""},
		{CriteriaAcceleratorGB200, ubuntu, training, CriteriaPlatformKubeflow},
		{CriteriaAcceleratorL40S, ol, inference, ""},
		{CriteriaAcceleratorL40S, ol, training, ""},
		{CriteriaAcceleratorL40S, ol, training, CriteriaPlatformKubeflow},
	}

	var gated int
	for _, tt := range tests {
		criteria := &Criteria{
			Service:     CriteriaServiceOKE,
			Accelerator: tt.accelerator,
			OS:          tt.os,
			Intent:      tt.intent,
			Platform:    tt.platform,
		}
		result, err := NewBuilder().BuildFromCriteriaWithProfile(t.Context(), criteria, "")
		if err != nil {
			t.Fatalf("%s: BuildFromCriteriaWithProfile() failed: %v", criteria.String(), err)
		}
		deploysNetworkOperator := false
		for _, ref := range result.ComponentRefs {
			if ref.Name == "network-operator" {
				deploysNetworkOperator = true
			}
		}
		var gate *Constraint
		for i := range result.Constraints {
			if result.Constraints[i].Name == gatePath {
				gate = &result.Constraints[i]
			}
		}
		switch {
		case deploysNetworkOperator && gate == nil:
			t.Errorf("%s deploys network-operator without a %s constraint", criteria.String(), gatePath)
		case deploysNetworkOperator && (gate.Value != "absent" || gate.Remediation == ""):
			t.Errorf("%s: %s = %q (remediation set: %v), want absent with remediation",
				criteria.String(), gatePath, gate.Value, gate.Remediation != "")
		case !deploysNetworkOperator && gate != nil:
			t.Errorf("%s gates %s but does not deploy network-operator", criteria.String(), gatePath)
		case deploysNetworkOperator:
			gated++
		}
	}
	if gated == 0 {
		t.Fatal("no OKE recipe deploys network-operator, so the gate invariant checked nothing")
	}

	// Snapshot resolution checks only a leaf's own constraints, so every
	// overlay that inherits the gate must also declare it.
	store, err := loadMetadataStore(t.Context())
	if err != nil {
		t.Fatalf("loadMetadataStore() failed: %v", err)
	}
	for name, overlay := range store.Overlays {
		chain, err := store.resolveInheritanceChain(name)
		if err != nil {
			t.Fatalf("resolveInheritanceChain(%s) failed: %v", name, err)
		}
		for _, ancestor := range chain {
			if ancestor == overlay || !declaresConstraint(ancestor, gatePath) {
				continue
			}
			if !declaresConstraint(overlay, gatePath) {
				t.Errorf("%s inherits %s from %s but does not declare it", name, gatePath, ancestor.Metadata.Name)
			}
		}
	}
}

func declaresConstraint(overlay *RecipeMetadata, name string) bool {
	for _, c := range overlay.Spec.Constraints {
		if c.Name == name {
			return true
		}
	}
	return false
}

// TestOKEGpuStackProfileResolution pins the OKE family conversion: the
// oke-ol overlay declares gpuStack with default oci-managed (the
// Oracle-managed cluster — image driver + the NvidiaGpuPlugin add-on as
// the external advertiser) and alternative operator-managed
// (bring-your-own driverless image, add-on removed; the operator installs
// driver, toolkit, and plugin, and the DRA root moves in lockstep).
// Constraint placement pins the #2363 decision: each value carries the
// canonical control-plane signal K8s.oke-addons.nvidia-gpu-plugin
// (installed/absent) as a durable GENERATION constraint joined into
// spec.constraints — no readiness constraints, no driver-state gates.
func TestOKEGpuStackProfileResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		selection      string
		wantValue      string
		wantAdvertiser string
		wantDriver     bool
		wantToolkit    bool
		wantPlugin     bool
		wantDRARoot    string
		wantAssume     bool
		// wantAddonConstraint: the expected K8s.oke-addons.nvidia-gpu-plugin
		// generation-constraint value for the selected profile value.
		wantAddonConstraint string
		// wantLegacyConstraint: the expected
		// K8s.oke-legacy-plugin.nvidia-gpu-device-plugin generation-constraint
		// value; empty asserts the tripwire is ABSENT (oci-managed must not
		// carry it — the managed add-on reconciles the same DaemonSet name).
		wantLegacyConstraint string
	}{
		{
			name:                 "default selection is oci-managed with the external advertiser",
			selection:            "",
			wantValue:            "oci-managed",
			wantAdvertiser:       allocpolicy.AdvertiserExternal,
			wantDriver:           false,
			wantToolkit:          false,
			wantPlugin:           false,
			wantDRARoot:          "/",
			wantAssume:           true,
			wantAddonConstraint:  "installed",
			wantLegacyConstraint: "",
		},
		{
			name:                 "operator-managed owns driver, toolkit, plugin, and the DRA root",
			selection:            "gpuStack=operator-managed",
			wantValue:            "operator-managed",
			wantAdvertiser:       "",
			wantDriver:           true,
			wantToolkit:          true,
			wantPlugin:           true,
			wantDRARoot:          "/run/nvidia/driver",
			wantAssume:           false,
			wantAddonConstraint:  "absent",
			wantLegacyConstraint: "none",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, err := NewBuilder().BuildFromCriteriaWithProfile(
				t.Context(), okeCriteria(), tt.selection)
			if err != nil {
				t.Fatalf("BuildFromCriteriaWithProfile() failed: %v", err)
			}
			selected := result.Metadata.SelectedProfile
			if selected == nil {
				t.Fatal("metadata.selectedProfile is nil")
				return
			}
			if selected.Name != "gpuStack" || selected.Value != tt.wantValue {
				t.Errorf("selectedProfile = %s=%s, want gpuStack=%s", selected.Name, selected.Value, tt.wantValue)
			}
			if selected.Advertiser != tt.wantAdvertiser {
				t.Errorf("advertiser = %q, want %q", selected.Advertiser, tt.wantAdvertiser)
			}
			if result.APIVersion != RecipeProfileAPIVersion {
				t.Errorf("apiVersion = %q, want %q", result.APIVersion, RecipeProfileAPIVersion)
			}

			// Declaration-wide ownedPaths: identical for every selection.
			wantOwned := map[string][]string{
				"gpu-operator": {
					"devicePlugin.enabled", "driver.enabled",
					"driver.useOpenKernelModules", "enabled",
					"hostPaths.driverInstallDir", "toolkit.enabled",
				},
				"nvidia-dra-driver-gpu": {"enabled", "nvidiaDriverRoot"},
				"nvsentinel": {
					"enabled", "janitor.config.controllers.gpuReset.resetJob.hostDriverRootPath",
					"janitor.config.controllers.gpuReset.serviceManager.name",
					"janitor.config.controllers.gpuReset.serviceManager.spec.apps",
					"janitor.config.controllers.gpuReset.serviceManager.spec.namespace",
					"janitor.config.controllers.gpuReset.serviceManager.spec.restoreTimeout",
					"janitor.config.controllers.gpuReset.serviceManager.spec.teardownTimeout",
					"labeler.assumeDriverInstalled",
				},
			}
			for component, want := range wantOwned {
				got := selected.OwnedPaths[component]
				if len(got) != len(want) {
					t.Errorf("ownedPaths[%s] = %v, want %v", component, got, want)
					continue
				}
				for i := range want {
					if got[i] != want[i] {
						t.Errorf("ownedPaths[%s] = %v, want %v", component, got, want)
						break
					}
				}
			}

			gpuValues, err := result.GetValuesForComponentWithContext(t.Context(), "gpu-operator")
			if err != nil {
				t.Fatalf("GetValuesForComponentWithContext(gpu-operator): %v", err)
			}
			if v, ok := nestedBool(gpuValues, "driver", "enabled"); !ok || v != tt.wantDriver {
				t.Errorf("driver.enabled = %v (set: %v), want %v", v, ok, tt.wantDriver)
			}
			if v, ok := nestedBool(gpuValues, "toolkit", "enabled"); !ok || v != tt.wantToolkit {
				t.Errorf("toolkit.enabled = %v (set: %v), want %v", v, ok, tt.wantToolkit)
			}
			if v, ok := nestedBool(gpuValues, "devicePlugin", "enabled"); !ok || v != tt.wantPlugin {
				t.Errorf("devicePlugin.enabled = %v (set: %v), want %v", v, ok, tt.wantPlugin)
			}

			draValues, err := result.GetValuesForComponentWithContext(t.Context(), "nvidia-dra-driver-gpu")
			if err != nil {
				t.Fatalf("GetValuesForComponentWithContext(nvidia-dra-driver-gpu): %v", err)
			}
			if root, _ := draValues["nvidiaDriverRoot"].(string); root != tt.wantDRARoot {
				t.Errorf("nvidiaDriverRoot = %q, want %q", root, tt.wantDRARoot)
			}

			nvsValues, err := result.GetValuesForComponentWithContext(t.Context(), nvsentinelComponent)
			if err != nil {
				t.Fatalf("GetValuesForComponentWithContext(nvsentinel): %v", err)
			}
			if v, ok := nestedBool(nvsValues, "labeler", "assumeDriverInstalled"); !ok || v != tt.wantAssume {
				t.Errorf("assumeDriverInstalled = %v (set: %v), want %v", v, ok, tt.wantAssume)
			}

			// The canonical control-plane signal (#2363 decision): each
			// value's add-on constraint joins spec.constraints (durable,
			// re-evaluated by the validate pre-flight), and nothing routes
			// to validation.readiness.
			const addonPath = "K8s.oke-addons.nvidia-gpu-plugin"
			var gotAddon string
			const legacyPath = "K8s.oke-legacy-plugin.nvidia-gpu-device-plugin"
			var gotLegacy string
			for _, c := range result.Constraints {
				if c.Name == addonPath {
					gotAddon = c.Value
				}
				if c.Name == legacyPath {
					gotLegacy = c.Value
				}
			}
			if gotAddon != tt.wantAddonConstraint {
				t.Errorf("generation constraint %s = %q, want %q", addonPath, gotAddon, tt.wantAddonConstraint)
			}
			if gotLegacy != tt.wantLegacyConstraint {
				t.Errorf("generation constraint %s = %q, want %q (empty = tripwire must be absent)",
					legacyPath, gotLegacy, tt.wantLegacyConstraint)
			}
			if result.Validation != nil && result.Validation.Readiness != nil {
				for _, c := range result.Validation.Readiness.Constraints {
					if c.Name == addonPath {
						t.Errorf("add-on qualification leaked into validation.readiness — it is a durable generation constraint")
					}
				}
			}
			// No driver-state gates in either phase: qualification rests on
			// the control-plane signal alone (no ClusterPolicy readback, no
			// GPU.hardware.driver-loaded — both rejected in #2363).
			for _, c := range result.Constraints {
				if c.Name == "GPU.hardware.driver-loaded" {
					t.Errorf("driver-loaded gate found in spec.constraints — rejected by the #2363 decision")
				}
			}
		})
	}
}
