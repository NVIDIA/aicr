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

package bundler

import (
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/recipe/providertest"
)

// Two synthetic registry components, merged in through a
// LayeredDataProvider so the cases never depend on which real components
// declare requiredValues. requiredSecretFixture requires a plain value and a
// secret; optionalSecretFixture has a secret nothing requires.
const (
	requiredSecretFixture = "required-secret-fixture"
	optionalSecretFixture = "optional-secret-fixture"
)

func requiredValuesFixtureProvider(t *testing.T) recipe.DataProvider {
	t.Helper()
	registryYAML := []byte(`apiVersion: aicr.run/v1beta1
kind: ComponentRegistry
components:
  - name: ` + requiredSecretFixture + `
    displayName: Required Secret Fixture
    valueOverrideKeys: [rsf]
    requiredValues:
      - controlPlane.url
      - controlPlane.token
    installTimeSecretPaths:
      - controlPlane.token
  - name: ` + optionalSecretFixture + `
    displayName: Optional Secret Fixture
    installTimeSecretPaths:
      - auth.password
`)
	tmpData := providertest.WriteDir(t, map[string][]byte{recipe.RegistryFileName: registryYAML})
	embedded := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	layered, err := recipe.NewLayeredDataProvider(embedded, recipe.LayeredProviderConfig{ExternalDir: tmpData})
	if err != nil {
		t.Fatalf("NewLayeredDataProvider: %v", err)
	}
	recipe.EvictCachedRegistry(layered)
	t.Cleanup(func() { recipe.EvictCachedRegistry(layered) })
	return layered
}

func TestValidateRequiredAndSecretValues(t *testing.T) {
	provider := requiredValuesFixtureProvider(t)

	tests := []struct {
		name      string
		component string
		values    map[string]any
		dynamic   map[string][]string
		wantErr   []string // substrings; nil means no error
	}{
		{
			name:      "nothing supplied names every path with the right hint",
			component: requiredSecretFixture,
			values:    map[string]any{"controlPlane": map[string]any{"url": "", "token": ""}},
			wantErr:   []string{"required values not set", "controlPlane.url (pass --set rsf:<path>=<value>", "controlPlane.token (an install-time secret: declare --dynamic rsf:<path>"},
		},
		{
			name:      "set value and dynamic secret satisfy both",
			component: requiredSecretFixture,
			values:    map[string]any{"controlPlane": map[string]any{"url": "https://cp.example", "token": ""}},
			dynamic:   map[string][]string{"rsf": {"controlPlane.token"}},
		},
		{
			name:      "dynamic declaration on an ancestor covers the required path",
			component: requiredSecretFixture,
			values:    map[string]any{},
			dynamic:   map[string][]string{requiredSecretFixture: {"controlPlane"}},
		},
		{
			name:      "secret written by an override is rejected even when declared dynamic",
			component: requiredSecretFixture,
			values:    map[string]any{"controlPlane": map[string]any{"url": "https://cp.example", "token": "s3cret"}},
			dynamic:   map[string][]string{"rsf": {"controlPlane.token"}},
			wantErr:   []string{"\"controlPlane.token\" is an install-time secret and must not be set", "--dynamic rsf:controlPlane.token"},
		},
		{
			name:      "whitespace counts as empty",
			component: requiredSecretFixture,
			values:    map[string]any{"controlPlane": map[string]any{"url": "  ", "token": ""}},
			dynamic:   map[string][]string{"rsf": {"controlPlane.token"}},
			wantErr:   []string{"required values not set: controlPlane.url"},
		},
		{
			name:      "optional secret left empty passes",
			component: optionalSecretFixture,
			values:    map[string]any{"auth": map[string]any{"password": ""}},
		},
		{
			name:      "optional secret set by an override is rejected",
			component: optionalSecretFixture,
			values:    map[string]any{"auth": map[string]any{"password": "hunter2"}},
			wantErr:   []string{"\"auth.password\" is an install-time secret", "--dynamic " + optionalSecretFixture + ":auth.password"},
		},
		{
			name:      "component without declarations is not judged",
			component: "gpu-operator",
			values:    map[string]any{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New(WithConfig(config.NewConfig(config.WithDynamicValues(tt.dynamic))))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			err = b.validateRequiredAndSecretValues(tt.component, tt.values, provider)
			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), want)
				}
			}
		})
	}
}

func TestRejectInstallTimeSecretsOutsideHelm(t *testing.T) {
	provider := requiredValuesFixtureProvider(t)
	refs := func(names ...string) []recipe.ComponentRef {
		out := make([]recipe.ComponentRef, 0, len(names))
		for _, n := range names {
			out = append(out, recipe.ComponentRef{Name: n, Type: recipe.ComponentTypeHelm})
		}
		return out
	}

	tests := []struct {
		name     string
		deployer config.DeployerType
		refs     []recipe.ComponentRef
		dynamic  map[string][]string
		wantErr  string
	}{
		{"helm carries a required secret", config.DeployerHelm, refs(requiredSecretFixture), map[string][]string{"rsf": {"controlPlane.token"}}, ""},
		{"flux refuses a required secret before it is declared", config.DeployerFlux, refs(requiredSecretFixture), nil, "only the helm deployer supports"},
		{"fleet refuses a declared optional secret", config.DeployerFleet, refs(optionalSecretFixture), map[string][]string{optionalSecretFixture: {"auth.password"}}, "\"auth.password\" is an install-time secret"},
		{"terraform ships an undeclared optional secret as an empty stub", config.DeployerTerraform, refs(optionalSecretFixture), nil, ""},
		{"argocd-helm ignores components without secrets", config.DeployerArgoCDHelm, refs("gpu-operator"), map[string][]string{"gpu-operator": {"driver.version"}}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New(WithConfig(config.NewConfig(
				config.WithDeployer(tt.deployer),
				config.WithDynamicValues(tt.dynamic),
			)))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			err = b.rejectInstallTimeSecretsOutsideHelm(tt.refs, provider)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			if !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), string(tt.deployer)) {
				t.Errorf("error = %q, want it to contain %q and name deployer %q", err.Error(), tt.wantErr, tt.deployer)
			}
		})
	}
}

func TestIsEmptyValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		v    any
		want bool
	}{
		{"nil", nil, true},
		{"empty string", "", true},
		{"blank string", " \t", true},
		{"string", "x", false},
		{"empty map", map[string]any{}, true},
		{"map", map[string]any{"a": 1}, false},
		{"empty slice", []any{}, true},
		{"slice", []any{"a"}, false},
		{"false is a setting", false, false},
		{"zero is a setting", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isEmptyValue(tt.v); got != tt.want {
				t.Errorf("isEmptyValue(%#v) = %v, want %v", tt.v, got, tt.want)
			}
		})
	}
}
