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

package artifact

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

func inventoryFixture() string {
	return `apiVersion: aicr.run/v1beta1
kind: ArtifactInventory
artifacts:
- name: controller
  type: chart
  reference: oci://registry.example.test:5000/charts/controller
  version: v1.2.3-rc.1_build.1
  manifestDigest: sha256:` + strings.Repeat("a", 64) + `
  archiveDigest: sha256:` + strings.Repeat("b", 64) + "\n"
}

func TestParseInventory(t *testing.T) {
	base := inventoryFixture()
	tests := []struct {
		name, input string
		valid       bool
	}{
		{"literal chart tag", base, true},
		{"empty inventory", "apiVersion: aicr.run/v1beta1\nkind: ArtifactInventory\nartifacts: []", true},
		{"unknown version", strings.Replace(base, "v1beta1", "v1", 1), false},
		{"wrong kind", strings.Replace(base, "ArtifactInventory", "ArtifactBindings", 1), false},
		{"missing array", "apiVersion: aicr.run/v1beta1\nkind: ArtifactInventory", false},
		{"unknown field", base + "secret: forbidden\n", false},
		{"duplicate key", base + "kind: ArtifactInventory\n", false},
		{"non string key", base + "1: bad\n", false},
		{"null", base + "metadata: null\n", false},
		{"anchor", strings.Replace(base, "artifacts:", "artifacts: &items", 1), false},
		{"explicit tag", strings.Replace(base, "type: chart", "type: !!str chart", 1), false},
		{"multiple documents", base + "---\n{}\n", false},
		{"trailing empty document", base + "---\n", false},
		{"raw build tag", strings.Replace(base, "_build.1", "+build.1", 1), false},
		{"multiple underscores", strings.Replace(base, "_build.1", "_build_1", 1), false},
		{"range", strings.Replace(base, "v1.2.3-rc.1_build.1", ">=1.2.3", 1), false},
		{"short version", strings.Replace(base, "v1.2.3-rc.1_build.1", "v1.2", 1), false},
		{"numeric label", strings.Replace(base, "v1.2.3-rc.1_build.1", "123", 1), false},
		{"credentials", strings.Replace(base, "registry.example.test", "user:password@registry.example.test", 1), false},
		{"image tag in reference", strings.Replace(base, "charts/controller", "charts/controller:tag", 1), false},
		{"uppercase scheme", strings.Replace(base, "oci://", "OCI://", 1), false},
		{"unqualified host", strings.Replace(base, "registry.example.test:5000", "registry", 1), false},
		{"localhost", strings.Replace(base, "registry.example.test:5000", "localhost", 1), true},
		{"host with port", strings.Replace(base, "registry.example.test:5000", "registry:5000", 1), true},
		{"wrong field case", strings.Replace(base, "manifestDigest:", "manifestdigest:", 1), false},
		{"duplicate field case", base + "  manifestdigest: sha256:" + strings.Repeat("c", 64) + "\n", false},
		{"uppercase reference", strings.Replace(base, "charts/controller", "charts/Controller", 1), false},
		{"uppercase digest", strings.Replace(base, strings.Repeat("a", 64), strings.Repeat("A", 64), 1), false},
		{"oversize", strings.Repeat(" ", defaults.ArtifactDocumentBytes+1), false},
		{"invalid UTF8", base + string([]byte{255}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseInventory([]byte(tt.input))
			if tt.valid {
				if err != nil {
					t.Fatal(err)
				}
				if got.Kind != InventoryKind {
					t.Fatal(got)
				}
			} else {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			}
		})
	}
	parsed, err := ParseInventory([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseInventory(encoded); err != nil {
		t.Fatal(err)
	}
}

func TestBindingsLayeringAndAccounting(t *testing.T) {
	base, err := ParseBindings([]byte(`apiVersion: aicr.run/v1beta1
kind: ArtifactBindings
charts:
 controller: {component: controller}
 stale: {component: missing-component}
images: {}
`))
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := ParseBindings([]byte(`apiVersion: aicr.run/v1beta1
kind: ArtifactBindings
charts: {}
images: {}
ignoredArtifacts:
 controller: {reason: Not deployed}
`))
	if err != nil {
		t.Fatal(err)
	}
	merged, err := Merge(base, replacement)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Charts) != 1 || merged.Charts["stale"].Component != "missing-component" {
		t.Fatal(merged)
	}
	if _, exists := base.Charts["controller"]; !exists {
		t.Fatal("input mutated")
	}
	inventory, err := ParseInventory([]byte(inventoryFixture()))
	if err != nil {
		t.Fatal(err)
	}
	if err := Account(inventory, merged); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		bindings *Bindings
		valid    bool
	}{
		{"empty", EmptyBindings(), false},
		{"chart", base, true},
		{"ignored", merged, true},
		{"wrong type", &Bindings{Images: map[string]Image{"controller": {}}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Account(inventory, tt.bindings)
			if tt.valid {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			}
		})
	}
}

func TestParseBindings(t *testing.T) {
	prefix := "apiVersion: aicr.run/v1beta1\nkind: ArtifactBindings\n"
	tests := []struct {
		name, body string
		valid      bool
	}{
		{"empty", "charts: {}\nimages: {}", true},
		{"missing map", "charts: {}", false},
		{"unknown field", "charts: {}\nimages: {}\nextra: {}", false},
		{"wrong field case", "charts: {}\nimages: {}\nCharts: {}", false},
		{"disjoint", "charts: {a: {component: a}}\nimages: {}\nignoredArtifacts: {a: {reason: ignored}}", false},
		{"duplicate chart", "charts: {a: {component: a}, b: {component: a}}\nimages: {}", false},
		{"empty reason", "charts: {}\nimages: {}\nignoredArtifacts: {a: {reason: ''}}", false},
		{"inactive valid target", "charts: {}\nimages: {a: {targets: [{component: absent, valuesFile: absent.yaml, write: {reference: /image}}]}}", true},
		{"traversal", "charts: {}\nimages: {a: {targets: [{component: a, valuesFile: ../a.yaml, write: {reference: /image}}]}}", false},
		{"mixed writes", "charts: {}\nimages: {a: {targets: [{component: a, valuesFile: a.yaml, write: {reference: /image, digest: /digest}}]}}", false},
		{"invalid pointer", "charts: {}\nimages: {a: {targets: [{component: a, valuesFile: a.yaml, write: {reference: /bad~2}}]}}", false},
		{"empty targets", "charts: {}\nimages: {a: {targets: []}}", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseBindings([]byte(prefix + tt.body))
			if tt.valid {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			}
		})
	}
}

func TestArtifactDocumentBounds(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"depth", []byte("apiVersion: aicr.run/v1beta1\nkind: ArtifactInventory\nartifacts: " + strings.Repeat("[", defaults.ArtifactDocumentDepth+1) + strings.Repeat("]", defaults.ArtifactDocumentDepth+1))},
		{"duplicate JSON", []byte(`{"apiVersion":"aicr.run/v1beta1","kind":"ArtifactInventory","kind":"ArtifactInventory","artifacts":[]}`)},
		{"null optional platform", []byte(strings.Replace(inventoryFixture(), "  type: chart", "  type: image\n  platform: null", 1))},
		{"empty optional label", []byte(inventoryFixture() + "metadata: {name: ''}\n")},
		{"long chart tag", []byte(strings.Replace(inventoryFixture(), "v1.2.3-rc.1_build.1", "v1.2.3_"+strings.Repeat("a", defaults.ArtifactChartTagBytes), 1))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseInventory(tt.data)
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}
}
