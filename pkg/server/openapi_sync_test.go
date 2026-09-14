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

func TestOpenAPIDRAEvictionNodeLabelContract(t *testing.T) {
	specPath := filepath.Join("..", "..", "api", "aicr", "v1", "server.yaml")
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read spec %q: %v", specPath, err)
	}
	var spec map[string]any
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	const parameterRef = "#/components/parameters/DRAEvictionNodeLabel"
	for _, path := range []string{"/v1/bundle", "/v1/bundle"} {
		t.Run(path, func(t *testing.T) {
			operation := openAPIObjectAt(t, spec, "paths", path, "post")
			parameters := openAPISequence(t, operation["parameters"], path+" parameters")
			refCount := 0
			for _, value := range parameters {
				parameter := openAPIObject(t, value, path+" parameter")
				if parameter["$ref"] == parameterRef {
					refCount++
				}
				if parameter["name"] == "dra-eviction-node-label" {
					t.Error("dra-eviction-node-label must use the shared component parameter")
				}
			}
			if refCount != 1 {
				t.Errorf("DRA eviction parameter reference count = %d, want 1", refCount)
			}
		})
	}

	parameter := openAPIObjectAt(t, spec, "components", "parameters", "DRAEvictionNodeLabel")
	if got := parameter["name"]; got != "dra-eviction-node-label" {
		t.Errorf("component parameter name = %v, want dra-eviction-node-label", got)
	}
	schema := openAPIObjectAt(t, parameter, "schema")
	// The eviction contract is opt-in (issue #2469): the parameter must carry
	// no default, so an omitted parameter injects neither half.
	if got, ok := schema["default"]; ok {
		t.Errorf("component parameter default = %v, want none (opt-in)", got)
	}
}

func TestOpenAPIBundleContract(t *testing.T) {
	specPath := filepath.Join("..", "..", "api", "aicr", "v1", "server.yaml")
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read spec %q: %v", specPath, err)
	}
	var spec map[string]any
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	content := openAPIObjectAt(t, spec,
		"paths", "/v1/bundle", "post", "requestBody", "content")
	for _, mediaType := range []string{"application/json", "application/x-yaml"} {
		schema := openAPIObjectAt(t, content, mediaType, "schema")
		if got := schema["$ref"]; got != "#/components/schemas/BundleRecipeRequest" {
			t.Errorf("%s request schema = %v, want BundleRecipeRequest", mediaType, got)
		}
	}

	responses := openAPIObjectAt(t, spec, "paths", "/v1/bundle", "post", "responses")
	for _, status := range []string{"401", "404", "503", "504"} {
		if _, ok := responses[status]; !ok {
			t.Errorf("/v1/bundle response %s is not declared", status)
		}
	}

	schemas := openAPIObjectAt(t, spec, "components", "schemas")
	union := openAPIObjectAt(t, schemas, "BundleRecipeRequest")
	refs := map[string]bool{}
	for _, item := range openAPISequence(t, union["oneOf"], "BundleRecipeRequest.oneOf") {
		schema := openAPIObject(t, item, "BundleRecipeRequest.oneOf item")
		ref, _ := schema["$ref"].(string)
		refs[ref] = true
	}
	for _, ref := range []string{
		"#/components/schemas/LegacyBundleRecipeRequest",
		"#/components/schemas/ProfileRecipeResponse",
		"#/components/schemas/ConfiguredRecipeResponse",
	} {
		if !refs[ref] {
			t.Errorf("BundleRecipeRequest.oneOf does not reference %s", ref)
		}
	}
	// The strict response schema must NOT be a request branch: reusing it
	// there re-requires kind: RecipeResult and re-rejects the legacy shapes
	// (kind absent/empty) the v2 decode path accepts.
	if refs["#/components/schemas/LegacyRecipeResponse"] {
		t.Error("BundleRecipeRequest.oneOf must not reuse the strict LegacyRecipeResponse response schema")
	}

	// The legacy request branch covers the whole accepted legacy square:
	// apiVersion absent/""/v1alpha2 × kind absent/""/RecipeResult, headers
	// optional. Anything narrower leaves /v1/bundle stricter than its own
	// server (DecodeRecipeResult enforces kind only when non-empty).
	legacyBranch := openAPIObjectAt(t, schemas, "LegacyBundleRecipeRequest")
	legacyBranchAllOf := openAPISequence(t, legacyBranch["allOf"], "LegacyBundleRecipeRequest.allOf")
	legacyOverlay := openAPIObject(t, legacyBranchAllOf[1], "LegacyBundleRecipeRequest overlay")
	if _, required := legacyOverlay["required"]; required {
		t.Error("LegacyBundleRecipeRequest must not require header fields")
	}
	legacyAPIVersion := openAPIObjectAt(t, legacyOverlay, "properties", "apiVersion")
	legacyAPIVersions := openAPISequence(t, legacyAPIVersion["enum"],
		"LegacyBundleRecipeRequest apiVersion enum")
	for _, value := range []string{"", "aicr.run/v1alpha2", header.GroupVersionV1} {
		if !openAPIHasString(legacyAPIVersions, value) {
			t.Errorf("LegacyBundleRecipeRequest apiVersion enum missing %q", value)
		}
	}
	legacyKind := openAPIObjectAt(t, legacyOverlay, "properties", "kind")
	legacyKinds := openAPISequence(t, legacyKind["enum"],
		"LegacyBundleRecipeRequest kind enum")
	for _, value := range []string{"", "RecipeResult"} {
		if !openAPIHasString(legacyKinds, value) {
			t.Errorf("LegacyBundleRecipeRequest kind enum missing %q", value)
		}
	}
	legacyConfigurationNot := openAPIObjectAt(t, legacyOverlay, "not")
	if !openAPIHasString(
		openAPISequence(t, legacyConfigurationNot["required"],
			"LegacyBundleRecipeRequest not.required"),
		"configuration",
	) {

		t.Error("LegacyBundleRecipeRequest does not prohibit configuration")
	}
	if _, ok := union["discriminator"]; ok {
		t.Error("BundleRecipeRequest must not discriminate a versionless branch by apiVersion")
	}

	configured := openAPIObjectAt(t, schemas, "ConfiguredRecipeResponse")
	configuredAllOf := openAPISequence(t, configured["allOf"], "ConfiguredRecipeResponse.allOf")
	if len(configuredAllOf) != 2 {
		t.Fatalf("ConfiguredRecipeResponse.allOf has %d entries, want 2", len(configuredAllOf))
	}
	configuredClosure := openAPIObject(t, configuredAllOf[1], "ConfiguredRecipeResponse closure")
	if closed, ok := configuredClosure["additionalProperties"].(bool); !ok || closed {
		t.Errorf("ConfiguredRecipeResponse additionalProperties = %v, want false",
			configuredClosure["additionalProperties"])
	}
	configuredRequired := openAPISequence(t, configuredClosure["required"],
		"ConfiguredRecipeResponse.required")
	if !openAPIHasString(configuredRequired, "configuration") {
		t.Error("ConfiguredRecipeResponse does not require configuration")
	}
	configuredMetadata := openAPIObjectAt(t, configuredClosure, "properties", "metadata")
	configuredMetadataAllOf := openAPISequence(t, configuredMetadata["allOf"],
		"ConfiguredRecipeResponse metadata.allOf")
	if len(configuredMetadataAllOf) != 2 {
		t.Fatalf("ConfiguredRecipeResponse metadata.allOf has %d entries, want 2",
			len(configuredMetadataAllOf))
	}
	configuredMetadataConstraint := openAPIObject(t, configuredMetadataAllOf[1],
		"ConfiguredRecipeResponse metadata constraint")
	configuredMetadataNot := openAPIObjectAt(t, configuredMetadataConstraint, "not")
	if !openAPIHasString(
		openAPISequence(t, configuredMetadataNot["required"],
			"ConfiguredRecipeResponse metadata.not.required"),
		"selectedProfile",
	) {

		t.Error("ConfiguredRecipeResponse does not prohibit selectedProfile")
	}
	configuredMetadataNames := openAPISequence(t,
		openAPIObjectAt(t, configuredMetadataConstraint, "propertyNames")["enum"],
		"ConfiguredRecipeResponse metadata.propertyNames.enum")
	for _, field := range []string{
		"version", "appliedOverlays", "excludedOverlays", "constraintWarnings",
		"gpuDriverState", "mariaDBOperatorState", "selectedProfile",
	} {
		if !openAPIHasString(configuredMetadataNames, field) {
			t.Errorf("ConfiguredRecipeResponse metadata does not allow %s", field)
		}
	}
	configuredComponentNames := openAPISequence(t,
		openAPIObjectAt(t, configuredClosure,
			"properties", "componentRefs", "items", "propertyNames")["enum"],
		"ConfiguredRecipeResponse componentRefs propertyNames.enum")
	if !openAPIHasString(configuredComponentNames, "name") ||
		!openAPIHasString(configuredComponentNames, "expectedResources") {

		t.Error("ConfiguredRecipeResponse componentRefs is missing supported fields")
	}
	configuredConstraintNames := openAPISequence(t,
		openAPIObjectAt(t, configuredClosure,
			"properties", "constraints", "items", "propertyNames")["enum"],
		"ConfiguredRecipeResponse constraints propertyNames.enum")
	for _, field := range []string{"name", "value", "severity", "remediation", "unit"} {
		if !openAPIHasString(configuredConstraintNames, field) {
			t.Errorf("ConfiguredRecipeResponse constraints does not allow %s", field)
		}
	}

	profile := openAPIObjectAt(t, schemas, "ProfileRecipeResponse")
	profileAllOf := openAPISequence(t, profile["allOf"], "ProfileRecipeResponse.allOf")
	if len(profileAllOf) != 2 {
		t.Fatalf("ProfileRecipeResponse.allOf has %d entries, want 2", len(profileAllOf))
	}
	profileClosure := openAPIObject(t, profileAllOf[1], "ProfileRecipeResponse closure")
	if closed, ok := profileClosure["additionalProperties"].(bool); !ok || closed {
		t.Errorf("ProfileRecipeResponse additionalProperties = %v, want false", profileClosure["additionalProperties"])
	}
	profileMetadata := openAPIObjectAt(t, profileClosure, "properties", "metadata")
	if !openAPIHasString(
		openAPISequence(t, profileMetadata["required"], "ProfileRecipeResponse metadata.required"),
		"selectedProfile",
	) {

		t.Error("ProfileRecipeResponse metadata does not require selectedProfile")
	}
	profileMetadataNames := openAPISequence(t,
		openAPIObjectAt(t, profileMetadata, "propertyNames")["enum"],
		"ProfileRecipeResponse metadata.propertyNames.enum")
	if !openAPIHasString(profileMetadataNames, "mariaDBOperatorState") {
		t.Error("ProfileRecipeResponse metadata does not allow mariaDBOperatorState")
	}
	profileConfiguration := openAPIObjectAt(t, profileClosure, "properties", "configuration")
	if got := profileConfiguration["$ref"]; got != "#/components/schemas/ConfiguredRecipeConfiguration" {
		t.Errorf("ProfileRecipeResponse configuration = %v, want ConfiguredRecipeConfiguration", got)
	}
	profileExcludedOverlay := openAPIObjectAt(t, profileMetadata,
		"properties", "excludedOverlays", "items", "propertyNames")
	profileExcludedOverlayNames := openAPISequence(t, profileExcludedOverlay["enum"],
		"ProfileRecipeResponse excludedOverlays propertyNames.enum")
	excludedOverlayFields := []string{"name", "reason"}
	if len(profileExcludedOverlayNames) != len(excludedOverlayFields) {
		t.Errorf("ProfileRecipeResponse excludedOverlays allows %d fields, want %d",
			len(profileExcludedOverlayNames), len(excludedOverlayFields))
	}
	for _, field := range excludedOverlayFields {
		if !openAPIHasString(profileExcludedOverlayNames, field) {
			t.Errorf("ProfileRecipeResponse excludedOverlays does not allow %s", field)
		}
	}
	baseExcludedOverlay := openAPIObjectAt(t, schemas, "RecipeResponseBase",
		"properties", "metadata", "properties", "excludedOverlays", "items")
	baseExcludedOverlayBranches := openAPISequence(t, baseExcludedOverlay["oneOf"],
		"RecipeResponseBase excludedOverlays oneOf")
	baseExcludedOverlayTypes := map[any]bool{}
	var baseExcludedOverlayObject map[string]any
	for _, branch := range baseExcludedOverlayBranches {
		branchObject := openAPIObject(t, branch,
			"RecipeResponseBase excludedOverlays oneOf item")
		baseExcludedOverlayTypes[branchObject["type"]] = true
		if branchObject["type"] == "object" {
			baseExcludedOverlayObject = branchObject
		}
	}
	for _, wantType := range []string{"string", "object"} {
		if !baseExcludedOverlayTypes[wantType] {
			t.Errorf("RecipeResponseBase excludedOverlays does not accept %s entries", wantType)
		}
	}
	if baseExcludedOverlayObject == nil {
		t.Fatal("RecipeResponseBase excludedOverlays has no object branch")
	}
	baseExcludedOverlayName := openAPIObjectAt(t, baseExcludedOverlayObject,
		"properties", "name")
	if got := baseExcludedOverlayName["minLength"]; got != 1 {
		t.Errorf("RecipeResponseBase excludedOverlays object name minLength = %v, want 1", got)
	}
	profileConstraintWarning := openAPIObjectAt(t, profileMetadata,
		"properties", "constraintWarnings", "items", "propertyNames")
	profileConstraintWarningNames := openAPISequence(t, profileConstraintWarning["enum"],
		"ProfileRecipeResponse constraintWarnings propertyNames.enum")
	constraintWarningFields := []string{"overlay", "constraint", "expected", "actual", "reason"}
	if len(profileConstraintWarningNames) != len(constraintWarningFields) {
		t.Errorf("ProfileRecipeResponse constraintWarnings allows %d fields, want %d",
			len(profileConstraintWarningNames), len(constraintWarningFields))
	}
	for _, field := range constraintWarningFields {
		if !openAPIHasString(profileConstraintWarningNames, field) {
			t.Errorf("ProfileRecipeResponse constraintWarnings does not allow %s", field)
		}
	}
	componentFieldTypes := map[string]string{
		"name":               "string",
		"namespace":          "string",
		"chart":              "string",
		"type":               "string",
		"source":             "string",
		"version":            "string",
		"tag":                "string",
		"valuesFile":         "string",
		"overrides":          "object",
		"patches":            "array",
		"dependencyRefs":     "array",
		"manifestFiles":      "array",
		"preManifestFiles":   "array",
		"path":               "string",
		"cleanup":            "boolean",
		"expectedResources":  "array",
		"healthCheckAsserts": "string",
		"healthCheckSkip":    "boolean",
	}
	recipeResponseBase := openAPIObjectAt(t, schemas, "RecipeResponseBase")
	componentProperties := openAPIObjectAt(t, recipeResponseBase,
		"properties", "componentRefs", "items", "properties")
	for field, wantType := range componentFieldTypes {
		property := openAPIObjectAt(t, componentProperties, field)
		if got := property["type"]; got != wantType {
			t.Errorf("RecipeResponse componentRefs.%s type = %v, want %s", field, got, wantType)
		}
	}
	profileComponent := openAPIObjectAt(t, profileClosure,
		"properties", "componentRefs", "items")
	profileComponentNames := openAPISequence(t,
		openAPIObjectAt(t, profileComponent, "propertyNames")["enum"],
		"ProfileRecipeResponse componentRefs propertyNames.enum")
	if len(profileComponentNames) != len(componentFieldTypes) {
		t.Errorf("ProfileRecipeResponse componentRefs allows %d fields, want %d",
			len(profileComponentNames), len(componentFieldTypes))
	}
	for field := range componentFieldTypes {
		if !openAPIHasString(profileComponentNames, field) {
			t.Errorf("ProfileRecipeResponse componentRefs does not allow %s", field)
		}
	}
	expectedResource := openAPIObjectAt(t, profileComponent,
		"properties", "expectedResources", "items")
	if closed, ok := expectedResource["additionalProperties"].(bool); !ok || closed {
		t.Errorf("ProfileRecipeResponse expectedResources additionalProperties = %v, want false",
			expectedResource["additionalProperties"])
	}
	expectedResourceProperties := openAPIObjectAt(t, expectedResource, "properties")
	for _, field := range []string{"kind", "name", "namespace"} {
		property := openAPIObjectAt(t, expectedResourceProperties, field)
		if got := property["type"]; got != "string" {
			t.Errorf("ProfileRecipeResponse expectedResources.%s type = %v, want string", field, got)
		}
	}

	// The legacy request branch prohibits selectedProfile and keeps its
	// header enums to the legacy square only — a v1alpha3 artifact must
	// still fail this branch so oneOf matches exactly one.
	legacyBranchMetadata := openAPIObjectAt(t, legacyOverlay,
		"properties", "metadata", "not")
	if !openAPIHasString(
		openAPISequence(t, legacyBranchMetadata["required"],
			"LegacyBundleRecipeRequest metadata.not.required"),
		"selectedProfile",
	) {

		t.Error("LegacyBundleRecipeRequest does not prohibit selectedProfile")
	}
}

// TestOpenAPIProfileTrackParametersAreUniversal asserts the profile-track query
// parameters are declared on both methods of every recipe-resolving endpoint.
//
// This replaces TestOpenAPISlurmAccountingModeIsV2Only, whose whole assertion
// was that slurmAccountingMode appeared on /v2 and not on /v1. Collapsing the
// families deleted the distinction that test measured, and deleting it with its
// premise would have left the parameter with no spec coverage at all — a
// silent drop from the frozen v1 surface would not fail anything.
//
// profile is included for the same reason: it is the other half of what the
// collapse made universal, and it was previously covered only as part of the
// v1-versus-v2 split.
func TestOpenAPIProfileTrackParametersAreUniversal(t *testing.T) {
	specPath := filepath.Join("..", "..", "api", "aicr", "v1", "server.yaml")
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read spec %q: %v", specPath, err)
	}
	var spec map[string]any
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	parameters := []struct {
		name string
		ref  string
	}{
		{name: "slurmAccountingMode", ref: "#/components/parameters/SlurmAccountingMode"},
		{name: "profile", ref: "#/components/parameters/ProfileSelection"},
	}

	for _, path := range []string{"/v1/recipe", "/v1/query"} {
		for _, method := range []string{"get", "post"} {
			for _, want := range parameters {
				t.Run(want.name+" "+method+" "+path, func(t *testing.T) {
					operation := openAPIObjectAt(t, spec, "paths", path, method)
					declared := openAPISequence(t, operation["parameters"],
						"operation.parameters")
					if len(declared) == 0 {
						t.Fatalf("%s %s declares no parameters; the parse shape is "+
							"wrong and this assertion would pass vacuously",
							method, path)
					}

					for _, value := range declared {
						parameter := openAPIObject(t, value, "operation parameter")
						if parameter["name"] == want.name || parameter["$ref"] == want.ref {
							return
						}
					}
					t.Errorf("%s %s does not declare %q; it is part of the frozen v1 "+
						"surface and the handler reads it", method, path, want.name)
				})
			}
		}
	}
}

func openAPIObjectAt(t *testing.T, root map[string]any, path ...string) map[string]any {
	t.Helper()
	current := root
	for _, key := range path {
		current = openAPIObject(t, current[key], key)
	}
	return current
}

func openAPIObject(t *testing.T, value any, label string) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s = %T, want object", label, value)
	}
	return object
}

func openAPISequence(t *testing.T, value any, label string) []any {
	t.Helper()
	sequence, ok := value.([]any)
	if !ok {
		t.Fatalf("%s = %T, want array", label, value)
	}
	return sequence
}

func openAPIHasString(values []any, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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
