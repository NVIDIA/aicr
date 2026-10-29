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

package aicrjob

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// Golden fixtures were produced by each supported AICR minor's pinned image
// for the lane criteria (see testdata/<version>/README.md): the lowest
// supported release and the newest.
var fixtureVersions = []string{"v1.0.0", "v1.1.2"}

// Lane recipe fixtures, the same for every fixture version.
var laneFixtures = map[string]struct {
	file       string
	apiVersion string
	criteria   Criteria
}{
	"eks": {"recipe-h100-eks-ubuntu-training-kubeflow.json", "aicr.run/v1",
		Criteria{Service: "eks", Accelerator: "h100", Intent: "training", OS: "ubuntu", Platform: "kubeflow"}},
	"gke": {"recipe-h100-gke-cos-training-kubeflow.json", "aicr.run/v1beta2",
		Criteria{Service: "gke", Accelerator: "h100", Intent: "training", OS: "cos", Platform: "kubeflow"}},
}

// fixture reads a v1.0.0 fixture.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	return fixtureAt(t, "v1.0.0", name)
}

// fixtureAt reads a fixture produced by an AICR version.
func fixtureAt(t *testing.T, version, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", version, name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func TestParseLaneFixtures(t *testing.T) {
	t.Parallel()
	for _, version := range fixtureVersions {
		for lane, f := range laneFixtures {
			t.Run(version+"/"+lane, func(t *testing.T) {
				t.Parallel()
				parseLaneFixture(t, version, f.file, f.apiVersion, f.criteria)
			})
		}
	}
}

func parseLaneFixture(t *testing.T, version, file, apiVersion string, criteria Criteria) {
	t.Helper()
	r, err := ParseRecipeResult(fixtureAt(t, version, file), version)
	if err != nil {
		t.Fatalf("ParseRecipeResult() = %v", err)
	}
	if r.APIVersion != apiVersion || *r.Criteria != criteria {
		t.Errorf("apiVersion=%s criteria=%+v", r.APIVersion, *r.Criteria)
	}
	if len(r.ComponentRefs) != 16 {
		t.Errorf("components = %d, want 16", len(r.ComponentRefs))
	}
	var gpu *ComponentRef
	for i := range r.ComponentRefs {
		if r.ComponentRefs[i].Name == "gpu-operator" {
			gpu = &r.ComponentRefs[i]
		}
	}
	if gpu == nil || gpu.Chart == "" || gpu.Version == "" || len(gpu.DependencyRefs) == 0 {
		t.Errorf("gpu-operator component = %+v", gpu)
	}
}

func TestParseRejectsUnsupportedArtifacts(t *testing.T) {
	t.Parallel()
	base := fixture(t, laneFixtures["eks"].file)
	edit := func(f func(map[string]any)) []byte {
		var m map[string]any
		if err := json.Unmarshal(base, &m); err != nil {
			t.Fatal(err)
		}
		f(m)
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	tests := []struct {
		name   string
		data   []byte
		reason string
	}{
		{"unknown apiVersion", edit(func(m map[string]any) { m["apiVersion"] = "aicr.run/v2" }), ReasonArtifactUnsupported},
		{"wrong kind", edit(func(m map[string]any) { m["kind"] = "Snapshot" }), ReasonArtifactUnsupported},
		{"missing deploymentOrder", edit(func(m map[string]any) { delete(m, "deploymentOrder") }), ReasonArtifactUnsupported},
		{"null componentRefs", edit(func(m map[string]any) { m["componentRefs"] = nil }), ReasonArtifactUnsupported},
		{"unknown dependency", edit(func(m map[string]any) {
			c := m["componentRefs"].([]any)[0].(map[string]any)
			c["dependencyRefs"] = []any{"no-such-component"}
		}), ReasonArtifactUnsupported},
		{"order mismatch", edit(func(m map[string]any) { m["deploymentOrder"] = []any{"gpu-operator"} }), ReasonArtifactUnsupported},
		{"no release version", edit(func(m map[string]any) { delete(m["metadata"].(map[string]any), "version") }), ReasonVersionMismatch},
		{"other release version", edit(func(m map[string]any) { m["metadata"].(map[string]any)["version"] = "1.1.0" }), ReasonVersionMismatch},
		{"not JSON", []byte("apiVersion: aicr.run/v1"), ReasonArtifactUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseRecipeResult(tt.data, "v1.0.0")
			se, ok := err.(*operrors.StructuredError) //nolint:errorlint // returned directly
			if !ok || se.Code != operrors.ErrCodeInvalidRequest || se.Context["reason"] != tt.reason {
				t.Fatalf("ParseRecipeResult() = %v, want INVALID_REQUEST with %s", err, tt.reason)
			}
		})
	}
}

// AICR's additive rules: a field AICR adds later is ignored.
func TestParseIgnoresAddedFields(t *testing.T) {
	t.Parallel()
	var m map[string]any
	if err := json.Unmarshal(fixture(t, laneFixtures["eks"].file), &m); err != nil {
		t.Fatal(err)
	}
	m["futureField"] = map[string]any{"x": 1}
	m["componentRefs"].([]any)[0].(map[string]any)["futureComponentField"] = true
	data, _ := json.Marshal(m)
	if _, err := ParseRecipeResult(data, "v1.0.0"); err != nil {
		t.Fatalf("an added field broke parsing: %v", err)
	}
}

// Contract test against AICR's published schema: every field the operator
// reads exists in the schema with the same name, and every field the schema
// requires is one the operator reads.
func TestRecipeResultMatchesAICRSchema(t *testing.T) {
	t.Parallel()
	for _, version := range fixtureVersions {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			matchRecipeResultSchema(t, fixtureAt(t, version, filepath.Join("schemas", "RecipeResult.schema.json")))
		})
	}
}

func matchRecipeResultSchema(t *testing.T, data []byte) {
	t.Helper()
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("read schema: %v", err)
	}
	ours := jsonFields(reflect.TypeFor[RecipeResult]())
	for _, f := range ours {
		if _, ok := schema.Properties[f]; !ok {
			t.Errorf("operator reads %q, which AICR's schema does not define", f)
		}
	}
	for _, f := range schema.Required {
		if !contains(ours, f) {
			t.Errorf("AICR's schema requires %q, which the operator does not read", f)
		}
	}
	var component struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	var refs struct {
		Items json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(schema.Properties["componentRefs"], &refs); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(refs.Items, &component); err != nil {
		t.Fatal(err)
	}
	for _, f := range jsonFields(reflect.TypeFor[ComponentRef]()) {
		if _, ok := component.Properties[f]; !ok {
			t.Errorf("operator reads componentRefs[].%s, which AICR's schema does not define", f)
		}
	}
}

func jsonFields(t reflect.Type) []string {
	var out []string
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// The evidence name follows AICR's RecipeNameFor: the set criteria in a fixed
// order, "recipe" when none is set, and the selected profile for a profiled
// recipe, as GKE's gpuStack=gke-default (found on the GKE H100 lane).
func TestEvidenceName(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		recipe RecipeResult
		want   string
	}{
		"criteria order": {RecipeResult{Criteria: &Criteria{Service: "eks", Accelerator: "h100", OS: "ubuntu", Intent: "training", Platform: "kubeflow"}},
			"h100-eks-ubuntu-training-kubeflow"},
		"wildcards and gaps": {RecipeResult{Criteria: &Criteria{Service: "any", Accelerator: "h100", Intent: "inference"}}, "h100-inference"},
		"no criteria":        {RecipeResult{Criteria: &Criteria{Service: "any"}}, "recipe"},
		"profiled": {RecipeResult{
			Metadata: RecipeMetadata{SelectedProfile: &SelectedProfile{Name: "gpuStack", Value: "gke-default"}},
			Criteria: &Criteria{Service: "gke", Accelerator: "h100", OS: "cos", Intent: "training", Platform: "kubeflow"},
		}, "h100-gke-cos-training-kubeflow-gpustack-gke-default"},
	} {
		if got := tc.recipe.EvidenceName(); got != tc.want {
			t.Errorf("%s: EvidenceName() = %q, want %q", name, got, tc.want)
		}
	}
}
