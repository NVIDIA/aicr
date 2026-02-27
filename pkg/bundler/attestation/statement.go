// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"fmt"
	"time"

	intoto "github.com/in-toto/attestation/go/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/google/uuid"
)

// SLSA and in-toto constants.
const (
	SLSAProvenanceType = "https://slsa.dev/provenance/v1"
	BundleBuildType    = "https://aicr.nvidia.com/bundle/v1"
)

// StatementMetadata provides build context for the SLSA predicate.
type StatementMetadata struct {
	// Recipe name that produced this bundle.
	Recipe string

	// RecipeSource indicates where the recipe came from ("embedded" or "external").
	RecipeSource string

	// Components lists the component names in the bundle.
	Components []string

	// OutputDir is the bundle output directory.
	OutputDir string

	// BuilderID identifies who created this bundle (e.g., OIDC email or workflow URI).
	BuilderID string

	// ToolVersion is the aicr version that produced this bundle (e.g., "v1.0.0").
	ToolVersion string
}

// BuildStatement constructs an in-toto Statement v1 with a SLSA Build Provenance v1
// predicate. Returns the statement as serialized JSON.
func BuildStatement(subject AttestSubject, metadata StatementMetadata) ([]byte, error) {
	if subject.Name == "" || len(subject.Digest) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "subject name and digest are required")
	}
	for algo, value := range subject.Digest {
		if value == "" {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				"empty digest value for algorithm "+algo)
		}
		if algo == "sha256" && len(value) != 64 {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("sha256 digest must be 64 hex characters, got %d", len(value)))
		}
	}

	// Build SLSA predicate as a structpb.Struct
	predicate, err := buildPredicate(subject, metadata)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to build SLSA predicate", err)
	}

	// Construct the in-toto Statement using the official types
	stmt := &intoto.Statement{
		Type: intoto.StatementTypeUri,
		Subject: []*intoto.ResourceDescriptor{
			{
				Name:   subject.Name,
				Digest: subject.Digest,
			},
		},
		PredicateType: SLSAProvenanceType,
		Predicate:     predicate,
	}

	// Validate the statement against the in-toto spec
	if err := stmt.Validate(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "invalid in-toto statement", err)
	}

	return protojson.Marshal(stmt)
}

// buildPredicate constructs the SLSA Build Provenance v1 predicate.
func buildPredicate(subject AttestSubject, metadata StatementMetadata) (*structpb.Struct, error) {
	// Build resolvedDependencies as a list of maps
	// Convert map[string]string to map[string]any for structpb compatibility
	deps := make([]any, 0, len(subject.ResolvedDependencies))
	for _, dep := range subject.ResolvedDependencies {
		digestAny := make(map[string]any, len(dep.Digest))
		for k, v := range dep.Digest {
			digestAny[k] = v
		}
		deps = append(deps, map[string]any{
			"uri":    dep.URI,
			"digest": digestAny,
		})
	}

	// Build components list as []any for structpb compatibility
	components := make([]any, 0, len(metadata.Components))
	for _, c := range metadata.Components {
		components = append(components, c)
	}

	predicateMap := map[string]any{
		"buildDefinition": map[string]any{
			"buildType": BundleBuildType,
			"externalParameters": map[string]any{
				"recipe":       metadata.Recipe,
				"recipeSource": metadata.RecipeSource,
			},
			"internalParameters": map[string]any{
				"components":  components,
				"outputDir":   metadata.OutputDir,
				"toolVersion": metadata.ToolVersion,
			},
			"resolvedDependencies": deps,
		},
		"runDetails": map[string]any{
			"builder": map[string]any{
				"id": metadata.BuilderID,
			},
			"metadata": map[string]any{
				"invocationId": uuid.New().String(),
				"startedOn":    time.Now().UTC().Format(time.RFC3339),
			},
		},
	}

	return structpb.NewStruct(predicateMap)
}
