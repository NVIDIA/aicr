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

//go:build darwin || linux

package versionpins

import (
	"context"
	stderrors "errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// TestLoadBlockedOpenTimesOutOnFIFO is the real-syscall complement to the
// deterministic seam tests: a FIFO with no writer blocks in open(2) itself,
// so only a context-bounded open can return. The open seam here still calls
// the real os.Open — the seam only signals entry, so the release below is
// deterministic rather than racing the worker to the syscall.
func TestLoadBlockedOpenTimesOutOnFIFO(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), FileName)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	started := make(chan struct{})
	swapSeams(t, func(p string) (*os.File, error) {
		close(started)
		return os.Open(p) // genuinely blocks until a writer appears
	}, nil)

	// releaseWriter unpins the worker from open(2) — Go cannot cancel a
	// blocked syscall. `started` fires before the worker's open, so the
	// non-blocking write-open (ENXIO until a reader waits) must connect
	// within the retry window; not connecting is a real failure that would
	// leave the worker pinned for the life of the test binary. Once the
	// writer connects, the worker's open returns a FIFO handle, the
	// in-worker fstat rejects it as non-regular, and the worker closes it.
	// Idempotent on SUCCESS only: the success path and cleanup both call it,
	// and a failed attempt leaves `released` false so cleanup can retry.
	// Body and cleanup run on the test goroutine, so no locking is needed.
	released := false
	releaseWriter := func(fail func(format string, args ...any)) {
		if released {
			return
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
			if err == nil {
				// The reader is released the moment the writer connects, so
				// mark success before the Close check — a Close failure must
				// not trigger a misleading cleanup retry.
				released = true
				// Writable handle: Close may surface a write-side error.
				if closeErr := w.Close(); closeErr != nil {
					fail("closing release writer: %v", closeErr)
				}
				return
			}
			if time.Now().After(deadline) {
				fail("release writer never connected; worker left pinned in open(2): %v", err)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	// Explicit cancellation after `started` (rather than a pre-set deadline)
	// removes the scheduling race where a short deadline could expire before
	// the worker even enters the seam.
	ctx, cancel := context.WithCancel(context.Background())
	done := loadAsync(ctx, fifo)
	t.Cleanup(func() {
		cancel()
		releaseWriter(t.Errorf)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("async Load never returned during cleanup; the worker may be stranded")
		}
	})

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never entered open")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Load() succeeded on a writer-less FIFO; the open should have blocked and timed out")
		}
		if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
			t.Errorf("error code = %v, want %v", err, errors.ErrCodeTimeout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Load did not unblock while open(2) was blocked on the FIFO")
	}

	releaseWriter(func(format string, args ...any) {
		t.Helper()
		t.Fatalf(format, args...)
	})
}
