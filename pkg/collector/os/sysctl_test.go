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

package os

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/measurement"
)

const sysctlSubtypeName = "sysctl"

func TestSysctlCollector_Collect_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.TODO())

	collector := &Collector{}

	// Start collection and cancel mid-way
	go func() {
		// Give it a moment to start walking before canceling
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	m, err := collector.Collect(ctx)

	// Context cancellation during walk should return context error
	if err != nil {
		if m != nil {
			t.Error("Expected nil measurement on error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Logf("Got error: %v (expected context.Canceled or nil)", err)
		}
	}
}

func TestSysctlCollector_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.TODO()
	collector := &Collector{}

	m, err := collector.Collect(ctx)
	if err != nil {
		// /proc/sys might not exist on all systems
		if errors.Is(err, os.ErrNotExist) {
			t.Skip("/proc/sys not available on this system")
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

	// Find the sysctl subtype
	var sysctlSubtype *measurement.Subtype
	for i := range m.Subtypes {
		if m.Subtypes[i].Name == sysctlSubtypeName {
			sysctlSubtype = &m.Subtypes[i]
			break
		}
	}

	if sysctlSubtype == nil {
		t.Fatal("Expected to find sysctl subtype")
		return
	}

	// Validate that Data is a map
	params := sysctlSubtype.Data
	if params == nil {
		t.Error("Expected non-nil Data map")
		return
	}

	// Most systems have many sysctl parameters
	if len(params) == 0 {
		t.Error("Expected at least one sysctl parameter")
	}

	t.Logf("Found %d sysctl parameters", len(params))

	// Verify no /proc/sys/net entries (should be excluded)
	for key := range params {
		if strings.HasPrefix(key, "/proc/sys/net") {
			t.Errorf("Found /proc/sys/net entry which should be excluded: %s", key)
		}

		if !strings.HasPrefix(key, "/proc/sys") {
			t.Errorf("Key doesn't start with /proc/sys: %s", key)
		}
	}
}

func TestSysctlCollector_ExcludesNet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.TODO()
	collector := &Collector{}

	m, err := collector.Collect(ctx)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Skip("/proc/sys not available on this system")
			return
		}
		t.Fatalf("Collect() failed: %v", err)
	}

	if m == nil || len(m.Subtypes) == 0 {
		return
	}

	// Find sysctl subtype
	var sysctlSubtype *measurement.Subtype
	for i := range m.Subtypes {
		if m.Subtypes[i].Name == sysctlSubtypeName {
			sysctlSubtype = &m.Subtypes[i]
			break
		}
	}

	if sysctlSubtype == nil {
		return
	}

	params := sysctlSubtype.Data

	// Ensure no network parameters are included
	for key := range params {
		if strings.Contains(key, "/net/") {
			t.Errorf("Network sysctl should be excluded: %s", key)
		}
	}
}

func TestSysctlCollector_MultiLineKeyValueParsing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.TODO()
	collector := &Collector{}

	m, err := collector.Collect(ctx)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Skip("/proc/sys not available on this system")
			return
		}
		t.Fatalf("Collect() failed: %v", err)
	}

	if m == nil || len(m.Subtypes) == 0 {
		t.Fatal("Expected non-nil measurement with subtypes")
	}

	// Find sysctl subtype
	var sysctlSubtype *measurement.Subtype
	for i := range m.Subtypes {
		if m.Subtypes[i].Name == sysctlSubtypeName {
			sysctlSubtype = &m.Subtypes[i]
			break
		}
	}

	if sysctlSubtype == nil {
		t.Fatal("Expected to find sysctl subtype")
		return
	}

	params := sysctlSubtype.Data

	// Check if /proc/sys/sunrpc/transports exists and has been parsed
	// This file typically contains lines like: "tcp 1048576\nudp 32768\nrdma 1048576"
	var foundTransportKeys bool
	for key := range params {
		if strings.HasPrefix(key, "/proc/sys/sunrpc/transports/") {
			foundTransportKeys = true
			// Verify the key format: /proc/sys/sunrpc/transports/<protocol>
			parts := strings.Split(key, "/")
			if len(parts) < 6 {
				t.Errorf("Expected extended path format, got: %s", key)
			}
			// Check that the value is a string (not the multi-line content)
			val := params[key]
			if valStr, ok := val.Any().(string); ok {
				if strings.Contains(valStr, "\n") {
					t.Errorf("Multi-line value should be split, but found newline in: %s = %s", key, valStr)
				}
			}
			t.Logf("Found parsed transport key: %s = %v", key, params[key])
		}
	}

	// If the file exists, we should find parsed keys
	if _, err := os.Stat("/proc/sys/sunrpc/transports"); err == nil {
		if !foundTransportKeys {
			// Check if the original file is still there (shouldn't be if it was parsed)
			if _, exists := params["/proc/sys/sunrpc/transports"]; exists {
				content := params["/proc/sys/sunrpc/transports"]
				if valStr, ok := content.Any().(string); ok {
					if strings.Contains(valStr, "\n") {
						t.Error("Multi-line /proc/sys/sunrpc/transports should have been parsed into separate keys")
					}
				}
			} else {
				t.Error("Expected to find parsed /proc/sys/sunrpc/transports/* keys")
			}
		}
	}
}

func TestSysctlCollector_SingleLineValues(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.TODO()
	collector := &Collector{}

	m, err := collector.Collect(ctx)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Skip("/proc/sys not available on this system")
			return
		}
		t.Fatalf("Collect() failed: %v", err)
	}

	if m == nil || len(m.Subtypes) == 0 {
		t.Fatal("Expected non-nil measurement with subtypes")
	}

	// Find sysctl subtype
	var sysctlSubtype *measurement.Subtype
	for i := range m.Subtypes {
		if m.Subtypes[i].Name == sysctlSubtypeName {
			sysctlSubtype = &m.Subtypes[i]
			break
		}
	}

	if sysctlSubtype == nil {
		t.Fatal("Expected to find sysctl subtype")
		return
	}

	params := sysctlSubtype.Data

	// Single-line files should be stored with their original path (not split)
	// Check for common single-value sysctl parameters
	singleValuePaths := []string{
		"/proc/sys/kernel/hostname",
		"/proc/sys/kernel/ostype",
		"/proc/sys/kernel/osrelease",
	}

	for _, path := range singleValuePaths {
		if _, err := os.Stat(path); err == nil {
			// File exists, check if it's in params
			if val, exists := params[path]; exists {
				if valStr, ok := val.Any().(string); ok {
					// Single-line values shouldn't have been extended
					if strings.Contains(path, "//") {
						t.Errorf("Single-line value has double slash: %s", path)
					}
					t.Logf("Single-line value preserved: %s = %s", path, valStr)
				}
			}
		}
	}
}

func TestSysctlCollector_MixedContent(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.TODO()
	collector := &Collector{}

	m, err := collector.Collect(ctx)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Skip("/proc/sys not available on this system")
			return
		}
		t.Fatalf("Collect() failed: %v", err)
	}

	if m == nil || len(m.Subtypes) == 0 {
		return
	}

	// Find sysctl subtype
	var sysctlSubtype *measurement.Subtype
	for i := range m.Subtypes {
		if m.Subtypes[i].Name == "sysctl" {
			sysctlSubtype = &m.Subtypes[i]
			break
		}
	}

	if sysctlSubtype == nil {
		return
	}

	params := sysctlSubtype.Data

	// Verify that no values contain unprocessed multi-line content with key-value pattern
	for key, val := range params {
		if valStr, ok := val.Any().(string); ok {
			// If it has newlines, check if it looks like unparsed key-value pairs
			if strings.Contains(valStr, "\n") {
				lines := strings.Split(valStr, "\n")
				allKeyValue := true
				for _, line := range lines {
					line = strings.TrimSpace(line)
					if line == "" {
						continue
					}
					parts := strings.Fields(line)
					if len(parts) < 2 {
						allKeyValue = false
						break
					}
				}
				if allKeyValue && len(lines) > 1 {
					t.Errorf("Found unparsed multi-line key-value content at %s: %q", key, valStr)
				}
			}
		}
	}
}

func TestSysctlCollector_ParsesSysctlParameters(t *testing.T) {
	tests := []struct {
		name             string
		fileStructure    map[string]string // path -> content
		expectedParams   map[string]string // expected key-value pairs
		excludeKeys      []string          // keys that should NOT appear
		netFilesIncluded bool              // whether to create files under net/
	}{
		{
			name: "single-line parameters",
			fileStructure: map[string]string{
				"kernel/hostname":  "testhost",
				"kernel/ostype":    "Linux",
				"kernel/osrelease": "6.8.0-1028-aws",
				"vm/swappiness":    "60",
			},
			expectedParams: map[string]string{
				"kernel/hostname":  "testhost",
				"kernel/ostype":    "Linux",
				"kernel/osrelease": "6.8.0-1028-aws",
				"vm/swappiness":    "60",
			},
		},
		{
			name: "multi-line key-value format",
			fileStructure: map[string]string{
				"sunrpc/transports": "tcp 1048576\nudp 32768\nrdma 1048576",
			},
			expectedParams: map[string]string{
				"sunrpc/transports/tcp":  "1048576",
				"sunrpc/transports/udp":  "32768",
				"sunrpc/transports/rdma": "1048576",
			},
			excludeKeys: []string{"sunrpc/transports"}, // original file should not appear
		},
		{
			name: "mixed single and multi-line",
			fileStructure: map[string]string{
				"kernel/hostname":     "server1",
				"sunrpc/transports":   "tcp 262144\nudp 65536",
				"kernel/printk":       "4 4 1 7",
				"kernel/sched_domain": "cpu0 domain0\ncpu1 domain1",
			},
			expectedParams: map[string]string{
				"kernel/hostname":          "server1",
				"sunrpc/transports/tcp":    "262144",
				"sunrpc/transports/udp":    "65536",
				"kernel/printk":            "4 4 1 7",
				"kernel/sched_domain/cpu0": "domain0",
				"kernel/sched_domain/cpu1": "domain1",
			},
		},
		{
			name: "network parameters excluded",
			fileStructure: map[string]string{
				"kernel/hostname":     "testhost",
				"net/ipv4/ip_forward": "1",
				"net/core/somaxconn":  "4096",
				"vm/swappiness":       "10",
			},
			expectedParams: map[string]string{
				"kernel/hostname": "testhost",
				"vm/swappiness":   "10",
			},
			excludeKeys: []string{
				"net/ipv4/ip_forward",
				"net/core/somaxconn",
			},
			netFilesIncluded: true,
		},
		{
			name: "multi-line non-key-value format preserved",
			fileStructure: map[string]string{
				"kernel/sched_features": "GENTLE_FAIR_SLEEPERS\nSTART_DEBIT\nNEXT_BUDDY",
			},
			expectedParams: map[string]string{
				"kernel/sched_features": "GENTLE_FAIR_SLEEPERS\nSTART_DEBIT\nNEXT_BUDDY",
			},
		},
		{
			name: "empty files handled",
			fileStructure: map[string]string{
				"kernel/hostname": "testhost",
				"kernel/empty":    "",
				"vm/swappiness":   "60",
			},
			expectedParams: map[string]string{
				"kernel/hostname": "testhost",
				"vm/swappiness":   "60",
			},
		},
		{
			name: "values with spaces",
			fileStructure: map[string]string{
				"kernel/version":   "6.8.0-1028-aws SMP PREEMPT_DYNAMIC",
				"crypto/fips_name": "Linux Kernel Cryptographic API",
			},
			expectedParams: map[string]string{
				"kernel/version":   "6.8.0-1028-aws SMP PREEMPT_DYNAMIC",
				"crypto/fips_name": "Linux Kernel Cryptographic API",
			},
		},
		{
			name: "nested directory structure",
			fileStructure: map[string]string{
				"kernel/hostname":             "host1",
				"fs/inotify/max_user_watches": "524288",
				"vm/zone_reclaim_mode":        "0",
			},
			expectedParams: map[string]string{
				"kernel/hostname":             "host1",
				"fs/inotify/max_user_watches": "524288",
				"vm/zone_reclaim_mode":        "0",
			},
		},
		{
			name: "special characters in values",
			fileStructure: map[string]string{
				"kernel/core_pattern": "|/usr/share/apport/apport -p%p -s%s -c%c -d%d -P%P -u%u -g%g -- %E",
				"kernel/modprobe":     "/sbin/modprobe",
			},
			expectedParams: map[string]string{
				"kernel/core_pattern": "|/usr/share/apport/apport -p%p -s%s -c%c -d%d -P%P -u%u -g%g -- %E",
				"kernel/modprobe":     "/sbin/modprobe",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temporary directory structure
			tmpRoot := t.TempDir()

			for relPath, content := range tt.fileStructure {
				fullPath := filepath.Join(tmpRoot, relPath)

				// Create parent directories
				if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
					t.Fatalf("Failed to create directory: %v", err)
				}

				// Write file
				if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
					t.Fatalf("Failed to write file %s: %v", fullPath, err)
				}
			}

			// Temporarily override sysctlRoot and sysctlNetPrefix
			originalRoot := sysctlRoot
			originalNetPrefix := sysctlNetPrefix
			defer func() {
				sysctlRoot = originalRoot
				sysctlNetPrefix = originalNetPrefix
			}()
			sysctlRoot = tmpRoot
			sysctlNetPrefix = filepath.Join(tmpRoot, "net")

			// Run the collector
			ctx := context.TODO()
			collector := &Collector{}
			subtype, err := collector.collectSysctl(ctx)
			if err != nil {
				t.Fatalf("collectSysctl() failed: %v", err)
			}

			if subtype == nil {
				t.Fatal("Expected non-nil subtype")
				return
			}

			if subtype.Name != "sysctl" {
				t.Errorf("Expected subtype name 'sysctl', got %q", subtype.Name)
			}

			data := subtype.Data

			// Verify expected parameters
			for relPath, expectedValue := range tt.expectedParams {
				fullKey := filepath.Join(tmpRoot, relPath)
				reading, exists := data[fullKey]
				if !exists {
					t.Errorf("Expected parameter %q not found in results", relPath)
					continue
				}

				actualValue := reading.String()
				if actualValue != expectedValue {
					t.Errorf("Parameter %q: expected value %q, got %q", relPath, expectedValue, actualValue)
				}

				t.Logf("✓ Parameter: %s = %s", relPath, actualValue)
			}

			// Verify excluded keys are NOT present
			for _, excludeKey := range tt.excludeKeys {
				fullKey := filepath.Join(tmpRoot, excludeKey)
				if _, exists := data[fullKey]; exists {
					t.Errorf("Excluded parameter %q should not be in results", excludeKey)
				} else {
					t.Logf("✓ Excluded: %s", excludeKey)
				}
			}

			t.Logf("Total parameters parsed: %d", len(data))
		})
	}
}

func TestSysctlCollector_HandlesEdgeCases(t *testing.T) {
	tests := []struct {
		name          string
		fileStructure map[string]string
		setupFunc     func(tmpRoot string) error // custom setup if needed
		wantErr       bool
		validate      func(t *testing.T, data map[string]measurement.Reading)
	}{
		{
			name: "whitespace only content",
			fileStructure: map[string]string{
				"kernel/hostname": "testhost",
				"kernel/spaces":   "   \t\n   ",
				"vm/swappiness":   "60",
			},
			validate: func(t *testing.T, data map[string]measurement.Reading) {
				// Should have hostname and swappiness, spaces file should be skipped (empty after trim)
				if len(data) < 2 {
					t.Errorf("Expected at least 2 parameters, got %d", len(data))
				}
			},
		},
		{
			name: "numeric values",
			fileStructure: map[string]string{
				"vm/swappiness":  "60",
				"vm/dirty_ratio": "20",
				"kernel/pid_max": "4194304",
				"fs/file-max":    "9223372036854775807",
			},
			validate: func(t *testing.T, data map[string]measurement.Reading) {
				if len(data) != 4 {
					t.Errorf("Expected 4 parameters, got %d", len(data))
				}
			},
		},
		{
			name: "multi-line with single field (not key-value)",
			fileStructure: map[string]string{
				"kernel/sched_features": "GENTLE_FAIR_SLEEPERS\nSTART_DEBIT\nNO_NEXT_BUDDY",
			},
			validate: func(t *testing.T, data map[string]measurement.Reading) {
				// Should be stored as-is (not split) because lines don't have 2+ fields
				found := false
				for key, val := range data {
					if strings.HasSuffix(key, "kernel/sched_features") {
						found = true
						valStr := val.String()
						if !strings.Contains(valStr, "\n") {
							t.Errorf("Multi-line non-KV should preserve newlines, got: %q", valStr)
						}
					}
				}
				if !found {
					t.Error("Expected to find kernel/sched_features")
				}
			},
		},
		{
			name: "values with multiple spaces",
			fileStructure: map[string]string{
				"kernel/printk":         "4    4    1    7",
				"kernel/random/boot_id": "550e8400-e29b-41d4-a716-446655440000",
			},
			validate: func(t *testing.T, data map[string]measurement.Reading) {
				for key, val := range data {
					if strings.HasSuffix(key, "kernel/printk") {
						valStr := val.String()
						// Should preserve the multi-space value
						if valStr != "4    4    1    7" {
							t.Errorf("Expected spaces preserved, got: %q", valStr)
						}
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temporary directory structure
			tmpRoot := t.TempDir()

			for relPath, content := range tt.fileStructure {
				fullPath := filepath.Join(tmpRoot, relPath)

				if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
					t.Fatalf("Failed to create directory: %v", err)
				}

				if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
					t.Fatalf("Failed to write file %s: %v", fullPath, err)
				}
			}

			// Custom setup if provided
			if tt.setupFunc != nil {
				if err := tt.setupFunc(tmpRoot); err != nil {
					t.Fatalf("Setup function failed: %v", err)
				}
			}

			// Temporarily override sysctlRoot
			originalRoot := sysctlRoot
			originalNetPrefix := sysctlNetPrefix
			defer func() {
				sysctlRoot = originalRoot
				sysctlNetPrefix = originalNetPrefix
			}()
			sysctlRoot = tmpRoot
			sysctlNetPrefix = filepath.Join(tmpRoot, "net")

			// Run the collector
			ctx := context.TODO()
			collector := &Collector{}
			subtype, err := collector.collectSysctl(ctx)

			if tt.wantErr {
				if err == nil {
					t.Error("Expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("collectSysctl() unexpected error: %v", err)
			}

			if tt.validate != nil {
				tt.validate(t, subtype.Data)
			}
		})
	}
}

func TestSysctlCollector_FilterPatterns(t *testing.T) {
	// Test that filter patterns work correctly
	tmpRoot := t.TempDir()

	// Create files including one that matches the filter pattern
	files := map[string]string{
		"kernel/hostname": "testhost",
		"dev/cdrom/info":  "CD-ROM information",
		"dev/cdrom/debug": "0",
		"vm/swappiness":   "60",
	}

	for relPath, content := range files {
		fullPath := filepath.Join(tmpRoot, relPath)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatalf("Failed to create directory: %v", err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			t.Fatalf("Failed to write file: %v", err)
		}
	}

	// Override sysctlRoot and filter pattern to match temp directory
	originalRoot := sysctlRoot
	originalNetPrefix := sysctlNetPrefix
	originalFilter := filterOutSysctlKeys
	defer func() {
		sysctlRoot = originalRoot
		sysctlNetPrefix = originalNetPrefix
		filterOutSysctlKeys = originalFilter
	}()
	sysctlRoot = tmpRoot
	sysctlNetPrefix = filepath.Join(tmpRoot, "net")
	// Update filter pattern to match temp directory structure
	filterOutSysctlKeys = []string{
		filepath.Join(tmpRoot, "dev/cdrom/*"),
	}

	// Run collector
	ctx := context.TODO()
	collector := &Collector{}
	subtype, err := collector.collectSysctl(ctx)
	if err != nil {
		t.Fatalf("collectSysctl() failed: %v", err)
	}

	data := subtype.Data

	// Verify /dev/cdrom/* entries are filtered out
	for key := range data {
		if strings.Contains(key, "/dev/cdrom/") {
			t.Errorf("Found filtered key that should be excluded: %s", key)
		}
	}

	// Verify other entries are present
	foundHostname := false
	foundSwappiness := false
	for key := range data {
		if strings.HasSuffix(key, "kernel/hostname") {
			foundHostname = true
		}
		if strings.HasSuffix(key, "vm/swappiness") {
			foundSwappiness = true
		}
	}

	if !foundHostname {
		t.Error("Expected to find kernel/hostname")
	}
	if !foundSwappiness {
		t.Error("Expected to find vm/swappiness")
	}

	t.Logf("✓ Filter patterns working correctly, found %d params", len(data))
}
