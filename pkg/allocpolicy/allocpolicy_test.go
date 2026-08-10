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

package allocpolicy

import (
	stderrors "errors"
	"sort"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func boolPtr(v bool) *bool { return &v }

// TestDescriptorIsDeterministicAndCoversKnownVocabulary pins the canonical
// descriptor: component order is lexicographic, per-entry selector paths
// are sorted, and the contents match the vocabulary the three pre-existing
// private copies carried (pkg/bundler, pkg/validator/v1, pkg/recipe). An
// entry disappearing here would silently unlock recomputed closures on
// older authentic recipes — the append-only contract.
func TestDescriptorIsDeterministicAndCoversKnownVocabulary(t *testing.T) {
	t.Parallel()

	entries := Descriptor()
	if len(entries) != 4 {
		t.Fatalf("Descriptor() returned %d entries, want 4", len(entries))
	}
	if !sort.SliceIsSorted(entries, func(i, j int) bool {
		return entries[i].Component < entries[j].Component
	}) {

		t.Error("Descriptor() entries are not sorted by component")
	}
	want := map[string][]string{
		ComponentGPUOperator:    {PathDevicePluginEnabled},
		ComponentGPUOperatorOCP: {PathDevicePluginEnabled},
		ComponentDRADriver:      {PathDRAGPUsEnabledOverride, PathDRAGPUsEnabled},
		ComponentDRADriverOCP:   {PathDRAGPUsEnabledOverride, PathDRAGPUsEnabled},
	}
	// Consume want keys as entries match so a duplicated component (which
	// keeps len(entries)==4 while silently dropping another pinned entry)
	// fails the test instead of slipping past the map lookup.
	unconsumed := make(map[string]struct{}, len(want))
	for k := range want {
		unconsumed[k] = struct{}{}
	}
	for _, entry := range entries {
		wantPaths, ok := want[entry.Component]
		if !ok {
			t.Errorf("unexpected descriptor component %q", entry.Component)
			continue
		}
		if _, fresh := unconsumed[entry.Component]; !fresh {
			t.Errorf("duplicate descriptor component %q", entry.Component)
			continue
		}
		delete(unconsumed, entry.Component)
		if !sort.StringsAreSorted(entry.SelectorPaths) {
			t.Errorf("selector paths for %q are not sorted: %v", entry.Component, entry.SelectorPaths)
		}
		if len(entry.SelectorPaths) != len(wantPaths) {
			t.Errorf("selector paths for %q = %v, want %v", entry.Component, entry.SelectorPaths, wantPaths)
			continue
		}
		for i, p := range wantPaths {
			if entry.SelectorPaths[i] != p {
				t.Errorf("selector paths for %q = %v, want %v", entry.Component, entry.SelectorPaths, wantPaths)
				break
			}
		}
	}
	for k := range unconsumed {
		t.Errorf("descriptor is missing pinned component %q", k)
	}

	// Callers may not be able to mutate shared state through the returned
	// slice.
	entries[0].SelectorPaths[0] = "mutated"
	if Descriptor()[0].SelectorPaths[0] == "mutated" {
		t.Error("Descriptor() shares mutable state across calls")
	}
}

func TestSelectorPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		component string
		want      []string
	}{
		{"gpu-operator", ComponentGPUOperator, []string{PathDevicePluginEnabled}},
		{"gpu-operator-ocp", ComponentGPUOperatorOCP, []string{PathDevicePluginEnabled}},
		{"dra driver", ComponentDRADriver, []string{PathDRAGPUsEnabledOverride, PathDRAGPUsEnabled}},
		{"unknown component", "cert-manager", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := SelectorPaths(tt.component)
			if len(got) != len(tt.want) {
				t.Fatalf("SelectorPaths(%q) = %v, want %v", tt.component, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("SelectorPaths(%q) = %v, want %v", tt.component, got, tt.want)
				}
			}
		})
	}

	// The two operator flavors carry the same advertiser switch
	// (devicePlugin.enabled), so their selector paths must stay identical —
	// OCP recipes swap the component, not the vocabulary.
	t.Run("ocp paths match gpu-operator", func(t *testing.T) {
		t.Parallel()
		op := SelectorPaths(ComponentGPUOperator)
		ocp := SelectorPaths(ComponentGPUOperatorOCP)
		if len(op) != len(ocp) {
			t.Fatalf("SelectorPaths(ocp) = %v, want %v", ocp, op)
		}
		for i := range op {
			if op[i] != ocp[i] {
				t.Fatalf("SelectorPaths(ocp) = %v, want %v", ocp, op)
			}
		}
	})
}

func TestValidateAdvertiser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		advertiser string
		wantErr    bool
	}{
		{"empty is valid", "", false},
		{"external is valid", AdvertiserExternal, false},
		{"unknown rejected", "csp", true},
		{"case-sensitive", "External", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateAdvertiser(tt.advertiser)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ValidateAdvertiser(%q) error = %v, wantErr %v", tt.advertiser, err, tt.wantErr)
			}
			if err != nil && !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want %s", err, errors.ErrCodeInvalidRequest)
			}
		})
	}
}

func TestCheckCoherence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		obs     Observation
		wantErr bool
	}{
		// Empty advertiser: the resolver's non-external #1327 verdicts —
		// the same tuple rows ResolveGPUAllocationPolicy enforces at
		// validation time (gate/resolver symmetry).
		{"empty advertiser, device-plugin default", Observation{
			DevicePluginEnabled: boolPtr(true), DRAGPUsEnabled: boolPtr(false)}, false},
		{"empty advertiser, device plugin only (DRA absent)", Observation{
			DevicePluginEnabled: boolPtr(true)}, false},
		{"empty advertiser, DRA opt-in with waiver and plugin off", Observation{
			DevicePluginEnabled: boolPtr(false), DRAGPUsEnabled: boolPtr(true),
			DRAGPUsEnabledOverride: boolPtr(true)}, false},
		{"empty advertiser, gpus enabled without waiver: chart guard", Observation{
			DevicePluginEnabled: boolPtr(false), DRAGPUsEnabled: boolPtr(true)}, true},
		{"empty advertiser, dual advertisement (plugin + DRA)", Observation{
			DevicePluginEnabled: boolPtr(true), DRAGPUsEnabled: boolPtr(true),
			DRAGPUsEnabledOverride: boolPtr(true)}, true},
		{"empty advertiser, no whole-GPU advertiser", Observation{
			DevicePluginEnabled: boolPtr(false), DRAGPUsEnabled: boolPtr(false)}, true},
		{"empty advertiser, no advertiser at all (both nil)", Observation{}, true},
		{"empty advertiser, inert waiver", Observation{
			DevicePluginEnabled: boolPtr(true), DRAGPUsEnabled: boolPtr(false),
			DRAGPUsEnabledOverride: boolPtr(true)}, true},
		{"external with both nil readings", Observation{Advertiser: AdvertiserExternal}, false},
		{"external with an inert waiver rejected", Observation{
			Advertiser: AdvertiserExternal, DevicePluginEnabled: boolPtr(false),
			DRAGPUsEnabled: boolPtr(false), DRAGPUsEnabledOverride: boolPtr(true)}, true},
		{"external with device plugin off and DRA off", Observation{
			Advertiser: AdvertiserExternal, DevicePluginEnabled: boolPtr(false), DRAGPUsEnabled: boolPtr(false)}, false},
		{"external + devicePlugin.enabled=true rejected", Observation{
			Advertiser: AdvertiserExternal, DevicePluginEnabled: boolPtr(true)}, true},
		{"external + DRA gpus.enabled=true rejected", Observation{
			Advertiser: AdvertiserExternal, DRAGPUsEnabled: boolPtr(true)}, true},
		{"unknown advertiser rejected", Observation{Advertiser: "managed"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := CheckCoherence(tt.obs)
			if (err != nil) != tt.wantErr {
				t.Fatalf("CheckCoherence(%+v) error = %v, wantErr %v", tt.obs, err, tt.wantErr)
			}
			if err != nil && !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want %s", err, errors.ErrCodeInvalidRequest)
			}
		})
	}
}

// TestIdentityIsStable pins the descriptor identity bytes: evidence
// predicates persist this value, so an accidental identity change would
// spuriously invalidate every profiled evidence pointer. Update this
// constant ONLY on a deliberate descriptor expansion, which is a
// re-qualification and evidence re-signing event (ADR-015).
func TestIdentityIsStable(t *testing.T) {
	t.Parallel()

	first := Identity()
	if first != Identity() {
		t.Fatal("Identity() is not deterministic across calls")
	}
	if len(first) != 64 {
		t.Fatalf("Identity() = %q, want a sha256 hex digest", first)
	}
	const pinned = "cafd61f00de1871f6a23c8ea9205ed52dce03304fe179bca733a9f976bf3e247"
	if first != pinned {
		t.Fatalf("Identity() = %q, want pinned %q — descriptor contents changed; "+
			"if this is a deliberate append-only expansion, update the pin and treat it "+
			"as a re-qualification and evidence re-signing event", first, pinned)
	}
	if got := IdentityFor(Descriptor()); got != pinned {
		t.Fatalf("IdentityFor(Descriptor()) = %q, want %q — the full-descriptor "+
			"identity must equal Identity()", got, pinned)
	}
}

// TestIdentityForIsRecipeScoped pins the ADR-015 recipe-scoped identity
// semantics: the empty set has a stable pinned identity (sha256 of the
// empty canonical string), a subset identity differs from the full
// descriptor's, and the result is independent of entry and path input
// order. Evidence predicates persist IdentityFor values, so an accidental
// change here would spuriously invalidate profiled evidence pointers.
func TestIdentityForIsRecipeScoped(t *testing.T) {
	t.Parallel()

	// sha256("") — the identity of a recipe whose closure contributes no
	// descriptor entries.
	const pinnedEmpty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := IdentityFor(nil); got != pinnedEmpty {
		t.Fatalf("IdentityFor(nil) = %q, want pinned empty-set identity %q", got, pinnedEmpty)
	}
	if got := IdentityFor([]Entry{}); got != pinnedEmpty {
		t.Fatalf("IdentityFor([]) = %q, want pinned empty-set identity %q", got, pinnedEmpty)
	}

	full := Descriptor()
	subset := []Entry{full[0], full[2]} // gpu-operator + DRA driver (the GKE closure shape)
	if IdentityFor(subset) == IdentityFor(full) {
		t.Fatal("subset identity equals full-descriptor identity — the scope is not recipe-bound")
	}

	// Order independence: reversed entries and reversed selector paths
	// produce the same identity.
	reversed := []Entry{
		{Component: full[2].Component, SelectorPaths: []string{
			full[2].SelectorPaths[1], full[2].SelectorPaths[0],
		}},
		{Component: full[0].Component, SelectorPaths: full[0].SelectorPaths},
	}
	if IdentityFor(reversed) != IdentityFor(subset) {
		t.Fatal("IdentityFor is input-order dependent — the identity must be canonical")
	}

	// Duplicate-component entries: paths split across two entries for the
	// same component must hash identically regardless of entry order and
	// of how the paths are partitioned — the canonicalization merges per
	// component before sorting.
	splitA := []Entry{
		{Component: full[2].Component, SelectorPaths: []string{full[2].SelectorPaths[0]}},
		{Component: full[0].Component, SelectorPaths: full[0].SelectorPaths},
		{Component: full[2].Component, SelectorPaths: []string{full[2].SelectorPaths[1]}},
	}
	splitB := []Entry{
		{Component: full[2].Component, SelectorPaths: []string{full[2].SelectorPaths[1]}},
		{Component: full[2].Component, SelectorPaths: []string{full[2].SelectorPaths[0]}},
		{Component: full[0].Component, SelectorPaths: full[0].SelectorPaths},
	}
	if IdentityFor(splitA) != IdentityFor(subset) {
		t.Fatal("IdentityFor differs when a component's paths are split across entries")
	}
	if IdentityFor(splitA) != IdentityFor(splitB) {
		t.Fatal("IdentityFor is order-dependent for duplicate-component entries")
	}

	// Duplicate component:path pairs: repeating an already-contributed pair
	// (whether within one entry's paths or via a duplicate entry) must not
	// change the identity — the canonicalization is set-based, so the
	// identity depends only on WHICH pairs contribute, not how many times.
	duplicated := []Entry{
		full[0],
		{Component: full[0].Component, SelectorPaths: full[0].SelectorPaths},
		{Component: full[2].Component, SelectorPaths: append(
			append([]string(nil), full[2].SelectorPaths...),
			full[2].SelectorPaths[0],
		)},
	}
	if IdentityFor(duplicated) != IdentityFor(subset) {
		t.Fatal("IdentityFor differs when component:path pairs are duplicated — the identity must be set-based")
	}
}
