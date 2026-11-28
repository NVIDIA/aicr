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

// Package loggingtest captures the default slog logger's output in tests and
// reads the captured JSON records.
//
// The default logger is process-global, so a test that captures it must not
// run in parallel with another test that logs.
package loggingtest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"log/slog"
	"sync"
	"testing"
)

// Buffer holds captured log output. It is safe for concurrent use, so a
// test can read it while a goroutine under test logs.
type Buffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write appends p to the buffer.
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns the output captured so far.
func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Bytes returns a copy of the output captured so far.
func (b *Buffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

// CaptureText sends the default logger's records at level and above to the
// returned Buffer as text until t ends.
func CaptureText(t testing.TB, level slog.Level) *Buffer {
	t.Helper()
	b := &Buffer{}
	capture(t, slog.NewTextHandler(b, &slog.HandlerOptions{Level: level}))
	return b
}

// CaptureJSON sends the default logger's records at level and above to the
// returned Buffer as JSON until t ends. Read a record with FindRecord, so an
// attribute is read by name instead of matched as a substring.
func CaptureJSON(t testing.TB, level slog.Level) *Buffer {
	t.Helper()
	b := &Buffer{}
	capture(t, slog.NewJSONHandler(b, &slog.HandlerOptions{Level: level}))
	return b
}

func capture(t testing.TB, h slog.Handler) {
	prev := slog.Default()
	// slog.SetDefault also redirects the log package, which restoring prev
	// does not undo when prev uses the default handler.
	prevWriter, prevFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevWriter)
		log.SetFlags(prevFlags)
	})
}

// FindRecord returns the first JSON record in logs whose msg is want, with
// numbers decoded as json.Number. It fails t when no record carries want or
// logs does not decode.
func FindRecord(t testing.TB, logs []byte, want string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(logs))
	dec.UseNumber()
	for {
		var rec map[string]any
		switch err := dec.Decode(&rec); {
		case errors.Is(err, io.EOF):
			t.Fatalf("no log record with msg %q; captured:\n%s", want, logs)
			return nil
		case err != nil:
			t.Fatalf("decoding captured log: %v; captured:\n%s", err, logs)
			return nil
		}
		if msg, ok := rec["msg"].(string); ok && msg == want {
			return rec
		}
	}
}

// WantIntAttr fails t unless rec carries key with the integer value want. A
// missing key is reported distinctly from a wrong value, so deleting an
// attribute and mis-sourcing one are not confusable failures.
func WantIntAttr(t testing.TB, rec map[string]any, key string, want int64) {
	t.Helper()
	msg := rec["msg"]
	raw, ok := rec[key]
	if !ok {
		t.Fatalf("%q record has no %q attribute; record = %v", msg, key, rec)
		return
	}
	num, ok := raw.(json.Number)
	if !ok {
		t.Fatalf("%q record %q = %v (%T), want a number", msg, key, raw, raw)
		return
	}
	got, err := num.Int64()
	if err != nil {
		t.Fatalf("%q record %q = %v, not an integer: %v", msg, key, raw, err)
		return
	}
	if got != want {
		t.Errorf("%q record %q = %d, want %d", msg, key, got, want)
	}
}
