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

package verifier

import (
	"context"
	"strings"
	"testing"
)

func TestMaterializeBundle_DirAcceptsParentOrSummary(t *testing.T) {
	bundleDir := buildTestBundle(t)

	mat, err := MaterializeBundle(context.Background(),
		VerifyOptions{Input: bundleDir}, InputFormDir, nil)
	if err != nil {
		t.Fatalf("MaterializeBundle(parent): %v", err)
	}
	mat.Cleanup()

	mat2, err := MaterializeBundle(context.Background(),
		VerifyOptions{Input: summaryDirOf(t, bundleDir)}, InputFormDir, nil)
	if err != nil {
		t.Fatalf("MaterializeBundle(summary): %v", err)
	}
	mat2.Cleanup()
}

func TestMaterializeBundle_DirRejectsNonBundle(t *testing.T) {
	_, err := MaterializeBundle(context.Background(),
		VerifyOptions{Input: t.TempDir()}, InputFormDir, nil)
	if err == nil {
		t.Errorf("expected error for empty directory")
	}
}

func TestFormatOCIReference(t *testing.T) {
	tests := []struct {
		name              string
		registry, repo, t string
		want              string
	}{
		{"tag", "ghcr.io", "owner/repo", "v1", "ghcr.io/owner/repo:v1"},
		{"digest", "ghcr.io", "owner/repo", "sha256:" + strings.Repeat("a", 64),
			"ghcr.io/owner/repo@sha256:" + strings.Repeat("a", 64)},
		{"localhost tag", "localhost:5000", "repo", "latest", "localhost:5000/repo:latest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatOCIReference(tt.registry, tt.repo, tt.t)
			if got != tt.want {
				t.Errorf("formatOCIReference = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseOCIReference(t *testing.T) {
	tests := []struct {
		name       string
		in         string
		wantReg    string
		wantRepo   string
		wantTarget string
		wantErr    bool
	}{
		{"with tag", "ghcr.io/owner/aicr-evidence:v1", "ghcr.io", "owner/aicr-evidence", "v1", false},
		{"with digest", "ghcr.io/owner/aicr-evidence@sha256:" + strings.Repeat("a", 64),
			"ghcr.io", "owner/aicr-evidence", "sha256:" + strings.Repeat("a", 64), false},
		{"oci scheme prefix", "oci://ghcr.io/owner/aicr-evidence:v1",
			"ghcr.io", "owner/aicr-evidence", "v1", false},
		{"missing target", "ghcr.io/owner/aicr-evidence", "", "", "", true},
		{"invalid", "::not-a-ref", "", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, repo, target, err := parseOCIReference(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if reg != tt.wantReg || repo != tt.wantRepo || target != tt.wantTarget {
				t.Errorf("got (%q, %q, %q), want (%q, %q, %q)",
					reg, repo, target, tt.wantReg, tt.wantRepo, tt.wantTarget)
			}
		})
	}
}
