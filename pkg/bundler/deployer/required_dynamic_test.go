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

package deployer

import (
	"context"
	"reflect"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/recipe/providertest"
)

const requiredDynamicFixture = "required-dynamic-fixture"

func requiredDynamicFixtureProvider(t *testing.T) recipe.DataProvider {
	t.Helper()
	registryYAML := []byte(`apiVersion: aicr.run/v1beta1
kind: ComponentRegistry
components:
  - name: ` + requiredDynamicFixture + `
    displayName: Required Dynamic Fixture
    requiredValues:
      - controlPlane.url
      - controlPlane.token
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

func TestResolveRequiredDynamicPaths(t *testing.T) {
	provider := requiredDynamicFixtureProvider(t)
	refs := []recipe.ComponentRef{
		{Name: requiredDynamicFixture, Type: recipe.ComponentTypeHelm},
		{Name: "gpu-operator", Type: recipe.ComponentTypeHelm},
	}

	tests := []struct {
		name    string
		dynamic map[string][]string
		want    map[string][]string
	}{
		{"no dynamic values", nil, map[string][]string{}},
		{"exact required path", map[string][]string{requiredDynamicFixture: {"controlPlane.token"}}, map[string][]string{requiredDynamicFixture: {"controlPlane.token"}}},
		{"ancestor declaration yields every required leaf beneath it", map[string][]string{requiredDynamicFixture: {"controlPlane"}}, map[string][]string{requiredDynamicFixture: {"controlPlane.url", "controlPlane.token"}}},
		{"descendant declaration yields the declaration itself", map[string][]string{requiredDynamicFixture: {"controlPlane.token.value"}}, map[string][]string{requiredDynamicFixture: {"controlPlane.token.value"}}},
		{"overlapping declarations are not repeated", map[string][]string{requiredDynamicFixture: {"controlPlane", "controlPlane.token"}}, map[string][]string{requiredDynamicFixture: {"controlPlane.url", "controlPlane.token"}}},
		{"unrelated dynamic path is dropped", map[string][]string{requiredDynamicFixture: {"tolerations"}, "gpu-operator": {"driver.version"}}, map[string][]string{}},
		{"mixed keeps only the required ones", map[string][]string{requiredDynamicFixture: {"tolerations", "controlPlane.url"}}, map[string][]string{requiredDynamicFixture: {"controlPlane.url"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveRequiredDynamicPaths(context.Background(), provider, refs, tt.dynamic)
			if err != nil {
				t.Fatalf("ResolveRequiredDynamicPaths: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveRequiredDynamicPaths_RegistryFailureIsFatal(t *testing.T) {
	t.Parallel()
	_, err := ResolveRequiredDynamicPaths(context.Background(), failingProvider{},
		[]recipe.ComponentRef{{Name: requiredDynamicFixture}}, map[string][]string{requiredDynamicFixture: {"controlPlane.token"}})
	if err == nil {
		t.Fatal("expected an error when the registry cannot be loaded")
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
}

func TestResolveRequiredDynamicPaths_ContextCancelled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ResolveRequiredDynamicPaths(ctx, failingProvider{},
		[]recipe.ComponentRef{{Name: requiredDynamicFixture}}, map[string][]string{requiredDynamicFixture: {"x"}})
	if err == nil {
		t.Fatal("expected an error from a cancelled context")
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeTimeout)
}
