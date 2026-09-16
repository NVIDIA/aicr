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

package header_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/header"
)

// captureWarnings redirects the default logger for one test and returns what it
// wrote. Not parallel-safe: slog.SetDefault is process-wide.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	// Fresh recorder per test: dedup is the behavior under test, and the
	// process-wide one makes `go test -count=2` observe every subject already
	// seen and emit nothing.
	header.ResetAPIVersionRecorderForTest()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestWarnDeprecatedAPIVersion covers the ADR-022 §3 promise in RELEASE.md that
// a loader "accepts the deprecated shape and warns, naming the file and the
// release that stops reading it". Naming the file is the part most easily lost,
// because it is what makes the warning actionable in a catalog of 120 overlays.
func TestWarnDeprecatedAPIVersion(t *testing.T) {
	tests := []struct {
		name       string
		apiVersion string
		target     string
		wantWarn   bool
	}{
		{"stable alpha warns", header.GroupVersion, header.GroupVersionV1, true},
		{"profile alpha warns", header.RecipeResultGroupVersion, header.GroupVersionV1Beta2, true},
		{"absent header warns", "", header.GroupVersionV1, true},
		{"stable target is silent", header.GroupVersionV1, header.GroupVersionV1, false},
		{"authoring target is silent", header.GroupVersionV1Beta1, header.GroupVersionV1Beta1, false},
		{"profile target is silent", header.GroupVersionV1Beta2, header.GroupVersionV1Beta2, false},
		{"unknown value is silent", "example.com/v1", header.GroupVersionV1, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf := captureWarnings(t)
			// Subject embeds the path and is the dedup key, so a path unique to
			// this subtest keeps the process-wide recorder from suppressing it
			// because an earlier subtest warned about the same apiVersion.
			path := "testdata/" + strings.ReplaceAll(tt.name, " ", "-") + ".yaml"

			header.WarnDeprecatedAPIVersion(path, tt.apiVersion, tt.target)

			got := buf.String()
			if !tt.wantWarn {
				if got != "" {
					t.Fatalf("WarnDeprecatedAPIVersion(%q) warned when it should not: %s", tt.apiVersion, got)
				}
				return
			}
			if got == "" {
				t.Fatalf("WarnDeprecatedAPIVersion(%q) emitted no warning", tt.apiVersion)
			}
			if !strings.Contains(got, path) {
				t.Errorf("warning does not name the file %q: %s", path, got)
			}
			if !strings.Contains(got, header.AlphaRemovedIn) {
				t.Errorf("warning does not name the removal release %q: %s", header.AlphaRemovedIn, got)
			}
			if !strings.Contains(got, tt.target) {
				t.Errorf("warning does not name the replacement %q: %s", tt.target, got)
			}
		})
	}
}

// TestWarnDeprecatedAPIVersionDedupsPerFile pins the granularity decision. A
// catalog scan re-reading one file must not restate the same warning, while a
// second offending file must still be named — otherwise a user fixes the one
// file they were told about and the next run names another.
func TestWarnDeprecatedAPIVersionDedupsPerFile(t *testing.T) {
	buf := captureWarnings(t)

	header.WarnDeprecatedAPIVersion("dedup/first.yaml", header.GroupVersion, header.GroupVersionV1)
	header.WarnDeprecatedAPIVersion("dedup/first.yaml", header.GroupVersion, header.GroupVersionV1)
	header.WarnDeprecatedAPIVersion("dedup/second.yaml", header.GroupVersion, header.GroupVersionV1)

	// Count records, not substrings: each record names the path twice, once in
	// the rendered message and once in the subject attribute.
	got := buf.String()
	records := func(path string) int {
		n := 0
		for line := range strings.SplitSeq(strings.TrimSpace(got), "\n") {
			if strings.Contains(line, path) {
				n++
			}
		}
		return n
	}
	if n := records("dedup/first.yaml"); n != 1 {
		t.Errorf("first file warned %d times, want 1: %s", n, got)
	}
	if n := records("dedup/second.yaml"); n != 1 {
		t.Errorf("second file warned %d times, want 1: %s", n, got)
	}
}
