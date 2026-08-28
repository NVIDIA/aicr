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
	"fmt"
	"maps"
	"slices"
	"sort"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

type effectiveProfileDeclaration struct {
	Source      string
	Declaration *ProfileDeclaration
}

// resolveProfileDeclaration finds the one declaration reachable from every
// criteria-matched candidate chain. It runs before snapshot filtering and
// deduplicates the same declaring source reached through multiple chains.
func (s *MetadataStore) resolveProfileDeclaration(overlays []*RecipeMetadata) (*effectiveProfileDeclaration, error) {
	found := make(map[string]*ProfileDeclaration)
	add := func(source string, metadata *RecipeMetadata) error {
		if err := ValidateRecipeMetadataProfile(metadata); err != nil {
			return errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid profile contract in overlay %q", source))
		}
		if metadata != nil && metadata.Spec.Profile != nil {
			found[source] = metadata.Spec.Profile
		}
		return nil
	}

	if err := add(baseRecipeName, s.Base); err != nil {
		return nil, err
	}
	for _, overlay := range overlays {
		chain, err := s.resolveInheritanceChain(overlay.Metadata.Name)
		if err != nil {
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest,
				fmt.Sprintf("failed to resolve profile declaration for overlay %q", overlay.Metadata.Name))
		}
		for _, metadata := range chain {
			source := metadata.Metadata.Name
			if metadata == s.Base || source == "" {
				source = baseRecipeName
			}
			if err := add(source, metadata); err != nil {
				return nil, err
			}
		}
	}

	if len(found) == 0 {
		return nil, nil //nolint:nilnil // no declaration is a valid unprofiled composition
	}
	if len(found) > 1 {
		sources := make([]string, 0, len(found))
		for source := range found {
			sources = append(sources, source)
		}
		sort.Strings(sources)
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("resolved composition has multiple profile declarations from %v", sources))
	}
	for source, declaration := range found {
		return &effectiveProfileDeclaration{Source: source, Declaration: declaration}, nil
	}
	return nil, nil //nolint:nilnil // unreachable defensive fallback for an empty map
}

func ensureProfileDeclarationSurvived(
	before, after *effectiveProfileDeclaration,
	excluded []ExcludedOverlay,
	warnings []ConstraintWarning,
) error {

	if before == nil {
		return nil
	}
	if after != nil && after.Source == before.Source {
		return nil
	}
	return errors.NewWithContext(
		errors.ErrCodeInvalidRequest,
		fmt.Sprintf("profile declaration from overlay %q was removed by snapshot constraint filtering", before.Source),
		map[string]any{
			"excludedOverlays":   excluded,
			"constraintWarnings": warnings,
		},
	)
}

func (s *MetadataStore) resolveAppliedProfileDeclaration(
	appliedOverlays []string,
) (*effectiveProfileDeclaration, error) {

	survivingOverlays := make([]*RecipeMetadata, 0, len(appliedOverlays))
	for _, name := range appliedOverlays {
		if name == baseRecipeName {
			continue
		}
		if overlay, exists := s.Overlays[name]; exists {
			survivingOverlays = append(survivingOverlays, overlay)
		}
	}
	return s.resolveProfileDeclaration(survivingOverlays)
}

func applyEffectiveProfile(
	mergedSpec *RecipeMetadataSpec,
	effective *effectiveProfileDeclaration,
	rawSelection string,
	evaluator ConstraintEvaluatorFunc,
) (*SelectedProfile, error) {

	selection, err := ParseProfileSelection(rawSelection)
	if err != nil {
		return nil, err
	}
	if effective == nil {
		if selection != nil {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile %q was selected, but the resolved composition declares no profile", selection.Name))
		}
		return nil, nil //nolint:nilnil // no declaration produces an unprofiled result
	}

	ownedPaths, err := ValidateProfileDeclaration(effective.Declaration)
	if err != nil {
		return nil, err
	}
	valueName := effective.Declaration.Default
	if selection != nil {
		if selection.Name != effective.Declaration.Name {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("resolved composition declares profile %q, not %q",
					effective.Declaration.Name, selection.Name))
		}
		valueName = selection.Value
	}
	value, ok := effective.Declaration.Values[valueName]
	if !ok {
		names := make([]string, 0, len(effective.Declaration.Values))
		for name := range effective.Declaration.Values {
			names = append(names, name)
		}
		sort.Strings(names)
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("profile %q has no value %q; valid values: %v",
				effective.Declaration.Name, valueName, names))
	}

	componentIndex := make(map[string]int, len(mergedSpec.ComponentRefs))
	for i := range mergedSpec.ComponentRefs {
		componentIndex[mergedSpec.ComponentRefs[i].Name] = i
	}
	components := slices.Sorted(maps.Keys(ownedPaths))
	for _, component := range components {
		index, exists := componentIndex[component]
		if !exists || !mergedSpec.ComponentRefs[index].IsEnabled() {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile %q references component %q, which is not enabled in the surviving composition",
					effective.Declaration.Name, component))
		}
	}

	constraintNames := make(map[string]struct{}, len(mergedSpec.Constraints))
	for _, constraint := range mergedSpec.Constraints {
		constraintNames[constraint.Name] = struct{}{}
	}
	for _, constraint := range value.Constraints {
		if _, collision := constraintNames[constraint.Name]; collision {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("profile %q value %q constraint %q collides with the composed recipe",
					effective.Declaration.Name, valueName, constraint.Name))
		}
		constraintNames[constraint.Name] = struct{}{}
		if evaluator != nil {
			result := evaluator(constraint)
			switch {
			case result.Error != nil && isNotFoundEvalError(result.Error):
				return nil, errors.NewWithContext(
					errors.ErrCodeInvalidRequest,
					fmt.Sprintf("profile %s=%s cannot be validated because reading %q is unavailable",
						effective.Declaration.Name, valueName, constraint.Name),
					map[string]any{
						constraintContextKey: constraint.Name,
						"expected":           constraint.Value,
						"actual":             result.Actual,
						"cause":              result.Error.Error(),
					},
				)
			case result.Error != nil:
				return nil, errors.PropagateOrWrap(result.Error, errors.ErrCodeInternal,
					fmt.Sprintf("failed to evaluate profile constraint %q", constraint.Name))
			case !result.Passed:
				return nil, errors.NewWithContext(
					errors.ErrCodeInvalidRequest,
					fmt.Sprintf("profile %s=%s constraint %q failed",
						effective.Declaration.Name, valueName, constraint.Name),
					map[string]any{
						constraintContextKey: constraint.Name,
						"expected":           constraint.Value,
						"actual":             result.Actual,
					},
				)
			}
		}
		mergedSpec.Constraints = append(mergedSpec.Constraints, constraint)
	}
	sort.Slice(mergedSpec.Constraints, func(i, j int) bool {
		return mergedSpec.Constraints[i].Name < mergedSpec.Constraints[j].Name
	})

	// Readiness constraints are deliberately NOT evaluated here: they name
	// state that is only meaningful post-deployment (ADR-015) — a
	// deployment-outcome check absent from a fresh pre-deployment snapshot,
	// or externally-grounded qualification state asserted at readiness —
	// so generation must not gate on them. They route into
	// spec.validation.readiness.constraints, where the aicr validate
	// pre-flight (checkReadiness) evaluates them with the same fail-closed
	// exit as every other readiness gate. Collisions are checked against the
	// readiness phase only: the same measurement path may legitimately carry
	// a generation-time pre-condition AND a readiness-time post-deployment
	// state (the DD5 pattern), and the phases report independently.
	if len(value.ReadinessConstraints) > 0 {
		// Defensive clone: both callers already hand in a deep-cloned
		// Validation (initBaseMergedSpec, RecipeMetadataSpec.Merge), so
		// this guards future call sites rather than a live aliasing risk.
		mergedSpec.Validation = cloneValidationConfig(mergedSpec.Validation)
		if mergedSpec.Validation == nil {
			mergedSpec.Validation = &ValidationConfig{}
		}
		if mergedSpec.Validation.Readiness == nil {
			mergedSpec.Validation.Readiness = &ValidationPhase{}
		}
		readinessNames := make(map[string]struct{},
			len(mergedSpec.Validation.Readiness.Constraints)+len(value.ReadinessConstraints))
		for _, existing := range mergedSpec.Validation.Readiness.Constraints {
			readinessNames[existing.Name] = struct{}{}
		}
		for _, constraint := range value.ReadinessConstraints {
			if _, collision := readinessNames[constraint.Name]; collision {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("profile %q value %q readiness constraint %q collides with the composed recipe's readiness constraints",
						effective.Declaration.Name, valueName, constraint.Name))
			}
			readinessNames[constraint.Name] = struct{}{}
			mergedSpec.Validation.Readiness.Constraints =
				append(mergedSpec.Validation.Readiness.Constraints, constraint)
		}
	}

	for _, profileRef := range value.ComponentRefs {
		index := componentIndex[profileRef.Name]
		if mergedSpec.ComponentRefs[index].Overrides == nil {
			mergedSpec.ComponentRefs[index].Overrides = make(map[string]any)
		} else {
			mergedSpec.ComponentRefs[index].Overrides =
				serializer.DeepCopyAnyMap(mergedSpec.ComponentRefs[index].Overrides)
		}
		deepMergeMap(mergedSpec.ComponentRefs[index].Overrides, profileRef.Overrides)
	}

	return &SelectedProfile{
		Name:       effective.Declaration.Name,
		Value:      valueName,
		Advertiser: value.Advertiser,
		OwnedPaths: cloneOwnedPaths(ownedPaths),
	}, nil
}
