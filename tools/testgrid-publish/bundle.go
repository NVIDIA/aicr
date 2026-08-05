// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// readBoundedFile reads a file up to maxBytes. It returns ErrCodeInvalidRequest
// if the file exceeds the limit, preventing OOM on attacker-supplied bundles.
func readBoundedFile(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // callers validate path origin
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s exceeds %d-byte size limit", path, maxBytes))
	}
	return data, nil
}

// parseCriteria reads recipe.yaml from bundleDir and returns a
// RecipeCriteria suitable for CoordinateFor.
func parseCriteria(bundleDir string) (RecipeCriteria, error) {
	path := filepath.Join(bundleDir, attestation.RecipeFilename)
	data, err := readBoundedFile(path, defaults.MaxRecipePOSTBytes)
	if err != nil {
		if os.IsNotExist(err) {
			return RecipeCriteria{}, errors.Wrap(errors.ErrCodeNotFound,
				"recipe.yaml not found in bundle", err)
		}
		return RecipeCriteria{}, err // already structured (e.g. ErrCodeInvalidRequest for size limit)
	}

	r, err := recipe.DecodeRecipeResult(data, serializer.FormatYAML)
	if err != nil {
		return RecipeCriteria{}, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest,
			"failed to parse recipe.yaml")
	}
	if r.Criteria == nil {
		return RecipeCriteria{}, errors.New(errors.ErrCodeInvalidRequest,
			"recipe.yaml has no criteria")
	}

	// Normalize: trim whitespace and lowercase so "EKS" and " eks " both
	// map to the same GCS group path as the config-gen taxonomy expects.
	c := r.Criteria
	service := strings.ToLower(strings.TrimSpace(string(c.Service)))
	accelerator := strings.ToLower(strings.TrimSpace(string(c.Accelerator)))
	os_ := strings.ToLower(strings.TrimSpace(string(c.OS)))
	intent := strings.ToLower(strings.TrimSpace(string(c.Intent)))
	platform := strings.ToLower(strings.TrimSpace(string(c.Platform)))

	if service == "" || accelerator == "" || os_ == "" || intent == "" {
		return RecipeCriteria{}, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("recipe.yaml missing required criteria fields: service=%q accelerator=%q os=%q intent=%q",
				service, accelerator, os_, intent))
	}

	criteria := RecipeCriteria{
		Service:     service,
		Accelerator: accelerator,
		OS:          os_,
		Intent:      intent,
		Platform:    platform,
	}

	// A profiled recipe (metadata.selectedProfile — already contract-
	// validated by the strict DecodeRecipeResult above) records the shared
	// profile segment for consumers that need it; the TestGrid coordinate
	// itself deliberately stays unsuffixed (the digest-bound build ID
	// partitions per value — see RecipeCriteria.ProfileSegment).
	if r.Metadata.SelectedProfile != nil {
		segment, segErr := attestation.ProfileSegment(r.Metadata.SelectedProfile)
		if segErr != nil {
			return RecipeCriteria{}, segErr
		}
		criteria.ProfileSegment = segment
	}

	// Extract k8s constraint from recipe constraints list.
	for _, con := range r.Constraints {
		if con.Name == "K8s.server.version" {
			criteria.K8sConstraint = con.Value
			break
		}
	}

	return criteria, nil
}

// loadPredicate reads the in-toto Statement from the bundle and returns
// the decoded Predicate. Returns an error when the file is absent or
// malformed — callers should fall back to sensible defaults.
func loadPredicate(bundleDir string) (*attestation.Predicate, error) {
	path := filepath.Join(bundleDir, attestation.StatementFilename)
	data, err := readBoundedFile(path, defaults.MaxBundlePOSTBytes)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.Wrap(errors.ErrCodeNotFound, "statement.intoto.json not found", err)
		}
		return nil, err
	}

	// The statement is a JSON object with predicateType and predicate fields.
	var stmt struct {
		PredicateType string          `json:"predicateType"`
		Predicate     json.RawMessage `json:"predicate"`
	}
	if err := json.Unmarshal(data, &stmt); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"failed to parse statement.intoto.json", err)
	}
	if stmt.Predicate == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "statement has no predicate field")
	}

	var pred attestation.Predicate
	if err := json.Unmarshal(stmt.Predicate, &pred); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"failed to decode predicate", err)
	}
	// The shared bidirectional contract, not just a type allowlist: v1 must
	// not carry a profile block, v2 must carry a well-formed one, anything
	// else (including an absent predicateType) is unknown. Without it a v1
	// statement smuggling a profile block — evidence every other consumer
	// rejects — would publish to TestGrid.
	if err := attestation.ValidatePredicateTypeCoherence(stmt.PredicateType, &pred); err != nil {
		return nil, err
	}
	return &pred, nil
}
