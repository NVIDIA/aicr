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

// Package artifact defines the neutral release inventory and catalog bindings.
package artifact

import "github.com/NVIDIA/aicr/pkg/header"

const (
	APIVersion    = header.AuthoringGroupVersion
	InventoryKind = "ArtifactInventory"
	BindingsKind  = "ArtifactBindings"
)

// Options selects composition inputs; an inventory is the activation boundary.
type Options struct {
	Inventory        string   `json:"inventory,omitempty" yaml:"inventory,omitempty"`
	AllowCatalogPins []string `json:"allowCatalogPins,omitempty" yaml:"allowCatalogPins,omitempty"`
}

// Inventory contains producer identities without catalog component names.
type Inventory struct {
	APIVersion string     `json:"apiVersion" yaml:"apiVersion"`
	Kind       string     `json:"kind" yaml:"kind"`
	Metadata   *Metadata  `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	Artifacts  []Identity `json:"artifacts" yaml:"artifacts"`
}

// Metadata is an optional producer label.
type Metadata struct {
	Name string `json:"name,omitempty" yaml:"name,omitempty"`
}

// Identity retains literal OCI identities; a chart's manifest is not its archive.
type Identity struct {
	Name           string    `json:"name" yaml:"name"`
	Type           string    `json:"type" yaml:"type"`
	Reference      string    `json:"reference" yaml:"reference"`
	Version        string    `json:"version" yaml:"version"`
	ManifestDigest string    `json:"manifestDigest" yaml:"manifestDigest"`
	ArchiveDigest  string    `json:"archiveDigest,omitempty" yaml:"archiveDigest,omitempty"`
	Platform       *Platform `json:"platform,omitempty" yaml:"platform,omitempty"`
}

// Platform asserts a single manifest platform, independently of the host.
type Platform struct {
	OS           string  `json:"os" yaml:"os"`
	Architecture string  `json:"architecture" yaml:"architecture"`
	Variant      *string `json:"variant,omitempty" yaml:"variant,omitempty"`
}

// Bindings maps stable artifact names to catalog dispositions.
type Bindings struct {
	APIVersion       string             `json:"apiVersion" yaml:"apiVersion"`
	Kind             string             `json:"kind" yaml:"kind"`
	Charts           map[string]Chart   `json:"charts" yaml:"charts"`
	Images           map[string]Image   `json:"images" yaml:"images"`
	IgnoredArtifacts map[string]Ignored `json:"ignoredArtifacts" yaml:"ignoredArtifacts,omitempty"`
}

// Chart binds a chart archive to one component.
type Chart struct {
	Component string `json:"component" yaml:"component"`
}

// Image binds a manifest to one or more explicit values targets.
type Image struct {
	Targets []Target `json:"targets" yaml:"targets"`
}

// Target names an existing values file and its supported identity layout.
type Target struct {
	Component  string    `json:"component" yaml:"component"`
	ValuesFile string    `json:"valuesFile" yaml:"valuesFile"`
	Platform   *Platform `json:"platform,omitempty" yaml:"platform,omitempty"`
	Write      Write     `json:"write" yaml:"write"`
}

// Write supports a full reference or a split repository/digest, with tag clearing.
type Write struct {
	Reference  string `json:"reference,omitempty" yaml:"reference,omitempty"`
	Repository string `json:"repository,omitempty" yaml:"repository,omitempty"`
	Digest     string `json:"digest,omitempty" yaml:"digest,omitempty"`
	ClearTag   string `json:"clearTag,omitempty" yaml:"clearTag,omitempty"`
}

// Ignored accounts for an artifact without waiving selected-component coverage.
type Ignored struct {
	Reason string `json:"reason" yaml:"reason"`
}

// Provenance records the immutable inputs and every applied or ignored identity.
type Provenance struct {
	InventoryDigest   string            `json:"inventoryDigest" yaml:"inventoryDigest"`
	BindingsDigest    string            `json:"bindingsDigest" yaml:"bindingsDigest"`
	BaseCatalogDigest string            `json:"baseCatalogDigest" yaml:"baseCatalogDigest"`
	AllowCatalogPins  []string          `json:"allowCatalogPins" yaml:"allowCatalogPins"`
	Verification      string            `json:"verification" yaml:"verification"`
	Applied           []Applied         `json:"applied" yaml:"applied"`
	Ignored           []IgnoredIdentity `json:"ignored" yaml:"ignored"`
}

// Applied associates a producer identity with its effective binding.
type Applied struct {
	Artifact Identity `json:"artifact" yaml:"artifact"`
	Chart    *Chart   `json:"chart,omitempty" yaml:"chart,omitempty"`
	Image    *Image   `json:"image,omitempty" yaml:"image,omitempty"`
}

// IgnoredIdentity preserves audit information for an intentionally unused artifact.
type IgnoredIdentity struct {
	Artifact Identity `json:"artifact" yaml:"artifact"`
	Reason   string   `json:"reason" yaml:"reason"`
}

// Equal compares platform assertions, including whether a variant was supplied.
func (p *Platform) Equal(other *Platform) bool {
	if p == nil || other == nil {
		return p == other
	}
	if p.OS != other.OS || p.Architecture != other.Architecture {
		return false
	}
	if p.Variant == nil || other.Variant == nil {
		return p.Variant == other.Variant
	}
	return *p.Variant == *other.Variant
}
