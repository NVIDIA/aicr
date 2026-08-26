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
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	stderrors "errors"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/distribution/reference"
	"github.com/opencontainers/go-digest"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/errdef"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/NVIDIA/aicr/pkg/defaults"
	apperrors "github.com/NVIDIA/aicr/pkg/errors"
)

const (
	recipeResolvedTag       = "resolved"
	recipeMaterializePrefix = "recipe-data-"
)

var (
	errOCIRecipeDownloadLimit     = stderrors.New("OCI recipe download limit exceeded")
	errOCIRecipeRetryTrafficLimit = stderrors.New("OCI recipe retry traffic limit exceeded")
	errOCIRecipeExpandedLimit     = stderrors.New("OCI recipe expanded archive limit exceeded")
)

// RecipePullOptions configures staging of one AICR recipe artifact from an OCI
// repository. Repository may have an optional oci:// prefix but must not embed
// a tag or digest. Selector is required and is either a Distribution tag or an
// immutable sha256 digest. TempDir is an existing real directory used only as
// the parent of a unique private child; empty selects os.TempDir().
type RecipePullOptions struct {
	Repository string
	Selector   string
	TempDir    string
}

type recipePullValidation struct {
	normalized     string
	registry       string
	repository     string
	selectorDigest digest.Digest
}

// ValidateRecipePullOptions performs the I/O-free structural validation used
// before staging and reports whether Selector is an immutable sha256 digest.
// An empty TempDir selects os.TempDir; a nonempty value is syntax-checked here,
// while StageRecipeArtifact verifies that it can create the private child.
func ValidateRecipePullOptions(opts RecipePullOptions) (bool, error) {
	validated, err := validateRecipePullOptions(opts)
	if err != nil {
		return false, err
	}
	return validated.selectorDigest != "", nil
}

// StagedRecipeArtifact owns the private OCI content and, after an explicit
// Materialize call, the extracted recipe tree. Staging alone never exposes or
// activates recipe data. Close must be called when the artifact is no longer
// needed.
type StagedRecipeArtifact struct {
	mu sync.Mutex

	layout        *ownedLayout
	store         *rootOCIStore
	remote        recipeArtifactRepository
	descriptor    ociv1.Descriptor
	manifest      ociv1.Manifest
	selectorHash  digest.Digest
	reference     string
	registry      string
	repository    string
	materialized  string
	authorization recipeMaterializationAuthorization
	closed        bool
	closeOnce     sync.Once
	closeErr      error
	extract       recipeExtractLimits
}

// Descriptor returns a defensive copy of the immutable resolved manifest
// descriptor. The returned descriptor cannot mutate the staged artifact.
func (a *StagedRecipeArtifact) Descriptor() ociv1.Descriptor {
	if a == nil {
		return ociv1.Descriptor{}
	}
	return cloneRootStoreDescriptor(a.descriptor)
}

// Reference returns the canonical digest-bound reference for the staged
// artifact, regardless of whether the caller selected it by tag or digest.
func (a *StagedRecipeArtifact) Reference() string {
	if a == nil {
		return ""
	}
	return a.reference
}

// Materialize extracts the already-staged artifact into a distinct private
// subtree. Callers must perform their required verification before invoking
// this method. Concurrent calls serialize and return the same completed tree.
// A failed extraction removes its partial subtree and may be retried.
func (a *StagedRecipeArtifact) Materialize(ctx context.Context) (string, error) {
	if a == nil {
		return "", apperrors.New(apperrors.ErrCodeInvalidRequest,
			"staged OCI recipe artifact is required")
	}
	ctx, cancel := context.WithTimeout(ctx, defaults.OCIRecipePullTimeout)
	defer cancel()

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return "", apperrors.New(apperrors.ErrCodeUnavailable,
			"staged OCI recipe artifact is closed")
	}
	if err := recipeContextError(ctx, "OCI recipe materialization canceled"); err != nil {
		return "", err
	}
	if a.authorization == recipeMaterializationUnauthorized {
		return "", apperrors.New(apperrors.ErrCodeUnauthorized,
			"OCI recipe artifact materialization requires successful authorization")
	}
	if a.materialized != "" {
		return a.materialized, nil
	}
	if a.layout == nil || a.store == nil || len(a.manifest.Layers) != 1 {
		return "", apperrors.New(apperrors.ErrCodeInternal,
			"staged OCI recipe artifact is incomplete")
	}
	if err := a.layout.validate(); err != nil {
		return "", err
	}

	name, err := createUniqueDirectory(ctx, a.layout.child, recipeMaterializePrefix)
	if err != nil {
		return "", err
	}
	root, err := a.layout.child.OpenRoot(name)
	if err != nil {
		cleanupErr := a.layout.child.RemoveAll(name)
		return "", stderrors.Join(
			apperrors.Wrap(apperrors.ErrCodeInternal,
				"failed to open OCI recipe materialization root", err),
			wrapRecipeCleanupError(cleanupErr),
		)
	}

	materializeErr := a.extractLayer(ctx, root, a.manifest.Layers[0])
	closeErr := root.Close()
	if closeErr != nil {
		closeErr = apperrors.Wrap(apperrors.ErrCodeInternal,
			"failed to close OCI recipe materialization root", closeErr)
	}
	if materializeErr != nil || closeErr != nil {
		cleanupErr := a.layout.child.RemoveAll(name)
		return "", stderrors.Join(materializeErr, closeErr, wrapRecipeCleanupError(cleanupErr))
	}
	if err := a.layout.validate(); err != nil {
		cleanupErr := a.layout.child.RemoveAll(name)
		return "", stderrors.Join(err, wrapRecipeCleanupError(cleanupErr))
	}
	a.materialized = filepath.Join(a.layout.Path(), name)
	return a.materialized, nil
}

// Close waits for any in-flight authorization or Materialize call, closes
// registry connections, and removes only the unique child owned by this
// artifact. Its checked result is cached for idempotent concurrent calls.
func (a *StagedRecipeArtifact) Close() error {
	if a == nil {
		return nil
	}
	a.closeOnce.Do(func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.closed = true
		if a.remote != nil {
			a.remote.CloseIdleConnections()
		}
		if a.layout != nil {
			a.closeErr = a.layout.Close()
		}
	})
	return a.closeErr
}

// StageRecipeArtifact resolves and downloads one AICR recipe artifact into a
// bounded private OCI CAS. It validates the complete artifact shape and every
// content digest, but deliberately does not verify provenance or materialize
// files; those remain explicit later steps.
func StageRecipeArtifact(ctx context.Context, opts RecipePullOptions) (*StagedRecipeArtifact, error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.OCIRecipePullTimeout)
	defer cancel()
	return stageRecipeArtifactWithDependencies(ctx, opts, defaultRecipePullDependencies())
}

type recipeArtifactRepository interface {
	Resolve(context.Context, string) (ociv1.Descriptor, error)
	Fetch(context.Context, ociv1.Descriptor) (io.ReadCloser, error)
	Referrers(
		context.Context,
		ociv1.Descriptor,
		string,
		func([]ociv1.Descriptor) error,
	) error
	CloseIdleConnections()
}

type remoteRecipeArtifactRepository struct {
	repository *remote.Repository
	client     *http.Client
}

func (r *remoteRecipeArtifactRepository) Resolve(
	ctx context.Context,
	selector string,
) (ociv1.Descriptor, error) {

	return r.repository.Resolve(ctx, selector)
}

func (r *remoteRecipeArtifactRepository) Fetch(
	ctx context.Context,
	descriptor ociv1.Descriptor,
) (io.ReadCloser, error) {

	return r.repository.Fetch(ctx, descriptor)
}

func (r *remoteRecipeArtifactRepository) Referrers(
	ctx context.Context,
	descriptor ociv1.Descriptor,
	artifactType string,
	callback func([]ociv1.Descriptor) error,
) error {

	return r.repository.Referrers(ctx, descriptor, artifactType, callback)
}

func (r *remoteRecipeArtifactRepository) CloseIdleConnections() {
	if r.client != nil {
		r.client.CloseIdleConnections()
	}
}

type recipePullDependencies struct {
	newLayout         func(context.Context, string) (*ownedLayout, error)
	newStore          func(context.Context, *ownedLayout) (*rootOCIStore, error)
	newRepository     func(context.Context, string) (recipeArtifactRepository, error)
	maxAttempts       int
	maxDownloadBytes  int64
	maxRetryTraffic   int64
	initialBackoff    time.Duration
	perAttemptTimeout time.Duration
	waitBackoff       func(context.Context, time.Duration) error
	extract           recipeExtractLimits
}

func defaultRecipePullDependencies() recipePullDependencies {
	return recipePullDependencies{
		newLayout:         newOwnedLayout,
		newStore:          newRootOCIStore,
		newRepository:     newRemoteRecipeArtifactRepository,
		maxAttempts:       defaults.OCIRecipePullRetries,
		maxDownloadBytes:  defaults.MaxOCIRecipeDownloadBytes,
		maxRetryTraffic:   defaults.MaxOCIRecipeRetryTrafficBytes,
		initialBackoff:    defaults.OCIRecipePullBackoff,
		perAttemptTimeout: defaults.OCIRecipePullAttemptTimeout,
		waitBackoff:       waitRecipePullBackoff,
		extract:           defaultRecipeExtractLimits(),
	}
}

func newRemoteRecipeArtifactRepository(
	ctx context.Context,
	repository string,
) (recipeArtifactRepository, error) {

	repo, err := remote.NewRepository(repository)
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
			"failed to initialize OCI recipe repository", err)
	}
	transport := defaults.NewHTTPTransport()
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	client := &http.Client{
		Timeout:   defaults.OCIRecipePullAttemptTimeout,
		Transport: transport,
	}
	authClient := &auth.Client{Client: client, Cache: auth.NewCache()}
	contextErr := recipeContextError(ctx, "OCI recipe repository initialization canceled")
	if contextErr != nil {
		return nil, contextErr
	}
	credentialStore, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	if err != nil {
		return nil, apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
			"failed to load Docker credential configuration", err)
	}
	authClient.Credential = recipeDockerCredential(credentialStore)
	repo.Client = authClient
	repo.ManifestMediaTypes = []string{ociv1.MediaTypeImageManifest}
	repo.MaxMetadataBytes = defaults.MaxOCIRecipeManifestBytes
	return &remoteRecipeArtifactRepository{repository: repo, client: client}, nil
}

// recipeDockerCredential adapts ORAS credential lookup to AICR's structured
// errors without reimplementing Docker config or credential-helper behavior.
// Docker config and helpers are trusted operator configuration, so a lookup
// failure is a deterministic InvalidRequest and must not consume registry
// retries. A missing credential is returned as auth.EmptyCredential; if the
// registry requires Basic auth, ORAS emits auth.ErrBasicCredentialNotFound and
// classifyRecipePullFailure maps that distinct condition to Unauthorized.
func recipeDockerCredential(store credentials.Store) auth.CredentialFunc {
	credential := credentials.Credential(store)
	return func(ctx context.Context, hostport string) (auth.Credential, error) {
		resolved, err := credential(ctx, hostport)
		if err == nil {
			return resolved, nil
		}
		if contextErr := recipeContextError(ctx,
			"Docker credential resolution canceled"); contextErr != nil {
			return auth.EmptyCredential, contextErr
		}
		return auth.EmptyCredential, apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
			"failed to resolve Docker registry credentials", err)
	}
}

func stageRecipeArtifactWithDependencies(
	ctx context.Context,
	opts RecipePullOptions,
	deps recipePullDependencies,
) (_ *StagedRecipeArtifact, retErr error) {

	validated, err := validateRecipePullOptions(opts)
	if err != nil {
		return nil, err
	}
	if contextErr := recipeContextError(ctx, "OCI recipe staging canceled"); contextErr != nil {
		return nil, contextErr
	}
	parent := opts.TempDir
	if parent == "" {
		parent = os.TempDir()
	}
	layout, err := deps.newLayout(ctx, parent)
	if err != nil {
		return nil, err
	}
	keepLayout := false
	defer func() {
		if keepLayout {
			return
		}
		if closeErr := layout.Close(); closeErr != nil {
			retErr = stderrors.Join(retErr, closeErr)
		}
	}()
	store, err := deps.newStore(ctx, layout)
	if err != nil {
		return nil, apperrors.PropagateOrWrap(err, apperrors.ErrCodeInternal,
			"failed to create private OCI recipe content store")
	}
	repo, err := deps.newRepository(ctx, validated.normalized)
	if err != nil {
		return nil, err
	}
	keepRepository := false
	defer func() {
		if !keepRepository {
			repo.CloseIdleConnections()
		}
	}()

	descriptor, manifest, err := pullRecipeGraphWithRetry(
		ctx, repo, store, opts.Selector, validated.selectorDigest, deps)
	if err != nil {
		return nil, err
	}
	if err := store.Tag(ctx, descriptor, recipeResolvedTag); err != nil {
		return nil, apperrors.PropagateOrWrap(err, apperrors.ErrCodeInternal,
			"failed to record resolved OCI recipe manifest")
	}
	if err := layout.validate(); err != nil {
		return nil, err
	}

	artifact := &StagedRecipeArtifact{
		layout:       layout,
		store:        store,
		remote:       repo,
		descriptor:   immutableDescriptor(descriptor),
		manifest:     cloneRecipeManifest(manifest),
		selectorHash: validated.selectorDigest,
		reference:    validated.normalized + "@" + descriptor.Digest.String(),
		registry:     validated.registry,
		repository:   validated.repository,
		extract:      deps.extract,
	}
	keepLayout = true
	keepRepository = true
	return artifact, nil
}

type recipePullState struct {
	descriptor     ociv1.Descriptor
	manifest       ociv1.Manifest
	manifestStaged bool
	configStaged   bool
	layerStaged    bool
}

// pullRecipeGraphWithRetry deliberately uses the ORAS repository primitives
// instead of oras.CopyGraph. The recipe contract requires sequential root
// validation before successor downloads and actual-byte accounting across
// retries. Expressing those policies through CopyGraph would still require a
// custom source wrapper and FindSuccessors implementation. ORAS owns registry
// resolution, fetch transport, authentication, and content verification here.
func pullRecipeGraphWithRetry(
	ctx context.Context,
	repository recipeArtifactRepository,
	store *rootOCIStore,
	selector string,
	digestSelector digest.Digest,
	deps recipePullDependencies,
) (ociv1.Descriptor, ociv1.Manifest, error) {

	attempts := max(deps.maxAttempts, 1)
	state := recipePullState{}
	traffic := recipeDownloadBudget{limit: deps.maxRetryTraffic}
	backoff := deps.initialBackoff
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := recipeContextError(ctx, "OCI recipe pull canceled"); err != nil {
			return ociv1.Descriptor{}, ociv1.Manifest{}, err
		}
		download := recipeDownloadBudget{limit: deps.maxDownloadBytes}
		lastErr = runRecipePullAttempt(ctx, deps.perAttemptTimeout, func(attemptCtx context.Context) error {
			return pullRecipeGraphAttempt(
				attemptCtx, repository, store, selector, digestSelector, &state,
				&download, &traffic, deps.maxDownloadBytes)
		})
		if ctx.Err() != nil {
			return ociv1.Descriptor{}, ociv1.Manifest{},
				recipeContextError(ctx, "OCI recipe pull canceled")
		}
		if lastErr == nil {
			return immutableDescriptor(state.descriptor), cloneRecipeManifest(state.manifest), nil
		}
		if !isTransientRecipePullError(lastErr) {
			return ociv1.Descriptor{}, ociv1.Manifest{}, classifyRecipePullFailure(lastErr, false)
		}
		if attempt == attempts {
			break
		}
		slog.Debug("retrying OCI recipe pull after transient failure",
			"attempt", attempt,
			"nextAttempt", attempt+1,
			"maxAttempts", attempts,
			"backoff", backoff,
			"error", lastErr,
		)
		if err := deps.waitBackoff(ctx, backoff); err != nil {
			return ociv1.Descriptor{}, ociv1.Manifest{}, err
		}
		backoff *= 2
	}
	return ociv1.Descriptor{}, ociv1.Manifest{}, classifyRecipePullFailure(lastErr, true)
}

// runRecipePullAttempt bounds one attempt without retroactively replacing its
// completed result when the deadline expires concurrently with the return.
func runRecipePullAttempt(
	ctx context.Context,
	timeout time.Duration,
	attempt func(context.Context) error,
) error {

	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return attempt(attemptCtx)
}

func pullRecipeGraphAttempt(
	ctx context.Context,
	repository recipeArtifactRepository,
	store *rootOCIStore,
	selector string,
	digestSelector digest.Digest,
	state *recipePullState,
	download *recipeDownloadBudget,
	traffic *recipeDownloadBudget,
	maxDownloadBytes int64,
) error {

	if state.descriptor.Digest == "" {
		descriptor, err := repository.Resolve(ctx, selector)
		if err != nil {
			return err
		}
		if err := validateRecipeManifestDescriptor(descriptor, digestSelector); err != nil {
			return err
		}
		state.descriptor = immutableDescriptor(descriptor)
	}
	if !state.manifestStaged {
		manifestBytes, err := fetchRecipeManifest(ctx, repository, state.descriptor, traffic)
		if err != nil {
			return err
		}
		manifest, err := validateRecipeManifestWithLimit(manifestBytes, maxDownloadBytes)
		if err != nil {
			return err
		}
		if err := store.Push(ctx, state.descriptor, bytes.NewReader(manifestBytes)); err != nil {
			return apperrors.PropagateOrWrap(err, apperrors.ErrCodeInternal,
				"failed to stage OCI recipe manifest")
		}
		state.manifest = manifest
		state.manifestStaged = true
	}
	if !state.configStaged {
		if err := fetchRecipeBlob(ctx, repository, store, state.manifest.Config,
			maxDownloadBytes, download, traffic, "config"); err != nil {
			return err
		}
		state.configStaged = true
	}
	if !state.layerStaged {
		if err := fetchRecipeBlob(ctx, repository, store, state.manifest.Layers[0],
			defaults.MaxOCIRecipeLayerBytes, download, traffic, "layer"); err != nil {
			return err
		}
		state.layerStaged = true
	}
	return recipeContextError(ctx, "OCI recipe pull canceled")
}

func fetchRecipeManifest(
	ctx context.Context,
	repository recipeArtifactRepository,
	descriptor ociv1.Descriptor,
	traffic *recipeDownloadBudget,
) ([]byte, error) {

	if err := validateRecipeRetryTraffic(descriptor.Size, traffic); err != nil {
		return nil, err
	}
	reader, err := repository.Fetch(ctx, descriptor)
	if err != nil {
		return nil, err
	}
	tracked := newContextReadCloser(ctx, reader)
	limited := &recipeLimitedReader{
		reader:       tracked,
		limit:        defaults.MaxOCIRecipeManifestBytes,
		traffic:      traffic,
		trafficError: errOCIRecipeRetryTrafficLimit,
	}
	data, readErr := content.ReadAll(limited, descriptor)
	closeErr := tracked.Close()
	if readErr != nil {
		return nil, stderrors.Join(
			classifyRecipeContentFailure(ctx, "manifest", readErr),
			recipeResponseCloseError("OCI recipe manifest", closeErr),
		)
	}
	// content.ReadAll consumed through EOF and verified the descriptor. A
	// read-only HTTP response Close failure cannot invalidate those bytes.
	return data, nil
}

func fetchRecipeBlob(
	ctx context.Context,
	repository recipeArtifactRepository,
	store *rootOCIStore,
	descriptor ociv1.Descriptor,
	perBlobLimit int64,
	budget *recipeDownloadBudget,
	traffic *recipeDownloadBudget,
	label string,
) error {

	if err := validateRecipeBlobDescriptor(descriptor, perBlobLimit, budget.remaining()); err != nil {
		return err
	}
	if err := validateRecipeRetryTraffic(descriptor.Size, traffic); err != nil {
		return err
	}
	reader, err := repository.Fetch(ctx, descriptor)
	if err != nil {
		return err
	}
	tracked := newContextReadCloser(ctx, reader)
	limited := &recipeLimitedReader{
		reader:       tracked,
		limit:        perBlobLimit,
		budget:       budget,
		traffic:      traffic,
		trafficError: errOCIRecipeRetryTrafficLimit,
	}
	pushErr := store.Push(ctx, descriptor, limited)
	closeErr := tracked.Close()
	if pushErr != nil {
		return stderrors.Join(
			classifyRecipeContentFailure(ctx, label, pushErr),
			recipeResponseCloseError("OCI recipe "+label, closeErr),
		)
	}
	// Push consumed through EOF, verified the descriptor, synced the writable
	// file, and published it. A response Close failure is cleanup-only now.
	return nil
}

func recipeResponseCloseError(contentLabel string, err error) error {
	if err == nil {
		return nil
	}
	return apperrors.Wrap(apperrors.ErrCodeUnavailable,
		"failed to close "+contentLabel+" response", err)
}

type recipeDownloadBudget struct {
	read  int64
	limit int64
}

func (b *recipeDownloadBudget) remaining() int64 {
	if b == nil {
		return 0
	}
	return b.limit - b.read
}

type recipeLimitedReader struct {
	reader       io.Reader
	limit        int64
	read         int64
	budget       *recipeDownloadBudget
	traffic      *recipeDownloadBudget
	limitError   error
	trafficError error
}

func (r *recipeLimitedReader) Read(p []byte) (int, error) {
	if r.read > r.limit || r.budget != nil && r.budget.read > r.budget.limit ||
		r.traffic != nil && r.traffic.read > r.traffic.limit {

		return 0, r.exceededError()
	}
	remaining := r.limit - r.read
	if r.budget != nil && r.budget.remaining() < remaining {
		remaining = r.budget.remaining()
	}
	if r.traffic != nil && r.traffic.remaining() < remaining {
		remaining = r.traffic.remaining()
	}
	if remaining < 0 {
		return 0, r.exceededError()
	}
	if int64(len(p)) > remaining+1 {
		p = p[:remaining+1]
	}
	n, err := r.reader.Read(p)
	r.read += int64(n)
	if r.budget != nil {
		r.budget.read += int64(n)
	}
	if r.traffic != nil {
		r.traffic.read += int64(n)
	}
	if r.read > r.limit || r.budget != nil && r.budget.read > r.budget.limit ||
		r.traffic != nil && r.traffic.read > r.traffic.limit {

		return n, r.exceededError()
	}
	return n, err
}

func (r *recipeLimitedReader) exceededError() error {
	if r.traffic != nil && r.traffic.read > r.traffic.limit {
		if r.trafficError != nil {
			return r.trafficError
		}
		return errOCIRecipeRetryTrafficLimit
	}
	if r.limitError != nil {
		return r.limitError
	}
	return errOCIRecipeDownloadLimit
}

func validateRecipeRetryTraffic(size int64, traffic *recipeDownloadBudget) error {
	if traffic == nil || size < 0 || size >= traffic.remaining() {
		return apperrors.Wrap(apperrors.ErrCodeUnavailable,
			"OCI recipe retry traffic limit exceeded", errOCIRecipeRetryTrafficLimit)
	}
	return nil
}

func validateRecipePullOptions(
	opts RecipePullOptions,
) (recipePullValidation, error) {

	validated := recipePullValidation{}
	input := opts.Repository
	if input == "" || strings.TrimSpace(input) != input {
		return validated, apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe repository must be non-empty and contain no surrounding whitespace")
	}
	if after, ok := strings.CutPrefix(input, "oci://"); ok {
		input = after
	} else if strings.Contains(input, "://") {
		return validated, apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe repository supports only the optional oci:// scheme")
	}
	named, err := reference.ParseNormalizedNamed(input)
	if err != nil {
		return validated, apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
			"invalid OCI recipe repository", err)
	}
	if !reference.IsNameOnly(named) {
		return validated, apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe repository must not contain a tag or digest")
	}
	validated.registry = reference.Domain(named)
	validated.repository = reference.Path(named)
	if err := validateRegistryReference(validated.registry, validated.repository); err != nil {
		return recipePullValidation{}, err
	}
	if opts.Selector == "" || strings.TrimSpace(opts.Selector) != opts.Selector {
		return recipePullValidation{}, apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe selector is required and must contain no surrounding whitespace")
	}
	if strings.Contains(opts.Selector, ":") {
		validated.selectorDigest = digest.Digest(opts.Selector)
		if validated.selectorDigest.Algorithm() != digest.SHA256 {
			return recipePullValidation{}, apperrors.New(apperrors.ErrCodeInvalidRequest,
				"OCI recipe digest selector must use sha256")
		}
		if err := validated.selectorDigest.Validate(); err != nil {
			return recipePullValidation{}, apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
				"invalid OCI recipe digest selector", err)
		}
	} else if err := validateDistributionTag(opts.Selector); err != nil {
		return recipePullValidation{}, err
	}
	if opts.TempDir != "" && strings.TrimSpace(opts.TempDir) != opts.TempDir {
		return recipePullValidation{}, apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe temporary-directory parent must contain no surrounding whitespace")
	}
	validated.normalized = validated.registry + "/" + validated.repository
	return validated, nil
}

func validateRecipeManifestDescriptor(desc ociv1.Descriptor, selector digest.Digest) error {
	if desc.MediaType != ociv1.MediaTypeImageManifest {
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe source must resolve to an OCI image manifest")
	}
	if desc.Digest.Algorithm() != digest.SHA256 {
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			"resolved OCI recipe manifest must use sha256")
	}
	if err := desc.Digest.Validate(); err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
			"resolved OCI recipe manifest digest is invalid", err)
	}
	if selector != "" && desc.Digest != selector {
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			"resolved OCI recipe manifest does not match digest selector")
	}
	if desc.Size < 0 || desc.Size > defaults.MaxOCIRecipeManifestBytes {
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe manifest exceeds size limit")
	}
	if len(desc.URLs) != 0 || len(desc.Data) != 0 || desc.Platform != nil ||
		desc.ArtifactType != "" && desc.ArtifactType != artifactType {

		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			"resolved OCI recipe manifest descriptor contains unsupported fields")
	}
	return nil
}

func validateRecipeManifest(data []byte) (ociv1.Manifest, error) {
	return validateRecipeManifestWithLimit(data, defaults.MaxOCIRecipeDownloadBytes)
}

func validateRecipeManifestWithLimit(data []byte, maxDownloadBytes int64) (ociv1.Manifest, error) {
	var manifest ociv1.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ociv1.Manifest{}, apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
			"OCI recipe manifest is not valid JSON", err)
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != ociv1.MediaTypeImageManifest {
		return ociv1.Manifest{}, apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe manifest has an unsupported schema or media type")
	}
	if manifest.ArtifactType != artifactType {
		return ociv1.Manifest{}, apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI artifact is not an AICR recipe artifact")
	}
	if manifest.Subject != nil {
		return ociv1.Manifest{}, apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe manifest must not contain a subject")
	}
	if !isCanonicalRecipeConfig(manifest.Config) {
		return ociv1.Manifest{}, apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe manifest config is not AICR's canonical empty config")
	}
	if len(manifest.Layers) != 1 {
		return ociv1.Manifest{}, apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe manifest must contain exactly one layer")
	}
	layer := manifest.Layers[0]
	if layer.MediaType != ociv1.MediaTypeImageLayerGzip {
		return ociv1.Manifest{}, apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe layer must be a gzip-compressed OCI image layer")
	}
	if len(layer.URLs) != 0 || len(layer.Data) != 0 || layer.Platform != nil || layer.ArtifactType != "" {
		return ociv1.Manifest{}, apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe layer descriptor contains unsupported fields")
	}
	validationErr := validateRecipeBlobDescriptor(
		layer, defaults.MaxOCIRecipeLayerBytes, maxDownloadBytes-manifest.Config.Size)
	if validationErr != nil {
		return ociv1.Manifest{}, validationErr
	}
	return cloneRecipeManifest(manifest), nil
}

func isCanonicalRecipeConfig(desc ociv1.Descriptor) bool {
	canonical := ociv1.DescriptorEmptyJSON
	return desc.MediaType == canonical.MediaType && desc.Digest == canonical.Digest &&
		desc.Size == canonical.Size && bytes.Equal(desc.Data, canonical.Data) &&
		len(desc.URLs) == 0 && len(desc.Annotations) == 0 && desc.Platform == nil &&
		desc.ArtifactType == ""
}

func validateRecipeBlobDescriptor(desc ociv1.Descriptor, perBlobLimit, remaining int64) error {
	if desc.Digest.Algorithm() != digest.SHA256 {
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe blob must use sha256")
	}
	if err := desc.Digest.Validate(); err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
			"OCI recipe blob digest is invalid", err)
	}
	if desc.Size < 0 || desc.Size > perBlobLimit || desc.Size > remaining {
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe blob exceeds download size limit")
	}
	return nil
}

func cloneRecipeManifest(manifest ociv1.Manifest) ociv1.Manifest {
	clone := manifest
	clone.Config = cloneRootStoreDescriptor(manifest.Config)
	clone.Layers = make([]ociv1.Descriptor, len(manifest.Layers))
	for i := range manifest.Layers {
		clone.Layers[i] = cloneRootStoreDescriptor(manifest.Layers[i])
	}
	if manifest.Subject != nil {
		subject := cloneRootStoreDescriptor(*manifest.Subject)
		clone.Subject = &subject
	}
	if manifest.Annotations != nil {
		clone.Annotations = make(map[string]string, len(manifest.Annotations))
		maps.Copy(clone.Annotations, manifest.Annotations)
	}
	return clone
}

func classifyRecipeContentFailure(ctx context.Context, label string, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return recipeContextError(ctx, "OCI recipe "+label+" download canceled")
	}
	if stderrors.Is(err, errOCIRecipeDownloadLimit) {
		return apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
			"OCI recipe "+label+" exceeds download size limit", err)
	}
	if stderrors.Is(err, errOCIRecipeRetryTrafficLimit) {
		return apperrors.Wrap(apperrors.ErrCodeUnavailable,
			"OCI recipe retry traffic limit exceeded", err)
	}
	if stderrors.Is(err, io.ErrUnexpectedEOF) {
		return apperrors.Wrap(apperrors.ErrCodeUnavailable,
			"OCI recipe "+label+" download was interrupted", err)
	}
	if stderrors.Is(err, content.ErrInvalidDescriptorSize) ||
		stderrors.Is(err, content.ErrMismatchedDigest) ||
		stderrors.Is(err, content.ErrTrailingData) {

		return apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
			"OCI recipe "+label+" content does not match its descriptor", err)
	}
	return err
}

func isTransientRecipePullError(err error) bool {
	if structured, ok := stderrors.AsType[*apperrors.StructuredError](err); ok {
		switch structured.Code {
		case apperrors.ErrCodeTimeout, apperrors.ErrCodeUnavailable,
			apperrors.ErrCodeRateLimitExceeded:
			return true
		case apperrors.ErrCodeNotFound, apperrors.ErrCodeUnauthorized,
			apperrors.ErrCodeInternal, apperrors.ErrCodeInvalidRequest,
			apperrors.ErrCodeMethodNotAllowed, apperrors.ErrCodeConflict,
			apperrors.ErrCodeCanceled:
			return false
		default:
			return false
		}
	}
	return isTransientPushError(err) || apperrors.IsTransient(err)
}

func classifyRecipePullFailure(err error, exhausted bool) error {
	if err == nil {
		return nil
	}
	if _, ok := stderrors.AsType[*apperrors.StructuredError](err); ok {
		if exhausted && isTransientRecipePullError(err) {
			return apperrors.Wrap(apperrors.ErrCodeUnavailable,
				"OCI recipe pull failed after retries", err)
		}
		return err
	}
	if response, ok := stderrors.AsType[*errcode.ErrorResponse](err); ok {
		var code apperrors.ErrorCode
		switch response.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			code = apperrors.ErrCodeUnauthorized
		case http.StatusNotFound:
			code = apperrors.ErrCodeNotFound
		case http.StatusTooManyRequests:
			code = apperrors.ErrCodeRateLimitExceeded
		default:
			switch {
			case response.StatusCode >= 500:
				code = apperrors.ErrCodeUnavailable
			case response.StatusCode >= 400:
				code = apperrors.ErrCodeInvalidRequest
			default:
				code = apperrors.ErrCodeInternal
			}
		}
		return apperrors.Wrap(code, "OCI recipe pull failed", err)
	}
	if stderrors.Is(err, errdef.ErrNotFound) {
		return apperrors.Wrap(apperrors.ErrCodeNotFound, "OCI recipe artifact was not found", err)
	}
	if stderrors.Is(err, auth.ErrBasicCredentialNotFound) {
		return apperrors.Wrap(apperrors.ErrCodeUnauthorized,
			"OCI registry credentials are required", err)
	}
	if isRemoteDigestHeaderMismatch(err) {
		return apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
			"OCI recipe selector response digest does not match the requested digest", err)
	}
	if exhausted && isTransientRecipePullError(err) {
		return apperrors.Wrap(apperrors.ErrCodeUnavailable,
			"OCI recipe pull failed after retries", err)
	}
	if isTransientRecipePullError(err) {
		return apperrors.Wrap(apperrors.ErrCodeUnavailable, "OCI recipe pull failed", err)
	}
	return apperrors.Wrap(apperrors.ErrCodeInternal, "OCI recipe pull failed", err)
}

// isRemoteDigestHeaderMismatch recognizes the exact uncoded error shape emitted
// by remote.verifyContentDigest in the pinned oras-go v2.6.2. oras-go does not
// expose a type or sentinel for this condition, so dependency upgrades must
// re-verify this message grammar. Validate the complete shape (method, HTTPS
// URL, field name, and two valid sha256 digests) instead of classifying an
// arbitrary substring match.
func isRemoteDigestHeaderMismatch(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	if !strings.HasPrefix(message, `HEAD "https://`) &&
		!strings.HasPrefix(message, `GET "https://`) {

		return false
	}
	const marker = `: invalid response; digest mismatch in Docker-Content-Digest: received "`
	markerIndex := strings.Index(message, marker)
	if markerIndex < 0 || message[markerIndex-1] != '"' {
		return false
	}
	received, expected, ok := strings.Cut(message[markerIndex+len(marker):], `" when expecting "`)
	if !ok || !strings.HasSuffix(expected, `"`) {
		return false
	}
	expected = strings.TrimSuffix(expected, `"`)
	return isValidSHA256Digest(received) && isValidSHA256Digest(expected)
}

func isValidSHA256Digest(value string) bool {
	parsed := digest.Digest(value)
	return parsed.Algorithm() == digest.SHA256 && parsed.Validate() == nil
}

func waitRecipePullBackoff(ctx context.Context, backoff time.Duration) error {
	timer := time.NewTimer(jitterDuration(backoff))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return recipeContextError(ctx, "OCI recipe retry canceled")
	case <-timer.C:
		return nil
	}
}

type recipeExtractLimits struct {
	maxTotal int64
	maxFile  int64
	maxFiles int
}

func defaultRecipeExtractLimits() recipeExtractLimits {
	return recipeExtractLimits{
		maxTotal: defaults.MaxOCIRecipeExtractedBytes,
		maxFile:  defaults.MaxOCIRecipeFileBytes,
		maxFiles: defaults.MaxOCIRecipeFiles,
	}
}

func (a *StagedRecipeArtifact) extractLayer(
	ctx context.Context,
	root *os.Root,
	descriptor ociv1.Descriptor,
) error {

	reader, err := a.store.Fetch(ctx, descriptor)
	if err != nil {
		return apperrors.PropagateOrWrap(err, apperrors.ErrCodeInternal,
			"failed to open staged OCI recipe layer")
	}
	tracked := newContextReadCloser(ctx, reader)
	verifyReader := content.NewVerifyReader(tracked, descriptor)
	extractErr := extractRecipeArchive(ctx, verifyReader, root, a.extract)
	if extractErr == nil {
		extractErr = verifyReader.Verify()
		if extractErr != nil {
			extractErr = apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
				"staged OCI recipe layer no longer matches its descriptor", extractErr)
		}
	}
	closeErr := tracked.Close()
	if closeErr != nil {
		closeErr = apperrors.Wrap(apperrors.ErrCodeInternal,
			"failed to close staged OCI recipe layer", closeErr)
	}
	return stderrors.Join(extractErr, closeErr)
}

type recipeArchiveNode uint8

const (
	recipeArchiveDirectory recipeArchiveNode = iota + 1
	recipeArchiveFile
)

type recipeArchiveState struct {
	root      *os.Root
	limits    recipeExtractLimits
	nodes     map[string]recipeArchiveNode
	explicit  map[string]struct{}
	total     int64
	nodeCount int
}

func extractRecipeArchive(
	ctx context.Context,
	compressed io.Reader,
	root *os.Root,
	limits recipeExtractLimits,
) error {

	if limits.maxTotal <= 0 || limits.maxFile <= 0 || limits.maxFiles <= 0 {
		return apperrors.New(apperrors.ErrCodeInternal,
			"OCI recipe extraction limits must be positive")
	}
	buffered := bufio.NewReader(compressed)
	gzipReader, err := gzip.NewReader(buffered)
	if err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
			"OCI recipe layer is not valid gzip", err)
	}
	gzipReader.Multistream(false)
	expanded := &recipeLimitedReader{
		reader:     gzipReader,
		limit:      limits.maxTotal,
		limitError: errOCIRecipeExpandedLimit,
	}
	tarReader := tar.NewReader(expanded)
	state := recipeArchiveState{
		root:     root,
		limits:   limits,
		nodes:    make(map[string]recipeArchiveNode),
		explicit: make(map[string]struct{}),
	}
	for {
		if err := recipeContextError(ctx, "OCI recipe extraction canceled"); err != nil {
			_ = gzipReader.Close()
			return err
		}
		header, nextErr := tarReader.Next()
		if stderrors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			_ = gzipReader.Close()
			if stderrors.Is(nextErr, errOCIRecipeExpandedLimit) {
				return expandedArchiveLimitError(nextErr)
			}
			return apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
				"OCI recipe layer contains a malformed tar archive", nextErr)
		}
		if err := state.extractEntry(ctx, tarReader, header); err != nil {
			_ = gzipReader.Close()
			return err
		}
	}
	if _, err := io.Copy(io.Discard, expanded); err != nil {
		_ = gzipReader.Close()
		if stderrors.Is(err, errOCIRecipeExpandedLimit) {
			return expandedArchiveLimitError(err)
		}
		return apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
			"OCI recipe gzip stream is truncated", err)
	}
	if err := gzipReader.Close(); err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
			"failed to close OCI recipe gzip reader", err)
	}
	if _, err := buffered.Peek(1); !stderrors.Is(err, io.EOF) {
		if err != nil {
			return apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
				"failed to inspect OCI recipe gzip trailer", err)
		}
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe layer contains trailing or concatenated gzip data")
	}
	return nil
}

func (s *recipeArchiveState) extractEntry(
	ctx context.Context,
	tarReader *tar.Reader,
	header *tar.Header,
) error {

	rel, err := validateRecipeArchivePath(header.Name, header.Typeflag == tar.TypeDir)
	if err != nil {
		return err
	}
	if _, duplicate := s.explicit[rel]; duplicate {
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe layer contains duplicate path "+header.Name)
	}
	s.explicit[rel] = struct{}{}
	if hasSparseMetadata(header) {
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe layer contains an unsupported sparse entry")
	}
	switch header.Typeflag {
	case tar.TypeDir:
		return s.extractDirectory(rel, header.Name)
	case tar.TypeReg:
		return s.extractFile(ctx, tarReader, rel, header)
	default:
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe layer contains an unsupported archive entry type")
	}
}

func (s *recipeArchiveState) extractDirectory(rel, archiveName string) error {
	if err := ensureRecipeArchiveParents(
		s.root, rel, s.nodes, &s.nodeCount, s.limits.maxFiles); err != nil {
		return err
	}
	if s.nodes[rel] == recipeArchiveFile {
		return archiveTypeConflict(archiveName)
	}
	if s.nodes[rel] == 0 {
		if err := addRecipeArchiveNode(&s.nodeCount, s.limits.maxFiles); err != nil {
			return err
		}
		if err := s.root.Mkdir(rel, 0o700); err != nil {
			return apperrors.Wrap(apperrors.ErrCodeInternal,
				"failed to create OCI recipe directory", err)
		}
		s.nodes[rel] = recipeArchiveDirectory
	}
	if err := s.root.Chmod(rel, 0o700); err != nil {
		return apperrors.Wrap(apperrors.ErrCodeInternal,
			"failed to secure OCI recipe directory", err)
	}
	return nil
}

func (s *recipeArchiveState) extractFile(
	ctx context.Context,
	tarReader *tar.Reader,
	rel string,
	header *tar.Header,
) error {

	if s.nodes[rel] != 0 {
		return archiveTypeConflict(header.Name)
	}
	if header.Size < 0 || header.Size > s.limits.maxFile ||
		header.Size > s.limits.maxTotal-s.total {

		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe archive entry exceeds extracted size limit")
	}
	if err := ensureRecipeArchiveParents(
		s.root, rel, s.nodes, &s.nodeCount, s.limits.maxFiles); err != nil {
		return err
	}
	if err := addRecipeArchiveNode(&s.nodeCount, s.limits.maxFiles); err != nil {
		return err
	}
	written, err := writeRecipeArchiveFile(ctx, s.root, rel, tarReader, header.Size)
	if err != nil {
		return err
	}
	s.total += written
	s.nodes[rel] = recipeArchiveFile
	return nil
}

func validateRecipeArchivePath(name string, directory bool) (string, error) {
	if name == "" || strings.ContainsRune(name, '\x00') || strings.Contains(name, `\`) {
		return "", apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe layer contains an unsafe archive path")
	}
	cleanName := name
	if directory {
		cleanName = strings.TrimSuffix(cleanName, "/")
	}
	if cleanName == "" || path.Clean(cleanName) != cleanName {
		return "", apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe layer contains a non-canonical archive path")
	}
	rel := filepath.FromSlash(cleanName)
	if !filepath.IsLocal(rel) {
		return "", apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe layer contains a non-local archive path")
	}
	for component := range strings.SplitSeq(cleanName, "/") {
		if component == "" || component == "." || component == ".." {
			return "", apperrors.New(apperrors.ErrCodeInvalidRequest,
				"OCI recipe layer contains an unsafe archive path component")
		}
	}
	return rel, nil
}

func ensureRecipeArchiveParents(
	root *os.Root,
	rel string,
	nodes map[string]recipeArchiveNode,
	nodeCount *int,
	maxNodes int,
) error {

	parent := filepath.Dir(rel)
	if parent == "." {
		return nil
	}
	current := ""
	for component := range strings.SplitSeq(filepath.ToSlash(parent), "/") {
		current = filepath.Join(current, component)
		if nodes[current] == recipeArchiveFile {
			return archiveTypeConflict(rel)
		}
		if nodes[current] == 0 {
			if err := addRecipeArchiveNode(nodeCount, maxNodes); err != nil {
				return err
			}
			if err := root.Mkdir(current, 0o700); err != nil && !stderrors.Is(err, fs.ErrExist) {
				return apperrors.Wrap(apperrors.ErrCodeInternal,
					"failed to create OCI recipe parent directory", err)
			}
			if err := root.Chmod(current, 0o700); err != nil {
				return apperrors.Wrap(apperrors.ErrCodeInternal,
					"failed to secure OCI recipe parent directory", err)
			}
			nodes[current] = recipeArchiveDirectory
		}
	}
	return nil
}

func addRecipeArchiveNode(count *int, max int) error {
	if *count >= max {
		return apperrors.New(apperrors.ErrCodeInvalidRequest,
			"OCI recipe archive exceeds filesystem-node limit")
	}
	*count++
	return nil
}

func expandedArchiveLimitError(cause error) error {
	return apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
		"OCI recipe archive exceeds expanded size limit", cause)
}

func writeRecipeArchiveFile(
	ctx context.Context,
	root *os.Root,
	rel string,
	reader io.Reader,
	size int64,
) (written int64, retErr error) {

	file, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, apperrors.Wrap(apperrors.ErrCodeInternal,
			"failed to create OCI recipe file", err)
	}
	closed := false
	defer func() {
		if !closed {
			if closeErr := file.Close(); closeErr != nil {
				retErr = stderrors.Join(retErr, apperrors.Wrap(apperrors.ErrCodeInternal,
					"failed to close partial OCI recipe file", closeErr))
			}
		}
		if retErr != nil {
			if removeErr := root.Remove(rel); removeErr != nil && !stderrors.Is(removeErr, fs.ErrNotExist) {
				retErr = stderrors.Join(retErr, apperrors.Wrap(apperrors.ErrCodeInternal,
					"failed to remove partial OCI recipe file", removeErr))
			}
		}
	}()
	buffer := make([]byte, copyBufferSize)
	remaining := size
	for remaining > 0 {
		if err := recipeContextError(ctx, "OCI recipe file extraction canceled"); err != nil {
			return written, err
		}
		chunk := min(remaining, int64(len(buffer)))
		n, readErr := reader.Read(buffer[:chunk])
		if n > 0 {
			writeN, writeErr := file.Write(buffer[:n])
			written += int64(writeN)
			remaining -= int64(writeN)
			if writeErr != nil {
				return written, apperrors.Wrap(apperrors.ErrCodeInternal,
					"failed to write OCI recipe file", writeErr)
			}
			if writeN != n {
				return written, apperrors.Wrap(apperrors.ErrCodeInternal,
					"failed to write complete OCI recipe file", io.ErrShortWrite)
			}
		}
		if readErr != nil && remaining > 0 {
			if stderrors.Is(readErr, io.EOF) {
				readErr = io.ErrUnexpectedEOF
			}
			return written, apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
				"OCI recipe archive file is truncated", readErr)
		}
		if n == 0 {
			return written, apperrors.Wrap(apperrors.ErrCodeInvalidRequest,
				"OCI recipe archive file made no read progress", io.ErrNoProgress)
		}
	}
	if err := file.Sync(); err != nil {
		return written, apperrors.Wrap(apperrors.ErrCodeInternal,
			"failed to sync OCI recipe file", err)
	}
	if err := file.Chmod(0o600); err != nil {
		return written, apperrors.Wrap(apperrors.ErrCodeInternal,
			"failed to secure OCI recipe file", err)
	}
	if err := file.Close(); err != nil {
		closed = true
		return written, apperrors.Wrap(apperrors.ErrCodeInternal,
			"failed to close OCI recipe file", err)
	}
	closed = true
	return written, nil
}

func hasSparseMetadata(header *tar.Header) bool {
	for key := range header.PAXRecords {
		if strings.HasPrefix(key, "GNU.sparse.") || key == "SCHILY.filetype" &&
			header.PAXRecords[key] == "sparse" {

			return true
		}
	}
	return false
}

func archiveTypeConflict(name string) error {
	return apperrors.New(apperrors.ErrCodeInvalidRequest,
		"OCI recipe layer contains a file/directory type conflict at "+name)
}

func wrapRecipeCleanupError(err error) error {
	if err == nil || stderrors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return apperrors.Wrap(apperrors.ErrCodeInternal,
		"failed to remove partial OCI recipe materialization", err)
}

func recipeContextError(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		if stderrors.Is(err, context.Canceled) {
			return apperrors.Wrap(apperrors.ErrCodeCanceled, message, err)
		}
		return apperrors.Wrap(apperrors.ErrCodeTimeout, message, err)
	}
	return nil
}
