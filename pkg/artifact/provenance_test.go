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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

func TestValidateProvenance(t *testing.T) {
	inventory, err := ParseInventory([]byte(inventoryFixture()))
	if err != nil {
		t.Fatal(err)
	}
	base := &Provenance{InventoryDigest: "sha256:" + strings.Repeat("a", 64), BindingsDigest: "sha256:" + strings.Repeat("b", 64), BaseCatalogDigest: "sha256:" + strings.Repeat("c", 64), AllowCatalogPins: []string{}, Verification: "metadata-only", Applied: []Applied{{Artifact: inventory.Artifacts[0], Chart: &Chart{Component: "controller"}}}, Ignored: []IgnoredIdentity{}}
	if err := ValidateProvenance(base); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name   string
		mutate func(*Provenance)
	}{
		{"digest", func(p *Provenance) { p.InventoryDigest = "invalid" }},
		{"verification", func(p *Provenance) { p.Verification = "verified" }},
		{"missing array", func(p *Provenance) { p.Ignored = nil }},
		{"duplicate exceptions", func(p *Provenance) { p.AllowCatalogPins = []string{"controller", "controller"} }},
		{"unordered exceptions", func(p *Provenance) { p.AllowCatalogPins = []string{"z", "a"} }},
		{"duplicate artifact", func(p *Provenance) { p.Applied = append(p.Applied, p.Applied[0]) }},
		{"missing binding", func(p *Provenance) { p.Applied[0].Chart = nil }},
		{"wrong type", func(p *Provenance) { p.Applied[0].Artifact.Type = "image" }},
		{"missing archive", func(p *Provenance) { p.Applied[0].Artifact.ArchiveDigest = "" }},
		{"ignored overlap", func(p *Provenance) {
			p.Ignored = append(p.Ignored, IgnoredIdentity{Artifact: p.Applied[0].Artifact, Reason: "unused"})
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := base.Clone()
			tt.mutate(p)
			errorstest.WantReportedCode(t, ValidateProvenance(p), errors.ErrCodeInvalidRequest)
		})
	}
	if err := ValidateProvenance(base); err != nil {
		t.Fatal("clone mutation reached original:", err)
	}
}

func TestPlatformEqualityAndClone(t *testing.T) {
	v8, v9 := "v8", "v9"
	original := &Platform{OS: "linux", Architecture: "arm64", Variant: &v8}
	for _, tt := range []struct {
		name  string
		other *Platform
		equal bool
	}{
		{"same variant", &Platform{OS: "linux", Architecture: "arm64", Variant: &v8}, true},
		{"other variant", &Platform{OS: "linux", Architecture: "arm64", Variant: &v9}, false},
		{"absent variant", &Platform{OS: "linux", Architecture: "arm64"}, false},
		{"absent platform", nil, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if original.Equal(tt.other) != tt.equal {
				t.Fatal("wrong platform match")
			}
		})
	}
	copy := clonePlatform(original)
	*copy.Variant = "v9"
	if *original.Variant != "v8" {
		t.Fatal("variant alias survived clone")
	}
	empty := ""
	errorstest.WantReportedCode(t, validatePlatform(&Platform{OS: "linux", Architecture: "arm64", Variant: &empty}), errors.ErrCodeInvalidRequest)
}
