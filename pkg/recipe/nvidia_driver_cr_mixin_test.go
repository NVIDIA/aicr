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
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"github.com/NVIDIA/aicr/pkg/manifest"
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

	body, err := store.provider.ReadFile(ctx, nvidiaDriverCRManifest)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	for _, required := range []string{
		"kind: NVIDIADriver",
		"$driver.nvidiaDriverCRD.enabled",
		"not $driver.nvidiaDriverCRD.deployDefaultCR",
		".Values.nvidiaDriver.version",
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
		ChartVersion:  "26.7.0",
		Values:        values,
	})
	if err != nil {
		t.Fatalf("render manifest: %v", err)
	}
	for _, want := range []string{
		"kind: NVIDIADriver",
		"repository: nvcr.io/nvidia",
		`version: '{{ default "580.173.02" .Values.nvidiaDriver.version }}'`,
		`maxParallelUpgrades: !!int '{{ if hasKey .Values.nvidiaDriver.upgradePolicy "maxParallelUpgrades" }}{{ .Values.nvidiaDriver.upgradePolicy.maxParallelUpgrades }}{{ else }}5{{ end }}'`,
	} {
		if !strings.Contains(string(rendered), want) {
			t.Errorf("rendered manifest does not contain %q:\n%s", want, rendered)
		}
	}

	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := nvidiaDriverCRValues()
			tc.mutate(values)
			disabledRendered, renderErr := manifest.Render(body, manifest.RenderInput{
				ComponentName: "gpu-operator",
				Namespace:     "gpu-operator",
				ChartName:     "gpu-operator",
				ChartVersion:  "26.7.0",
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

	nested, err := template.New("nvidia-driver").Funcs(sprig.TxtFuncMap()).Parse(string(rendered))
	if err != nil {
		t.Fatalf("parse generated Helm template: %v", err)
	}
	var final strings.Builder
	err = nested.Execute(&final, map[string]any{
		"Values": map[string]any{
			"nvidiaDriver": map[string]any{
				"upgradePolicy": map[string]any{
					"maxParallelUpgrades": 0,
					"drain": map[string]any{
						"timeoutSeconds": 0,
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("render generated Helm template: %v", err)
	}
	for _, want := range []string{
		"maxParallelUpgrades: !!int '0'",
		"timeoutSeconds: !!int '0'",
	} {
		if !strings.Contains(final.String(), want) {
			t.Errorf("generated Helm template does not preserve zero override %q:\n%s", want, final.String())
		}
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
			"nvidiaDriverCRD": map[string]any{
				"enabled":         true,
				"deployDefaultCR": false,
			},
		},
		"nvidiaDriver": map[string]any{
			"repository":       "nvcr.io/nvidia",
			"image":            "driver",
			"version":          "580.173.02",
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
	}
}
