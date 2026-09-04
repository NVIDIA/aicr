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
	"fmt"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
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

// TestToInternalAgentConfig_ProjectsAKSGPUPoolsPath pins the SDK-level
// plumbing for the AKS pool projection: Client.CollectSnapshot translates
// the facade AgentConfig through toInternalAgentConfig, and a dropped
// AKSGPUPoolsPath would silently produce snapshots whose missing reading
// fails AKS profile-qualified resolution closed.
func TestToInternalAgentConfig_ProjectsAKSGPUPoolsPath(t *testing.T) {
	got := toInternalAgentConfig(&AgentConfig{
		Namespace:       "gpu-operator",
		AKSGPUPoolsPath: "/tmp/pools.json",
		OKEAddonsPath:   "/tmp/addons.json",
	})
	if got.AKSGPUPoolsPath != "/tmp/pools.json" {
		t.Fatalf("AKSGPUPoolsPath = %q, want /tmp/pools.json", got.AKSGPUPoolsPath)
	}
	if got.OKEAddonsPath != "/tmp/addons.json" {
		t.Fatalf("OKEAddonsPath = %q, want /tmp/addons.json", got.OKEAddonsPath)
	}
	if got.Namespace != "gpu-operator" {
		t.Fatalf("Namespace = %q, want gpu-operator", got.Namespace)
	}
	if toInternalAgentConfig(nil) != nil {
		t.Fatal("toInternalAgentConfig(nil) should stay nil")
	}
}

// TestAgentConfigMirrorsInternal turns the facade AgentConfig ↔
// snapshotter.AgentConfig mirror from a convention into an invariant.
//
// The mirror used to be maintained by hand, and its own doc comment conceded
// that "a future field added to either side stays at its zero value until
// plumbed through" — which is exactly what happened: ClusterConfigPath and
// DiscoverNetwork existed upstream while the facade silently dropped them.
// Two assertions close that gap:
//
//  1. Shape — every field of one struct has a same-named, same-typed field on
//     the other. Adding, removing, or retyping a field on either side fails
//     here.
//  2. Value — every facade field set to a distinct non-zero value must arrive
//     on the internal struct. This catches a field that exists on both sides
//     but is missing from (or misassigned in) toInternalAgentConfig, which a
//     shape check alone cannot see.
//
// A new field of a type the value generator does not model fails loudly
// rather than being skipped, so extending the mirror always forces a
// deliberate update here.
func TestAgentConfigMirrorsInternal(t *testing.T) {
	facadeType := reflect.TypeFor[AgentConfig]()
	internalType := reflect.TypeFor[snapshotter.AgentConfig]()

	fieldTypes := func(t reflect.Type) map[string]reflect.Type {
		out := make(map[string]reflect.Type, t.NumField())
		for f := range t.Fields() {
			out[f.Name] = f.Type
		}
		return out
	}

	facadeFields := fieldTypes(facadeType)
	internalFields := fieldTypes(internalType)

	for name, ft := range facadeFields {
		it, ok := internalFields[name]
		if !ok {
			t.Errorf("facade AgentConfig.%s has no counterpart on snapshotter.AgentConfig — "+
				"remove it or add the upstream field", name)
			continue
		}
		if ft != it {
			t.Errorf("AgentConfig.%s type mismatch: facade %s, internal %s", name, ft, it)
		}
	}
	for name := range internalFields {
		if _, ok := facadeFields[name]; !ok {
			t.Errorf("snapshotter.AgentConfig.%s is not mirrored on the facade AgentConfig — "+
				"SDK and CLI callers cannot set it, so it stays at its zero value on every run", name)
		}
	}

	// Value round-trip: fill every facade field with a distinct non-zero
	// value, project, and require it to land.
	cfg := &AgentConfig{}
	filled := reflect.ValueOf(cfg).Elem()
	for i := range facadeType.NumField() {
		field := facadeType.Field(i)
		filled.Field(i).Set(nonZeroFieldValue(t, field.Name, field.Type, i))
	}

	projected := reflect.ValueOf(toInternalAgentConfig(cfg)).Elem()
	for name, want := range facadeFields {
		_ = want
		src := filled.FieldByName(name)
		dst := projected.FieldByName(name)
		if !dst.IsValid() {
			continue // already reported by the shape check above
		}
		if !reflect.DeepEqual(src.Interface(), dst.Interface()) {
			t.Errorf("toInternalAgentConfig dropped or misassigned %s: got %v, want %v",
				name, dst.Interface(), src.Interface())
		}
	}
}

// nonZeroFieldValue builds a distinct non-zero value for a mirror field.
// seed makes every string unique so a copy-paste misassignment (JobName set
// from Namespace, say) is caught rather than compared equal. Unmodeled kinds
// fail the test: a new field type must be handled deliberately, never skipped.
func nonZeroFieldValue(t *testing.T, name string, typ reflect.Type, seed int) reflect.Value {
	t.Helper()

	// resource.Quantity has unexported state, so it cannot be built by
	// generic reflection — parse a real quantity instead.
	if typ == reflect.TypeFor[resource.Quantity]() {
		return reflect.ValueOf(resource.MustParse(fmt.Sprintf("%d", seed+1)))
	}

	//nolint:exhaustive // The default branch fails the test for any unmodeled
	// kind on purpose: a new AgentConfig field type must be handled here
	// deliberately rather than silently skipped by the round-trip.
	switch typ.Kind() {
	case reflect.String:
		return reflect.ValueOf(fmt.Sprintf("mirror-%s-%d", name, seed)).Convert(typ)
	case reflect.Bool:
		return reflect.ValueOf(true).Convert(typ)
	case reflect.Int, reflect.Int64:
		return reflect.ValueOf(int64(seed + 1)).Convert(typ)
	case reflect.Slice:
		slice := reflect.MakeSlice(typ, 1, 1)
		slice.Index(0).Set(nonZeroFieldValue(t, name, typ.Elem(), seed))
		return slice
	case reflect.Map:
		m := reflect.MakeMap(typ)
		m.SetMapIndex(
			nonZeroFieldValue(t, name+"-key", typ.Key(), seed),
			nonZeroFieldValue(t, name+"-value", typ.Elem(), seed+1),
		)
		return m
	case reflect.Struct:
		v := reflect.New(typ).Elem()
		for i := range typ.NumField() {
			f := typ.Field(i)
			if f.Type.Kind() == reflect.String && v.Field(i).CanSet() {
				v.Field(i).Set(nonZeroFieldValue(t, name+"."+f.Name, f.Type, seed))
				return v
			}
		}
		t.Fatalf("AgentConfig.%s: struct type %s has no settable string field; "+
			"extend nonZeroFieldValue so the mirror round-trip stays meaningful", name, typ)
	default:
		t.Fatalf("AgentConfig.%s: unhandled field kind %s; extend nonZeroFieldValue "+
			"so the new field is actually round-tripped", name, typ.Kind())
	}
	return reflect.Value{}
}
