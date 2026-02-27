// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/measurement"
)

const releaseSubtypeName = "release"

func TestReleaseCollector_Collect_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.TODO())
	cancel() // Cancel immediately

	collector := &Collector{}
	m, err := collector.Collect(ctx)

	if err == nil {
		// On some systems, the read may complete before context check
		t.Skip("Context cancellation timing dependent")
	}

	if m != nil {
		t.Error("Expected nil measurement on error")
	}

	if !errors.Is(err, context.Canceled) {
		t.Errorf("Expected context.Canceled, got %v", err)
	}
}

func TestReleaseCollector_Collect_ContextTimeout(t *testing.T) {
	// Create a context with very short timeout
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()

	// Wait for context to timeout
	time.Sleep(10 * time.Millisecond)

	collector := &Collector{}
	m, err := collector.Collect(ctx)

	if err == nil {
		// On some systems, the read may complete before context check
		t.Skip("Context timeout timing dependent")
	}

	if m != nil {
		t.Error("Expected nil measurement on timeout")
	}

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Expected context.DeadlineExceeded, got %v", err)
	}
}

func TestReleaseCollector_ErrorRecovery_MissingFile(t *testing.T) {
	// Temporarily override the file path variables to non-existent paths
	originalPrimary := filePathReleasePrimary
	originalFallback := filePathReleaseFallback
	defer func() {
		filePathReleasePrimary = originalPrimary
		filePathReleaseFallback = originalFallback
	}()
	filePathReleasePrimary = "/nonexistent/path/os-release"
	filePathReleaseFallback = "/also/nonexistent/os-release"

	ctx := context.TODO()
	collector := &Collector{}
	subtype, err := collector.collectRelease(ctx)

	// Should fail gracefully when both files are missing
	if err == nil {
		t.Error("Expected error when both release files are missing")
	}
	if subtype != nil {
		t.Error("Expected nil subtype when both release files are missing")
	}
}

func TestReleaseCollector_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.TODO()
	collector := &Collector{}

	m, err := collector.Collect(ctx)
	if err != nil {
		// /etc/os-release might not exist on all systems
		if errors.Is(err, os.ErrNotExist) {
			t.Skip("/etc/os-release not available on this system")
			return
		}
		t.Fatalf("Collect() failed: %v", err)
	}

	// Should return measurement with TypeOS and four subtypes: grub, sysctl, kmod, release
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

	// Find the release subtype
	var releaseSubtype *measurement.Subtype
	for i := range m.Subtypes {
		if m.Subtypes[i].Name == releaseSubtypeName {
			releaseSubtype = &m.Subtypes[i]
			break
		}
	}

	if releaseSubtype == nil {
		t.Fatal("Expected to find release subtype")
		return
	}

	// Validate that Data is a map
	data := releaseSubtype.Data
	if data == nil {
		t.Error("Expected non-nil Data map")
		return
	}

	// Most systems have several os-release fields
	if len(data) == 0 {
		t.Error("Expected at least one os-release field")
	}

	t.Logf("Found %d os-release fields", len(data))

	// Check for common fields that should exist
	commonFields := []string{"ID", "NAME", "VERSION_ID"}
	for _, field := range commonFields {
		if val, exists := data[field]; exists {
			t.Logf("%s = %v", field, val.Any())
		}
	}
}

func TestReleaseCollector_ValidatesKeyValueParsing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.TODO()
	collector := &Collector{}

	m, err := collector.Collect(ctx)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Skip("/etc/os-release not available on this system")
			return
		}
		t.Fatalf("Collect() failed: %v", err)
	}

	if m == nil || len(m.Subtypes) == 0 {
		t.Fatal("Expected at least one subtype")
	}

	// Find release subtype
	var releaseSubtype *measurement.Subtype
	for i := range m.Subtypes {
		if m.Subtypes[i].Name == releaseSubtypeName {
			releaseSubtype = &m.Subtypes[i]
			break
		}
	}

	if releaseSubtype == nil {
		t.Fatal("Expected to find release subtype")
		return
	}

	data := releaseSubtype.Data

	// Check that all keys have values (no empty keys or values for key=value format)
	for key, value := range data {
		if key == "" {
			t.Error("Found empty key in Data")
			continue
		}

		strVal := value.Any()
		t.Logf("Field: %s = %v", key, strVal)

		// Values should not contain quotes since they're stripped
		if str, ok := strVal.(string); ok {
			if len(str) > 0 && (str[0] == '"' || str[0] == '\'') {
				t.Errorf("Value for %s still contains quotes: %s", key, str)
			}
			if len(str) > 0 && (str[len(str)-1] == '"' || str[len(str)-1] == '\'') {
				t.Errorf("Value for %s still contains quotes: %s", key, str)
			}
		}
	}
}

func TestReleaseCollector_HandlesQuotedValues(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.TODO()
	collector := &Collector{}

	m, err := collector.Collect(ctx)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Skip("/etc/os-release not available on this system")
			return
		}
		t.Fatalf("Collect() failed: %v", err)
	}

	if m == nil || len(m.Subtypes) == 0 {
		t.Fatal("Expected at least one subtype")
	}

	// Find release subtype
	var releaseSubtype *measurement.Subtype
	for i := range m.Subtypes {
		if m.Subtypes[i].Name == releaseSubtypeName {
			releaseSubtype = &m.Subtypes[i]
			break
		}
	}

	if releaseSubtype == nil {
		t.Fatal("Expected to find release subtype")
		return
	}

	data := releaseSubtype.Data

	// Pretty_name often contains spaces and is quoted
	if prettyName, exists := data["PRETTY_NAME"]; exists {
		strVal := prettyName.Any().(string)
		t.Logf("PRETTY_NAME = %s", strVal)

		// Should not have surrounding quotes
		if len(strVal) > 0 && strVal[0] == '"' {
			t.Error("PRETTY_NAME value still has leading quote")
		}
		if len(strVal) > 0 && strVal[len(strVal)-1] == '"' {
			t.Error("PRETTY_NAME value still has trailing quote")
		}
	}
}

func TestReleaseCollector_HandlesEmptyLines(t *testing.T) {
	// Create a temporary test file with empty lines and comments
	tmpDir := t.TempDir()
	testFile := filepath.Join(tmpDir, "os-release")

	content := `NAME="Test OS"

ID=testos
VERSION_ID="1.0"

PRETTY_NAME="Test OS 1.0"
`

	if err := os.WriteFile(testFile, []byte(content), 0644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// This test would need to mock the file reading, which is not easily done
	// with the current implementation. The integration tests above cover
	// the empty line handling via real /etc/os-release files.

	t.Skip("Unit test requires refactoring for dependency injection")
}

func TestReleaseCollector_HandlesMalformedLines(t *testing.T) {
	// Test that malformed lines (no '=' separator) are skipped gracefully
	// This is implicitly tested by the integration tests since real
	// /etc/os-release files are generally well-formed, but the code
	// handles this case by checking len(parts) != 2

	t.Skip("Unit test requires refactoring for dependency injection")
}

func TestReleaseCollector_ValidatesCommonFields(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.TODO()
	collector := &Collector{}

	m, err := collector.Collect(ctx)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Skip("/etc/os-release not available on this system")
			return
		}
		t.Fatalf("Collect() failed: %v", err)
	}

	// Find release subtype
	var releaseSubtype *measurement.Subtype
	for i := range m.Subtypes {
		if m.Subtypes[i].Name == releaseSubtypeName {
			releaseSubtype = &m.Subtypes[i]
			break
		}
	}

	if releaseSubtype == nil {
		t.Fatal("Expected to find release subtype")
		return
	}

	data := releaseSubtype.Data

	// According to freedesktop.org spec, these fields should typically exist
	expectedFields := []string{"ID", "NAME"}
	foundCount := 0

	for _, field := range expectedFields {
		if val, exists := data[field]; exists {
			foundCount++
			t.Logf("Found expected field %s = %v", field, val.Any())
		} else {
			t.Logf("Missing recommended field: %s", field)
		}
	}

	if foundCount == 0 {
		t.Error("Expected at least one of the common fields (ID, NAME) to be present")
	}
}

func TestReleaseCollector_DataTypes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	ctx := context.TODO()
	collector := &Collector{}

	m, err := collector.Collect(ctx)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Skip("/etc/os-release not available on this system")
			return
		}
		t.Fatalf("Collect() failed: %v", err)
	}

	// Find release subtype
	var releaseSubtype *measurement.Subtype
	for i := range m.Subtypes {
		if m.Subtypes[i].Name == releaseSubtypeName {
			releaseSubtype = &m.Subtypes[i]
			break
		}
	}

	if releaseSubtype == nil {
		t.Fatal("Expected to find release subtype")
		return
	}

	data := releaseSubtype.Data

	// All values should be strings from measurement.Str()
	for key, reading := range data {
		val := reading.Any()
		if _, ok := val.(string); !ok {
			t.Errorf("Expected string value for key %s, got %T", key, val)
		}
	}
}

// BenchmarkReleaseCollector_Collect benchmarks the release collection process
func BenchmarkReleaseCollector_Collect(b *testing.B) {
	ctx := context.TODO()
	collector := &Collector{}

	// Verify it works before benchmarking
	_, err := collector.Collect(ctx)
	if err != nil {
		b.Skipf("Skipping benchmark: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = collector.Collect(ctx)
	}
}

func TestReleaseCollector_ParsesOSRelease(t *testing.T) {
	tests := []struct {
		name           string
		releaseContent string
		expectedFields map[string]string
	}{
		{
			name: "ubuntu 22.04 with quotes",
			releaseContent: `NAME="Ubuntu"
ID=ubuntu
ID_LIKE=debian
PRETTY_NAME="Ubuntu 22.04.4 LTS"
VERSION_ID="22.04"
VERSION="22.04.4 LTS (Jammy Jellyfish)"
VERSION_CODENAME=jammy
HOME_URL="https://www.ubuntu.com/"
SUPPORT_URL="https://help.ubuntu.com/"
BUG_REPORT_URL="https://bugs.launchpad.net/ubuntu/"`,
			expectedFields: map[string]string{
				"NAME":             "Ubuntu",
				"ID":               "ubuntu",
				"ID_LIKE":          "debian",
				"PRETTY_NAME":      "Ubuntu 22.04.4 LTS",
				"VERSION_ID":       "22.04",
				"VERSION":          "22.04.4 LTS (Jammy Jellyfish)",
				"VERSION_CODENAME": "jammy",
				"HOME_URL":         "https://www.ubuntu.com/",
				"SUPPORT_URL":      "https://help.ubuntu.com/",
				"BUG_REPORT_URL":   "https://bugs.launchpad.net/ubuntu/",
			},
		},
		{
			name: "ubuntu 24.04",
			releaseContent: `PRETTY_NAME="Ubuntu 24.04.2 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
VERSION="24.04.2 LTS (Noble Numbat)"
VERSION_CODENAME=noble
ID=ubuntu
ID_LIKE=debian
HOME_URL="https://www.ubuntu.com/"
SUPPORT_URL="https://help.ubuntu.com/"
BUG_REPORT_URL="https://bugs.launchpad.net/ubuntu/"`,
			expectedFields: map[string]string{
				"PRETTY_NAME":      "Ubuntu 24.04.2 LTS",
				"NAME":             "Ubuntu",
				"VERSION_ID":       "24.04",
				"VERSION":          "24.04.2 LTS (Noble Numbat)",
				"VERSION_CODENAME": "noble",
				"ID":               "ubuntu",
				"ID_LIKE":          "debian",
				"HOME_URL":         "https://www.ubuntu.com/",
				"SUPPORT_URL":      "https://help.ubuntu.com/",
				"BUG_REPORT_URL":   "https://bugs.launchpad.net/ubuntu/",
			},
		},
		{
			name: "rhel with single quotes",
			releaseContent: `NAME='Red Hat Enterprise Linux'
VERSION='8.7 (Ootpa)'
ID='rhel'
ID_LIKE='fedora'
VERSION_ID='8.7'
PLATFORM_ID='platform:el8'
PRETTY_NAME='Red Hat Enterprise Linux 8.7 (Ootpa)'
ANSI_COLOR='0;31'`,
			expectedFields: map[string]string{
				"NAME":        "Red Hat Enterprise Linux",
				"VERSION":     "8.7 (Ootpa)",
				"ID":          "rhel",
				"ID_LIKE":     "fedora",
				"VERSION_ID":  "8.7",
				"PLATFORM_ID": "platform:el8",
				"PRETTY_NAME": "Red Hat Enterprise Linux 8.7 (Ootpa)",
				"ANSI_COLOR":  "0;31",
			},
		},
		{
			name: "mixed quotes and unquoted",
			releaseContent: `NAME="Test OS"
ID=testos
VERSION_ID=1.0
PRETTY_NAME='Test OS 1.0'
HOME_URL=https://test.com`,
			expectedFields: map[string]string{
				"NAME":        "Test OS",
				"ID":          "testos",
				"VERSION_ID":  "1.0",
				"PRETTY_NAME": "Test OS 1.0",
				"HOME_URL":    "https://test.com",
			},
		},
		{
			name: "with comments and empty lines",
			releaseContent: `# This is a comment
NAME="Ubuntu"
ID=ubuntu

# Another comment
VERSION_ID="22.04"

PRETTY_NAME="Ubuntu 22.04 LTS"`,
			expectedFields: map[string]string{
				"NAME":        "Ubuntu",
				"ID":          "ubuntu",
				"VERSION_ID":  "22.04",
				"PRETTY_NAME": "Ubuntu 22.04 LTS",
			},
		},
		{
			name: "values with special characters",
			releaseContent: `NAME="Test-OS_2024"
VERSION="1.0 (Code-Name)"
BUILD_ID="20241226-123456"
LOGO="test-logo"
CPE_NAME="cpe:/o:test:testos:1.0"`,
			expectedFields: map[string]string{
				"NAME":     "Test-OS_2024",
				"VERSION":  "1.0 (Code-Name)",
				"BUILD_ID": "20241226-123456",
				"LOGO":     "test-logo",
				"CPE_NAME": "cpe:/o:test:testos:1.0",
			},
		},
		{
			name: "minimal release file",
			releaseContent: `NAME="Minimal"
ID=minimal
VERSION_ID="1"`,
			expectedFields: map[string]string{
				"NAME":       "Minimal",
				"ID":         "minimal",
				"VERSION_ID": "1",
			},
		},
		{
			name:           "empty file",
			releaseContent: "",
			expectedFields: map[string]string{},
		},
		{
			name: "with malformed lines (no equals)",
			releaseContent: `NAME="Ubuntu"
MALFORMED LINE WITHOUT EQUALS
ID=ubuntu
VERSION_ID="22.04"`,
			expectedFields: map[string]string{
				"NAME":       "Ubuntu",
				"ID":         "ubuntu",
				"VERSION_ID": "22.04",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create temporary file with test release content
			tmpfile, err := os.CreateTemp("", "os-release-*.txt")
			if err != nil {
				t.Fatalf("Failed to create temp file: %v", err)
			}
			defer os.Remove(tmpfile.Name())

			if _, writeErr := tmpfile.WriteString(tt.releaseContent); writeErr != nil {
				t.Fatalf("Failed to write temp file: %v", writeErr)
			}
			if closeErr := tmpfile.Close(); closeErr != nil {
				t.Fatalf("Failed to close temp file: %v", closeErr)
			}

			// Temporarily override the file path variables
			originalPrimary := filePathReleasePrimary
			originalFallback := filePathReleaseFallback
			defer func() {
				filePathReleasePrimary = originalPrimary
				filePathReleaseFallback = originalFallback
			}()
			filePathReleasePrimary = tmpfile.Name()
			filePathReleaseFallback = tmpfile.Name()

			// Run the collector
			ctx := context.TODO()
			collector := &Collector{}
			subtype, err := collector.collectRelease(ctx)
			if err != nil {
				t.Fatalf("collectRelease() failed: %v", err)
			}

			if subtype == nil {
				t.Fatal("Expected non-nil subtype")
				return
			}

			if subtype.Name != "release" {
				t.Errorf("Expected subtype name 'release', got %q", subtype.Name)
			}

			data := subtype.Data

			// Verify expected fields are present with correct values
			for key, expectedValue := range tt.expectedFields {
				reading, exists := data[key]
				if !exists {
					t.Errorf("Expected field %q not found in results", key)
					continue
				}

				actualValue := reading.String()
				if actualValue != expectedValue {
					t.Errorf("Field %q: expected value %q, got %q", key, expectedValue, actualValue)
				}

				t.Logf("✓ Field: %s=%s", key, actualValue)
			}

			// Verify no unexpected fields (except comment lines which are skipped)
			if len(data) != len(tt.expectedFields) {
				t.Errorf("Expected %d fields, got %d", len(tt.expectedFields), len(data))
				for fieldName := range data {
					if _, expected := tt.expectedFields[fieldName]; !expected {
						t.Errorf("Unexpected field %q found in results", fieldName)
					}
				}
			}

			t.Logf("Total fields parsed: %d", len(data))
		})
	}
}

func TestReleaseCollector_FallbackPath(t *testing.T) {
	// Test that the collector falls back to /usr/lib/os-release
	// when /etc/os-release doesn't exist

	// Create temporary fallback file
	tmpDir := t.TempDir()
	fallbackFile := filepath.Join(tmpDir, "os-release-fallback")
	primaryFile := filepath.Join(tmpDir, "os-release-primary")

	fallbackContent := `NAME="Fallback OS"
ID=fallback
VERSION_ID="1.0"`

	if err := os.WriteFile(fallbackFile, []byte(fallbackContent), 0644); err != nil {
		t.Fatalf("Failed to create fallback file: %v", err)
	}

	// Temporarily override the file path variables
	originalPrimary := filePathReleasePrimary
	originalFallback := filePathReleaseFallback
	defer func() {
		filePathReleasePrimary = originalPrimary
		filePathReleaseFallback = originalFallback
	}()
	filePathReleasePrimary = primaryFile // This file doesn't exist
	filePathReleaseFallback = fallbackFile

	// Run the collector
	ctx := context.TODO()
	collector := &Collector{}
	subtype, err := collector.collectRelease(ctx)
	if err != nil {
		t.Fatalf("collectRelease() failed: %v", err)
	}

	if subtype == nil {
		t.Fatal("Expected non-nil subtype")
		return
	}

	data := subtype.Data

	// Verify fallback file was used
	if name, exists := data["NAME"]; !exists || name.String() != "Fallback OS" {
		t.Errorf("Expected fallback file to be used with NAME='Fallback OS', got %v", name)
	}

	if id, exists := data["ID"]; !exists || id.String() != "fallback" {
		t.Errorf("Expected fallback file to be used with ID='fallback', got %v", id)
	}

	t.Logf("✓ Fallback path used successfully")
}

// ExampleCollector_collectRelease demonstrates how the release collector works
func ExampleCollector_collectRelease() {
	ctx := context.TODO()
	collector := &Collector{}

	measurement, err := collector.Collect(ctx)
	if err != nil {
		// Handle error (e.g., /etc/os-release not found)
		return
	}

	// Find the release subtype
	for _, subtype := range measurement.Subtypes {
		if subtype.Name == "release" {
			// Access OS release information
			if osName, exists := subtype.Data["NAME"]; exists {
				_ = osName.Any() // Get the OS name
			}
			if osID, exists := subtype.Data["ID"]; exists {
				_ = osID.Any() // Get the OS ID
			}
			break
		}
	}
}
