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
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestRenovateRegexMatchesEveryAnnotation compiles the matchString committed in
// .github/renovate.json5 and runs it against recipes/registry.yaml. Renovate's
// regex dialect and Go's RE2 agree on the subset used here (named groups, no
// lookaround), and Go 1.22+ accepts the (?<name>...) spelling, so this catches a
// config/data mismatch without a Renovate run.
func TestRenovateRegexMatchesEveryAnnotation(t *testing.T) {
	cfg, err := os.ReadFile(filepath.Join(testRepoRoot(t), ".github", "renovate.json5"))
	if err != nil {
		t.Fatalf("read renovate.json5: %v", err)
	}

	// The .settings.yaml custom manager (above the registry.yaml one) has three
	// matchStrings containing the same "# renovate: datasource=" substring, so
	// the selector also requires "defaultVersion", which only the
	// recipes/registry.yaml manager's matchString contains.
	var quoted string
	for _, line := range strings.Split(string(cfg), "\n") {
		if !strings.Contains(line, `# renovate: datasource=`) || !strings.Contains(line, `defaultVersion`) {
			continue
		}
		first, last := strings.Index(line, `"`), strings.LastIndex(line, `"`)
		if first < 0 || last <= first {
			t.Fatalf("matchString line is not a double-quoted literal: %s", line)
		}
		quoted = line[first : last+1]
		break
	}
	if quoted == "" {
		t.Fatal("no registry-chart matchString found in .github/renovate.json5")
	}

	pattern, err := strconv.Unquote(quoted)
	if err != nil {
		t.Fatalf("unquote matchString %s: %v", quoted, err)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("compile matchString %q: %v", pattern, err)
	}

	data, err := os.ReadFile(filepath.Join(testRepoRoot(t), "recipes", "registry.yaml"))
	if err != nil {
		t.Fatalf("read registry.yaml: %v", err)
	}
	matches := re.FindAllStringSubmatch(string(data), -1)
	if len(matches) != 35 {
		t.Fatalf("matchString matched %d annotations, want 35", len(matches))
	}

	idx := re.SubexpIndex("currentValue")
	if idx < 0 {
		t.Fatal("matchString has no currentValue capture group")
	}
	pins, err := LoadPins(testRepoRoot(t))
	if err != nil {
		t.Fatalf("LoadPins: %v", err)
	}
	want := map[string]bool{}
	for _, p := range pins {
		if p.Annotated {
			want[p.DepName+"@"+p.Version] = true
		}
	}
	depIdx := re.SubexpIndex("depName")
	for _, m := range matches {
		key := m[depIdx] + "@" + m[idx]
		if !want[key] {
			t.Errorf("regex captured %q, which no annotated pin declares", key)
		}
	}
}
