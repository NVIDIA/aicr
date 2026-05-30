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

// Package recipe provides recipe building and matching functionality.
package recipe

import (
	"context"
	"embed"
	stderrors "errors"
	"fmt"
	"io/fs"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/recipes"
	"gopkg.in/yaml.v3"
)

// GetEmbeddedFS returns the embedded data filesystem.
// This is used by the CLI to create layered data providers.
func GetEmbeddedFS() embed.FS {
	return recipes.FS
}

// GetManifestContent retrieves a manifest file from the package-global
// DataProvider. Path should be relative to data directory (e.g.,
// "components/network-operator/manifests/nfd-network-rule.yaml").
//
// This entry point is preserved for back-compat with callers that have no
// RecipeResult-bound provider available. Internally derives a
// defaults.FileReadTimeout-bounded context so a hung backing store still
// returns instead of blocking the goroutine. Callers operating against a
// per-tenant Builder should prefer GetManifestContentWithProvider so the
// lookup honors the bound provider; callers that already hold a
// context.Context should use GetManifestContentWithContext.
func GetManifestContent(path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaults.FileReadTimeout)
	defer cancel()
	return GetManifestContentWithContext(ctx, nil, path)
}

// GetManifestContentWithProvider reads a manifest file from the supplied
// DataProvider. A nil provider falls back to the package-level embedded-data
// singleton so callers that thread a possibly-nil RecipeResult.DataProvider()
// through can rely on the embedded fallback without an explicit nil check.
//
// Internally derives a defaults.FileReadTimeout-bounded context. Callers
// that already hold a context.Context should use
// GetManifestContentWithContext to honor their own deadline instead.
//
// Path should be relative to the data root (e.g.,
// "components/network-operator/manifests/nfd-network-rule.yaml").
func GetManifestContentWithProvider(dp DataProvider, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaults.FileReadTimeout)
	defer cancel()
	return GetManifestContentWithContext(ctx, dp, path)
}

// GetManifestContentWithContext reads a manifest file from the supplied
// DataProvider, honoring the caller's context for cancellation/timeout.
// A nil provider falls back to GetDataProvider().
func GetManifestContentWithContext(ctx context.Context, dp DataProvider, path string) ([]byte, error) {
	if dp == nil {
		dp = defaultEmbeddedProvider
	}
	content, err := dp.ReadFile(ctx, path)
	if err != nil {
		if stderrors.Is(err, fs.ErrNotExist) {
			return nil, errors.Wrap(errors.ErrCodeNotFound, fmt.Sprintf("manifest file not found: %q", path), err)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to read manifest file %q", path), err)
	}
	return content, nil
}

// RecipeInput is an interface that both Recipe and RecipeResult implement.
// This allows bundlers to work with either format during the transition period.
type RecipeInput interface {
	// GetComponentRef returns the component reference for a given component name.
	// Returns nil if the component is not found.
	GetComponentRef(name string) *ComponentRef

	// GetValuesForComponent returns the values map for a given component.
	// For Recipe, this extracts values from measurements.
	// For RecipeResult, this loads values from the component's valuesFile.
	GetValuesForComponent(name string) (map[string]any, error)

	// GetVersion returns the recipe version (CLI version that generated the recipe).
	// Returns empty string if version is not available.
	GetVersion() string

	// GetCriteria returns the criteria used to generate this recipe.
	// Returns nil if criteria is not available (e.g., for legacy Recipe format).
	GetCriteria() *Criteria
}

// Ensure Recipe implements RecipeInput
var _ RecipeInput = (*Recipe)(nil)

// GetComponentRef returns nil for Recipe (v1 format doesn't have components).
func (r *Recipe) GetComponentRef(name string) *ComponentRef {
	return nil
}

// GetValuesForComponent extracts values from measurements for Recipe.
// This maintains backward compatibility with the legacy measurements-based format.
func (r *Recipe) GetValuesForComponent(name string) (map[string]any, error) {
	// For legacy Recipe, values are embedded in measurements
	// This is a no-op - bundlers extract their own values from measurements
	return make(map[string]any), nil
}

// GetVersion returns the recipe version from metadata.
func (r *Recipe) GetVersion() string {
	if r.Metadata == nil {
		return ""
	}
	return r.Metadata["recipe-version"]
}

// GetCriteria returns nil for Recipe (v1 format doesn't have criteria).
func (r *Recipe) GetCriteria() *Criteria {
	return nil
}

// Ensure RecipeResult implements RecipeInput
var _ RecipeInput = (*RecipeResult)(nil)

// GetVersion returns the recipe version from metadata.
func (r *RecipeResult) GetVersion() string {
	return r.Metadata.Version
}

// GetCriteria returns the criteria used to generate this recipe result.
func (r *RecipeResult) GetCriteria() *Criteria {
	return r.Criteria
}

// GetComponentRef returns the component reference for a given component name.
func (r *RecipeResult) GetComponentRef(name string) *ComponentRef {
	for i := range r.ComponentRefs {
		if r.ComponentRefs[i].Name == name {
			return &r.ComponentRefs[i]
		}
	}
	return nil
}

// GetValuesForComponent loads values from the component's valuesFile and inline overrides.
//
// Internally derives a defaults.FileReadTimeout-bounded context so a hung
// backing store still returns instead of blocking the goroutine. Callers
// that already hold a context.Context should use
// GetValuesForComponentWithContext to honor their own deadline.
func (r *RecipeResult) GetValuesForComponent(name string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaults.FileReadTimeout)
	defer cancel()
	return r.GetValuesForComponentWithContext(ctx, name)
}

// GetValuesForComponentWithContext loads values from the component's
// valuesFile and inline overrides, honoring the caller's context for
// cancellation/timeout.
//
// Merge order: base values → ValuesFile → Overrides (highest precedence).
// This supports three patterns:
//  1. ValuesFile only: Traditional separate file approach
//  2. Overrides only: Fully self-contained recipe with inline overrides
//  3. ValuesFile + Overrides: Hybrid - reusable base with recipe-specific tweaks
//
// File lookups route through the DataProvider bound to this result (set when
// the result was built by a Builder via WithDataProvider). When no provider
// is bound, lookups fall back to the package-level embedded-data singleton.
func (r *RecipeResult) GetValuesForComponentWithContext(ctx context.Context, name string) (map[string]any, error) {
	ref := r.GetComponentRef(name)
	if ref == nil {
		return nil, errors.New(errors.ErrCodeNotFound, fmt.Sprintf("component %q not found in recipe", name))
	}

	// Start with empty result
	result := make(map[string]any)

	// If no valuesFile and no overrides, return empty map
	if ref.ValuesFile == "" && len(ref.Overrides) == 0 {
		return result, nil
	}

	// Resolve provider once: prefer the result-bound provider (per-tenant
	// isolation), fall back to the embedded-data singleton when the result
	// was constructed without a Builder (e.g. decoded from a recipe file
	// before BindDataProvider has been called).
	provider := r.provider
	if provider == nil {
		provider = defaultEmbeddedProvider
	}

	// Step 1: Load base and/or overlay values from files (if ValuesFile specified)
	if ref.ValuesFile != "" {
		// Determine if this is an overlay values file (not the base values.yaml)
		baseValuesFile := fmt.Sprintf("components/%s/values.yaml", name)
		isOverlay := ref.ValuesFile != baseValuesFile

		if isOverlay {
			// Load base values first
			baseData, err := provider.ReadFile(ctx, baseValuesFile)
			if err != nil {
				// If base file doesn't exist, that's okay - just use overlay
				result = make(map[string]any)
			} else {
				err = yaml.Unmarshal(baseData, &result)
				if err != nil {
					return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to parse base values file %q", baseValuesFile), err)
				}
			}

			// Load overlay values
			overlayData, err := provider.ReadFile(ctx, ref.ValuesFile)
			if err != nil {
				return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to read overlay values file %q", ref.ValuesFile), err)
			}

			var overlayValues map[string]any
			if err := yaml.Unmarshal(overlayData, &overlayValues); err != nil {
				return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to parse overlay values file %q", ref.ValuesFile), err)
			}

			// Merge overlay into base (overlay takes precedence over base)
			mergeValues(result, overlayValues)
		} else {
			// Just load the base values file
			data, err := provider.ReadFile(ctx, ref.ValuesFile)
			if err != nil {
				return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to read values file %q", ref.ValuesFile), err)
			}

			if err := yaml.Unmarshal(data, &result); err != nil {
				return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to parse values file %q", ref.ValuesFile), err)
			}
		}
	}

	// Step 2: Apply inline overrides (highest precedence)
	if len(ref.Overrides) > 0 {
		mergeValues(result, ref.Overrides)
	}

	return result, nil
}

// mergeValues recursively merges src into dst.
// For maps, it recursively merges nested keys.
// For other types, src values override dst values.
// A nil value in src deletes the key from dst (explicit null override).
//
// Non-map values (scalars, slices, nested maps when dst lacks a peer map)
// are deep-copied via deepCopyAny so that mutation of dst never aliases
// back into src. Without this, []any values from a cached overlay would
// be shared with the caller's result, and a downstream --set or dynamic
// injection mutating an index of the slice would corrupt the cache.
func mergeValues(dst, src map[string]any) {
	for key, srcVal := range src {
		if srcVal == nil {
			delete(dst, key)
			continue
		}
		if srcMap, srcOK := srcVal.(map[string]any); srcOK {
			if dstMap, dstOK := dst[key].(map[string]any); dstOK {
				mergeValues(dstMap, srcMap)
				continue
			}
			dst[key] = serializer.DeepCopyAnyMap(srcMap)
			continue
		}
		dst[key] = serializer.DeepCopyAny(srcVal)
	}
}

// hasComponentRefs checks if the input is a RecipeResult with component references.
func hasComponentRefs(input RecipeInput) bool {
	_, ok := input.(*RecipeResult)
	return ok
}
