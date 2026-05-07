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

package os

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/measurement"
)

func TestKModCollector_Collect(t *testing.T) {
	ctx := context.TODO()
	collector := &Collector{}

	// This test validates the interface works correctly
	m, err := collector.Collect(ctx)
	if err != nil {
		if m != nil {
			t.Error("Expected nil measurement on error")
		}
		if errors.Is(err, os.ErrNotExist) {
			t.Skip("/proc/modules not available on this system")
			return
		}
		if !errors.Is(err, os.ErrPermission) {
			t.Errorf("Collect() unexpected error = %v", err)
		}
	}
}

func TestKModCollector_Collect_ContextCancellation(t *testing.T) {
	// Use an already-expired context to ensure deterministic cancellation
	ctx, cancel := context.WithDeadline(context.TODO(), time.Now().Add(-time.Second))
	defer cancel()

	collector := &Collector{}
	m, err := collector.Collect(ctx)

	if err == nil {
		t.Fatal("expected error from canceled context")
	}

	if m != nil {
		t.Error("Expected nil measurement on error")
	}
}

func TestKModCollector_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.TODO()
	collector := &Collector{}

	m, err := collector.Collect(ctx)
	if err != nil {
		// /proc/modules might not exist on all systems
		if errors.Is(err, os.ErrNotExist) {
			t.Skip("/proc/modules not available")
			return
		}
		t.Fatalf("Collect() failed: %v", err)
	}

	// Should return measurement with TypeOS and three subtypes
	if m == nil {
		t.Fatal("Expected non-nil measurement")
		return
	}

	if m.Type != measurement.TypeOS {
		t.Errorf("Expected type %s, got %s", measurement.TypeOS, m.Type)
	}

	if len(m.Subtypes) != 4 {
		t.Errorf("Expected 4 subtypes (grub, sysctl, kmod, release), got %d", len(m.Subtypes))
		return
	}

	// Find the kmod subtype
	var kmodSubtype *measurement.Subtype
	for i := range m.Subtypes {
		if m.Subtypes[i].Name == "kmod" {
			kmodSubtype = &m.Subtypes[i]
			break
		}
	}

	if kmodSubtype == nil {
		t.Fatal("Expected to find kmod subtype")
		return
	}

	// Validate that Data contains module names
	data := kmodSubtype.Data
	if data == nil {
		t.Error("Expected non-nil Data map")
		return
	}

	// Most systems have at least a few kernel modules loaded
	if len(data) == 0 {
		t.Error("Expected at least one kernel module")
	}

	t.Logf("Found %d loaded kernel modules", len(data))
}

func TestKModCollector_ParsesModules(t *testing.T) {
	tests := []struct {
		name            string
		modulesContent  string
		expectedModules []string
	}{
		{
			name: "typical kernel modules",
			modulesContent: `nvidia_uvm 1605632 0 - Live 0x0000000000000000 (POE)
nvidia_drm 69632 0 - Live 0x0000000000000000 (POE)
nvidia_modeset 1286144 1 nvidia_drm, Live 0x0000000000000000 (POE)
nvidia 56623104 2 nvidia_uvm,nvidia_modeset, Live 0x0000000000000000 (POE)
drm_kms_helper 311296 1 nvidia_drm, Live 0x0000000000000000
drm 622592 3 nvidia_drm,drm_kms_helper, Live 0x0000000000000000`,
			expectedModules: []string{"nvidia_uvm", "nvidia_drm", "nvidia_modeset", "nvidia", "drm_kms_helper", "drm"},
		},
		{
			name: "network modules",
			modulesContent: `mlx5_core 1720320 0 - Live 0x0000000000000000
mlx5_ib 421888 0 - Live 0x0000000000000000
ib_core 442368 1 mlx5_ib, Live 0x0000000000000000`,
			expectedModules: []string{"mlx5_core", "mlx5_ib", "ib_core"},
		},
		{
			name:            "single module",
			modulesContent:  `ext4 937984 1 - Live 0x0000000000000000`,
			expectedModules: []string{"ext4"},
		},
		{
			name: "modules with dependencies",
			modulesContent: `nf_conntrack 180224 2 nf_conntrack_ipv4,nf_nat, Live 0x0000000000000000
nf_nat 36864 1 nf_nat_ipv4, Live 0x0000000000000000
ip_tables 28672 0 - Live 0x0000000000000000`,
			expectedModules: []string{"nf_conntrack", "nf_nat", "ip_tables"},
		},
		{
			name: "mixed format with various states",
			modulesContent: `usb_storage 77824 0 - Live 0x0000000000000000
sd_mod 57344 2 - Live 0x0000000000000000
uas 32768 0 - Live 0x0000000000000000`,
			expectedModules: []string{"usb_storage", "sd_mod", "uas"},
		},
		{
			name:            "empty file",
			modulesContent:  "",
			expectedModules: []string{},
		},
		{
			name: "modules with special characters in dependencies",
			modulesContent: `bluetooth 737280 41 btrtl,btintel,btbcm,bnep,btusb,rfcomm, Live 0x0000000000000000
rfkill 32768 7 bluetooth,cfg80211, Live 0x0000000000000000`,
			expectedModules: []string{"bluetooth", "rfkill"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temporary file with test modules content
			tmpfile, err := os.CreateTemp("", "modules-*.txt")
			if err != nil {
				t.Fatalf("Failed to create temp file: %v", err)
			}
			defer os.Remove(tmpfile.Name())

			if _, writeErr := tmpfile.WriteString(tt.modulesContent); writeErr != nil {
				t.Fatalf("Failed to write temp file: %v", writeErr)
			}
			tmpfile.Close()

			// Temporarily override the file path variable
			originalPath := filePathKMod
			defer func() { filePathKMod = originalPath }()
			filePathKMod = tmpfile.Name()

			// Run the collector
			ctx := context.TODO()
			collector := &Collector{}
			subtype, err := collector.collectKMod(ctx)
			if err != nil {
				t.Fatalf("collectKMod() failed: %v", err)
			}

			if subtype == nil {
				t.Fatal("Expected non-nil subtype")
				return
			}

			if subtype.Name != "kmod" {
				t.Errorf("Expected subtype name 'kmod', got %q", subtype.Name)
			}

			data := subtype.Data

			// Verify expected modules are present
			for _, moduleName := range tt.expectedModules {
				reading, exists := data[moduleName]
				if !exists {
					t.Errorf("Expected module %q not found in results", moduleName)
					continue
				}

				// Verify the value is a boolean true
				value := reading.Any()
				boolVal, ok := value.(bool)
				if !ok || !boolVal {
					t.Errorf("Module %q: expected bool(true), got %v (type %T)", moduleName, value, value)
				}

				t.Logf("✓ Module loaded: %s", moduleName)
			}

			// Verify no unexpected modules
			if len(data) != len(tt.expectedModules) {
				t.Errorf("Expected %d modules, got %d", len(tt.expectedModules), len(data))
				for moduleName := range data {
					found := false
					for _, expected := range tt.expectedModules {
						if moduleName == expected {
							found = true
							break
						}
					}
					if !found {
						t.Errorf("Unexpected module %q found in results", moduleName)
					}
				}
			}

			t.Logf("Total modules parsed: %d", len(data))
		})
	}
}
