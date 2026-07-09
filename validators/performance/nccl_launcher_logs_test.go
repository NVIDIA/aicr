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
	"context"
	stderrors "errors"
	"testing"
	"time"
)

// A representative all_reduce_perf results block (one data row + summary trailer).
const ncclCompleteLog = `#       size         count      type   redop    root     time   algbw   busbw #wrong
#        (B)    (elements)                               (us)  (GB/s)  (GB/s)
 8589934592    2147483648     float     sum      -1   48298   177.85  333.47      0   48292   177.87  333.51      0
# Out of bounds values : 0 OK
# Avg bus bandwidth    : 333.49
`

func TestNcclLauncherLogComplete(t *testing.T) {
	tests := []struct {
		name string
		logs string
		want bool
	}{
		{"empty", "", false},
		{"header only (no data rows, no trailer)", "#   size   count   type\n#   (B)\n", false},
		{"init noise only", "NCCL INFO Bootstrap : Using eth0\nsome mpirun warmup line\n", false},
		// An early data row without the trailer must NOT count as complete: the
		// parser keys on the *last* row, so a log truncated before the largest
		// size would otherwise short-circuit the retry loop (CodeRabbit #1691).
		{"early data row but no trailer", "#       size ...\n         1024          256     float     sum      -1     12    0.10    0.09      0     12    0.10    0.09      0\n", false},
		{"full sweep with trailer", ncclCompleteLog, true},
		{"summary trailer only", "# Avg bus bandwidth    : 333.49\n", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ncclLauncherLogComplete(tt.logs); got != tt.want {
				t.Errorf("ncclLauncherLogComplete() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReadLauncherLogsUntilComplete_ReturnsWhenComplete(t *testing.T) {
	// First read is already complete — returns immediately, no retries.
	calls := 0
	fetch := func(context.Context) (string, error) { calls++; return ncclCompleteLog, nil }
	logs, err := readLauncherLogsUntilComplete(context.Background(), fetch, 5, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ncclLauncherLogComplete(logs) {
		t.Errorf("expected complete logs, got %q", logs)
	}
	if calls != 1 {
		t.Errorf("expected 1 fetch, got %d", calls)
	}
}

func TestReadLauncherLogsUntilComplete_RetriesUntilComplete(t *testing.T) {
	// Truncated (empty) reads twice, then the full results table lands.
	calls := 0
	fetch := func(context.Context) (string, error) {
		calls++
		if calls < 3 {
			return "", nil
		}
		return ncclCompleteLog, nil
	}
	logs, err := readLauncherLogsUntilComplete(context.Background(), fetch, 5, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ncclLauncherLogComplete(logs) {
		t.Errorf("expected complete logs after retries, got %q", logs)
	}
	if calls != 3 {
		t.Errorf("expected 3 fetches, got %d", calls)
	}
}

func TestReadLauncherLogsUntilComplete_ReturnsLastReadWhenNeverComplete(t *testing.T) {
	// Never completes: must return the last (incomplete) read after the budget,
	// not an error — so the caller's parse-failure path can surface it.
	calls := 0
	fetch := func(context.Context) (string, error) { calls++; return "partial noise, no table", nil }
	logs, err := readLauncherLogsUntilComplete(context.Background(), fetch, 3, time.Millisecond)
	if err != nil {
		t.Fatalf("expected no error (last read returned for diagnosis), got %v", err)
	}
	if ncclLauncherLogComplete(logs) {
		t.Errorf("expected incomplete logs to be returned as-is, got complete %q", logs)
	}
	if calls != 3 {
		t.Errorf("expected exactly the attempt budget (3) fetches, got %d", calls)
	}
}

func TestReadLauncherLogsUntilComplete_PropagatesFetchError(t *testing.T) {
	sentinel := stderrors.New("logs api unavailable")
	fetch := func(context.Context) (string, error) { return "", sentinel }
	_, err := readLauncherLogsUntilComplete(context.Background(), fetch, 5, time.Millisecond)
	if !stderrors.Is(err, sentinel) {
		t.Errorf("expected the fetch error to propagate, got %v", err)
	}
}

func TestReadLauncherLogsUntilComplete_ReturnsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // canceled before the retry sleep
	calls := 0
	fetch := func(context.Context) (string, error) { calls++; return "still no table", nil }
	logs, err := readLauncherLogsUntilComplete(ctx, fetch, 5, time.Hour)
	if err != nil {
		t.Fatalf("expected no error on cancel (last read returned), got %v", err)
	}
	if ncclLauncherLogComplete(logs) {
		t.Errorf("expected incomplete logs, got complete %q", logs)
	}
	// One fetch, then the canceled context short-circuits the sleep.
	if calls != 1 {
		t.Errorf("expected 1 fetch before cancel short-circuit, got %d", calls)
	}
}
