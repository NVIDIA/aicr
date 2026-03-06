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

func TestExtractVersionFromImage(t *testing.T) {
	tests := []struct {
		name  string
		image string
		want  string
	}{
		{
			name:  "standard image with tag",
			image: "nvcr.io/nvidia/gpu-operator:v24.9.0",
			want:  "v24.9.0",
		},
		{
			name:  "tag without v prefix",
			image: "nvcr.io/nvidia/gpu-operator:24.9.0",
			want:  "v24.9.0",
		},
		{
			name:  "tag with suffix after dash",
			image: "nvcr.io/nvidia/gpu-operator:v24.9.0-ubi8",
			want:  "v24.9.0",
		},
		{
			name:  "tag with suffix no v prefix",
			image: "nvcr.io/nvidia/gpu-operator:24.9.0-ubi8",
			want:  "v24.9.0",
		},
		{
			name:  "no tag returns empty",
			image: "nvcr.io/nvidia/gpu-operator",
			want:  "",
		},
		{
			name:  "empty string returns empty",
			image: "",
			want:  "",
		},
		{
			name:  "multiple colons returns empty",
			image: "host:5000/nvidia/gpu-operator:v1.0.0",
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractVersionFromImage(tt.image)
			if got != tt.want {
				t.Errorf("extractVersionFromImage(%q) = %q, want %q", tt.image, got, tt.want)
			}
		})
	}
}

func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    string
	}{
		{
			name:    "already has v prefix",
			version: "v24.9.0",
			want:    "v24.9.0",
		},
		{
			name:    "no v prefix",
			version: "24.9.0",
			want:    "v24.9.0",
		},
		{
			name:    "empty string",
			version: "",
			want:    "",
		},
		{
			name:    "whitespace only",
			version: "   ",
			want:    "",
		},
		{
			name:    "whitespace around version",
			version: "  24.9.0  ",
			want:    "v24.9.0",
		},
		{
			name:    "v prefix with whitespace",
			version: "  v1.2.3  ",
			want:    "v1.2.3",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeVersion(tt.version)
			if got != tt.want {
				t.Errorf("normalizeVersion(%q) = %q, want %q", tt.version, got, tt.want)
			}
		})
	}
}
