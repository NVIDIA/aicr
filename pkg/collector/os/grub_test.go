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

const grubSubtypeName = "grub"

func TestGrubCollector_Collect_ContextCancellation(t *testing.T) {
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

func TestGrubCollector_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.TODO()
	collector := &Collector{}

	m, err := collector.Collect(ctx)
	if err != nil {
		// /proc/cmdline might not exist on all systems
		if errors.Is(err, os.ErrNotExist) {
			t.Skip("/proc/cmdline not available on this system")
			return
		}
		t.Fatalf("Collect() failed: %v", err)
	}

	// Should return measurement with TypeOS and three subtypes: grub, sysctl, kmod
	if m == nil {
		t.Fatal("Expected non-nil measurement")
		return
	}

	if m.Type != measurement.TypeOS {
		t.Errorf("Expected type %s, got %s", measurement.TypeOS, m.Type)
	}

	if len(m.Subtypes) != 4 {
		t.Errorf("Expected exactly 4 subtypes (grub, sysctl, kmod, release), got %d", len(m.Subtypes))
		return
	}

	// Find the grub subtype
	var grubSubtype *measurement.Subtype
	for i := range m.Subtypes {
		if m.Subtypes[i].Name == grubSubtypeName {
			grubSubtype = &m.Subtypes[i]
			break
		}
	}

	if grubSubtype == nil {
		t.Fatal("Expected to find grub subtype")
		return
	}

	// Validate that Data is a map
	props := grubSubtype.Data
	if props == nil {
		t.Error("Expected non-nil Data map")
		return
	}

	// Most systems have at least a few boot parameters
	if len(props) == 0 {
		t.Error("Expected at least one boot parameter")
	}

	t.Logf("Found %d boot parameters", len(props))
}

func TestGrubCollector_ValidatesParsing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.TODO()
	collector := &Collector{}

	m, err := collector.Collect(ctx)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Skip("/proc/cmdline not available on this system")
			return
		}
		t.Fatalf("Collect() failed: %v", err)
	}

	if m == nil || len(m.Subtypes) == 0 {
		t.Fatal("Expected at least one subtype")
	}

	// Find grub subtype
	var grubSubtype *measurement.Subtype
	for i := range m.Subtypes {
		if m.Subtypes[i].Name == grubSubtypeName {
			grubSubtype = &m.Subtypes[i]
			break
		}
	}

	if grubSubtype == nil {
		t.Fatal("Expected to find grub subtype")
		return
	}

	props := grubSubtype.Data

	// Check that we can parse both key-only and key=value formats
	hasKeyOnly := false
	hasKeyValue := false

	for key, value := range props {
		if key == "" {
			t.Error("Found empty key in Properties")
			continue
		}

		strVal := value.Any()
		if strVal == "" {
			hasKeyOnly = true
			t.Logf("Key-only param: %s", key)
		} else {
			hasKeyValue = true
			t.Logf("Key=value param: %s=%v", key, strVal)
		}
	}

	t.Logf("Has key-only params: %v, Has key=value params: %v", hasKeyOnly, hasKeyValue)
}

func TestGrubCollector_ParsesKeyOnlyParameters(t *testing.T) {
	tests := []struct {
		name           string
		cmdlineContent string
		expectedKeys   map[string]string // key -> expected value ("" for key-only)
		filteredKeys   []string          // keys that should be filtered out
	}{
		{
			name:           "mixed key-only and key-value",
			cmdlineContent: "BOOT_IMAGE=/boot/vmlinuz ro quiet splash root=/dev/sda1 panic=-1",
			expectedKeys: map[string]string{
				"BOOT_IMAGE": "/boot/vmlinuz",
				"ro":         "",
				"quiet":      "",
				"splash":     "",
				"panic":      "-1",
			},
			filteredKeys: []string{"root"},
		},
		{
			name:           "all key-only parameters",
			cmdlineContent: "ro quiet nokaslr",
			expectedKeys: map[string]string{
				"ro":      "",
				"quiet":   "",
				"nokaslr": "",
			},
			filteredKeys: []string{},
		},
		{
			name:           "all key-value parameters",
			cmdlineContent: "hugepages=5128 hugepagesz=2M panic=-1",
			expectedKeys: map[string]string{
				"hugepages":  "5128",
				"hugepagesz": "2M",
				"panic":      "-1",
			},
			filteredKeys: []string{},
		},
		{
			name:           "grub parameters with security settings",
			cmdlineContent: "BOOT_IMAGE=/boot/vmlinuz-6.8.0 apparmor=1 security=apparmor audit=1 audit_backlog_limit=8192 ro",
			expectedKeys: map[string]string{
				"BOOT_IMAGE":          "/boot/vmlinuz-6.8.0",
				"apparmor":            "1",
				"security":            "apparmor",
				"audit":               "1",
				"audit_backlog_limit": "8192",
				"ro":                  "",
			},
			filteredKeys: []string{},
		},
		{
			name:           "root parameter is filtered",
			cmdlineContent: "ro root=/dev/sda1 quiet root=UUID=1234-5678",
			expectedKeys: map[string]string{
				"ro":    "",
				"quiet": "",
			},
			filteredKeys: []string{"root"},
		},
		{
			name:           "gb200 style parameters",
			cmdlineContent: "BOOT_IMAGE=/boot/vmlinuz-6.8.0-1028-aws apparmor=1 audit=1 audit_backlog_limit=8192 hugepages=5128 hugepagesz=2M init_on_alloc=0 nokaslr numa_balancing=disable panic=-1 ro security=apparmor",
			expectedKeys: map[string]string{
				"BOOT_IMAGE":          "/boot/vmlinuz-6.8.0-1028-aws",
				"apparmor":            "1",
				"audit":               "1",
				"audit_backlog_limit": "8192",
				"hugepages":           "5128",
				"hugepagesz":          "2M",
				"init_on_alloc":       "0",
				"nokaslr":             "",
				"numa_balancing":      "disable",
				"panic":               "-1",
				"ro":                  "",
				"security":            "apparmor",
			},
			filteredKeys: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temporary file with test cmdline content
			tmpfile, err := os.CreateTemp("", "cmdline-*.txt")
			if err != nil {
				t.Fatalf("Failed to create temp file: %v", err)
			}
			defer os.Remove(tmpfile.Name())

			if _, writeErr := tmpfile.WriteString(tt.cmdlineContent); writeErr != nil {
				t.Fatalf("Failed to write temp file: %v", writeErr)
			}
			tmpfile.Close()

			// Temporarily override the file path variable
			originalPath := filePathGrub
			defer func() { filePathGrub = originalPath }()
			filePathGrub = tmpfile.Name()

			// Run the collector
			ctx := context.TODO()
			collector := &Collector{}
			subtype, err := collector.collectGRUB(ctx)
			if err != nil {
				t.Fatalf("collectGRUB() failed: %v", err)
			}

			if subtype == nil {
				t.Fatal("Expected non-nil subtype")
				return
			}

			if subtype.Name != "grub" {
				t.Errorf("Expected subtype name 'grub', got %q", subtype.Name)
			}

			props := subtype.Data

			// Verify expected keys are present with correct values
			for key, expectedValue := range tt.expectedKeys {
				reading, exists := props[key]
				if !exists {
					t.Errorf("Expected key %q not found in results", key)
					continue
				}

				actualValue := reading.String()
				if actualValue != expectedValue {
					t.Errorf("Key %q: expected value %q, got %q", key, expectedValue, actualValue)
				}

				// Log key-only vs key-value for visibility
				if expectedValue == "" {
					t.Logf("✓ Key-only parameter: %s", key)
				} else {
					t.Logf("✓ Key-value parameter: %s=%s", key, expectedValue)
				}
			}

			// Verify filtered keys are NOT present
			for _, filteredKey := range tt.filteredKeys {
				if _, exists := props[filteredKey]; exists {
					t.Errorf("Filtered key %q should not be present in results", filteredKey)
				} else {
					t.Logf("✓ Filtered out: %s", filteredKey)
				}
			}

			// Verify no unexpected keys
			for key := range props {
				if _, expected := tt.expectedKeys[key]; !expected {
					t.Errorf("Unexpected key %q found in results", key)
				}
			}

			t.Logf("Total parameters parsed: %d", len(props))
		})
	}
}
