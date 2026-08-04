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

package recipe

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

func TestConfiguredRecipeResultVersionAndStrictDecode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		wantErr string
	}{
		{
			name: "v1alpha3 requires configuration",
			wantErr: "requires metadata.selectedProfile or " +
				"configuration.slurm.accounting",
			body: `apiVersion: aicr.run/v1alpha3
kind: RecipeResult
criteria:
  platform: slurm
componentRefs: []
deploymentOrder: []
`,
		},
		{
			name: "v1alpha2 rejects accounting configuration",
			wantErr: "configuration.slurm.accounting requires " +
				"apiVersion",
			body: `apiVersion: aicr.run/v1alpha2
kind: RecipeResult
criteria:
  platform: slurm
configuration:
  slurm:
    accounting:
      mode: disabled
componentRefs: []
deploymentOrder: []
`,
		},
		{
			name:    "v1alpha3 rejects unknown fields",
			wantErr: "unknownField",
			body: `apiVersion: aicr.run/v1alpha3
kind: RecipeResult
criteria:
  platform: slurm
configuration:
  slurm:
    accounting:
      mode: disabled
unknownField: true
componentRefs: []
deploymentOrder: []
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "recipe.yaml")
			if err := os.WriteFile(path, []byte(tt.body), 0600); err != nil {
				t.Fatalf("write recipe: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), defaults.FileReadTimeout)
			defer cancel()
			_, err := LoadFromFileWithProvider(
				ctx, path, "", "test", nil)
			if err == nil {
				t.Fatal("LoadFromFileWithProvider() error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("LoadFromFileWithProvider() error = %v, want containing %q",
					err, tt.wantErr)
			}
		})
	}
}

func TestLoadConfiguredRecipeRejectsDisabledAccountingComponent(t *testing.T) {
	t.Parallel()

	result, err := NewBuilder().BuildFromCriteria(t.Context(), &Criteria{
		Service:     CriteriaServiceEKS,
		Accelerator: CriteriaAcceleratorH100,
		Intent:      CriteriaIntentTraining,
		OS:          CriteriaOSUbuntu,
		Platform:    CriteriaPlatformSlurm,
	}, WithAccountingMode(AccountingModeAICRProvided))
	if err != nil {
		t.Fatalf("BuildFromCriteria() error = %v", err)
	}
	result.GetComponentRef(mariaDBOperatorComponentName).
		Overrides[componentEnabledOverrideKey] = false

	data, err := serializer.MarshalYAMLDeterministic(result)
	if err != nil {
		t.Fatalf("MarshalYAMLDeterministic() error = %v", err)
	}
	path := filepath.Join(t.TempDir(), "recipe.yaml")
	if writeErr := os.WriteFile(path, data, 0600); writeErr != nil {
		t.Fatalf("write recipe: %v", writeErr)
	}

	ctx, cancel := context.WithTimeout(context.Background(), defaults.FileReadTimeout)
	defer cancel()
	_, err = LoadFromFileWithProvider(ctx, path, "", "test", nil)
	if err == nil {
		t.Fatal("LoadFromFileWithProvider() error = nil, want disabled component rejection")
	}
	for _, detail := range []string{mariaDBOperatorComponentName, "must be enabled", string(AccountingModeAICRProvided)} {
		if !strings.Contains(err.Error(), detail) {
			t.Fatalf("LoadFromFileWithProvider() error = %v, want containing %q", err, detail)
		}
	}
}
