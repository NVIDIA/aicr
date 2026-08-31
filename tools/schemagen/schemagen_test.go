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

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// schemaDir is the committed output, relative to this package.
const schemaDir = "../../api/aicr/v1/schemas"

// TestCommittedSchemasAreFresh asserts the committed files match what the
// current Go types produce.
//
// This is the freshness half of issue #2113 scope item 4. Without it the
// schemas would be a snapshot of whatever the types looked like when someone
// last remembered to run the generator — and the breaking-change gate built on
// them would be diffing a contract that no longer describes the artifacts.
func TestCommittedSchemasAreFresh(t *testing.T) {
	artifacts := Artifacts()
	if len(artifacts) == 0 {
		t.Fatal("no artifacts declared; every assertion below would pass vacuously")
	}

	for _, artifact := range artifacts {
		t.Run(artifact.Kind, func(t *testing.T) {
			want, err := Render(artifact)
			if err != nil {
				t.Fatalf("render: %v", err)
			}

			path := filepath.Join(schemaDir, FileName(artifact.Kind))
			got, err := os.ReadFile(filepath.Clean(path))
			if err != nil {
				t.Fatalf("read committed schema %s: %v (run `make schemas`)", path, err)
			}

			if !bytes.Equal(got, want) {
				t.Errorf("%s is stale: it no longer matches the %s Go type.\n"+
					"Run `make schemas` and commit the result.",
					path, artifact.Type.String())
			}
		})
	}
}

// TestEveryCommittedSchemaIsDeclared asserts the directory holds no schema for
// an artifact the generator no longer produces.
//
// Freshness alone only checks the files it knows about. A kind removed from
// Artifacts() would leave its schema behind, still published, still diffed by
// the gate, describing a contract nothing emits.
func TestEveryCommittedSchemaIsDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, artifact := range Artifacts() {
		declared[FileName(artifact.Kind)] = true
	}

	entries, err := os.ReadDir(schemaDir)
	if err != nil {
		t.Fatalf("read %s: %v", schemaDir, err)
	}
	if len(entries) == 0 {
		t.Fatal("schema directory is empty; run `make schemas`")
	}

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".schema.json") {
			continue
		}
		if !declared[entry.Name()] {
			t.Errorf("%s is committed but no artifact declares it; delete it or "+
				"restore the artifact", entry.Name())
		}
	}
}

// TestSchemasDescribeRealArtifacts validates committed artifacts against their
// schema.
//
// Freshness proves the schema matches the Go type. It does not prove the Go
// type matches what is on disk — and the catalog is authored by hand, so those
// can disagree. This walks real overlays and mixins and checks the two
// properties a wrong schema would violate: a required field the document omits,
// or a document field the schema never declares.
//
// A field the schema omits is the dangerous direction. It reads as a passing
// validation while the breaking-change gate silently stops protecting that
// field.
func TestSchemasDescribeRealArtifacts(t *testing.T) {
	cases := []struct {
		kind    string
		glob    string
		minimum int
	}{
		{kind: "RecipeMetadata", glob: "../../recipes/overlays/*.yaml", minimum: 50},
		{kind: "RecipeMixin", glob: "../../recipes/mixins/*.yaml", minimum: 3},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			loaded := loadSchema(t, tc.kind)

			files, err := filepath.Glob(tc.glob)
			if err != nil {
				t.Fatalf("glob %s: %v", tc.glob, err)
			}
			if len(files) < tc.minimum {
				t.Fatalf("found %d files matching %s, want at least %d; the corpus "+
					"moved and this test would pass over almost nothing",
					len(files), tc.glob, tc.minimum)
			}

			var checked int
			for _, file := range files {
				raw, readErr := os.ReadFile(filepath.Clean(file))
				if readErr != nil {
					t.Errorf("read %s: %v", file, readErr)
					continue
				}
				var document map[string]any
				if err := yaml.Unmarshal(raw, &document); err != nil {
					t.Errorf("parse %s: %v", file, err)
					continue
				}
				// Documents of another kind share these directories in some
				// layouts; check only what this schema claims to describe.
				if kind, _ := document["kind"].(string); kind != tc.kind {
					continue
				}
				checked++
				checkDocument(t, file, document, loaded)
			}

			// Counting globbed files is not the same as counting validated
			// ones. If the kind moved out of the document root every file would
			// be skipped and this test would report success having validated
			// nothing -- the exact shape of the inert guard #2462 shipped.
			if checked < tc.minimum {
				t.Errorf("validated %d %s documents out of %d files, want at least "+
					"%d; the kind filter is skipping documents it should check",
					checked, tc.kind, len(files), tc.minimum)
			}
		})
	}
}

// schemaNode is the subset of the generated document this test reads.
type schemaNode struct {
	Type       string                 `json:"-"`
	Nullable   bool                   `json:"-"`
	Properties map[string]*schemaNode `json:"properties"`
	Required   []string               `json:"required"`
	Items      *schemaNode            `json:"items"`
	Enum       []string               `json:"enum"`
}

// UnmarshalJSON reads `type` in either form the dialect allows: a bare string,
// or an array pairing the type with "null".
//
// Reading it as a string alone silently left Type empty for every nullable
// field, which matchesType treats as "anything goes" -- the checks below would
// have quietly stopped constraining exactly the fields this round made
// nullable.
func (n *schemaNode) UnmarshalJSON(data []byte) error {
	type alias schemaNode
	var raw struct {
		alias
		Type json.RawMessage `json:"type"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*n = schemaNode(raw.alias)

	if len(raw.Type) == 0 {
		return nil
	}
	var single string
	if err := json.Unmarshal(raw.Type, &single); err == nil {
		n.Type = single
		return nil
	}
	var many []string
	if err := json.Unmarshal(raw.Type, &many); err != nil {
		return fmt.Errorf("type is neither a string nor an array: %s", raw.Type)
	}
	for _, entry := range many {
		if entry == "null" {
			n.Nullable = true
			continue
		}
		n.Type = entry
	}
	return nil
}

func loadSchema(t *testing.T, kind string) *schemaNode {
	t.Helper()

	path := filepath.Join(schemaDir, FileName(kind))
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var node schemaNode
	if err := json.Unmarshal(raw, &node); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(node.Properties) == 0 {
		t.Fatalf("%s declares no properties; this test would pass over everything", path)
	}
	return &node
}

// checkDocument walks a document against a schema node.
//
// Presence checks alone let a wrong-typed scalar or an out-of-enum value pass
// while the published JSON Schema rejects the same document, so the gate would
// report a validity it does not have. Types, enums and array elements are
// checked too.
func checkDocument(t *testing.T, file string, document map[string]any, node *schemaNode) {
	t.Helper()

	for _, required := range node.Required {
		if _, ok := document[required]; !ok {
			t.Errorf("%s: schema requires %q but the document omits it; the schema "+
				"would reject an artifact the project ships", file, required)
		}
	}

	keys := make([]string, 0, len(document))
	for key := range document {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		child, declared := node.Properties[key]
		if !declared {
			t.Errorf("%s: document has field %q that the schema does not declare; "+
				"the published contract omits a field real artifacts carry",
				file, key)
			continue
		}
		checkValue(t, file+"."+key, document[key], child)
	}
}

// checkValue validates one value against a schema node.
func checkValue(t *testing.T, path string, value any, node *schemaNode) {
	t.Helper()

	// An explicitly empty YAML key is present with a nil value. Waving it
	// through meant a document could carry null where the published schema
	// permits only a string, and this gate would still call it valid.
	if value == nil {
		if node.Type != "" && !node.Nullable {
			t.Errorf("%s: document holds null but the schema declares type %q "+
				"without null; the published schema would reject this artifact",
				path, node.Type)
		}
		return
	}

	if node.Type != "" && !matchesType(value, node.Type) {
		t.Errorf("%s: document holds %T but the schema declares type %q; the "+
			"published schema would reject this artifact", path, value, node.Type)
		return
	}

	if len(node.Enum) > 0 {
		if text, ok := value.(string); ok && !containsString(node.Enum, text) {
			t.Errorf("%s: value %q is not among the %d values the schema permits "+
				"(%s)", path, text, len(node.Enum), strings.Join(node.Enum, ", "))
		}
	}

	switch typed := value.(type) {
	case map[string]any:
		// A map-typed field (propertyNames, no properties) accepts any keys, so
		// only descend when the schema actually describes fields.
		if len(node.Properties) > 0 {
			checkDocument(t, path, typed, node)
		}
	case []any:
		if node.Items == nil {
			return
		}
		for i, item := range typed {
			checkValue(t, fmt.Sprintf("%s[%d]", path, i), item, node.Items)
		}
	}
}

// matchesType reports whether a decoded YAML value fits a JSON Schema type.
func matchesType(value any, want string) bool {
	switch want {
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "integer":
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			return true
		default:
			return false
		}
	case "number":
		switch value.(type) {
		case float32, float64, int, int64:
			return true
		default:
			return false
		}
	case "array":
		_, ok := value.([]any)
		return ok
	case "object":
		_, ok := value.(map[string]any)
		return ok
	default:
		// An empty type is an open value, which anything satisfies.
		return true
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
