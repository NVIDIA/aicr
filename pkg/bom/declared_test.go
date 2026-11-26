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

package bom

import (
	"slices"
	"strings"
	"testing"

	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
)

const declaredValid = `component: c
charts:
  - repository: oci://example.com/charts
    chart: c
    sourceVersion: 1.0.0
    images:
      - image: example.com/c/initc:1.0.0
        source: INIT_IMAGE env of the operator
      - image: example.com/c/extra:1.0.0
        enabledBy: extra.enabled
        source: rendered only when extra.enabled is true
  - repository: https://mirror.example.com
    chart: c
    sourceVersion: 1.0.0
    images:
      - image: mirror.example.com/c/initc:1.0.0
        source: INIT_IMAGE env of the operator
manifests:
  - operatorImage: example.com/c/operator:v1@sha256:45aa299d92c21a1abc0e9731694f01be637ab5c6c9681a687aedf142b0468d45
    images:
      - image: alpine:3.10
        source: compiled-in default
`

func TestParseDeclaredImages(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		wantErr string // substring, empty when valid
	}{
		{"valid", declaredValid, ""},
		{"empty", "", "is empty"},
		{"unknown field", "component: c\nextra: 1\n", "parse declared images"},
		{"no component", "charts: []\n", `name component ""`},
		{"other component", "component: d\ncharts: []\n", `of component "c" name component "d"`},
		{"no entries", "component: c\n", "declares no charts or manifests"},
		{"chart missing version", `component: c
charts:
  - repository: r
    chart: c
    images: [{image: "a:1", source: s}]
`, "needs repository, chart and sourceVersion"},
		{"duplicate chart", `component: c
charts:
  - {repository: r, chart: c, sourceVersion: "1", images: [{image: "a:1", source: s}]}
  - {repository: r, chart: c, sourceVersion: "1", images: [{image: "b:1", source: s}]}
`, "duplicate entry"},
		{"duplicate chart under another path prefix", `component: c
charts:
  - {repository: r, chart: org/c, sourceVersion: "1", images: [{image: "a:1", source: s}]}
  - {repository: r, chart: c, sourceVersion: "1", images: [{image: "b:1", source: s}]}
`, "duplicate entry"},
		{"duplicate chart under a trailing slash", `component: c
charts:
  - {repository: "oci://r", chart: c, sourceVersion: "1", images: [{image: "a:1", source: s}]}
  - {repository: "oci://r/", chart: c, sourceVersion: "1", images: [{image: "b:1", source: s}]}
`, "duplicate entry"},
		{"chart lists no images", `component: c
charts:
  - {repository: r, chart: c, sourceVersion: "1", images: []}
`, "lists no images"},
		{"untagged image", `component: c
charts:
  - {repository: r, chart: c, sourceVersion: "1", images: [{image: "example.com/a", source: s}]}
`, "needs a tag or digest"},
		{"invalid image", `component: c
charts:
  - {repository: r, chart: c, sourceVersion: "1", images: [{image: "Bad:Ref:1", source: s}]}
`, `image "Bad:Ref:1"`},
		{"image without source", `component: c
charts:
  - {repository: r, chart: c, sourceVersion: "1", images: [{image: "a:1"}]}
`, "has no source"},
		{"duplicate image", `component: c
charts:
  - {repository: r, chart: c, sourceVersion: "1", images: [{image: "a:1", source: s}, {image: "a:1", source: t}]}
`, "twice"},
		{"untagged operator image", `component: c
manifests:
  - {operatorImage: "example.com/op", images: [{image: "a:1", source: s}]}
`, "operatorImage"},
		{"duplicate manifest", `component: c
manifests:
  - {operatorImage: "op:1", images: [{image: "a:1", source: s}]}
  - {operatorImage: "op:1", images: [{image: "b:1", source: s}]}
`, "duplicate entry"},
		{"enabledBy on a manifest entry", `component: c
manifests:
  - {operatorImage: "op:1", images: [{image: "a:1", enabledBy: x.enabled, source: s}]}
`, "only chart entries support"},
		{"second YAML document", declaredValid + "---\ncomponent: d\n", "more than one YAML document"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDeclaredImages("c", []byte(tt.data))
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseDeclaredImages() error = %v", err)
				}
				if got.Component != "c" {
					t.Errorf("Component = %q, want c", got.Component)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseDeclaredImages() = %+v, want error containing %q", got, tt.wantErr)
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestDeclaredImagesLookup(t *testing.T) {
	d, err := ParseDeclaredImages("c", []byte(declaredValid))
	if err != nil {
		t.Fatalf("ParseDeclaredImages() error = %v", err)
	}
	tests := []struct {
		name string
		got  int
		want int
	}{
		{"exact chart", entryLen(d.Chart("oci://example.com/charts", "c", "1.0.0")), 2},
		{"chart at other version", entryLen(d.Chart("oci://example.com/charts", "c", "2.0.0")), -1},
		{"chart from other repository", entryLen(d.Chart("oci://other.example.com", "c", "1.0.0")), -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %d, want %d", tt.got, tt.want)
			}
		})
	}
}

func TestDeclaredImagesChartFor(t *testing.T) {
	d, err := ParseDeclaredImages("c", []byte(strings.Replace(declaredValid, "manifests:", `  - repository: https://third.example.com
    chart: other
    sourceVersion: 3.0.0
    images:
      - image: example.com/other:3.0.0
        source: env
manifests:`, 1)))
	if err != nil {
		t.Fatalf("ParseDeclaredImages() error = %v", err)
	}
	single, err := ParseDeclaredImages("c", []byte(`component: c
charts:
  - {repository: oci://example.com/charts, chart: c, sourceVersion: 1.0.0, images: [{image: "a:1", source: s}]}
`))
	if err != nil {
		t.Fatalf("ParseDeclaredImages() error = %v", err)
	}
	split, err := ParseDeclaredImages("c", []byte(`component: c
charts:
  - {repository: https://ngc.example.com, chart: c, sourceVersion: 2.0.0, images: [{image: "ngc:2", source: s}]}
  - {repository: oci://vendor.example.com, chart: c, sourceVersion: 1.0.0, images: [{image: "vendor:1", source: s}]}
`))
	if err != nil {
		t.Fatalf("ParseDeclaredImages() error = %v", err)
	}

	tests := []struct {
		name                       string
		d                          *DeclaredImages
		repository, chart, version string
		wantRepository             string
		wantMsg                    string // substring, empty when an entry is expected
	}{
		{"same repository wins", d, "https://mirror.example.com", "c", "1.0.0", "https://mirror.example.com", ""},
		{"chart path prefix ignored", d, "oci://example.com/charts", "example/c", "1.0.0", "oci://example.com/charts", ""},
		{"relocated repository, single match", single, "oci://registry.internal/charts", "c", "1.0.0",
			"oci://example.com/charts", ""},
		{"relocated repository, ambiguous", d, "oci://registry.internal/charts", "c", "1.0.0", "",
			"matches entries from 2 other repositories"},
		{"version with no entry", d, "oci://example.com/charts", "c", "2.0.0", "",
			`component "c": no entry for chart oci://example.com/charts c@2.0.0`},
		{"chart with no entry", d, "https://third.example.com", "missing", "3.0.0", "", "no entry for chart"},
		{"known repository at another version ignores other publishers", split, "https://ngc.example.com", "c",
			"1.0.0", "", "no entry for chart https://ngc.example.com c@1.0.0"},
		{"trailing slash names the same repository", split, "https://ngc.example.com/", "c", "1.0.0", "",
			"no entry for chart https://ngc.example.com/ c@1.0.0"},
		{"unknown repository falls back to the only match", split, "oci://registry.internal", "c", "1.0.0",
			"oci://vendor.example.com", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.d.ChartFor(tt.repository, tt.chart, tt.version)
			if tt.wantMsg != "" {
				if err == nil {
					t.Fatalf("ChartFor() = %+v, want error containing %q", got, tt.wantMsg)
				}
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
				if !strings.Contains(err.Error(), tt.wantMsg) {
					t.Errorf("error = %q, want it to contain %q", err, tt.wantMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("ChartFor() error = %v", err)
			}
			if got.Repository != tt.wantRepository {
				t.Errorf("ChartFor() repository = %q, want %q", got.Repository, tt.wantRepository)
			}
		})
	}
}

// entryLen returns the image count of c, or -1 for a nil entry.
func entryLen(c *DeclaredChart) int {
	if c == nil {
		return -1
	}
	return len(c.Images)
}

func TestDeclaredImagesManifestsFor(t *testing.T) {
	d, err := ParseDeclaredImages("c", []byte(`component: c
manifests:
  - {operatorImage: "example.com/c/operator:v1", images: [{image: "a:1", source: s}]}
  - {operatorImage: "example.com/c/operator:v2", images: [{image: "b:2", source: s}]}
`))
	if err != nil {
		t.Fatalf("ParseDeclaredImages() error = %v", err)
	}

	tests := []struct {
		name   string
		images []string
		want   []string // operator images of the returned entries, nil for an error
	}{
		{"one operator image", []string{"x:1", "example.com/c/operator:v1"}, []string{"example.com/c/operator:v1"}},
		{"both operator images", []string{"example.com/c/operator:v2", "example.com/c/operator:v1"},
			[]string{"example.com/c/operator:v1", "example.com/c/operator:v2"}},
		{"no declared operator image", []string{"example.com/c/operator:v3"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := d.ManifestsFor(tt.images)
			if tt.want == nil {
				if err == nil {
					t.Fatalf("ManifestsFor() = %+v, want error", got)
				}
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
				if !strings.Contains(err.Error(), "no entry for an operator image") {
					t.Errorf("error = %q, want it to name the missing entry", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ManifestsFor() error = %v", err)
			}
			ops := make([]string, 0, len(got))
			for _, m := range got {
				ops = append(ops, m.OperatorImage)
			}
			if !slices.Equal(ops, tt.want) {
				t.Errorf("ManifestsFor() operator images = %q, want %q", ops, tt.want)
			}
		})
	}
}
