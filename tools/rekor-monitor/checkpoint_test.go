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
	"os"
	"path/filepath"
	"strings"
	"testing"
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
