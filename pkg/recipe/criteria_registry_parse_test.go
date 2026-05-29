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
)

// withRegistry runs fn against a clean DefaultRegistry, restoring the
// original state on exit so individual tests don't leak registrations
// into siblings that share the package singleton.
//
// Strict mode is forced off here because tests in this file describe
// the default permissive behavior; cases that exercise strict mode
// call SetStrict(true) explicitly inside the test body. Forcing it off
// makes the tests robust to AICR_CRITERIA_STRICT being set in the
// surrounding environment (e.g., make qualify sets it to gate the
// upstream catalog).
func withRegistry(t *testing.T, fn func()) {
	t.Helper()
	reg := DefaultRegistry()
	reg.Reset()
	reg.SetStrict(false)
	t.Cleanup(func() {
		reg.Reset()
	})
	fn()
}

func TestParseCriteriaServiceType_RegistryFallback(t *testing.T) {
	withRegistry(t, func() {
		// Without registration, an unknown value must still error so OSS
		// behavior is preserved when no external catalog has been loaded.
		if _, err := ParseCriteriaServiceType("ncp-customer-x"); err == nil {
			t.Fatal("expected error for unknown service before registration")
		}

		// After registering an external value (as if loaded via --data),
		// parsing must admit it and return the value typed.
		DefaultRegistry().Register(FieldService, "ncp-customer-x", OriginExternal)
		got, err := ParseCriteriaServiceType("ncp-customer-x")
		if err != nil {
			t.Fatalf("ParseCriteriaServiceType after Register error = %v", err)
		}
		if got != CriteriaServiceType("ncp-customer-x") {
			t.Errorf("ParseCriteriaServiceType = %q, want ncp-customer-x", got)
		}

		// Whitespace + case normalization must match the fast-path behavior.
		got, err = ParseCriteriaServiceType("  NCP-CUSTOMER-X  ")
		if err != nil {
			t.Fatalf("normalized lookup error = %v", err)
		}
		if got != CriteriaServiceType("ncp-customer-x") {
			t.Errorf("normalized = %q, want ncp-customer-x", got)
		}
	})
}

func TestParseCriteriaServiceType_StrictRejectsExternal(t *testing.T) {
	withRegistry(t, func() {
		DefaultRegistry().Register(FieldService, "ncp-customer-x", OriginExternal)
		DefaultRegistry().SetStrict(true)

		if _, err := ParseCriteriaServiceType("ncp-customer-x"); err == nil {
			t.Error("strict mode must reject external-only values")
		}
		// Canonical OSS values must still pass in strict mode.
		if _, err := ParseCriteriaServiceType("eks"); err != nil {
			t.Errorf("strict mode must still admit OSS canonical values; got %v", err)
		}
	})
}

func TestParseCriteriaServiceType_StrictAcceptsEmbedded(t *testing.T) {
	withRegistry(t, func() {
		DefaultRegistry().Register(FieldService, "self-managed-overlay", OriginEmbedded)
		DefaultRegistry().SetStrict(true)
		got, err := ParseCriteriaServiceType("self-managed-overlay")
		if err != nil {
			t.Fatalf("strict mode rejected an embedded value: %v", err)
		}
		if got != CriteriaServiceType("self-managed-overlay") {
			t.Errorf("got = %q, want self-managed-overlay", got)
		}
	})
}

func TestParseCriteriaAcceleratorType_RegistryFallback(t *testing.T) {
	withRegistry(t, func() {
		if _, err := ParseCriteriaAcceleratorType("mi300x"); err == nil {
			t.Fatal("expected error before registration")
		}
		DefaultRegistry().Register(FieldAccelerator, "mi300x", OriginExternal)
		got, err := ParseCriteriaAcceleratorType("mi300x")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if got != CriteriaAcceleratorType("mi300x") {
			t.Errorf("got = %q, want mi300x", got)
		}
	})
}

func TestParseCriteriaIntentType_RegistryFallback(t *testing.T) {
	withRegistry(t, func() {
		DefaultRegistry().Register(FieldIntent, "fine-tuning", OriginExternal)
		got, err := ParseCriteriaIntentType("fine-tuning")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if got != CriteriaIntentType("fine-tuning") {
			t.Errorf("got = %q, want fine-tuning", got)
		}
	})
}

func TestParseCriteriaOSType_RegistryFallback(t *testing.T) {
	withRegistry(t, func() {
		DefaultRegistry().Register(FieldOS, "bottlerocket", OriginExternal)
		got, err := ParseCriteriaOSType("bottlerocket")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if got != CriteriaOSType("bottlerocket") {
			t.Errorf("got = %q, want bottlerocket", got)
		}
	})
}

func TestParseCriteriaPlatformType_RegistryFallback(t *testing.T) {
	withRegistry(t, func() {
		DefaultRegistry().Register(FieldPlatform, "nvmesh", OriginExternal)
		got, err := ParseCriteriaPlatformType("nvmesh")
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		if got != CriteriaPlatformType("nvmesh") {
			t.Errorf("got = %q, want nvmesh", got)
		}
	})
}

func TestAllCriteriaServiceTypes_UnionWithRegistry(t *testing.T) {
	withRegistry(t, func() {
		base := GetCriteriaServiceTypes()
		if got := AllCriteriaServiceTypes(); !reflect.DeepEqual(got, base) {
			t.Errorf("empty registry must yield static list, got %v want %v", got, base)
		}

		DefaultRegistry().Register(FieldService, "ncp-x", OriginExternal)
		DefaultRegistry().Register(FieldService, "eks", OriginExternal) // already in static — must dedupe
		got := AllCriteriaServiceTypes()
		want := []string{"aks", "bcm", "eks", "gke", "kind", "lke", "ncp-x", "oke"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("AllCriteriaServiceTypes = %v, want %v", got, want)
		}

		// Strict mode hides external contributions but preserves static list.
		DefaultRegistry().SetStrict(true)
		got = AllCriteriaServiceTypes()
		if !reflect.DeepEqual(got, base) {
			t.Errorf("strict AllCriteriaServiceTypes = %v, want %v (static only)", got, base)
		}
	})
}

func TestAllCriteriaTypes_AllDimensions(t *testing.T) {
	withRegistry(t, func() {
		DefaultRegistry().Register(FieldAccelerator, "mi300x", OriginExternal)
		DefaultRegistry().Register(FieldIntent, "fine-tuning", OriginExternal)
		DefaultRegistry().Register(FieldOS, "bottlerocket", OriginExternal)
		DefaultRegistry().Register(FieldPlatform, "nvmesh", OriginExternal)

		assertContains := func(field string, got []string, want string) {
			t.Helper()
			if !slices.Contains(got, want) {
				t.Errorf("AllCriteria%sTypes() missing %q; got %v", field, want, got)
			}
		}
		assertContains("Accelerator", AllCriteriaAcceleratorTypes(), "mi300x")
		assertContains("Intent", AllCriteriaIntentTypes(), "fine-tuning")
		assertContains("OS", AllCriteriaOSTypes(), "bottlerocket")
		assertContains("Platform", AllCriteriaPlatformTypes(), "nvmesh")
	})
}

func TestMergeCriteriaTypes_NoMutationOfInput(t *testing.T) {
	static := []string{"a", "b", "c"}
	original := make([]string, len(static))
	copy(original, static)
	_ = mergeCriteriaTypes(static, []string{"d"})
	if !reflect.DeepEqual(static, original) {
		t.Errorf("mergeCriteriaTypes mutated input slice: got %v, want %v", static, original)
	}
}

func TestCriteriaValidate_AdmitsRegisteredValues(t *testing.T) {
	withRegistry(t, func() {
		DefaultRegistry().Register(FieldService, "ncp-customer-x", OriginExternal)
		DefaultRegistry().Register(FieldAccelerator, "mi300x", OriginExternal)

		c := &Criteria{
			Service:     "ncp-customer-x",
			Accelerator: "mi300x",
			Intent:      CriteriaIntentTraining,
			OS:          CriteriaOSUbuntu,
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("Validate() with registered values rejected: %v", err)
		}
	})
}

func TestCriteriaValidate_StrictRejectsExternal(t *testing.T) {
	withRegistry(t, func() {
		DefaultRegistry().Register(FieldService, "ncp-customer-x", OriginExternal)
		DefaultRegistry().SetStrict(true)
		c := &Criteria{Service: "ncp-customer-x"}
		if err := c.Validate(); err == nil {
			t.Error("strict-mode Validate must reject external-only value")
		}
	})
}
