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

package recipe

import (
	"reflect"
	"slices"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

func TestBuildSummaryNil(t *testing.T) {
	got, err := BuildSummary(nil)
	if got != nil {
		t.Errorf("BuildSummary(nil) = %v, want nil summary", got)
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
}

func TestBuildSummaryIdentity(t *testing.T) {
	tests := []struct {
		name       string
		meta       RecipeResultMetadata
		wantDriver string
		wantProf   string
	}{
		{name: "driver unknown when empty", wantDriver: "unknown"},
		{
			name:       "driver preinstalled",
			meta:       RecipeResultMetadata{GPUDriverState: GPUDriverStatePreinstalled},
			wantDriver: GPUDriverStatePreinstalled,
		},
		{
			name:       "driver absent",
			meta:       RecipeResultMetadata{GPUDriverState: GPUDriverStateAbsent},
			wantDriver: GPUDriverStateAbsent,
		},
		{
			name: "selected profile",
			meta: RecipeResultMetadata{
				SelectedProfile: &SelectedProfile{Name: "gpuStack", Value: "operator-managed"},
			},
			wantDriver: "unknown",
			wantProf:   "gpuStack=operator-managed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.meta.Version = "v1.2.3"
			tt.meta.AppliedOverlays = []string{"base", "leaf"}
			r := &RecipeResult{
				APIVersion: "aicr.run/v1",
				Kind:       "RecipeResult",
				Metadata:   tt.meta,
				Criteria:   &Criteria{Service: CriteriaServiceEKS, Accelerator: CriteriaAcceleratorH100, Nodes: 4},
			}
			got, err := BuildSummary(r)
			if err != nil {
				t.Fatalf("BuildSummary() error = %v", err)
			}
			if got.APIVersion != RecipeSummaryAPIVersion || got.Kind != RecipeSummaryKind {
				t.Errorf("summary header = %q %q, want %q %q", got.APIVersion, got.Kind, RecipeSummaryAPIVersion, RecipeSummaryKind)
			}
			if want := (SummarySource{APIVersion: "aicr.run/v1", Kind: "RecipeResult"}); got.Source != want || got.Version != "v1.2.3" {
				t.Errorf("source = %+v version = %q, want %+v v1.2.3", got.Source, got.Version, want)
			}
			if got.GPUDriver != tt.wantDriver {
				t.Errorf("GPUDriver = %q, want %q", got.GPUDriver, tt.wantDriver)
			}
			if got.Profile != tt.wantProf {
				t.Errorf("Profile = %q, want %q", got.Profile, tt.wantProf)
			}
			if got.Criteria != *r.Criteria {
				t.Errorf("Criteria = %+v, want %+v", got.Criteria, *r.Criteria)
			}
			if !slices.Equal(got.AppliedOverlays, []string{"base", "leaf"}) {
				t.Errorf("AppliedOverlays = %v", got.AppliedOverlays)
			}
		})
	}
}

func TestBuildSummaryMinimal(t *testing.T) {
	got, err := BuildSummary(&RecipeResult{})
	if err != nil {
		t.Fatalf("BuildSummary() error = %v", err)
	}
	if len(got.Components) != 0 || len(got.Constraints) != 0 || len(got.Validation) != 0 || got.Profile != "" {
		t.Errorf("minimal recipe summary has content: %+v", got)
	}
	if got.Criteria != (Criteria{}) {
		t.Errorf("Criteria = %+v, want zero value for a recipe without criteria", got.Criteria)
	}
	if got.Kind != RecipeSummaryKind || got.Source != (SummarySource{}) {
		t.Errorf("kind = %q source = %+v, want %q and an empty source", got.Kind, got.Source, RecipeSummaryKind)
	}
}

func TestBuildSummaryConstraints(t *testing.T) {
	r := &RecipeResult{Constraints: []Constraint{
		{Name: "K8s.server.version", Value: ">= 1.32.4", Severity: "error"},
		{Name: "OS.release.ID", Value: "ubuntu"},
	}}
	got, err := BuildSummary(r)
	if err != nil {
		t.Fatalf("BuildSummary() error = %v", err)
	}
	want := []SummaryConstraint{
		{Name: "K8s.server.version", Value: ">= 1.32.4"},
		{Name: "OS.release.ID", Value: "ubuntu"},
	}
	if !reflect.DeepEqual(got.Constraints, want) {
		t.Errorf("Constraints = %+v, want %+v", got.Constraints, want)
	}
}

func TestBuildSummaryComponents(t *testing.T) {
	refs := []ComponentRef{
		{Name: "a", Type: ComponentTypeHelm, Version: "1.0.0", Namespace: "ns-a"},
		{Name: "b", Type: ComponentTypeKustomize, Tag: "v2.0.0", DependencyRefs: []string{"a"}},
		{Name: "c", Type: ComponentTypeHelm, Version: "3.0.0", Namespace: "ns-c", DependencyRefs: []string{"a", "b"}},
	}
	names := func(cs []SummaryComponent) []string {
		out := make([]string, len(cs))
		for i, c := range cs {
			out[i] = c.Name
		}
		return out
	}

	tests := []struct {
		name  string
		order []string
		want  []string
	}{
		{name: "deployment order wins", order: []string{"c", "a", "b"}, want: []string{"c", "a", "b"}},
		{name: "missing order falls back to component order", order: nil, want: []string{"a", "b", "c"}},
		{name: "unlisted components follow in component order", order: []string{"c"}, want: []string{"c", "a", "b"}},
		{name: "unknown and repeated names are skipped", order: []string{"zzz", "b", "b"}, want: []string{"b", "a", "c"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildSummary(&RecipeResult{ComponentRefs: refs, DeploymentOrder: tt.order})
			if err != nil {
				t.Fatalf("BuildSummary() error = %v", err)
			}
			if !slices.Equal(names(got.Components), tt.want) {
				t.Errorf("component order = %v, want %v", names(got.Components), tt.want)
			}
		})
	}

	got, err := BuildSummary(&RecipeResult{ComponentRefs: refs})
	if err != nil {
		t.Fatalf("BuildSummary() error = %v", err)
	}
	want := []SummaryComponent{
		{Name: "a", Type: "Helm", Version: "1.0.0", Namespace: "ns-a"},
		{Name: "b", Type: "Kustomize", Version: "v2.0.0", DependsOn: []string{"a"}},
		{Name: "c", Type: "Helm", Version: "3.0.0", Namespace: "ns-c", DependsOn: []string{"a", "b"}},
	}
	if !reflect.DeepEqual(got.Components, want) {
		t.Errorf("Components = %+v, want %+v (kustomize tag shown as version)", got.Components, want)
	}
}

func TestBuildSummaryOmitsDisabledComponents(t *testing.T) {
	refs := []ComponentRef{
		{Name: "cert-manager", Overrides: map[string]any{"enabled": false}},
		{Name: "nfd", Overrides: map[string]any{"install": false}},
		{Name: "gpu-operator", DependencyRefs: []string{"cert-manager", "nfd", "external-crds"}},
		{Name: "kai-scheduler", Overrides: map[string]any{"enabled": true}, DependencyRefs: []string{"gpu-operator", "nfd"}},
	}
	want := []SummaryComponent{
		// The undeclared dependency stays; edges to disabled components are pruned.
		{Name: "gpu-operator", DependsOn: []string{"external-crds"}},
		{Name: "kai-scheduler", DependsOn: []string{"gpu-operator"}},
	}

	tests := []struct {
		name  string
		order []string
	}{
		{name: "deployment order of enabled components", order: []string{"gpu-operator", "kai-scheduler"}},
		{name: "disabled names in the order are skipped", order: []string{"nfd", "gpu-operator", "cert-manager", "kai-scheduler"}},
		{name: "no deployment order", order: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildSummary(&RecipeResult{ComponentRefs: refs, DeploymentOrder: tt.order})
			if err != nil {
				t.Fatalf("BuildSummary() error = %v", err)
			}
			if !reflect.DeepEqual(got.Components, want) {
				t.Errorf("Components = %+v, want %+v", got.Components, want)
			}
		})
	}
}

func TestBuildSummaryDoesNotAliasRecipe(t *testing.T) {
	r := &RecipeResult{
		Metadata:      RecipeResultMetadata{AppliedOverlays: []string{"base"}},
		ComponentRefs: []ComponentRef{{Name: "a", DependencyRefs: []string{"x"}}},
		Validation:    &ValidationConfig{Readiness: &ValidationPhase{Checks: []string{"c1"}}},
	}
	got, err := BuildSummary(r)
	if err != nil {
		t.Fatalf("BuildSummary() error = %v", err)
	}
	got.AppliedOverlays[0] = "changed"
	got.Components[0].DependsOn[0] = "changed"
	got.Validation[0].Checks[0] = "changed"
	if r.Metadata.AppliedOverlays[0] != "base" || r.ComponentRefs[0].DependencyRefs[0] != "x" ||
		r.Validation.Readiness.Checks[0] != "c1" {

		t.Errorf("mutating the summary changed the recipe: %+v", r)
	}
}

func TestBuildSummaryValidation(t *testing.T) {
	tests := []struct {
		name string
		v    *ValidationConfig
		want []SummaryPhase
	}{
		{name: "no validation block", v: nil, want: nil},
		{name: "phases without checks are omitted", v: &ValidationConfig{
			Readiness:  &ValidationPhase{Timeout: "5m"},
			Deployment: &ValidationPhase{},
		}, want: nil},
		{name: "fixed phase order", v: &ValidationConfig{
			Conformance: &ValidationPhase{Checks: []string{"conf"}},
			Performance: &ValidationPhase{Checks: []string{"perf1", "perf2"}},
			Deployment:  &ValidationPhase{Checks: []string{"dep"}},
			Readiness:   &ValidationPhase{Checks: []string{"ready"}},
		}, want: []SummaryPhase{
			{Name: SummaryPhaseReadiness, Checks: []string{"ready"}},
			{Name: SummaryPhaseDeployment, Checks: []string{"dep"}},
			{Name: SummaryPhaseConformance, Checks: []string{"conf"}},
			{Name: SummaryPhasePerformance, Checks: []string{"perf1", "perf2"}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildSummary(&RecipeResult{Validation: tt.v})
			if err != nil {
				t.Fatalf("BuildSummary() error = %v", err)
			}
			if !reflect.DeepEqual(got.Validation, tt.want) {
				t.Errorf("Validation = %+v, want %+v", got.Validation, tt.want)
			}
		})
	}
}

func TestBuildSummaryComponentVersionUsesType(t *testing.T) {
	tests := []struct {
		name string
		ref  ComponentRef
		want string
	}{
		{"helm", ComponentRef{Type: ComponentTypeHelm, Version: "1.0.0"}, "1.0.0"},
		{"kustomize", ComponentRef{Type: ComponentTypeKustomize, Tag: "v2.0.0"}, "v2.0.0"},
		{"unversioned helm", ComponentRef{Type: ComponentTypeHelm}, ""},
		{"untagged kustomize", ComponentRef{Type: ComponentTypeKustomize}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildSummary(&RecipeResult{ComponentRefs: []ComponentRef{tt.ref}})
			if err != nil {
				t.Fatal(err)
			}
			if got.Components[0].Version != tt.want {
				t.Errorf("Version = %q, want %q", got.Components[0].Version, tt.want)
			}
		})
	}
}
