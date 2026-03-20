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

package result

import (
	"errors"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler/types"
)

// TestResult_New tests result creation
func TestResult_New(t *testing.T) {
	bundlerType := types.BundleType("gpu-operator")

	result := New(bundlerType)

	if result == nil {
		t.Fatal("New() returned nil")
		return
	}

	if result.Type != bundlerType {
		t.Errorf("Type = %s, want %s", result.Type, bundlerType)
	}

	if result.Files == nil {
		t.Error("Files slice is nil")
	}

	if len(result.Files) != 0 {
		t.Errorf("New result should have 0 files, got %d", len(result.Files))
	}

	if result.Errors == nil {
		t.Error("Errors slice is nil")
	}

	if len(result.Errors) != 0 {
		t.Errorf("New result should have 0 errors, got %d", len(result.Errors))
	}

	if result.Success {
		t.Error("New result should not be marked as success")
	}

	if result.Size != 0 {
		t.Errorf("New result should have 0 size, got %d", result.Size)
	}

	if result.Duration != 0 {
		t.Errorf("New result should have 0 duration, got %v", result.Duration)
	}

	if result.Checksum != "" {
		t.Errorf("New result should have empty checksum, got %s", result.Checksum)
	}
}

// TestResult_AddFile tests adding files to result
func TestResult_AddFile(t *testing.T) {
	result := New(types.BundleType("gpu-operator"))

	// Add single file
	result.AddFile("/path/to/file1.txt", 100)

	if len(result.Files) != 1 {
		t.Errorf("Expected 1 file, got %d", len(result.Files))
	}

	if result.Files[0] != "/path/to/file1.txt" {
		t.Errorf("File path = %s, want /path/to/file1.txt", result.Files[0])
	}

	if result.Size != 100 {
		t.Errorf("Size = %d, want 100", result.Size)
	}

	// Add multiple files
	result.AddFile("/path/to/file2.txt", 200)
	result.AddFile("/path/to/file3.txt", 300)

	if len(result.Files) != 3 {
		t.Errorf("Expected 3 files, got %d", len(result.Files))
	}

	expectedSize := int64(600)
	if result.Size != expectedSize {
		t.Errorf("Size = %d, want %d", result.Size, expectedSize)
	}

	// Verify all files are present
	expectedFiles := []string{
		"/path/to/file1.txt",
		"/path/to/file2.txt",
		"/path/to/file3.txt",
	}

	for i, expected := range expectedFiles {
		if result.Files[i] != expected {
			t.Errorf("Files[%d] = %s, want %s", i, result.Files[i], expected)
		}
	}
}

// TestResult_AddFile_ZeroSize tests adding file with zero size
func TestResult_AddFile_ZeroSize(t *testing.T) {
	result := New(types.BundleType("gpu-operator"))

	result.AddFile("/path/to/empty.txt", 0)

	if len(result.Files) != 1 {
		t.Errorf("Expected 1 file, got %d", len(result.Files))
	}

	if result.Size != 0 {
		t.Errorf("Size = %d, want 0", result.Size)
	}
}

// TestResult_AddError tests adding errors to result
func TestResult_AddError(t *testing.T) {
	result := New(types.BundleType("gpu-operator"))

	// Add single error
	err1 := errors.New("first error")
	result.AddError(err1)

	if len(result.Errors) != 1 {
		t.Errorf("Expected 1 error, got %d", len(result.Errors))
	}

	if result.Errors[0] != "first error" {
		t.Errorf("Error = %s, want 'first error'", result.Errors[0])
	}

	// Add multiple errors
	err2 := errors.New("second error")
	err3 := errors.New("third error")
	result.AddError(err2)
	result.AddError(err3)

	if len(result.Errors) != 3 {
		t.Errorf("Expected 3 errors, got %d", len(result.Errors))
	}

	expectedErrors := []string{"first error", "second error", "third error"}
	for i, expected := range expectedErrors {
		if result.Errors[i] != expected {
			t.Errorf("Errors[%d] = %s, want %s", i, result.Errors[i], expected)
		}
	}
}

// TestResult_AddError_Nil tests adding nil error (should be ignored)
func TestResult_AddError_Nil(t *testing.T) {
	result := New(types.BundleType("gpu-operator"))

	result.AddError(nil)

	if len(result.Errors) != 0 {
		t.Errorf("Adding nil error should not add to Errors, got %d errors", len(result.Errors))
	}

	// Add real error, then nil, then another real error
	result.AddError(errors.New("error 1"))
	result.AddError(nil)
	result.AddError(errors.New("error 2"))

	if len(result.Errors) != 2 {
		t.Errorf("Expected 2 errors (nil should be ignored), got %d", len(result.Errors))
	}
}

// TestResult_MarkSuccess tests marking result as successful
func TestResult_MarkSuccess(t *testing.T) {
	result := New(types.BundleType("gpu-operator"))

	if result.Success {
		t.Error("New result should not be successful")
	}

	result.MarkSuccess()

	if !result.Success {
		t.Error("Result should be marked as successful")
	}

	// Mark success multiple times (should be idempotent)
	result.MarkSuccess()
	result.MarkSuccess()

	if !result.Success {
		t.Error("Result should remain successful after multiple calls")
	}
}

// TestResult_Duration tests setting duration
func TestResult_Duration(t *testing.T) {
	result := New(types.BundleType("gpu-operator"))

	duration := 5 * time.Second
	result.Duration = duration

	if result.Duration != duration {
		t.Errorf("Duration = %v, want %v", result.Duration, duration)
	}
}

// TestResult_Checksum tests setting checksum
func TestResult_Checksum(t *testing.T) {
	result := New(types.BundleType("gpu-operator"))

	checksum := "a1b2c3d4e5f6"
	result.Checksum = checksum

	if result.Checksum != checksum {
		t.Errorf("Checksum = %s, want %s", result.Checksum, checksum)
	}
}

// TestResult_CompleteWorkflow tests a complete result workflow
func TestResult_CompleteWorkflow(t *testing.T) {
	result := New(types.BundleType("gpu-operator"))

	// Add files
	result.AddFile("/output/values.yaml", 1024)
	result.AddFile("/output/manifest.yaml", 2048)
	result.AddFile("/output/README.md", 512)

	// Add some errors
	result.AddError(errors.New("warning: deprecated field used"))
	result.AddError(errors.New("info: skipped optional file"))

	// Set additional properties
	result.Duration = 3 * time.Second
	result.Checksum = "abc123def456"

	// Mark as successful (despite warnings)
	result.MarkSuccess()

	// Verify complete state
	if len(result.Files) != 3 {
		t.Errorf("Expected 3 files, got %d", len(result.Files))
	}

	expectedSize := int64(3584) // 1024 + 2048 + 512
	if result.Size != expectedSize {
		t.Errorf("Size = %d, want %d", result.Size, expectedSize)
	}

	if len(result.Errors) != 2 {
		t.Errorf("Expected 2 errors, got %d", len(result.Errors))
	}

	if !result.Success {
		t.Error("Result should be marked as successful")
	}

	if result.Duration != 3*time.Second {
		t.Errorf("Duration = %v, want 3s", result.Duration)
	}

	if result.Checksum != "abc123def456" {
		t.Errorf("Checksum = %s, want abc123def456", result.Checksum)
	}

	if result.Type != types.BundleType("gpu-operator") {
		t.Errorf("Type = %s, want %s", result.Type, types.BundleType("gpu-operator"))
	}
}

// TestResult_MultipleTypes tests results for different bundler types
func TestResult_MultipleTypes(t *testing.T) {
	types := []types.BundleType{
		types.BundleType("gpu-operator"),
		types.BundleType("network-operator"),
		types.BundleType("custom-bundler"),
	}

	for _, bundlerType := range types {
		result := New(bundlerType)

		if result.Type != bundlerType {
			t.Errorf("Type = %s, want %s", result.Type, bundlerType)
		}

		// Each result should be independent
		result.AddFile("/test/file.txt", 100)
		result.MarkSuccess()

		if len(result.Files) != 1 {
			t.Errorf("Expected 1 file for %s, got %d", bundlerType, len(result.Files))
		}
	}
}

// TestResult_LargeFileSize tests adding large file sizes
func TestResult_LargeFileSize(t *testing.T) {
	result := New(types.BundleType("gpu-operator"))

	// Add files with large sizes
	largeSize := int64(1024 * 1024 * 1024) // 1 GB
	result.AddFile("/large/file1.bin", largeSize)
	result.AddFile("/large/file2.bin", largeSize)

	expectedTotal := largeSize * 2
	if result.Size != expectedTotal {
		t.Errorf("Size = %d, want %d", result.Size, expectedTotal)
	}
}

// TestResult_SetOCIMetadata tests setting OCI metadata.
func TestResult_SetOCIMetadata(t *testing.T) {
	result := New(types.BundleType("gpu-operator"))

	result.SetOCIMetadata("sha256:abc123", "ghcr.io/nvidia/bundle:v1.0.0", true)

	if result.OCIDigest != "sha256:abc123" {
		t.Errorf("OCIDigest = %q, want sha256:abc123", result.OCIDigest)
	}
	if result.OCIReference != "ghcr.io/nvidia/bundle:v1.0.0" {
		t.Errorf("OCIReference = %q, want ghcr.io/nvidia/bundle:v1.0.0", result.OCIReference)
	}
	if !result.Pushed {
		t.Error("Pushed = false, want true")
	}

	// Test with pushed=false
	result2 := New(types.BundleType("network-operator"))
	result2.SetOCIMetadata("sha256:def456", "localhost:5000/test:latest", false)

	if result2.Pushed {
		t.Error("Pushed = true, want false")
	}
}

// TestResult_EmptyState tests result in empty state
func TestResult_EmptyState(t *testing.T) {
	result := New(types.BundleType("gpu-operator"))

	// Verify result is in valid empty state
	if len(result.Files) != 0 {
		t.Error("Empty result should have no files")
	}

	if len(result.Errors) != 0 {
		t.Error("Empty result should have no errors")
	}

	if result.Size != 0 {
		t.Error("Empty result should have 0 size")
	}

	if result.Success {
		t.Error("Empty result should not be marked as successful")
	}
}
