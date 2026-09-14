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
	"time"

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
	if err := observe(context.Background(), f, store, nil, &buf); err != nil {
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

// TestObserveShardRotationReportsAbandonedProgress covers the loud-report path:
// when a catch-up scan is mid-window at a rotation, the abandoned prior-shard
// cursor is surfaced with its index rather than silently dropped.
func TestObserveShardRotationReportsAbandonedProgress(t *testing.T) {
	store := checkpointStore{path: filepath.Join(t.TempDir(), "cp.txt")}
	if err := store.write(nil, &tlog.Checkpoint{Origin: "log2025-1", Size: 100, Hash: make([]byte, 32)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.writeProgress(37_500); err != nil { // a mid-window prior-shard cursor
		t.Fatalf("seed progress: %v", err)
	}
	f := &fakeMonitor{watch: true, cur: &tlog.Checkpoint{Origin: "log2026-1", Size: 5, Hash: make([]byte, 32)}}

	var buf bytes.Buffer
	if err := observe(context.Background(), f, store, nil, &buf); err != nil {
		t.Fatalf("observe() error = %v", err)
	}
	if !strings.Contains(buf.String(), "reached index 37500") {
		t.Errorf("expected the abandoned prior-shard index in the report, got: %q", buf.String())
	}
}

// TestObserveProgressBeyondWindowFailsClosed covers the fail-closed guard: a
// persisted progress strictly greater than the window end is impossible on an
// append-only same-shard log, so observe refuses to advance rather than reporting
// clean with entries left unscanned.
func TestObserveProgressBeyondWindowFailsClosed(t *testing.T) {
	store := checkpointStore{path: filepath.Join(t.TempDir(), "cp.txt")}
	seedCheckpoint(t, store, 100)                    // window (99, 149]
	if err := store.writeProgress(200); err != nil { // > window end index 149
		t.Fatalf("seed progress: %v", err)
	}
	f := &fakeMonitor{watch: true, cur: testCheckpoint(150)}
	err := observe(context.Background(), f, store, nil, io.Discard)
	if err == nil {
		t.Fatal("observe() error = nil, want a refuse-to-advance error")
	}
	if !strings.Contains(err.Error(), "exceeds window end") {
		t.Errorf("error = %v, want it to mention exceeding the window end", err)
	}
	if f.scanned {
		t.Error("scanIdentity must not run when progress is beyond the window")
	}
	if cp, _ := store.read(); cp == nil || cp.Size != 100 {
		t.Errorf("checkpoint = %v, want held at 100 (must not advance)", cp)
	}
}

// TestObserveRejectsNonPositiveChunkSize covers the loop-invariant guard: a
// non-positive chunk size can never advance the scan, so observe fails fast
// instead of burning the whole pass deadline.
func TestObserveRejectsNonPositiveChunkSize(t *testing.T) {
	defer withScanBounds(0, 1_000)() // chunkSize = 0
	store := checkpointStore{path: filepath.Join(t.TempDir(), "cp.txt")}
	seedCheckpoint(t, store, 100)
	f := &fakeMonitor{watch: true, cur: testCheckpoint(150)}
	err := observe(context.Background(), f, store, nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "scanChunkSize must be positive") {
		t.Fatalf("observe() error = %v, want a scanChunkSize guard error", err)
	}
}

// TestRealMainPropagatesRestoreError exercises realMain's early exit: a bad
// restore-zip fails in the single up-front restore (before the retry loop and any
// network call), so realMain returns the operational exit code without ever
// invoking the monitor pass.
func TestRealMainPropagatesRestoreError(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "corrupt.zip")
	if err := os.WriteFile(zp, []byte("not a zip"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	called := false
	runFn := func(context.Context, options, io.Writer) error {
		called = true
		return nil
	}
	code := realMain([]string{"--file", filepath.Join(dir, "cp.txt"), "--restore-zip", zp}, runFn, io.Discard)
	if code != 3 {
		t.Errorf("realMain() = %d, want 3 (operational) on a bad restore-zip", code)
	}
	if called {
		t.Error("runFn must not be invoked when the up-front restore fails")
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
	// scanRanges records every (start, end] chunk scanIdentity was called with,
	// so incremental-scan tests can assert chunking, resume, and full coverage.
	scanRanges [][2]int64
	// findAt, when > 0, makes scanIdentity return f.found only for the chunk
	// whose (start, end] range covers findAt (i.e. start < findAt <= end); other
	// chunks return clean. Lets a test place a finding partway through a backlog.
	// The default (0) returns f.found/f.failed for every chunk (prior behavior).
	findAt int64
}

func (f *fakeMonitor) watchesIdentity() bool { return f.watch }

func (f *fakeMonitor) checkConsistency(_ context.Context, _ *tlog.Checkpoint) (*tlog.Checkpoint, error) {
	return f.cur, f.consErr
}

func (f *fakeMonitor) scanIdentity(_ context.Context, start, end int64) ([]identity.MonitoredIdentity, []identity.FailedLogEntry, error) {
	f.scanned = true
	f.scanRanges = append(f.scanRanges, [2]int64{start, end})
	if f.scanErr != nil {
		return nil, nil, f.scanErr
	}
	if f.findAt > 0 {
		if start < f.findAt && f.findAt <= end {
			return f.found, f.failed, nil
		}
		return nil, nil, nil // clean chunk
	}
	return f.found, f.failed, nil
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
		knownTags map[string]bool // nil => suppression disabled (passthrough)
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
			name:     "known release match suppressed -> clean pass advances",
			seedPrev: 100,
			fake: fakeMonitor{watch: true, cur: testCheckpoint(150), found: []identity.MonitoredIdentity{
				{FoundIdentityEntries: []identity.LogEntry{{CertSubject: "https://github.com/NVIDIA/aicr/.github/workflows/on-tag.yaml@refs/tags/v0.18.0-rc1"}}}}},
			knownTags: map[string]bool{"v0.18.0-rc1": true},
			wantErr:   false,
			wantScan:  true,
			wantAdvTo: 150,
		},
		{
			name:     "unknown tag match -> finding does not advance",
			seedPrev: 100,
			fake: fakeMonitor{watch: true, cur: testCheckpoint(150), found: []identity.MonitoredIdentity{
				{FoundIdentityEntries: []identity.LogEntry{{CertSubject: "https://github.com/NVIDIA/aicr/.github/workflows/on-tag.yaml@refs/tags/v9.9.9-attacker"}}}}},
			knownTags: map[string]bool{"v0.18.0-rc1": true},
			wantErr:   true,
			wantScan:  true,
			wantAdvTo: 100,
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

			err := observe(context.Background(), &f, store, tt.knownTags, os.Stdout)
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

// TestObserveIncrementalCatchUp drives observe() across several runs over a
// window larger than one run's cap, asserting the scan resumes from persisted
// progress, the signed checkpoint advances only once fully caught up, and the
// scanned chunks contiguously cover the window.
func TestObserveIncrementalCatchUp(t *testing.T) {
	defer withScanBounds(10, 20)() // chunk=10, cap=20 entries/run

	store := checkpointStore{path: filepath.Join(t.TempDir(), "cp.txt")}
	seedCheckpoint(t, store, 100) // prev.Size=100 -> window start index 99
	const head = 160              // window (99, 159], 60 entries -> 3 runs at 20/run

	var ranges [][2]int64
	for run := 1; run <= 3; run++ {
		f := &fakeMonitor{watch: true, cur: testCheckpoint(head)}
		if err := observe(context.Background(), f, store, nil, os.Stdout); err != nil {
			t.Fatalf("run %d: observe() error = %v", run, err)
		}
		ranges = append(ranges, f.scanRanges...)

		cp, err := store.read()
		if err != nil {
			t.Fatalf("run %d: read checkpoint: %v", run, err)
		}
		prog, err := store.readProgress()
		if err != nil {
			t.Fatalf("run %d: read progress: %v", run, err)
		}
		switch run {
		case 1, 2:
			if cp == nil || cp.Size != 100 {
				t.Errorf("run %d: checkpoint = %v, want held at 100 until caught up", run, cp)
			}
			wantProg := int64(99 + run*20)
			if prog != wantProg {
				t.Errorf("run %d: progress = %d, want %d", run, prog, wantProg)
			}
		case 3:
			if cp == nil || cp.Size != head {
				t.Errorf("run 3: checkpoint = %v, want advanced to %d", cp, head)
			}
			if prog != 0 {
				t.Errorf("run 3: progress = %d, want reset to 0", prog)
			}
		}
	}

	// The chunks must contiguously cover (99, 159] with no gaps or overlaps.
	wantStart := int64(99)
	for i, r := range ranges {
		if r[0] != wantStart {
			t.Fatalf("chunk %d start = %d, want %d (non-contiguous)", i, r[0], wantStart)
		}
		if r[1]-r[0] > 10 {
			t.Fatalf("chunk %d = (%d, %d], larger than chunk size 10", i, r[0], r[1])
		}
		wantStart = r[1]
	}
	if wantStart != head-1 {
		t.Errorf("scan reached %d, want the window end %d", wantStart, head-1)
	}
}

// TestObserveFindingHaltsCatchUp places a finding partway through a large
// window and asserts the scan halts there: it returns a finding error, does not
// advance the signed checkpoint, and does not persist progress past the finding
// chunk (so the finding re-detects until triaged).
func TestObserveFindingHaltsCatchUp(t *testing.T) {
	defer withScanBounds(10, 1_000)() // chunk=10, cap large so one run reaches the finding

	store := checkpointStore{path: filepath.Join(t.TempDir(), "cp.txt")}
	seedCheckpoint(t, store, 100) // window start index 99
	f := &fakeMonitor{
		watch:  true,
		cur:    testCheckpoint(200),
		found:  []identity.MonitoredIdentity{{FoundIdentityEntries: []identity.LogEntry{{CertSubject: "x"}}}},
		findAt: 135, // lands in the (129, 139] chunk
	}
	err := observe(context.Background(), f, store, nil, os.Stdout)
	if err == nil {
		t.Fatal("observe() error = nil, want a finding error")
	}
	cp, _ := store.read()
	if cp == nil || cp.Size != 100 {
		t.Errorf("checkpoint = %v, want held at 100 on a finding", cp)
	}
	prog, _ := store.readProgress()
	// Clean chunks up to (129, 139] advanced progress to 129 (the finding chunk
	// itself is not persisted), so progress must be < the finding chunk end.
	if prog != 129 {
		t.Errorf("progress = %d, want 129 (last clean chunk before the finding)", prog)
	}

	// Cross-run re-detection: a second run against the same store must re-enter the
	// finding chunk and re-alert, not skip past it. This is the property that keeps
	// a finding alert open until triaged; without it, persisting the finding chunk's
	// progress would silently auto-close the alert on the next clean-looking pass.
	f2 := &fakeMonitor{
		watch:  true,
		cur:    testCheckpoint(200),
		found:  []identity.MonitoredIdentity{{FoundIdentityEntries: []identity.LogEntry{{CertSubject: "x"}}}},
		findAt: 135,
	}
	if err := observe(context.Background(), f2, store, nil, io.Discard); err == nil {
		t.Fatal("second run: observe() error = nil, want the finding re-detected")
	}
	if len(f2.scanRanges) == 0 || f2.scanRanges[0][0] != 129 {
		t.Errorf("second run resumed at %v, want the first chunk to start at 129 (finding chunk re-entered)", f2.scanRanges)
	}
}

// TestObserveCatchUpDivergesGoesDegraded drives a catch-up where the head grows
// faster than one run can scan, so `remaining` climbs. After maxCatchUpStallRuns
// consecutive non-decreasing passes, observe must return a *catchUpStalledError
// (classified degraded, not retryable) instead of reporting clean forever.
func TestObserveCatchUpDivergesGoesDegraded(t *testing.T) {
	defer withScanBounds(10, 20)() // scan only 20 entries/run
	store := checkpointStore{path: filepath.Join(t.TempDir(), "cp.txt")}
	seedCheckpoint(t, store, 100) // window start index 99

	head := uint64(200)
	var lastErr error
	for run := 1; run <= 4; run++ {
		f := &fakeMonitor{watch: true, cur: testCheckpoint(head)}
		lastErr = observe(context.Background(), f, store, nil, io.Discard)
		if run < 4 && lastErr != nil {
			t.Fatalf("run %d: observe() = %v, want nil (still catching up)", run, lastErr)
		}
		head += 30 // head outpaces the 20/run scan, so remaining rises
	}
	if lastErr == nil {
		t.Fatal("run 4: observe() = nil, want a degraded not-converging error")
	}
	if _, ok := stderrors.AsType[*catchUpStalledError](lastErr); !ok {
		t.Fatalf("run 4: error type = %T, want *catchUpStalledError", lastErr)
	}
	if got := classify(lastErr); got != classDegraded {
		t.Errorf("classify = %q, want %q", got, classDegraded)
	}
	if cp, _ := store.read(); cp == nil || cp.Size != 100 {
		t.Errorf("checkpoint = %v, want held at 100 on a diverged pass", cp)
	}
}

// TestObserveCatchUpConvergesStaysClean drives a catch-up where the scan outpaces
// head growth, so `remaining` shrinks every run: the stall count must never
// climb and observe must stay clean throughout.
func TestObserveCatchUpConvergesStaysClean(t *testing.T) {
	defer withScanBounds(10, 50)() // scan 50 entries/run, faster than growth
	store := checkpointStore{path: filepath.Join(t.TempDir(), "cp.txt")}
	seedCheckpoint(t, store, 100)

	head := uint64(400) // window (99, 399]
	for run := 1; run <= 3; run++ {
		f := &fakeMonitor{watch: true, cur: testCheckpoint(head)}
		if err := observe(context.Background(), f, store, nil, io.Discard); err != nil {
			t.Fatalf("run %d: observe() = %v, want nil (converging)", run, err)
		}
		if tr, _ := store.readScanTrend(); tr.stall != 0 {
			t.Fatalf("run %d: stall count = %d, want 0 while converging", run, tr.stall)
		}
		head += 10 // head grows slower than the 50/run scan, so remaining falls
	}
}

// TestScanTrendRoundTrip covers the scan-trend companion: write then read returns
// the triple; a two-field (legacy) file reads with passes=0; reset and an absent
// file read as the zero value; malformed and out-of-range files error.
func TestScanTrendRoundTrip(t *testing.T) {
	store := checkpointStore{path: filepath.Join(t.TempDir(), "cp.txt")}
	if tr, err := store.readScanTrend(); err != nil || tr != (scanTrend{}) {
		t.Fatalf("readScanTrend (absent) = %+v,%v; want zero,nil", tr, err)
	}
	if err := store.writeScanTrend(scanTrend{bestRemaining: 4242, stall: 2, passes: 9}); err != nil {
		t.Fatalf("writeScanTrend: %v", err)
	}
	if tr, err := store.readScanTrend(); err != nil || tr != (scanTrend{4242, 2, 9}) {
		t.Fatalf("readScanTrend = %+v,%v; want {4242 2 9},nil", tr, err)
	}
	// Legacy two-field file: passes defaults to 0.
	if err := os.WriteFile(store.stallPath(), []byte("5000 1\n"), 0o600); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	if tr, err := store.readScanTrend(); err != nil || tr != (scanTrend{5000, 1, 0}) {
		t.Fatalf("readScanTrend (legacy) = %+v,%v; want {5000 1 0},nil", tr, err)
	}
	if err := store.resetScanTrend(); err != nil {
		t.Fatalf("resetScanTrend: %v", err)
	}
	if tr, err := store.readScanTrend(); err != nil || tr != (scanTrend{}) {
		t.Fatalf("readScanTrend (after reset) = %+v,%v; want zero,nil", tr, err)
	}
	for _, bad := range []string{"garbage", "1 2 3 4", "-1 2 3", "10 -2 3", "10 2 x"} {
		if err := os.WriteFile(store.stallPath(), []byte(bad+"\n"), 0o600); err != nil {
			t.Fatalf("seed %q: %v", bad, err)
		}
		if _, err := store.readScanTrend(); err == nil {
			t.Errorf("readScanTrend(%q) = nil error, want an error", bad)
		}
	}
}

// TestCheckpointProgressRoundTrip covers the scan-progress companion: write then
// read returns the value; an absent or empty file reads as 0 (fresh window).
func TestCheckpointProgressRoundTrip(t *testing.T) {
	store := checkpointStore{path: filepath.Join(t.TempDir(), "cp.txt")}
	if n, err := store.readProgress(); err != nil || n != 0 {
		t.Fatalf("readProgress (absent) = %d, %v; want 0, nil", n, err)
	}
	if err := store.writeProgress(123456); err != nil {
		t.Fatalf("writeProgress: %v", err)
	}
	if n, err := store.readProgress(); err != nil || n != 123456 {
		t.Fatalf("readProgress = %d, %v; want 123456, nil", n, err)
	}
	if err := os.WriteFile(store.progressPath(), []byte("  \n"), 0o600); err != nil {
		t.Fatalf("seed empty: %v", err)
	}
	if n, err := store.readProgress(); err != nil || n != 0 {
		t.Fatalf("readProgress (blank) = %d, %v; want 0, nil", n, err)
	}
}

// withScanBounds temporarily shrinks the scan chunk size and per-run cap for a
// test, returning a restore function to defer.
func withScanBounds(chunk, maxPerRun int64) func() {
	origChunk, origCap := scanChunkSize, maxScanEntriesPerRun
	scanChunkSize, maxScanEntriesPerRun = chunk, maxPerRun
	return func() { scanChunkSize, maxScanEntriesPerRun = origChunk, origCap }
}

// TestObserveResumeAtWindowEnd covers the recovery path where persisted progress
// already reaches the window end (a prior run scanned fully but its checkpoint
// advance did not persist): the next run must advance without rescanning the
// whole window (which, for a large backlog, would just time out again).
func TestObserveResumeAtWindowEnd(t *testing.T) {
	store := checkpointStore{path: filepath.Join(t.TempDir(), "cp.txt")}
	seedCheckpoint(t, store, 100) // window (99, 149]
	const head = 150
	if err := store.writeProgress(149); err != nil { // == window end index
		t.Fatalf("seed progress: %v", err)
	}
	f := &fakeMonitor{watch: true, cur: testCheckpoint(head)}
	if err := observe(context.Background(), f, store, nil, os.Stdout); err != nil {
		t.Fatalf("observe: %v", err)
	}
	if f.scanned {
		t.Error("scanIdentity must not run when progress already covers the window")
	}
	cp, _ := store.read()
	if cp == nil || cp.Size != head {
		t.Errorf("checkpoint = %v, want advanced to %d", cp, head)
	}
	if prog, _ := store.readProgress(); prog != 0 {
		t.Errorf("progress = %d, want reset to 0", prog)
	}
}

// TestObserveSoftBudgetHaltsScan covers the primary (time-based) bound: when the
// pass deadline is within scanBudgetHeadroom, the scan stops after the current
// chunk with progress saved and a clean (exit 0) partial pass, resuming next run
// -- independent of the per-run entry cap, which is set large here so only the
// soft time budget can halt the scan.
func TestObserveSoftBudgetHaltsScan(t *testing.T) {
	defer withScanBounds(10, 1_000_000)() // chunk=10, cap large so only the time budget binds
	origHeadroom := scanBudgetHeadroom
	scanBudgetHeadroom = 24 * time.Hour // any live deadline is already "within headroom"
	defer func() { scanBudgetHeadroom = origHeadroom }()

	store := checkpointStore{path: filepath.Join(t.TempDir(), "cp.txt")}
	seedCheckpoint(t, store, 100) // window (99, 159]
	const head = 160

	// Deadline in the future (ctx not canceled, so no operational error) but nearer
	// than the headroom, so the soft budget is already spent after the first chunk.
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()

	f := &fakeMonitor{watch: true, cur: testCheckpoint(head)}
	if err := observe(ctx, f, store, nil, os.Stdout); err != nil {
		t.Fatalf("observe() error = %v, want a clean partial pass", err)
	}
	if len(f.scanRanges) != 1 {
		t.Fatalf("scanned %d chunks, want exactly 1 before the soft budget halts", len(f.scanRanges))
	}
	if prog, err := store.readProgress(); err != nil || prog != 109 { // 99 + one chunk of 10
		t.Errorf("progress = %d, %v; want 109, nil (one chunk saved)", prog, err)
	}
	if cp, _ := store.read(); cp == nil || cp.Size != 100 {
		t.Errorf("checkpoint = %v, want held at 100 (not caught up)", cp)
	}
}
