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

package bom

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	cdx "github.com/CycloneDX/cyclonedx-go"
	"github.com/google/uuid"

	"github.com/NVIDIA/aicr/pkg/errors"
)

const (
	defaultRootName = "aicr"
	defaultSupplier = "NVIDIA Corporation"
)

// ComponentResult.Type identifiers, shared so producers (tools/bom, the
// attestation builder) and this renderer cannot drift on the string values.
// Type is compared case-insensitively at render time because the attestation
// builder supplies the recipe ComponentType enum ("Kustomize") while the
// standalone generator supplies the lowercase kind ("kustomize").
const (
	TypeHelm      = "helm"
	TypeKustomize = "kustomize"
	TypeManifest  = "manifest"
)

// Metadata identifies the artifact the BOM describes (e.g., the AICR repo
// itself, or a specific recipe bundle).
type Metadata struct {
	Name        string // e.g., "aicr" or "recipe-h100-eks-ubuntu-training"
	Version     string // e.g., "v0.12.1" or recipe version
	Description string
	Supplier    string // organization name; defaults to "NVIDIA Corporation"
	ToolName    string // tool that generated the BOM; defaults to "aicr"
	ToolVersion string // version of the generating tool

	// Deterministic suppresses run-specific metadata so the artifact can
	// be diffed across runs. Affects both WriteMarkdown (the "_Generated
	// <timestamp>..._" line is omitted) and BuildBOM (a deterministic
	// SerialNumber is derived from Name+Version and Timestamp is omitted).
	// Use for committed doc artifacts, SLSA-style reproducible builds,
	// and any other bit-for-bit reproducible output path.
	Deterministic bool

	// SerialNumber, if non-empty, overrides the BOM serial number. When
	// empty and Deterministic is false, BuildBOM generates a random UUID.
	// When empty and Deterministic is true, BuildBOM derives a serial
	// from Name+Version. Use to inject a caller-supplied identifier
	// (e.g., commit SHA) without forcing the Deterministic mode.
	SerialNumber string

	// Timestamp, if non-empty, overrides the BOM metadata timestamp
	// (RFC3339). When empty and Deterministic is false, BuildBOM uses
	// time.Now().UTC(). When empty and Deterministic is true, the
	// timestamp is omitted entirely.
	Timestamp string

	// NoTitle suppresses the H1 title line in WriteMarkdown so the body
	// can be embedded as a section of a larger document (e.g., the
	// auto-generated middle of docs/user/container-images.md, where the
	// title and surrounding prose are hand-edited).
	NoTitle bool

	// RenderFidelity, when non-empty, is stated in the Markdown header and
	// attached to the CycloneDX metadata as an aicr:render:fidelity
	// property. Producers set it to describe HOW image sets were obtained
	// (tools/bom sets RenderFidelityCatalogParity); producers with a
	// different story leave it empty rather than stamp a false claim.
	RenderFidelity string
}

// ComponentResult is the per-component image survey input to BuildBOM.
// It carries the metadata needed to render a CycloneDX `application`
// component plus the list of image references it deploys.
type ComponentResult struct {
	Name        string   // component identifier, e.g., "gpu-operator"
	DisplayName string   // human-readable name
	Type        string   // "helm", "kustomize", or "manifest"
	Repository  string   // chart repository URL (helm only)
	Chart       string   // chart name (helm only)
	Version     string   // chart version if pinned
	Namespace   string   // default namespace
	Pinned      bool     // whether the chart version is pinned in the recipe
	Images      []string // sorted, deduplicated image references
	Warnings    []string // non-fatal issues to attach as properties
}

// BuildBOM constructs a CycloneDX 1.6 BOM from a sorted list of component
// surveys. The graph is:
//
//	metadata.component (Metadata.Name)
//	  └─ each ComponentResult as an `application` (bom-ref: "<name>/<comp>")
//	       └─ each unique image as a `container` (bom-ref: "img:<ref>")
//
// Image entries are de-duplicated across components. For a BOM that also
// carries per-source version variants — with fail-closed bom-ref uniqueness —
// use BuildBOMWithVariants; this legacy entry point is preserved unchanged
// for existing importers.
func BuildBOM(meta Metadata, results []ComponentResult) *cdx.BOM {
	doc, _ := buildBOMDoc(meta, results, nil, false) // unchecked mode cannot error
	return doc
}

// buildBOMDoc is the shared builder behind BuildBOM (checked=false: legacy
// semantics, duplicate component refs are emitted as-is and no variants) and
// BuildBOMWithVariants (checked=true: every bom-ref claims through a global
// identity set and collisions fail closed).
func buildBOMDoc(meta Metadata, results []ComponentResult, variants []VariantResult, checked bool) (*cdx.BOM, error) {
	if meta.Name == "" {
		meta.Name = defaultRootName
	}
	if meta.Supplier == "" {
		meta.Supplier = defaultSupplier
	}
	if meta.ToolName == "" {
		meta.ToolName = defaultRootName
	}

	bom := cdx.NewBOM()
	bom.SerialNumber = bomSerialNumber(meta)
	bom.Metadata = &cdx.Metadata{
		Timestamp: bomTimestamp(meta),
		Tools: &cdx.ToolsChoice{
			Components: &[]cdx.Component{{
				Type:    cdx.ComponentTypeApplication,
				Name:    meta.ToolName,
				Version: meta.ToolVersion,
			}},
		},
		Component: &cdx.Component{
			BOMRef:      meta.Name,
			Type:        cdx.ComponentTypeApplication,
			Name:        meta.Name,
			Version:     meta.Version,
			Description: meta.Description,
			Supplier: &cdx.OrganizationalEntity{
				Name: meta.Supplier,
			},
		},
	}
	if meta.RenderFidelity != "" {
		bom.Metadata.Properties = &[]cdx.Property{
			{Name: "aicr:render:fidelity", Value: meta.RenderFidelity},
		}
	}

	// Copy before sorting so callers (e.g., pkg/bundler when it consumes
	// this) don't observe their input slice reordered.
	sorted := append([]ComponentResult(nil), results...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Name < sorted[j].Name
	})

	claims := newRefClaims(meta.Name, checked)
	var (
		comps []cdx.Component
		deps  []cdx.Dependency
	)
	rootChildren := make([]string, 0, len(sorted)+len(variants))

	for _, r := range sorted {
		compRef := meta.Name + "/" + r.Name
		if err := claims.claimRef(compRef); err != nil {
			return nil, err
		}
		rootChildren = append(rootChildren, compRef)

		props := componentProperties(r)
		comps = append(comps, cdx.Component{
			BOMRef:      compRef,
			Type:        cdx.ComponentTypeApplication,
			Name:        r.Name,
			Description: r.DisplayName,
			Version:     r.Version,
			Properties:  &props,
		})
		var err error
		comps, deps, err = appendImageEntries(claims, comps, deps, compRef, r.Images)
		if err != nil {
			return nil, err
		}
	}

	// Variants (checked mode only): version-qualified refs so default
	// entries stay untouched, sorted by (name, version) for determinism.
	sortedVariants := append([]VariantResult(nil), variants...)
	sort.Slice(sortedVariants, func(i, j int) bool {
		if sortedVariants[i].Name != sortedVariants[j].Name {
			return sortedVariants[i].Name < sortedVariants[j].Name
		}
		return sortedVariants[i].Version < sortedVariants[j].Version
	})
	for _, v := range sortedVariants {
		compRef := meta.Name + "/" + v.Name + "@" + v.Version
		if err := claims.claimRef(compRef); err != nil {
			return nil, err
		}
		rootChildren = append(rootChildren, compRef)
		comps = append(comps, variantComponent(compRef, v))
		var err error
		comps, deps, err = appendImageEntries(claims, comps, deps, compRef, v.Images)
		if err != nil {
			return nil, err
		}
	}

	if len(rootChildren) > 0 {
		deps = append([]cdx.Dependency{{Ref: meta.Name, Dependencies: refList(rootChildren)}}, deps...)
	}

	bom.Components = &comps
	bom.Dependencies = &deps
	return bom, nil
}

// refClaims tracks bom-ref identity across a document build. refs is the
// global identity set used in checked mode: root, components, variants, and
// images all pass through it (CycloneDX requires unique bom-refs). The value
// records whether the ref is an image — images legitimately repeat across
// components (a repeat is a dedup, not a collision), any other reuse fails
// closed. Unchecked (legacy) mode keeps the historical behavior: no claims,
// image-level dedup only via seen.
type refClaims struct {
	checked bool
	seen    map[string]struct{}
	refs    map[string]bool
}

func newRefClaims(rootRef string, checked bool) *refClaims {
	return &refClaims{
		checked: checked,
		seen:    map[string]struct{}{},
		refs:    map[string]bool{rootRef: false},
	}
}

// claimRef claims a component/variant identity; a duplicate fails closed in
// checked mode.
func (c *refClaims) claimRef(ref string) error {
	if !c.checked {
		return nil
	}
	if _, dup := c.refs[ref]; dup {
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"duplicate bom-ref %q: component and variant identities must be unique "+
				"(CycloneDX requires unique bom-refs and the dependency graph would be ambiguous)", ref))
	}
	c.refs[ref] = false
	return nil
}

// claimImage claims an image ref, reporting whether it is fresh (needs an
// entry). A repeat of another image is a cross-component dedup; colliding
// with a non-image identity fails closed in checked mode.
func (c *refClaims) claimImage(ref string) (bool, error) {
	if _, dup := c.seen[ref]; dup {
		return false, nil
	}
	if c.checked {
		if isImage, exists := c.refs[ref]; exists && !isImage {
			return false, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"duplicate bom-ref %q: an image reference collides with a component identity", ref))
		}
		c.refs[ref] = true
	}
	c.seen[ref] = struct{}{}
	return true, nil
}

// appendImageEntries claims each image under compRef, appends container
// entries for fresh images, and records the compRef→images dependency edge.
func appendImageEntries(claims *refClaims, comps []cdx.Component, deps []cdx.Dependency, compRef string, images []string) ([]cdx.Component, []cdx.Dependency, error) {
	imgRefs := make([]string, 0, len(images))
	for _, img := range images {
		fresh, err := claims.claimImage("img:" + img)
		if err != nil {
			return nil, nil, err
		}
		if fresh {
			comps = append(comps, imageComponent(img))
		}
		imgRefs = append(imgRefs, "img:"+img)
	}
	if len(imgRefs) > 0 {
		deps = append(deps, cdx.Dependency{
			Ref:          compRef,
			Dependencies: refList(imgRefs),
		})
	}
	return comps, deps, nil
}

// componentProperties names the deployment properties by effective component
// type so a Kustomize component's Git source and tag are not mislabeled as a
// Helm repository/version (the CycloneDX doc already declares
// aicr:component:type). Helm and manifest components keep the aicr:helm:*
// names. Chart is Helm-only and omitted for Kustomize.
func componentProperties(r ComponentResult) []cdx.Property {
	kustomize := strings.EqualFold(r.Type, TypeKustomize)
	props := []cdx.Property{
		{Name: "aicr:component:type", Value: r.Type},
	}
	if r.Repository != "" {
		name := "aicr:helm:repository"
		if kustomize {
			name = "aicr:kustomize:source"
		}
		props = append(props, cdx.Property{Name: name, Value: r.Repository})
	}
	if r.Chart != "" && !kustomize {
		props = append(props, cdx.Property{Name: "aicr:helm:chart", Value: r.Chart})
	}
	if r.Version != "" {
		name := "aicr:helm:version"
		if kustomize {
			name = "aicr:kustomize:tag"
		}
		props = append(props, cdx.Property{Name: name, Value: r.Version})
	}
	if r.Namespace != "" {
		name := "aicr:helm:namespace"
		if kustomize {
			name = "aicr:kustomize:namespace"
		}
		props = append(props, cdx.Property{Name: name, Value: r.Namespace})
	}
	props = append(props, cdx.Property{Name: "aicr:version:pinned", Value: strconv.FormatBool(r.Pinned)})
	for _, w := range r.Warnings {
		props = append(props, cdx.Property{Name: "aicr:render:warning", Value: w})
	}
	return props
}

// imageComponent renders one image reference as a CycloneDX container entry.
func imageComponent(img string) cdx.Component {
	ref := ParseImageRef(img)
	return cdx.Component{
		BOMRef:     "img:" + img,
		Type:       cdx.ComponentTypeContainer,
		Name:       ref.Registry + "/" + ref.Repository,
		Version:    versionOrTag(ref),
		PackageURL: ref.PURL(),
		Properties: &[]cdx.Property{
			{Name: "aicr:image:registry", Value: ref.Registry},
			{Name: "aicr:image:repository", Value: ref.Repository},
			{Name: "aicr:image:tag", Value: ref.Tag},
			{Name: "aicr:image:digest", Value: ref.Digest},
		},
	}
}

// bomSerialNumber picks a SerialNumber for the BOM based on Metadata. The
// precedence is: explicit override > deterministic derivation > random UUID.
func bomSerialNumber(meta Metadata) string {
	if meta.SerialNumber != "" {
		return meta.SerialNumber
	}
	if meta.Deterministic {
		// Derive a stable UUIDv5 in the URL namespace from Name+Version
		// so two runs of the same inputs produce identical bytes.
		seed := meta.Name + "@" + meta.Version
		return "urn:uuid:" + uuid.NewSHA1(uuid.NameSpaceURL, []byte(seed)).String()
	}
	return "urn:uuid:" + uuid.NewString()
}

// bomTimestamp picks a Timestamp for the BOM metadata. Returns empty string
// in deterministic mode (CycloneDX permits omitting the timestamp).
func bomTimestamp(meta Metadata) string {
	if meta.Timestamp != "" {
		return meta.Timestamp
	}
	if meta.Deterministic {
		return ""
	}
	return time.Now().UTC().Format(time.RFC3339)
}

func versionOrTag(r ImageRef) string {
	if r.Digest != "" {
		return r.Digest
	}
	return r.Tag
}

func refList(refs []string) *[]string {
	out := append([]string{}, refs...)
	sort.Strings(out)
	return &out
}
