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
	"io"
	"testing"
	"time"
)

func TestRealMain(t *testing.T) {
	ok := func(context.Context, options, io.Writer) error { return nil }
	fail := func(context.Context, options, io.Writer) error { return stderrors.New("boom") }
	tests := []struct {
		name string
		args []string
		run  runFunc
		want int
	}{
		{name: "bad args -> 2", args: []string{"--nope"}, run: ok, want: 2},
		{name: "run error -> 1", args: nil, run: fail, want: 1},
		{name: "clean -> 0", args: nil, run: ok, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := realMain(tt.args, tt.run); got != tt.want {
				t.Errorf("realMain() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantErr     bool
		wantFile    string
		wantSubject string
		wantIssuer  string
		wantTimeout time.Duration
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
		})
	}
}
