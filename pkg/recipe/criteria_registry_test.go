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
	"context"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func TestCriteriaRegistry_Register(t *testing.T) {
	t.Setenv(strictModeEnvVar, "")
	tests := []struct {
		name     string
		field    CriteriaField
		value    string
		origin   CriteriaOrigin
		wantHas  bool
		wantEmb  bool
		wantVals []string
	}{
		{"external service", FieldService, "ncp-x", OriginExternal, true, false, []string{"ncp-x"}},
		{"embedded accelerator", FieldAccelerator, "h100", OriginEmbedded, true, true, []string{"h100"}},
		{"empty value ignored", FieldService, "", OriginEmbedded, false, false, nil},
		{"any wildcard ignored", FieldService, "any", OriginEmbedded, false, false, nil},
		{"whitespace trimmed", FieldOS, "  ubuntu  ", OriginEmbedded, true, true, []string{"ubuntu"}},
		{"case normalized", FieldPlatform, "Kubeflow", OriginEmbedded, true, true, []string{"kubeflow"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newCriteriaRegistry()
			r.Register(tt.field, tt.value, tt.origin)
			normalized := normalizeCriteriaValue(tt.value)
			if got := r.Has(tt.field, normalized); got != tt.wantHas {
				t.Errorf("Has(%q, %q) = %v, want %v", tt.field, normalized, got, tt.wantHas)
			}
			if got := r.HasEmbedded(tt.field, normalized); got != tt.wantEmb {
				t.Errorf("HasEmbedded(%q, %q) = %v, want %v", tt.field, normalized, got, tt.wantEmb)
			}
			if got := r.Values(tt.field); !reflect.DeepEqual(got, tt.wantVals) {
				t.Errorf("Values(%q) = %v, want %v", tt.field, got, tt.wantVals)
			}
		})
	}
}

func TestCriteriaRegistry_EmbeddedNotDowngraded(t *testing.T) {
	t.Setenv(strictModeEnvVar, "")
	r := newCriteriaRegistry()
	r.Register(FieldService, "ncp-x", OriginEmbedded)
	r.Register(FieldService, "ncp-x", OriginExternal) // attempted downgrade
	if !r.HasEmbedded(FieldService, "ncp-x") {
		t.Error("embedded origin must survive a later external registration")
	}
}

func TestCriteriaRegistry_ExternalUpgradedToEmbedded(t *testing.T) {
	t.Setenv(strictModeEnvVar, "")
	r := newCriteriaRegistry()
	r.Register(FieldService, "ncp-x", OriginExternal)
	r.Register(FieldService, "ncp-x", OriginEmbedded)
	if !r.HasEmbedded(FieldService, "ncp-x") {
		t.Error("later embedded registration must upgrade an external value")
	}
}

func TestCriteriaRegistry_StrictMode(t *testing.T) {
	t.Setenv(strictModeEnvVar, "")
	r := newCriteriaRegistry()
	r.Register(FieldService, "eks", OriginEmbedded)
	r.Register(FieldService, "ncp-internal", OriginExternal)

	// Permissive: both visible.
	if !r.Has(FieldService, "eks") {
		t.Error("permissive mode must admit embedded values")
	}
	if !r.Has(FieldService, "ncp-internal") {
		t.Error("permissive mode must admit external values")
	}
	if got := r.Values(FieldService); !reflect.DeepEqual(got, []string{"eks", "ncp-internal"}) {
		t.Errorf("Values permissive = %v, want [eks ncp-internal]", got)
	}

	// Strict: only embedded.
	r.SetStrict(true)
	if !r.Has(FieldService, "eks") {
		t.Error("strict mode must still admit embedded values")
	}
	if r.Has(FieldService, "ncp-internal") {
		t.Error("strict mode must reject external values")
	}
	if got := r.Values(FieldService); !reflect.DeepEqual(got, []string{"eks"}) {
		t.Errorf("Values strict = %v, want [eks]", got)
	}
}

func TestCriteriaRegistry_Reset(t *testing.T) {
	t.Setenv(strictModeEnvVar, "")
	r := newCriteriaRegistry()
	r.Register(FieldService, "ncp-x", OriginExternal)
	r.SetStrict(true)

	r.Reset()
	if r.Has(FieldService, "ncp-x") {
		t.Error("Reset must clear registered values")
	}
	if r.IsStrict() {
		t.Error("Reset must restore strict flag from env (unset → false)")
	}
}

func TestCriteriaRegistry_ResetReadsEnv(t *testing.T) {
	t.Setenv(strictModeEnvVar, "true")
	r := newCriteriaRegistry()
	if !r.IsStrict() {
		t.Error("new registry must inherit AICR_CRITERIA_STRICT=true")
	}
	r.SetStrict(false)
	r.Reset()
	if !r.IsStrict() {
		t.Error("Reset must re-read AICR_CRITERIA_STRICT")
	}
}

func TestCriteriaRegistry_Values_NilForUnknownField(t *testing.T) {
	r := newCriteriaRegistry()
	if got := r.Values(FieldPlatform); got != nil {
		t.Errorf("unknown field must return nil slice, got %v", got)
	}
}

func TestCriteriaRegistry_ConcurrentAccess(t *testing.T) {
	r := newCriteriaRegistry()
	const goroutines = 16
	const perGoroutine = 64

	var wg sync.WaitGroup
	wg.Add(goroutines * 2)
	for g := range goroutines {
		go func() {
			defer wg.Done()
			for i := range perGoroutine {
				origin := OriginEmbedded
				if (g+i)%2 == 1 {
					origin = OriginExternal
				}
				r.Register(FieldService, "ncp-shared", origin)
			}
		}()
		go func() {
			defer wg.Done()
			for range perGoroutine {
				_ = r.Has(FieldService, "ncp-shared")
				_ = r.HasEmbedded(FieldService, "ncp-shared")
				_ = r.Values(FieldService)
			}
		}()
	}
	wg.Wait()

	if !r.HasEmbedded(FieldService, "ncp-shared") {
		t.Error("at least one OriginEmbedded write must have stuck")
	}
}

func TestIsStrictModeFromEnv(t *testing.T) {
	tests := []struct {
		raw  string
		want bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"no", false},
		{"off", false},
		{"1", true},
		{"true", true},
		{"True", true},
		{"YES", true},
		{"on", true},
		{"  TRUE ", true},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			t.Setenv(strictModeEnvVar, tt.raw)
			if got := isStrictModeFromEnv(); got != tt.want {
				t.Errorf("isStrictModeFromEnv(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestCriteriaRegistry_RegisterOnZeroValue(t *testing.T) {
	// External callers may legally construct &CriteriaRegistry{} (the
	// type is exported even though newCriteriaRegistry is not); Register
	// must defensively initialize the inner map instead of panicking on
	// nil-map assignment.
	var r CriteriaRegistry
	r.Register(FieldService, "ncp-zero", OriginExternal)
	if !r.Has(FieldService, "ncp-zero") {
		t.Error("Register on a zero-value CriteriaRegistry must succeed")
	}
}

func TestSeedCriteriaRegistry(t *testing.T) {
	t.Setenv(strictModeEnvVar, "")
	tests := []struct {
		name       string
		source     string
		wantOrigin CriteriaOrigin
	}{
		{"embedded source", "embedded", OriginEmbedded},
		{"external source", "external", OriginExternal},
		{"merged is strict-safe external", "merged", OriginExternal},
		{"unknown source is strict-safe external", "", OriginExternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use a distinct in-memory provider per case so each test
			// gets an isolated registry from GetCriteriaRegistryFor.
			dp := newInMemoryProvider("seed-"+tt.name, map[string][]byte{})
			t.Cleanup(func() { EvictCachedCriteriaRegistry(dp) })

			c := &Criteria{
				Service:     "ncp-x",
				Accelerator: "mi300x",
				Intent:      "fine-tuning",
				OS:          "bottlerocket",
				Platform:    "nvmesh",
			}
			seedCriteriaRegistry(c, tt.source, dp)
			reg := GetCriteriaRegistryFor(dp)

			checks := []struct {
				field CriteriaField
				value string
			}{
				{FieldService, "ncp-x"},
				{FieldAccelerator, "mi300x"},
				{FieldIntent, "fine-tuning"},
				{FieldOS, "bottlerocket"},
				{FieldPlatform, "nvmesh"},
			}
			for _, ck := range checks {
				if !reg.Has(ck.field, ck.value) {
					t.Errorf("registry.Has(%q, %q) = false; want registered", ck.field, ck.value)
				}
				gotEmbedded := reg.HasEmbedded(ck.field, ck.value)
				wantEmbedded := tt.wantOrigin == OriginEmbedded
				if gotEmbedded != wantEmbedded {
					t.Errorf("registry.HasEmbedded(%q, %q) = %v, want %v",
						ck.field, ck.value, gotEmbedded, wantEmbedded)
				}
			}
		})
	}
}

// buildProviderWithServiceCriteria returns an inMemoryDataProvider whose
// single overlay declares the supplied custom service criteria value. Loading
// its metadata store seeds the provider's criteria registry with that value
// (OriginExternal, since inMemoryDataProvider.Source never returns
// "embedded"). Used by the per-provider isolation test.
func buildProviderWithServiceCriteria(t *testing.T, tag, serviceValue string) DataProvider {
	t.Helper()

	baseYAML := []byte(`kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: base
spec:
  componentRefs: []
`)

	overlayYAML := fmt.Appendf(nil, `kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: %s-overlay
spec:
  base: base
  criteria:
    service: %s
  componentRefs: []
`, tag, serviceValue)

	files := map[string][]byte{
		"overlays/base.yaml":        baseYAML,
		"overlays/" + tag + ".yaml": overlayYAML,
	}
	return newInMemoryProvider(tag, files)
}

// TestGetCriteriaRegistryFor_PerProviderIsolation verifies that loading two
// distinct DataProviders that register DIFFERENT external criteria values
// seeds two distinct criteria registries with no cross-contamination: each
// provider's registry knows only its own value.
func TestGetCriteriaRegistryFor_PerProviderIsolation(t *testing.T) {
	t.Setenv(strictModeEnvVar, "")
	t.Cleanup(ResetMetadataStoreForTesting)
	t.Cleanup(ResetCriteriaRegistryForTesting)

	const valA = "ncp-alpha"
	const valB = "ncp-beta"

	dpA := buildProviderWithServiceCriteria(t, "alpha", valA)
	dpB := buildProviderWithServiceCriteria(t, "beta", valB)

	ctx := context.Background()
	if _, err := LoadMetadataStoreFor(ctx, dpA); err != nil {
		t.Fatalf("LoadMetadataStoreFor(A): %v", err)
	}
	if _, err := LoadMetadataStoreFor(ctx, dpB); err != nil {
		t.Fatalf("LoadMetadataStoreFor(B): %v", err)
	}

	regA := GetCriteriaRegistryFor(dpA)
	regB := GetCriteriaRegistryFor(dpB)

	if regA == regB {
		t.Fatal("expected distinct criteria registries for distinct providers")
	}

	// Each registry must know its own value...
	if !regA.Has(FieldService, valA) {
		t.Errorf("registry A missing its own value %q", valA)
	}
	if !regB.Has(FieldService, valB) {
		t.Errorf("registry B missing its own value %q", valB)
	}
	// ...and must NOT know the other provider's value.
	if regA.Has(FieldService, valB) {
		t.Errorf("registry A leaked provider B's value %q", valB)
	}
	if regB.Has(FieldService, valA) {
		t.Errorf("registry B leaked provider A's value %q", valA)
	}

	// ParseService routes through the per-provider registry, so each
	// provider's registry admits its own value and rejects the other's.
	if _, err := regA.ParseService(valA); err != nil {
		t.Errorf("regA.ParseService(%q) rejected own value: %v", valA, err)
	}
	if _, err := regA.ParseService(valB); err == nil {
		t.Errorf("regA.ParseService(%q) admitted foreign value", valB)
	}
}

// TestEvictCachedCriteriaRegistry verifies that EvictCachedCriteriaRegistry
// drops the cached entry so the next GetCriteriaRegistryFor rebuilds an empty
// registry (the seeded value is gone until the catalog is reloaded), and that
// a nil provider is a no-op that does not clobber other entries.
func TestEvictCachedCriteriaRegistry(t *testing.T) {
	t.Setenv(strictModeEnvVar, "")
	t.Cleanup(ResetMetadataStoreForTesting)
	t.Cleanup(ResetCriteriaRegistryForTesting)

	const val = "ncp-evict"
	dp := buildProviderWithServiceCriteria(t, "evict", val)

	if _, err := LoadMetadataStoreFor(context.Background(), dp); err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	first := GetCriteriaRegistryFor(dp)
	if !first.Has(FieldService, val) {
		t.Fatalf("seeded registry missing %q", val)
	}
	if !CachedCriteriaRegistryContainsForTesting(dp) {
		t.Fatal("expected cache to contain entry after load")
	}

	// nil is a no-op: must not panic and must not clobber dp's entry.
	EvictCachedCriteriaRegistry(nil)
	if !CachedCriteriaRegistryContainsForTesting(dp) {
		t.Fatal("EvictCachedCriteriaRegistry(nil) clobbered an existing entry")
	}

	EvictCachedCriteriaRegistry(dp)
	if CachedCriteriaRegistryContainsForTesting(dp) {
		t.Fatal("expected entry dropped after evict")
	}

	// Next access rebuilds a fresh (empty, not-yet-reseeded) registry.
	second := GetCriteriaRegistryFor(dp)
	if second == first {
		t.Error("expected a fresh registry instance after evict")
	}
	if second.Has(FieldService, val) {
		t.Error("rebuilt registry must be empty until the catalog is reloaded")
	}
}

// TestGetCriteriaRegistryFor_StrictPerRegistry verifies each per-provider
// registry honors AICR_CRITERIA_STRICT at construction independently.
func TestGetCriteriaRegistryFor_StrictPerRegistry(t *testing.T) {
	t.Setenv(strictModeEnvVar, "true")
	t.Cleanup(ResetCriteriaRegistryForTesting)

	dp := newInMemoryProvider("strict-probe", map[string][]byte{})
	reg := GetCriteriaRegistryFor(dp)
	if !reg.IsStrict() {
		t.Error("per-provider registry must inherit AICR_CRITERIA_STRICT=true at construction")
	}
}

func TestSeedCriteriaRegistry_NilCriteria(t *testing.T) {
	dp := newInMemoryProvider("seed-nil", map[string][]byte{})
	t.Cleanup(func() { EvictCachedCriteriaRegistry(dp) })
	// Must not panic.
	seedCriteriaRegistry(nil, "embedded", dp)
	reg := GetCriteriaRegistryFor(dp)
	if got := reg.Values(FieldService); len(got) != 0 {
		t.Errorf("nil criteria must not register anything, got %v", got)
	}
}

func TestSeedCriteriaRegistry_SkipsWildcardAndEmpty(t *testing.T) {
	dp := newInMemoryProvider("seed-skip", map[string][]byte{})
	t.Cleanup(func() { EvictCachedCriteriaRegistry(dp) })
	c := &Criteria{
		Service:     CriteriaServiceAny, // "any" must be skipped
		Accelerator: "",                 // empty must be skipped
		Intent:      "training",         // real value, must register
	}
	seedCriteriaRegistry(c, "embedded", dp)
	reg := GetCriteriaRegistryFor(dp)
	if reg.Has(FieldService, "any") {
		t.Error("registry should not record wildcard 'any'")
	}
	if reg.Has(FieldAccelerator, "") {
		t.Error("registry should not record empty value")
	}
	if !reg.HasEmbedded(FieldIntent, "training") {
		t.Error("real value must register")
	}
}
