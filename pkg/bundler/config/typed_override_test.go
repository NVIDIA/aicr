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

package config

import (
	"reflect"
	"testing"
)

func TestParseValueOverridesJSON(t *testing.T) {
	tests := []struct {
		name      string
		specs     []string
		wantErr   bool
		component string
		path      string
		wantValue any
	}{
		{
			name:      "list value",
			specs:     []string{`agentgateway:allowedSourceRanges=["216.228.127.128/30"]`},
			component: "agentgateway",
			path:      "allowedSourceRanges",
			wantValue: []any{"216.228.127.128/30"},
		},
		{
			name:      "object value",
			specs:     []string{`gpuoperator:driver.env={"FOO":"bar"}`},
			component: "gpuoperator",
			path:      "driver.env",
			wantValue: map[string]any{"FOO": "bar"},
		},
		{
			name:      "scalar string value (quoted JSON)",
			specs:     []string{`comp:field="hello"`},
			component: "comp",
			path:      "field",
			wantValue: "hello",
		},
		{
			name:      "bool value",
			specs:     []string{`comp:field=true`},
			component: "comp",
			path:      "field",
			wantValue: true,
		},
		{
			name:    "invalid JSON",
			specs:   []string{`comp:field=[not json`},
			wantErr: true,
		},
		{
			name:    "missing colon",
			specs:   []string{`no-colon`},
			wantErr: true,
		},
		{
			name:    "missing equals",
			specs:   []string{`comp:path-no-equals`},
			wantErr: true,
		},
		{
			name:    "template injection in path rejected",
			specs:   []string{`comp:{{evil}}=[]`},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseValueOverridesJSON(tt.specs)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseValueOverridesJSON() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(got) != 1 {
				t.Fatalf("got %d entries, want 1", len(got))
			}
			if got[0].Component != tt.component || got[0].Path != tt.path {
				t.Errorf("got component=%q path=%q, want %q/%q", got[0].Component, got[0].Path, tt.component, tt.path)
			}
			if !reflect.DeepEqual(got[0].Value, tt.wantValue) {
				t.Errorf("value = %#v, want %#v", got[0].Value, tt.wantValue)
			}
		})
	}
}

func TestParseTypedOverrides_DecoderError(t *testing.T) {
	_, err := ParseTypedOverrides([]string{"comp:field=x"}, "--set-file", func(string) (any, error) {
		return nil, errInvalidTestValue
	})
	if err == nil {
		t.Fatal("expected error when decoder fails, got nil")
	}
}

var errInvalidTestValue = &decoderTestError{}

type decoderTestError struct{}

func (*decoderTestError) Error() string { return "decode failed" }

func TestWithValueOverridesTypedPaths(t *testing.T) {
	t.Run("applies and last-wins per path", func(t *testing.T) {
		c := NewConfig(WithValueOverridesTypedPaths([]TypedComponentPath{
			{Component: "agentgateway", Path: "allowedSourceRanges", Value: []any{"1.0.0.0/8"}},
			{Component: "agentgateway", Path: "allowedSourceRanges", Value: []any{"2.0.0.0/8"}},
			{Component: "gpuoperator", Path: "driver.env", Value: map[string]any{"A": "b"}},
		}))

		got := c.ValueOverridesTyped()
		if !reflect.DeepEqual(got["agentgateway"]["allowedSourceRanges"], []any{"2.0.0.0/8"}) {
			t.Errorf("allowedSourceRanges = %#v, want last value [2.0.0.0/8]", got["agentgateway"]["allowedSourceRanges"])
		}
		if !reflect.DeepEqual(got["gpuoperator"]["driver.env"], map[string]any{"A": "b"}) {
			t.Errorf("driver.env = %#v", got["gpuoperator"]["driver.env"])
		}
	})

	t.Run("getter returns a deep copy", func(t *testing.T) {
		c := NewConfig(WithValueOverridesTypedPaths([]TypedComponentPath{
			{Component: "comp", Path: "list", Value: []any{"a"}},
		}))
		got := c.ValueOverridesTyped()
		// Mutate the returned copy; the config must not observe the change.
		got["comp"]["list"] = []any{"mutated"}
		got["comp"]["new"] = "x"

		fresh := c.ValueOverridesTyped()
		if !reflect.DeepEqual(fresh["comp"]["list"], []any{"a"}) {
			t.Errorf("config backing map was mutated through getter: %#v", fresh["comp"]["list"])
		}
		if _, ok := fresh["comp"]["new"]; ok {
			t.Error("added key leaked back into config backing map")
		}
	})

	t.Run("no typed overrides returns nil", func(t *testing.T) {
		c := NewConfig()
		if got := c.ValueOverridesTyped(); got != nil {
			t.Errorf("ValueOverridesTyped() = %#v, want nil", got)
		}
	})

	t.Run("deep-copies value on insertion", func(t *testing.T) {
		// A caller that retains and mutates the source value after NewConfig
		// must not be able to reach into the Config's backing store.
		srcList := []any{"a"}
		srcMap := map[string]any{"k": "v"}
		c := NewConfig(WithValueOverridesTypedPaths([]TypedComponentPath{
			{Component: "comp", Path: "list", Value: srcList},
			{Component: "comp", Path: "obj", Value: srcMap},
		}))

		// Mutate the originals; the Config must be unaffected.
		srcList[0] = "MUTATED"
		srcMap["k"] = "MUTATED"

		got := c.ValueOverridesTyped()
		if !reflect.DeepEqual(got["comp"]["list"], []any{"a"}) {
			t.Errorf("list aliased source on insertion: %#v", got["comp"]["list"])
		}
		if !reflect.DeepEqual(got["comp"]["obj"], map[string]any{"k": "v"}) {
			t.Errorf("obj aliased source on insertion: %#v", got["comp"]["obj"])
		}
	})
}
