// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"testing"
)

func TestComputeChecksum(t *testing.T) {
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
			got := ComputeChecksum(tt.content)
			if got != tt.want {
				t.Errorf("ComputeChecksum() = %v, want %v", got, tt.want)
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

func TestExtractCustomLabels(t *testing.T) {
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
			got := ExtractCustomLabels(tt.config)
			if len(got) != len(tt.want) {
				t.Errorf("ExtractCustomLabels() len = %v, want %v", len(got), len(tt.want))
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("ExtractCustomLabels()[%v] = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

func TestExtractCustomAnnotations(t *testing.T) {
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
			got := ExtractCustomAnnotations(tt.config)
			if len(got) != len(tt.want) {
				t.Errorf("ExtractCustomAnnotations() len = %v, want %v", len(got), len(tt.want))
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("ExtractCustomAnnotations()[%v] = %v, want %v", k, got[k], v)
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

func TestBoolToString(t *testing.T) {
	tests := []struct {
		name  string
		value bool
		want  string
	}{
		{"true value", true, StrTrue},
		{"false value", false, StrFalse},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BoolToString(tt.value)
			if got != tt.want {
				t.Errorf("BoolToString(%v) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestParseBoolString(t *testing.T) {
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
			got := ParseBoolString(tt.value)
			if got != tt.want {
				t.Errorf("ParseBoolString(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
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
				if !contains(result, "# GPU Operator Helm Values") {
					t.Error("missing component name in header")
				}
				if !contains(result, "# Bundler Version: 1.2.3") {
					t.Error("missing bundler version in header")
				}
				if !contains(result, "# Recipe Version: 2.0.0") {
					t.Error("missing recipe version in header")
				}
				if !contains(result, "key: value") {
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
				if !contains(result, "# Generated from Cloud Native Stack Recipe") {
					t.Error("missing standard header line")
				}
				if !contains(result, "test: data") {
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
				if !contains(result, "driver:") {
					t.Error("missing driver section")
				}
				if !contains(result, "version: 550.0.0") {
					t.Error("missing driver version")
				}
				if !contains(result, "mig:") {
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
				if !contains(result, "null") {
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
				if !contains(result, "- item1") {
					t.Error("missing first item")
				}
				if !contains(result, "- item2") {
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
