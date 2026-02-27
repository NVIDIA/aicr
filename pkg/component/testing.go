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

package component

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// BundlerInterface defines the interface that bundlers must implement for testing.
type BundlerInterface interface {
	Make(ctx context.Context, input recipe.RecipeInput, outputDir string) (*result.Result, error)
}

// TestHarness provides common testing utilities for bundlers.
type TestHarness struct {
	t             *testing.T
	bundlerName   string
	expectedFiles []string
	recipeBuilder func() *RecipeBuilder
}

// NewTestHarness creates a new test harness for a bundler.
func NewTestHarness(t *testing.T, bundlerName string) *TestHarness {
	return &TestHarness{
		t:             t,
		bundlerName:   bundlerName,
		expectedFiles: []string{},
	}
}

// WithExpectedFiles sets the list of files expected to be generated.
func (h *TestHarness) WithExpectedFiles(files []string) *TestHarness {
	h.expectedFiles = files
	return h
}

// WithRecipeBuilder sets a custom recipe builder function.
func (h *TestHarness) WithRecipeBuilder(builder func() *RecipeBuilder) *TestHarness {
	h.recipeBuilder = builder
	return h
}

// TestMake tests the Make method of a bundler with standard assertions.
func (h *TestHarness) TestMake(bundler BundlerInterface) {
	ctx := context.Background()
	tmpDir := h.t.TempDir()

	rec := h.getRecipe()
	result, err := bundler.Make(ctx, rec, tmpDir)
	if err != nil {
		h.t.Fatalf("Make() error = %v", err)
	}

	h.AssertResult(result, tmpDir)
}

// TestMakeWithConfig tests the Make method with a specific config.
func (h *TestHarness) TestMakeWithConfig(bundler BundlerInterface, _ *config.Config) {
	ctx := context.Background()
	tmpDir := h.t.TempDir()

	rec := h.getRecipe()
	result, err := bundler.Make(ctx, rec, tmpDir)
	if err != nil {
		h.t.Fatalf("Make() error = %v", err)
	}

	h.AssertResult(result, tmpDir)
}

// AssertResult performs standard assertions on a bundler result.
func (h *TestHarness) AssertResult(result *result.Result, outputDir string) {
	if result == nil {
		h.t.Fatal("Make() returned nil result")
		return
	}

	if !result.Success {
		h.t.Error("Make() should succeed")
	}

	if len(result.Files) == 0 {
		h.t.Error("Make() produced no files")
	}

	// Verify bundle directory structure
	bundleDir := filepath.Join(outputDir, h.bundlerName)
	if _, err := os.Stat(bundleDir); os.IsNotExist(err) {
		h.t.Errorf("Make() did not create %s directory", h.bundlerName)
	}

	// Verify expected files exist
	for _, file := range h.expectedFiles {
		path := filepath.Join(bundleDir, file)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			h.t.Errorf("Expected file %s not found", file)
		}
	}
}

// AssertFileExists checks if a file exists in the bundle directory.
func (h *TestHarness) AssertFileExists(outputDir, filename string) {
	bundleDir := filepath.Join(outputDir, h.bundlerName)
	path := filepath.Join(bundleDir, filename)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		h.t.Errorf("Expected file %s not found", filename)
	}
}

// getRecipe returns a test recipe, using custom builder if set.
func (h *TestHarness) getRecipe() *recipe.Recipe {
	if h.recipeBuilder != nil {
		return h.recipeBuilder().Build()
	}
	return h.createDefaultRecipe()
}

// createDefaultRecipe creates a basic recipe for testing.
func (h *TestHarness) createDefaultRecipe() *recipe.Recipe {
	r := &recipe.Recipe{
		Measurements: []*measurement.Measurement{
			{
				Type: measurement.TypeK8s,
				Subtypes: []measurement.Subtype{
					{
						Name: "config",
						Data: map[string]measurement.Reading{
							"version": measurement.Str("1.28.0"),
						},
					},
				},
			},
		},
	}
	r.Init(header.KindRecipe, recipe.RecipeAPIVersion, "test")
	return r
}

// TestTemplateGetter tests a template getter function.
func TestTemplateGetter(t *testing.T, getTemplate func(string) (string, bool), expectedTemplates []string) {
	for _, name := range expectedTemplates {
		t.Run(name, func(t *testing.T) {
			tmpl, ok := getTemplate(name)
			if !ok {
				t.Errorf("GetTemplate(%s) not found", name)
			}
			if tmpl == "" {
				t.Errorf("GetTemplate(%s) returned empty template", name)
			}
		})
	}

	// Test non-existent template
	t.Run("nonexistent", func(t *testing.T) {
		_, ok := getTemplate("nonexistent")
		if ok {
			t.Error("GetTemplate() should return false for non-existent template")
		}
	})
}

// RecipeBuilder helps build test recipes with fluent API.
type RecipeBuilder struct {
	measurements []*measurement.Measurement
}

// NewRecipeBuilder creates a new recipe builder.
func NewRecipeBuilder() *RecipeBuilder {
	return &RecipeBuilder{
		measurements: []*measurement.Measurement{},
	}
}

// WithK8sMeasurement adds a K8s measurement with the given subtypes.
func (rb *RecipeBuilder) WithK8sMeasurement(subtypes ...measurement.Subtype) *RecipeBuilder {
	rb.measurements = append(rb.measurements, &measurement.Measurement{
		Type:     measurement.TypeK8s,
		Subtypes: subtypes,
	})
	return rb
}

// WithGPUMeasurement adds a GPU measurement with the given subtypes.
func (rb *RecipeBuilder) WithGPUMeasurement(subtypes ...measurement.Subtype) *RecipeBuilder {
	rb.measurements = append(rb.measurements, &measurement.Measurement{
		Type:     measurement.TypeGPU,
		Subtypes: subtypes,
	})
	return rb
}

// WithOSMeasurement adds an OS measurement with the given subtypes.
func (rb *RecipeBuilder) WithOSMeasurement(subtypes ...measurement.Subtype) *RecipeBuilder {
	rb.measurements = append(rb.measurements, &measurement.Measurement{
		Type:     measurement.TypeOS,
		Subtypes: subtypes,
	})
	return rb
}

// WithSystemDMeasurement adds a SystemD measurement with the given subtypes.
func (rb *RecipeBuilder) WithSystemDMeasurement(subtypes ...measurement.Subtype) *RecipeBuilder {
	rb.measurements = append(rb.measurements, &measurement.Measurement{
		Type:     measurement.TypeSystemD,
		Subtypes: subtypes,
	})
	return rb
}

// Build creates the recipe.
func (rb *RecipeBuilder) Build() *recipe.Recipe {
	r := &recipe.Recipe{
		Measurements: rb.measurements,
	}
	r.Init(header.KindRecipe, recipe.RecipeAPIVersion, "test")
	return r
}

// ImageSubtype creates an image subtype with common image data.
func ImageSubtype(images map[string]string) measurement.Subtype {
	data := make(map[string]measurement.Reading)
	for k, v := range images {
		data[k] = measurement.Str(v)
	}
	return measurement.Subtype{
		Name: "image",
		Data: data,
	}
}

// RegistrySubtype creates a registry subtype with registry information.
func RegistrySubtype(registry map[string]string) measurement.Subtype {
	data := make(map[string]measurement.Reading)
	for k, v := range registry {
		data[k] = measurement.Str(v)
	}
	return measurement.Subtype{
		Name: "registry",
		Data: data,
	}
}

// ConfigSubtype creates a config subtype with common config data.
func ConfigSubtype(configs map[string]any) measurement.Subtype {
	data := make(map[string]measurement.Reading)
	for k, v := range configs {
		switch val := v.(type) {
		case string:
			data[k] = measurement.Str(val)
		case bool:
			data[k] = measurement.Bool(val)
		case int:
			data[k] = measurement.Int(val)
		case float64:
			data[k] = measurement.Float64(val)
		}
	}
	return measurement.Subtype{
		Name: "config",
		Data: data,
	}
}

// SMISubtype creates an SMI subtype for GPU measurements.
func SMISubtype(data map[string]string) measurement.Subtype {
	readings := make(map[string]measurement.Reading)
	for k, v := range data {
		readings[k] = measurement.Str(v)
	}
	return measurement.Subtype{
		Name: "smi",
		Data: readings,
	}
}

// GrubSubtype creates a grub subtype for OS measurements.
func GrubSubtype(data map[string]string) measurement.Subtype {
	readings := make(map[string]measurement.Reading)
	for k, v := range data {
		readings[k] = measurement.Str(v)
	}
	return measurement.Subtype{
		Name: "grub",
		Data: readings,
	}
}

// SysctlSubtype creates a sysctl subtype for OS measurements.
func SysctlSubtype(data map[string]string) measurement.Subtype {
	readings := make(map[string]measurement.Reading)
	for k, v := range data {
		readings[k] = measurement.Str(v)
	}
	return measurement.Subtype{
		Name: "sysctl",
		Data: readings,
	}
}

// ServiceSubtype creates a service subtype for SystemD measurements.
func ServiceSubtype(serviceName string, data map[string]string) measurement.Subtype {
	readings := make(map[string]measurement.Reading)
	for k, v := range data {
		readings[k] = measurement.Str(v)
	}
	return measurement.Subtype{
		Name: serviceName,
		Data: readings,
	}
}

// TestValidateRecipe is a reusable test for recipe validation.
func TestValidateRecipe(t *testing.T, validateFunc func(*recipe.Recipe) error) {
	tests := []struct {
		name    string
		recipe  *recipe.Recipe
		wantErr bool
	}{
		{
			name:    "nil recipe",
			recipe:  nil,
			wantErr: true,
		},
		{
			name: "empty measurements",
			recipe: &recipe.Recipe{
				Measurements: []*measurement.Measurement{},
			},
			wantErr: true,
		},
		{
			name: "valid recipe",
			recipe: NewRecipeBuilder().
				WithK8sMeasurement(ConfigSubtype(map[string]any{
					"version": "1.28.0",
				})).
				Build(),
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateFunc(tt.recipe)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateRecipe() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// AssertConfigValue checks if a config value matches the expected value.
func AssertConfigValue(t *testing.T, config map[string]string, key, expected string) {
	t.Helper()
	if val, ok := config[key]; !ok {
		t.Errorf("Config missing key %s", key)
	} else if val != expected {
		t.Errorf("Config[%s] = %s, want %s", key, val, expected)
	}
}

// RecipeResultBuilder helps build test RecipeResult objects with fluent API.
type RecipeResultBuilder struct {
	componentRefs []recipe.ComponentRef
	criteria      *recipe.Criteria
}

// NewRecipeResultBuilder creates a new RecipeResult builder.
func NewRecipeResultBuilder() *RecipeResultBuilder {
	return &RecipeResultBuilder{
		componentRefs: []recipe.ComponentRef{},
		criteria:      &recipe.Criteria{},
	}
}

// WithComponent adds a component reference with inline overrides.
func (rb *RecipeResultBuilder) WithComponent(name, version string, overrides map[string]any) *RecipeResultBuilder {
	rb.componentRefs = append(rb.componentRefs, recipe.ComponentRef{
		Name:      name,
		Version:   version,
		Overrides: overrides,
	})
	return rb
}

// WithComponentAndSource adds a component reference with source and inline overrides.
func (rb *RecipeResultBuilder) WithComponentAndSource(name, version, source string, overrides map[string]any) *RecipeResultBuilder {
	rb.componentRefs = append(rb.componentRefs, recipe.ComponentRef{
		Name:      name,
		Version:   version,
		Source:    source,
		Overrides: overrides,
	})
	return rb
}

// Build creates the RecipeResult.
func (rb *RecipeResultBuilder) Build() *recipe.RecipeResult {
	return &recipe.RecipeResult{
		Kind:          "RecipeResult",
		APIVersion:    recipe.RecipeAPIVersion,
		ComponentRefs: rb.componentRefs,
		Criteria:      rb.criteria,
	}
}

// TestMakeWithRecipeResult tests the Make method using RecipeResult with inline overrides.
func (h *TestHarness) TestMakeWithRecipeResult(bundler BundlerInterface, recipeResult *recipe.RecipeResult) {
	ctx := context.Background()
	tmpDir := h.t.TempDir()

	result, err := bundler.Make(ctx, recipeResult, tmpDir)
	if err != nil {
		h.t.Fatalf("Make() error = %v", err)
	}

	h.AssertResult(result, tmpDir)
}

// BundlerFactory is a function that creates a new bundler instance.
type BundlerFactory func(*config.Config) BundlerInterface

// StandardBundlerTestConfig holds configuration for running standard bundler tests.
type StandardBundlerTestConfig struct {
	// ComponentName is the name of the component (e.g., "cert-manager").
	ComponentName string

	// NewBundler creates a new bundler instance.
	NewBundler BundlerFactory

	// GetTemplate is the template getter function.
	GetTemplate TemplateFunc

	// ExpectedTemplates lists templates that should be available (e.g., ["README.md"]).
	ExpectedTemplates []string

	// ExpectedFiles lists files that should be generated (e.g., ["values.yaml", "README.md"]).
	ExpectedFiles []string

	// DefaultOverrides are the default values to use in test recipes.
	DefaultOverrides map[string]any
}

// RunStandardBundlerTests runs all standard tests for a bundler.
// This includes TestNewBundler, TestBundler_Make, and TestGetTemplate.
// Use this to reduce test boilerplate in bundler packages.
//
// Example usage:
//
//	func TestBundler(t *testing.T) {
//	    internal.RunStandardBundlerTests(t, internal.StandardBundlerTestConfig{
//	        ComponentName:     "cert-manager",
//	        NewBundler:        func(cfg *config.Config) internal.BundlerInterface { return NewBundler(cfg) },
//	        GetTemplate:       GetTemplate,
//	        ExpectedTemplates: []string{"README.md"},
//	        ExpectedFiles:     []string{"values.yaml", "README.md", "checksums.txt"},
//	    })
//	}
func RunStandardBundlerTests(t *testing.T, cfg StandardBundlerTestConfig) {
	t.Run("NewBundler", func(t *testing.T) {
		runNewBundlerTests(t, cfg)
	})

	t.Run("Make", func(t *testing.T) {
		runMakeTests(t, cfg)
	})

	if cfg.GetTemplate != nil && len(cfg.ExpectedTemplates) > 0 {
		t.Run("GetTemplate", func(t *testing.T) {
			TestTemplateGetter(t, cfg.GetTemplate, cfg.ExpectedTemplates)
		})
	}
}

// runNewBundlerTests tests bundler creation.
func runNewBundlerTests(t *testing.T, cfg StandardBundlerTestConfig) {
	tests := []struct {
		name string
		cfg  *config.Config
	}{
		{
			name: "with nil config",
			cfg:  nil,
		},
		{
			name: "with valid config",
			cfg:  config.NewConfig(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := cfg.NewBundler(tt.cfg)
			if b == nil {
				t.Fatal("NewBundler() returned nil")
			}
		})
	}
}

// runMakeTests tests the Make method with various inputs.
func runMakeTests(t *testing.T, cfg StandardBundlerTestConfig) {
	tests := []struct {
		name       string
		recipe     *recipe.RecipeResult
		wantErr    bool
		verifyFunc func(t *testing.T, outputDir string)
	}{
		{
			name:    "valid recipe with component",
			recipe:  createStandardTestRecipeResult(cfg.ComponentName, cfg.DefaultOverrides),
			wantErr: false,
			verifyFunc: func(t *testing.T, outputDir string) {
				bundleDir := filepath.Join(outputDir, cfg.ComponentName)
				for _, file := range cfg.ExpectedFiles {
					path := filepath.Join(bundleDir, file)
					if _, err := os.Stat(path); os.IsNotExist(err) {
						t.Errorf("Expected file %s not found", file)
					}
				}
			},
		},
		{
			name:    "missing component",
			recipe:  createRecipeResultWithoutComponent(cfg.ComponentName),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			b := cfg.NewBundler(nil)
			ctx := context.Background()

			result, err := b.Make(ctx, tt.recipe, tmpDir)

			if (err != nil) != tt.wantErr {
				t.Errorf("Make() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if result == nil {
					t.Error("Make() returned nil result")
					return
				}
				if len(result.Files) == 0 {
					t.Error("Make() returned no files")
				}

				if tt.verifyFunc != nil {
					tt.verifyFunc(t, tmpDir)
				}
			}
		})
	}
}

// createStandardTestRecipeResult creates a test RecipeResult with the given component.
func createStandardTestRecipeResult(componentName string, overrides map[string]any) *recipe.RecipeResult {
	if overrides == nil {
		overrides = map[string]any{
			"operator": map[string]any{
				"version": "v25.3.4",
			},
		}
	}

	return &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: recipe.RecipeAPIVersion,
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:      componentName,
				Type:      "Helm",
				Source:    "https://helm.ngc.nvidia.com/nvidia",
				Version:   "v25.3.4",
				Overrides: overrides,
			},
		},
	}
}

// createRecipeResultWithoutComponent creates a RecipeResult without the specified component.
func createRecipeResultWithoutComponent(componentName string) *recipe.RecipeResult {
	// Use a different component name to simulate missing component
	otherName := "other-component"
	if componentName == otherName {
		otherName = "different-component"
	}

	return &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: recipe.RecipeAPIVersion,
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    otherName,
				Type:    "Helm",
				Version: "v1.0.0",
			},
		},
	}
}
