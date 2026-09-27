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
	"fmt"
	"time"
)

// AICR artifact API versioning. These constants are the single source of
// truth for every AICR artifact group/version. Package-local emitters and
// readers select a version by wire kind and schema track; see ADR-022.
//
// Three tracks exist. StableGroupVersion, AuthoringGroupVersion, and
// ProfileGroupVersion name the value each track emits; since the v0.22 emitter
// switch (#2416) each equals its target, GroupVersionV1, GroupVersionV1Beta1
// and GroupVersionV1Beta2 respectively.
//
// The three carried one shared string before the v0.22 emitter switch, which is
// what let a package alias that string directly and still look correct while
// emitting the wrong value later. Alias the constant for your track, never the
// string it happens to equal; the tracks hold distinct values now and a
// collapsed alias shows up as a wrong value rather than a latent one.
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

	// StableGroupVersion is the value emitted for the ADR-022 stable artifact
	// track: Snapshot, the default RecipeResult, RecipeCriteria, and
	// BundleProvenance. It reached its §2 target in v0.22 (#2416).
	StableGroupVersion = GroupVersionV1

	// AuthoringGroupVersion is the value emitted for the ADR-022 authoring and
	// configuration track: AICRConfig, ordinary RecipeMetadata, RecipeMixin,
	// and ComponentRegistry. It reached its §2 target in v0.22 (#2416).
	AuthoringGroupVersion = GroupVersionV1Beta1

	// ProfileGroupVersion is the value emitted for the ADR-022 profile-bearing
	// track: profile RecipeMetadata and RecipeResult. It reached its §2 target
	// in v0.22 (#2416).
	ProfileGroupVersion = GroupVersionV1Beta2

	// GroupVersionV1Beta1 is the target authoring/configuration group/version.
	GroupVersionV1Beta1 = APIGroup + "/" + APIVersionV1Beta1

	// GroupVersionV1Beta2 is the target profile-bearing group/version.
	GroupVersionV1Beta2 = APIGroup + "/" + APIVersionV1Beta2

	// GroupVersionV1 is the target stable public artifact group/version.
	GroupVersionV1 = APIGroup + "/" + APIVersionV1
)

// Retired artifact API versions. ADR-022 §3 bound the alpha values to N+2,
// which is v1.0.0 (#2417); at that release they left every accepted set.
//
// They survive only so a rejection can name what the value was. An artifact
// written by an older aicr is a common and recoverable situation, and
// "unsupported apiVersion" alone does not tell its author that the value was
// withdrawn deliberately or which release withdrew it. Never add either to an
// accepted set — RetirementNote is their only legitimate consumer.
const (
	RetiredGroupVersionV1Alpha2 = APIGroup + "/v1alpha2"
	RetiredGroupVersionV1Alpha3 = APIGroup + "/v1alpha3"
)

// AlphaRemovedIn is the release that stopped reading the alpha apiVersion
// values and the legacy empty header.
const AlphaRemovedIn = "v1.0.0"

// RetirementNote returns a parenthetical explaining that an apiVersion was
// withdrawn, or "" for any other value — including one that is merely unknown,
// where there is nothing specific to say.
//
// Callers append it to a message that already names the observed value, the
// expected value, and how to regenerate the artifact. Splitting it out this way
// keeps each caller's message in its own vocabulary ("recapture the snapshot",
// "regenerate the criteria") while the reason an old artifact stopped working
// is worded once.
//
// An absent apiVersion gets no note here, because whether that value ever
// loaded is a property of the calling reader rather than of the value. Readers
// that did accept it call RetirementNoteWithAbsent instead.
func RetirementNote(apiVersion string) string {
	switch apiVersion {
	case RetiredGroupVersionV1Alpha2, RetiredGroupVersionV1Alpha3:
		return fmt.Sprintf(" (%s was retired in %s)", apiVersion, AlphaRemovedIn)
	default:
		return ""
	}
}

// RetirementNoteWithAbsent is RetirementNote for the five readers whose
// tolerance of an absent apiVersion survived until AlphaRemovedIn: the Snapshot,
// RecipeCriteria, and RecipeResult inputs whose artifacts predate the field.
//
// Every other reader must call RetirementNote. An AICRConfig never loaded
// without a header, and a RecipeMetadata overlay stopped in v0.21 with the
// catalog scanner (#2421) — telling either author the value "was accepted"
// until v1.0.0 sends them looking for a regression that never happened, which
// is worse than the bare "unsupported apiVersion" this clause exists to improve.
func RetirementNoteWithAbsent(apiVersion string) string {
	if apiVersion == "" {
		return fmt.Sprintf(" (an absent apiVersion was accepted before %s)", AlphaRemovedIn)
	}
	return RetirementNote(apiVersion)
}

// IsSupportedAPIVersion reports whether v is an artifact apiVersion this binary
// understands. The empty string is intentionally NOT supported here: callers
// that tolerate a missing apiVersion for backward compatibility with older
// artifacts must special-case "" before calling this.
//
// This compatibility helper covers the stable artifact track only. Callers
// reading authoring or profile-bearing artifacts must use the corresponding
// schema-track helper instead of treating versions as globally interchangeable.
func IsSupportedAPIVersion(v string) bool {
	return v == GroupVersionV1
}

// IsSupportedAuthoringAPIVersion reports whether v is accepted for an
// ADR-022 authoring/configuration artifact.
func IsSupportedAuthoringAPIVersion(v string) bool {
	return v == GroupVersionV1Beta1
}

// IsSupportedProfileAPIVersion reports whether v is accepted for a
// profile-bearing RecipeMetadata or RecipeResult.
func IsSupportedProfileAPIVersion(v string) bool {
	return v == GroupVersionV1Beta2
}

// IsSupportedBundleInfoAPIVersion reports whether v is accepted for a
// BundleInfo. Unlike the other stable-track artifacts, this kind shipped
// directly at its ADR-022 target with no alpha predecessor, so no BundleInfo
// carrying a retired value ever legitimately existed. Kept distinct from
// IsSupportedAPIVersion, which the two now agree with only by coincidence.
func IsSupportedBundleInfoAPIVersion(v string) bool {
	return v == StableGroupVersion
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
	KindBundleInfo   Kind = "BundleInfo"
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
