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

package cli

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestIsReleaseVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    bool
	}{
		{"release", "0.14.0", true},
		{"release with v prefix", "v0.14.0", true},
		{"empty", "", false},
		{"dev default", versionDefault, false},
		{"next snapshot", "v0.14.0-next", false},
		{"next snapshot mid-string", "0.14.0-next.3", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isReleaseVersion(tt.version); got != tt.want {
				t.Errorf("isReleaseVersion(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

func TestVersionSkewDetected(t *testing.T) {
	tests := []struct {
		name     string
		versions []string
		want     bool
	}{
		{"all identical release", []string{"0.14.0", "0.14.0", "0.14.0"}, false},
		{"two distinct releases", []string{"0.14.0", "0.13.0", "0.14.0"}, true},
		{"three distinct releases", []string{"0.14.0", "0.13.0", "0.12.0"}, true},
		{"one release rest empty", []string{"0.14.0", "", ""}, false},
		{"one release rest dev", []string{"0.14.0", versionDefault, "v0.14.0-next"}, false},
		{"all dev", []string{versionDefault, versionDefault, versionDefault}, false},
		{"all empty", []string{"", "", ""}, false},
		{"distinct but one is dev", []string{"0.14.0", "0.13.0", versionDefault}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := versionSkewDetected(tt.versions...); got != tt.want {
				t.Errorf("versionSkewDetected(%v) = %v, want %v", tt.versions, got, tt.want)
			}
		})
	}
}

func TestWarnVersionSkew(t *testing.T) {
	tests := []struct {
		name                     string
		binary, recipe, snapshot string
		wantWarn                 bool
		wantContains             []string
	}{
		{
			name:     "skew warns and names all three versions",
			binary:   "0.14.0",
			recipe:   "0.13.0",
			snapshot: "0.13.0",
			wantWarn: true,
			wantContains: []string{
				"version skew", "binaryVersion=0.14.0", "recipeVersion=0.13.0", "snapshotVersion=0.13.0",
			},
		},
		{
			name:     "aligned releases stay silent",
			binary:   "0.14.0",
			recipe:   "0.14.0",
			snapshot: "0.14.0",
			wantWarn: false,
		},
		{
			name:         "empty snapshot version renders as unknown",
			binary:       "0.14.0",
			recipe:       "0.13.0",
			snapshot:     "",
			wantWarn:     true,
			wantContains: []string{"snapshotVersion=unknown"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			original := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
			defer slog.SetDefault(original)

			warnVersionSkew(tt.binary, tt.recipe, tt.snapshot)

			out := buf.String()
			gotWarn := strings.Contains(out, "level=WARN")
			if gotWarn != tt.wantWarn {
				t.Fatalf("warn emitted = %v, want %v (output: %q)", gotWarn, tt.wantWarn, out)
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q; got %q", want, out)
				}
			}
		})
	}
}
