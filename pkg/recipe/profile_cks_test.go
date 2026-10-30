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

	"github.com/NVIDIA/aicr/pkg/allocpolicy"
)

func cksCriteria(accelerator CriteriaAcceleratorType) *Criteria {
	return &Criteria{
		Service:     CriteriaServiceCKS,
		Accelerator: accelerator,
		OS:          CriteriaOSUbuntu,
		Intent:      CriteriaIntentTraining,
	}
}

func TestCKSGPUStackProfile(t *testing.T) {
	t.Parallel()

	for _, accelerator := range []CriteriaAcceleratorType{CriteriaAcceleratorRTXPro6000, CriteriaAcceleratorH100} {
		t.Run(string(accelerator), func(t *testing.T) {
			t.Parallel()
			for _, selection := range []string{"", "gpuStack=coreweave"} {
				t.Run(selection, func(t *testing.T) {
					t.Parallel()
					result, err := NewBuilder().BuildFromCriteriaWithProfile(t.Context(), cksCriteria(accelerator), selection)
					if err != nil {
						t.Fatalf("resolve CoreWeave recipe: %v", err)
					}
					selected := result.Metadata.SelectedProfile
					if selected == nil {
						t.Fatal("missing gpuStack profile")
					}
					if selected.Name != "gpuStack" || selected.Value != "coreweave" || selected.Advertiser != allocpolicy.AdvertiserExternal {
						t.Fatalf("selected profile = %+v, want gpuStack=coreweave with external advertiser", selected)
					}
					for _, name := range []string{"gpu-operator", "nfd", "nvidia-dra-driver-gpu", "dra-node-labeler", "nvsentinel", "prometheus-adapter"} {
						ref := result.GetComponentRef(name)
						if ref == nil || ref.IsEnabled() {
							t.Errorf("%s must be explicitly disabled", name)
						}
					}
					for _, name := range []string{"network-operator", "nodewright-customizations"} {
						if ref := result.GetComponentRef(name); ref != nil && ref.IsEnabled() {
							t.Errorf("%s must not manage CoreWeave hosts", name)
						}
					}
					for _, name := range []string{"cert-manager", "nodewright-operator", "prometheus-operator-crds", "kube-prometheus-stack", "k8s-ephemeral-storage-metrics", "kai-scheduler"} {
						ref := result.GetComponentRef(name)
						if ref == nil || !ref.IsEnabled() {
							t.Errorf("missing enabled %s", name)
						}
					}
					values, err := result.GetValuesForComponentWithContext(t.Context(), "kube-prometheus-stack")
					if err != nil {
						t.Fatalf("hydrate Prometheus values: %v", err)
					}
					nodeExporter, ok := values["nodeExporter"].(map[string]any)
					if !ok || nodeExporter["enabled"] != false {
						t.Errorf("nodeExporter = %#v, want enabled=false to avoid CoreWeave's host port 9100", values["nodeExporter"])
					}
					for _, path := range []string{"GPU.hardware.driver-loaded", "K8s.policy.devicePlugin.enabled"} {
						if !slices.ContainsFunc(result.Constraints, func(c Constraint) bool { return c.Name == path && c.Value == "true" }) {
							t.Errorf("missing provider prerequisite %s=true", path)
						}
					}
					if err := result.PrepareAndValidateWithContext(t.Context()); err != nil {
						t.Fatalf("validate CoreWeave artifact: %v", err)
					}
					if err := result.ValidateProfileLock(t.Context(), result.ComponentRefs, nil, nil); err != nil {
						t.Fatalf("provider-owned GPU components must be omittable at bundle time: %v", err)
					}
				})
			}
		})
	}
}

// TestCKSDeploymentValidation resolves the wildcard and service inheritance
// together: disabling GPU Operator must also remove its deployment checks,
// while keeping checks for the components AICR owns and usable provider GPUs.
func TestCKSDeploymentValidation(t *testing.T) {
	t.Parallel()

	for _, selection := range []string{"", "gpuStack=coreweave"} {
		t.Run(selection, func(t *testing.T) {
			t.Parallel()
			result, err := NewBuilder().BuildFromCriteriaWithProfile(t.Context(), cksCriteria(), selection)
			if err != nil {
				t.Fatalf("resolve CoreWeave recipe: %v", err)
			}
			if result.Validation == nil || result.Validation.Deployment == nil {
				t.Fatal("missing deployment validation")
			}
			deployment := result.Validation.Deployment
			wantChecks := []string{"expected-resources", "check-nvidia-smi"}
			if !slices.Equal(deployment.Checks, wantChecks) {
				t.Errorf("deployment checks = %v, want %v", deployment.Checks, wantChecks)
			}
			for _, constraint := range deployment.Constraints {
				if constraint.Name == "Deployment.gpu-operator.version" {
					t.Errorf("provider-managed GPU Operator must not inherit version constraint %q", constraint.Value)
				}
			}
			if result.Validation.Conformance == nil ||
				!slices.Equal(result.Validation.Conformance.Checks, []string{"platform-health"}) {

				t.Errorf("conformance = %+v, want only platform-health", result.Validation.Conformance)
			}
		})
	}
}

func TestCKSProfileQualification(t *testing.T) {
	t.Parallel()

	for _, accelerator := range []CriteriaAcceleratorType{CriteriaAcceleratorRTXPro6000, CriteriaAcceleratorH100} {
		t.Run(string(accelerator), func(t *testing.T) {
			t.Parallel()
			tests := []struct {
				name    string
				driver  string
				plugin  string
				wantErr string
			}{
				{name: "provider stack ready", driver: "true", plugin: "true"},
				{name: "driver not loaded", driver: "false", plugin: "true", wantErr: "GPU.hardware.driver-loaded"},
				{name: "plugin disabled", driver: "true", plugin: "false", wantErr: "K8s.policy.devicePlugin.enabled"},
				{name: "driver reading missing", plugin: "true", wantErr: "GPU.hardware.driver-loaded"},
				{name: "plugin reading missing", driver: "true", wantErr: "K8s.policy.devicePlugin.enabled"},
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
					_, err := NewBuilder().BuildFromCriteriaWithEvaluatorAndProfile(t.Context(), cksCriteria(accelerator), evaluate, "")
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
		})
	}
}

func TestCKSRejectsCompetingAdvertiser(t *testing.T) {
	t.Parallel()

	for _, accelerator := range []CriteriaAcceleratorType{CriteriaAcceleratorRTXPro6000, CriteriaAcceleratorH100} {
		t.Run(string(accelerator), func(t *testing.T) {
			t.Parallel()
			result, err := NewBuilder().BuildFromCriteria(t.Context(), cksCriteria(accelerator))
			if err != nil {
				t.Fatalf("resolve CoreWeave recipe: %v", err)
			}
			ref := result.GetComponentRef("gpu-operator")
			if ref == nil {
				t.Fatal("missing GPU Operator component ref")
			}
			ref.Overrides["enabled"] = true
			if err := result.PrepareAndValidateWithContext(t.Context()); err == nil || !strings.Contains(err.Error(), "external advertiser") {
				t.Fatalf("artifact validation error = %v, want competing-advertiser rejection", err)
			}
		})
	}
}
