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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/manifest"
	"gopkg.in/yaml.v3"
)

const (
	nvidiaDriverCRMixin    = "gpu-operator-nvidia-driver-cr"
	nvidiaDriverCRManifest = "components/gpu-operator/manifests/nvidia-driver.yaml"
)

func TestNVIDIADriverCRMixinIsOptInManifestOnly(t *testing.T) {
	ctx, store := objectMonitorStore(t)
	mixin, ok := store.Mixins[nvidiaDriverCRMixin]
	if !ok {
		t.Fatalf("%s mixin not present", nvidiaDriverCRMixin)
	}
	ref, ok := findComponentRefByName(mixin.Spec.ComponentRefs, "gpu-operator")
	if !ok {
		t.Fatal("mixin has no gpu-operator componentRef")
	}
	if len(ref.ManifestFiles) != 1 || ref.ManifestFiles[0] != nvidiaDriverCRManifest {
		t.Fatalf("manifestFiles = %v, want [%s]", ref.ManifestFiles, nvidiaDriverCRManifest)
	}
	if len(ref.Overrides) != 0 {
		t.Fatalf("mixin sets fleet-specific overrides: %v", ref.Overrides)
	}
	registry, err := GetComponentRegistryFor(store.provider)
	if err != nil {
		t.Fatalf("load component registry: %v", err)
	}
	gpuOperator := registry.Get("gpu-operator")
	if gpuOperator == nil {
		t.Fatal("gpu-operator is missing from component registry")
	}
	if !gpuOperator.ManifestsUseChartCRDs {
		t.Error("gpu-operator manifestsUseChartCRDs must be enabled for fresh Helmfile installs")
	}

	body, err := store.provider.ReadFile(ctx, nvidiaDriverCRManifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	for _, required := range []string{
		"kind: NVIDIADriver",
		"$driver.nvidiaDriverCRD.enabled",
		`eq (toString $driver.nvidiaDriverCRD.deployDefaultCR) "false"`,
		"$nvidiaDriver.version | quote",
		"$vals.gdrcopy",
		"$vals.gds",
	} {
		if !strings.Contains(string(body), required) {
			t.Errorf("manifest does not contain %q", required)
		}
	}

	values := nvidiaDriverCRValues()
	rendered, err := manifest.Render(body, manifest.RenderInput{
		ComponentName: "gpu-operator",
		Namespace:     "gpu-operator",
		ChartName:     "gpu-operator",
		ChartVersion:  "26.7.2",
		Values:        values,
	})
	if err != nil {
		t.Fatalf("render manifest: %v", err)
	}
	for _, want := range []string{
		"kind: NVIDIADriver",
		"repository: nvcr.io/nvidia",
		`version: "580.173.02"`,
		"imagePullPolicy: IfNotPresent",
		"- driver-registry",
		"certConfig:",
		"custom-driver-certs",
		"licensingConfig:",
		"secretName: vgpu-license",
		"nlsEnabled: false",
		"maxParallelUpgrades: 5",
		"gds:",
		"image: nvidia-fs",
		"gdrcopy:",
		"image: gdrdrv",
	} {
		if !strings.Contains(string(rendered), want) {
			t.Errorf("rendered manifest does not contain %q:\n%s", want, rendered)
		}
	}
	// Flux packages this template unchanged and helm-controller renders it once.
	// A surviving delimiter means the manifest still depends on a localformat-only
	// second render.
	if strings.Contains(string(rendered), "{{") {
		t.Errorf("single-pass render left nested Helm expressions:\n%s", rendered)
	}

	withoutOptionalConfigs := nvidiaDriverCRValues()
	delete(withoutOptionalConfigs["driver"].(map[string]any), "certConfig")
	delete(withoutOptionalConfigs["driver"].(map[string]any), "licensingConfig")
	withoutOptionalConfigsRendered, err := manifest.Render(body, manifest.RenderInput{
		ComponentName: "gpu-operator",
		Namespace:     "gpu-operator",
		ChartName:     "gpu-operator",
		ChartVersion:  "26.7.2",
		Values:        withoutOptionalConfigs,
	})
	if err != nil {
		t.Fatalf("render manifest without optional configurations: %v", err)
	}
	for _, absent := range []string{"certConfig:", "licensingConfig:"} {
		if strings.Contains(string(withoutOptionalConfigsRendered), absent) {
			t.Errorf("rendered unset configuration %q:\n%s", absent, withoutOptionalConfigsRendered)
		}
	}

	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "driver disabled",
			mutate: func(values map[string]any) {
				values["driver"].(map[string]any)["enabled"] = false
			},
		},
		{
			name: "CRD disabled",
			mutate: func(values map[string]any) {
				values["driver"].(map[string]any)["nvidiaDriverCRD"].(map[string]any)["enabled"] = false
			},
		},
		{
			name: "default CR enabled",
			mutate: func(values map[string]any) {
				values["driver"].(map[string]any)["nvidiaDriverCRD"].(map[string]any)["deployDefaultCR"] = true
			},
		},
		{
			name: "deployDefaultCR omitted",
			mutate: func(values map[string]any) {
				delete(values["driver"].(map[string]any)["nvidiaDriverCRD"].(map[string]any), "deployDefaultCR")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := nvidiaDriverCRValues()
			tc.mutate(values)
			disabledRendered, renderErr := manifest.Render(body, manifest.RenderInput{
				ComponentName: "gpu-operator",
				Namespace:     "gpu-operator",
				ChartName:     "gpu-operator",
				ChartVersion:  "26.7.2",
				Values:        values,
			})
			if renderErr != nil {
				t.Fatalf("render manifest: %v", renderErr)
			}
			if strings.Contains(string(disabledRendered), "kind: NVIDIADriver") {
				t.Errorf("rendered disabled NVIDIADriver CR:\n%s", disabledRendered)
			}
		})
	}

	zeroValues := nvidiaDriverCRValues()
	zeroPolicy := zeroValues["nvidiaDriver"].(map[string]any)["upgradePolicy"].(map[string]any)
	zeroPolicy["maxParallelUpgrades"] = 0
	zeroPolicy["drain"].(map[string]any)["timeoutSeconds"] = 0
	zeroRendered, err := manifest.Render(body, manifest.RenderInput{
		ComponentName: "gpu-operator",
		Namespace:     "gpu-operator",
		ChartName:     "gpu-operator",
		ChartVersion:  "26.7.2",
		Values:        zeroValues,
	})
	if err != nil {
		t.Fatalf("render zero-valued policy: %v", err)
	}
	var zeroCR struct {
		Spec struct {
			UpgradePolicy struct {
				MaxParallelUpgrades *int `yaml:"maxParallelUpgrades"`
				Drain               struct {
					TimeoutSeconds *int `yaml:"timeoutSeconds"`
				} `yaml:"drain"`
			} `yaml:"upgradePolicy"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(zeroRendered, &zeroCR); err != nil {
		t.Fatalf("parse zero-valued policy: %v", err)
	}
	if got := zeroCR.Spec.UpgradePolicy.MaxParallelUpgrades; got == nil || *got != 0 {
		t.Errorf("spec.upgradePolicy.maxParallelUpgrades = %v, want 0", got)
	}
	if got := zeroCR.Spec.UpgradePolicy.Drain.TimeoutSeconds; got == nil || *got != 0 {
		t.Errorf("spec.upgradePolicy.drain.timeoutSeconds = %v, want 0", got)
	}

	for name, overlay := range store.Overlays {
		for _, adopted := range overlay.Spec.Mixins {
			if adopted == nvidiaDriverCRMixin {
				t.Errorf("overlay %q adopts opt-in mixin %q", name, nvidiaDriverCRMixin)
			}
		}
	}
}

func nvidiaDriverCRValues() map[string]any {
	return map[string]any{
		"driver": map[string]any{
			"enabled": true,
			"certConfig": map[string]any{
				"name": "custom-driver-certs",
			},
			"licensingConfig": map[string]any{
				"secretName": "vgpu-license",
				"nlsEnabled": false,
			},
			"nvidiaDriverCRD": map[string]any{
				"enabled":         true,
				"deployDefaultCR": false,
			},
		},
		"nvidiaDriver": map[string]any{
			"repository":       "nvcr.io/nvidia",
			"image":            "driver",
			"version":          "580.173.02",
			"imagePullPolicy":  "IfNotPresent",
			"imagePullSecrets": []string{"driver-registry"},
			"driverType":       "gpu",
			"kernelModuleType": "auto",
			"usePrecompiled":   false,
			"manager":          map[string]any{"image": "k8s-driver-manager"},
			"upgradePolicy": map[string]any{
				"autoUpgrade":         true,
				"maxParallelUpgrades": 5,
				"maxUnavailable":      "25%",
				"waitForCompletion":   map[string]any{"timeoutSeconds": 0},
				"gpuPodDeletion":      map[string]any{"timeoutSeconds": 300},
				"drain": map[string]any{
					"enable":         true,
					"force":          true,
					"podSelector":    "",
					"timeoutSeconds": 600,
					"deleteEmptyDir": true,
				},
			},
		},
		"gds": map[string]any{
			"enabled":         false,
			"repository":      "nvcr.io/nvidia/cloud-native",
			"image":           "nvidia-fs",
			"version":         "2.29.4",
			"imagePullPolicy": "IfNotPresent",
		},
		"gdrcopy": map[string]any{
			"enabled":         true,
			"repository":      "nvcr.io/nvidia/cloud-native",
			"image":           "gdrdrv",
			"version":         "v2.6",
			"imagePullPolicy": "IfNotPresent",
		},
	}
}
