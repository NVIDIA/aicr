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

package attestation

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindBinaryAttestation(t *testing.T) {
	// Create temp dir with a fake binary and attestation
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "aicr")
	attestPath := filepath.Join(dir, "aicr-attestation.sigstore.json")

	if err := os.WriteFile(binaryPath, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attestPath, []byte(`{"fake":"attestation"}`), 0600); err != nil {
		t.Fatal(err)
	}

	path, err := FindBinaryAttestation(binaryPath)
	if err != nil {
		t.Fatalf("FindBinaryAttestation() error: %v", err)
	}
	if path != attestPath {
		t.Errorf("FindBinaryAttestation() = %q, want %q", path, attestPath)
	}
}

func TestComputeFileDigest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0600); err != nil {
		t.Fatal(err)
	}

	digest, err := ComputeFileDigest(path)
	if err != nil {
		t.Fatalf("ComputeFileDigest() error: %v", err)
	}

	// SHA256 of "hello world"
	expected := "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"
	if digest != expected {
		t.Errorf("ComputeFileDigest() = %q, want %q", digest, expected)
	}
}

func TestComputeFileDigest_NotFound(t *testing.T) {
	_, err := ComputeFileDigest("/nonexistent/file")
	if err == nil {
		t.Error("ComputeFileDigest() should return error for nonexistent file")
	}
}

func TestFindBinaryAttestation_NotFound(t *testing.T) {
	dir := t.TempDir()
	binaryPath := filepath.Join(dir, "aicr")

	if err := os.WriteFile(binaryPath, []byte("binary"), 0755); err != nil {
		t.Fatal(err)
	}

	_, err := FindBinaryAttestation(binaryPath)
	if err == nil {
		t.Error("FindBinaryAttestation() with missing attestation should return error")
	}
}
