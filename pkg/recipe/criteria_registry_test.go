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
	"os"
	"path/filepath"
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

func TestLoadCatalog_DoesNotLeakRegistryOnFailure(t *testing.T) {
	t.Setenv(strictModeEnvVar, "")

	// Build an external data directory with a well-formed overlay that
	// would introduce a custom service value, followed by a malformed
	// overlay that fails YAML parsing. The well-formed overlay is
	// processed first (lexical filepath.WalkDir order) — under the
	// pre-fix behavior it would have seeded the registry before the
	// second file's parse error aborted the load.
	tmp := t.TempDir()
	overlaysDir := filepath.Join(tmp, "overlays")
	if err := os.MkdirAll(overlaysDir, 0o755); err != nil {
		t.Fatalf("mkdir overlays: %v", err)
	}
	writeFile := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tmp, rel), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	writeFile("registry.yaml", `apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components: []
`)
	writeFile("overlays/01-good.yaml", `apiVersion: aicr.nvidia.com/v1alpha1
kind: RecipeMetadata
metadata:
  name: pending-leak-overlay
spec:
  base: base
  criteria:
    service: pending-leak-service
    accelerator: h100
    intent: training
  componentRefs: []
`)
	// Malformed YAML — fails decode, aborts the walk.
	writeFile("overlays/02-bad.yaml", "this: : not yaml\n  ::\n")

	layered, err := NewLayeredDataProvider(
		NewEmbeddedDataProvider(GetEmbeddedFS(), ""),
		LayeredProviderConfig{ExternalDir: tmp},
	)
	if err != nil {
		t.Fatalf("NewLayeredDataProvider: %v", err)
	}

	// Snapshot + restore process globals so this test does not poison
	// the singleton state observed by tests that run after it.
	prev := GetDataProvider() //nolint:staticcheck // exercises legacy global-provider swap; tracked by #983 Stage 2
	t.Cleanup(func() {
		SetDataProvider(prev) //nolint:staticcheck // exercises legacy global-provider swap; tracked by #983 Stage 2
		ResetMetadataStoreForTesting()
		DefaultRegistry().Reset()
	})
	SetDataProvider(layered) //nolint:staticcheck // exercises legacy global-provider swap; tracked by #983 Stage 2
	ResetMetadataStoreForTesting()
	DefaultRegistry().Reset()

	if loadErr := LoadCatalog(context.Background()); loadErr == nil {
		t.Fatal("expected LoadCatalog to error on malformed overlay")
	}
	if DefaultRegistry().Has(FieldService, "pending-leak-service") {
		t.Error("malformed catalog load leaked staged criteria into registry; " +
			"deferred commit must skip registry mutation when validation fails")
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

func TestDefaultRegistry_Singleton(t *testing.T) {
	a := DefaultRegistry()
	b := DefaultRegistry()
	if a != b {
		t.Error("DefaultRegistry must return the same singleton on every call")
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
			reg := DefaultRegistry()
			reg.Reset()
			t.Cleanup(reg.Reset)

			c := &Criteria{
				Service:     "ncp-x",
				Accelerator: "h200",
				Intent:      "fine-tuning",
				OS:          "bottlerocket",
				Platform:    "nvmesh",
			}
			seedCriteriaRegistry(c, tt.source)

			checks := []struct {
				field CriteriaField
				value string
			}{
				{FieldService, "ncp-x"},
				{FieldAccelerator, "h200"},
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

func TestSeedCriteriaRegistry_NilCriteria(t *testing.T) {
	reg := DefaultRegistry()
	reg.Reset()
	t.Cleanup(reg.Reset)
	// Must not panic.
	seedCriteriaRegistry(nil, "embedded")
	if got := reg.Values(FieldService); len(got) != 0 {
		t.Errorf("nil criteria must not register anything, got %v", got)
	}
}

func TestSeedCriteriaRegistry_SkipsWildcardAndEmpty(t *testing.T) {
	reg := DefaultRegistry()
	reg.Reset()
	t.Cleanup(reg.Reset)
	c := &Criteria{
		Service:     CriteriaServiceAny, // "any" must be skipped
		Accelerator: "",                 // empty must be skipped
		Intent:      "training",         // real value, must register
	}
	seedCriteriaRegistry(c, "embedded")
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
