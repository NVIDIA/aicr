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

// Package mirror discovers container images and Helm charts referenced by
// a recipe and emits the list in formats consumable by air-gap tools
// (Hauler, Zarf) and general-purpose formats (JSON, YAML).
package mirror

import (
	"encoding/json"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// Format enumerates the output formats mirror list supports.
type Format string

const (
	// FormatYAML renders the discovery result as YAML (default).
	FormatYAML Format = "yaml"
	// FormatJSON renders the discovery result as JSON.
	FormatJSON Format = "json"
	// FormatHauler renders a Hauler manifest (content.hauler.cattle.io/v1).
	FormatHauler Format = "hauler"
	// FormatZarf renders a Zarf package config (ZarfPackageConfig).
	FormatZarf Format = "zarf"
)

// SupportedFormats returns the list of valid format strings for shell
// completion and usage text.
func SupportedFormats() []string {
	return []string{
		string(FormatYAML),
		string(FormatJSON),
		string(FormatHauler),
		string(FormatZarf),
	}
}

// MirrorList is the discovery result containing all container images and
// Helm charts referenced by a recipe. It is the serialization-ready
// payload for json/yaml formats and the input to format-specific
// renderers (Hauler, Zarf).
type MirrorList struct {
	// Images is the global sorted, deduplicated set of container images.
	Images []string `json:"images" yaml:"images"`

	// Charts is the list of Helm charts referenced by the recipe.
	Charts []ChartRef `json:"charts" yaml:"charts"`

	// Components is the per-component image breakdown (for detailed output).
	Components []ComponentImages `json:"components" yaml:"components"`

	// Metadata carries recipe provenance for traceability.
	Metadata MirrorListMetadata `json:"metadata" yaml:"metadata"`
}

// ChartRef describes a Helm chart artifact needed by the recipe.
type ChartRef struct {
	// Name is the component name that references this chart.
	Name string `json:"name" yaml:"name"`
	// Repository is the Helm repository URL (oci:// or https://).
	Repository string `json:"repository" yaml:"repository"`
	// Chart is the Helm chart name.
	Chart string `json:"chart" yaml:"chart"`
	// Version is the pinned chart version.
	Version string `json:"version" yaml:"version"`
	// Namespace is the target Kubernetes namespace.
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
}

// ComponentImages groups discovered images by the component that
// references them.
type ComponentImages struct {
	// Component is the component name.
	Component string `json:"component" yaml:"component"`
	// Type is the component type ("helm" or "kustomize").
	Type string `json:"type" yaml:"type"`
	// Images is the sorted, deduplicated list of container images.
	Images []string `json:"images" yaml:"images"`
	// Warnings contains non-fatal issues encountered during discovery
	// (e.g., helm template render warnings).
	Warnings []string `json:"warnings,omitempty" yaml:"warnings,omitempty"`
}

// MirrorListMetadata carries recipe provenance.
type MirrorListMetadata struct {
	// RecipeVersion is the CLI version that generated the recipe.
	RecipeVersion string `json:"recipeVersion,omitempty" yaml:"recipeVersion,omitempty"`
	// Criteria is a human-readable summary of the recipe criteria.
	Criteria string `json:"criteria,omitempty" yaml:"criteria,omitempty"`
}

// Render writes the MirrorList to w in the given format.
func Render(w io.Writer, list *MirrorList, format Format) error {
	if list == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "mirror list is nil")
	}

	switch format {
	case FormatJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(list); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to encode JSON", err)
		}
		return nil
	case FormatYAML:
		enc := yaml.NewEncoder(w)
		enc.SetIndent(2)
		if err := enc.Encode(list); err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to encode YAML", err)
		}
		return nil
	case FormatHauler:
		return renderHauler(w, list)
	case FormatZarf:
		return renderZarf(w, list)
	default:
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported format: %q", format))
	}
}
