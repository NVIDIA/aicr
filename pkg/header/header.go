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
// truth for every AICR artifact group/version. Package-local emitters and
// readers select a version by wire kind and schema track; see ADR-022.
//
// Three tracks exist. StableGroupVersion, AuthoringGroupVersion, and
// ProfileGroupVersion name the value each track emits today; GroupVersionV1,
// GroupVersionV1Beta1, and GroupVersionV1Beta2 name where each is headed.
// StableGroupVersion and AuthoringGroupVersion carry the same string during
// the reader-first release and diverge at the emitter switch, so a package
// emitter must alias the constant for its track rather than the string it
// happens to equal. Aliasing GroupVersion directly is what made the switch a
// refactor instead of an edit.
//
// Evolution policy (see docs/design/011-artifact-apiversion-policy.md and
// docs/design/022-artifact-maturity-and-deprecation.md): schema changes within
// a version must be additive-only; a breaking change requires a new version
// segment. Alpha versions owe no deprecation window. Beta versions remain
// readable for two AICR releases after deprecation, and GA versions remain
// readable through the current AICR major version. Version bumps that owe a
// window stage readers before emitters.
const (
	// Domain is the single source of truth for the AICR API domain. Every
	// role (apiVersion group, K8s label/annotation keys, attestation and
	// provenance URI hosts, UUIDv5 namespace seed) derives from this value.
	Domain = "aicr.run"

	// APIGroup is the API group for AICR artifacts.
	APIGroup = Domain

	// APIVersionV1Alpha2 is the current artifact API version segment.
	APIVersionV1Alpha2 = "v1alpha2"

	// APIVersionV1Alpha3 is the strict RecipeResult schema carrying typed
	// desired-state configuration. Other artifact kinds remain on v1alpha2.
	APIVersionV1Alpha3 = "v1alpha3"

	// APIVersionV1Beta1 is the ADR-022 target for authoring and configuration
	// artifacts: AICRConfig, ordinary RecipeMetadata, RecipeMixin, and
	// ComponentRegistry.
	APIVersionV1Beta1 = "v1beta1"

	// APIVersionV1Beta2 is the ADR-022 target for profile-bearing
	// RecipeMetadata and RecipeResult artifacts.
	APIVersionV1Beta2 = "v1beta2"

	// APIVersionV1 is the ADR-022 target for stable public artifacts:
	// Snapshot, default RecipeResult, RecipeCriteria, and BundleProvenance.
	APIVersionV1 = "v1"

	// GroupVersion is the canonical "group/version" string for AICR artifacts.
	GroupVersion = APIGroup + "/" + APIVersionV1Alpha2

	// RecipeResultGroupVersion is the current configured RecipeResult schema.
	RecipeResultGroupVersion = APIGroup + "/" + APIVersionV1Alpha3

	// StableGroupVersion is the value emitted for the ADR-022 stable artifact
	// track: Snapshot, the default RecipeResult, RecipeCriteria, and
	// BundleProvenance. Its §2 target is GroupVersionV1.
	StableGroupVersion = GroupVersion

	// AuthoringGroupVersion is the value emitted for the ADR-022 authoring and
	// configuration track: AICRConfig, ordinary RecipeMetadata, RecipeMixin,
	// and ComponentRegistry. Its §2 target is GroupVersionV1Beta1.
	AuthoringGroupVersion = GroupVersion

	// ProfileGroupVersion is the value emitted for the ADR-022 profile-bearing
	// track: profile RecipeMetadata and RecipeResult. Its §2 target is
	// GroupVersionV1Beta2.
	ProfileGroupVersion = RecipeResultGroupVersion

	// GroupVersionV1Beta1 is the target authoring/configuration group/version.
	GroupVersionV1Beta1 = APIGroup + "/" + APIVersionV1Beta1

	// GroupVersionV1Beta2 is the target profile-bearing group/version.
	GroupVersionV1Beta2 = APIGroup + "/" + APIVersionV1Beta2

	// GroupVersionV1 is the target stable public artifact group/version.
	GroupVersionV1 = APIGroup + "/" + APIVersionV1
)

// IsSupportedAPIVersion reports whether v is an artifact apiVersion this binary
// understands. The empty string is intentionally NOT supported here: callers
// that tolerate a missing apiVersion for backward compatibility with older
// artifacts must special-case "" before calling this.
//
// This compatibility helper covers the stable artifact track only. Callers
// reading authoring or profile-bearing artifacts must use the corresponding
// schema-track helper instead of treating versions as globally interchangeable.
func IsSupportedAPIVersion(v string) bool {
	switch v {
	case GroupVersion, GroupVersionV1:
		return true
	default:
		return false
	}
}

// IsSupportedAuthoringAPIVersion reports whether v is accepted for an
// ADR-022 authoring/configuration artifact during the Release N reader-first
// window.
func IsSupportedAuthoringAPIVersion(v string) bool {
	switch v {
	case GroupVersion, GroupVersionV1Beta1:
		return true
	default:
		return false
	}
}

// IsSupportedProfileAPIVersion reports whether v is accepted for a
// profile-bearing RecipeMetadata or RecipeResult during the Release N
// reader-first window.
func IsSupportedProfileAPIVersion(v string) bool {
	switch v {
	case RecipeResultGroupVersion, GroupVersionV1Beta2:
		return true
	default:
		return false
	}
}

// IsSupportedRecipeResultAPIVersion reports whether v is a RecipeResult
// version understood by this binary. The gate is the union of the default and
// profile-bearing schema tracks; callers must still enforce the bidirectional
// version/profile discriminator contract.
func IsSupportedRecipeResultAPIVersion(v string) bool {
	return IsSupportedAPIVersion(v) || IsSupportedProfileAPIVersion(v)
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
