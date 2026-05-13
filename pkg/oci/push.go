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

// Package oci provides utilities for packaging and pushing OCI artifacts.
package oci

import (
	"context"
	"crypto/tls"
	stderrors "errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/distribution/reference"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	"oras.land/oras-go/v2/content/oci"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/NVIDIA/aicr/pkg/defaults"
	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

const (
	// artifactType is the OCI media type for AICR bundle artifacts.
	//
	// Artifacts with this type package a directory tree into an OCI artifact using ORAS.
	// The artifact contains standard OCI layout (manifest, config, layers) but is not
	// a runnable container image - it's an opaque bundle of files.
	//
	// Use cases: distributing AICR bundles (configs, assets) via OCI registries.
	// Consumers that don't understand this type should treat it as a non-executable blob.
	artifactType = "application/vnd.nvidia.aicr.artifact"

	// reproducibleTimestamp is the default timestamp for reproducible builds.
	// Use a fixed date (Unix epoch) to ensure builds are deterministic.
	reproducibleTimestamp = "1970-01-01T00:00:00Z"
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
	// MediaType is the manifest media type
	// (typically application/vnd.oci.image.manifest.v1+json).
	MediaType string
	// Size is the manifest's byte length. Surfaced so callers can
	// construct an OCI subject descriptor for the Referrers API
	// without re-fetching the manifest from the registry.
	Size int64
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
	// MediaType is the manifest media type.
	MediaType string
	// Size is the manifest's byte length. Surfaced so the caller can
	// build a subject descriptor for OCI Referrers attachment without
	// re-fetching the manifest.
	Size int64
	// Reference is the full image reference (registry/repository:tag).
	Reference string
}

// validateRegistryReference validates the registry and repository format.
func validateRegistryReference(registry, repository string) error {
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
func Package(ctx context.Context, opts PackageOptions) (retResult *PackageResult, retErr error) {
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
	if err := validateRegistryReference(opts.Registry, opts.Repository); err != nil {
		return nil, err
	}

	// Determine the directory to package from
	packageFromDir, cleanup, err := preparePushDir(ctx, opts.SourceDir, opts.SubDir)
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
	defer func() {
		// File store close may flush state; surface as a wrapped error.
		if closeErr := fs.Close(); closeErr != nil && retErr == nil {
			retErr = apperrors.Wrap(apperrors.ErrCodeInternal, "failed to close file store", closeErr)
		}
	}()

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
	packOpts.ManifestAnnotations[ociv1.AnnotationCreated] = reproducibleTimestamp

	manifestDesc, err := oras.PackManifest(ctx, fs, oras.PackManifestVersion1_1, artifactType, packOpts)
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

	// Copy from file store to OCI layout store. This is a local-only copy
	// (no network), but still bounded so a wedged store can't hang forever.
	copyOpts := oras.DefaultCopyOptions
	copyOpts.Concurrency = defaults.OCIPushConcurrency

	pushCtx, pushCancel := context.WithTimeout(ctx, defaults.RegistryPushTimeout)
	defer pushCancel()
	desc, err := oras.Copy(pushCtx, fs, opts.Tag, ociStore, opts.Tag, copyOpts)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to copy to OCI store", err)
	}

	return &PackageResult{
		Digest:    desc.Digest.String(),
		MediaType: desc.MediaType,
		Size:      desc.Size,
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
	if err := validateRegistryReference(opts.Registry, opts.Repository); err != nil {
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
	authClient, err := createAuthClientForHost(registryHost, opts.PlainHTTP, opts.InsecureTLS)
	if err != nil {
		slog.Warn("failed to initialize Docker credential store, continuing without authentication",
			"error", err)
	}
	repo.Client = authClient

	// Copy from OCI store to remote repository, bounded by a per-attempt
	// timeout and wrapped in a small retry policy for transient failures.
	copyOpts := oras.DefaultCopyOptions
	copyOpts.Concurrency = defaults.OCIPushConcurrency

	desc, err := copyWithRetry(ctx, ociStore, opts.Tag, repo, opts.Tag, copyOpts, oras.Copy)
	if err != nil {
		return nil, err
	}

	return &PushResult{
		Digest:    desc.Digest.String(),
		MediaType: desc.MediaType,
		Size:      desc.Size,
		Reference: refString,
	}, nil
}

// ReferrerOptions configures a single-blob OCI manifest attached via
// the OCI 1.1 Referrers API. Used by Sigstore Bundle attachment and
// similar "annotation manifest" patterns where the *referring* manifest
// is the artifact and the subject points at what it refers to.
type ReferrerOptions struct {
	// Registry is the OCI registry host (e.g., "ghcr.io").
	Registry string
	// Repository is the same repository the subject artifact lives in.
	Repository string
	// PlainHTTP forces HTTP (used for local registry tests).
	PlainHTTP bool
	// InsecureTLS disables TLS verification for self-signed registries.
	InsecureTLS bool

	// ArtifactType identifies the referrer manifest's purpose, e.g.
	// "application/vnd.dev.sigstore.bundle.v0.3+json". The same value
	// is used as the layer media type so a referrer with one blob is
	// self-describing.
	ArtifactType string
	// LayerContent is the single blob the referrer wraps.
	LayerContent []byte

	// Subject is the descriptor of the artifact this referrer points
	// at. cosign's /v2/<name>/referrers/<digest> discovery uses
	// Subject.Digest to match.
	Subject ociv1.Descriptor

	// Annotations apply to the referrer manifest.
	Annotations map[string]string
}

// PushReferrer pushes a single-layer OCI manifest with a Subject set,
// attaching it as a Referrer of the subject artifact. cosign discovers
// signatures attached this way via the OCI Distribution 1.1 Referrers
// API. The tag is derived from the referrer manifest digest so multiple
// referrers can coexist without colliding on a fixed tag.
func PushReferrer(ctx context.Context, opts ReferrerOptions) (*PushResult, error) {
	if err := validateRegistryReference(opts.Registry, opts.Repository); err != nil {
		return nil, err
	}
	fs, tmpDir, tag, err := packReferrer(ctx, opts)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = fs.Close()
		if rmErr := os.RemoveAll(tmpDir); rmErr != nil {
			slog.Warn("failed to remove referrer temp dir", "path", tmpDir, "error", rmErr)
		}
	}()

	registryHost := stripProtocol(opts.Registry)
	repo, err := remote.NewRepository(fmt.Sprintf("%s/%s", registryHost, opts.Repository))
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to initialize remote repository", err)
	}
	repo.PlainHTTP = opts.PlainHTTP

	authClient, err := createAuthClientForHost(registryHost, opts.PlainHTTP, opts.InsecureTLS)
	if err != nil {
		slog.Warn("failed to initialize credential store; continuing without auth", "error", err)
	}
	repo.Client = authClient

	copyOpts := oras.DefaultCopyOptions
	copyOpts.Concurrency = defaults.OCIPushConcurrency
	desc, err := copyWithRetry(ctx, fs, tag, repo, tag, copyOpts, oras.Copy)
	if err != nil {
		return nil, err
	}

	return &PushResult{
		Digest:    desc.Digest.String(),
		MediaType: desc.MediaType,
		Size:      desc.Size,
		Reference: fmt.Sprintf("%s/%s@%s", registryHost, opts.Repository, desc.Digest.String()),
	}, nil
}

// packReferrer builds the referrer manifest in a local file store and
// returns the store, temp dir path, and the digest-derived tag. The
// caller defers closing fs and removing tmpDir.
func packReferrer(ctx context.Context, opts ReferrerOptions) (*file.Store, string, string, error) {
	if opts.ArtifactType == "" {
		return nil, "", "", apperrors.New(apperrors.ErrCodeInvalidRequest, "ArtifactType is required")
	}
	if len(opts.LayerContent) == 0 {
		return nil, "", "", apperrors.New(apperrors.ErrCodeInvalidRequest, "LayerContent must be non-empty")
	}
	if opts.Subject.Digest == "" {
		return nil, "", "", apperrors.New(apperrors.ErrCodeInvalidRequest, "Subject.Digest is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, "", "", apperrors.Wrap(apperrors.ErrCodeUnavailable, "operation canceled", err)
	}

	tmpDir, err := os.MkdirTemp("", "oras-referrer-*")
	if err != nil {
		return nil, "", "", apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create temp dir", err)
	}

	const layerFilename = "payload"
	layerPath := filepath.Join(tmpDir, layerFilename)
	if writeErr := os.WriteFile(layerPath, opts.LayerContent, 0o600); writeErr != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, "", "", apperrors.Wrap(apperrors.ErrCodeInternal, "failed to stage referrer layer", writeErr)
	}

	fs, err := file.New(tmpDir)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return nil, "", "", apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create referrer file store", err)
	}
	fs.TarReproducible = true

	layerDesc, err := fs.Add(ctx, layerFilename, opts.ArtifactType, layerPath)
	if err != nil {
		_ = fs.Close()
		_ = os.RemoveAll(tmpDir)
		return nil, "", "", apperrors.Wrap(apperrors.ErrCodeInternal, "failed to add referrer layer", err)
	}

	subject := opts.Subject
	packOpts := oras.PackManifestOptions{
		Layers:  []ociv1.Descriptor{layerDesc},
		Subject: &subject,
	}
	packOpts.ManifestAnnotations = make(map[string]string, len(opts.Annotations)+1)
	for k, v := range opts.Annotations {
		packOpts.ManifestAnnotations[k] = v
	}
	packOpts.ManifestAnnotations[ociv1.AnnotationCreated] = reproducibleTimestamp

	manifestDesc, err := oras.PackManifest(ctx, fs, oras.PackManifestVersion1_1, opts.ArtifactType, packOpts)
	if err != nil {
		_ = fs.Close()
		_ = os.RemoveAll(tmpDir)
		return nil, "", "", apperrors.Wrap(apperrors.ErrCodeInternal, "failed to pack referrer manifest", err)
	}

	tag := strings.TrimPrefix(manifestDesc.Digest.String(), "sha256:")
	if tagErr := fs.Tag(ctx, manifestDesc, tag); tagErr != nil {
		_ = fs.Close()
		_ = os.RemoveAll(tmpDir)
		return nil, "", "", apperrors.Wrap(apperrors.ErrCodeInternal, "failed to tag referrer manifest", tagErr)
	}

	return fs, tmpDir, tag, nil
}

// copyFunc matches the signature of oras.Copy and is injected into
// copyWithRetry so tests can stub network behavior without a registry.
type copyFunc func(ctx context.Context, src oras.ReadOnlyTarget, srcRef string, dst oras.Target, dstRef string, opts oras.CopyOptions) (ociv1.Descriptor, error)

// copyWithRetry wraps a copy call with a per-attempt timeout, bounded
// retries, and exponential backoff with +/-25% jitter. Only transient
// errors are retried; context.Canceled and 4xx-class registry responses
// fail fast.
func copyWithRetry(ctx context.Context, src oras.ReadOnlyTarget, srcRef string, dst oras.Target, dstRef string, opts oras.CopyOptions, copy copyFunc) (ociv1.Descriptor, error) {
	return copyWithRetryConfig(ctx, src, srcRef, dst, dstRef, opts, copy,
		defaults.RegistryPushRetries, defaults.RegistryPushBackoff, defaults.RegistryPushTimeout)
}

// copyWithRetryConfig is the underlying retry implementation, parameterized
// for testability. Production callers should use copyWithRetry which
// supplies the defaults.
func copyWithRetryConfig(ctx context.Context, src oras.ReadOnlyTarget, srcRef string, dst oras.Target, dstRef string, opts oras.CopyOptions, copy copyFunc, maxAttempts int, initialBackoff, perAttemptTimeout time.Duration) (ociv1.Descriptor, error) {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	var (
		desc    ociv1.Descriptor
		lastErr error
	)
	backoff := initialBackoff
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ociv1.Descriptor{}, apperrors.Wrap(apperrors.ErrCodeUnavailable, "operation canceled before push", ctxErr)
		}

		pushCtx, pushCancel := context.WithTimeout(ctx, perAttemptTimeout)
		desc, lastErr = copy(pushCtx, src, srcRef, dst, dstRef, opts)
		pushCancel()
		if lastErr == nil {
			return desc, nil
		}

		// Don't retry if the parent context was canceled or for non-transient
		// errors (e.g., 4xx auth/validation failures from the registry).
		if stderrors.Is(lastErr, context.Canceled) || stderrors.Is(ctx.Err(), context.Canceled) {
			return ociv1.Descriptor{}, apperrors.Wrap(apperrors.ErrCodeUnavailable, "registry push canceled", lastErr)
		}
		if !isTransientPushError(lastErr) {
			return ociv1.Descriptor{}, apperrors.Wrap(apperrors.ErrCodeUnavailable, "registry push failed", lastErr)
		}

		if attempt == maxAttempts {
			break
		}

		slog.Warn("oci push retry", "attempt", attempt, "error", lastErr)

		// Sleep with backoff, but honor context cancellation.
		sleep := jitterDuration(backoff)
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ociv1.Descriptor{}, apperrors.Wrap(apperrors.ErrCodeUnavailable, "registry push canceled during backoff", ctx.Err())
		case <-timer.C:
		}
		backoff *= 2
	}

	return ociv1.Descriptor{}, apperrors.Wrap(apperrors.ErrCodeUnavailable, "registry push failed after retries", lastErr)
}

// isTransientPushError reports whether err looks like a recoverable
// registry/network failure that warrants a retry.
//
// Transient: per-attempt context.DeadlineExceeded, net.Error with Timeout()
// true, generic network connectivity failures (matched by pkg/errors.IsNetworkError),
// and 5xx / 429 registry responses.
//
// Not transient: context.Canceled (caller asked to stop) and 4xx registry
// responses (auth, not-found, invalid manifest, etc.).
func isTransientPushError(err error) bool {
	if err == nil {
		return false
	}
	if stderrors.Is(err, context.Canceled) {
		return false
	}

	// Per-attempt deadline expired — registry is slow but the caller's parent
	// context still has budget. Worth another attempt.
	if stderrors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// Typed network timeouts (e.g., TLS handshake, response header) usually
	// satisfy net.Error.Timeout().
	var netErr net.Error
	if stderrors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	// HTTP responses surfaced through oras-go's errcode.ErrorResponse.
	// Retry only on 5xx and 429; 4xx are caller errors.
	var respErr *errcode.ErrorResponse
	if stderrors.As(err, &respErr) {
		switch {
		case respErr.StatusCode >= 500 && respErr.StatusCode <= 599:
			return true
		case respErr.StatusCode == http.StatusTooManyRequests:
			return true
		default:
			return false
		}
	}

	// Generic network-level errors (DNS, dial, connection refused, etc.).
	return apperrors.IsNetworkError(err)
}

// jitterDuration applies +/-25% jitter to d.
func jitterDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	// Range: [0.75*d, 1.25*d). rand.Float64 is in [0.0, 1.0).
	// math/rand/v2 is appropriate here: jitter is a backoff scheduler input,
	// not a security-sensitive value.
	jitter := 0.75 + rand.Float64()*0.5 //nolint:gosec // non-cryptographic jitter
	return time.Duration(float64(d) * jitter)
}

// preparePushDir prepares the directory for pushing by hard-linking the
// source tree into a temp directory. Always uses a temp dir — never the
// caller's source directory directly — so the oras file store, which
// can write manifest blobs into its root, never leaves stray files in
// user space. (The org.opencontainers.image.title annotation has been
// observed materializing as a literal filename inside the root the
// file store is constructed against.)
//
// When subDir is set, the temp dir mirrors the subdir layout so the
// resulting OCI artifact preserves the same path structure.
//
// Returns the directory to push from and a cleanup function (always
// non-nil now that the no-temp-dir shortcut is gone). ctx cancels the
// directory walk so a parent timeout (push timeout, server shutdown)
// terminates staging work without orphaning the temp dir.
func preparePushDir(ctx context.Context, sourceDir, subDir string) (string, func(), error) {
	tempDir, err := os.MkdirTemp("", "oras-push-*")
	if err != nil {
		return "", nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create temp directory", err)
	}

	cleanup := func() {
		if err := os.RemoveAll(tempDir); err != nil {
			slog.Warn("failed to cleanup temp directory",
				"path", tempDir,
				"error", err)
		}
	}

	srcPath := sourceDir
	dstPath := tempDir
	if subDir != "" {
		// Reject non-local paths (absolute, parent-traversing, or
		// containing reserved Windows names) so filepath.Join can't
		// produce a hardlink target outside the temp dir or a source
		// outside the caller's sourceDir.
		if !filepath.IsLocal(subDir) {
			cleanup()
			return "", nil, apperrors.New(apperrors.ErrCodeInvalidRequest,
				"subDir must be a local relative path: "+subDir)
		}
		srcPath = filepath.Join(sourceDir, subDir)
		dstPath = filepath.Join(tempDir, subDir)
	}
	if err := hardLinkDir(ctx, srcPath, dstPath); err != nil {
		// $TMPDIR is often on a different filesystem from sourceDir
		// (tmpfs, overlayfs in containers, NFS-mounted workspaces),
		// in which case os.Link returns EXDEV. Fall back to a full
		// recursive copy so the push still succeeds — the temp dir
		// still gives the oras file store its own root so the
		// "annotation as filename" leak the no-shortcut refactor was
		// meant to prevent is still avoided.
		if stderrors.Is(err, syscall.EXDEV) {
			if copyErr := copyDir(ctx, srcPath, dstPath); copyErr != nil {
				cleanup()
				return "", nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to copy files across filesystems", copyErr)
			}
			return tempDir, cleanup, nil
		}
		cleanup()
		return "", nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create hard links", err)
	}

	return tempDir, cleanup, nil
}

// stripProtocol removes http:// or https:// prefix from a registry URL.
func stripProtocol(registry string) string {
	registry = strings.TrimPrefix(registry, "https://")
	registry = strings.TrimPrefix(registry, "http://")
	return registry
}

// createAuthClientForHost creates an HTTP client with optional TLS
// configuration and Docker credential support. Returns an error if the
// credential store initialization fails, but the client is still usable
// without credentials. The host argument is used only for logging when
// TLS verification is disabled.
func createAuthClientForHost(host string, plainHTTP, insecureTLS bool) (*auth.Client, error) {
	credStore, credErr := credentials.NewStoreFromDocker(credentials.StoreOptions{})

	transport := defaults.NewHTTPTransport()
	if !plainHTTP && insecureTLS {
		slog.Warn("TLS verification disabled for OCI registry", "registry", host)
		// Clone any existing TLS config so future hardening defaults
		// applied in defaults.NewHTTPTransport (e.g., MinVersion, cipher
		// suites) are preserved when toggling InsecureSkipVerify.
		var cfg *tls.Config
		if transport.TLSClientConfig != nil {
			cfg = transport.TLSClientConfig.Clone()
		} else {
			cfg = &tls.Config{} //nolint:gosec // populated below; defaults track NewHTTPTransport
		}
		cfg.InsecureSkipVerify = true //nolint:gosec
		transport.TLSClientConfig = cfg
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
// intended for Linux/container environments. ctx is checked on entry
// and per directory entry so a large tree's walk respects cancellation.
func hardLinkDir(ctx context.Context, src, dst string) error {
	if err := ctx.Err(); err != nil {
		return apperrors.Wrap(apperrors.ErrCodeUnavailable, "hard link walk canceled", err)
	}
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
		if err := ctx.Err(); err != nil {
			return apperrors.Wrap(apperrors.ErrCodeUnavailable, "hard link walk canceled", err)
		}
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := hardLinkDir(ctx, srcPath, dstPath); err != nil {
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

// copyDir is the cross-filesystem fallback for hardLinkDir. Hard links
// require src and dst to share an inode space (same filesystem); when
// $TMPDIR is on a different mount (tmpfs, overlay, NFS), os.Link
// returns EXDEV and we fall back to a full byte copy. Streams via
// io.Copy so multi-GB bundles don't materialize in memory. ctx is
// checked per directory entry; a canceled walk returns
// ErrCodeUnavailable so a parent timeout can short-circuit a large
// fallback copy.
func copyDir(ctx context.Context, src, dst string) error {
	if err := ctx.Err(); err != nil {
		return apperrors.Wrap(apperrors.ErrCodeUnavailable, "copy walk canceled", err)
	}
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
		if err := ctx.Err(); err != nil {
			return apperrors.Wrap(apperrors.ErrCodeUnavailable, "copy walk canceled", err)
		}
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := copyDir(ctx, srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(ctx, srcPath, dstPath); err != nil {
			return err
		}
	}

	return nil
}

// copyFile streams src → dst, preserving the source mode and capturing
// close errors on the writable handle so flush failures aren't lost.
// ctx is checked on entry only; in-flight io.Copy does not poll the
// context — a single file's copy is bounded by its own size, not by
// the walk duration.
func copyFile(ctx context.Context, src, dst string) (retErr error) {
	if err := ctx.Err(); err != nil {
		return apperrors.Wrap(apperrors.ErrCodeUnavailable, "copy canceled", err)
	}
	info, err := os.Stat(src)
	if err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to stat source file", err)
	}

	in, err := os.Open(filepath.Clean(src)) //nolint:gosec // src is bundle content under the caller's directory
	if err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to open source file", err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(filepath.Clean(dst), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode()) //nolint:gosec // dst is package-controlled tempdir
	if err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create destination file", err)
	}
	defer func() {
		if closeErr := out.Close(); closeErr != nil && retErr == nil {
			retErr = apperrors.Wrap(apperrors.ErrCodeInternal, "failed to close destination file", closeErr)
		}
	}()

	if _, err := io.Copy(out, in); err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to copy file content", err)
	}
	return nil
}
