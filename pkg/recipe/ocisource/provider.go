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

// Package ocisource provides an owned recipe.DataProvider backed by one
// immutable-digest-selected OCI recipe artifact.
package ocisource

import (
	"context"
	stderrors "errors"
	"io"
	"io/fs"
	"sync"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	apperrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/oci"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const closedSource = "OCI recipe source (closed)"

// Config configures one OCI-backed recipe provider. PullOptions must identify
// the artifact with an immutable sha256 manifest digest.
type Config struct {
	PullOptions oci.RecipePullOptions
}

// Provider owns one staged OCI recipe artifact and delegates recipe reads to
// a standard LayeredDataProvider. The pointer is deliberately the cache key
// exposed to recipe: pointer dynamic values are comparable, and each Provider
// therefore receives isolated metadata, component, and criteria caches.
type Provider struct {
	mu       sync.Mutex
	inflight sync.WaitGroup

	delegate recipe.DataProvider
	artifact stagedRecipeArtifact
	closed   bool

	closeOnce sync.Once
	closeErr  error
}

type stagedRecipeArtifact interface {
	AuthorizeDigestMaterialization(context.Context) error
	Materialize(context.Context) (string, error)
	Close() error
}

type dependencies struct {
	stage           func(context.Context, oci.RecipePullOptions) (stagedRecipeArtifact, error)
	newLayered      func(*recipe.EmbeddedDataProvider, recipe.LayeredProviderConfig) (recipe.DataProvider, error)
	validateCatalog func(context.Context, recipe.DataProvider) error
	timeout         time.Duration
}

var (
	_ recipe.DataProvider  = (*Provider)(nil)
	_ io.Closer            = (*Provider)(nil)
	_ stagedRecipeArtifact = (*oci.StagedRecipeArtifact)(nil)
)

// New stages, digest-authorizes, and materializes one OCI artifact, then
// validates and exposes it as an overlay over embedded. One shared operation
// deadline bounded by defaults.OCIRecipeConstructionTimeout covers every
// phase; nested low-level phase deadlines can only shorten it.
func New(
	ctx context.Context,
	embedded *recipe.EmbeddedDataProvider,
	config Config,
) (*Provider, error) {

	return newWithDependencies(ctx, embedded, config, defaultDependencies())
}

func defaultDependencies() dependencies {
	return dependencies{
		stage: func(
			ctx context.Context,
			opts oci.RecipePullOptions,
		) (stagedRecipeArtifact, error) {

			return oci.StageRecipeArtifact(ctx, opts)
		},
		newLayered: func(
			embedded *recipe.EmbeddedDataProvider,
			config recipe.LayeredProviderConfig,
		) (recipe.DataProvider, error) {

			return recipe.NewLayeredDataProvider(embedded, config)
		},
		validateCatalog: validateCatalog,
		timeout:         defaults.OCIRecipeConstructionTimeout,
	}
}

func newWithDependencies(
	ctx context.Context,
	embedded *recipe.EmbeddedDataProvider,
	config Config,
	deps dependencies,
) (*Provider, error) {

	if ctx == nil {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest,
			"context is required for OCI recipe source construction")
	}
	if embedded == nil {
		return nil, apperrors.New(apperrors.ErrCodeInvalidRequest,
			"embedded recipe data provider is required")
	}
	if err := validateDependencies(deps); err != nil {
		return nil, err
	}

	operationCtx, cancel := context.WithTimeout(ctx, deps.timeout)
	defer cancel()

	artifact, stageErr := deps.stage(operationCtx, config.PullOptions)
	if stageErr != nil {
		return nil, joinArtifactCleanup(stageErr, artifact)
	}
	if artifact == nil {
		return nil, apperrors.New(apperrors.ErrCodeInternal,
			"OCI recipe staging returned an incomplete artifact")
	}
	if contextErr := operationContextError(operationCtx); contextErr != nil {
		return nil, joinArtifactCleanup(contextErr, artifact)
	}

	if authorizationErr := artifact.AuthorizeDigestMaterialization(operationCtx); authorizationErr != nil {
		return nil, joinArtifactCleanup(authorizationErr, artifact)
	}
	if contextErr := operationContextError(operationCtx); contextErr != nil {
		return nil, joinArtifactCleanup(contextErr, artifact)
	}

	materialized, materializeErr := artifact.Materialize(operationCtx)
	if materializeErr != nil {
		return nil, joinArtifactCleanup(materializeErr, artifact)
	}
	if contextErr := operationContextError(operationCtx); contextErr != nil {
		return nil, joinArtifactCleanup(contextErr, artifact)
	}
	if materialized == "" {
		return nil, joinArtifactCleanup(
			apperrors.New(apperrors.ErrCodeInternal,
				"OCI recipe materialization returned an empty directory"), artifact)
	}

	delegate, layeredErr := deps.newLayered(embedded, recipe.LayeredProviderConfig{
		ExternalDir:   materialized,
		MaxFileSize:   defaults.MaxOCIRecipeFileBytes,
		AllowSymlinks: false,
	})
	if layeredErr != nil {
		return nil, joinArtifactCleanup(
			apperrors.PropagateOrWrap(layeredErr, apperrors.ErrCodeInvalidRequest,
				"invalid materialized OCI recipe catalog"), artifact)
	}
	if delegate == nil {
		return nil, joinArtifactCleanup(
			apperrors.New(apperrors.ErrCodeInternal,
				"layered OCI recipe data provider is incomplete"), artifact)
	}

	provider := &Provider{delegate: delegate, artifact: artifact}
	if validationErr := deps.validateCatalog(operationCtx, provider); validationErr != nil {
		primary := operationContextError(operationCtx)
		if primary == nil {
			primary = apperrors.PropagateOrWrap(validationErr, apperrors.ErrCodeInvalidRequest,
				"invalid materialized OCI recipe catalog")
		}
		return nil, stderrors.Join(primary, provider.Close())
	}
	if err := operationContextError(operationCtx); err != nil {
		return nil, stderrors.Join(err, provider.Close())
	}
	return provider, nil
}

func validateDependencies(deps dependencies) error {
	if deps.stage == nil || deps.newLayered == nil || deps.validateCatalog == nil {
		return apperrors.New(apperrors.ErrCodeInternal,
			"OCI recipe source constructor dependencies are incomplete")
	}
	if deps.timeout <= 0 {
		return apperrors.New(apperrors.ErrCodeInternal,
			"OCI recipe source construction timeout must be positive")
	}
	return nil
}

func validateCatalog(ctx context.Context, provider recipe.DataProvider) error {
	// Force the layered provider's special registry merge before activation.
	// This rejects malformed external registry YAML in addition to the required
	// file check performed by NewLayeredDataProvider.
	if _, err := provider.ReadFile(ctx, recipe.RegistryFileName); err != nil {
		return apperrors.PropagateOrWrap(err, apperrors.ErrCodeInvalidRequest,
			"invalid OCI recipe component registry")
	}
	if _, err := recipe.LoadMetadataStoreFor(ctx, provider); err != nil {
		return apperrors.PropagateOrWrap(err, apperrors.ErrCodeInvalidRequest,
			"invalid OCI recipe metadata catalog")
	}
	return nil
}

func operationContextError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		if stderrors.Is(err, context.Canceled) {
			return apperrors.Wrap(apperrors.ErrCodeCanceled,
				"OCI recipe source construction canceled", err)
		}
		return apperrors.Wrap(apperrors.ErrCodeTimeout,
			"OCI recipe source construction timed out", err)
	}
	return nil
}

func joinArtifactCleanup(primary error, artifact stagedRecipeArtifact) error {
	if artifact == nil {
		return primary
	}
	cleanupErr := artifact.Close()
	if cleanupErr == nil {
		return primary
	}
	return stderrors.Join(primary, apperrors.PropagateOrWrap(
		cleanupErr, apperrors.ErrCodeInternal,
		"failed to clean up staged OCI recipe artifact"))
}

func (p *Provider) beginIO() (recipe.DataProvider, error) {
	if p == nil {
		return nil, providerUnavailableError()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.delegate == nil {
		return nil, providerUnavailableError()
	}
	p.inflight.Add(1)
	return p.delegate, nil
}

// ReadFile delegates to the standard layered provider while the owned
// materialization is open.
func (p *Provider) ReadFile(ctx context.Context, path string) ([]byte, error) {
	delegate, err := p.beginIO()
	if err != nil {
		return nil, err
	}
	defer p.inflight.Done()
	return delegate.ReadFile(ctx, path)
}

// WalkDir delegates to the standard layered provider while the owned
// materialization is open. The provider mutex is not held across callbacks;
// Close coordinates with the in-flight registration instead.
func (p *Provider) WalkDir(ctx context.Context, root string, fn fs.WalkDirFunc) error {
	delegate, err := p.beginIO()
	if err != nil {
		return err
	}
	defer p.inflight.Done()
	return delegate.WalkDir(ctx, root, fn)
}

// Source delegates provenance to the layered provider while open and returns
// a stable description after the materialized workspace has been closed.
func (p *Provider) Source(path string) string {
	if p == nil {
		return closedSource
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.delegate == nil {
		return closedSource
	}
	return p.delegate.Source(path)
}

// Close drains in-flight recipe reads and walks, evicts every cache keyed by
// this Provider, and releases only the private artifact child it owns. The
// checked result is cached for safe concurrent, idempotent use.
func (p *Provider) Close() error {
	if p == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()

		p.inflight.Wait()
		recipe.EvictCachedStore(p)
		recipe.EvictCachedRegistry(p)
		recipe.EvictCachedCriteriaRegistry(p)

		p.mu.Lock()
		artifact := p.artifact
		p.artifact = nil
		p.delegate = nil
		p.mu.Unlock()
		if artifact != nil {
			p.closeErr = apperrors.PropagateOrWrap(artifact.Close(),
				apperrors.ErrCodeInternal, "failed to close OCI recipe source")
		}
	})
	return p.closeErr
}

func providerUnavailableError() error {
	return apperrors.New(apperrors.ErrCodeUnavailable,
		"OCI recipe data provider is closed")
}
