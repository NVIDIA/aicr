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

package server_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"gopkg.in/yaml.v3"
)

// TestOpenAPIEnumsMatchGoTypes asserts that every criteria-field enum in
// api/aicr/v1/server.yaml matches the canonical list returned by the
// corresponding pkg/recipe.GetCriteria*Types function.
//
// Drift here is a contract bug: clients that conform to the OpenAPI spec
// will reject inputs the server actually accepts (or generate types that
// reject server outputs). Adding a new value to a Go criteria type must
// be reflected in the spec — and vice versa — and this test enforces it.
//
// Sites checked:
//   - Query parameters (- name: <field>) under any operation
//   - Schema properties (Criteria.properties.<field>) under components.schemas
//
// "any" is allowed to appear in the spec as a wildcard but is NOT part of
// the Go type list, so it is stripped before comparison.
func TestOpenAPIEnumsMatchGoTypes(t *testing.T) {
	specPath := filepath.Join("..", "..", "api", "aicr", "v1", "server.yaml")
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read spec %q: %v", specPath, err)
	}

	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	// Canonical Go enums, keyed by criteria field name as it appears in the spec.
	// "gpu" is a back-compat alias for "accelerator" and shares its enum.
	canonical := map[string][]string{
		"service":     recipe.GetCriteriaServiceTypes(),
		"accelerator": recipe.GetCriteriaAcceleratorTypes(),
		"gpu":         recipe.GetCriteriaAcceleratorTypes(),
		"intent":      recipe.GetCriteriaIntentTypes(),
		"os":          recipe.GetCriteriaOSTypes(),
		"platform":    recipe.GetCriteriaPlatformTypes(),
	}

	sites := collectCriteriaEnumSites(&root, canonical)

	for field, want := range canonical {
		observed, ok := sites[field]
		if !ok {
			t.Errorf("server.yaml: no enum sites found for criteria field %q", field)
			continue
		}
		sortedWant := append([]string(nil), want...)
		sort.Strings(sortedWant)
		for i, enum := range observed {
			got := stripAny(enum)
			sort.Strings(got)
			if !equalStrings(got, sortedWant) {
				t.Errorf("criteria field %q, enum site %d: got %v (sans \"any\"), want %v",
					field, i, got, sortedWant)
			}
		}
	}
}

type openAPIContractSchema struct {
	Ref        string                           `yaml:"$ref"`
	AllOf      []openAPIContractSchema          `yaml:"allOf"`
	Required   []string                         `yaml:"required"`
	Properties map[string]openAPIContractSchema `yaml:"properties"`
	Enum       []string                         `yaml:"enum"`
}

type openAPIContractMedia struct {
	Schema openAPIContractSchema `yaml:"schema"`
}

// openAPIContractOperation models the two sides of an operation this gate
// constrains: the request body schema, and the success-response schema. Both
// are needed — checking only the component definitions would let a future edit
// repoint an operation at a different schema with every assertion still green.
type openAPIContractOperation struct {
	RequestBody struct {
		Content map[string]openAPIContractMedia `yaml:"content"`
	} `yaml:"requestBody"`
	Responses map[string]struct {
		Content map[string]openAPIContractMedia `yaml:"content"`
	} `yaml:"responses"`
}

type openAPIBundleContract struct {
	Paths map[string]struct {
		Get  openAPIContractOperation `yaml:"get"`
		Post openAPIContractOperation `yaml:"post"`
	} `yaml:"paths"`
	Components struct {
		Schemas map[string]openAPIContractSchema `yaml:"schemas"`
	} `yaml:"components"`
}

// allOfConstraint splits a wrapper schema's allOf into the shared base $ref and
// the wrapper's own inline constraint object, identifying each by content
// rather than by position. allOf is semantically unordered, so a spec author
// who swaps the two entries writes an equivalent contract and must not trip
// this gate.
//
// It accounts for every entry rather than picking out the two it recognizes.
// allOf intersects its branches, so an unrecognized third $ref would tighten
// the effective contract — grafting a strict schema onto BundleRecipeRequest
// would make validators reject the versionless requests this gate exists to
// protect, and ignoring the entry would leave the test green while it happened.
// A duplicated base $ref is rejected for the same reason: counted rather than
// flagged by a boolean, so a second one cannot pass unnoticed.
func allOfConstraint(t *testing.T, schema openAPIContractSchema, baseRef string) openAPIContractSchema {
	t.Helper()

	var constraints []openAPIContractSchema
	var baseRefs int
	for _, entry := range schema.AllOf {
		switch {
		case entry.Ref == baseRef:
			baseRefs++
		case entry.Ref != "":
			t.Fatalf("allOf references unexpected schema %q; only %q plus one inline "+
				"constraint object are allowed, and allOf intersects every branch",
				entry.Ref, baseRef)
		default:
			constraints = append(constraints, entry)
		}
	}
	if baseRefs != 1 {
		t.Fatalf("allOf references %q %d times, want exactly 1", baseRef, baseRefs)
	}
	if len(constraints) != 1 {
		t.Fatalf("allOf has %d inline constraint objects, want exactly 1", len(constraints))
	}
	return constraints[0]
}

func TestOpenAPIV1BundleRecipeContract(t *testing.T) {
	specPath := filepath.Join("..", "..", "api", "aicr", "v1", "server.yaml")
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read spec %q: %v", specPath, err)
	}

	var spec openAPIBundleContract
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	for _, tt := range []struct {
		name string
		got  string
	}{
		{
			name: "v1 bundle request body",
			got: spec.Paths["/v1/bundle"].Post.RequestBody.
				Content["application/json"].Schema.Ref,
		},
		{
			name: "deprecated bundle wrapper",
			got:  spec.Components.Schemas["BundleRequest"].Properties["recipe"].Ref,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if want := "#/components/schemas/BundleRecipeRequest"; tt.got != want {
				t.Errorf("$ref = %q, want %q", tt.got, want)
			}
		})
	}

	// Response sites, checked separately from the RecipeResponse component
	// below. Asserting only the component's shape would let a future edit
	// repoint an operation's 200 at RecipeResponseBase or BundleRecipeRequest
	// — silently relaxing responses to the permissive legacy enums — while
	// every other assertion here stayed green.
	recipePath := spec.Paths["/v1/recipe"]
	for _, tt := range []struct {
		name string
		got  string
	}{
		{
			name: "GET /v1/recipe 200",
			got:  recipePath.Get.Responses["200"].Content["application/json"].Schema.Ref,
		},
		{
			name: "POST /v1/recipe 200",
			got:  recipePath.Post.Responses["200"].Content["application/json"].Schema.Ref,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if want := "#/components/schemas/RecipeResponse"; tt.got != want {
				t.Errorf("$ref = %q, want %q", tt.got, want)
			}
		})
	}

	for _, tt := range []struct {
		name        string
		schema      openAPIContractSchema
		required    []string
		apiVersions []string
		kinds       []string
	}{
		{
			name:        "versioned response",
			schema:      spec.Components.Schemas["RecipeResponse"],
			required:    []string{"apiVersion", "kind"},
			apiVersions: []string{recipe.RecipeAPIVersion},
			kinds:       []string{recipe.RecipeResultKind},
		},
		{
			// The bundle request also admits the legacy Recipe kind: that was
			// the value this contract published through v0.18.0, and the
			// handler validates no header field, so dropping it from the enum
			// would make previously conforming clients spec-invalid against a
			// request-validating gateway.
			name:        "bundle request",
			schema:      spec.Components.Schemas["BundleRecipeRequest"],
			apiVersions: []string{"", recipe.RecipeAPIVersion},
			kinds:       []string{"", string(header.KindRecipe), recipe.RecipeResultKind},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			closure := allOfConstraint(t, tt.schema, "#/components/schemas/RecipeResponseBase")
			if !equalStringsUnordered(closure.Required, tt.required) {
				t.Errorf("required = %v, want %v", closure.Required, tt.required)
			}
			gotAPIVersions := closure.Properties["apiVersion"].Enum
			if !equalStringsUnordered(gotAPIVersions, tt.apiVersions) {
				t.Errorf("apiVersion enum = %v, want %v", gotAPIVersions, tt.apiVersions)
			}
			gotKinds := closure.Properties["kind"].Enum
			if !equalStringsUnordered(gotKinds, tt.kinds) {
				t.Errorf("kind enum = %v, want %v", gotKinds, tt.kinds)
			}
		})
	}

	base := spec.Components.Schemas["RecipeResponseBase"]
	if len(base.Required) != 0 {
		t.Fatal("RecipeResponseBase must not require apiVersion; wrappers own header requirements")
	}
	for _, name := range []string{"apiVersion", "kind"} {
		property, ok := base.Properties[name]
		if !ok {
			t.Errorf("RecipeResponseBase is missing %s property", name)
			continue
		}
		if got := property.Enum; len(got) != 0 {
			t.Errorf("RecipeResponseBase %s enum = %v, want wrapper-owned enum", name, got)
		}
	}
}

// collectCriteriaEnumSites walks the YAML tree and returns every enum array
// that belongs to a known criteria field, keyed by field name.
//
// Two patterns are recognized:
//
//  1. OpenAPI parameter:
//     - name: <field>
//     in: query
//     schema:
//     enum: [...]
//
//  2. OpenAPI schema property:
//     <field>:
//     type: string
//     enum: [...]
func collectCriteriaEnumSites(root *yaml.Node, names map[string][]string) map[string][][]string {
	out := map[string][][]string{}

	var walk func(*yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil {
			return
		}
		switch n.Kind {
		case yaml.ScalarNode, yaml.AliasNode:
			// Leaves — nothing to recurse into.
		case yaml.DocumentNode, yaml.SequenceNode:
			for _, c := range n.Content {
				walk(c)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				key, val := n.Content[i], n.Content[i+1]

				// Pattern 1: parameter object — current mapping has "name: <field>"
				if key.Value == "name" {
					if _, want := names[val.Value]; want {
						if enum := findEnumInSchemaSibling(n); enum != nil {
							out[val.Value] = append(out[val.Value], enum)
						}
					}
				}

				// Pattern 2: schema property — key is a known field name and value
				// is a mapping with an "enum" child. Avoid matching the parameter
				// "name: <field>" form (where val is a scalar string).
				if _, want := names[key.Value]; want && val.Kind == yaml.MappingNode {
					if enum := findDirectEnum(val); enum != nil {
						out[key.Value] = append(out[key.Value], enum)
					}
				}

				walk(val)
			}
		}
	}
	walk(root)
	return out
}

// findEnumInSchemaSibling searches a parameter mapping for a "schema" child
// and returns its "enum" array, if present.
func findEnumInSchemaSibling(paramObj *yaml.Node) []string {
	for i := 0; i+1 < len(paramObj.Content); i += 2 {
		if paramObj.Content[i].Value == "schema" {
			return findDirectEnum(paramObj.Content[i+1])
		}
	}
	return nil
}

// findDirectEnum returns the "enum" array of a schema mapping, or nil.
func findDirectEnum(schema *yaml.Node) []string {
	if schema == nil || schema.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(schema.Content); i += 2 {
		if schema.Content[i].Value != "enum" {
			continue
		}
		seq := schema.Content[i+1]
		if seq.Kind != yaml.SequenceNode {
			return nil
		}
		out := make([]string, 0, len(seq.Content))
		for _, c := range seq.Content {
			out = append(out, c.Value)
		}
		return out
	}
	return nil
}

func stripAny(s []string) []string {
	out := make([]string, 0, len(s))
	for _, v := range s {
		if v == "any" {
			continue
		}
		out = append(out, v)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// equalStringsUnordered reports whether a and b hold the same elements in any
// order. It is an order-insensitive slice compare, NOT set equality: duplicates
// are significant, so ["a","a"] and ["a"] differ. That is the behavior wanted
// for enum comparison — a repeated enum member is itself a spec defect worth
// failing on, not something to silently collapse.
func equalStringsUnordered(a, b []string) bool {
	sortedA := append([]string(nil), a...)
	sortedB := append([]string(nil), b...)
	sort.Strings(sortedA)
	sort.Strings(sortedB)
	return equalStrings(sortedA, sortedB)
}
