// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

//go:build unix

package config

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
)

// fifo makes a named pipe and, at cleanup, opens it for writing so a read
// still waiting on it returns and its goroutine ends.
func fifo(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	t.Cleanup(func() {
		if w, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
	})
	return path
}

// waitFor polls cond until it holds, failing the test after 5s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after 5s waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A FIFO with no writer blocks the open in the kernel. Ctrl-C cancels the
// command's context, and the read must return at once rather than hang. The
// abandoned read's goroutine ends once a writer releases it, so a canceled
// read leaks nothing in a long-lived caller.
func TestReadFileReturnsWhenCanceledOnAFIFOWithoutAWriter(t *testing.T) {
	path := fifo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := runtime.NumGoroutine()
	done := make(chan error, 1)
	go func() {
		_, err := readFile(ctx, path)
		done <- err
	}()

	// A writer can open the FIFO without blocking only while a reader is
	// blocked opening it, so once this open succeeds the read is in the
	// kernel. Holding the writer open then keeps the read blocked, in read(2).
	var w *os.File
	waitFor(t, "the read to block opening the FIFO", func() bool {
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return false
		}
		w = f
		return true
	})
	cancel()
	select {
	case err := <-done:
		errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
	case <-time.After(5 * time.Second):
		t.Fatal("readFile still blocked 5s after its context was canceled")
	}

	// Closing the writer ends the read with EOF.
	_ = w.Close()
	waitFor(t, "the canceled read's goroutine to exit", func() bool {
		return runtime.NumGoroutine() <= before
	})
}

// A context canceled before the read starts, as when Ctrl-C lands first,
// reports canceled too, not timeout.
func TestReadFileReportsAnAlreadyCanceledContextAsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := readFile(ctx, fifo(t))
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
}

// A pipe is a supported source, as `--config <(...)` and `--config
// /dev/stdin` are: the read waits for the writer and returns what it wrote.
func TestReadFileReadsAFIFOWithAWriter(t *testing.T) {
	path := fifo(t)
	want := "kind: AICRConfig\n"
	go func() {
		w, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		_, _ = w.WriteString(want)
		_ = w.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := readFile(ctx, path)
	if err != nil {
		t.Fatalf("readFile() = %v", err)
	}
	if string(got) != want {
		t.Errorf("readFile() = %q, want %q", got, want)
	}
}
