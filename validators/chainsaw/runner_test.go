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

package chainsaw

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/validation/field"
)

func TestIsChainsawTest(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "valid chainsaw test",
			raw: `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: gpu-operator-health`,
			want: true,
		},
		{
			name: "raw k8s resource",
			raw: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-operator`,
			want: false,
		},
		{
			name: "has apiVersion but wrong kind",
			raw: `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Configuration`,
			want: false,
		},
		{
			name: "has kind Test but wrong apiVersion",
			raw: `apiVersion: v1
kind: Test`,
			want: false,
		},
		{
			name: "empty string",
			raw:  "",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isChainsawTest(tt.raw)
			if got != tt.want {
				t.Errorf("isChainsawTest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSplitYAMLDocuments(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantLen int
		wantErr bool
	}{
		{
			name:    "empty string",
			raw:     "",
			wantLen: 0,
			wantErr: false,
		},
		{
			name: "single document",
			raw: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test`,
			wantLen: 1,
			wantErr: false,
		},
		{
			name: "two documents",
			raw: `apiVersion: v1
kind: ConfigMap
metadata:
  name: test1
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: test2`,
			wantLen: 2,
			wantErr: false,
		},
		{
			name:    "only separator",
			raw:     "---",
			wantLen: 0,
			wantErr: false,
		},
		{
			name: "leading separator",
			raw: `---
apiVersion: v1
kind: Service
metadata:
  name: test`,
			wantLen: 1,
			wantErr: false,
		},
		{
			name:    "invalid yaml",
			raw:     "{{invalid: yaml: [}",
			wantLen: 0,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docs, err := splitYAMLDocuments(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Errorf("splitYAMLDocuments() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if len(docs) != tt.wantLen {
				t.Errorf("splitYAMLDocuments() returned %d docs, want %d", len(docs), tt.wantLen)
			}
		})
	}
}

func TestSplitYAMLDocumentsContent(t *testing.T) {
	raw := `apiVersion: v1
kind: ConfigMap
metadata:
  name: doc1
---
apiVersion: v1
kind: Service
metadata:
  name: doc2`

	docs, err := splitYAMLDocuments(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("expected 2 docs, got %d", len(docs))
	}

	if kind, ok := docs[0]["kind"].(string); !ok || kind != "ConfigMap" {
		t.Errorf("doc[0] kind = %v, want ConfigMap", docs[0]["kind"])
	}
	if kind, ok := docs[1]["kind"].(string); !ok || kind != "Service" {
		t.Errorf("doc[1] kind = %v, want Service", docs[1]["kind"])
	}
}

func TestFormatFieldErrors(t *testing.T) {
	tests := []struct {
		name string
		errs field.ErrorList
		want string
	}{
		{
			name: "empty list",
			errs: field.ErrorList{},
			want: "",
		},
		{
			name: "single error",
			errs: field.ErrorList{
				field.Invalid(field.NewPath("spec", "replicas"), 0, "must be positive"),
			},
			want: `spec.replicas: Invalid value: 0: must be positive`,
		},
		{
			name: "multiple errors joined with semicolon",
			errs: field.ErrorList{
				field.NotFound(field.NewPath("metadata", "name"), "test"),
				field.Required(field.NewPath("spec"), "field required"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatFieldErrors(tt.errs)
			if tt.want != "" && got != tt.want {
				t.Errorf("formatFieldErrors() = %q, want %q", got, tt.want)
			}
			// For multiple errors, just verify the separator is present.
			if tt.name == "multiple errors joined with semicolon" {
				if got == "" {
					t.Error("formatFieldErrors() returned empty for multiple errors")
				}
				if len(tt.errs) > 1 && !containsSemicolon(got) {
					t.Errorf("formatFieldErrors() = %q, expected semicolon separator", got)
				}
			}
		})
	}
}

func containsSemicolon(s string) bool {
	for _, c := range s {
		if c == ';' {
			return true
		}
	}
	return false
}
