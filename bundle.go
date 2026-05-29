// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package aicr

import (
	"context"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler"
	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// BundleConfig is the bundler configuration — deployer mode, value
// overrides, node selectors, tolerations, vendoring, app/chart names,
// etc. Transparent alias of pkg/bundler/config.Config (the alias is
// tracked by #1078, matching the Recipe / AllowLists pattern). Construct
// one with config.NewConfig(config.WithDeployer(...), ...) — the same
// builder the CLI bundle command and the REST /v1/bundle handler use, so
// MakeBundle reproduces their exact output byte-for-byte.
type BundleConfig = config.Config

// BundleAttester signs bundle content. Transparent alias of
// pkg/bundler/attestation.Attester. The zero value of BundleOptions
// leaves this nil, in which case MakeBundle uses the bundler's
// no-op attester (the same default bundler.New applies when --attest
// is not set).
type BundleAttester = attestation.Attester

// BundleArtifact summarizes a completed bundle generation: file count,
// total size, duration, per-bundler results, and the output directory
// the files were written to. Transparent alias of
// pkg/bundler/result.Output (#1078 wraps it). Inspect HasErrors() for
// non-fatal per-bundler failures; the bundle files themselves are on
// disk under OutputDir.
type BundleArtifact = *result.Output

// BundleOptions configures a MakeBundle call. It mirrors exactly what
// bundler.New / (*DefaultBundler).Make accept so the facade reproduces
// the same full deployer-mode bundle artifact the CLI bundle command
// and REST /v1/bundle handler produce today.
type BundleOptions struct {
	// Config carries the bundler configuration (deployer mode, value
	// overrides, node selectors/tolerations, vendoring, app/chart
	// names). When nil, MakeBundle uses config.NewConfig() — the same
	// default bundler.New applies (Helm deployer, no overrides).
	Config *BundleConfig

	// Attester signs bundle content. When nil, MakeBundle uses the
	// no-op attester (matching bundler.New's default when --attest is
	// not set). The CLI builds this via attestation.ResolveAttesterLazy
	// when --attest is passed.
	Attester BundleAttester

	// OutputDir is the directory bundle files are written to. Empty
	// means the current directory ("."), matching Make's default.
	OutputDir string

	// Timeout optionally caps the bundle run. When > 0, MakeBundle wraps
	// the caller's context with context.WithTimeout(ctx, Timeout) so the
	// run is bounded by the smaller of this and any tighter parent
	// deadline. When 0 (the zero value), MakeBundle imposes NO
	// facade-level deadline and runs under the caller's ctx as-is —
	// large bundles, --vendor-charts, and attestation/signing can each
	// exceed a fixed cap. The REST /v1/bundle handler sets this to
	// defaults.BundleHandlerTimeout to preserve its 60s request boundary;
	// the CLI bundle command leaves it 0 so long bundles are uncapped.
	Timeout time.Duration
}

// adoptRecipe wraps a raw, externally-supplied *Recipe (the full
// pkg/recipe.RecipeResult, e.g. decoded from a /v1/bundle POST body) into a
// Client-owned *RecipeResult ready for MakeBundle. It binds the Client's own
// DataProvider onto the recipe so provider-scoped lookups (values files,
// manifest files, external data files) resolve against the Client's recipe
// source rather than the package global, and stamps the owner token so the
// result passes MakeBundle's assertOwns check. This is the REST analog of
// LoadRecipe: LoadRecipe reads from a path through the provider, adoptRecipe
// takes an already-decoded RecipeResult and binds the same provider.
//
// Synchronization mirrors LoadRecipe: snapshot the per-Client provider under
// the read lock so a concurrent Close can't race the read. Unlike the
// resolve/load paths it does not register in the inflight WaitGroup — it does
// no cache-using work itself (the subsequent MakeBundle call does, and
// registers there).
//
// Errors:
//   - ErrCodeInvalidRequest when the Client, ctx, or recipe is nil, or when
//     the Client has been Closed.
func (c *Client) adoptRecipe(ctx context.Context, recipe *Recipe) (*RecipeResult, error) {
	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if recipe == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "nil Recipe")
	}

	c.mu.RLock()
	if c.builder == nil {
		c.mu.RUnlock()
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	dp := c.dp
	c.mu.RUnlock()

	// Deep-copy the caller-supplied recipe BEFORE binding the provider.
	// BindDataProvider mutates the receiver's unexported provider field, and
	// the input *Recipe is caller-owned: a caller reusing one *Recipe across
	// two Clients would otherwise have the second adopt overwrite the first's
	// binding, breaking per-Client isolation. DeepCopy leaves the copy's
	// provider nil for BindDataProvider to set, so the original is untouched.
	cp := recipe.DeepCopy()
	cp.BindDataProvider(dp)

	result, err := loadedResultFromInternal(cp)
	if err != nil {
		return nil, err
	}
	result.owner = c
	return result, nil
}

// AdoptRecipe wraps a raw pkg/recipe.RecipeResult — typically decoded from an
// external source such as a REST /v1/bundle POST body — into a Client-owned
// *RecipeResult ready for MakeBundle. The returned RecipeResult is bound to
// this Client's DataProvider and owner-stamped, so it passes MakeBundle's
// ownership and provider-isolation checks exactly as a LoadRecipe result does.
//
// Use this when the caller already holds a fully-hydrated RecipeResult (not a
// criteria request or a file path) and needs to bundle it through the facade.
// In-process consumers that resolve via ResolveRecipe / LoadRecipe should use
// those results directly; AdoptRecipe is for the decode-then-bundle boundary.
func (c *Client) AdoptRecipe(ctx context.Context, recipe *Recipe) (*RecipeResult, error) {
	return c.adoptRecipe(ctx, recipe)
}

// MakeBundle generates the full deployer-mode bundle for a previously
// resolved or loaded RecipeResult, writing the bundle files under
// opts.OutputDir and returning a BundleArtifact summary. Unlike
// BundleComponents (which returns per-component Helm values + manifests
// in memory), MakeBundle produces the SAME complete artifact the CLI
// bundle command emits — README, deploy.sh, per-component directories,
// checksums — in the deployer layout selected by opts.Config.Deployer()
// (helm, argocd, argocd-helm, flux, helmfile).
//
// # When to call
//
// Call AFTER Client.ResolveRecipe or Client.LoadRecipe; pass that call's
// *RecipeResult unchanged. MakeBundle bundles from recipe.Resolved() (the
// full pkg/recipe.RecipeResult), which carries this Client's own
// DataProvider — so provider-scoped lookups (values files, manifest
// files) resolve against the Client's recipe source rather than the
// package global.
//
// # Allowlist enforcement
//
// When the Client was constructed WithAllowLists, MakeBundle validates
// the recipe's criteria against the allowlist before bundling — same
// fencing the resolve path and the REST /v1/bundle handler apply. A
// recipe whose criteria fall outside the allowlist is rejected with the
// allowlist's structured error. A recipe with nil Criteria (a loaded,
// already-hydrated or bare RecipeResult file) skips the check, matching
// the handler's `recipeResult.Criteria != nil` guard.
//
// # Synchronization
//
// Read-locks Client.mu so a concurrent Close can't race the bundle, and
// registers in the inflight WaitGroup so Close drains before evicting
// caches — the same protocol as BundleComponents. A facade-level timeout
// is opt-in via opts.Timeout: when set (> 0) it bounds the run by the
// smaller of opts.Timeout and any tighter caller deadline; when unset
// (0) MakeBundle runs under the caller's context with NO added cap. The
// REST /v1/bundle handler sets opts.Timeout = defaults.BundleHandlerTimeout
// to keep its 60s request boundary; the CLI bundle command leaves it 0 so
// large bundles, --vendor-charts, and attestation/signing are uncapped.
//
// Errors:
//   - ErrCodeInvalidRequest when the Client, ctx, or recipe is nil, when
//     recipe lacks internal state (constructed outside Resolve/Load), when
//     the recipe was produced by a different Client, or when the Client
//     has been Closed.
//   - Allowlist and bundler errors propagate with their structured codes.
func (c *Client) MakeBundle(ctx context.Context, recipe *RecipeResult, opts BundleOptions) (BundleArtifact, error) {
	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if recipe == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "nil RecipeResult")
	}
	if recipe.internal == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"RecipeResult has no internal recipe state — call Client.ResolveRecipe or Client.LoadRecipe to obtain a bundle-able RecipeResult")
	}
	if err := c.assertOwns(recipe); err != nil {
		return nil, err
	}

	// Snapshot Client state under the read lock — same pattern as
	// BundleComponents. The closed-Client check reads c.builder; the
	// bundle itself runs unlocked off recipe.internal (which carries
	// this Client's bound DataProvider).
	c.mu.RLock()
	if c.builder == nil {
		c.mu.RUnlock()
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	c.inflight.Add(1)
	c.mu.RUnlock()
	defer c.inflight.Done()

	// Apply a facade-level deadline only when the caller opts in via
	// opts.Timeout. context.WithTimeout honors the smaller of the parent
	// deadline and ours, so a caller with a tighter deadline keeps it.
	// When opts.Timeout is 0 the caller's ctx governs unchanged — the CLI
	// bundle path runs uncapped (large bundles, --vendor-charts, and
	// signing can exceed any fixed cap), while the REST handler passes
	// defaults.BundleHandlerTimeout to retain its 60s boundary. Placed
	// after the guards so already-canceled-context tests flow through the
	// same error paths.
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	// Enforce the Client's allowlists against the recipe criteria, mirroring
	// the REST /v1/bundle handler's `AllowLists != nil && Criteria != nil`
	// gate. A loaded recipe with nil Criteria skips the check (it carries no
	// criteria to validate); a resolved recipe always has criteria.
	if recipe.internal.Criteria != nil {
		if err := c.enforceAllowLists(recipe.internal.Criteria); err != nil {
			return nil, err
		}
	}

	cfg := opts.Config
	if cfg == nil {
		cfg = config.NewConfig()
	}

	bundlerOpts := []bundler.Option{bundler.WithConfig(cfg)}
	if opts.Attester != nil {
		bundlerOpts = append(bundlerOpts, bundler.WithAttester(opts.Attester))
	}
	b, err := bundler.New(bundlerOpts...)
	if err != nil {
		// Don't re-wrap — bundler.New returns structured errors with the
		// right code (ErrCodeNotFound for a missing binary attestation,
		// ErrCodeInternal for executable-path resolution failures).
		return nil, err
	}

	out, err := b.Make(ctx, recipe.internal, opts.OutputDir)
	if err != nil {
		// Propagate as-is: Make returns structured errors (validation,
		// timeout, internal) with the appropriate codes.
		return nil, err
	}
	return out, nil
}
