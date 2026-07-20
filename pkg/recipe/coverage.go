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
	stderrors "errors"
	"fmt"
	"sort"
	"strings"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// coverageDimension names one criteria dimension subject to the coverage
// post-condition and knows how to read its value from a Criteria.
//
// nodes is deliberately absent: no overlay gates on nodes, so covering it
// would reject every --nodes query. It remains a matching dimension but
// carries no coverage guarantee (issue #1542, design 4.3).
type coverageDimension struct {
	name  string
	value func(*Criteria) string
}

// coverageDimensions is ordered; all coverage reporting uses this order.
var coverageDimensions = []coverageDimension{
	{"service", func(c *Criteria) string { return string(c.Service) }},
	{"accelerator", func(c *Criteria) string { return string(c.Accelerator) }},
	{"intent", func(c *Criteria) string { return string(c.Intent) }},
	{"os", func(c *Criteria) string { return string(c.OS) }},
	{"platform", func(c *Criteria) string { return string(c.Platform) }},
}

// isSpecifiedCriteriaValue reports whether a criteria field value is
// explicitly stated ("" and "any" both mean unstated, consistent with
// MatchesCriteriaField).
func isSpecifiedCriteriaValue(v string) bool {
	return v != "" && v != CriteriaAnyValue
}

// uncoveredDimensions returns the names of query-stated dimensions that no
// applied overlay carries with the exact stated value, in coverageDimensions
// order. appliedOverlays holds the names of every merged recipe (base and
// full inheritance chains), matching RecipeResult metadata semantics.
func (s *MetadataStore) uncoveredDimensions(criteria *Criteria, appliedOverlays []string) []string {
	uncovered := []string{}
	for _, dim := range coverageDimensions {
		want := dim.value(criteria)
		if !isSpecifiedCriteriaValue(want) {
			continue
		}
		covered := false
		for _, name := range appliedOverlays {
			meta, ok := s.GetRecipeByName(name)
			if !ok || meta.Spec.Criteria == nil {
				continue
			}
			if dim.value(meta.Spec.Criteria) == want {
				covered = true
				break
			}
		}
		if !covered {
			uncovered = append(uncovered, dim.name)
		}
	}
	return uncovered
}

// completionTuplesFor returns the minimal sets of additional (unstated)
// dimension values under which some overlay would cover dimName=want.
// An overlay contributes a candidate tuple when it carries dimName=want and
// does not conflict with any stated query dimension. Empty tuples (overlay
// covers the value with no additions — possible only when it was
// constraint-excluded) are dropped; the exclusion context tells that story.
func (s *MetadataStore) completionTuplesFor(criteria *Criteria, dimName, want string) []map[string]string {
	candidates := []map[string]string{}
	for _, overlay := range s.Overlays {
		oc := overlay.Spec.Criteria
		if oc == nil {
			continue
		}
		tuple, ok := completionTuple(criteria, oc, dimName, want)
		if !ok {
			continue
		}
		candidates = append(candidates, tuple)
	}
	return minimalTuples(candidates)
}

// completionTuple extracts the unstated-dimension requirements of overlay
// criteria oc for a query, or ok=false when oc does not carry dimName=want
// or conflicts with a stated dimension.
func completionTuple(criteria, oc *Criteria, dimName, want string) (map[string]string, bool) {
	tuple := map[string]string{}
	carries := false
	for _, dim := range coverageDimensions {
		overlayVal := dim.value(oc)
		if dim.name == dimName {
			if overlayVal != want {
				return nil, false
			}
			carries = true
			continue
		}
		if !isSpecifiedCriteriaValue(overlayVal) {
			continue // overlay wildcard imposes nothing
		}
		queryVal := dim.value(criteria)
		if isSpecifiedCriteriaValue(queryVal) {
			if queryVal != overlayVal {
				return nil, false // conflicts with a stated dimension
			}
			continue
		}
		tuple[dim.name] = overlayVal
	}
	if !carries {
		return nil, false
	}
	return tuple, true
}

// minimalTuples dedupes, drops empty tuples and any tuple that is a
// superset of another, and sorts deterministically (size, then canonical
// key=value string).
func minimalTuples(in []map[string]string) []map[string]string {
	seen := map[string]map[string]string{}
	for _, t := range in {
		if len(t) == 0 {
			continue
		}
		seen[tupleKey(t)] = t
	}
	uniq := make([]map[string]string, 0, len(seen))
	for _, t := range seen {
		uniq = append(uniq, t)
	}
	out := []map[string]string{}
	for _, t := range uniq {
		minimal := true
		for _, u := range uniq {
			if tupleKey(u) != tupleKey(t) && isSubsetTuple(u, t) {
				minimal = false
				break
			}
		}
		if minimal {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) < len(out[j])
		}
		return tupleKey(out[i]) < tupleKey(out[j])
	})
	return out
}

// tupleKey renders a tuple canonically in coverageDimensions order.
func tupleKey(t map[string]string) string {
	parts := []string{}
	for _, dim := range coverageDimensions {
		if v, ok := t[dim.name]; ok {
			parts = append(parts, dim.name+"="+v)
		}
	}
	return strings.Join(parts, ", ")
}

func isSubsetTuple(sub, super map[string]string) bool {
	if len(sub) >= len(super) {
		return false
	}
	for k, v := range sub {
		if super[k] != v {
			return false
		}
	}
	return true
}

// verifyCriteriaCoverage enforces the resolution post-condition (issue
// #1542): every query-stated dimension must be honored by at least one
// applied overlay. It returns nil when covered, else ErrCodeInvalidRequest
// carrying per-dimension completion suggestions. excluded/warnings (from the
// evaluator path) are attached for context when present — a stated dimension
// whose only coverage was constraint-excluded is still uncovered.
func (s *MetadataStore) verifyCriteriaCoverage(criteria *Criteria, appliedOverlays []string, excluded []ExcludedOverlay, warnings []ConstraintWarning) error {
	uncovered := s.uncoveredDimensions(criteria, appliedOverlays)
	if len(uncovered) == 0 {
		return nil
	}

	clauses := make([]string, 0, len(uncovered))
	entries := make([]map[string]any, 0, len(uncovered))
	for _, dimName := range uncovered {
		want := criteriaDimensionValue(criteria, dimName)
		tuples := s.completionTuplesFor(criteria, dimName, want)
		onlyExcluded := len(tuples) == 0 && s.excludedOverlayProvides(dimName, want, excluded)
		clauses = append(clauses, completionClause(criteria, dimName, want, tuples, onlyExcluded))
		entries = append(entries, map[string]any{
			"dimension":        dimName,
			"requestedValue":   want,
			"validCompletions": tuples,
		})
	}

	ctx := map[string]any{"uncovered": entries}
	if len(excluded) > 0 {
		ctx["excludedOverlays"] = excluded
	}
	if len(warnings) > 0 {
		ctx["constraintWarnings"] = warnings
	}
	return aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest,
		strings.Join(clauses, "; "), ctx)
}

// excludedOverlayProvides reports whether any constraint-excluded overlay
// carries dimName=want — i.e. the dimension has a provider in the catalog
// that was removed by constraint evaluation rather than never existing.
func (s *MetadataStore) excludedOverlayProvides(dimName, want string, excluded []ExcludedOverlay) bool {
	for _, ex := range excluded {
		meta, ok := s.GetRecipeByName(ex.Name)
		if !ok || meta.Spec.Criteria == nil {
			continue
		}
		if criteriaDimensionValue(meta.Spec.Criteria, dimName) == want {
			return true
		}
	}
	return false
}

// criteriaDimensionValue reads one named dimension's value.
func criteriaDimensionValue(c *Criteria, dimName string) string {
	for _, dim := range coverageDimensions {
		if dim.name == dimName {
			return dim.value(c)
		}
	}
	return ""
}

// completionClause renders one uncovered dimension's error clause.
// Single-field wording is used ONLY when all minimal tuples are singletons
// over the same dimension (design 5.1). onlyExcluded distinguishes "the
// catalog has no such recipe" from "a recipe exists but every provider was
// constraint-excluded" — saying "no recipe provides" in the latter case
// would contradict the excludedOverlays context attached to the same error.
func completionClause(criteria *Criteria, dimName, want string, tuples []map[string]string, onlyExcluded bool) string {
	stated := criteria.String()
	if len(tuples) == 0 {
		if onlyExcluded {
			return fmt.Sprintf("%s '%s' for %s is provided only by overlays excluded by failing constraints (see excludedOverlays)",
				dimName, want, stated)
		}
		return fmt.Sprintf("no recipe provides %s '%s' for %s", dimName, want, stated)
	}
	if key, values, ok := sameDimensionSingletons(tuples); ok {
		return fmt.Sprintf("%s '%s' for %s requires %s (valid: %s)",
			dimName, want, stated, key, strings.Join(values, ", "))
	}
	rendered := make([]string, 0, len(tuples))
	for _, t := range tuples {
		rendered = append(rendered, "("+tupleKey(t)+")")
	}
	return fmt.Sprintf("%s '%s' requires additional criteria; supported combinations: %s",
		dimName, want, strings.Join(rendered, ", "))
}

// sameDimensionSingletons reports whether every tuple is a single-entry map
// over one shared key, returning that key and the sorted values.
func sameDimensionSingletons(tuples []map[string]string) (string, []string, bool) {
	key := ""
	values := []string{}
	for _, t := range tuples {
		if len(t) != 1 {
			return "", nil, false
		}
		for k, v := range t {
			if key == "" {
				key = k
			}
			if k != key {
				return "", nil, false
			}
			values = append(values, v)
		}
	}
	sort.Strings(values)
	return key, values, true
}

// isNotFoundEvalError reports whether a constraint-evaluation error is the
// evaluator's designed "measurement absent from snapshot" signal
// (pkg/constraints wraps it with ErrCodeNotFound). NotFound degrades
// gracefully (exclusion); every other code fails the build (design 5.2).
func isNotFoundEvalError(err error) bool {
	return stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeNotFound, ""))
}
