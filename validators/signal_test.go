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

package validators

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"aicr.run/pkg/defaults"
	"aicr.run/pkg/logging/loggingtest"
	"aicr.run/pkg/validator/ctrf"
)

func fakeSIGTERM(t *testing.T) (chan os.Signal, *int) {
	t.Helper()
	events := make(chan os.Signal, 1)
	stopped := new(int)
	original := subscribeSIGTERM
	subscribeSIGTERM = func() (<-chan os.Signal, func()) {
		return events, func() { *stopped++ }
	}
	t.Cleanup(func() { subscribeSIGTERM = original })
	return events, stopped
}

func TestRunCheckSIGTERM(t *testing.T) {
	tests := []struct {
		name       string
		checkErr   error
		timeout    time.Duration
		waitSignal bool
	}{
		{"honors cancellation", context.Canceled, time.Minute, true},
		{"swallows cancellation", nil, time.Minute, true},
		{"queued signal and nil return", nil, time.Minute, false},
		{"timeout before SIGTERM", nil, -time.Second, true},
		{"skip after interruption", Skip("no verdict"), time.Minute, true},
		{"long error", errors.New(strings.Repeat("x", defaults.TerminationLogMaxSize)), time.Minute, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			events, stopped := fakeSIGTERM(t)
			path := withTempTerminationLog(t)
			ctx, cancel := newCheckContext(tt.timeout)
			defer cancel()
			cleaned := false
			code := runCheckWithContext(&Context{Ctx: ctx, Cancel: cancel}, func(c *Context) error {
				defer func() { cleaned = true }()
				events <- syscall.SIGTERM
				if !tt.waitSignal {
					return tt.checkErr
				}
				select {
				case <-c.Ctx.(*checkContext).signalCtx.Done():
				case <-time.After(5 * time.Second):
					t.Fatal("SIGTERM was not delivered")
				}
				return tt.checkErr
			})
			if code != exitCodeTerminated || !cleaned {
				t.Fatalf("code=%d cleaned=%v", code, cleaned)
			}
			data, err := os.ReadFile(filepath.Clean(path))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(data), "terminated by SIGTERM before the check completed") {
				t.Errorf("termination message=%q", data)
			}
			if len(data) > defaults.TerminationLogMaxSize {
				t.Errorf("oversized message: %d", len(data))
			}
			cancel() // Joins the signal watcher, including when the signal already ended it.
			if *stopped == 0 {
				t.Error("signal subscription not released")
			}
		})
	}
}

// TestRunCheckPanicAfterSIGTERM verifies a panic outranks a SIGTERM that
// arrived first.
func TestRunCheckPanicAfterSIGTERM(t *testing.T) {
	events, _ := fakeSIGTERM(t)
	path := withTempTerminationLog(t)
	ctx, cancel := newCheckContext(time.Minute)
	defer cancel()
	code := runCheckWithContext(&Context{Ctx: ctx, Cancel: cancel}, func(c *Context) error {
		events <- syscall.SIGTERM
		select {
		case <-c.Ctx.(*checkContext).signalCtx.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("SIGTERM was not delivered")
		}
		panic("boom")
	})
	if code != exitCodePanicked {
		t.Fatalf("code=%d want %d", code, exitCodePanicked)
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "check panicked: boom") {
		t.Errorf("termination message=%q", data)
	}
}

func TestCheckContextRelease(t *testing.T) {
	for _, signalFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "signaled"}[signalFirst], func(t *testing.T) {
			events, stopped := fakeSIGTERM(t)
			ctx, cancel := newCheckContext(time.Minute)
			if signalFirst {
				events <- syscall.SIGTERM
				<-ctx.Done()
			}
			cancel()
			cancel()
			if !errors.Is(ctx.Err(), context.Canceled) || *stopped == 0 {
				t.Fatalf("err=%v stopped=%d", ctx.Err(), *stopped)
			}
			// cancel waits for done, so no signal goroutine can survive this return.
		})
	}
}

func TestRunCheckWithoutSIGTERM(t *testing.T) {
	tests := []struct {
		name      string
		checkErr  error
		panicWith any
		timeout   time.Duration
		want      int
		message   string
	}{
		{"pass", nil, nil, time.Minute, 0, ""},
		{"fail", errors.New("check failed"), nil, time.Minute, 1, "check failed"},
		{"skip", Skip("not applicable"), nil, time.Minute, exitCodeSkip, "not applicable"},
		{"timeout", context.DeadlineExceeded, nil, -time.Second, 1, "context deadline exceeded"},
		{"panic", nil, "boom", time.Minute, exitCodePanicked, "check panicked: boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeSIGTERM(t)
			path := withTempTerminationLog(t)
			logs := loggingtest.CaptureJSON(t, slog.LevelInfo)
			ctx, cancel := newCheckContext(tt.timeout)
			defer cancel()
			code := runCheckWithContext(&Context{Ctx: ctx, Cancel: cancel}, func(*Context) error {
				if tt.panicWith != nil {
					panic(tt.panicWith)
				}
				return tt.checkErr
			})
			if code != tt.want {
				t.Errorf("code=%d want %d", code, tt.want)
			}
			if tt.panicWith != nil {
				if ctrf.ExitCodeToCTRFStatus(int32(code)) != ctrf.StatusOther {
					t.Errorf("status=%s, want %s", ctrf.ExitCodeToCTRFStatus(int32(code)), ctrf.StatusOther)
				}
				rec := loggingtest.FindRecord(t, logs.Bytes(), "check panicked")
				if stack, _ := rec["stack"].(string); !strings.Contains(stack, "callCheck") {
					t.Errorf("logged panic stack = %q, want the stack at the panic", stack)
				}
			}
			if tt.message != "" {
				data, err := os.ReadFile(filepath.Clean(path))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(data), tt.message) || strings.Contains(string(data), "SIGTERM") {
					t.Errorf("message=%q", data)
				}
			}
		})
	}
}
