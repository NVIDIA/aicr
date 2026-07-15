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
	"bytes"
	"compress/gzip"
	"context"
	stderrors "errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"

	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

const (
	archiveHelperEnv    = "AICR_ARCHIVE_CHECKPOINT_C_HELPER"
	archiveHelperRoot   = "AICR_ARCHIVE_CHECKPOINT_C_ROOT"
	archiveHelperOutput = "AICR_ARCHIVE_CHECKPOINT_C_OUTPUT"
	archiveHelperPrefix = "AICR_ARCHIVE_CHECKPOINT_C_PREFIX"
	archiveHelperUmask  = "AICR_ARCHIVE_CHECKPOINT_C_UMASK"
)

func TestBuildDeterministicTarGzipStableAcrossOrderMtimeAndUmask(t *testing.T) {
	for _, prefix := range []string{"", "aicr-bundle"} {
		t.Run("prefix="+prefix, func(t *testing.T) {
			archives := make([][]byte, 0, 2)
			for i, mask := range []int{0o022, 0o077} {
				root := filepath.Join(t.TempDir(), "run-"+strconv.Itoa(i))
				if err := os.MkdirAll(root, 0o700); err != nil {
					t.Fatal(err)
				}
				output := filepath.Join(root, "archive.tgz")
				cmd := exec.Command(os.Args[0], "-test.run=^TestBuildDeterministicTarGzipSubprocessHelper$")
				cmd.Env = append(os.Environ(),
					archiveHelperEnv+"=1",
					archiveHelperRoot+"="+root,
					archiveHelperOutput+"="+output,
					archiveHelperPrefix+"="+prefix,
					archiveHelperUmask+"="+strconv.Itoa(mask),
				)
				if combined, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("archive helper umask %#o: %v: %s", mask, err, combined)
				}
				data, err := os.ReadFile(output)
				if err != nil {
					t.Fatal(err)
				}
				archives = append(archives, data)
			}
			if !bytes.Equal(archives[0], archives[1]) {
				t.Fatalf("archive bytes differ across umask/order/mtime: %s vs %s",
					digest.FromBytes(archives[0]), digest.FromBytes(archives[1]))
			}
			assertCanonicalArchiveBytes(t, archives[0], prefix)
		})
	}
}

func TestBuildDeterministicTarGzipSubprocessHelper(t *testing.T) {
	if os.Getenv(archiveHelperEnv) != "1" {
		return
	}
	mask, parseErr := strconv.Atoi(os.Getenv(archiveHelperUmask))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	oldMask := syscall.Umask(mask)
	defer syscall.Umask(oldMask)
	root := os.Getenv(archiveHelperRoot)
	source := filepath.Join(root, "source")
	output := filepath.Join(root, "output")
	tempRoot := filepath.Join(root, "temp")
	for _, dir := range []string{source, output, tempRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMPDIR", tempRoot)
	longName := "templates/" + strings.Repeat("界", 60) + ".yaml"
	files := []struct {
		name string
		body string
		mode os.FileMode
	}{
		{name: "Chart.yaml", body: "apiVersion: v2\nname: aicr-bundle\nversion: 1.2.3\n", mode: 0o640},
		{name: longName, body: "kind: ConfigMap\n", mode: 0o600},
		{name: "templates/run.sh", body: "#!/bin/sh\nexit 0\n", mode: 0o750},
		{name: "values.yaml", body: "enabled: true\n", mode: 0o640},
	}
	for i, file := range files {
		full := filepath.Join(source, filepath.FromSlash(file.name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(file.body), 0o666); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(full, file.mode); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(int64(100+i*100+mask), 0)
		if err := os.Chtimes(full, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	selection := []string{"values.yaml", "templates/run.sh", longName, "Chart.yaml"}
	if mask == 0o077 {
		sort.Strings(selection)
	}
	prepared, err := preparePackageSource(context.Background(), source, output, "", selection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := prepared.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	prepared.files = append([]string(nil), selection...)
	layout, err := newOwnedLayout(context.Background(), output)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := layout.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}()
	archiveName, descriptor, err := buildDeterministicTarGzip(
		context.Background(), prepared, layout, archiveOptions{Prefix: os.Getenv(archiveHelperPrefix)})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(layout.Path(), archiveName))
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Size != int64(len(data)) || descriptor.Digest != digest.FromBytes(data) {
		t.Fatalf("descriptor mismatch: %+v", descriptor)
	}
	if err := os.WriteFile(os.Getenv(archiveHelperOutput), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertCanonicalArchiveBytes(t *testing.T, data []byte, prefix string) {
	t.Helper()
	if len(data) < 10 {
		t.Fatalf("gzip data too short: %d", len(data))
	}
	if data[3] != 0 || data[4] != 0 || data[5] != 0 || data[6] != 0 || data[7] != 0 {
		t.Fatalf("gzip flags/mtime are not canonical: %v", data[:10])
	}
	if data[8] != 2 || data[9] != 255 {
		t.Fatalf("gzip XFL/OS = %d/%d, want 2/255", data[8], data[9])
	}
	gzipReader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gzipReader.Close() }()
	if !gzipReader.ModTime.IsZero() || gzipReader.Name != "" || gzipReader.Comment != "" || len(gzipReader.Extra) != 0 {
		t.Fatalf("gzip header contains ambient metadata: %+v", gzipReader.Header)
	}

	tReader := tar.NewReader(gzipReader)
	var names []string
	longPAXSeen := false
	wantFileModes := map[string]int64{
		"Chart.yaml": 0o640,
		"templates/" + strings.Repeat("界", 60) + ".yaml": 0o600,
		"templates/run.sh": 0o750,
		"values.yaml":      0o640,
	}
	for {
		header, nextErr := tReader.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			t.Fatal(nextErr)
		}
		names = append(names, header.Name)
		if header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" {
			t.Errorf("entry %q has owner metadata: %+v", header.Name, header)
		}
		if !archiveTimeIsZero(header.ModTime) || !archiveTimeIsZero(header.AccessTime) || !archiveTimeIsZero(header.ChangeTime) {
			t.Errorf("entry %q has timestamp metadata: %+v", header.Name, header)
		}
		if header.Typeflag == tar.TypeDir && header.Mode != 0o755 {
			t.Errorf("directory %q mode = %#o", header.Name, header.Mode)
		}
		if header.Typeflag == tar.TypeReg {
			rel := header.Name
			if prefix != "" {
				rel = strings.TrimPrefix(rel, prefix+"/")
			}
			wantMode, ok := wantFileModes[rel]
			if !ok {
				t.Errorf("unexpected regular file %q", header.Name)
			} else if header.Mode != wantMode {
				t.Errorf("file %q mode = %#o, want preserved %#o", header.Name, header.Mode, wantMode)
			}
		}
		if header.Format != tar.FormatUSTAR && header.Format != tar.FormatPAX {
			t.Errorf("entry %q format = %v, want canonical USTAR or PAX", header.Name, header.Format)
		}
		for key := range header.PAXRecords {
			if key != "path" && key != "linkpath" {
				t.Errorf("entry %q has ambient PAX key %q", header.Name, key)
			}
		}
		if strings.Contains(header.Name, strings.Repeat("界", 60)) {
			longPAXSeen = header.Format == tar.FormatPAX && header.PAXRecords["path"] == header.Name
		}
	}
	if !sort.StringsAreSorted(names) {
		t.Fatalf("tar entries are not sorted: %v", names)
	}
	if prefix != "" && (len(names) == 0 || names[0] != prefix+"/") {
		t.Fatalf("archive root = %v, want %q", names, prefix+"/")
	}
	if !longPAXSeen {
		t.Fatal("long non-ASCII path did not use a path-only PAX extension")
	}
}

func archiveTimeIsZero(value time.Time) bool {
	return value.IsZero() || value.Unix() == 0
}

func TestBuildDeterministicTarGzipCloseErrorContract(t *testing.T) {
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "a.txt"), []byte("alpha"), 0o640); err != nil {
		t.Fatal(err)
	}
	safeOCITempRoot(t)
	prepared, err := preparePackageSource(
		context.Background(), sourceDir, outputDir, "", []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prepared.Close() })

	for _, phase := range []string{"tar", "gzip"} {
		t.Run(phase, func(t *testing.T) {
			injected := stderrors.New(phase + " close failed")
			deps := defaultArchiveDependencies()
			if phase == "tar" {
				deps.closeTar = func(writer *tar.Writer) error {
					return stderrors.Join(writer.Close(), injected)
				}
			} else {
				deps.closeGzip = func(writer *gzip.Writer) error {
					return stderrors.Join(writer.Close(), injected)
				}
			}
			layout := newTestOwnedLayout(t, t.TempDir())
			name, descriptor, buildErr := buildDeterministicTarGzipWithDependencies(
				context.Background(), prepared, layout, archiveOptions{}, deps)
			assertErrorCode(t, buildErr, apperrors.ErrCodeInternal)
			if !stderrors.Is(buildErr, injected) {
				t.Fatalf("error = %v, want cause %v", buildErr, injected)
			}
			if name != "" || !reflect.DeepEqual(descriptor, ociv1.Descriptor{}) {
				t.Fatalf("failed archive = (%q, %+v)", name, descriptor)
			}
			assertDirectoryEmpty(t, layout.Path())
		})
	}
}

func TestBuildDeterministicTarGzipStagedReadCancellation(t *testing.T) {
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "a.txt"), []byte("alpha"), 0o640); err != nil {
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
	closed := make(chan struct{})
	deps := defaultArchiveDependencies()
	deps.openSource = func(callCtx context.Context, source *preparedSource, name string) (archiveSource, error) {
		file, openErr := source.open(callCtx, name)
		if openErr != nil {
			return nil, openErr
		}
		return &blockingArchiveSource{archiveSource: file, started: started, closed: closed}, nil
	}
	type result struct {
		name string
		desc ociv1.Descriptor
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		name, desc, buildErr := buildDeterministicTarGzipWithDependencies(
			ctx, prepared, layout, archiveOptions{}, deps)
		resultCh <- result{name: name, desc: desc, err: buildErr}
	}()
	<-started
	cancel()
	got := <-resultCh
	assertErrorCode(t, got.err, apperrors.ErrCodeTimeout)
	if got.name != "" || !reflect.DeepEqual(got.desc, ociv1.Descriptor{}) {
		t.Fatalf("canceled archive = (%q, %+v)", got.name, got.desc)
	}
	select {
	case <-closed:
	default:
		t.Fatal("cancellation did not close the staged-source reader")
	}
	assertDirectoryEmpty(t, layout.Path())
}

func TestBuildDeterministicTarGzipRejectsSameSizeMutationAfterOpen(t *testing.T) {
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	const original = "original bytes"
	const replacement = "modified bytes"
	if len(original) != len(replacement) {
		t.Fatal("test fixture must preserve size")
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "payload.txt"), []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}
	safeOCITempRoot(t)
	prepared, err := preparePackageSource(
		context.Background(), sourceDir, outputDir, "", []string{"payload.txt"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prepared.Close() })
	layout := newTestOwnedLayout(t, outputDir)
	deps := defaultArchiveDependencies()
	deps.openSource = func(callCtx context.Context, source *preparedSource, name string) (archiveSource, error) {
		file, openErr := source.open(callCtx, name)
		if openErr != nil {
			return nil, openErr
		}
		if writeErr := os.WriteFile(filepath.Join(source.dir, filepath.FromSlash(name)),
			[]byte(replacement), 0o640); writeErr != nil {
			_ = file.Close()
			return nil, writeErr
		}
		return file, nil
	}
	name, descriptor, buildErr := buildDeterministicTarGzipWithDependencies(
		context.Background(), prepared, layout, archiveOptions{}, deps)
	assertErrorCode(t, buildErr, apperrors.ErrCodeInternal)
	if name != "" || !reflect.DeepEqual(descriptor, ociv1.Descriptor{}) {
		t.Fatalf("corrupt archive = (%q, %+v)", name, descriptor)
	}
	assertDirectoryEmpty(t, layout.Path())
}

func TestBuildDeterministicTarGzipDescriptorReadCancellationClosesReader(t *testing.T) {
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "a.txt"), []byte("alpha"), 0o640); err != nil {
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
	reader := newCloseUnblockedReader(nil)
	deps := defaultArchiveDependencies()
	deps.openVerify = func(context.Context, *os.Root, string) (io.ReadCloser, error) {
		return reader, nil
	}
	type result struct {
		name string
		desc ociv1.Descriptor
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		name, desc, buildErr := buildDeterministicTarGzipWithDependencies(
			ctx, prepared, layout, archiveOptions{}, deps)
		resultCh <- result{name: name, desc: desc, err: buildErr}
	}()
	<-reader.started
	cancel()
	var got result
	select {
	case got = <-resultCh:
	case <-time.After(time.Second):
		_ = reader.Close()
		got = <-resultCh
		t.Fatal("descriptor cancellation did not close the blocking verification reader")
	}
	assertErrorCode(t, got.err, apperrors.ErrCodeTimeout)
	if got.name != "" || !reflect.DeepEqual(got.desc, ociv1.Descriptor{}) {
		t.Fatalf("canceled descriptor read = (%q, %+v)", got.name, got.desc)
	}
	select {
	case <-reader.closed:
	default:
		t.Fatal("descriptor verification reader was not closed")
	}
	assertDirectoryEmpty(t, layout.Path())
}

func TestBuildDeterministicTarGzipCloseCancellation(t *testing.T) {
	sourceDir := t.TempDir()
	outputDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "a.txt"), []byte("alpha"), 0o640); err != nil {
		t.Fatal(err)
	}
	safeOCITempRoot(t)
	prepared, err := preparePackageSource(
		context.Background(), sourceDir, outputDir, "", []string{"a.txt"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prepared.Close() })

	for _, phase := range []string{"tar", "gzip"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			deps := defaultArchiveDependencies()
			if phase == "tar" {
				deps.closeTar = func(writer *tar.Writer) error {
					closeErr := writer.Close()
					cancel()
					return stderrors.Join(closeErr, ctx.Err())
				}
			} else {
				deps.closeGzip = func(writer *gzip.Writer) error {
					closeErr := writer.Close()
					cancel()
					return stderrors.Join(closeErr, ctx.Err())
				}
			}
			layout := newTestOwnedLayout(t, t.TempDir())
			name, descriptor, buildErr := buildDeterministicTarGzipWithDependencies(
				ctx, prepared, layout, archiveOptions{}, deps)
			assertErrorCode(t, buildErr, apperrors.ErrCodeTimeout)
			if name != "" || !reflect.DeepEqual(descriptor, ociv1.Descriptor{}) {
				t.Fatalf("canceled archive = (%q, %+v)", name, descriptor)
			}
			assertDirectoryEmpty(t, layout.Path())
		})
	}
}

type blockingArchiveSource struct {
	archiveSource
	started   chan struct{}
	closed    chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
	closeErr  error
}

func (r *blockingArchiveSource) Read([]byte) (int, error) {
	r.startOnce.Do(func() { close(r.started) })
	<-r.closed
	return 0, os.ErrClosed
}

func (r *blockingArchiveSource) Close() error {
	r.closeOnce.Do(func() {
		close(r.closed)
		r.closeErr = r.archiveSource.Close()
	})
	return r.closeErr
}
