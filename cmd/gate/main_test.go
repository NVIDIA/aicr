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
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/NVIDIA/aicr/pkg/chainsawgate/runner"
)

func TestProgressLine(t *testing.T) {
	components := map[string]runner.ComponentResult{
		"a": {Result: runner.ResultPass},
		"b": {Result: runner.ResultFail, Message: strings.Repeat("x", 80)},
	}
	rs := runner.ReadyState{Reason: runner.ReasonStabilizing}
	line := progressLine(5*time.Second, components, rs)
	if !strings.Contains(line, "a: Pass") || !strings.Contains(line, "b: Fail") {
		t.Fatalf("progressLine = %q", line)
	}
	if !strings.Contains(line, "...") {
		t.Fatalf("expected truncated message, got %q", line)
	}
}

func TestSummaryReason(t *testing.T) {
	if got := summaryReason(runner.EvalResult{AllPass: true}); got != "stabilizing" {
		t.Fatalf("summaryReason(all pass) = %q", got)
	}
	got := summaryReason(runner.EvalResult{
		Components: map[string]runner.ComponentResult{
			"x": {Result: runner.ResultFail, Message: "boom"},
		},
	})
	if got != "boom" {
		t.Fatalf("summaryReason(fail) = %q", got)
	}
}

func TestShortMsg(t *testing.T) {
	if got := shortMsg("short"); got != "short" {
		t.Fatalf("shortMsg = %q", got)
	}
	long := strings.Repeat("a", 80)
	if got := shortMsg(long); len(got) != 63 || !strings.HasSuffix(got, "...") {
		t.Fatalf("shortMsg long = %q (len %d)", got, len(got))
	}
}

func TestLoop_StabilityWindowAfterEvalLatency(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stability := 30 * time.Second
		poll := 5 * time.Second
		evalLatency := 10 * time.Second

		orig := evaluateFn
		defer func() { evaluateFn = orig }()

		start := time.Now()
		var call atomic.Int32
		evaluateFn = func(ctx context.Context, bundle map[string]string, opts runner.Options) (runner.EvalResult, error) {
			call.Add(1)
			time.Sleep(evalLatency)
			return runner.EvalResult{
				AllPass: true,
				Components: map[string]runner.ComponentResult{
					"c": {Result: runner.ResultPass},
				},
			}, nil
		}

		opts := runner.Options{
			PollInterval:    poll,
			StabilityWindow: stability,
			MaxWait:         5 * time.Minute,
		}
		bundle := map[string]string{"c": "/bundle/c.yaml"}

		done := make(chan int, 1)
		go func() {
			done <- loop(context.Background(), bundle, opts, start)
		}()

		synctest.Wait()
		if call.Load() != 1 {
			t.Fatalf("after first eval call = %d, want 1", call.Load())
		}

		// First pass anchors at T+evalLatency; need stability (30s) plus one
		// poll/eval cycle (poll 5s + eval 10s) before Ready clears.
		time.Sleep(stability + poll + evalLatency + time.Second)
		synctest.Wait()

		select {
		case code := <-done:
			if code != exitOK {
				t.Fatalf("loop exit = %d, want %d", code, exitOK)
			}
		default:
			t.Fatal("loop did not exit after full stability window")
		}
	})
}

func TestLoop_DeadlineExceeded(t *testing.T) {
	orig := evaluateFn
	defer func() { evaluateFn = orig }()

	evaluateFn = func(context.Context, map[string]string, runner.Options) (runner.EvalResult, error) {
		return runner.EvalResult{
			AllPass: false,
			Components: map[string]runner.ComponentResult{
				"c": {Result: runner.ResultFail},
			},
		}, nil
	}

	opts := runner.Options{
		PollInterval:    time.Millisecond,
		StabilityWindow: time.Second,
		MaxWait:         5 * time.Millisecond,
	}
	if got := loop(context.Background(), map[string]string{"c": "x"}, opts, time.Now()); got != exitDeadline {
		t.Fatalf("loop = %d, want %d", got, exitDeadline)
	}
}
