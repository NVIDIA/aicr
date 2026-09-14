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

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
)

// writeSource drops a source document into a temp dir and returns its path.
func writeSource(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "openvex.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	return path
}

func TestRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		source   string
		omitOut  bool
		omitIn   bool
		image    string
		digest   string
		wantErr  bool
		wantKept string
	}{
		{
			name:     "writes the projection",
			source:   sourceDocument,
			image:    benchImage,
			digest:   amd64Digest,
			wantKept: "bound 1 statement(s)",
		},
		{
			name:     "image with no statements still writes a document",
			source:   sourceDocument,
			image:    "ghcr.io/nvidia/aicr",
			digest:   amd64Digest,
			wantKept: "bound 0 statement(s)",
		},
		{name: "missing -out", source: sourceDocument, omitOut: true, image: benchImage, digest: amd64Digest, wantErr: true},
		{name: "missing -in", source: sourceDocument, omitIn: true, image: benchImage, digest: amd64Digest, wantErr: true},
		{name: "empty source file", source: "", image: benchImage, digest: amd64Digest, wantErr: true},
		{name: "bad digest", source: sourceDocument, image: benchImage, digest: "sha256:nope", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := options{
				in:     writeSource(t, tt.source),
				out:    filepath.Join(t.TempDir(), "bound.openvex.json"),
				image:  tt.image,
				digest: tt.digest,
			}
			if tt.omitOut {
				o.out = ""
			}
			if tt.omitIn {
				o.in = ""
			}
			var stdout bytes.Buffer
			err := run(o, &stdout)
			if (err != nil) != tt.wantErr {
				t.Fatalf("run() error = %v, wantErr %t", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !strings.Contains(stdout.String(), tt.wantKept) {
				t.Errorf("run() stdout = %q, want it to contain %q", stdout.String(), tt.wantKept)
			}
			written, readErr := os.ReadFile(o.out)
			if readErr != nil {
				t.Fatalf("read projection: %v", readErr)
			}
			if len(written) == 0 {
				t.Error("projection file is empty")
			}
		})
	}
}

// TestRunOnMissingFile keeps the failure a clean error rather than a panic; a
// release that cannot find its VEX source must stop before attesting.
func TestRunOnMissingFile(t *testing.T) {
	t.Parallel()
	o := options{
		in:     filepath.Join(t.TempDir(), "absent.json"),
		out:    filepath.Join(t.TempDir(), "bound.json"),
		image:  benchImage,
		digest: amd64Digest,
	}
	if err := run(o, &bytes.Buffer{}); err == nil {
		t.Fatal("run() error = nil, want a failure on a missing source")
	}
}

// TestReadSourceEnforcesSizeCap covers the bounded-read rule: the source is
// streamed under a limit rather than allocated whole, so a path that resolves
// to something unbounded fails instead of exhausting memory.
func TestReadSourceEnforcesSizeCap(t *testing.T) {
	t.Parallel()
	oversized := strings.Repeat("a", int(defaults.MaxOpenVEXBytes)+1)
	if _, err := readSource(writeSource(t, oversized)); err == nil {
		t.Fatal("readSource() error = nil, want a size-limit failure")
	}
	atLimit := strings.Repeat("a", int(defaults.MaxOpenVEXBytes))
	if _, err := readSource(writeSource(t, atLimit)); err != nil {
		t.Fatalf("readSource() error = %v at exactly the limit, want success", err)
	}
}
