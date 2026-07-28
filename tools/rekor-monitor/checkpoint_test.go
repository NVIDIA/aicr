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

package main

import (
	"archive/zip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// writeZip creates a zip at path containing the given name->content entries.
func writeZip(t *testing.T, path string, entries map[string]string) {
	t.Helper()
	f, err := os.Create(path) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("create zip: %v", err)
	}
	zw := zip.NewWriter(f)
	for name, content := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create entry: %v", err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("zip write entry: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("file close: %v", err)
	}
}

func TestCheckpointStoreRead(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file is first run", func(t *testing.T) {
		cp, err := checkpointStore{path: filepath.Join(dir, "nope.txt")}.read()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cp != nil {
			t.Errorf("checkpoint = %v, want nil", cp)
		}
	})

	t.Run("empty file is first run", func(t *testing.T) {
		p := filepath.Join(dir, "empty.txt")
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		cp, err := checkpointStore{path: p}.read()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cp != nil {
			t.Errorf("checkpoint = %v, want nil", cp)
		}
	})

	t.Run("malformed file errors", func(t *testing.T) {
		p := filepath.Join(dir, "bad.txt")
		if err := os.WriteFile(p, []byte("not a checkpoint"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := (checkpointStore{path: p}).read(); err == nil {
			t.Error("expected error for malformed checkpoint, got nil")
		}
	})
}

func TestCheckpointStoreRestore(t *testing.T) {
	dir := t.TempDir()

	t.Run("no restore-zip is a no-op", func(t *testing.T) {
		dest := filepath.Join(dir, "noop.txt")
		if err := (checkpointStore{path: dest}).restore(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Error("dest should not exist when no restore-zip is given")
		}
	})

	t.Run("missing zip is first run", func(t *testing.T) {
		dest := filepath.Join(dir, "first.txt")
		s := checkpointStore{path: dest, restoreZip: filepath.Join(dir, "absent.zip")}
		if err := s.restore(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Error("dest should not exist when the artifact zip is absent (first run)")
		}
	})

	t.Run("present zip restores the checkpoint", func(t *testing.T) {
		dest := filepath.Join(dir, "restored.txt")
		zp := filepath.Join(dir, "present.zip")
		writeZip(t, zp, map[string]string{"restored.txt": "origin\n7\nh\n"})
		s := checkpointStore{path: dest, restoreZip: zp}
		if err := s.restore(context.Background()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatalf("read dest: %v", err)
		}
		if string(got) != "origin\n7\nh\n" {
			t.Errorf("dest content = %q", string(got))
		}
	})
}

func TestExtractCheckpointFromZip(t *testing.T) {
	tests := []struct {
		name    string
		entries map[string]string // written via writeZip when rawZip is nil
		rawZip  []byte            // raw bytes to write instead (for the corrupt case)
		want    string            // expected dest content when no error
		wantErr bool
	}{
		{
			name:    "entry matching dest name",
			entries: map[string]string{"checkpoint_v2.txt": "origin\n42\nhash\n", "other.txt": "ignored"},
			want:    "origin\n42\nhash\n",
		},
		{
			name:    "sole entry with different name",
			entries: map[string]string{"whatever.txt": "solo"},
			want:    "solo",
		},
		{
			name:    "no match among multiple entries",
			entries: map[string]string{"a.txt": "a", "b.txt": "b"},
			wantErr: true,
		},
		{
			name:    "empty entry",
			entries: map[string]string{"checkpoint_v2.txt": ""},
			wantErr: true,
		},
		{
			name:    "oversized entry rejected by size guard",
			entries: map[string]string{"checkpoint_v2.txt": strings.Repeat("x", maxCheckpointBytes+1)},
			wantErr: true,
		},
		{
			name:    "corrupt zip",
			rawZip:  []byte("not a zip"),
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "checkpoint_v2.txt")
			zp := filepath.Join(dir, "in.zip")
			if tt.rawZip != nil {
				if err := os.WriteFile(zp, tt.rawZip, 0o600); err != nil {
					t.Fatalf("write raw zip: %v", err)
				}
			} else {
				writeZip(t, zp, tt.entries)
			}

			err := extractCheckpointFromZip(context.Background(), zp, dest)
			if (err != nil) != tt.wantErr {
				t.Fatalf("extractCheckpointFromZip() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			got, rerr := os.ReadFile(dest)
			if rerr != nil {
				t.Fatalf("read dest: %v", rerr)
			}
			if string(got) != tt.want {
				t.Errorf("dest content = %q, want %q", string(got), tt.want)
			}
		})
	}
}

func TestExtractCheckpointFromZipOversizedArchive(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "big.zip")
	// Sparse file larger than maxArchiveBytes; the pre-open size guard rejects it
	// before any parse, so it need not be a valid zip.
	f, err := os.Create(zp)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := f.Truncate(maxArchiveBytes + 1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := extractCheckpointFromZip(context.Background(), zp, filepath.Join(dir, "cp.txt")); err == nil {
		t.Error("expected error for an oversized archive")
	}
}

func TestExtractCheckpointFromZipNestedDest(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "in.zip")
	writeZip(t, zp, map[string]string{"checkpoint_v2.txt": "origin\n5\nh\n"})
	dest := filepath.Join(dir, "a", "b", "checkpoint_v2.txt") // parent dirs do not exist yet
	if err := extractCheckpointFromZip(context.Background(), zp, dest); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != "origin\n5\nh\n" {
		t.Errorf("dest content = %q", string(got))
	}
}

func TestExtractCheckpointFromZipCanceled(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "in.zip")
	// Multiple entries so the member-scan loop runs and observes cancellation.
	writeZip(t, zp, map[string]string{"a.txt": "a", "b.txt": "b"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-canceled
	if err := extractCheckpointFromZip(ctx, zp, filepath.Join(dir, "cp.txt")); err == nil {
		t.Error("expected a cancellation error")
	}
}

// TestCheckpointStoreRestoreProgress verifies the scan-progress companion round
// trips through the artifact zip: when present it is restored (so the next run
// resumes), and when absent restore is a no-op (progress reads as 0).
func TestCheckpointStoreRestoreProgress(t *testing.T) {
	t.Run("companion present is restored", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "restored.txt")
		zp := filepath.Join(dir, "artifact.zip")
		writeZip(t, zp, map[string]string{
			"restored.txt":       "origin\n7\nh\n",
			"restored.txt.scan":  "424242\n",
			"restored.txt.stall": "5000 2 4\n",
		})
		s := checkpointStore{path: dest, restoreZip: zp}
		if err := s.restore(context.Background()); err != nil {
			t.Fatalf("restore: %v", err)
		}
		n, err := s.readProgress()
		if err != nil {
			t.Fatalf("readProgress: %v", err)
		}
		if n != 424242 {
			t.Errorf("restored progress = %d, want 424242", n)
		}
		tr, err := s.readScanTrend()
		if err != nil {
			t.Fatalf("readScanTrend: %v", err)
		}
		if tr != (scanTrend{5000, 2, 4}) {
			t.Errorf("restored trend = %+v, want {5000 2 4}", tr)
		}
	})

	t.Run("companion absent leaves progress at 0", func(t *testing.T) {
		dir := t.TempDir()
		dest := filepath.Join(dir, "restored.txt")
		zp := filepath.Join(dir, "artifact.zip")
		writeZip(t, zp, map[string]string{"restored.txt": "origin\n7\nh\n"})
		s := checkpointStore{path: dest, restoreZip: zp}
		if err := s.restore(context.Background()); err != nil {
			t.Fatalf("restore: %v", err)
		}
		n, err := s.readProgress()
		if err != nil {
			t.Fatalf("readProgress: %v", err)
		}
		if n != 0 {
			t.Errorf("progress = %d, want 0 (no companion)", n)
		}
	})
}

// TestAdvanceCheckpointResetsProgressBeforeWrite pins advanceCheckpoint's
// fail-closed ordering: progress and the trend are reset BEFORE the signed
// checkpoint is written, so a failed checkpoint write can never leave the
// dangerous (new checkpoint, stale old-window progress) pair on disk. Forced by
// making the checkpoint path a directory, so store.write fails while the
// sibling .scan/.stall companions remain writable.
func TestAdvanceCheckpointResetsProgressBeforeWrite(t *testing.T) {
	dir := t.TempDir()
	cpDir := filepath.Join(dir, "cp") // a directory AT the checkpoint path -> store.write fails
	if err := os.Mkdir(cpDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	store := checkpointStore{path: cpDir}
	if err := store.writeProgress(123456); err != nil { // stale mid-window progress
		t.Fatalf("seed progress: %v", err)
	}
	if err := store.writeScanTrend(scanTrend{bestRemaining: 500, stall: 2, passes: 5}); err != nil {
		t.Fatalf("seed trend: %v", err)
	}
	if err := advanceCheckpoint(store, testCheckpoint(100), testCheckpoint(200)); err == nil {
		t.Fatal("advanceCheckpoint() = nil, want the checkpoint write to fail (path is a directory)")
	}
	if n, _ := store.readProgress(); n != 0 {
		t.Errorf("progress = %d, want 0 (reset before the failed checkpoint write)", n)
	}
	if tr, _ := store.readScanTrend(); tr != (scanTrend{}) {
		t.Errorf("trend = %+v, want zero (reset before the failed checkpoint write)", tr)
	}
}

// TestRealMainRestoresOnceAcrossRetries pins the restore hoist: the artifact is
// restored a single time in realMain, so an operational retry resumes from the
// progress the failed attempt saved rather than re-restoring the (older) archived
// value. Re-restoring per attempt (the round-1 clobber bug) would make attempt 2
// read 100 again instead of 150.
func TestRealMainRestoresOnceAcrossRetries(t *testing.T) {
	dir := t.TempDir()
	cp := filepath.Join(dir, "cp.txt")
	zp := filepath.Join(dir, "artifact.zip")
	writeZip(t, zp, map[string]string{
		"cp.txt":      "checkpoint-bytes\n", // restore only extracts; content is not parsed here
		"cp.txt.scan": "100\n",
	})
	var reads []int64
	attempt := 0
	fn := func(_ context.Context, opts options, _ io.Writer) error {
		attempt++
		s := checkpointStore{path: opts.checkpointFile}
		n, err := s.readProgress()
		if err != nil {
			t.Fatalf("attempt %d readProgress: %v", attempt, err)
		}
		reads = append(reads, n)
		if attempt == 1 {
			if werr := s.writeProgress(n + 50); werr != nil {
				t.Fatalf("writeProgress: %v", werr)
			}
			return errors.New(errors.ErrCodeUnavailable, "blip")
		}
		return nil
	}
	if code := realMain([]string{"--file", cp, "--restore-zip", zp}, fn, io.Discard); code != 0 {
		t.Errorf("realMain() = %d, want 0", code)
	}
	if len(reads) != 2 || reads[0] != 100 || reads[1] != 150 {
		t.Errorf("progress reads across attempts = %v, want [100 150] (restored once, no clobber)", reads)
	}
}
