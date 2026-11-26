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

package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"aicr.run/pkg/config"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
)

func TestValidate_RecipeCatalogs(t *testing.T) {
	tests := []struct {
		name     string
		catalogs []string
		external bool
		wantErr  string
	}{
		{name: "none"},
		{name: "nvidia and a directory", catalogs: []string{"nvidia", "./partner"}},
		{name: "empty entry", catalogs: []string{"nvidia", " "}, wantErr: "spec.recipe.catalogs[1] is empty"},
		{name: "with external-only", catalogs: []string{"nvidia"}, external: true, wantErr: "mutually exclusive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newValid()
			cfg.Spec.Recipe.Catalogs = tt.catalogs
			cfg.Spec.Recipe.ExternalOnly = tt.external
			err := cfg.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Validate() = %v, want it to contain %q", err, tt.wantErr)
			}
			if tt.wantErr != "" {
				errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
			}
		})
	}
}

// writeConfigDocument writes an AICRConfig document whose spec.recipe is the
// given YAML, indented under "recipe:".
func writeConfigDocument(t *testing.T, recipeSpec string) string {
	t.Helper()
	doc := fmt.Sprintf("apiVersion: %s\nkind: %s\nmetadata:\n  name: test\nspec:\n  recipe:\n%s",
		config.APIVersion, config.Kind, recipeSpec)
	path := filepath.Join(t.TempDir(), "aicr-config.yaml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestLoad_RecipeCatalogsFromDocument pins the wire form: spec.recipe.catalogs
// is read from a config document, not only settable on the struct.
func TestLoad_RecipeCatalogsFromDocument(t *testing.T) {
	t.Run("a list", func(t *testing.T) {
		path := writeConfigDocument(t, "    catalogs:\n      - nvidia\n      - ./partner\n")
		cfg, err := config.Load(t.Context(), path)
		if err != nil {
			t.Fatalf("Load() = %v", err)
		}
		if got, want := cfg.Spec.Recipe.CatalogList(), []string{"nvidia", "./partner"}; !slices.Equal(got, want) {
			t.Errorf("catalogs = %v, want %v", got, want)
		}
	})

	t.Run("not with externalOnly", func(t *testing.T) {
		path := writeConfigDocument(t, "    data: ./partner\n    externalOnly: true\n    catalogs:\n      - nvidia\n")
		_, err := config.Load(t.Context(), path)
		if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("Load() = %v, want the catalogs/externalOnly conflict", err)
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	})

	t.Run("an empty entry", func(t *testing.T) {
		path := writeConfigDocument(t, "    catalogs:\n      - nvidia\n      - \"\"\n")
		_, err := config.Load(t.Context(), path)
		if err == nil || !strings.Contains(err.Error(), "spec.recipe.catalogs[1] is empty") {
			t.Fatalf("Load() = %v, want the empty-entry error", err)
		}
		errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
	})
}

func TestRecipeSpecCatalogList(t *testing.T) {
	var nilSpec *config.RecipeSpec
	if got := nilSpec.CatalogList(); got != nil {
		t.Errorf("nil spec CatalogList = %v, want nil", got)
	}
	spec := &config.RecipeSpec{Catalogs: []string{"nvidia", "./partner"}}
	if got := spec.CatalogList(); !slices.Equal(got, []string{"nvidia", "./partner"}) {
		t.Errorf("CatalogList = %v", got)
	}
}
