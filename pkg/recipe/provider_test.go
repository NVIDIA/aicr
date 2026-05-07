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

	"gopkg.in/yaml.v3"
)

// testEmptyRegistryContent is a minimal registry.yaml for testing.
const testEmptyRegistryContent = `apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components: []
`

// TestEmbeddedDataProvider tests the embedded data provider.
func TestEmbeddedDataProvider(t *testing.T) {
	provider := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")

	t.Run("read existing file", func(t *testing.T) {
		data, err := provider.ReadFile("registry.yaml")
		if err != nil {
			t.Fatalf("failed to read registry.yaml: %v", err)
		}
		if len(data) == 0 {
			t.Error("registry.yaml is empty")
		}
	})

	t.Run("read non-existent file", func(t *testing.T) {
		_, err := provider.ReadFile("non-existent.yaml")
		if err == nil {
			t.Error("expected error for non-existent file")
		}
	})

	t.Run("source returns embedded", func(t *testing.T) {
		source := provider.Source("registry.yaml")
		if source != sourceEmbedded {
			t.Errorf("expected source %q, got %q", sourceEmbedded, source)
		}
	})
}

// TestLayeredDataProvider_RequiresRegistry tests that external dir must have registry.yaml.
func TestLayeredDataProvider_RequiresRegistry(t *testing.T) {
	// Create temp directory without registry.yaml
	tmpDir := t.TempDir()

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	_, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})

	if err == nil {
		t.Error("expected error when registry.yaml is missing")
	}
}

// TestLayeredDataProvider_MergesRegistry tests registry merging.
func TestLayeredDataProvider_MergesRegistry(t *testing.T) {
	// Create temp directory with registry.yaml
	tmpDir := t.TempDir()

	// Create a registry with a custom component
	registryContent := `apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: custom-component
    displayName: Custom Component
    helm:
      defaultRepository: https://example.com/charts
      defaultChart: custom/custom-component
`
	if err := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(registryContent), 0600); err != nil {
		t.Fatalf("failed to write registry.yaml: %v", err)
	}

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	// Read merged registry
	data, err := provider.ReadFile("registry.yaml")
	if err != nil {
		t.Fatalf("failed to read registry.yaml: %v", err)
	}

	// Should contain both embedded and custom components
	content := string(data)
	if !strings.Contains(content, "custom-component") {
		t.Error("merged registry should contain custom-component from external")
	}
	if !strings.Contains(content, "gpu-operator") {
		t.Error("merged registry should contain gpu-operator from embedded")
	}
}

// TestLayeredDataProvider_OverridesFile tests file replacement.
func TestLayeredDataProvider_OverridesFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Create registry.yaml (required)
	registryContent := testEmptyRegistryContent
	if err := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(registryContent), 0600); err != nil {
		t.Fatalf("failed to write registry.yaml: %v", err)
	}

	// Create overlays directory
	overlaysDir := filepath.Join(tmpDir, "overlays")
	if err := os.MkdirAll(overlaysDir, 0755); err != nil {
		t.Fatalf("failed to create overlays dir: %v", err)
	}

	// Create a custom base.yaml that will override embedded (now in overlays/)
	baseContent := `apiVersion: aicr.nvidia.com/v1alpha1
kind: RecipeMetadata
metadata:
  name: custom-base
spec:
  components: []
`
	if err := os.WriteFile(filepath.Join(overlaysDir, "base.yaml"), []byte(baseContent), 0600); err != nil {
		t.Fatalf("failed to write base.yaml: %v", err)
	}

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	// Read overlays/base.yaml - should get external version
	data, err := provider.ReadFile("overlays/base.yaml")
	if err != nil {
		t.Fatalf("failed to read overlays/base.yaml: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "custom-base") {
		t.Error("overlays/base.yaml should be from external directory")
	}

	// Check source
	source := provider.Source("overlays/base.yaml")
	if source != "external" {
		t.Errorf("expected source 'external', got %q", source)
	}
}

// TestLayeredDataProvider_AddsNewFile tests adding new files.
func TestLayeredDataProvider_AddsNewFile(t *testing.T) {
	tmpDir := t.TempDir()

	// Create registry.yaml (required)
	registryContent := testEmptyRegistryContent
	if err := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(registryContent), 0600); err != nil {
		t.Fatalf("failed to write registry.yaml: %v", err)
	}

	// Create a new overlay that doesn't exist in embedded
	overlaysDir := filepath.Join(tmpDir, "overlays")
	if err := os.MkdirAll(overlaysDir, 0755); err != nil {
		t.Fatalf("failed to create overlays dir: %v", err)
	}

	overlayContent := `apiVersion: aicr.nvidia.com/v1alpha1
kind: RecipeMetadata
metadata:
  name: custom-overlay
spec:
  criteria:
    service: custom
  components: []
`
	if err := os.WriteFile(filepath.Join(overlaysDir, "custom-overlay.yaml"), []byte(overlayContent), 0600); err != nil {
		t.Fatalf("failed to write custom-overlay.yaml: %v", err)
	}

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	// Read new overlay
	data, err := provider.ReadFile("overlays/custom-overlay.yaml")
	if err != nil {
		t.Fatalf("failed to read custom-overlay.yaml: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "custom-overlay") {
		t.Error("should be able to read custom overlay from external")
	}
}

// TestLayeredDataProvider_SecurityChecks tests security validations.
func TestLayeredDataProvider_SecurityChecks(t *testing.T) {
	t.Run("rejects symlinks", func(t *testing.T) {
		tmpDir := t.TempDir()

		// Create registry.yaml
		registryContent := testEmptyRegistryContent
		if err := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(registryContent), 0600); err != nil {
			t.Fatalf("failed to write registry.yaml: %v", err)
		}

		// Create a symlink
		symlinkPath := filepath.Join(tmpDir, "symlink.yaml")
		targetPath := filepath.Join(tmpDir, "registry.yaml")
		if err := os.Symlink(targetPath, symlinkPath); err != nil {
			t.Skipf("cannot create symlinks: %v", err)
		}

		embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
		_, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
			ExternalDir:   tmpDir,
			AllowSymlinks: false,
		})

		if err == nil {
			t.Error("expected error for symlink")
		}
	})

	t.Run("rejects large files", func(t *testing.T) {
		tmpDir := t.TempDir()

		// Create registry.yaml that exceeds size limit
		largeContent := make([]byte, 100) // Small for test, but we'll set a tiny limit
		if err := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), largeContent, 0600); err != nil {
			t.Fatalf("failed to write registry.yaml: %v", err)
		}

		embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
		_, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
			ExternalDir: tmpDir,
			MaxFileSize: 10, // Very small limit
		})

		if err == nil {
			t.Error("expected error for file exceeding size limit")
		}
	})

	t.Run("rejects missing directory", func(t *testing.T) {
		embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
		_, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
			ExternalDir: "/non/existent/path",
		})

		if err == nil {
			t.Error("expected error for non-existent directory")
		}
	})
}

// TestLayeredDataProvider_FallsBackToEmbedded tests fallback behavior.
func TestLayeredDataProvider_FallsBackToEmbedded(t *testing.T) {
	tmpDir := t.TempDir()

	// Create registry.yaml (required)
	registryContent := testEmptyRegistryContent
	if err := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(registryContent), 0600); err != nil {
		t.Fatalf("failed to write registry.yaml: %v", err)
	}

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	// Read overlays/base.yaml - should fall back to embedded since we didn't override it
	data, err := provider.ReadFile("overlays/base.yaml")
	if err != nil {
		t.Fatalf("failed to read overlays/base.yaml: %v", err)
	}

	if len(data) == 0 {
		t.Error("overlays/base.yaml should not be empty")
	}

	// Source should be embedded
	source := provider.Source("overlays/base.yaml")
	if source != "embedded" {
		t.Errorf("expected source 'embedded', got %q", source)
	}
}

// TestLayeredDataProvider_IntegrationWithRegistry tests that the layered provider
// correctly merges registry files by testing the merged content directly.
func TestLayeredDataProvider_IntegrationWithRegistry(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a registry with an additional custom component
	registryContent := `apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: custom-operator
    displayName: Custom Operator
    helm:
      defaultRepository: https://custom.example.com/charts
      defaultChart: custom/custom-operator
      defaultVersion: v1.0.0
`
	if err := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(registryContent), 0600); err != nil {
		t.Fatalf("failed to write registry.yaml: %v", err)
	}

	// Create layered provider
	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	layered, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	// Read the merged registry directly from the provider
	mergedData, err := layered.ReadFile("registry.yaml")
	if err != nil {
		t.Fatalf("failed to read merged registry: %v", err)
	}

	// Parse the merged registry
	var registry ComponentRegistry
	if err := yaml.Unmarshal(mergedData, &registry); err != nil {
		t.Fatalf("failed to parse merged registry: %v", err)
	}

	// Build index for lookup
	registry.byName = make(map[string]*ComponentConfig, len(registry.Components))
	for i := range registry.Components {
		comp := &registry.Components[i]
		registry.byName[comp.Name] = comp
	}

	// Verify custom component exists
	customComp := registry.Get("custom-operator")
	if customComp == nil {
		t.Error("custom-operator should exist in merged registry")
	} else if customComp.DisplayName != "Custom Operator" {
		t.Errorf("custom-operator displayName = %q, want 'Custom Operator'", customComp.DisplayName)
	}

	// Verify embedded components still exist
	gpuOp := registry.Get("gpu-operator")
	if gpuOp == nil {
		t.Error("gpu-operator should still exist from embedded registry")
	}

	certManager := registry.Get("cert-manager")
	if certManager == nil {
		t.Error("cert-manager should still exist from embedded registry")
	}
}

// TestLayeredDataProvider_OverrideComponentValues tests overriding component values files.
func TestLayeredDataProvider_OverrideComponentValues(t *testing.T) {
	tmpDir := t.TempDir()

	// Create required registry.yaml
	if err := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(testEmptyRegistryContent), 0600); err != nil {
		t.Fatalf("failed to write registry.yaml: %v", err)
	}

	// Create custom values file for cert-manager
	componentsDir := filepath.Join(tmpDir, "components", "cert-manager")
	if err := os.MkdirAll(componentsDir, 0755); err != nil {
		t.Fatalf("failed to create components dir: %v", err)
	}

	customValues := `# Custom values for testing
installCRDs: false
customField: customValue
`
	if err := os.WriteFile(filepath.Join(componentsDir, "values.yaml"), []byte(customValues), 0600); err != nil {
		t.Fatalf("failed to write custom values: %v", err)
	}

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	// Read the custom values
	data, err := provider.ReadFile("components/cert-manager/values.yaml")
	if err != nil {
		t.Fatalf("failed to read custom values: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "customField") {
		t.Error("custom values should contain customField")
	}
	if !strings.Contains(content, "customValue") {
		t.Error("custom values should contain customValue")
	}

	// Verify source is external
	source := provider.Source("components/cert-manager/values.yaml")
	if source != "external" {
		t.Errorf("expected source 'external', got %q", source)
	}
}

// TestLayeredDataProvider_WalkDir tests walking directories with layered provider.
func TestLayeredDataProvider_WalkDir(t *testing.T) {
	tmpDir := t.TempDir()

	// Create registry.yaml (required)
	if err := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(testEmptyRegistryContent), 0600); err != nil {
		t.Fatalf("failed to write registry.yaml: %v", err)
	}

	// Create external overlays directory with a custom file
	overlaysDir := filepath.Join(tmpDir, "overlays")
	if err := os.MkdirAll(overlaysDir, 0755); err != nil {
		t.Fatalf("failed to create overlays dir: %v", err)
	}

	customOverlay := `apiVersion: aicr.nvidia.com/v1alpha1
kind: RecipeMetadata
metadata:
  name: walk-test-overlay
spec:
  criteria:
    service: walktest
  componentRefs: []
`
	if err := os.WriteFile(filepath.Join(overlaysDir, "walk-test.yaml"), []byte(customOverlay), 0600); err != nil {
		t.Fatalf("failed to write walk-test.yaml: %v", err)
	}

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	t.Run("walks overlays directory", func(t *testing.T) {
		var files []string
		err := provider.WalkDir("overlays", func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("WalkDir failed: %v", err)
		}

		// Should find both external and embedded overlay files
		if len(files) == 0 {
			t.Error("expected to find overlay files")
		}

		// Check that external file is included
		foundExternal := false
		for _, f := range files {
			if strings.Contains(f, "walk-test.yaml") {
				foundExternal = true
				break
			}
		}
		if !foundExternal {
			t.Error("expected to find external walk-test.yaml in walk results")
		}
	})

	t.Run("walks root directory", func(t *testing.T) {
		var files []string
		err := provider.WalkDir("", func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("WalkDir failed: %v", err)
		}

		if len(files) == 0 {
			t.Error("expected to find files in root walk")
		}
	})
}

// TestLayeredDataProvider_WalkDirWithOverride tests walking when external overrides embedded files.
func TestLayeredDataProvider_WalkDirWithOverride(t *testing.T) {
	tmpDir := t.TempDir()

	// Create registry.yaml (required)
	if err := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(testEmptyRegistryContent), 0600); err != nil {
		t.Fatalf("failed to write registry.yaml: %v", err)
	}

	// Create overlays directory with a file that has same name as one in embedded
	overlaysDir := filepath.Join(tmpDir, "overlays")
	if err := os.MkdirAll(overlaysDir, 0755); err != nil {
		t.Fatalf("failed to create overlays dir: %v", err)
	}

	// Create an overlay file with unique content
	externalOverlay := `apiVersion: aicr.nvidia.com/v1alpha1
kind: RecipeMetadata
metadata:
  name: external-only-overlay
spec:
  criteria:
    service: external-test
  componentRefs: []
`
	if err := os.WriteFile(filepath.Join(overlaysDir, "external-test.yaml"), []byte(externalOverlay), 0600); err != nil {
		t.Fatalf("failed to write external-test.yaml: %v", err)
	}

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	// Walk overlays - should include both external and embedded files
	var files []string
	err = provider.WalkDir("overlays", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir failed: %v", err)
	}

	// Should have multiple files (external + embedded)
	if len(files) < 2 {
		t.Errorf("expected at least 2 files (external + embedded), got %d", len(files))
	}

	// Check external file is present
	foundExternal := false
	for _, f := range files {
		if strings.Contains(f, "external-test.yaml") {
			foundExternal = true
			break
		}
	}
	if !foundExternal {
		t.Error("expected to find external-test.yaml in walk results")
	}
}

// TestLayeredDataProvider_SourceForRegistry tests source reporting for registry file.
func TestLayeredDataProvider_SourceForRegistry(t *testing.T) {
	tmpDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(testEmptyRegistryContent), 0600); err != nil {
		t.Fatalf("failed to write registry.yaml: %v", err)
	}

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	source := provider.Source("registry.yaml")
	if !strings.Contains(source, "merged") {
		t.Errorf("expected source to contain 'merged', got %q", source)
	}
	if !strings.Contains(source, "embedded") {
		t.Errorf("expected source to contain 'embedded', got %q", source)
	}
	if !strings.Contains(source, "external") {
		t.Errorf("expected source to contain 'external', got %q", source)
	}
}

// TestLayeredDataProvider_CachedRegistry tests that merged registry is cached.
func TestLayeredDataProvider_CachedRegistry(t *testing.T) {
	tmpDir := t.TempDir()

	registryContent := `apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: cache-test-component
    displayName: Cache Test
    helm:
      defaultRepository: https://example.com/charts
      defaultChart: cache-test
`
	if err := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(registryContent), 0600); err != nil {
		t.Fatalf("failed to write registry.yaml: %v", err)
	}

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	// First read
	data1, err := provider.ReadFile("registry.yaml")
	if err != nil {
		t.Fatalf("first read failed: %v", err)
	}

	// Second read should return cached result
	data2, err := provider.ReadFile("registry.yaml")
	if err != nil {
		t.Fatalf("second read failed: %v", err)
	}

	if string(data1) != string(data2) {
		t.Error("cached registry should return same content")
	}
}

// TestEmbeddedDataProvider_WalkDir tests walking embedded filesystem.
func TestEmbeddedDataProvider_WalkDir(t *testing.T) {
	provider := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")

	var files []string
	err := provider.WalkDir("overlays", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir failed: %v", err)
	}

	if len(files) == 0 {
		t.Error("expected to find overlay files in embedded data")
	}
}

// TestLayeredDataProvider_NotDirectory tests error when path is not a directory.
func TestLayeredDataProvider_NotDirectory(t *testing.T) {
	// Create a file instead of directory
	tmpFile, err := os.CreateTemp("", "notadir-*.txt")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	tmpFile.Close()
	defer os.Remove(tmpFile.Name())

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	_, err = NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpFile.Name(),
	})

	if err == nil {
		t.Error("expected error when external path is not a directory")
	}
}

// TestLayeredDataProvider_InvalidExternalRegistry tests error handling for invalid registry.
func TestLayeredDataProvider_InvalidExternalRegistry(t *testing.T) {
	tmpDir := t.TempDir()

	// Create invalid YAML registry
	invalidRegistry := `apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: [invalid yaml structure
`
	if err := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(invalidRegistry), 0600); err != nil {
		t.Fatalf("failed to write registry.yaml: %v", err)
	}

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	// Reading registry should fail due to invalid YAML
	_, err = provider.ReadFile("registry.yaml")
	if err == nil {
		t.Error("expected error for invalid external registry YAML")
	}
}

// TestLayeredDataProvider_ReadExternalFileError tests error when external file can't be read.
func TestLayeredDataProvider_ReadExternalFileError(t *testing.T) {
	tmpDir := t.TempDir()

	// Create registry.yaml (required)
	if err := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(testEmptyRegistryContent), 0600); err != nil {
		t.Fatalf("failed to write registry.yaml: %v", err)
	}

	// Create a file that will be tracked
	testFile := filepath.Join(tmpDir, "test-file.yaml")
	if err := os.WriteFile(testFile, []byte("content"), 0600); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	// Delete the file after provider creation
	if removeErr := os.Remove(testFile); removeErr != nil {
		t.Fatalf("failed to remove test file: %v", removeErr)
	}

	// Reading should fail
	_, err = provider.ReadFile("test-file.yaml")
	if err == nil {
		t.Error("expected error when external file can't be read")
	}
}

// TestLayeredDataProvider_AllowSymlinks tests that symlinks work when allowed.
func TestLayeredDataProvider_AllowSymlinks(t *testing.T) {
	tmpDir := t.TempDir()

	// Create registry.yaml
	if err := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(testEmptyRegistryContent), 0600); err != nil {
		t.Fatalf("failed to write registry.yaml: %v", err)
	}

	// Create a target file
	targetContent := "target content"
	targetPath := filepath.Join(tmpDir, "target.yaml")
	if err := os.WriteFile(targetPath, []byte(targetContent), 0600); err != nil {
		t.Fatalf("failed to write target.yaml: %v", err)
	}

	// Create a symlink
	symlinkPath := filepath.Join(tmpDir, "symlink.yaml")
	if err := os.Symlink(targetPath, symlinkPath); err != nil {
		t.Skipf("cannot create symlinks: %v", err)
	}

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir:   tmpDir,
		AllowSymlinks: true, // Allow symlinks
	})
	if err != nil {
		t.Fatalf("failed to create layered provider with AllowSymlinks=true: %v", err)
	}

	// Should be able to read via symlink
	data, err := provider.ReadFile("symlink.yaml")
	if err != nil {
		t.Fatalf("failed to read symlink file: %v", err)
	}

	if string(data) != targetContent {
		t.Errorf("expected content %q, got %q", targetContent, string(data))
	}
}

// TestDataProviderGeneration tests that generation increments correctly.
func TestDataProviderGeneration(t *testing.T) {
	// Save original state
	originalProvider := globalDataProvider
	originalGen := dataProviderGeneration
	defer func() {
		globalDataProvider = originalProvider
		dataProviderGeneration = originalGen
	}()

	startGen := getDataProviderGeneration()

	// Setting a provider should increment generation
	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	SetDataProvider(embedded)

	newGen := getDataProviderGeneration()
	if newGen != startGen+1 {
		t.Errorf("expected generation %d, got %d", startGen+1, newGen)
	}

	// Setting again should increment again
	SetDataProvider(embedded)
	if getDataProviderGeneration() != startGen+2 {
		t.Errorf("expected generation %d, got %d", startGen+2, getDataProviderGeneration())
	}
}

func TestLayeredDataProvider_ExternalFiles(t *testing.T) {
	tmpDir := t.TempDir()

	// Create registry.yaml (required)
	if err := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(testEmptyRegistryContent), 0600); err != nil {
		t.Fatal(err)
	}

	// Create additional external files
	if err := os.MkdirAll(filepath.Join(tmpDir, "components", "test"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "components", "test", "values.yaml"), []byte("key: value"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "overrides.yaml"), []byte("override: true"), 0600); err != nil {
		t.Fatal(err)
	}

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	files := provider.ExternalFiles()
	if len(files) == 0 {
		t.Fatal("ExternalFiles() returned empty, expected at least 3 files")
	}

	// Should be sorted
	for i := 1; i < len(files); i++ {
		if files[i] < files[i-1] {
			t.Errorf("ExternalFiles() not sorted: %q before %q", files[i-1], files[i])
		}
	}
}

func TestLayeredDataProvider_ExternalDir(t *testing.T) {
	tmpDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(testEmptyRegistryContent), 0600); err != nil {
		t.Fatal(err)
	}

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatal(err)
	}

	if provider.ExternalDir() != tmpDir {
		t.Errorf("ExternalDir() = %q, want %q", provider.ExternalDir(), tmpDir)
	}
}

// testExternalCatalogContent is a minimal catalog.yaml for testing.
const testExternalCatalogContent = `apiVersion: aicr.nvidia.com/v1
kind: ValidatorCatalog
metadata:
  name: custom-validators
  version: "1.0.0"
validators:
  - name: custom-check
    phase: deployment
    description: "Custom deployment check"
    image: example.com/custom/validator:v1.0.0
    timeout: 3m
    args: ["custom-check"]
    env: []
`

// setupCatalogTestDir creates a temp directory with registry.yaml and optionally
// validators/catalog.yaml for catalog merge tests.
func setupCatalogTestDir(t *testing.T, catalogContent string) string {
	t.Helper()
	tmpDir := t.TempDir()

	// Create required registry.yaml
	if err := os.WriteFile(filepath.Join(tmpDir, "registry.yaml"), []byte(testEmptyRegistryContent), 0600); err != nil {
		t.Fatalf("failed to write registry.yaml: %v", err)
	}

	if catalogContent != "" {
		validatorsDir := filepath.Join(tmpDir, "validators")
		if err := os.MkdirAll(validatorsDir, 0755); err != nil {
			t.Fatalf("failed to create validators dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(validatorsDir, "catalog.yaml"), []byte(catalogContent), 0600); err != nil {
			t.Fatalf("failed to write catalog.yaml: %v", err)
		}
	}

	return tmpDir
}

// TestLayeredDataProvider_MergesCatalog tests catalog merging with external data.
func TestLayeredDataProvider_MergesCatalog(t *testing.T) {
	tmpDir := setupCatalogTestDir(t, testExternalCatalogContent)

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	// Read merged catalog
	data, err := provider.ReadFile("validators/catalog.yaml")
	if err != nil {
		t.Fatalf("failed to read catalog: %v", err)
	}

	content := string(data)

	// Should contain custom validator from external
	if !strings.Contains(content, "custom-check") {
		t.Error("merged catalog should contain custom-check from external")
	}

	// Should contain embedded validators
	if !strings.Contains(content, "operator-health") {
		t.Error("merged catalog should contain operator-health from embedded")
	}
	if !strings.Contains(content, "nccl-all-reduce-bw") {
		t.Error("merged catalog should contain nccl-all-reduce-bw from embedded")
	}
}

// TestLayeredDataProvider_CatalogOverrideByName tests that external validators
// override embedded validators with the same name.
func TestLayeredDataProvider_CatalogOverrideByName(t *testing.T) {
	// Override operator-health with a custom image and timeout
	overrideCatalog := `apiVersion: aicr.nvidia.com/v1
kind: ValidatorCatalog
metadata:
  name: custom-validators
  version: "1.0.0"
validators:
  - name: operator-health
    phase: deployment
    description: "Custom operator health check"
    image: example.com/custom/deployment:v2.0.0
    timeout: 5m
    args: ["operator-health", "--custom"]
    env: []
`
	tmpDir := setupCatalogTestDir(t, overrideCatalog)

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	data, err := provider.ReadFile("validators/catalog.yaml")
	if err != nil {
		t.Fatalf("failed to read catalog: %v", err)
	}

	// Parse the merged result
	var cat catalogForMerge
	if err := yaml.Unmarshal(data, &cat); err != nil {
		t.Fatalf("failed to parse merged catalog: %v", err)
	}

	// Find operator-health — should have the external image
	for _, v := range cat.Validators {
		name, _ := v["name"].(string)
		if name == "operator-health" {
			image, _ := v["image"].(string)
			if image != "example.com/custom/deployment:v2.0.0" {
				t.Errorf("operator-health image = %q, want %q", image, "example.com/custom/deployment:v2.0.0")
			}
			desc, _ := v["description"].(string)
			if desc != "Custom operator health check" {
				t.Errorf("operator-health description = %q, want %q", desc, "Custom operator health check")
			}
			return
		}
	}
	t.Error("operator-health not found in merged catalog")
}

// TestLayeredDataProvider_CatalogNoCatalogInExternal tests fallback when external
// directory does not contain validators/catalog.yaml.
func TestLayeredDataProvider_CatalogNoCatalogInExternal(t *testing.T) {
	tmpDir := setupCatalogTestDir(t, "") // No catalog in external

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	// Should fall back to embedded catalog
	data, err := provider.ReadFile("validators/catalog.yaml")
	if err != nil {
		t.Fatalf("failed to read catalog: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "operator-health") {
		t.Error("catalog should fall back to embedded when external has no catalog")
	}

	// Source should be embedded
	source := provider.Source("validators/catalog.yaml")
	if source != sourceEmbedded {
		t.Errorf("expected source %q, got %q", sourceEmbedded, source)
	}
}

// TestLayeredDataProvider_CatalogSourceMerged tests Source() for catalog when external exists.
func TestLayeredDataProvider_CatalogSourceMerged(t *testing.T) {
	tmpDir := setupCatalogTestDir(t, testExternalCatalogContent)

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	source := provider.Source("validators/catalog.yaml")
	if !strings.Contains(source, "merged") {
		t.Errorf("expected source to contain 'merged', got %q", source)
	}
	if !strings.Contains(source, "embedded") {
		t.Errorf("expected source to contain 'embedded', got %q", source)
	}
	if !strings.Contains(source, "external") {
		t.Errorf("expected source to contain 'external', got %q", source)
	}
}

// TestLayeredDataProvider_CachedCatalog tests that merged catalog is cached.
func TestLayeredDataProvider_CachedCatalog(t *testing.T) {
	tmpDir := setupCatalogTestDir(t, testExternalCatalogContent)

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	// First read
	data1, err := provider.ReadFile("validators/catalog.yaml")
	if err != nil {
		t.Fatalf("first read failed: %v", err)
	}

	// Second read should return cached result
	data2, err := provider.ReadFile("validators/catalog.yaml")
	if err != nil {
		t.Fatalf("second read failed: %v", err)
	}

	if string(data1) != string(data2) {
		t.Error("cached catalog should return same content")
	}
}

// TestLayeredDataProvider_InvalidExternalCatalog tests error handling for invalid catalog YAML.
func TestLayeredDataProvider_InvalidExternalCatalog(t *testing.T) {
	invalidCatalog := `apiVersion: aicr.nvidia.com/v1
kind: ValidatorCatalog
validators:
  - name: [invalid yaml structure
`
	tmpDir := setupCatalogTestDir(t, invalidCatalog)

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	_, err = provider.ReadFile("validators/catalog.yaml")
	if err == nil {
		t.Error("expected error for invalid external catalog YAML")
	}
}

// TestLayeredDataProvider_CatalogMergePreservesOrder tests that embedded validator
// order is preserved and new external validators are appended.
func TestLayeredDataProvider_CatalogMergePreservesOrder(t *testing.T) {
	// Add a new validator and override an existing one
	externalCatalog := `apiVersion: aicr.nvidia.com/v1
kind: ValidatorCatalog
metadata:
  name: custom-validators
  version: "1.0.0"
validators:
  - name: operator-health
    phase: deployment
    description: "Overridden operator health"
    image: example.com/custom/deployment:v2.0.0
    timeout: 5m
    args: ["operator-health"]
    env: []
  - name: new-custom-validator
    phase: conformance
    description: "Brand new validator"
    image: example.com/custom/conformance:v1.0.0
    timeout: 3m
    args: ["custom"]
    env: []
`
	tmpDir := setupCatalogTestDir(t, externalCatalog)

	embedded := NewEmbeddedDataProvider(GetEmbeddedFS(), ".")
	provider, err := NewLayeredDataProvider(embedded, LayeredProviderConfig{
		ExternalDir: tmpDir,
	})
	if err != nil {
		t.Fatalf("failed to create layered provider: %v", err)
	}

	data, err := provider.ReadFile("validators/catalog.yaml")
	if err != nil {
		t.Fatalf("failed to read catalog: %v", err)
	}

	var cat catalogForMerge
	if err := yaml.Unmarshal(data, &cat); err != nil {
		t.Fatalf("failed to parse merged catalog: %v", err)
	}

	// First validator should still be operator-health (embedded order preserved)
	if len(cat.Validators) == 0 {
		t.Fatal("expected validators in merged catalog")
	}
	firstName, _ := cat.Validators[0]["name"].(string)
	if firstName != "operator-health" {
		t.Errorf("first validator = %q, want %q", firstName, "operator-health")
	}

	// Last validator should be the new external one (appended)
	lastName, _ := cat.Validators[len(cat.Validators)-1]["name"].(string)
	if lastName != "new-custom-validator" {
		t.Errorf("last validator = %q, want %q", lastName, "new-custom-validator")
	}

	// Overridden operator-health should have external image
	firstImage, _ := cat.Validators[0]["image"].(string)
	if firstImage != "example.com/custom/deployment:v2.0.0" {
		t.Errorf("operator-health image = %q, want %q", firstImage, "example.com/custom/deployment:v2.0.0")
	}
}
