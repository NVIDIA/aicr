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
	stderrors "errors"
	"io/fs"
	"slices"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// failingProvider is a DataProvider whose every read fails, so the registry
// cannot be loaded. Used to exercise the fail-closed path below.
type failingProvider struct{}

func (failingProvider) ReadFile(context.Context, string) ([]byte, error) {
	return nil, stderrors.New("registry unavailable")
}

func (failingProvider) WalkDir(context.Context, string, fs.WalkDirFunc) error {
	return stderrors.New("registry unavailable")
}

func (failingProvider) Source(path string) string { return "failing://" + path }

// ownsCRDsFixture returns a component the registry marks ownsCRDs, along with
// a ref resolved against the registry pin. Sourced from the registry rather
// than hardcoded so enrolling or retiring a component cannot leave these tests
// asserting against a config that no longer exists.
func ownsCRDsFixture(t *testing.T) (string, recipe.ComponentRef) {
	t.Helper()
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	for _, name := range registry.Names() {
		cfg := registry.Get(name)
		if cfg == nil || !cfg.OwnsCRDs {
			continue
		}
		ref := recipe.ComponentRef{Name: name, Type: recipe.ComponentTypeHelm}
		ref.ApplyRegistryDefaults(cfg)
		return name, ref
	}
	t.Fatal("no ownsCRDs component in the registry; these tests would prove nothing")
	return "", recipe.ComponentRef{}
}

// nonOwningName returns a registry component that does NOT set ownsCRDs, so
// the negative case is a real registry entry rather than an unknown name; those
// two reach the same result by different paths.
func nonOwningName(t *testing.T) string {
	t.Helper()
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	for _, name := range registry.Names() {
		cfg := registry.Get(name)
		if cfg != nil && !cfg.OwnsCRDs && cfg.Helm.DefaultChart != "" {
			return name
		}
	}
	t.Fatal("every registry Helm component sets ownsCRDs; the opt-in design is broken")
	return ""
}

func TestResolveCRDOwners(t *testing.T) {
	t.Parallel()
	owning, registryRef := ownsCRDsFixture(t)
	other := nonOwningName(t)

	// Each mutator takes the registry-resolved ref and breaks one coordinate.
	// The audit ownsCRDs records covers the pinned chart only, so every one of
	// these must fail closed.
	tests := []struct {
		name string
		ref  recipe.ComponentRef
		want bool
	}{
		{
			name: "registry-resolved ref owns its CRDs",
			ref:  registryRef,
			want: true,
		},
		{
			name: "version override points at an unaudited chart",
			ref: func() recipe.ComponentRef {
				r := registryRef
				r.Version = "0.0.0-not-the-pin"
				return r
			}(),
			want: false,
		},
		{
			name: "source override points at an unaudited repository",
			ref: func() recipe.ComponentRef {
				r := registryRef
				r.Source = "https://charts.example.invalid"
				return r
			}(),
			want: false,
		},
		{
			name: "chart override points at an unaudited chart",
			ref: func() recipe.ComponentRef {
				r := registryRef
				r.Chart = "some-other-chart"
				return r
			}(),
			want: false,
		},
		{
			name: "registry component without the flag",
			ref:  recipe.ComponentRef{Name: other, Type: recipe.ComponentTypeHelm},
			want: false,
		},
		{
			name: "component absent from the registry",
			ref:  recipe.ComponentRef{Name: "not-a-registry-component", Type: recipe.ComponentTypeHelm},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			owners, err := ResolveCRDOwners(context.Background(), nil, []recipe.ComponentRef{tt.ref})
			if err != nil {
				t.Fatalf("ResolveCRDOwners: %v", err)
			}
			if got := owners[tt.ref.Name]; got != tt.want {
				t.Errorf("owners[%q] = %v, want %v (chart=%q source=%q version=%q)",
					tt.ref.Name, got, tt.want, tt.ref.EffectiveChart(), tt.ref.Source, tt.ref.Version)
			}
		})
	}

	// A component absent from the map reads as false, which is what every
	// caller relies on. Asserting the map holds only the owner keeps a future
	// change from recording `false` entries that callers might iterate.
	t.Run("non-owners are absent, not false", func(t *testing.T) {
		t.Parallel()
		owners, err := ResolveCRDOwners(context.Background(), nil, []recipe.ComponentRef{
			registryRef,
			{Name: other, Type: recipe.ComponentTypeHelm},
		})
		if err != nil {
			t.Fatalf("ResolveCRDOwners: %v", err)
		}
		if len(owners) != 1 {
			t.Errorf("len(owners) = %d, want 1 (only %q)\ngot: %v", len(owners), owning, owners)
		}
	})
}

// TestResolveCRDOwners_ContextCancelled pins the fail-closed direction on
// cancellation: an error, not an empty map. An empty map is indistinguishable
// from "nothing owns its CRDs", which would silently restore the stranded-CRD
// behavior the flag exists to fix.
func TestResolveCRDOwners_ContextCancelled(t *testing.T) {
	t.Parallel()
	_, ref := ownsCRDsFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	owners, err := ResolveCRDOwners(ctx, nil, []recipe.ComponentRef{ref})
	if err == nil {
		t.Fatalf("ResolveCRDOwners on a cancelled context returned no error (owners=%v)", owners)
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")) {
		t.Errorf("error code = %v, want ErrCodeTimeout", err)
	}
	if owners != nil {
		t.Errorf("owners = %v, want nil on error", owners)
	}
}

// TestUsesRegistryChart_StripsRepoAliasPrefix covers registryChartName.
// ApplyRegistryDefaults strips everything before the last "/" when defaulting
// ref.Chart, so comparing against the unstripped defaultChart silently fails
// for every component whose registry entry carries a repo-alias prefix, which
// is how gatekeeper was enrolled in ownsCRDs and never emitted the policy.
func TestUsesRegistryChart_StripsRepoAliasPrefix(t *testing.T) {
	t.Parallel()

	cfg := &recipe.ComponentConfig{}
	cfg.Helm.DefaultRepository = "https://open-policy-agent.github.io/gatekeeper/charts"
	cfg.Helm.DefaultChart = "gatekeeper/gatekeeper"
	cfg.Helm.DefaultVersion = "3.22.2"

	ref := recipe.ComponentRef{Name: "gatekeeper", Type: recipe.ComponentTypeHelm}
	ref.ApplyRegistryDefaults(cfg)

	if got := ref.EffectiveChart(); got != "gatekeeper" {
		t.Fatalf("ApplyRegistryDefaults left Chart = %q, want the stripped %q; "+
			"this test no longer covers what it claims", got, "gatekeeper")
	}
	if !UsesRegistryChart(ref, cfg) {
		t.Error("UsesRegistryChart = false for a ref resolved from a repo-alias-prefixed defaultChart")
	}
}

// TestUsesRegistryChart_NoHelmChart pins the fail-closed default for a
// component with no Helm chart at all (kustomize or manifest-only): there is
// no chart whose CRDs could have been audited.
func TestUsesRegistryChart_NoHelmChart(t *testing.T) {
	t.Parallel()

	cfg := &recipe.ComponentConfig{}
	if UsesRegistryChart(recipe.ComponentRef{Name: "x"}, cfg) {
		t.Error("UsesRegistryChart = true for a component with no Helm chart")
	}
}

// TestResolveCRDOwners_RegistryFailureIsFatal pins the other fail-closed
// direction. Returning an empty map on a registry failure would read as
// "no component owns its CRDs", which is indistinguishable from the correct
// answer for a recipe that genuinely has none: every deployer would silently
// skip the CRD step and strand the schema, which is the defect the flag exists
// to fix. An error is the only safe answer.
func TestResolveCRDOwners_RegistryFailureIsFatal(t *testing.T) {
	t.Parallel()

	owners, err := ResolveCRDOwners(context.Background(), failingProvider{}, []recipe.ComponentRef{
		{Name: "k8s-aibom", Type: recipe.ComponentTypeHelm},
	})
	if err == nil {
		t.Fatalf("ResolveCRDOwners with an unreadable registry returned no error (owners=%v)", owners)
	}
	if owners != nil {
		t.Errorf("owners = %v, want nil on error", owners)
	}
}

// crdExclusionFixtures returns, from the live registry, one ownsCRDs component
// that declares subchart exclusions (with its list) and one that declares none.
func crdExclusionFixtures(t *testing.T) (withExcl string, excl []string, withoutExcl string) {
	t.Helper()
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	for _, name := range registry.Names() {
		cfg := registry.Get(name)
		if cfg == nil || !cfg.OwnsCRDs {
			continue
		}
		if len(cfg.OwnsCRDsExcludeSubcharts) > 0 && withExcl == "" {
			withExcl, excl = name, cfg.OwnsCRDsExcludeSubcharts
		}
		if len(cfg.OwnsCRDsExcludeSubcharts) == 0 && withoutExcl == "" {
			withoutExcl = name
		}
	}
	if withExcl == "" || withoutExcl == "" {
		t.Fatalf("registry lacks an ownsCRDs component with (%q) and without (%q) subchart exclusions",
			withExcl, withoutExcl)
	}
	return withExcl, excl, withoutExcl
}

func TestResolveCRDExclusions(t *testing.T) {
	t.Parallel()
	withExcl, excl, withoutExcl := crdExclusionFixtures(t)

	tests := []struct {
		name   string
		owners map[string]bool
		want   map[string][]string
	}{
		{
			name:   "owner with exclusions",
			owners: map[string]bool{withExcl: true},
			want:   map[string][]string{withExcl: excl},
		},
		{
			name:   "owner without exclusions is omitted",
			owners: map[string]bool{withoutExcl: true},
			want:   map[string][]string{},
		},
		{
			name:   "component not resolved as owner is omitted",
			owners: map[string]bool{withExcl: false},
			want:   map[string][]string{},
		},
		{
			name:   "component absent from the registry is omitted",
			owners: map[string]bool{"not-a-registry-component": true},
			want:   map[string][]string{},
		},
		{
			name:   "mixed owners",
			owners: map[string]bool{withExcl: true, withoutExcl: true, "not-a-registry-component": true},
			want:   map[string][]string{withExcl: excl},
		},
		{
			name:   "nil owners",
			owners: nil,
			want:   map[string][]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveCRDExclusions(nil, tt.owners)
			if err != nil {
				t.Fatalf("ResolveCRDExclusions: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("ResolveCRDExclusions = %v, want %v", got, tt.want)
			}
			for name, wantList := range tt.want {
				if !slices.Equal(got[name], wantList) {
					t.Errorf("exclusions[%q] = %v, want %v", name, got[name], wantList)
				}
			}
		})
	}
}

// TestResolveCRDExclusions_ReturnsCopy guards the registry's slice against
// mutation by a caller of the returned map.
func TestResolveCRDExclusions_ReturnsCopy(t *testing.T) {
	t.Parallel()
	withExcl, excl, _ := crdExclusionFixtures(t)
	want := slices.Clone(excl)

	got, err := ResolveCRDExclusions(nil, map[string]bool{withExcl: true})
	if err != nil {
		t.Fatalf("ResolveCRDExclusions: %v", err)
	}
	got[withExcl][0] = "mutated"

	again, err := ResolveCRDExclusions(nil, map[string]bool{withExcl: true})
	if err != nil {
		t.Fatalf("ResolveCRDExclusions: %v", err)
	}
	if !slices.Equal(again[withExcl], want) {
		t.Errorf("registry exclusions mutated through returned slice: got %v, want %v", again[withExcl], want)
	}
}

func TestResolveCRDExclusions_RegistryFailureIsFatal(t *testing.T) {
	t.Parallel()

	got, err := ResolveCRDExclusions(failingProvider{}, map[string]bool{"nvsentinel": true})
	if err == nil {
		t.Fatalf("ResolveCRDExclusions with an unreadable registry returned no error (got=%v)", got)
	}
	if got != nil {
		t.Errorf("exclusions = %v, want nil on error", got)
	}
}
