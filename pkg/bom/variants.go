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
	"io"
	"sort"
	"strings"

	cdx "github.com/CycloneDX/cyclonedx-go"
)

// RenderFidelityCatalogParity documents the uniform rendering limitation of
// every entry in the catalog BOM (tools/bom) — default and variant alike:
// charts are templated with the shared per-component values file only. It is
// caller-supplied via Metadata.RenderFidelity because other BOM producers
// have a different fidelity story and must not carry a false claim. See
// issue #1611.
const RenderFidelityCatalogParity = "catalog-parity: charts are rendered with the shared " +
	"recipes/components/<name>/values.yaml; per-recipe overlay overrides are not applied"

// VariantResult is a per-source version variant of a registry component: an
// explicit base/overlay/mixin Helm componentRef.version pin that differs from
// the component's registry defaultVersion. Variants are derived from recipe
// data (the declared pins themselves), aggregated by (Name, Version), and
// rendered at catalog parity with the REGISTRY chart coordinates. See issue
// #1611 — coordinate or type divergence and effective-inheritance resolution
// are explicit non-goals of the variants model.
type VariantResult struct {
	Name       string   // component identifier, e.g., "kube-prometheus-stack"
	Version    string   // the divergent pinned chart version
	Sources    []string // sorted base/overlay/mixin metadata.names declaring the pin
	Repository string   // chart repository URL (registry coordinates)
	Chart      string   // chart name (registry coordinates)
	Namespace  string   // default namespace
	Images     []string // sorted, deduplicated image references at this version
	Warnings   []string // non-fatal issues to attach as properties
}

// BuildBOMWithVariants is the variant-aware companion to BuildBOM: default
// component entries are byte-identical to BuildBOM's, variant entries carry
// version-qualified bom-refs ("<meta.Name>/<comp>@<ver>") with sorted
// aicr:variant:sources, and — unlike the legacy entry point — every bom-ref
// claims through a global identity set, so duplicates fail closed with
// ErrCodeInvalidRequest instead of producing an ambiguous dependency graph.
func BuildBOMWithVariants(meta Metadata, results []ComponentResult, variants []VariantResult) (*cdx.BOM, error) {
	return buildBOMDoc(meta, results, variants, true)
}

// WriteMarkdownWithVariants is the variant-aware companion to WriteMarkdown:
// variants render as a separate "Version variants" table (distinct column
// headers, so the Components table and its freshness gate stay intact) plus
// per-variant image sections; the section is omitted entirely when no
// variants exist. The legacy WriteMarkdown output is unchanged.
func WriteMarkdownWithVariants(w io.Writer, meta Metadata, results []ComponentResult, variants []VariantResult) error {
	return writeMarkdownDoc(w, meta, results, variants)
}

// variantComponent renders one VariantResult as a CycloneDX application
// entry with its version-qualified bom-ref and variant properties.
func variantComponent(compRef string, v VariantResult) cdx.Component {
	srcs := append([]string(nil), v.Sources...)
	sort.Strings(srcs)
	props := []cdx.Property{
		{Name: "aicr:component:type", Value: TypeHelm},
		{Name: "aicr:variant:of", Value: v.Name},
		{Name: "aicr:variant:sources", Value: strings.Join(srcs, ",")},
	}
	if v.Repository != "" {
		props = append(props, cdx.Property{Name: "aicr:helm:repository", Value: v.Repository})
	}
	if v.Chart != "" {
		props = append(props, cdx.Property{Name: "aicr:helm:chart", Value: v.Chart})
	}
	props = append(props, cdx.Property{Name: "aicr:helm:version", Value: v.Version})
	if v.Namespace != "" {
		props = append(props, cdx.Property{Name: "aicr:helm:namespace", Value: v.Namespace})
	}
	props = append(props, cdx.Property{Name: "aicr:version:pinned", Value: "true"})
	for _, w := range v.Warnings {
		props = append(props, cdx.Property{Name: "aicr:render:warning", Value: w})
	}
	return cdx.Component{
		BOMRef:     compRef,
		Type:       cdx.ComponentTypeApplication,
		Name:       v.Name,
		Version:    v.Version,
		Properties: &props,
	}
}
