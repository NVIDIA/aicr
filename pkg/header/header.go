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

package header

import (
	"time"
)

// AICR artifact API versioning. These constants are the single source of
// truth for the group/version stamped into every AICR artifact header
// (snapshot, recipe, config). Package-local aliases (e.g.
// snapshotter.FullAPIVersion, recipe.RecipeAPIVersion, config.APIVersion)
// must reference GroupVersion rather than redeclaring the literal.
//
// Evolution policy (see docs/design/011-artifact-apiversion-policy.md):
// schema changes within a version must be additive-only; a breaking change
// requires a new version segment, after which both the old and new value are
// accepted during a transition window before the old one is retired.
const (
	// APIGroup is the API group for AICR artifacts.
	APIGroup = "aicr.nvidia.com"

	// APIVersionV1Alpha1 is the current artifact API version segment.
	APIVersionV1Alpha1 = "v1alpha1"

	// GroupVersion is the canonical "group/version" string for AICR artifacts.
	GroupVersion = APIGroup + "/" + APIVersionV1Alpha1
)

// IsSupportedAPIVersion reports whether v is an artifact apiVersion this binary
// understands. The empty string is intentionally NOT supported here: callers
// that tolerate a missing apiVersion for backward compatibility with older
// artifacts must special-case "" before calling this.
//
// When the artifact schema is bumped, add the new value to the set below
// (keeping the prior value) for the transition window, then remove the old
// value once the deprecation window closes.
func IsSupportedAPIVersion(v string) bool {
	switch v {
	case GroupVersion:
		return true
	default:
		return false
	}
}

// Kind represents the type of AICR resource.
// All AICR resources should use these constants for consistency.
type Kind string

// Valid Kind constants for all AICR resource types.
const (
	KindSnapshot     Kind = "Snapshot"
	KindRecipe       Kind = "Recipe"
	KindRecipeResult Kind = "RecipeResult"
)

// String returns the string representation of the Kind.
func (k Kind) String() string {
	return string(k)
}

// newHeader creates a new Header instance with an initialized Metadata map.
func newHeader() *Header {
	return &Header{
		Metadata: make(map[string]string),
	}
}

// Header contains metadata and versioning information for AICR resources.
// It follows Kubernetes-style resource conventions with Kind, APIVersion, and Metadata fields.
type Header struct {
	// Kind is the type of the snapshot object.
	Kind Kind `json:"kind,omitempty" yaml:"kind,omitempty"`

	// APIVersion is the API version of the snapshot object.
	APIVersion string `json:"apiVersion,omitempty" yaml:"apiVersion,omitempty"`

	// Metadata contains key-value pairs with metadata about the snapshot.
	Metadata map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
}

// Init initializes the Header with the specified kind, apiVersion, and version.
// It sets the Kind, APIVersion, and populates Metadata with timestamp and version.
// Uses unprefixed keys (timestamp, version) for all kinds.
//
// The timestamp is wall-clock time. Reproducible-build callers (SLSA, signed
// artifacts) must inject a fixed timestamp via InitWithTime to keep the
// serialized header byte-stable across runs.
func (h *Header) Init(kind Kind, apiVersion string, version string) {
	h.InitWithTime(kind, apiVersion, version, time.Now().UTC())
}

// InitWithTime is like Init but uses the caller-supplied timestamp. Use this
// when the header feeds into a digest, signature, or otherwise reproducible
// artifact — derive ts from a content-addressable source (commit SHA, the
// SOURCE_DATE_EPOCH environment variable, etc.).
func (h *Header) InitWithTime(kind Kind, apiVersion string, version string, ts time.Time) {
	h.Kind = kind
	h.APIVersion = apiVersion
	h.Metadata = make(map[string]string)

	// Use unprefixed keys for all kinds
	h.Metadata["timestamp"] = ts.UTC().Format(time.RFC3339)
	if version != "" {
		h.Metadata["version"] = version
	}
}
