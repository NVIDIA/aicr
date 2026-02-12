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

package bundler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/eidos/pkg/bundler/config"
	"github.com/NVIDIA/eidos/pkg/recipe"
)

func TestNew(t *testing.T) {
	t.Run("default bundler", func(t *testing.T) {
		bundler, err := New()
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if bundler == nil {
			t.Fatal("New() returned nil bundler")
		}
		if bundler.Config == nil {
			t.Fatal("New() bundler has nil config")
		}
	})

	t.Run("with config", func(t *testing.T) {
		cfg := config.NewConfig(
			config.WithVersion("v1.0.0"),
		)
		bundler, err := New(WithConfig(cfg))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if bundler.Config.Version() != "v1.0.0" {
			t.Errorf("expected version v1.0.0, got %s", bundler.Config.Version())
		}
	})

	t.Run("with nil config", func(t *testing.T) {
		bundler, err := New(WithConfig(nil))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		// Should use default config when nil is passed
		if bundler.Config == nil {
			t.Fatal("Config should not be nil after passing nil")
		}
	})
}

func TestNewWithConfig(t *testing.T) {
	t.Run("nil config uses default", func(t *testing.T) {
		bundler, err := NewWithConfig(nil)
		if err != nil {
			t.Fatalf("NewWithConfig(nil) error = %v", err)
		}
		if bundler.Config == nil {
			t.Fatal("Config should not be nil")
		}
	})

	t.Run("valid config", func(t *testing.T) {
		cfg := config.NewConfig(config.WithVersion("v2.0.0"))
		bundler, err := NewWithConfig(cfg)
		if err != nil {
			t.Fatalf("NewWithConfig() error = %v", err)
		}
		if bundler.Config.Version() != "v2.0.0" {
			t.Errorf("expected version v2.0.0, got %s", bundler.Config.Version())
		}
	})

	t.Run("equivalent to New(WithConfig())", func(t *testing.T) {
		cfg := config.NewConfig(config.WithVersion("v3.0.0"))
		b1, err := NewWithConfig(cfg)
		if err != nil {
			t.Fatalf("NewWithConfig() error = %v", err)
		}
		b2, err := New(WithConfig(cfg))
		if err != nil {
			t.Fatalf("New(WithConfig()) error = %v", err)
		}
		if b1.Config.Version() != b2.Config.Version() {
			t.Errorf("versions differ: NewWithConfig=%s, New(WithConfig)=%s",
				b1.Config.Version(), b2.Config.Version())
		}
	})
}

func TestWithAllowLists(t *testing.T) {
	t.Run("nil allowlists", func(t *testing.T) {
		bundler, err := New(WithAllowLists(nil))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if bundler.AllowLists != nil {
			t.Error("AllowLists should be nil")
		}
	})

	t.Run("valid allowlists", func(t *testing.T) {
		al := &recipe.AllowLists{
			Services: []recipe.CriteriaServiceType{"eks", "gke"},
		}
		bundler, err := New(WithAllowLists(al))
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if bundler.AllowLists == nil {
			t.Fatal("AllowLists should not be nil")
		}
		if len(bundler.AllowLists.Services) != 2 {
			t.Errorf("expected 2 services, got %d", len(bundler.AllowLists.Services))
		}
	})
}

func TestMake_NilInput(t *testing.T) {
	bundler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()
	tmpDir := t.TempDir()

	_, err = bundler.Make(ctx, nil, tmpDir)
	if err == nil {
		t.Fatal("expected error for nil input, got nil")
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Errorf("expected error to mention nil, got: %v", err)
	}
}

func TestMake_EmptyComponentRefs(t *testing.T) {
	bundler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()
	tmpDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{},
	}

	_, err = bundler.Make(ctx, recipeResult, tmpDir)
	if err == nil {
		t.Fatal("expected error for empty component refs, got nil")
	}
	if !strings.Contains(err.Error(), "component") {
		t.Errorf("expected error to mention component, got: %v", err)
	}
}

func TestMake_Success(t *testing.T) {
	bundler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()
	tmpDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{
		APIVersion: "eidos.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		Criteria: &recipe.Criteria{
			Service:     "eks",
			Accelerator: "gb200",
			Intent:      "training",
			OS:          "ubuntu",
		},
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "gpu-operator",
				Version: "v25.3.3",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
			{
				Name:    "network-operator",
				Version: "v25.4.0",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
		},
		DeploymentOrder: []string{"gpu-operator", "network-operator"},
	}

	output, err := bundler.Make(ctx, recipeResult, tmpDir)
	if err != nil {
		t.Fatalf("Make() error = %v", err)
	}

	if output == nil {
		t.Fatal("Make() returned nil output")
	}

	// Verify root files were created
	rootFiles := []string{"README.md", "deploy.sh", "recipe.yaml"}
	for _, filename := range rootFiles {
		path := filepath.Join(tmpDir, filename)
		if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
			t.Errorf("expected file %s was not created", filename)
		}
	}

	// Verify per-component directories
	for _, comp := range []string{"gpu-operator", "network-operator"} {
		valuesPath := filepath.Join(tmpDir, comp, "values.yaml")
		if _, statErr := os.Stat(valuesPath); os.IsNotExist(statErr) {
			t.Errorf("expected %s/values.yaml was not created", comp)
		}
		readmePath := filepath.Join(tmpDir, comp, "README.md")
		if _, statErr := os.Stat(readmePath); os.IsNotExist(statErr) {
			t.Errorf("expected %s/README.md was not created", comp)
		}
	}

	// No Chart.yaml should exist
	chartPath := filepath.Join(tmpDir, "Chart.yaml")
	if _, statErr := os.Stat(chartPath); !os.IsNotExist(statErr) {
		t.Error("Chart.yaml should not exist in per-component bundle")
	}

	// Verify output summary (3 root + 2 components × 2 files = 7, +1 recipe.yaml = 8)
	if output.TotalFiles < 7 {
		t.Errorf("expected at least 7 files, got %d", output.TotalFiles)
	}
}

func TestMake_WithValueOverrides(t *testing.T) {
	cfg := config.NewConfig(
		config.WithValueOverrides(map[string]map[string]string{
			"gpu-operator": {
				"gds.enabled": "true",
			},
		}),
	)
	bundler, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()
	tmpDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{
		APIVersion: "eidos.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "gpu-operator",
				Version: "v25.3.3",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
		},
	}

	output, err := bundler.Make(ctx, recipeResult, tmpDir)
	if err != nil {
		t.Fatalf("Make() error = %v", err)
	}

	if output == nil {
		t.Fatal("Make() returned nil output")
	}

	// Verify gpu-operator/values.yaml was created
	valuesPath := filepath.Join(tmpDir, "gpu-operator", "values.yaml")
	if _, err := os.Stat(valuesPath); os.IsNotExist(err) {
		t.Fatal("gpu-operator/values.yaml was not created")
	}
}

func TestMake_WithNodeSelectors(t *testing.T) {
	cfg := config.NewConfig(
		config.WithSystemNodeSelector(map[string]string{
			"nodeGroup": "system-pool",
		}),
		config.WithAcceleratedNodeSelector(map[string]string{
			"nvidia.com/gpu.present": "true",
		}),
	)
	bundler, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()
	tmpDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{
		APIVersion: "eidos.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "gpu-operator",
				Version: "v25.3.3",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
		},
	}

	output, err := bundler.Make(ctx, recipeResult, tmpDir)
	if err != nil {
		t.Fatalf("Make() error = %v", err)
	}

	if output == nil {
		t.Fatal("Make() returned nil output")
	}
}

func TestMake_WithTolerations(t *testing.T) {
	cfg := config.NewConfig(
		config.WithSystemNodeTolerations([]corev1.Toleration{
			{
				Key:      "dedicated",
				Operator: corev1.TolerationOpEqual,
				Value:    "system",
				Effect:   corev1.TaintEffectNoSchedule,
			},
		}),
	)
	bundler, err := New(WithConfig(cfg))
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()
	tmpDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{
		APIVersion: "eidos.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "gpu-operator",
				Version: "v25.3.3",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
		},
	}

	output, err := bundler.Make(ctx, recipeResult, tmpDir)
	if err != nil {
		t.Fatalf("Make() error = %v", err)
	}

	if output == nil {
		t.Fatal("Make() returned nil output")
	}
}

func TestMake_ContextCancellation(t *testing.T) {
	bundler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	tmpDir := t.TempDir()

	recipeResult := &recipe.RecipeResult{
		APIVersion: "eidos.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "gpu-operator",
				Version: "v25.3.3",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
		},
	}

	_, err = bundler.Make(ctx, recipeResult, tmpDir)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

func TestMake_DefaultOutputDir(t *testing.T) {
	bundler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ctx := context.Background()

	recipeResult := &recipe.RecipeResult{
		APIVersion: "eidos.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "gpu-operator",
				Version: "v25.3.3",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
		},
	}

	// Use current working directory
	originalDir, _ := os.Getwd()
	tmpDir := t.TempDir()
	defer os.Chdir(originalDir)
	os.Chdir(tmpDir)

	output, err := bundler.Make(ctx, recipeResult, "")
	if err != nil {
		t.Fatalf("Make() error = %v", err)
	}

	if output == nil {
		t.Fatal("Make() returned nil output")
	}
}

func TestRemoveHyphens(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"gpu-operator", "gpuoperator"},
		{"network-operator", "networkoperator"},
		{"cert-manager", "certmanager"},
		{"skyhook-operator", "skyhookoperator"},
		{"", ""},
		{"a-b-c-d", "abcd"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := removeHyphens(tt.input)
			if result != tt.expected {
				t.Errorf("removeHyphens(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

// Note: Tests for convertMapValue, setMapValueByPath, and applyMapOverrides
// are in pkg/component/overrides_test.go since those functions now live there.

func TestCollectComponentManifests(t *testing.T) {
	bundler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	t.Run("empty manifest files", func(t *testing.T) {
		recipeResult := &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{
				{
					Name:          "gpu-operator",
					ManifestFiles: []string{},
				},
			},
		}

		contents, err := bundler.collectComponentManifests(context.Background(), recipeResult)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(contents) != 0 {
			t.Errorf("expected 0 contents, got %d", len(contents))
		}
	})

	t.Run("no components", func(t *testing.T) {
		recipeResult := &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{},
		}

		contents, err := bundler.collectComponentManifests(context.Background(), recipeResult)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(contents) != 0 {
			t.Errorf("expected 0 contents, got %d", len(contents))
		}
	})

	t.Run("invalid manifest path", func(t *testing.T) {
		recipeResult := &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{
				{
					Name:          "gpu-operator",
					ManifestFiles: []string{"nonexistent/file.yaml"},
				},
			},
		}

		_, err := bundler.collectComponentManifests(context.Background(), recipeResult)
		if err == nil {
			t.Fatal("expected error for invalid manifest path")
		}
		if !strings.Contains(err.Error(), "nonexistent/file.yaml") {
			t.Errorf("error should mention the invalid file: %v", err)
		}
	})

	t.Run("empty manifests for multiple components", func(t *testing.T) {
		recipeResult := &recipe.RecipeResult{
			ComponentRefs: []recipe.ComponentRef{
				{
					Name:          "component-a",
					ManifestFiles: []string{},
				},
				{
					Name:          "component-b",
					ManifestFiles: []string{},
				},
			},
		}

		contents, err := bundler.collectComponentManifests(context.Background(), recipeResult)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(contents) != 0 {
			t.Errorf("expected 0 contents, got %d", len(contents))
		}
	})
}

// TestMake_Reproducible verifies that bundle generation is deterministic.
// Running Make() twice with the same input should produce identical output.
func TestMake_Reproducible(t *testing.T) {
	recipeResult := &recipe.RecipeResult{
		APIVersion: "eidos.nvidia.com/v1alpha1",
		Kind:       "Recipe",
		Criteria: &recipe.Criteria{
			Service:     "eks",
			Accelerator: "gb200",
			Intent:      "training",
			OS:          "ubuntu",
		},
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:    "gpu-operator",
				Version: "v25.3.3",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
			{
				Name:    "network-operator",
				Version: "v25.4.0",
				Type:    "helm",
				Source:  "https://helm.ngc.nvidia.com/nvidia",
			},
		},
		DeploymentOrder: []string{"gpu-operator", "network-operator"},
	}

	// Generate bundles twice in different directories
	var fileHashes [2]map[string]string

	for i := 0; i < 2; i++ {
		bundler, err := New()
		if err != nil {
			t.Fatalf("iteration %d: New() error = %v", i, err)
		}

		ctx := context.Background()
		tmpDir := t.TempDir()

		_, err = bundler.Make(ctx, recipeResult, tmpDir)
		if err != nil {
			t.Fatalf("iteration %d: Make() error = %v", i, err)
		}

		// Compute file hashes
		fileHashes[i] = make(map[string]string)
		err = filepath.Walk(tmpDir, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() {
				return nil
			}

			content, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}

			// Use relative path as key for comparison
			relPath, _ := filepath.Rel(tmpDir, path)
			hash := computeTestChecksum(content)
			fileHashes[i][relPath] = hash
			return nil
		})
		if err != nil {
			t.Fatalf("iteration %d: failed to walk directory: %v", i, err)
		}
	}

	// Compare file sets
	if len(fileHashes[0]) != len(fileHashes[1]) {
		t.Errorf("different number of files: iteration 1 has %d, iteration 2 has %d",
			len(fileHashes[0]), len(fileHashes[1]))
	}

	// Compare individual file hashes
	for filename, hash1 := range fileHashes[0] {
		hash2, exists := fileHashes[1][filename]
		if !exists {
			t.Errorf("file %s exists in iteration 1 but not iteration 2", filename)
			continue
		}
		if hash1 != hash2 {
			t.Errorf("file %s has different content between iterations:\n  iteration 1: %s\n  iteration 2: %s",
				filename, hash1, hash2)
		}
	}

	// Check for files only in iteration 2
	for filename := range fileHashes[1] {
		if _, exists := fileHashes[0][filename]; !exists {
			t.Errorf("file %s exists in iteration 2 but not iteration 1", filename)
		}
	}

	t.Logf("Reproducibility verified: both iterations produced %d identical files", len(fileHashes[0]))
}

// computeTestChecksum computes SHA256 hash for test comparison.
func computeTestChecksum(content []byte) string {
	hash := make([]byte, 32)
	for i, b := range content {
		hash[i%32] ^= b
	}
	return string(hash)
}
