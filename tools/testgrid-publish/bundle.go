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

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
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

// recipeFile is the minimal subset of recipe.yaml we need for coordinate
// resolution. Field names match the on-disk YAML schema.
type recipeFile struct {
	Criteria struct {
		Service     string `yaml:"service"`
		Accelerator string `yaml:"accelerator"`
		OS          string `yaml:"os"`
		Intent      string `yaml:"intent"`
		Platform    string `yaml:"platform"`
	} `yaml:"criteria"`
	// k8s constraint lives outside criteria in the recipe spec.
	Constraints []struct {
		Name  string `yaml:"name"`
		Value string `yaml:"value"`
	} `yaml:"constraints"`
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

	var r recipeFile
	if err := yaml.Unmarshal(data, &r); err != nil {
		return RecipeCriteria{}, errors.Wrap(errors.ErrCodeInvalidRequest,
			"failed to parse recipe.yaml", err)
	}

	// Normalize: trim whitespace and lowercase so "EKS" and " eks " both
	// map to the same GCS group path as the config-gen taxonomy expects.
	c := r.Criteria
	service := strings.ToLower(strings.TrimSpace(c.Service))
	accelerator := strings.ToLower(strings.TrimSpace(c.Accelerator))
	os_ := strings.ToLower(strings.TrimSpace(c.OS))
	intent := strings.ToLower(strings.TrimSpace(c.Intent))
	platform := strings.ToLower(strings.TrimSpace(c.Platform))

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
	if stmt.PredicateType != "" && stmt.PredicateType != attestation.PredicateTypeV1 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported predicateType %q, expected %q",
				stmt.PredicateType, attestation.PredicateTypeV1))
	}
	if stmt.Predicate == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "statement has no predicate field")
	}

	var pred attestation.Predicate
	if err := json.Unmarshal(stmt.Predicate, &pred); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"failed to decode predicate", err)
	}
	return &pred, nil
}
