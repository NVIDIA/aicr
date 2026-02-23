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

package checksum

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateChecksums(t *testing.T) {
	t.Parallel()

	t.Run("generates checksums for files", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()

		// Create test files
		file1 := filepath.Join(tmpDir, "file1.txt")
		file2 := filepath.Join(tmpDir, "file2.txt")

		if err := os.WriteFile(file1, []byte("content1"), 0644); err != nil {
			t.Fatalf("failed to create file1: %v", err)
		}
		if err := os.WriteFile(file2, []byte("content2"), 0644); err != nil {
			t.Fatalf("failed to create file2: %v", err)
		}

		// Generate checksums
		err := GenerateChecksums(context.Background(), tmpDir, []string{file1, file2})
		if err != nil {
			t.Fatalf("GenerateChecksums() error = %v", err)
		}

		// Verify checksums.txt was created
		checksumPath := GetChecksumFilePath(tmpDir)
		data, err := os.ReadFile(checksumPath)
		if err != nil {
			t.Fatalf("failed to read checksums.txt: %v", err)
		}
		content := string(data)

		// Check that both files are in the checksums
		if !strings.Contains(content, "file1.txt") {
			t.Error("checksums.txt should contain file1.txt")
		}
		if !strings.Contains(content, "file2.txt") {
			t.Error("checksums.txt should contain file2.txt")
		}

		// Check format: should have sha256 hash followed by two spaces and filename
		lines := strings.Split(strings.TrimSpace(content), "\n")
		if len(lines) != 2 {
			t.Errorf("expected 2 lines, got %d", len(lines))
		}
		for _, line := range lines {
			parts := strings.Split(line, "  ")
			if len(parts) != 2 {
				t.Errorf("invalid checksum format: %s", line)
			}
			// SHA256 hash should be 64 hex characters
			if len(parts[0]) != 64 {
				t.Errorf("expected 64 character hash, got %d: %s", len(parts[0]), parts[0])
			}
		}
	})

	t.Run("returns error on context cancellation", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := GenerateChecksums(ctx, t.TempDir(), []string{})
		if err == nil {
			t.Error("expected error for cancelled context")
		}
	})

	t.Run("returns error for non-existent file", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		nonExistent := filepath.Join(tmpDir, "does-not-exist.txt")

		err := GenerateChecksums(context.Background(), tmpDir, []string{nonExistent})
		if err == nil {
			t.Error("expected error for non-existent file")
		}
	})

	t.Run("handles empty file list", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()

		err := GenerateChecksums(context.Background(), tmpDir, []string{})
		if err != nil {
			t.Fatalf("GenerateChecksums() error = %v", err)
		}

		// Verify checksums.txt was created (even if empty)
		checksumPath := GetChecksumFilePath(tmpDir)
		data, err := os.ReadFile(checksumPath)
		if err != nil {
			t.Fatalf("failed to read checksums.txt: %v", err)
		}

		// Should just have a newline
		if string(data) != "\n" {
			t.Errorf("expected empty checksums to have just newline, got %q", string(data))
		}
	})

	t.Run("handles nested files", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()
		subDir := filepath.Join(tmpDir, "subdir")

		if err := os.MkdirAll(subDir, 0755); err != nil {
			t.Fatalf("failed to create subdir: %v", err)
		}

		nestedFile := filepath.Join(subDir, "nested.txt")
		if err := os.WriteFile(nestedFile, []byte("nested content"), 0644); err != nil {
			t.Fatalf("failed to create nested file: %v", err)
		}

		err := GenerateChecksums(context.Background(), tmpDir, []string{nestedFile})
		if err != nil {
			t.Fatalf("GenerateChecksums() error = %v", err)
		}

		// Verify the relative path includes the subdir
		checksumPath := GetChecksumFilePath(tmpDir)
		data, err := os.ReadFile(checksumPath)
		if err != nil {
			t.Fatalf("failed to read checksums.txt: %v", err)
		}

		if !strings.Contains(string(data), "subdir/nested.txt") {
			t.Errorf("expected relative path subdir/nested.txt, got %s", string(data))
		}
	})
}

func TestVerifyChecksums(t *testing.T) {
	t.Parallel()

	t.Run("valid checksums pass", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		// Create files and generate checksums
		file1 := filepath.Join(dir, "file1.txt")
		file2 := filepath.Join(dir, "sub/file2.txt")
		if err := os.MkdirAll(filepath.Join(dir, "sub"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file1, []byte("content1"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file2, []byte("content2"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := GenerateChecksums(context.Background(), dir, []string{file1, file2}); err != nil {
			t.Fatal(err)
		}

		errs := VerifyChecksums(dir)
		if len(errs) != 0 {
			t.Errorf("VerifyChecksums() = %v, want no errors", errs)
		}
	})

	t.Run("tampered file detected", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		file1 := filepath.Join(dir, "file1.txt")
		if err := os.WriteFile(file1, []byte("original"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := GenerateChecksums(context.Background(), dir, []string{file1}); err != nil {
			t.Fatal(err)
		}

		// Tamper with the file
		if err := os.WriteFile(file1, []byte("tampered"), 0644); err != nil {
			t.Fatal(err)
		}

		errs := VerifyChecksums(dir)
		if len(errs) == 0 {
			t.Error("VerifyChecksums() should detect tampered file")
		}
	})

	t.Run("missing checksums file", func(t *testing.T) {
		t.Parallel()

		errs := VerifyChecksums(t.TempDir())
		if len(errs) == 0 {
			t.Error("VerifyChecksums() should report missing checksums.txt")
		}
	})
}

func TestCountEntries(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	file1 := filepath.Join(dir, "a.txt")
	file2 := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(file1, []byte("a"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file2, []byte("b"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := GenerateChecksums(context.Background(), dir, []string{file1, file2}); err != nil {
		t.Fatal(err)
	}

	count := CountEntries(dir)
	if count != 2 {
		t.Errorf("CountEntries() = %d, want 2", count)
	}
}

func TestGetChecksumFilePath(t *testing.T) {
	t.Parallel()

	path := GetChecksumFilePath("/some/bundle/dir")
	expected := "/some/bundle/dir/checksums.txt"

	if path != expected {
		t.Errorf("GetChecksumFilePath() = %s, want %s", path, expected)
	}
}
