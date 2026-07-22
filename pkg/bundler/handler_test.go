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

package bundler

import (
	"archive/zip"
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	"github.com/NVIDIA/aicr/pkg/errors"
)

var testZipHeaders = []string{
	"Content-Type",
	"Content-Disposition",
	"X-Bundle-Files",
	"X-Bundle-Size",
	"X-Bundle-Duration",
}

type trackingResponseWriter struct {
	header      http.Header
	statusCodes []int
	writes      int
	body        bytes.Buffer
}

func newTrackingResponseWriter() *trackingResponseWriter {
	return &trackingResponseWriter{header: make(http.Header)}
}

func (w *trackingResponseWriter) Header() http.Header {
	return w.header
}

func (w *trackingResponseWriter) WriteHeader(statusCode int) {
	w.statusCodes = append(w.statusCodes, statusCode)
}

func (w *trackingResponseWriter) Write(data []byte) (int, error) {
	w.writes++
	return w.body.Write(data)
}

func (w *trackingResponseWriter) committed() bool {
	return len(w.statusCodes) > 0 || w.writes > 0
}

type cancelAfterFirstRead struct {
	data   []byte
	offset int
	reads  int
	cancel context.CancelFunc
}

func (r *cancelAfterFirstRead) Read(buffer []byte) (int, error) {
	if r.offset == len(r.data) {
		return 0, io.EOF
	}
	r.reads++
	read := copy(buffer, r.data[r.offset:])
	r.offset += read
	if r.reads == 1 {
		r.cancel()
	}
	return read, nil
}

// TestParseBundleConfig_Bundlers pins the query-param parsing of the
// `bundlers` positive component-name filter on POST /v1/bundle: comma
// delimited, whitespace trimmed, empty segments dropped, and an all-empty
// value rejected with ErrCodeInvalidRequest. See #1531.
func TestParseBundleConfig_Bundlers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		present  bool
		bundlers string // raw query value, used only when present
		expected []string
		wantErr  bool
	}{
		{
			name:     "absent param means no filter",
			expected: nil,
		},
		{
			name:     "single name",
			present:  true,
			bundlers: "gpu-operator",
			expected: []string{"gpu-operator"},
		},
		{
			name:     "multiple names with whitespace trimmed",
			present:  true,
			bundlers: "gpu-operator, network-operator ,cert-manager",
			expected: []string{"gpu-operator", "network-operator", "cert-manager"},
		},
		{
			name:     "empty segments dropped",
			present:  true,
			bundlers: "gpu-operator,,network-operator,",
			expected: []string{"gpu-operator", "network-operator"},
		},
		{
			name:     "all-empty value rejected",
			present:  true,
			bundlers: ",, ,",
			wantErr:  true,
		},
		{
			name:    "explicit empty value rejected",
			present: true,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			target := "/v1/bundle"
			if tt.present {
				target += "?bundlers=" + url.QueryEscape(tt.bundlers)
			}
			r := httptest.NewRequest("POST", target, nil)

			cfg, err := ParseBundleConfig(r)
			if tt.wantErr {
				if err == nil {
					t.Fatal("ParseBundleConfig() expected error, got nil")
				}
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("ParseBundleConfig() error code = %v, want ErrCodeInvalidRequest", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBundleConfig() error = %v", err)
			}
			if got := cfg.Bundlers(); !reflect.DeepEqual(got, tt.expected) {
				t.Errorf("Bundlers() = %v, want %v", got, tt.expected)
			}
		})
	}
}

func TestParseBundleConfig_Serial(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		target  string
		want    bool
		wantErr bool
	}{
		{"absent defaults to false", "/v1/bundle", false, false},
		{"serial=true", "/v1/bundle?serial=true", true, false},
		{"serial=false", "/v1/bundle?serial=false", false, false},
		{"non-boolean rejected", "/v1/bundle?serial=maybe", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := ParseBundleConfig(httptest.NewRequest("POST", tt.target, nil))
			if tt.wantErr {
				if err == nil {
					t.Fatal("ParseBundleConfig() expected error, got nil")
				}
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseBundleConfig() error = %v", err)
			}
			if got := cfg.Serial(); got != tt.want {
				t.Errorf("Serial() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStreamZipResponseContext_VerifiedInventory(t *testing.T) {
	dir, inventory := writeVerifiedZipBundle(t, true)
	output := &result.Output{
		TotalFiles:    999,
		TotalSize:     999,
		TotalDuration: 2 * time.Second,
	}

	first := httptest.NewRecorder()
	if err := StreamZipResponseContext(context.Background(), first, dir, output); err != nil {
		t.Fatalf("StreamZipResponseContext() error = %v", err)
	}
	second := httptest.NewRecorder()
	if err := StreamZipResponse(second, dir, output); err != nil {
		t.Fatalf("StreamZipResponse() error = %v", err)
	}
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatal("ZIP output differs across unchanged bundle archives")
	}

	if got := first.Header().Get("X-Bundle-Files"); got != strconv.Itoa(len(inventory.RelativeFiles())) {
		t.Errorf("X-Bundle-Files = %q, want %d", got, len(inventory.RelativeFiles()))
	}
	if got := first.Header().Get("X-Bundle-Size"); got != strconv.FormatInt(inventory.TotalSize(), 10) {
		t.Errorf("X-Bundle-Size = %q, want %d", got, inventory.TotalSize())
	}
	if got := first.Header().Get("X-Bundle-Duration"); got != output.TotalDuration.String() {
		t.Errorf("X-Bundle-Duration = %q, want %q", got, output.TotalDuration)
	}

	archive := readZipArchive(t, first.Body.Bytes())
	wantNames := make([]string, 0, len(inventory.RelativeDirectories())+len(inventory.RelativeFiles()))
	for _, directory := range inventory.RelativeDirectories() {
		wantNames = append(wantNames, directory+"/")
	}
	wantNames = append(wantNames, inventory.RelativeFiles()...)
	gotNames := make([]string, 0, len(archive.File))
	counts := make(map[string]int, len(archive.File))
	canonicalTime := time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, file := range archive.File {
		gotNames = append(gotNames, file.Name)
		counts[file.Name]++
		if strings.ContainsRune(file.Name, '\\') {
			t.Errorf("ZIP entry %q is not slash-normalized", file.Name)
		}
		if !file.Modified.Equal(canonicalTime) {
			t.Errorf("ZIP entry %q modified time = %s, want %s", file.Name, file.Modified, canonicalTime)
		}
		if strings.HasSuffix(file.Name, "/") {
			if got, want := file.Mode(), os.ModeDir|0755; got != want {
				t.Errorf("ZIP directory %q mode = %v, want %v", file.Name, got, want)
			}
		}
	}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Errorf("ZIP entries = %v, want %v", gotNames, wantNames)
	}
	for _, name := range wantNames {
		if counts[name] != 1 {
			t.Errorf("ZIP entry %q count = %d, want 1", name, counts[name])
		}
	}
	for _, metadata := range attestation.BundleMetadataPaths() {
		if counts[metadata] != 1 {
			t.Errorf("attestation metadata %q count = %d, want 1", metadata, counts[metadata])
		}
	}
	if mode := zipFileByName(t, archive, "deploy.sh").Mode(); mode.Perm() != 0755 {
		t.Errorf("deploy.sh mode = %v, want executable 0755", mode)
	}
}

func TestStreamZipResponseContext_RejectsBeforeCommit(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, dir string)
	}{
		{
			name: "unmanaged root file",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				writeTestFile(t, filepath.Join(dir, "unmanaged.txt"), []byte("unmanaged"), 0644)
			},
		},
		{
			name: "unmanaged directory",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.Mkdir(filepath.Join(dir, "unmanaged"), 0755); err != nil {
					t.Fatalf("Mkdir() error = %v", err)
				}
			},
		},
		{
			name: "attestation metadata near miss",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				writeTestFile(t,
					filepath.Join(dir, "attestation", "bundle-attestation.sigstore.json.bak"),
					[]byte("near miss"), 0600)
			},
		},
		{
			name: "symlink",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.Symlink("deploy.sh", filepath.Join(dir, "deploy-link")); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
			},
		},
		{
			name: "malformed manifest",
			mutate: func(t *testing.T, dir string) {
				t.Helper()
				writeTestFile(t, filepath.Join(dir, checksum.ChecksumFileName), []byte("malformed\n"), 0600)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, _ := writeVerifiedZipBundle(t, false)
			tt.mutate(t, dir)
			writer := newTrackingResponseWriter()
			err := StreamZipResponseContext(context.Background(), writer, dir, &result.Output{})
			if err == nil {
				t.Fatal("StreamZipResponseContext() expected error, got nil")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("StreamZipResponseContext() error = %v, want ErrCodeInvalidRequest", err)
			}
			if writer.committed() {
				t.Fatalf("response committed before integrity failure: headers=%v writes=%d", writer.statusCodes, writer.writes)
			}
			for _, header := range testZipHeaders {
				if value := writer.Header().Get(header); value != "" {
					t.Errorf("header %s = %q after preflight failure, want empty", header, value)
				}
			}
		})
	}
}

func TestStreamZipResponseContext_NilOutput(t *testing.T) {
	dir, _ := writeVerifiedZipBundle(t, false)
	writer := newTrackingResponseWriter()
	err := StreamZipResponseContext(context.Background(), writer, dir, nil)
	if err == nil {
		t.Fatal("StreamZipResponseContext() expected error, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("StreamZipResponseContext() error = %v, want ErrCodeInvalidRequest", err)
	}
	if writer.committed() {
		t.Fatal("nil output committed the response")
	}
}

func TestStreamZipResponseContext_CanceledBeforeStage(t *testing.T) {
	dir, _ := writeVerifiedZipBundle(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writer := newTrackingResponseWriter()
	err := StreamZipResponseContext(ctx, writer, dir, &result.Output{})
	if err == nil {
		t.Fatal("StreamZipResponseContext() expected error, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("StreamZipResponseContext() error = %v, want ErrCodeTimeout", err)
	}
	if writer.committed() {
		t.Fatal("canceled archive committed the response")
	}
}

func TestStreamZipResponseContext_CopyCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	data := bytes.Repeat([]byte("a"), 1024*1024)
	reader := &cancelAfterFirstRead{data: data, cancel: cancel}
	var destination bytes.Buffer
	err := copyZipEntryContext(ctx, &destination, reader)
	if err == nil {
		t.Fatal("copyZipEntryContext() expected error, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("copyZipEntryContext() error = %v, want ErrCodeTimeout", err)
	}
	if reader.reads != 1 {
		t.Errorf("source reads = %d, want 1", reader.reads)
	}
	if reader.offset >= len(data) {
		t.Fatalf("copy consumed %d bytes, want less than %d", reader.offset, len(data))
	}
}

func TestStreamZipResponseContext_StagedInventoryIsolatedFromSource(t *testing.T) {
	dir, _ := writeVerifiedZipBundle(t, false)
	stagedDir, _, cleanup, err := checksum.StageVerifiedBundle(
		context.Background(), dir, checksum.InventoryOptions{})
	if err != nil {
		t.Fatalf("StageVerifiedBundle() error = %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := cleanup(); cleanupErr != nil {
			t.Errorf("StageVerifiedBundle cleanup error = %v", cleanupErr)
		}
	})

	replacement := []byte("replacement payload")
	writeTestFile(t, filepath.Join(dir, "deploy.sh"), replacement, 0755)
	recorder := httptest.NewRecorder()
	if err := StreamZipResponseContext(context.Background(), recorder, stagedDir, &result.Output{}); err != nil {
		t.Fatalf("StreamZipResponseContext() error = %v", err)
	}
	archive := readZipArchive(t, recorder.Body.Bytes())
	archived := readZipFile(t, zipFileByName(t, archive, "deploy.sh"))
	if bytes.Equal(archived, replacement) {
		t.Fatal("archive observed payload replacement from the original source")
	}
	if got, want := string(archived), "#!/bin/sh\necho verified\n"; got != want {
		t.Errorf("archived deploy.sh = %q, want %q", got, want)
	}
}

func TestStreamZipResponseContext_CleanupFailure(t *testing.T) {
	cleanupFailure := stderrors.New("injected stage cleanup failure")

	newDependencies := func(t *testing.T, cancel context.CancelFunc) (streamZipDependencies, *int) {
		t.Helper()
		deps := defaultStreamZipDependencies()
		stage := deps.stageVerifiedBundle
		deps.stageVerifiedBundle = func(
			ctx context.Context,
			dir string,
			opts checksum.InventoryOptions,
		) (string, *checksum.Inventory, func() error, error) {

			stagedDir, inventory, cleanup, err := stage(ctx, dir, opts)
			if err != nil {
				return "", nil, nil, err
			}
			if cancel != nil {
				cancel()
			}
			return stagedDir, inventory, func() error {
				return stderrors.Join(cleanup(), cleanupFailure)
			}, nil
		}
		warnCalls := 0
		deps.warn = func(string, ...any) { warnCalls++ }
		return deps, &warnCalls
	}

	t.Run("cleanup-only failure clears success", func(t *testing.T) {
		dir, _ := writeVerifiedZipBundle(t, false)
		deps, warnCalls := newDependencies(t, nil)
		writer := newTrackingResponseWriter()
		err := streamZipResponseContextWithDependencies(
			context.Background(), writer, dir, &result.Output{}, deps)
		if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
			t.Fatalf("StreamZipResponseContext cleanup error = %v, want ErrCodeInternal", err)
		}
		if !stderrors.Is(err, cleanupFailure) {
			t.Errorf("error = %v, want injected cleanup cause", err)
		}
		if *warnCalls != 0 {
			t.Errorf("warning calls = %d, want 0 without a primary error", *warnCalls)
		}
	})

	t.Run("archive failure preserves primary and logs cleanup", func(t *testing.T) {
		dir, _ := writeVerifiedZipBundle(t, false)
		ctx, cancel := context.WithCancel(context.Background())
		deps, warnCalls := newDependencies(t, cancel)
		writer := newTrackingResponseWriter()
		err := streamZipResponseContextWithDependencies(ctx, writer, dir, &result.Output{}, deps)
		if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			t.Fatalf("StreamZipResponseContext primary error = %v, want ErrCodeTimeout", err)
		}
		if stderrors.Is(err, cleanupFailure) {
			t.Errorf("primary error was joined with cleanup failure: %v", err)
		}
		if *warnCalls != 1 {
			t.Errorf("warning calls = %d, want 1", *warnCalls)
		}
	})
}

func TestStreamZipResponseContext_CleansStageBeforeFinalizingZIP(t *testing.T) {
	dir, _ := writeVerifiedZipBundle(t, false)
	writer := newTrackingResponseWriter()
	deps := defaultStreamZipDependencies()
	stage := deps.stageVerifiedBundle
	cleanupCalls := 0
	cleanupObservedUnfinalizedArchive := false
	deps.stageVerifiedBundle = func(
		ctx context.Context,
		dir string,
		opts checksum.InventoryOptions,
	) (string, *checksum.Inventory, func() error, error) {

		stagedDir, inventory, cleanup, err := stage(ctx, dir, opts)
		if err != nil {
			return "", nil, nil, err
		}
		return stagedDir, inventory, func() error {
			cleanupCalls++
			_, archiveErr := zip.NewReader(
				bytes.NewReader(writer.body.Bytes()), int64(writer.body.Len()))
			cleanupObservedUnfinalizedArchive = archiveErr != nil
			return cleanup()
		}, nil
	}

	if err := streamZipResponseContextWithDependencies(
		context.Background(), writer, dir, &result.Output{}, deps); err != nil {
		t.Fatalf("StreamZipResponseContext() error = %v", err)
	}
	if cleanupCalls != 1 {
		t.Fatalf("stage cleanup calls = %d, want 1", cleanupCalls)
	}
	if !cleanupObservedUnfinalizedArchive {
		t.Error("stage cleanup ran after ZIP finalization")
	}
	archive := readZipArchive(t, writer.body.Bytes())
	if len(archive.File) == 0 {
		t.Error("finalized archive contains no entries")
	}
}

func writeVerifiedZipBundle(t *testing.T, includeMetadata bool) (string, *checksum.Inventory) {
	t.Helper()
	dir := t.TempDir()
	deployPath := filepath.Join(dir, "deploy.sh")
	configPath := filepath.Join(dir, "nested", "config.yaml")
	writeTestFile(t, deployPath, []byte("#!/bin/sh\necho verified\n"), 0755)
	writeTestFile(t, configPath, []byte("enabled: true\n"), 0644)
	if err := checksum.GenerateChecksums(context.Background(), dir, []string{configPath, deployPath}); err != nil {
		t.Fatalf("GenerateChecksums() error = %v", err)
	}

	opts := checksum.InventoryOptions{}
	if includeMetadata {
		for index, rel := range attestation.BundleMetadataPaths() {
			writeTestFile(t, filepath.Join(dir, filepath.FromSlash(rel)),
				[]byte(fmt.Sprintf("metadata-%d", index)), 0600)
		}
		opts.AllowedMetadataPaths = attestation.BundleMetadataPaths()
	}
	_, inventory, _, err := checksum.ReadAndVerifyBundle(context.Background(), dir, opts)
	if err != nil {
		t.Fatalf("ReadAndVerifyBundle() error = %v", err)
	}
	return dir, inventory
}

func writeTestFile(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", path, err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("Chmod(%q) error = %v", path, err)
	}
}

func readZipArchive(t *testing.T, data []byte) *zip.Reader {
	t.Helper()
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("zip.NewReader() error = %v", err)
	}
	return archive
}

func zipFileByName(t *testing.T, archive *zip.Reader, name string) *zip.File {
	t.Helper()
	for _, file := range archive.File {
		if file.Name == name {
			return file
		}
	}
	t.Fatalf("ZIP entry %q not found", name)
	return nil
}

func readZipFile(t *testing.T, file *zip.File) []byte {
	t.Helper()
	reader, err := file.Open()
	if err != nil {
		t.Fatalf("Open(%q) error = %v", file.Name, err)
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		t.Fatalf("ReadAll(%q) error = %v", file.Name, readErr)
	}
	if closeErr != nil {
		t.Fatalf("Close(%q) error = %v", file.Name, closeErr)
	}
	return data
}
