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
	"encoding/json"
	"sort"
	"time"

	intoto "github.com/in-toto/attestation/go/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/fingerprint"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// PredicateInputs is the data BuildPredicate needs.
type PredicateInputs struct {
	AttestedAt              time.Time
	AICRVersion             string
	ValidatorCatalogVersion string
	ValidatorImages         []ValidatorImage
	Recipe                  RecipeRef
	Fingerprint             fingerprint.Fingerprint
	CriteriaMatch           fingerprint.MatchResult
	Phases                  map[Phase]PhaseSummary
	BOM                     BOMRef
	Manifest                ManifestRef

	// Redaction is nil for full bundles and set for minimal bundles.
	Redaction *RedactionInfo

	// Profile is nil for unprofiled recipes and set for profile-bearing
	// ones.
	Profile *ProfilePredicate
}

// BuildPredicate constructs the predicate body from in. The result matches
// the shape PredicateTypeV3 requires (see StatementPredicateType) whether
// or not the recipe is profile-bearing. ValidatorImages is sorted by image
// for deterministic field ordering.
func BuildPredicate(in PredicateInputs) *Predicate {
	images := append([]ValidatorImage(nil), in.ValidatorImages...)
	sort.Slice(images, func(i, j int) bool {
		return images[i].Image < images[j].Image
	})

	// Filters to the canonical phase set. Any key in in.Phases outside
	// AllPhases is silently dropped.
	phases := map[Phase]PhaseSummary{}
	for _, p := range AllPhases {
		if v, ok := in.Phases[p]; ok {
			phases[p] = v
		}
	}

	return &Predicate{
		SchemaVersion:           PredicateSchemaVersion,
		AttestedAt:              in.AttestedAt.UTC().Truncate(time.Second),
		AICRVersion:             in.AICRVersion,
		ValidatorCatalogVersion: in.ValidatorCatalogVersion,
		ValidatorImages:         images,
		Recipe:                  in.Recipe,
		Fingerprint:             in.Fingerprint,
		CriteriaMatch:           in.CriteriaMatch,
		Phases:                  phases,
		BOM:                     in.BOM,
		Manifest:                in.Manifest,
		Redaction:               in.Redaction,
		Profile:                 in.Profile,
	}
}

// StatementPredicateType returns the predicate type for newly produced
// evidence, always PredicateTypeV3. PredicateTypeV1 and V2 are assigned
// only to historic evidence already on disk.
func StatementPredicateType(_ *Predicate) string {
	return PredicateTypeV3
}

// ValidatePredicateTypeCoherence enforces the type contract shared by every
// evidence consumer. v1 must not carry a profile block, and v2 must carry a
// well-formed one. A profiled recipe attested under v1 would silently lose
// its descriptor identity, exactly the pre-expansion evidence the v2
// cut-over exists to invalidate. v3 allows either. Its profile block's
// presence reflects whether the recipe carries metadata.selectedProfile,
// not the predicate type, so a present block is validated the same way v2
// validates one, but is never required. Any other predicateType is
// rejected as unknown.
func ValidatePredicateTypeCoherence(predicateType string, pred *Predicate) error {
	switch predicateType {
	case PredicateTypeV1:
		if pred != nil && pred.Profile != nil {
			return errors.New(errors.ErrCodeInvalidRequest,
				"predicateType "+PredicateTypeV1+" cannot carry a profile block; profile-bearing evidence requires "+PredicateTypeV2)
		}
		return nil
	case PredicateTypeV2:
		if pred == nil || pred.Profile == nil {
			return errors.New(errors.ErrCodeInvalidRequest,
				"predicateType "+PredicateTypeV2+" requires the predicate profile block")
		}
		return validateProfileBlock(pred.Profile)
	case PredicateTypeV3:
		if pred != nil && pred.Profile != nil {
			return validateProfileBlock(pred.Profile)
		}
		return nil
	default:
		return errors.New(errors.ErrCodeInvalidRequest,
			"unexpected predicateType "+predicateType)
	}
}

// validateProfileBlock checks that p carries a non-empty Selection
// matching the recipe profile grammar and a non-empty
// PolicyDescriptorIdentity.
func validateProfileBlock(p *ProfilePredicate) error {
	if p.Selection == "" || p.PolicyDescriptorIdentity == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			"predicate profile block requires selection and policyDescriptorIdentity")
	}
	if _, err := recipe.ParseProfileSelection(p.Selection); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest,
			"predicate profile block carries a malformed selection", err)
	}
	return nil
}

// SubjectName returns the in-toto subject[0].name for a recipe.
func SubjectName(recipeName string) string {
	return SubjectNamePrefix + recipeName
}

// BuildStatement constructs the in-toto Statement carrying our
// recipe-evidence predicate, typed via StatementPredicateType (always
// PredicateTypeV3 for a newly built predicate). The returned bytes are
// protobuf-canonical JSON suitable
// for DSSE wrapping. The recipe canonicalization happens upstream;
// callers pass in the already-computed subject digest.
func BuildStatement(recipeName, recipeSubjectDigest string, pred *Predicate) ([]byte, error) {
	if recipeName == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "recipe name is required")
	}
	if recipeSubjectDigest == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "recipe subject digest is required")
	}
	if len(recipeSubjectDigest) != 64 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "recipe subject digest must be 64 hex characters")
	}
	if pred == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "predicate is required")
	}
	// Producer-side coherence: StatementPredicateType keys off profile
	// presence, so type-vs-profile agreement is structural — but a
	// hand-built v2 profile block missing selection/policyDescriptorIdentity
	// would sign fine and only fail at verify/ingest. Fail closed here.
	if err := ValidatePredicateTypeCoherence(StatementPredicateType(pred), pred); err != nil {
		return nil, err
	}

	predicate, err := predicateAsStruct(pred)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to convert predicate to struct", err)
	}

	stmt := &intoto.Statement{
		Type: intoto.StatementTypeUri,
		Subject: []*intoto.ResourceDescriptor{
			{
				Name:   SubjectName(recipeName),
				Digest: map[string]string{"sha256": recipeSubjectDigest},
			},
		},
		PredicateType: StatementPredicateType(pred),
		Predicate:     predicate,
	}
	if vErr := stmt.Validate(); vErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "in-toto statement failed validation", vErr)
	}

	out, err := protojson.Marshal(stmt)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to marshal in-toto statement", err)
	}
	return out, nil
}

// BuildArtifactStatement constructs an in-toto Statement whose subject is
// an OCI artifact (ociRef + artifactDigest). cosign's Referrers-API
// discovery anchors on the artifact digest, so the signed subject must
// match. Recipe identity is preserved via predicate.recipe.{name,digest},
// which BuildArtifactStatement requires to be populated.
//
// predicateType is taken from the caller rather than derived via
// StatementPredicateType. A freshly built bundle types it V3, but a bundle
// reconstructed from an on-disk statement (Publish, SignExisting) must
// re-sign under the type it was ORIGINALLY built with, because
// pred.Recipe.Digest was computed with that type's canonicalization
// algorithm. Stamping every reconstructed bundle V3 here would sign a V3
// statement around a legacy digest, which the verifier's
// SubjectDigestForType then recomputes under V3 canonicalization and
// rejects as a mismatch.
func BuildArtifactStatement(ociRef, artifactDigest, predicateType string, pred *Predicate) ([]byte, error) {
	if ociRef == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "OCI reference is required")
	}
	if artifactDigest == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "artifact digest is required")
	}
	if len(artifactDigest) != 64 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "artifact digest must be 64 hex characters")
	}
	if pred == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "predicate is required")
	}
	if pred.Recipe.Name == "" || pred.Recipe.Digest == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "predicate.recipe.{name,digest} must be populated for artifact-subject statement")
	}
	if predicateType == "" {
		predicateType = StatementPredicateType(pred)
	}
	// Producer-side coherence — same rationale as BuildStatement.
	if err := ValidatePredicateTypeCoherence(predicateType, pred); err != nil {
		return nil, err
	}

	predicate, err := predicateAsStruct(pred)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to convert predicate to struct", err)
	}

	stmt := &intoto.Statement{
		Type: intoto.StatementTypeUri,
		Subject: []*intoto.ResourceDescriptor{
			{
				Name:   ociRef,
				Digest: map[string]string{"sha256": artifactDigest},
			},
		},
		PredicateType: predicateType,
		Predicate:     predicate,
	}
	if vErr := stmt.Validate(); vErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "in-toto artifact statement failed validation", vErr)
	}

	out, err := protojson.Marshal(stmt)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to marshal in-toto artifact statement", err)
	}
	return out, nil
}

// predicateAsStruct serializes the Predicate via JSON (the on-the-wire
// shape) and re-parses it as a structpb.Struct so it can be embedded
// in the in-toto Statement protobuf. Going through JSON guarantees
// the shape on disk and the shape inside the Statement match.
func predicateAsStruct(pred *Predicate) (*structpb.Struct, error) {
	body, err := json.Marshal(pred)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to marshal predicate", err)
	}
	s := &structpb.Struct{}
	if err := protojson.Unmarshal(body, s); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "predicate is not valid struct JSON", err)
	}
	return s, nil
}
