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
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
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
	// SourceFiles is the complete file set to package. Nil recursively
	// discovers files; a non-nil empty slice is invalid.
	SourceFiles []string
	// Annotations are additional manifest annotations to include.
	// Standard OCI annotations (org.opencontainers.image.*) are recommended.
	Annotations map[string]string
}

// PackageResult contains the result of local OCI packaging.
type PackageResult struct {
	// Descriptor is the exact immutable manifest descriptor produced locally.
	Descriptor ociv1.Descriptor
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
func Package(ctx context.Context, opts PackageOptions) (*PackageResult, error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.OCIBundlePublishTimeout)
	defer cancel()
	op, err := packageGenericOperation(ctx, opts)
	if err != nil {
		return nil, err
	}
	result, err := op.release()
	if err != nil {
		return nil, err
	}
	return result, nil
}

// PushFromStore pushes an already-packaged OCI artifact from a local OCI store to a remote registry.
//
//nolint:unparam // PushResult is part of the public API, returned for future callers
func PushFromStore(
	ctx context.Context,
	storePath string,
	expected ociv1.Descriptor,
	opts PushOptions,
) (*PushResult, error) {

	ctx, cancel := context.WithTimeout(ctx, defaults.OCIBundlePublishTimeout)
	defer cancel()
	return pushFromStoreWithDependencies(ctx, storePath, expected, opts, defaultPushOperationDependencies())
}

type genericPackageDependencies struct {
	prepareSource func(context.Context, string, string, string, []string) (*preparedSource, error)
	newLayout     func(context.Context, string) (*ownedLayout, error)
	beforeLayout  func() error
	buildArchive  func(context.Context, *preparedSource, *ownedLayout, archiveOptions) (string, ociv1.Descriptor, error)
	newStore      func(context.Context, *ownedLayout) (localOCIStore, error)
	beforeStore   func() error
	afterStore    func() error
	pushFileBlob  func(context.Context, localOCIStore, *ownedLayout, ociv1.Descriptor, string) error
	removeArchive func(*ownedLayout, string) error
	stageManifest func(context.Context, localOCIStore, string, ociv1.Descriptor, PackageOptions) (ociv1.Descriptor, error)
}

func defaultGenericPackageDependencies() genericPackageDependencies {
	return genericPackageDependencies{
		prepareSource: preparePackageSource,
		newLayout:     newOwnedLayout,
		buildArchive:  buildDeterministicTarGzip,
		newStore: func(ctx context.Context, layout *ownedLayout) (localOCIStore, error) {
			return newRootOCIStore(ctx, layout)
		},
		pushFileBlob: pushFileBlob,
		removeArchive: func(layout *ownedLayout, archiveName string) error {
			if err := layout.validate(); err != nil {
				return err
			}
			if err := validateSelectedPath(archiveName); err != nil || strings.Contains(archiveName, "/") {
				return apperrors.New(apperrors.ErrCodeInternal, "archive name is not layout-relative")
			}
			if err := layout.child.Remove(archiveName); err != nil {
				return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to remove staged archive", err)
			}
			return nil
		},
		stageManifest: stageGenericOCIManifest,
	}
}

type packageOperation struct {
	layout   *ownedLayout
	store    localOCIStore
	manifest ociv1.Descriptor
	result   PackageResult
}

func packageGenericOperation(ctx context.Context, opts PackageOptions) (*packageOperation, error) {
	return packageGenericOperationWithDependencies(ctx, opts, defaultGenericPackageDependencies())
}

func packageGenericOperationWithDependencies(
	ctx context.Context,
	opts PackageOptions,
	deps genericPackageDependencies,
) (_ *packageOperation, retErr error) {

	if err := validatePackageOperation(ctx, opts); err != nil {
		return nil, err
	}
	prepared, prepareErr := prepareGenericPackageSource(ctx, opts, deps)
	if prepareErr != nil {
		return nil, prepareErr
	}
	defer func() {
		if closeErr := prepared.Close(); closeErr != nil {
			if retErr == nil {
				retErr = closeErr
			} else {
				slog.Warn("failed to close prepared OCI source after primary error", "error", closeErr)
			}
		}
	}()
	if err := prepared.validate(); err != nil {
		return nil, err
	}
	localCtx, localCancel := context.WithTimeout(ctx, defaults.OCILocalPackageTimeout)
	defer localCancel()
	if err := runPackageHook(deps.beforeLayout, "OCI layout creation precondition failed"); err != nil {
		return nil, err
	}
	if err := prepared.validate(); err != nil {
		return nil, err
	}

	layout, layoutErr := deps.newLayout(localCtx, opts.OutputDir)
	if layoutErr != nil {
		return nil, layoutErr
	}
	keepLayout := false
	defer func() {
		if keepLayout {
			return
		}
		if closeErr := layout.Close(); closeErr != nil {
			if retErr == nil {
				retErr = closeErr
			} else {
				slog.Warn("failed to remove OCI layout after primary error", "error", closeErr)
			}
		}
	}()
	if err := prepared.validate(); err != nil {
		return nil, err
	}
	if err := layout.validate(); err != nil {
		return nil, err
	}

	archiveName, archiveDescriptor, archiveErr := deps.buildArchive(
		localCtx, prepared, layout, archiveOptions{})
	if archiveErr != nil {
		return nil, archiveErr
	}
	if err := prepared.validate(); err != nil {
		return nil, err
	}
	if err := layout.validate(); err != nil {
		return nil, err
	}
	if err := runPackageHook(deps.beforeStore, "OCI store open precondition failed"); err != nil {
		return nil, err
	}
	if err := layout.validate(); err != nil {
		return nil, err
	}
	store, storeErr := deps.newStore(localCtx, layout)
	if storeErr != nil {
		return nil, apperrors.PropagateOrWrap(
			storeErr, apperrors.ErrCodeInternal, "failed to create OCI layout store")
	}
	if err := runPackageHook(deps.afterStore, "OCI store open postcondition failed"); err != nil {
		return nil, err
	}
	if err := layout.validate(); err != nil {
		return nil, err
	}
	if blobErr := deps.pushFileBlob(localCtx, store, layout, archiveDescriptor, archiveName); blobErr != nil {
		return nil, apperrors.PropagateOrWrap(blobErr, apperrors.ErrCodeInternal,
			"failed to push archive into OCI layout")
	}
	if err := layout.validate(); err != nil {
		return nil, err
	}
	if removeErr := deps.removeArchive(layout, archiveName); removeErr != nil {
		return nil, apperrors.PropagateOrWrap(removeErr, apperrors.ErrCodeInternal,
			"failed to remove staged archive")
	}
	if err := layout.validate(); err != nil {
		return nil, err
	}
	manifest, manifestErr := deps.stageManifest(localCtx, store, archiveName, archiveDescriptor, opts)
	if manifestErr != nil {
		return nil, manifestErr
	}
	if err := layout.validate(); err != nil {
		return nil, err
	}
	if err := prepared.Close(); err != nil {
		return nil, err
	}
	manifest = immutableDescriptor(manifest)
	refString := fmt.Sprintf("%s/%s:%s", stripProtocol(opts.Registry), opts.Repository, opts.Tag)
	op := &packageOperation{
		layout:   layout,
		store:    store,
		manifest: manifest,
		result: PackageResult{
			Descriptor: manifest,
			Digest:     manifest.Digest.String(),
			MediaType:  manifest.MediaType,
			Size:       manifest.Size,
			Reference:  refString,
			StorePath:  layout.Path(),
		},
	}
	keepLayout = true
	return op, nil
}

func validatePackageOperation(ctx context.Context, opts PackageOptions) error {
	if err := validatePackageOptions(opts); err != nil {
		return err
	}
	return contextError(ctx, "OCI packaging canceled")
}

func prepareGenericPackageSource(
	ctx context.Context,
	opts PackageOptions,
	deps genericPackageDependencies,
) (*preparedSource, error) {

	stageCtx, cancel := context.WithTimeout(ctx, defaults.OCISourceStageTimeout)
	defer cancel()
	return deps.prepareSource(
		stageCtx, opts.SourceDir, opts.OutputDir, opts.SubDir, opts.SourceFiles)
}

func runPackageHook(hook func() error, message string) error {
	if hook == nil {
		return nil
	}
	if err := hook(); err != nil {
		return apperrors.PropagateOrWrap(err, apperrors.ErrCodeInternal, message)
	}
	return nil
}

func validatePackageOptions(opts PackageOptions) error {
	if opts.Tag == "" {
		return apperrors.New(apperrors.ErrCodeInvalidRequest, "tag is required for OCI packaging")
	}
	if opts.Registry == "" {
		return apperrors.New(apperrors.ErrCodeInvalidRequest, "registry is required for OCI packaging")
	}
	if opts.Repository == "" {
		return apperrors.New(apperrors.ErrCodeInvalidRequest, "repository is required for OCI packaging")
	}
	if err := validateDistributionTag(opts.Tag); err != nil {
		return err
	}
	return validateRegistryReference(opts.Registry, opts.Repository)
}

func immutableDescriptor(descriptor ociv1.Descriptor) ociv1.Descriptor {
	return ociv1.Descriptor{
		MediaType: descriptor.MediaType,
		Digest:    descriptor.Digest,
		Size:      descriptor.Size,
	}
}

func pushFileBlob(
	ctx context.Context,
	store localOCIStore,
	layout *ownedLayout,
	descriptor ociv1.Descriptor,
	archiveName string,
) (retErr error) {

	if err := layout.validate(); err != nil {
		return err
	}
	file, err := layout.child.Open(archiveName)
	if err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to open staged archive", err)
	}
	if err := layout.validate(); err != nil {
		_ = file.Close()
		return err
	}
	reader := newContextReadCloser(ctx, file)
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			if retErr == nil {
				retErr = apperrors.Wrap(apperrors.ErrCodeInternal, "failed to close staged archive", closeErr)
			} else {
				slog.Warn("failed to close staged archive after primary error", "error", closeErr)
			}
		}
	}()
	if err := store.Push(ctx, descriptor, reader); err != nil {
		if ctx.Err() != nil {
			return contextError(ctx, "local OCI blob push canceled")
		}
		return apperrors.PropagateOrWrap(
			err, apperrors.ErrCodeInternal, "failed to push staged archive")
	}
	return nil
}

func stageGenericOCIManifest(
	ctx context.Context,
	store localOCIStore,
	_ string,
	archiveDescriptor ociv1.Descriptor,
	opts PackageOptions,
) (ociv1.Descriptor, error) {

	packOpts := oras.PackManifestOptions{Layers: []ociv1.Descriptor{archiveDescriptor}}
	packOpts.ManifestAnnotations = make(map[string]string, len(opts.Annotations)+1)
	for key, value := range opts.Annotations {
		packOpts.ManifestAnnotations[key] = value
	}
	packOpts.ManifestAnnotations[ociv1.AnnotationCreated] = reproducibleTimestamp
	manifest, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, artifactType, packOpts)
	if err != nil {
		return ociv1.Descriptor{}, apperrors.PropagateOrWrap(
			err, apperrors.ErrCodeInternal, "failed to pack OCI manifest")
	}
	if err := store.Tag(ctx, manifest, opts.Tag); err != nil {
		return ociv1.Descriptor{}, apperrors.PropagateOrWrap(
			err, apperrors.ErrCodeInternal, "failed to tag OCI manifest")
	}
	return manifest, nil
}

func (o *packageOperation) Close() error {
	if o == nil || o.layout == nil {
		return nil
	}
	return o.layout.Close()
}

func (o *packageOperation) release() (*PackageResult, error) {
	path, err := o.layout.release()
	if err != nil {
		return nil, err
	}
	result := o.result
	result.StorePath = path
	return &result, nil
}

type pushOperationDependencies struct {
	newTarget         func(PushOptions) (publicationTarget, error)
	openStoreRoot     func(context.Context, string, func(*os.Root) error) (*rootedDirectory, error)
	newReadOnlyStore  func(fs.FS) content.ReadOnlyStorage
	beforeStoreOpen   func() error
	afterStoreOpen    func() error
	closeStoreRoot    func(*os.Root) error
	maxAttempts       int
	initialBackoff    time.Duration
	perAttemptTimeout time.Duration
	copyGraph         copyGraphFunc
	pushAttempt       pushDescriptorAttemptFunc
	beforeAttempt     func(int) error
	source            func(content.ReadOnlyStorage) content.ReadOnlyStorage
}

type pushDescriptorAttemptFunc func(
	context.Context,
	content.ReadOnlyStorage,
	publicationTarget,
	ociv1.Descriptor,
	string,
	apperrors.ErrorCode,
	oras.CopyGraphOptions,
	copyGraphFunc,
) error

func defaultPushOperationDependencies() pushOperationDependencies {
	return pushOperationDependencies{
		newTarget: newPublicationTarget,
		openStoreRoot: func(
			ctx context.Context,
			path string,
			closeRoot func(*os.Root) error,
		) (*rootedDirectory, error) {

			return openRootedDirectoryWithClose(
				ctx, path, apperrors.ErrCodeInvalidRequest, nil, closeRoot)
		},
		newReadOnlyStore:  func(fsys fs.FS) content.ReadOnlyStorage { return oci.NewStorageFromFS(fsys) },
		closeStoreRoot:    func(root *os.Root) error { return root.Close() },
		maxAttempts:       defaults.RegistryPushRetries,
		initialBackoff:    defaults.RegistryPushBackoff,
		perAttemptTimeout: defaults.RegistryPushTimeout,
		copyGraph:         oras.CopyGraph,
		pushAttempt:       pushFrozenDescriptorAttempt,
		source:            func(source content.ReadOnlyStorage) content.ReadOnlyStorage { return source },
	}
}

func newPublicationTarget(opts PushOptions) (publicationTarget, error) {
	registryHost := stripProtocol(opts.Registry)
	repository, err := remote.NewRepository(fmt.Sprintf("%s/%s", registryHost, opts.Repository))
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "failed to initialize remote repository", err)
	}
	repository.PlainHTTP = opts.PlainHTTP
	authClient, authErr := createAuthClientForHost(registryHost, opts.PlainHTTP, opts.InsecureTLS)
	if authErr != nil {
		slog.Warn("failed to initialize Docker credential store, continuing without authentication", "error", authErr)
	}
	repository.Client = authClient
	return &remotePublicationTarget{publicationTargetCore: repository, client: authClient.Client}, nil
}

type remotePublicationTarget struct {
	publicationTargetCore
	client *http.Client
}

func (t *remotePublicationTarget) CloseIdleConnections() {
	if t.client != nil {
		t.client.CloseIdleConnections()
	}
}

func pushPackageOperation(
	ctx context.Context,
	op *packageOperation,
	opts PushOptions,
) (*PushResult, error) {

	return pushPackageOperationWithDependencies(ctx, op, opts, defaultPushOperationDependencies())
}

func pushPackageOperationWithDependencies(
	ctx context.Context,
	op *packageOperation,
	opts PushOptions,
	deps pushOperationDependencies,
) (*PushResult, error) {

	if op == nil || op.layout == nil || op.store == nil {
		return nil, apperrors.New(apperrors.ErrCodeInternal, "package operation is incomplete")
	}
	return pushFrozenDescriptor(ctx, deps.source(op.store), op.manifest, opts,
		apperrors.ErrCodeInternal, op.layout.validate, deps)
}

func pushFromStoreWithDependencies(
	ctx context.Context,
	storePath string,
	expected ociv1.Descriptor,
	opts PushOptions,
	deps pushOperationDependencies,
) (*PushResult, error) {

	if err := validatePushOptions(opts); err != nil {
		return nil, err
	}
	if err := validateExpectedManifest(expected); err != nil {
		return nil, err
	}
	if err := contextError(ctx, "OCI store push canceled"); err != nil {
		return nil, err
	}
	storeRoot, rootOpenErr := deps.openStoreRoot(ctx, storePath, deps.closeStoreRoot)
	if rootOpenErr != nil {
		return nil, rootOpenErr
	}
	closeStore := func(primary error) error {
		closeErr := storeRoot.close("public OCI store root", deps.closeStoreRoot)
		if primary != nil && closeErr != nil {
			slog.Warn("failed to close public OCI store after primary error",
				"error", closeErr, "primary", primary)
			return primary
		}
		return stderrors.Join(primary, closeErr)
	}
	if deps.beforeStoreOpen != nil {
		if err := deps.beforeStoreOpen(); err != nil {
			return nil, closeStore(apperrors.PropagateOrWrap(
				err, apperrors.ErrCodeInvalidRequest, "public OCI store open precondition failed"))
		}
	}
	if err := storeRoot.validate(apperrors.ErrCodeInvalidRequest); err != nil {
		return nil, closeStore(err)
	}
	store := deps.newReadOnlyStore(storeRoot.root.FS())
	if deps.afterStoreOpen != nil {
		if err := deps.afterStoreOpen(); err != nil {
			return nil, closeStore(apperrors.PropagateOrWrap(
				err, apperrors.ErrCodeInvalidRequest, "public OCI store open postcondition failed"))
		}
	}
	if err := storeRoot.validate(apperrors.ErrCodeInvalidRequest); err != nil {
		return nil, closeStore(err)
	}
	validate := func() error { return storeRoot.validate(apperrors.ErrCodeInvalidRequest) }
	result, pushErr := pushFrozenDescriptor(ctx, deps.source(store), immutableDescriptor(expected), opts,
		apperrors.ErrCodeInvalidRequest, validate, deps)
	if closeErr := closeStore(pushErr); closeErr != nil {
		return nil, closeErr
	}
	return result, nil
}

func validatePushOptions(opts PushOptions) error {
	if opts.Tag == "" {
		return apperrors.New(apperrors.ErrCodeInvalidRequest, "tag is required to push OCI image")
	}
	if opts.Registry == "" || opts.Repository == "" {
		return apperrors.New(apperrors.ErrCodeInvalidRequest, "registry and repository are required")
	}
	if err := validateDistributionTag(opts.Tag); err != nil {
		return err
	}
	return validateRegistryReference(opts.Registry, opts.Repository)
}

func validateExpectedManifest(expected ociv1.Descriptor) error {
	if expected.MediaType == "" || expected.Digest == "" || expected.Size <= 0 {
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			"expected OCI manifest descriptor requires media type, digest, and positive size")
	}
	if err := expected.Digest.Validate(); err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInvalidRequest, "expected OCI manifest digest is invalid", err)
	}
	switch expected.MediaType {
	case ociv1.MediaTypeImageManifest,
		ociv1.MediaTypeImageIndex,
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json":
		return nil
	default:
		return apperrors.New(apperrors.ErrCodeInvalidRequest, "expected descriptor is not a supported OCI manifest")
	}
}

func pushFrozenDescriptor(
	ctx context.Context,
	source content.ReadOnlyStorage,
	expected ociv1.Descriptor,
	opts PushOptions,
	sourceFailureCode apperrors.ErrorCode,
	validateSource func() error,
	deps pushOperationDependencies,
) (*PushResult, error) {

	if err := validatePushOptions(opts); err != nil {
		return nil, err
	}
	if err := validateExpectedManifest(expected); err != nil {
		if sourceFailureCode == apperrors.ErrCodeInternal {
			return nil, apperrors.Wrap(apperrors.ErrCodeInternal, "generated OCI descriptor is invalid", err)
		}
		return nil, err
	}
	target, err := deps.newTarget(opts)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, apperrors.New(apperrors.ErrCodeInternal, "OCI publication target is nil")
	}
	defer target.CloseIdleConnections()
	attempts := deps.maxAttempts
	if attempts < 1 {
		attempts = 1
	}
	backoff := deps.initialBackoff
	copyOptions := oras.DefaultCopyGraphOptions
	copyOptions.Concurrency = defaults.OCIPushConcurrency
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := contextError(ctx, "OCI registry push canceled"); err != nil {
			return nil, err
		}
		if deps.beforeAttempt != nil {
			if err := deps.beforeAttempt(attempt); err != nil {
				return nil, apperrors.PropagateOrWrap(err, apperrors.ErrCodeInternal,
					"OCI push attempt precondition failed")
			}
		}
		if err := validateSource(); err != nil {
			return nil, err
		}
		attemptCtx, cancel := context.WithTimeout(ctx, deps.perAttemptTimeout)
		lastErr = deps.pushAttempt(
			attemptCtx, source, target, expected, opts.Tag, sourceFailureCode, copyOptions, deps.copyGraph)
		parentErr := ctx.Err()
		attemptErr := attemptCtx.Err()
		cancel()
		if parentErr != nil {
			return nil, contextError(ctx, "OCI registry push canceled")
		}
		if attemptErr != nil {
			lastErr = attemptErr
		}
		if lastErr == nil {
			if err := contextError(ctx, "OCI registry push canceled"); err != nil {
				return nil, err
			}
			return &PushResult{
				Digest:    expected.Digest.String(),
				MediaType: expected.MediaType,
				Size:      expected.Size,
				Reference: fmt.Sprintf("%s/%s:%s", stripProtocol(opts.Registry), opts.Repository, opts.Tag),
			}, nil
		}
		if ctx.Err() != nil {
			return nil, contextError(ctx, "OCI registry push canceled")
		}
		if !isTransientPushError(lastErr) && !apperrors.IsTransient(lastErr) {
			return nil, classifyRegistryPushFailure(lastErr, false)
		}
		if attempt == attempts {
			break
		}
		timer := time.NewTimer(jitterDuration(backoff))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, contextError(ctx, "OCI registry retry canceled")
		case <-timer.C:
		}
		backoff *= 2
	}
	return nil, classifyRegistryPushFailure(lastErr, true)
}

func pushFrozenDescriptorAttempt(
	ctx context.Context,
	source content.ReadOnlyStorage,
	target publicationTarget,
	expected ociv1.Descriptor,
	tag string,
	sourceFailureCode apperrors.ErrorCode,
	copyOptions oras.CopyGraphOptions,
	copyGraph copyGraphFunc,
) error {

	if err := copyGraphWithTrackedSource(
		ctx, source, target, expected, sourceFailureCode, copyOptions, copyGraph); err != nil {
		return err
	}
	remoteDescriptor, err := target.Resolve(ctx, expected.Digest.String())
	if err != nil {
		return err
	}
	if !content.Equal(remoteDescriptor, expected) {
		return apperrors.New(apperrors.ErrCodeInternal,
			"remote OCI descriptor does not match the immutable local descriptor")
	}
	if err := target.Tag(ctx, expected, tag); err != nil {
		return err
	}
	return nil
}

func classifyRegistryPushFailure(err error, exhausted bool) error {
	if err == nil {
		return nil
	}
	var response *errcode.ErrorResponse
	if stderrors.As(err, &response) {
		var code apperrors.ErrorCode
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			code = apperrors.ErrCodeUnauthorized
		case http.StatusNotFound:
			code = apperrors.ErrCodeNotFound
		case http.StatusConflict:
			code = apperrors.ErrCodeConflict
		case http.StatusTooManyRequests:
			code = apperrors.ErrCodeRateLimitExceeded
		default:
			switch {
			case response.StatusCode >= 400 && response.StatusCode < 500:
				code = apperrors.ErrCodeInvalidRequest
			case response.StatusCode >= 500:
				code = apperrors.ErrCodeUnavailable
			default:
				code = apperrors.ErrCodeInternal
			}
		}
		return apperrors.Wrap(code, "OCI registry push failed", err)
	}
	if exhausted && (isTransientPushError(err) || apperrors.IsTransient(err)) {
		return apperrors.Wrap(
			apperrors.ErrCodeUnavailable, "OCI registry push failed after retries", err)
	}
	var structured *apperrors.StructuredError
	if stderrors.As(err, &structured) {
		return err
	}
	return apperrors.Wrap(apperrors.ErrCodeInternal, "OCI registry push failed", err)
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

	// ExcludedRoots optionally names existing real directories that referrer
	// staging must remain outside in both lexical and resolved filesystem
	// topology. Evidence publication requires at least one exclusion; generic
	// low-level callers may leave the slice nil when no local trust root exists.
	ExcludedRoots []string

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

type referrerPushDependencies struct {
	pack      func(context.Context, ReferrerOptions) (*referrerPackage, error)
	newTarget func(PushOptions) (publicationTarget, error)
	copy      copyFunc
}

func defaultReferrerPushDependencies() referrerPushDependencies {
	return referrerPushDependencies{
		pack:      packReferrer,
		newTarget: newPublicationTarget,
		copy: func(
			ctx context.Context,
			src oras.ReadOnlyTarget,
			srcRef string,
			dst oras.Target,
			dstRef string,
			opts oras.CopyOptions,
		) (ociv1.Descriptor, error) {

			return copyWithRetry(ctx, src, srcRef, dst, dstRef, opts, oras.Copy)
		},
	}
}

// PushReferrer pushes a single-layer OCI manifest with a Subject set,
// attaching it as a Referrer of the subject artifact. cosign discovers
// signatures attached this way via the OCI Distribution 1.1 Referrers
// API. The tag is derived from the referrer manifest digest so multiple
// referrers can coexist without colliding on a fixed tag.
func PushReferrer(ctx context.Context, opts ReferrerOptions) (*PushResult, error) {
	return pushReferrerWithDependencies(ctx, opts, defaultReferrerPushDependencies())
}

func pushReferrerWithDependencies(
	ctx context.Context,
	opts ReferrerOptions,
	deps referrerPushDependencies,
) (retResult *PushResult, retErr error) {

	ctx, cancel := context.WithTimeout(ctx, defaults.OCIBundlePublishTimeout)
	defer cancel()

	if err := contextError(ctx, "OCI referrer push canceled"); err != nil {
		return nil, err
	}
	if err := validateRegistryReference(opts.Registry, opts.Repository); err != nil {
		return nil, err
	}
	packed, packErr := deps.pack(ctx, opts)
	if packErr != nil {
		return nil, packErr
	}
	defer func() {
		if closeErr := packed.Close(); closeErr != nil {
			if retErr == nil {
				retResult = nil
				retErr = closeErr
			} else {
				slog.Warn("failed to close referrer package after primary error",
					"error", closeErr, "primary", retErr)
			}
		}
	}()
	if validateErr := packed.validate(); validateErr != nil {
		return nil, validateErr
	}

	target, targetErr := deps.newTarget(PushOptions{
		Registry:    opts.Registry,
		Repository:  opts.Repository,
		PlainHTTP:   opts.PlainHTTP,
		InsecureTLS: opts.InsecureTLS,
	})
	if targetErr != nil {
		return nil, targetErr
	}
	if target == nil {
		return nil, apperrors.New(apperrors.ErrCodeInternal, "OCI publication target is nil")
	}
	defer target.CloseIdleConnections()

	copyOpts := oras.DefaultCopyOptions
	copyOpts.Concurrency = defaults.OCIPushConcurrency
	if validateErr := packed.validate(); validateErr != nil {
		return nil, validateErr
	}
	desc, copyErr := deps.copy(ctx, packed.store, packed.tag, target, packed.tag, copyOpts)
	if copyErr != nil {
		return nil, wrapContextualIOFailure(
			ctx, copyErr, apperrors.ErrCodeInternal, "failed to push OCI referrer")
	}
	if err := contextError(ctx, "OCI referrer push canceled"); err != nil {
		return nil, err
	}
	if validateErr := packed.validate(); validateErr != nil {
		return nil, validateErr
	}

	registryHost := stripProtocol(opts.Registry)
	return &PushResult{
		Digest:    desc.Digest.String(),
		MediaType: desc.MediaType,
		Size:      desc.Size,
		Reference: fmt.Sprintf("%s/%s@%s", registryHost, opts.Repository, desc.Digest.String()),
	}, nil
}

type referrerPackage struct {
	store          *file.Store
	workspace      *Workspace
	tag            string
	closeStore     func(*file.Store) error
	closeWorkspace func(*Workspace) error
	closeOnce      sync.Once
	closeErr       error
}

func (p *referrerPackage) validate() error {
	if p == nil || p.store == nil || p.workspace == nil || p.tag == "" {
		return apperrors.New(apperrors.ErrCodeInternal, "referrer package ownership is incomplete")
	}
	return p.workspace.validate()
}

// Close releases the referrer store before removing its private workspace.
// The checked result is cached so callers can safely invoke Close more than once.
func (p *referrerPackage) Close() error {
	if p == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		if p.store != nil {
			if p.closeStore == nil {
				p.closeErr = stderrors.Join(p.closeErr, apperrors.New(
					apperrors.ErrCodeInternal, "referrer store cleanup is unavailable"))
			} else if err := p.closeStore(p.store); err != nil {
				p.closeErr = stderrors.Join(p.closeErr, apperrors.Wrap(
					apperrors.ErrCodeInternal, "failed to close referrer file store", err))
			}
		}
		if p.workspace != nil {
			if p.closeWorkspace == nil {
				p.closeErr = stderrors.Join(p.closeErr, apperrors.New(
					apperrors.ErrCodeInternal, "referrer workspace cleanup is unavailable"))
			} else if err := p.closeWorkspace(p.workspace); err != nil {
				p.closeErr = stderrors.Join(p.closeErr, apperrors.PropagateOrWrap(
					err, apperrors.ErrCodeInternal, "failed to close referrer workspace"))
			}
		}
	})
	return p.closeErr
}

type referrerPackDependencies struct {
	newWorkspace   func(context.Context, string, ...string) (*Workspace, error)
	newStore       func(string) (*file.Store, error)
	closeStore     func(*file.Store) error
	closeWorkspace func(*Workspace) error
}

func defaultReferrerPackDependencies() referrerPackDependencies {
	return referrerPackDependencies{
		newWorkspace:   NewPrivateWorkspace,
		newStore:       file.New,
		closeStore:     func(store *file.Store) error { return store.Close() },
		closeWorkspace: func(workspace *Workspace) error { return workspace.Close() },
	}
}

func normalizeReferrerPackDependencies(deps referrerPackDependencies) referrerPackDependencies {
	defaults := defaultReferrerPackDependencies()
	if deps.newWorkspace == nil {
		deps.newWorkspace = defaults.newWorkspace
	}
	if deps.newStore == nil {
		deps.newStore = defaults.newStore
	}
	if deps.closeStore == nil {
		deps.closeStore = defaults.closeStore
	}
	if deps.closeWorkspace == nil {
		deps.closeWorkspace = defaults.closeWorkspace
	}
	return deps
}

// packReferrer builds the referrer manifest in a private local file store.
func packReferrer(ctx context.Context, opts ReferrerOptions) (*referrerPackage, error) {
	return packReferrerWithDependencies(ctx, opts, defaultReferrerPackDependencies())
}

func packReferrerWithDependencies(
	ctx context.Context,
	opts ReferrerOptions,
	deps referrerPackDependencies,
) (*referrerPackage, error) {

	deps = normalizeReferrerPackDependencies(deps)

	if opts.ArtifactType == "" {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest, "ArtifactType is required")
	}
	if len(opts.LayerContent) == 0 {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest, "LayerContent must be non-empty")
	}
	if opts.Subject.Digest == "" {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest, "Subject.Digest is required")
	}
	if err := contextError(ctx, "referrer packaging canceled"); err != nil {
		return nil, err
	}

	workspace, workspaceErr := deps.newWorkspace(ctx, "aicr-oci-referrer-", opts.ExcludedRoots...)
	if workspaceErr != nil {
		return nil, apperrors.PropagateOrWrap(
			workspaceErr, apperrors.ErrCodeInternal, "failed to create referrer workspace")
	}
	packed := &referrerPackage{
		workspace:      workspace,
		closeStore:     deps.closeStore,
		closeWorkspace: deps.closeWorkspace,
	}
	fail := func(primary error) (*referrerPackage, error) {
		if cleanupErr := packed.Close(); cleanupErr != nil {
			slog.Warn("failed to close rejected referrer package after primary error",
				"error", cleanupErr, "primary", primary)
		}
		return nil, primary
	}
	if validateErr := workspace.validate(); validateErr != nil {
		return fail(validateErr)
	}

	const layerFilename = "payload"
	if writeErr := writeReferrerLayer(ctx, workspace, layerFilename, opts.LayerContent); writeErr != nil {
		return fail(writeErr)
	}
	if validateErr := workspace.validate(); validateErr != nil {
		return fail(validateErr)
	}
	fs, storeErr := deps.newStore(workspace.Path())
	if storeErr != nil {
		return fail(apperrors.Wrap(
			apperrors.ErrCodeInternal, "failed to create referrer file store", storeErr))
	}
	packed.store = fs
	fs.TarReproducible = true
	if validateErr := workspace.validate(); validateErr != nil {
		return fail(validateErr)
	}

	layerPath := filepath.Join(workspace.Path(), layerFilename)
	layerDesc, addErr := fs.Add(ctx, layerFilename, opts.ArtifactType, layerPath)
	if addErr != nil {
		return fail(wrapContextualIOFailure(
			ctx, addErr, apperrors.ErrCodeInternal, "failed to add referrer layer"))
	}
	if validateErr := workspace.validate(); validateErr != nil {
		return fail(validateErr)
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

	manifestDesc, manifestErr := oras.PackManifest(
		ctx, fs, oras.PackManifestVersion1_1, opts.ArtifactType, packOpts)
	if manifestErr != nil {
		return fail(wrapContextualIOFailure(
			ctx, manifestErr, apperrors.ErrCodeInternal, "failed to pack referrer manifest"))
	}
	if validateErr := workspace.validate(); validateErr != nil {
		return fail(validateErr)
	}

	tag := strings.TrimPrefix(manifestDesc.Digest.String(), "sha256:")
	if tagErr := fs.Tag(ctx, manifestDesc, tag); tagErr != nil {
		return fail(wrapContextualIOFailure(
			ctx, tagErr, apperrors.ErrCodeInternal, "failed to tag referrer manifest"))
	}
	if validateErr := workspace.validate(); validateErr != nil {
		return fail(validateErr)
	}
	packed.tag = tag
	return packed, nil
}

func writeReferrerLayer(
	ctx context.Context,
	workspace *Workspace,
	name string,
	content []byte,
) (retErr error) {

	if err := contextError(ctx, "referrer layer staging canceled"); err != nil {
		return err
	}
	if err := workspace.validate(); err != nil {
		return err
	}
	file, err := workspace.child.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to create referrer layer", err)
	}
	defer func() {
		if closeErr := file.Close(); closeErr != nil {
			retErr = stderrors.Join(retErr, apperrors.Wrap(
				apperrors.ErrCodeInternal, "failed to close referrer layer", closeErr))
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInternal, "failed to secure referrer layer", err)
	}
	opened, openedErr := file.Stat()
	named, namedErr := workspace.child.Lstat(name)
	if openedErr != nil || namedErr != nil || !opened.Mode().IsRegular() ||
		named.Mode()&os.ModeSymlink != 0 || !named.Mode().IsRegular() ||
		opened.Mode().Perm() != 0o600 || named.Mode().Perm() != 0o600 ||
		opened.Size() != 0 || named.Size() != 0 || !os.SameFile(opened, named) {

		return apperrors.Wrap(apperrors.ErrCodeInternal,
			"referrer layer identity changed during creation", stderrors.Join(openedErr, namedErr))
	}
	written, writeErr := file.Write(content)
	if writeErr == nil && written != len(content) {
		writeErr = io.ErrShortWrite
	}
	if writeErr != nil {
		return wrapContextualIOFailure(
			ctx, writeErr, apperrors.ErrCodeInternal, "failed to write referrer layer")
	}
	if err := contextError(ctx, "referrer layer staging canceled"); err != nil {
		return err
	}
	if err := workspace.validate(); err != nil {
		return err
	}
	openedAfter, openedAfterErr := file.Stat()
	namedAfter, namedAfterErr := workspace.child.Lstat(name)
	if openedAfterErr != nil || namedAfterErr != nil ||
		!openedAfter.Mode().IsRegular() || namedAfter.Mode()&os.ModeSymlink != 0 ||
		!namedAfter.Mode().IsRegular() || openedAfter.Mode().Perm() != 0o600 ||
		namedAfter.Mode().Perm() != 0o600 ||
		openedAfter.Size() != int64(len(content)) || namedAfter.Size() != int64(len(content)) ||
		!os.SameFile(opened, openedAfter) || !os.SameFile(opened, namedAfter) {

		return apperrors.Wrap(apperrors.ErrCodeInternal,
			"referrer layer identity changed after write",
			stderrors.Join(openedAfterErr, namedAfterErr))
	}

	return nil
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
			return ociv1.Descriptor{}, contextError(ctx, "registry push canceled before attempt")
		}

		pushCtx, pushCancel := context.WithTimeout(ctx, perAttemptTimeout)
		desc, lastErr = copy(pushCtx, src, srcRef, dst, dstRef, opts)
		parentErr := contextError(ctx, "registry push canceled")
		attemptErr := contextError(pushCtx, "registry push attempt canceled")
		pushCancel()
		if parentErr != nil {
			return ociv1.Descriptor{}, parentErr
		}
		if attemptErr != nil {
			if attempt == maxAttempts {
				return ociv1.Descriptor{}, attemptErr
			}
			lastErr = attemptErr
		} else if lastErr == nil {
			if err := contextError(ctx, "registry push canceled"); err != nil {
				return ociv1.Descriptor{}, err
			}
			return desc, nil
		}

		// Don't retry if the parent context was canceled or for non-transient
		// errors (e.g., 4xx auth/validation failures from the registry).
		if ctx.Err() != nil {
			return ociv1.Descriptor{}, contextError(ctx, "registry push canceled")
		}
		if stderrors.Is(lastErr, context.Canceled) {
			return ociv1.Descriptor{}, classifyRegistryPushFailure(lastErr, false)
		}
		if !isTransientPushError(lastErr) && !apperrors.IsTransient(lastErr) {
			return ociv1.Descriptor{}, classifyRegistryPushFailure(lastErr, false)
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
			return ociv1.Descriptor{}, contextError(ctx, "registry push canceled during backoff")
		case <-timer.C:
		}
		backoff *= 2
	}

	return ociv1.Descriptor{}, classifyRegistryPushFailure(lastErr, true)
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
		// Registry uploads can legitimately exceed the generic HTTP client
		// ceiling. Dial/TLS/header bounds live on the transport; the registry
		// attempt context is the total-operation deadline.
		Client: &http.Client{Transport: transport},
		Cache:  auth.NewCache(),
	}

	// Only set credential function if store was created successfully
	if credErr == nil && credStore != nil {
		client.Credential = credentials.Credential(credStore)
	}

	return client, credErr
}
