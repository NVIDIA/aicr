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

// Package index is the build-time inventory of committed recipe-evidence
// pointers. The binary embeds pointer presence (recipe id, source slug, and
// signer class). It does not embed pointer files, verify signatures, or
// contact a registry.
package index

import (
	_ "embed"
	"fmt"
	"os"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// EnvVar is the process-wide opt-in. A value of 1, true, yes, or on enables
// the check. Any other value, including empty, leaves it off.
const EnvVar = "AICR_REQUIRE_EVIDENCE"

// HealthDocURL is the recipe-health page named in a rejection. The page's
// Evidence column is dashboard presence, which is a different signal from
// the committed pointers this package records.
const HealthDocURL = "https://github.com/NVIDIA/aicr/blob/main/docs/user/recipe-health.md"

//go:embed pointers.yaml
var pointersYAML []byte

// CatalogSource is one overlay's catalog name and provenance. Source is
// recipe.CatalogSourceEmbedded for a leaf shipped in the binary.
type CatalogSource struct {
	Name   string
	Source string
}

type indexDocument struct {
	SchemaVersion int              `yaml:"schemaVersion"`
	Recipes       []recipePointers `yaml:"recipes"`
}

type recipePointers struct {
	ID      string          `yaml:"id"`
	Sources []sourcePointer `yaml:"sources"`
}

type sourcePointer struct {
	Slug  string `yaml:"slug"`
	Class string `yaml:"class"`
}

var (
	loadOnce sync.Once
	loaded   map[string][]sourcePointer
	loadErr  error
)

// EnabledFromEnv reports whether EnvVar is set to a recognized truthy value.
func EnabledFromEnv() bool {
	enabled, ok := ParseToggle(os.Getenv(EnvVar))
	return ok && enabled
}

// ParseToggle classifies a toggle spelling.
//
// Recognized truthy values are 1, true, yes, and on. Recognized falsy values
// are 0, false, no, and off. Matching is case-insensitive and ignores
// surrounding space. An empty string is falsy and ok, so a missing value
// stays distinct from a typo. ok is false only for a non-empty unrecognized
// value.
func ParseToggle(raw string) (enabled bool, ok bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "0", "false", "no", "off":
		return false, true
	case "1", "true", "yes", "on":
		return true, true
	default:
		return false, false
	}
}

// LeafSource returns the provenance of the last applied overlay. An overlay
// the catalog does not name returns an empty source so the check fails closed.
func LeafSource(applied []string, entries []CatalogSource) string {
	if len(applied) == 0 {
		return ""
	}
	leaf := applied[len(applied)-1]
	for _, entry := range entries {
		if entry.Name == leaf {
			return entry.Source
		}
	}
	return ""
}

// ExternalSource rejects result because it was resolved against a recipe
// source other than the embedded catalog. A committed pointer attests that
// catalog only, so FilesystemSource and OCISource fail even when the leaf
// overlay is still the embedded one. That covers a --data directory that
// replaces a parent overlay, a values file, or registry.yaml.
func ExternalSource(result *recipe.RecipeResult) error {
	if result == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "resolved recipe is required")
	}
	return missingEvidence(attestation.RecipeNameFor(result), "resolved with an external data source")
}

// Check rejects a resolved recipe that has no committed evidence pointer.
//
// The recipe id is attestation.RecipeNameFor, so a profiled resolve carries
// the selected value and a pointer on the unprofiled id does not cover it.
// leafSource must be recipe.CatalogSourceEmbedded. A leaf loaded from --data
// fails even when its id matches a committed pointer, because that pointer
// attests the embedded recipe. Callers that can see the client source call
// ExternalSource first: Check only sees the leaf overlay, and a --data
// change to anything else still reports an embedded leaf. The check reads
// the embedded index only.
func Check(result *recipe.RecipeResult, leafSource string) error {
	if result == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "resolved recipe is required")
	}
	id := attestation.RecipeNameFor(result)
	if leafSource != recipe.CatalogSourceEmbedded {
		return missingEvidence(id, "resolved leaf is not the embedded catalog recipe")
	}
	pointers, err := pointersFor(id)
	if err != nil {
		return err
	}
	if len(pointers) == 0 {
		return missingEvidence(id, "")
	}
	return nil
}

func missingEvidence(id, why string) error {
	if id == "" {
		id = "resolved recipe"
	}
	msg := fmt.Sprintf("recipe %q has no published evidence", id)
	if why != "" {
		msg += " (" + why + ")"
	}
	return errors.New(errors.ErrCodeNotFound, msg+": "+HealthDocURL)
}

func pointersFor(id string) ([]sourcePointer, error) {
	doc, err := document()
	if err != nil {
		return nil, err
	}
	return doc[id], nil
}

func document() (map[string][]sourcePointer, error) {
	loadOnce.Do(func() {
		loaded, loadErr = decodePointers(pointersYAML)
	})
	if loadErr != nil {
		return nil, loadErr
	}
	return loaded, nil
}

func decodePointers(raw []byte) (map[string][]sourcePointer, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	var doc indexDocument
	if err := dec.Decode(&doc); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "evidence pointer index is not valid YAML", err)
	}
	if doc.SchemaVersion != 1 {
		return nil, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("evidence pointer index schemaVersion %d is unsupported", doc.SchemaVersion))
	}
	out := make(map[string][]sourcePointer, len(doc.Recipes))
	for _, recipeEntry := range doc.Recipes {
		if recipeEntry.ID == "" {
			return nil, errors.New(errors.ErrCodeInternal, "evidence pointer index entry is missing an id")
		}
		if _, exists := out[recipeEntry.ID]; exists {
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("evidence pointer index lists recipe %q more than once", recipeEntry.ID))
		}
		if len(recipeEntry.Sources) == 0 {
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("evidence pointer index lists recipe %q with no sources", recipeEntry.ID))
		}
		out[recipeEntry.ID] = recipeEntry.Sources
	}
	return out, nil
}
