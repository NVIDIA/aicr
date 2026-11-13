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
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/helm/helmtest"
	"github.com/NVIDIA/aicr/pkg/manifest"
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
		if limits["memory"] != "12Gi" {
			t.Fatalf("maintenance-operator-chart.operator.resources.limits.memory = %v, want 12Gi "+
				"(the subchart default 128Mi is below the operator's working set, about 5.1Gi at 2500 nodes)", limits["memory"])
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

// TestNicConfigurationDaemonResources renders each NicClusterPolicy that
// deploys the nic-configuration-daemon with its leaf's effective values. With
// no override the policy sets no daemon resources, so the Network Operator's
// default applies (#3698); a networkoperator override reaches the policy.
func TestNicConfigurationDaemonResources(t *testing.T) {
	t.Parallel()

	leaves := []struct {
		name     string
		criteria *recipe.Criteria
		manifest string
	}{
		{
			name: "generic gb300",
			criteria: &recipe.Criteria{
				Service:     recipe.CriteriaServiceGeneric,
				Accelerator: recipe.CriteriaAcceleratorGB300,
				OS:          recipe.CriteriaOSUbuntu,
				Intent:      recipe.CriteriaIntentTraining,
			},
			manifest: "nic-cluster-policy-generic-gb300.yaml",
		},
		{
			name: "nvke vr200",
			criteria: &recipe.Criteria{
				Service:     recipe.CriteriaServiceNVKE,
				Accelerator: recipe.CriteriaAcceleratorVR200,
				OS:          recipe.CriteriaOSUbuntu,
				Intent:      recipe.CriteriaIntentTraining,
			},
			manifest: "nic-cluster-policy-nvke-vr200.yaml",
		},
	}
	cases := []struct {
		name           string
		overrides      map[string]string
		wantLimitMem   string
		wantRequestMem string
		wantRequestCPU string
		wantErr        bool
	}{
		{name: "operator default"},
		{
			name: "override reaches the policy",
			overrides: map[string]string{
				"nicConfigurationOperator.configurationDaemon.resources.limits.memory":   "8Gi",
				"nicConfigurationOperator.configurationDaemon.resources.requests.memory": "1Gi",
			},
			wantLimitMem:   "8Gi",
			wantRequestMem: "1Gi",
		},
		{
			name: "override with a CPU request",
			overrides: map[string]string{
				"nicConfigurationOperator.configurationDaemon.resources.limits.memory":   "8Gi",
				"nicConfigurationOperator.configurationDaemon.resources.requests.memory": "1Gi",
				"nicConfigurationOperator.configurationDaemon.resources.requests.cpu":    "100m",
			},
			wantLimitMem:   "8Gi",
			wantRequestMem: "1Gi",
			wantRequestCPU: "100m",
		},
		{
			name: "limit without a request fails",
			overrides: map[string]string{
				"nicConfigurationOperator.configurationDaemon.resources.limits.memory": "4Gi",
			},
			wantErr: true,
		},
		{
			name: "request without a limit fails",
			overrides: map[string]string{
				"nicConfigurationOperator.configurationDaemon.resources.requests.memory": "1Gi",
			},
			wantErr: true,
		},
		{
			name: "CPU request alone fails",
			overrides: map[string]string{
				"nicConfigurationOperator.configurationDaemon.resources.requests.cpu": "200m",
			},
			wantErr: true,
		},
	}

	for _, leaf := range leaves {
		t.Run(leaf.name, func(t *testing.T) {
			t.Parallel()

			result, err := recipe.NewBuilder().BuildFromCriteriaWithProfile(t.Context(), leaf.criteria, "")
			if err != nil {
				t.Fatalf("BuildFromCriteriaWithProfile: %v", err)
			}
			var ref *recipe.ComponentRef
			for i := range result.ComponentRefs {
				if result.ComponentRefs[i].Name == "network-operator" {
					ref = &result.ComponentRefs[i]
					break
				}
			}
			if ref == nil {
				t.Fatal("network-operator component not found in the resolved recipe")
			}
			var manifestPath string
			for _, p := range ref.ManifestFiles {
				if strings.HasSuffix(p, leaf.manifest) {
					manifestPath = p
				}
			}
			if manifestPath == "" {
				t.Fatalf("network-operator manifestFiles %v do not include %s", ref.ManifestFiles, leaf.manifest)
			}
			content, err := recipe.GetManifestContentWithContext(t.Context(), nil, manifestPath)
			if err != nil {
				t.Fatalf("load %s: %v", manifestPath, err)
			}

			for _, tt := range cases {
				t.Run(tt.name, func(t *testing.T) {
					values, err := recipe.GetComponentValuesWithContext(t.Context(), nil, ref)
					if err != nil {
						t.Fatalf("resolve effective network-operator values: %v", err)
					}
					if err = component.ApplyMapOverrides(values, tt.overrides); err != nil {
						t.Fatalf("apply overrides: %v", err)
					}
					rendered, err := manifest.Render(content, manifest.RenderInput{
						ComponentName: ref.Name,
						Values:        values,
					})
					if tt.wantErr {
						errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
					} else {
						if err != nil {
							t.Fatalf("render %s: %v", manifestPath, err)
						}
						assertDaemonResources(t, rendered, tt.wantLimitMem, tt.wantRequestMem, tt.wantRequestCPU)
					}

					// A flux bundle ships the manifest as a chart template, so Helm renders it.
					t.Run("helm", func(t *testing.T) {
						helmtest.RequireBinary(t)
						out, helmErr := helmTemplateManifest(t, leaf.manifest, content, ref.Name, values)
						if tt.wantErr {
							if helmErr == nil || !strings.Contains(string(out), "must set both requests.memory and limits.memory") {
								t.Fatalf("helm template error = %v, want the incomplete-override failure:\n%s", helmErr, out)
							}
							return
						}
						if helmErr != nil {
							t.Fatalf("helm template: %v\n%s", helmErr, out)
						}
						assertDaemonResources(t, out, tt.wantLimitMem, tt.wantRequestMem, tt.wantRequestCPU)
					})
				})
			}
		})
	}
}

// assertDaemonResources checks the nic-configuration-daemon resources of a
// rendered NicClusterPolicy. An empty wantLimitMem means the policy must set
// none.
func assertDaemonResources(t *testing.T, rendered []byte, wantLimitMem, wantRequestMem, wantRequestCPU string) {
	t.Helper()

	var policy struct {
		Spec struct {
			NicConfigurationOperator struct {
				ConfigurationDaemon struct {
					Image              string `yaml:"image"`
					ContainerResources []struct {
						Name     string            `yaml:"name"`
						Requests map[string]string `yaml:"requests"`
						Limits   map[string]string `yaml:"limits"`
					} `yaml:"containerResources"`
				} `yaml:"configurationDaemon"`
			} `yaml:"nicConfigurationOperator"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(rendered, &policy); err != nil {
		t.Fatalf("rendered policy does not parse: %v\n%s", err, rendered)
	}
	daemon := policy.Spec.NicConfigurationOperator.ConfigurationDaemon
	if daemon.Image == "" {
		t.Fatalf("rendered policy has no configurationDaemon:\n%s", rendered)
	}
	res := daemon.ContainerResources
	if wantLimitMem == "" {
		if len(res) != 0 {
			t.Fatalf("configurationDaemon.containerResources = %+v, want none so the operator's default applies", res)
		}
		return
	}
	if len(res) != 1 || res[0].Name != "nic-configuration-daemon" {
		t.Fatalf("configurationDaemon.containerResources = %+v, want one nic-configuration-daemon entry", res)
	}
	if got := res[0].Limits["memory"]; got != wantLimitMem {
		t.Errorf("limits.memory = %q, want %q", got, wantLimitMem)
	}
	if got := res[0].Requests["memory"]; got != wantRequestMem {
		t.Errorf("requests.memory = %q, want %q", got, wantRequestMem)
	}
	if got := res[0].Requests["cpu"]; got != wantRequestCPU {
		t.Errorf("requests.cpu = %q, want %q", got, wantRequestCPU)
	}
}

// helmTemplateManifest renders one manifest with `helm template`, as a chart
// template with the component's values nested under its name.
func helmTemplateManifest(t *testing.T, name string, content []byte, componentName string, values map[string]any) ([]byte, error) {
	t.Helper()

	valuesYAML, err := yaml.Marshal(map[string]any{componentName: values})
	if err != nil {
		t.Fatalf("marshal values: %v", err)
	}
	chartDir := t.TempDir()
	files := map[string][]byte{
		"Chart.yaml":                     []byte("apiVersion: v2\nname: nic-policy-test\nversion: 0.1.0\n"),
		"values.yaml":                    valuesYAML,
		filepath.Join("templates", name): content,
	}
	for rel, data := range files {
		path := filepath.Join(chartDir, rel)
		if err = os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("create %s: %v", filepath.Dir(path), err)
		}
		if err = os.WriteFile(path, data, 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "helm", "template", "test-release", chartDir) //nolint:gosec // controlled test arguments
	return cmd.CombinedOutput()
}
