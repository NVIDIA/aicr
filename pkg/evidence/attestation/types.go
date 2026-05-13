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

package attestation

import (
	"time"

	"github.com/NVIDIA/aicr/pkg/fingerprint"
)

// Public stability constants. These match the V1 schema documented in
// ADR-007 (docs/design/007-recipe-evidence.md) and are part of the V1
// stability boundary.
const (
	// PredicateTypeV1 is the in-toto predicateType URI for recipe evidence.
	PredicateTypeV1 = "https://aicr.nvidia.com/recipe-evidence/v1"

	// PredicateSchemaVersion is the recipe-evidence predicate schema version.
	PredicateSchemaVersion = "1.0.0"

	// PointerSchemaVersion is the on-disk pointer file schema version.
	PointerSchemaVersion = "1.0.0"

	// ManifestSchemaVersion is the manifest.json schema version.
	ManifestSchemaVersion = "1.0.0"

	// BOMFormat is the BOM format we emit. CycloneDX is the only V1 option.
	BOMFormat = "CycloneDX"

	// SubjectNamePrefix is prepended to the recipe name in subject[0].name.
	SubjectNamePrefix = "recipe:"

	// AttestationFilename is the bundle-relative path of the signed
	// in-toto Statement (DSSE-wrapped, cosign keyless).
	AttestationFilename = "attestation.intoto.jsonl"

	// StatementFilename is the bundle-relative path of the unsigned
	// in-toto Statement. Always written by Build so the bundle
	// directory is self-contained and can be signed later (via cosign
	// or another DSSE signer) without re-running aicr validate.
	StatementFilename = "statement.intoto.json"

	// ManifestFilename is the bundle-relative path of the integrity inventory.
	ManifestFilename = "manifest.json"

	// RecipeFilename is the bundle-relative path of the canonical recipe YAML.
	RecipeFilename = "recipe.yaml"

	// SnapshotFilename is the bundle-relative path of the validate-time snapshot.
	SnapshotFilename = "snapshot.yaml"

	// BOMFilename is the bundle-relative path of the CycloneDX BOM.
	BOMFilename = "bom.cdx.json"

	// SummaryBundleDirName is the local-output directory for the summary bundle.
	SummaryBundleDirName = "summary-bundle"

	// LogsBundleDirName is the local-output directory for the optional logs bundle.
	LogsBundleDirName = "logs-bundle"

	// PointerFilename is the local-output filename for the pointer YAML.
	PointerFilename = "pointer.yaml"

	// ctrfDirName is the bundle subdirectory that holds per-phase CTRF reports.
	ctrfDirName = "ctrf"

	// phasesDirName is the logs-bundle subdirectory grouping per-phase logs.
	phasesDirName = "phases"

	// logsDirName is the inner directory under each phase that holds log files.
	logsDirName = "logs"

	// DefaultCycloneDXVersion is the BOM spec version we record when callers
	// don't override it. Mirrors what `make bom` produces today.
	DefaultCycloneDXVersion = "1.6"

	// defaultEvidenceTag is the OCI tag used when the operator gives a
	// reference without one. The OCI digest is the canonical address;
	// the tag exists to satisfy registries that require one.
	defaultEvidenceTag = "v1"
)

// Phase enumerates the validation phases that may appear in the bundle.
// String values must match the on-disk filenames under ctrf/<phase>.json
// and the predicate's phases.<key> map keys.
type Phase string

// Validation phases recorded in the predicate.
const (
	PhaseDeployment  Phase = "deployment"
	PhasePerformance Phase = "performance"
	PhaseConformance Phase = "conformance"
)

// AllPhases lists every phase in the canonical order. Used by manifest
// builders to iterate deterministically.
var AllPhases = []Phase{PhaseDeployment, PhasePerformance, PhaseConformance}

// Predicate is the body of the signed in-toto Statement. It serializes
// to JSON for the on-the-wire predicate and to YAML for human-readable
// embedding in spec docs.
//
// Fingerprint and CriteriaMatch are pkg/fingerprint types used
// directly so the predicate-v1 schema stays exactly aligned with what
// fingerprint.FromMeasurements and Fingerprint.Match produce.
type Predicate struct {
	SchemaVersion           string                  `json:"schemaVersion" yaml:"schemaVersion"`
	AttestedAt              time.Time               `json:"attestedAt" yaml:"attestedAt"`
	AICRVersion             string                  `json:"aicrVersion" yaml:"aicrVersion"`
	ValidatorCatalogVersion string                  `json:"validatorCatalogVersion" yaml:"validatorCatalogVersion"`
	ValidatorImages         []ValidatorImage        `json:"validatorImages" yaml:"validatorImages"`
	Fingerprint             fingerprint.Fingerprint `json:"fingerprint" yaml:"fingerprint"`
	CriteriaMatch           fingerprint.MatchResult `json:"criteriaMatch" yaml:"criteriaMatch"`
	Phases                  map[Phase]PhaseSummary  `json:"phases" yaml:"phases"`
	BOM                     BOMRef                  `json:"bom" yaml:"bom"`
	Manifest                ManifestRef             `json:"manifest" yaml:"manifest"`
}

// ValidatorImage records one validator image that ran during the
// validate session. The list is sorted by Image for determinism.
type ValidatorImage struct {
	Image  string `json:"image" yaml:"image"`
	Digest string `json:"digest" yaml:"digest"`
}

// PhaseSummary is the per-phase outcome recorded in the predicate.
// CTRFDigest is the sha256 of the corresponding ctrf/<phase>.json file
// in the bundle, providing a hash-based binding from the signed
// predicate to the unsigned but pre-committed phase report.
type PhaseSummary struct {
	Passed     int    `json:"passed" yaml:"passed"`
	Failed     int    `json:"failed" yaml:"failed"`
	Skipped    int    `json:"skipped" yaml:"skipped"`
	CTRFDigest string `json:"ctrfDigest" yaml:"ctrfDigest"`
}

// BOMRef records the CycloneDX BOM bundled alongside the predicate.
type BOMRef struct {
	Format     string `json:"format" yaml:"format"`
	Version    string `json:"version" yaml:"version"`
	Digest     string `json:"digest" yaml:"digest"`
	ImageCount int    `json:"imageCount" yaml:"imageCount"`
}

// ManifestRef binds the bundle's manifest.json to the signed predicate.
// The manifest itself enumerates per-file sha256 hashes; this digest
// closes the integrity chain so any other file in the bundle is bound
// to the signature transitively.
type ManifestRef struct {
	Digest    string `json:"digest" yaml:"digest"`
	FileCount int    `json:"fileCount" yaml:"fileCount"`
}

// Manifest is the bundle integrity inventory. Serialized as
// manifest.json. The order of Files is sorted by Path for determinism.
type Manifest struct {
	SchemaVersion string         `json:"schemaVersion"`
	Files         []ManifestFile `json:"files"`
}

// ManifestFile describes a single file in the bundle. Path is
// bundle-relative and slash-separated.
type ManifestFile struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"mediaType,omitempty"`
}

// Pointer is the on-disk file at recipes/evidence/<recipe>.yaml that
// binds the repo to the OCI bundle by content hash.
type Pointer struct {
	SchemaVersion string               `json:"schemaVersion" yaml:"schemaVersion"`
	Recipe        string               `json:"recipe" yaml:"recipe"`
	Attestations  []PointerAttestation `json:"attestations" yaml:"attestations"`
}

// PointerAttestation is one entry in the pointer's attestations list.
// V1 always emits exactly one entry; schema 2.0 will support multiple.
type PointerAttestation struct {
	Bundle PointerBundle `json:"bundle" yaml:"bundle"`

	// Signer is nil for unsigned bundles. When set, Identity and Issuer
	// are always populated; RekorLogIndex remains nil when the bundle
	// was signed with --no-rekor so a consumer can distinguish "no
	// Rekor entry" from "Rekor entry at index 0" (which is a legitimate
	// log position the first-ever entry occupies).
	Signer *PointerSigner `json:"signer,omitempty" yaml:"signer,omitempty"`

	AttestedAt    time.Time                  `json:"attestedAt" yaml:"attestedAt"`
	Fingerprint   PointerFingerprint         `json:"fingerprint" yaml:"fingerprint"`
	CriteriaMatch PointerCriteriaMatch       `json:"criteriaMatch" yaml:"criteriaMatch"`
	PhaseSummary  map[Phase]PointerPhaseStat `json:"phaseSummary" yaml:"phaseSummary"`
	LogsBundle    *PointerLogsBundle         `json:"logsBundle,omitempty" yaml:"logsBundle,omitempty"`
}

// PointerBundle references the OCI artifact carrying the signed bundle.
// OCI and Digest are empty strings when the bundle has been emitted
// locally but not yet pushed.
type PointerBundle struct {
	OCI           string `json:"oci" yaml:"oci"`
	Digest        string `json:"digest" yaml:"digest"`
	PredicateType string `json:"predicateType" yaml:"predicateType"`
}

// PointerSigner records OIDC identity claims for the attestation
// signer. Present only on signed bundles (see PointerAttestation.Signer
// for the unsigned case). RekorLogIndex is nil when no Rekor entry was
// created (e.g., signed with --no-rekor); a nil pointer distinguishes
// that case from a legitimate Rekor index 0 — the first-ever Rekor
// entry occupies that position.
type PointerSigner struct {
	Identity      string `json:"identity" yaml:"identity"`
	Issuer        string `json:"issuer" yaml:"issuer"`
	RekorLogIndex *int64 `json:"rekorLogIndex,omitempty" yaml:"rekorLogIndex,omitempty"`
}

// PointerFingerprint is a denormalized fingerprint for fast pointer
// reads. The bundle's predicate is authoritative; this is a cache.
// Mirrors the dimensions fingerprint.Fingerprint captures (intent and
// platform are recipe-author choices, not cluster facts; they surface
// in CriteriaMatch instead).
type PointerFingerprint struct {
	Service     string `json:"service,omitempty" yaml:"service,omitempty"`
	Accelerator string `json:"accelerator,omitempty" yaml:"accelerator,omitempty"`
	OS          string `json:"os,omitempty" yaml:"os,omitempty"`
	K8sVersion  string `json:"k8sVersion,omitempty" yaml:"k8sVersion,omitempty"`
	Region      string `json:"region,omitempty" yaml:"region,omitempty"`
}

// PointerCriteriaMatch is the denormalized matched/unmatched flag for
// PR sidebar display.
type PointerCriteriaMatch struct {
	Matched bool `json:"matched" yaml:"matched"`
}

// PointerPhaseStat is the denormalized per-phase pass/fail counts for
// the pointer. Skipped/error counts live in the bundle's CTRF.
type PointerPhaseStat struct {
	Passed int `json:"passed" yaml:"passed"`
	Failed int `json:"failed" yaml:"failed"`
}

// PointerLogsBundle is present in the pointer when the contributor
// pushed the optional logs bundle as a separate OCI artifact.
type PointerLogsBundle struct {
	OCI    string `json:"oci" yaml:"oci"`
	Digest string `json:"digest" yaml:"digest"`
}
