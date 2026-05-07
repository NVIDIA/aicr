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
	"strings"
	"testing"
)

const testMutatedValue = "changed"

func Test_computeChecksum(t *testing.T) {
	tests := []struct {
		name    string
		content []byte
		want    string
	}{
		{
			name:    "empty content",
			content: []byte{},
			want:    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
		{
			name:    "hello world",
			content: []byte("hello world"),
			want:    "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeChecksum(tt.content)
			if got != tt.want {
				t.Errorf("computeChecksum() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetConfigValue(t *testing.T) {
	tests := []struct {
		name         string
		config       map[string]string
		key          string
		defaultValue string
		want         string
	}{
		{
			name:         "key exists",
			config:       map[string]string{"key": "value"},
			key:          "key",
			defaultValue: "default",
			want:         "value",
		},
		{
			name:         "key missing",
			config:       map[string]string{},
			key:          "key",
			defaultValue: "default",
			want:         "default",
		},
		{
			name:         "empty value uses default",
			config:       map[string]string{"key": ""},
			key:          "key",
			defaultValue: "default",
			want:         "default",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetConfigValue(tt.config, tt.key, tt.defaultValue)
			if got != tt.want {
				t.Errorf("GetConfigValue() = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_extractCustomLabels(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]string
		want   map[string]string
	}{
		{
			name: "extracts labels",
			config: map[string]string{
				"label_env":  "prod",
				"label_team": "platform",
				"other_key":  "value",
			},
			want: map[string]string{
				"env":  "prod",
				"team": "platform",
			},
		},
		{
			name:   "empty config",
			config: map[string]string{},
			want:   map[string]string{},
		},
		{
			name: "no labels",
			config: map[string]string{
				"key": "value",
			},
			want: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractCustomLabels(tt.config)
			if len(got) != len(tt.want) {
				t.Errorf("extractCustomLabels() len = %v, want %v", len(got), len(tt.want))
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("extractCustomLabels()[%v] = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

func Test_extractCustomAnnotations(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]string
		want   map[string]string
	}{
		{
			name: "extracts annotations",
			config: map[string]string{
				"annotation_key1": "value1",
				"annotation_key2": "value2",
				"other_key":       "value",
			},
			want: map[string]string{
				"key1": "value1",
				"key2": "value2",
			},
		},
		{
			name:   "empty config",
			config: map[string]string{},
			want:   map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractCustomAnnotations(tt.config)
			if len(got) != len(tt.want) {
				t.Errorf("extractCustomAnnotations() len = %v, want %v", len(got), len(tt.want))
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("extractCustomAnnotations()[%v] = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

func TestMarshalYAML(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		want    string
		wantErr bool
	}{
		{
			name:    "simple string",
			value:   "hello",
			want:    "hello\n",
			wantErr: false,
		},
		{
			name:    "simple map",
			value:   map[string]string{"key": "value"},
			want:    "key: value\n",
			wantErr: false,
		},
		{
			name: "nested struct",
			value: struct {
				Name    string `yaml:"name"`
				Version string `yaml:"version"`
			}{Name: "test", Version: "v1.0.0"},
			want:    "name: test\nversion: v1.0.0\n",
			wantErr: false,
		},
		{
			name:    "slice",
			value:   []string{"a", "b", "c"},
			want:    "- a\n- b\n- c\n",
			wantErr: false,
		},
		{
			name:    "nil value",
			value:   nil,
			want:    "null\n",
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MarshalYAML(tt.value)
			if (err != nil) != tt.wantErr {
				t.Errorf("MarshalYAML() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && string(got) != tt.want {
				t.Errorf("MarshalYAML() = %q, want %q", string(got), tt.want)
			}
		})
	}
}

func Test_parseBoolString(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"true string", "true", true},
		{"false string", "false", false},
		{"1 value", "1", true},
		{"0 value", "0", false},
		{"empty string", "", false},
		{"other string", "yes", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseBoolString(tt.value)
			if got != tt.want {
				t.Errorf("parseBoolString(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

// TestDeepCopyMap verifies deep copy produces an independent copy that
// preserves types and structure, and mutations to the copy don't leak.
func TestDeepCopyMap(t *testing.T) {
	t.Run("nil returns empty map", func(t *testing.T) {
		result := DeepCopyMap(nil)
		if result == nil {
			t.Error("nil input should return non-nil empty map")
		}
		if len(result) != 0 {
			t.Errorf("nil input should return empty map, got %v", result)
		}
	})

	t.Run("nested maps are independent", func(t *testing.T) {
		original := map[string]any{
			"driver": map[string]any{
				"version":  "580",
				"registry": "nvcr.io",
			},
			"enabled": true,
		}
		copied := DeepCopyMap(original)

		copied["driver"].(map[string]any)["version"] = testMutatedValue
		copied["enabled"] = false

		if original["driver"].(map[string]any)["version"] != "580" {
			t.Error("mutation leaked to original nested map")
		}
		if original["enabled"] != true {
			t.Error("mutation leaked to original scalar")
		}
	})

	t.Run("slices are independent", func(t *testing.T) {
		original := map[string]any{
			"items": []any{
				map[string]any{"key": "nvidia.com/gpu", "effect": "NoSchedule"},
			},
		}
		copied := DeepCopyMap(original)

		copied["items"].([]any)[0].(map[string]any)["key"] = testMutatedValue

		origKey := original["items"].([]any)[0].(map[string]any)["key"]
		if origKey != "nvidia.com/gpu" {
			t.Errorf("mutation leaked to original slice element, got %v", origKey)
		}
	})

	t.Run("preserves types without drift", func(t *testing.T) {
		original := map[string]any{
			"count":   42,
			"ratio":   3.14,
			"name":    "test",
			"enabled": true,
			"empty":   nil,
		}
		copied := DeepCopyMap(original)

		if v, ok := copied["count"].(int); !ok || v != 42 {
			t.Errorf("count type/value wrong: %T %v", copied["count"], copied["count"])
		}
		if v, ok := copied["ratio"].(float64); !ok || v != 3.14 {
			t.Errorf("ratio type/value wrong: %T %v", copied["ratio"], copied["ratio"])
		}
		if v, ok := copied["name"].(string); !ok || v != "test" {
			t.Errorf("name type/value wrong: %T %v", copied["name"], copied["name"])
		}
		if v, ok := copied["enabled"].(bool); !ok || !v {
			t.Errorf("enabled type/value wrong: %T %v", copied["enabled"], copied["enabled"])
		}
		if copied["empty"] != nil {
			t.Errorf("empty should be nil, got %v", copied["empty"])
		}
	})

	t.Run("deeply nested structure", func(t *testing.T) {
		original := map[string]any{
			"a": map[string]any{
				"b": map[string]any{
					"c": map[string]any{"d": "deep"},
				},
			},
		}
		copied := DeepCopyMap(original)

		copied["a"].(map[string]any)["b"].(map[string]any)["c"].(map[string]any)["d"] = testMutatedValue

		origVal := original["a"].(map[string]any)["b"].(map[string]any)["c"].(map[string]any)["d"]
		if origVal != "deep" {
			t.Error("mutation leaked through deeply nested maps")
		}
	})
}

func TestMarshalYAMLWithHeader(t *testing.T) {
	tests := []struct {
		name    string
		value   any
		header  ValuesHeader
		verify  func(t *testing.T, result string)
		wantErr bool
	}{
		{
			name:  "includes header with all fields",
			value: map[string]string{"key": "value"},
			header: ValuesHeader{
				ComponentName:  "GPU Operator",
				BundlerVersion: "1.2.3",
				RecipeVersion:  "2.0.0",
			},
			verify: func(t *testing.T, result string) {
				if !strings.Contains(result, "# GPU Operator Helm Values") {
					t.Error("missing component name in header")
				}
				if !strings.Contains(result, "# Bundler Version: 1.2.3") {
					t.Error("missing bundler version in header")
				}
				if !strings.Contains(result, "# Recipe Version: 2.0.0") {
					t.Error("missing recipe version in header")
				}
				if !strings.Contains(result, "key: value") {
					t.Error("missing YAML content")
				}
			},
		},
		{
			name:  "handles empty header fields",
			value: map[string]string{"test": "data"},
			header: ValuesHeader{
				ComponentName:  "",
				BundlerVersion: "",
				RecipeVersion:  "",
			},
			verify: func(t *testing.T, result string) {
				if !strings.Contains(result, "# Generated from Cloud Native Stack Recipe") {
					t.Error("missing standard header line")
				}
				if !strings.Contains(result, "test: data") {
					t.Error("missing YAML content")
				}
			},
		},
		{
			name: "handles complex nested structure",
			value: map[string]any{
				"driver": map[string]any{
					"version": "550.0.0",
					"enabled": true,
				},
				"mig": map[string]any{
					"strategy": "mixed",
				},
			},
			header: ValuesHeader{
				ComponentName:  "Test Component",
				BundlerVersion: "v1.0.0",
				RecipeVersion:  "v2.0.0",
			},
			verify: func(t *testing.T, result string) {
				if !strings.Contains(result, "driver:") {
					t.Error("missing driver section")
				}
				if !strings.Contains(result, "version: 550.0.0") {
					t.Error("missing driver version")
				}
				if !strings.Contains(result, "mig:") {
					t.Error("missing mig section")
				}
			},
		},
		{
			name:  "handles nil value",
			value: nil,
			header: ValuesHeader{
				ComponentName:  "Test",
				BundlerVersion: "1.0.0",
				RecipeVersion:  "1.0.0",
			},
			verify: func(t *testing.T, result string) {
				if !strings.Contains(result, "null") {
					t.Error("nil should serialize to null")
				}
			},
		},
		{
			name:  "handles slice values",
			value: []string{"item1", "item2"},
			header: ValuesHeader{
				ComponentName:  "List Test",
				BundlerVersion: "1.0.0",
				RecipeVersion:  "1.0.0",
			},
			verify: func(t *testing.T, result string) {
				if !strings.Contains(result, "- item1") {
					t.Error("missing first item")
				}
				if !strings.Contains(result, "- item2") {
					t.Error("missing second item")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MarshalYAMLWithHeader(tt.value, tt.header)
			if (err != nil) != tt.wantErr {
				t.Errorf("MarshalYAMLWithHeader() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && tt.verify != nil {
				tt.verify(t, string(got))
			}
		})
	}
}
