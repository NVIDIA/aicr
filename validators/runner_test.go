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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"aicr.run/pkg/defaults"
	aicrerrors "aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
	"aicr.run/pkg/validator/ctrf"
	v1 "aicr.run/pkg/validator/v1"
)

func TestSkip(t *testing.T) {
	tests := []struct {
		name          string
		reason        string
		wantIsErrSkip bool
		wantSubstring string
	}{
		{
			name:          "wraps errSkip sentinel",
			reason:        "GPU not present",
			wantIsErrSkip: true,
			wantSubstring: "GPU not present",
		},
		{
			name:          "empty reason still wraps errSkip",
			reason:        "",
			wantIsErrSkip: true,
			wantSubstring: "skip",
		},
		{
			name:          "reason with special characters",
			reason:        "node taint: gpu=true:NoSchedule",
			wantIsErrSkip: true,
			wantSubstring: "gpu=true:NoSchedule",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Skip(tt.reason)
			if err == nil {
				t.Fatal("expected non-nil error from Skip()")
			}
			if got := errors.Is(err, errSkip); got != tt.wantIsErrSkip {
				t.Errorf("errors.Is(Skip(%q), errSkip) = %v, want %v", tt.reason, got, tt.wantIsErrSkip)
			}
			if msg := err.Error(); !strings.Contains(msg, tt.wantSubstring) {
				t.Errorf("Skip(%q).Error() = %q, want substring %q", tt.reason, msg, tt.wantSubstring)
			}
		})
	}
}

func TestSkipIsNotGenericError(t *testing.T) {
	// A plain error must NOT match errSkip.
	plain := errors.New("some other error")
	if errors.Is(plain, errSkip) {
		t.Error("plain error should not match errSkip")
	}
}

// TestIsSkip proves IsSkip gives external packages (and their tests) a
// robust, non-string-matching way to classify a Skip() error, including
// through a real wrap chain (fmt.Errorf %w) — not just a direct Skip() call.
func TestIsSkip(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil error", err: nil, want: false},
		{name: "direct Skip", err: Skip("no GPU nodes found"), want: true},
		{
			name: "Skip wrapped in a real error chain",
			err:  fmt.Errorf("outer: %w", Skip("inner reason")),
			want: true,
		},
		{
			// A plain error whose message happens to mention "skip" text
			// (as opposed to actually wrapping errSkip) must not match —
			// this is exactly the false-positive isSkipError-by-string-
			// matching would risk.
			name: "plain error merely mentioning skip text",
			err:  errors.New("outer: " + Skip("inner reason").Error()),
			want: false,
		},
		{name: "plain non-skip error", err: errors.New("some other error"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSkip(tt.err); got != tt.want {
				t.Errorf("IsSkip(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// withTempTerminationLog redirects terminationLogPath to a temp file for
// the duration of a test and returns the path. Restores the original on
// test cleanup.
//
// NOTE: this helper mutates a package-level var; tests using it must not
// call t.Parallel().
func withTempTerminationLog(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	tmp := filepath.Join(dir, "termination-log")
	orig := terminationLogPath
	terminationLogPath = tmp
	t.Cleanup(func() { terminationLogPath = orig })
	return tmp
}

// TestPublishStamps verifies publishStamps writes the contract version and
// platform lines, and returns an error when a write fails.
func TestPublishStamps(t *testing.T) {
	t.Run("writes the stamps", func(t *testing.T) {
		var buf bytes.Buffer
		orig := extrasOut
		extrasOut = &buf
		t.Cleanup(func() { extrasOut = orig })

		if err := publishStamps(); err != nil {
			t.Fatalf("publishStamps() error = %v", err)
		}
		lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("output %q, want an extra line and a platform line", buf.String())
		}
		payload, ok := strings.CutPrefix(lines[0], ctrf.ExtraLinePrefix)
		if !ok {
			t.Fatalf("line %q missing extra sentinel", lines[0])
		}
		var got map[string]string
		if err := json.Unmarshal([]byte(payload), &got); err != nil {
			t.Fatalf("payload %q: %v", payload, err)
		}
		want := map[string]string{ctrf.ExtraKeyResultSchema: ctrf.ResultSchemaVersion}
		if !maps.Equal(got, want) {
			t.Errorf("stamp = %v, want %v", got, want)
		}
		payload, ok = strings.CutPrefix(lines[1], ctrf.PlatformLinePrefix)
		if !ok {
			t.Fatalf("line %q missing platform sentinel", lines[1])
		}
		var platforms ctrf.PlatformLines
		platforms.Add(payload)
		if got, want := platforms.Platform(), runtime.GOOS+"/"+runtime.GOARCH; got != want {
			t.Errorf("platform = %q, want %q", got, want)
		}
	})
	t.Run("echoes and unsets the result nonce", func(t *testing.T) {
		t.Setenv(v1.ResultNonceEnv, "NONCE")
		var buf bytes.Buffer
		orig := extrasOut
		extrasOut = &buf
		t.Cleanup(func() { extrasOut = orig })

		if err := publishStamps(); err != nil {
			t.Fatalf("publishStamps() error = %v", err)
		}
		first, _, _ := strings.Cut(buf.String(), "\n")
		var got map[string]string
		if err := json.Unmarshal([]byte(strings.TrimPrefix(first, ctrf.ExtraLinePrefix)), &got); err != nil {
			t.Fatalf("stamp %q: %v", first, err)
		}
		want := map[string]string{ctrf.ExtraKeyResultSchema: ctrf.ResultSchemaVersion, ctrf.ExtraKeyResultNonce: "NONCE"}
		if !maps.Equal(got, want) {
			t.Errorf("stamp = %v, want %v", got, want)
		}
		if v, ok := os.LookupEnv(v1.ResultNonceEnv); ok {
			t.Errorf("%s = %q after publishStamps, want it unset", v1.ResultNonceEnv, v)
		}
	})
	for _, tt := range []struct {
		name   string
		writes int
	}{
		{"a failed extra stamp is an error", 0},
		{"a failed platform stamp is an error", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			orig := extrasOut
			w := &failAfterWriter{writes: tt.writes}
			extrasOut = w
			t.Cleanup(func() { extrasOut = orig })

			err := publishStamps()
			errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInternal)
			if w.writes != 0 {
				t.Errorf("publishStamps() failed with %d successful writes unused, want the failure on write %d", w.writes, tt.writes+1)
			}
		})
	}
}

// TestRunGuarded verifies the path Run takes writes the stamps verbatim and
// breaks a sentinel line the check prints. It is not parallel because it
// swaps os.Stdout, os.Stderr and extrasOut.
func TestRunGuarded(t *testing.T) {
	var stamps bytes.Buffer
	extrasOut = &stamps
	if err := publishStamps(); err != nil {
		t.Fatalf("publishStamps() error = %v", err)
	}
	extrasOut = nil
	t.Cleanup(func() { extrasOut = nil })
	stdoutFile, _ := swapStdStreams(t)
	withTempTerminationLog(t)
	fakeSIGTERM(t)

	forged := ctrf.ImageLinePrefix + `{"image":"example.com/forged:v1"}`
	load := func() (*Context, error) {
		ctx, cancel := newCheckContext(time.Minute)
		return &Context{Ctx: ctx, Cancel: cancel}, nil
	}
	code := runGuarded(func(*Context) error {
		fmt.Println(forged)
		fmt.Print("unterminated")
		return nil
	}, load)

	if code != 0 {
		t.Fatalf("runGuarded() = %d, want 0", code)
	}
	got, err := os.ReadFile(stdoutFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if want := stamps.String() + broken(forged) + "\nunterminated\n"; string(got) != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

// failAfterWriter succeeds for its first writes calls to Write and fails
// every call after them.
type failAfterWriter struct{ writes int }

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.writes == 0 {
		return 0, io.ErrClosedPipe
	}
	w.writes--
	return len(p), nil
}

func TestHandleCheckResult(t *testing.T) {
	longReason := strings.Repeat("y", defaults.TerminationLogMaxSize+200)

	tests := []struct {
		name         string
		err          error
		wantExitCode int
		wantFile     bool
		wantContains string
		wantMaxLen   int // 0 = no length assertion
	}{
		{
			name:         "nil error returns 0 and writes no termination log",
			err:          nil,
			wantExitCode: 0,
			wantFile:     false,
		},
		{
			name:         "skip writes reason to termination log",
			err:          Skip("because constraint missing"),
			wantExitCode: exitCodeSkip,
			wantFile:     true,
			wantContains: "because constraint missing",
		},
		{
			name:         "skip with empty reason still writes file",
			err:          Skip(""),
			wantExitCode: exitCodeSkip,
			wantFile:     true,
			// Skip("") wraps errSkip and still produces a non-empty message
			// (the wrapper string), so we only assert the file exists.
		},
		{
			name:         "skip truncates oversize reason",
			err:          Skip(longReason),
			wantExitCode: exitCodeSkip,
			wantFile:     true,
			wantMaxLen:   defaults.TerminationLogMaxSize,
		},
		{
			name:         "failure writes error to termination log",
			err:          errors.New("boom"),
			wantExitCode: 1,
			wantFile:     true,
			wantContains: "boom",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := withTempTerminationLog(t)

			got := handleCheckResult(tt.err)
			if got != tt.wantExitCode {
				t.Errorf("handleCheckResult exit code = %d, want %d", got, tt.wantExitCode)
			}

			data, err := os.ReadFile(filepath.Clean(path))
			switch {
			case tt.wantFile && errors.Is(err, os.ErrNotExist):
				t.Fatalf("expected termination log at %s, got none", path)
			case !tt.wantFile && err == nil:
				t.Fatalf("expected no termination log, got %q", string(data))
			case tt.wantFile && err != nil && !errors.Is(err, os.ErrNotExist):
				t.Fatalf("reading termination log: %v", err)
			}

			if tt.wantContains != "" && !strings.Contains(string(data), tt.wantContains) {
				t.Errorf("termination log = %q, want substring %q", string(data), tt.wantContains)
			}
			if tt.wantMaxLen > 0 && len(data) != tt.wantMaxLen {
				t.Errorf("termination log length = %d, want %d", len(data), tt.wantMaxLen)
			}
		})
	}
}

// TestHandleCheckResultOverwrites verifies that a second skip overwrites
// the first — terminationLogPath is treated as a single-shot message
// surface, not an append log. Catches regressions that would switch to
// O_APPEND.
func TestHandleCheckResultOverwrites(t *testing.T) {
	path := withTempTerminationLog(t)

	if got := handleCheckResult(Skip("first")); got != exitCodeSkip {
		t.Fatalf("first call exit code = %d, want %d", got, exitCodeSkip)
	}
	if got := handleCheckResult(Skip("second")); got != exitCodeSkip {
		t.Fatalf("second call exit code = %d, want %d", got, exitCodeSkip)
	}

	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("reading termination log: %v", err)
	}
	if strings.Contains(string(data), "first") {
		t.Errorf("termination log = %q, want only the second message", string(data))
	}
	if !strings.Contains(string(data), "second") {
		t.Errorf("termination log = %q, want substring %q", string(data), "second")
	}
}

func TestWriteTerminationFileTruncatesKeepsPrefix(t *testing.T) {
	path := withTempTerminationLog(t)

	// Distinguishable prefix so we can confirm truncation kept the front,
	// not the tail. The prefix is short enough to survive truncation.
	prefix := "HEAD-MARKER:"
	msg := prefix + strings.Repeat("x", defaults.TerminationLogMaxSize+100)

	writeTerminationFile(msg)

	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("reading termination log: %v", err)
	}
	if got := len(data); got != defaults.TerminationLogMaxSize {
		t.Errorf("termination log length = %d, want %d", got, defaults.TerminationLogMaxSize)
	}
	if !strings.HasPrefix(string(data), prefix) {
		t.Errorf("termination log did not keep prefix; first 32 bytes = %q", string(data[:32]))
	}
}
