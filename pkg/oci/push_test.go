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

package oci

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	stderrors "errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/NVIDIA/aicr/pkg/bundler"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// testOCIResult holds common results from OCI packaging operations in tests.
type testOCIResult struct {
	Digest       string
	LayoutDir    string
	ManifestPath string
}

// extractFilesFromOCIArtifact reads an OCI layout and extracts the file list from the artifact layer.
// Returns a map of relative file path to file content.
func extractFilesFromOCIArtifact(t *testing.T, ociLayoutDir, digest string) map[string]string {
	t.Helper()

	// Read manifest
	manifestPath := filepath.Join(ociLayoutDir, "blobs", "sha256", strings.TrimPrefix(digest, "sha256:"))
	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("Failed to read manifest: %v", err)
	}

	var manifest ociv1.Manifest
	if unmarshalErr := json.Unmarshal(manifestData, &manifest); unmarshalErr != nil {
		t.Fatalf("Failed to unmarshal manifest: %v", unmarshalErr)
	}

	if len(manifest.Layers) == 0 {
		t.Fatal("Manifest has no layers")
	}

	// Read and extract the layer
	layerDigest := manifest.Layers[0].Digest.String()
	layerPath := filepath.Join(ociLayoutDir, "blobs", "sha256", strings.TrimPrefix(layerDigest, "sha256:"))
	layerFile, err := os.Open(layerPath)
	if err != nil {
		t.Fatalf("Failed to open layer: %v", err)
	}
	defer layerFile.Close()

	gzr, err := gzip.NewReader(layerFile)
	if err != nil {
		t.Fatalf("Failed to create gzip reader: %v", err)
	}
	defer gzr.Close()

	// Extract all files
	extractedFiles := make(map[string]string)
	tr := tar.NewReader(gzr)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Failed to read tar entry: %v", err)
		}

		if header.Typeflag == tar.TypeReg {
			content, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("Failed to read tar file content: %v", err)
			}
			extractedFiles[header.Name] = string(content)
		}
	}

	return extractedFiles
}

// packageToOCILayout packages a directory into an OCI layout store and returns the result.
// This is a test helper that replicates the core OCI packaging logic for test verification.
func packageToOCILayout(t *testing.T, ctx context.Context, sourceDir, tag string) *testOCIResult {
	t.Helper()

	ociLayoutDir := t.TempDir()
	ociStore, err := oci.New(ociLayoutDir)
	if err != nil {
		t.Fatalf("Failed to create OCI layout store: %v", err)
	}

	fs, err := file.New(sourceDir)
	if err != nil {
		t.Fatalf("Failed to create file store: %v", err)
	}
	defer func() { _ = fs.Close() }()

	fs.TarReproducible = true

	layerDesc, err := fs.Add(ctx, ".", ociv1.MediaTypeImageLayerGzip, sourceDir)
	if err != nil {
		t.Fatalf("Failed to add directory to store: %v", err)
	}

	packOpts := oras.PackManifestOptions{
		Layers: []ociv1.Descriptor{layerDesc},
	}
	manifestDesc, err := oras.PackManifest(ctx, fs, oras.PackManifestVersion1_1, artifactType, packOpts)
	if err != nil {
		t.Fatalf("Failed to pack manifest: %v", err)
	}

	if tagErr := fs.Tag(ctx, manifestDesc, tag); tagErr != nil {
		t.Fatalf("Failed to tag manifest: %v", tagErr)
	}

	desc, err := oras.Copy(ctx, fs, tag, ociStore, tag, oras.DefaultCopyOptions)
	if err != nil {
		t.Fatalf("Failed to copy to OCI layout: %v", err)
	}

	return &testOCIResult{
		Digest:       desc.Digest.String(),
		LayoutDir:    ociLayoutDir,
		ManifestPath: filepath.Join(ociLayoutDir, "blobs", "sha256", strings.TrimPrefix(desc.Digest.String(), "sha256:")),
	}
}

func TestStripProtocol(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "https prefix",
			input:    "https://ghcr.io",
			expected: "ghcr.io",
		},
		{
			name:     "http prefix",
			input:    "http://localhost:5000",
			expected: "localhost:5000",
		},
		{
			name:     "no prefix",
			input:    "registry.example.com",
			expected: "registry.example.com",
		},
		{
			name:     "with port no prefix",
			input:    "localhost:5000",
			expected: "localhost:5000",
		},
		{
			name:     "https with path",
			input:    "https://ghcr.io/nvidia",
			expected: "ghcr.io/nvidia",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripProtocol(tt.input)
			if got != tt.expected {
				t.Errorf("stripProtocol(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestPushFromStore_EmptyTag(t *testing.T) {
	// PushFromStore should fail when tag is empty
	_, err := PushFromStore(context.Background(), "/nonexistent", PushOptions{
		Registry:   "localhost:5000",
		Repository: "test/repo",
		Tag:        "", // Empty tag should fail
	})

	if err == nil {
		t.Error("PushFromStore() expected error for empty tag, got nil")
	}

	// Error message should contain the expected text (structured errors wrap the message)
	if !strings.Contains(err.Error(), "tag is required to push OCI image") {
		t.Errorf("PushFromStore() error = %q, want to contain %q", err.Error(), "tag is required to push OCI image")
	}
}

func TestPushFromStore_InvalidReference(t *testing.T) {
	// PushFromStore should fail for invalid registry references
	_, err := PushFromStore(context.Background(), "/nonexistent", PushOptions{
		Registry:   "invalid registry with spaces",
		Repository: "test/repo",
		Tag:        "v1.0.0",
	})

	if err == nil {
		t.Error("PushFromStore() expected error for invalid registry, got nil")
	}
}

func TestPushOptions_Defaults(t *testing.T) {
	opts := PushOptions{
		SourceDir:  "/tmp/test",
		Registry:   "ghcr.io",
		Repository: "nvidia/aicr",
		Tag:        "v1.0.0",
	}

	// Verify defaults
	if opts.PlainHTTP != false {
		t.Error("PlainHTTP should default to false")
	}
	if opts.InsecureTLS != false {
		t.Error("InsecureTLS should default to false")
	}
}

func TestPushResult_Fields(t *testing.T) {
	result := PushResult{
		Digest:    "sha256:abc123",
		Reference: "ghcr.io/nvidia/aicr:v1.0.0",
	}

	if result.Digest != "sha256:abc123" {
		t.Errorf("Digest = %q, want %q", result.Digest, "sha256:abc123")
	}
	if result.Reference != "ghcr.io/nvidia/aicr:v1.0.0" {
		t.Errorf("Reference = %q, want %q", result.Reference, "ghcr.io/nvidia/aicr:v1.0.0")
	}
}

func TestValidateRegistryReferenceFormat(t *testing.T) {
	tests := []struct {
		name       string
		registry   string
		repository string
		wantErr    bool
	}{
		{
			name:       "valid ghcr.io",
			registry:   "ghcr.io",
			repository: "nvidia/aicr",
			wantErr:    false,
		},
		{
			name:       "valid localhost with port",
			registry:   "localhost:5000",
			repository: "test/repo",
			wantErr:    false,
		},
		{
			name:       "valid with https prefix",
			registry:   "https://ghcr.io",
			repository: "nvidia/aicr",
			wantErr:    false,
		},
		{
			name:       "invalid registry with spaces",
			registry:   "invalid registry",
			repository: "test/repo",
			wantErr:    true,
		},
		{
			name:       "invalid repository with uppercase",
			registry:   "ghcr.io",
			repository: "NVIDIA/AICR",
			wantErr:    true,
		},
		{
			name:       "invalid repository with special chars",
			registry:   "ghcr.io",
			repository: "test/repo@latest",
			wantErr:    true,
		},
		{
			name:       "valid complex repository",
			registry:   "registry.example.com:5000",
			repository: "org/team/project",
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRegistryReference(tt.registry, tt.repository)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateRegistryReference() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPackage_Validation(t *testing.T) {
	ctx := context.Background()

	// Test missing tag
	_, err := Package(ctx, PackageOptions{
		SourceDir:  ".",
		OutputDir:  t.TempDir(),
		Registry:   "ghcr.io",
		Repository: "test/repo",
		Tag:        "",
	})
	if err == nil || !strings.Contains(err.Error(), "tag is required for OCI packaging") {
		t.Errorf("Package() expected tag error, got: %v", err)
	}

	// Test missing registry
	_, err = Package(ctx, PackageOptions{
		SourceDir:  ".",
		OutputDir:  t.TempDir(),
		Registry:   "",
		Repository: "test/repo",
		Tag:        "v1.0.0",
	})
	if err == nil || !strings.Contains(err.Error(), "registry is required for OCI packaging") {
		t.Errorf("Package() expected registry error, got: %v", err)
	}

	// Test missing repository
	_, err = Package(ctx, PackageOptions{
		SourceDir:  ".",
		OutputDir:  t.TempDir(),
		Registry:   "ghcr.io",
		Repository: "",
		Tag:        "v1.0.0",
	})
	if err == nil || !strings.Contains(err.Error(), "repository is required for OCI packaging") {
		t.Errorf("Package() expected repository error, got: %v", err)
	}
}

func TestPackage_CreatesOCILayout(t *testing.T) {
	ctx := context.Background()

	// Create source directory with test files
	sourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "test.yaml"), []byte("content: test"), 0o644); err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	outputDir := t.TempDir()

	result, err := Package(ctx, PackageOptions{
		SourceDir:  sourceDir,
		OutputDir:  outputDir,
		Registry:   "ghcr.io",
		Repository: "test/repo",
		Tag:        "v1.0.0",
	})
	if err != nil {
		t.Fatalf("Package() error = %v", err)
	}

	// Verify result fields
	if result.Digest == "" {
		t.Error("Package() result has empty digest")
	}
	if result.Reference != "ghcr.io/test/repo:v1.0.0" {
		t.Errorf("Package() reference = %q, want %q", result.Reference, "ghcr.io/test/repo:v1.0.0")
	}
	if result.StorePath == "" {
		t.Error("Package() result has empty store path")
	}

	// Verify OCI layout was created
	ociLayoutFile := filepath.Join(result.StorePath, "oci-layout")
	if _, err := os.Stat(ociLayoutFile); os.IsNotExist(err) {
		t.Errorf("Package() did not create oci-layout file at %s", ociLayoutFile)
	}

	// Verify index.json exists
	indexFile := filepath.Join(result.StorePath, "index.json")
	if _, err := os.Stat(indexFile); os.IsNotExist(err) {
		t.Errorf("Package() did not create index.json at %s", indexFile)
	}

	t.Logf("Package() created OCI layout at %s with digest %s", result.StorePath, result.Digest)
}

// TestOCIPackagingIntegration is an integration test that uses the REAL DefaultBundler
// to generate per-component bundle output and the REAL OCI packaging code to create an artifact.
// This verifies the entire pipeline from recipe → bundler → OCI artifact.
func TestOCIPackagingIntegration(t *testing.T) {
	ctx := context.Background()

	// Create output directory for bundler
	bundleOutputDir := t.TempDir()

	// Create a test RecipeResult with cert-manager component reference
	// (RecipeResult is required because bundlers use GetComponentRef)
	rec := &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: recipe.RecipeAPIVersion,
		ComponentRefs: []recipe.ComponentRef{
			{
				Name:       "cert-manager",
				Type:       "Helm",
				Source:     "https://charts.jetstack.io",
				Version:    "v1.14.0",
				ValuesFile: "components/cert-manager/values.yaml",
			},
		},
	}

	// Use the DefaultBundler to generate per-component bundle
	cfg := config.NewConfig(
		config.WithIncludeChecksums(true),
	)
	b, err := bundler.NewWithConfig(cfg)
	if err != nil {
		t.Fatalf("bundler.NewWithConfig() error = %v", err)
	}

	output, err := b.Make(ctx, rec, bundleOutputDir)
	if err != nil {
		t.Fatalf("Bundler.Make() error = %v", err)
	}

	if output.HasErrors() {
		t.Fatalf("Bundler.Make() had errors: %v", output.Errors)
	}

	// Verify bundler created files (per-component bundle is in the output dir directly)
	if _, statErr := os.Stat(bundleOutputDir); os.IsNotExist(statErr) {
		t.Fatalf("Bundler did not create output directory")
	}

	t.Logf("Bundler created %d files in %s", output.TotalFiles, bundleOutputDir)

	// Use helper to package to OCI layout
	tag := "v1.0.0-integration-test"
	ociResult := packageToOCILayout(t, ctx, bundleOutputDir, tag)

	// Verify the manifest was pushed with a valid digest
	if ociResult.Digest == "" {
		t.Error("Pushed manifest has empty digest")
	}

	// Read and verify the manifest structure
	manifestData, err := os.ReadFile(ociResult.ManifestPath)
	if err != nil {
		t.Fatalf("Failed to read manifest: %v", err)
	}

	var manifest ociv1.Manifest
	if unmarshalErr := json.Unmarshal(manifestData, &manifest); unmarshalErr != nil {
		t.Fatalf("Failed to unmarshal manifest: %v", unmarshalErr)
	}

	// Verify artifact type matches what Package() uses
	if manifest.ArtifactType != artifactType {
		t.Errorf("Manifest artifactType = %q, want %q", manifest.ArtifactType, artifactType)
	}

	// Verify we have exactly one layer
	if len(manifest.Layers) != 1 {
		t.Fatalf("Manifest has %d layers, want 1", len(manifest.Layers))
	}

	// Use helper to extract files
	extractedFiles := extractFilesFromOCIArtifact(t, ociResult.LayoutDir, ociResult.Digest)

	// Collect file names for verification
	fileNames := make([]string, 0, len(extractedFiles))
	for name := range extractedFiles {
		fileNames = append(fileNames, name)
	}

	// Verify expected per-component bundle files are present
	expectedFiles := []string{
		"README.md",
		"deploy.sh",
		"checksums.txt",
	}

	sort.Strings(fileNames)
	sort.Strings(expectedFiles)

	for _, expected := range expectedFiles {
		found := false
		for _, actual := range fileNames {
			if actual == expected {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Expected file %q not found in OCI artifact. Got files: %v", expected, fileNames)
		}
	}

	t.Logf("Integration test passed: OCI artifact contains %d files from real bundler output, digest: %s",
		len(fileNames), ociResult.Digest)
}

// TestOCIArtifactStructure tests the OCI packaging with synthetic test files
// to verify the artifact structure is correct.
func TestOCIArtifactStructure(t *testing.T) {
	ctx := context.Background()

	// Create a temporary bundle directory with test files
	bundleDir := t.TempDir()
	testFiles := map[string]string{
		"manifest.yaml":           "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test",
		"helm/chart/Chart.yaml":   "apiVersion: v2\nname: test-chart\nversion: 1.0.0",
		"helm/chart/values.yaml":  "replicaCount: 1\nimage:\n  tag: latest",
		"terraform/main.tf":       "resource \"null_resource\" \"test\" {}",
		"scripts/install.sh":      "#!/bin/bash\necho 'Installing...'",
		"README.md":               "# Test Bundle\nThis is a test bundle.",
		"nested/deep/config.json": `{"key": "value", "nested": {"foo": "bar"}}`,
	}

	for path, content := range testFiles {
		fullPath := filepath.Join(bundleDir, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
			t.Fatalf("Failed to create directory for %s: %v", path, err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			t.Fatalf("Failed to write test file %s: %v", path, err)
		}
	}

	// Use helper to package to OCI layout
	tag := "v1.0.0-test"
	ociResult := packageToOCILayout(t, ctx, bundleDir, tag)

	// Verify the manifest was pushed
	if ociResult.Digest == "" {
		t.Error("Pushed manifest has empty digest")
	}

	// Read and verify the manifest structure
	manifestData, err := os.ReadFile(ociResult.ManifestPath)
	if err != nil {
		t.Fatalf("Failed to read manifest: %v", err)
	}

	var manifest ociv1.Manifest
	if unmarshalErr := json.Unmarshal(manifestData, &manifest); unmarshalErr != nil {
		t.Fatalf("Failed to unmarshal manifest: %v", unmarshalErr)
	}

	// Verify artifact type
	if manifest.ArtifactType != artifactType {
		t.Errorf("Manifest artifactType = %q, want %q", manifest.ArtifactType, artifactType)
	}

	// Verify we have exactly one layer
	if len(manifest.Layers) != 1 {
		t.Fatalf("Manifest has %d layers, want 1", len(manifest.Layers))
	}

	// Use helper to extract files and verify
	extractedFiles := extractFilesFromOCIArtifact(t, ociResult.LayoutDir, ociResult.Digest)

	// Verify all expected files are present with correct content
	for expectedPath, expectedContent := range testFiles {
		actualContent, ok := extractedFiles[expectedPath]
		if !ok {
			t.Errorf("Expected file %q not found in artifact", expectedPath)
			continue
		}
		if actualContent != expectedContent {
			t.Errorf("File %q content mismatch:\n  got:  %q\n  want: %q", expectedPath, actualContent, expectedContent)
		}
	}

	// Verify no unexpected files
	for path := range extractedFiles {
		if _, ok := testFiles[path]; !ok {
			t.Errorf("Unexpected file in artifact: %q", path)
		}
	}

	t.Logf("Successfully verified OCI artifact with %d files, digest: %s", len(extractedFiles), ociResult.Digest)
}

// TestOCIReproducibleBuild verifies that builds are deterministic.
func TestOCIReproducibleBuild(t *testing.T) {
	ctx := context.Background()

	// Create a bundle directory with test files
	bundleDir := t.TempDir()
	testFiles := map[string]string{
		"file1.yaml": "content: one",
		"file2.yaml": "content: two",
		"file3.yaml": "content: three",
	}

	for path, content := range testFiles {
		fullPath := filepath.Join(bundleDir, path)
		if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
			t.Fatalf("Failed to write test file %s: %v", path, err)
		}
	}

	// Build twice and compare digests
	var digests []string
	for i := 0; i < 2; i++ {
		ociLayoutDir := t.TempDir()
		ociStore, err := oci.New(ociLayoutDir)
		if err != nil {
			t.Fatalf("Iteration %d: Failed to create OCI layout store: %v", i, err)
		}

		fs, err := file.New(bundleDir)
		if err != nil {
			t.Fatalf("Iteration %d: Failed to create file store: %v", i, err)
		}

		// Critical: enable reproducible tars
		fs.TarReproducible = true

		layerDesc, err := fs.Add(ctx, ".", ociv1.MediaTypeImageLayerGzip, bundleDir)
		if err != nil {
			_ = fs.Close()
			t.Fatalf("Iteration %d: Failed to add directory to store: %v", i, err)
		}

		packOpts := oras.PackManifestOptions{
			Layers: []ociv1.Descriptor{layerDesc},
			// Use fixed timestamp for reproducible manifest
			ManifestAnnotations: map[string]string{
				ociv1.AnnotationCreated: reproducibleTimestamp,
			},
		}
		manifestDesc, err := oras.PackManifest(ctx, fs, oras.PackManifestVersion1_1, artifactType, packOpts)
		if err != nil {
			_ = fs.Close()
			t.Fatalf("Iteration %d: Failed to pack manifest: %v", i, err)
		}

		tag := "repro-test"
		if tagErr := fs.Tag(ctx, manifestDesc, tag); tagErr != nil {
			_ = fs.Close()
			t.Fatalf("Iteration %d: Failed to tag manifest: %v", i, tagErr)
		}

		desc, err := oras.Copy(ctx, fs, tag, ociStore, tag, oras.DefaultCopyOptions)
		_ = fs.Close()
		if err != nil {
			t.Fatalf("Iteration %d: Failed to copy to OCI layout: %v", i, err)
		}

		digests = append(digests, desc.Digest.String())
	}

	// Verify both builds produced the same digest
	if digests[0] != digests[1] {
		t.Errorf("Reproducible builds produced different digests:\n  build 1: %s\n  build 2: %s", digests[0], digests[1])
	} else {
		t.Logf("Reproducible build verified: both iterations produced digest %s", digests[0])
	}
}

// TestHardLinkDir tests the hardLinkDir function for various scenarios.
func TestHardLinkDir(t *testing.T) {
	t.Run("simple directory", func(t *testing.T) {
		srcDir := t.TempDir()
		dstDir := t.TempDir()

		// Create test files in source
		testFiles := map[string]string{
			"file1.txt": "content 1",
			"file2.txt": "content 2",
		}
		for name, content := range testFiles {
			if err := os.WriteFile(filepath.Join(srcDir, name), []byte(content), 0o644); err != nil {
				t.Fatalf("failed to create test file: %v", err)
			}
		}

		dstPath := filepath.Join(dstDir, "linked")
		if err := hardLinkDir(context.Background(), srcDir, dstPath); err != nil {
			t.Fatalf("hardLinkDir() error = %v", err)
		}

		// Verify all files were linked
		for name, expectedContent := range testFiles {
			content, err := os.ReadFile(filepath.Join(dstPath, name))
			if err != nil {
				t.Errorf("failed to read linked file %s: %v", name, err)
				continue
			}
			if string(content) != expectedContent {
				t.Errorf("file %s content = %q, want %q", name, string(content), expectedContent)
			}
		}
	})

	t.Run("nested directories", func(t *testing.T) {
		srcDir := t.TempDir()
		dstDir := t.TempDir()

		// Create nested structure
		nestedDir := filepath.Join(srcDir, "level1", "level2")
		if err := os.MkdirAll(nestedDir, 0o755); err != nil {
			t.Fatalf("failed to create nested dirs: %v", err)
		}
		if err := os.WriteFile(filepath.Join(nestedDir, "deep.txt"), []byte("deep content"), 0o644); err != nil {
			t.Fatalf("failed to create deep file: %v", err)
		}

		dstPath := filepath.Join(dstDir, "linked")
		if err := hardLinkDir(context.Background(), srcDir, dstPath); err != nil {
			t.Fatalf("hardLinkDir() error = %v", err)
		}

		// Verify nested file exists
		content, err := os.ReadFile(filepath.Join(dstPath, "level1", "level2", "deep.txt"))
		if err != nil {
			t.Fatalf("failed to read nested file: %v", err)
		}
		if string(content) != "deep content" {
			t.Errorf("nested file content = %q, want %q", string(content), "deep content")
		}
	})

	t.Run("source not exist", func(t *testing.T) {
		dstDir := t.TempDir()
		err := hardLinkDir(context.Background(), "/nonexistent/path", filepath.Join(dstDir, "linked"))
		if err == nil {
			t.Error("hardLinkDir() expected error for nonexistent source, got nil")
		}
		if !strings.Contains(err.Error(), "failed to stat source directory") {
			t.Errorf("hardLinkDir() error = %q, want to contain 'failed to stat source directory'", err.Error())
		}
	})

	t.Run("empty directory", func(t *testing.T) {
		srcDir := t.TempDir()
		dstDir := t.TempDir()

		dstPath := filepath.Join(dstDir, "linked")
		if err := hardLinkDir(context.Background(), srcDir, dstPath); err != nil {
			t.Fatalf("hardLinkDir() error = %v", err)
		}

		// Verify destination exists and is a directory
		info, err := os.Stat(dstPath)
		if err != nil {
			t.Fatalf("failed to stat destination: %v", err)
		}
		if !info.IsDir() {
			t.Error("destination should be a directory")
		}
	})

	t.Run("verifies hard link (same inode)", func(t *testing.T) {
		srcDir := t.TempDir()
		dstDir := t.TempDir()

		srcFile := filepath.Join(srcDir, "test.txt")
		if err := os.WriteFile(srcFile, []byte("test content"), 0o644); err != nil {
			t.Fatalf("failed to create test file: %v", err)
		}

		dstPath := filepath.Join(dstDir, "linked")
		if err := hardLinkDir(context.Background(), srcDir, dstPath); err != nil {
			t.Fatalf("hardLinkDir() error = %v", err)
		}

		// Get inode of source and destination files
		srcInfo, err := os.Stat(srcFile)
		if err != nil {
			t.Fatalf("failed to stat source: %v", err)
		}
		dstInfo, err := os.Stat(filepath.Join(dstPath, "test.txt"))
		if err != nil {
			t.Fatalf("failed to stat destination: %v", err)
		}

		// On Unix systems, hard links share the same inode
		if !os.SameFile(srcInfo, dstInfo) {
			t.Error("source and destination should be hard links (same file)")
		}
	})
}

// snapshotDir returns a sorted list of "name=size" strings for every
// regular file directly inside dir (non-recursive). Used to assert that
// a directory's file set is unchanged across an operation.
func snapshotDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %q: %v", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, statErr := e.Info()
		if statErr != nil {
			t.Fatalf("Info %q: %v", e.Name(), statErr)
		}
		out = append(out, e.Name()+"="+strconv.FormatInt(info.Size(), 10))
	}
	sort.Strings(out)
	return out
}

// TestPreparePushDir tests the preparePushDir function.
func TestPreparePushDir(t *testing.T) {
	t.Run("no subdir hardlinks to temp dir, leaves source untouched", func(t *testing.T) {
		// Regression: the no-subdir path used to return sourceDir
		// directly, letting the oras file store write manifest blobs
		// (named after the OCI title annotation, e.g., "AICR Recipe
		// Evidence" with spaces) into the caller's bundle directory.
		// preparePushDir must always return a tempdir so the file
		// store is never rooted in user space.
		srcDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(srcDir, "a.yaml"), []byte("a"), 0o644); err != nil {
			t.Fatalf("seed file: %v", err)
		}
		if err := os.WriteFile(filepath.Join(srcDir, "b.json"), []byte("b"), 0o644); err != nil {
			t.Fatalf("seed file: %v", err)
		}
		before := snapshotDir(t, srcDir)

		result, cleanup, err := preparePushDir(context.Background(), srcDir, "")
		if err != nil {
			t.Fatalf("preparePushDir() error = %v", err)
		}
		if cleanup == nil {
			t.Fatal("cleanup must always be non-nil (no shortcut return)")
		}
		defer cleanup()

		if result == srcDir {
			t.Fatalf("preparePushDir must not return sourceDir directly; got %q == srcDir", result)
		}
		after := snapshotDir(t, srcDir)
		if !reflect.DeepEqual(before, after) {
			t.Errorf("source directory was modified by preparePushDir; before=%v after=%v", before, after)
		}
		for _, name := range []string{"a.yaml", "b.json"} {
			if _, statErr := os.Stat(filepath.Join(result, name)); statErr != nil {
				t.Errorf("expected %q hard-linked into temp dir, got: %v", name, statErr)
			}
		}
	})

	t.Run("with subdir creates temp with links", func(t *testing.T) {
		srcDir := t.TempDir()

		// Create subdir with content
		subDir := filepath.Join(srcDir, "mysubdir")
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatalf("failed to create subdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(subDir, "file.txt"), []byte("content"), 0o644); err != nil {
			t.Fatalf("failed to create file: %v", err)
		}

		result, cleanup, err := preparePushDir(context.Background(), srcDir, "mysubdir")
		if err != nil {
			t.Fatalf("preparePushDir() error = %v", err)
		}
		if cleanup == nil {
			t.Fatal("cleanup should not be nil when subdir specified")
		}
		defer cleanup()

		// Verify the structure preserves the subdir path
		expectedFile := filepath.Join(result, "mysubdir", "file.txt")
		content, err := os.ReadFile(expectedFile)
		if err != nil {
			t.Fatalf("failed to read linked file: %v", err)
		}
		if string(content) != "content" {
			t.Errorf("file content = %q, want %q", string(content), "content")
		}
	})

	t.Run("cleanup removes temp directory", func(t *testing.T) {
		srcDir := t.TempDir()

		// Create subdir with content
		subDir := filepath.Join(srcDir, "mysubdir")
		if err := os.MkdirAll(subDir, 0o755); err != nil {
			t.Fatalf("failed to create subdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(subDir, "file.txt"), []byte("content"), 0o644); err != nil {
			t.Fatalf("failed to create file: %v", err)
		}

		result, cleanup, err := preparePushDir(context.Background(), srcDir, "mysubdir")
		if err != nil {
			t.Fatalf("preparePushDir() error = %v", err)
		}

		// Call cleanup
		cleanup()

		// Verify temp directory is gone
		if _, err := os.Stat(result); !os.IsNotExist(err) {
			t.Errorf("temp directory should be removed after cleanup, but still exists: %s", result)
		}
	})

	t.Run("nonexistent subdir fails", func(t *testing.T) {
		srcDir := t.TempDir()

		_, cleanup, err := preparePushDir(context.Background(), srcDir, "nonexistent")
		if err == nil {
			if cleanup != nil {
				cleanup()
			}
			t.Error("preparePushDir() expected error for nonexistent subdir, got nil")
		}
	})
}

// TestContextCancellation tests that operations respect context cancellation.
func TestContextCancellation(t *testing.T) {
	t.Run("Package respects canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		_, err := Package(ctx, PackageOptions{
			SourceDir:  t.TempDir(),
			OutputDir:  t.TempDir(),
			Registry:   "ghcr.io",
			Repository: "test/repo",
			Tag:        "v1.0.0",
		})

		if err == nil {
			t.Error("Package() expected error for canceled context, got nil")
		}
		if !strings.Contains(err.Error(), "canceled") {
			t.Errorf("Package() error = %q, want to contain 'canceled'", err.Error())
		}
	})

	t.Run("PushFromStore respects canceled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Cancel immediately

		_, err := PushFromStore(ctx, "/nonexistent", PushOptions{
			Registry:   "localhost:5000",
			Repository: "test/repo",
			Tag:        "v1.0.0",
		})

		if err == nil {
			t.Error("PushFromStore() expected error for canceled context, got nil")
		}
		if !strings.Contains(err.Error(), "canceled") {
			t.Errorf("PushFromStore() error = %q, want to contain 'canceled'", err.Error())
		}
	})
}

// TestCreateAuthClient tests the auth client creation function.
func TestCreateAuthClient(t *testing.T) {
	t.Run("creates client with default settings", func(t *testing.T) {
		client, _ := createAuthClientForHost("ghcr.io", false, false)
		if client == nil {
			t.Fatal("createAuthClientForHost() returned nil client")
		}
		if client.Client == nil {
			t.Error("createAuthClientForHost() client.Client is nil")
		}
		if client.Cache == nil {
			t.Error("createAuthClientForHost() client.Cache is nil")
		}
	})

	t.Run("creates client with plainHTTP", func(t *testing.T) {
		client, _ := createAuthClientForHost("ghcr.io", true, false)
		if client == nil {
			t.Fatal("createAuthClientForHost() returned nil client")
		}
	})

	t.Run("creates client with insecureTLS", func(t *testing.T) {
		client, _ := createAuthClientForHost("ghcr.io", false, true)
		if client == nil {
			t.Fatal("createAuthClientForHost() returned nil client")
		}
		// Verify TLS config has InsecureSkipVerify set
		transport, ok := client.Client.Transport.(*http.Transport)
		if !ok {
			t.Fatal("createAuthClientForHost() transport is not *http.Transport")
		}
		if transport.TLSClientConfig == nil {
			t.Error("createAuthClientForHost() TLSClientConfig is nil with insecureTLS=true")
		} else if !transport.TLSClientConfig.InsecureSkipVerify {
			t.Error("createAuthClientForHost() InsecureSkipVerify is false with insecureTLS=true")
		}
	})
}

// TestPushFromStore_MorePaths tests additional error paths in PushFromStore.
func TestPushFromStore_MorePaths(t *testing.T) {
	ctx := context.Background()

	t.Run("invalid store path", func(t *testing.T) {
		_, err := PushFromStore(ctx, "/nonexistent/path/to/store", PushOptions{
			Registry:   "localhost:5000",
			Repository: "test/repo",
			Tag:        "v1.0.0",
		})
		if err == nil {
			t.Error("PushFromStore() expected error for invalid store path, got nil")
		}
	})

	t.Run("valid store but missing tag in store", func(t *testing.T) {
		// Create an empty OCI layout store
		storeDir := t.TempDir()
		ociLayoutPath := filepath.Join(storeDir, "oci-layout")
		if err := os.WriteFile(ociLayoutPath, []byte(`{"imageLayoutVersion": "1.0.0"}`), 0o644); err != nil {
			t.Fatalf("Failed to create oci-layout file: %v", err)
		}
		indexPath := filepath.Join(storeDir, "index.json")
		if err := os.WriteFile(indexPath, []byte(`{"schemaVersion": 2, "manifests": []}`), 0o644); err != nil {
			t.Fatalf("Failed to create index.json file: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(storeDir, "blobs", "sha256"), 0o755); err != nil {
			t.Fatalf("Failed to create blobs directory: %v", err)
		}

		_, err := PushFromStore(ctx, storeDir, PushOptions{
			Registry:   "localhost:5000",
			Repository: "test/repo",
			Tag:        "v1.0.0",
			PlainHTTP:  true, // Use plainHTTP to avoid TLS issues in test
		})
		// This should fail because the tag doesn't exist in the store
		if err == nil {
			t.Error("PushFromStore() expected error for missing tag, got nil")
		}
	})
}

// TestPackage_MorePaths tests additional paths in Package function.
func TestPackage_MorePaths(t *testing.T) {
	ctx := context.Background()

	t.Run("with reproducibleTimestamp annotation", func(t *testing.T) {
		sourceDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(sourceDir, "test.yaml"), []byte("test: data"), 0o644); err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}

		result, err := Package(ctx, PackageOptions{
			SourceDir:  sourceDir,
			OutputDir:  t.TempDir(),
			Registry:   "ghcr.io",
			Repository: "test/repo",
			Tag:        "v1.0.0",
		})
		if err != nil {
			t.Fatalf("Package() error = %v", err)
		}
		if result.Digest == "" {
			t.Error("Package() result has empty digest")
		}
	})

	t.Run("nonexistent source directory", func(t *testing.T) {
		_, err := Package(ctx, PackageOptions{
			SourceDir:  "/nonexistent/source/dir",
			OutputDir:  t.TempDir(),
			Registry:   "ghcr.io",
			Repository: "test/repo",
			Tag:        "v1.0.0",
		})
		if err == nil {
			t.Error("Package() expected error for nonexistent source dir, got nil")
		}
	})

	t.Run("invalid output directory", func(t *testing.T) {
		sourceDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(sourceDir, "test.yaml"), []byte("test: data"), 0o644); err != nil {
			t.Fatalf("Failed to create test file: %v", err)
		}

		_, err := Package(ctx, PackageOptions{
			SourceDir:  sourceDir,
			OutputDir:  "/nonexistent/output/dir",
			Registry:   "ghcr.io",
			Repository: "test/repo",
			Tag:        "v1.0.0",
		})
		if err == nil {
			t.Error("Package() expected error for invalid output dir, got nil")
		}
	})
}

// fakeNetTimeoutErr satisfies net.Error with Timeout()=true so we can drive
// the transient-error branch without hitting the network.
type fakeNetTimeoutErr struct{}

func (fakeNetTimeoutErr) Error() string   { return "fake network timeout" }
func (fakeNetTimeoutErr) Timeout() bool   { return true }
func (fakeNetTimeoutErr) Temporary() bool { return true } //nolint:staticcheck // legacy net.Error API

// stubCopy returns a copyFunc that records its invocations and returns the
// supplied per-attempt errors in order. After the slice is exhausted it
// returns nil (success).
func stubCopy(errs []error, calls *atomic.Int32) copyFunc {
	return func(_ context.Context, _ oras.ReadOnlyTarget, _ string, _ oras.Target, _ string, _ oras.CopyOptions) (ociv1.Descriptor, error) {
		idx := int(calls.Add(1)) - 1
		if idx >= len(errs) {
			return ociv1.Descriptor{}, nil
		}
		return ociv1.Descriptor{}, errs[idx]
	}
}

func TestCopyWithRetry_SucceedsOnFirstAttempt(t *testing.T) {
	var calls atomic.Int32
	stub := stubCopy(nil, &calls) // never returns error

	_, err := copyWithRetryConfig(context.Background(), nil, "src", nil, "dst",
		oras.DefaultCopyOptions, stub, 3, time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("copyWithRetryConfig() unexpected error: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("copy attempts = %d, want 1", got)
	}
}

func TestCopyWithRetry_RetriesTransientThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	// First two calls return transient timeouts, third succeeds.
	stub := stubCopy([]error{fakeNetTimeoutErr{}, fakeNetTimeoutErr{}}, &calls)

	start := time.Now()
	_, err := copyWithRetryConfig(context.Background(), nil, "src", nil, "dst",
		oras.DefaultCopyOptions, stub, 3, time.Millisecond, time.Second)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("copyWithRetryConfig() unexpected error: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("copy attempts = %d, want 3", got)
	}
	// With a 1ms initial backoff and +/-25% jitter, doubling per attempt,
	// total sleep is at most a few ms; 5s is a generous upper bound for CI.
	if elapsed > 5*time.Second {
		t.Errorf("copyWithRetryConfig took too long: %v", elapsed)
	}
}

func TestCopyWithRetry_ExhaustsRetriesOnPersistentTransient(t *testing.T) {
	var calls atomic.Int32
	stub := stubCopy([]error{fakeNetTimeoutErr{}, fakeNetTimeoutErr{}, fakeNetTimeoutErr{}}, &calls)

	_, err := copyWithRetryConfig(context.Background(), nil, "src", nil, "dst",
		oras.DefaultCopyOptions, stub, 3, time.Millisecond, time.Second)
	if err == nil {
		t.Fatal("copyWithRetryConfig() expected error after retries exhausted, got nil")
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("copy attempts = %d, want 3", got)
	}
	if !strings.Contains(err.Error(), "registry push failed after retries") {
		t.Errorf("error = %q, want to contain 'registry push failed after retries'", err.Error())
	}
}

func TestCopyWithRetry_DoesNotRetryNonTransientError(t *testing.T) {
	var calls atomic.Int32
	// 401 Unauthorized — must not retry.
	respErr := &errcode.ErrorResponse{StatusCode: http.StatusUnauthorized}
	stub := stubCopy([]error{respErr}, &calls)

	_, err := copyWithRetryConfig(context.Background(), nil, "src", nil, "dst",
		oras.DefaultCopyOptions, stub, 3, time.Millisecond, time.Second)
	if err == nil {
		t.Fatal("copyWithRetryConfig() expected error for 401, got nil")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("copy attempts = %d, want 1 (no retry on 4xx)", got)
	}
	if !strings.Contains(err.Error(), "registry push failed") {
		t.Errorf("error = %q, want to contain 'registry push failed'", err.Error())
	}
	if strings.Contains(err.Error(), "after retries") {
		t.Errorf("error should not mention retries for non-transient error: %q", err.Error())
	}
}

func TestCopyWithRetry_RetriesOn5xxAnd429(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		wantCalls  int32
	}{
		{"500 internal server error", http.StatusInternalServerError, 3},
		{"502 bad gateway", http.StatusBadGateway, 3},
		{"503 service unavailable", http.StatusServiceUnavailable, 3},
		{"429 too many requests", http.StatusTooManyRequests, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			respErr := &errcode.ErrorResponse{StatusCode: tt.statusCode}
			stub := stubCopy([]error{respErr, respErr, respErr}, &calls)

			_, err := copyWithRetryConfig(context.Background(), nil, "src", nil, "dst",
				oras.DefaultCopyOptions, stub, 3, time.Millisecond, time.Second)
			if err == nil {
				t.Fatal("expected error after exhausting retries on transient status")
			}
			if got := calls.Load(); got != tt.wantCalls {
				t.Errorf("copy attempts = %d, want %d", got, tt.wantCalls)
			}
		})
	}
}

func TestCopyWithRetry_DoesNotRetryOn4xxOtherThan429(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{"400 bad request", http.StatusBadRequest},
		{"401 unauthorized", http.StatusUnauthorized},
		{"403 forbidden", http.StatusForbidden},
		{"404 not found", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			respErr := &errcode.ErrorResponse{StatusCode: tt.statusCode}
			stub := stubCopy([]error{respErr, respErr, respErr}, &calls)

			_, err := copyWithRetryConfig(context.Background(), nil, "src", nil, "dst",
				oras.DefaultCopyOptions, stub, 3, time.Millisecond, time.Second)
			if err == nil {
				t.Fatal("expected error for non-transient 4xx")
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("copy attempts = %d, want 1 (no retry on 4xx)", got)
			}
		})
	}
}

func TestCopyWithRetry_DoesNotRetryOnContextCanceled(t *testing.T) {
	var calls atomic.Int32
	stub := stubCopy([]error{context.Canceled}, &calls)

	_, err := copyWithRetryConfig(context.Background(), nil, "src", nil, "dst",
		oras.DefaultCopyOptions, stub, 3, time.Millisecond, time.Second)
	if err == nil {
		t.Fatal("copyWithRetryConfig() expected error for context.Canceled, got nil")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("copy attempts = %d, want 1 (no retry on context.Canceled)", got)
	}
}

func TestCopyWithRetry_StopsWhenParentContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before any attempt

	var calls atomic.Int32
	stub := stubCopy([]error{fakeNetTimeoutErr{}, fakeNetTimeoutErr{}, fakeNetTimeoutErr{}}, &calls)

	_, err := copyWithRetryConfig(ctx, nil, "src", nil, "dst",
		oras.DefaultCopyOptions, stub, 3, time.Millisecond, time.Second)
	if err == nil {
		t.Fatal("copyWithRetryConfig() expected error for canceled parent context, got nil")
	}
	if got := calls.Load(); got > 0 {
		t.Errorf("copy attempts = %d, want 0 when parent ctx already canceled", got)
	}
}

func TestCopyWithRetry_SingleAttemptHonored(t *testing.T) {
	// maxAttempts=1 should never retry, even for a transient error.
	var calls atomic.Int32
	stub := stubCopy([]error{fakeNetTimeoutErr{}}, &calls)

	_, err := copyWithRetryConfig(context.Background(), nil, "src", nil, "dst",
		oras.DefaultCopyOptions, stub, 1, time.Millisecond, time.Second)
	if err == nil {
		t.Fatal("copyWithRetryConfig() expected error, got nil")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("copy attempts = %d, want 1", got)
	}
}

func TestCopyWithRetry_RetriesOnPerAttemptDeadlineExceeded(t *testing.T) {
	var calls atomic.Int32
	stub := stubCopy([]error{context.DeadlineExceeded, context.DeadlineExceeded}, &calls)

	_, err := copyWithRetryConfig(context.Background(), nil, "src", nil, "dst",
		oras.DefaultCopyOptions, stub, 3, time.Millisecond, time.Second)
	if err != nil {
		t.Fatalf("copyWithRetryConfig() expected success after retry, got %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("copy attempts = %d, want 3", got)
	}
}

func TestIsTransientPushError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context.Canceled", context.Canceled, false},
		{"context.DeadlineExceeded", context.DeadlineExceeded, true},
		{"net timeout", fakeNetTimeoutErr{}, true},
		{"500", &errcode.ErrorResponse{StatusCode: 500}, true},
		{"502", &errcode.ErrorResponse{StatusCode: 502}, true},
		{"429", &errcode.ErrorResponse{StatusCode: 429}, true},
		{"401", &errcode.ErrorResponse{StatusCode: 401}, false},
		{"404", &errcode.ErrorResponse{StatusCode: 404}, false},
		{"plain error", stderrors.New("something else"), false},
		{"network error string", &net.OpError{Op: "dial", Err: stderrors.New("connection refused")}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientPushError(tt.err); got != tt.want {
				t.Errorf("isTransientPushError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestCopyDir_RecursiveContentAndModes verifies the EXDEV fallback path.
// preparePushDir uses copyDir when hard links fail (e.g., $TMPDIR is on a
// different filesystem from sourceDir, common in containers with tmpfs
// /tmp). The fallback must reproduce the directory tree, file content,
// and mode bits — anything less leaves the oras push pointing at an
// incomplete bundle.
func TestCopyDir_RecursiveContentAndModes(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "out")

	// Tree: src/{a.txt, sub/b.txt}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("beta"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := copyDir(context.Background(), src, dst); err != nil {
		t.Fatalf("copyDir: %v", err)
	}

	for path, want := range map[string]string{
		filepath.Join(dst, "a.txt"):        "alpha",
		filepath.Join(dst, "sub", "b.txt"): "beta",
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		if string(body) != want {
			t.Errorf("%s = %q, want %q", path, body, want)
		}
	}

	// Modes must round-trip — the oras file store reads files via os.Open
	// (mode-agnostic) but downstream consumers may rely on permission
	// bits, so the EXDEV fallback must not silently downgrade them.
	for path, want := range map[string]os.FileMode{
		filepath.Join(dst, "a.txt"):        0o600,
		filepath.Join(dst, "sub", "b.txt"): 0o644,
		filepath.Join(dst, "sub"):          0o755,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("stat %s: %v", path, err)
			continue
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s perm = %o, want %o", path, got, want)
		}
	}
}

// TestCopyDir_RespectsCanceledContext locks in the contract that the
// EXDEV-fallback walk surfaces context cancellation rather than running
// to completion. A pre-canceled ctx must fail fast before any file I/O.
func TestCopyDir_RespectsCanceledContext(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := copyDir(ctx, src, filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatalf("expected error for canceled context")
	}
}

// TestHardLinkDir_RespectsCanceledContext mirrors the cancel test on the
// hardlink path so both walks share the same cancellation guarantee.
func TestHardLinkDir_RespectsCanceledContext(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := hardLinkDir(ctx, src, filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatalf("expected error for canceled context")
	}
}

func TestJitterDuration(t *testing.T) {
	t.Run("zero returns zero", func(t *testing.T) {
		if got := jitterDuration(0); got != 0 {
			t.Errorf("jitterDuration(0) = %v, want 0", got)
		}
	})

	t.Run("within +/-25%", func(t *testing.T) {
		base := 100 * time.Millisecond
		minD := time.Duration(float64(base) * 0.75)
		maxD := time.Duration(float64(base) * 1.25)
		// Sample several times to guard against single unlucky draws.
		for i := 0; i < 100; i++ {
			got := jitterDuration(base)
			if got < minD || got >= maxD {
				t.Fatalf("jitterDuration(%v) = %v, want in [%v, %v)", base, got, minD, maxD)
			}
		}
	})
}
