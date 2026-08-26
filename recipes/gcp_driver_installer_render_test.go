// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package recipes

import (
	"os"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/manifest"
)

// TestGCPDriverInstallerRenderedTolerations pins the rendered toleration
// content of the values-gated cos-gpu-installer DaemonSet — the #2360 P1
// fix: an SDK caller (Client.MakeBundle with nil configuration) injects no
// acceleratedTolerations, and without the template fallback the DaemonSet
// renders with no tolerations at all, scheduled away from GKE's
// auto-tainted GPU pools while the health check passes on the untainted
// subset. The Go-side gate parity lives in
// pkg/bundler/validations/checks_test.go (TestCheckDriverOwnershipCoherence);
// this test pins the template side of the same contracts, including the
// string-"true" gate (the template compares toString, not a bool).
func TestGCPDriverInstallerRenderedTolerations(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("components/gcp-driver-installer/manifests/nvidia-driver-installer.yaml")
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	render := func(t *testing.T, values map[string]any) string {
		t.Helper()
		out, renderErr := manifest.Render(content, manifest.RenderInput{
			ComponentName: "gcp-driver-installer",
			Namespace:     "kube-system",
			Values:        values,
		})
		if renderErr != nil {
			t.Fatalf("Render() failed: %v", renderErr)
		}
		return string(out)
	}
	gated := func(gate any, extra map[string]any) map[string]any {
		values := map[string]any{
			"installer":     map[string]any{"enabled": gate},
			"driverVersion": "580.173.02",
		}
		for k, v := range extra {
			values[k] = v
		}
		return values
	}

	t.Run("no acceleratedTolerations → tolerate-all fallback renders", func(t *testing.T) {
		t.Parallel()
		out := render(t, gated(true, nil))
		if !strings.Contains(out, "tolerations:") || !strings.Contains(out, "- operator: Exists") {
			t.Errorf("rendered DaemonSet lacks the tolerate-all fallback; got tolerations block:\n%s",
				excerpt(out, "tolerations:"))
		}
	})

	t.Run("supplied acceleratedTolerations replace the fallback", func(t *testing.T) {
		t.Parallel()
		out := render(t, gated(true, map[string]any{
			"acceleratedTolerations": []any{
				map[string]any{"key": "dedicated", "operator": "Equal", "value": "gpu", "effect": "NoSchedule"},
			},
		}))
		if !strings.Contains(out, "key: dedicated") {
			t.Errorf("supplied toleration missing from render:\n%s", excerpt(out, "tolerations:"))
		}
		if strings.Contains(out, "- operator: Exists") {
			t.Errorf("tolerate-all fallback rendered alongside supplied tolerations "+
				"(the wildcard would defeat explicit restrictive tolerations):\n%s",
				excerpt(out, "tolerations:"))
		}
	})

	t.Run("string-true gate renders the DaemonSet (parity with the Go gate)", func(t *testing.T) {
		t.Parallel()
		out := render(t, gated("true", nil))
		if !strings.Contains(out, "kind: DaemonSet") {
			t.Errorf("installer.enabled=\"true\" (string) rendered nothing — the template "+
				"gate diverged from BundleSuppliesGKEDriver's fmt.Sprint parity; got:\n%s", out)
		}
	})

	t.Run("gate off renders nothing", func(t *testing.T) {
		t.Parallel()
		for name, gate := range map[string]any{"bool false": false, "string false": "false"} {
			if out := render(t, gated(gate, nil)); strings.Contains(out, "kind: DaemonSet") {
				t.Errorf("%s: gated-off render still produced the DaemonSet", name)
			}
		}
	})
}

// excerpt returns ~10 lines of s starting at the first occurrence of marker,
// keeping failure output readable instead of dumping the whole manifest.
func excerpt(s, marker string) string {
	idx := strings.Index(s, marker)
	if idx < 0 {
		return "(marker not found)"
	}
	lines := strings.SplitN(s[idx:], "\n", 11)
	if len(lines) == 11 {
		lines = lines[:10]
	}
	return strings.Join(lines, "\n")
}
