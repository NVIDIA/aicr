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
	"testing"
)

func TestLoadAndValidatePointer_HappyPath(t *testing.T) {
	body := `schemaVersion: 1.0.0
recipe: h100-eks-ubuntu-training
attestations:
- bundle:
    oci: ghcr.io/owner/aicr-evidence:v1
    digest: sha256:abc
    predicateType: https://aicr.nvidia.com/recipe-evidence/v1
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
attestations: [{bundle: {predicateType: https://aicr.nvidia.com/recipe-evidence/v1}, attestedAt: 2026-05-08T10:23:11Z}]
`},
		{"missing recipe", `schemaVersion: 1.0.0
attestations: [{bundle: {predicateType: https://aicr.nvidia.com/recipe-evidence/v1}, attestedAt: 2026-05-08T10:23:11Z}]
`},
		{"no attestations", `schemaVersion: 1.0.0
recipe: x
attestations: []
`},
		{"multiple attestations", `schemaVersion: 1.0.0
recipe: x
attestations:
- {bundle: {predicateType: https://aicr.nvidia.com/recipe-evidence/v1}, attestedAt: 2026-05-08T10:23:11Z}
- {bundle: {predicateType: https://aicr.nvidia.com/recipe-evidence/v1}, attestedAt: 2026-05-08T10:23:11Z}
`},
		{"wrong predicate type", `schemaVersion: 1.0.0
recipe: x
attestations: [{bundle: {predicateType: wrong}, attestedAt: 2026-05-08T10:23:11Z}]
`},
		{"bad digest format", `schemaVersion: 1.0.0
recipe: x
attestations:
- bundle: {oci: ghcr.io/x/y:v1, digest: no-prefix, predicateType: https://aicr.nvidia.com/recipe-evidence/v1}
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
