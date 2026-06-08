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

// Package aicr is the stable, public Go library surface for external
// consumers of the AI Cluster Runtime.
//
// External projects should import THIS package and use the types and
// constructors re-exported here. The underlying pkg/* packages are
// public and will remain importable, but this facade is the contract
// the project commits to via semver.
//
// # Surface
//
// Client exposes the four end-to-end operations the CLI / server share:
//
//   - ResolveRecipe / ResolveRecipeFromCriteria / ResolveRecipeFromSnapshot
//     and LoadRecipe — produce or load a *RecipeResult.
//   - BundleComponents — resolve Helm values and stitched manifests for
//     each component in a *RecipeResult.
//   - CollectSnapshot — deploy the snapshotter Job and retrieve a *Snapshot.
//   - ValidateState — evaluate a resolved recipe against a snapshot,
//     running deployment / conformance / performance phases.
//
// All facade types (Snapshot, AgentConfig, Criteria, RecipeRequest,
// RecipeResult, ComponentBundle, ComponentRef, PhaseResult, AllowLists)
// are facade-owned structs translated to and from the upstream pkg/*
// shapes, so internal field renames don't churn external callers.
//
// # Example
//
//	client, err := aicr.NewClient(
//	    aicr.WithRecipeSource(aicr.FilesystemSource("/etc/aicr/recipes")),
//	)
//	if err != nil {
//	    return err
//	}
//	defer client.Close()
//
//	result, err := client.ResolveRecipe(ctx, aicr.RecipeRequest{
//	    Service:     "eks",
//	    Region:      "us-east-1",
//	    Accelerator: "h100",
//	    Nodes:       8, // worker-node count, not GPU count
//	    Intent:      "training",
//	})
//
// # Stability
//
// This package's exported API follows semver. The underlying pkg/*
// packages may introduce breaking changes between minor releases; if
// you depend on them directly, pin AICR to a patch version and audit
// upgrades.
//
// # Concurrency and Client lifecycle
//
// Each Client owns its own DataProvider and per-DataProvider cached
// metadata store and component registry. Multiple Clients constructed
// from different sources can resolve recipes concurrently without
// clobbering each other — a property multi-tenant consumers (e.g., a
// controller managing one Client per per-tenant configuration) rely
// on. This is a v0.12+ guarantee; earlier facade builds mutated a
// process-global DataProvider via recipe.SetDataProvider and were
// unsafe to construct concurrently.
//
// **Retain and reuse Client instances.** The recipe package keys its
// internal caches on DataProvider identity (pointer-equality of the
// interface value). Each call to NewClient builds a fresh
// DataProvider, so two Clients constructed from the same recipe
// source still produce distinct cache entries and do their own
// directory walk on first use. Long-running consumers should cache
// Clients keyed by their configuration (e.g., a content hash of the
// recipe-source settings) rather than constructing one per request.
//
// **Call Close when done.** When a Client is no longer needed
// (cache eviction, controller shutdown), call Close to drop its
// metadata store and component registry from the recipe package's
// internal caches. Without this, memory grows monotonically with
// the number of unique DataProviders ever observed.
//
// See docs/integrator/go-library.md for the integration guide.
package aicr

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/aicr/pkg/constraints"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator"
	validatorv1 "github.com/NVIDIA/aicr/pkg/validator/v1"
	"sigs.k8s.io/yaml"
)

// Compile-time assertion that *Client satisfies io.Closer. Anchoring the
// Close() error signature against the standard interface documents why the
// method returns an error even though the current implementation can't fail
// — composite cleanup chains (errgroup.Go with deferred Close, defer-with-
// error patterns) rely on io.Closer's shape, and future cleanup steps may
// legitimately fail (e.g., flushing a metrics buffer on Close).
var _ io.Closer = (*Client)(nil)

// Client is the single entry point for external Go consumers.
//
// Concurrent ResolveRecipe calls are safe — the Builder itself is
// thread-safe over its read-only state. The mu guards the small
// window where Close swaps builder/dp to nil; without it,
// concurrent ResolveRecipe + Close on the same Client is a data
// race because the field write in Close is unsynchronised against
// the field read at the top of ResolveRecipe.
type Client struct {
	// mu protects builder and dp. Read locked by ResolveRecipe
	// (multiple concurrent reads are safe), write locked by Close
	// (exclusive while clearing). source doesn't change after
	// construction so it doesn't need locking.
	mu      sync.RWMutex
	builder *recipe.Builder
	dp      recipe.DataProvider
	source  recipeSource

	// version is threaded into the Builder via recipe.WithVersion so
	// resolved recipes carry it in Metadata.Version. Set by WithVersion;
	// applied once in NewClient before the builder is constructed. Doesn't
	// change after construction, so it doesn't need locking.
	version string

	// allowLists fences which criteria values the resolve path accepts.
	// nil means "no fencing — all values allowed". Set by WithAllowLists;
	// enforced in enforceAllowLists on the shared resolve path. Doesn't
	// change after construction, so it doesn't need locking.
	allowLists *AllowLists

	// inflight tracks in-flight cache-using operations so Close
	// can drain them before evicting the per-Client metadata-store
	// and component-registry caches. Without this, a ResolveRecipe
	// goroutine that releases mu before calling LoadMetadataStoreFor
	// can repopulate storeCache[dp] AFTER Close already evicted it
	// — violating the "Close frees this Client's caches" guarantee.
	// Each entry point Add(1)s under RLock (so Close's Wait can see
	// the increment) and Done()s on return; Close marks the Client
	// closed under write-lock, releases, then Wait()s.
	inflight sync.WaitGroup
}

// NewClient constructs a Client with the supplied functional options.
// Callers must provide a recipe source via WithRecipeSource.
//
// For FilesystemSource, the external directory is layered OVER the
// embedded recipe data — files in the directory override embedded
// equivalents, and recipes must include a registry.yaml at the root.
//
// OCI sources are not yet wired through to the loader and return an
// ErrCodeUnavailable error from NewClient until that gap is closed.
func NewClient(opts ...Option) (*Client, error) {
	c := &Client{}

	for _, opt := range opts {
		// Skip nil Option entries defensively — a caller building a
		// dynamic []Option (e.g., conditional appends) can hand us nil
		// without intending a panic. The cost of the guard is one
		// branch per option; the alternative is a hard crash inside
		// the With*-applied closure dereference.
		if opt == nil {
			continue
		}
		opt(c)
	}

	if c.source.kind == sourceKindUnset {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"recipe source is required — pass WithRecipeSource")
	}

	dp, err := buildDataProvider(c.source)
	if err != nil {
		return nil, err
	}

	// Bind the Builder to this Client's own DataProvider via
	// recipe.WithDataProvider. Pre-v0.12 the facade used
	// recipe.SetDataProvider here, mutating a process-global —
	// concurrent Clients constructed from different sources would
	// silently clobber each other. The per-Builder binding makes
	// each Client's resolve path use its own cached metadata
	// store and component registry.
	// Construction-time write to builder/dp doesn't need the lock —
	// the Client isn't visible to other goroutines until NewClient
	// returns — but using the same mu Lock pattern here keeps the
	// access pattern uniform and makes the field-mutation rule
	// trivial to verify by grep.
	builderOpts := []recipe.Option{recipe.WithDataProvider(dp)}
	if c.version != "" {
		builderOpts = append(builderOpts, recipe.WithVersion(c.version))
	}
	c.mu.Lock()
	c.builder = recipe.NewBuilder(builderOpts...)
	c.dp = dp
	c.mu.Unlock()

	slog.Debug("aicr client constructed",
		"source.kind", c.source.kind,
		"source.path", c.source.path,
		"source.registry", c.source.registry,
	)

	return c, nil
}

// Close releases this Client's cached metadata store and component
// registry from the recipe package's internal caches. Call when a
// Client is no longer needed (cache eviction in a higher-level
// memoiser, controller shutdown) to prevent unbounded memory
// growth — the recipe package keys its caches on DataProvider
// identity and does not auto-evict, so a process that observes many
// distinct recipe sources over time would otherwise grow memory
// monotonically.
//
// Safe to call on a nil receiver and safe to call multiple times
// (subsequent calls are no-ops). Always returns nil; the signature
// matches io.Closer so this can stand in for io.Closer in
// composite cleanup chains.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	dp := c.dp
	c.dp = nil
	c.builder = nil
	c.mu.Unlock()

	// Drain in-flight ResolveRecipe / BundleComponents /
	// CollectSnapshot / ValidateState calls before evicting. Each
	// entry point Add(1)s under the read lock; because Close
	// acquires the write lock, any in-flight increment is visible
	// here. New callers arriving after the write-lock release see
	// c.builder == nil and reject early without incrementing, so
	// the WaitGroup converges. Without this drain a resolve in
	// progress could repopulate storeCache[dp] after the Evict
	// calls below — silently leaking cache entries after Close.
	c.inflight.Wait()

	// Evict outside the lock — these touch the recipe package's
	// own caches and don't need our mu held. dp may be nil if
	// Close was already called or the Client was never fully
	// constructed; the recipe Evict helpers no-op on nil.
	if dp != nil {
		recipe.EvictCachedStore(dp)
		recipe.EvictCachedRegistry(dp)
	}
	return nil
}

// LoadCatalog eagerly loads (and caches) this Client's metadata store,
// which has the side effect of seeding THIS Client's per-provider criteria
// registry from every overlay's spec.criteria. Call it before parsing
// criteria through CriteriaRegistry so values contributed by a
// FilesystemSource --data overlay are admitted by the registry's lookups.
//
// This mirrors the pre-facade eager recipe.LoadCatalog the CLI ran after
// SetDataProvider, but seeds the Client's OWN provider registry rather than
// the process-global one — so two Clients built from different sources keep
// isolated criteria registries.
//
// Errors propagate with their structured codes preserved (a malformed
// overlay surfaces as ErrCodeInvalidRequest, not masked as ErrCodeInternal)
// via PropagateOrWrap.
//
// The same guards as the resolve methods apply: nil receiver and nil context
// are rejected with ErrCodeInvalidRequest, and a closed Client is rejected.
func (c *Client) LoadCatalog(ctx context.Context) error {
	if c == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}

	// Snapshot the per-Client provider under the read lock so a concurrent
	// Close can't race the read; Add to inflight under the lock so Close's
	// drain observes the increment. Same protocol as ResolveRecipeFromCriteria.
	c.mu.RLock()
	if c.builder == nil {
		c.mu.RUnlock()
		return errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	dp := c.dp
	c.inflight.Add(1)
	c.mu.RUnlock()
	defer c.inflight.Done()

	ctx, cancel := context.WithTimeout(ctx, defaults.RecipeOperationTimeout)
	defer cancel()

	if _, err := recipe.LoadMetadataStoreFor(ctx, dp); err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to load recipe catalog")
	}
	return nil
}

// CriteriaRegistry returns the per-DataProvider criteria registry for THIS
// Client. CLI/library callers use it to parse criteria values (so --data
// overlay contributions validate) and to apply strict mode against the same
// provider the Client resolves with. Call LoadCatalog first so the registry
// is seeded from the provider's overlays before parsing.
//
// Returns the registry for this Client's provider via
// recipe.GetCriteriaRegistryFor. On a nil Client this returns a fresh
// ephemeral registry so callers can defensively call without nil-checking,
// matching the lenient nil behavior of the other accessors.
func (c *Client) CriteriaRegistry() *CriteriaRegistry {
	if c == nil {
		return recipe.NewCriteriaRegistry()
	}
	c.mu.RLock()
	dp := c.dp
	c.mu.RUnlock()
	return recipe.GetCriteriaRegistryFor(dp)
}

// assertOwns rejects RecipeResults that were not produced by this Client.
// The owner field is stamped in ResolveRecipe with the producing Client's
// pointer identity; passing result A to client B silently mixed A's
// component refs with B's DataProvider reads before this check existed,
// producing wrong Helm values or supplemental manifests with no error.
//
// A nil owner means the caller bypassed ResolveRecipe (e.g., constructed
// the RecipeResult directly). That's a programmer error too — the
// internal field requires the facade to populate it, and the only public
// path is ResolveRecipe — so the check rejects nil owner as well.
//
// The error embeds %p of both pointers; in controller logs that lets an
// operator distinguish "wrong Client" from "no Client at all" without
// adding telemetry surface.
func (c *Client) assertOwns(r *RecipeResult) error {
	if r.owner == c {
		return nil
	}
	return errors.NewWithContext(errors.ErrCodeInvalidRequest,
		"RecipeResult was produced by a different Client (or constructed outside ResolveRecipe); cross-client bundle/validate is not permitted",
		map[string]any{
			"expectedOwner": fmt.Sprintf("%p", c),
			"actualOwner":   fmt.Sprintf("%p", r.owner),
		})
}

// enforceAllowLists rejects criteria values outside the Client's
// configured allowlists. When no allowlists are set (the common case),
// it is a no-op. The underlying AllowLists.ValidateCriteria already
// returns a pkg/errors-coded error, so the result is returned as-is
// rather than re-wrapped.
//
// Accepts the upstream pkg/recipe.Criteria because all in-tree callers
// (resolveCriteria, ResolveRecipeFromSnapshot) already hold the internal
// shape post-translation; routing back through the facade would force a
// pointless round-trip.
func (c *Client) enforceAllowLists(criteria *recipe.Criteria) error {
	if c.allowLists == nil {
		return nil
	}
	return ToInternalAllowLists(c.allowLists).ValidateCriteria(criteria)
}

// ResolveRecipe maps a RecipeRequest to a concrete validated recipe.
// It wraps pkg/recipe.Builder.BuildFromCriteria with a stable external
// request shape so AICR's internal Criteria type can evolve without
// breaking consumers.
//
// Pinned recipe references (req.PinnedName / req.PinnedVersion) are
// not yet supported by the facade and return ErrCodeUnavailable. The
// field is reserved so callers can adopt it without API churn when
// the underlying builder gains pinning support.
func (c *Client) ResolveRecipe(ctx context.Context, req RecipeRequest) (*RecipeResult, error) {
	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}

	// Snapshot builder under the read lock so a concurrent Close
	// can't race with the read. Multiple ResolveRecipe calls run
	// in parallel; only Close blocks them (briefly).
	//
	// Add to inflight while holding the read lock so Close's
	// write-lock-then-Wait protocol observes the increment. Done()
	// runs on return so Close's Wait converges once this call's
	// LoadMetadataStoreFor work has finished — preventing the
	// resolve from repopulating storeCache[dp] after Close evicted.
	c.mu.RLock()
	builder := c.builder
	if builder == nil {
		c.mu.RUnlock()
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	c.inflight.Add(1)
	c.mu.RUnlock()
	defer c.inflight.Done()

	// Apply a hard deadline so callers that pass an unbounded
	// context still get a bounded resolve. context.WithTimeout
	// honors the smaller of the parent deadline and ours, so
	// callers with a tighter deadline keep their value. Placed
	// AFTER the nil-receiver and closed-Client guards so tests
	// that pass an already-canceled context still flow through
	// the same error paths they did before.
	ctx, cancel := context.WithTimeout(ctx, defaults.RecipeOperationTimeout)
	defer cancel()

	if req.PinnedName != "" || req.PinnedVersion != "" {
		return nil, errors.NewWithContext(
			errors.ErrCodeUnavailable,
			"pinned recipe references are not yet supported by the facade",
			map[string]any{
				"pinnedName":    req.PinnedName,
				"pinnedVersion": req.PinnedVersion,
			},
		)
	}

	criteria, err := criteriaFromRequest(req, recipe.GetCriteriaRegistryFor(builder.DataProvider()))
	if err != nil {
		return nil, err
	}

	internal, err := c.resolveCriteria(ctx, builder, criteria)
	if err != nil {
		// Don't re-wrap with ErrCodeInternal — the builder already
		// returns a structured error with the appropriate code
		// (ErrCodeInvalidRequest for bad criteria, ErrCodeTimeout
		// for context expiry, etc.). Wrapping unconditionally would
		// mask the inner code from callers doing errors.Is checks
		// downstream. See AGENTS.md "Don't double-wrap errors that
		// already have proper codes".
		return nil, err
	}

	result, err := recipeResultFromInternal(internal)
	if err != nil {
		return nil, err
	}
	// Stamp the owning Client so BundleComponents / ValidateState can
	// reject cross-client misuse. Pointer identity is the token —
	// unforgeable from outside this package because RecipeResult.owner
	// is unexported.
	result.owner = c
	return result, nil
}

// resolveCriteria is the shared path: allowlist enforcement + build. Callers
// snapshot the builder under the read lock and pass it in; they own the lossy-
// vs-lossless projection and owner stamping. The criteria parameter is the
// upstream pkg/recipe.Criteria shape — facade entry points translate via
// toInternalCriteria before reaching here.
func (c *Client) resolveCriteria(ctx context.Context, builder *recipe.Builder, criteria *recipe.Criteria) (*recipe.RecipeResult, error) {
	if err := c.enforceAllowLists(criteria); err != nil {
		return nil, err
	}
	return builder.BuildFromCriteria(ctx, criteria)
}

// ResolveRecipeFromCriteria resolves a facade Criteria into a facade
// RecipeResult. The Components projection mirrors ResolveRecipe; callers
// needing the full upstream recipe (constraints, deployment order, metadata)
// access it via the returned result's Resolved() helper.
//
// Use this when the caller already speaks the facade Criteria type (e.g.,
// a REST handler that parsed criteria from an HTTP request and translated
// via WrapCriteria) rather than the RecipeRequest shape ResolveRecipe takes.
//
// Allowlist enforcement (WithAllowLists) applies here just as it does on the
// shared resolve path: criteria outside the configured allowlist are rejected
// before the recipe is built.
//
// The same guards and synchronization as ResolveRecipe apply: nil receiver,
// nil context, and nil criteria are rejected with ErrCodeInvalidRequest; a
// closed Client is rejected; a facade-level timeout bounds the resolve.
func (c *Client) ResolveRecipeFromCriteria(ctx context.Context, criteria *Criteria) (*RecipeResult, error) {
	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if criteria == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "criteria is required (got nil)")
	}

	// Snapshot builder under the read lock so a concurrent Close can't
	// race the read; Add to inflight under the lock so Close's drain
	// observes the increment. Same protocol as ResolveRecipe.
	c.mu.RLock()
	builder := c.builder
	if builder == nil {
		c.mu.RUnlock()
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	c.inflight.Add(1)
	c.mu.RUnlock()
	defer c.inflight.Done()

	ctx, cancel := context.WithTimeout(ctx, defaults.RecipeOperationTimeout)
	defer cancel()

	internal, err := c.resolveCriteria(ctx, builder, toInternalCriteria(criteria))
	if err != nil {
		return nil, err
	}
	result, err := recipeResultFromInternal(internal)
	if err != nil {
		return nil, err
	}
	result.owner = c
	return result, nil
}

// ResolveRecipeFromSnapshot resolves a recipe from explicit Criteria and
// evaluates its constraints against an observed cluster Snapshot, mirroring
// `aicr recipe --snapshot`. It returns the facade RecipeResult; callers
// needing the upstream recipe (ComponentRefs, deployment order, per-
// constraint evaluation results) access it via Resolved().
//
// Unlike ResolveRecipeFromCriteria — which builds the recipe without
// observing the cluster — this variant threads a constraint evaluator that
// runs each resolution constraint against snap via pkg/constraints.Evaluate.
// The CLI's `recipe --snapshot` path does the same: it derives criteria from
// the snapshot fingerprint, then calls BuildFromCriteriaWithEvaluator so the
// resolved recipe records whether each constraint passed against the observed
// state.
//
// Allowlist enforcement (WithAllowLists) applies here just as it does on the
// shared resolve path: criteria outside the configured allowlist are rejected
// before the recipe is built.
//
// The same guards and synchronization as ResolveRecipeFromCriteria apply: nil
// receiver, nil context, nil criteria, and nil snapshot are rejected with
// ErrCodeInvalidRequest; a closed Client is rejected; a facade-level timeout
// bounds the resolve. Builder errors propagate as-is (they already carry the
// appropriate pkg/errors code) rather than being re-wrapped.
func (c *Client) ResolveRecipeFromSnapshot(ctx context.Context, criteria *Criteria, snap *Snapshot) (*RecipeResult, error) {
	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if criteria == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "criteria is required (got nil)")
	}
	if snap == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "snapshot is required (got nil)")
	}

	// Snapshot builder under the read lock so a concurrent Close can't
	// race the read; Add to inflight under the lock so Close's drain
	// observes the increment. Same protocol as ResolveRecipeFromCriteria.
	c.mu.RLock()
	builder := c.builder
	if builder == nil {
		c.mu.RUnlock()
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	c.inflight.Add(1)
	c.mu.RUnlock()
	defer c.inflight.Done()

	ctx, cancel := context.WithTimeout(ctx, defaults.RecipeOperationTimeout)
	defer cancel()

	// Translate facade Criteria once; remaining steps operate on the
	// upstream shape.
	internalCriteria := toInternalCriteria(criteria)

	// Enforce allowlists before building — same fence resolveCriteria applies
	// on the criteria-only path. AllowLists.ValidateCriteria returns a
	// pkg/errors-coded error, so enforceAllowLists propagates it as-is.
	if err := c.enforceAllowLists(internalCriteria); err != nil {
		return nil, err
	}

	// Evaluate each resolution constraint against the observed snapshot,
	// mirroring the CLI's `recipe --snapshot` path. The evaluator bridges
	// pkg/constraints.EvalResult into the recipe package's
	// ConstraintEvalResult (kept distinct to avoid a recipe→constraints
	// import cycle).
	evaluator := func(constraint recipe.Constraint) recipe.ConstraintEvalResult {
		v := constraints.Evaluate(constraint, toInternalSnapshot(snap))
		return recipe.ConstraintEvalResult{
			Passed: v.Passed,
			Actual: v.Actual,
			Error:  v.Error,
		}
	}

	// Don't re-wrap the builder's error — it already returns a structured
	// error with the appropriate code (ErrCodeInvalidRequest for bad
	// criteria, ErrCodeTimeout for context expiry, etc.).
	internal, err := builder.BuildFromCriteriaWithEvaluator(ctx, internalCriteria, evaluator)
	if err != nil {
		return nil, err
	}
	result, err := recipeResultFromInternal(internal)
	if err != nil {
		return nil, err
	}
	result.owner = c
	return result, nil
}

// buildDataProvider constructs an isolated DataProvider for a single
// Client from the facade's recipeSource. Unlike the previous
// applySource, this does NOT call recipe.SetDataProvider — the
// returned provider is bound directly to one Client's Builder via
// recipe.WithDataProvider, so concurrent Clients with different
// sources don't interfere.
//
// FilesystemSource: layered provider over the embedded data and the
// external directory.
//
// OCISource: not yet supported. Returns ErrCodeUnavailable.
func buildDataProvider(s recipeSource) (recipe.DataProvider, error) {
	switch s.kind {
	case sourceKindUnset:
		// Unreachable: NewClient rejects sourceKindUnset before calling
		// buildDataProvider. Kept as an explicit case for lint exhaustiveness.
		return nil, errors.New(errors.ErrCodeInvalidRequest, "no recipe source configured")
	case sourceKindFilesystem:
		embedded := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), ".")
		layered, err := recipe.NewLayeredDataProvider(embedded, recipe.LayeredProviderConfig{
			ExternalDir: s.path,
		})
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				"construct layered data provider", err)
		}
		return layered, nil
	case sourceKindEmbedded:
		return recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "."), nil
	case sourceKindOCI:
		return nil, errors.NewWithContext(
			errors.ErrCodeUnavailable,
			"OCI recipe sources are not yet supported by the facade — use FilesystemSource for now",
			map[string]any{
				"registry": s.registry,
				"tag":      s.tag,
			},
		)
	default:
		return nil, errors.New(errors.ErrCodeInvalidRequest, "unknown recipe source kind")
	}
}

// criteriaFromRequest translates a facade RecipeRequest into AICR's
// internal Criteria type. Fields not representable in Criteria
// (Region is informational and recorded but not filtered on;
// PinnedName/PinnedVersion are rejected upstream in ResolveRecipe)
// are not passed through.
//
// Validation:
//   - req.Nodes < 0 is rejected. Zero is a valid "unspecified" sentinel
//     (matches CLI behavior and the doc on RecipeRequest.Nodes), but a
//     negative count is a programming error and the criteria builder
//     would silently treat it the same as zero — masking the bug.
func criteriaFromRequest(req RecipeRequest, reg *recipe.CriteriaRegistry) (*recipe.Criteria, error) {
	if req.Nodes < 0 {
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"RecipeRequest.Nodes must be >= 0",
			map[string]any{"nodes": req.Nodes})
	}

	opts := make([]recipe.RegistryCriteriaOption, 0, 6)

	if req.Service != "" {
		opts = append(opts, recipe.WithServiceRegistry(req.Service))
	}
	if req.Accelerator != "" {
		opts = append(opts, recipe.WithAcceleratorRegistry(req.Accelerator))
	}
	if req.Intent != "" {
		opts = append(opts, recipe.WithIntentRegistry(req.Intent))
	}
	if req.OS != "" {
		opts = append(opts, recipe.WithOSRegistry(req.OS))
	}
	if req.Platform != "" {
		opts = append(opts, recipe.WithPlatformRegistry(req.Platform))
	}
	if req.Nodes > 0 {
		opts = append(opts, recipe.WithNodesRegistry(int(req.Nodes)))
	}

	criteria, err := recipe.BuildCriteriaWithRegistry(reg, opts...)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "build criteria")
	}
	return criteria, nil
}

// recipeResultFromInternal converts AICR's internal RecipeResult into
// the facade shape. Isolating this mapping means field renames inside
// pkg/recipe only require a one-line facade edit.
//
// The internal RecipeResult has no authoritative Name field — recipes
// are keyed by Criteria. The facade therefore derives Name from
// Criteria.String(). If Criteria is nil an error is returned rather
// than returning an unusable unnamed result.
func recipeResultFromInternal(r *recipe.RecipeResult) (*RecipeResult, error) {
	if r == nil {
		return nil, errors.New(errors.ErrCodeInternal,
			"recipe builder returned nil RecipeResult")
	}
	if r.Criteria == nil {
		return nil, errors.New(errors.ErrCodeInternal,
			"recipe result has no criteria; cannot derive stable name")
	}
	return facadeResultFromInternal(r, r.Criteria.String()), nil
}

// loadedResultFromInternal wraps a recipe loaded from a file into the
// facade shape. Unlike recipeResultFromInternal — which is the resolve
// path, where a nil Criteria is a builder bug — a file loaded via
// LoadRecipe may legitimately carry no Criteria: an already-hydrated
// RecipeResult, or a bare/empty-kind RecipeResult file, both of which
// recipe.LoadFromFile accepts. Those flow through validate/bundle by
// their internal state and component refs, not by a criteria-derived
// Name, so the facade Name is derived from Criteria when present and is
// left empty otherwise. This preserves the CLI loader's tolerance of
// criteria-less recipe files (already-hydrated or bare RecipeResult),
// which the strict resolve path intentionally rejects.
func loadedResultFromInternal(r *recipe.RecipeResult) (*RecipeResult, error) {
	if r == nil {
		return nil, errors.New(errors.ErrCodeInternal,
			"recipe loader returned nil RecipeResult")
	}
	var name string
	if r.Criteria != nil {
		name = r.Criteria.String()
	}
	return facadeResultFromInternal(r, name), nil
}

// facadeResultFromInternal builds the facade RecipeResult from an internal
// recipe result and a pre-derived Name. Both the resolve path
// (recipeResultFromInternal) and the load path (loadedResultFromInternal)
// share this mapping so a pkg/recipe field rename is a single edit.
func facadeResultFromInternal(r *recipe.RecipeResult, name string) *RecipeResult {
	out := &RecipeResult{
		Name:         name,
		Version:      r.Metadata.Version,
		TranslatedAt: time.Now(),
		internal:     r,
	}
	for _, c := range r.ComponentRefs {
		out.Components = append(out.Components, ComponentRef{
			Name:      c.Name,
			Kind:      string(c.Type),
			Version:   c.Version,
			Source:    c.Source,
			Chart:     c.Chart,
			Namespace: c.Namespace,
		})
	}
	return out
}

// LoadRecipe loads a recipe from a file path (or cm:// ConfigMap URI,
// honoring kubeconfig) through THIS Client's data provider, and returns
// it as a Client-owned *RecipeResult ready for ValidateState /
// BundleComponents. Overlay inputs (kind: RecipeMetadata) are hydrated
// against the Client's provider, so an external --data overlay resolves
// against the same recipe source the Client was constructed with rather
// than the package global. An already-hydrated RecipeResult file is
// returned with its provider bound to the Client's provider.
//
// The returned RecipeResult is owner-stamped with this Client, so it
// passes ValidateState / BundleComponents' assertOwns check — same as a
// RecipeResult produced by ResolveRecipe.
//
// Errors:
//   - ErrCodeInvalidRequest when the Client is nil, ctx is nil, path is
//     empty, or the Client has been Closed.
//   - All loader errors propagate with their structured codes (e.g.,
//     ErrCodeInvalidRequest for an overlay without criteria,
//     ErrCodeInternal for a read or parse failure).
func (c *Client) LoadRecipe(ctx context.Context, path, kubeconfig string) (*RecipeResult, error) {
	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if path == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "recipe path is required (got empty)")
	}

	// Snapshot the per-Client provider under the read lock so a
	// concurrent Close can't race the read; Add to inflight under the
	// lock so Close's drain observes the increment. Same protocol as
	// ResolveRecipeFromCriteria.
	c.mu.RLock()
	if c.builder == nil {
		c.mu.RUnlock()
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	dp := c.dp
	c.inflight.Add(1)
	c.mu.RUnlock()
	defer c.inflight.Done()

	ctx, cancel := context.WithTimeout(ctx, defaults.RecipeOperationTimeout)
	defer cancel()

	loaded, err := recipe.LoadFromFileWithProvider(ctx, path, kubeconfig, c.version, dp)
	if err != nil {
		// Don't re-wrap — the loader already returns structured errors
		// with the right code (ErrCodeInvalidRequest for a bad overlay,
		// ErrCodeInternal for read/parse failures).
		return nil, err
	}

	// Use the load-path constructor (tolerant of a nil Criteria) rather than
	// the resolve-path one: recipe.LoadFromFile accepts already-hydrated and
	// bare RecipeResult files that carry no Criteria, and rejecting them here
	// would diverge from the CLI's historical loader behavior.
	result, err := loadedResultFromInternal(loaded)
	if err != nil {
		return nil, err
	}
	// Stamp the owning Client so ValidateState / BundleComponents accept
	// this result — same owner-token contract as ResolveRecipe.
	result.owner = c
	return result, nil
}

// BundleComponents resolves Helm values and rendered manifests for
// each component in a previously-resolved RecipeResult. The returned
// slice mirrors r.Components 1:1 — same order, same length — so
// callers correlate by index.
//
// # When to call
//
// Call AFTER ResolveRecipe; pass that call's *RecipeResult unchanged.
// BundleComponents reads the internal pkg/recipe.RecipeResult that
// ResolveRecipe attached to the facade RecipeResult — it does NOT
// re-resolve from criteria. A RecipeResult constructed by the caller
// (rather than returned from ResolveRecipe) has a nil internal field
// and BundleComponents returns ErrCodeInvalidRequest.
//
// # Per-Client DataProvider isolation
//
// Both values-file reads (Helm components) and manifest-file reads
// (Helm supplemental + Kustomize) are bound to this Client's own
// DataProvider via the WithProvider variants on the recipe package
// (recipe.RecipeResult.GetValuesForComponentWithProvider,
// recipe.GetManifestContentWithProvider). Two Clients constructed
// from different recipe sources can BundleComponents concurrently
// without contaminating each other's bundle output.
//
// History: pre-v0.2 the values and manifest paths short-circuited
// through recipe.GetDataProvider() — the process-global DataProvider
// singleton. With two Clients A and B pointing at different sources,
// an eviction+repopulate sequence on A's cache followed by a B
// BundleComponents call could return values or manifests resolved
// against A's recipe source. That gap is closed; the metadata store
// and component registry were already per-Client at the time and
// stayed correct throughout, so ResolveRecipe results never drifted.
//
// # Synchronization
//
// Read-locks Client.mu so a concurrent Close can't race the values
// load. The lock is held only across the snapshot of c.builder and
// c.dp; the values and manifest reads themselves run unlocked
// (consistent with ResolveRecipe's pattern). The DataProvider
// snapshot is the per-Client provider this Client owns — the same
// one its Builder is bound to via recipe.WithDataProvider.
func (c *Client) BundleComponents(ctx context.Context, r *RecipeResult) ([]ComponentBundle, error) {
	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if r == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "nil RecipeResult")
	}
	if r.internal == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"RecipeResult has no internal recipe state — call Client.ResolveRecipe to obtain a bundle-able RecipeResult")
	}
	if err := c.assertOwns(r); err != nil {
		return nil, err
	}

	// Snapshot Client state under the read lock — same pattern as
	// ResolveRecipe. Capture both builder (for the closed-Client
	// check) and dp (the per-Client DataProvider used to bound
	// values + manifest reads to this Client's own recipe source).
	// Release the lock before iterating components; the reads
	// themselves don't touch Client state.
	c.mu.RLock()
	if c.builder == nil {
		c.mu.RUnlock()
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	dp := c.dp
	c.inflight.Add(1)
	c.mu.RUnlock()
	defer c.inflight.Done()

	// Apply a hard deadline so callers that pass an unbounded
	// context still get a bounded bundle pass. context.WithTimeout
	// honors the smaller of the parent deadline and ours, so a
	// caller passing a tighter deadline keeps their value. Placed
	// AFTER the nil-receiver, nil-result, and closed-Client guards
	// so tests that pass already-canceled contexts still flow
	// through the same paths they did before.
	ctx, cancel := context.WithTimeout(ctx, defaults.RecipeOperationTimeout)
	defer cancel()

	// Honor an early ctx cancellation before doing the (potentially
	// disk-bound) values reads.
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled before bundling", err)
	}

	bundles := make([]ComponentBundle, 0, len(r.Components))
	for i := range r.Components {
		// Bail on every iteration so a long recipe doesn't hold
		// onto a canceled context.
		if err := ctx.Err(); err != nil {
			return bundles, errors.Wrap(errors.ErrCodeTimeout, "context cancelled mid-bundle", err)
		}

		facade := r.Components[i]
		bundle := ComponentBundle{Component: facade}

		// Normalise the Kind so callers that emit lowercased
		// kinds ("helm", "kustomize") and callers that emit the
		// canonical-cased kinds ("Helm", "Kustomize") both bundle
		// successfully. Downstream deployment code typically accepts
		// both forms, so the AICR contract intentionally matches.
		// Anything that doesn't normalise to one of the two
		// known kinds is rejected, not silently dropped — a
		// typo like "Helm " (trailing space) or "kustom" used
		// to fall through the default branch and return an
		// empty ComponentBundle with no signal to the caller.
		switch strings.ToLower(facade.Kind) {
		case "helm":
			// Per-Client isolation: r.internal carries the producing
			// Client's recipe.DataProvider via its unexported `provider`
			// field (set during ResolveRecipe → builder.BuildFromCriteria
			// → LoadMetadataStoreFor). GetValuesForComponent uses that
			// bound provider directly; no explicit dp arg needed here.
			values, err := r.internal.GetValuesForComponentWithContext(ctx, facade.Name)
			if err != nil {
				return bundles, errors.Wrap(errors.ErrCodeInternal,
					"resolve values for component "+facade.Name, err)
			}
			// Empty values map → nil HelmValues so callers can
			// distinguish "no recipe-contributed values" from
			// "explicit empty map" (the latter would marshal as
			// "{}\n", non-nil bytes).
			if len(values) > 0 {
				// sigs.k8s.io/yaml routes through encoding/json, which
				// emits map keys in sorted order — that determinism is
				// load-bearing here because downstream attestation
				// digests BundleStatement.HelmValues. Do NOT swap to
				// gopkg.in/yaml.v3 (randomized map order) without
				// switching to serializer.MarshalYAMLDeterministic.
				out, marshalErr := yaml.Marshal(values)
				if marshalErr != nil {
					return bundles, errors.Wrap(errors.ErrCodeInternal,
						"marshal Helm values for component "+facade.Name, marshalErr)
				}
				bundle.HelmValues = out
			}
			// Helm components MAY also carry supplemental manifest
			// files (e.g., gpu-operator's overlay attaches a
			// dcgm-exporter manifest, h100-gke-cos-training attaches
			// gke-nccl-tcpxo manifests). These are raw resources the
			// deployer should apply alongside the Helm release. Load
			// them into Manifests using the same multi-doc stitching
			// path Kustomize uses; downstream consumers split the
			// stream and apply each document. A Helm component
			// without manifestFiles leaves Manifests nil — the
			// existing one-Release-per-component path is unchanged.
			manifests, err := loadManifestFiles(ctx, r.internal, dp, facade.Name)
			if err != nil {
				return bundles, err
			}
			bundle.Manifests = manifests
		case "kustomize":
			// Kustomize components carry rendered manifests via
			// their ComponentRef.ManifestFiles — stitch each file's
			// content into a single multi-doc YAML byte slice.
			manifests, err := loadManifestFiles(ctx, r.internal, dp, facade.Name)
			if err != nil {
				return bundles, err
			}
			bundle.Manifests = manifests
		default:
			// Reject explicitly: a silent empty bundle hides
			// typos at the recipe-emit boundary and the caller
			// has no way to distinguish "component had nothing
			// to bundle" from "component Kind was unrecognized".
			return bundles, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("component %q has unknown Kind %q (expected Helm or Kustomize)",
					facade.Name, facade.Kind))
		}

		bundles = append(bundles, bundle)
	}

	return bundles, nil
}

// CollectSnapshot deploys the snapshotter Job to the cluster identified
// by cfg.Kubeconfig and returns the captured Snapshot.
//
// CollectSnapshot does NOT consult the Client's recipe data provider —
// the Client is required only to keep the facade surface uniform
// (every public operation goes through a Client) and to leave room
// for future per-Client telemetry hooks or cluster-connection caching
// without breaking signatures. CollectSnapshot is therefore safe even
// on a Client whose recipe source is unrelated to the target cluster.
//
// cfg.Kubeconfig is the path (or empty for in-cluster). cfg.Namespace,
// cfg.Image, cfg.ServiceAccountName must be set; other fields fall
// back to package defaults documented on snapshotter.AgentConfig.
//
// Errors:
//   - ErrCodeInvalidRequest when the Client is nil, cfg is nil, or
//     the Client has been Closed.
//   - All snapshotter errors propagate unwrapped — they already
//     carry the appropriate pkg/errors codes (ErrCodeInternal for
//     deployment failures, ErrCodeTimeout for context expiry, etc.).
//
// Concurrent CollectSnapshot calls are safe; each call constructs an
// independent run.
func (c *Client) CollectSnapshot(ctx context.Context, cfg *AgentConfig) (*Snapshot, error) {
	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if cfg == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "AgentConfig is required")
	}

	// Closed-Client check uses the same lock pattern as ResolveRecipe /
	// BundleComponents so a concurrent Close can't race with this read.
	c.mu.RLock()
	closed := c.builder == nil
	if closed {
		c.mu.RUnlock()
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	c.inflight.Add(1)
	c.mu.RUnlock()
	defer c.inflight.Done()

	// Apply a facade-level deadline so a caller passing context.Background()
	// still gets a bounded operation. Preference order:
	//   1. cfg.Timeout — caller-controlled, wins when set.
	//   2. SnapshotOperationTimeout — package default (matches CLISnapshotTimeout).
	// context.WithTimeout honors the smaller of the parent deadline and
	// the value supplied here, so callers with a tighter context keep it.
	cap := cfg.Timeout
	if cap <= 0 {
		cap = defaults.SnapshotOperationTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, cap)
	defer cancel()

	snap, err := snapshotter.DeployAndGetSnapshot(ctx, toInternalAgentConfig(cfg))
	if err != nil {
		return nil, err
	}
	return fromInternalSnapshot(snap), nil
}

// ValidateState evaluates a resolved recipe against an observed cluster
// snapshot, runs the selected validation phases (by default
// PhaseDeployment, PhaseConformance, PhasePerformance) in order, and
// returns one PhaseResult per phase run. Pass WithValidationPhases to
// restrict the run to a subset.
//
// recipe must come from a prior Client.ResolveRecipe call on this
// Client — it carries the unexported internal recipe state needed to
// drive constraint evaluation. Passing a RecipeResult constructed by
// the caller (or one produced by a different Client whose internal
// has since been evicted) returns ErrCodeInvalidRequest.
//
// snap is the Snapshot returned by Client.CollectSnapshot or by any
// other snapshotter source.
//
// opts configure the validator run. Pass WithValidationNoCluster(true)
// from unit tests so no Kubernetes resources are created and every
// check reports as "skipped". WithValidationNamespace, WithValidationRunID,
// WithValidationCleanup, WithValidationTolerations,
// WithValidationNodeSelector, and WithValidationPhases cover the
// production-controller knobs. The validator catalog loads through this
// Client's own DataProvider, so a Client built from FilesystemSource
// validates against that recipe source rather than the package global.
//
// Errors:
//   - ErrCodeInvalidRequest when the Client, recipe, or snap is nil,
//     when recipe lacks internal state, or when the Client has been
//     Closed.
//   - All validator errors propagate unwrapped — readiness-check
//     failures surface as ErrCodeInvalidRequest, infrastructure
//     failures as ErrCodeInternal.
//
// All phases run by default and produce results regardless of earlier
// failures. Pass WithValidationFailFast(true) to stop after the first
// failed phase (useful for skipping expensive checks like inference-perf
// when deployment already failed). Callers wanting per-phase control can
// reach into pkg/validator.ValidatePhase directly.
func (c *Client) ValidateState(
	ctx context.Context,
	recipe *RecipeResult,
	snap *Snapshot,
	opts ...ValidateOption,
) ([]*PhaseResult, error) {

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
			"RecipeResult has no internal recipe state — call Client.ResolveRecipe to obtain a validatable RecipeResult")
	}
	if err := c.assertOwns(recipe); err != nil {
		return nil, err
	}
	if snap == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "nil Snapshot")
	}

	c.mu.RLock()
	closed := c.builder == nil
	if closed {
		c.mu.RUnlock()
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	// Snapshot the per-Client provider so the validator catalog loads
	// from THIS Client's recipe source rather than the package global.
	// version is snapshotted under the same lock so it can be threaded into
	// the validator (it rewrites :latest images to the release tag and
	// populates AICR_CLI_VERSION). It doesn't change after construction, so
	// the lock is belt-and-suspenders, but keeps the read uniform with dp.
	dp := c.dp
	clientVersion := c.version
	c.inflight.Add(1)
	c.mu.RUnlock()
	defer c.inflight.Done()

	// ValidateOption is a facade-owned wrapper that captures into an
	// internal validateConfig. Build the config once so we can read BOTH
	// the derived []validator.Option AND the configured phases from a
	// single options pass — the phases are a ValidatePhases parameter,
	// not a validator.Option, so they can't ride in the option slice.
	// A future renamed or added validator.With* is a one-line edit in
	// validateOptionsFromConfig and zero edits on the facade surface.
	cfg := buildValidateConfig(opts)

	// Apply a facade-level deadline only as opted into by WithValidationTimeout,
	// mirroring MakeBundle. The default (cfg.timeout == nil) keeps the
	// ValidationOperationTimeout cap (75m), which sits ABOVE the largest
	// per-check Job timeout (the 65m inference-perf catalog timeout; the
	// CheckExecutionTimeout fallback is 55m), so a single hung check fires its
	// own per-check timeout first and surfaces as a structured check failure
	// rather than a wrapping deadline-exceeded that loses the per-check signal —
	// the behavior controllers rely on. It is a per-check ordering guarantee and
	// a coarse operation cap, not a bound on the serial sum of an all-phase run
	// (checks run serially), so a heavy all-phase run should set an explicit
	// timeout. A non-nil *0 imposes NO facade cap (the CLI path, where
	// per-validator timeouts govern an all-phase run that can include the 65m
	// inference-perf check); a non-nil >0 sets that explicit
	// cap. context.WithTimeout honors the smaller of the parent deadline and
	// ours, so a tighter caller deadline always wins.
	switch {
	case cfg.timeout == nil:
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaults.ValidationOperationTimeout)
		defer cancel()
	case *cfg.timeout > 0:
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *cfg.timeout)
		defer cancel()
	default:
		// *cfg.timeout == 0: no facade cap; run under the caller's ctx as-is.
	}

	// Reject unknown phase values before any cluster work. The CLI --phase
	// flag parses through validator.ParsePhase, but the facade's
	// WithValidationPhases takes typed Phase values directly — so a caller
	// typo like aicr.Phase("deploymnt") would otherwise reach ValidatePhases
	// and surface as an empty/skipped result instead of an error. Fail
	// closed with ErrCodeInvalidRequest.
	if len(cfg.phases) > 0 {
		valid := make([]string, len(validator.PhaseOrder))
		for i, p := range validator.PhaseOrder {
			valid[i] = string(p)
		}
		for _, p := range cfg.phases {
			if _, ok := validator.ParsePhase(string(p)); !ok {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("invalid validation phase %q (valid: %s)",
						string(p), strings.Join(valid, ", ")))
			}
		}
	}
	valOpts := validateOptionsFromConfig(cfg)
	// Thread the Client's provider so catalog.Load reads from this
	// Client's recipe source (nil dp falls back to the package global
	// inside validator.WithDataProvider). Appended after the user opts
	// so it isn't overridden by a translated option.
	valOpts = append(valOpts, validator.WithDataProvider(dp))
	// Thread the Client's version: the validator catalog uses it to rewrite
	// :latest images to the release tag and to populate AICR_CLI_VERSION. The
	// pre-facade CLI passed validator.WithVersion(version); without this the
	// facade silently dropped it. Empty version (controllers that don't set
	// WithVersion) is the validator's "unset" sentinel and changes nothing.
	valOpts = append(valOpts, validator.WithVersion(clientVersion))
	v := validator.New(valOpts...)

	// cfg.phases is nil unless WithValidationPhases was set; nil → the
	// validator runs PhaseOrder (all phases) per its documented default.
	// The internal recipe pointer is the same one BundleComponents uses,
	// threading the per-Client data provider through without re-resolving
	// the recipe. ValidatePhases takes a *v1.ValidationInput, not a
	// *recipe.RecipeResult, on github/main (post-PR #1015/#1066 refactor
	// that promoted validation inputs into the v1 catalog package).
	// ToValidationInput translates the internal recipe result into that
	// shape without re-resolving the recipe.
	internalPhases := make([]validator.Phase, len(cfg.phases))
	for i, p := range cfg.phases {
		internalPhases[i] = validator.Phase(p)
	}
	results, err := v.ValidatePhases(ctx, internalPhases, validatorv1.ToValidationInput(recipe.internal), toInternalSnapshot(snap))
	if err != nil {
		return nil, err
	}
	return fromInternalPhaseResults(results), nil
}

// loadManifestFiles concatenates the recipe-attached ManifestFiles for
// a component into a single multi-doc YAML byte slice.
//
// Both Helm and Kustomize components may carry ManifestFiles:
//   - Kustomize components use ManifestFiles as their primary payload —
//     no Helm chart, just raw manifests stitched together.
//   - Helm components use ManifestFiles for SUPPLEMENTAL resources the
//     deployer should apply alongside the chart (e.g., gpu-operator's
//     dcgm-exporter overlay or h100-gke-cos-training's gke-nccl-tcpxo
//     manifests).
//
// Files are joined with a "\n---\n" separator so the result is a
// canonical multi-doc YAML stream callers can split with the standard
// `\n---\n` boundary or a yaml.NewYAMLOrJSONDecoder. A component with
// no ManifestFiles returns (nil, nil) — callers treat nil as "no
// supplemental manifests for this component."
//
// Errors:
//   - ErrCodeTimeout when ctx is canceled between manifest reads. The
//     helper rechecks ctx.Err() each iteration so a component with many
//     manifestFiles doesn't continue reading after the caller has given
//     up. The underlying provider.ReadFile itself is not ctx-aware
//     (DataProvider has no ctx parameter), so a single read in progress
//     when cancellation fires runs to completion — the bound is one
//     extra read per cancellation, not the whole remaining list.
//   - ErrCodeInternal when the component name isn't present on the
//     internal RecipeResult (would be a builder bug).
//   - ErrCodeInternal wrapped around the underlying read error when a
//     listed manifest file can't be loaded from the data provider.
func loadManifestFiles(ctx context.Context, internal *recipe.RecipeResult, dp recipe.DataProvider, componentName string) ([]byte, error) {
	ref := internal.GetComponentRef(componentName)
	if ref == nil {
		return nil, errors.New(errors.ErrCodeInternal,
			"component "+componentName+" missing from internal RecipeResult")
	}
	if len(ref.ManifestFiles) == 0 {
		return nil, nil
	}
	var combined []byte
	for _, path := range ref.ManifestFiles {
		// Bail before each read so a canceled caller doesn't keep
		// stitching manifests they no longer want. ctx.Err() is
		// cheap; doing this per-file gives a bounded worst case of
		// one in-flight read after cancellation.
		if err := ctx.Err(); err != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout,
				"context canceled mid-manifest-load", err)
		}
		// Read manifests via the per-Client DataProvider so multi-
		// Client processes don't cross-contaminate. dp may be nil if
		// the caller is the legacy CLI/API server path (Client always
		// supplies a non-nil dp); GetManifestContentWithProvider falls
		// back to the package global in that case.
		content, err := recipe.GetManifestContentWithContext(ctx, dp, path)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				"read manifest "+path, err)
		}
		if len(combined) > 0 {
			combined = append(combined, []byte("\n---\n")...)
		}
		combined = append(combined, content...)
	}
	return combined, nil
}
