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
	// ones; it selects PredicateTypeV2 for the enclosing statement.
	Profile *ProfilePredicate
}

// BuildPredicate constructs the predicate body from inputs (the shared
// shape behind predicateType v1 for unprofiled recipes and v2 when
// Profile is set — see StatementPredicateType). The
// returned Predicate has deterministic field ordering: ValidatorImages
// is sorted by image, Phases iteration order is the canonical
// AllPhases sequence (the map is fine because Go's JSON marshaller
// sorts map keys).
func BuildPredicate(in PredicateInputs) *Predicate {
	images := append([]ValidatorImage(nil), in.ValidatorImages...)
	sort.Slice(images, func(i, j int) bool {
		return images[i].Image < images[j].Image
	})

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

// StatementPredicateType returns the predicate type a predicate requires:
// PredicateTypeV2 when it carries a profile block, PredicateTypeV1
// otherwise.
func StatementPredicateType(pred *Predicate) string {
	if pred != nil && pred.Profile != nil {
		return PredicateTypeV2
	}
	return PredicateTypeV1
}

// ValidatePredicateTypeCoherence enforces the bidirectional type contract
// shared by every evidence consumer: v1 must not carry a profile block, v2
// must carry a well-formed one, and any other type is unknown. A profiled
// recipe attested under v1 would silently lose its descriptor identity —
// exactly the pre-expansion evidence the v2 cut-over exists to invalidate.
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
		if pred.Profile.Selection == "" || pred.Profile.PolicyDescriptorIdentity == "" {
			return errors.New(errors.ErrCodeInvalidRequest,
				"predicate profile block requires selection and policyDescriptorIdentity")
		}
		if _, err := recipe.ParseProfileSelection(pred.Profile.Selection); err != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				"predicate profile block carries a malformed selection", err)
		}
		return nil
	default:
		return errors.New(errors.ErrCodeInvalidRequest,
			"unexpected predicateType "+predicateType)
	}
}

// SubjectName returns the in-toto subject[0].name for a recipe.
func SubjectName(recipeName string) string {
	return SubjectNamePrefix + recipeName
}

// BuildStatement constructs the in-toto Statement carrying our
// recipe-evidence predicate, typed via StatementPredicateType (v1 for
// unprofiled recipes, v2 when the predicate carries a profile block).
// The returned bytes are protobuf-canonical JSON suitable
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
func BuildArtifactStatement(ociRef, artifactDigest string, pred *Predicate) ([]byte, error) {
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
	// Producer-side coherence — same rationale as BuildStatement.
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
				Name:   ociRef,
				Digest: map[string]string{"sha256": artifactDigest},
			},
		},
		PredicateType: StatementPredicateType(pred),
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
