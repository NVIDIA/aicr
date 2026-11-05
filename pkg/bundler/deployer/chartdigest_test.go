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
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/deployer/deployertest"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const testChartDigest = "sha256:5e0f2b7c9a1d3e5f7a9b1c3d5e7f9a1b3c5d7e9f1a3b5c7d9e1f3a5b7c9d1e3f"

// registryRef resolves a ref for name against dp's registry pin.
func registryRef(t *testing.T, dp recipe.DataProvider, name string) recipe.ComponentRef {
	t.Helper()
	registry, err := recipe.GetComponentRegistryFor(dp)
	if err != nil {
		t.Fatalf("GetComponentRegistryFor: %v", err)
	}
	cfg := registry.Get(name)
	if cfg == nil {
		t.Fatalf("component %q is not in the test registry", name)
	}
	ref := recipe.ComponentRef{Name: name, Type: recipe.ComponentTypeHelm}
	ref.ApplyRegistryDefaults(cfg)
	return ref
}

func TestResolveChartDigests(t *testing.T) {
	dp := deployertest.DigestDataProvider(t, testChartDigest)
	pinned := registryRef(t, dp, deployertest.DigestedComponent)

	// Each mutator breaks one coordinate of the pinned chart. The digest
	// identifies the pinned archive only, so every one must get none.
	tests := []struct {
		name string
		ref  recipe.ComponentRef
		want string
	}{
		{name: "registry-resolved ref gets the pinned digest", ref: pinned, want: testChartDigest},
		{
			name: "version override",
			ref:  func() recipe.ComponentRef { r := pinned; r.Version = "9.9.9"; return r }(),
		},
		{
			name: "source override",
			ref:  func() recipe.ComponentRef { r := pinned; r.Source = "https://charts.example.invalid"; return r }(),
		},
		{
			name: "chart override",
			ref:  func() recipe.ComponentRef { r := pinned; r.Chart = "other-chart"; return r }(),
		},
		{name: "registry entry without a digest", ref: registryRef(t, dp, deployertest.UndigestedComponent)},
		{name: "component absent from the registry", ref: recipe.ComponentRef{Name: "not-in-registry", Type: recipe.ComponentTypeHelm}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			digests, err := ResolveChartDigests(context.Background(), dp, []recipe.ComponentRef{tt.ref})
			if err != nil {
				t.Fatalf("ResolveChartDigests: %v", err)
			}
			got, ok := digests[tt.ref.Name]
			if got != tt.want || ok != (tt.want != "") {
				t.Errorf("digests[%q] = %q (present %v), want %q (chart=%q source=%q version=%q)",
					tt.ref.Name, got, ok, tt.want, tt.ref.EffectiveChart(), tt.ref.Source, tt.ref.Version)
			}
		})
	}
}

// TestResolveChartDigests_FailsClosed pins the error direction: an empty map
// reads as "nothing to verify", which would vendor every chart unchecked.
func TestResolveChartDigests_FailsClosed(t *testing.T) {
	t.Run("registry unavailable", func(t *testing.T) {
		_, err := ResolveChartDigests(context.Background(), failingProvider{},
			[]recipe.ComponentRef{{Name: deployertest.DigestedComponent, Type: recipe.ComponentTypeHelm}})
		if err == nil {
			t.Fatal("ResolveChartDigests() error = nil, want the registry failure")
		}
	})
	t.Run("context canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := ResolveChartDigests(ctx, deployertest.DigestDataProvider(t, testChartDigest),
			[]recipe.ComponentRef{{Name: deployertest.DigestedComponent, Type: recipe.ComponentTypeHelm}})
		if err == nil {
			t.Fatal("ResolveChartDigests() error = nil, want cancellation")
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
	})
}
