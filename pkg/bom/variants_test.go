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
	"bytes"
	"strings"
	"testing"

	cdx "github.com/CycloneDX/cyclonedx-go"
)

// encodeBOM serializes a CycloneDX BOM to canonical JSON for byte-stable
// regression comparisons.
func encodeBOM(t *testing.T, doc *cdx.BOM) string {
	t.Helper()
	var buf bytes.Buffer
	enc := cdx.NewBOMEncoder(&buf, cdx.BOMFileFormatJSON)
	enc.SetPretty(true)
	if err := enc.Encode(doc); err != nil {
		t.Fatalf("encode BOM: %v", err)
	}
	return buf.String()
}

func sampleVariants() []VariantResult {
	return []VariantResult{
		{
			Name:       "kube-prometheus-stack",
			Version:    "83.7.0",
			Sources:    []string{"platform-x", "aks"}, // deliberately unsorted input
			Repository: "https://prometheus-community.github.io/helm-charts",
			Chart:      "prometheus-community/kube-prometheus-stack",
			Namespace:  "monitoring",
			Images:     []string{"quay.io/prometheus/prometheus:v2.83.0"},
		},
	}
}

// TestBuildBOMWithVariants pins the variant contract: default component
// bom-refs are identical to the legacy builder's, variant entries get
// version-qualified refs with sorted aicr:variant:sources, variant images
// join the dependency graph, and the caller-supplied fidelity note lands in
// the CycloneDX metadata.
func TestBuildBOMWithVariants(t *testing.T) {
	results := []ComponentResult{{
		Name: "kube-prometheus-stack", Type: TypeHelm, Version: "84.4.0",
		Images: []string{"quay.io/prometheus/prometheus:v3.0.0"},
	}}
	doc, err := BuildBOMWithVariants(Metadata{
		Name: "aicr", Version: "v1", RenderFidelity: RenderFidelityCatalogParity,
	}, results, sampleVariants())
	if err != nil {
		t.Fatalf("BuildBOMWithVariants: %v", err)
	}

	var defaultRef, variantRef *cdx.Component
	for i := range *doc.Components {
		c := &(*doc.Components)[i]
		switch c.BOMRef {
		case "aicr/kube-prometheus-stack":
			defaultRef = c
		case "aicr/kube-prometheus-stack@83.7.0":
			variantRef = c
		}
	}
	if defaultRef == nil {
		t.Fatal("default component bom-ref changed or missing — variants must not alter default identities")
	}
	if variantRef == nil {
		t.Fatal("variant entry aicr/kube-prometheus-stack@83.7.0 missing")
	}
	if variantRef.Version != "83.7.0" {
		t.Errorf("variant version = %q, want 83.7.0", variantRef.Version)
	}
	var sources string
	for _, p := range *variantRef.Properties {
		if p.Name == "aicr:variant:sources" {
			sources = p.Value
		}
	}
	if sources != "aks,platform-x" {
		t.Errorf("aicr:variant:sources = %q, want sorted \"aks,platform-x\"", sources)
	}

	var rootDeps, variantDeps []string
	for _, d := range *doc.Dependencies {
		if d.Dependencies == nil {
			continue
		}
		switch d.Ref {
		case "aicr":
			rootDeps = *d.Dependencies
		case "aicr/kube-prometheus-stack@83.7.0":
			variantDeps = *d.Dependencies
		}
	}
	found := map[string]bool{}
	for _, r := range rootDeps {
		found[r] = true
	}
	if !found["aicr/kube-prometheus-stack"] || !found["aicr/kube-prometheus-stack@83.7.0"] {
		t.Errorf("root dependencies = %v, want both default and variant refs", rootDeps)
	}
	if len(variantDeps) != 1 || variantDeps[0] != "img:quay.io/prometheus/prometheus:v2.83.0" {
		t.Errorf("variant deps = %v, want the variant's rendered image", variantDeps)
	}

	var fidelity string
	if doc.Metadata.Properties != nil {
		for _, p := range *doc.Metadata.Properties {
			if p.Name == "aicr:render:fidelity" {
				fidelity = p.Value
			}
		}
	}
	if fidelity != RenderFidelityCatalogParity {
		t.Errorf("aicr:render:fidelity = %q, want the caller-supplied catalog-parity note", fidelity)
	}
}

// TestBuildBOMWithVariants_DuplicateRefsFailClosed pins the checked mode's
// fail-closed contract, which the legacy BuildBOM deliberately does not have.
func TestBuildBOMWithVariants_DuplicateRefsFailClosed(t *testing.T) {
	tests := []struct {
		name     string
		results  []ComponentResult
		variants []VariantResult
	}{
		{
			"duplicate components",
			[]ComponentResult{{Name: "a", Type: TypeHelm}, {Name: "a", Type: TypeHelm}},
			nil,
		},
		{
			"duplicate variants",
			nil,
			[]VariantResult{
				{Name: "a", Version: "1.0.0"},
				{Name: "a", Version: "1.0.0"},
			},
		},
		{
			// meta.Name "img:nvcr.io" + component "gpu:v1" produces the
			// application ref "img:nvcr.io/gpu:v1", colliding with the
			// container entry for image "nvcr.io/gpu:v1".
			"image ref colliding with a component identity",
			[]ComponentResult{
				{Name: "gpu:v1", Type: TypeHelm, Version: "1.0.0"},
				{Name: "other", Type: TypeHelm, Version: "1.0.0", Images: []string{"nvcr.io/gpu:v1"}},
			},
			nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := Metadata{Name: "aicr"}
			if tt.name == "image ref colliding with a component identity" {
				meta.Name = "img:nvcr.io"
			}
			_, err := BuildBOMWithVariants(meta, tt.results, tt.variants)
			if err == nil {
				t.Fatal("BuildBOMWithVariants accepted duplicate bom-refs")
			}
			if !strings.Contains(err.Error(), "duplicate bom-ref") {
				t.Errorf("error = %v, want a duplicate bom-ref rejection", err)
			}
		})
	}

	// Cross-component image reuse stays a dedup, not a collision.
	doc, err := BuildBOMWithVariants(Metadata{Name: "aicr"}, []ComponentResult{
		{Name: "a", Type: TypeHelm, Version: "1.0.0", Images: []string{"nvcr.io/shared:v1"}},
		{Name: "b", Type: TypeHelm, Version: "1.0.0", Images: []string{"nvcr.io/shared:v1"}},
	}, nil)
	if err != nil {
		t.Fatalf("BuildBOMWithVariants rejected a legitimate cross-component image dedup: %v", err)
	}
	count := 0
	for _, c := range *doc.Components {
		if c.BOMRef == "img:nvcr.io/shared:v1" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("shared image entries = %d, want exactly 1", count)
	}
}

// TestWriteMarkdownWithVariants pins the Markdown projection: a separate
// Version-variants table with distinct headers, per-variant image sections,
// the caller-supplied fidelity note in a code span, and full omission when
// no variants exist.
func TestWriteMarkdownWithVariants(t *testing.T) {
	var buf bytes.Buffer
	err := WriteMarkdownWithVariants(&buf, Metadata{
		Name: "aicr", Deterministic: true, RenderFidelity: RenderFidelityCatalogParity,
	}, sampleResults(), sampleVariants())
	if err != nil {
		t.Fatalf("WriteMarkdownWithVariants: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"## Version variants",
		"| Component | Variant Version | Declared By | Images |",
		"| kube-prometheus-stack | 83.7.0 | aks, platform-x | 1 |",
		"### kube-prometheus-stack@83.7.0 (variant)",
		"- `quay.io/prometheus/prometheus:v2.83.0`",
		"_Rendering fidelity:_",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown missing %q:\n%s", want, out)
		}
	}

	var noVar bytes.Buffer
	if err := WriteMarkdownWithVariants(&noVar, Metadata{Name: "aicr", Deterministic: true}, sampleResults(), nil); err != nil {
		t.Fatalf("WriteMarkdownWithVariants(no variants): %v", err)
	}
	if strings.Contains(noVar.String(), "## Version variants") {
		t.Error("variants section should be omitted entirely when no divergent pins exist")
	}
	if strings.Contains(noVar.String(), "_Rendering fidelity:") {
		t.Error("fidelity note should be absent without a caller-supplied value")
	}
}

// TestLegacyEntryPointsUnchanged pins the compatibility contract: the legacy
// BuildBOM/WriteMarkdown surfaces are byte-equivalent to the variant-aware
// entry points called with no variants and no fidelity — external importers
// see no behavioral change.
func TestLegacyEntryPointsUnchanged(t *testing.T) {
	meta := Metadata{Name: "aicr", Version: "v1", Deterministic: true}
	results := sampleResults()

	var legacy, modern bytes.Buffer
	if err := WriteMarkdown(&legacy, meta, results); err != nil {
		t.Fatalf("WriteMarkdown: %v", err)
	}
	if err := WriteMarkdownWithVariants(&modern, meta, results, nil); err != nil {
		t.Fatalf("WriteMarkdownWithVariants: %v", err)
	}
	if legacy.String() != modern.String() {
		t.Error("WriteMarkdown and WriteMarkdownWithVariants(nil) diverged")
	}

	// The CycloneDX surface must match too: the legacy BuildBOM and the
	// variant-aware BuildBOMWithVariants(nil) must serialize byte-identically,
	// locking component/dependency ordering and image dedup for importers.
	modernBOM, err := BuildBOMWithVariants(meta, results, nil)
	if err != nil {
		t.Fatalf("BuildBOMWithVariants: %v", err)
	}
	if encodeBOM(t, BuildBOM(meta, results)) != encodeBOM(t, modernBOM) {
		t.Error("BuildBOM and BuildBOMWithVariants(nil) serialized differently")
	}

	// Legacy BuildBOM tolerates duplicate component names exactly as before.
	doc := BuildBOM(meta, []ComponentResult{
		{Name: "a", Type: TypeHelm}, {Name: "a", Type: TypeHelm},
	})
	count := 0
	for _, c := range *doc.Components {
		if c.BOMRef == "aicr/a" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("legacy BuildBOM emitted %d entries for the duplicate, want the historical 2", count)
	}
}
