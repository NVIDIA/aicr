// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package aicr

import (
	"reflect"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestWrapCriteria_Nil verifies that wrapping a nil upstream criteria
// returns nil — the facade preserves the "unspecified" sentinel.
func TestWrapCriteria_Nil(t *testing.T) {
	if got := WrapCriteria(nil); got != nil {
		t.Fatalf("WrapCriteria(nil) = %+v, want nil", got)
	}
}

// TestToInternalCriteria_Nil verifies the inverse: a nil facade criteria
// translates to a nil upstream pointer (so resolve-path nil checks fire).
func TestToInternalCriteria_Nil(t *testing.T) {
	if got := toInternalCriteria(nil); got != nil {
		t.Fatalf("toInternalCriteria(nil) = %+v, want nil", got)
	}
}

// TestWrapCriteria_AllFieldsProjected confirms every enum-typed field
// on pkg/recipe.Criteria projects to its plain-string counterpart on
// the facade, and that Nodes is carried through unchanged.
func TestWrapCriteria_AllFieldsProjected(t *testing.T) {
	src := &recipe.Criteria{
		Service:     recipe.CriteriaServiceEKS,
		Accelerator: recipe.CriteriaAcceleratorH100,
		Intent:      recipe.CriteriaIntentTraining,
		OS:          recipe.CriteriaOSUbuntu,
		Platform:    recipe.CriteriaPlatformKubeflow,
		Nodes:       8,
	}
	got := WrapCriteria(src)
	if got == nil {
		t.Fatal("WrapCriteria returned nil for non-nil input")
	}
	want := &Criteria{
		Service:     "eks",
		Accelerator: "h100",
		Intent:      "training",
		OS:          "ubuntu",
		Platform:    "kubeflow",
		Nodes:       8,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("WrapCriteria mismatch\n  got:  %+v\n  want: %+v", got, want)
	}
}

// TestCriteriaRoundTrip verifies WrapCriteria followed by
// toInternalCriteria reconstructs the original upstream criteria
// byte-for-byte — the projection is lossless.
func TestCriteriaRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   *recipe.Criteria
	}{
		{
			name: "fully populated",
			in: &recipe.Criteria{
				Service:     recipe.CriteriaServiceGKE,
				Accelerator: recipe.CriteriaAcceleratorB200,
				Intent:      recipe.CriteriaIntentInference,
				OS:          recipe.CriteriaOSCOS,
				Platform:    recipe.CriteriaPlatformNIM,
				Nodes:       16,
			},
		},
		{
			name: "zero-valued",
			in:   &recipe.Criteria{},
		},
		{
			name: "any sentinel",
			in: &recipe.Criteria{
				Service:     recipe.CriteriaServiceAny,
				Accelerator: recipe.CriteriaAcceleratorAny,
				Intent:      recipe.CriteriaIntentAny,
				OS:          recipe.CriteriaOSAny,
				Platform:    recipe.CriteriaPlatformAny,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toInternalCriteria(WrapCriteria(tc.in))
			if !reflect.DeepEqual(got, tc.in) {
				t.Errorf("round-trip mismatch\n  got:  %+v\n  want: %+v", got, tc.in)
			}
		})
	}
}

// TestWrapAllowLists_Nil confirms a nil upstream AllowLists wraps to
// nil — preserves "no fencing" semantics across the boundary.
func TestWrapAllowLists_Nil(t *testing.T) {
	if got := WrapAllowLists(nil); got != nil {
		t.Fatalf("WrapAllowLists(nil) = %+v, want nil", got)
	}
}

// TestToInternalAllowLists_Nil confirms the inverse direction.
func TestToInternalAllowLists_Nil(t *testing.T) {
	if got := ToInternalAllowLists(nil); got != nil {
		t.Fatalf("ToInternalAllowLists(nil) = %+v, want nil", got)
	}
}

// TestWrapAllowLists_AllSlicesProjected confirms every typed enum slice
// projects to the corresponding plain-string slice on the facade.
func TestWrapAllowLists_AllSlicesProjected(t *testing.T) {
	src := &recipe.AllowLists{
		Accelerators: []recipe.CriteriaAcceleratorType{
			recipe.CriteriaAcceleratorH100,
			recipe.CriteriaAcceleratorB200,
		},
		Services: []recipe.CriteriaServiceType{
			recipe.CriteriaServiceEKS,
			recipe.CriteriaServiceGKE,
		},
		Intents: []recipe.CriteriaIntentType{
			recipe.CriteriaIntentTraining,
		},
		OSTypes: []recipe.CriteriaOSType{
			recipe.CriteriaOSUbuntu,
			recipe.CriteriaOSRHEL,
		},
	}
	got := WrapAllowLists(src)
	if got == nil {
		t.Fatal("WrapAllowLists returned nil for non-nil input")
	}
	want := &AllowLists{
		Accelerators: []string{"h100", "b200"},
		Services:     []string{"eks", "gke"},
		Intents:      []string{"training"},
		OSTypes:      []string{"ubuntu", "rhel"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("WrapAllowLists mismatch\n  got:  %+v\n  want: %+v", got, want)
	}
}

// TestAllowListsRoundTrip confirms WrapAllowLists followed by
// ToInternalAllowLists reconstructs the upstream AllowLists.
func TestAllowListsRoundTrip(t *testing.T) {
	src := &recipe.AllowLists{
		Accelerators: []recipe.CriteriaAcceleratorType{
			recipe.CriteriaAcceleratorH100,
			recipe.CriteriaAcceleratorL40,
		},
		Services: []recipe.CriteriaServiceType{recipe.CriteriaServiceEKS},
		Intents:  []recipe.CriteriaIntentType{recipe.CriteriaIntentInference},
		OSTypes:  []recipe.CriteriaOSType{recipe.CriteriaOSUbuntu},
	}
	got := ToInternalAllowLists(WrapAllowLists(src))
	if !reflect.DeepEqual(got, src) {
		t.Errorf("AllowLists round-trip mismatch\n  got:  %+v\n  want: %+v", got, src)
	}
}

// TestWrapAllowLists_EmptySlicesBecomeNil verifies the facade preserves
// IsEmpty semantics by mapping empty slices to nil (so a nil-receiver
// or all-nil-slices AllowLists still reports "no fencing").
func TestWrapAllowLists_EmptySlicesBecomeNil(t *testing.T) {
	src := &recipe.AllowLists{
		Accelerators: []recipe.CriteriaAcceleratorType{},
		Services:     nil,
		Intents:      []recipe.CriteriaIntentType{},
		OSTypes:      nil,
	}
	got := WrapAllowLists(src)
	if got == nil {
		t.Fatal("WrapAllowLists returned nil for non-nil input")
	}
	if got.Accelerators != nil || got.Services != nil || got.Intents != nil || got.OSTypes != nil {
		t.Errorf("expected all slices to project to nil; got %+v", got)
	}
	// Round-trip the empty-but-non-nil facade through toInternal and
	// confirm the upstream AllowLists.IsEmpty returns true so the
	// resolve-path "no fencing" branch is taken.
	rt := ToInternalAllowLists(got)
	if rt == nil {
		t.Fatal("ToInternalAllowLists returned nil for non-nil empty input")
	}
	if !rt.IsEmpty() {
		t.Errorf("round-tripped AllowLists.IsEmpty() = false, want true")
	}
}

// TestStringsFromTypes_NilAndEmpty confirms the generic helper treats
// nil and empty input identically (returns nil) — critical so the
// facade preserves IsEmpty semantics.
func TestStringsFromTypes_NilAndEmpty(t *testing.T) {
	if got := stringsFromTypes[recipe.CriteriaServiceType](nil); got != nil {
		t.Errorf("stringsFromTypes(nil) = %v, want nil", got)
	}
	if got := stringsFromTypes([]recipe.CriteriaServiceType{}); got != nil {
		t.Errorf("stringsFromTypes(empty) = %v, want nil", got)
	}
}

// TestTypesFromStrings_NilAndEmpty mirrors the prior test for the
// reverse generic helper.
func TestTypesFromStrings_NilAndEmpty(t *testing.T) {
	if got := typesFromStrings[recipe.CriteriaServiceType](nil); got != nil {
		t.Errorf("typesFromStrings(nil) = %v, want nil", got)
	}
	if got := typesFromStrings[recipe.CriteriaServiceType]([]string{}); got != nil {
		t.Errorf("typesFromStrings(empty) = %v, want nil", got)
	}
}

// TestStringsFromTypes_PreservesOrder confirms the generic helper does
// not reorder elements — ordering is observable in error messages and
// must be stable across the boundary.
func TestStringsFromTypes_PreservesOrder(t *testing.T) {
	in := []recipe.CriteriaAcceleratorType{
		recipe.CriteriaAcceleratorB200,
		recipe.CriteriaAcceleratorH100,
		recipe.CriteriaAcceleratorL40,
	}
	got := stringsFromTypes(in)
	want := []string{"b200", "h100", "l40"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ordering mismatch\n  got:  %v\n  want: %v", got, want)
	}
}
