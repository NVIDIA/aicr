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

// Package oci provides utilities for packaging and pushing OCI artifacts.
package oci

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/distribution/reference"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"

	"github.com/NVIDIA/aicr/pkg/defaults"
	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

const (
	// ArtifactType is the OCI media type for AICR bundle artifacts.
	//
	// Artifacts with this type package a directory tree into an OCI artifact using ORAS.
	// The artifact contains standard OCI layout (manifest, config, layers) but is not
	// a runnable container image - it's an opaque bundle of files.
	//
	// Use cases: distributing AICR bundles (configs, assets) via OCI registries.
	// Consumers that don't understand this type should treat it as a non-executable blob.
	ArtifactType = "application/vnd.nvidia.aicr.artifact"

	// Default timestamp for reproducible builds.
	// Use a fixed date (Unix epoch) to ensure builds are deterministic.
	ReproducibleTimestamp = "1970-01-01T00:00:00Z"
)

// registryHostPattern validates registry host format (host:port or host).
var registryHostPattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*(:[0-9]+)?$`)

// repositoryPattern validates repository path format.
var repositoryPattern = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)

// PackageOptions configures local OCI packaging.
type PackageOptions struct {
	// SourceDir is the directory containing artifacts to package.
	SourceDir string
	// OutputDir is where the OCI Image Layout will be created.
	OutputDir string
	// Registry is the OCI registry host for the reference (e.g., "ghcr.io").
	Registry string
	// Repository is the image repository path (e.g., "nvidia/aicr").
	Repository string
	// Tag is the image tag (e.g., "v1.0.0", "latest").
	Tag string
	// SubDir optionally limits packaging to a subdirectory within SourceDir.
	SubDir string
	// Annotations are additional manifest annotations to include.
	// Standard OCI annotations (org.opencontainers.image.*) are recommended.
	Annotations map[string]string
}

// PackageResult contains the result of local OCI packaging.
type PackageResult struct {
	// Digest is the SHA256 digest of the packaged artifact.
	Digest string
	// Reference is the full image reference (registry/repository:tag).
	Reference string
	// StorePath is the path to the OCI Image Layout directory.
	StorePath string
}

// PushOptions configures the OCI push operation.
type PushOptions struct {
	// SourceDir is the directory containing artifacts to push.
	SourceDir string
	// Registry is the OCI registry host (e.g., "ghcr.io", "localhost:5000").
	Registry string
	// Repository is the image repository path (e.g., "nvidia/aicr").
	Repository string
	// Tag is the image tag (e.g., "v1.0.0", "latest").
	Tag string
	// PlainHTTP uses HTTP instead of HTTPS for the registry connection.
	PlainHTTP bool
	// InsecureTLS skips TLS certificate verification.
	InsecureTLS bool
}

// PushResult contains the result of a successful OCI push.
type PushResult struct {
	// Digest is the SHA256 digest of the pushed artifact.
	Digest string
	// Reference is the full image reference (registry/repository:tag).
	Reference string
}

// ValidateRegistryReference validates the registry and repository format.
func ValidateRegistryReference(registry, repository string) error {
	registryHost := stripProtocol(registry)

	if !registryHostPattern.MatchString(registryHost) {
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid registry host format '%s': must be a valid hostname with optional port", registryHost))
	}

	if !repositoryPattern.MatchString(repository) {
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid repository format '%s': must be lowercase alphanumeric with optional separators (., _, -) and path segments", repository))
	}

	return nil
}

// Package creates a local OCI artifact in OCI Image Layout format.
// This stores the artifact locally without pushing to a remote registry.
func Package(ctx context.Context, opts PackageOptions) (*PackageResult, error) {
	if opts.Tag == "" {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest, "tag is required for OCI packaging")
	}

	if opts.Registry == "" {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest, "registry is required for OCI packaging")
	}

	if opts.Repository == "" {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest, "repository is required for OCI packaging")
	}

	// Validate registry and repository format
	if err := ValidateRegistryReference(opts.Registry, opts.Repository); err != nil {
		return nil, err
	}

	// Determine the directory to package from
	packageFromDir, cleanup, err := preparePushDir(opts.SourceDir, opts.SubDir)
	if err != nil {
		return nil, err
	}
	if cleanup != nil {
		defer cleanup()
	}

	// Check for context cancellation before expensive operations
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeUnavailable, "operation canceled", ctxErr)
	}

	// Convert to absolute path
	absSourceDir, err := filepath.Abs(packageFromDir)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to get absolute path for source dir", err)
	}

	// Strip protocol from registry for docker reference compatibility
	registryHost := stripProtocol(opts.Registry)

	// Build and validate the image reference
	refString := fmt.Sprintf("%s/%s:%s", registryHost, opts.Repository, opts.Tag)
	if _, parseErr := reference.ParseNormalizedNamed(refString); parseErr != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInvalidRequest, fmt.Sprintf("invalid image reference '%s'", refString), parseErr)
	}

	// Create OCI Image Layout store at output directory
	ociStorePath := filepath.Join(opts.OutputDir, "oci-layout")
	if mkdirErr := os.MkdirAll(ociStorePath, 0o755); mkdirErr != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create OCI store directory", mkdirErr)
	}

	ociStore, err := oci.New(ociStorePath)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create OCI store", err)
	}
	// Note: oci.Store doesn't require explicit closing

	// Create a file store to read from source directory
	fs, err := file.New(absSourceDir)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create file store", err)
	}
	defer func() { _ = fs.Close() }()

	// Make tars deterministic for reproducible builds
	fs.TarReproducible = true

	// Check for context cancellation before adding files
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeUnavailable, "operation canceled", ctxErr)
	}

	// Add all contents from the file store root
	layerDesc, err := fs.Add(ctx, ".", ociv1.MediaTypeImageLayerGzip, absSourceDir)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to add source directory to store", err)
	}

	// Pack an OCI 1.1 manifest with our artifact type
	packOpts := oras.PackManifestOptions{
		Layers: []ociv1.Descriptor{layerDesc},
	}

	// Build manifest annotations - always set a fixed timestamp for reproducibility
	packOpts.ManifestAnnotations = make(map[string]string)
	for k, v := range opts.Annotations {
		packOpts.ManifestAnnotations[k] = v
	}

	// Always add consistent creation timestamp to ensure reproducible builds
	packOpts.ManifestAnnotations[ociv1.AnnotationCreated] = ReproducibleTimestamp

	manifestDesc, err := oras.PackManifest(ctx, fs, oras.PackManifestVersion1_1, ArtifactType, packOpts)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to pack manifest", err)
	}

	// Tag the manifest in file store
	if tagErr := fs.Tag(ctx, manifestDesc, opts.Tag); tagErr != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to tag manifest", tagErr)
	}

	// Check for context cancellation before copy operation
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeUnavailable, "operation canceled", ctxErr)
	}

	// Copy from file store to OCI layout store
	desc, err := oras.Copy(ctx, fs, opts.Tag, ociStore, opts.Tag, oras.DefaultCopyOptions)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to copy to OCI store", err)
	}

	return &PackageResult{
		Digest:    desc.Digest.String(),
		Reference: refString,
		StorePath: ociStorePath,
	}, nil
}

// PushFromStore pushes an already-packaged OCI artifact from a local OCI store to a remote registry.
//
//nolint:unparam // PushResult is part of the public API, returned for future callers
func PushFromStore(ctx context.Context, storePath string, opts PushOptions) (*PushResult, error) {
	if opts.Tag == "" {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest, "tag is required to push OCI image")
	}

	// Validate registry and repository format
	if err := ValidateRegistryReference(opts.Registry, opts.Repository); err != nil {
		return nil, err
	}

	// Check for context cancellation
	if err := ctx.Err(); err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeUnavailable, "operation canceled", err)
	}

	// Strip protocol from registry for docker reference compatibility
	registryHost := stripProtocol(opts.Registry)

	// Build the reference string
	refString := fmt.Sprintf("%s/%s:%s", registryHost, opts.Repository, opts.Tag)

	// Open existing OCI store
	ociStore, err := oci.New(storePath)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to open OCI store", err)
	}
	// Note: oci.Store doesn't require explicit closing

	// Prepare remote repository
	repo, err := remote.NewRepository(fmt.Sprintf("%s/%s", registryHost, opts.Repository))
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to initialize remote repository", err)
	}
	repo.PlainHTTP = opts.PlainHTTP

	// Configure auth client using Docker credentials if available
	authClient, err := createAuthClient(opts.PlainHTTP, opts.InsecureTLS)
	if err != nil {
		slog.Warn("failed to initialize Docker credential store, continuing without authentication",
			"error", err)
	}
	repo.Client = authClient

	// Copy from OCI store to remote repository
	desc, err := oras.Copy(ctx, ociStore, opts.Tag, repo, opts.Tag, oras.DefaultCopyOptions)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeUnavailable, "failed to push artifact to registry", err)
	}

	return &PushResult{
		Digest:    desc.Digest.String(),
		Reference: refString,
	}, nil
}

// preparePushDir prepares the directory for pushing.
// If subDir is specified, creates a temp directory with hard links.
// Returns the directory to push from and an optional cleanup function.
func preparePushDir(sourceDir, subDir string) (string, func(), error) {
	if subDir == "" {
		return sourceDir, nil, nil
	}

	// When pushing a subdirectory, preserve its path structure in the image
	// Create a temp dir and use hard links (fast, no extra disk space)
	tempDir, err := os.MkdirTemp("", "oras-push-*")
	if err != nil {
		return "", nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create temp directory", err)
	}

	srcPath := filepath.Join(sourceDir, subDir)
	dstPath := filepath.Join(tempDir, subDir)
	if err := hardLinkDir(srcPath, dstPath); err != nil {
		if removeErr := os.RemoveAll(tempDir); removeErr != nil {
			slog.Warn("failed to cleanup temp directory after error",
				"path", tempDir,
				"error", removeErr)
		}
		return "", nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create hard links", err)
	}

	cleanup := func() {
		if err := os.RemoveAll(tempDir); err != nil {
			slog.Warn("failed to cleanup temp directory",
				"path", tempDir,
				"error", err)
		}
	}
	return tempDir, cleanup, nil
}

// stripProtocol removes http:// or https:// prefix from a registry URL.
func stripProtocol(registry string) string {
	registry = strings.TrimPrefix(registry, "https://")
	registry = strings.TrimPrefix(registry, "http://")
	return registry
}

// createAuthClient creates an HTTP client with optional TLS configuration
// and Docker credential support. Returns an error if credential store
// initialization fails, but the client is still usable without credentials.
func createAuthClient(plainHTTP, insecureTLS bool) (*auth.Client, error) {
	credStore, credErr := credentials.NewStoreFromDocker(credentials.StoreOptions{})

	transport := http.DefaultTransport.(*http.Transport).Clone()
	if !plainHTTP && insecureTLS {
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
		} else {
			transport.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec
		}
	}

	client := &auth.Client{
		Client: &http.Client{Timeout: defaults.HTTPClientTimeout, Transport: transport},
		Cache:  auth.NewCache(),
	}

	// Only set credential function if store was created successfully
	if credErr == nil && credStore != nil {
		client.Credential = credentials.Credential(credStore)
	}

	return client, credErr
}

// hardLinkDir recursively creates hard links from src to dst.
// This is much faster than copying and uses no additional disk space.
//
// Note: Hard links may not work on Windows for files on different volumes
// or filesystems that don't support them. This function is primarily
// intended for Linux/container environments.
func hardLinkDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to stat source directory", err)
	}

	if mkdirErr := os.MkdirAll(dst, srcInfo.Mode()); mkdirErr != nil {
		return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create destination directory", mkdirErr)
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to read source directory", err)
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := hardLinkDir(srcPath, dstPath); err != nil {
				return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to hard link subdirectory", err)
			}
		} else {
			if err := os.Link(srcPath, dstPath); err != nil {
				return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create hard link", err)
			}
		}
	}

	return nil
}
