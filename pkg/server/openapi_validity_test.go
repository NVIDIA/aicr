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

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Structural validity for the frozen REST contract (issue #2112, scope item 2).
//
// tools/openapi-diff answers "did this change break the contract"; it does not
// answer "is the contract coherent". Those fail differently: a dangling $ref or
// a duplicate operationId is present in both baseline and spec, so a diff sees
// no change and passes while every generated client is wrong.
//
// This is not a substitute for a full OpenAPI linter. It covers the internal
// consistency the collapse in #2464 actually put at risk — that change deleted
// eleven schemas, and verifying nothing pointed at them was a manual step at
// the time.

const baselineRelPath = "../../api/aicr/v1/server.baseline.yaml"

// TestOpenAPIRefsResolve asserts every internal $ref points at something that
// exists.
//
// A dangling $ref is invisible to the diff gate — both sides carry it — and
// invisible to the route tests, which read paths rather than schemas. It
// surfaces only when a client generator or validator chokes on it.
func TestOpenAPIRefsResolve(t *testing.T) {
	for _, spec := range []string{specRelPath, baselineRelPath} {
		t.Run(filepath.Base(spec), func(t *testing.T) {
			doc := loadOpenAPIDocument(t, spec)

			refs := collectRefs(doc)
			if len(refs) == 0 {
				t.Fatal("no $refs found; the walk is broken and every assertion " +
					"below would pass vacuously")
			}

			var dangling []string
			for ref := range refs {
				if !strings.HasPrefix(ref, "#/") {
					// External refs are a different risk (SSRF, availability)
					// and are not used here; flag rather than silently skip.
					dangling = append(dangling, ref+" (external ref)")
					continue
				}
				if !refExists(doc, ref) {
					dangling = append(dangling, ref)
				}
			}
			sort.Strings(dangling)
			for _, ref := range dangling {
				t.Errorf("%s: $ref %q does not resolve; a generated client cannot "+
					"be built from this document", spec, ref)
			}
		})
	}
}

// TestOpenAPIHasNoOrphanedComponents asserts every declared component is
// referenced.
//
// An orphan is not a correctness bug on its own, but it is how a deleted
// surface leaves residue: #2464 removed the /v2 family and eleven schemas
// became unreachable, which was caught by reading the file. An orphan that
// survives is a schema reviewers assume is live, and the breaking-change gate
// will faithfully protect it forever.
func TestOpenAPIHasNoOrphanedComponents(t *testing.T) {
	doc := loadOpenAPIDocument(t, specRelPath)

	refs := collectRefs(doc)
	components, ok := doc["components"].(map[string]any)
	if !ok {
		t.Fatal("spec declares no components object")
	}

	// Every group the document declares, not a hand-picked subset: an orphaned
	// header is the same residue class as an orphaned schema, and omitting
	// headers meant the check could not see one.
	var checked int
	for kind, raw := range components {
		group, groupOK := raw.(map[string]any)
		if !groupOK {
			// Skipping silently would let a document with one valid group and
			// one malformed group pass this check while half of it was never
			// inspected.
			t.Errorf("components.%s is %T, want an object; its entries cannot be "+
				"checked for orphans", kind, raw)
			continue
		}
		for name := range group {
			checked++
			ref := fmt.Sprintf("#/components/%s/%s", kind, name)
			if !isReferenced(refs, ref) {
				t.Errorf("component %s is declared but never referenced; delete it "+
					"or wire it up", ref)
			}
		}
	}

	if checked == 0 {
		t.Fatal("no components inspected; the parse shape is wrong and this " +
			"assertion would pass vacuously")
	}
}

// TestOpenAPIOperationIDsAreUniqueAndPresent asserts each operation carries a
// distinct operationId.
//
// Generators derive method names from operationId. A duplicate silently
// collapses two operations into one method, or produces a client that cannot
// compile, and neither the diff gate nor the route tests look at the field.
func TestOpenAPIOperationIDsAreUniqueAndPresent(t *testing.T) {
	doc := loadOpenAPIDocument(t, specRelPath)

	paths, ok := doc["paths"].(map[string]any)
	if !ok || len(paths) == 0 {
		t.Fatal("spec declares no paths; this assertion would pass vacuously")
	}

	seen := map[string]string{}
	var operations int

	for path, item := range paths {
		pathItem, itemOK := item.(map[string]any)
		if !itemOK {
			continue
		}
		for method, raw := range pathItem {
			if !httpMethods[strings.ToLower(method)] {
				continue
			}
			operation, opOK := raw.(map[string]any)
			if !opOK {
				continue
			}
			operations++

			id, _ := operation["operationId"].(string)
			location := strings.ToUpper(method) + " " + path
			if id == "" {
				t.Errorf("%s has no operationId; client generators name methods "+
					"from it", location)
				continue
			}
			if prior, dup := seen[id]; dup {
				t.Errorf("operationId %q is used by both %s and %s; a generated "+
					"client cannot have two methods with one name",
					id, prior, location)
				continue
			}
			seen[id] = location

			if responses, hasResponses := operation["responses"].(map[string]any); !hasResponses ||
				len(responses) == 0 {

				t.Errorf("%s declares no responses", location)
			}
		}
	}

	if operations == 0 {
		t.Fatal("no operations inspected; the parse shape is wrong")
	}
}

// TestOpenAPIBaselineIsAWholeDocument guards the baseline against silent
// truncation.
//
// make openapi-baseline rebuilds the file by splicing a header onto the spec.
// If that ever emits a partial document, tools/openapi-diff would compare
// against a stub and report no breaking changes for anything the stub omits —
// the gate would pass precisely when it should fail loudest. An early version
// of the target did truncate, which is why this exists.
func TestOpenAPIBaselineIsAWholeDocument(t *testing.T) {
	spec := loadOpenAPIDocument(t, specRelPath)
	baseline := loadOpenAPIDocument(t, baselineRelPath)

	for _, key := range []string{"openapi", "info", "paths", "components"} {
		if _, ok := baseline[key]; !ok {
			t.Errorf("baseline is missing the top-level %q key; it is not a whole "+
				"OpenAPI document and the diff gate would compare against a stub", key)
		}
	}

	specPaths, _ := spec["paths"].(map[string]any)
	basePaths, _ := baseline["paths"].(map[string]any)
	if len(basePaths) == 0 {
		t.Fatal("baseline declares no paths; the diff gate would report no " +
			"breaking changes no matter what the spec did")
	}

	// The baseline may legitimately lag the spec on additive change, so this is
	// a floor rather than an equality check: it catches truncation without
	// forcing a refresh on every additive edit.
	if len(basePaths) < len(specPaths)/2 {
		t.Errorf("baseline declares %d paths against the spec's %d; that gap "+
			"suggests truncation rather than additive drift",
			len(basePaths), len(specPaths))
	}
}

// isReferenced reports whether a component is reached by any $ref.
//
// Exact string equality is not enough: the spec points into subschemas, e.g.
// #/components/schemas/RecipeResponseBase/properties/configuration. A component
// reached only through such a pointer is live, and comparing whole strings
// would report it as an orphan. Matching on the pointer prefix at a path
// boundary avoids that without letting a similarly-named component match.
func isReferenced(refs map[string]bool, ref string) bool {
	if refs[ref] {
		return true
	}
	for candidate := range refs {
		if strings.HasPrefix(candidate, ref+"/") {
			return true
		}
	}
	return false
}

// loadOpenAPIDocument parses an OpenAPI document into a generic tree.
func loadOpenAPIDocument(t *testing.T, path string) map[string]any {
	t.Helper()

	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parse %q: %v", path, err)
	}
	if len(doc) == 0 {
		t.Fatalf("%q parsed to an empty document", path)
	}
	return doc
}

// collectRefs walks the document and returns every $ref value it finds.
func collectRefs(node any) map[string]bool {
	refs := map[string]bool{}
	walkRefs(node, refs)
	return refs
}

func walkRefs(node any, refs map[string]bool) {
	switch typed := node.(type) {
	case map[string]any:
		for key, value := range typed {
			if key == "$ref" {
				if ref, ok := value.(string); ok {
					refs[ref] = true
				}
				continue
			}
			walkRefs(value, refs)
		}
	case []any:
		for _, item := range typed {
			walkRefs(item, refs)
		}
	}
}

// refExists resolves a local JSON pointer of the form #/a/b/c.
func refExists(doc map[string]any, ref string) bool {
	current := any(doc)
	for _, segment := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		// JSON pointer escaping; rare here but wrong to ignore.
		segment = strings.ReplaceAll(segment, "~1", "/")
		segment = strings.ReplaceAll(segment, "~0", "~")

		container, ok := current.(map[string]any)
		if !ok {
			return false
		}
		current, ok = container[segment]
		if !ok {
			return false
		}
	}
	return true
}
