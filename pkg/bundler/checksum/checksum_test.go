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

package checksum

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/errors"
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

		// Supply reverse order; the serialized manifest must be deterministic.
		err := GenerateChecksums(context.Background(), tmpDir, []string{file2, file1})
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
		if !strings.HasSuffix(lines[0], "  file1.txt") || !strings.HasSuffix(lines[1], "  file2.txt") {
			t.Errorf("checksum paths are not sorted: %#v", lines)
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

	t.Run("rejects empty file list", func(t *testing.T) {
		t.Parallel()

		tmpDir := t.TempDir()

		err := GenerateChecksums(context.Background(), tmpDir, []string{})
		requireInventoryErrorCode(t, err, errors.ErrCodeInvalidRequest)
		if _, statErr := os.Stat(GetChecksumFilePath(tmpDir)); !os.IsNotExist(statErr) {
			t.Errorf("empty generation wrote checksums.txt: %v", statErr)
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
		if strings.Contains(string(data), "\\") {
			t.Errorf("checksums.txt path was not slash normalized: %q", data)
		}
	})

	invalidTests := []struct {
		name  string
		setup func(t *testing.T, dir string) []string
	}{
		{
			name: "untracked file beside listed file",
			setup: func(t *testing.T, dir string) []string {
				tracked := filepath.Join(dir, "tracked.txt")
				if err := os.WriteFile(tracked, []byte("tracked"), 0600); err != nil {
					t.Fatalf("WriteFile() tracked error = %v", err)
				}
				if err := os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("extra"), 0600); err != nil {
					t.Fatalf("WriteFile() extra error = %v", err)
				}
				return []string{tracked}
			},
		},
		{
			name: "file outside bundle directory",
			setup: func(t *testing.T, _ string) []string {
				outside := filepath.Join(t.TempDir(), "outside.txt")
				if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
					t.Fatalf("WriteFile() outside error = %v", err)
				}
				return []string{outside}
			},
		},
		{
			name: "duplicate file argument",
			setup: func(t *testing.T, dir string) []string {
				file := filepath.Join(dir, "payload.txt")
				if err := os.WriteFile(file, []byte("payload"), 0600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
				return []string{file, file}
			},
		},
		{
			name: "symlink argument",
			setup: func(t *testing.T, dir string) []string {
				target := filepath.Join(dir, "target.txt")
				if err := os.WriteFile(target, []byte("target"), 0600); err != nil {
					t.Fatalf("WriteFile() target error = %v", err)
				}
				link := filepath.Join(dir, "payload.txt")
				if err := os.Symlink(target, link); err != nil {
					t.Fatalf("Symlink() error = %v", err)
				}
				return []string{link}
			},
		},
		{
			name: "pre-existing unexpected directory",
			setup: func(t *testing.T, dir string) []string {
				file := filepath.Join(dir, "payload.txt")
				if err := os.WriteFile(file, []byte("payload"), 0600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
				if err := os.Mkdir(filepath.Join(dir, "empty"), 0755); err != nil {
					t.Fatalf("Mkdir() error = %v", err)
				}
				return []string{file}
			},
		},
	}

	for _, tt := range invalidTests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			err := GenerateChecksums(context.Background(), dir, tt.setup(t, dir))
			requireInventoryErrorCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}

	t.Run("regenerates an existing root checksum file", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		payload := filepath.Join(dir, "payload.txt")
		if err := os.WriteFile(payload, []byte("payload"), 0600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		if err := GenerateChecksums(context.Background(), dir, []string{payload}); err != nil {
			t.Fatalf("first GenerateChecksums() error = %v", err)
		}
		first, err := os.ReadFile(GetChecksumFilePath(dir))
		if err != nil {
			t.Fatalf("ReadFile() first error = %v", err)
		}
		if secondErr := GenerateChecksums(context.Background(), dir, []string{payload}); secondErr != nil {
			t.Fatalf("second GenerateChecksums() error = %v", secondErr)
		}
		second, err := os.ReadFile(GetChecksumFilePath(dir))
		if err != nil {
			t.Fatalf("ReadFile() second error = %v", err)
		}
		if string(second) != string(first) {
			t.Errorf("regenerated checksums = %q, want %q", second, first)
		}
	})
}

func TestGenerateChecksums_PostPublishVerificationFailureRollsBack(t *testing.T) {
	tests := []struct {
		name          string
		priorManifest []byte
	}{
		{name: "removes newly published manifest"},
		{name: "restores prior manifest", priorManifest: []byte("prior manifest bytes\n")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			payload := filepath.Join(dir, "payload.txt")
			if err := os.WriteFile(payload, []byte("payload"), 0600); err != nil {
				t.Fatalf("WriteFile(payload) error = %v", err)
			}
			checksumPath := GetChecksumFilePath(dir)
			if tt.priorManifest != nil {
				if err := os.WriteFile(checksumPath, tt.priorManifest, 0600); err != nil {
					t.Fatalf("WriteFile(prior checksums) error = %v", err)
				}
			}

			ctx := &cancelOnChecksumPublicationContext{
				Context:      context.Background(),
				checksumPath: checksumPath,
				prior:        tt.priorManifest,
			}
			err := GenerateChecksums(ctx, dir, []string{payload})
			if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
				t.Fatalf("GenerateChecksums() error = %v, want ErrCodeTimeout", err)
			}

			got, readErr := os.ReadFile(checksumPath)
			if tt.priorManifest == nil {
				if !os.IsNotExist(readErr) {
					t.Errorf("ReadFile(checksums) error = %v, want not exist", readErr)
				}
				return
			}
			if readErr != nil {
				t.Fatalf("ReadFile(restored checksums) error = %v", readErr)
			}
			if string(got) != string(tt.priorManifest) {
				t.Errorf("restored checksums = %q, want %q", got, tt.priorManifest)
			}
		})
	}
}

func TestGenerateChecksums_RootSwapBeforePublicationDoesNotMutateReplacement(t *testing.T) {
	parent := t.TempDir()
	bundleDir := filepath.Join(parent, "bundle")
	if err := os.Mkdir(bundleDir, 0700); err != nil {
		t.Fatalf("Mkdir(bundle) error = %v", err)
	}
	payload := filepath.Join(bundleDir, "payload.txt")
	if err := os.WriteFile(payload, []byte("payload"), 0600); err != nil {
		t.Fatalf("WriteFile(payload) error = %v", err)
	}

	originalDir := filepath.Join(parent, "original")
	replacementMarker := filepath.Join(bundleDir, "replacement-marker")
	deps := defaultChecksumGenerationDependencies()
	deps.afterValidation = func() error {
		if err := os.Rename(bundleDir, originalDir); err != nil {
			return err
		}
		if err := os.Mkdir(bundleDir, 0700); err != nil {
			return err
		}
		return os.WriteFile(replacementMarker, []byte("replacement"), 0600)
	}

	err := generateChecksumsWithDependencies(
		context.Background(), bundleDir, []string{payload}, deps)
	requireInventoryErrorCode(t, err, errors.ErrCodeInvalidRequest)
	if _, statErr := os.Lstat(filepath.Join(originalDir, ChecksumFileName)); !os.IsNotExist(statErr) {
		t.Errorf("original checksums.txt error = %v, want not exist", statErr)
	}
	assertReplacementFileContents(t, replacementMarker)
	if _, statErr := os.Lstat(filepath.Join(bundleDir, ChecksumFileName)); !os.IsNotExist(statErr) {
		t.Errorf("replacement checksums.txt error = %v, want not exist", statErr)
	}
}

func TestGenerateChecksums_RootSwapBeforeVerificationRollsBackAnchoredOriginal(t *testing.T) {
	tests := []struct {
		name          string
		priorManifest []byte
	}{
		{name: "removes newly published manifest"},
		{name: "restores prior manifest", priorManifest: []byte("prior manifest bytes\n")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent := t.TempDir()
			bundleDir := filepath.Join(parent, "bundle")
			if err := os.Mkdir(bundleDir, 0700); err != nil {
				t.Fatalf("Mkdir(bundle) error = %v", err)
			}
			payload := filepath.Join(bundleDir, "payload.txt")
			if err := os.WriteFile(payload, []byte("payload"), 0600); err != nil {
				t.Fatalf("WriteFile(payload) error = %v", err)
			}
			if tt.priorManifest != nil {
				if err := os.WriteFile(
					filepath.Join(bundleDir, ChecksumFileName), tt.priorManifest, 0640); err != nil {
					t.Fatalf("WriteFile(prior checksums) error = %v", err)
				}
			}

			originalDir := filepath.Join(parent, "original")
			replacementMarker := filepath.Join(bundleDir, "replacement-marker")
			deps := defaultChecksumGenerationDependencies()
			deps.beforeVerification = func() error {
				if err := os.Rename(bundleDir, originalDir); err != nil {
					return err
				}
				if err := os.Mkdir(bundleDir, 0700); err != nil {
					return err
				}
				return os.WriteFile(replacementMarker, []byte("replacement"), 0600)
			}

			err := generateChecksumsWithDependencies(
				context.Background(), bundleDir, []string{payload}, deps)
			requireInventoryErrorCode(t, err, errors.ErrCodeInvalidRequest)
			got, readErr := os.ReadFile(filepath.Join(originalDir, ChecksumFileName))
			if tt.priorManifest == nil {
				if !os.IsNotExist(readErr) {
					t.Errorf("original checksums.txt error = %v, want not exist", readErr)
				}
			} else {
				if readErr != nil {
					t.Fatalf("ReadFile(restored checksums) error = %v", readErr)
				}
				if string(got) != string(tt.priorManifest) {
					t.Errorf("restored checksums = %q, want %q", got, tt.priorManifest)
				}
			}
			assertReplacementFileContents(t, replacementMarker)
			if _, statErr := os.Lstat(filepath.Join(bundleDir, ChecksumFileName)); !os.IsNotExist(statErr) {
				t.Errorf("replacement checksums.txt error = %v, want not exist", statErr)
			}
		})
	}
}

type cancelOnChecksumPublicationContext struct {
	context.Context
	checksumPath string
	prior        []byte
	canceled     atomic.Bool
}

func (c *cancelOnChecksumPublicationContext) Err() error {
	if c.canceled.Load() {
		return context.Canceled
	}
	data, err := os.ReadFile(c.checksumPath)
	published := c.prior == nil && err == nil ||
		c.prior != nil && err == nil && string(data) != string(c.prior)
	if published {
		c.canceled.Store(true)
		return context.Canceled
	}
	return nil
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

	t.Run("path traversal rejected", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		// Write a checksums.txt with a path traversal entry
		content := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  ../../etc/passwd\n"
		checksumPath := filepath.Join(dir, ChecksumFileName)
		if err := os.WriteFile(checksumPath, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}

		errs := VerifyChecksums(dir)
		if len(errs) == 0 {
			t.Fatal("VerifyChecksums() should reject path traversal")
		}
	})

	t.Run("malformed data returns an error description", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ChecksumFileName), []byte("malformed\n"), 0600); err != nil {
			t.Fatal(err)
		}
		errs := VerifyChecksumsFromData(dir, []byte("malformed\n"))
		if len(errs) == 0 {
			t.Fatal("VerifyChecksumsFromData() expected an error description")
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

func TestSHA256RawContext(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "payload.txt")
	if err := os.WriteFile(path, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Run("legacy wrapper matches context API", func(t *testing.T) {
		t.Parallel()

		legacy, err := SHA256Raw(path)
		if err != nil {
			t.Fatalf("SHA256Raw() error = %v", err)
		}
		withContext, err := SHA256RawContext(context.Background(), path)
		if err != nil {
			t.Fatalf("SHA256RawContext() error = %v", err)
		}
		if string(withContext) != string(legacy) {
			t.Errorf("SHA256RawContext() = %x, want %x", withContext, legacy)
		}
	})

	t.Run("caller cancellation propagates", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := SHA256RawContext(ctx, path)
		if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			t.Errorf("SHA256RawContext() error = %v, want ErrCodeTimeout", err)
		}
	})

	t.Run("rejects FIFO without blocking past cancellation", func(t *testing.T) {
		fifo := filepath.Join(t.TempDir(), "payload.pipe")
		if err := unix.Mkfifo(fifo, 0600); err != nil {
			t.Skipf("FIFO unsupported: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := SHA256RawContext(ctx, fifo)
			result <- err
		}()

		select {
		case err := <-result:
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("SHA256RawContext(FIFO) error = %v, want ErrCodeInvalidRequest", err)
			}
		case <-time.After(time.Second):
			t.Error("SHA256RawContext(FIFO) remained blocked after context cancellation")
			go func() {
				writer, err := os.OpenFile(fifo, os.O_WRONLY, 0)
				if err == nil {
					_ = writer.Close()
				}
			}()
			select {
			case <-result:
			case <-time.After(time.Second):
			}
		}
	})
}

func TestSHA256RawContext_RejectsFIFOReplacementBeforeOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload.txt")
	if err := os.WriteFile(path, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(dir, "payload.original")

	result := make(chan error, 1)
	go func() {
		_, err := sha256RawContextWithOpener(
			context.Background(), path,
			func(name string, flag int, perm os.FileMode) (*os.File, error) {
				if err := os.Rename(name, original); err != nil {
					return nil, err
				}
				if err := unix.Mkfifo(name, 0600); err != nil {
					return nil, err
				}
				return os.OpenFile(name, flag, perm)
			},
		)
		result <- err
	}()

	select {
	case err := <-result:
		requireInventoryErrorCode(t, err, errors.ErrCodeInvalidRequest)
	case <-time.After(time.Second):
		t.Error("SHA256RawContext blocked after a regular file was replaced by a FIFO")
		go func() {
			writer, err := os.OpenFile(path, os.O_WRONLY, 0)
			if err == nil {
				_ = writer.Close()
			}
		}()
		select {
		case <-result:
		case <-time.After(time.Second):
		}
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

func TestWriteChecksums(t *testing.T) {
	t.Parallel()

	// setup returns the Output that WriteChecksums will receive (or nil).
	// May stage files on disk under tmpDir before returning.
	tests := []struct {
		name       string
		setup      func(t *testing.T, tmpDir string) *deployer.Output
		wantErr    bool
		wantCode   errors.ErrorCode
		wantMsg    string
		assertPass func(t *testing.T, tmpDir string, output *deployer.Output)
	}{
		{
			name: "appends checksum file and updates size",
			setup: func(t *testing.T, tmpDir string) *deployer.Output {
				t.Helper()
				file1 := filepath.Join(tmpDir, "a.txt")
				file2 := filepath.Join(tmpDir, "b.txt")
				if err := os.WriteFile(file1, []byte("aaa"), 0600); err != nil {
					t.Fatalf("write file1: %v", err)
					return nil
				}
				if err := os.WriteFile(file2, []byte("bbbb"), 0600); err != nil {
					t.Fatalf("write file2: %v", err)
					return nil
				}
				return &deployer.Output{
					Files:     []string{file1, file2},
					TotalSize: 7,
				}
			},
			assertPass: func(t *testing.T, _ string, output *deployer.Output) {
				t.Helper()
				if len(output.Files) != 3 {
					t.Fatalf("expected 3 entries in output.Files, got %d", len(output.Files))
					return
				}
				if !strings.HasSuffix(output.Files[2], ChecksumFileName) {
					t.Errorf("last entry should be %s, got %s", ChecksumFileName, output.Files[2])
				}
				info, err := os.Stat(output.Files[2])
				if err != nil {
					t.Fatalf("stat checksum file: %v", err)
					return
				}
				if output.TotalSize != 7+info.Size() {
					t.Errorf("TotalSize = %d, want %d", output.TotalSize, 7+info.Size())
				}
			},
		},
		{
			name: "propagates underlying error when source file missing",
			setup: func(_ *testing.T, tmpDir string) *deployer.Output {
				return &deployer.Output{
					Files: []string{filepath.Join(tmpDir, "does-not-exist")},
				}
			},
			wantErr:  true,
			wantCode: errors.ErrCodeInvalidRequest,
			wantMsg:  "missing",
		},
		{
			name:     "rejects nil output",
			setup:    func(_ *testing.T, _ string) *deployer.Output { return nil },
			wantErr:  true,
			wantCode: errors.ErrCodeInvalidRequest,
			wantMsg:  "output is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			tmpDir := t.TempDir()
			output := tt.setup(t, tmpDir)

			err := WriteChecksums(context.Background(), tmpDir, output)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
					return
				}
				var structErr *errors.StructuredError
				if !stderrors.As(err, &structErr) {
					t.Fatalf("expected *errors.StructuredError, got %T: %v", err, err)
					return
				}
				if structErr.Code != tt.wantCode {
					t.Errorf("error code = %s, want %s", structErr.Code, tt.wantCode)
				}
				if tt.wantMsg != "" && !strings.Contains(err.Error(), tt.wantMsg) {
					t.Errorf("error message should contain %q, got: %v", tt.wantMsg, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("WriteChecksums() unexpected error = %v", err)
				return
			}
			if tt.assertPass != nil {
				tt.assertPass(t, tmpDir, output)
			}
		})
	}
}
