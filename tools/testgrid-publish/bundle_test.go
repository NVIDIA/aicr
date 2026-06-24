// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES. All rights reserved.
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
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

func TestParseCriteria(t *testing.T) {
	tests := []struct {
		name           string
		recipeYAML     string
		wantService    string
		wantAccel      string
		wantOS         string
		wantIntent     string
		wantPlatform   string
		wantConstraint string
		wantErr        bool
	}{
		{
			name: "full criteria with k8s constraint",
			recipeYAML: `
criteria:
  service: eks
  accelerator: h100
  os: ubuntu
  intent: training
  platform: kubeflow
constraints:
  - name: K8s.server.version
    value: ">=1.28"
`,
			wantService:    "eks",
			wantAccel:      "h100",
			wantOS:         "ubuntu",
			wantIntent:     "training",
			wantPlatform:   "kubeflow",
			wantConstraint: ">=1.28",
		},
		{
			name: "bare intent no platform no constraint",
			recipeYAML: `
criteria:
  service: gke
  accelerator: h100
  os: cos
  intent: inference
`,
			wantService:  "gke",
			wantAccel:    "h100",
			wantOS:       "cos",
			wantIntent:   "inference",
			wantPlatform: "",
		},
		{
			name: "uppercase service and accelerator are normalized",
			recipeYAML: `
criteria:
  service: EKS
  accelerator: H100
  os: Ubuntu
  intent: Training
  platform: KubeFlow
`,
			wantService:  "eks",
			wantAccel:    "h100",
			wantOS:       "ubuntu",
			wantIntent:   "training",
			wantPlatform: "kubeflow",
		},
		{
			name: "whitespace in quoted criteria fields is trimmed",
			recipeYAML: `
criteria:
  service: " eks "
  accelerator: " h100 "
  os: ubuntu
  intent: training
`,
			wantService: "eks",
			wantAccel:   "h100",
			wantOS:      "ubuntu",
			wantIntent:  "training",
		},
		{
			name: "missing service",
			recipeYAML: `
criteria:
  accelerator: h100
  os: ubuntu
  intent: training
`,
			wantErr: true,
		},
		{
			name:       "invalid yaml",
			recipeYAML: "this: is: not: valid: yaml: : :",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			recipeFile := filepath.Join(dir, attestation.RecipeFilename)
			if err := os.WriteFile(recipeFile, []byte(tt.recipeYAML), 0o600); err != nil {
				t.Fatal(err)
			}

			got, err := parseCriteria(dir)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseCriteria() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.Service != tt.wantService {
				t.Errorf("Service = %q, want %q", got.Service, tt.wantService)
			}
			if got.Accelerator != tt.wantAccel {
				t.Errorf("Accelerator = %q, want %q", got.Accelerator, tt.wantAccel)
			}
			if got.OS != tt.wantOS {
				t.Errorf("OS = %q, want %q", got.OS, tt.wantOS)
			}
			if got.Intent != tt.wantIntent {
				t.Errorf("Intent = %q, want %q", got.Intent, tt.wantIntent)
			}
			if got.Platform != tt.wantPlatform {
				t.Errorf("Platform = %q, want %q", got.Platform, tt.wantPlatform)
			}
			if got.K8sConstraint != tt.wantConstraint {
				t.Errorf("K8sConstraint = %q, want %q", got.K8sConstraint, tt.wantConstraint)
			}
		})
	}
}

func TestParseCriteriaMissingFile(t *testing.T) {
	_, err := parseCriteria(t.TempDir())
	if err == nil {
		t.Fatal("expected error for missing recipe.yaml")
	}
}

func TestParseCriteriaOversizeFile(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, defaults.MaxRecipePOSTBytes+1)
	copy(big, []byte("criteria:\n  service: eks\n"))
	if err := os.WriteFile(filepath.Join(dir, attestation.RecipeFilename), big, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := parseCriteria(dir)
	if err == nil {
		t.Fatal("expected error for oversized recipe.yaml")
	}
}

func TestLoadPredicate(t *testing.T) {
	t.Run("valid statement", func(t *testing.T) {
		dir := t.TempDir()
		ts := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
		pred := attestation.Predicate{
			AttestedAt:  ts,
			AICRVersion: "v0.12.3",
		}
		predBytes, _ := json.Marshal(pred)

		stmt := map[string]any{
			"_type":         "https://in-toto.io/Statement/v0.1",
			"predicateType": attestation.PredicateTypeV1,
			"predicate":     json.RawMessage(predBytes),
		}
		stmtBytes, _ := json.Marshal(stmt)
		if err := os.WriteFile(
			filepath.Join(dir, attestation.StatementFilename),
			stmtBytes, 0o600,
		); err != nil {
			t.Fatal(err)
		}

		got, err := loadPredicate(dir)
		if err != nil {
			t.Fatalf("loadPredicate() unexpected error: %v", err)
		}
		if got.AICRVersion != "v0.12.3" {
			t.Errorf("AICRVersion = %q, want %q", got.AICRVersion, "v0.12.3")
		}
		if !got.AttestedAt.Equal(ts) {
			t.Errorf("AttestedAt = %v, want %v", got.AttestedAt, ts)
		}
	})

	t.Run("missing statement returns error", func(t *testing.T) {
		_, err := loadPredicate(t.TempDir())
		if err == nil {
			t.Fatal("expected error for missing statement.intoto.json")
		}
	})

	t.Run("invalid json returns error", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(
			filepath.Join(dir, attestation.StatementFilename),
			[]byte("not json"), 0o600,
		); err != nil {
			t.Fatal(err)
		}
		_, err := loadPredicate(dir)
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})

	t.Run("statement with no predicate field returns error", func(t *testing.T) {
		dir := t.TempDir()
		stmt := map[string]any{
			"_type":         "https://in-toto.io/Statement/v0.1",
			"predicateType": attestation.PredicateTypeV1,
			// no "predicate" field
		}
		stmtBytes, _ := json.Marshal(stmt)
		if err := os.WriteFile(
			filepath.Join(dir, attestation.StatementFilename),
			stmtBytes, 0o600,
		); err != nil {
			t.Fatal(err)
		}
		_, err := loadPredicate(dir)
		if err == nil {
			t.Fatal("expected error when predicate field is missing")
		}
	})
}
