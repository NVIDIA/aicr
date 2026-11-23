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
	"slices"
	"strings"
	"testing"

	"aicr.run/pkg/config"
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
		})
	}
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
