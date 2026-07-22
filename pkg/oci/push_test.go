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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencontainers/go-digest"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/NVIDIA/aicr/pkg/bundler"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	apperrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// testOCIResult holds common results from OCI packaging operations in tests.
type testOCIResult struct {
	Digest       string
	LayoutDir    string
	ManifestPath string
}

func validTestManifestDescriptor() ociv1.Descriptor {
	return content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, []byte("test-manifest"))
}

func storedTestManifestBytes(id string) []byte {
	return []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":%q,"digest":"sha256:44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a","size":2},"layers":[],"annotations":{"test.id":%q}}`,
		ociv1.MediaTypeImageManifest, ociv1.MediaTypeImageConfig, id))
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
	_, err := PushFromStore(context.Background(), "/nonexistent", validTestManifestDescriptor(), PushOptions{
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
	_, err := PushFromStore(context.Background(), "/nonexistent", validTestManifestDescriptor(), PushOptions{
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

		_, err := PushFromStore(ctx, "/nonexistent", validTestManifestDescriptor(), PushOptions{
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

func TestNewPublicationTargetOwnsRegistryTransportCleanup(t *testing.T) {
	target, err := newPublicationTarget(PushOptions{
		Registry:   "ghcr.io",
		Repository: "test/repo",
	})
	if err != nil {
		t.Fatalf("newPublicationTarget() error = %v", err)
	}
	managed, ok := target.(*remotePublicationTarget)
	if !ok {
		t.Fatalf("newPublicationTarget() type = %T, want *remotePublicationTarget", target)
	}
	transport := &closeIdleCountingTransport{}
	managed.client.Transport = transport
	target.CloseIdleConnections()
	if got := transport.calls.Load(); got != 1 {
		t.Fatalf("transport CloseIdleConnections calls = %d, want 1", got)
	}
}

type closeIdleCountingTransport struct {
	calls atomic.Int32
}

func (t *closeIdleCountingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, stderrors.New("unexpected registry request")
}

func (t *closeIdleCountingTransport) CloseIdleConnections() {
	t.calls.Add(1)
}

func TestPushReferrerClosesRegistryTransport(t *testing.T) {
	subject := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, []byte("subject"))
	opts := ReferrerOptions{
		Registry:     "ghcr.io",
		Repository:   "test/repo",
		ArtifactType: "application/vnd.test.referrer",
		LayerContent: []byte("payload"),
		Subject:      subject,
	}
	for _, tt := range []struct {
		name    string
		copyErr error
	}{
		{name: "success"},
		{name: "failure", copyErr: apperrors.New(apperrors.ErrCodeUnavailable, "copy failed")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			target := newTestPublicationTarget()
			deps := defaultReferrerPushDependencies()
			deps.newTarget = func(PushOptions) (publicationTarget, error) { return target, nil }
			deps.copy = func(
				context.Context,
				oras.ReadOnlyTarget,
				string,
				oras.Target,
				string,
				oras.CopyOptions,
			) (ociv1.Descriptor, error) {

				return subject, tt.copyErr
			}
			result, err := pushReferrerWithDependencies(context.Background(), opts, deps)
			if tt.copyErr != nil {
				if err == nil || result != nil {
					t.Fatalf("failed PushReferrer result=%+v error=%v", result, err)
				}
			} else if err != nil || result == nil {
				t.Fatalf("PushReferrer result=%+v error=%v", result, err)
			}
			if got := target.idleCloses.Load(); got != 1 {
				t.Fatalf("CloseIdleConnections calls = %d, want 1", got)
			}
		})
	}
}

// TestPushFromStore_MorePaths tests additional error paths in PushFromStore.
func TestPushFromStore_MorePaths(t *testing.T) {
	ctx := context.Background()

	t.Run("invalid store path", func(t *testing.T) {
		_, err := PushFromStore(ctx, "/nonexistent/path/to/store", validTestManifestDescriptor(), PushOptions{
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

		_, err := PushFromStore(ctx, storeDir, validTestManifestDescriptor(), PushOptions{
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

func TestCopyWithRetry_CancellationWinsSuccessfulAttempt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	want := validTestManifestDescriptor()
	var calls atomic.Int32
	copy := func(
		context.Context,
		oras.ReadOnlyTarget,
		string,
		oras.Target,
		string,
		oras.CopyOptions,
	) (ociv1.Descriptor, error) {

		calls.Add(1)
		cancel()
		return want, nil
	}

	got, err := copyWithRetryConfig(ctx, nil, "src", nil, "dst",
		oras.DefaultCopyOptions, copy, 1, time.Millisecond, time.Second)
	assertErrorCode(t, err, apperrors.ErrCodeTimeout)
	if !reflect.DeepEqual(got, ociv1.Descriptor{}) {
		t.Fatalf("copyWithRetryConfig() descriptor = %+v after cancellation, want zero", got)
	}
	if gotCalls := calls.Load(); gotCalls != 1 {
		t.Fatalf("copy attempts = %d, want 1", gotCalls)
	}
}

func TestCopyWithRetry_AttemptDeadlineWinsSuccessfulReturn(t *testing.T) {
	want := validTestManifestDescriptor()
	copy := func(
		attemptCtx context.Context,
		_ oras.ReadOnlyTarget,
		_ string,
		_ oras.Target,
		_ string,
		_ oras.CopyOptions,
	) (ociv1.Descriptor, error) {

		<-attemptCtx.Done()
		return want, nil
	}

	got, err := copyWithRetryConfig(context.Background(), nil, "src", nil, "dst",
		oras.DefaultCopyOptions, copy, 1, time.Millisecond, time.Millisecond)
	assertErrorCode(t, err, apperrors.ErrCodeTimeout)
	if !reflect.DeepEqual(got, ociv1.Descriptor{}) {
		t.Fatalf("copyWithRetryConfig() descriptor = %+v after attempt deadline, want zero", got)
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

type testPublicationTarget struct {
	mu          sync.Mutex
	blobs       map[digest.Digest][]byte
	descriptors map[digest.Digest]ociv1.Descriptor
	tags        map[string]ociv1.Descriptor
	order       []string
	resolve     func(context.Context, string) (ociv1.Descriptor, error)
	tag         func(context.Context, ociv1.Descriptor, string) error
	calls       atomic.Int32
	idleCloses  atomic.Int32
}

func (t *testPublicationTarget) CloseIdleConnections() {
	t.idleCloses.Add(1)
}

func newTestPublicationTarget() *testPublicationTarget {
	return &testPublicationTarget{
		blobs:       make(map[digest.Digest][]byte),
		descriptors: make(map[digest.Digest]ociv1.Descriptor),
		tags:        make(map[string]ociv1.Descriptor),
	}
}

func (t *testPublicationTarget) record(value string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.order = append(t.order, value)
	t.calls.Add(1)
}

func (t *testPublicationTarget) Exists(_ context.Context, desc ociv1.Descriptor) (bool, error) {
	t.record("destination Exists")
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.blobs[desc.Digest]
	return ok, nil
}

func (t *testPublicationTarget) Fetch(_ context.Context, desc ociv1.Descriptor) (io.ReadCloser, error) {
	t.record("destination Fetch")
	t.mu.Lock()
	defer t.mu.Unlock()
	data, ok := t.blobs[desc.Digest]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), data...))), nil
}

func (t *testPublicationTarget) Push(_ context.Context, desc ociv1.Descriptor, r io.Reader) error {
	t.record("destination Push")
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if digest.FromBytes(data) != desc.Digest || int64(len(data)) != desc.Size {
		return stderrors.New("pushed bytes do not match descriptor")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.blobs[desc.Digest] = append([]byte(nil), data...)
	t.descriptors[desc.Digest] = desc
	return nil
}

func (t *testPublicationTarget) Resolve(ctx context.Context, reference string) (ociv1.Descriptor, error) {
	t.record("digest Resolve")
	if t.resolve != nil {
		return t.resolve(ctx, reference)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if desc, ok := t.tags[reference]; ok {
		return desc, nil
	}
	parsed := digest.Digest(reference)
	if desc, ok := t.descriptors[parsed]; ok {
		return desc, nil
	}
	return ociv1.Descriptor{}, os.ErrNotExist
}

func (t *testPublicationTarget) Tag(ctx context.Context, desc ociv1.Descriptor, reference string) error {
	t.record("Tag")
	if t.tag != nil {
		if err := t.tag(ctx, desc, reference); err != nil {
			return err
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tags[reference] = desc
	return nil
}

func (t *testPublicationTarget) snapshotOrder() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.order...)
}

func TestPackageOperationOwnsLayoutUntilCloseOrRelease(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("alpha"), 0o640); err != nil {
		t.Fatal(err)
	}
	safeOCITempRoot(t)
	op, err := packageGenericOperation(context.Background(), PackageOptions{
		SourceDir: source, OutputDir: output, SourceFiles: []string{"a.txt"},
		Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
	})
	if err != nil {
		t.Fatalf("packageGenericOperation() error = %v", err)
	}
	if op.layout == nil || op.store == nil || reflect.DeepEqual(op.manifest, ociv1.Descriptor{}) {
		t.Fatalf("package operation did not retain owner/store/manifest: %+v", op)
	}
	if !content.Equal(op.manifest, op.result.Descriptor) {
		t.Fatalf("operation manifest = %+v, result descriptor = %+v", op.manifest, op.result.Descriptor)
	}
	if op.manifest.Annotations != nil || op.manifest.URLs != nil || op.manifest.Data != nil || op.manifest.Platform != nil {
		t.Fatalf("operation retained mutable/non-authoritative descriptor fields: %+v", op.manifest)
	}
	if op.result.Digest != op.manifest.Digest.String() ||
		op.result.MediaType != op.manifest.MediaType || op.result.Size != op.manifest.Size {

		t.Fatalf("PackageResult scalar fields do not mirror Descriptor: %+v", op.result)
	}
	layoutPath := op.layout.Path()
	if _, statErr := os.Stat(layoutPath); statErr != nil {
		t.Fatalf("owned layout missing before Close(): %v", statErr)
	}
	entries, err := os.ReadDir(layoutPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tar.gz") {
			t.Fatalf("successful OCI layout retained plaintext archive %q", entry.Name())
		}
	}
	if err := op.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := os.Lstat(layoutPath); !os.IsNotExist(err) {
		t.Fatalf("owned layout remains after Close(): %v", err)
	}
	if err := op.Close(); err != nil {
		t.Fatalf("repeated Close() error = %v", err)
	}
}

func TestPackageOperationStandalonePackageReleasesLayout(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("alpha"), 0o640); err != nil {
		t.Fatal(err)
	}
	safeOCITempRoot(t)
	result, err := Package(context.Background(), PackageOptions{
		SourceDir: source, OutputDir: output, SourceFiles: []string{"a.txt"},
		Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
	})
	if err != nil {
		t.Fatalf("Package() error = %v", err)
	}
	if reflect.DeepEqual(result.Descriptor, ociv1.Descriptor{}) || result.StorePath == "" {
		t.Fatalf("Package() result = %+v, want frozen descriptor and store path", result)
	}
	if _, err := os.Stat(result.StorePath); err != nil {
		t.Fatalf("released StorePath missing: %v", err)
	}
	if filepath.Dir(result.StorePath) != output {
		t.Fatalf("StorePath parent = %q, want output %q", filepath.Dir(result.StorePath), output)
	}
}

func TestPackageOperationFailureRemovesStageAndPartialLayout(t *testing.T) {
	for _, tt := range []struct {
		name   string
		inject func(*genericPackageDependencies, error)
	}{
		{
			name: "local blob push",
			inject: func(deps *genericPackageDependencies, injected error) {
				deps.pushFileBlob = func(
					context.Context,
					localOCIStore,
					*ownedLayout,
					ociv1.Descriptor,
					string,
				) error {

					return injected
				}
			},
		},
		{
			name: "archive removal",
			inject: func(deps *genericPackageDependencies, injected error) {
				deps.removeArchive = func(*ownedLayout, string) error { return injected }
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := t.TempDir()
			output := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("alpha"), 0o640); err != nil {
				t.Fatal(err)
			}
			tempRoot := safeOCITempRoot(t)
			injected := stderrors.New(tt.name + " failed")
			deps := defaultGenericPackageDependencies()
			tt.inject(&deps, injected)
			op, err := packageGenericOperationWithDependencies(context.Background(), PackageOptions{
				SourceDir: source, OutputDir: output, SourceFiles: []string{"a.txt"},
				Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
			}, deps)
			assertErrorCode(t, err, apperrors.ErrCodeInternal)
			if !stderrors.Is(err, injected) {
				t.Fatalf("error = %v, want cause %v", err, injected)
			}
			if op != nil {
				t.Fatal("failed package returned an operation")
			}
			assertDirectoryEmpty(t, output)
			assertDirectoryEmpty(t, tempRoot)
		})
	}
}

func TestPackageOperationPreservesStructuredNewStoreError(t *testing.T) {
	tests := []struct {
		name string
		code apperrors.ErrorCode
	}{
		{name: "timeout", code: apperrors.ErrCodeTimeout},
		{name: "invalid request", code: apperrors.ErrCodeInvalidRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := t.TempDir()
			output := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("alpha"), 0o640); err != nil {
				t.Fatal(err)
			}
			tempRoot := safeOCITempRoot(t)
			injected := apperrors.New(tt.code, "structured store creation failure")
			deps := defaultGenericPackageDependencies()
			deps.newStore = func(context.Context, *ownedLayout) (localOCIStore, error) {
				return nil, injected
			}

			op, err := packageGenericOperationWithDependencies(context.Background(), PackageOptions{
				SourceDir: source, OutputDir: output, SourceFiles: []string{"a.txt"},
				Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
			}, deps)
			if op != nil {
				t.Fatalf("structured store creation failure returned operation: %+v", op)
			}
			assertErrorCode(t, err, tt.code)
			var structured *apperrors.StructuredError
			if !stderrors.As(err, &structured) || structured != injected {
				t.Fatalf("error = %v, want original structured error %v", err, injected)
			}
			assertDirectoryEmpty(t, output)
			assertDirectoryEmpty(t, tempRoot)
		})
	}
}

func TestPushFileBlobPreservesStructuredStoreError(t *testing.T) {
	tests := []struct {
		name string
		code apperrors.ErrorCode
	}{
		{name: "timeout", code: apperrors.ErrCodeTimeout},
		{name: "conflict", code: apperrors.ErrCodeConflict},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			layout, err := newOwnedLayout(context.Background(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if closeErr := layout.Close(); closeErr != nil {
					t.Errorf("layout Close() error = %v", closeErr)
				}
			})
			const archiveName = "bundle.tar.gz"
			archive := []byte("archive")
			if writeErr := layout.child.WriteFile(archiveName, archive, 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			descriptor := content.NewDescriptorFromBytes(ociv1.MediaTypeImageLayerGzip, archive)
			injected := apperrors.New(tt.code, "structured local store push failure")
			store := &plainFailureLocalStore{pushErr: injected}

			err = pushFileBlob(context.Background(), store, layout, descriptor, archiveName)
			assertErrorCode(t, err, tt.code)
			var structured *apperrors.StructuredError
			if !stderrors.As(err, &structured) || structured != injected {
				t.Fatalf("error = %v, want original structured error %v", err, injected)
			}
		})
	}
}

func TestPackageOperationPreparedCleanupFailureReturnsNoOwnedResult(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("alpha"), 0o640); err != nil {
		t.Fatal(err)
	}
	tempRoot := safeOCITempRoot(t)
	injected := stderrors.New("prepared workspace cleanup failed")
	deps := defaultGenericPackageDependencies()
	deps.prepareSource = func(
		ctx context.Context,
		sourceDir, outputDir, subDir string,
		sourceFiles []string,
	) (*preparedSource, error) {

		prepared, err := preparePackageSource(ctx, sourceDir, outputDir, subDir, sourceFiles)
		if err != nil {
			return nil, err
		}
		prepared.workspace.deps.removeAll = func(*os.Root, string) error { return injected }
		return prepared, nil
	}

	op, err := packageGenericOperationWithDependencies(context.Background(), PackageOptions{
		SourceDir: source, OutputDir: output, SourceFiles: []string{"a.txt"},
		Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
	}, deps)
	assertErrorCode(t, err, apperrors.ErrCodeInternal)
	if !stderrors.Is(err, injected) {
		t.Fatalf("error = %v, want cause %v", err, injected)
	}
	if op != nil {
		t.Fatalf("cleanup failure returned inaccessible operation: %+v", op)
	}
	assertDirectoryEmpty(t, output)
	if entries, readErr := os.ReadDir(tempRoot); readErr != nil {
		t.Fatal(readErr)
	} else if len(entries) != 1 {
		t.Fatalf("temp entries = %v, want only intentionally unremovable workspace", entries)
	}
}

func TestPackageOperationRevalidatesCallerRootsBeforeLayoutMutation(t *testing.T) {
	for _, swapped := range []string{"source", "output"} {
		t.Run(swapped, func(t *testing.T) {
			parent := t.TempDir()
			source := filepath.Join(parent, "source")
			output := filepath.Join(parent, "output")
			if err := os.Mkdir(source, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(output, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("alpha"), 0o600); err != nil {
				t.Fatal(err)
			}
			safeOCITempRoot(t)
			deps := defaultGenericPackageDependencies()
			deps.prepareSource = func(
				ctx context.Context,
				sourceDir, outputDir, subDir string,
				files []string,
			) (*preparedSource, error) {

				prepared, err := preparePackageSource(ctx, sourceDir, outputDir, subDir, files)
				if err != nil {
					return nil, err
				}
				target := source
				if swapped == "output" {
					target = output
				}
				if err := os.Rename(target, target+"-moved"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(target, "sentinel"), []byte("replacement"), 0o600); err != nil {
					t.Fatal(err)
				}
				return prepared, nil
			}
			op, err := packageGenericOperationWithDependencies(context.Background(), PackageOptions{
				SourceDir: source, OutputDir: output, SourceFiles: []string{"a.txt"},
				Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
			}, deps)
			if op != nil {
				t.Fatalf("caller root swap returned operation: %+v", op)
			}
			assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
			target := source
			if swapped == "output" {
				target = output
			}
			if got := directoryEntryNames(t, target); !reflect.DeepEqual(got, []string{"sentinel"}) {
				t.Fatalf("replacement mutated: %v", got)
			}
		})
	}
}

func TestPackageOperationValidatesLayoutBeforeStoreOpen(t *testing.T) {
	for _, swapped := range []string{"child", "parent"} {
		t.Run(swapped, func(t *testing.T) {
			source := t.TempDir()
			output := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("alpha"), 0o600); err != nil {
				t.Fatal(err)
			}
			safeOCITempRoot(t)
			deps := defaultGenericPackageDependencies()
			var layout *ownedLayout
			deps.newLayout = func(ctx context.Context, outputDir string) (*ownedLayout, error) {
				created, err := newOwnedLayout(ctx, outputDir)
				layout = created
				return created, err
			}
			var replacement, moved string
			deps.beforeStore = func() error {
				if swapped == "child" {
					moved = layout.Path() + "-moved"
					if err := os.Rename(layout.Path(), moved); err != nil {
						return err
					}
					if err := os.Mkdir(layout.Path(), 0o700); err != nil {
						return err
					}
					replacement = layout.Path()
				} else {
					moved = layout.parentPath + "-moved"
					if err := os.Rename(layout.parentPath, moved); err != nil {
						return err
					}
					if err := os.Mkdir(layout.parentPath, 0o700); err != nil {
						return err
					}
					replacement = filepath.Join(layout.parentPath, layout.childName)
					if err := os.Mkdir(replacement, 0o700); err != nil {
						return err
					}
				}
				return os.WriteFile(filepath.Join(replacement, "sentinel"), []byte("replacement"), 0o600)
			}
			var storeCalled atomic.Bool
			deps.newStore = func(ctx context.Context, owned *ownedLayout) (localOCIStore, error) {
				storeCalled.Store(true)
				return newRootOCIStore(ctx, owned)
			}
			op, err := packageGenericOperationWithDependencies(context.Background(), PackageOptions{
				SourceDir: source, OutputDir: output, SourceFiles: []string{"a.txt"},
				Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
			}, deps)
			if op != nil {
				t.Fatalf("swapped layout returned operation: %+v", op)
			}
			assertErrorCode(t, err, apperrors.ErrCodeInternal)
			if storeCalled.Load() {
				t.Fatal("OCI store opened after retained layout swap")
			}
			if got := directoryEntryNames(t, replacement); !reflect.DeepEqual(got, []string{"sentinel"}) {
				t.Fatalf("replacement mutated: %v", got)
			}
			if removeErr := os.RemoveAll(replacement); removeErr != nil {
				t.Error(removeErr)
			}
			if removeErr := os.RemoveAll(moved); removeErr != nil {
				t.Error(removeErr)
			}
		})
	}
}

func TestPackageOperationValidatesLayoutAfterStoreOpen(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("alpha"), 0o600); err != nil {
		t.Fatal(err)
	}
	safeOCITempRoot(t)
	deps := defaultGenericPackageDependencies()
	var layout *ownedLayout
	deps.newLayout = func(ctx context.Context, outputDir string) (*ownedLayout, error) {
		created, err := newOwnedLayout(ctx, outputDir)
		layout = created
		return created, err
	}
	var replacement, moved string
	deps.afterStore = func() error {
		moved = layout.Path() + "-moved"
		if err := os.Rename(layout.Path(), moved); err != nil {
			return err
		}
		if err := os.Mkdir(layout.Path(), 0o700); err != nil {
			return err
		}
		replacement = layout.Path()
		return os.WriteFile(filepath.Join(replacement, "sentinel"), []byte("replacement"), 0o600)
	}
	op, err := packageGenericOperationWithDependencies(context.Background(), PackageOptions{
		SourceDir: source, OutputDir: output, SourceFiles: []string{"a.txt"},
		Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
	}, deps)
	if op != nil {
		t.Fatalf("post-open layout swap returned operation: %+v", op)
	}
	assertErrorCode(t, err, apperrors.ErrCodeInternal)
	if got := directoryEntryNames(t, replacement); !reflect.DeepEqual(got, []string{"sentinel"}) {
		t.Fatalf("replacement mutated: %v", got)
	}
	if removeErr := os.RemoveAll(replacement); removeErr != nil {
		t.Error(removeErr)
	}
	if removeErr := os.RemoveAll(moved); removeErr != nil {
		t.Error(removeErr)
	}
}

func TestPackageAndPushOwnershipCleanupAndRelease(t *testing.T) {
	for _, tt := range []struct {
		name    string
		pushErr error
	}{
		{name: "remote failure removes owned layout", pushErr: apperrors.New(apperrors.ErrCodeUnavailable, "remote failed")},
		{name: "remote success releases only after push"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			source := t.TempDir()
			output := t.TempDir()
			if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("alpha"), 0o640); err != nil {
				t.Fatal(err)
			}
			safeOCITempRoot(t)
			deps := defaultPackageAndPushDependencies()
			var pushedPath string
			deps.pushOperation = func(_ context.Context, op *packageOperation, _ PushOptions) (*PushResult, error) {
				pushedPath = op.layout.Path()
				if _, err := os.Stat(pushedPath); err != nil {
					t.Fatalf("layout not retained through push: %v", err)
				}
				if tt.pushErr != nil {
					return nil, tt.pushErr
				}
				return &PushResult{
					Digest: op.manifest.Digest.String(), MediaType: op.manifest.MediaType,
					Size: op.manifest.Size, Reference: "ghcr.io/test/repo:v1",
				}, nil
			}
			result, err := packageAndPushWithDependencies(context.Background(), OutputConfig{
				SourceDir: source, OutputDir: output, SourceFiles: []string{"a.txt"},
				Reference: &Reference{IsOCI: true, Registry: "ghcr.io", Repository: "test/repo", Tag: "v1"},
			}, deps)
			if tt.pushErr != nil {
				if !stderrors.Is(err, tt.pushErr) {
					t.Fatalf("error = %v, want %v", err, tt.pushErr)
				}
				if result != nil {
					t.Fatalf("result = %+v on error", result)
				}
				if _, statErr := os.Lstat(pushedPath); !os.IsNotExist(statErr) {
					t.Fatalf("failed push left layout: %v", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("packageAndPushWithDependencies() error = %v", err)
			}
			if result.StorePath != pushedPath {
				t.Fatalf("StorePath = %q, want %q", result.StorePath, pushedPath)
			}
			if _, statErr := os.Stat(pushedPath); statErr != nil {
				t.Fatalf("released layout missing: %v", statErr)
			}
		})
	}
}

func TestPackageAndPushPathSwapStopsBeforeRegistryIO(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("alpha"), 0o640); err != nil {
		t.Fatal(err)
	}
	safeOCITempRoot(t)
	deps := defaultPackageAndPushDependencies()
	var replacement string
	deps.beforePush = func(op *packageOperation) error {
		original := op.layout.Path()
		if err := os.Rename(original, original+"-moved"); err != nil {
			return err
		}
		if err := os.Mkdir(original, 0o700); err != nil {
			return err
		}
		replacement = original
		return os.WriteFile(filepath.Join(original, "sentinel"), []byte("replacement"), 0o600)
	}
	var registryCalled atomic.Bool
	deps.pushOperation = func(context.Context, *packageOperation, PushOptions) (*PushResult, error) {
		registryCalled.Store(true)
		return nil, stderrors.New("unexpected registry call")
	}
	result, err := packageAndPushWithDependencies(context.Background(), OutputConfig{
		SourceDir: source, OutputDir: output, SourceFiles: []string{"a.txt"},
		Reference: &Reference{IsOCI: true, Registry: "ghcr.io", Repository: "test/repo", Tag: "v1"},
	}, deps)
	assertErrorCode(t, err, apperrors.ErrCodeInternal)
	if result != nil {
		t.Fatalf("result = %+v on path swap", result)
	}
	if registryCalled.Load() {
		t.Fatal("registry called after package-to-push path swap")
	}
	data, readErr := os.ReadFile(filepath.Join(replacement, "sentinel"))
	if readErr != nil || string(data) != "replacement" {
		t.Fatalf("replacement changed: data=%q err=%v", data, readErr)
	}
}

func TestPackageAndPushDigestCopyGraphAndTagRetry(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("alpha"), 0o640); err != nil {
		t.Fatal(err)
	}
	safeOCITempRoot(t)
	op, err := packageGenericOperation(context.Background(), PackageOptions{
		SourceDir: source, OutputDir: output, SourceFiles: []string{"a.txt"},
		Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = op.Close() })

	target := newTestPublicationTarget()
	target.resolve = func(_ context.Context, ref string) (ociv1.Descriptor, error) {
		if ref != op.manifest.Digest.String() {
			t.Fatalf("Resolve reference = %q, want immutable digest %q", ref, op.manifest.Digest.String())
		}
		return op.manifest, nil
	}
	var tagCalls atomic.Int32
	target.tag = func(_ context.Context, desc ociv1.Descriptor, rawTag string) error {
		if !content.Equal(desc, op.manifest) || rawTag != "v1" {
			t.Fatalf("Tag(%+v, %q), want frozen descriptor and raw tag", desc, rawTag)
		}
		if tagCalls.Add(1) == 1 {
			return fakeNetTimeoutErr{}
		}
		return nil
	}
	deps := defaultPushOperationDependencies()
	deps.newTarget = func(PushOptions) (publicationTarget, error) { return target, nil }
	deps.maxAttempts = 2
	deps.initialBackoff = 0
	deps.perAttemptTimeout = time.Second
	var graphCalls atomic.Int32
	deps.copyGraph = func(_ context.Context, src content.ReadOnlyStorage, dst content.Storage, root ociv1.Descriptor, _ oras.CopyGraphOptions) error {
		if _, ok := any(src).(content.Resolver); ok {
			t.Fatal("copy source exposes resolver")
		}
		if _, ok := any(dst).(content.Tagger); ok {
			t.Fatal("copy destination exposes tagger")
		}
		if !content.Equal(root, op.manifest) {
			t.Fatalf("CopyGraph root = %+v, want %+v", root, op.manifest)
		}
		graphCalls.Add(1)
		return nil
	}
	deps.beforeAttempt = func(attempt int) error {
		// Mutating the local tag/index between attempts must not change authority.
		otherBytes := storedTestManifestBytes("retagged")
		other := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, otherBytes)
		if pushErr := op.store.Push(context.Background(), other, bytes.NewReader(otherBytes)); pushErr != nil && !strings.Contains(pushErr.Error(), "already exists") {
			return pushErr
		}
		return op.store.Tag(context.Background(), other, "v1")
	}
	result, err := pushPackageOperationWithDependencies(context.Background(), op, PushOptions{
		Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
	}, deps)
	if err != nil {
		t.Fatalf("pushPackageOperationWithDependencies() error = %v", err)
	}
	if result.Digest != op.manifest.Digest.String() || result.MediaType != op.manifest.MediaType || result.Size != op.manifest.Size {
		t.Fatalf("PushResult = %+v, want frozen descriptor %+v", result, op.manifest)
	}
	if graphCalls.Load() != 2 || tagCalls.Load() != 2 {
		t.Fatalf("whole-operation retry counts graph=%d tag=%d, want 2/2", graphCalls.Load(), tagCalls.Load())
	}
}

func TestPackageAndPushDigestMismatchStopsTagAndCleans(t *testing.T) {
	source := t.TempDir()
	output := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "a.txt"), []byte("alpha"), 0o640); err != nil {
		t.Fatal(err)
	}
	safeOCITempRoot(t)
	op, err := packageGenericOperation(context.Background(), PackageOptions{
		SourceDir: source, OutputDir: output, SourceFiles: []string{"a.txt"},
		Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	layoutPath := op.layout.Path()
	target := newTestPublicationTarget()
	target.resolve = func(context.Context, string) (ociv1.Descriptor, error) {
		return content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, []byte("mismatch")), nil
	}
	deps := defaultPushOperationDependencies()
	deps.newTarget = func(PushOptions) (publicationTarget, error) { return target, nil }
	deps.maxAttempts = 1
	deps.copyGraph = func(context.Context, content.ReadOnlyStorage, content.Storage, ociv1.Descriptor, oras.CopyGraphOptions) error {
		return nil
	}
	_, err = pushPackageOperationWithDependencies(context.Background(), op, PushOptions{
		Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
	}, deps)
	assertErrorCode(t, err, apperrors.ErrCodeInternal)
	if strings.Contains(strings.Join(target.snapshotOrder(), ","), "Tag") {
		t.Fatal("Tag called after digest mismatch")
	}
	if closeErr := op.Close(); closeErr != nil {
		t.Fatalf("operation cleanup = %v", closeErr)
	}
	if _, statErr := os.Lstat(layoutPath); !os.IsNotExist(statErr) {
		t.Fatalf("layout remains: %v", statErr)
	}
}

func TestPushFromStoreDescriptorValidation(t *testing.T) {
	validDigest := digest.FromBytes([]byte("manifest"))
	for _, tt := range []struct {
		name string
		desc ociv1.Descriptor
	}{
		{name: "zero"},
		{name: "missing media type", desc: ociv1.Descriptor{Digest: validDigest, Size: 8}},
		{name: "malformed digest", desc: ociv1.Descriptor{MediaType: ociv1.MediaTypeImageManifest, Digest: "sha256:nope", Size: 8}},
		{name: "zero size", desc: ociv1.Descriptor{MediaType: ociv1.MediaTypeImageManifest, Digest: validDigest}},
		{name: "unsupported root media type", desc: ociv1.Descriptor{MediaType: "application/octet-stream", Digest: validDigest, Size: 8}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result, err := PushFromStore(context.Background(), t.TempDir(), tt.desc, PushOptions{
				Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
			})
			assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
			if result != nil {
				t.Fatalf("result = %+v on malformed descriptor", result)
			}
		})
	}
}

func TestPushFromStoreDigestIgnoresMutableLocalTag(t *testing.T) {
	storePath, store, expected := createFrozenTestStore(t)
	otherBytes := storedTestManifestBytes("other")
	other := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, otherBytes)
	if err := store.Push(context.Background(), other, bytes.NewReader(otherBytes)); err != nil {
		t.Fatal(err)
	}
	if err := store.Tag(context.Background(), other, "v1"); err != nil {
		t.Fatal(err)
	}
	target := newTestPublicationTarget()
	target.resolve = func(_ context.Context, ref string) (ociv1.Descriptor, error) {
		if ref != expected.Digest.String() {
			t.Fatalf("Resolve(%q), want %q", ref, expected.Digest.String())
		}
		return expected, nil
	}
	deps := defaultPushOperationDependencies()
	deps.newTarget = func(PushOptions) (publicationTarget, error) { return target, nil }
	deps.maxAttempts = 1
	deps.copyGraph = func(context.Context, content.ReadOnlyStorage, content.Storage, ociv1.Descriptor, oras.CopyGraphOptions) error {
		return nil
	}
	result, err := pushFromStoreWithDependencies(context.Background(), storePath, expected, PushOptions{
		Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
	}, deps)
	if err != nil {
		t.Fatalf("pushFromStoreWithDependencies() error = %v", err)
	}
	if result.Digest != expected.Digest.String() || result.MediaType != expected.MediaType || result.Size != expected.Size {
		t.Fatalf("result = %+v, want expected descriptor %+v", result, expected)
	}
}

func TestPushFromStoreChecksRetainedRootClose(t *testing.T) {
	storePath, _, expected := createFrozenTestStore(t)
	target := newTestPublicationTarget()
	target.resolve = func(context.Context, string) (ociv1.Descriptor, error) { return expected, nil }
	injected := stderrors.New("public store root close failed")
	deps := defaultPushOperationDependencies()
	deps.newTarget = func(PushOptions) (publicationTarget, error) { return target, nil }
	deps.maxAttempts = 1
	deps.copyGraph = func(
		context.Context,
		content.ReadOnlyStorage,
		content.Storage,
		ociv1.Descriptor,
		oras.CopyGraphOptions,
	) error {

		return nil
	}
	deps.closeStoreRoot = func(root *os.Root) error {
		return stderrors.Join(root.Close(), injected)
	}

	result, err := pushFromStoreWithDependencies(context.Background(), storePath, expected, PushOptions{
		Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
	}, deps)
	if result != nil {
		t.Fatalf("root close failure returned result: %+v", result)
	}
	assertErrorCode(t, err, apperrors.ErrCodeInternal)
	if !stderrors.Is(err, injected) {
		t.Fatalf("error = %v, want %v", err, injected)
	}
}

func TestPushFromStoreValidatesPathAroundStoreOpen(t *testing.T) {
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			storePath, _, expected := createFrozenTestStore(t)
			moved := storePath + "-moved"
			var replacement string
			swap := func() error {
				if err := os.Rename(storePath, moved); err != nil {
					return err
				}
				if err := os.Mkdir(storePath, 0o700); err != nil {
					return err
				}
				replacement = storePath
				return os.WriteFile(filepath.Join(storePath, "sentinel"), []byte("replacement"), 0o600)
			}
			deps := defaultPushOperationDependencies()
			if phase == "before" {
				deps.beforeStoreOpen = swap
			} else {
				deps.afterStoreOpen = swap
			}
			result, err := pushFromStoreWithDependencies(context.Background(), storePath, expected, PushOptions{
				Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
			}, deps)
			if result != nil {
				t.Fatalf("swapped store returned result: %+v", result)
			}
			assertErrorCode(t, err, apperrors.ErrCodeInvalidRequest)
			if got := directoryEntryNames(t, replacement); !reflect.DeepEqual(got, []string{"sentinel"}) {
				t.Fatalf("replacement mutated: %v", got)
			}
			if removeErr := os.RemoveAll(replacement); removeErr != nil {
				t.Error(removeErr)
			}
			if removeErr := os.RemoveAll(moved); removeErr != nil {
				t.Error(removeErr)
			}
		})
	}
}

func TestPushFromStorePathSwapDuringFetchReadsRetainedRoot(t *testing.T) {
	storePath, _, expected := createFrozenTestStore(t)
	moved := storePath + "-moved"
	replacementBytes := bytes.Repeat([]byte("x"), int(expected.Size))
	var swapOnce sync.Once
	var swapErr error
	swap := func() {
		swapOnce.Do(func() {
			if err := os.Rename(storePath, moved); err != nil {
				swapErr = err
				return
			}
			blobDir := filepath.Join(storePath, "blobs", expected.Digest.Algorithm().String())
			if err := os.MkdirAll(blobDir, 0o700); err != nil {
				swapErr = err
				return
			}
			swapErr = os.WriteFile(
				filepath.Join(blobDir, expected.Digest.Encoded()), replacementBytes, 0o600)
		})
	}

	target := newTestPublicationTarget()
	target.resolve = func(context.Context, string) (ociv1.Descriptor, error) { return expected, nil }
	deps := defaultPushOperationDependencies()
	deps.newTarget = func(PushOptions) (publicationTarget, error) { return target, nil }
	deps.maxAttempts = 1
	deps.source = func(source content.ReadOnlyStorage) content.ReadOnlyStorage {
		return &testReadOnlyStorage{
			exists: source.Exists,
			fetch: func(ctx context.Context, desc ociv1.Descriptor) (io.ReadCloser, error) {
				swap()
				if swapErr != nil {
					return nil, swapErr
				}
				return source.Fetch(ctx, desc)
			},
		}
	}
	deps.copyGraph = func(
		context.Context,
		content.ReadOnlyStorage,
		content.Storage,
		ociv1.Descriptor,
		oras.CopyGraphOptions,
	) error {

		return nil
	}

	result, err := pushFromStoreWithDependencies(context.Background(), storePath, expected, PushOptions{
		Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
	}, deps)
	if err != nil {
		t.Fatalf("PushFromStore() error = %v", err)
	}
	if result == nil || result.Digest != expected.Digest.String() {
		t.Fatalf("PushFromStore() result = %+v, want retained-root descriptor", result)
	}
	visibleBlob, err := os.ReadFile(filepath.Join(
		storePath, "blobs", expected.Digest.Algorithm().String(), expected.Digest.Encoded()))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(visibleBlob, replacementBytes) {
		t.Fatalf("replacement blob mutated: %q", visibleBlob)
	}
	if err := os.RemoveAll(storePath); err != nil {
		t.Error(err)
	}
	if err := os.RemoveAll(moved); err != nil {
		t.Error(err)
	}
}

func TestPushFromStoreLocalRootRequiredEvenWhenRemoteAlreadyHasRoot(t *testing.T) {
	for _, tt := range []struct {
		name    string
		corrupt bool
	}{
		{name: "missing"},
		{name: "corrupt", corrupt: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			storePath := t.TempDir()
			store, err := oci.NewWithContext(context.Background(), storePath)
			if err != nil {
				t.Fatal(err)
			}
			expectedBytes := storedTestManifestBytes("expected")
			expected := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, expectedBytes)
			if tt.corrupt {
				if err := store.Push(context.Background(), expected, bytes.NewReader(expectedBytes)); err != nil {
					t.Fatal(err)
				}
				blobPath := filepath.Join(storePath, "blobs", "sha256", expected.Digest.Encoded())
				if err := os.Chmod(blobPath, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(blobPath, []byte("corrupt"), 0o400); err != nil {
					t.Fatal(err)
				}
			}
			target := newTestPublicationTarget()
			target.blobs[expected.Digest] = append([]byte(nil), expectedBytes...)
			target.descriptors[expected.Digest] = expected
			deps := defaultPushOperationDependencies()
			deps.newTarget = func(PushOptions) (publicationTarget, error) { return target, nil }
			var copyCalled atomic.Bool
			deps.copyGraph = func(context.Context, content.ReadOnlyStorage, content.Storage, ociv1.Descriptor, oras.CopyGraphOptions) error {
				copyCalled.Store(true)
				return nil
			}
			result, pushErr := pushFromStoreWithDependencies(context.Background(), storePath, expected, PushOptions{
				Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
			}, deps)
			assertErrorCode(t, pushErr, apperrors.ErrCodeInvalidRequest)
			if result != nil {
				t.Fatalf("result = %+v on missing/corrupt root", result)
			}
			if copyCalled.Load() || target.calls.Load() != 0 {
				t.Fatalf("remote presence bypassed local verification: copy=%v targetCalls=%d", copyCalled.Load(), target.calls.Load())
			}
		})
	}
}

func TestPackageAndPushContextStorageAndPushFromStoreContextStorage(t *testing.T) {
	rootBytes := storedTestManifestBytes("context-root")
	configBytes := []byte("config")
	root := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, rootBytes)
	config := content.NewDescriptorFromBytes(ociv1.MediaTypeImageConfig, configBytes)
	for _, tt := range []struct {
		name    string
		prepare func(*testing.T) func(context.Context, pushOperationDependencies) error
	}{
		{
			name: "generated operation",
			prepare: func(t *testing.T) func(context.Context, pushOperationDependencies) error {
				source := t.TempDir()
				output := t.TempDir()
				if err := os.WriteFile(filepath.Join(source, "a"), []byte("a"), 0o600); err != nil {
					t.Fatal(err)
				}
				safeOCITempRoot(t)
				op, err := packageGenericOperation(context.Background(), PackageOptions{
					SourceDir: source, OutputDir: output, SourceFiles: []string{"a"},
					Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if closeErr := op.Close(); closeErr != nil {
						t.Errorf("package operation Close() error = %v", closeErr)
					}
				})
				op.manifest = root
				return func(ctx context.Context, deps pushOperationDependencies) error {
					result, pushErr := pushPackageOperationWithDependencies(ctx, op, PushOptions{
						Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
					}, deps)
					if result != nil {
						return stderrors.New("unexpected result")
					}
					return pushErr
				}
			},
		},
		{
			name: "public store",
			prepare: func(t *testing.T) func(context.Context, pushOperationDependencies) error {
				storePath, _, _ := createFrozenTestStore(t)
				return func(ctx context.Context, deps pushOperationDependencies) error {
					result, pushErr := pushFromStoreWithDependencies(ctx, storePath, root, PushOptions{
						Registry: "ghcr.io", Repository: "test/repo", Tag: "v1",
					}, deps)
					if result != nil {
						return stderrors.New("unexpected result")
					}
					return pushErr
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			run := tt.prepare(t)
			ctx, cancel := context.WithCancel(context.Background())
			blocking := newCloseUnblockedReader(nil)
			fakeSource := &testReadOnlyStorage{
				fetch: func(_ context.Context, desc ociv1.Descriptor) (io.ReadCloser, error) {
					if desc.Digest == root.Digest {
						return io.NopCloser(bytes.NewReader(rootBytes)), nil
					}
					return blocking, nil
				},
			}
			target := newTestPublicationTarget()
			deps := defaultPushOperationDependencies()
			deps.source = func(content.ReadOnlyStorage) content.ReadOnlyStorage { return fakeSource }
			deps.newTarget = func(PushOptions) (publicationTarget, error) { return target, nil }
			deps.maxAttempts = 1
			deps.copyGraph = func(_ context.Context, src content.ReadOnlyStorage, _ content.Storage, _ ociv1.Descriptor, _ oras.CopyGraphOptions) error {
				reader, err := src.Fetch(ctx, config)
				if err != nil {
					return err
				}
				_, err = reader.Read(make([]byte, 1))
				return err
			}
			done := make(chan error, 1)
			go func() { done <- run(ctx, deps) }()
			<-blocking.started
			cancel()
			err := <-done
			assertErrorCode(t, err, apperrors.ErrCodeTimeout)
			select {
			case <-blocking.closed:
			default:
				t.Fatal("attempt cancellation did not close blocked reader")
			}
			if target.calls.Load() != 0 {
				t.Fatalf("destination/resolve/tag called after source cancellation: %v", target.snapshotOrder())
			}
		})
	}
}

func createFrozenTestStore(t *testing.T) (string, *oci.Store, ociv1.Descriptor) {
	t.Helper()
	storePath := t.TempDir()
	store, err := oci.NewWithContext(context.Background(), storePath)
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes := storedTestManifestBytes("frozen")
	desc := content.NewDescriptorFromBytes(ociv1.MediaTypeImageManifest, manifestBytes)
	if err := store.Push(context.Background(), desc, bytes.NewReader(manifestBytes)); err != nil {
		t.Fatal(err)
	}
	if err := store.Tag(context.Background(), desc, "v1"); err != nil {
		t.Fatal(err)
	}
	return storePath, store, desc
}
