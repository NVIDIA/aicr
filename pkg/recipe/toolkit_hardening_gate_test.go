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

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/manifest"
)

const toolkitHardeningManifest = "components/gpu-operator/manifests/nvidia-toolkit-hardening-aks.yaml"

// renderToolkitHardening renders the AKS device-isolation hardening manifest
// with an explicit gpu-operator.toolkit.enabled value.
func renderToolkitHardening(t *testing.T, toolkitEnabled bool) string {
	t.Helper()
	content, err := GetEmbeddedFS().ReadFile(toolkitHardeningManifest)
	if err != nil {
		t.Fatalf("read %s: %v", toolkitHardeningManifest, err)
	}
	rendered, rerr := manifest.Render(content, manifest.RenderInput{
		ComponentName: "gpu-operator",
		Namespace:     "gpu-operator",
		ChartName:     "gpu-operator",
		ChartVersion:  "1.0.0",
		// manifest.Render exposes this map as .Values[ComponentName], so pass
		// the raw gpu-operator values (not wrapped under the component name).
		Values: map[string]any{
			"toolkit": map[string]any{"enabled": toolkitEnabled},
		},
	})
	if rerr != nil {
		t.Fatalf("render hardening manifest (toolkit.enabled=%v): %v", toolkitEnabled, rerr)
	}
	return string(rendered)
}

// TestToolkitHardeningGate pins the toolkit.enabled gate on the AKS
// device-isolation hardening DaemonSet:
//   - driver-only profile (toolkit.enabled=false): the DaemonSet renders, with
//     the fail-closed sentinel-removal on assert failure.
//   - GPU-Operator-managed fallback (toolkit.enabled=true): the DaemonSet is
//     omitted entirely (the operator owns the toolkit; the hardened keys come
//     from gpu-operator.toolkit.env instead — see TestAKSToolkitEnvHardened).
//
// The gate uses `eq (toString (index $toolkit "enabled")) "false"`, so it
// compares the effective boolean explicitly rather than relying on Helm's
// `default`, which treats a boolean false as empty.
func TestToolkitHardeningGate(t *testing.T) {
	driverOnly := renderToolkitHardening(t, false)
	if !strings.Contains(driverOnly, "kind: DaemonSet") {
		t.Errorf("toolkit.enabled=false must render the hardening DaemonSet:\n%s", driverOnly)
	}
	if !strings.Contains(driverOnly, "rm -f /tmp/hardened") {
		t.Error("driver-only rendering must include the fail-closed sentinel removal (rm -f /tmp/hardened)")
	}

	fallback := renderToolkitHardening(t, true)
	if strings.Contains(fallback, "kind: DaemonSet") {
		t.Errorf("toolkit.enabled=true (managed fallback) must omit the hardening DaemonSet:\n%s", fallback)
	}
}

// TestAKSToolkitEnvHardened asserts both AKS gpu-operator values files carry the
// exact uppercase toolkit-hardening env vars bound to their correct string
// values, so the operator-owned toolkit is hardened in the GPU-Operator-managed
// fallback where the DaemonSet is gated off. It parses the YAML and asserts the
// exact name→value mapping (rather than searching for names and values
// independently), so swapping the two security values would fail the test.
func TestAKSToolkitEnvHardened(t *testing.T) {
	// The two security-relevant keys and their required values. Both matter:
	// the env-var key must be "false" (deny the NVIDIA_VISIBLE_DEVICES path) and
	// the volume-mounts key must be "true" (allow the device plugin's allocated
	// volume-mounts). A swap would silently reopen the env-var isolation gap.
	wantPairs := map[string]string{
		"ACCEPT_NVIDIA_VISIBLE_DEVICES_ENVVAR_WHEN_UNPRIVILEGED": "false",
		"ACCEPT_NVIDIA_VISIBLE_DEVICES_AS_VOLUME_MOUNTS":         "true",
	}

	// toolkitValues models just the toolkit.env list we assert on.
	type toolkitValues struct {
		Toolkit struct {
			Env []struct {
				Name  string `yaml:"name"`
				Value string `yaml:"value"`
			} `yaml:"env"`
		} `yaml:"toolkit"`
	}

	for _, f := range []string{
		"components/gpu-operator/values-aks.yaml",
		"components/gpu-operator/values-aks-training.yaml",
	} {
		content, err := GetEmbeddedFS().ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		var tv toolkitValues
		if uerr := yaml.Unmarshal(content, &tv); uerr != nil {
			t.Fatalf("unmarshal %s: %v", f, uerr)
		}

		// Build the actual name→value map from the parsed env list.
		got := make(map[string]string, len(tv.Toolkit.Env))
		for _, e := range tv.Toolkit.Env {
			got[e.Name] = e.Value
		}

		for name, want := range wantPairs {
			val, ok := got[name]
			if !ok {
				t.Errorf("%s: toolkit.env missing %s", f, name)
				continue
			}
			if val != want {
				t.Errorf("%s: toolkit.env %s = %q, want %q", f, name, val, want)
			}
		}
	}
}
