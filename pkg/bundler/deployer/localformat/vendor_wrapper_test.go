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

package localformat

import "testing"

func TestNestUnderSubchart(t *testing.T) {
	tests := []struct {
		name      string
		values    map[string]any
		subchart  string
		wantNil   bool
		wantOuter string
	}{
		{"happy", map[string]any{"a": 1}, "foo", false, "foo"},
		{"nil values", nil, "foo", true, ""},
		{"empty values", map[string]any{}, "foo", true, ""},
		{"empty subchart", map[string]any{"a": 1}, "", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nestUnderSubchart(tt.values, tt.subchart)
			if tt.wantNil {
				if got != nil {
					t.Errorf("got %v, want nil", got)
				}
				return
			}
			if _, ok := got[tt.wantOuter]; !ok {
				t.Errorf("expected outer key %q in %v", tt.wantOuter, got)
			}
		})
	}
}

func TestNestUnderSubchart_InnerMapDeepCopy(t *testing.T) {
	// nestUnderSubchart deep-copies the inner map so the caller's source
	// map is unaffected by downstream mutation of the returned value.
	// Without this, a later writer mutating the result would silently
	// mutate the caller's values map and produce non-deterministic
	// bundle content.
	in := map[string]any{"a": 1}
	out := nestUnderSubchart(in, "foo")
	out["foo"].(map[string]any)["b"] = 2

	if _, leaked := in["b"]; leaked {
		t.Errorf("mutation of nested map leaked back to caller; deep copy not effective")
	}
	if got := in["a"]; got != 1 {
		t.Errorf("source map mutated: a = %v, want 1", got)
	}
}
