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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	cdx "github.com/CycloneDX/cyclonedx-go"
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
		meta.Name = "aicr"
	}
	if meta.Supplier == "" {
		meta.Supplier = "NVIDIA Corporation"
	}
	if meta.ToolName == "" {
		meta.ToolName = "aicr"
	}

	bom := cdx.NewBOM()
	bom.SerialNumber = "urn:uuid:" + newUUIDv4()
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

	sort.Slice(results, func(i, j int) bool {
		return results[i].Name < results[j].Name
	})

	var (
		comps []cdx.Component
		deps  []cdx.Dependency
		seen  = map[string]struct{}{}
	)
	rootChildren := make([]string, 0, len(results))

	for _, r := range results {
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
		props = append(props, cdx.Property{Name: "aicr:version:pinned", Value: boolStr(r.Pinned)})
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

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func refList(refs []string) *[]string {
	out := append([]string{}, refs...)
	sort.Strings(out)
	return &out
}

// newUUIDv4 returns a random UUID v4 without depending on github.com/google/uuid.
func newUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to a timestamped pseudo-UUID; non-fatal for BOM identity.
		ts := time.Now().UnixNano()
		return fmt.Sprintf("00000000-0000-4000-8000-%012x", ts&0xffffffffffff)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]))
}
