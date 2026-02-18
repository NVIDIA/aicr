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

package recipe

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	eidoserrors "github.com/NVIDIA/eidos/pkg/errors"
	"gopkg.in/yaml.v3"
)

var (
	metadataStoreOnce   sync.Once
	cachedMetadataStore *MetadataStore
	cachedMetadataErr   error
)

// MetadataStore holds the base recipe and all overlays.
type MetadataStore struct {
	// Base is the base recipe metadata.
	Base *RecipeMetadata

	// Overlays is a list of overlay recipes indexed by name.
	Overlays map[string]*RecipeMetadata

	// ValuesFiles contains embedded values file contents indexed by filename.
	ValuesFiles map[string][]byte
}

// loadMetadataStore loads and caches the metadata store from the data provider.
func loadMetadataStore(_ context.Context) (*MetadataStore, error) {
	metadataStoreOnce.Do(func() {
		// Record cache miss on first load
		recipeCacheMisses.Inc()

		store := &MetadataStore{
			Overlays:    make(map[string]*RecipeMetadata),
			ValuesFiles: make(map[string][]byte),
		}

		provider := GetDataProvider()

		// Load all YAML files from data directory
		err := provider.WalkDir("", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return eidoserrors.Wrap(eidoserrors.ErrCodeInternal, "failed to walk data directory", err)
			}
			if d.IsDir() {
				return nil
			}

			filename := filepath.Base(path)

			// Handle component files (files in the components/ directory)
			if strings.Contains(path, "components/") {
				content, readErr := provider.ReadFile(path)
				if readErr != nil {
					return eidoserrors.Wrap(eidoserrors.ErrCodeInternal, fmt.Sprintf("failed to read component file %s", path), readErr)
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
			content, readErr := provider.ReadFile(path)
			if readErr != nil {
				return eidoserrors.Wrap(eidoserrors.ErrCodeInternal, fmt.Sprintf("failed to read %s", path), readErr)
			}

			var metadata RecipeMetadata
			if parseErr := yaml.Unmarshal(content, &metadata); parseErr != nil {
				return eidoserrors.Wrap(eidoserrors.ErrCodeInvalidRequest, fmt.Sprintf("failed to parse %s", path), parseErr)
			}

			// Validate kind field
			if metadata.Kind != "" && metadata.Kind != RecipeMetadataKind {
				return eidoserrors.New(eidoserrors.ErrCodeInvalidRequest,
					fmt.Sprintf("invalid kind in %s: got %q, expected %q", path, metadata.Kind, RecipeMetadataKind))
			}

			// Categorize as base or overlay
			// base.yaml is now in overlays/ directory but still identified by filename
			if filename == "base.yaml" && strings.Contains(path, "overlays/") {
				store.Base = &metadata
			} else {
				store.Overlays[metadata.Metadata.Name] = &metadata
			}

			return nil
		})

		if err != nil {
			cachedMetadataErr = err
			return
		}

		if store.Base == nil {
			cachedMetadataErr = eidoserrors.New(eidoserrors.ErrCodeInternal, "base.yaml not found")
			return
		}

		// Validate base recipe dependencies
		if err := store.Base.Spec.ValidateDependencies(); err != nil {
			cachedMetadataErr = eidoserrors.Wrap(eidoserrors.ErrCodeInvalidRequest, "base recipe validation failed", err)
			return
		}

		cachedMetadataStore = store
	})

	// Record cache hit if store was already loaded (not on first load)
	if cachedMetadataStore != nil && cachedMetadataErr == nil {
		recipeCacheHits.Inc()
	}

	if cachedMetadataErr != nil {
		return nil, cachedMetadataErr
	}
	if cachedMetadataStore == nil {
		return nil, eidoserrors.New(eidoserrors.ErrCodeInternal, "metadata store not initialized")
	}
	return cachedMetadataStore, nil
}

// GetValuesFile returns the content of a values file by filename.
func (s *MetadataStore) GetValuesFile(filename string) ([]byte, error) {
	content, exists := s.ValuesFiles[filename]
	if !exists {
		return nil, eidoserrors.New(eidoserrors.ErrCodeNotFound, fmt.Sprintf("values file not found: %s", filename))
	}
	return content, nil
}

// GetRecipeByName returns a recipe metadata by name.
// Returns the base recipe if name is "base", otherwise looks up in overlays.
func (s *MetadataStore) GetRecipeByName(name string) (*RecipeMetadata, bool) {
	if name == "" || name == "base" {
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
	for currentName != "" && currentName != "base" {
		// Check for cycle
		if visited[currentName] {
			return nil, eidoserrors.New(eidoserrors.ErrCodeInvalidRequest,
				fmt.Sprintf("circular inheritance detected: recipe %q references itself in inheritance chain", currentName))
		}
		visited[currentName] = true

		// Get the recipe
		recipe, exists := s.GetRecipeByName(currentName)
		if !exists {
			return nil, eidoserrors.New(eidoserrors.ErrCodeNotFound,
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

// FindMatchingOverlays finds all overlays that match the given criteria.
// Returns overlays sorted by specificity (least specific first).
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

	// Sort by specificity (least specific first, so more specific overlays are applied later)
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].Spec.Criteria.Specificity() < matches[j].Spec.Criteria.Specificity()
	})

	return matches
}

// initBaseMergedSpec creates a copy of the base spec for overlay merging.
func (s *MetadataStore) initBaseMergedSpec() (RecipeMetadataSpec, []string) {
	mergedSpec := RecipeMetadataSpec{
		Constraints:   make([]Constraint, len(s.Base.Spec.Constraints)),
		ComponentRefs: make([]ComponentRef, len(s.Base.Spec.ComponentRefs)),
	}
	copy(mergedSpec.Constraints, s.Base.Spec.Constraints)
	copy(mergedSpec.ComponentRefs, s.Base.Spec.ComponentRefs)
	return mergedSpec, []string{"base"}
}

// mergeOverlayChains resolves inheritance chains and merges overlays into the spec.
func (s *MetadataStore) mergeOverlayChains(overlays []*RecipeMetadata, mergedSpec *RecipeMetadataSpec, appliedOverlays []string) ([]string, error) {
	processedChains := make(map[string]bool)

	for _, overlay := range overlays {
		chain, err := s.resolveInheritanceChain(overlay.Metadata.Name)
		if err != nil {
			return appliedOverlays, eidoserrors.WrapWithContext(
				eidoserrors.ErrCodeInvalidRequest,
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
func finalizeRecipeResult(criteria *Criteria, mergedSpec *RecipeMetadataSpec, appliedOverlays []string) (*RecipeResult, error) {
	if err := mergedSpec.ValidateDependencies(); err != nil {
		return nil, eidoserrors.Wrap(eidoserrors.ErrCodeInvalidRequest, "merged recipe validation failed", err)
	}

	deployOrder, err := mergedSpec.TopologicalSort()
	if err != nil {
		return nil, eidoserrors.Wrap(eidoserrors.ErrCodeInternal, "failed to compute deployment order", err)
	}

	applyRegistryDefaults(mergedSpec.ComponentRefs)

	result := &RecipeResult{
		Kind:            RecipeResultKind,
		APIVersion:      RecipeAPIVersion,
		Criteria:        criteria,
		Constraints:     mergedSpec.Constraints,
		ComponentRefs:   mergedSpec.ComponentRefs,
		DeploymentOrder: deployOrder,
		Validation:      mergedSpec.Validation,
	}
	result.Metadata.AppliedOverlays = appliedOverlays

	return result, nil
}

// BuildRecipeResult builds a RecipeResult by merging base with matching overlays.
// Each matching overlay is resolved through its inheritance chain before merging.
// This enables multi-level inheritance: base → intermediate → overlay.
func (s *MetadataStore) BuildRecipeResult(ctx context.Context, criteria *Criteria) (*RecipeResult, error) {
	select {
	case <-ctx.Done():
		return nil, eidoserrors.WrapWithContext(
			eidoserrors.ErrCodeTimeout,
			"build recipe result context cancelled during initialization",
			ctx.Err(),
			map[string]any{"stage": "initialization"},
		)
	default:
	}

	overlays := s.FindMatchingOverlays(criteria)
	mergedSpec, appliedOverlays := s.initBaseMergedSpec()

	appliedOverlays, err := s.mergeOverlayChains(overlays, &mergedSpec, appliedOverlays)
	if err != nil {
		return nil, err
	}

	if len(appliedOverlays) <= 1 {
		slog.Warn("no environment-specific overlays matched, using base configuration only",
			"criteria", criteria.String(),
			"hint", "recipe may not be optimized for your environment")
	}

	return finalizeRecipeResult(criteria, &mergedSpec, appliedOverlays)
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
	if evaluator == nil {
		return s.BuildRecipeResult(ctx, criteria)
	}

	select {
	case <-ctx.Done():
		return nil, eidoserrors.WrapWithContext(
			eidoserrors.ErrCodeTimeout,
			"build recipe result context cancelled during initialization",
			ctx.Err(),
			map[string]any{"stage": "initialization"},
		)
	default:
	}

	// Find matching overlays and filter by constraint evaluation
	overlays := s.FindMatchingOverlays(criteria)

	var filteredOverlays []*RecipeMetadata
	var excludedOverlays []string
	var constraintWarnings []ConstraintWarning

	for _, overlay := range overlays {
		slog.Debug("evaluating overlay constraints",
			"overlay", overlay.Metadata.Name,
			"constraint_count", len(overlay.Spec.Constraints))

		passed, warnings := s.evaluateOverlayConstraints(overlay, evaluator)
		if passed {
			filteredOverlays = append(filteredOverlays, overlay)
			slog.Debug("overlay passed all constraints",
				"overlay", overlay.Metadata.Name)
		} else {
			excludedOverlays = append(excludedOverlays, overlay.Metadata.Name)
			constraintWarnings = append(constraintWarnings, warnings...)
			slog.Info("excluding overlay due to constraint failures",
				"overlay", overlay.Metadata.Name,
				"failed_constraints", len(warnings))
		}
	}

	mergedSpec, appliedOverlays := s.initBaseMergedSpec()

	appliedOverlays, err := s.mergeOverlayChains(filteredOverlays, &mergedSpec, appliedOverlays)
	if err != nil {
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

	result, err := finalizeRecipeResult(criteria, &mergedSpec, appliedOverlays)
	if err != nil {
		return nil, err
	}
	result.Metadata.ExcludedOverlays = excludedOverlays
	result.Metadata.ConstraintWarnings = constraintWarnings

	return result, nil
}

// evaluateOverlayConstraints evaluates all constraints in an overlay.
// Returns true if all constraints pass, false otherwise.
// Returns warnings for any constraints that failed or had errors.
func (s *MetadataStore) evaluateOverlayConstraints(overlay *RecipeMetadata, evaluator ConstraintEvaluatorFunc) (bool, []ConstraintWarning) {
	if len(overlay.Spec.Constraints) == 0 {
		// No constraints means the overlay passes
		return true, nil
	}

	var warnings []ConstraintWarning
	allPassed := true

	for _, constraint := range overlay.Spec.Constraints {
		result := evaluator(constraint)

		switch {
		case result.Error != nil:
			// Treat evaluation errors as failures with a warning
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
				"constraint", constraint.Name,
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
				"constraint", constraint.Name,
				"expected", constraint.Value,
				"actual", result.Actual)
		default:
			slog.Debug("constraint passed",
				"overlay", overlay.Metadata.Name,
				"constraint", constraint.Name,
				"expected", constraint.Value,
				"actual", result.Actual)
		}
	}

	return allPassed, warnings
}

// applyRegistryDefaults fills in ComponentRef fields from ComponentConfig defaults.
// This allows registry.yaml to specify default values that are applied to components
// that don't explicitly set them in recipes.
func applyRegistryDefaults(refs []ComponentRef) {
	registry, err := GetComponentRegistry()
	if err != nil {
		slog.Warn("failed to get component registry for defaults", "error", err)
		return
	}

	for i := range refs {
		config := registry.Get(refs[i].Name)
		if config != nil {
			refs[i].ApplyRegistryDefaults(config)
		}
	}
}
