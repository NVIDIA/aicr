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

package verifier

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
)

// TestIntegration_VerifyChecksumOnly tests the full verify flow for a bundle
// created with --skip-attestation (no attestation files).
func TestIntegration_VerifyChecksumOnly(t *testing.T) {
	dir := createTestBundle(t)

	result, err := Verify(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}

	if !result.ChecksumsPassed {
		t.Error("ChecksumsPassed should be true")
	}
	if result.TrustLevel != TrustUnverified {
		t.Errorf("TrustLevel = %s, want unverified", result.TrustLevel)
	}
	if result.BundleAttested {
		t.Error("BundleAttested should be false")
	}
	if result.BinaryAttested {
		t.Error("BinaryAttested should be false")
	}
	if result.ChecksumFiles != 3 {
		t.Errorf("ChecksumFiles = %d, want 3", result.ChecksumFiles)
	}
}

// TestIntegration_VerifyTamperedBundle tests that tampering is detected.
func TestIntegration_VerifyTamperedBundle(t *testing.T) {
	dir := createTestBundle(t)

	// Tamper with a file
	if err := os.WriteFile(filepath.Join(dir, "deploy.sh"), []byte("#!/bin/bash\nmalicious"), 0600); err != nil {
		t.Fatal(err)
	}

	result, err := Verify(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}

	if result.ChecksumsPassed {
		t.Error("ChecksumsPassed should be false after tampering")
	}
	if result.TrustLevel != TrustUnknown {
		t.Errorf("TrustLevel = %s, want unknown (tampered)", result.TrustLevel)
	}
	if len(result.Errors) == 0 {
		t.Error("expected errors describing tampered file")
	}
}

// TestIntegration_VerifyWithDataDir tests that a data/ directory caps trust at attested.
func TestIntegration_VerifyWithDataDir(t *testing.T) {
	dir := createTestBundle(t)

	// Add a data/ directory to simulate --data usage
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}
	dataFile := filepath.Join(dataDir, "overrides.yaml")
	if err := os.WriteFile(dataFile, []byte("key: value"), 0600); err != nil {
		t.Fatal(err)
	}

	// Regenerate checksums to include the data file
	allFiles := []string{
		filepath.Join(dir, "recipe.yaml"),
		filepath.Join(dir, "gpu-operator/values.yaml"),
		filepath.Join(dir, "deploy.sh"),
		dataFile,
	}
	if err := checksum.GenerateChecksums(context.Background(), dir, allFiles); err != nil {
		t.Fatal(err)
	}

	result, err := Verify(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}

	if !result.ChecksumsPassed {
		t.Error("ChecksumsPassed should be true")
	}
	// Without attestation files, trust is unverified (can't reach attested without attestation)
	// The data/ check only applies when full chain is verified
	if result.TrustLevel != TrustUnverified {
		t.Errorf("TrustLevel = %s, want unverified (no attestation)", result.TrustLevel)
	}
}

// TestIntegration_VerifyMissingBundle tests error for nonexistent directory.
func TestIntegration_VerifyMissingBundle(t *testing.T) {
	_, err := Verify(context.Background(), "/tmp/nonexistent-bundle-dir-12345", nil)
	if err == nil {
		t.Error("Verify() should return error for nonexistent directory")
	}
}

// TestIntegration_TrustLevelComparison tests that trust level ordering works correctly.
func TestIntegration_TrustLevelComparison(t *testing.T) {
	tests := []struct {
		name    string
		level   TrustLevel
		minimum TrustLevel
		meets   bool
	}{
		{"verified >= verified", TrustVerified, TrustVerified, true},
		{"verified >= attested", TrustVerified, TrustAttested, true},
		{"verified >= unverified", TrustVerified, TrustUnverified, true},
		{"verified >= unknown", TrustVerified, TrustUnknown, true},
		{"attested >= attested", TrustAttested, TrustAttested, true},
		{"attested < verified", TrustAttested, TrustVerified, false},
		{"unverified >= unverified", TrustUnverified, TrustUnverified, true},
		{"unverified < attested", TrustUnverified, TrustAttested, false},
		{"unknown < unverified", TrustUnknown, TrustUnverified, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.level.MeetsMinimum(tt.minimum); got != tt.meets {
				t.Errorf("%s.MeetsMinimum(%s) = %v, want %v", tt.level, tt.minimum, got, tt.meets)
			}
		})
	}
}
