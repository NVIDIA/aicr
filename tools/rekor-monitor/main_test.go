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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/transparency-dev/merkle/proof"
)

func TestRealMain(t *testing.T) {
	ok := func(context.Context, options, io.Writer) error { return nil }
	tamper := func(context.Context, options, io.Writer) error {
		return errors.Wrap(errors.ErrCodeInternal, "consistency verification failed",
			fmt.Errorf("consistency check failed: %w", proof.RootMismatchError{}))
	}
	identity := func(context.Context, options, io.Writer) error {
		return errors.New(errors.ErrCodeConflict, "1 match")
	}
	operational := func(context.Context, options, io.Writer) error {
		return errors.New(errors.ErrCodeUnavailable, "shards down")
	}
	degraded := func(context.Context, options, io.Writer) error {
		return &catchUpStalledError{remaining: 5, reason: "no new low-water mark for 3 consecutive passes"}
	}
	tests := []struct {
		name      string
		args      []string
		run       runFunc
		wantCode  int
		wantLine  string // substring expected in stdout; "" to skip
		wantEmpty bool   // assert nothing was written to w (no classification leak)
	}{
		{"bad args -> 2", []string{"--nope"}, ok, 2, "", true},
		{"clean -> 0", nil, ok, 0, "CLASSIFICATION=clean", false},
		{"tamper -> 1", nil, tamper, 1, "CLASSIFICATION=tamper", false},
		{"identity -> 1", nil, identity, 1, "CLASSIFICATION=identity", false},
		{"operational -> 3", nil, operational, 3, "CLASSIFICATION=operational", false},
		// classDegraded must exit 3 (a non-security failure), NOT 1 -- exit 1 would
		// route a catch-up stall to the security alert + Slack page this scheme
		// exists to prevent.
		{"degraded -> 3", nil, degraded, 3, "CLASSIFICATION=degraded", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if got := realMain(tt.args, tt.run, &buf); got != tt.wantCode {
				t.Errorf("realMain() = %d, want %d", got, tt.wantCode)
			}
			if tt.wantLine != "" && !strings.Contains(buf.String(), tt.wantLine) {
				t.Errorf("stdout %q missing %q", buf.String(), tt.wantLine)
			}
			if tt.wantEmpty && buf.Len() != 0 {
				t.Errorf("stdout = %q, want empty (no classification leak)", buf.String())
			}
		})
	}
}

func TestRealMainBadKnownTagsFile(t *testing.T) {
	calls := 0
	fn := func(context.Context, options, io.Writer) error { calls++; return nil }
	code := realMain([]string{"--known-tags-file", "/nonexistent/does-not-exist"}, fn, io.Discard)
	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if calls != 0 {
		t.Errorf("run func called %d times, want 0 (fail fast before running)", calls)
	}
}

func TestRunWithRetry(t *testing.T) {
	t.Run("retries operational then succeeds", func(t *testing.T) {
		calls := 0
		fn := func(context.Context, options, io.Writer) error {
			calls++
			if calls < 3 {
				return errors.New(errors.ErrCodeUnavailable, "blip")
			}
			return nil
		}
		if err := runWithRetry(context.Background(), options{}, io.Discard, fn); err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if calls != 3 {
			t.Errorf("calls = %d, want 3", calls)
		}
	})

	t.Run("does not retry a security classification", func(t *testing.T) {
		calls := 0
		fn := func(context.Context, options, io.Writer) error {
			calls++
			return errors.New(errors.ErrCodeConflict, "identity hit")
		}
		err := runWithRetry(context.Background(), options{}, io.Discard, fn)
		if err == nil {
			t.Fatal("err = nil, want conflict")
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1 (no retry on security)", calls)
		}
	})

	t.Run("gives up after max attempts on persistent operational", func(t *testing.T) {
		calls := 0
		fn := func(context.Context, options, io.Writer) error {
			calls++
			return errors.New(errors.ErrCodeUnavailable, "down")
		}
		if err := runWithRetry(context.Background(), options{}, io.Discard, fn); err == nil {
			t.Fatal("err = nil, want operational error after retries")
		}
		if calls != maxPassAttempts {
			t.Errorf("calls = %d, want %d", calls, maxPassAttempts)
		}
	})

	t.Run("cancelled context returns operational after one attempt", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		fn := func(context.Context, options, io.Writer) error {
			calls++
			return errors.New(errors.ErrCodeUnavailable, "blip")
		}
		err := runWithRetry(ctx, options{}, io.Discard, fn)
		if err == nil {
			t.Fatal("err = nil, want cancellation error")
		}
		if classify(err) != classOperational {
			t.Errorf("classify = %q, want operational", classify(err))
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1", calls)
		}
	})

	t.Run("does not retry a degraded (catch-up stalled) classification", func(t *testing.T) {
		calls := 0
		fn := func(context.Context, options, io.Writer) error {
			calls++
			return &catchUpStalledError{remaining: 5, reason: "stalled"}
		}
		err := runWithRetry(context.Background(), options{}, io.Discard, fn)
		if err == nil {
			t.Fatal("err = nil, want a degraded error")
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1 (degraded is deterministic; retrying just re-scans)", calls)
		}
	})

	t.Run("does not retry when too little pass budget remains", func(t *testing.T) {
		// A near-expired deadline: an operational failure must NOT be retried into a
		// trivial one-chunk partial pass that would mask it. defer withScanBounds so
		// scanBudgetHeadroom is the default (the guard compares against it).
		ctx, cancel := context.WithTimeout(context.Background(), scanBudgetHeadroom/2)
		defer cancel()
		calls := 0
		fn := func(context.Context, options, io.Writer) error {
			calls++
			return errors.New(errors.ErrCodeUnavailable, "late blip")
		}
		if err := runWithRetry(ctx, options{}, io.Discard, fn); err == nil {
			t.Fatal("err = nil, want the operational failure reported, not masked")
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1 (no retry without budget)", calls)
		}
	})

	t.Run("does not retry a tamper classification", func(t *testing.T) {
		calls := 0
		fn := func(context.Context, options, io.Writer) error {
			calls++
			return errors.Wrap(errors.ErrCodeInternal, "consistency verification failed",
				fmt.Errorf("consistency check failed: %w", proof.RootMismatchError{}))
		}
		err := runWithRetry(context.Background(), options{}, io.Discard, fn)
		if err == nil {
			t.Fatal("err = nil, want tamper error")
		}
		if calls != 1 {
			t.Errorf("calls = %d, want 1 (no retry on security)", calls)
		}
	})
}

func TestClassify(t *testing.T) {
	wrappedTamper := errors.Wrap(errors.ErrCodeInternal, "consistency verification failed",
		fmt.Errorf("consistency check failed: %w", proof.RootMismatchError{
			ExpectedRoot: []byte{1}, CalculatedRoot: []byte{2},
		}))
	tests := []struct {
		name string
		err  error
		want classification
	}{
		{"tamper: root mismatch (wrapped)", wrappedTamper, classTamper},
		{"identity: conflict", errors.New(errors.ErrCodeConflict, "1 match"), classIdentity},
		{"degraded: catch-up stalled", &catchUpStalledError{remaining: 5, reason: "stalled"}, classDegraded},
		{"operational: unavailable", errors.New(errors.ErrCodeUnavailable, "shards"), classOperational},
		{"operational: timeout", errors.New(errors.ErrCodeTimeout, "stall"), classOperational},
		{"operational: malformed proof (not mismatch)", errors.Wrap(errors.ErrCodeInternal, "consistency verification failed", fmt.Errorf("building consistency proof: %w", stderrors.New("empty proof"))), classOperational},
		{"operational: plain error", stderrors.New("boom"), classOperational},
		{"nil error -> operational", nil, classOperational},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classify(tt.err); got != tt.want {
				t.Errorf("classify() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name              string
		args              []string
		wantErr           bool
		wantFile          string
		wantSubject       string
		wantIssuer        string
		wantTimeout       time.Duration
		wantKnownTagsFile string
	}{
		{
			name:        "defaults",
			args:        nil,
			wantFile:    "checkpoint_v2.txt",
			wantTimeout: defaultTimeout,
		},
		{
			name:        "identity flags",
			args:        []string{"--file", "cp.txt", "--cert-subject", "^sub$", "--cert-issuer", "^iss$", "--timeout", "30s"},
			wantFile:    "cp.txt",
			wantSubject: "^sub$",
			wantIssuer:  "^iss$",
			wantTimeout: 30 * time.Second,
		},
		{
			name:              "known tags file",
			args:              []string{"--known-tags-file", "tags.txt"},
			wantFile:          "checkpoint_v2.txt",
			wantTimeout:       defaultTimeout,
			wantKnownTagsFile: "tags.txt",
		},
		{
			name:    "issuer without subject rejected",
			args:    []string{"--cert-issuer", "^iss$"},
			wantErr: true,
		},
		{
			name:    "non-positive timeout rejected",
			args:    []string{"--timeout", "0"},
			wantErr: true,
		},
		{
			name:    "unknown flag rejected",
			args:    []string{"--nope"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := parseFlags(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseFlags() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if opts.checkpointFile != tt.wantFile {
				t.Errorf("checkpointFile = %q, want %q", opts.checkpointFile, tt.wantFile)
			}
			if opts.certSubject != tt.wantSubject {
				t.Errorf("certSubject = %q, want %q", opts.certSubject, tt.wantSubject)
			}
			if opts.certIssuer != tt.wantIssuer {
				t.Errorf("certIssuer = %q, want %q", opts.certIssuer, tt.wantIssuer)
			}
			if opts.timeout != tt.wantTimeout {
				t.Errorf("timeout = %v, want %v", opts.timeout, tt.wantTimeout)
			}
			if opts.knownTagsFile != tt.wantKnownTagsFile {
				t.Errorf("knownTagsFile = %q, want %q", opts.knownTagsFile, tt.wantKnownTagsFile)
			}
		})
	}
}

func TestLoadKnownTags(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "tags.txt")
	if err := os.WriteFile(good, []byte("v0.18.0-rc1\nv0.17.0\n\n  v0.16.0  \n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	crlf := filepath.Join(dir, "crlf.txt")
	if err := os.WriteFile(crlf, []byte("v1.0.0\r\nv1.0.0\r\nv2.0.0\r\n"), 0o600); err != nil {
		t.Fatalf("seed crlf: %v", err)
	}
	oversize := filepath.Join(dir, "oversize.txt")
	if err := os.WriteFile(oversize, bytes.Repeat([]byte("a\n"), maxKnownTagsFileBytes), 0o600); err != nil {
		t.Fatalf("seed oversize: %v", err)
	}
	tests := []struct {
		name     string
		path     string
		want     map[string]bool
		wantErr  bool
		wantCode error // when wantErr, the error must match this code via errors.Is
	}{
		{name: "empty path -> empty set", path: "", want: map[string]bool{}},
		{name: "reads + trims + skips blanks", path: good, want: map[string]bool{"v0.18.0-rc1": true, "v0.17.0": true, "v0.16.0": true}},
		{name: "crlf + duplicate tags", path: crlf, want: map[string]bool{"v1.0.0": true, "v2.0.0": true}},
		{name: "missing file when path set -> error", path: filepath.Join(dir, "nope.txt"), wantErr: true, wantCode: errors.New(errors.ErrCodeInvalidRequest, "")},
		{name: "oversize file -> error", path: oversize, wantErr: true, wantCode: errors.New(errors.ErrCodeInvalidRequest, "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := loadKnownTags(tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if tt.wantCode != nil && !stderrors.Is(err, tt.wantCode) {
					t.Errorf("code = %v, want %v", err, tt.wantCode)
				}
				return
			}
			if got == nil {
				t.Fatal("want non-nil set")
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d tags, want %d (%v)", len(got), len(tt.want), got)
			}
			for k := range tt.want {
				if !got[k] {
					t.Errorf("missing tag %q", k)
				}
			}
		})
	}
}
