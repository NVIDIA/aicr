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

// createTestBundle creates a minimal bundle directory with checksums generated
// by the checksum package (same code path as real bundle creation).
func createTestBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// Create some content files
	files := map[string]string{
		"recipe.yaml":              "apiVersion: v1\nkind: Recipe\n",
		"gpu-operator/values.yaml": "driver:\n  version: 570.86.16\n",
		"deploy.sh":                "#!/bin/bash\nhelm install ...\n",
	}

	filePaths := make([]string, 0, len(files))
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		filePaths = append(filePaths, path)
	}

	// Generate checksums using the same code path as real bundle creation
	if err := checksum.GenerateChecksums(context.Background(), dir, filePaths); err != nil {
		t.Fatal(err)
	}

	return dir
}

func TestVerify_ChecksumsOnly(t *testing.T) {
	dir := createTestBundle(t)

	result, err := Verify(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}

	if !result.ChecksumsPassed {
		t.Error("ChecksumsPassed = false, want true")
	}
	if result.TrustLevel != TrustUnverified {
		t.Errorf("TrustLevel = %s, want unverified", result.TrustLevel)
	}
	if result.BundleAttested {
		t.Error("BundleAttested = true, want false")
	}
}

func TestVerify_MissingChecksums(t *testing.T) {
	dir := t.TempDir()

	result, err := Verify(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}

	if result.TrustLevel != TrustUnknown {
		t.Errorf("TrustLevel = %s, want unknown", result.TrustLevel)
	}
}

func TestVerify_TamperedFile(t *testing.T) {
	dir := createTestBundle(t)

	// Tamper with a file after checksums were generated
	if err := os.WriteFile(filepath.Join(dir, "recipe.yaml"), []byte("tampered content"), 0600); err != nil {
		t.Fatal(err)
	}

	result, err := Verify(context.Background(), dir, nil)
	if err != nil {
		t.Fatalf("Verify() error: %v", err)
	}

	if result.ChecksumsPassed {
		t.Error("ChecksumsPassed = true, want false (file was tampered)")
	}
	if len(result.Errors) == 0 {
		t.Error("expected errors for tampered file")
	}
}

func TestVerify_NonexistentDir(t *testing.T) {
	_, err := Verify(context.Background(), "/nonexistent/path", nil)
	if err == nil {
		t.Error("Verify() with nonexistent dir should return error")
	}
}
