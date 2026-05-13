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
}

// BuildPredicate constructs the v1 predicate body from inputs. The
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
	}
}

// SubjectName returns the in-toto subject[0].name for a recipe.
func SubjectName(recipeName string) string {
	return SubjectNamePrefix + recipeName
}

// BuildStatement constructs the in-toto Statement carrying our v1
// predicate. The returned bytes are protobuf-canonical JSON suitable
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
		PredicateType: PredicateTypeV1,
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
		PredicateType: PredicateTypeV1,
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
