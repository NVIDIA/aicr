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
	"strings"
	"testing"
)

func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestPinnedChartVersion(t *testing.T) {
	tests := []struct {
		name     string
		registry string
		want     string
		wantErr  bool
	}{
		{
			name: "reads the component's pinned version",
			registry: "components:\n  - name: other\n    helm:\n      defaultVersion: v9.9.9\n" +
				"  - name: nvcre\n    helm:\n      defaultVersion: v0.2.0\n",
			want: "v0.2.0",
		},
		{
			name:     "component absent",
			registry: "components:\n  - name: other\n    helm:\n      defaultVersion: v1.0.0\n",
			wantErr:  true,
		},
		{
			// An unpinned chart must not silently resolve to the default
			// branch: the closure would describe whatever upstream had at
			// generation time rather than what AICR ships.
			name:     "component present but unpinned",
			registry: "components:\n  - name: nvcre\n    helm: {}\n",
			wantErr:  true,
		},
		{
			name:     "malformed registry",
			registry: "components: [this is: not: valid\n",
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pinnedChartVersion(writeTemp(t, "registry.yaml", tt.registry))
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPinnedChartVersionMissingFile(t *testing.T) {
	if _, err := pinnedChartVersion(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Error("pinnedChartVersion on a missing registry = nil error, want failure")
	}
}

// TestRenderIsDeterministic guards the -check comparison: if marshaling walked
// randomized map order, a regeneration with identical inputs would report the
// committed file as stale.
func TestRenderIsDeterministic(t *testing.T) {
	doc := &closure{
		Component:     "nvcre",
		SourceVersion: "v0.2.0",
		Platform:      "aws",
		Architecture:  "h100",
		Entries:       []string{"communication/nccl-all-reduce"},
		Images: []closureImage{
			{Image: "example.invalid/b:v1", Digest: "sha256:bb"},
			{Image: "example.invalid/a:v1", Digest: "sha256:aa"},
		},
		RuntimeFetch: []runtimeFetch{{URL: "https://example.invalid/x.git", Ref: "v1"}},
	}

	first, err := render(doc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for i := range 5 {
		again, rerr := render(doc)
		if rerr != nil {
			t.Fatalf("render %d: %v", i, rerr)
		}
		if string(again) != string(first) {
			t.Fatalf("render is not deterministic on pass %d", i)
		}
	}

	out := string(first)
	if !strings.Contains(out, "GENERATED FILE - DO NOT EDIT") {
		t.Error("rendered output is missing the generated-file marker")
	}
	if !strings.Contains(out, "sha256:aa") || !strings.Contains(out, "sha256:bb") {
		t.Errorf("rendered output dropped an image digest:\n%s", out)
	}
}

func TestDedupeFetches(t *testing.T) {
	in := []runtimeFetch{
		{URL: "https://example.invalid/b.git", Ref: "v2"},
		{URL: "https://example.invalid/a.git", Ref: "v1"},
		{URL: "https://example.invalid/b.git", Ref: "v2"},
		// Same repository at a different ref is a distinct fetch, not a dupe.
		{URL: "https://example.invalid/a.git", Ref: "v2"},
	}
	got := dedupeFetches(in)
	if len(got) != 3 {
		t.Fatalf("got %d fetches, want 3: %v", len(got), got)
	}
	if got[0].URL != "https://example.invalid/a.git" || got[0].Ref != "v1" {
		t.Errorf("not sorted by URL then ref: %v", got)
	}
}

func TestSortedUnique(t *testing.T) {
	got := sortedUnique([]string{"c", "a", "b", "a"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// TestNormalizeEntries pins the review follow-up on #3087: trimming happened
// inside the resolution loop, so the scope recorded in the committed closure
// kept whatever the flag was given.
func TestNormalizeEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input []string
		want  []string
	}{
		{
			// The flag value CodeRabbit named: a space after the comma and a
			// trailing one wrote " b" and "" into the file, and " b" sorted
			// ahead of "a".
			name:  "trailing and interior whitespace",
			input: []string{"training/b", " communication/a", ""},
			want:  []string{"communication/a", "training/b"},
		},
		{
			name:  "already normalized",
			input: []string{"communication/a", "training/b"},
			want:  []string{"communication/a", "training/b"},
		},
		{
			name:  "all empty",
			input: []string{"", "  "},
			want:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := normalizeEntries(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("normalizeEntries(%q) = %q, want %q", tt.input, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("entry %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
