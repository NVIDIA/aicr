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

package validations

import (
	"context"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
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
			// Fail closed: unknown function names are a recipe/registry
			// contract violation (typo, renamed/removed validator) and
			// must produce an error so a severity:error check cannot be
			// silently bypassed.
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
			wantErrors:    1,
			wantMsg:       "unknown validation function",
		},
		{
			name:          "workload selector missing with message",
			componentName: "nodewright-customizations",
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
					{Name: "nodewright-customizations"},
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
			componentName: "nodewright-customizations",
			validations: []recipe.ComponentValidationConfig{
				{
					Function:   "CheckWorkloadSelectorMissing",
					Severity:   "warning",
					Conditions: map[string][]string{"intent": {"training"}},
				},
			},
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "nodewright-customizations"},
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
			componentName: "nodewright-customizations",
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
					{Name: "nodewright-customizations"},
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
		{
			name:          "info severity suppresses warnings",
			componentName: "nodewright-customizations",
			validations: []recipe.ComponentValidationConfig{
				{
					Function:   "CheckWorkloadSelectorMissing",
					Severity:   "info",
					Conditions: map[string][]string{"intent": {"training"}},
					Message:    "Consider setting --workload-selector",
				},
			},
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "nodewright-customizations"},
				},
				Criteria: &recipe.Criteria{
					Intent: recipe.CriteriaIntentTraining,
				},
			},
			bundlerConfig: config.NewConfig(),
			wantWarnings:  0,
			wantErrors:    0,
		},
		{
			name:          "info severity suppresses errors from checks",
			componentName: "nodewright-customizations",
			validations: []recipe.ComponentValidationConfig{
				{
					Function:   "CheckAcceleratedSelectorMissing",
					Severity:   "info",
					Conditions: map[string][]string{"intent": {"training", "inference"}},
					Message:    "Consider setting --accelerated-node-selector",
				},
			},
			recipeResult: &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: "nodewright-customizations"},
				},
				Criteria: &recipe.Criteria{
					Intent: recipe.CriteriaIntentTraining,
				},
			},
			bundlerConfig: config.NewConfig(),
			wantWarnings:  0,
			wantErrors:    0,
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

// TestRunComponentValidations covers the shared component preflight used by
// both DefaultBundler.Make and Client.BundleComponents: registry-declared
// validations run for every component in the recipe, severity:error results
// block (first error returned), and a nil bundler config — the SDK path,
// which has no bundle-time overrides — still arms recipe-only rules such as
// the driver-ownership Rule 1.
func TestRunComponentValidations(t *testing.T) {
	driverlessAKS := func(driverEnabled bool) *recipe.RecipeResult {
		r := &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{{
				Name: "gpu-operator",
				Overrides: map[string]any{
					"driver": map[string]any{"enabled": driverEnabled},
				},
			}},
			Criteria: &recipe.Criteria{Service: recipe.CriteriaServiceAKS},
		}
		r.Metadata.GPUDriverState = recipe.GPUDriverStateAbsent
		return r
	}

	t.Run("nil recipe result → no-op", func(t *testing.T) {
		warnings, err := RunComponentValidations(context.Background(), nil, nil)
		if err != nil || len(warnings) != 0 {
			t.Fatalf("RunComponentValidations(nil) = (%v, %v), want (nil, nil)", warnings, err)
		}
	})

	t.Run("driverless recipe + nil config → blocking error", func(t *testing.T) {
		_, err := RunComponentValidations(context.Background(), driverlessAKS(false), nil)
		if err == nil {
			t.Fatal("expected blocking error for driver.enabled=false on a driverless cluster, got nil")
		}
		if !strings.Contains(err.Error(), "driverless") {
			t.Errorf("error missing driver-ownership context: %v", err)
		}
	})

	t.Run("coherent recipe + nil config → passes", func(t *testing.T) {
		warnings, err := RunComponentValidations(context.Background(), driverlessAKS(true), nil)
		if err != nil {
			t.Fatalf("RunComponentValidations = %v, want nil", err)
		}
		for _, w := range warnings {
			t.Logf("warning: %s", w)
		}
	})

	aicrProvidedAccounting := func(state string) *recipe.RecipeResult {
		r := &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{{Name: "mariadb-operator"}},
			Configuration: &recipe.RecipeConfiguration{
				Slurm: &recipe.SlurmConfiguration{
					Accounting: &recipe.SlurmAccountingConfiguration{
						Mode: recipe.AccountingModeAICRProvided,
					},
				},
			},
		}
		r.Metadata.MariaDBOperatorState = state
		return r
	}
	tests := []struct {
		name        string
		state       string
		wantErr     string
		wantWarning string
	}{
		{
			name:    "MariaDB CR conflict blocks with customer-managed remediation",
			state:   recipe.MariaDBOperatorStateCRsDetected,
			wantErr: "customer-managed",
		},
		{
			name:        "MariaDB API-only state warns and allows",
			state:       recipe.MariaDBOperatorStateAPIDetected,
			wantWarning: "customer-managed",
		},
		{
			name:        "missing MariaDB evidence warns and allows",
			wantWarning: "current snapshot",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings, err := RunComponentValidations(
				context.Background(), aicrProvidedAccounting(tt.state), nil)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("RunComponentValidations() error = nil, want containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("RunComponentValidations() error = %v, want containing %q",
						err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("RunComponentValidations() error = %v, want nil", err)
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], tt.wantWarning) {
				t.Errorf("warnings = %v, want one advisory containing %q",
					warnings, tt.wantWarning)
			}
		})
	}

	t.Run("cancelled context → timeout error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := RunComponentValidations(ctx, driverlessAKS(true), nil)
		if err == nil {
			t.Fatal("expected context-cancellation error, got nil")
		}
	})
}
