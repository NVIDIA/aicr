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
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
	"gopkg.in/yaml.v3"
)

const baseRecipeName = "base"

// storeCacheEntry holds a lazily-built MetadataStore (or load error) keyed by
// the DataProvider identity. sync.Once guarantees that concurrent callers for
// the same provider populate the entry exactly once and all observe the same
// singleton; distinct providers populate distinct entries and never share
// state. This is the in-process multi-tenant isolation primitive that
// replaces the former package-global cachedMetadataStore/cachedMetadataGen
// triple — see LoadMetadataStoreFor.
type storeCacheEntry struct {
	once  sync.Once
	store *MetadataStore
	err   error
}

// storeCache holds storeCacheEntry pointers keyed by DataProvider identity.
// Two Builders bound to different DataProvider values populate distinct
// entries; a single provider value yields a single shared store regardless
// of caller goroutine count.
var storeCache sync.Map // map[DataProvider]*storeCacheEntry

// MetadataStore holds the base recipe and all overlays.
type MetadataStore struct {
	// Base is the base recipe metadata.
	Base *RecipeMetadata

	// Overlays is a list of overlay recipes indexed by name.
	Overlays map[string]*RecipeMetadata

	// OverlaySources maps overlay name to its data provenance string
	// (e.g., "embedded", "external"). Populated during buildMetadataStore
	// by calling provider.Source(path) for each overlay file.
	OverlaySources map[string]string

	// Mixins is a map of composable mixin fragments indexed by name.
	Mixins map[string]*RecipeMixin

	// ValuesFiles contains embedded values file contents indexed by filename.
	ValuesFiles map[string][]byte

	// provider is the DataProvider that produced this store. Components and
	// callers that need provider-bound lookups (component registry, manifest
	// content) should consult this field rather than the embedded default so
	// per-provider isolation holds.
	provider DataProvider
}

// pendingRegistryEntry stages an overlay's criteria + provider source so the
// loader can defer the actual seedCriteriaRegistry call until after the
// full catalog passes validation.
type pendingRegistryEntry struct {
	criteria *Criteria
	source   string
}

const constraintContextKey = "constraint"

// LoadMetadataStoreFor loads (and caches) the metadata store for the supplied
// DataProvider. Concurrent callers with the same provider observe the same
// singleton; distinct providers populate distinct cache entries and never
// share state. This is the multi-tenant entry point used by Builders bound
// via WithDataProvider.
//
// A nil provider falls back to the embedded catalog
// (defaultEmbeddedProvider), matching the legacy loadMetadataStore(ctx)
// entry point below.
//
// Context cancellation that fires during the first build is surfaced AND
// auto-evicted: when entry.err is context.Canceled or context.DeadlineExceeded
// (per isTransientLoadError), the cache entry is removed via
// storeCache.CompareAndDelete so the next caller for the same provider loads
// from scratch with its own ctx. Without the auto-eviction the sync.Once
// semantics would otherwise lock every subsequent caller into the first
// caller's cancellation error. Non-transient errors (file-not-found, schema
// invalid, dependency cycle) ARE preserved by sync.Once — they're
// deterministic for the provider and concurrent callers shouldn't all re-run
// the same broken walk.
//
// Callers no longer need to drop the entry via EvictCachedStore for a
// transient retry; EvictCachedStore remains for tests that mutate the
// provider's backing data between loads.
//
// Note: only the first caller's ctx governs the build. Subsequent callers that
// arrive while the build is in flight block on the same sync.Once and do not
// observe their own ctx until the first caller's build returns. Callers that
// need strict per-request deadline enforcement (e.g., HTTP handlers bound by
// ServerHandlerTimeout running alongside a slower CLI loader) should invoke
// LoadMetadataStoreFor in a goroutine and select on their own ctx.Done() and
// the result channel.
func LoadMetadataStoreFor(ctx context.Context, dp DataProvider) (*MetadataStore, error) {
	if dp == nil {
		dp = defaultEmbeddedProvider
	}
	e, _ := storeCache.LoadOrStore(dp, &storeCacheEntry{})
	entry := e.(*storeCacheEntry)
	entry.once.Do(func() {
		entry.store, entry.err = buildMetadataStore(ctx, dp)
	})
	if entry.err != nil && isTransientLoadError(entry.err) {
		// Don't permanently cache a transient ctx cancellation. With
		// sync.Once semantics the first caller's cancellation would
		// poison every later caller for this dp, even those whose
		// own contexts are healthy. Drop the entry so the next call
		// retries from scratch with the new caller's ctx.
		//
		// CompareAndDelete only removes the entry if it still matches
		// this *storeCacheEntry — a concurrent retry that already
		// stored a fresh entry is left undisturbed.
		storeCache.CompareAndDelete(dp, e)
	}
	return entry.store, entry.err
}

// isTransientLoadError reports whether err is a context cancellation
// or deadline-exceeded — the failure classes that should not be
// permanently cached. A "real" config error (file not found, schema
// invalid) is deterministic for a given DataProvider and stays cached
// so concurrent callers don't all re-run the same broken walk.
func isTransientLoadError(err error) bool {
	return stderrors.Is(err, context.Canceled) || stderrors.Is(err, context.DeadlineExceeded)
}

// loadMetadataStore is the back-compat entry point that loads from the
// embedded catalog (defaultEmbeddedProvider — never a --data tree). New
// callers — especially those that need per-tenant isolation — should use
// LoadMetadataStoreFor directly with a caller-supplied provider.
func loadMetadataStore(ctx context.Context) (*MetadataStore, error) {
	return LoadMetadataStoreFor(ctx, defaultEmbeddedProvider)
}

// buildMetadataStore performs the actual catalog walk against the supplied
// provider. It is pure with respect to shared package state — the
// only side effect on package state is the call to seedCriteriaRegistry,
// which seeds the criteria registry bound to the supplied provider (via
// GetCriteriaRegistryFor) from every overlay's spec.criteria. So loading a
// provider's metadata store seeds THAT provider's criteria registry, exactly
// like the metadata store and component registry are per-provider.
//
// The returned MetadataStore has its provider field set so downstream
// lookups (e.g., applyRegistryDefaults) can route through the originating
// provider rather than the embedded default.
func buildMetadataStore(ctx context.Context, provider DataProvider) (*MetadataStore, error) {
	// Record cache miss on first load for the provider.
	recipeCacheMisses.Inc()

	store := &MetadataStore{
		Overlays:       make(map[string]*RecipeMetadata),
		OverlaySources: make(map[string]string),
		Mixins:         make(map[string]*RecipeMixin),
		ValuesFiles:    make(map[string][]byte),
		provider:       provider,
	}

	// Staged criteria registry entries. Each overlay's criteria are
	// appended during the walk but only applied to this provider's
	// registry after every later validation (walk completion, base
	// presence, dependency validation) succeeds. Keeps the registry
	// transactional with respect to catalog-load success.
	var pendingRegistry []pendingRegistryEntry

	// Load all YAML files from data directory.
	walkErr := provider.WalkDir(ctx, "", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to walk data directory", err)
		}
		if ctx.Err() != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeTimeout, "context canceled during metadata load", ctx.Err())
		}
		if d.IsDir() {
			return nil
		}

		filename := filepath.Base(path)

		// Skip health check assert files (not recipe metadata)
		if strings.Contains(path, "checks/") {
			return nil
		}

		// Handle mixin files (files in the mixins/ directory)
		if strings.HasPrefix(path, "mixins/") {
			if !strings.HasSuffix(filename, ".yaml") {
				return nil
			}
			content, readErr := provider.ReadFile(ctx, path)
			if readErr != nil {
				return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, fmt.Sprintf("failed to read mixin %s", path), readErr)
			}
			var mixinHeader RecipeMetadataHeader
			if parseErr := yaml.Unmarshal(content, &mixinHeader); parseErr != nil {
				return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest,
					fmt.Sprintf("failed to parse mixin header %s", path), parseErr)
			}
			if headerErr := validateRecipeMixinCatalogHeader(
				mixinHeader.Kind, mixinHeader.APIVersion, path,
			); headerErr != nil {
				return headerErr
			}
			var mixin RecipeMixin
			decoder := yaml.NewDecoder(bytes.NewReader(content))
			decoder.KnownFields(true)
			if parseErr := decoder.Decode(&mixin); parseErr != nil {
				return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("failed to parse mixin %s (unknown fields are not allowed)", path), parseErr)
			}
			if _, exists := store.Mixins[mixin.Metadata.Name]; exists {
				return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					fmt.Sprintf("duplicate mixin name %q in %s", mixin.Metadata.Name, path))
			}
			if pathErr := validateConstraintPaths(
				mixin.Spec.Constraints, path, locSpecConstraints); pathErr != nil {
				return pathErr
			}
			store.Mixins[mixin.Metadata.Name] = &mixin
			slog.Debug("loaded mixin", "name", mixin.Metadata.Name, "path", path)
			return nil
		}

		// Handle component files (files in the components/ directory)
		if strings.Contains(path, "components/") {
			content, readErr := provider.ReadFile(ctx, path)
			if readErr != nil {
				return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, fmt.Sprintf("failed to read component file %s", path), readErr)
			}
			// Store with relative path (e.g., "components/cert-manager/values.yaml")
			store.ValuesFiles[path] = content
			return nil
		}

		// Skip non-YAML files
		if !strings.HasSuffix(filename, ".yaml") {
			return nil
		}

		// Skip old data-v1.yaml format and registry.yaml (handled separately)
		if filename == "data-v1.yaml" || filename == "registry.yaml" {
			return nil
		}

		// Read and parse metadata file
		content, readErr := provider.ReadFile(ctx, path)
		if readErr != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, fmt.Sprintf("failed to read %s", path), readErr)
		}

		var metadataHeader RecipeMetadataHeader
		if parseErr := yaml.Unmarshal(content, &metadataHeader); parseErr != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("failed to parse header for %s", path), parseErr)
		}
		isRecipeMetadata, profileVersion, headerErr := classifyRecipeMetadataCatalogHeader(
			&metadataHeader, path,
		)
		if headerErr != nil {
			return headerErr
		}
		if !isRecipeMetadata {
			slog.Debug("skipping non-recipe YAML", "path", path, "kind", metadataHeader.Kind)
			return nil
		}

		var metadata RecipeMetadata
		if profileVersion {
			decoder := yaml.NewDecoder(bytes.NewReader(content))
			decoder.KnownFields(true)
			if parseErr := decoder.Decode(&metadata); parseErr != nil {
				return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest,
					fmt.Sprintf("failed to parse %s as strict %s RecipeMetadata", path, metadataHeader.APIVersion),
					parseErr)
			}
			var trailing any
			if parseErr := decoder.Decode(&trailing); !stderrors.Is(parseErr, io.EOF) {
				if parseErr == nil {
					return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
						fmt.Sprintf("profile RecipeMetadata %s contains multiple YAML documents", path))
				}
				return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest,
					fmt.Sprintf("failed to check trailing content in %s", path), parseErr)
			}
		} else if parseErr := yaml.Unmarshal(content, &metadata); parseErr != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("failed to parse %s", path), parseErr)
		}

		if profileErr := ValidateRecipeMetadataProfile(&metadata); profileErr != nil {
			return aicrerrors.PropagateOrWrap(profileErr, aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid profile declaration in %s", path))
		}
		if profileVersion && metadata.Metadata.Name == "" {
			return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile RecipeMetadata %s requires metadata.name", path))
		}

		// Reject non-addressable constraint measurement paths before this file
		// joins the store (#1783). Returning here also keeps the staged
		// criteria registry transactional: the seedCriteriaRegistry commit
		// loop below the walk never runs.
		if pathErr := validateSpecConstraintPaths(&metadata.Spec, path); pathErr != nil {
			return pathErr
		}

		// Categorize as base or overlay
		// base.yaml is now in overlays/ directory but still identified by filename
		if filename == "base.yaml" && strings.Contains(path, "overlays/") {
			store.Base = &metadata
		} else {
			store.Overlays[metadata.Metadata.Name] = &metadata
			store.OverlaySources[metadata.Metadata.Name] = provider.Source(path)
			// Fail closed when an external overlay gates on nodes: nodes no
			// longer participates in Criteria.Matches() (#1781), so the
			// overlay would silently match every query and override configs
			// it was never intended to reach. This fires at catalog load time
			// (not request time) and surfaces as ErrCodeInvalidRequest so
			// callers can distinguish it from internal errors. Operators must
			// either remove criteria.nodes from their overlay or strip it to zero.
			isExternal := provider.Source(path) == CatalogSourceExternal
			hasNodes := metadata.Spec.Criteria != nil && metadata.Spec.Criteria.Nodes != 0
			if isExternal && hasNodes {
				return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					fmt.Sprintf("external overlay %q sets criteria.nodes=%d which is no longer used for overlay selection (#1781): "+
						"nodes is metadata-only and the overlay would silently match every query; "+
						"remove or zero criteria.nodes in your --data catalog",
						metadata.Metadata.Name, metadata.Spec.Criteria.Nodes))
			}
			// Stage this overlay's criteria for registration; the
			// actual call to seedCriteriaRegistry is deferred until
			// after every overlay parses cleanly, the base recipe is
			// found, and dependency validation passes — see the
			// commit loop below the walk. This prevents a malformed
			// file later in the walk from leaving partial criteria
			// values in this provider's registry.
			pendingRegistry = append(pendingRegistry, pendingRegistryEntry{
				criteria: metadata.Spec.Criteria,
				source:   provider.Source(path),
			})
		}

		return nil
	})

	if walkErr != nil {
		// A bare context.Canceled / DeadlineExceeded surfaces here when the
		// provider returns ctx.Err() before invoking the per-entry callback
		// (the in-flight cancellation guard inside DataProvider.WalkDir).
		// LoadMetadataStoreFor keys its transient-eviction logic on
		// stderrors.Is(err, context.Canceled) and downstream callers depend
		// on the ErrCodeTimeout shape; wrap so both invariants hold.
		if stderrors.Is(walkErr, context.Canceled) || stderrors.Is(walkErr, context.DeadlineExceeded) {
			return nil, aicrerrors.Wrap(aicrerrors.ErrCodeTimeout, "context canceled during metadata load", walkErr)
		}
		return nil, walkErr
	}

	if store.Base == nil {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInternal, "base.yaml not found")
	}

	// Validate base recipe dependencies
	if err := store.Base.Spec.ValidateDependencies(); err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "base recipe validation failed", err)
	}

	// Catalog fully validated — commit staged criteria registrations
	// to this provider's registry now. Any earlier error return path
	// above leaves the registry untouched. Routing through provider
	// keeps the criteria registry per-provider, mirroring the
	// per-provider metadata store and component registry.
	for _, entry := range pendingRegistry {
		seedCriteriaRegistry(entry.criteria, entry.source, provider)
	}

	return store, nil
}

func validateRecipeMixinCatalogHeader(kind, apiVersion, path string) error {
	if kind != RecipeMixinKind {
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("mixin file %s has kind %q, expected %q; use a RecipeMixin document compatible with this aicr release",
				path, kind, RecipeMixinKind))
	}
	if !header.IsSupportedAuthoringAPIVersion(apiVersion) {
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("mixin file %s has apiVersion %q, expected %q or %q for %s; update the catalog header for this aicr release",
				path, apiVersion, header.GroupVersion, header.GroupVersionV1Beta1, RecipeMixinKind))
	}
	header.WarnDeprecatedAPIVersion(path, apiVersion, header.GroupVersionV1Beta1)
	return nil
}

func classifyRecipeMetadataCatalogHeader(
	metadata *RecipeMetadataHeader,
	path string,
) (bool, bool, error) {

	if metadata.Kind != RecipeMetadataKind {
		if isKnownNonMetadataAICRKind(metadata.Kind) {
			return false, false, nil
		}
		if strings.HasPrefix(metadata.APIVersion, header.APIGroup+"/") {
			return false, false, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("AICR catalog file %s has kind %q, expected %q; fix the wire kind or move the unrelated document outside the recipe catalog",
					path, metadata.Kind, RecipeMetadataKind))
		}
		return false, false, nil
	}

	profileVersion := header.IsSupportedProfileAPIVersion(metadata.APIVersion)
	if !profileVersion && !header.IsSupportedAuthoringAPIVersion(metadata.APIVersion) {
		return false, false, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("RecipeMetadata file %s has apiVersion %q, expected %q, %q, %q, or %q; update the catalog header for this aicr release",
				path, metadata.APIVersion, header.GroupVersion, header.GroupVersionV1Beta1,
				header.RecipeResultGroupVersion, header.GroupVersionV1Beta2))
	}
	// Naming the wrong target would be worse than naming none: the reader would
	// follow it into a second failure.
	target := header.GroupVersionV1Beta1
	if profileVersion {
		target = header.GroupVersionV1Beta2
	}
	header.WarnDeprecatedAPIVersion(path, metadata.APIVersion, target)
	return true, profileVersion, nil
}

func isKnownNonMetadataAICRKind(kind string) bool {
	switch kind {
	case "AICRConfig", "BundleProvenance", ComponentRegistryKind,
		RecipeCriteriaKind, RecipeMixinKind, RecipeResultKind,
		string(header.KindRecipe), string(header.KindSnapshot):

		return true
	default:
		return false
	}
}

// EvictCachedStore drops the cached MetadataStore for the supplied provider.
// No-op when the provider has no cache entry. Safe on nil — callers do not
// need to guard. Used by tests that mutate a provider's backing data between
// loads, and by Task 7's eviction tests.
func EvictCachedStore(dp DataProvider) {
	if dp == nil {
		return
	}
	storeCache.Delete(dp)
}

// CachedStoreCountForTesting returns the number of distinct DataProvider
// entries currently held in the metadata-store cache. Exposed for tests
// in the aicr facade that assert Client.Close evicts the cached store —
// paired with CachedRegistryCountForTesting in components.go so a single
// test can verify both halves of the per-Client cache are released.
//
// Test-only by convention (the _ForTesting suffix); never call from
// production code.
//
// NOTE: this returns the GLOBAL count across every DataProvider in the
// package. A parallel test in another package using its own
// DataProvider will perturb the count. Tests that need a stable signal
// scoped to a specific DataProvider should prefer
// CachedStoreContainsForTesting.
func CachedStoreCountForTesting() int {
	n := 0
	storeCache.Range(func(_, _ any) bool {
		n++
		return true
	})
	return n
}

// CachedStoreContainsForTesting reports whether the metadata-store
// cache has an entry for the supplied DataProvider. Scoped per-provider
// so it is robust under parallel test execution: each test that uses a
// distinct DataProvider can observe ONLY its own entry's
// presence/absence.
//
// Test-only by convention (the _ForTesting suffix); never call from
// production code.
func CachedStoreContainsForTesting(dp DataProvider) bool {
	_, ok := storeCache.Load(dp)
	return ok
}

// LoadCatalog eagerly loads the embedded recipe catalog into the package
// cache, which has the side effect of seeding the criteria registry from
// every embedded overlay's spec.criteria. Provider-bound catalogs (`--data`
// trees) are seeded the same way when LoadMetadataStoreFor first builds
// them — the CLI routes through the aicr.Client facade's LoadCatalog,
// which does exactly that for the client-bound provider. If the catalog
// is malformed, this surfaces the error before any criteria validation
// runs (and before the registry is half-populated).
func LoadCatalog(ctx context.Context) error {
	_, err := loadMetadataStore(ctx)
	return err
}

// ResetMetadataStoreForTesting clears every cached metadata store so tests
// can reload against fresh providers without leaking state across cases.
// Must only be called from tests.
func ResetMetadataStoreForTesting() {
	storeCache.Range(func(k, _ any) bool {
		storeCache.Delete(k)
		return true
	})
}

// GetValuesFile returns the content of a values file by filename.
func (s *MetadataStore) GetValuesFile(filename string) ([]byte, error) {
	content, exists := s.ValuesFiles[filename]
	if !exists {
		return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound, fmt.Sprintf("values file not found: %s", filename))
	}
	return content, nil
}

// GetRecipeByName returns a recipe metadata by name.
// Returns the base recipe if name is "base", otherwise looks up in overlays.
func (s *MetadataStore) GetRecipeByName(name string) (*RecipeMetadata, bool) {
	if name == "" || name == baseRecipeName {
		return s.Base, s.Base != nil
	}
	overlay, exists := s.Overlays[name]
	return overlay, exists
}

// resolveInheritanceChain builds the inheritance chain for a recipe.
// Returns recipes in order from root (base) to the target recipe.
// Detects cycles in the inheritance chain.
func (s *MetadataStore) resolveInheritanceChain(recipeName string) ([]*RecipeMetadata, error) {
	// Track visited recipes to detect cycles
	visited := make(map[string]bool)
	var chain []*RecipeMetadata

	currentName := recipeName
	for currentName != "" && currentName != baseRecipeName {
		// Check for cycle
		if visited[currentName] {
			return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("circular inheritance detected: recipe %q references itself in inheritance chain", currentName))
		}
		visited[currentName] = true

		// Get the recipe
		recipe, exists := s.GetRecipeByName(currentName)
		if !exists {
			return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound,
				fmt.Sprintf("recipe %q not found (referenced in inheritance chain)", currentName))
		}

		chain = append(chain, recipe)

		// Move to parent
		currentName = recipe.Spec.Base
	}

	// Reverse so chain goes from root (base) to target
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}

	// Prepend base at the start (root of all inheritance)
	if s.Base != nil {
		chain = append([]*RecipeMetadata{s.Base}, chain...)
	}

	return chain, nil
}

// FindMatchingOverlays finds all overlays that match the given criteria and
// returns maximal leaf candidates sorted by specificity (least specific first).
//
// Maximal leaf selection: after collecting all matching overlays, any overlay
// that is an ancestor (via spec.base chain) of another matching overlay is
// filtered out. Only the most-specific leaves survive as candidates. Their
// full inheritance chains are still resolved during merging, so ancestor
// content is not lost — it is just not applied as a separate independent
// candidate.
//
// This is used by both BuildRecipeResult and BuildRecipeResultWithEvaluator
// to ensure consistent candidate selection regardless of call site.
func (s *MetadataStore) FindMatchingOverlays(criteria *Criteria) []*RecipeMetadata {
	matches := make([]*RecipeMetadata, 0, len(s.Overlays))

	for _, overlay := range s.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		if overlay.Spec.Criteria.Matches(criteria) {
			matches = append(matches, overlay)
		}
	}

	// Filter to maximal leaf candidates
	matches = s.filterToMaximalLeaves(matches)

	// Sort by specificity (least specific first, so more specific overlays are applied later).
	// SliceStable guarantees deterministic output when overlays share the same specificity.
	sort.SliceStable(matches, func(i, j int) bool {
		si, sj := matches[i].Spec.Criteria.Specificity(), matches[j].Spec.Criteria.Specificity()
		if si != sj {
			return si < sj
		}
		return matches[i].Metadata.Name < matches[j].Metadata.Name
	})

	return matches
}

// filterToMaximalLeaves removes any matching overlay that is an ancestor
// (via spec.base chain) of another matching overlay. This ensures only the
// most-specific leaves are returned as candidates.
func (s *MetadataStore) filterToMaximalLeaves(matches []*RecipeMetadata) []*RecipeMetadata {
	// Build set of all ancestors of matching overlays
	ancestors := make(map[string]bool)
	for _, overlay := range matches {
		visited := make(map[string]bool)
		base := overlay.Spec.Base
		for base != "" && base != baseRecipeName {
			if visited[base] {
				break // cycle detected — stop walking
			}
			visited[base] = true
			ancestors[base] = true
			if recipe, exists := s.GetRecipeByName(base); exists {
				base = recipe.Spec.Base
			} else {
				break
			}
		}
	}

	// Keep only overlays that are not ancestors of another match
	leaves := make([]*RecipeMetadata, 0, len(matches))
	for _, overlay := range matches {
		if !ancestors[overlay.Metadata.Name] {
			leaves = append(leaves, overlay)
		}
	}

	if filtered := len(matches) - len(leaves); filtered > 0 {
		slog.Debug("filtered ancestor overlays from candidates",
			"removed", filtered, "remaining", len(leaves))
	}

	return leaves
}

// mergeMixins resolves and merges mixin fragments referenced by spec.mixins.
// Mixins are merged after the inheritance chain, contributing only constraints
// and componentRefs.
//
// Constraint semantics: a mixin constraint whose name already exists in the
// inheritance chain (or another mixin) is rejected — constraints don't have a
// merge semantic, so a name collision is unambiguously a conflict.
//
// ComponentRef semantics: a mixin componentRef whose name already exists is
// allowed when the mixin entry sets nothing beyond the safe additive field
// set ({Namespace, ManifestFiles, PreManifestFiles}), OR sets only Overrides
// paths the target component's own registry entry has explicitly
// allowlisted for mixin use (see mixinOverridesSafeForMerge). Every other
// identity/sourcing field (Chart, Type, Source, Version, Tag, Path,
// ValuesFile, Patches, DependencyRefs, Cleanup, ExpectedResources,
// HealthCheckAsserts) still conflicts unconditionally — this preserves
// ADR-005's "no silent chart identity override" mitigation while letting
// OS-conditional mixins like os-talos contribute namespace +
// preManifestFiles overrides, and opted-in mixins like
// nvsentinel-observability contribute allowlisted values, to components
// already declared upstream.
//
// The Mixins field is cleared from the result afterward. Returns the set of
// mixin-contributed constraint names for post-compose evaluation.
func (s *MetadataStore) mergeMixins(ctx context.Context, mergedSpec *RecipeMetadataSpec) (map[string]bool, error) {
	mixinConstraintNames := make(map[string]bool)
	if len(mergedSpec.Mixins) == 0 {
		return mixinConstraintNames, nil
	}

	// Build index of existing constraint and component names for conflict detection
	existingConstraints := make(map[string]bool)
	for _, c := range mergedSpec.Constraints {
		existingConstraints[c.Name] = true
	}
	existingComponents := make(map[string]bool)
	for _, c := range mergedSpec.ComponentRefs {
		existingComponents[c.Name] = true
	}

	for _, mixinName := range mergedSpec.Mixins {
		mixin, exists := s.Mixins[mixinName]
		if !exists {
			return nil, aicrerrors.New(aicrerrors.ErrCodeNotFound,
				fmt.Sprintf("mixin %q not found in recipes/mixins/", mixinName))
		}

		// Constraint conflict: a mixin constraint with the same name as one
		// already in scope is unambiguously a conflict because constraints
		// don't compose.
		for _, c := range mixin.Spec.Constraints {
			if existingConstraints[c.Name] {
				return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					fmt.Sprintf("mixin %q constraint %q conflicts with inheritance chain or another mixin", mixinName, c.Name))
			}
		}

		// A mixin's own componentRefs list must not repeat a component
		// name: mergeMixins validates each entry against the pre-mixin
		// state (see below), never against a sibling entry in the SAME
		// mixin, and RecipeMetadataSpec.Merge collapses same-named refs
		// last-writer-wins with no further check -- two contradictory
		// entries for one component would both pass validation
		// individually and then silently resolve to whichever was last.
		if dupName, dup := duplicateComponentRefName(mixin.Spec.ComponentRefs); dup {
			return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("mixin %q declares component %q more than once in its own componentRefs list", mixinName, dupName))
		}

		// ComponentRef collision: a mixin may set any field freely on a
		// genuinely new component, but on one already in the chain, only
		// safe additive fields plus registry-allowlisted Overrides paths
		// (mixinOverridesSafeForMerge). See mixinComponentRefSafeForMerge.
		for _, c := range mixin.Spec.ComponentRefs {
			isExisting := existingComponents[c.Name]
			if isExisting {
				if offending, ok := mixinComponentRefSafeForMerge(c); !ok {
					return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
						fmt.Sprintf("mixin %q component %q sets identity/sourcing field %q which conflicts with the inheritance chain; mixins may only contribute Namespace, ManifestFiles, PreManifestFiles, or registry-allowlisted Overrides paths to an existing component", mixinName, c.Name, offending))
				}
			} else {
				// A component whose owner hasn't declared a
				// mixinSafeOverridePaths allowlist at all hasn't opted into
				// this mechanism -- a mixin introducing it fresh keeps the
				// pre-existing freedom to set any field (e.g.
				// agentgateway-crds, kubeflow-trainer). A component that HAS
				// an allowlist is protected the same way whether it's
				// already chained or being introduced fresh: its ValuesFile
				// content is never inspected, so it's rejected outright here
				// (like mixinComponentRefSafeForMerge already does for an
				// existing component) -- a mixin could otherwise smuggle a
				// non-allowlisted value (e.g. nvsentinel's excluded
				// global.tracing.endpoint) through a values file instead of
				// inline Overrides.
				hasAllowlist, err := componentHasMixinAllowlist(s.provider, c.Name)
				if err != nil {
					return nil, err
				}
				if !hasAllowlist {
					continue
				}
				if c.ValuesFile != "" {
					return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
						fmt.Sprintf("mixin %q component %q sets valuesFile %q while introducing an allowlisted component; a mixin may only supply such a component's values via registry-allowlisted inline overrides, never a values file", mixinName, c.Name, c.ValuesFile))
				}
			}
			if len(c.Overrides) == 0 {
				continue
			}
			var existingLayers []map[string]any
			if isExisting {
				existing, _ := findComponentRefByName(mergedSpec.ComponentRefs, c.Name)
				var err error
				existingLayers, err = existingRawOverrideLayers(ctx, s.provider, existing)
				if err != nil {
					return nil, err
				}
			}
			if err := mixinOverridesSafeForMerge(s.provider, mixinName, c.Name, c.Overrides, existingLayers); err != nil {
				return nil, err
			}
		}

		// Merge mixin content. Componentrefs with names matching existing
		// entries are merged via mergeComponentRef (see RecipeMetadataSpec.Merge).
		mixinSpec := RecipeMetadataSpec{
			Constraints:   mixin.Spec.Constraints,
			ComponentRefs: mixin.Spec.ComponentRefs,
		}
		mergedSpec.Merge(&mixinSpec)

		// Track mixin contributions for future conflict detection
		for _, c := range mixin.Spec.Constraints {
			existingConstraints[c.Name] = true
			mixinConstraintNames[c.Name] = true
		}
		for _, c := range mixin.Spec.ComponentRefs {
			existingComponents[c.Name] = true
		}

		slog.Debug("merged mixin", "name", mixinName,
			"constraints", len(mixin.Spec.Constraints),
			"components", len(mixin.Spec.ComponentRefs))
	}

	// Strip mixins from the materialized result — loader metadata only
	mergedSpec.Mixins = nil
	return mixinConstraintNames, nil
}

// mixinEvalResult holds the outcome of post-compose mixin constraint evaluation.
type mixinEvalResult struct {
	// Failed is true if any mixin constraint failed evaluation.
	Failed bool
	// ExcludedOverlays are the overlays excluded due to the failure.
	ExcludedOverlays []ExcludedOverlay
	// Warnings are the constraint warnings for the failing constraints.
	Warnings []ConstraintWarning
	// Spec is the rebuilt spec (without the failed candidate chains) if failed, or nil if all passed.
	Spec *RecipeMetadataSpec
	// AppliedOverlays is the surviving applied overlays if failed.
	AppliedOverlays []string
}

// evaluateMixinConstraints evaluates the fully composed constraint set
// (including mixin-contributed constraints) against the snapshot evaluator.
// This runs after mergeMixins so that constraints moved from inline overlay
// definitions to mixins are still validated against the snapshot.
//
// If any mixin constraint fails, only the candidate chains that contributed the
// failing mixin constraints are excluded. Independent overlays
// (e.g., monitoring-hpa) are preserved. This maintains the existing
// maximal-leaf filtering behavior for non-mixin overlays.
func (s *MetadataStore) evaluateMixinConstraints(
	ctx context.Context,
	mergedSpec *RecipeMetadataSpec,
	evaluator ConstraintEvaluatorFunc,
	mixinConstraintNames map[string]bool,
	candidateOverlays []string,
) (mixinEvalResult, error) {

	if evaluator == nil || len(mixinConstraintNames) == 0 {
		return mixinEvalResult{}, nil
	}

	constraintCandidates, err := s.buildMixinConstraintCandidateIndex(candidateOverlays)
	if err != nil {
		return mixinEvalResult{}, err
	}

	var failedConstraints []ConstraintWarning
	failedCandidates := make(map[string]bool)
	for _, constraint := range mergedSpec.Constraints {
		if !mixinConstraintNames[constraint.Name] {
			continue // already evaluated per-overlay
		}
		if constraintErr := validateConstraintWarningSource(constraint); constraintErr != nil {
			return mixinEvalResult{}, constraintErr
		}
		result := evaluator(constraint)
		if result.Error != nil && !isNotFoundEvalError(result.Error) {
			// Fail closed on non-NotFound evaluation errors (design 5.2).
			// ConstraintEvaluatorFunc returns a plain error; propagate it
			// as-is when it already carries a structured code, otherwise
			// wrap so it doesn't reach the server layer as an uncoded 500.
			return mixinEvalResult{}, aicrerrors.PropagateOrWrap(result.Error, aicrerrors.ErrCodeInternal, "constraint evaluation failed")
		}
		if !result.Passed {
			affectedCandidates := constraintCandidates[constraint.Name]
			if len(affectedCandidates) == 0 {
				return mixinEvalResult{}, aicrerrors.NewWithContext(
					aicrerrors.ErrCodeInternal,
					"failed to map mixin constraint to candidate chain",
					map[string]any{
						constraintContextKey: constraint.Name,
						"candidate_count":    len(candidateOverlays),
					},
				)
			}
			for _, candidate := range affectedCandidates {
				failedCandidates[candidate] = true
				failedConstraints = append(failedConstraints, ConstraintWarning{
					Overlay:    candidate,
					Constraint: constraint.Name,
					Expected:   constraint.Value,
					Actual:     result.Actual,
					Reason:     buildMixinConstraintWarningReason(constraint, result),
				})
			}
		}
	}

	if len(failedConstraints) == 0 {
		return mixinEvalResult{}, nil
	}

	var excluded []ExcludedOverlay
	survivingCandidates := make([]*RecipeMetadata, 0, len(candidateOverlays))
	for _, name := range candidateOverlays {
		if failedCandidates[name] {
			excluded = append(excluded, ExcludedOverlay{
				Name:   name,
				Reason: ExcludedOverlayReasonMixinConstraintFailed,
			})
			continue
		}
		overlay, exists := s.Overlays[name]
		if !exists {
			return mixinEvalResult{}, aicrerrors.New(
				aicrerrors.ErrCodeNotFound,
				fmt.Sprintf("overlay %q not found during mixin fallback rebuild", name),
			)
		}
		survivingCandidates = append(survivingCandidates, overlay)
	}

	// Rebuild from the surviving candidate leaves so any shared ancestors remain
	// present when still needed by another surviving chain.
	rebuiltSpec, survivingApplied := s.initBaseMergedSpec()
	survivingApplied, err = s.mergeOverlayChains(survivingCandidates, &rebuiltSpec, survivingApplied)
	if err != nil {
		return mixinEvalResult{}, err
	}
	if _, err := s.mergeMixins(ctx, &rebuiltSpec); err != nil {
		return mixinEvalResult{}, err
	}

	slog.Warn("post-compose constraint evaluation failed, excluding affected mixin chains",
		"failed_constraints", len(failedConstraints),
		"excluded", excluded,
		"surviving", survivingApplied)

	return mixinEvalResult{
		Failed:           true,
		ExcludedOverlays: excluded,
		Warnings:         failedConstraints,
		Spec:             &rebuiltSpec,
		AppliedOverlays:  survivingApplied,
	}, nil
}

func buildMixinConstraintWarningReason(constraint Constraint, result ConstraintEvalResult) string {
	if result.Error != nil {
		return fmt.Sprintf("mixin-constraint-failed: %s", result.Error.Error())
	}
	return fmt.Sprintf("mixin-constraint-failed: expected %s, got %s", constraint.Value, result.Actual)
}

func validateConstraintWarningSource(constraint Constraint) error {
	if constraint.Name == "" {
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			"constraint name is required for snapshot evaluation")
	}
	if constraint.Value == "" {
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("constraint %q value is required for snapshot evaluation", constraint.Name))
	}
	return nil
}

// buildMixinConstraintCandidateIndex maps mixin-contributed constraint names to
// the candidate leaf overlays whose inheritance chains contribute them.
func (s *MetadataStore) buildMixinConstraintCandidateIndex(candidateOverlays []string) (map[string][]string, error) {
	index := make(map[string][]string)
	for _, candidate := range candidateOverlays {
		chain, err := s.resolveInheritanceChain(candidate)
		if err != nil {
			return nil, aicrerrors.WrapWithContext(
				aicrerrors.ErrCodeInvalidRequest,
				"failed to resolve candidate chain for mixin constraint evaluation",
				err,
				map[string]any{"overlay": candidate},
			)
		}

		seen := make(map[string]bool)
		for _, recipe := range chain {
			for _, mixinName := range recipe.Spec.Mixins {
				mixin, exists := s.Mixins[mixinName]
				if !exists {
					continue
				}
				for _, constraint := range mixin.Spec.Constraints {
					if seen[constraint.Name] {
						continue
					}
					index[constraint.Name] = append(index[constraint.Name], candidate)
					seen[constraint.Name] = true
				}
			}
		}
	}

	return index, nil
}

// initBaseMergedSpec creates a copy of the base spec for overlay merging.
// Validation is deep-cloned so downstream Merge calls cannot reach back
// into the cached base ValidationConfig and mutate it.
func (s *MetadataStore) initBaseMergedSpec() (RecipeMetadataSpec, []string) {
	mergedSpec := RecipeMetadataSpec{
		Constraints:   make([]Constraint, len(s.Base.Spec.Constraints)),
		ComponentRefs: make([]ComponentRef, len(s.Base.Spec.ComponentRefs)),
		Validation:    cloneValidationConfig(s.Base.Spec.Validation),
	}
	copy(mergedSpec.Constraints, s.Base.Spec.Constraints)
	copy(mergedSpec.ComponentRefs, s.Base.Spec.ComponentRefs)
	return mergedSpec, []string{baseRecipeName}
}

// mergeOverlayChains resolves inheritance chains and merges overlays into the spec.
func (s *MetadataStore) mergeOverlayChains(overlays []*RecipeMetadata, mergedSpec *RecipeMetadataSpec, appliedOverlays []string) ([]string, error) {
	processedChains := make(map[string]bool)

	for _, overlay := range overlays {
		chain, err := s.resolveInheritanceChain(overlay.Metadata.Name)
		if err != nil {
			return appliedOverlays, aicrerrors.WrapWithContext(
				aicrerrors.ErrCodeInvalidRequest,
				"failed to resolve inheritance chain",
				err,
				map[string]any{
					"overlay": overlay.Metadata.Name,
				},
			)
		}

		// Skip base (index 0) since we already started with it
		for i := 1; i < len(chain); i++ {
			recipe := chain[i]
			if processedChains[recipe.Metadata.Name] {
				continue
			}
			processedChains[recipe.Metadata.Name] = true
			mergedSpec.Merge(&recipe.Spec)
			appliedOverlays = append(appliedOverlays, recipe.Metadata.Name)
		}
	}

	return appliedOverlays, nil
}

// finalizeRecipeResult validates, sorts, and builds the final RecipeResult.
// The provider is consulted when filling in ComponentRef defaults from the
// component registry so per-provider isolation holds. A nil provider falls
// back to the embedded catalog (via GetComponentRegistryFor's
// defaultEmbeddedProvider fallback).
func finalizeRecipeResult(provider DataProvider, criteria *Criteria, mergedSpec *RecipeMetadataSpec, appliedOverlays []string) (*RecipeResult, error) {
	if err := mergedSpec.ValidateDependencies(); err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "merged recipe validation failed", err)
	}

	deployOrder, err := mergedSpec.TopologicalSort()
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to compute deployment order", err)
	}

	// Canonicalize case-insensitively-matched types before defaulting, so the
	// exact-type switch in ApplyRegistryDefaults (and downstream deployers) see
	// the canonical constant. See issue #1584.
	canonicalizeComponentTypes(mergedSpec.ComponentRefs)

	if err := applyRegistryDefaults(provider, mergedSpec.ComponentRefs); err != nil {
		return nil, err
	}

	result := &RecipeResult{
		Kind:            RecipeResultKind,
		APIVersion:      RecipeResultAPIVersion,
		Criteria:        criteria,
		Constraints:     mergedSpec.Constraints,
		ComponentRefs:   mergedSpec.ComponentRefs,
		DeploymentOrder: deployOrder,
		Validation:      mergedSpec.Validation,
		provider:        provider,
	}
	result.Metadata.AppliedOverlays = appliedOverlays

	// Reject duplicate component-ref names before they reach ValidateCoherence
	// or any caller of BuildRecipeResult/ResolveRecipe. RecipeMetadataSpec.Merge
	// silently last-wins-collapses same-name refs via componentMap, so this
	// only fires when an external --data base carries duplicates AND no
	// matching overlay merge collapses them. Mirrors the same check
	// PrepareAndValidate runs at the file-load/adopt/bundle boundary, so the
	// criteria-resolve boundary (aicr recipe -o, aicr query,
	// Client.ResolveRecipe) fails closed on the same recipes too. See #1874.
	if err := validateRefNames(mergedSpec.ComponentRefs); err != nil {
		return nil, err
	}

	// Reject refs whose deployment-shape fields are incoherent (e.g. a Helm ref
	// that also carries a Kustomize tag/path), after defaults populate Type.
	// The same check also runs at the load and adopt boundaries (see
	// RecipeResult.ValidateCoherence callers) so externally-supplied hydrated
	// RecipeResults — the bundle/validate -r and POST /v1/bundle paths — are
	// covered too, not only criteria-resolved recipes. See issue #1584.
	if err := result.ValidateCoherence(); err != nil {
		return nil, err
	}

	return result, nil
}

// BuildRecipeResult builds a RecipeResult by merging base with matching overlays.
// Each matching overlay is resolved through its inheritance chain before merging.
// This enables multi-level inheritance: base → intermediate → overlay.
func (s *MetadataStore) BuildRecipeResult(ctx context.Context, criteria *Criteria) (*RecipeResult, error) {
	return s.BuildRecipeResultWithProfile(ctx, criteria, "")
}

// BuildRecipeResultWithProfile builds a RecipeResult and applies an explicit
// name=value profile selection, or the declaration's default when selection is
// empty.
func (s *MetadataStore) BuildRecipeResultWithProfile(ctx context.Context, criteria *Criteria, selection string) (*RecipeResult, error) {
	select {
	case <-ctx.Done():
		return nil, aicrerrors.WrapWithContext(
			aicrerrors.ErrCodeTimeout,
			"build recipe result context cancelled during initialization",
			ctx.Err(),
			map[string]any{keyStage: stageInitialization},
		)
	default:
	}

	overlays := s.FindMatchingOverlays(criteria)
	effectiveProfile, err := s.resolveProfileDeclaration(overlays)
	if err != nil {
		return nil, err
	}

	mergedSpec, appliedOverlays := s.initBaseMergedSpec()

	appliedOverlays, err = s.mergeOverlayChains(overlays, &mergedSpec, appliedOverlays)
	if err != nil {
		return nil, err
	}

	// Merge mixin fragments referenced by overlays in the chain
	if _, mixinErr := s.mergeMixins(ctx, &mergedSpec); mixinErr != nil {
		return nil, mixinErr
	}

	// Post-condition (issue #1542): every stated criteria dimension must be
	// honored by the final applied set. Runs after mergeOverlayChains so
	// ancestor-supplied coverage counts; mixins carry no criteria and cannot
	// change coverage.
	if coverageErr := s.verifyCriteriaCoverage(criteria, appliedOverlays, nil, nil); coverageErr != nil {
		return nil, coverageErr
	}

	if len(appliedOverlays) <= 1 {
		slog.Warn("no environment-specific overlays matched, using base configuration only",
			"criteria", criteria.String(),
			"hint", "recipe may not be optimized for your environment")
	}

	selected, err := applyEffectiveProfile(&mergedSpec, effectiveProfile, selection, nil)
	if err != nil {
		return nil, err
	}
	result, err := finalizeRecipeResult(s.provider, criteria, &mergedSpec, appliedOverlays) //nolint:contextcheck // finalizeRecipeResult routes registry lookups through GetComponentRegistryFor, which is sync.Once-cached and bounds first-load I/O via an internal bounded background context (loadComponentRegistryFor). Threading ctx here is tracked separately if/when the cascade needs caller-driven cancellation.
	if err != nil {
		return nil, err
	}
	if selected == nil {
		return result, nil
	}
	result.APIVersion = RecipeProfileAPIVersion
	result.Metadata.SelectedProfile = selected
	if err := result.ValidateProfileValuesWithContext(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// BuildRecipeResultWithEvaluator builds a RecipeResult by merging base with matching overlays,
// filtering overlays based on constraint evaluation using the provided evaluator function.
//
// This method extends BuildRecipeResult with constraint-aware filtering:
//   - Each overlay that matches by criteria is tested against its constraints
//   - Overlays with failing constraints are excluded from the merge
//   - Warnings about excluded overlays are included in the result metadata
//
// The evaluator function is called for each constraint in each matching overlay.
// If evaluator is nil, this method behaves identically to BuildRecipeResult.
func (s *MetadataStore) BuildRecipeResultWithEvaluator(ctx context.Context, criteria *Criteria, evaluator ConstraintEvaluatorFunc) (*RecipeResult, error) {
	return s.BuildRecipeResultWithEvaluatorAndProfile(ctx, criteria, evaluator, "")
}

// BuildRecipeResultWithEvaluatorAndProfile is the snapshot-filtered profile
// resolution path.
func (s *MetadataStore) BuildRecipeResultWithEvaluatorAndProfile(
	ctx context.Context,
	criteria *Criteria,
	evaluator ConstraintEvaluatorFunc,
	selection string,
) (*RecipeResult, error) {

	if evaluator == nil {
		return s.BuildRecipeResultWithProfile(ctx, criteria, selection)
	}

	select {
	case <-ctx.Done():
		return nil, aicrerrors.WrapWithContext(
			aicrerrors.ErrCodeTimeout,
			"build recipe result context cancelled during initialization",
			ctx.Err(),
			map[string]any{keyStage: stageInitialization},
		)
	default:
	}

	// Find matching overlays and filter by constraint evaluation
	overlays := s.FindMatchingOverlays(criteria)
	effectiveProfile, err := s.resolveProfileDeclaration(overlays)
	if err != nil {
		return nil, err
	}

	var filteredOverlays []*RecipeMetadata
	var excludedOverlays []ExcludedOverlay
	var constraintWarnings []ConstraintWarning

	for _, overlay := range overlays {
		slog.Debug("evaluating overlay constraints",
			"overlay", overlay.Metadata.Name,
			"constraint_count", len(overlay.Spec.Constraints))

		passed, warnings, evalErr := s.evaluateOverlayConstraints(overlay, evaluator)
		if evalErr != nil {
			return nil, evalErr
		}
		if passed {
			filteredOverlays = append(filteredOverlays, overlay)
			slog.Debug("overlay passed all constraints",
				"overlay", overlay.Metadata.Name)
		} else {
			excludedOverlays = append(excludedOverlays, ExcludedOverlay{
				Name:   overlay.Metadata.Name,
				Reason: ExcludedOverlayReasonConstraintFailed,
			})
			constraintWarnings = append(constraintWarnings, warnings...)
			slog.Info("excluding overlay due to constraint failures",
				"overlay", overlay.Metadata.Name,
				"failed_constraints", len(warnings))
		}
	}

	mergedSpec, appliedOverlays := s.initBaseMergedSpec()

	appliedOverlays, err = s.mergeOverlayChains(filteredOverlays, &mergedSpec, appliedOverlays)
	if err != nil {
		return nil, err
	}

	// Merge mixin fragments referenced by overlays in the chain.
	mixinConstraintNames, err := s.mergeMixins(ctx, &mergedSpec)
	if err != nil {
		return nil, err
	}

	// Evaluate mixin-contributed constraints against the snapshot.
	// Per-overlay constraints were evaluated before merge (above), but mixin
	// constraints are only present after mergeMixins. Without this post-compose
	// evaluation, a mixin constraint (e.g., kernel >= 6.8 from os-ubuntu) could
	// fail against the snapshot but the candidate would still be selected.
	candidateOverlays := make([]string, 0, len(filteredOverlays))
	for _, overlay := range filteredOverlays {
		candidateOverlays = append(candidateOverlays, overlay.Metadata.Name)
	}
	mixinResult, err := s.evaluateMixinConstraints(ctx, &mergedSpec, evaluator, mixinConstraintNames, candidateOverlays)
	if err != nil {
		return nil, err
	}
	if mixinResult.Failed {
		excludedOverlays = append(excludedOverlays, mixinResult.ExcludedOverlays...)
		constraintWarnings = append(constraintWarnings, mixinResult.Warnings...)
		mergedSpec = *mixinResult.Spec
		appliedOverlays = mixinResult.AppliedOverlays
	}

	survivingProfile, err := s.resolveAppliedProfileDeclaration(appliedOverlays)
	if err != nil {
		return nil, err
	}
	if survivalErr := ensureProfileDeclarationSurvived(
		effectiveProfile, survivingProfile, excludedOverlays, constraintWarnings,
	); survivalErr != nil {
		return nil, survivalErr
	}

	// Post-condition (issue #1542): runs against the FINAL applied set —
	// after per-overlay constraint exclusion AND the mixin-failure fallback
	// rebuild — so a stated dimension whose only coverage was excluded
	// fails loudly instead of shipping a silent partial (design 5.4).
	if err = s.verifyCriteriaCoverage(criteria, appliedOverlays, excludedOverlays, constraintWarnings); err != nil {
		return nil, err
	}

	if len(excludedOverlays) > 0 {
		slog.Warn("some overlays were excluded due to constraint failures",
			"excluded", excludedOverlays,
			"applied", appliedOverlays,
			"criteria", criteria.String())
	}

	if len(appliedOverlays) <= 1 {
		if len(excludedOverlays) > 0 {
			slog.Warn("all matching overlays were excluded due to constraint failures, using base configuration only",
				"excluded_count", len(excludedOverlays),
				"criteria", criteria.String())
		} else {
			slog.Warn("no environment-specific overlays matched, using base configuration only",
				"criteria", criteria.String(),
				"hint", "recipe may not be optimized for your environment")
		}
	}

	selected, err := applyEffectiveProfile(&mergedSpec, effectiveProfile, selection, evaluator)
	if err != nil {
		return nil, err
	}
	result, err := finalizeRecipeResult(s.provider, criteria, &mergedSpec, appliedOverlays) //nolint:contextcheck // see BuildRecipeResult: registry I/O is sync.Once-cached + bounded inside loadComponentRegistryFor.
	if err != nil {
		return nil, err
	}
	if selected != nil {
		result.APIVersion = RecipeProfileAPIVersion
		result.Metadata.SelectedProfile = selected
		if err := result.ValidateProfileValuesWithContext(ctx); err != nil {
			return nil, err
		}
	}
	result.Metadata.ExcludedOverlays = excludedOverlays
	result.Metadata.ConstraintWarnings = constraintWarnings

	return result, nil
}

// evaluateOverlayConstraints evaluates all constraints in an overlay.
// Returns true if all constraints pass, false otherwise.
// Returns warnings for clean mismatches and NotFound evaluation errors (the
// graceful-degradation signal); any other evaluation error aborts immediately
// and is returned as the third value instead of being folded into a warning.
// The third return value is non-nil (the evaluator's own structured error,
// code preserved) for any non-NotFound evaluation error — a malformed
// constraint or internal evaluation failure must not silently degrade the
// recipe (issue #1542, design 5.2).
func (s *MetadataStore) evaluateOverlayConstraints(overlay *RecipeMetadata, evaluator ConstraintEvaluatorFunc) (bool, []ConstraintWarning, error) {
	if len(overlay.Spec.Constraints) == 0 {
		// No constraints means the overlay passes
		return true, nil, nil
	}

	var warnings []ConstraintWarning
	allPassed := true

	for _, constraint := range overlay.Spec.Constraints {
		if err := validateConstraintWarningSource(constraint); err != nil {
			return false, nil, err
		}
		result := evaluator(constraint)

		switch {
		case result.Error != nil && !isNotFoundEvalError(result.Error):
			// Fail closed: a malformed constraint or internal evaluation
			// failure must not silently degrade the recipe (issue #1542,
			// design 5.2). ConstraintEvaluatorFunc returns a plain error;
			// propagate as-is when it already carries a structured code,
			// otherwise wrap so it doesn't reach the server layer uncoded.
			return false, nil, aicrerrors.PropagateOrWrap(result.Error, aicrerrors.ErrCodeInternal, "constraint evaluation failed")
		case result.Error != nil:
			// NotFound: the snapshot does not exhibit this measurement —
			// the designed graceful-degradation signal. Exclude with warning.
			warnings = append(warnings, ConstraintWarning{
				Overlay:    overlay.Metadata.Name,
				Constraint: constraint.Name,
				Expected:   constraint.Value,
				Actual:     result.Actual,
				Reason:     result.Error.Error(),
			})
			allPassed = false
			slog.Debug("constraint evaluation error",
				"overlay", overlay.Metadata.Name,
				constraintContextKey, constraint.Name,
				"error", result.Error)
		case !result.Passed:
			warnings = append(warnings, ConstraintWarning{
				Overlay:    overlay.Metadata.Name,
				Constraint: constraint.Name,
				Expected:   constraint.Value,
				Actual:     result.Actual,
				Reason:     fmt.Sprintf("expected %s, got %s", constraint.Value, result.Actual),
			})
			allPassed = false
			slog.Debug("constraint failed",
				"overlay", overlay.Metadata.Name,
				constraintContextKey, constraint.Name,
				"expected", constraint.Value,
				"actual", result.Actual)
		default:
			slog.Debug("constraint passed",
				"overlay", overlay.Metadata.Name,
				constraintContextKey, constraint.Name,
				"expected", constraint.Value,
				"actual", result.Actual)
		}
	}

	return allPassed, warnings, nil
}

// mixinComponentRefSafeForMerge reports whether a mixin's componentRef sets
// only fields that are unconditionally safe to merge into an existing
// component (Name, Namespace, ManifestFiles, PreManifestFiles). Identity /
// sourcing fields (Chart, Type, Source, Version, Tag, Path, ValuesFile,
// Patches, DependencyRefs, Cleanup, ExpectedResources, HealthCheckAsserts)
// silently override the chain's chosen chart and so a mixin must NOT set
// them — see ADR-005's "Silent constraint override" mitigation. Returns the
// first offending field name so the resolver's error message names the
// violation rather than handing the recipe author a generic "conflict"
// message.
//
// Overrides is deliberately NOT one of the unconditionally-safe fields, but
// also not unconditionally rejected here: mergeMixins checks it separately
// via mixinOverridesSafeForMerge, which allows only registry-allowlisted
// paths (ComponentConfig.MixinSafeOverridePaths, declared by the target
// component's own owner) that don't collide with a value the chain already
// set. Allowing Overrides unconditionally would let any mixin silently
// change any already-chained component's behavior with no code-review-
// visible signal at the composing leaf — exactly the hazard ADR-005's
// "Silent constraint override" mitigation exists to block; the per-path
// allowlist is what keeps that mitigation intact while still letting a
// mixin touch values its target explicitly opted in for.
//
// The check is symmetric with the merge semantics in mergeComponentRef: the
// safe set is exactly the set of fields the merge handles additively or as
// pure namespace remap. Any new ComponentRef field that joins the additive
// set must also be added here.
func mixinComponentRefSafeForMerge(c ComponentRef) (string, bool) {
	switch {
	case c.Chart != "":
		return "chart", false
	case c.Type != "":
		return "type", false
	case c.Source != "":
		return "source", false
	case c.Version != "":
		return "version", false
	case c.Tag != "":
		return "tag", false
	case c.Path != "":
		return "path", false
	case c.ValuesFile != "":
		return "valuesFile", false
	case len(c.Patches) > 0:
		return "patches", false
	case len(c.DependencyRefs) > 0:
		return "dependencyRefs", false
	case c.Cleanup:
		return "cleanup", false
	case len(c.ExpectedResources) > 0:
		return "expectedResources", false
	case c.HealthCheckAsserts != "":
		return "healthCheckAsserts", false
	case c.HealthCheckSkip:
		return "healthCheckSkip", false
	}
	return "", true
}

// findComponentRefByName returns the ComponentRef named name from refs, and
// whether it was found. Linear scan: refs holds at most a few dozen
// components, and this only runs once per mixin componentRef collision.
func findComponentRefByName(refs []ComponentRef, name string) (ComponentRef, bool) {
	for _, r := range refs {
		if r.Name == name {
			return r, true
		}
	}
	return ComponentRef{}, false
}

// duplicateComponentRefName returns the first component name that appears
// more than once in refs, and whether one was found. refs is a single
// mixin's own componentRefs list -- at most a handful of entries, so a
// linear scan with a small seen-set is simplest.
func duplicateComponentRefName(refs []ComponentRef) (string, bool) {
	seen := make(map[string]bool, len(refs))
	for _, r := range refs {
		if seen[r.Name] {
			return r.Name, true
		}
		seen[r.Name] = true
	}
	return "", false
}

// componentHasMixinAllowlist reports whether name's registry entry declares
// a non-empty MixinSafeOverridePaths. A component the owner hasn't opted
// into this mechanism at all (no entry, or an entry with no allowlist) is
// unaffected by mixin-override validation -- e.g. agentgateway-crds and
// kubeflow-trainer, whose mixins have always freely supplied their initial
// ValuesFile/Overrides when introducing them fresh.
func componentHasMixinAllowlist(provider DataProvider, name string) (bool, error) {
	registry, err := GetComponentRegistryFor(provider)
	if err != nil {
		return false, aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeInternal, "load component registry for mixin override validation")
	}
	comp := registry.Get(name)
	return comp != nil && len(comp.MixinSafeOverridePaths) > 0, nil
}

// mixinOverridesSafeForMerge validates a mixin's Overrides against the
// target component's registry-declared MixinSafeOverridePaths allowlist,
// and against paths already set by the leaf's own inheritance chain or an
// earlier-merged mixin (existingLayers — nil for a component the mixin is
// introducing fresh). mergeMixins folds each mixin's contribution into
// mergedSpec.ComponentRefs before moving to the next, so this one check
// covers both leaf-vs-mixin and mixin-vs-mixin collisions.
func mixinOverridesSafeForMerge(provider DataProvider, mixinName, componentName string, mixinOverrides map[string]any, existingLayers []map[string]any) error {
	registry, err := GetComponentRegistryFor(provider)
	if err != nil {
		return aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeInternal, "load component registry for mixin override validation")
	}
	var allowlist []string
	if comp := registry.Get(componentName); comp != nil {
		allowlist = comp.MixinSafeOverridePaths
	}

	mixinContext := fmt.Sprintf("mixin %q component %q overrides", mixinName, componentName)

	// Checked before either walk below -- see rejectEmptyMapValues' doc
	// comment for why an empty map must be rejected outright rather than
	// caught by the path-level checks that follow.
	if emptyMapErr := rejectEmptyMapValues(mixinOverrides, mixinContext); emptyMapErr != nil {
		return emptyMapErr
	}

	mixinPaths, err := overrideLeafPaths(mixinOverrides, mixinContext)
	if err != nil {
		return err
	}

	for _, p := range mixinPaths {
		if !slices.Contains(allowlist, p) {
			return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("mixin %q component %q sets overrides path %q, which is not in the component's registry-declared mixinSafeOverridePaths allowlist %v — a mixin may only set paths its target component has explicitly opted in for mixin use", mixinName, componentName, p, allowlist))
		}
	}

	// Checked per mixin path against each raw existing layer (base
	// values.yaml, ValuesFile, inline Overrides), not a flattened walk of
	// the existing side: pathConfiguredInRaw only ever indexes a mixin's
	// own (small, allowlisted) leaf paths, so an unrelated dotted key or a
	// null-cleared value elsewhere in the tree never has to be interpreted.
	for _, mp := range mixinPaths {
		for _, layer := range existingLayers {
			if existingPath, collide := pathConfiguredInRaw(layer, mp); collide {
				return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					fmt.Sprintf("mixin %q component %q overrides path %q collides with path %q already set by the leaf's own chain or another mixin — mixins may not silently overwrite a value the chain already configured", mixinName, componentName, mp, existingPath))
			}
		}
	}

	// Composing onto an already-disabled target (overrides.enabled: false)
	// is allowed but likely a no-op the recipe author should know about --
	// warn rather than block, since disabling is a legitimate chain
	// decision this validation doesn't own.
	if enabledVal, ok := existingConfiguredBool(existingLayers, "enabled"); ok && !enabledVal {
		slog.Warn(fmt.Sprintf("mixin %q sets overrides on component %q, which the recipe's inheritance chain already disables (overrides.enabled: false) -- these values will have no effect unless something later re-enables the component", mixinName, componentName),
			"mixin", mixinName, "component", componentName)
	}
	return nil
}

// existingConfiguredBool returns the value and presence of key in the
// highest-precedence layer of existingLayers (ordered low to high, as
// existingRawOverrideLayers returns them) that sets it at all, mirroring
// resolveComponentValues' merge precedence without needing the merged
// result: a layer that sets key to a non-bool (including explicit null)
// still wins over a lower layer's bool, matching mergeValues' last-
// writer-wins semantics.
func existingConfiguredBool(existingLayers []map[string]any, key string) (bool, bool) {
	for i := len(existingLayers) - 1; i >= 0; i-- {
		if v, exists := existingLayers[i][key]; exists {
			b, isBool := v.(bool)
			return b, isBool
		}
	}
	return false, false
}

// rejectEmptyMapValues walks overrides and errors on the first empty map
// value found at any depth, naming contextLabel (e.g. mixin+component) in
// the message since the walk itself has no notion of where overrides came
// from. Scoped to mixin overrides only (not leaf/chain overrides, which
// are not mixin-constrained): a mixin declaring an empty map anywhere is
// always a mistake, whether it sits at an ancestor of an allowlisted path
// (would silently blank whatever the leaf already set there) or at the
// exact leaf path itself (would pass the allowlist check verbatim and
// write a map where the chart expects a scalar).
func rejectEmptyMapValues(overrides map[string]any, contextLabel string) error {
	var walk func(map[string]any, string) error
	walk = func(values map[string]any, prefix string) error {
		for key, value := range values {
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			nested, ok := value.(map[string]any)
			if !ok {
				continue
			}
			if len(nested) == 0 {
				return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					fmt.Sprintf("%s: path %q is an empty map ({}); a mixin override must set a real "+
						"scalar or non-empty map, never an empty one -- an empty map can silently blank an "+
						"existing value at an ancestor path, or pass an allowlist check on the leaf path "+
						"itself and write a map where the chart expects a scalar", contextLabel, path))
			}
			if err := walk(nested, path); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(overrides, "")
}

// overrideLeafPaths flattens a nested Overrides map into sorted dotted leaf
// paths, e.g. {"global":{"tracing":{"enabled":true}}} produces
// ["global.tracing.enabled"]. An empty nested map still contributes its own
// path (not skipped): deepMergeMap treats a map-typed source value, even an
// empty one, as authoritative over a non-map destination and REPLACES it
// (dst[k] = DeepCopyAnyMap(svMap), regardless of len(svMap)) -- so
// {"global":{"tracing":{}}} can silently blank an existing scalar at
// global.tracing during the real merge. Reporting the path itself lets the
// allowlist/collision checks catch that: the empty-map path won't match any
// leaf-level allowlist entry, so it's rejected the same as any other
// non-allowlisted path, and it collides with an existing "global.tracing"
// entry the normal way. contextLabel is named in the one error this can
// return, since the walk itself has no notion of where overrides came from.
func overrideLeafPaths(overrides map[string]any, contextLabel string) ([]string, error) {
	var paths []string
	var walk func(map[string]any, string) error
	walk = func(values map[string]any, prefix string) error {
		for key, value := range values {
			if key == "" || strings.Contains(key, ".") {
				return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					fmt.Sprintf("%s: override key %q must be nonempty and may not contain a literal dot", contextLabel, key))
			}
			path := key
			if prefix != "" {
				path = prefix + "." + key
			}
			if nested, ok := value.(map[string]any); ok {
				if len(nested) == 0 {
					paths = append(paths, path)
					continue
				}
				if err := walk(nested, path); err != nil {
					return err
				}
				continue
			}
			paths = append(paths, path)
		}
		return nil
	}
	if err := walk(overrides, ""); err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

// pathsIntersect reports whether a and b name the same value, or one is an
// ancestor of the other in dotted-path notation (e.g. "global.tracing" and
// "global.tracing.enabled" intersect: a map set at the parent path replaces
// everything under it, and a leaf set at the child path collides with
// whatever the parent already established). Mirrors the ancestor/
// descendant intersection test pkg/bundler/validations/checks.go's
// dynamicPathIntersections uses for the equivalent --dynamic-path problem.
func pathsIntersect(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+".") || strings.HasPrefix(b, a+".")
}

// pathConfiguredInRaw walks path's dotted segments through raw (an
// unmerged, un-null-deleted values map) and reports the shallowest dotted
// prefix raw has explicitly set (any value, including null or an empty
// map), or ("", false) if no prefix is present. Stops at the first
// non-map or empty-map segment, since path can't exist any deeper there,
// and that ancestor is itself a configured value. Only ever indexes
// path's own segments, so it never has to interpret unrelated keys
// elsewhere in raw (e.g. a dotted podAnnotations entry).
func pathConfiguredInRaw(raw map[string]any, path string) (string, bool) {
	segments := strings.Split(path, ".")
	cur := raw
	for i, seg := range segments {
		val, exists := cur[seg]
		if !exists {
			return "", false
		}
		prefix := strings.Join(segments[:i+1], ".")
		nested, isMap := val.(map[string]any)
		if !isMap || len(nested) == 0 || i == len(segments)-1 {
			return prefix, true
		}
		cur = nested
	}
	return "", false
}

// existingRawOverrideLayers returns the unmerged raw values maps that
// configure ref — component base values.yaml (if ref.ValuesFile is set
// and differs from it), ref.ValuesFile's content, and ref.Overrides — for
// collision detection via pathConfiguredInRaw. Deliberately not
// resolveComponentValues' merged result: null-deletion during that merge
// would make "never mentioned" indistinguishable from "explicitly cleared."
func existingRawOverrideLayers(ctx context.Context, provider DataProvider, ref ComponentRef) ([]map[string]any, error) {
	if provider == nil {
		provider = defaultEmbeddedProvider
	}
	var layers []map[string]any

	if ref.ValuesFile != "" {
		baseValuesFile := fmt.Sprintf("components/%s/values.yaml", ref.Name)
		if ref.ValuesFile != baseValuesFile {
			if baseData, err := provider.ReadFile(ctx, baseValuesFile); err == nil {
				var baseValues map[string]any
				if err := yaml.Unmarshal(baseData, &baseValues); err != nil {
					return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal,
						fmt.Sprintf("parse base values file %q for component %q mixin collision check", baseValuesFile, ref.Name), err)
				}
				layers = append(layers, baseValues)
			}
		}
		data, err := provider.ReadFile(ctx, ref.ValuesFile)
		if err != nil {
			return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal,
				fmt.Sprintf("read values file %q for component %q mixin collision check", ref.ValuesFile, ref.Name), err)
		}
		var vfValues map[string]any
		if err := yaml.Unmarshal(data, &vfValues); err != nil {
			return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal,
				fmt.Sprintf("parse values file %q for component %q mixin collision check", ref.ValuesFile, ref.Name), err)
		}
		layers = append(layers, vfValues)
	}

	if len(ref.Overrides) > 0 {
		layers = append(layers, ref.Overrides)
	}

	return layers, nil
}

// applyRegistryDefaults fills in ComponentRef fields from ComponentConfig
// defaults AND hydrates healthCheck.assertFile content via the bound
// DataProvider. Returns an error if the registry cannot be loaded or if
// hydration cannot read a declared assertFile — silently no-op'ing would
// emit partial ComponentRefs that downstream bundlers / validators would
// reject far from the root cause.
//
// The provider parameter routes both the registry lookup and assertFile
// reads through a specific DataProvider so per-provider isolation holds.
// A nil provider falls back to the embedded catalog via
// GetComponentRegistryFor's defaultEmbeddedProvider fallback.
//
// See hydrateHealthCheckAsserts for the skip / inline-wins / no-assertFile
// rules and #1219 for the motivation.
func applyRegistryDefaults(provider DataProvider, refs []ComponentRef) error {
	registry, err := GetComponentRegistryFor(provider)
	if err != nil {
		return aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeInternal, "failed to get component registry for defaults")
	}

	for i := range refs {
		config := registry.Get(refs[i].Name)
		if config != nil {
			refs[i].ApplyRegistryDefaults(config)
		}
	}

	return hydrateHealthCheckAsserts(provider, registry, refs)
}

// hydrateHealthCheckAsserts loads each registry-declared healthCheck.assertFile
// through the bound DataProvider and stamps the content onto the matching
// ComponentRef.HealthCheckAsserts. Hydration is skipped when:
//   - the overlay has set HealthCheckSkip (rollback / external-data override
//     path — see ComponentRef field doc), or
//   - the overlay already declared HealthCheckAsserts inline (the inline
//     value wins; never silently overwrite caller intent), or
//   - the registry has no assertFile entry for this component.
//
// Hydration runs unconditionally even when the overlay declares
// ExpectedResources. The deployment validator's previous mutex
// (`len(ref.ExpectedResources) == 0`) was dropped in PR #1220 so both
// the chainsaw path and the ExpectedResources path now execute
// side-by-side with source-tagged CLI output. The transitional skip
// added in PR #1234 was removed in lockstep — see the
// k8s-nim-operator case study in #660 for context on why both signals
// are useful (registry asserts deeper Pod-level readiness; overlay
// ExpectedResources asserts the operator-installed singleton Deployment
// is healthy).
//
// Disabled components (overrides.enabled: false) ARE hydrated unconditionally
// so the on-disk recipe.yaml artifact carries the same content regardless of
// enablement; runtime execution is filtered separately by enabledComponentRefs.
//
// Read-bounded with defaults.FileReadTimeout per call to mirror
// loadComponentRegistryFor — a hung backing store can't park recipe
// resolution indefinitely. nil provider falls back to the embedded default,
// matching applyRegistryDefaults / GetComponentRegistryFor.
//
//nolint:contextcheck // ctx is bounded internally; threading caller ctx would require a public-API change to applyRegistryDefaults and every transitive caller — same tradeoff as loadComponentRegistryFor.
func hydrateHealthCheckAsserts(provider DataProvider, registry *ComponentRegistry, refs []ComponentRef) error {
	if provider == nil {
		provider = defaultEmbeddedProvider
	}
	for i := range refs {
		ref := &refs[i]
		if ref.HealthCheckSkip {
			continue
		}
		if ref.HealthCheckAsserts != "" {
			continue
		}
		config := registry.Get(ref.Name)
		if config == nil || config.HealthCheck.AssertFile == "" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), defaults.FileReadTimeout)
		data, err := provider.ReadFile(ctx, config.HealthCheck.AssertFile)
		cancel()
		if err != nil {
			// PropagateOrWrap so a structured ReadFile error (e.g.,
			// ErrCodeTimeout from the bounded ctx, or ErrCodeNotFound from
			// the layered provider) preserves its inner code instead of
			// being flattened to ErrCodeInternal. Falls back to Wrap for
			// non-structured stdlib errors (e.g., fs.ErrNotExist), which
			// is what the embedded provider surfaces today.
			return aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeInternal,
				fmt.Sprintf("failed to read healthCheck.assertFile for component %q from %q",
					ref.Name, config.HealthCheck.AssertFile))
		}
		ref.HealthCheckAsserts = string(data)
	}
	return nil
}
