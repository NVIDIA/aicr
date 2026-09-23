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

package k8s

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
)

func writeGKEPoolsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pools.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write pools file: %v", err)
	}
	return path
}

func TestProjectGKEGPUPools(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		wantDriver string // "" = gpu-driver-installation key must be absent
		wantCount  int
		wantPools  string
	}{
		{
			// The bundle-installer pool-creation requirement.
			name: "all pools disabled",
			content: `[
			  {"name":"gpu1","config":{"accelerators":[
			    {"acceleratorType":"nvidia-h100-80gb","gpuDriverInstallationConfig":{"gpuDriverVersion":"INSTALLATION_DISABLED"}}
			  ]}},
			  {"name":"system","config":{}}
			]`,
			wantDriver: "Disabled",
			wantCount:  1,
			wantPools:  "gpu1=Disabled",
		},
		{
			// The gke-default requirement: GKE installs the driver.
			name: "all pools default",
			content: `[
			  {"name":"gpu1","config":{"accelerators":[
			    {"acceleratorType":"nvidia-h100-80gb","gpuDriverInstallationConfig":{"gpuDriverVersion":"DEFAULT"}}
			  ]}}
			]`,
			wantDriver: "Installed",
			wantCount:  1,
			wantPools:  "gpu1=Installed",
		},
		{
			name: "latest driver version counts as installed",
			content: `[
			  {"name":"gpu1","config":{"accelerators":[
			    {"acceleratorType":"nvidia-h100-80gb","gpuDriverInstallationConfig":{"gpuDriverVersion":"LATEST"}}
			  ]}}
			]`,
			wantDriver: "Installed",
			wantCount:  1,
			wantPools:  "gpu1=Installed",
		},
		{
			// GKE's default behavior for an absent config depends on the
			// cluster's control-plane version, so this must not assert
			// Installed.
			name: "absent gpuDriverInstallationConfig projects NotConfigured",
			content: `[
			  {"name":"gpu1","config":{"accelerators":[{"acceleratorType":"nvidia-h100-80gb"}]}}
			]`,
			wantDriver: "NotConfigured",
			wantCount:  1,
			wantPools:  "gpu1=NotConfigured",
		},
		{
			// Explicit unspecified is likewise version-dependent, so it
			// gets its own non-committal state, distinct from an absent
			// config.
			name: "unspecified driver version projects NotInstalled",
			content: `[
			  {"name":"gpu1","config":{"accelerators":[
			    {"acceleratorType":"nvidia-h100-80gb","gpuDriverInstallationConfig":{"gpuDriverVersion":"GPU_DRIVER_VERSION_UNSPECIFIED"}}
			  ]}}
			]`,
			wantDriver: "NotInstalled",
			wantCount:  1,
			wantPools:  "gpu1=NotInstalled",
		},
		{
			name: "empty driver version projects NotInstalled",
			content: `[
			  {"name":"gpu1","config":{"accelerators":[
			    {"acceleratorType":"nvidia-h100-80gb","gpuDriverInstallationConfig":{"gpuDriverVersion":""}}
			  ]}}
			]`,
			wantDriver: "NotInstalled",
			wantCount:  1,
			wantPools:  "gpu1=NotInstalled",
		},
		{
			// Below GKE's earliest documented default-install threshold,
			// an absent config resolves to Disabled, not a fallback state.
			name: "absent config on a pre-threshold version resolves to Disabled",
			content: `[
			  {"name":"gpu1","version":"1.28.5-gke.100000","config":{"accelerators":[
			    {"acceleratorType":"nvidia-h100-80gb"}
			  ]}}
			]`,
			wantDriver: "Disabled",
			wantCount:  1,
			wantPools:  "gpu1=Disabled",
		},
		{
			// Between the two thresholds, a regular (non-auto-provisioned)
			// pool already gets the default-install behavior.
			name: "unspecified on a mid-range regular pool resolves to Installed",
			content: `[
			  {"name":"gpu1","version":"1.31.0-gke.500000","config":{"accelerators":[
			    {"acceleratorType":"nvidia-h100-80gb","gpuDriverInstallationConfig":{"gpuDriverVersion":"GPU_DRIVER_VERSION_UNSPECIFIED"}}
			  ]}}
			]`,
			wantDriver: "Installed",
			wantCount:  1,
			wantPools:  "gpu1=Installed",
		},
		{
			// At version 1.31.0-gke.500000, GKE has not yet extended
			// the default-install behavior to auto-provisioned pools.
			name: "unspecified on a mid-range auto-provisioned pool resolves to Disabled",
			content: `[
			  {"name":"gpu1","version":"1.31.0-gke.500000","autoscaling":{"autoprovisioned":true},"config":{"accelerators":[
			    {"acceleratorType":"nvidia-h100-80gb","gpuDriverInstallationConfig":{"gpuDriverVersion":"GPU_DRIVER_VERSION_UNSPECIFIED"}}
			  ]}}
			]`,
			wantDriver: "Disabled",
			wantCount:  1,
			wantPools:  "gpu1=Disabled",
		},
		{
			// At and after the later threshold, auto-provisioned pools
			// also get the default-install behavior.
			name: "unspecified on a post-NAP-threshold auto-provisioned pool resolves to Installed",
			content: `[
			  {"name":"gpu1","version":"1.33.0-gke.999999","autoscaling":{"autoprovisioned":true},"config":{"accelerators":[
			    {"acceleratorType":"nvidia-h100-80gb","gpuDriverInstallationConfig":{"gpuDriverVersion":"GPU_DRIVER_VERSION_UNSPECIFIED"}}
			  ]}}
			]`,
			wantDriver: "Installed",
			wantCount:  1,
			wantPools:  "gpu1=Installed",
		},
		{
			// An unparseable version can't be resolved against the
			// thresholds, so this must not guess.
			name: "unparseable version falls back to NotConfigured",
			content: `[
			  {"name":"gpu1","version":"not-a-version","config":{"accelerators":[
			    {"acceleratorType":"nvidia-h100-80gb"}
			  ]}}
			]`,
			wantDriver: "NotConfigured",
			wantCount:  1,
			wantPools:  "gpu1=NotConfigured",
		},
		{
			// Disagreeing pools must not produce a clean value. This
			// models a labeled bundle-installer pool whose driver
			// installation was never actually disabled, the exact gap
			// this reading qualifies against.
			name: "mixed modes across pools project Mixed",
			content: `[
			  {"name":"gpu1","config":{"accelerators":[
			    {"acceleratorType":"nvidia-h100-80gb","gpuDriverInstallationConfig":{"gpuDriverVersion":"INSTALLATION_DISABLED"}}
			  ]}},
			  {"name":"gpu2","config":{"accelerators":[
			    {"acceleratorType":"nvidia-h100-80gb","gpuDriverInstallationConfig":{"gpuDriverVersion":"DEFAULT"}}
			  ]}}
			]`,
			wantDriver: "Mixed",
			wantCount:  2,
			wantPools:  "gpu1=Disabled,gpu2=Installed",
		},
		{
			// Disagreement within a single pool's accelerators also fails
			// closed, not just across pools.
			name: "mixed modes within one pool project Mixed",
			content: `[
			  {"name":"gpu1","config":{"accelerators":[
			    {"acceleratorType":"nvidia-h100-80gb","gpuDriverInstallationConfig":{"gpuDriverVersion":"INSTALLATION_DISABLED"}},
			    {"acceleratorType":"nvidia-h100-80gb","gpuDriverInstallationConfig":{"gpuDriverVersion":"DEFAULT"}}
			  ]}}
			]`,
			wantDriver: "Mixed",
			wantCount:  1,
			wantPools:  "gpu1=Mixed",
		},
		{
			// An unknown provider value is preserved verbatim so the
			// fail-closed error names what was actually observed.
			name: "unknown driver version preserved",
			content: `[
			  {"name":"gpu1","config":{"accelerators":[
			    {"acceleratorType":"nvidia-h100-80gb","gpuDriverInstallationConfig":{"gpuDriverVersion":"FUTURE_MODE"}}
			  ]}}
			]`,
			wantDriver: "FUTURE_MODE",
			wantCount:  1,
			wantPools:  "gpu1=FUTURE_MODE",
		},
		{
			// A pool with no accelerators is not a GPU pool.
			name:       "no GPU pools omits the reading",
			content:    `[{"name":"system","config":{}},{"name":"nolist","config":null}]`,
			wantDriver: "",
			wantCount:  0,
		},
		{
			name:       "empty accelerators list is not a GPU pool",
			content:    `[{"name":"gpu1","config":{"accelerators":[]}}]`,
			wantDriver: "",
			wantCount:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subtype, err := ProjectGKEGPUPools(t.Context(), writeGKEPoolsFile(t, tt.content))
			if err != nil {
				t.Fatalf("ProjectGKEGPUPools() error = %v", err)
			}
			if subtype.Name != SubtypeGKEGPUPools {
				t.Fatalf("subtype = %q, want %q", subtype.Name, SubtypeGKEGPUPools)
			}

			driver, present := subtype.Data["gpu-driver-installation"]
			if tt.wantDriver == "" {
				if present {
					t.Fatalf("gpu-driver-installation = %v, want the key absent", driver.Any())
				}
			} else if got, _ := driver.Any().(string); got != tt.wantDriver {
				t.Fatalf("gpu-driver-installation = %v, want %q", driver.Any(), tt.wantDriver)
			}

			if got, _ := subtype.Data["gpu-pool-count"].Any().(int); got != tt.wantCount {
				t.Fatalf("gpu-pool-count = %v, want %d", subtype.Data["gpu-pool-count"].Any(), tt.wantCount)
			}
			if tt.wantPools != "" {
				if got, _ := subtype.Data["gpu-pools"].Any().(string); got != tt.wantPools {
					t.Fatalf("gpu-pools = %v, want %q", subtype.Data["gpu-pools"].Any(), tt.wantPools)
				}
			}
		})
	}
}

func TestProjectGKEGPUPoolsFailsLoud(t *testing.T) {
	tests := []struct {
		name    string
		path    func(t *testing.T) string
		wantErr string
	}{
		{
			name:    "missing file",
			path:    func(t *testing.T) string { t.Helper(); return filepath.Join(t.TempDir(), "absent.json") },
			wantErr: "failed to open",
		},
		{
			name: "not the gcloud array shape",
			path: func(t *testing.T) string {
				t.Helper()
				return writeGKEPoolsFile(t, `{"nodePools":[]}`)
			},
			wantErr: "gcloud container node-pools list",
		},
		{
			// json.Unmarshal accepts a top-level null into a slice
			// without error. The explicit-input contract must reject it.
			name: "top-level JSON null",
			path: func(t *testing.T) string {
				t.Helper()
				return writeGKEPoolsFile(t, `null`)
			},
			wantErr: "got JSON null",
		},
		{
			name: "malformed JSON",
			path: func(t *testing.T) string {
				t.Helper()
				return writeGKEPoolsFile(t, `[{"name":`)
			},
			wantErr: "failed to decode",
		},
		{
			name: "oversized file",
			path: func(t *testing.T) string {
				t.Helper()
				return writeGKEPoolsFile(t, "["+strings.Repeat(" ", 1<<20)+"]")
			},
			wantErr: "exceeds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ProjectGKEGPUPools(t.Context(), tt.path(t))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ProjectGKEGPUPools() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestGKEReadingShapeMatchesProfileContract locks the projection to the
// reading the GKE gpuStack declaration references
// (K8s.gke-gpu-pools.gpu-driver-installation) so drift between the
// collector and the recipe constraints surfaces here.
func TestGKEReadingShapeMatchesProfileContract(t *testing.T) {
	subtype, err := ProjectGKEGPUPools(t.Context(), writeGKEPoolsFile(t,
		`[{"name":"gpu1","config":{"accelerators":[
		    {"acceleratorType":"nvidia-h100-80gb","gpuDriverInstallationConfig":{"gpuDriverVersion":"INSTALLATION_DISABLED"}}
		  ]}}]`))
	if err != nil {
		t.Fatalf("ProjectGKEGPUPools() error = %v", err)
	}

	m := measurement.NewMeasurement(measurement.TypeK8s).WithSubtype(subtype).Build()
	if m.Type != measurement.TypeK8s {
		t.Fatalf("measurement type = %q, want %q", m.Type, measurement.TypeK8s)
	}
	if subtype.Name != "gke-gpu-pools" {
		t.Fatalf("subtype = %q, want the literal gke-gpu-pools the constraints name", subtype.Name)
	}
	if got, _ := subtype.Data["gpu-driver-installation"].Any().(string); got != "Disabled" {
		t.Fatalf("gpu-driver-installation = %v, want Disabled", subtype.Data["gpu-driver-installation"].Any())
	}
}

func TestGKEOmittedDriverMode(t *testing.T) {
	tests := []struct {
		name            string
		version         string
		autoprovisioned bool
		want            string
	}{
		{"below the earliest threshold", "1.28.5-gke.100000", false, gkeDriverInstallDisabled},
		{"below the earliest threshold even when auto-provisioned", "1.28.5-gke.100000", true, gkeDriverInstallDisabled},
		{"exactly the earliest threshold, regular pool", "1.30.1-gke.1156000", false, gkeDriverInstalled},
		{"exactly the earliest threshold, auto-provisioned pool", "1.30.1-gke.1156000", true, gkeDriverInstallDisabled},
		{"between the thresholds, regular pool", "1.31.0-gke.500000", false, gkeDriverInstalled},
		{"between the thresholds, auto-provisioned pool", "1.31.0-gke.500000", true, gkeDriverInstallDisabled},
		{"exactly the NAP threshold, auto-provisioned pool", "1.32.2-gke.1297000", true, gkeDriverInstalled},
		{"past the NAP threshold, auto-provisioned pool", "1.33.0-gke.999999", true, gkeDriverInstalled},
		{"unparseable version falls back", "not-a-version", false, "fallback"},
		{"missing version falls back", "", true, "fallback"},
		{"version without a gke build suffix falls back", "1.32.4", false, "fallback"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gkeOmittedDriverMode(tt.version, tt.autoprovisioned, "fallback")
			if got != tt.want {
				t.Fatalf("gkeOmittedDriverMode(%q, %v) = %q, want %q", tt.version, tt.autoprovisioned, got, tt.want)
			}
		})
	}
}
