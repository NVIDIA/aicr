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
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
)

func TestRun(t *testing.T) {
	t.Parallel()
	exe, err := aicrBinary()
	if err != nil {
		t.Fatalf("read test binary: %v", err)
	}
	b := newLayout(t)
	index := b.index(b.image(testImage{platform: amd64, layers: []testLayer{{entries: []entry{
		file("etc/x", "y"),
		{name: "ko-app/aicr", typeflag: tar.TypeReg, body: exe},
		{name: "usr/lib/libother.so", typeflag: tar.TypeReg, body: []byte("\x7fELF not go")},
	}}}}))
	valid := func(t *testing.T) options {
		dir := t.TempDir()
		return options{
			layout:   b.dir,
			platform: "linux/amd64",
			index:    index,
			out:      filepath.Join(dir, "out"),
			coverage: filepath.Join(dir, "coverage.json"),
		}
	}
	tests := []struct {
		name    string
		mutate  func(*options)
		want    errors.ErrorCode
		wantMsg string
	}{
		{name: "writes coverage"},
		{name: "-out with a trailing separator", mutate: func(o *options) { o.out += string(filepath.Separator) }},
		{name: "missing -layout", mutate: func(o *options) { o.layout = "" }, want: errors.ErrCodeInvalidRequest, wantMsg: "-layout is required"},
		{name: "missing -platform", mutate: func(o *options) { o.platform = "" }, want: errors.ErrCodeInvalidRequest, wantMsg: "-platform is required"},
		{name: "missing -index", mutate: func(o *options) { o.index = "" }, want: errors.ErrCodeInvalidRequest, wantMsg: "-index is required"},
		{name: "malformed -index", mutate: func(o *options) { o.index = "sha256:nope" }, want: errors.ErrCodeInvalidRequest, wantMsg: "index digest must be"},
		{name: "missing -out", mutate: func(o *options) { o.out = "" }, want: errors.ErrCodeInvalidRequest, wantMsg: "-out is required"},
		{name: "missing -coverage", mutate: func(o *options) { o.coverage = "" }, want: errors.ErrCodeInvalidRequest, wantMsg: "-coverage is required"},
		{name: "-coverage inside -out", mutate: func(o *options) { o.coverage = filepath.Join(o.out, "coverage.json") }, want: errors.ErrCodeInvalidRequest, wantMsg: "outside -out"},
		{name: "-coverage is -out", mutate: func(o *options) { o.coverage = o.out }, want: errors.ErrCodeInvalidRequest, wantMsg: "outside -out"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := valid(t)
			if tt.mutate != nil {
				tt.mutate(&o)
			}
			var stdout bytes.Buffer
			err := run(context.Background(), o, &stdout)
			if tt.want != "" {
				errorstest.WantReportedCode(t, err, tt.want)
				if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
					t.Errorf("error = %v, want one naming %q", err, tt.wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if !strings.Contains(stdout.String(), "1 layers, 3 files, 1 AICR executables, 1 other binaries") {
				t.Errorf("summary = %q", stdout.String())
			}
			data, err := os.ReadFile(o.coverage)
			if err != nil {
				t.Fatalf("coverage: %v", err)
			}
			// .github/actions/image-checks reads these names with jq.
			var report map[string]any
			if err = json.Unmarshal(data, &report); err != nil {
				t.Fatalf("coverage is not JSON: %v", err)
			}
			keys := func(m map[string]any) []string {
				out := make([]string, 0, len(m))
				for k := range m {
					out = append(out, k)
				}
				slices.Sort(out)
				return out
			}
			if got := keys(report); !slices.Equal(got, []string{"config", "index", "layers", "manifest", "platform", "totals"}) {
				t.Errorf("coverage keys = %v", got)
			}
			totals, _ := report["totals"].(map[string]any)
			want := map[string]float64{
				"files": 3, "empty": 0, "binaries": 2, "aicrExecutables": 1,
				"symlinks": 0, "hardlinks": 0, "whiteouts": 0, "special": 0,
			}
			for key, value := range want {
				if totals[key] != value {
					t.Errorf("totals.%s = %v, want %v", key, totals[key], value)
				}
			}
			if _, ok := totals["bytes"]; !ok || len(totals) != len(want)+1 {
				t.Errorf("totals keys = %v", keys(totals))
			}
			if report["index"] != index || report["platform"] != "linux/amd64" {
				t.Errorf("coverage identity = %v %v", report["index"], report["platform"])
			}
		})
	}
}

// The report path is checked as the filesystem resolves it, so a symlink
// cannot route the report into the tree the scanners read. Each case is
// rejected before anything is unpacked.
func TestRunResolvesCoveragePath(t *testing.T) {
	t.Parallel()
	b := newLayout(t)
	index := b.index(b.image(testImage{platform: amd64, layers: []testLayer{{entries: []entry{file("etc/x", "y")}}}}))
	tests := []struct {
		name string
		// setup returns -coverage for an -out of dir/out.
		setup   func(t *testing.T, dir string) string
		wantMsg string
	}{
		{
			name: "-out through a linked directory",
			setup: func(t *testing.T, dir string) string {
				if err := os.Symlink(dir, filepath.Join(dir, "reports")); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(dir, "reports", "out")
			},
			wantMsg: "outside -out",
		},
		{
			name: "a directory linked to -out before it exists",
			setup: func(t *testing.T, dir string) string {
				if err := os.Symlink(filepath.Join(dir, "out"), filepath.Join(dir, "reports")); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(dir, "reports", "coverage.json")
			},
			wantMsg: "failed to resolve",
		},
		{
			name: "a report that is a symlink",
			setup: func(t *testing.T, dir string) string {
				link := filepath.Join(dir, "coverage.json")
				if err := os.Symlink(filepath.Join(dir, "out", "coverage.json"), link); err != nil {
					t.Fatal(err)
				}
				return link
			},
			wantMsg: "must not be a symlink",
		},
		{
			// The kernel follows reports to real/sub before applying "..", so
			// this names real/coverage.json, a symlink; cleaned first, it
			// would name dir/coverage.json, which does not exist.
			name: "a symlinked report reached through .. after a link",
			setup: func(t *testing.T, dir string) string {
				if err := os.MkdirAll(filepath.Join(dir, "real", "sub"), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(dir, "real", "sub"), filepath.Join(dir, "reports")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(dir, "elsewhere"), filepath.Join(dir, "real", "coverage.json")); err != nil {
					t.Fatal(err)
				}
				sep := string(filepath.Separator)
				return filepath.Join(dir, "reports") + sep + ".." + sep + "coverage.json"
			},
			wantMsg: "must not be a symlink",
		},
		{
			name: "a report path ending in ..",
			setup: func(_ *testing.T, dir string) string {
				return filepath.Join(dir, "reports") + string(filepath.Separator) + ".."
			},
			wantMsg: "must end in a file or directory name",
		},
		{
			name: "a missing report directory",
			setup: func(_ *testing.T, dir string) string {
				return filepath.Join(dir, "absent", "coverage.json")
			},
			wantMsg: "failed to resolve",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			o := options{layout: b.dir, platform: "linux/amd64", index: index, out: filepath.Join(dir, "out")}
			o.coverage = tt.setup(t, dir)
			err := run(context.Background(), o, &bytes.Buffer{})
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			if err == nil || !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %v, want one naming %q", err, tt.wantMsg)
			}
			if _, statErr := os.Lstat(o.out); !os.IsNotExist(statErr) {
				t.Errorf("-out was created before the report path was rejected (%v)", statErr)
			}
		})
	}

	// A report beside -out through a linked parent directory stays valid.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "real"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	o := options{layout: b.dir, platform: "linux/amd64", index: index,
		out: filepath.Join(dir, "linked", "out"), coverage: filepath.Join(dir, "linked", "coverage.json")}
	if err := run(context.Background(), o, &bytes.Buffer{}); err != nil {
		t.Fatalf("run with a linked parent: %v", err)
	}
}
