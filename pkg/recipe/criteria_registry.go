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
	"os"
	"sort"
	"strings"
	"sync"
)

// CriteriaField enumerates the criteria dimensions tracked by the registry.
// Using typed constants instead of bare strings prevents stringly-typed
// mismatches between registration sites and lookup sites.
type CriteriaField string

const (
	// FieldService is the Kubernetes service criteria dimension
	// (e.g., eks, gke, aks, …).
	FieldService CriteriaField = "service"
	// FieldAccelerator is the GPU/accelerator criteria dimension
	// (e.g., h100, gb200, …).
	FieldAccelerator CriteriaField = "accelerator"
	// FieldIntent is the workload intent criteria dimension
	// (e.g., training, inference).
	FieldIntent CriteriaField = "intent"
	// FieldOS is the GPU node operating-system criteria dimension
	// (e.g., ubuntu, cos, …).
	FieldOS CriteriaField = "os"
	// FieldPlatform is the platform/framework criteria dimension
	// (e.g., kubeflow, nim, …).
	FieldPlatform CriteriaField = "platform"
)

// CriteriaOrigin identifies whether a registered value came from the
// embedded OSS catalog or from a runtime-mounted external catalog (--data).
// Strict mode uses the distinction to reject external-only values when
// validating the upstream catalog in CI.
type CriteriaOrigin int

const (
	// OriginEmbedded marks values contributed by overlays loaded from the
	// AICR binary's embedded data filesystem (i.e., the OSS catalog).
	OriginEmbedded CriteriaOrigin = iota
	// OriginExternal marks values contributed by overlays loaded from
	// `--data` (the per-invocation extension path).
	OriginExternal
)

// strictModeEnvVar is the environment variable that toggles strict-mode
// criteria validation when set to a truthy value ("1", "true", "yes",
// case-insensitive). Wired into CI via the Makefile and honored on every
// invocation so the upstream OSS catalog cannot accidentally depend on
// external-only criteria values.
const strictModeEnvVar = "AICR_CRITERIA_STRICT"

// CriteriaRegistry holds the catalog-discovered set of valid criteria
// values per dimension, with origin tracking so strict mode can
// distinguish embedded (OSS) from external (--data) contributions.
//
// The static switch arms inside ParseCriteria{Service,Accelerator,Intent,
// OS,Platform}Type remain the fast path for canonical / aliased values
// (e.g., "self-managed" → "any", "al2" → "amazonlinux"). Anything the
// switches do not recognize falls through to the registry on lookup.
type CriteriaRegistry struct {
	mu     sync.RWMutex
	values map[CriteriaField]map[string]CriteriaOrigin
	strict bool
}

// newCriteriaRegistry is the package-private alias for NewCriteriaRegistry,
// kept so existing in-package callers (and tests) read naturally.
func newCriteriaRegistry() *CriteriaRegistry {
	return NewCriteriaRegistry()
}

// NewCriteriaRegistry constructs an empty criteria registry, honoring
// AICR_CRITERIA_STRICT at construction. Use this when you need an
// ephemeral registry that is not tied to a DataProvider — e.g. config
// YAML validation that runs before the Client/Builder is wired and only
// needs the hardcoded OSS fast-path values to validate. For
// per-DataProvider registries (the common case — seeded by overlays
// loaded for a specific provider) use GetCriteriaRegistryFor(dp).
func NewCriteriaRegistry() *CriteriaRegistry {
	r := &CriteriaRegistry{
		values: make(map[CriteriaField]map[string]CriteriaOrigin),
	}
	r.strict = isStrictModeFromEnv()
	return r
}

// Register records value under field with the given origin. Empty values
// and the wildcard "any" are ignored — they never need to be registered
// because the Parse functions handle them via the fast path. If the same
// (field, value) pair has been registered before, OriginEmbedded wins
// over OriginExternal so that an external overlay redeclaring a public
// value does not downgrade the value's origin and silently break strict
// mode.
func (r *CriteriaRegistry) Register(field CriteriaField, value string, origin CriteriaOrigin) {
	value = normalizeCriteriaValue(value)
	if value == "" || value == CriteriaAnyValue {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Lazy-init the outer map so a zero-value or externally constructed
	// CriteriaRegistry (i.e., one not built via NewCriteriaRegistry) does
	// not panic on first Register.
	if r.values == nil {
		r.values = make(map[CriteriaField]map[string]CriteriaOrigin)
	}

	bucket, ok := r.values[field]
	if !ok {
		bucket = make(map[string]CriteriaOrigin)
		r.values[field] = bucket
	}
	existing, present := bucket[value]
	if present && existing == OriginEmbedded {
		// Do not downgrade embedded to external.
		return
	}
	bucket[value] = origin
}

// Has reports whether value is known for field, regardless of origin.
// Returns false in strict mode unless the value originates from an
// embedded overlay.
func (r *CriteriaRegistry) Has(field CriteriaField, value string) bool {
	value = normalizeCriteriaValue(value)
	r.mu.RLock()
	defer r.mu.RUnlock()
	bucket, ok := r.values[field]
	if !ok {
		return false
	}
	origin, present := bucket[value]
	if !present {
		return false
	}
	if r.strict && origin != OriginEmbedded {
		return false
	}
	return true
}

// HasEmbedded reports whether value is known for field from the
// embedded OSS catalog, ignoring external contributions. Used by tests
// and by introspection paths that explicitly need the OSS-only set.
func (r *CriteriaRegistry) HasEmbedded(field CriteriaField, value string) bool {
	value = normalizeCriteriaValue(value)
	r.mu.RLock()
	defer r.mu.RUnlock()
	bucket, ok := r.values[field]
	if !ok {
		return false
	}
	origin, present := bucket[value]
	return present && origin == OriginEmbedded
}

// Values returns the registered values for field as a sorted slice.
// In strict mode, only embedded values are returned. The result is a
// copy so callers may mutate it freely.
func (r *CriteriaRegistry) Values(field CriteriaField) []string {
	r.mu.RLock()
	bucket, ok := r.values[field]
	if !ok {
		r.mu.RUnlock()
		return nil
	}
	strict := r.strict
	out := make([]string, 0, len(bucket))
	for v, origin := range bucket {
		if strict && origin != OriginEmbedded {
			continue
		}
		out = append(out, v)
	}
	r.mu.RUnlock()
	sort.Strings(out)
	return out
}

// SetStrict toggles strict-mode validation. In strict mode, registry
// lookups admit only OriginEmbedded values; external contributions are
// hidden as if they had never been registered. The upstream OSS CI gate
// turns this on (via AICR_CRITERIA_STRICT) so the embedded catalog
// cannot accidentally depend on internal-only values.
func (r *CriteriaRegistry) SetStrict(strict bool) {
	r.mu.Lock()
	r.strict = strict
	r.mu.Unlock()
}

// IsStrict reports whether the registry is in strict mode.
func (r *CriteriaRegistry) IsStrict() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.strict
}

// Reset clears all registered values and the strict flag. Intended for
// use in tests that need a clean slate between cases.
func (r *CriteriaRegistry) Reset() {
	r.mu.Lock()
	r.values = make(map[CriteriaField]map[string]CriteriaOrigin)
	r.strict = isStrictModeFromEnv()
	r.mu.Unlock()
}

// criteriaRegistryCacheEntry holds the lazily-built CriteriaRegistry for a
// single DataProvider identity. sync.Once gates concurrent first-load callers
// onto the same registry so two goroutines never construct competing
// instances for the same provider. This mirrors the per-provider isolation
// primitive used by storeCache (metadata_store.go) and registryCache
// (components.go).
type criteriaRegistryCacheEntry struct {
	once     sync.Once
	registry *CriteriaRegistry
}

// criteriaRegistryCache holds criteriaRegistryCacheEntry pointers keyed by
// DataProvider identity. Two callers bound to different DataProvider values
// populate distinct entries; a single provider value yields a single shared
// registry regardless of caller goroutine count. EvictCachedCriteriaRegistry
// drops a single entry so a caller can force a rebuild after rotating a
// provider's backing data without disturbing other providers.
var criteriaRegistryCache sync.Map // map[DataProvider]*criteriaRegistryCacheEntry

// GetCriteriaRegistryFor returns the criteria registry for the supplied
// DataProvider, constructing it lazily on first access. Concurrent callers
// with the same provider observe the same singleton; distinct providers
// populate distinct cache entries and never share state. Each per-provider
// registry honors AICR_CRITERIA_STRICT at construction.
//
// A nil provider returns a fresh ephemeral registry (not inserted into the
// cache) — only the hardcoded OSS fast-path values will validate. Callers
// that need overlay values to validate must pass a non-nil provider.
func GetCriteriaRegistryFor(dp DataProvider) *CriteriaRegistry {
	if dp == nil {
		return NewCriteriaRegistry()
	}
	e, _ := criteriaRegistryCache.LoadOrStore(dp, &criteriaRegistryCacheEntry{})
	entry := e.(*criteriaRegistryCacheEntry)
	entry.once.Do(func() {
		entry.registry = newCriteriaRegistry()
	})
	return entry.registry
}

// EvictCachedCriteriaRegistry drops the cached criteria registry for the
// supplied provider so the next GetCriteriaRegistryFor call rebuilds an empty
// registry. Passing a nil provider is a no-op (callers handle that case
// explicitly to avoid silently evicting the package-global registry).
func EvictCachedCriteriaRegistry(dp DataProvider) {
	if dp == nil {
		return
	}
	criteriaRegistryCache.Delete(dp)
}

// CachedCriteriaRegistryContainsForTesting reports whether the criteria
// registry cache has an entry for the supplied DataProvider.
//
// Test-only by convention (the _ForTesting suffix); never call from
// production code.
func CachedCriteriaRegistryContainsForTesting(dp DataProvider) bool {
	_, ok := criteriaRegistryCache.Load(dp)
	return ok
}

// ResetCriteriaRegistryForTesting drops every cached criteria registry so the
// next GetCriteriaRegistryFor call rebuilds from scratch. This must only be
// called from tests.
func ResetCriteriaRegistryForTesting() {
	criteriaRegistryCache.Range(func(k, _ any) bool {
		criteriaRegistryCache.Delete(k)
		return true
	})
}

// normalizeCriteriaValue lower-cases and trims a criteria value to
// match the normalization performed inside the ParseCriteria*Type
// switches, so registered values compare against parsed input
// case-insensitively.
func normalizeCriteriaValue(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// seedCriteriaRegistry registers each non-empty value from c into the
// criteria registry bound to dp under the given source. Used by the overlay
// loader so values declared in YAML overlay catalogs (embedded or
// `--data`) become valid CLI / API inputs without a binary rebuild.
//
// Routing through dp keeps the criteria registry per-provider: loading a
// provider's metadata store seeds THAT provider's registry, exactly like the
// metadata store itself is per-provider. A nil dp falls back to the
// package-global registry via GetCriteriaRegistryFor, preserving the legacy
// global path.
//
// source is the string returned by the DataProvider's Source(path); we
// translate it to a CriteriaOrigin here so callers don't have to know
// about the registry's internal enum. Only the literal "embedded" sentinel
// maps to OriginEmbedded — anything else (external, merged, or an unknown
// future value) maps to OriginExternal so strict mode fails closed on
// non-OSS contributions even if a new DataProvider source category is
// introduced later.
func seedCriteriaRegistry(c *Criteria, source string, dp DataProvider) {
	if c == nil {
		return
	}
	origin := OriginExternal
	if source == sourceEmbedded {
		origin = OriginEmbedded
	}
	reg := GetCriteriaRegistryFor(dp)
	reg.Register(FieldService, string(c.Service), origin)
	reg.Register(FieldAccelerator, string(c.Accelerator), origin)
	reg.Register(FieldIntent, string(c.Intent), origin)
	reg.Register(FieldOS, string(c.OS), origin)
	reg.Register(FieldPlatform, string(c.Platform), origin)
}

// isStrictModeFromEnv reads AICR_CRITERIA_STRICT and returns true when
// it is set to a truthy value. Recognized truthy spellings: "1", "true",
// "yes", "on" (case-insensitive).
func isStrictModeFromEnv() bool {
	switch normalizeCriteriaValue(os.Getenv(strictModeEnvVar)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
