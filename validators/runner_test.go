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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
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
