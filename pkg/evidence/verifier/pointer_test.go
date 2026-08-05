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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

func TestLoadAndValidatePointer_HappyPath(t *testing.T) {
	body := `schemaVersion: 1.0.0
recipe: h100-eks-ubuntu-training
attestations:
- bundle:
    oci: ghcr.io/owner/aicr-evidence:v1
    digest: sha256:abc
    predicateType: https://aicr.run/recipe-evidence/v1
  attestedAt: 2026-05-08T10:23:11Z
`
	p := filepath.Join(t.TempDir(), "pointer.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := LoadAndValidatePointer(p)
	if err != nil {
		t.Fatalf("LoadAndValidatePointer: %v", err)
	}
	if got.Recipe != "h100-eks-ubuntu-training" {
		t.Errorf("Recipe = %q", got.Recipe)
	}
}

func TestLoadAndValidatePointer_Rejects(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"unsupported schema", `schemaVersion: 2.0.0
recipe: x
attestations: [{bundle: {predicateType: https://aicr.run/recipe-evidence/v1}, attestedAt: 2026-05-08T10:23:11Z}]
`},
		{"missing recipe", `schemaVersion: 1.0.0
attestations: [{bundle: {predicateType: https://aicr.run/recipe-evidence/v1}, attestedAt: 2026-05-08T10:23:11Z}]
`},
		{"no attestations", `schemaVersion: 1.0.0
recipe: x
attestations: []
`},
		{"multiple attestations", `schemaVersion: 1.0.0
recipe: x
attestations:
- {bundle: {predicateType: https://aicr.run/recipe-evidence/v1}, attestedAt: 2026-05-08T10:23:11Z}
- {bundle: {predicateType: https://aicr.run/recipe-evidence/v1}, attestedAt: 2026-05-08T10:23:11Z}
`},
		{"wrong predicate type", `schemaVersion: 1.0.0
recipe: x
attestations: [{bundle: {predicateType: wrong}, attestedAt: 2026-05-08T10:23:11Z}]
`},
		{"bad digest format", `schemaVersion: 1.0.0
recipe: x
attestations:
- bundle: {oci: ghcr.io/x/y:v1, digest: no-prefix, predicateType: https://aicr.run/recipe-evidence/v1}
  attestedAt: 2026-05-08T10:23:11Z
`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "pointer.yaml")
			if err := os.WriteFile(p, []byte(tt.body), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			if _, err := LoadAndValidatePointer(p); err == nil {
				t.Errorf("expected error")
			}
		})
	}
}

func TestLoadAndValidatePointer_RejectsHuge(t *testing.T) {
	big := make([]byte, pointerSizeCeiling+1)
	for i := range big {
		big[i] = 'a'
	}
	p := filepath.Join(t.TempDir(), "huge.yaml")
	if err := os.WriteFile(p, big, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadAndValidatePointer(p); err == nil {
		t.Errorf("expected error for oversize pointer")
	}
}

// TestValidatePointerProfileSuffixConsistency pins the per-value evidence
// invariant: a profiled pointer whose recipe name lacks the profile path
// segment is rejected — an unsuffixed name would collapse two values into
// one evidence directory.
func TestValidatePointerProfileSuffixConsistency(t *testing.T) {
	base := func() *attestation.Pointer {
		return &attestation.Pointer{
			SchemaVersion: "1.0.0",
			Recipe:        "h100-aks-ubuntu-training-gpustack-operator-managed",
			Profile:       "gpuStack=operator-managed",
			Attestations: []attestation.PointerAttestation{{
				// Profiled pointers require v2 evidence since the ADR-015
				// descriptor-currentness cut-over.
				Bundle: attestation.PointerBundle{PredicateType: attestation.PredicateTypeV2},
			}},
		}
	}

	// Bidirectional type coherence: a profiled pointer on v1 evidence is
	// pre-cut-over output and must be re-signed; v2 evidence without a
	// profile selection is malformed.
	tests := []struct {
		name    string
		mutate  func(p *attestation.Pointer)
		wantErr string // "" means the pointer must validate
	}{
		{"suffixed profiled pointer valid",
			func(*attestation.Pointer) {}, ""},
		{"profiled pointer on v1 evidence rejected",
			func(p *attestation.Pointer) {
				p.Attestations[0].Bundle.PredicateType = attestation.PredicateTypeV1
			}, "profile-bearing recipes require"},
		{"unprofiled pointer on v2 evidence rejected",
			func(p *attestation.Pointer) {
				p.Profile = ""
				p.Recipe = "h100-aks-ubuntu-training"
			}, "without a profile selection"},
		{"unsuffixed profiled recipe name rejected",
			func(p *attestation.Pointer) {
				p.Recipe = "h100-aks-ubuntu-training"
			}, "profile path segment"},
		{"malformed profile selection rejected",
			func(p *attestation.Pointer) {
				p.Profile = "not-a-selection"
			}, "name=value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := base()
			tt.mutate(p)
			err := validatePointer(p)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validatePointer() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validatePointer() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}
