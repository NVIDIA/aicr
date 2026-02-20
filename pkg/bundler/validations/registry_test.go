// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package validations

import (
	"context"
	"strings"
	"testing"

	"github.com/NVIDIA/eidos/pkg/bundler/config"
	"github.com/NVIDIA/eidos/pkg/recipe"
)

func TestGet(t *testing.T) {
	tests := []struct {
		name     string
		function string
		wantNil  bool
	}{
		{
			name:     "existing function",
			function: "CheckWorkloadSelectorMissing",
			wantNil:  false,
		},
		{
			name:     "another existing function",
			function: "CheckAcceleratedSelectorMissing",
			wantNil:  false,
		},
		{
			name:     "non-existent function",
			function: "NonExistentFunction",
			wantNil:  true,
		},
		{
			name:     "empty function name",
			function: "",
			wantNil:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Get(tt.function)
			if (got == nil) != tt.wantNil {
				t.Errorf("Get(%q) = %v, want nil=%v", tt.function, got, tt.wantNil)
			}
		})
	}
}

func TestRegister(t *testing.T) {
	// Test that we can register a custom validation function
	testFn := func(ctx context.Context, componentName string, recipeResult *recipe.RecipeResult, bundlerConfig *config.Config, conditions map[string][]string) ([]string, []error) {
		return []string{"test warning"}, nil
	}

	Register("TestFunction", testFn)

	// Verify it's registered
	got := Get("TestFunction")
	if got == nil {
		t.Fatal("Get() returned nil for registered function")
	}

	// Verify it works
	ctx := context.Background()
	warnings, errors := got(ctx, "test-component", &recipe.RecipeResult{}, config.NewConfig(), nil)
	if len(warnings) != 1 {
		t.Errorf("registered function returned %d warnings, want 1", len(warnings))
	}
	if len(errors) != 0 {
		t.Errorf("registered function returned %d errors, want 0", len(errors))
	}
}

func TestRunValidations(t *testing.T) {
	tests := []struct {
		name          string
		componentName string
		validations   []recipe.ComponentValidationConfig
		recipeResult  *recipe.RecipeResult
		bundlerConfig *config.Config
		wantWarnings  int
		wantErrors    int
		wantMsgInWarn bool
		wantMsg       string
	}{
		{
			name:          "no validations",
			componentName: "test-component",
			validations:   []recipe.ComponentValidationConfig{},
			recipeResult:  &recipe.RecipeResult{},
			bundlerConfig: config.NewConfig(),
			wantWarnings:  0,
			wantErrors:    0,
		},
		{
			name:          "unknown function",
			componentName: "test-component",
			validations: []recipe.ComponentValidationConfig{
				{
					Function: "UnknownFunction",
					Severity: "warning",
				},
			},
			recipeResult:  &recipe.RecipeResult{},
			bundlerConfig: config.NewConfig(),
			wantWarnings:  0,
			wantErrors:    0,
		},
		{
			name:          "workload selector missing with message",
			componentName: "skyhook-customizations",
			validations: []recipe.ComponentValidationConfig{
				{
					Function:   "CheckWorkloadSelectorMissing",
					Severity:   "warning",
					Conditions: map[string][]string{"intent": {"training"}},
					Message:    "Custom detail message",
				},
			},
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "skyhook-customizations"},
				},
				Criteria: &recipe.Criteria{
					Intent: recipe.CriteriaIntentTraining,
				},
			},
			bundlerConfig: config.NewConfig(),
			wantWarnings:  1,
			wantErrors:    0,
			wantMsgInWarn: true,
			wantMsg:       "Custom detail message",
		},
		{
			name:          "workload selector missing without message",
			componentName: "skyhook-customizations",
			validations: []recipe.ComponentValidationConfig{
				{
					Function:   "CheckWorkloadSelectorMissing",
					Severity:   "warning",
					Conditions: map[string][]string{"intent": {"training"}},
				},
			},
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "skyhook-customizations"},
				},
				Criteria: &recipe.Criteria{
					Intent: recipe.CriteriaIntentTraining,
				},
			},
			bundlerConfig: config.NewConfig(),
			wantWarnings:  1,
			wantErrors:    0,
			wantMsgInWarn: false,
		},
		{
			name:          "error severity converts warnings to errors",
			componentName: "skyhook-customizations",
			validations: []recipe.ComponentValidationConfig{
				{
					Function:   "CheckWorkloadSelectorMissing",
					Severity:   "error",
					Conditions: map[string][]string{"intent": {"training"}},
					Message:    "This is an error",
				},
			},
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "skyhook-customizations"},
				},
				Criteria: &recipe.Criteria{
					Intent: recipe.CriteriaIntentTraining,
				},
			},
			bundlerConfig: config.NewConfig(),
			wantWarnings:  0,
			wantErrors:    1,
			wantMsgInWarn: false,
			wantMsg:       "This is an error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			warnings, errors := RunValidations(ctx, tt.componentName, tt.validations, tt.recipeResult, tt.bundlerConfig)

			if len(warnings) != tt.wantWarnings {
				t.Errorf("RunValidations() warnings = %d, want %d", len(warnings), tt.wantWarnings)
			}
			if len(errors) != tt.wantErrors {
				t.Errorf("RunValidations() errors = %d, want %d", len(errors), tt.wantErrors)
			}

			if tt.wantMsgInWarn && len(warnings) > 0 {
				found := false
				for _, w := range warnings {
					if strings.Contains(w, tt.wantMsg) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("RunValidations() warnings = %v, want to contain %q", warnings, tt.wantMsg)
				}
			}

			if tt.wantErrors > 0 && len(errors) > 0 && tt.wantMsg != "" {
				found := false
				for _, e := range errors {
					if strings.Contains(e.Error(), tt.wantMsg) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("RunValidations() errors = %v, want to contain %q", errors, tt.wantMsg)
				}
			}
		})
	}
}
