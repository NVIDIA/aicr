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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

const testNav = `navigation:
  - section: Overview
    contents:
      - page: Introduction
        path: README.md
  - section: User Guide
    contents:
      - page: Guide
        path: user/guide.md
      - section: Nested
        contents:
          - page: Deep
            path: user/deep/page.md
`

func TestClassify(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"docs/README.md":         "",
		"docs/user/guide.md":     "",
		"docs/design/001-adr.md": "",
		"pkg/widget/widget.go":   "",
	})
	s := site{root: root, published: map[string]bool{"docs/README.md": true, "docs/user/guide.md": true}}
	const page = "docs/user/guide.md"

	tests := []struct {
		name       string
		url        string
		wantKind   linkKind
		wantReason string
	}{
		{"published page with fragment", "../README.md#overview", kindRelative, ""},
		{"same page with query", "guide.md?tab=cli", kindRelative, ""},
		{"same-page anchor", "#usage", kindIgnored, ""},
		{"query only", "?tab=cli", kindIgnored, ""},
		{"external", "https://kubernetes.io/docs/", kindIgnored, ""},
		{"mailto", "mailto:a@example.com", kindIgnored, ""},
		{"protocol-relative", "//cdn.example.com/x.js", kindIgnored, ""},
		{"github issue", "https://github.com/NVIDIA/aicr/issues/1", kindIgnored, ""},
		{"github tag ref", "https://github.com/NVIDIA/aicr/blob/v1.0.0/gone.go", kindIgnored, ""},
		{"github branch with main prefix", "https://github.com/NVIDIA/aicr/blob/main-old/gone.go", kindIgnored, ""},
		{"other github repo", "https://github.com/NVIDIA/other/blob/main/gone.go", kindIgnored, ""},
		{"github tree", "https://github.com/NVIDIA/aicr/tree/main/pkg/gadget", kindIgnored, ""},
		{"github blob with line anchor", "https://github.com/NVIDIA/aicr/blob/main/pkg/widget/widget.go#L1", kindGitHub, ""},
		{"github directory with trailing slash", "https://github.com/NVIDIA/aicr/blob/main/pkg/widget/", kindGitHub, ""},
		{"github mixed-case owner and repo", "https://github.com/nvidia/AICR/blob/main/pkg/widget/widget.go", kindGitHub, ""},
		{"github www over http", "http://www.github.com/NVIDIA/aicr/blob/main/pkg/widget/widget.go", kindGitHub, ""},
		{"github repository root", "https://github.com/NVIDIA/aicr/blob/main", kindGitHub, ""},
		{"github unpublished doc", "https://github.com/NVIDIA/aicr/blob/main/docs/design/001-adr.md", kindGitHub, ""},
		{
			"unpublished page", "../design/001-adr.md", kindRelative,
			"resolves to docs/design/001-adr.md, which docs/index.yml does not publish",
		},
		{"missing page", "../READM.md", kindRelative, "resolves to docs/READM.md, which does not exist"},
		{"directory", "../", kindRelative, "resolves to docs, which docs/index.yml does not publish"},
		{"escapes the root", "../../../x.md", kindRelative, "resolves to ../x.md, which does not exist"},
		{
			"root-relative", "/aicr/user-guide/cli-reference", kindRelative,
			"a root-relative link cannot be resolved offline. Link to the page by relative path",
		},
		{"malformed relative escape", "../design/%zz.md", kindRelative, "malformed percent-encoding"},
		{
			"github missing file", "https://github.com/NVIDIA/aicr/blob/main/pkg/widget/missing.go", kindGitHub,
			"names pkg/widget/missing.go, which does not exist in the repository",
		},
		{
			"github dot segments", "https://github.com/NVIDIA/aicr/blob/main/pkg/widget/../widget/widget.go", kindGitHub,
			"names pkg/widget/../widget/widget.go, which is not a canonical repository path",
		},
		{"github malformed escape", "https://github.com/NVIDIA/aicr/blob/main/pkg/%zz", kindGitHub, "malformed percent-encoding"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, reason := s.classify(page, tt.url)
			if kind != tt.wantKind || reason != tt.wantReason {
				t.Errorf("classify(%q) = (%v, %q), want (%v, %q)", tt.url, kind, reason, tt.wantKind, tt.wantReason)
			}
		})
	}
}

func TestExtractLinks(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []link
	}{
		{
			name: "inline and bracketed destinations",
			src:  "# Title\n\nSee [a](a.md) and\n[b](<b c.md>).\n",
			want: []link{{"a.md", 3}, {"b c.md", 4}},
		},
		{
			name: "reference link is reported at its definition",
			src:  "Use [the guide][g] twice: [again][g].\n\n[g]: guide.md#top\n",
			want: []link{{"guide.md#top", 3}},
		},
		{
			name: "code spans and fences hide links",
			src:  "`[x](code.md)`\n\n```markdown\n[y](fenced.md)\n```\n\n~~~\n[z](tilde.md)\n~~~\n\n[real](real.md)\n",
			want: []link{{"real.md", 11}},
		},
		{
			name: "bare URL, autolink, and email",
			src: "Bare https://github.com/NVIDIA/aicr/blob/main/go.mod.\n" +
				"Angle <https://example.com/x> and mail <a@example.com> or b@example.com.\n",
			want: []link{{"https://github.com/NVIDIA/aicr/blob/main/go.mod", 1}, {"https://example.com/x", 2}},
		},
		{
			name: "list items and table cells",
			src:  "- [item](item.md)\n  continued [more](more.md)\n\n| A | B |\n|---|---|\n| [cell](cell.md) | x |\n",
			want: []link{{"item.md", 1}, {"more.md", 2}, {"cell.md", 6}},
		},
	}
	md := goldmark.New(goldmark.WithExtensions(extension.GFM))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractLinks(md, []byte(tt.src))
			if !slices.Equal(got, tt.want) {
				t.Errorf("extractLinks() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPublishedPages(t *testing.T) {
	tests := []struct {
		name    string
		nav     string
		want    []string
		wantErr string
	}{
		{
			name: "nested sections are included and sorted",
			nav:  testNav,
			want: []string{"docs/README.md", "docs/user/deep/page.md", "docs/user/guide.md"},
		},
		{
			name: "duplicates collapse",
			nav:  "navigation:\n  - path: a.md\n  - path: a.md\n",
			want: []string{"docs/a.md"},
		},
		{name: "no paths", nav: "navigation: []\n", wantErr: "no page paths found"},
		{name: "non-string path", nav: "navigation:\n  - path: [a]\n", wantErr: "not a non-empty string"},
		{name: "empty path", nav: "navigation:\n  - path: \"\"\n", wantErr: "not a non-empty string"},
		{name: "invalid yaml", nav: "navigation: [\n", wantErr: "parse docs/index.yml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := publishedPages([]byte(tt.nav))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("publishedPages() error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("publishedPages() error = %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("publishedPages() = %v, want %v", got, tt.want)
			}
		})
	}
}

// writeTree creates each file under root, making parent directories.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRun(t *testing.T) {
	base := map[string]string{
		"docs/index.yml":         testNav,
		"docs/README.md":         "# Intro\n\nSee the [guide](user/guide.md).\n",
		"docs/user/deep/page.md": "# Deep\n\n[Up](../guide.md)\n",
		"docs/design/001-adr.md": "# ADR\n\n[broken](nowhere.md)\n",
		"pkg/widget/widget.go":   "package widget\n",
	}
	tests := []struct {
		name           string
		guide          string
		extra          map[string]string
		wantViolations int
		wantOut        []string
		wantErr        string
	}{
		{
			name:    "reachable links pass",
			guide:   "[Home](../README.md) and [source](https://github.com/NVIDIA/aicr/blob/main/pkg/widget/widget.go)\n",
			wantOut: []string{"OK: 3 relative link(s) and 1 GitHub main link(s) on 3 published page(s) resolve"},
		},
		{
			name:           "unreachable links are reported with the remedy",
			guide:          "# Guide\n\n[ADR](../design/001-adr.md)\n\n[gone](https://github.com/NVIDIA/aicr/blob/main/pkg/gone.go)\n",
			wantViolations: 2,
			wantOut: []string{
				"DOC-LINK: docs/user/guide.md:3: ../design/001-adr.md: resolves to docs/design/001-adr.md, which docs/index.yml does not publish",
				"DOC-LINK: docs/user/guide.md:5: https://github.com/NVIDIA/aicr/blob/main/pkg/gone.go: names pkg/gone.go, which does not exist in the repository",
				"link https://github.com/NVIDIA/aicr/blob/main/<path> instead",
			},
		},
		{
			name:    "a nav entry without a file fails to run",
			guide:   "[Home](../README.md)\n",
			extra:   map[string]string{"docs/index.yml": testNav + "      - page: Missing\n        path: user/missing.md\n"},
			wantErr: "docs/index.yml references docs/user/missing.md, which cannot be read",
		},
		{
			name:  "no relative links fails to run",
			guide: "Only [external](https://example.com).\n",
			extra: map[string]string{
				"docs/README.md":         "# Intro\n",
				"docs/user/deep/page.md": "# Deep\n",
			},
			wantErr: "no relative links were found on any published page",
		},
		{
			name:    "a missing nav fails to run",
			guide:   "[Home](../README.md)\n",
			extra:   map[string]string{"docs/index.yml": ""},
			wantErr: "no page paths found",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root, base)
			writeTree(t, root, map[string]string{"docs/user/guide.md": tt.guide})
			writeTree(t, root, tt.extra)

			var out bytes.Buffer
			violations, err := run(context.Background(), root, &out)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("run() error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("run() error = %v", err)
			}
			if violations != tt.wantViolations {
				t.Errorf("run() violations = %d, want %d\n%s", violations, tt.wantViolations, out.String())
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(out.String(), want) {
					t.Errorf("run() output missing %q in:\n%s", want, out.String())
				}
			}
			if strings.Contains(out.String(), "nowhere.md") {
				t.Errorf("run() scanned an unpublished page:\n%s", out.String())
			}
		})
	}
}
