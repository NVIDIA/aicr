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

package api_test

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

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
