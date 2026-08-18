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

package aicr

import (
	stderrors "errors"
	"log/slog"
	"slices"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// CriteriaDimension names one criteria dimension subject to the
// criteria-coverage post-condition (issue #1542): the five dimensions an
// applied overlay can honor, and therefore the five a coverage failure can
// report as uncovered.
//
// nodes is deliberately absent. No overlay gates on nodes, so it never
// participates in overlay selection or coverage — see pkg/recipe/coverage.go.
type CriteriaDimension string

// The criteria dimensions subject to the coverage post-condition. The string
// values match pkg/recipe.CoverageDimensionNames exactly; a test asserts that
// equality, because a mismatch would silently unmark a stated dimension and
// let WithSnapshotCriteriaRelaxation clear something the caller stated.
const (
	DimensionService     CriteriaDimension = "service"
	DimensionAccelerator CriteriaDimension = "accelerator"
	DimensionIntent      CriteriaDimension = "intent"
	DimensionOS          CriteriaDimension = "os"
	DimensionPlatform    CriteriaDimension = "platform"
)

// AllCriteriaDimensions returns every dimension subject to the coverage
// post-condition, in canonical order. Useful for declaring that every
// dimension was caller-stated:
//
//	aicr.WithSnapshotCriteriaRelaxation(aicr.AllCriteriaDimensions()...)
//
// which enables the policy but permits nothing to be relaxed — equivalent to
// strict resolution, stated explicitly.
func AllCriteriaDimensions() []CriteriaDimension {
	return []CriteriaDimension{
		DimensionService,
		DimensionAccelerator,
		DimensionIntent,
		DimensionOS,
		DimensionPlatform,
	}
}

// WithSnapshotCriteriaRelaxation enables the relax-and-retry policy that
// `aicr recipe --snapshot` applies on top of a snapshot resolve, and declares
// which criteria dimensions the caller received explicitly.
//
// # What it does
//
// A snapshot resolve is STRICT by default: every stated criteria dimension
// must be honored by an applied overlay, or resolution fails with
// ErrCodeInvalidRequest carrying details.uncovered. That is the right default
// for criteria a user typed, but wrong for criteria DERIVED from a snapshot
// fingerprint — a Kind-style overlay tree can be deliberately OS-agnostic
// while the fingerprint still detects a concrete os on the node. No recipe
// content distinguishes that detected value, so failing on it would reject a
// legitimate query.
//
// With this option, a coverage failure whose uncovered dimensions were ALL
// derived (i.e. absent from stated) clears those dimensions back to unstated
// and retries the resolve exactly once. The dimensions actually cleared are
// reported in RecipeResult.RelaxedDimensions.
//
// # The invariant
//
// A dimension named in stated is NEVER relaxed. If any uncovered dimension
// was stated, the original coverage error propagates unchanged — no retry.
// Relaxing a value the caller explicitly asked for would silently resolve a
// different recipe than the one requested.
//
// # Passing no dimensions is meaningful
//
// Presence of the option enables the policy; the argument only narrows what
// may be relaxed. Calling it with no dimensions is the common case — every
// dimension came from the fingerprint and all are relaxable:
//
//	aicr.WithSnapshotCriteriaRelaxation()                    // relax anything uncovered
//	aicr.WithSnapshotCriteriaRelaxation(aicr.DimensionOS)    // relax anything but os
//
// Omitting the option entirely preserves strict behavior. Note the difference
// from an option keyed on a non-empty argument list: the all-derived case is
// exactly the query the policy exists to serve, so it must not silently fall
// back to strict.
//
// # Scope
//
// Valid only on the snapshot resolve path (ResolveRecipeFromSnapshot and
// friends). On ResolveRecipeFromCriteria there is no fingerprint — every
// dimension is caller-supplied — so the option is rejected with
// ErrCodeInvalidRequest rather than ignored: silently dropping it would leave
// a caller believing they had `--snapshot` semantics when they did not.
//
// An unrecognized dimension name is rejected when the resolve call runs. It is
// not treated as an unknown-but-harmless label, because the failure mode is
// leaving a stated dimension unmarked and relaxing it.
func WithSnapshotCriteriaRelaxation(stated ...CriteriaDimension) RecipeResolveOption {
	return func(cfg *recipeResolveConfig) {
		cfg.relaxDerived = true
		for _, dim := range stated {
			next, ok := cfg.stated.with(dim)
			if !ok {
				cfg.recordOptErr(errors.NewWithContext(errors.ErrCodeInvalidRequest,
					"unknown criteria dimension in WithSnapshotCriteriaRelaxation",
					map[string]any{
						"dimension": string(dim),
						"valid":     recipe.CoverageDimensionNames(),
					}))
				return
			}
			cfg.stated = next
		}
	}
}

// validCriteriaDimension reports whether dim is one of the coverage dimensions.
func validCriteriaDimension(dim CriteriaDimension) bool {
	return slices.Contains(AllCriteriaDimensions(), dim)
}

// statedDimensionSet is a set of coverage dimensions the caller declared it
// stated explicitly.
//
// A map would be the natural shape, but recipeResolveConfig must stay
// COMPARABLE: RecipeResolveOption is func(*recipeResolveConfig), so the
// unexported struct is reachable from an exported signature and the API
// compatibility gate tracks whether it can be compared with ==. A map field
// silently flips that, which api-diff reports as an incompatible change. A
// bitmask costs nothing in readability here, since the dimension set is fixed
// at five and closed by the coverage post-condition.
type statedDimensionSet uint8

// dimensionBit maps a dimension to its bit, or reports it unknown. Position in
// AllCriteriaDimensions is the bit index, so the two cannot disagree.
func dimensionBit(dim CriteriaDimension) (statedDimensionSet, bool) {
	i := slices.Index(AllCriteriaDimensions(), dim)
	if i < 0 {
		return 0, false
	}
	return 1 << uint(i), true
}

// has reports whether dim was declared caller-stated. An unknown dimension is
// never in the set — but options reject those up front, so has never decides
// the invariant on its own.
func (s statedDimensionSet) has(dim CriteriaDimension) bool {
	bit, ok := dimensionBit(dim)
	return ok && s&bit != 0
}

// with returns s plus dim, reporting false when dim is not a coverage
// dimension.
func (s statedDimensionSet) with(dim CriteriaDimension) (statedDimensionSet, bool) {
	bit, ok := dimensionBit(dim)
	if !ok {
		return s, false
	}
	return s | bit, true
}

// relaxDerivedCoverage inspects a recipe-resolution error for the
// criteria-coverage post-condition (issue #1542, pkg/recipe/coverage.go) and,
// when EVERY uncovered dimension was derived (i.e. absent from stated), clears
// those dimensions back to unstated on a COPY of criteria and returns it for a
// single retry, along with the dimensions cleared.
//
// ok is false when err is not a coverage failure, or when any uncovered
// dimension was caller-stated — in which case the caller propagates err
// unchanged. This is the enforcement point for the never-relax-stated
// invariant documented on WithSnapshotCriteriaRelaxation.
func relaxDerivedCoverage(
	err error,
	criteria *recipe.Criteria,
	stated statedDimensionSet,
) (relaxed *recipe.Criteria, cleared []CriteriaDimension, ok bool) {

	uncovered := uncoveredCoverageDimensions(err)
	if len(uncovered) == 0 {
		return nil, nil, false
	}
	for _, dim := range uncovered {
		if stated.has(dim) {
			return nil, nil, false
		}
	}

	next := *criteria
	cleared = make([]CriteriaDimension, 0, len(uncovered))
	for _, dim := range uncovered {
		detected := criteriaDimensionValue(&next, dim)
		clearCriteriaDimension(&next, dim)
		cleared = append(cleared, dim)
		slog.Warn("relaxing snapshot-detected criteria dimension: no recipe content distinguishes it",
			"dimension", string(dim), "detectedValue", detected)
	}
	return &next, cleared, true
}

// uncoveredCoverageDimensions extracts the uncovered dimension names from a
// recipe-resolution error, or nil when err does not carry the
// criteria-coverage post-condition failure (pkg/recipe/coverage.go's
// verifyCriteriaCoverage builds ErrCodeInvalidRequest with a
// Context["uncovered"] []map[string]any, each entry keyed by "dimension").
//
// Every StructuredError in the wrap chain is inspected rather than only the
// outermost one, so an intermediate wrap added between the builder and this
// caller cannot silently disable relaxation. The coverage error is identified
// by its own node's code and context, regardless of outer decoration.
func uncoveredCoverageDimensions(err error) []CriteriaDimension {
	for cur := err; cur != nil; {
		var se *errors.StructuredError
		if !stderrors.As(cur, &se) {
			return nil
		}
		if se.Code == errors.ErrCodeInvalidRequest {
			if names := uncoveredDimensionNames(se.Context["uncovered"]); len(names) > 0 {
				return names
			}
		}
		cur = se.Unwrap()
	}
	return nil
}

// uncoveredDimensionNames pulls the "dimension" names out of a coverage
// error's uncovered payload. It accepts both the in-process shape built by
// verifyCriteriaCoverage ([]map[string]any) and the decoded-JSON shape
// ([]any of map[string]any) so a marshaling boundary cannot silently disable
// relaxation.
//
// A name that is not a known coverage dimension is skipped rather than
// returned: relaxation switches on these names, and clearCriteriaDimension
// would no-op on an unknown one, producing an unchanged retry that fails
// identically. Skipping keeps the "did anything relax?" answer honest.
func uncoveredDimensionNames(raw any) []CriteriaDimension {
	var entries []map[string]any
	switch v := raw.(type) {
	case []map[string]any:
		entries = v
	case []any:
		for _, e := range v {
			if m, ok := e.(map[string]any); ok {
				entries = append(entries, m)
			}
		}
	default:
		return nil
	}
	names := make([]CriteriaDimension, 0, len(entries))
	for _, e := range entries {
		name, ok := e["dimension"].(string)
		if !ok {
			continue
		}
		dim := CriteriaDimension(name)
		if !validCriteriaDimension(dim) {
			slog.Warn("ignoring unrecognized dimension in coverage error",
				"dimension", name, "valid", recipe.CoverageDimensionNames())
			continue
		}
		names = append(names, dim)
	}
	if len(names) == 0 {
		return nil
	}
	return names
}

// criteriaDimensionValue reads one of the 5 coverage dimensions by name.
func criteriaDimensionValue(c *recipe.Criteria, dim CriteriaDimension) string {
	switch dim {
	case DimensionService:
		return string(c.Service)
	case DimensionAccelerator:
		return string(c.Accelerator)
	case DimensionIntent:
		return string(c.Intent)
	case DimensionOS:
		return string(c.OS)
	case DimensionPlatform:
		return string(c.Platform)
	default:
		return ""
	}
}

// clearCriteriaDimension resets one of the 5 coverage dimensions to unstated
// ("any"), by name.
func clearCriteriaDimension(c *recipe.Criteria, dim CriteriaDimension) {
	switch dim {
	case DimensionService:
		c.Service = recipe.CriteriaServiceAny
	case DimensionAccelerator:
		c.Accelerator = recipe.CriteriaAcceleratorAny
	case DimensionIntent:
		c.Intent = recipe.CriteriaIntentAny
	case DimensionOS:
		c.OS = recipe.CriteriaOSAny
	case DimensionPlatform:
		c.Platform = recipe.CriteriaPlatformAny
	}
}
