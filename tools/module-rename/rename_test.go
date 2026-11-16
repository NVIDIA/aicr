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
	"reflect"
	"strings"
	"testing"
)

const (
	testFrom = "github.com/acme/widget"
	testTo   = "widget.dev"
)

var testGoDirs = []string{"cmd", "pkg"}

func TestRewrite(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		want      string
		wantCount int
	}{
		{"import spec", `"github.com/acme/widget/pkg/x"`, `"widget.dev/pkg/x"`, 1},
		{"bare quoted module path", `"github.com/acme/widget"`, `"widget.dev"`, 1},
		{"module directive", "module github.com/acme/widget", "module widget.dev", 1},
		{"module directive with trailing space", "module github.com/acme/widget  \n", "module widget.dev  \n", 1},
		{"linker flag", `-X 'github.com/acme/widget/pkg/cli.version=1'`, `-X 'widget.dev/pkg/cli.version=1'`, 1},
		{"key value pair", "mode=export package=github.com/acme/widget/pkg/x", "mode=export package=widget.dev/pkg/x", 1},
		{"two on one line", `"github.com/acme/widget/pkg/a" "github.com/acme/widget/cmd/b"`, `"widget.dev/pkg/a" "widget.dev/cmd/b"`, 2},
		{"url with scheme", "https://github.com/acme/widget/pkg/x", "https://github.com/acme/widget/pkg/x", 0},
		{"sigstore identity", "github.com/acme/widget/.github/workflows/a.yaml", "github.com/acme/widget/.github/workflows/a.yaml", 0},
		{"prefix string", `"github.com/acme/widget/"`, `"github.com/acme/widget/"`, 0},
		{"regexp pattern", "`github.com/acme/widget/.*`", "`github.com/acme/widget/.*`", 0},
		{"query parameter", "?to=github.com/acme/widget/", "?to=github.com/acme/widget/", 0},
		{"git clone url", "github.com/acme/widget.git", "github.com/acme/widget.git", 0},
		{"longer module name", "github.com/acme/widgetry/pkg/x", "github.com/acme/widgetry/pkg/x", 0},
		{"directory without Go code", "github.com/acme/widget/example", "github.com/acme/widget/example", 0},
		{"directory name prefix", "github.com/acme/widget/pkgs/x", "github.com/acme/widget/pkgs/x", 0},
		{"nested under another path", "example.com/github.com/acme/widget/pkg/x", "example.com/github.com/acme/widget/pkg/x", 0},
		{"module directive with suffix", "module github.com/acme/widget/v2", "module github.com/acme/widget/v2", 0},
		{"crlf preserved", "a\r\nimport \"github.com/acme/widget/pkg/x\"\r\n", "a\r\nimport \"widget.dev/pkg/x\"\r\n", 1},
		{"no final newline preserved", "x \"github.com/acme/widget/pkg/x\"", "x \"widget.dev/pkg/x\"", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, count := rewrite([]byte(tt.in), testFrom, testTo, testGoDirs)
			if string(got) != tt.want || count != tt.wantCount {
				t.Fatalf("rewrite(%q) = %q, %d; want %q, %d", tt.in, got, count, tt.want, tt.wantCount)
			}
			again, againCount := rewrite(got, testFrom, testTo, testGoDirs)
			if string(again) != string(got) || againCount != 0 {
				t.Fatalf("second rewrite changed %q to %q (%d replacements)", got, again, againCount)
			}
		})
	}
}

func TestLeftovers(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []int
	}{
		{"prefix string", `"github.com/acme/widget/"`, []int{1}},
		{"coverage root flag", `-root=github.com/acme/widget \`, []int{1}},
		{"repository mention", "built by github.com/acme/widget CI", []int{1}},
		{"keep marker", "- pkg: github.com/acme/widget # module-rename:keep", nil},
		{"web route", "https://github.com/acme/widget/blob/main/x", nil},
		{"web route without scheme", "see github.com/acme/widget/tree/main", nil},
		{"sigstore identity", "github.com/acme/widget/.github/workflows/on-tag.yaml@refs/tags/v1", nil},
		{"git clone url", "git clone github.com/acme/widget.git", nil},
		{"longer module name", "github.com/acme/widgetry", nil},
		{"line number", "one\ntwo\n\"github.com/acme/widget/\"\n", []int{3}},
		{"crlf line number", "one\r\n\"github.com/acme/widget/\"\r\n", []int{2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := leftovers([]byte(tt.in), testFrom); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("leftovers(%q) = %v; want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestGoTopDirs(t *testing.T) {
	got := goTopDirs([]string{"pkg/a/a.go", "cmd/x/main.go", "docs/a.md", "go.mod", "tools/t/t.go", "root.go"})
	want := []string{"cmd", "pkg", "tools"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("goTopDirs = %v; want %v", got, want)
	}
}

func TestParseSkip(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{"entries comments and blanks", "# header\n\ndocs/user/  # held\ninstall\n", []string{"docs/user/", "install"}, false},
		{"two fields", "docs/user/ install\n", nil, true},
		{"empty", "", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseSkip(strings.NewReader(tt.in))
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseSkip error = %v; wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseSkip = %v; want %v", got, tt.want)
			}
		})
	}
}

func TestSkipped(t *testing.T) {
	skip := []string{"docs/user/", "install"}
	tests := []struct {
		path string
		want bool
	}{
		{"docs/user/installation.md", true},
		{"install", true},
		{"installer.sh", false},
		{"docs/userguide.md", false},
		{"pkg/a/a.go", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := skipped(tt.path, skip); got != tt.want {
				t.Fatalf("skipped(%q) = %v; want %v", tt.path, got, tt.want)
			}
		})
	}
}
