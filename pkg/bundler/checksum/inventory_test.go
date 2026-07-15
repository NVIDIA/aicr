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
	"crypto/sha256"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestVerifyBundle(t *testing.T) {
	t.Parallel()

	t.Run("exact payload and checksum file succeed", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		data := writeTestBundle(t, dir, map[string]testFile{
			"payload.txt": {content: []byte("payload"), mode: 0644},
		})

		manifest, inventory, err := VerifyBundle(context.Background(), dir, data, InventoryOptions{})
		if err != nil {
			t.Fatalf("VerifyBundle() error = %v", err)
		}
		if manifest.Len() != 1 {
			t.Errorf("Manifest.Len() = %d, want 1", manifest.Len())
		}
		if inventory.ManifestLen() != manifest.Len() {
			t.Errorf("Inventory.ManifestLen() = %d, want %d", inventory.ManifestLen(), manifest.Len())
		}
		wantChecksumDigest := sha256.Sum256(data)
		if got := inventory.ChecksumDigest(); got != wantChecksumDigest {
			t.Errorf("Inventory.ChecksumDigest() = %x, want %x", got, wantChecksumDigest)
		}
		wantFiles := []string{ChecksumFileName, "payload.txt"}
		if got := inventory.RelativeFiles(); !reflect.DeepEqual(got, wantFiles) {
			t.Errorf("RelativeFiles() = %#v, want %#v", got, wantFiles)
		}
		wantSize := int64(len(data) + len("payload"))
		if got := inventory.TotalSize(); got != wantSize {
			t.Errorf("TotalSize() = %d, want %d", got, wantSize)
		}
	})

	metadataTests := []struct {
		name string
		path string
	}{
		{name: "bundle attestation", path: "attestation/bundle-attestation.sigstore.json"},
		{name: "binary attestation", path: "attestation/aicr-attestation.sigstore.json"},
	}
	for _, tt := range metadataTests {
		t.Run(tt.name+" succeeds only when allowed", func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			data := writeTestBundle(t, dir, map[string]testFile{
				"payload.txt": {content: []byte("payload"), mode: 0644},
			})
			writeTestFile(t, dir, tt.path, []byte("attestation"), 0600)
			opts := InventoryOptions{AllowedMetadataPaths: []string{tt.path}}

			_, inventory, err := VerifyBundle(context.Background(), dir, data, opts)
			if err != nil {
				t.Fatalf("VerifyBundle() error = %v", err)
			}
			if !containsString(inventory.RelativeFiles(), tt.path) {
				t.Errorf("RelativeFiles() = %#v, missing %q", inventory.RelativeFiles(), tt.path)
			}
			if got := inventory.RelativeDirectories(); !reflect.DeepEqual(got, []string{"attestation"}) {
				t.Errorf("RelativeDirectories() = %#v, want attestation", got)
			}
		})

		t.Run(tt.name+" fails without allowance", func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			data := writeTestBundle(t, dir, map[string]testFile{
				"payload.txt": {content: []byte("payload"), mode: 0644},
			})
			writeTestFile(t, dir, tt.path, []byte("attestation"), 0600)

			_, _, err := VerifyBundle(context.Background(), dir, data, InventoryOptions{})
			requireInventoryErrorCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}

	invalidTests := []struct {
		name  string
		opts  InventoryOptions
		setup func(t *testing.T, dir string) []byte
	}{
		{
			name: "allowlisted near miss",
			opts: InventoryOptions{AllowedMetadataPaths: []string{
				"attestation/bundle-attestation.sigstore.json",
			}},
			setup: func(t *testing.T, dir string) []byte {
				data := writeTestBundle(t, dir, map[string]testFile{
					"payload.txt": {content: []byte("payload"), mode: 0644},
				})
				writeTestFile(t, dir, "attestation/bundle-attestation.sigstore.json.bak", []byte("near miss"), 0600)
				return data
			},
		},
		{
			name: "unexpected root file",
			setup: func(t *testing.T, dir string) []byte {
				data := writeTestBundle(t, dir, map[string]testFile{
					"payload.txt": {content: []byte("payload"), mode: 0644},
				})
				writeTestFile(t, dir, "extra.txt", []byte("extra"), 0600)
				return data
			},
		},
		{
			name: "unexpected nested file",
			setup: func(t *testing.T, dir string) []byte {
				data := writeTestBundle(t, dir, map[string]testFile{
					"payload.txt": {content: []byte("payload"), mode: 0644},
				})
				writeTestFile(t, dir, "nested/extra.txt", []byte("extra"), 0600)
				return data
			},
		},
		{
			name: "unexpected empty directory",
			setup: func(t *testing.T, dir string) []byte {
				data := writeTestBundle(t, dir, map[string]testFile{
					"payload.txt": {content: []byte("payload"), mode: 0644},
				})
				if err := os.Mkdir(filepath.Join(dir, "empty"), 0755); err != nil {
					t.Fatalf("Mkdir() error = %v", err)
				}
				return data
			},
		},
		{
			name: "empty attestation directory",
			opts: fullBundleMetadataOptions,
			setup: func(t *testing.T, dir string) []byte {
				data := writeTestBundle(t, dir, map[string]testFile{
					"payload.txt": {content: []byte("payload"), mode: 0644},
				})
				if err := os.Mkdir(filepath.Join(dir, "attestation"), 0755); err != nil {
					t.Fatalf("Mkdir() error = %v", err)
				}
				return data
			},
		},
		{
			name: "listed symlink",
			setup: func(t *testing.T, dir string) []byte {
				targetDir := t.TempDir()
				target := filepath.Join(targetDir, "target.txt")
				if err := os.WriteFile(target, []byte("target"), 0600); err != nil {
					t.Fatalf("WriteFile() target error = %v", err)
				}
				if err := os.Symlink(target, filepath.Join(dir, "payload.txt")); err != nil {
					t.Fatalf("Symlink() error = %v", err)
				}
				return writeTestManifest(t, dir, map[string][]byte{"payload.txt": []byte("target")})
			},
		},
		{
			name: "unlisted symlink",
			setup: func(t *testing.T, dir string) []byte {
				data := writeTestBundle(t, dir, map[string]testFile{
					"payload.txt": {content: []byte("payload"), mode: 0644},
				})
				if err := os.Symlink(filepath.Join(dir, "payload.txt"), filepath.Join(dir, "extra-link")); err != nil {
					t.Fatalf("Symlink() error = %v", err)
				}
				return data
			},
		},
		{
			name: "symlinked directory",
			setup: func(t *testing.T, dir string) []byte {
				data := writeTestBundle(t, dir, map[string]testFile{
					"payload.txt": {content: []byte("payload"), mode: 0644},
				})
				target := t.TempDir()
				if err := os.Symlink(target, filepath.Join(dir, "linked")); err != nil {
					t.Fatalf("Symlink() error = %v", err)
				}
				return data
			},
		},
		{
			name: "fifo",
			setup: func(t *testing.T, dir string) []byte {
				data := writeTestBundle(t, dir, map[string]testFile{
					"payload.txt": {content: []byte("payload"), mode: 0644},
				})
				fifo := filepath.Join(dir, "pipe")
				if err := unix.Mkfifo(fifo, 0600); err != nil {
					t.Skipf("FIFO unsupported: %v", err)
				}
				return data
			},
		},
		{
			name: "listed directory",
			setup: func(t *testing.T, dir string) []byte {
				if err := os.Mkdir(filepath.Join(dir, "payload"), 0755); err != nil {
					t.Fatalf("Mkdir() error = %v", err)
				}
				return writeTestManifest(t, dir, map[string][]byte{"payload": nil})
			},
		},
		{
			name: "missing listed file",
			setup: func(t *testing.T, dir string) []byte {
				return writeTestManifest(t, dir, map[string][]byte{"missing.txt": []byte("missing")})
			},
		},
		{
			name: "checksum mismatch",
			setup: func(t *testing.T, dir string) []byte {
				writeTestFile(t, dir, "payload.txt", []byte("actual"), 0600)
				return writeTestManifest(t, dir, map[string][]byte{"payload.txt": []byte("expected")})
			},
		},
	}

	for _, tt := range invalidTests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			data := tt.setup(t, dir)
			_, _, err := VerifyBundle(context.Background(), dir, data, tt.opts)
			requireInventoryErrorCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}

	t.Run("context canceled before walk", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		data := writeTestBundle(t, dir, map[string]testFile{
			"payload.txt": {content: []byte("payload"), mode: 0644},
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, _, err := VerifyBundle(ctx, dir, data, InventoryOptions{})
		requireInventoryErrorCode(t, err, errors.ErrCodeTimeout)
	})

	t.Run("context canceled during multi file walk", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		files := make(map[string]testFile, 128)
		for n := range 128 {
			files[filepath.Join("payload", leftPadNumber(n))] = testFile{mode: 0600}
		}
		data := writeTestBundle(t, dir, files)
		ctx := &cancelAfterErrContext{Context: context.Background(), cancelAfter: 180}

		_, _, err := VerifyBundle(ctx, dir, data, InventoryOptions{})
		requireInventoryErrorCode(t, err, errors.ErrCodeTimeout)
		if got := ctx.calls.Load(); got <= ctx.cancelAfter {
			t.Errorf("context Err() calls = %d, want more than %d", got, ctx.cancelAfter)
		}
	})

	t.Run("accessors are defensive and Open revalidates content", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		data := writeTestBundle(t, dir, map[string]testFile{
			"nested/payload.txt": {content: []byte("payload"), mode: 0644},
		})
		_, inventory, err := VerifyBundle(context.Background(), dir, data, InventoryOptions{})
		if err != nil {
			t.Fatalf("VerifyBundle() error = %v", err)
		}

		files := inventory.RelativeFiles()
		files[0] = "changed"
		if inventory.RelativeFiles()[0] == "changed" {
			t.Error("RelativeFiles() exposed internal state")
		}
		directories := inventory.RelativeDirectories()
		directories[0] = "changed"
		if inventory.RelativeDirectories()[0] == "changed" {
			t.Error("RelativeDirectories() exposed internal state")
		}
		wantAbsolute := make([]string, 0, len(inventory.RelativeFiles()))
		for _, rel := range inventory.RelativeFiles() {
			wantAbsolute = append(wantAbsolute, filepath.Join(dir, filepath.FromSlash(rel)))
		}
		if got := inventory.AbsoluteFiles(); !reflect.DeepEqual(got, wantAbsolute) {
			t.Errorf("AbsoluteFiles() = %#v, want %#v", got, wantAbsolute)
		}

		file, err := inventory.Open(context.Background(), "nested/payload.txt")
		if err != nil {
			t.Fatalf("Inventory.Open() error = %v", err)
		}
		opened, readErr := io.ReadAll(file)
		closeErr := file.Close()
		if readErr != nil {
			t.Fatalf("ReadAll() error = %v", readErr)
		}
		if closeErr != nil {
			t.Fatalf("Close() error = %v", closeErr)
		}
		if string(opened) != "payload" {
			t.Errorf("Inventory.Open() content = %q, want payload", opened)
		}

		if mutationErr := os.WriteFile(
			filepath.Join(dir, "nested/payload.txt"), []byte("changed"), 0644,
		); mutationErr != nil {
			t.Fatalf("WriteFile() mutation error = %v", mutationErr)
		}
		_, err = inventory.Open(context.Background(), "nested/payload.txt")
		requireInventoryErrorCode(t, err, errors.ErrCodeInvalidRequest)
		_, err = inventory.Open(context.Background(), "not-in-inventory.txt")
		requireInventoryErrorCode(t, err, errors.ErrCodeInvalidRequest)
	})
}

func TestReadAndVerifyBundle(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	wantData := writeTestBundle(t, dir, map[string]testFile{
		"payload.txt": {content: []byte("payload"), mode: 0644},
	})

	manifest, inventory, gotData, err := ReadAndVerifyBundle(context.Background(), dir, InventoryOptions{})
	if err != nil {
		t.Fatalf("ReadAndVerifyBundle() error = %v", err)
	}
	if manifest.Len() != 1 {
		t.Errorf("Manifest.Len() = %d, want 1", manifest.Len())
	}
	if inventory == nil {
		t.Fatal("ReadAndVerifyBundle() inventory is nil")
	}
	if !reflect.DeepEqual(gotData, wantData) {
		t.Errorf("ReadAndVerifyBundle() data = %q, want %q", gotData, wantData)
	}
	gotData[0] ^= 0xff
	reread, readErr := os.ReadFile(filepath.Join(dir, ChecksumFileName))
	if readErr != nil {
		t.Fatalf("ReadFile() error = %v", readErr)
	}
	if !reflect.DeepEqual(reread, wantData) {
		t.Error("returned checksum bytes alias on-disk data")
	}
}

func TestReadAndVerifyBundle_MissingManifestSentinel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		prepare      func(t *testing.T, dir string)
		wantSentinel bool
	}{
		{
			name:         "missing manifest",
			prepare:      func(*testing.T, string) {},
			wantSentinel: true,
		},
		{
			name: "malformed manifest",
			prepare: func(t *testing.T, dir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, ChecksumFileName), []byte("malformed\n"), 0600); err != nil {
					t.Fatal(err)
				}
			},
			wantSentinel: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			tt.prepare(t, dir)

			_, _, _, err := ReadAndVerifyBundle(context.Background(), dir, InventoryOptions{})
			if err == nil {
				t.Fatal("ReadAndVerifyBundle() error = nil, want failure")
			}
			if got := stderrors.Is(err, ErrChecksumManifestMissing); got != tt.wantSentinel {
				t.Errorf("errors.Is(error, ErrChecksumManifestMissing) = %v, want %v", got, tt.wantSentinel)
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("ReadAndVerifyBundle() error = %v, want ErrCodeInvalidRequest", err)
			}
			if tt.wantSentinel && !stderrors.Is(err, os.ErrNotExist) {
				t.Errorf("ReadAndVerifyBundle() error = %v, want os.ErrNotExist cause", err)
			}
		})
	}
}

func TestOpenRegular_RejectsFIFOReplacementBeforeOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payload.txt")
	if err := os.WriteFile(path, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(dir, "payload.original")

	result := make(chan error, 1)
	go func() {
		_, err := openRegularWithOpener(
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
		t.Error("openRegular blocked after a regular file was replaced by a FIFO")
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

func TestStageVerifiedBundle(t *testing.T) {
	t.Parallel()

	sourceDir := t.TempDir()
	checksumData := writeTestBundle(t, sourceDir, map[string]testFile{
		"bin/run.sh":    {content: []byte("#!/bin/sh\nexit 0\n"), mode: 0755},
		"config/config": {content: []byte("config"), mode: 0640},
	})
	metadataPath := "attestation/bundle-attestation.sigstore.json"
	writeTestFile(t, sourceDir, metadataPath, []byte("attestation"), 0600)
	opts := InventoryOptions{AllowedMetadataPaths: []string{metadataPath}}

	stagedDir, staged, cleanup, err := StageVerifiedBundle(context.Background(), sourceDir, opts)
	if err != nil {
		t.Fatalf("StageVerifiedBundle() error = %v", err)
	}
	if cleanup == nil {
		t.Fatal("StageVerifiedBundle() cleanup is nil")
	}
	if !strings.HasPrefix(filepath.Base(stagedDir), "aicr-bundle-stage-") {
		t.Errorf("StageVerifiedBundle() dir = %q, want aicr-bundle-stage- prefix", stagedDir)
	}

	wantFiles := []string{
		metadataPath,
		"bin/run.sh",
		ChecksumFileName,
		"config/config",
	}
	sort.Strings(wantFiles)
	if got := staged.RelativeFiles(); !reflect.DeepEqual(got, wantFiles) {
		t.Errorf("staged RelativeFiles() = %#v, want %#v", got, wantFiles)
	}
	if got := staged.ManifestLen(); got != 2 {
		t.Errorf("staged ManifestLen() = %d, want 2", got)
	}
	wantChecksumDigest := sha256.Sum256(checksumData)
	if got := staged.ChecksumDigest(); got != wantChecksumDigest {
		t.Errorf("staged ChecksumDigest() = %x, want %x", got, wantChecksumDigest)
	}
	for _, rel := range wantFiles {
		sourceInfo, statErr := os.Stat(filepath.Join(sourceDir, filepath.FromSlash(rel)))
		if statErr != nil {
			t.Fatalf("Stat() source %q error = %v", rel, statErr)
		}
		stagedInfo, statErr := os.Stat(filepath.Join(stagedDir, filepath.FromSlash(rel)))
		if statErr != nil {
			t.Fatalf("Stat() staged %q error = %v", rel, statErr)
		}
		if stagedInfo.Mode().Perm() != sourceInfo.Mode().Perm() {
			t.Errorf("staged %q mode = %v, want %v", rel, stagedInfo.Mode().Perm(), sourceInfo.Mode().Perm())
		}
	}
	stagedChecksumData, readErr := os.ReadFile(filepath.Join(stagedDir, ChecksumFileName))
	if readErr != nil {
		t.Fatalf("ReadFile() staged checksum error = %v", readErr)
	}
	if !reflect.DeepEqual(stagedChecksumData, checksumData) {
		t.Errorf("staged checksum bytes = %q, want %q", stagedChecksumData, checksumData)
	}

	if cleanupErr := cleanup(); cleanupErr != nil {
		t.Fatalf("cleanup() error = %v", cleanupErr)
	}
	if cleanupErr := cleanup(); cleanupErr != nil {
		t.Fatalf("second cleanup() error = %v", cleanupErr)
	}
	if _, statErr := os.Stat(stagedDir); !os.IsNotExist(statErr) {
		t.Errorf("cleanup did not remove stage: %v", statErr)
	}
}

func TestNewPrivateBundleStage_SourceSwap(t *testing.T) {
	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "source")
	if err := os.Mkdir(sourceDir, 0700); err != nil {
		t.Fatalf("Mkdir(source) error = %v", err)
	}
	checksumData := writeTestBundle(t, sourceDir, map[string]testFile{
		"payload.txt": {content: []byte("verified payload"), mode: 0600},
	})
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)

	originalDir := filepath.Join(sourceParent, "original")
	replacementMarker := filepath.Join(sourceDir, "replacement-marker")
	deps := defaultPrivateBundleStageDependencies()
	deps.beforeSourceOpen = func(path string) error {
		if err := os.Rename(path, originalDir); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0700); err != nil {
			return err
		}
		return os.WriteFile(replacementMarker, []byte("replacement"), 0600)
	}

	stage, err := newPrivateBundleStage(context.Background(), sourceDir, deps)
	requireInventoryErrorCode(t, err, errors.ErrCodeInvalidRequest)
	if stage != nil {
		t.Fatal("newPrivateBundleStage() returned a stage after source swap")
	}
	assertVerifiedBundleUnchanged(t, originalDir, checksumData)
	assertReplacementFileContents(t, replacementMarker)
	assertNoStageResidue(t, tempRoot)
}

func TestStageVerifiedBundle_SourceSwap(t *testing.T) {
	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "source")
	if err := os.Mkdir(sourceDir, 0700); err != nil {
		t.Fatalf("Mkdir(source) error = %v", err)
	}
	checksumData := writeTestBundle(t, sourceDir, map[string]testFile{
		"payload.txt": {content: []byte("verified payload"), mode: 0600},
	})
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)

	originalDir := filepath.Join(sourceParent, "original")
	replacementMarker := filepath.Join(sourceDir, "replacement-marker")
	deps := defaultPrivateBundleStageDependencies()
	deps.beforeSourceOpen = func(path string) error {
		if err := os.Rename(path, originalDir); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0700); err != nil {
			return err
		}
		return os.WriteFile(replacementMarker, []byte("replacement"), 0600)
	}

	stagedDir, staged, cleanup, err := stageVerifiedBundleWithDependencies(
		context.Background(), sourceDir, InventoryOptions{}, deps)
	requireInventoryErrorCode(t, err, errors.ErrCodeInvalidRequest)
	if stagedDir != "" || staged != nil || cleanup != nil {
		t.Fatalf("failed stage results = (%q, %#v, %p), want empty/nil/nil", stagedDir, staged, cleanup)
	}
	assertVerifiedBundleUnchanged(t, originalDir, checksumData)
	assertReplacementFileContents(t, replacementMarker)
	assertNoStageResidue(t, tempRoot)
}

func TestNewPrivateBundleStage_TempSwap(t *testing.T) {
	sourceDir := t.TempDir()
	checksumData := writeTestBundle(t, sourceDir, map[string]testFile{
		"payload.txt": {content: []byte("verified payload"), mode: 0600},
	})
	tempParent := t.TempDir()
	tempRoot := filepath.Join(tempParent, "temp")
	if err := os.Mkdir(tempRoot, 0700); err != nil {
		t.Fatalf("Mkdir(temp) error = %v", err)
	}
	t.Setenv("TMPDIR", tempRoot)

	originalTemp := filepath.Join(tempParent, "original-temp")
	replacementMarker := filepath.Join(tempRoot, "replacement-marker")
	deps := defaultPrivateBundleStageDependencies()
	deps.beforeTempOpen = func(path string) error {
		if err := os.Rename(path, originalTemp); err != nil {
			return err
		}
		if err := os.Mkdir(path, 0700); err != nil {
			return err
		}
		return os.WriteFile(replacementMarker, []byte("replacement"), 0600)
	}

	stage, err := newPrivateBundleStage(context.Background(), sourceDir, deps)
	requireInventoryErrorCode(t, err, errors.ErrCodeInternal)
	if stage != nil {
		t.Fatal("newPrivateBundleStage() returned a stage after temp-root swap")
	}
	assertVerifiedBundleUnchanged(t, sourceDir, checksumData)
	assertReplacementFileContents(t, replacementMarker)
	assertNoStageResidue(t, tempRoot)
}

func TestNewPrivateBundleStage_TempChildSwap(t *testing.T) {
	sourceDir := t.TempDir()
	checksumData := writeTestBundle(t, sourceDir, map[string]testFile{
		"payload.txt": {content: []byte("verified payload"), mode: 0600},
	})
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)

	var movedChild string
	var replacementMarker string
	deps := defaultPrivateBundleStageDependencies()
	deps.beforeChildRevalidate = func(parentPath, childName string) error {
		movedChild = filepath.Join(parentPath, childName+"-moved")
		if err := os.Rename(filepath.Join(parentPath, childName), movedChild); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Join(parentPath, childName), 0700); err != nil {
			return err
		}
		replacementMarker = filepath.Join(parentPath, childName, "replacement-marker")
		return os.WriteFile(replacementMarker, []byte("replacement"), 0600)
	}

	stage, err := newPrivateBundleStage(context.Background(), sourceDir, deps)
	requireInventoryErrorCode(t, err, errors.ErrCodeInternal)
	if stage != nil {
		t.Fatal("newPrivateBundleStage() returned a stage after child swap")
	}
	assertVerifiedBundleUnchanged(t, sourceDir, checksumData)
	assertReplacementFileContents(t, replacementMarker)
	if movedChild == "" {
		t.Fatal("child-swap seam was not invoked")
	}
	if err := os.RemoveAll(movedChild); err != nil {
		t.Fatalf("RemoveAll(moved child) error = %v", err)
	}
}

func TestStageVerifiedBundle_TempSwap(t *testing.T) {
	t.Run("configured temp root", func(t *testing.T) {
		sourceDir := t.TempDir()
		checksumData := writeTestBundle(t, sourceDir, map[string]testFile{
			"payload.txt": {content: []byte("verified payload"), mode: 0600},
		})
		tempParent := t.TempDir()
		tempRoot := filepath.Join(tempParent, "temp")
		if err := os.Mkdir(tempRoot, 0700); err != nil {
			t.Fatalf("Mkdir(temp) error = %v", err)
		}
		t.Setenv("TMPDIR", tempRoot)
		originalTemp := filepath.Join(tempParent, "original-temp")
		replacementMarker := filepath.Join(tempRoot, "replacement-marker")
		deps := defaultPrivateBundleStageDependencies()
		deps.beforeTempOpen = func(path string) error {
			if err := os.Rename(path, originalTemp); err != nil {
				return err
			}
			if err := os.Mkdir(path, 0700); err != nil {
				return err
			}
			return os.WriteFile(replacementMarker, []byte("replacement"), 0600)
		}

		stagedDir, staged, cleanup, err := stageVerifiedBundleWithDependencies(
			context.Background(), sourceDir, InventoryOptions{}, deps)
		requireInventoryErrorCode(t, err, errors.ErrCodeInternal)
		if stagedDir != "" || staged != nil || cleanup != nil {
			t.Fatalf("failed stage results = (%q, %#v, %p), want empty/nil/nil", stagedDir, staged, cleanup)
		}
		assertVerifiedBundleUnchanged(t, sourceDir, checksumData)
		assertReplacementFileContents(t, replacementMarker)
		assertNoStageResidue(t, tempRoot)
	})

	t.Run("created private child", func(t *testing.T) {
		sourceDir := t.TempDir()
		checksumData := writeTestBundle(t, sourceDir, map[string]testFile{
			"payload.txt": {content: []byte("verified payload"), mode: 0600},
		})
		tempRoot := t.TempDir()
		t.Setenv("TMPDIR", tempRoot)
		var movedChild string
		var replacementMarker string
		deps := defaultPrivateBundleStageDependencies()
		deps.beforeChildRevalidate = func(parentPath, childName string) error {
			movedChild = filepath.Join(parentPath, childName+"-moved")
			if err := os.Rename(filepath.Join(parentPath, childName), movedChild); err != nil {
				return err
			}
			if err := os.Mkdir(filepath.Join(parentPath, childName), 0700); err != nil {
				return err
			}
			replacementMarker = filepath.Join(parentPath, childName, "replacement-marker")
			return os.WriteFile(replacementMarker, []byte("replacement"), 0600)
		}

		stagedDir, staged, cleanup, err := stageVerifiedBundleWithDependencies(
			context.Background(), sourceDir, InventoryOptions{}, deps)
		requireInventoryErrorCode(t, err, errors.ErrCodeInternal)
		if stagedDir != "" || staged != nil || cleanup != nil {
			t.Fatalf("failed stage results = (%q, %#v, %p), want empty/nil/nil", stagedDir, staged, cleanup)
		}
		assertVerifiedBundleUnchanged(t, sourceDir, checksumData)
		assertReplacementFileContents(t, replacementMarker)
		if movedChild == "" {
			t.Fatal("child-swap seam was not invoked")
		}
		if err := os.RemoveAll(movedChild); err != nil {
			t.Fatalf("RemoveAll(moved child) error = %v", err)
		}
	})
}

func TestNewPrivateBundleStage_TempSourceAlias(t *testing.T) {
	tests := []struct {
		name     string
		tempRoot func(t *testing.T, sourceDir string) string
	}{
		{
			name: "source directory",
			tempRoot: func(_ *testing.T, sourceDir string) string {
				return sourceDir
			},
		},
		{
			name: "symlink alias",
			tempRoot: func(t *testing.T, sourceDir string) string {
				alias := filepath.Join(t.TempDir(), "source-alias")
				if err := os.Symlink(sourceDir, alias); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return alias
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sourceDir := t.TempDir()
			checksumData := writeTestBundle(t, sourceDir, map[string]testFile{
				"payload.txt": {content: []byte("verified payload"), mode: 0600},
			})
			t.Setenv("TMPDIR", tt.tempRoot(t, sourceDir))

			stage, err := newPrivateBundleStage(
				context.Background(), sourceDir, defaultPrivateBundleStageDependencies())
			requireInventoryErrorCode(t, err, errors.ErrCodeInternal)
			if stage != nil {
				t.Fatal("newPrivateBundleStage() returned unsafe stage")
			}
			assertVerifiedBundleUnchanged(t, sourceDir, checksumData)
			assertNoStageResidue(t, sourceDir)
		})
	}
}

func TestStageVerifiedBundle_TempSourceAlias(t *testing.T) {
	tests := []struct {
		name     string
		tempRoot func(t *testing.T, sourceDir string) string
	}{
		{name: "source directory", tempRoot: func(_ *testing.T, sourceDir string) string { return sourceDir }},
		{
			name: "symlink alias",
			tempRoot: func(t *testing.T, sourceDir string) string {
				alias := filepath.Join(t.TempDir(), "source-alias")
				if err := os.Symlink(sourceDir, alias); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return alias
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sourceDir := t.TempDir()
			checksumData := writeTestBundle(t, sourceDir, map[string]testFile{
				"payload.txt": {content: []byte("verified payload"), mode: 0600},
			})
			t.Setenv("TMPDIR", tt.tempRoot(t, sourceDir))

			stagedDir, staged, cleanup, err := StageVerifiedBundle(
				context.Background(), sourceDir, InventoryOptions{})
			requireInventoryErrorCode(t, err, errors.ErrCodeInternal)
			if stagedDir != "" || staged != nil || cleanup != nil {
				t.Fatalf("failed stage results = (%q, %#v, %p), want empty/nil/nil", stagedDir, staged, cleanup)
			}
			assertVerifiedBundleUnchanged(t, sourceDir, checksumData)
			assertNoStageResidue(t, sourceDir)
		})
	}
}

func TestStageVerifiedBundle_Cleanup(t *testing.T) {
	sourceDir := t.TempDir()
	writeTestBundle(t, sourceDir, map[string]testFile{
		"payload.txt": {content: []byte("verified payload"), mode: 0600},
	})
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)

	removeFailure := stderrors.New("injected anchored removal failure")
	closeFailure := stderrors.New("injected root close failure")
	deps := defaultPrivateBundleStageDependencies()
	defaultRemoveAll := deps.removeAll
	defaultCloseRoot := deps.closeRoot
	var removeCalls atomic.Int64
	deps.removeAll = func(root *os.Root, name string) error {
		removeCalls.Add(1)
		return stderrors.Join(defaultRemoveAll(root, name), removeFailure)
	}
	deps.closeRoot = func(root *os.Root) error {
		rootName := filepath.Clean(root.Name())
		closeErr := defaultCloseRoot(root)
		if rootName == filepath.Clean(sourceDir) {
			return closeErr
		}
		return stderrors.Join(closeErr, closeFailure)
	}

	stagedDir, staged, cleanup, err := stageVerifiedBundleWithDependencies(
		context.Background(), sourceDir, InventoryOptions{}, deps)
	if err != nil {
		t.Fatalf("stageVerifiedBundleWithDependencies() error = %v", err)
	}
	if stagedDir == "" || staged == nil || cleanup == nil {
		t.Fatalf("successful stage results = (%q, %#v, %p), want populated", stagedDir, staged, cleanup)
	}

	const callers = 16
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for index := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[index] = cleanup()
		}()
	}
	wg.Wait()
	for index, cleanupErr := range errs {
		if !stderrors.Is(cleanupErr, removeFailure) || !stderrors.Is(cleanupErr, closeFailure) {
			t.Errorf("cleanup[%d] error = %v, want joined injected failures", index, cleanupErr)
		}
		if !stderrors.Is(cleanupErr, errs[0]) || !sameErrorInstance(cleanupErr, errs[0]) {
			t.Errorf("cleanup[%d] returned a different cached error instance", index)
		}
	}
	if got := removeCalls.Load(); got != 1 {
		t.Errorf("anchored removal calls = %d, want 1", got)
	}
	if _, statErr := os.Stat(stagedDir); !os.IsNotExist(statErr) {
		t.Errorf("cleanup did not remove stage: %v", statErr)
	}
}

func TestStageVerifiedBundle_StagingFailureJoinsCleanupFailure(t *testing.T) {
	sourceDir := t.TempDir()
	payloadPath := filepath.Join(sourceDir, "payload.txt")
	writeTestBundle(t, sourceDir, map[string]testFile{
		"payload.txt": {content: []byte("verified payload"), mode: 0600},
	})
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)

	cleanupFailure := stderrors.New("injected stage cleanup failure")
	deps := defaultPrivateBundleStageDependencies()
	defaultRemoveAll := deps.removeAll
	deps.beforeChildRevalidate = func(string, string) error {
		return os.WriteFile(payloadPath, []byte("tampered payload"), 0600)
	}
	deps.removeAll = func(root *os.Root, name string) error {
		return stderrors.Join(defaultRemoveAll(root, name), cleanupFailure)
	}

	stagedDir, staged, cleanup, err := stageVerifiedBundleWithDependencies(
		context.Background(), sourceDir, InventoryOptions{}, deps)
	requireInventoryErrorCode(t, err, errors.ErrCodeInvalidRequest)
	if !stderrors.Is(err, cleanupFailure) {
		t.Errorf("StageVerifiedBundle() error = %v, want joined cleanup failure", err)
	}
	if stagedDir != "" || staged != nil || cleanup != nil {
		t.Fatalf("failed stage results = (%q, %#v, %p), want empty/nil/nil", stagedDir, staged, cleanup)
	}
	assertNoStageResidue(t, tempRoot)
}

func TestStageVerifiedBundle_ChildLstatCleanup(t *testing.T) {
	sourceDir := t.TempDir()
	checksumData := writeTestBundle(t, sourceDir, map[string]testFile{
		"payload.txt": {content: []byte("verified payload"), mode: 0600},
	})
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)

	lstatFailure := stderrors.New("injected initial child lstat failure")
	deps := defaultPrivateBundleStageDependencies()
	defaultRemoveAll := deps.removeAll
	var lstatCalls atomic.Int64
	var removeCalls atomic.Int64
	deps.childLstat = func(root *os.Root, name string) (os.FileInfo, error) {
		lstatCalls.Add(1)
		return nil, lstatFailure
	}
	deps.removeAll = func(root *os.Root, name string) error {
		removeCalls.Add(1)
		return defaultRemoveAll(root, name)
	}

	stagedDir, staged, cleanup, err := stageVerifiedBundleWithDependencies(
		context.Background(), sourceDir, InventoryOptions{}, deps)
	requireInventoryErrorCode(t, err, errors.ErrCodeInternal)
	if !stderrors.Is(err, lstatFailure) {
		t.Errorf("StageVerifiedBundle() error = %v, want injected lstat cause", err)
	}
	if stagedDir != "" || staged != nil || cleanup != nil {
		t.Fatalf("failed stage results = (%q, %#v, %p), want empty/nil/nil", stagedDir, staged, cleanup)
	}
	if got := removeCalls.Load(); got != 1 {
		t.Errorf("anchored child removals = %d, want 1", got)
	}
	if got := lstatCalls.Load(); got != 1 {
		t.Errorf("child lstat calls = %d, want 1 persistent failure without cleanup retry", got)
	}
	assertVerifiedBundleUnchanged(t, sourceDir, checksumData)
	assertNoStageResidue(t, tempRoot)
}

func TestPrivateBundleStageAllocation_CleanupUnidentifiedChild(t *testing.T) {
	tempRoot := t.TempDir()
	const childName = "aicr-bundle-stage-unidentified"
	if err := os.Mkdir(filepath.Join(tempRoot, childName), 0700); err != nil {
		t.Fatalf("Mkdir(stage) error = %v", err)
	}
	root, err := os.OpenRoot(tempRoot)
	if err != nil {
		t.Fatalf("OpenRoot(temp) error = %v", err)
	}
	t.Cleanup(func() {
		if closeErr := root.Close(); closeErr != nil {
			t.Errorf("Close(temp root) error = %v", closeErr)
		}
	})

	lstatFailure := stderrors.New("injected persistent child lstat failure")
	deps := defaultPrivateBundleStageDependencies()
	defaultRemoveAll := deps.removeAll
	var lstatCalls atomic.Int64
	var removeCalls atomic.Int64
	deps.childLstat = func(*os.Root, string) (os.FileInfo, error) {
		lstatCalls.Add(1)
		return nil, lstatFailure
	}
	deps.removeAll = func(root *os.Root, name string) error {
		removeCalls.Add(1)
		return defaultRemoveAll(root, name)
	}
	allocation := &privateBundleStageAllocation{
		temp:       &stableDirectoryRoot{root: root},
		childName:  childName,
		childOwned: true,
		deps:       deps,
	}

	if cleanupErr := allocation.cleanupChild(); cleanupErr != nil {
		t.Fatalf("cleanupChild() error = %v", cleanupErr)
	}
	if got := lstatCalls.Load(); got != 0 {
		t.Errorf("cleanup child lstat calls = %d, want 0 without an established identity", got)
	}
	if got := removeCalls.Load(); got != 1 {
		t.Errorf("anchored child removals = %d, want 1", got)
	}
	assertNoStageResidue(t, tempRoot)
}

func TestStageVerifiedBundle_SourceSwapAfterOpen(t *testing.T) {
	sourceParent := t.TempDir()
	sourceDir := filepath.Join(sourceParent, "source")
	if err := os.Mkdir(sourceDir, 0700); err != nil {
		t.Fatalf("Mkdir(source) error = %v", err)
	}
	checksumData := writeTestBundle(t, sourceDir, map[string]testFile{
		"payload.txt": {content: []byte("verified payload"), mode: 0600},
	})
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)

	originalDir := filepath.Join(sourceParent, "original")
	replacementMarker := filepath.Join(sourceDir, "replacement-marker")
	deps := defaultPrivateBundleStageDependencies()
	var hookCalls atomic.Int64
	deps.beforeChildCreate = func() error {
		hookCalls.Add(1)
		if err := os.Rename(sourceDir, originalDir); err != nil {
			return err
		}
		if err := os.Mkdir(sourceDir, 0700); err != nil {
			return err
		}
		return os.WriteFile(replacementMarker, []byte("replacement"), 0600)
	}

	stagedDir, staged, cleanup, err := stageVerifiedBundleWithDependencies(
		context.Background(), sourceDir, InventoryOptions{}, deps)
	requireInventoryErrorCode(t, err, errors.ErrCodeInvalidRequest)
	if stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Errorf("source swap error = %v, must not be classified ErrCodeInternal", err)
	}
	if stagedDir != "" || staged != nil || cleanup != nil {
		t.Fatalf("failed stage results = (%q, %#v, %p), want empty/nil/nil", stagedDir, staged, cleanup)
	}
	if got := hookCalls.Load(); got != 1 {
		t.Errorf("before-child-create hook calls = %d, want 1", got)
	}
	assertVerifiedBundleUnchanged(t, originalDir, checksumData)
	assertReplacementFileContents(t, replacementMarker)
	replacementEntries, readErr := os.ReadDir(sourceDir)
	if readErr != nil {
		t.Fatalf("ReadDir(replacement source) error = %v", readErr)
	}
	if len(replacementEntries) != 1 || replacementEntries[0].Name() != filepath.Base(replacementMarker) {
		t.Errorf("replacement source entries = %v, want only replacement marker", replacementEntries)
	}
	assertNoStageResidue(t, tempRoot)
}

func TestStageVerifiedBundle_SourceCloseFailureCleanup(t *testing.T) {
	sourceDir := t.TempDir()
	checksumData := writeTestBundle(t, sourceDir, map[string]testFile{
		"payload.txt": {content: []byte("verified payload"), mode: 0600},
	})
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)

	closeFailure := stderrors.New("injected source root close failure")
	deps := defaultPrivateBundleStageDependencies()
	defaultCloseRoot := deps.closeRoot
	var sourceCloseCalls atomic.Int64
	deps.closeRoot = func(root *os.Root) error {
		closeErr := defaultCloseRoot(root)
		if filepath.Clean(root.Name()) == filepath.Clean(sourceDir) {
			sourceCloseCalls.Add(1)
			return stderrors.Join(closeErr, closeFailure)
		}
		return closeErr
	}

	stagedDir, staged, cleanup, err := stageVerifiedBundleWithDependencies(
		context.Background(), sourceDir, InventoryOptions{}, deps)
	requireInventoryErrorCode(t, err, errors.ErrCodeInternal)
	if !stderrors.Is(err, closeFailure) {
		t.Errorf("StageVerifiedBundle() error = %v, want source close cause", err)
	}
	if stagedDir != "" || staged != nil || cleanup != nil {
		t.Fatalf("failed stage results = (%q, %#v, %p), want empty/nil/nil", stagedDir, staged, cleanup)
	}
	if got := sourceCloseCalls.Load(); got != 1 {
		t.Errorf("source root close calls = %d, want 1", got)
	}
	assertVerifiedBundleUnchanged(t, sourceDir, checksumData)
	assertNoStageResidue(t, tempRoot)
}

func assertVerifiedBundleUnchanged(t *testing.T, sourceDir string, wantChecksumData []byte) {
	t.Helper()
	_, inventory, checksumData, err := ReadAndVerifyBundle(context.Background(), sourceDir, InventoryOptions{})
	if err != nil {
		t.Fatalf("ReadAndVerifyBundle(%q) error = %v", sourceDir, err)
	}
	if inventory == nil || len(inventory.RelativeFiles()) == 0 {
		t.Fatalf("ReadAndVerifyBundle(%q) returned empty inventory", sourceDir)
	}
	if !reflect.DeepEqual(checksumData, wantChecksumData) {
		t.Errorf("bundle checksum bytes changed: got %q, want %q", checksumData, wantChecksumData)
	}
}

func assertReplacementFileContents(t *testing.T, path string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", path, err)
	}
	if string(got) != "replacement" {
		t.Errorf("ReadFile(%q) = %q, want replacement", path, got)
	}
}

func sameErrorInstance(got, want error) bool {
	return reflect.ValueOf(got).Pointer() == reflect.ValueOf(want).Pointer()
}

func assertNoStageResidue(t *testing.T, root string) {
	t.Helper()
	residue, err := filepath.Glob(filepath.Join(root, "aicr-bundle-stage-*"))
	if err != nil {
		t.Fatalf("Glob(stage residue) error = %v", err)
	}
	if len(residue) != 0 {
		t.Errorf("stage residue = %v, want none", residue)
	}
}

func TestStageVerifiedBundle_RejectsMetadataDigestMismatch(t *testing.T) {
	t.Parallel()

	const metadataPath = "attestation/bundle-attestation.sigstore.json"
	source := &Inventory{
		files: []string{metadataPath},
		verified: map[string]verifiedFile{
			metadataPath: {digest: [sha256.Size]byte{1}},
		},
	}
	staged := &Inventory{
		files: []string{metadataPath},
		verified: map[string]verifiedFile{
			metadataPath: {digest: [sha256.Size]byte{2}},
		},
	}

	err := compareInventoryDigests(source, staged)
	requireInventoryErrorCode(t, err, errors.ErrCodeInvalidRequest)
}

func TestStageVerifiedBundle_RejectsStageInsideSource(t *testing.T) {
	tests := []struct {
		name    string
		tempDir func(t *testing.T, sourceDir string) (string, string)
	}{
		{
			name: "direct source directory",
			tempDir: func(_ *testing.T, sourceDir string) (string, string) {
				return sourceDir, sourceDir
			},
		},
		{
			name: "symlink alias of source directory",
			tempDir: func(t *testing.T, sourceDir string) (string, string) {
				t.Helper()
				alias := filepath.Join(t.TempDir(), "source-alias")
				if err := os.Symlink(sourceDir, alias); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return alias, sourceDir
			},
		},
		{
			name: "symlink alias of nested source directory",
			tempDir: func(t *testing.T, sourceDir string) (string, string) {
				t.Helper()
				target := filepath.Join(sourceDir, "nested")
				alias := filepath.Join(t.TempDir(), "nested-alias")
				if err := os.Symlink(target, alias); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return alias, target
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sourceDir := t.TempDir()
			writeTestBundle(t, sourceDir, map[string]testFile{
				"nested/payload.txt": {content: []byte("payload"), mode: 0600},
			})
			tempDir, residueDir := tt.tempDir(t, sourceDir)
			t.Setenv("TMPDIR", tempDir)

			stagedDir, staged, cleanup, err := StageVerifiedBundle(
				context.Background(), sourceDir, InventoryOptions{})
			requireInventoryErrorCode(t, err, errors.ErrCodeInternal)
			if stagedDir != "" {
				t.Errorf("StageVerifiedBundle() dir = %q, want empty", stagedDir)
			}
			if staged != nil {
				t.Errorf("StageVerifiedBundle() inventory = %#v, want nil", staged)
			}
			if cleanup != nil {
				t.Fatal("StageVerifiedBundle() cleanup is non-nil after rejected stage placement")
			}
			residue, globErr := filepath.Glob(filepath.Join(residueDir, "aicr-bundle-stage-*"))
			if globErr != nil {
				t.Fatalf("Glob() error = %v", globErr)
			}
			if len(residue) != 0 {
				t.Errorf("stage residue = %v, want none", residue)
			}
		})
	}
}

func TestValidateOutputRoot(t *testing.T) {
	t.Parallel()

	t.Run("real directory with ordinary content succeeds", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		writeTestFile(t, dir, "nested/payload.txt", []byte("payload"), 0600)
		if err := ValidateOutputRoot(context.Background(), dir); err != nil {
			t.Fatalf("ValidateOutputRoot() error = %v", err)
		}
	})

	invalidTests := []struct {
		name  string
		setup func(t *testing.T) string
	}{
		{
			name: "symlink root",
			setup: func(t *testing.T) string {
				target := t.TempDir()
				link := filepath.Join(t.TempDir(), "root-link")
				if err := os.Symlink(target, link); err != nil {
					t.Fatalf("Symlink() error = %v", err)
				}
				return link
			},
		},
		{
			name: "nested symlink",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				if err := os.Symlink(t.TempDir(), filepath.Join(dir, "linked")); err != nil {
					t.Fatalf("Symlink() error = %v", err)
				}
				return dir
			},
		},
		{
			name: "fifo",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				if err := unix.Mkfifo(filepath.Join(dir, "pipe"), 0600); err != nil {
					t.Skipf("FIFO unsupported: %v", err)
				}
				return dir
			},
		},
		{
			name: "unix socket",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				listener, err := net.Listen("unix", filepath.Join(dir, "socket"))
				if err != nil {
					t.Skipf("Unix socket unsupported: %v", err)
				}
				t.Cleanup(func() {
					if err := listener.Close(); err != nil {
						t.Errorf("listener.Close() error = %v", err)
					}
				})
				return dir
			},
		},
		{
			name: "device",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				if err := unix.Mknod(filepath.Join(dir, "device"), unix.S_IFCHR|0600, 0); err != nil {
					t.Skipf("device creation unsupported: %v", err)
				}
				return dir
			},
		},
	}

	for _, tt := range invalidTests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := tt.setup(t)
			err := ValidateOutputRoot(context.Background(), dir)
			requireInventoryErrorCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}

	t.Run("context canceled", func(t *testing.T) {
		t.Parallel()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := ValidateOutputRoot(ctx, t.TempDir())
		requireInventoryErrorCode(t, err, errors.ErrCodeTimeout)
	})
}

type testFile struct {
	content []byte
	mode    os.FileMode
}

func writeTestBundle(t *testing.T, dir string, files map[string]testFile) []byte {
	t.Helper()

	contents := make(map[string][]byte, len(files))
	for rel, file := range files {
		mode := file.mode
		if mode == 0 {
			mode = 0600
		}
		writeTestFile(t, dir, rel, file.content, mode)
		contents[filepath.ToSlash(rel)] = file.content
	}
	return writeTestManifest(t, dir, contents)
}

func writeTestFile(t *testing.T, dir, rel string, content []byte, mode os.FileMode) {
	t.Helper()

	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(full, content, mode); err != nil {
		t.Fatalf("WriteFile() %q error = %v", rel, err)
	}
}

func writeTestManifest(t *testing.T, dir string, contents map[string][]byte) []byte {
	t.Helper()

	paths := make([]string, 0, len(contents))
	for rel := range contents {
		paths = append(paths, rel)
	}
	sort.Strings(paths)
	entries := make([]Entry, 0, len(paths))
	for _, rel := range paths {
		digest := sha256.Sum256(contents[rel])
		entries = append(entries, Entry{Digest: hex.EncodeToString(digest[:]), Path: rel})
	}
	manifest := &Manifest{entries: entries}
	data, err := manifest.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ChecksumFileName), data, 0600); err != nil {
		t.Fatalf("WriteFile() checksums error = %v", err)
	}
	return data
}

func requireInventoryErrorCode(t *testing.T, err error, code errors.ErrorCode) {
	t.Helper()

	if err == nil {
		t.Fatalf("expected %s error, got nil", code)
	}
	if !stderrors.Is(err, errors.New(code, "")) {
		t.Errorf("error = %v, want code %s", err, code)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func leftPadNumber(n int) string {
	return fmt.Sprintf("%03d.txt", n)
}

type cancelAfterErrContext struct {
	context.Context
	cancelAfter int64
	calls       atomic.Int64
}

func (c *cancelAfterErrContext) Err() error {
	if c.calls.Add(1) > c.cancelAfter {
		return context.Canceled
	}
	return nil
}
