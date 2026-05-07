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

package component

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/result"
	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Mock bundler for testing
type mockBundler struct {
	makeFunc func(ctx context.Context, input recipe.RecipeInput, outputDir string) (*result.Result, error)
}

func (m *mockBundler) Make(ctx context.Context, input recipe.RecipeInput, outputDir string) (*result.Result, error) {
	if m.makeFunc != nil {
		return m.makeFunc(ctx, input, outputDir)
	}
	res := result.New("mock")
	res.AddFile(filepath.Join(outputDir, "mock", "test.txt"), 100)
	res.Success = true
	return res, nil
}

func TestTestHarness_NewTestHarness(t *testing.T) {
	h := NewTestHarness(t, "test-bundler")
	if h == nil {
		t.Fatal("NewTestHarness() returned nil")
	}
	if h.bundlerName != "test-bundler" {
		t.Errorf("bundlerName = %s, want test-bundler", h.bundlerName)
	}
	if h.expectedFiles == nil {
		t.Error("expectedFiles should be initialized")
	}
}

func TestTestHarness_WithExpectedFiles(t *testing.T) {
	files := []string{"file1.txt", "file2.yaml"}
	h := NewTestHarness(t, "test").WithExpectedFiles(files)

	if len(h.expectedFiles) != 2 {
		t.Errorf("expectedFiles length = %d, want 2", len(h.expectedFiles))
	}
}

func TestTestHarness_WithRecipeBuilder(t *testing.T) {
	h := NewTestHarness(t, "test").WithRecipeBuilder(NewRecipeBuilder)

	if h.recipeBuilder == nil {
		t.Error("recipeBuilder should be set")
	}
}

func TestTestHarness_TestMake(t *testing.T) {
	tmpDir := t.TempDir()

	// Create mock bundler that creates expected files
	mock := &mockBundler{
		makeFunc: func(ctx context.Context, input recipe.RecipeInput, outputDir string) (*result.Result, error) {
			bundleDir := filepath.Join(outputDir, "test-bundler")
			os.MkdirAll(bundleDir, 0755)
			os.WriteFile(filepath.Join(bundleDir, "test.txt"), []byte("test"), 0644)

			res := result.New("test-bundler")
			res.AddFile(filepath.Join(bundleDir, "test.txt"), 4)
			res.Success = true
			return res, nil
		},
	}

	h := NewTestHarness(t, "test-bundler").
		WithExpectedFiles([]string{"test.txt"}).
		WithRecipeBuilder(func() *RecipeBuilder {
			return NewRecipeBuilder().WithK8sMeasurement(
				ConfigSubtype(map[string]any{"version": "1.28.0"}),
			)
		})

	// Create a custom testMake function for testing
	testMakeFunc := func(bundler BundlerInterface) {
		ctx := context.Background()
		rec := h.getRecipe()
		result, err := bundler.Make(ctx, rec, tmpDir)
		if err != nil {
			t.Fatalf("Make() error = %v", err)
		}
		h.AssertResult(result, tmpDir)
	}

	testMakeFunc(mock)
}

func TestTestHarness_AssertResult(t *testing.T) {
	tmpDir := t.TempDir()
	bundleDir := filepath.Join(tmpDir, "test-bundler")
	os.MkdirAll(bundleDir, 0755)
	os.WriteFile(filepath.Join(bundleDir, "test.txt"), []byte("test"), 0644)

	res := result.New("test-bundler")
	res.AddFile(filepath.Join(bundleDir, "test.txt"), 4)
	res.Success = true

	h := NewTestHarness(t, "test-bundler").
		WithExpectedFiles([]string{"test.txt"})

	// Should not panic
	h.AssertResult(res, tmpDir)
}

func TestTestHarness_AssertFileExists(t *testing.T) {
	tmpDir := t.TempDir()
	bundleDir := filepath.Join(tmpDir, "test-bundler")
	os.MkdirAll(bundleDir, 0755)
	os.WriteFile(filepath.Join(bundleDir, "test.txt"), []byte("test"), 0644)

	h := NewTestHarness(t, "test-bundler")

	// Should not panic for existing file
	h.AssertFileExists(tmpDir, "test.txt")
}

func TestTestHarness_getRecipe(t *testing.T) {
	t.Run("with custom builder", func(t *testing.T) {
		h := NewTestHarness(t, "test").
			WithRecipeBuilder(func() *RecipeBuilder {
				return NewRecipeBuilder().WithK8sMeasurement(
					ConfigSubtype(map[string]any{"custom": "value"}),
				)
			})

		rec := h.getRecipe()
		if rec == nil {
			t.Fatal("getRecipe() returned nil")
		}
		if len(rec.Measurements) == 0 {
			t.Error("Recipe should have measurements")
		}
	})

	t.Run("with default recipe", func(t *testing.T) {
		h := NewTestHarness(t, "test")
		rec := h.getRecipe()
		if rec == nil {
			t.Fatal("getRecipe() returned nil")
		}
	})
}

func TestTestTemplateGetter(t *testing.T) {
	templates := map[string]string{
		"test1": "content1",
		"test2": "content2",
	}

	getter := func(name string) (string, bool) {
		tmpl, ok := templates[name]
		return tmpl, ok
	}

	TestTemplateGetter(t, getter, []string{"test1", "test2"})
}

func TestRecipeBuilder_NewRecipeBuilder(t *testing.T) {
	rb := NewRecipeBuilder()
	if rb == nil {
		t.Fatal("NewRecipeBuilder() returned nil")
	}
	if rb.measurements == nil {
		t.Error("measurements should be initialized")
	}
}

func TestRecipeBuilder_WithK8sMeasurement(t *testing.T) {
	rb := NewRecipeBuilder().
		WithK8sMeasurement(
			ConfigSubtype(map[string]any{"version": "1.28.0"}),
		)

	rec := rb.Build()
	if len(rec.Measurements) != 1 {
		t.Errorf("measurements length = %d, want 1", len(rec.Measurements))
	}
	if rec.Measurements[0].Type != measurement.TypeK8s {
		t.Errorf("measurement type = %v, want TypeK8s", rec.Measurements[0].Type)
	}
}

func TestRecipeBuilder_WithGPUMeasurement(t *testing.T) {
	rb := NewRecipeBuilder().
		WithGPUMeasurement(
			SMISubtype(map[string]string{"driver": "580"}),
		)

	rec := rb.Build()
	if len(rec.Measurements) != 1 {
		t.Errorf("measurements length = %d, want 1", len(rec.Measurements))
	}
	if rec.Measurements[0].Type != measurement.TypeGPU {
		t.Errorf("measurement type = %v, want TypeGPU", rec.Measurements[0].Type)
	}
}

func TestRecipeBuilder_WithOSMeasurement(t *testing.T) {
	rb := NewRecipeBuilder().
		WithOSMeasurement(
			ConfigSubtype(map[string]any{"kernel": "5.15"}),
		)

	rec := rb.Build()
	if len(rec.Measurements) != 1 {
		t.Errorf("measurements length = %d, want 1", len(rec.Measurements))
	}
	if rec.Measurements[0].Type != measurement.TypeOS {
		t.Errorf("measurement type = %v, want TypeOS", rec.Measurements[0].Type)
	}
}

func TestRecipeBuilder_WithSystemDMeasurement(t *testing.T) {
	rb := NewRecipeBuilder().
		WithSystemDMeasurement(
			ConfigSubtype(map[string]any{"service": "containerd"}),
		)

	rec := rb.Build()
	if len(rec.Measurements) != 1 {
		t.Errorf("measurements length = %d, want 1", len(rec.Measurements))
	}
	if rec.Measurements[0].Type != measurement.TypeSystemD {
		t.Errorf("measurement type = %v, want TypeSystemD", rec.Measurements[0].Type)
	}
}

func TestRecipeBuilder_Build(t *testing.T) {
	rb := NewRecipeBuilder().
		WithK8sMeasurement(
			ConfigSubtype(map[string]any{"version": "1.28.0"}),
		).
		WithGPUMeasurement(
			SMISubtype(map[string]string{"driver": "580"}),
		)

	rec := rb.Build()
	if rec == nil {
		t.Fatal("Build() returned nil")
	}
	if len(rec.Measurements) != 2 {
		t.Errorf("measurements length = %d, want 2", len(rec.Measurements))
	}
}

func TestImageSubtype(t *testing.T) {
	images := map[string]string{
		"gpu-operator": "v25.3.3",
		"driver":       "580.82.07",
	}

	subtype := ImageSubtype(images)
	if subtype.Name != "image" {
		t.Errorf("subtype name = %s, want image", subtype.Name)
	}
	if len(subtype.Data) != 2 {
		t.Errorf("data length = %d, want 2", len(subtype.Data))
	}
}

func TestConfigSubtype(t *testing.T) {
	configs := map[string]any{
		"string_val": "test",
		"bool_val":   true,
		"int_val":    42,
		"float_val":  3.14,
	}

	subtype := ConfigSubtype(configs)
	if subtype.Name != "config" {
		t.Errorf("subtype name = %s, want config", subtype.Name)
	}
	if len(subtype.Data) != 4 {
		t.Errorf("data length = %d, want 4", len(subtype.Data))
	}

	// Verify types are preserved
	if val, ok := subtype.Data["string_val"]; ok {
		if v, ok := val.Any().(string); !ok || v != "test" {
			t.Error("string_val should be string type")
		}
	}
	if val, ok := subtype.Data["bool_val"]; ok {
		if v, ok := val.Any().(bool); !ok || !v {
			t.Error("bool_val should be bool type")
		}
	}
}

func TestSMISubtype(t *testing.T) {
	data := map[string]string{
		"driver-version": "580.82.07",
		"cuda-version":   "13.1",
	}

	subtype := SMISubtype(data)
	if subtype.Name != "smi" {
		t.Errorf("subtype name = %s, want smi", subtype.Name)
	}
	if len(subtype.Data) != 2 {
		t.Errorf("data length = %d, want 2", len(subtype.Data))
	}
}

func TestTestValidateRecipe(t *testing.T) {
	validateFunc := func(r *recipe.Recipe) error {
		if r == nil {
			return fmt.Errorf("recipe is nil")
		}
		if len(r.Measurements) == 0 {
			return fmt.Errorf("recipe has empty measurements")
		}
		return nil
	}

	TestValidateRecipe(t, validateFunc)
}

func TestAssertConfigValue(t *testing.T) {
	config := map[string]string{
		"key1": "value1",
		"key2": "value2",
	}

	// Should not panic for correct value
	AssertConfigValue(t, config, "key1", "value1")
}
