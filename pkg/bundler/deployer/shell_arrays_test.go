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

package deployer

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// alwaysNonEmptyArrays lists unguarded array expansions that are safe because
// the array can never be empty, keyed by template path and the exact trimmed
// source line.
var alwaysNonEmptyArrays = map[string]string{
	`helm/templates/deploy.sh.tmpl:for key in "${keys[@]}"; do`: "keys starts with two taint keys and is only ever replaced by a non-empty list",
}

var (
	arrayExpansion   = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\[[@*]\]\}`)
	guardedExpansion = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\[@\]\+"\$\{([A-Za-z_][A-Za-z0-9_]*)\[@\]\}"\}`)
)

// unguardedArrayExpansions returns the expansions in line that bash before
// 4.4 reports as an unbound variable under set -u when the array is empty:
// ${name[@]} or ${name[*]}, quoted or not, outside a guarded
// ${name[@]+"${name[@]}"} that names one array twice.
func unguardedArrayExpansions(line string) []string {
	rest := guardedExpansion.ReplaceAllStringFunc(line, func(m string) string {
		names := guardedExpansion.FindStringSubmatch(m)
		if names[1] == names[2] {
			return ""
		}
		return m
	})
	return arrayExpansion.FindAllString(rest, -1)
}

func TestUnguardedArrayExpansions(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
	}{
		{name: "quoted", line: `helm "${A[@]}" x`, want: []string{`${A[@]}`}},
		{name: "unquoted", line: `helm ${A[@]} x`, want: []string{`${A[@]}`}},
		{name: "star", line: `echo "${A[*]}"`, want: []string{`${A[*]}`}},
		{name: "inside a word", line: `helm "--set=${A[@]}"`, want: []string{`${A[@]}`}},
		{name: "guarded", line: `helm ${A[@]+"${A[@]}"} x`},
		{name: "guard names another array", line: `helm ${B[@]+"${A[@]}"} x`, want: []string{`${A[@]}`}},
		{name: "length", line: `n=${#A[@]}`},
		{name: "indices", line: `for i in "${!A[@]}"; do`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := unguardedArrayExpansions(tt.line); !slices.Equal(got, tt.want) {
				t.Errorf("unguardedArrayExpansions(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

// The bundle scripts run under `set -u`, and bash before 4.4, which macOS
// still ships as /bin/bash, treats an expansion of an empty array as an
// unbound variable. Every array expansion in the deployers' shell sources,
// templates and the connection prologue spliced into them, therefore takes
// the guarded form unless the array can never be empty. CI runs a newer bash,
// so this is the check that keeps such a regression out.
func TestShellTemplatesGuardArrayExpansions(t *testing.T) {
	var checked int
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".sh.tmpl") && !strings.HasSuffix(path, ".sh") {
			return nil
		}
		checked++
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			if _, ok := alwaysNonEmptyArrays[filepath.ToSlash(path)+":"+strings.TrimSpace(line)]; ok {
				continue
			}
			for _, m := range unguardedArrayExpansions(line) {
				t.Errorf("%s:%d: %s is unbound under set -u on bash before 4.4 when the array is empty; use the guarded ${name[@]+\"${name[@]}\"} form",
					path, i+1, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk shell sources: %v", err)
	}
	if checked == 0 {
		t.Fatal("found no shell sources to check")
	}
}
