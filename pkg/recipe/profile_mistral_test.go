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

	"aicr.run/pkg/allocpolicy"
)

func mistralCriteria() *Criteria {
	return &Criteria{
		Service:     CriteriaServiceMistral,
		Accelerator: CriteriaAcceleratorGB300,
		OS:          CriteriaOSUbuntu,
		Intent:      CriteriaIntentTraining,
	}
}

func setComponentInstall(refs []ComponentRef, name string, install bool) []ComponentRef {
	out := make([]ComponentRef, len(refs))
	for i, ref := range refs {
		out[i] = ref
		if ref.Name != name {
			continue
		}
		overrides := make(map[string]any, len(ref.Overrides)+1)
		for k, v := range ref.Overrides {
			overrides[k] = v
		}
		overrides["install"] = install
		out[i].Overrides = overrides
	}
	return out
}

func TestMistralGPUStackProfile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		selection      string
		wantValue      string
		wantAdvertiser string
		wantGPUStack   bool
	}{
		{selection: "", wantValue: "mistral-managed", wantAdvertiser: allocpolicy.AdvertiserExternal},
		{selection: "gpuStack=mistral-managed", wantValue: "mistral-managed", wantAdvertiser: allocpolicy.AdvertiserExternal},
		{selection: "gpuStack=operator-managed", wantValue: "operator-managed", wantGPUStack: true},
	}
	for _, tt := range tests {
		t.Run(tt.wantValue+"/"+tt.selection, func(t *testing.T) {
			t.Parallel()
			result, err := NewBuilder().BuildFromCriteriaWithProfile(t.Context(), mistralCriteria(), tt.selection)
			if err != nil {
				t.Fatalf("resolve Mistral recipe: %v", err)
			}
			selected := result.Metadata.SelectedProfile
			if selected == nil || selected.Name != "gpuStack" || selected.Value != tt.wantValue || selected.Advertiser != tt.wantAdvertiser {
				t.Fatalf("selected profile = %+v, want gpuStack=%s advertiser %q", selected, tt.wantValue, tt.wantAdvertiser)
			}
			for _, name := range []string{"gpu-operator", "prometheus-adapter"} {
				ref := result.GetComponentRef(name)
				if ref == nil {
					t.Fatalf("%s must stay in the recipe inventory", name)
				}
				if ref.IsEnabled() != tt.wantGPUStack {
					t.Errorf("%s enabled = %v, want %v", name, ref.IsEnabled(), tt.wantGPUStack)
				}
			}
			for _, name := range []string{"network-operator", "nfd"} {
				if ref := result.GetComponentRef(name); ref != nil && ref.IsEnabled() {
					t.Errorf("%s is Mistral's and must stay disabled", name)
				}
			}
			for _, name := range []string{"nvidia-dra-driver-gpu", "cert-manager", "nodewright-operator"} {
				if ref := result.GetComponentRef(name); ref == nil || !ref.IsEnabled() {
					t.Errorf("missing enabled %s", name)
				}
			}
			nvsentinel, err := result.GetValuesForComponentWithContext(t.Context(), "nvsentinel")
			if err != nil {
				t.Fatalf("hydrate nvsentinel: %v", err)
			}
			labeler, _ := nvsentinel["labeler"].(map[string]any)
			if got := labeler["assumeDriverInstalled"]; got != !tt.wantGPUStack {
				t.Errorf("nvsentinel labeler.assumeDriverInstalled = %v, want %v", got, !tt.wantGPUStack)
			}
			hasDeploymentOrder := slices.Contains(result.DeploymentOrder, "gpu-operator")
			if hasDeploymentOrder != tt.wantGPUStack {
				t.Errorf("gpu-operator in deploymentOrder = %v, want %v", hasDeploymentOrder, tt.wantGPUStack)
			}
			for _, path := range []string{"GPU.hardware.driver-loaded", "K8s.policy.devicePlugin.enabled"} {
				has := slices.ContainsFunc(result.Constraints, func(c Constraint) bool { return c.Name == path && c.Value == "true" })
				if has == tt.wantGPUStack {
					t.Errorf("constraint %s present = %v, want %v", path, has, !tt.wantGPUStack)
				}
			}
			if err = result.PrepareAndValidateWithContext(t.Context()); err != nil {
				t.Fatalf("validate Mistral artifact: %v", err)
			}
			candidate := make(map[string]map[string]any)
			for _, ref := range result.ComponentRefs {
				if !ref.IsEnabled() {
					continue
				}
				values, valuesErr := result.GetValuesForComponentWithContext(t.Context(), ref.Name)
				if valuesErr != nil {
					t.Fatalf("hydrate %s: %v", ref.Name, valuesErr)
				}
				candidate[ref.Name] = values
			}
			if err = result.ValidateProfileLock(t.Context(), result.ComponentRefs, candidate, nil); err != nil {
				t.Fatalf("bundle that keeps the selected presence rejected: %v", err)
			}
			flipped := setComponentInstall(result.ComponentRefs, "gpu-operator", !tt.wantGPUStack)
			err = result.ValidateProfileLock(t.Context(), flipped, candidate, nil)
			if err == nil || !strings.Contains(err.Error(), "gpu-operator") {
				t.Fatalf("flipped gpu-operator presence error = %v, want presence rejection", err)
			}
		})
	}
}

func TestMistralProfileQualification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		driver  string
		plugin  string
		wantErr string
	}{
		{name: "provider stack ready", driver: "true", plugin: "true"},
		{name: "no Mistral GPU Operator", driver: "true", wantErr: "K8s.policy.devicePlugin.enabled"},
		{name: "driver not loaded", driver: "false", plugin: "true", wantErr: "GPU.hardware.driver-loaded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			evaluate := func(c Constraint) ConstraintEvalResult {
				actual := c.Value
				switch c.Name {
				case "GPU.hardware.driver-loaded":
					actual = tt.driver
				case "K8s.policy.devicePlugin.enabled":
					actual = tt.plugin
				}
				return ConstraintEvalResult{Passed: actual == c.Value, Actual: actual}
			}
			_, err := NewBuilder().BuildFromCriteriaWithEvaluatorAndProfile(t.Context(), mistralCriteria(), evaluate, "")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ready provider stack rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("resolve error = %v, want rejection naming %s", err, tt.wantErr)
			}
		})
	}
}
