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

package main

import (
	"testing"
)

func TestFlattenValues(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]any
		expected map[string]string
	}{
		{
			name:     "nil map",
			input:    nil,
			expected: map[string]string{},
		},
		{
			name:     "empty map",
			input:    map[string]any{},
			expected: map[string]string{},
		},
		{
			name: "flat string values",
			input: map[string]any{
				"key1": "value1",
				"key2": "value2",
			},
			expected: map[string]string{
				"key1": "value1",
				"key2": "value2",
			},
		},
		{
			name: "nested map",
			input: map[string]any{
				"driver": map[string]any{
					"version": "570.86.16",
					"enabled": true,
				},
			},
			expected: map[string]string{
				"driver.version": "570.86.16",
				"driver.enabled": "true",
			},
		},
		{
			name: "deeply nested map",
			input: map[string]any{
				"a": map[string]any{
					"b": map[string]any{
						"c": "deep",
					},
				},
			},
			expected: map[string]string{
				"a.b.c": "deep",
			},
		},
		{
			name: "boolean value",
			input: map[string]any{
				"enabled": true,
			},
			expected: map[string]string{
				"enabled": "true",
			},
		},
		{
			name: "numeric values",
			input: map[string]any{
				"replicas": float64(3),
				"count":    42,
			},
			expected: map[string]string{
				"replicas": "3",
				"count":    "42",
			},
		},
		{
			name: "array value",
			input: map[string]any{
				"tolerations": []any{"gpu", "infiniband"},
			},
			expected: map[string]string{
				"tolerations": `["gpu","infiniband"]`,
			},
		},
		{
			name: "empty array is skipped",
			input: map[string]any{
				"tolerations": []any{},
			},
			expected: map[string]string{},
		},
		{
			name: "mixed types",
			input: map[string]any{
				"name":    "operator",
				"enabled": false,
				"nested": map[string]any{
					"port": float64(8080),
				},
			},
			expected: map[string]string{
				"name":        "operator",
				"enabled":     "false",
				"nested.port": "8080",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := flattenValues(tt.input)
			if len(got) != len(tt.expected) {
				t.Errorf("flattenValues() returned %d keys, want %d\ngot:  %v\nwant: %v",
					len(got), len(tt.expected), got, tt.expected)
				return
			}
			for k, want := range tt.expected {
				if gotV, ok := got[k]; !ok {
					t.Errorf("flattenValues() missing key %q", k)
				} else if gotV != want {
					t.Errorf("flattenValues()[%q] = %q, want %q", k, gotV, want)
				}
			}
		})
	}
}

func TestHelmValuesEqual(t *testing.T) {
	tests := []struct {
		name     string
		expected string
		actual   string
		want     bool
	}{
		{
			name:     "identical strings",
			expected: "570.86.16",
			actual:   "570.86.16",
			want:     true,
		},
		{
			name:     "different strings",
			expected: "570.86.16",
			actual:   "570.86.17",
			want:     false,
		},
		{
			name:     "integer equality via float",
			expected: "3",
			actual:   "3.0",
			want:     true,
		},
		{
			name:     "float equality",
			expected: "3.14",
			actual:   "3.14",
			want:     true,
		},
		{
			name:     "float inequality",
			expected: "3.14",
			actual:   "3.15",
			want:     false,
		},
		{
			name:     "boolean true variants",
			expected: "true",
			actual:   "1",
			want:     true,
		},
		{
			name:     "boolean false variants",
			expected: "false",
			actual:   "0",
			want:     true,
		},
		{
			name:     "boolean mismatch",
			expected: "true",
			actual:   "false",
			want:     false,
		},
		{
			name:     "whitespace trimming",
			expected: "  hello  ",
			actual:   "hello",
			want:     true,
		},
		{
			name:     "empty strings",
			expected: "",
			actual:   "",
			want:     true,
		},
		{
			name:     "non-numeric non-bool mismatch",
			expected: "abc",
			actual:   "def",
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := helmValuesEqual(tt.expected, tt.actual)
			if got != tt.want {
				t.Errorf("helmValuesEqual(%q, %q) = %v, want %v",
					tt.expected, tt.actual, got, tt.want)
			}
		})
	}
}
