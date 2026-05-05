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
	"sort"
	"strconv"
	"time"

	cdx "github.com/CycloneDX/cyclonedx-go"
	"github.com/google/uuid"
)

const (
	defaultRootName = "aicr"
	defaultSupplier = "NVIDIA Corporation"
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
// Image entries are de-duplicated across components.
func BuildBOM(meta Metadata, results []ComponentResult) *cdx.BOM {
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
	bom.SerialNumber = "urn:uuid:" + uuid.NewString()
	bom.Metadata = &cdx.Metadata{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
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

	// Copy before sorting so callers (e.g., pkg/bundler when it consumes
	// this) don't observe their input slice reordered.
	sorted := append([]ComponentResult(nil), results...)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Name < sorted[j].Name
	})

	var (
		comps []cdx.Component
		deps  []cdx.Dependency
		seen  = map[string]struct{}{}
	)
	rootChildren := make([]string, 0, len(sorted))

	for _, r := range sorted {
		compRef := meta.Name + "/" + r.Name
		rootChildren = append(rootChildren, compRef)

		props := []cdx.Property{
			{Name: "aicr:component:type", Value: r.Type},
		}
		if r.Repository != "" {
			props = append(props, cdx.Property{Name: "aicr:helm:repository", Value: r.Repository})
		}
		if r.Chart != "" {
			props = append(props, cdx.Property{Name: "aicr:helm:chart", Value: r.Chart})
		}
		if r.Version != "" {
			props = append(props, cdx.Property{Name: "aicr:helm:version", Value: r.Version})
		}
		if r.Namespace != "" {
			props = append(props, cdx.Property{Name: "aicr:helm:namespace", Value: r.Namespace})
		}
		props = append(props, cdx.Property{Name: "aicr:version:pinned", Value: strconv.FormatBool(r.Pinned)})
		for _, w := range r.Warnings {
			props = append(props, cdx.Property{Name: "aicr:render:warning", Value: w})
		}

		comps = append(comps, cdx.Component{
			BOMRef:      compRef,
			Type:        cdx.ComponentTypeApplication,
			Name:        r.Name,
			Description: r.DisplayName,
			Version:     r.Version,
			Properties:  &props,
		})

		var imgRefs []string
		for _, img := range r.Images {
			ref := ParseImageRef(img)
			imgRef := "img:" + img
			if _, ok := seen[imgRef]; !ok {
				seen[imgRef] = struct{}{}
				comps = append(comps, cdx.Component{
					BOMRef:     imgRef,
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
				})
			}
			imgRefs = append(imgRefs, imgRef)
		}
		if len(imgRefs) > 0 {
			deps = append(deps, cdx.Dependency{
				Ref:          compRef,
				Dependencies: refList(imgRefs),
			})
		}
	}

	if len(rootChildren) > 0 {
		deps = append([]cdx.Dependency{{Ref: meta.Name, Dependencies: refList(rootChildren)}}, deps...)
	}

	bom.Components = &comps
	bom.Dependencies = &deps
	return bom
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
