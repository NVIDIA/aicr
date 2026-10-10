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
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

const testEntries = "testdata/entries"

func TestSelectorMatches(t *testing.T) {
	tests := []struct {
		name     string
		sel      selector
		platform string
		arch     string
		want     bool
	}{
		{"empty selector matches everything", selector{}, "aws", "h100", true},
		{"platform match", selector{platformEquals: "aws"}, "aws", "h100", true},
		{"platform mismatch", selector{platformEquals: "gcp"}, "aws", "h100", false},
		{"arch equals match", selector{archEquals: "gb300"}, "aws", "gb300", true},
		{"arch equals mismatch", selector{archEquals: "gb300"}, "aws", "h100", false},
		{"arch in match", selector{archIn: []string{"gb200", "gb300"}}, "aws", "gb300", true},
		{"arch in mismatch", selector{archIn: []string{"gb200", "gb300"}}, "aws", "h100", false},
		{"arch notIn admits h100", selector{archNotIn: []string{"gb200", "gb300"}}, "aws", "h100", true},
		{"arch notIn excludes gb300", selector{archNotIn: []string{"gb200", "gb300"}}, "aws", "gb300", false},
		{
			"both constraints must hold",
			selector{platformEquals: "aws", archNotIn: []string{"gb300"}},
			"gcp", "h100", false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.sel.matches(tt.platform, tt.arch); got != tt.want {
				t.Errorf("matches(%q, %q) = %v, want %v", tt.platform, tt.arch, got, tt.want)
			}
		})
	}
}

// TestResolveEntryExcludesOtherPlatformPaths is the regression guard for the
// scope ADR-025 draws. The sample entry carries a `:latest` image on its GB300
// block; resolving for h100 must not reach it, and resolving for gb300 must.
// A scanner that ignored selectors would pass the first assertion only by
// accident and fail the second.
func TestResolveEntryExcludesOtherPlatformPaths(t *testing.T) {
	tests := []struct {
		name        string
		arch        string
		want        []string
		notWant     []string
		wantFetches int
	}{
		{
			name:    "h100 takes the EFA path and its lib fragment",
			arch:    "h100",
			want:    []string{"example.invalid/base:v1", "example.invalid/nccl:pinned-tag", "example.invalid/efa-sidecar:v4"},
			notWant: []string{"example.invalid/nccl:latest", "example.invalid/tcpxo:v2"},
		},
		{
			name:    "gb300 takes the RoCE path and does reach the latest tag",
			arch:    "gb300",
			want:    []string{"example.invalid/base:v1", "example.invalid/nccl:latest"},
			notWant: []string{"example.invalid/nccl:pinned-tag", "example.invalid/efa-sidecar:v4"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			images, _, err := resolveEntry(testEntries, "communication/sample", "aws", tt.arch)
			if err != nil {
				t.Fatalf("resolveEntry: %v", err)
			}
			for _, w := range tt.want {
				if !slices.Contains(images, w) {
					t.Errorf("missing image %q; got %v", w, images)
				}
			}
			for _, n := range tt.notWant {
				if slices.Contains(images, n) {
					t.Errorf("image %q must not be on the %s path; got %v", n, tt.arch, images)
				}
			}
		})
	}
}

func TestResolveEntryCapturesRuntimeFetches(t *testing.T) {
	images, fetches, err := resolveEntry(testEntries, "training/fetches", "aws", "h100")
	if err != nil {
		t.Fatalf("resolveEntry: %v", err)
	}
	if !slices.Contains(images, "example.invalid/torch:v3") {
		t.Errorf("missing trainer image; got %v", images)
	}
	if len(fetches) != 1 {
		t.Fatalf("got %d runtime fetches, want 1: %v", len(fetches), fetches)
	}
	if fetches[0].URL != "https://example.invalid/Megatron-LM.git" {
		t.Errorf("fetch URL = %q", fetches[0].URL)
	}
	// The branch matters as much as the URL: a clone pinned to a tag is a
	// different exposure from one tracking a branch, and both are recorded so
	// the distinction survives into the generated file.
	if fetches[0].Ref != "core_v9.9.9" {
		t.Errorf("fetch ref = %q, want core_v9.9.9", fetches[0].Ref)
	}
}

func TestIncludeRejectsEscapingReference(t *testing.T) {
	t.Parallel()

	for _, fn := range []string{directiveLib, directiveIncludeTemplate, directiveIncludeFile} {
		for _, ref := range []string{"../../../etc/passwd", "/etc/passwd", ""} {
			t.Run(fn+" "+ref, func(t *testing.T) {
				t.Parallel()

				s := &entryScan{root: testEntries, entry: "training/nested", seen: map[string]struct{}{}}
				if err := s.include(directive{fn: fn, ref: ref}); err == nil {
					t.Errorf("include(%s %q) = nil error, want rejection", fn, ref)
				}
			})
		}
	}
}

// writeCatalog writes files, keyed by relative path, under a new temporary
// directory and returns the directory.
func writeCatalog(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return dir
}

func TestScanDirectives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		want    []directive
		wantErr bool
	}{
		{
			name: "lib piped through indent",
			body: `{{ lib "deps/a.yaml" . | indent 2 }}`,
			want: []directive{{fn: directiveLib, ref: "deps/a.yaml"}},
		},
		{
			name: "trim markers and includeTemplate",
			body: `{{- includeTemplate "configs/train.sh" . -}}`,
			want: []directive{{fn: directiveIncludeTemplate, ref: "configs/train.sh"}},
		},
		{
			name: "call nested in parentheses",
			body: `{{ indent 2 (lib "deps/a.yaml" .) }}`,
			want: []directive{{fn: directiveLib, ref: "deps/a.yaml"}},
		},
		{
			name: "includeFile and lib in separate actions",
			body: "{{ includeFile \"raw.txt\" }}\nkey: v\n{{ lib \"nccl/b.yaml\" . }}",
			want: []directive{
				{fn: directiveIncludeFile, ref: "raw.txt"},
				{fn: directiveLib, ref: "nccl/b.yaml"},
			},
		},
		{
			name: "field and conditional actions are not directives",
			body: `{{ .Values.library }} {{ if .SourceRepo }}{{ .SourceRepo }}{{ end }}`,
		},
		{
			name: "directive words outside an action are text",
			body: `echo lib "deps/a.yaml" includeTemplate "x"`,
		},
		{
			name: "directive word inside a string argument",
			body: `{{ printf "some lib text" }}`,
		},
		{
			name: "directive word inside a raw string argument",
			body: "{{ printf `uses includeFile here` }}",
		},
		{
			name: "template comments naming directives",
			body: `{{/* lib is spliced below */}} {{- /* includeTemplate "x" */ -}}`,
		},
		{
			name: "string argument ahead of a directive",
			body: `{{ printf "%s lib" (lib "deps/a.yaml" .) }}`,
			want: []directive{{fn: directiveLib, ref: "deps/a.yaml"}},
		},
		{
			name: "raw string target",
			body: "{{ lib `deps/a.yaml` . }}",
			want: []directive{{fn: directiveLib, ref: "deps/a.yaml"}},
		},
		{
			name: "escaped quote in target",
			body: `{{ lib "deps/a\"b.yaml" . }}`,
			want: []directive{{fn: directiveLib, ref: `deps/a"b.yaml`}},
		},
		{
			name:    "lib with a field argument",
			body:    `{{ lib .LibPath . }}`,
			wantErr: true,
		},
		{
			name:    "includeTemplate with a computed argument",
			body:    `{{ includeTemplate (printf "configs/%s.sh" .Name) . }}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := scanDirectives(tt.body)
			if (err != nil) != tt.wantErr {
				t.Fatalf("scanDirectives(%q) error = %v, wantErr %v", tt.body, err, tt.wantErr)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("scanDirectives(%q) = %+v, want %+v", tt.body, got, tt.want)
			}
		})
	}
}

// TestResolveEntryFollowsNestedDirectives checks that resolveEntry reaches a
// lib spliced in by a lib, a lib spliced in by an includeTemplate script, and a
// clone inside that script, and does not follow directives in includeFile content.
func TestResolveEntryFollowsNestedDirectives(t *testing.T) {
	t.Parallel()

	images, fetches, err := resolveEntry(testEntries, "training/nested", "aws", "h100")
	// raw.txt, spliced in by includeFile, names a lib that does not exist.
	if err != nil {
		t.Fatalf("resolveEntry: %v", err)
	}
	for _, want := range []string{
		"example.invalid/torch:v3",
		"example.invalid/nested-sidecar:v5",
		"example.invalid/raw-noted:v6",
	} {
		if !slices.Contains(images, want) {
			t.Errorf("missing image %q; got %v", want, images)
		}
	}

	got := dedupeFetches(fetches)
	want := []runtimeFetch{
		{URL: "https://example.invalid/Trainer.git", Ref: "v1.0.0"},
		{URL: "https://example.invalid/dataset.tar.gz"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("fetches = %+v, want %+v", got, want)
	}
}

func TestResolveEntryTerminatesOnLibCycle(t *testing.T) {
	t.Parallel()

	dir := writeCatalog(t, map[string]string{
		"communication/loop.yaml": "dependencies:\n{{ lib \"deps/a.yaml\" . }}\n",
		"_lib/deps/a.yaml":        "image: example.invalid/a:v1\n{{ lib \"deps/b.yaml\" . }}\n",
		"_lib/deps/b.yaml":        "image: example.invalid/b:v1\n{{ lib \"deps/a.yaml\" . }}\n",
	})

	images, _, err := resolveEntry(dir, "communication/loop", "aws", "h100")
	if err != nil {
		t.Fatalf("resolveEntry: %v", err)
	}
	slices.Sort(images)
	if want := []string{"example.invalid/a:v1", "example.invalid/b:v1"}; !slices.Equal(images, want) {
		t.Errorf("images = %v, want %v", images, want)
	}
}

func TestResolveEntryMissingIncludeIsAnError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		entry string
	}{
		{"missing nested lib", "dependencies:\n{{ lib \"deps/outer.yaml\" . }}\n"},
		{"missing includeTemplate", "dependencies:\n{{ includeTemplate \"configs/train.sh\" . }}\n"},
		{"missing includeFile", "dependencies:\n{{ includeFile \"raw.txt\" }}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			dir := writeCatalog(t, map[string]string{
				"training/e.yaml":      tt.entry,
				"_lib/deps/outer.yaml": "{{ lib \"nccl/absent.yaml\" . }}\n",
			})
			if _, _, err := resolveEntry(dir, "training/e", "aws", "h100"); err == nil {
				t.Error("resolveEntry = nil error, want failure on the missing target")
			}
		})
	}
}

func TestResolveEntrySymlinks(t *testing.T) {
	t.Parallel()

	symlink := func(t *testing.T, target, link string) {
		t.Helper()
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink %s -> %s: %v", link, target, err)
		}
	}

	t.Run("lib target escaping the catalog is rejected", func(t *testing.T) {
		t.Parallel()

		outside := writeCatalog(t, map[string]string{"secret.yaml": "image: example.invalid/outside:v1\n"})
		dir := writeCatalog(t, map[string]string{
			"training/e.yaml": "dependencies:\n{{ lib \"deps/escape.yaml\" . }}\n",
			"_lib/deps/.keep": "",
		})
		symlink(t, filepath.Join(outside, "secret.yaml"), filepath.Join(dir, "_lib", "deps", "escape.yaml"))

		_, _, err := resolveEntry(dir, "training/e", "aws", "h100")
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	})

	t.Run("entry escaping the catalog is rejected", func(t *testing.T) {
		t.Parallel()

		outside := writeCatalog(t, map[string]string{"entry.yaml": "image: example.invalid/outside:v1\n"})
		dir := writeCatalog(t, map[string]string{"training/.keep": ""})
		symlink(t, filepath.Join(outside, "entry.yaml"), filepath.Join(dir, "training", "e.yaml"))

		_, _, err := resolveEntry(dir, "training/e", "aws", "h100")
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	})

	t.Run("lib target symlinked within the catalog is read", func(t *testing.T) {
		t.Parallel()

		dir := writeCatalog(t, map[string]string{
			"training/e.yaml":     "dependencies:\n{{ lib \"deps/alias.yaml\" . }}\n",
			"_lib/deps/real.yaml": "image: example.invalid/inside:v1\n",
		})
		symlink(t, "real.yaml", filepath.Join(dir, "_lib", "deps", "alias.yaml"))

		images, _, err := resolveEntry(dir, "training/e", "aws", "h100")
		if err != nil {
			t.Fatalf("resolveEntry: %v", err)
		}
		if !slices.Equal(images, []string{"example.invalid/inside:v1"}) {
			t.Errorf("images = %v, want the symlinked lib's image", images)
		}
	})

	t.Run("catalog reached through a symlink is read", func(t *testing.T) {
		t.Parallel()

		dir := writeCatalog(t, map[string]string{
			"training/e.yaml":  "dependencies:\n{{ lib \"deps/a.yaml\" . }}\n",
			"_lib/deps/a.yaml": "image: example.invalid/a:v1\n",
		})
		link := filepath.Join(t.TempDir(), "entries")
		symlink(t, dir, link)

		images, _, err := resolveEntry(link, "training/e", "aws", "h100")
		if err != nil {
			t.Fatalf("resolveEntry: %v", err)
		}
		if !slices.Equal(images, []string{"example.invalid/a:v1"}) {
			t.Errorf("images = %v, want the lib's image", images)
		}
	})
}

func TestResolveEntryMissingEntryIsAnError(t *testing.T) {
	if _, _, err := resolveEntry(testEntries, "communication/absent", "aws", "h100"); err == nil {
		t.Error("resolveEntry on a missing entry = nil error, want failure")
	}
}

// TestScanRuntimeFetchesCloneForms pins the review follow-up on #3087: the
// single-regex form required the branch flag ahead of the URL, so the two
// other common spellings recorded no fetch at all and the closure implied the
// path could run from a mirrored registry alone.
func TestScanRuntimeFetchesCloneForms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		wantURL string
		wantRef string
	}{
		{
			name:    "branch flag before the url",
			body:    "git clone -b v1.2.3 https://github.com/o/r.git /src",
			wantURL: "https://github.com/o/r.git",
			wantRef: "v1.2.3",
		},
		{
			name:    "branch flag after the url",
			body:    "git clone https://github.com/o/r.git -b v1.2.3",
			wantURL: "https://github.com/o/r.git",
			wantRef: "v1.2.3",
		},
		{
			name:    "url with a target directory and no ref",
			body:    "git clone https://github.com/o/r.git /src",
			wantURL: "https://github.com/o/r.git",
		},
		{
			name:    "long branch flag after the url",
			body:    "git clone --depth 1 https://github.com/o/r.git --branch main",
			wantURL: "https://github.com/o/r.git",
			wantRef: "main",
		},
		{
			// NVCRE v0.6.0's shape. The remote moved into a variable assigned
			// from a template that makes it overridable, and the clone
			// references only the variable. Unresolved, this recorded no
			// fetch at all and the closure read as air-gap clean.
			name: "remote behind a shell variable assigned from a template",
			body: `repo="{{ if .SourceRepo }}{{ .SourceRepo }}{{ else }}https://github.com/NVIDIA/Megatron-LM.git{{ end }}"
git clone --depth 1 -b core_v0.15.2 "$repo" "$tmp"`,
			wantURL: "https://github.com/NVIDIA/Megatron-LM.git",
			wantRef: "core_v0.15.2",
		},
		{
			// git accepts an https remote with no .git suffix. Before the
			// fallback pattern this resolved to nothing, and since an
			// unresolvable remote fails closed, a valid clone halted the
			// derivation instead of being recorded.
			name:    "remote without a .git suffix",
			body:    "git clone https://github.com/o/r /src",
			wantURL: "https://github.com/o/r",
		},
		{
			name:    "suffixless remote behind a variable",
			body:    "repo=https://github.com/o/r\ngit clone \"$repo\" /src",
			wantURL: "https://github.com/o/r",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := scanRuntimeFetches(tt.body)
			if err != nil {
				t.Fatalf("scanRuntimeFetches(%q): %v", tt.body, err)
			}
			if len(got) != 1 {
				t.Fatalf("scanRuntimeFetches(%q) = %+v, want exactly one fetch", tt.body, got)
			}
			if got[0].URL != tt.wantURL {
				t.Errorf("URL = %q, want %q", got[0].URL, tt.wantURL)
			}
			if got[0].Ref != tt.wantRef {
				t.Errorf("Ref = %q, want %q", got[0].Ref, tt.wantRef)
			}
		})
	}
}

// TestScanRuntimeFetchesRejectsUnresolvableClone pins the fail-closed half:
// a clone whose remote cannot be resolved must stop the derivation rather
// than drop out of the inventory. An omitted clone is not a smaller closure,
// it is a closure that says the path needs no network at pod start.
func TestScanRuntimeFetchesRejectsUnresolvableClone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{
			name: "remote in an unassigned variable",
			body: `git clone --depth 1 "$repo" /src`,
		},
		{
			name: "variable assigned something that is not a git url",
			body: "repo=/mnt/local/mirror\ngit clone --depth 1 \"$repo\" /src",
		},
		{
			name: "ssh remote",
			body: "git clone git@github.com:o/r.git /src",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := scanRuntimeFetches(tt.body)
			if err == nil {
				t.Fatalf("scanRuntimeFetches(%q) = %+v, want an error", tt.body, got)
			}
		})
	}
}

// TestScanRuntimeFetchesReassignedRemote pins the second half of the
// preceding-assignments rule: a block that reassigns the remote between two
// clones must record both repositories. Resolving against the whole body gave
// each clone the last assignment, and because the refs then matched too,
// dedupeFetches collapsed the pair and the first repository left no trace.
func TestScanRuntimeFetchesReassignedRemote(t *testing.T) {
	t.Parallel()

	body := `repo=https://github.com/o/first.git
git clone --depth 1 -b v1 "$repo" /src/first
repo=https://github.com/o/second.git
git clone --depth 1 -b v1 "$repo" /src/second`

	got, err := scanRuntimeFetches(body)
	if err != nil {
		t.Fatalf("scanRuntimeFetches: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d fetches (%+v), want 2", len(got), got)
	}
	for i, want := range []string{
		"https://github.com/o/first.git",
		"https://github.com/o/second.git",
	} {
		if got[i].URL != want {
			t.Errorf("fetch %d URL = %q, want %q", i, got[i].URL, want)
		}
	}
}
