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
	"testing"
)

func TestNewTemplateGetter(t *testing.T) {
	tests := []struct {
		name      string
		templates map[string]string
		queryName string
		wantFound bool
		wantValue string
	}{
		{
			name: "finds existing template",
			templates: map[string]string{
				"README.md":     "# README content",
				"manifest.yaml": "apiVersion: v1",
			},
			queryName: "README.md",
			wantFound: true,
			wantValue: "# README content",
		},
		{
			name: "finds second template",
			templates: map[string]string{
				"README.md":     "# README",
				"manifest.yaml": "apiVersion: v1\nkind: ConfigMap",
			},
			queryName: "manifest.yaml",
			wantFound: true,
			wantValue: "apiVersion: v1\nkind: ConfigMap",
		},
		{
			name: "returns false for non-existent template",
			templates: map[string]string{
				"README.md": "content",
			},
			queryName: "nonexistent.yaml",
			wantFound: false,
			wantValue: "",
		},
		{
			name:      "empty templates map",
			templates: map[string]string{},
			queryName: "anything",
			wantFound: false,
			wantValue: "",
		},
		{
			name:      "nil templates map",
			templates: nil,
			queryName: "anything",
			wantFound: false,
			wantValue: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getter := NewTemplateGetter(tt.templates)
			if getter == nil {
				t.Fatal("NewTemplateGetter returned nil")
			}

			gotValue, gotFound := getter(tt.queryName)

			if gotFound != tt.wantFound {
				t.Errorf("found = %v, want %v", gotFound, tt.wantFound)
			}
			if gotValue != tt.wantValue {
				t.Errorf("value = %q, want %q", gotValue, tt.wantValue)
			}
		})
	}
}

func TestStandardTemplates(t *testing.T) {
	tests := []struct {
		name           string
		readmeTemplate string
		queryName      string
		wantFound      bool
		wantValue      string
	}{
		{
			name:           "finds README.md",
			readmeTemplate: "# My Component\n\nThis is the README.",
			queryName:      "README.md",
			wantFound:      true,
			wantValue:      "# My Component\n\nThis is the README.",
		},
		{
			name:           "does not find other templates",
			readmeTemplate: "# README",
			queryName:      "manifest.yaml",
			wantFound:      false,
			wantValue:      "",
		},
		{
			name:           "works with empty readme template",
			readmeTemplate: "",
			queryName:      "README.md",
			wantFound:      true,
			wantValue:      "",
		},
		{
			name:           "works with template containing go template syntax",
			readmeTemplate: "# {{ .Script.Name }}\nVersion: {{ .Script.Version }}",
			queryName:      "README.md",
			wantFound:      true,
			wantValue:      "# {{ .Script.Name }}\nVersion: {{ .Script.Version }}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			getter := StandardTemplates(tt.readmeTemplate)
			if getter == nil {
				t.Fatal("StandardTemplates returned nil")
			}

			gotValue, gotFound := getter(tt.queryName)

			if gotFound != tt.wantFound {
				t.Errorf("found = %v, want %v", gotFound, tt.wantFound)
			}
			if gotValue != tt.wantValue {
				t.Errorf("value = %q, want %q", gotValue, tt.wantValue)
			}
		})
	}
}

func TestTemplateFunc_MultipleTemplates(t *testing.T) {
	// Test that NewTemplateGetter properly handles multiple templates
	templates := map[string]string{
		"README.md":            "# README",
		"kernel-module-params": "kernel config",
		"dcgm-exporter":        "dcgm config",
	}

	getter := NewTemplateGetter(templates)

	// Verify all templates are accessible
	for name, expected := range templates {
		got, found := getter(name)
		if !found {
			t.Errorf("template %q not found", name)
			continue
		}
		if got != expected {
			t.Errorf("template %q = %q, want %q", name, got, expected)
		}
	}

	// Verify non-existent template returns false
	_, found := getter("nonexistent")
	if found {
		t.Error("expected nonexistent template to return false")
	}
}
