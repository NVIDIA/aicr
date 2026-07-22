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

package oci

import (
	stderrors "errors"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestHelmChartVersionFromTag(t *testing.T) {
	tests := []struct {
		name    string
		tag     string
		want    string
		wantErr bool
	}{
		{name: "release", tag: "1.2.3", want: "1.2.3"},
		{name: "prerelease", tag: "1.2.3-rc.1", want: "1.2.3-rc.1"},
		{name: "build metadata", tag: "1.2.3_build.5", want: "1.2.3+build.5"},
		{name: "empty", tag: "", wantErr: true},
		{name: "latest", tag: "latest", wantErr: true},
		{name: "leading v", tag: "v1.2.3", wantErr: true},
		{name: "invalid distribution syntax", tag: "1.2.3+build.5", wantErr: true},
		{name: "invalid build metadata", tag: "1.2.3_build..5", wantErr: true},
		{name: "multiple underscores", tag: "1.2.3_build_5", wantErr: true},
		{name: "ambiguous underscore", tag: "1.2_3", wantErr: true},
		{name: "129-byte tag", tag: "1.2.3-" + strings.Repeat("a", 123), wantErr: true},
		{name: "non-round-trip leading zero", tag: "01.2.3", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HelmChartVersionFromTag(tt.tag)
			if tt.wantErr {
				assertInvalidHelmVersionError(t, err)
				return
			}
			if err != nil {
				t.Fatalf("HelmChartVersionFromTag(%q) error = %v", tt.tag, err)
			}
			if got != tt.want {
				t.Errorf("HelmChartVersionFromTag(%q) = %q, want %q", tt.tag, got, tt.want)
			}
		})
	}

	// Distribution accepts this exact boundary. Helm SemVer validation is a
	// separate gate, so the error must not be an accidental length rejection.
	boundary := "1.2.3-" + strings.Repeat("a", 122)
	if len(boundary) != 128 {
		t.Fatalf("boundary fixture length = %d, want 128", len(boundary))
	}
	got, err := HelmChartVersionFromTag(boundary)
	if err != nil {
		t.Fatalf("HelmChartVersionFromTag(128-byte tag) error = %v", err)
	}
	if got != boundary {
		t.Errorf("HelmChartVersionFromTag(128-byte tag) = %q, want identity", got)
	}
}

func TestHelmTagFromChartVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
		wantErr bool
	}{
		{name: "release", version: "1.2.3", want: "1.2.3"},
		{name: "prerelease", version: "1.2.3-rc.1", want: "1.2.3-rc.1"},
		{name: "build metadata", version: "1.2.3+build.5", want: "1.2.3_build.5"},
		{name: "empty", version: "", wantErr: true},
		{name: "latest", version: "latest", wantErr: true},
		{name: "leading v", version: "v1.2.3", wantErr: true},
		{name: "underscore", version: "1.2.3_build.5", wantErr: true},
		{name: "invalid metadata", version: "1.2.3+build..5", wantErr: true},
		{name: "multiple metadata separators", version: "1.2.3+build+5", wantErr: true},
		{name: "encoded tag too long", version: "1.2.3+" + strings.Repeat("a", 123), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := HelmTagFromChartVersion(tt.version)
			if tt.wantErr {
				assertInvalidHelmVersionError(t, err)
				return
			}
			if err != nil {
				t.Fatalf("HelmTagFromChartVersion(%q) error = %v", tt.version, err)
			}
			if got != tt.want {
				t.Errorf("HelmTagFromChartVersion(%q) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}

func assertInvalidHelmVersionError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
	}
}
