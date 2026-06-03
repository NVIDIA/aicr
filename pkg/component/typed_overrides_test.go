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

package component

import (
	"reflect"
	"testing"
)

func TestApplyTypedOverrides(t *testing.T) {
	tests := []struct {
		name      string
		target    map[string]any
		overrides map[string]any
		wantErr   bool
		verify    func(t *testing.T, got map[string]any)
	}{
		{
			name:      "list value replaces and renders as slice",
			target:    map[string]any{"allowedSourceRanges": []any{}},
			overrides: map[string]any{"allowedSourceRanges": []any{"216.228.127.128/30"}},
			verify: func(t *testing.T, got map[string]any) {
				if !reflect.DeepEqual(got["allowedSourceRanges"], []any{"216.228.127.128/30"}) {
					t.Errorf("allowedSourceRanges = %#v", got["allowedSourceRanges"])
				}
			},
		},
		{
			name:      "creates intermediate maps for nested list",
			target:    map[string]any{},
			overrides: map[string]any{"a.b.list": []any{int64(1), int64(2)}},
			verify: func(t *testing.T, got map[string]any) {
				a := got["a"].(map[string]any)
				b := a["b"].(map[string]any)
				if !reflect.DeepEqual(b["list"], []any{int64(1), int64(2)}) {
					t.Errorf("a.b.list = %#v", b["list"])
				}
			},
		},
		{
			name: "object value deep-merges into existing map",
			target: map[string]any{
				"driver": map[string]any{
					"env":     map[string]any{"KEEP": "yes", "OVER": "old"},
					"version": "1.0",
				},
			},
			overrides: map[string]any{"driver": map[string]any{"env": map[string]any{"OVER": "new", "ADD": "x"}}},
			verify: func(t *testing.T, got map[string]any) {
				driver := got["driver"].(map[string]any)
				if driver["version"] != "1.0" {
					t.Errorf("version lost in merge: %#v", driver["version"])
				}
				env := driver["env"].(map[string]any)
				want := map[string]any{"KEEP": "yes", "OVER": "new", "ADD": "x"}
				if !reflect.DeepEqual(env, want) {
					t.Errorf("driver.env = %#v, want %#v", env, want)
				}
			},
		},
		{
			name:      "list replaces existing map (no merge for non-map)",
			target:    map[string]any{"field": map[string]any{"a": "b"}},
			overrides: map[string]any{"field": []any{"x"}},
			verify: func(t *testing.T, got map[string]any) {
				if !reflect.DeepEqual(got["field"], []any{"x"}) {
					t.Errorf("field = %#v, want [x]", got["field"])
				}
			},
		},
		{
			name:      "nil value in merged map deletes key",
			target:    map[string]any{"m": map[string]any{"keep": "1", "drop": "2"}},
			overrides: map[string]any{"m": map[string]any{"drop": nil}},
			verify: func(t *testing.T, got map[string]any) {
				m := got["m"].(map[string]any)
				if _, ok := m["drop"]; ok {
					t.Error("drop key should have been deleted")
				}
				if m["keep"] != "1" {
					t.Errorf("keep = %#v, want 1", m["keep"])
				}
			},
		},
		{
			// Regression: when no map pre-exists at the path, a null-valued key
			// in the override object must still be dropped (not stored as a
			// literal null), matching the documented delete semantics.
			name:      "nil value dropped when destination map is absent",
			target:    map[string]any{},
			overrides: map[string]any{"env": map[string]any{"DROP": nil, "KEEP": "v"}},
			verify: func(t *testing.T, got map[string]any) {
				env := got["env"].(map[string]any)
				if _, ok := env["DROP"]; ok {
					t.Errorf("DROP must be absent, got %#v", env["DROP"])
				}
				if env["KEEP"] != "v" {
					t.Errorf("KEEP = %#v, want v", env["KEEP"])
				}
			},
		},
		{
			// Regression: the destination-absent null-drop must also hold for a
			// NESTED map, not just the top-level path key. Here driver exists
			// but driver.env does not; a null inside the freshly-created env map
			// must be dropped, not stored as a literal null.
			name:      "nil value dropped in nested map when inner destination is absent",
			target:    map[string]any{"driver": map[string]any{"version": "1.0"}},
			overrides: map[string]any{"driver": map[string]any{"env": map[string]any{"DROP": nil, "KEEP": "v"}}},
			verify: func(t *testing.T, got map[string]any) {
				driver := got["driver"].(map[string]any)
				if driver["version"] != "1.0" {
					t.Errorf("version lost in merge: %#v", driver["version"])
				}
				env := driver["env"].(map[string]any)
				if _, ok := env["DROP"]; ok {
					t.Errorf("nested DROP must be absent, got %#v", env["DROP"])
				}
				if env["KEEP"] != "v" {
					t.Errorf("nested KEEP = %#v, want v", env["KEEP"])
				}
			},
		},
		{
			name:      "intermediate non-map segment is an error",
			target:    map[string]any{"a": "scalar"},
			overrides: map[string]any{"a.b": []any{"x"}},
			wantErr:   true,
		},
		{
			name:      "empty overrides is a no-op",
			target:    map[string]any{"x": "y"},
			overrides: map[string]any{},
			verify: func(t *testing.T, got map[string]any) {
				if got["x"] != "y" {
					t.Errorf("target mutated: %#v", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ApplyTypedOverrides(tt.target, tt.overrides)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ApplyTypedOverrides() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			tt.verify(t, tt.target)
		})
	}
}

func TestApplyTypedOverrides_NilTarget(t *testing.T) {
	if err := ApplyTypedOverrides(nil, map[string]any{"a": "b"}); err == nil {
		t.Error("expected error for nil target, got nil")
	}
}

// TestApplyTypedOverrides_NoAliasing verifies the override source is not
// aliased into the target — mutating the applied value must not reach back
// into the override map (and vice versa).
func TestApplyTypedOverrides_NoAliasing(t *testing.T) {
	src := []any{"a", "b"}
	overrides := map[string]any{"list": src}
	target := map[string]any{}

	if err := ApplyTypedOverrides(target, overrides); err != nil {
		t.Fatalf("ApplyTypedOverrides() error = %v", err)
	}

	// Mutate the source slice; target must be unaffected.
	src[0] = "MUTATED"
	got := target["list"].([]any)
	if got[0] != "a" {
		t.Errorf("target aliased override source: got[0] = %v, want a", got[0])
	}
}

// TestApplyTypedOverrides_OverlappingPathsDeterministic guards against the
// nondeterministic apply order that map-iteration produced before paths were
// sorted shallowest-first. When a parent-object path ("driver.env") and a
// deeper scalar path ("driver.env.HTTPS_PROXY") collide on a leaf, the deeper,
// more-specific override must always win — and the result must be identical
// across many runs (Go randomizes map iteration). Run repeatedly to make a
// regression flaky-fail loudly rather than pass intermittently.
func TestApplyTypedOverrides_OverlappingPathsDeterministic(t *testing.T) {
	for i := 0; i < 200; i++ {
		target := map[string]any{}
		overrides := map[string]any{
			// Parent object also carries HTTPS_PROXY, colliding with the deeper path.
			"driver.env":             map[string]any{"HTTPS_PROXY": "from-object", "NO_PROXY": "keep"},
			"driver.env.HTTPS_PROXY": "from-scalar",
		}
		if err := ApplyTypedOverrides(target, overrides); err != nil {
			t.Fatalf("ApplyTypedOverrides() error = %v", err)
		}
		env := target["driver"].(map[string]any)["env"].(map[string]any)
		if env["HTTPS_PROXY"] != "from-scalar" {
			t.Fatalf("run %d: HTTPS_PROXY = %#v, want \"from-scalar\" (deeper path must win deterministically)", i, env["HTTPS_PROXY"])
		}
		if env["NO_PROXY"] != "keep" {
			t.Fatalf("run %d: NO_PROXY = %#v, want \"keep\" (non-colliding object key must survive)", i, env["NO_PROXY"])
		}
	}
}
