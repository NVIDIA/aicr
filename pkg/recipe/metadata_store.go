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
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"gopkg.in/yaml.v3"
)

const baseRecipeName = "base"

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

	// Mixins is a map of composable mixin fragments indexed by name.
	Mixins map[string]*RecipeMixin

	// ValuesFiles contains embedded values file contents indexed by filename.
	ValuesFiles map[string][]byte
}

// loadMetadataStore loads and caches the metadata store from the data provider.
func loadMetadataStore(ctx context.Context) (*MetadataStore, error) {
	metadataStoreOnce.Do(func() {
		// Record cache miss on first load
		recipeCacheMisses.Inc()

		store := &MetadataStore{
			Overlays:    make(map[string]*RecipeMetadata),
			Mixins:      make(map[string]*RecipeMixin),
			ValuesFiles: make(map[string][]byte),
		}

		provider := GetDataProvider()

		// Load all YAML files from data directory
		err := provider.WalkDir("", func(path string, d fs.DirEntry, err error) error {
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
				content, readErr := provider.ReadFile(path)
				if readErr != nil {
					return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, fmt.Sprintf("failed to read mixin %s", path), readErr)
				}
				var mixin RecipeMixin
				decoder := yaml.NewDecoder(bytes.NewReader(content))
				decoder.KnownFields(true)
				if parseErr := decoder.Decode(&mixin); parseErr != nil {
					return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("failed to parse mixin %s (unknown fields are not allowed)", path), parseErr)
				}
				if mixin.Kind != RecipeMixinKind {
					return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
						fmt.Sprintf("mixin file %s has wrong kind %q, expected %q", path, mixin.Kind, RecipeMixinKind))
				}
				if _, exists := store.Mixins[mixin.Metadata.Name]; exists {
					return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
						fmt.Sprintf("duplicate mixin name %q in %s", mixin.Metadata.Name, path))
				}
				store.Mixins[mixin.Metadata.Name] = &mixin
				slog.Debug("loaded mixin", "name", mixin.Metadata.Name, "path", path)
				return nil
			}

			// Handle component files (files in the components/ directory)
			if strings.Contains(path, "components/") {
				content, readErr := provider.ReadFile(path)
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
			content, readErr := provider.ReadFile(path)
			if readErr != nil {
				return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, fmt.Sprintf("failed to read %s", path), readErr)
			}

			var metadata RecipeMetadata
			if parseErr := yaml.Unmarshal(content, &metadata); parseErr != nil {
				return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("failed to parse %s", path), parseErr)
			}

			// Skip files with a different kind (e.g., ValidatorCatalog).
			if metadata.Kind != "" && metadata.Kind != RecipeMetadataKind {
				slog.Debug("skipping non-recipe YAML", "path", path, "kind", metadata.Kind)
				return nil
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
			cachedMetadataErr = aicrerrors.New(aicrerrors.ErrCodeInternal, "base.yaml not found")
			return
		}

		// Validate base recipe dependencies
		if err := store.Base.Spec.ValidateDependencies(); err != nil {
			cachedMetadataErr = aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "base recipe validation failed", err)
			return
		}

		cachedMetadataStore = store
	})

	if cachedMetadataErr != nil {
		return nil, cachedMetadataErr
	}
	if cachedMetadataStore == nil {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInternal, "metadata store not initialized")
	}
	return cachedMetadataStore, nil
}

// ResetMetadataStoreForTesting clears the cached metadata store so that
// tests can reload with different data. Must only be called from tests.
func ResetMetadataStoreForTesting() {
	metadataStoreOnce = sync.Once{}
	cachedMetadataStore = nil
	cachedMetadataErr = nil
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
// and componentRefs. Detects conflicts: duplicate constraint names or component
// names between a mixin and the already-merged spec are rejected.
// The Mixins field is cleared from the result afterward.
// Returns the set of mixin-contributed constraint names for post-compose evaluation.
func (s *MetadataStore) mergeMixins(mergedSpec *RecipeMetadataSpec) (map[string]bool, error) {
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

		// Detect conflicts: mixin constraint/component names vs inheritance chain
		// and previously applied mixins (existingConstraints/existingComponents
		// are updated after each mixin merge)
		for _, c := range mixin.Spec.Constraints {
			if existingConstraints[c.Name] {
				return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					fmt.Sprintf("mixin %q constraint %q conflicts with inheritance chain or another mixin", mixinName, c.Name))
			}
		}
		for _, c := range mixin.Spec.ComponentRefs {
			if existingComponents[c.Name] {
				return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					fmt.Sprintf("mixin %q component %q conflicts with inheritance chain or another mixin", mixinName, c.Name))
			}
		}

		// Merge mixin content
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
		result := evaluator(constraint)
		if !result.Passed {
			affectedCandidates := constraintCandidates[constraint.Name]
			if len(affectedCandidates) == 0 {
				return mixinEvalResult{}, aicrerrors.NewWithContext(
					aicrerrors.ErrCodeInternal,
					"failed to map mixin constraint to candidate chain",
					map[string]any{
						"constraint":      constraint.Name,
						"candidate_count": len(candidateOverlays),
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
	if _, err := s.mergeMixins(&rebuiltSpec); err != nil {
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
func (s *MetadataStore) initBaseMergedSpec() (RecipeMetadataSpec, []string) {
	mergedSpec := RecipeMetadataSpec{
		Constraints:   make([]Constraint, len(s.Base.Spec.Constraints)),
		ComponentRefs: make([]ComponentRef, len(s.Base.Spec.ComponentRefs)),
		Validation:    s.Base.Spec.Validation,
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
func finalizeRecipeResult(criteria *Criteria, mergedSpec *RecipeMetadataSpec, appliedOverlays []string) (*RecipeResult, error) {
	if err := mergedSpec.ValidateDependencies(); err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "merged recipe validation failed", err)
	}

	deployOrder, err := mergedSpec.TopologicalSort()
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to compute deployment order", err)
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
		return nil, aicrerrors.WrapWithContext(
			aicrerrors.ErrCodeTimeout,
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

	// Merge mixin fragments referenced by overlays in the chain
	if _, err := s.mergeMixins(&mergedSpec); err != nil {
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
		return nil, aicrerrors.WrapWithContext(
			aicrerrors.ErrCodeTimeout,
			"build recipe result context cancelled during initialization",
			ctx.Err(),
			map[string]any{"stage": "initialization"},
		)
	default:
	}

	// Find matching overlays and filter by constraint evaluation
	overlays := s.FindMatchingOverlays(criteria)

	var filteredOverlays []*RecipeMetadata
	var excludedOverlays []ExcludedOverlay
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

	appliedOverlays, err := s.mergeOverlayChains(filteredOverlays, &mergedSpec, appliedOverlays)
	if err != nil {
		return nil, err
	}

	// Merge mixin fragments referenced by overlays in the chain.
	mixinConstraintNames, err := s.mergeMixins(&mergedSpec)
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
	mixinResult, err := s.evaluateMixinConstraints(&mergedSpec, evaluator, mixinConstraintNames, candidateOverlays)
	if err != nil {
		return nil, err
	}
	if mixinResult.Failed {
		excludedOverlays = append(excludedOverlays, mixinResult.ExcludedOverlays...)
		constraintWarnings = append(constraintWarnings, mixinResult.Warnings...)
		mergedSpec = *mixinResult.Spec
		appliedOverlays = mixinResult.AppliedOverlays
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
