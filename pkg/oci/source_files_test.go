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

package oci

import (
	"context"
	stderrors "errors"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"

	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

func writeSourceFixture(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{
		"a.txt":            "alpha",
		"nested/b.txt":     "beta",
		"nested/deep/c.sh": "#!/bin/sh\necho c\n",
		"other.txt":        "other",
	}
	for rel, body := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", rel, err)
		}
		mode := os.FileMode(0o640)
		if strings.HasSuffix(rel, ".sh") {
			mode = 0o750
		}
		if err := os.WriteFile(full, []byte(body), mode); err != nil {
			t.Fatalf("WriteFile(%q): %v", rel, err)
		}
	}
	return files
}

func safeOCITempRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatalf("Chmod(temp root): %v", err)
	}
	t.Setenv("TMPDIR", root)
	return root
}

func TestPathContainmentHelpersTreatFilesystemRootAsAncestor(t *testing.T) {
	t.Parallel()

	root := filepath.Clean(string(filepath.Separator))
	child := filepath.Join(root, "var", "lib", "aicr")
	tests := []struct {
		name string
		got  bool
	}{
		{name: "overlap root then child", got: pathsOverlap(root, child)},
		{name: "overlap child then root", got: pathsOverlap(child, root)},
		{name: "child below root", got: sameOrBelow(child, root)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if !tt.got {
				t.Fatal("filesystem root was not treated as an ancestor")
			}
		})
	}
}

func TestPathContainmentHelpersDetectConservativeCaseAliases(t *testing.T) {
	t.Parallel()

	root := filepath.Clean(string(filepath.Separator))
	parent := filepath.Join(root, "AICR-Case-Root", "Source")
	aliasedChild := filepath.Join(root, "aicr-case-root", "source", "nested")
	aliasedSibling := filepath.Join(root, "aicr-case-root", "source-other")
	tests := []struct {
		name string
		got  bool
		want bool
	}{
		{name: "overlap aliased child", got: pathsOverlap(parent, aliasedChild), want: true},
		{name: "overlap aliased parent", got: pathsOverlap(aliasedChild, parent), want: true},
		{name: "below aliased parent", got: sameOrBelow(aliasedChild, parent), want: true},
		{name: "component boundary remains distinct", got: sameOrBelow(aliasedSibling, parent), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if tt.got != tt.want {
				t.Fatalf("containment result = %v, want %v", tt.got, tt.want)
			}
		})
	}
}

func TestPreparedSourceReaderRejectsSameSizeMutationAfterOpen(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	const original = "original bytes"
	const replacement = "modified bytes"
	if len(original) != len(replacement) {
		t.Fatal("test fixture must preserve size")
	}
	if err := os.WriteFile(filepath.Join(source, "payload.txt"), []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	safeOCITempRoot(t)
	prepared, err := preparePackageSource(
		context.Background(), source, output, "", []string{"payload.txt"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := prepared.Close(); closeErr != nil {
			t.Errorf("prepared.Close() error = %v", closeErr)
		}
	})
	reader, err := prepared.open(context.Background(), "payload.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prepared.dir, "payload.txt"), []byte(replacement), 0o640); err != nil {
		t.Fatal(err)
	}
	_, readErr := io.ReadAll(reader)
	assertErrorCode(t, readErr, apperrors.ErrCodeInternal)
	assertErrorCode(t, reader.Close(), apperrors.ErrCodeInternal)
}

func TestPreparedSourceReaderCheckedCloseRejectsIncompleteConsumption(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	const original = "original bytes"
	if err := os.WriteFile(filepath.Join(source, "payload.txt"), []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	safeOCITempRoot(t)
	prepared, err := preparePackageSource(
		context.Background(), source, output, "", []string{"payload.txt"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := prepared.Close(); closeErr != nil {
			t.Errorf("prepared.Close() error = %v", closeErr)
		}
	})
	reader, err := prepared.open(context.Background(), "payload.txt")
	if err != nil {
		t.Fatal(err)
	}
	var prefix [1]byte
	if n, readErr := reader.Read(prefix[:]); readErr != nil || n != len(prefix) {
		t.Fatalf("reader.Read() = (%d, %v)", n, readErr)
	}
	assertErrorCode(t, reader.Close(), apperrors.ErrCodeInternal)
	assertErrorCode(t, reader.Close(), apperrors.ErrCodeInternal)
}

func TestVerifiedStagedFileCancellationClosesBlockingRawFile(t *testing.T) {
	raw, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &verifiedStagedFile{
		ctx:      ctx,
		file:     raw,
		reader:   newContextReadCloser(ctx, raw),
		digester: digest.SHA256.Digester(),
	}
	started := make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		close(started)
		var data [1]byte
		_, readErr := reader.Read(data[:])
		readDone <- readErr
	}()
	<-started
	select {
	case readErr := <-readDone:
		t.Fatalf("raw-file read returned before cancellation: %v", readErr)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case readErr := <-readDone:
		assertErrorCode(t, readErr, apperrors.ErrCodeTimeout)
	case <-time.After(time.Second):
		t.Fatal("cancellation did not close-unblock the raw-file read")
	}
	assertErrorCode(t, reader.Close(), apperrors.ErrCodeTimeout)
}

func TestVerifiedStagedFileCancellationWinsMetadataAndCloseErrors(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "payload.txt"), []byte("payload"), 0o640); err != nil {
		t.Fatal(err)
	}
	safeOCITempRoot(t)
	prepared, err := preparePackageSource(
		context.Background(), source, output, "", []string{"payload.txt"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := prepared.Close(); closeErr != nil {
			t.Errorf("prepared.Close() error = %v", closeErr)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closeErr := stderrors.New("raw staged file close failed")
	deps := defaultVerifiedStagedFileDependencies()
	var tracked *contextReadCloser
	deps.newReader = func(readerCtx context.Context, file *os.File) *contextReadCloser {
		tracked = newContextReadCloser(readerCtx, &closeErrorOSFile{File: file, closeErr: closeErr})
		return tracked
	}
	deps.beforeMetadataRevalidate = func() error {
		cancel()
		<-tracked.done
		return nil
	}
	reader, err := prepared.openWithDependencies(ctx, "payload.txt", deps)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.ReadAll(reader)
	assertErrorCode(t, readErr, apperrors.ErrCodeTimeout)
	closeResult := reader.Close()
	assertErrorCode(t, closeResult, apperrors.ErrCodeTimeout)
	if !stderrors.Is(closeResult, closeErr) {
		t.Fatalf("Close() error = %v, want secondary %v", closeResult, closeErr)
	}
}

type closeErrorOSFile struct {
	*os.File
	closeErr error
	once     sync.Once
	result   error
}

func (f *closeErrorOSFile) Close() error {
	f.once.Do(func() { f.result = stderrors.Join(f.File.Close(), f.closeErr) })
	return f.result
}

func TestPreparePackageSourceThreeStateSelection(t *testing.T) {
	for _, tt := range []struct {
		name        string
		sourceFiles []string
		subDir      string
		want        []string
		wantErr     bool
	}{
		{
			name: "nil recursively discovers sorted files",
			want: []string{"a.txt", "nested/b.txt", "nested/deep/c.sh", "other.txt"},
		},
		{
			name:   "nil with subdir preserves prefix",
			subDir: "nested",
			want:   []string{"nested/b.txt", "nested/deep/c.sh"},
		},
		{
			name:        "non-nil empty rejected",
			sourceFiles: []string{},
			wantErr:     true,
		},
		{
			name:        "non-nil empty with subdir rejected",
			sourceFiles: []string{},
			subDir:      "nested",
			wantErr:     true,
		},
		{
			name:        "explicit with subdir rejected",
			sourceFiles: []string{"a.txt"},
			subDir:      "nested",
			wantErr:     true,
		},
		{
			name:        "explicit complete set cloned and sorted",
			sourceFiles: []string{"nested/deep/c.sh", "a.txt"},
			want:        []string{"a.txt", "nested/deep/c.sh"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sourceDir := t.TempDir()
			outputDir := t.TempDir()
			files := writeSourceFixture(t, sourceDir)
			safeOCITempRoot(t)
			before := snapshotTree(t, sourceDir)
			selection := tt.sourceFiles
			prepared, err := preparePackageSource(
				context.Background(), sourceDir, outputDir, tt.subDir, selection)
			if tt.wantErr {
				assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
				if prepared != nil {
					t.Fatalf("preparePackageSource() = %+v on error, want nil", prepared)
				}
				return
			}
			if err != nil {
				t.Fatalf("preparePackageSource() error = %v", err)
			}
			t.Cleanup(func() {
				if closeErr := prepared.Close(); closeErr != nil {
					t.Errorf("prepared.Close() error = %v", closeErr)
				}
			})
			if got := prepared.relativeFiles(); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("relativeFiles() = %v, want %v", got, tt.want)
			}
			if selection != nil && !reflect.DeepEqual(selection, tt.sourceFiles) {
				t.Fatalf("caller SourceFiles mutated: got %v want %v", selection, tt.sourceFiles)
			}
			for _, rel := range tt.want {
				stagedPath := filepath.Join(prepared.dir, filepath.FromSlash(rel))
				data, readErr := os.ReadFile(stagedPath)
				if readErr != nil {
					t.Fatalf("ReadFile(staged %q): %v", rel, readErr)
				}
				if string(data) != files[rel] {
					t.Errorf("staged %q = %q, want %q", rel, data, files[rel])
				}
				srcInfo, statErr := os.Stat(filepath.Join(sourceDir, filepath.FromSlash(rel)))
				if statErr != nil {
					t.Fatal(statErr)
				}
				dstInfo, statErr := os.Stat(stagedPath)
				if statErr != nil {
					t.Fatal(statErr)
				}
				if os.SameFile(srcInfo, dstInfo) {
					t.Errorf("staged %q aliases source inode", rel)
				}
				if dstInfo.Mode().Perm() != srcInfo.Mode().Perm() {
					t.Errorf("staged %q mode = %o, want %o", rel, dstInfo.Mode().Perm(), srcInfo.Mode().Perm())
				}
				for parent := filepath.Dir(stagedPath); parent != prepared.dir && parent != "."; parent = filepath.Dir(parent) {
					parentInfo, parentErr := os.Stat(parent)
					if parentErr != nil {
						t.Fatal(parentErr)
					}
					if got := parentInfo.Mode().Perm(); got != 0o755 {
						t.Errorf("staged directory %q mode = %o, want 755", parent, got)
					}
				}
			}
			if got := snapshotTree(t, sourceDir); !reflect.DeepEqual(got, before) {
				t.Fatalf("source tree changed: before=%v after=%v", before, got)
			}
			if err := prepared.validate(); err != nil {
				t.Fatalf("prepared.validate() error = %v", err)
			}
		})
	}
}

func TestPreparePackageSourceRejectsInvalidPathsAndObjects(t *testing.T) {
	for _, tt := range []struct {
		name      string
		selection []string
		setup     func(t *testing.T, source string)
	}{
		{name: "empty path", selection: []string{""}},
		{name: "dot", selection: []string{"."}},
		{name: "absolute", selection: []string{"/tmp/x"}},
		{name: "parent", selection: []string{"../x"}},
		{name: "unclean", selection: []string{"nested/../a.txt"}},
		{name: "double slash", selection: []string{"nested//b.txt"}},
		{name: "backslash", selection: []string{`nested\b.txt`}},
		{name: "drive qualified", selection: []string{"C:/x"}},
		{name: "control character", selection: []string{"bad\tname"}},
		{name: "exact duplicate", selection: []string{"a.txt", "a.txt"}},
		{name: "case-fold duplicate", selection: []string{"a.txt", "A.TXT"}},
		{name: "missing", selection: []string{"missing.txt"}},
		{name: "directory", selection: []string{"nested"}},
		{
			name:      "leaf symlink",
			selection: []string{"link.txt"},
			setup: func(t *testing.T, source string) {
				t.Helper()
				if err := os.Symlink("a.txt", filepath.Join(source, "link.txt")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:      "ancestor symlink",
			selection: []string{"linked/b.txt"},
			setup: func(t *testing.T, source string) {
				t.Helper()
				if err := os.Symlink("nested", filepath.Join(source, "linked")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:      "fifo",
			selection: []string{"pipe"},
			setup: func(t *testing.T, source string) {
				t.Helper()
				if err := syscall.Mkfifo(filepath.Join(source, "pipe"), 0o600); err != nil {
					t.Skipf("mkfifo unsupported: %v", err)
				}
			},
		},
		{
			name:      "socket",
			selection: []string{"sock"},
			setup: func(t *testing.T, source string) {
				t.Helper()
				listener, err := net.Listen("unix", filepath.Join(source, "sock"))
				if err != nil {
					t.Skipf("unix sockets unsupported: %v", err)
				}
				t.Cleanup(func() { _ = listener.Close() })
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := t.TempDir()
			output := t.TempDir()
			writeSourceFixture(t, source)
			if tt.setup != nil {
				tt.setup(t, source)
			}
			safeOCITempRoot(t)
			prepared, err := preparePackageSource(context.Background(), source, output, "", tt.selection)
			assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
			if prepared != nil {
				t.Fatalf("preparePackageSource() = %+v on invalid input", prepared)
			}
		})
	}

	t.Run("source root symlink", func(t *testing.T) {
		realSource := t.TempDir()
		writeSourceFixture(t, realSource)
		alias := filepath.Join(t.TempDir(), "source-link")
		if err := os.Symlink(realSource, alias); err != nil {
			t.Fatal(err)
		}
		safeOCITempRoot(t)
		prepared, err := preparePackageSource(context.Background(), alias, t.TempDir(), "", nil)
		assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
		if prepared != nil {
			t.Fatal("symlink source unexpectedly produced a stage")
		}
	})

	for _, subDir := range []string{".", "../nested", "nested/../nested", `nested\deep`, "/tmp"} {
		t.Run("invalid subdir "+strings.ReplaceAll(subDir, "/", "_"), func(t *testing.T) {
			source := t.TempDir()
			writeSourceFixture(t, source)
			safeOCITempRoot(t)
			prepared, err := preparePackageSource(context.Background(), source, t.TempDir(), subDir, nil)
			assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
			if prepared != nil {
				t.Fatal("invalid SubDir returned prepared source")
			}
		})
	}

	t.Run("symlink subdir", func(t *testing.T) {
		source := t.TempDir()
		writeSourceFixture(t, source)
		if err := os.Symlink("nested", filepath.Join(source, "linked")); err != nil {
			t.Fatal(err)
		}
		safeOCITempRoot(t)
		prepared, err := preparePackageSource(context.Background(), source, t.TempDir(), "linked", nil)
		assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
		if prepared != nil {
			t.Fatal("symlink SubDir returned prepared source")
		}
	})

	t.Run("nil discovery rejects special entry", func(t *testing.T) {
		source := t.TempDir()
		if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("a"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(filepath.Join(source, "pipe"), 0o600); err != nil {
			t.Skipf("mkfifo unsupported: %v", err)
		}
		safeOCITempRoot(t)
		prepared, err := preparePackageSource(context.Background(), source, t.TempDir(), "", nil)
		assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
		if prepared != nil {
			t.Fatal("nil discovery accepted special entry")
		}
	})
}

func TestPreparePackageSourceRejectsEmptyAndOverlappingRoots(t *testing.T) {
	for _, tt := range []struct {
		name  string
		roots func(t *testing.T) (string, string)
		code  apperrors.ErrorCode
	}{
		{
			name:  "empty source",
			roots: func(t *testing.T) (string, string) { return "", t.TempDir() },
			code:  apperrors.ErrCodeInvalidRequest,
		},
		{
			name:  "empty output",
			roots: func(t *testing.T) (string, string) { return t.TempDir(), "" },
			code:  apperrors.ErrCodeInvalidRequest,
		},
		{
			name:  "equal roots",
			roots: func(t *testing.T) (string, string) { root := t.TempDir(); return root, root },
			code:  apperrors.ErrCodeInvalidRequest,
		},
		{
			name: "output below source",
			roots: func(t *testing.T) (string, string) {
				source := t.TempDir()
				output := filepath.Join(source, "out")
				if err := os.Mkdir(output, 0o755); err != nil {
					t.Fatal(err)
				}
				return source, output
			},
			code: apperrors.ErrCodeInvalidRequest,
		},
		{
			name: "output symlink alias",
			roots: func(t *testing.T) (string, string) {
				source := t.TempDir()
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(source, alias); err != nil {
					t.Fatal(err)
				}
				return source, alias
			},
			code: apperrors.ErrCodeInvalidRequest,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source, output := tt.roots(t)
			if source != "" {
				if info, err := os.Lstat(source); err == nil && info.IsDir() {
					_ = os.WriteFile(filepath.Join(source, "a.txt"), []byte("a"), 0o600)
				}
			}
			safeOCITempRoot(t)
			prepared, err := preparePackageSource(context.Background(), source, output, "", nil)
			assertErrorCode(t, err, tt.code)
			if prepared != nil {
				t.Fatal("invalid roots unexpectedly produced a stage")
			}
		})
	}
}

func TestPreparePackageSourceDetectsRootAndComponentSwaps(t *testing.T) {
	t.Run("source swap before open", func(t *testing.T) {
		parent := t.TempDir()
		source := filepath.Join(parent, "source")
		output := filepath.Join(parent, "output")
		if err := os.Mkdir(source, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(output, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		replacement := []byte("replacement")
		deps := defaultPrepareSourceDependencies()
		deps.beforeSourceOpen = func(path string) error {
			if err := os.Rename(path, path+"-moved"); err != nil {
				return err
			}
			if err := os.Mkdir(path, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(path, "a.txt"), replacement, 0o600)
		}
		safeOCITempRoot(t)
		prepared, err := preparePackageSourceWithDependencies(
			context.Background(), source, output, "", []string{"a.txt"}, deps)
		assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
		if prepared != nil {
			t.Fatal("source swap produced a stage")
		}
		data, readErr := os.ReadFile(filepath.Join(source, "a.txt"))
		if readErr != nil || !reflect.DeepEqual(data, replacement) {
			t.Fatalf("replacement source changed: data=%q err=%v", data, readErr)
		}
	})

	t.Run("output swap before open", func(t *testing.T) {
		parent := t.TempDir()
		source := filepath.Join(parent, "source")
		output := filepath.Join(parent, "output")
		if err := os.Mkdir(source, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(output, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		deps := defaultPrepareSourceDependencies()
		deps.beforeOutputOpen = func(path string) error {
			if err := os.Rename(path, path+"-moved"); err != nil {
				return err
			}
			return os.Mkdir(path, 0o755)
		}
		safeOCITempRoot(t)
		prepared, err := preparePackageSourceWithDependencies(
			context.Background(), source, output, "", []string{"a.txt"}, deps)
		assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
		if prepared != nil {
			t.Fatal("output swap produced a stage")
		}
	})

	t.Run("component replacement before descriptor open", func(t *testing.T) {
		source := t.TempDir()
		output := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("inside"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(outside, []byte("outside-secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		deps := defaultPrepareSourceDependencies()
		deps.beforeFileOpen = func(_ string) error {
			if err := os.Remove(filepath.Join(source, "a.txt")); err != nil {
				return err
			}
			return os.Symlink(outside, filepath.Join(source, "a.txt"))
		}
		safeOCITempRoot(t)
		prepared, err := preparePackageSourceWithDependencies(
			context.Background(), source, output, "", []string{"a.txt"}, deps)
		assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
		if prepared != nil {
			t.Fatal("component replacement produced a stage")
		}
	})

	t.Run("same-inode byte mutation after first hash", func(t *testing.T) {
		source := t.TempDir()
		output := t.TempDir()
		path := filepath.Join(source, "a.txt")
		if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
			t.Fatal(err)
		}
		deps := defaultPrepareSourceDependencies()
		deps.beforeFileRevalidate = func(_ string) error {
			return os.WriteFile(path, []byte("mutated!"), 0o600)
		}
		safeOCITempRoot(t)
		prepared, err := preparePackageSourceWithDependencies(
			context.Background(), source, output, "", []string{"a.txt"}, deps)
		assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
		if prepared != nil {
			t.Fatal("mutated source produced a stage")
		}
	})
}

func TestPreparePackageSourceHardlinkAliasesBecomeIndependentCopies(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	first := filepath.Join(source, "a.txt")
	second := filepath.Join(source, "b.txt")
	if err := os.WriteFile(first, []byte("same"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, second); err != nil {
		t.Fatal(err)
	}
	safeOCITempRoot(t)
	prepared, err := preparePackageSource(
		context.Background(), source, output, "", []string{"b.txt", "a.txt"})
	if err != nil {
		t.Fatalf("preparePackageSource() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := prepared.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	aInfo, err := os.Stat(filepath.Join(prepared.dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	bInfo, err := os.Stat(filepath.Join(prepared.dir, "b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	srcInfo, err := os.Stat(first)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(aInfo, bInfo) || os.SameFile(aInfo, srcInfo) || os.SameFile(bInfo, srcInfo) {
		t.Fatal("staged hardlink aliases were not copied to distinct inodes")
	}
}

func TestPreparePackageSourceCancellationCleansStage(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	writeSourceFixture(t, source)
	tempRoot := safeOCITempRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	prepared, err := preparePackageSource(ctx, source, output, "", nil)
	assertErrorCode(t, err, apperrors.ErrCodeTimeout)
	if prepared != nil {
		t.Fatal("canceled preparation produced a stage")
	}
	assertDirectoryEmpty(t, tempRoot)
}

func TestPreparePackageSourceCancellationAtInjectedSeams(t *testing.T) {
	for _, tt := range []struct {
		name   string
		inject func(*prepareSourceDependencies, context.CancelFunc)
	}{
		{
			name: "before source open",
			inject: func(deps *prepareSourceDependencies, cancel context.CancelFunc) {
				deps.beforeSourceOpen = func(string) error { cancel(); return nil }
			},
		},
		{
			name: "before output open",
			inject: func(deps *prepareSourceDependencies, cancel context.CancelFunc) {
				deps.beforeOutputOpen = func(string) error { cancel(); return nil }
			},
		},
		{
			name: "before selected file open",
			inject: func(deps *prepareSourceDependencies, cancel context.CancelFunc) {
				deps.beforeFileOpen = func(string) error { cancel(); return nil }
			},
		},
		{
			name: "before selected file revalidation",
			inject: func(deps *prepareSourceDependencies, cancel context.CancelFunc) {
				deps.beforeFileRevalidate = func(string) error { cancel(); return nil }
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := t.TempDir()
			output := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("a"), 0o600); err != nil {
				t.Fatal(err)
			}
			tempRoot := safeOCITempRoot(t)
			ctx, cancel := context.WithCancel(context.Background())
			deps := defaultPrepareSourceDependencies()
			tt.inject(&deps, cancel)
			prepared, err := preparePackageSourceWithDependencies(
				ctx, source, output, "", []string{"a.txt"}, deps)
			assertErrorCode(t, err, apperrors.ErrCodeTimeout)
			if prepared != nil {
				t.Fatal("canceled seam returned prepared source")
			}
			assertDirectoryEmpty(t, tempRoot)
		})
	}
}

func TestNewPrivateWorkspaceValidationAndUniqueness(t *testing.T) {
	excluded := t.TempDir()
	tempRoot := safeOCITempRoot(t)
	first, err := NewPrivateWorkspace(context.Background(), "aicr-test-", excluded)
	if err != nil {
		t.Fatalf("first NewPrivateWorkspace() error = %v", err)
	}
	second, err := NewPrivateWorkspace(context.Background(), "aicr-test-", excluded)
	if err != nil {
		t.Fatalf("second NewPrivateWorkspace() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := first.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if closeErr := second.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if first.Path() == second.Path() {
		t.Fatal("private workspaces reused a path")
	}
	for _, workspace := range []*Workspace{first, second} {
		info, statErr := os.Stat(workspace.Path())
		if statErr != nil {
			t.Fatal(statErr)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("workspace mode = %o, want 700", got)
		}
		if !strings.HasPrefix(workspace.Path(), tempRoot+string(filepath.Separator)) {
			t.Errorf("workspace %q not beneath configured temp root %q", workspace.Path(), tempRoot)
		}
	}

	for _, tt := range []struct {
		name     string
		prefix   string
		excluded func(t *testing.T) []string
	}{
		{name: "empty prefix", prefix: "", excluded: func(t *testing.T) []string { return []string{t.TempDir()} }},
		{name: "unsafe prefix", prefix: "../bad", excluded: func(t *testing.T) []string { return []string{t.TempDir()} }},
		{name: "empty excluded root", prefix: "safe-", excluded: func(*testing.T) []string { return []string{""} }},
		{name: "missing excluded root", prefix: "safe-", excluded: func(t *testing.T) []string { return []string{filepath.Join(t.TempDir(), "missing")} }},
		{name: "symlink excluded root", prefix: "safe-", excluded: func(t *testing.T) []string {
			realRoot := t.TempDir()
			link := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(realRoot, link); err != nil {
				t.Fatal(err)
			}
			return []string{link}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			workspace, createErr := NewPrivateWorkspace(context.Background(), tt.prefix, tt.excluded(t)...)
			assertErrorCode(t, createErr, apperrors.ErrCodeInvalidRequest)
			if workspace != nil {
				t.Fatal("invalid workspace request returned a workspace")
			}
		})
	}
}

func TestNewPrivateWorkspaceRejectsUnsafeTempTopologyAndSwaps(t *testing.T) {
	for _, tt := range []struct {
		name string
		set  func(t *testing.T) (string, string)
	}{
		{
			name: "temp inside excluded root",
			set: func(t *testing.T) (string, string) {
				excluded := t.TempDir()
				temp := filepath.Join(excluded, "tmp")
				if err := os.Mkdir(temp, 0o700); err != nil {
					t.Fatal(err)
				}
				return excluded, temp
			},
		},
		{
			name: "temp symlink",
			set: func(t *testing.T) (string, string) {
				excluded := t.TempDir()
				realTemp := t.TempDir()
				link := filepath.Join(t.TempDir(), "tmp-link")
				if err := os.Symlink(realTemp, link); err != nil {
					t.Fatal(err)
				}
				return excluded, link
			},
		},
		{
			name: "temp missing",
			set: func(t *testing.T) (string, string) {
				return t.TempDir(), filepath.Join(t.TempDir(), "missing")
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			excluded, temp := tt.set(t)
			t.Setenv("TMPDIR", temp)
			workspace, err := NewPrivateWorkspace(context.Background(), "safe-", excluded)
			assertErrorCode(t, err, apperrors.ErrCodeInternal)
			if workspace != nil {
				t.Fatal("unsafe temp root returned workspace")
			}
		})
	}

	t.Run("configured temp swapped before open", func(t *testing.T) {
		parent := t.TempDir()
		temp := filepath.Join(parent, "temp")
		if err := os.Mkdir(temp, 0o700); err != nil {
			t.Fatal(err)
		}
		excluded := t.TempDir()
		t.Setenv("TMPDIR", temp)
		deps := defaultWorkspaceDependencies()
		deps.beforeTempOpen = func(path string) error {
			if err := os.Rename(path, path+"-moved"); err != nil {
				return err
			}
			return os.Mkdir(path, 0o700)
		}
		workspace, err := newPrivateWorkspaceWithDependencies(
			context.Background(), "safe-", deps, excluded)
		assertErrorCode(t, err, apperrors.ErrCodeInternal)
		if workspace != nil {
			t.Fatal("swapped temp root returned workspace")
		}
		assertDirectoryEmpty(t, temp)
	})

	t.Run("created child swapped before revalidation", func(t *testing.T) {
		temp := safeOCITempRoot(t)
		excluded := t.TempDir()
		deps := defaultWorkspaceDependencies()
		var replacement string
		deps.beforeChildRevalidate = func(parent, child string) error {
			original := filepath.Join(parent, child)
			if err := os.Rename(original, original+"-moved"); err != nil {
				return err
			}
			if err := os.Mkdir(original, 0o700); err != nil {
				return err
			}
			replacement = original
			return os.WriteFile(filepath.Join(original, "sentinel"), []byte("replacement"), 0o600)
		}
		workspace, err := newPrivateWorkspaceWithDependencies(
			context.Background(), "safe-", deps, excluded)
		assertErrorCode(t, err, apperrors.ErrCodeInternal)
		if workspace != nil {
			t.Fatal("swapped child returned workspace")
		}
		data, readErr := os.ReadFile(filepath.Join(replacement, "sentinel"))
		if readErr != nil || string(data) != "replacement" {
			t.Fatalf("replacement changed: data=%q err=%v", data, readErr)
		}
		entries, readDirErr := os.ReadDir(temp)
		if readDirErr != nil {
			t.Fatal(readDirErr)
		}
		if len(entries) != 2 {
			t.Fatalf("observed-swap limitation should leave moved original and replacement; entries=%v", entries)
		}
	})
}

func TestWorkspaceCloseCheckedIdempotentAndSwapSafe(t *testing.T) {
	t.Run("concurrent cached success", func(t *testing.T) {
		safeOCITempRoot(t)
		workspace, err := NewPrivateWorkspace(context.Background(), "safe-", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		const callers = 8
		results := make(chan error, callers)
		var wg sync.WaitGroup
		for range callers {
			wg.Add(1)
			go func() { defer wg.Done(); results <- workspace.Close() }()
		}
		wg.Wait()
		close(results)
		for closeErr := range results {
			if closeErr != nil {
				t.Errorf("Close() error = %v", closeErr)
			}
		}
		if _, statErr := os.Lstat(workspace.Path()); !os.IsNotExist(statErr) {
			t.Fatalf("workspace still exists after Close(): %v", statErr)
		}
	})

	t.Run("removal and descriptor errors cached", func(t *testing.T) {
		safeOCITempRoot(t)
		removeErr := stderrors.New("remove failed")
		closeErr := stderrors.New("root close failed")
		deps := defaultWorkspaceDependencies()
		deps.removeAll = func(*os.Root, string) error { return removeErr }
		var injectClose atomic.Bool
		deps.closeRoot = func(root *os.Root) error {
			err := root.Close()
			if injectClose.Load() {
				return stderrors.Join(err, closeErr)
			}
			return err
		}
		workspace, err := newPrivateWorkspaceWithDependencies(
			context.Background(), "safe-", deps, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		injectClose.Store(true)
		first := workspace.Close()
		if !stderrors.Is(first, removeErr) || !stderrors.Is(first, closeErr) {
			t.Fatalf("Close() = %v, want joined removal/close errors", first)
		}
		second := workspace.Close()
		if second == nil || second.Error() != first.Error() {
			t.Fatalf("second Close() = %v, want cached %v", second, first)
		}
		_ = os.RemoveAll(workspace.Path())
	})

	t.Run("observed name swap leaves replacement", func(t *testing.T) {
		safeOCITempRoot(t)
		deps := defaultWorkspaceDependencies()
		var replacement string
		deps.beforeRemove = func(parent, child string) error {
			original := filepath.Join(parent, child)
			if err := os.Rename(original, original+"-moved"); err != nil {
				return err
			}
			if err := os.Mkdir(original, 0o700); err != nil {
				return err
			}
			replacement = original
			return os.WriteFile(filepath.Join(original, "sentinel"), []byte("replacement"), 0o600)
		}
		workspace, err := newPrivateWorkspaceWithDependencies(
			context.Background(), "safe-", deps, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		closeResult := workspace.Close()
		assertErrorCode(t, closeResult, apperrors.ErrCodeInternal)
		data, readErr := os.ReadFile(filepath.Join(replacement, "sentinel"))
		if readErr != nil || string(data) != "replacement" {
			t.Fatalf("replacement changed: data=%q err=%v", data, readErr)
		}
	})
}

func TestOwnedLayoutUniqueCloseReleaseAndSwap(t *testing.T) {
	output := t.TempDir()
	first, err := newOwnedLayout(context.Background(), output)
	if err != nil {
		t.Fatal(err)
	}
	second, err := newOwnedLayout(context.Background(), output)
	if err != nil {
		t.Fatal(err)
	}
	if first.Path() == second.Path() {
		t.Fatal("owned layouts reused a path")
	}
	if validateErr := first.validate(); validateErr != nil {
		t.Fatalf("first.validate() = %v", validateErr)
	}
	releasedPath, err := first.release()
	if err != nil {
		t.Fatalf("release() = %v", err)
	}
	if releasedPath != first.Path() {
		t.Fatalf("release path = %q, want %q", releasedPath, first.Path())
	}
	if _, err := os.Stat(releasedPath); err != nil {
		t.Fatalf("released layout missing: %v", err)
	}
	if closeErr := second.Close(); closeErr != nil {
		t.Fatalf("second.Close() = %v", closeErr)
	}
	if _, err := os.Lstat(second.Path()); !os.IsNotExist(err) {
		t.Fatalf("closed layout still exists: %v", err)
	}

	t.Run("observed removal swap leaves replacement", func(t *testing.T) {
		deps := defaultOwnedLayoutDependencies()
		var replacement string
		deps.beforeRemove = func(parent, child string) error {
			original := filepath.Join(parent, child)
			if err := os.Rename(original, original+"-moved"); err != nil {
				return err
			}
			if err := os.Mkdir(original, 0o700); err != nil {
				return err
			}
			replacement = original
			return os.WriteFile(filepath.Join(original, "sentinel"), []byte("replacement"), 0o600)
		}
		layout, createErr := newOwnedLayoutWithDependencies(context.Background(), t.TempDir(), deps)
		if createErr != nil {
			t.Fatal(createErr)
		}
		closeErr := layout.Close()
		assertErrorCode(t, closeErr, apperrors.ErrCodeInternal)
		data, readErr := os.ReadFile(filepath.Join(replacement, "sentinel"))
		if readErr != nil || string(data) != "replacement" {
			t.Fatalf("replacement changed: data=%q err=%v", data, readErr)
		}
	})
}

func TestOwnedLayoutRejectsOutputAndCreatedChildSwaps(t *testing.T) {
	t.Run("output swap before open", func(t *testing.T) {
		parent := t.TempDir()
		output := filepath.Join(parent, "output")
		if err := os.Mkdir(output, 0o755); err != nil {
			t.Fatal(err)
		}
		deps := defaultOwnedLayoutDependencies()
		deps.beforeOutputOpen = func(path string) error {
			if err := os.Rename(path, path+"-moved"); err != nil {
				return err
			}
			return os.Mkdir(path, 0o755)
		}
		layout, err := newOwnedLayoutWithDependencies(context.Background(), output, deps)
		assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
		if layout != nil {
			t.Fatal("swapped output returned layout")
		}
		assertDirectoryEmpty(t, output)
	})

	t.Run("child swap before post-create revalidation", func(t *testing.T) {
		output := t.TempDir()
		deps := defaultOwnedLayoutDependencies()
		var replacement string
		deps.beforeChildRevalidate = func(parent, child string) error {
			original := filepath.Join(parent, child)
			if err := os.Rename(original, original+"-moved"); err != nil {
				return err
			}
			if err := os.Mkdir(original, 0o700); err != nil {
				return err
			}
			replacement = original
			return os.WriteFile(filepath.Join(original, "sentinel"), []byte("replacement"), 0o600)
		}
		layout, err := newOwnedLayoutWithDependencies(context.Background(), output, deps)
		assertErrorCode(t, err, apperrors.ErrCodeInternal)
		if layout != nil {
			t.Fatal("swapped child returned layout")
		}
		data, readErr := os.ReadFile(filepath.Join(replacement, "sentinel"))
		if readErr != nil || string(data) != "replacement" {
			t.Fatalf("replacement changed: data=%q err=%v", data, readErr)
		}
	})
}

func TestOwnedLayoutReleaseCloseFailureRetainsCleanupAccountability(t *testing.T) {
	output := t.TempDir()
	injected := stderrors.New("descriptor close failed")
	deps := defaultOwnedLayoutDependencies()
	var calls atomic.Int32
	deps.closeRoot = func(root *os.Root) error {
		err := root.Close()
		if calls.Add(1) == 1 {
			return stderrors.Join(err, injected)
		}
		return err
	}
	layout, err := newOwnedLayoutWithDependencies(context.Background(), output, deps)
	if err != nil {
		t.Fatal(err)
	}
	path := layout.Path()
	_, err = layout.release()
	assertErrorCode(t, err, apperrors.ErrCodeInternal)
	if !stderrors.Is(err, injected) {
		t.Fatalf("release error = %v, want %v", err, injected)
	}
	if closeErr := layout.Close(); closeErr != nil && !stderrors.Is(closeErr, injected) {
		t.Fatalf("Close() error = %v, want nil or retained release error", closeErr)
	}
	if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
		t.Fatalf("layout name remains after failed release cleanup: %v", statErr)
	}
}

func TestPreparedSourceCloseReturnsWorkspaceError(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	safeOCITempRoot(t)
	removeErr := stderrors.New("remove failed")
	workspaceDeps := defaultWorkspaceDependencies()
	workspaceDeps.removeAll = func(*os.Root, string) error { return removeErr }
	deps := defaultPrepareSourceDependencies()
	deps.newWorkspace = func(ctx context.Context, prefix string, excluded ...string) (*Workspace, error) {
		return newPrivateWorkspaceWithDependencies(ctx, prefix, workspaceDeps, excluded...)
	}
	prepared, err := preparePackageSourceWithDependencies(
		context.Background(), source, output, "", []string{"a.txt"}, deps)
	if err != nil {
		t.Fatal(err)
	}
	first := prepared.Close()
	if !stderrors.Is(first, removeErr) {
		t.Fatalf("Close() = %v, want %v", first, removeErr)
	}
	second := prepared.Close()
	if second == nil || second.Error() != first.Error() {
		t.Fatalf("cached Close() = %v, want %v", second, first)
	}
	_ = os.RemoveAll(prepared.dir)
}

func TestPreparePackageSourceRejectsRetainedWorkspacePathSwaps(t *testing.T) {
	for _, tt := range []struct {
		name string
		swap func(*testing.T, *Workspace) (replacement, moved string)
	}{
		{
			name: "child name",
			swap: func(t *testing.T, workspace *Workspace) (string, string) {
				t.Helper()
				original := workspace.Path()
				moved := original + "-moved"
				if err := os.Rename(original, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(original, 0o700); err != nil {
					t.Fatal(err)
				}
				return original, moved
			},
		},
		{
			name: "parent path",
			swap: func(t *testing.T, workspace *Workspace) (string, string) {
				t.Helper()
				movedParent := workspace.parentPath + "-moved"
				if err := os.Rename(workspace.parentPath, movedParent); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(workspace.parentPath, 0o700); err != nil {
					t.Fatal(err)
				}
				replacement := filepath.Join(workspace.parentPath, workspace.childName)
				if err := os.Mkdir(replacement, 0o700); err != nil {
					t.Fatal(err)
				}
				return replacement, movedParent
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := t.TempDir()
			output := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("alpha"), 0o600); err != nil {
				t.Fatal(err)
			}
			safeOCITempRoot(t)

			deps := defaultPrepareSourceDependencies()
			var replacement, moved string
			deps.newWorkspace = func(ctx context.Context, prefix string, excluded ...string) (*Workspace, error) {
				workspace, err := NewPrivateWorkspace(ctx, prefix, excluded...)
				if err != nil {
					return nil, err
				}
				replacement, moved = tt.swap(t, workspace)
				if err := os.WriteFile(filepath.Join(replacement, "sentinel"), []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
				return workspace, nil
			}

			prepared, err := preparePackageSourceWithDependencies(
				context.Background(), source, output, "", []string{"a.txt"}, deps)
			if prepared != nil {
				if closeErr := prepared.Close(); closeErr != nil &&
					!stderrors.Is(closeErr, apperrors.New(apperrors.ErrCodeInternal, "")) {

					t.Errorf("prepared.Close() error = %v", closeErr)
				}
			}
			got := directoryEntryNames(t, replacement)
			want := []string{"sentinel"}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("replacement entries = %v, want %v", got, want)
			}
			if data, readErr := os.ReadFile(filepath.Join(replacement, "sentinel")); readErr != nil || string(data) != "replacement" {
				t.Errorf("replacement sentinel changed: data=%q err=%v", data, readErr)
			}
			if prepared != nil {
				t.Errorf("preparePackageSourceWithDependencies() returned a stage after %s swap", tt.name)
			}
			assertErrorCode(t, err, apperrors.ErrCodeInternal)

			if removeErr := os.RemoveAll(replacement); removeErr != nil {
				t.Error(removeErr)
			}
			if removeErr := os.RemoveAll(moved); removeErr != nil {
				t.Error(removeErr)
			}
		})
	}
}

func TestOwnedLayoutRejectsRetainedPathSwapsBeforeArchiveOpen(t *testing.T) {
	for _, tt := range []struct {
		name string
		swap func(*testing.T, *ownedLayout) (replacement, moved string)
	}{
		{
			name: "child name",
			swap: func(t *testing.T, layout *ownedLayout) (string, string) {
				t.Helper()
				moved := layout.Path() + "-moved"
				if err := os.Rename(layout.Path(), moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(layout.Path(), 0o700); err != nil {
					t.Fatal(err)
				}
				return layout.Path(), moved
			},
		},
		{
			name: "parent path",
			swap: func(t *testing.T, layout *ownedLayout) (string, string) {
				t.Helper()
				movedParent := layout.parentPath + "-moved"
				if err := os.Rename(layout.parentPath, movedParent); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(layout.parentPath, 0o700); err != nil {
					t.Fatal(err)
				}
				replacement := filepath.Join(layout.parentPath, layout.childName)
				if err := os.Mkdir(replacement, 0o700); err != nil {
					t.Fatal(err)
				}
				return replacement, movedParent
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := t.TempDir()
			output := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("alpha"), 0o600); err != nil {
				t.Fatal(err)
			}
			safeOCITempRoot(t)
			prepared, err := preparePackageSource(
				context.Background(), source, output, "", []string{"a.txt"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if closeErr := prepared.Close(); closeErr != nil {
					t.Errorf("prepared.Close() error = %v", closeErr)
				}
			})

			layout, err := newOwnedLayout(context.Background(), output)
			if err != nil {
				t.Fatal(err)
			}
			replacement, moved := tt.swap(t, layout)
			if err := os.WriteFile(filepath.Join(replacement, "sentinel"), []byte("replacement"), 0o600); err != nil {
				t.Fatal(err)
			}

			archiveName, _, buildErr := buildDeterministicTarGzip(
				context.Background(), prepared, layout, archiveOptions{})
			if closeErr := layout.Close(); closeErr != nil &&
				!stderrors.Is(closeErr, apperrors.New(apperrors.ErrCodeInternal, "")) {

				t.Errorf("layout.Close() error = %v", closeErr)
			}
			got := directoryEntryNames(t, replacement)
			want := []string{"sentinel"}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("replacement entries = %v, want %v", got, want)
			}
			if data, readErr := os.ReadFile(filepath.Join(replacement, "sentinel")); readErr != nil || string(data) != "replacement" {
				t.Errorf("replacement sentinel changed: data=%q err=%v", data, readErr)
			}
			if archiveName != "" {
				t.Errorf("archive name = %q after %s swap, want empty", archiveName, tt.name)
			}
			assertErrorCode(t, buildErr, apperrors.ErrCodeInternal)

			if removeErr := os.RemoveAll(replacement); removeErr != nil {
				t.Error(removeErr)
			}
			if removeErr := os.RemoveAll(moved); removeErr != nil {
				t.Error(removeErr)
			}
		})
	}
}

func TestPreparePackageSourceRejectsRootSwapsAtWorkspaceBoundary(t *testing.T) {
	for _, rootToSwap := range []string{"source", "output"} {
		t.Run(rootToSwap, func(t *testing.T) {
			parent := t.TempDir()
			source := filepath.Join(parent, "source")
			output := filepath.Join(parent, "output")
			if err := os.Mkdir(source, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(output, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("alpha"), 0o600); err != nil {
				t.Fatal(err)
			}
			safeOCITempRoot(t)

			var replacement string
			deps := defaultPrepareSourceDependencies()
			deps.newWorkspace = func(ctx context.Context, prefix string, excluded ...string) (*Workspace, error) {
				replacement = source
				if rootToSwap == "output" {
					replacement = output
				}
				if err := os.Rename(replacement, replacement+"-moved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(replacement, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(replacement, "sentinel"), []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
				return NewPrivateWorkspace(ctx, prefix, excluded...)
			}

			prepared, err := preparePackageSourceWithDependencies(
				context.Background(), source, output, "", []string{"a.txt"}, deps)
			if prepared != nil {
				if closeErr := prepared.Close(); closeErr != nil {
					t.Errorf("prepared.Close() error = %v", closeErr)
				}
			}
			got := snapshotTree(t, replacement)
			want := []string{"sentinel|-rw-------|replacement"}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("replacement mutated: got %v, want %v", got, want)
			}
			if prepared != nil {
				t.Errorf("phase-boundary %s swap returned a prepared source", rootToSwap)
			}
			assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
		})
	}
}

func TestWorkspaceConstructorsCheckPostCreateCleanup(t *testing.T) {
	for _, tt := range []struct {
		name string
		run  func(*testing.T, error, error) (string, bool, error)
	}{
		{
			name: "private workspace",
			run: func(t *testing.T, primary, cleanup error) (string, bool, error) {
				t.Helper()
				temp := safeOCITempRoot(t)
				deps := defaultWorkspaceDependencies()
				var childPath string
				deps.beforeChildRevalidate = func(parent, child string) error {
					childPath = filepath.Join(parent, child)
					return primary
				}
				deps.removeAll = func(root *os.Root, name string) error {
					return stderrors.Join(root.RemoveAll(name), cleanup)
				}
				workspace, err := newPrivateWorkspaceWithDependencies(
					context.Background(), "safe-", deps, t.TempDir())
				if workspace != nil {
					t.Errorf("constructor returned workspace on post-create failure")
					workspace.deps = defaultWorkspaceDependencies()
					if closeErr := workspace.Close(); closeErr != nil {
						t.Errorf("workspace.Close() error = %v", closeErr)
					}
				}
				if childPath == "" {
					t.Fatal("post-create hook was not called")
				}
				_, statErr := os.Lstat(childPath)
				residue := statErr == nil
				if statErr != nil && !os.IsNotExist(statErr) {
					t.Errorf("Lstat(%q): %v", childPath, statErr)
				}
				if removeErr := os.RemoveAll(childPath); removeErr != nil {
					t.Error(removeErr)
				}
				assertDirectoryEmpty(t, temp)
				return childPath, residue, err
			},
		},
		{
			name: "owned layout",
			run: func(t *testing.T, primary, cleanup error) (string, bool, error) {
				t.Helper()
				output := t.TempDir()
				deps := defaultOwnedLayoutDependencies()
				var childPath string
				deps.beforeChildRevalidate = func(parent, child string) error {
					childPath = filepath.Join(parent, child)
					return primary
				}
				deps.removeAll = func(root *os.Root, name string) error {
					return stderrors.Join(root.RemoveAll(name), cleanup)
				}
				layout, err := newOwnedLayoutWithDependencies(context.Background(), output, deps)
				if layout != nil {
					t.Errorf("constructor returned layout on post-create failure")
					layout.deps = defaultOwnedLayoutDependencies()
					if closeErr := layout.Close(); closeErr != nil {
						t.Errorf("layout.Close() error = %v", closeErr)
					}
				}
				if childPath == "" {
					t.Fatal("post-create hook was not called")
				}
				_, statErr := os.Lstat(childPath)
				residue := statErr == nil
				if statErr != nil && !os.IsNotExist(statErr) {
					t.Errorf("Lstat(%q): %v", childPath, statErr)
				}
				if removeErr := os.RemoveAll(childPath); removeErr != nil {
					t.Error(removeErr)
				}
				assertDirectoryEmpty(t, output)
				return childPath, residue, err
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			primary := stderrors.New("post-create failure")
			cleanup := stderrors.New("checked cleanup failure")
			childPath, residue, err := tt.run(t, primary, cleanup)
			if residue {
				t.Errorf("constructor left unchanged child %q behind", childPath)
			}
			if !stderrors.Is(err, primary) || !stderrors.Is(err, cleanup) {
				t.Fatalf("constructor error = %v, want primary %v and cleanup %v for %q",
					err, primary, cleanup, childPath)
			}
			assertErrorCode(t, err, apperrors.ErrCodeInternal)
		})
	}
}

func TestWorkspaceConstructorsCheckChmodFailureCleanup(t *testing.T) {
	for _, kind := range []string{"private workspace", "owned layout"} {
		t.Run(kind, func(t *testing.T) {
			primary := stderrors.New("child chmod failed")
			cleanup := stderrors.New("child cleanup failed")
			var err error
			var root string
			if kind == "private workspace" {
				root = safeOCITempRoot(t)
				deps := defaultWorkspaceDependencies()
				deps.chmodChild = func(*os.Root) error { return primary }
				deps.removeAll = func(parent *os.Root, name string) error {
					return stderrors.Join(parent.RemoveAll(name), cleanup)
				}
				workspace, createErr := newPrivateWorkspaceWithDependencies(
					context.Background(), "safe-", deps, t.TempDir())
				if workspace != nil {
					t.Fatal("chmod failure returned workspace")
				}
				err = createErr
			} else {
				root = t.TempDir()
				deps := defaultOwnedLayoutDependencies()
				deps.chmodChild = func(*os.Root) error { return primary }
				deps.removeAll = func(parent *os.Root, name string) error {
					return stderrors.Join(parent.RemoveAll(name), cleanup)
				}
				layout, createErr := newOwnedLayoutWithDependencies(context.Background(), root, deps)
				if layout != nil {
					t.Fatal("chmod failure returned layout")
				}
				err = createErr
			}
			assertErrorCode(t, err, apperrors.ErrCodeInternal)
			if !stderrors.Is(err, primary) || !stderrors.Is(err, cleanup) {
				t.Fatalf("error = %v, want primary %v and cleanup %v", err, primary, cleanup)
			}
			assertDirectoryEmpty(t, root)
		})
	}
}

func TestNewPrivateWorkspaceChecksExcludedRootClose(t *testing.T) {
	temp := safeOCITempRoot(t)
	injected := stderrors.New("excluded root close failed")
	deps := defaultWorkspaceDependencies()
	var closeCalls atomic.Int32
	deps.closeRoot = func(root *os.Root) error {
		err := root.Close()
		if closeCalls.Add(1) == 1 {
			return stderrors.Join(err, injected)
		}
		return err
	}
	workspace, err := newPrivateWorkspaceWithDependencies(
		context.Background(), "safe-", deps, t.TempDir())
	if workspace != nil {
		workspace.deps = defaultWorkspaceDependencies()
		if closeErr := workspace.Close(); closeErr != nil {
			t.Errorf("workspace.Close() error = %v", closeErr)
		}
		t.Errorf("constructor returned workspace after excluded-root close failure")
	}
	if closeCalls.Load() == 0 {
		t.Error("injected closeRoot was not used for excluded roots")
	}
	if !stderrors.Is(err, injected) {
		t.Fatalf("constructor error = %v, want %v", err, injected)
	}
	assertErrorCode(t, err, apperrors.ErrCodeInternal)
	assertDirectoryEmpty(t, temp)
}

func TestPackageGenericOperationCleanupFailureClearsOwnedResult(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	safeOCITempRoot(t)
	injected := stderrors.New("prepared source cleanup failed")
	workspaceDeps := defaultWorkspaceDependencies()
	workspaceDeps.removeAll = func(root *os.Root, name string) error {
		return stderrors.Join(root.RemoveAll(name), injected)
	}
	prepareDeps := defaultPrepareSourceDependencies()
	prepareDeps.newWorkspace = func(ctx context.Context, prefix string, excluded ...string) (*Workspace, error) {
		return newPrivateWorkspaceWithDependencies(ctx, prefix, workspaceDeps, excluded...)
	}
	prepared, err := preparePackageSourceWithDependencies(
		context.Background(), source, output, "", []string{"a.txt"}, prepareDeps)
	if err != nil {
		t.Fatal(err)
	}

	deps := defaultGenericPackageDependencies()
	deps.prepareSource = func(context.Context, string, string, string, []string) (*preparedSource, error) {
		return prepared, nil
	}
	var layoutPath string
	deps.newLayout = func(ctx context.Context, dir string) (*ownedLayout, error) {
		layout, createErr := newOwnedLayout(ctx, dir)
		if layout != nil {
			layoutPath = layout.Path()
		}
		return layout, createErr
	}
	op, err := packageGenericOperationWithDependencies(context.Background(), PackageOptions{
		SourceDir: source, OutputDir: output, SourceFiles: []string{"a.txt"},
		Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
	}, deps)
	leakedBeforeCallerCleanup := false
	if layoutPath != "" {
		_, statErr := os.Lstat(layoutPath)
		leakedBeforeCallerCleanup = statErr == nil
	}
	if op != nil {
		if closeErr := op.Close(); closeErr != nil {
			t.Errorf("leaked operation Close() error = %v", closeErr)
		}
	}
	if !stderrors.Is(err, injected) {
		t.Fatalf("operation error = %v, want %v", err, injected)
	}
	assertErrorCode(t, err, apperrors.ErrCodeInternal)
	if op != nil {
		t.Error("cleanup-only failure returned a non-nil live operation")
	}
	if leakedBeforeCallerCleanup {
		t.Errorf("cleanup-only failure left owned layout %q live", layoutPath)
	}
	if layoutPath != "" {
		if _, statErr := os.Lstat(layoutPath); !os.IsNotExist(statErr) {
			t.Errorf("layout remains after test cleanup: %v", statErr)
		}
	}
	if removeErr := os.RemoveAll(prepared.dir); removeErr != nil {
		t.Error(removeErr)
	}
}

func TestOwnedLayoutReleaseCloseFailuresCleanAndCache(t *testing.T) {
	for _, tt := range []struct {
		name     string
		failCall int32
	}{
		{name: "child close", failCall: 1},
		{name: "parent close", failCall: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			output := t.TempDir()
			injected := stderrors.New(tt.name + " failed")
			deps := defaultOwnedLayoutDependencies()
			var closeCalls atomic.Int32
			var removeCalls atomic.Int32
			deps.closeRoot = func(root *os.Root) error {
				err := root.Close()
				if closeCalls.Add(1) == tt.failCall {
					return stderrors.Join(err, injected)
				}
				return err
			}
			deps.removeAll = func(root *os.Root, name string) error {
				removeCalls.Add(1)
				return root.RemoveAll(name)
			}
			layout, err := newOwnedLayoutWithDependencies(context.Background(), output, deps)
			if err != nil {
				t.Fatal(err)
			}
			path := layout.Path()
			_, first := layout.release()
			if !stderrors.Is(first, injected) {
				t.Fatalf("release() error = %v, want %v", first, injected)
			}
			assertErrorCode(t, first, apperrors.ErrCodeInternal)
			callsAfterRelease := removeCalls.Load()
			second := layout.Close()
			if second == nil || second.Error() != first.Error() {
				t.Errorf("Close() = %v, want cached %v", second, first)
			}
			if removeCalls.Load() != callsAfterRelease {
				t.Errorf("repeated Close() retried removal: before=%d after=%d",
					callsAfterRelease, removeCalls.Load())
			}
			if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
				t.Errorf("failed release left owned layout %q: %v", path, statErr)
				if removeErr := os.RemoveAll(path); removeErr != nil {
					t.Error(removeErr)
				}
			}
		})
	}
}

func TestPreparePackageSourceMidStreamCancellationReturnsTimeout(t *testing.T) {
	for _, tt := range []struct {
		name string
		arm  func(*prepareSourceDependencies, *cancelAfterChecksContext)
	}{
		{
			name: "first copy hash",
			arm: func(deps *prepareSourceDependencies, ctx *cancelAfterChecksContext) {
				deps.beforeFileOpen = func(string) error { ctx.arm(3); return nil }
			},
		},
		{
			name: "second verification hash",
			arm: func(deps *prepareSourceDependencies, ctx *cancelAfterChecksContext) {
				deps.beforeFileRevalidate = func(string) error { ctx.arm(3); return nil }
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := t.TempDir()
			output := t.TempDir()
			content := make([]byte, 3*copyBufferSize+1)
			for i := range content {
				content[i] = byte(i % 251)
			}
			if err := os.WriteFile(filepath.Join(source, "large.bin"), content, 0o600); err != nil {
				t.Fatal(err)
			}
			temp := safeOCITempRoot(t)
			ctx := newCancelAfterChecksContext()
			deps := defaultPrepareSourceDependencies()
			tt.arm(&deps, ctx)
			prepared, err := preparePackageSourceWithDependencies(
				ctx, source, output, "", []string{"large.bin"}, deps)
			if prepared != nil {
				if closeErr := prepared.Close(); closeErr != nil {
					t.Errorf("prepared.Close() error = %v", closeErr)
				}
				t.Errorf("mid-stream cancellation returned prepared source")
			}
			assertErrorCode(t, err, apperrors.ErrCodeTimeout)
			assertDirectoryEmpty(t, temp)
		})
	}
}

type cancelAfterChecksContext struct {
	context.Context
	done      chan struct{}
	remaining atomic.Int32
	armed     atomic.Bool
	cancel    sync.Once
}

func newCancelAfterChecksContext() *cancelAfterChecksContext {
	return &cancelAfterChecksContext{Context: context.Background(), done: make(chan struct{})}
}

func (c *cancelAfterChecksContext) arm(checks int32) {
	c.remaining.Store(checks)
	c.armed.Store(true)
}

func (c *cancelAfterChecksContext) Done() <-chan struct{} {
	return c.done
}

func (c *cancelAfterChecksContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
	}
	if !c.armed.Load() {
		return c.Context.Err()
	}
	if c.remaining.Add(-1) > 0 {
		return nil
	}
	c.cancel.Do(func() { close(c.done) })
	return context.Canceled
}

func directoryEntryNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", root, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func snapshotTree(t *testing.T, root string) []string {
	t.Helper()
	var result []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		result = append(result, filepath.ToSlash(rel)+"|"+info.Mode().String()+"|"+string(mustReadRegular(t, path, info)))
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotTree(%q): %v", root, err)
	}
	sort.Strings(result)
	return result
}

func mustReadRegular(t *testing.T, path string, info fs.FileInfo) []byte {
	t.Helper()
	if !info.Mode().IsRegular() {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	return data
}

func assertDirectoryEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", path, err)
	}
	if len(entries) != 0 {
		t.Fatalf("directory %q has residue: %v", path, entries)
	}
}
