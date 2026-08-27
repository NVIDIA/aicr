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
	OneOf      []openAPIContractSchema          `yaml:"oneOf"`
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
			if want := "#/components/schemas/LegacyRecipeResponse"; tt.got != want {
				t.Errorf("$ref = %q, want %q", tt.got, want)
			}
		})
	}

	for _, tt := range []struct {
		name        string
		schema      openAPIContractSchema
		baseRef     string
		required    []string
		apiVersions []string
		kinds       []string
	}{
		{
			name:     "base response",
			schema:   spec.Components.Schemas["RecipeResponse"],
			required: []string{"apiVersion", "kind"},
			// Both emitted alpha versions plus both ADR-022 targets. The
			// targets are staged here a release before the emitter switch so
			// a client generated from this spec does not reject the first
			// response carrying one.
			apiVersions: []string{
				recipe.RecipeAPIVersion,
				recipe.ConfiguredRecipeResultAPIVersion,
				header.GroupVersionV1,
				header.GroupVersionV1Beta2,
			},
			kinds: []string{recipe.RecipeResultKind},
		},
		{
			name:        "profile response",
			schema:      spec.Components.Schemas["ProfileRecipeResponse"],
			baseRef:     "#/components/schemas/RecipeResponse",
			required:    []string{"metadata", "componentRefs"},
			apiVersions: []string{recipe.ConfiguredRecipeResultAPIVersion, header.GroupVersionV1Beta2},
			kinds:       []string{recipe.RecipeResultKind},
		},
		{
			// /v1 responses are narrowed to the default track by
			// LegacyRecipeResponse, which wraps RecipeResponse and drops the
			// profile-track versions from the apiVersion enum. RecipeResponse
			// itself also admits v1alpha3 and v1beta2 for /v2, so asserting on
			// it directly would no longer prove /v1 stays default-track.
			// required and kind come from the RecipeResponse base and are
			// checked there; this case owns the narrowing.
			//
			// The narrowing is by schema track, not by maturity: the ADR-022
			// default target belongs here, because /v1 emits it at the
			// emitter-switch release. Pinning this to v1alpha2 alone would
			// break /v1 against its own published contract at that release,
			// and v1alpha2 is retired one release later.
			name:    "versioned response",
			schema:  spec.Components.Schemas["LegacyRecipeResponse"],
			baseRef: "#/components/schemas/RecipeResponse",
			apiVersions: []string{
				recipe.RecipeAPIVersion,
				header.GroupVersionV1,
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			baseRef := tt.baseRef
			if baseRef == "" {
				baseRef = "#/components/schemas/RecipeResponseBase"
			}
			closure := allOfConstraint(t, tt.schema, baseRef)
			if tt.required != nil && !equalStringsUnordered(closure.Required, tt.required) {
				t.Errorf("required = %v, want %v", closure.Required, tt.required)
			}
			gotAPIVersions := closure.Properties["apiVersion"].Enum
			if !equalStringsUnordered(gotAPIVersions, tt.apiVersions) {
				t.Errorf("apiVersion enum = %v, want %v", gotAPIVersions, tt.apiVersions)
			}
			gotKinds := closure.Properties["kind"].Enum
			if tt.kinds != nil && !equalStringsUnordered(gotKinds, tt.kinds) {
				t.Errorf("kind enum = %v, want %v", gotKinds, tt.kinds)
			}
		})
	}

	criteriaVersions := spec.Components.Schemas["RecipeCriteria"].Properties["apiVersion"].Enum
	wantCriteriaVersions := []string{recipe.RecipeCriteriaAPIVersion, header.GroupVersionV1}
	if !equalStringsUnordered(criteriaVersions, wantCriteriaVersions) {
		t.Errorf("RecipeCriteria apiVersion enum = %v, want %v", criteriaVersions, wantCriteriaVersions)
	}

	bundleRequest := spec.Components.Schemas["BundleRecipeRequest"]
	if len(bundleRequest.OneOf) != 2 {
		t.Fatalf("BundleRecipeRequest.oneOf has %d entries, want 2", len(bundleRequest.OneOf))
	}
	refs := make(map[string]bool, len(bundleRequest.OneOf))
	for _, branch := range bundleRequest.OneOf {
		refs[branch.Ref] = true
	}
	for _, ref := range []string{
		"#/components/schemas/LegacyBundleRecipeV1Request",
		"#/components/schemas/ConfiguredRecipeResponse",
	} {
		if !refs[ref] {
			t.Errorf("BundleRecipeRequest.oneOf does not reference %s", ref)
		}
	}

	// The v1 legacy branch retains every default-track header shape the
	// handler normalizes, while the profile-track branch remains closed.
	legacyBundle := spec.Components.Schemas["LegacyBundleRecipeV1Request"]
	legacyClosure := allOfConstraint(t, legacyBundle, "#/components/schemas/RecipeResponseBase")
	if got, want := legacyClosure.Properties["apiVersion"].Enum,
		[]string{"", recipe.RecipeAPIVersion, header.GroupVersionV1}; !equalStringsUnordered(got, want) {
		t.Errorf("LegacyBundleRecipeV1Request apiVersion enum = %v, want %v", got, want)
	}
	if got, want := legacyClosure.Properties["kind"].Enum,
		[]string{"", string(header.KindRecipe), recipe.RecipeResultKind}; !equalStringsUnordered(got, want) {
		t.Errorf("LegacyBundleRecipeV1Request kind enum = %v, want %v", got, want)
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

func TestOpenAPIV2BundleContract(t *testing.T) {
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
		"paths", "/v2/bundle", "post", "requestBody", "content")
	for _, mediaType := range []string{"application/json", "application/x-yaml"} {
		schema := openAPIObjectAt(t, content, mediaType, "schema")
		if got := schema["$ref"]; got != "#/components/schemas/BundleRecipeV2Request" {
			t.Errorf("%s request schema = %v, want BundleRecipeV2Request", mediaType, got)
		}
	}

	responses := openAPIObjectAt(t, spec, "paths", "/v2/bundle", "post", "responses")
	for _, status := range []string{"401", "404", "503", "504"} {
		if _, ok := responses[status]; !ok {
			t.Errorf("/v2/bundle response %s is not declared", status)
		}
	}

	schemas := openAPIObjectAt(t, spec, "components", "schemas")
	union := openAPIObjectAt(t, schemas, "BundleRecipeV2Request")
	refs := map[string]bool{}
	for _, item := range openAPISequence(t, union["oneOf"], "BundleRecipeV2Request.oneOf") {
		schema := openAPIObject(t, item, "BundleRecipeV2Request.oneOf item")
		ref, _ := schema["$ref"].(string)
		refs[ref] = true
	}
	for _, ref := range []string{
		"#/components/schemas/LegacyBundleRecipeV2Request",
		"#/components/schemas/ProfileRecipeResponse",
		"#/components/schemas/ConfiguredRecipeResponse",
	} {
		if !refs[ref] {
			t.Errorf("BundleRecipeV2Request.oneOf does not reference %s", ref)
		}
	}
	// The strict response schema must NOT be a request branch: reusing it
	// there re-requires kind: RecipeResult and re-rejects the legacy shapes
	// (kind absent/empty) the v2 decode path accepts.
	if refs["#/components/schemas/LegacyRecipeResponse"] {
		t.Error("BundleRecipeV2Request.oneOf must not reuse the strict LegacyRecipeResponse response schema")
	}

	// The legacy request branch covers the whole accepted legacy square:
	// apiVersion absent/""/v1alpha2 × kind absent/""/RecipeResult, headers
	// optional. Anything narrower leaves /v2/bundle stricter than its own
	// server (DecodeRecipeResult enforces kind only when non-empty).
	legacyBranch := openAPIObjectAt(t, schemas, "LegacyBundleRecipeV2Request")
	legacyBranchAllOf := openAPISequence(t, legacyBranch["allOf"], "LegacyBundleRecipeV2Request.allOf")
	legacyOverlay := openAPIObject(t, legacyBranchAllOf[1], "LegacyBundleRecipeV2Request overlay")
	if _, required := legacyOverlay["required"]; required {
		t.Error("LegacyBundleRecipeV2Request must not require header fields")
	}
	legacyAPIVersion := openAPIObjectAt(t, legacyOverlay, "properties", "apiVersion")
	legacyAPIVersions := openAPISequence(t, legacyAPIVersion["enum"],
		"LegacyBundleRecipeV2Request apiVersion enum")
	for _, value := range []string{"", "aicr.run/v1alpha2", header.GroupVersionV1} {
		if !openAPIHasString(legacyAPIVersions, value) {
			t.Errorf("LegacyBundleRecipeV2Request apiVersion enum missing %q", value)
		}
	}
	legacyKind := openAPIObjectAt(t, legacyOverlay, "properties", "kind")
	legacyKinds := openAPISequence(t, legacyKind["enum"],
		"LegacyBundleRecipeV2Request kind enum")
	for _, value := range []string{"", "RecipeResult"} {
		if !openAPIHasString(legacyKinds, value) {
			t.Errorf("LegacyBundleRecipeV2Request kind enum missing %q", value)
		}
	}
	legacyConfigurationNot := openAPIObjectAt(t, legacyOverlay, "not")
	if !openAPIHasString(
		openAPISequence(t, legacyConfigurationNot["required"],
			"LegacyBundleRecipeV2Request not.required"),
		"configuration",
	) {

		t.Error("LegacyBundleRecipeV2Request does not prohibit configuration")
	}
	if _, ok := union["discriminator"]; ok {
		t.Error("BundleRecipeV2Request must not discriminate a versionless branch by apiVersion")
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

	legacy := openAPIObjectAt(t, schemas, "LegacyRecipeResponse")
	legacyAllOf := openAPISequence(t, legacy["allOf"], "LegacyRecipeResponse.allOf")
	if len(legacyAllOf) != 2 {
		t.Fatalf("LegacyRecipeResponse.allOf has %d entries, want 2", len(legacyAllOf))
	}
	legacyMetadata := openAPIObjectAt(t,
		openAPIObject(t, legacyAllOf[1], "LegacyRecipeResponse version schema"),
		"properties", "metadata", "not")
	if !openAPIHasString(
		openAPISequence(t, legacyMetadata["required"], "LegacyRecipeResponse metadata.not.required"),
		"selectedProfile",
	) {

		t.Error("LegacyRecipeResponse does not prohibit selectedProfile")
	}

	// The legacy request branch prohibits selectedProfile and keeps its
	// header enums to the legacy square only — a v1alpha3 artifact must
	// still fail this branch so oneOf matches exactly one.
	legacyBranchMetadata := openAPIObjectAt(t, legacyOverlay,
		"properties", "metadata", "not")
	if !openAPIHasString(
		openAPISequence(t, legacyBranchMetadata["required"],
			"LegacyBundleRecipeV2Request metadata.not.required"),
		"selectedProfile",
	) {

		t.Error("LegacyBundleRecipeV2Request does not prohibit selectedProfile")
	}
}

func TestOpenAPIV1BundleLegacyConfigurationContract(t *testing.T) {
	specPath := filepath.Join("..", "..", "api", "aicr", "v1", "server.yaml")
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read spec %q: %v", specPath, err)
	}
	var spec map[string]any
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	schemas := openAPIObjectAt(t, spec, "components", "schemas")
	bundleRequest := openAPIObjectAt(t, schemas, "BundleRecipeRequest")
	refs := map[string]bool{}
	for _, branchValue := range openAPISequence(t, bundleRequest["oneOf"], "BundleRecipeRequest.oneOf") {
		branch := openAPIObject(t, branchValue, "BundleRecipeRequest.oneOf item")
		ref, _ := branch["$ref"].(string)
		refs[ref] = true
	}
	if len(refs) != 2 || !refs["#/components/schemas/LegacyBundleRecipeV1Request"] ||
		!refs["#/components/schemas/ConfiguredRecipeResponse"] {

		t.Fatalf("BundleRecipeRequest.oneOf refs = %v, want legacy and configured branches", refs)
	}
	if refs["#/components/schemas/ProfileRecipeResponse"] {
		t.Error("BundleRecipeRequest must not admit metadata.selectedProfile on /v1/bundle")
	}

	legacy := openAPIObjectAt(t, schemas, "LegacyBundleRecipeV1Request")
	legacyAllOf := openAPISequence(t, legacy["allOf"], "LegacyBundleRecipeV1Request.allOf")
	if len(legacyAllOf) != 2 {
		t.Fatalf("LegacyBundleRecipeV1Request.allOf has %d entries, want 2", len(legacyAllOf))
	}
	legacyOverlay := openAPIObject(t, legacyAllOf[1], "LegacyBundleRecipeV1Request overlay")
	legacyNot := openAPIObjectAt(t, legacyOverlay, "not")
	if !openAPIHasString(
		openAPISequence(t, legacyNot["required"], "LegacyBundleRecipeV1Request not.required"),
		"configuration",
	) {

		t.Error("LegacyBundleRecipeV1Request does not prohibit configuration")
	}
	legacyMetadataNot := openAPIObjectAt(t, legacyOverlay, "properties", "metadata", "not")
	if !openAPIHasString(openAPISequence(t, legacyMetadataNot["required"],
		"LegacyBundleRecipeV1Request metadata.not.required"), "selectedProfile") {

		t.Error("LegacyBundleRecipeV1Request does not prohibit selectedProfile")
	}

	configured := openAPIObjectAt(t, schemas, "ConfiguredRecipeResponse")
	configuredAllOf := openAPISequence(t, configured["allOf"], "ConfiguredRecipeResponse.allOf")
	configuredClosure := openAPIObject(t, configuredAllOf[1], "ConfiguredRecipeResponse closure")
	if closed, ok := configuredClosure["additionalProperties"].(bool); !ok || closed {
		t.Errorf("ConfiguredRecipeResponse additionalProperties = %v, want false",
			configuredClosure["additionalProperties"])
	}
	if !openAPIHasString(openAPISequence(t, configuredClosure["required"],
		"ConfiguredRecipeResponse.required"), "configuration") {

		t.Error("ConfiguredRecipeResponse does not require configuration")
	}
	configuredVersions := openAPISequence(t,
		openAPIObjectAt(t, configuredClosure, "properties", "apiVersion")["enum"],
		"ConfiguredRecipeResponse apiVersion enum")
	for _, version := range []string{recipe.ConfiguredRecipeResultAPIVersion, header.GroupVersionV1Beta2} {
		if !openAPIHasString(configuredVersions, version) {
			t.Errorf("ConfiguredRecipeResponse apiVersion enum missing %q", version)
		}
	}
	configuredMetadata := openAPIObjectAt(t, configuredClosure, "properties", "metadata")
	configuredMetadataAllOf := openAPISequence(t, configuredMetadata["allOf"],
		"ConfiguredRecipeResponse metadata.allOf")
	configuredMetadataConstraint := openAPIObject(t, configuredMetadataAllOf[1],
		"ConfiguredRecipeResponse metadata constraint")
	configuredMetadataNot := openAPIObjectAt(t, configuredMetadataConstraint, "not")
	if !openAPIHasString(openAPISequence(t, configuredMetadataNot["required"],
		"ConfiguredRecipeResponse metadata.not.required"), "selectedProfile") {

		t.Error("ConfiguredRecipeResponse does not prohibit selectedProfile")
	}
	configurationProperty := openAPIObjectAt(t, configuredClosure, "properties", "configuration")
	configurationPropertyAllOf := openAPISequence(t, configurationProperty["allOf"],
		"ConfiguredRecipeResponse configuration.allOf")
	var configurationRefs, configurationConstraints []map[string]any
	for _, value := range configurationPropertyAllOf {
		entry := openAPIObject(t, value, "ConfiguredRecipeResponse configuration.allOf entry")
		if entry["$ref"] == "#/components/schemas/ConfiguredRecipeConfiguration" {
			configurationRefs = append(configurationRefs, entry)
		} else {
			configurationConstraints = append(configurationConstraints, entry)
		}
	}
	if len(configurationRefs) != 1 || len(configurationConstraints) != 1 {
		t.Fatalf("ConfiguredRecipeResponse configuration.allOf has %d schema refs and %d constraints, want 1 each",
			len(configurationRefs), len(configurationConstraints))
	}
	if !openAPIHasString(openAPISequence(t, configurationConstraints[0]["required"],
		"ConfiguredRecipeResponse configuration required"), "slurm") {

		t.Error("ConfiguredRecipeResponse configuration does not require slurm")
	}
	configuration := openAPIObjectAt(t, schemas, "ConfiguredRecipeConfiguration")
	configurationAllOf := openAPISequence(t, configuration["allOf"],
		"ConfiguredRecipeConfiguration.allOf")
	configurationClosure := openAPIObject(t, configurationAllOf[1],
		"ConfiguredRecipeConfiguration closure")
	if closed, ok := configurationClosure["additionalProperties"].(bool); !ok || closed {
		t.Errorf("ConfiguredRecipeConfiguration additionalProperties = %v, want false",
			configurationClosure["additionalProperties"])
	}
}

func TestOpenAPISlurmAccountingModeIsV2Only(t *testing.T) {
	specPath := filepath.Join("..", "..", "api", "aicr", "v1", "server.yaml")
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read spec %q: %v", specPath, err)
	}
	var spec map[string]any
	if err := yaml.Unmarshal(data, &spec); err != nil {
		t.Fatalf("parse spec: %v", err)
	}

	tests := []struct {
		path string
		want bool
	}{
		{path: "/v1/recipe"},
		{path: "/v1/query"},
		{path: "/v2/recipe", want: true},
		{path: "/v2/query", want: true},
	}
	for _, tt := range tests {
		for _, method := range []string{"get", "post"} {
			t.Run(tt.path+" "+method, func(t *testing.T) {
				operation := openAPIObjectAt(t, spec, "paths", tt.path, method)
				parameters := openAPISequence(t, operation["parameters"], "operation.parameters")
				found := false
				for _, value := range parameters {
					parameter := openAPIObject(t, value, "operation parameter")
					if parameter["name"] == "slurmAccountingMode" ||
						parameter["$ref"] == "#/components/parameters/SlurmAccountingMode" {

						found = true
					}
				}
				if found != tt.want {
					t.Errorf("slurmAccountingMode declared = %v, want %v", found, tt.want)
				}
			})
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
