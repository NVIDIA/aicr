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

// Command schemagen writes JSON Schema documents for AICR's published artifacts.
//
// Artifact schemas are one of the four surfaces ROADMAP section 1 freezes at v1
// (issue #2113). The committed output serves two audiences that would otherwise
// disagree: integrators validating catalogs and snapshots they author, and the
// merge gate diffing the contract for breaking changes.
//
// Run via `make schemas`. TestCommittedSchemasAreFresh fails when the committed
// files drift from the Go types, so the two cannot silently diverge.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/schema"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
)

// SchemaBaseURI is the canonical location the generated documents claim.
const SchemaBaseURI = "https://aicr.run/schemas/v1"

// Artifact is one covered artifact kind.
type Artifact struct {
	// Kind is the artifact's `kind` value and the schema file's base name.
	Kind string

	// Type is the Go type that defines the artifact's wire shape.
	Type reflect.Type

	// Description tells an integrator what the artifact is for, since the
	// schema file is read without the Go source at hand.
	Description string

	// Authored marks an artifact a human writes (a catalog overlay) rather than
	// one AICR emits. See schema.Options.Authored.
	Authored bool
}

// Artifacts is the covered set, matching the kinds named in issue #2113.
//
// Recipe is deliberately absent: it is not a distinct wire type. What callers
// post and receive is RecipeResult, and adding an alias schema would publish a
// contract nothing emits.
func Artifacts() []Artifact {
	return []Artifact{
		{
			Kind: "Snapshot",
			Type: reflect.TypeOf(snapshotter.Snapshot{}),
			Description: "Captured cluster state: the measurements collected from a " +
				"live cluster, plus an advisory fingerprint. Consumers that bear " +
				"trust must recompute the fingerprint from measurements rather " +
				"than read the embedded value.",
		},
		{
			Kind: "RecipeResult",
			Type: reflect.TypeOf(recipe.RecipeResult{}),
			Description: "A fully resolved recipe: merged constraints, component " +
				"references and deployment order. This is the artifact the REST " +
				"API returns and the bundler consumes.",
		},
		{
			Kind:     "RecipeMetadata",
			Authored: true,
			Type:     reflect.TypeOf(recipe.RecipeMetadata{}),
			Description: "An authored recipe overlay, as found in an external " +
				"--data catalog.",
		},
		{
			Kind:     "RecipeMixin",
			Authored: true,
			Type:     reflect.TypeOf(recipe.RecipeMixin{}),
			Description: "A composable fragment of shared overlay content. Carries " +
				"only constraints and component references.",
		},
		{
			Kind:     "RecipeCriteria",
			Authored: true,
			Type:     reflect.TypeOf(recipe.RecipeCriteria{}),
			Description: "The criteria that select a recipe: service, accelerator, " +
				"intent, os, platform and node count.",
		},
	}
}

// criteriaEnums maps the criteria types to their registered values so the
// schemas enumerate what is actually accepted.
//
// Values come from the same accessors the CLI and REST layers validate against,
// so a new accelerator appears in the schema without anyone remembering to add
// it here. Hardcoding the lists would create a third place to update and a new
// way for the published contract to be wrong.
func criteriaEnums() map[string][]string {
	// GetCriteria*Types deliberately omits the "any" wildcard -- the OpenAPI
	// spec adds it back, and docs/contributor/api-server.md records that the
	// parity test strips it before comparing. Publishing the raw lists produced
	// schemas that rejected every overlay written with `service: any`.
	withAny := func(values []string) []string {
		return append(append([]string(nil), values...), "any")
	}
	return map[string][]string{
		"recipe.CriteriaServiceType":     withAny(recipe.GetCriteriaServiceTypes()),
		"recipe.CriteriaAcceleratorType": withAny(recipe.GetCriteriaAcceleratorTypes()),
		"recipe.CriteriaIntentType":      withAny(recipe.GetCriteriaIntentTypes()),
		"recipe.CriteriaOSType":          withAny(recipe.GetCriteriaOSTypes()),
		"recipe.CriteriaPlatformType":    withAny(recipe.GetCriteriaPlatformTypes()),
	}
}

// Render produces the schema bytes for one artifact.
//
// Output is deterministic: json.Marshal sorts map keys, required lists are
// sorted at generation, and the trailing newline makes the file diff cleanly.
// Reproducibility matters because these files are compared byte-for-byte by the
// freshness check.
func Render(artifact Artifact) ([]byte, error) {
	generated, err := schema.Generate(artifact.Type, schema.Options{
		Title:       artifact.Kind,
		Description: artifact.Description,
		ID:          fmt.Sprintf("%s/%s.schema.json", SchemaBaseURI, artifact.Kind),
		Enums:       criteriaEnums(),
		Authored:    artifact.Authored,
	})
	if err != nil {
		return nil, fmt.Errorf("generate %s: %w", artifact.Kind, err)
	}

	encoded, err := json.MarshalIndent(generated, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", artifact.Kind, err)
	}
	return append(encoded, '\n'), nil
}

// FileName is the committed file name for an artifact kind.
func FileName(kind string) string {
	return kind + ".schema.json"
}

func main() {
	outDir := flag.String("out", filepath.Join("api", "aicr", "v1", "schemas"),
		"directory to write schema files into")
	flag.Parse()

	if err := run(*outDir); err != nil {
		fmt.Fprintf(os.Stderr, "schemagen: %v\n", err)
		os.Exit(1)
	}
}

func run(outDir string) error {
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return fmt.Errorf("create %s: %w", outDir, err)
	}

	artifacts := Artifacts()
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Kind < artifacts[j].Kind })

	for _, artifact := range artifacts {
		encoded, err := Render(artifact)
		if err != nil {
			return err
		}
		path := filepath.Join(outDir, FileName(artifact.Kind))
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		fmt.Printf("wrote %s\n", path)
	}
	return nil
}
