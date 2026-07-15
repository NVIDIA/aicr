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
	"bytes"
	"context"
	stderrors "errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sigstore/rekor-monitor/pkg/identity"
	tlog "github.com/transparency-dev/formats/log"
)

// TestObserveShardRotation covers the yearly shard rotation path: prev and cur
// are different logs, so the identity scan is skipped, the rotation is reported,
// and the cursor advances to the new shard.
func TestObserveShardRotation(t *testing.T) {
	store := checkpointStore{path: filepath.Join(t.TempDir(), "cp.txt")}
	if err := store.write(nil, &tlog.Checkpoint{Origin: "log2025-1", Size: 100, Hash: make([]byte, 32)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	f := &fakeMonitor{watch: true, cur: &tlog.Checkpoint{Origin: "log2026-1", Size: 5, Hash: make([]byte, 32)}}

	var buf bytes.Buffer
	if err := observe(context.Background(), f, store, &buf); err != nil {
		t.Fatalf("observe() error = %v", err)
	}
	if f.scanned {
		t.Error("identity scan must be skipped across a shard rotation")
	}
	if !strings.Contains(buf.String(), "shard rotation") {
		t.Errorf("expected a shard-rotation report, got: %q", buf.String())
	}
	cp, err := store.read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if cp == nil || cp.Origin != "log2026-1" {
		t.Errorf("cursor = %v, want advanced to the new shard", cp)
	}
}

// TestRunPropagatesRestoreError exercises run()'s early exit: a bad restore-zip
// fails before any network call (newMonitor), so run returns without contacting
// TUF or Rekor.
func TestRunPropagatesRestoreError(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "corrupt.zip")
	if err := os.WriteFile(zp, []byte("not a zip"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	opts := options{checkpointFile: filepath.Join(dir, "cp.txt"), restoreZip: zp}
	if err := run(context.Background(), opts, io.Discard); err == nil {
		t.Error("run should propagate a restore failure before contacting the network")
	}
}

// fakeMonitor is a monitorChecks stub so observe() can be tested without any
// network access.
type fakeMonitor struct {
	watch   bool
	cur     *tlog.Checkpoint
	consErr error
	found   []identity.MonitoredIdentity
	failed  []identity.FailedLogEntry
	scanErr error
	scanned bool // set when scanIdentity is invoked
}

func (f *fakeMonitor) watchesIdentity() bool { return f.watch }

func (f *fakeMonitor) checkConsistency(_ context.Context, _ *tlog.Checkpoint) (*tlog.Checkpoint, error) {
	return f.cur, f.consErr
}

func (f *fakeMonitor) scanIdentity(_ context.Context, _, _ int64) ([]identity.MonitoredIdentity, []identity.FailedLogEntry, error) {
	f.scanned = true
	return f.found, f.failed, f.scanErr
}

func testCheckpoint(size uint64) *tlog.Checkpoint {
	return &tlog.Checkpoint{Origin: "test.rekor.sigstore.dev", Size: size, Hash: make([]byte, 32)}
}

// seedCheckpoint writes a prior checkpoint so store.read() returns it (i.e. not
// a first run). It also exercises checkpointStore.write.
func seedCheckpoint(t *testing.T, store checkpointStore, size uint64) {
	t.Helper()
	if err := store.write(nil, testCheckpoint(size)); err != nil {
		t.Fatalf("seed checkpoint: %v", err)
	}
}

func TestObserve(t *testing.T) {
	boom := stderrors.New("boom")

	tests := []struct {
		name      string
		seedPrev  uint64 // 0 => first run (no prior checkpoint)
		fake      fakeMonitor
		wantErr   bool
		wantScan  bool   // whether scanIdentity should have run
		wantAdvTo uint64 // checkpoint size expected on disk after the pass (0 => unchanged/absent)
	}{
		{
			name:      "first run baselines and skips scan",
			seedPrev:  0,
			fake:      fakeMonitor{watch: true, cur: testCheckpoint(100)},
			wantScan:  false,
			wantAdvTo: 100,
		},
		{
			name:      "consistency-only advances without scanning",
			seedPrev:  100,
			fake:      fakeMonitor{watch: false, cur: testCheckpoint(150)},
			wantScan:  false,
			wantAdvTo: 150,
		},
		{
			name:      "clean identity scan advances",
			seedPrev:  100,
			fake:      fakeMonitor{watch: true, cur: testCheckpoint(150)},
			wantScan:  true,
			wantAdvTo: 150,
		},
		{
			name:      "identity finding returns error and does NOT advance",
			seedPrev:  100,
			fake:      fakeMonitor{watch: true, cur: testCheckpoint(150), found: []identity.MonitoredIdentity{{}}},
			wantErr:   true,
			wantScan:  true,
			wantAdvTo: 100, // must not advance past a finding (sticky until triaged)
		},
		{
			name:      "consistency break does not advance",
			seedPrev:  100,
			fake:      fakeMonitor{watch: true, consErr: boom},
			wantErr:   true,
			wantScan:  false,
			wantAdvTo: 100,
		},
		{
			name:      "scan error does not advance",
			seedPrev:  100,
			fake:      fakeMonitor{watch: true, cur: testCheckpoint(150), scanErr: boom},
			wantErr:   true,
			wantScan:  true,
			wantAdvTo: 100,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := checkpointStore{path: filepath.Join(t.TempDir(), "cp.txt")}
			if tt.seedPrev > 0 {
				seedCheckpoint(t, store, tt.seedPrev)
			}
			f := tt.fake // copy so scanned is per-subtest

			err := observe(context.Background(), &f, store, os.Stdout)
			if (err != nil) != tt.wantErr {
				t.Fatalf("observe() error = %v, wantErr %v", err, tt.wantErr)
			}
			if f.scanned != tt.wantScan {
				t.Errorf("scanIdentity called = %v, want %v", f.scanned, tt.wantScan)
			}
			cp, rerr := store.read()
			if rerr != nil {
				t.Fatalf("read after observe: %v", rerr)
			}
			if tt.wantAdvTo == 0 {
				if cp != nil {
					t.Errorf("checkpoint = %v, want none", cp)
				}
			} else if cp == nil || cp.Size != tt.wantAdvTo {
				t.Errorf("checkpoint size = %v, want %d", cp, tt.wantAdvTo)
			}
		})
	}
}
