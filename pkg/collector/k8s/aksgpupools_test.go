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

func writeAKSPoolsFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pools.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write pools file: %v", err)
	}
	return path
}

func TestProjectAKSGPUPools(t *testing.T) {
	tests := []struct {
		name       string
		content    string
		wantDriver string // "" = gpu-driver key must be absent
		wantCount  int
		wantPools  string
	}{
		{
			// The AKS "Driver only" default: gpuProfile present, Install.
			name: "all pools Install",
			content: `[
			  {"name":"gpu1","vmSize":"Standard_ND96isr_H100_v5","gpuProfile":{"driver":"Install"}},
			  {"name":"gpu2","vmSize":"Standard_NC24ads_A100_v4","gpuProfile":{"driver":"Install"}},
			  {"name":"system","vmSize":"Standard_D8s_v6"}
			]`,
			wantDriver: "Install",
			wantCount:  2,
			wantPools:  "gpu1=Install,gpu2=Install",
		},
		{
			name: "all pools None",
			content: `[
			  {"name":"gpu1","vmSize":"Standard_ND96isr_H100_v5","gpuProfile":{"driver":"None"}}
			]`,
			wantDriver: "None",
			wantCount:  1,
			wantPools:  "gpu1=None",
		},
		{
			// gpuProfile absent on a GPU pool follows the provider's
			// documented Install default (ADR-015 DD3).
			name: "absent gpuProfile defaults to Install",
			content: `[
			  {"name":"gpu1","vmSize":"standard_nd96isr_h100_v5"}
			]`,
			wantDriver: "Install",
			wantCount:  1,
			wantPools:  "gpu1=Install",
		},
		{
			// Explicit null gpuProfile is the same absent case.
			name: "null gpuProfile defaults to Install",
			content: `[
			  {"name":"gpu1","vmSize":"Standard_NC24ads_A100_v4","gpuProfile":null}
			]`,
			wantDriver: "Install",
			wantCount:  1,
			wantPools:  "gpu1=Install",
		},
		{
			// Disagreeing pools must not produce a clean value.
			name: "mixed modes project Mixed",
			content: `[
			  {"name":"gpu1","vmSize":"Standard_ND96isr_H100_v5","gpuProfile":{"driver":"Install"}},
			  {"name":"gpu2","vmSize":"Standard_ND96isr_H100_v5","gpuProfile":{"driver":"None"}}
			]`,
			wantDriver: "Mixed",
			wantCount:  2,
			wantPools:  "gpu1=Install,gpu2=None",
		},
		{
			// A fully AKS-managed pool is out of scope and must not
			// satisfy either declared constraint.
			name: "managed pool projects Managed",
			content: `[
			  {"name":"gpu1","vmSize":"Standard_ND96isr_H100_v5","gpuProfile":{"nvidia":{"managementMode":"Managed"}}}
			]`,
			wantDriver: "Managed",
			wantCount:  1,
			wantPools:  "gpu1=Managed",
		},
		{
			// Unmanaged delegates only the Kubernetes GPU stack to the
			// user; the driver still follows gpuProfile.driver, so this
			// is a supported azure-managed pool, not a Managed rejection.
			name: "unmanaged nvidia block follows the driver field",
			content: `[
			  {"name":"gpu1","vmSize":"Standard_ND96isr_H100_v5","gpuProfile":{"driver":"Install","nvidia":{"managementMode":"Unmanaged"}}}
			]`,
			wantDriver: "Install",
			wantCount:  1,
			wantPools:  "gpu1=Install",
		},
		{
			// An unknown managementMode with the nvidia block present
			// fails closed via the Managed marker.
			name: "unknown managementMode fails closed as Managed",
			content: `[
			  {"name":"gpu1","vmSize":"Standard_ND96isr_H100_v5","gpuProfile":{"driver":"Install","nvidia":{"managementMode":"Future"}}}
			]`,
			wantDriver: "Managed",
			wantCount:  1,
			wantPools:  "gpu1=Managed",
		},
		{
			// An unknown provider value is preserved verbatim so the
			// fail-closed error names what was actually observed.
			name: "unknown driver value preserved",
			content: `[
			  {"name":"gpu1","vmSize":"Standard_ND96isr_H100_v5","gpuProfile":{"driver":"Future"}}
			]`,
			wantDriver: "Future",
			wantCount:  1,
			wantPools:  "gpu1=Future",
		},
		{
			// NV family counts as a GPU pool; NG (AMD) and CPU pools do not.
			name: "family filtering",
			content: `[
			  {"name":"viz","vmSize":"Standard_NV36ads_A10_v5","gpuProfile":{"driver":"Install"}},
			  {"name":"amd","vmSize":"Standard_NG32ads_V620_v1","gpuProfile":{"driver":"None"}},
			  {"name":"cpu","vmSize":"Standard_D8s_v6"}
			]`,
			wantDriver: "Install",
			wantCount:  1,
			wantPools:  "viz=Install",
		},
		{
			// AMD MI300X is an ND size AKS requires creating with
			// --gpu-driver none; counting it beside an NVIDIA Install
			// pool would falsely aggregate to Mixed and reject a valid
			// azure-managed cluster.
			name: "AMD MI300X inside the ND family is excluded",
			content: `[
			  {"name":"gpu1","vmSize":"Standard_ND96isr_H100_v5","gpuProfile":{"driver":"Install"}},
			  {"name":"amd","vmSize":"Standard_ND96isr_MI300X_v5","gpuProfile":{"driver":"None"}}
			]`,
			wantDriver: "Install",
			wantCount:  1,
			wantPools:  "gpu1=Install",
		},
		{
			// AMD Radeon Pro V620/V710 live inside the NV family.
			name: "AMD NV-family sizes are excluded",
			content: `[
			  {"name":"viz","vmSize":"Standard_NV36ads_A10_v5","gpuProfile":{"driver":"Install"}},
			  {"name":"amdviz","vmSize":"Standard_NV28adms_V710_v5","gpuProfile":{"driver":"None"}}
			]`,
			wantDriver: "Install",
			wantCount:  1,
			wantPools:  "viz=Install",
		},
		{
			// An all-AMD cluster has no NVIDIA GPU pools: the reading is
			// omitted and profile resolution fails closed as unavailable.
			name:       "AMD-only cluster omits the reading",
			content:    `[{"name":"amd","vmSize":"Standard_ND96isr_MI300X_v5","gpuProfile":{"driver":"None"}}]`,
			wantDriver: "",
			wantCount:  0,
		},
		{
			// No GPU pools: the gpu-driver key is omitted so constraint
			// evaluation reports the reading unavailable and fails closed.
			name:       "no GPU pools omits the reading",
			content:    `[{"name":"system","vmSize":"Standard_D8s_v6"}]`,
			wantDriver: "",
			wantCount:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			subtype, err := ProjectAKSGPUPools(t.Context(), writeAKSPoolsFile(t, tt.content))
			if err != nil {
				t.Fatalf("ProjectAKSGPUPools(t.Context(), ) error = %v", err)
			}
			if subtype.Name != SubtypeAKSGPUPools {
				t.Fatalf("subtype = %q, want %q", subtype.Name, SubtypeAKSGPUPools)
			}

			driver, present := subtype.Data["gpu-driver"]
			if tt.wantDriver == "" {
				if present {
					t.Fatalf("gpu-driver = %v, want the key absent", driver.Any())
				}
			} else if got, _ := driver.Any().(string); got != tt.wantDriver {
				t.Fatalf("gpu-driver = %v, want %q", driver.Any(), tt.wantDriver)
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

func TestProjectAKSGPUPoolsFailsLoud(t *testing.T) {
	tests := []struct {
		name    string
		path    func(t *testing.T) string
		wantErr string
	}{
		{
			// The descriptor-first open runs before any read, so a
			// missing file surfaces at open.
			name:    "missing file",
			path:    func(t *testing.T) string { t.Helper(); return filepath.Join(t.TempDir(), "absent.json") },
			wantErr: "failed to open",
		},
		{
			name: "not the az array shape",
			path: func(t *testing.T) string {
				t.Helper()
				return writeAKSPoolsFile(t, `{"value":[]}`)
			},
			wantErr: "az aks nodepool list",
		},
		{
			// json.Unmarshal accepts a top-level null into a slice
			// without error; the explicit-input contract must reject it.
			name: "top-level JSON null",
			path: func(t *testing.T) string {
				t.Helper()
				return writeAKSPoolsFile(t, `null`)
			},
			wantErr: "got JSON null",
		},
		{
			name: "malformed JSON",
			path: func(t *testing.T) string {
				t.Helper()
				return writeAKSPoolsFile(t, `[{"name":`)
			},
			wantErr: "failed to decode",
		},
		{
			name: "oversized file",
			path: func(t *testing.T) string {
				t.Helper()
				return writeAKSPoolsFile(t, "["+strings.Repeat(" ", 1<<20)+"]")
			},
			wantErr: "exceeds",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ProjectAKSGPUPools(t.Context(), tt.path(t))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ProjectAKSGPUPools(t.Context(), ) error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestReadRejectsNonRegularFile pins the descriptor-first gate: a bounded
// reader limits bytes, not time, so a symlink is rejected at open
// (O_NOFOLLOW) and a non-regular file is rejected on the opened
// descriptor before any read.
func TestReadRejectsNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	_, dirErr := ProjectAKSGPUPools(t.Context(), dir)
	if dirErr == nil || !strings.Contains(dirErr.Error(), "not a regular file") {
		t.Fatalf("directory error = %v, want not-a-regular-file rejection", dirErr)
	}

	target := writeAKSPoolsFile(t, `[]`)
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	_, linkErr := ProjectAKSGPUPools(t.Context(), link)
	if linkErr == nil || !strings.Contains(linkErr.Error(), "not a regular file") {
		t.Fatalf("symlink error = %v, want not-a-regular-file rejection", linkErr)
	}
}

// The fail-loud orchestration contract (a bad file fails the snapshot run,
// never degrades to a missing reading) is pinned in pkg/snapshotter tests —
// the projection no longer rides the collector path at all.

// TestReadingShapeMatchesProfileContract locks the projection to the reading
// the AKS gpuStack declaration references (K8s.aks-gpu-pools.gpu-driver) so
// drift between the collector and the recipe constraints surfaces here.
func TestReadingShapeMatchesProfileContract(t *testing.T) {
	subtype, err := ProjectAKSGPUPools(t.Context(), writeAKSPoolsFile(t,
		`[{"name":"gpu1","vmSize":"Standard_ND96isr_H100_v5","gpuProfile":{"driver":"Install"}}]`))
	if err != nil {
		t.Fatalf("ProjectAKSGPUPools(t.Context(), ) error = %v", err)
	}

	m := measurement.NewMeasurement(measurement.TypeK8s).WithSubtype(subtype).Build()
	if m.Type != measurement.TypeK8s {
		t.Fatalf("measurement type = %q, want %q", m.Type, measurement.TypeK8s)
	}
	if subtype.Name != "aks-gpu-pools" {
		t.Fatalf("subtype = %q, want the literal aks-gpu-pools the constraints name", subtype.Name)
	}
	if got, _ := subtype.Data["gpu-driver"].Any().(string); got != "Install" {
		t.Fatalf("gpu-driver = %v, want Install", subtype.Data["gpu-driver"].Any())
	}
}
