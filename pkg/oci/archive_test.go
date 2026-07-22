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
	"archive/tar"
	"compress/gzip"
	"context"
	stderrors "errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/opencontainers/go-digest"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content/file"

	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

func TestPackage_SourceFilesArchiveContainsOnlyFrozenSortedSelection(t *testing.T) {
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	writeSourceFixture(t, sourceDir)
	safeOCITempRoot(t)
	prepared, err := preparePackageSource(
		context.Background(), sourceDir, outputDir, "", []string{"nested/deep/c.sh", "a.txt"})
	if err != nil {
		t.Fatalf("preparePackageSource() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := prepared.Close(); closeErr != nil {
			t.Errorf("prepared.Close() error = %v", closeErr)
		}
	})

	// The archive must consume only the staged snapshot. A later caller-tree
	// mutation cannot change the frozen bytes or add content.
	if writeErr := os.WriteFile(filepath.Join(sourceDir, "a.txt"), []byte("mutated"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if writeErr := os.WriteFile(filepath.Join(sourceDir, "late.txt"), []byte("late"), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}

	layout := newTestOwnedLayout(t, outputDir)
	archiveName, desc, err := buildDeterministicTarGzip(
		context.Background(), prepared, layout, archiveOptions{})
	if err != nil {
		t.Fatalf("buildDeterministicTarGzip() error = %v", err)
	}
	archivePath := filepath.Join(layout.Path(), archiveName)
	if desc.MediaType != ociv1.MediaTypeImageLayerGzip {
		t.Errorf("descriptor MediaType = %q, want %q", desc.MediaType, ociv1.MediaTypeImageLayerGzip)
	}
	data, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	if desc.Size != int64(len(data)) || desc.Digest != digest.FromBytes(data) {
		t.Fatalf("descriptor = %+v, archive bytes size=%d digest=%s", desc, len(data), digest.FromBytes(data))
	}
	files, entries := readArchiveFiles(t, archivePath)
	wantEntries := []string{"a.txt", "nested/", "nested/deep/", "nested/deep/c.sh"}
	if !reflect.DeepEqual(entries, wantEntries) {
		t.Fatalf("archive entries = %v, want %v", entries, wantEntries)
	}
	if files["a.txt"] != "alpha" || files["nested/deep/c.sh"] != "#!/bin/sh\necho c\n" {
		t.Fatalf("archive files = %v, want frozen source bytes", files)
	}
	if _, ok := files["other.txt"]; ok {
		t.Fatal("archive included an unselected file")
	}
}

// TestBuildDeterministicTarGzipSetsFileStoreUnpackTitle locks the two layer
// annotations an ORAS file store needs to MATERIALIZE the layer to disk on
// pull. oras.Copy into a file.New(dir) store unpacks a layer into the path
// named by org.opencontainers.image.title (here ".", the store root) when
// io.deis.oras.content.unpack is "true"; WITHOUT the title annotation the
// store treats the layer as an unnamed blob and writes NOTHING to disk, so a
// downstream file-store pull (e.g. the evidence verifier's materialize step)
// sees an empty directory. Regression guard for the generic archive path —
// dropping the title annotation broke every `main` UAT verify (issue #1793).
func TestBuildDeterministicTarGzipSetsFileStoreUnpackTitle(t *testing.T) {
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	writeSourceFixture(t, sourceDir)
	safeOCITempRoot(t)
	prepared, err := preparePackageSource(context.Background(), sourceDir, outputDir, "", nil)
	if err != nil {
		t.Fatalf("preparePackageSource() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := prepared.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	layout := newTestOwnedLayout(t, outputDir)
	_, desc, err := buildDeterministicTarGzip(context.Background(), prepared, layout, archiveOptions{})
	if err != nil {
		t.Fatalf("buildDeterministicTarGzip() error = %v", err)
	}
	if got := desc.Annotations[ociv1.AnnotationTitle]; got != "." {
		t.Errorf("layer %q annotation = %q, want %q — an ORAS file store unpacks into this path; "+
			"empty means the layer is never written to disk on pull", ociv1.AnnotationTitle, got, ".")
	}
	if got := desc.Annotations[file.AnnotationUnpack]; got != "true" {
		t.Errorf("layer %q annotation = %q, want \"true\"", file.AnnotationUnpack, got)
	}
}

func TestPackage_SourceFilesArchivePreservesSubDirPrefix(t *testing.T) {
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	writeSourceFixture(t, sourceDir)
	safeOCITempRoot(t)
	prepared, err := preparePackageSource(context.Background(), sourceDir, outputDir, "nested", nil)
	if err != nil {
		t.Fatalf("preparePackageSource() error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := prepared.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	layout := newTestOwnedLayout(t, outputDir)
	archiveName, _, err := buildDeterministicTarGzip(
		context.Background(), prepared, layout, archiveOptions{})
	if err != nil {
		t.Fatalf("buildDeterministicTarGzip() error = %v", err)
	}
	archivePath := filepath.Join(layout.Path(), archiveName)
	_, entries := readArchiveFiles(t, archivePath)
	want := []string{"nested/", "nested/b.txt", "nested/deep/", "nested/deep/c.sh"}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("archive entries = %v, want %v", entries, want)
	}
}

func TestPackage_SourceFilesArchiveCancellationAndCloseFailure(t *testing.T) {
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "a.txt"), []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	safeOCITempRoot(t)
	prepared, err := preparePackageSource(
		context.Background(), sourceDir, outputDir, "", []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prepared.Close() })

	t.Run("pre-canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		layout := newTestOwnedLayout(t, t.TempDir())
		path, desc, buildErr := buildDeterministicTarGzip(ctx, prepared, layout, archiveOptions{})
		assertErrorCode(t, buildErr, apperrors.ErrCodeTimeout)
		if path != "" || !reflect.DeepEqual(desc, ociv1.Descriptor{}) {
			t.Fatalf("canceled build = (%q, %+v), want empty results", path, desc)
		}
		assertDirectoryEmpty(t, layout.Path())
	})

	t.Run("checked archive close", func(t *testing.T) {
		closeErr := stderrors.New("archive close failed")
		deps := defaultArchiveDependencies()
		deps.closeFile = func(file *os.File) error {
			return stderrors.Join(file.Close(), closeErr)
		}
		layout := newTestOwnedLayout(t, t.TempDir())
		path, desc, buildErr := buildDeterministicTarGzipWithDependencies(
			context.Background(), prepared, layout, archiveOptions{}, deps)
		assertErrorCode(t, buildErr, apperrors.ErrCodeInternal)
		if !stderrors.Is(buildErr, closeErr) {
			t.Fatalf("build error = %v, want close cause %v", buildErr, closeErr)
		}
		if path != "" || !reflect.DeepEqual(desc, ociv1.Descriptor{}) {
			t.Fatalf("failed build = (%q, %+v), want empty results", path, desc)
		}
		assertDirectoryEmpty(t, layout.Path())
	})
}

func TestBuildDeterministicTarGzipRejectsOwnedLayoutParentSwapBeforeMutation(t *testing.T) {
	sourceDir := t.TempDir()
	preparedOutput := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "a.txt"), []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	safeOCITempRoot(t)
	prepared, prepareErr := preparePackageSource(
		context.Background(), sourceDir, preparedOutput, "", []string{"a.txt"})
	if prepareErr != nil {
		t.Fatal(prepareErr)
	}
	t.Cleanup(func() { _ = prepared.Close() })

	container := t.TempDir()
	output := filepath.Join(container, "output")
	if err := os.Mkdir(output, 0o755); err != nil {
		t.Fatal(err)
	}
	layout, layoutErr := newOwnedLayout(context.Background(), output)
	if layoutErr != nil {
		t.Fatal(layoutErr)
	}
	t.Cleanup(func() { _ = layout.Close() })
	movedOutput := output + "-moved"
	if err := os.Rename(output, movedOutput); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(output, 0o755); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(output, layout.childName)
	if err := os.Mkdir(replacement, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(replacement, "sentinel"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	archiveName, descriptor, buildErr := buildDeterministicTarGzip(
		context.Background(), prepared, layout, archiveOptions{})
	assertErrorCode(t, buildErr, apperrors.ErrCodeInternal)
	if archiveName != "" || !reflect.DeepEqual(descriptor, ociv1.Descriptor{}) {
		t.Fatalf("swapped layout returned archive = (%q, %+v)", archiveName, descriptor)
	}
	entries, readErr := os.ReadDir(replacement)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 1 || entries[0].Name() != "sentinel" {
		t.Fatalf("replacement mutated: entries=%v", entries)
	}
}

func TestBuildDeterministicTarGzipMidStreamCancellationReturnsTimeout(t *testing.T) {
	for _, phase := range []string{"archive writer", "descriptor verification reader"} {
		t.Run(phase, func(t *testing.T) {
			sourceDir := t.TempDir()
			outputDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(sourceDir, "a.txt"), []byte("alpha"), 0o600); err != nil {
				t.Fatal(err)
			}
			safeOCITempRoot(t)
			prepared, err := preparePackageSource(
				context.Background(), sourceDir, outputDir, "", []string{"a.txt"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = prepared.Close() })
			layout := newTestOwnedLayout(t, outputDir)

			ctx, cancel := context.WithCancel(context.Background())
			started := make(chan struct{})
			deps := defaultArchiveDependencies()
			if phase == "archive writer" {
				deps.wrapWriter = func(ctx context.Context, _ io.Writer) io.Writer {
					return &cancellationBlockingWriter{ctx: ctx, started: started}
				}
			} else {
				deps.openVerify = func(ctx context.Context, root *os.Root, name string) (io.ReadCloser, error) {
					file, openErr := root.Open(name)
					if openErr != nil {
						return nil, openErr
					}
					return &cancellationBlockingReader{ctx: ctx, file: file, started: started}, nil
				}
			}
			type buildResult struct {
				name string
				desc ociv1.Descriptor
				err  error
			}
			result := make(chan buildResult, 1)
			go func() {
				name, desc, buildErr := buildDeterministicTarGzipWithDependencies(
					ctx, prepared, layout, archiveOptions{}, deps)
				result <- buildResult{name: name, desc: desc, err: buildErr}
			}()
			<-started
			cancel()
			got := <-result
			assertErrorCode(t, got.err, apperrors.ErrCodeTimeout)
			if got.name != "" || !reflect.DeepEqual(got.desc, ociv1.Descriptor{}) {
				t.Fatalf("canceled build = (%q, %+v), want empty result", got.name, got.desc)
			}
			assertDirectoryEmpty(t, layout.Path())
		})
	}
}

type cancellationBlockingWriter struct {
	ctx     context.Context
	started chan struct{}
	once    sync.Once
}

func (w *cancellationBlockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.ctx.Done()
	return 0, w.ctx.Err()
}

type cancellationBlockingReader struct {
	ctx     context.Context
	file    *os.File
	started chan struct{}
	once    sync.Once
}

func (r *cancellationBlockingReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func (r *cancellationBlockingReader) Close() error { return r.file.Close() }

func newTestOwnedLayout(t *testing.T, output string) *ownedLayout {
	t.Helper()
	layout, err := newOwnedLayout(context.Background(), output)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if closeErr := layout.Close(); closeErr != nil {
			t.Errorf("layout.Close() error = %v", closeErr)
		}
	})
	return layout
}

func readArchiveFiles(t *testing.T, archivePath string) (map[string]string, []string) {
	t.Helper()
	file, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gzipReader.Close() }()
	tarReader := tar.NewReader(gzipReader)
	files := make(map[string]string)
	var entries []string
	for {
		header, nextErr := tarReader.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		entries = append(entries, header.Name)
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, readErr := io.ReadAll(tarReader)
		if readErr != nil {
			t.Fatal(readErr)
		}
		files[strings.TrimPrefix(header.Name, "./")] = string(data)
	}
	return files, entries
}
