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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadFromFile(t *testing.T) {
	tests := []struct {
		name        string
		yamlContent string
		filePath    string // override file path (skip writing yamlContent)
		wantErr     bool
		errContain  string
		checkResult func(t *testing.T, rec *RecipeResult)
	}{
		{
			name:       "nonexistent file returns error",
			filePath:   "/tmp/does-not-exist-aicr-test.yaml",
			wantErr:    true,
			errContain: "failed to load recipe",
		},
		{
			name:        "RecipeResult loads directly",
			yamlContent: "kind: RecipeResult\napiVersion: aicr.nvidia.com/v1alpha1\ncriteria:\n  service: eks\n",
			wantErr:     false,
			checkResult: func(t *testing.T, rec *RecipeResult) {
				t.Helper()
				if rec.Kind != RecipeResultKind {
					t.Errorf("kind = %q, want %q", rec.Kind, RecipeResultKind)
				}
			},
		},
		{
			name:        "RecipeMetadata with criteria auto-hydrates",
			yamlContent: "kind: RecipeMetadata\napiVersion: aicr.nvidia.com/v1alpha1\nmetadata:\n  name: test\nspec:\n  criteria:\n    service: eks\n    accelerator: h100\n    intent: training\n",
			wantErr:     false,
			checkResult: func(t *testing.T, rec *RecipeResult) {
				t.Helper()
				if rec.Kind != RecipeResultKind {
					t.Errorf("kind = %q, want %q", rec.Kind, RecipeResultKind)
				}
				if len(rec.ComponentRefs) == 0 {
					t.Error("expected hydrated recipe with components")
				}
			},
		},
		{
			name:        "RecipeMetadata without criteria errors",
			yamlContent: "kind: RecipeMetadata\napiVersion: aicr.nvidia.com/v1alpha1\nmetadata:\n  name: test\nspec: {}\n",
			wantErr:     true,
			errContain:  "has no criteria",
		},
		{
			name:        "RecipeMixin kind rejected",
			yamlContent: "kind: RecipeMixin\napiVersion: aicr.nvidia.com/v1alpha1\nmetadata:\n  name: test\nspec: {}\n",
			wantErr:     true,
			errContain:  `kind "RecipeMixin"`,
		},
		{
			name:        "unknown kind rejected",
			yamlContent: "kind: SomethingElse\napiVersion: aicr.nvidia.com/v1alpha1\n",
			wantErr:     true,
			errContain:  `kind "SomethingElse"`,
		},
		{
			name:        "empty kind allowed",
			yamlContent: "apiVersion: aicr.nvidia.com/v1alpha1\ncriteria:\n  service: eks\n",
			wantErr:     false,
			checkResult: func(t *testing.T, rec *RecipeResult) {
				t.Helper()
				if rec.Kind != "" {
					t.Errorf("kind = %q, want empty", rec.Kind)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recipeFile := tt.filePath
			if recipeFile == "" {
				dir := t.TempDir()
				recipeFile = filepath.Join(dir, "recipe.yaml")
				if err := os.WriteFile(recipeFile, []byte(tt.yamlContent), 0o600); err != nil {
					t.Fatalf("failed to write test recipe file: %v", err)
				}
			}

			rec, err := LoadFromFile(t.Context(), recipeFile, "", "test")

			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.errContain != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContain) {
					t.Errorf("error = %v, want error containing %q", err, tt.errContain)
				}
			}
			if tt.checkResult != nil && err == nil {
				tt.checkResult(t, rec)
			}
		})
	}
}
