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

package oci

import (
	"testing"
)

func TestParseOutputTarget(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		wantIsOCI bool
		wantReg   string
		wantRepo  string
		wantTag   string
		wantDir   string
		wantErr   bool
	}{
		{
			name:      "local directory relative",
			input:     "./bundle-out",
			wantIsOCI: false,
			wantDir:   "./bundle-out",
		},
		{
			name:      "local directory absolute",
			input:     "/tmp/bundles",
			wantIsOCI: false,
			wantDir:   "/tmp/bundles",
		},
		{
			name:      "local directory current",
			input:     ".",
			wantIsOCI: false,
			wantDir:   ".",
		},
		{
			name:      "OCI with tag",
			input:     "oci://ghcr.io/nvidia/bundle:v1.0.0",
			wantIsOCI: true,
			wantReg:   "ghcr.io",
			wantRepo:  "nvidia/bundle",
			wantTag:   "v1.0.0",
		},
		{
			name:      "OCI without tag returns empty (caller applies default)",
			input:     "oci://ghcr.io/nvidia/bundle",
			wantIsOCI: true,
			wantReg:   "ghcr.io",
			wantRepo:  "nvidia/bundle",
			wantTag:   "",
		},
		{
			name:      "OCI with port and tag",
			input:     "oci://localhost:5000/test/bundle:v1",
			wantIsOCI: true,
			wantReg:   "localhost:5000",
			wantRepo:  "test/bundle",
			wantTag:   "v1",
		},
		{
			name:      "OCI with port no tag returns empty (caller applies default)",
			input:     "oci://localhost:5000/test/bundle",
			wantIsOCI: true,
			wantReg:   "localhost:5000",
			wantRepo:  "test/bundle",
			wantTag:   "",
		},
		{
			name:      "OCI deeply nested repository",
			input:     "oci://ghcr.io/org/team/project/bundle:latest",
			wantIsOCI: true,
			wantReg:   "ghcr.io",
			wantRepo:  "org/team/project/bundle",
			wantTag:   "latest",
		},
		{
			name:    "OCI invalid reference",
			input:   "oci://",
			wantErr: true,
		},
		{
			name:    "OCI invalid characters",
			input:   "oci://ghcr.io/INVALID/Bundle:v1",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ref, err := ParseOutputTarget(tt.input)

			if (err != nil) != tt.wantErr {
				t.Errorf("ParseOutputTarget() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				return
			}

			if ref.IsOCI != tt.wantIsOCI {
				t.Errorf("ParseOutputTarget() IsOCI = %v, want %v", ref.IsOCI, tt.wantIsOCI)
			}
			if ref.Registry != tt.wantReg {
				t.Errorf("ParseOutputTarget() Registry = %v, want %v", ref.Registry, tt.wantReg)
			}
			if ref.Repository != tt.wantRepo {
				t.Errorf("ParseOutputTarget() Repository = %v, want %v", ref.Repository, tt.wantRepo)
			}
			if ref.Tag != tt.wantTag {
				t.Errorf("ParseOutputTarget() Tag = %v, want %v", ref.Tag, tt.wantTag)
			}
			if ref.LocalPath != tt.wantDir {
				t.Errorf("ParseOutputTarget() LocalPath = %v, want %v", ref.LocalPath, tt.wantDir)
			}
		})
	}
}

func TestReference_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ref  *Reference
		want string
	}{
		{
			name: "local path",
			ref: &Reference{
				IsOCI:     false,
				LocalPath: "./bundle",
			},
			want: "./bundle",
		},
		{
			name: "OCI with tag",
			ref: &Reference{
				IsOCI:      true,
				Registry:   "ghcr.io",
				Repository: "nvidia/bundle",
				Tag:        "v1.0.0",
			},
			want: "oci://ghcr.io/nvidia/bundle:v1.0.0",
		},
		{
			name: "OCI without tag",
			ref: &Reference{
				IsOCI:      true,
				Registry:   "ghcr.io",
				Repository: "nvidia/bundle",
				Tag:        "",
			},
			want: "oci://ghcr.io/nvidia/bundle",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := tt.ref.String(); got != tt.want {
				t.Errorf("Reference.String() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReference_WithTag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ref     *Reference
		newTag  string
		wantTag string
	}{
		{
			name: "local path unchanged",
			ref: &Reference{
				IsOCI:     false,
				LocalPath: "./bundle",
			},
			newTag:  "v2.0.0",
			wantTag: "",
		},
		{
			name: "OCI reference gets new tag",
			ref: &Reference{
				IsOCI:      true,
				Registry:   "ghcr.io",
				Repository: "nvidia/bundle",
				Tag:        "v1.0.0",
			},
			newTag:  "v2.0.0",
			wantTag: "v2.0.0",
		},
		{
			name: "OCI reference without tag gets tag",
			ref: &Reference{
				IsOCI:      true,
				Registry:   "ghcr.io",
				Repository: "nvidia/bundle",
				Tag:        "",
			},
			newTag:  "v1.0.0",
			wantTag: "v1.0.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			result := tt.ref.WithTag(tt.newTag)
			if result.Tag != tt.wantTag {
				t.Errorf("Reference.WithTag() Tag = %v, want %v", result.Tag, tt.wantTag)
			}
			// Ensure original is not modified for OCI refs
			if tt.ref.IsOCI && result != tt.ref && tt.ref.Tag == tt.wantTag {
				t.Error("Reference.WithTag() modified original reference")
			}
		})
	}
}
