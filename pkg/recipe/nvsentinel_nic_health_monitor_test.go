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
	"os"
	"strings"
	"testing"
)

const nicHealthMonitorMixin = "nvsentinel-nic-health-monitor"

// TestNVSentinelNICDriverCheckEnabledEverywhere pins step one of NIC fault
// detection:
// SysLogsNICDriverError joins the three GPU checks in every recipe, not just
// the Mellanox platforms. Helm replaces lists rather than merging them, so
// the values file must restate the chart's three defaults alongside the new
// entry -- a partial list would silently drop XID/SXID/GPUFallenOff
// detection fleet-wide, which is the failure this test exists to catch.
//
// Every pattern also carries processingStrategy: STORE_ONLY, overriding the
// subchart-wide EXECUTE_REMEDIATION default without disturbing the GPU
// checks that keep it.
func TestNVSentinelNICDriverCheckEnabledEverywhere(t *testing.T) {
	t.Parallel()

	wantChecks := []string{
		"SysLogsXIDError",
		"SysLogsSXIDError",
		"SysLogsGPUFallenOff",
		"SysLogsNICDriverError",
	}

	for _, tt := range nicHealthMonitorPlatforms() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			values := resolveNVSentinelValues(t, tt.criteria)
			syslog, ok := values["syslog-health-monitor"].(map[string]any)
			if !ok {
				t.Fatal("syslog-health-monitor values missing; the NIC driver checks are not applied to this platform")
			}

			raw, ok := syslog["enabledChecks"].([]any)
			if !ok {
				t.Fatal("syslog-health-monitor.enabledChecks missing or not a list")
			}
			got := make([]string, 0, len(raw))
			for _, c := range raw {
				s, _ := c.(string)
				got = append(got, s)
			}
			if len(got) != len(wantChecks) {
				t.Fatalf("enabledChecks = %v, want exactly %v (Helm replaces the list, so every default must be restated)", got, wantChecks)
			}
			for i, want := range wantChecks {
				if got[i] != want {
					t.Fatalf("enabledChecks = %v, want %v", got, wantChecks)
				}
			}

			detection, ok := syslog["nicDriverDetection"].(map[string]any)
			if !ok {
				t.Fatal("syslog-health-monitor.nicDriverDetection missing; SysLogsNICDriverError is enabled with no patterns to match")
			}
			patterns, ok := detection["patterns"].([]any)
			if !ok || len(patterns) == 0 {
				t.Fatal("nicDriverDetection.patterns missing or empty")
			}
			for _, p := range patterns {
				pattern, _ := p.(map[string]any)
				name, _ := pattern["name"].(string)
				if enabled, _ := pattern["enabled"].(bool); !enabled {
					t.Errorf("pattern %q is not enabled", name)
				}
				if strategy, _ := pattern["processingStrategy"].(string); strategy != "STORE_ONLY" {
					t.Errorf("pattern %q processingStrategy = %q, want STORE_ONLY -- several NIC faults "+
						"recommend REPLACE_VM, the most destructive action in the pipeline", name, strategy)
				}
			}
		})
	}
}

// TestNVSentinelNICHealthMonitorScopedToMellanoxPlatforms pins step two: the nic-health-monitor subchart is on for AKS and OKE, the two
// families that deploy network-operator/ConnectX, and off everywhere else.
// Upstream supports Mellanox only, so enabling it on EKS (aws-efa) or GKE
// COS (gke-nccl-tcpxo) would run a DaemonSet that discovers no devices.
//
// The mixin is referenced from the aks and oke-ol root overlays; this
// resolves real leaves to prove the reference actually reaches them through
// the inheritance chain.
func TestNVSentinelNICHealthMonitorScopedToMellanoxPlatforms(t *testing.T) {
	t.Parallel()

	store, err := loadMetadataStore(t.Context())
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}
	if _, ok := store.Mixins[nicHealthMonitorMixin]; !ok {
		t.Fatalf("%s mixin not present; check recipes/mixins/%s.yaml", nicHealthMonitorMixin, nicHealthMonitorMixin)
	}

	for _, tt := range nicHealthMonitorPlatforms() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			values := resolveNVSentinelValues(t, tt.criteria)
			enabled, set := nestedBool(valuesGlobal(t, values), "nicHealthMonitor", "enabled")

			switch {
			case tt.wantMonitor && !set:
				t.Fatalf("global.nicHealthMonitor.enabled is unset, want true -- %s deploys "+
					"network-operator/ConnectX and should compose the %s mixin", tt.name, nicHealthMonitorMixin)
			case tt.wantMonitor && !enabled:
				t.Fatalf("global.nicHealthMonitor.enabled = false, want true")
			case !tt.wantMonitor && set && enabled:
				t.Fatalf("global.nicHealthMonitor.enabled = true, want it left unset -- %s has no "+
					"Mellanox NICs, so the monitor would find no devices to check", tt.name)
			}

			if !tt.wantMonitor {
				return
			}
			monitor, ok := values["nic-health-monitor"].(map[string]any)
			if !ok {
				t.Fatal("nic-health-monitor values missing")
			}
			if strategy, _ := monitor["processingStrategy"].(string); strategy != "STORE_ONLY" {
				t.Errorf("nic-health-monitor.processingStrategy = %q, want STORE_ONLY -- link_downed and "+
					"several sibling counters treat any increment as fatal and remediate with REPLACE_VM", strategy)
			}
		})
	}
}

type nicHealthMonitorPlatform struct {
	name        string
	criteria    *Criteria
	wantMonitor bool
}

// nicHealthMonitorPlatforms spans both sides of the Mellanox boundary, plus
// the OKE accelerators the issue's own table omits (a100) so a future
// per-accelerator split has to update this list deliberately.
func nicHealthMonitorPlatforms() []nicHealthMonitorPlatform {
	return []nicHealthMonitorPlatform{
		{
			name:        "AKS H100 training",
			criteria:    &Criteria{Service: CriteriaServiceAKS, Accelerator: CriteriaAcceleratorH100, OS: CriteriaOSUbuntu, Intent: CriteriaIntentTraining},
			wantMonitor: true,
		},
		{
			name:        "AKS H100 inference dynamo",
			criteria:    &Criteria{Service: CriteriaServiceAKS, Accelerator: CriteriaAcceleratorH100, OS: CriteriaOSUbuntu, Intent: CriteriaIntentInference, Platform: CriteriaPlatformDynamo},
			wantMonitor: true,
		},
		{
			name:        "OKE GB200 training",
			criteria:    &Criteria{Service: CriteriaServiceOKE, Accelerator: CriteriaAcceleratorGB200, OS: CriteriaOSUbuntu, Intent: CriteriaIntentTraining},
			wantMonitor: true,
		},
		{
			name:        "OKE L40S training",
			criteria:    &Criteria{Service: CriteriaServiceOKE, Accelerator: CriteriaAcceleratorL40S, OS: CriteriaOSOracleLinux, Intent: CriteriaIntentTraining},
			wantMonitor: true,
		},
		{
			name:        "OKE A100 training",
			criteria:    &Criteria{Service: CriteriaServiceOKE, Accelerator: CriteriaAcceleratorA100, OS: CriteriaOSUbuntu, Intent: CriteriaIntentTraining},
			wantMonitor: true,
		},
		{
			name:     "EKS H100 training (aws-efa, not Mellanox)",
			criteria: &Criteria{Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100, OS: CriteriaOSUbuntu, Intent: CriteriaIntentTraining},
		},
		{
			name:     "GKE COS H100 training (gke-nccl-tcpxo, not Mellanox)",
			criteria: &Criteria{Service: CriteriaServiceGKE, Accelerator: CriteriaAcceleratorH100, OS: CriteriaOSCOS, Intent: CriteriaIntentTraining},
		},
		{
			name:     "Kind H100 training (network-operator simulated, no real NICs)",
			criteria: &Criteria{Service: CriteriaServiceKind, Accelerator: CriteriaAcceleratorH100, Intent: CriteriaIntentTraining},
		},
	}
}

func resolveNVSentinelValues(t *testing.T, criteria *Criteria) map[string]any {
	t.Helper()

	result, err := NewBuilder().BuildFromCriteria(t.Context(), criteria)
	if err != nil {
		t.Fatalf("BuildFromCriteria: %v", err)
	}
	ref := result.GetComponentRef(nvsentinelComponent)
	if ref == nil {
		t.Fatal("nvsentinel componentRef missing — the assertions would be vacuous")
	}
	if !ref.IsEnabled() {
		t.Fatal("nvsentinel is disabled — the assertions would be vacuous")
	}
	values, err := result.GetValuesForComponentWithContext(t.Context(), nvsentinelComponent)
	if err != nil {
		t.Fatalf("GetValuesForComponentWithContext(nvsentinel): %v", err)
	}

	return values
}

func valuesGlobal(t *testing.T, values map[string]any) map[string]any {
	t.Helper()
	global, ok := values["global"].(map[string]any)
	if !ok {
		t.Fatal("nvsentinel values have no global block")
	}

	return global
}

// TestNICHealthMonitorImagePinnedEverywhere keeps every hand-maintained copy
// of the nic-health-monitor image in step with nvsentinel's registry pin, and
// requires each surface to carry it at all.
//
// Same two silent failures TestObjectMonitorImagePinnedEverywhere guards for
// its own image: the subchart image inherits the parent chart's version, so
// bumping defaultVersion moves the deployed image while these copies keep
// naming the old one; and dropping the image from the scan, mirror or
// documentation surfaces would drop this image from coverage while
// leaving every other test green. Absence is an error, not a skip.
func TestNICHealthMonitorImagePinnedEverywhere(t *testing.T) {
	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	nvsentinel := registry.Get("nvsentinel")
	if nvsentinel == nil {
		t.Fatal("nvsentinel not found in registry")
	}
	version := nvsentinel.Helm.DefaultVersion
	if version == "" {
		t.Fatal("nvsentinel registry entry has no defaultVersion")
	}
	const image = "ghcr.io/nvidia/nvsentinel/nic-health-monitor"

	const scanWorkflow = "../../.github/workflows/vuln-scan-images.yaml"
	tag, found := scanMatrixTagFor(t, scanWorkflow, image)
	switch {
	case !found:
		t.Errorf("%s has no scan matrix entry for %s -- the scan job must cover this image", scanWorkflow, image)
	case tag != version:
		t.Errorf("%s pins tag %q for %s, want nvsentinel's defaultVersion %q", scanWorkflow, tag, image, version)
	}

	for _, s := range []struct{ path, why string }{
		{"../../tools/mirror-e2e", "the mirror job must cover this image"},
		{"../../docs/user/container-images.md", "the image must stay documented"},
		{"../../pkg/bundler/validations/nvsentinel_nic_health_monitor_render_test.go", "the render test asserts this image reaches the chart"},
	} {
		data, err := os.ReadFile(s.path)
		if err != nil {
			t.Errorf("reading %s: %v", s.path, err)
			continue
		}
		body := string(data)
		if !strings.Contains(body, image) {
			t.Errorf("%s no longer references %s -- %s", s.path, image, s.why)
			continue
		}
		if !strings.Contains(body, image+":"+version) {
			t.Errorf("%s does not pin %s:%s; bump it alongside nvsentinel's defaultVersion", s.path, image, version)
		}
	}
}
