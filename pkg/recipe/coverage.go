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
// nodes is deliberately absent: no overlay in the embedded catalog gates on
// nodes, so it does not participate in overlay selection or coverage.
// nodes IS included in Criteria.Specificity() so that nodes-only CLI queries
// pass the minimum-specificity guard — but it is NOT in Criteria.Matches(),
// so it never filters overlays. External --data catalogs with criteria.nodes
// set on any overlay are rejected at load time (ErrCodeInvalidRequest) to
// prevent silent match-all behavior; operators must remove or zero
// criteria.nodes before upgrading. See issue #1781 (design 4.3, #1542).
type coverageDimension struct {
	name  string
	value func(*Criteria) string

	// strict marks a dimension the caller must state when omitting it would
	// resolve to a weaker recipe than the catalog can produce. See
	// strictDimensionGaps for the rule and why os is the only one.
	strict bool
}

// coverageDimensions is ordered; all coverage reporting uses this order.
var coverageDimensions = []coverageDimension{
	{name: string(FieldService), value: func(c *Criteria) string { return string(c.Service) }},
	{name: string(FieldAccelerator), value: func(c *Criteria) string { return string(c.Accelerator) }},
	{name: string(FieldIntent), value: func(c *Criteria) string { return string(c.Intent) }},
	// os is the only strict dimension, and the reason is the driver.
	//
	// Every other dimension degrades to a smaller but coherent recipe when
	// omitted: no --platform yields no Slurm/Kubeflow layer, no --intent
	// yields untuned GPU Operator values, no --accelerator yields generic
	// GPU config. Each answer is complete, just less specific.
	//
	// os is different because it decides whether the GPU driver can be
	// installed at all. On Ubuntu the GPU Operator installs the driver, so
	// an OS-agnostic recipe is a real answer; that is why recipes/overlays
	// carries eks.yaml with no os. On COS the operator installs no driver
	// (Google supplies it) and the device-plugin owner differs, which is why
	// every gke overlay is os-gated and no OS-agnostic gke recipe exists to
	// hand back. Resolving one anyway would emit a recipe whose driver story
	// is wrong rather than merely generic.
	//
	// The driver argument is a property of installing NVIDIA drivers on Linux
	// rather than of this catalog's shape, so it carries to external --data
	// catalogs. Note what that does and does not claim: it says os is the
	// right dimension to demand, NOT that every catalog shape is served well
	// by demanding it. A split-coverage external catalog (service overlay,
	// os-agnostic accelerator overlay, one os-gated tuned leaf) is rejected
	// here asking for an os, because no single overlay carries the stated
	// combination. That is deliberate and the escape hatch is explicit:
	// declare an os-agnostic overlay carrying the combination, which is the
	// same assertion eks.yaml makes.
	//
	// The assumption that would break the driver argument itself is a cluster
	// whose node pools run different operating systems. AICR has no model for
	// that; it is expressed as separate recipes.
	{name: string(FieldOS), value: func(c *Criteria) string { return string(c.OS) }, strict: true},
	{name: string(FieldPlatform), value: func(c *Criteria) string { return string(c.Platform) }},
}

// CoverageDimensionNames returns the criteria dimension names subject to the
// coverage post-condition, in canonical (coverageDimensions) order.
//
// These are the exact strings that appear as the "dimension" key of each
// details.uncovered entry on a coverage failure, so a caller acting on that
// error — clearing the reported dimensions and retrying, as
// pkg/client/v1's snapshot-criteria relaxation does — can pin its own
// dimension vocabulary against this list rather than hand-copying it.
// nodes is absent for the reason given on coverageDimension.
func CoverageDimensionNames() []string {
	names := make([]string, 0, len(coverageDimensions))
	for _, dim := range coverageDimensions {
		names = append(names, dim.name)
	}
	return names
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
		// Completeness holds: every stated dimension is carried by something.
		// That is necessary but not sufficient — it is satisfied when service
		// and accelerator are honored by two SEPARATE overlays while no single
		// overlay covers the combination. Joint sufficiency catches that, and
		// absorbs the retired requireOSIfNeeded guard (issue #1782).
		if gaps := s.strictDimensionGaps(criteria, appliedOverlays); len(gaps) > 0 {
			return strictGapError(criteria, gaps, excluded, warnings)
		}
		return nil
	}

	clauses := make([]string, 0, len(uncovered))
	entries := make([]map[string]any, 0, len(uncovered))
	for _, dimName := range uncovered {
		want := criteriaDimensionValue(criteria, dimName)
		tuples := s.completionTuplesFor(criteria, dimName, want)
		// constraintExcluded distinguishes WHY the dimension is uncovered: an
		// overlay carrying it exists but the observed cluster failed its
		// constraints, versus no overlay states it at all. Callers that relax
		// uncovered dimensions and retry (pkg/client/v1) must not relax the
		// former — doing so converts "your cluster fails this overlay's
		// requirements" into a broader recipe that silently succeeds.
		constraintExcluded := s.excludedOverlayProvides(dimName, want, excluded)
		onlyExcluded := len(tuples) == 0 && constraintExcluded
		clauses = append(clauses, completionClause(criteria, dimName, want, tuples, onlyExcluded))
		entries = append(entries, map[string]any{
			"dimension":          dimName,
			"requestedValue":     want,
			"validCompletions":   tuples,
			"constraintExcluded": constraintExcluded,
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

// setCriteriaDimension writes one named dimension's value. Write-side twin of
// criteriaDimensionValue; strictDimensionGaps uses it to probe what the
// applied set would look like had the caller stated a dimension.
func setCriteriaDimension(c *Criteria, name, value string) {
	switch name {
	case string(FieldService):
		c.Service = CriteriaServiceType(value)
	case string(FieldAccelerator):
		c.Accelerator = CriteriaAcceleratorType(value)
	case string(FieldIntent):
		c.Intent = CriteriaIntentType(value)
	case string(FieldOS):
		c.OS = CriteriaOSType(value)
	case string(FieldPlatform):
		c.Platform = CriteriaPlatformType(value)
	}
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

// strictGap names a strict dimension the caller must state, with the values
// that would reach the overlays currently being skipped.
type strictGap struct {
	dimension   string
	validValues []string
}

// jointlyCarriesAllStated reports whether some applied overlay carries every
// dimension the query states, with matching values.
//
// This is the escape hatch that keeps the generic tier valid. When one
// overlay already honors the whole stated combination, nothing was silently
// dropped and no further criteria are demanded — `--service eks` resolves
// through eks.yaml and is never asked for an os. Per-dimension coverage
// cannot express this: it is satisfied when service and accelerator are
// honored by two SEPARATE overlays, which is a pile of ingredients rather
// than the recipe for the combination (issue #1782).
func (s *MetadataStore) jointlyCarriesAllStated(criteria *Criteria, applied []string) bool {
	for _, name := range applied {
		meta, ok := s.GetRecipeByName(name)
		if !ok || meta.Spec.Criteria == nil {
			continue
		}
		carriesAll := true
		for _, dim := range coverageDimensions {
			want := dim.value(criteria)
			if !isSpecifiedCriteriaValue(want) {
				continue
			}
			if dim.value(meta.Spec.Criteria) != want {
				carriesAll = false
				break
			}
		}
		if carriesAll {
			return true
		}
	}
	return false
}

// strictDimensionGaps returns the strict dimensions the caller must state,
// or nil when resolution may proceed.
//
// The rule, replacing the retired requireOSIfNeeded guard:
//
//	Resolution fails when NO applied overlay jointly carries every stated
//	dimension AND stating a strict dimension would reach an overlay that is
//	currently being skipped.
//
// Both halves are required. The first is jointlyCarriesAllStated above. The
// second is what detects the loss: if naming an os would pull in an overlay
// that is not applied, that overlay's content is being dropped by omission
// rather than by choice.
//
// The demand is for PRESENCE, not for a particular value. validValues is
// advisory, matching the retired guard's "specify an OS (valid: cos)"
// wording; supplying a value outside it is legal and falls through to the
// ordinary completeness path above.
//
// requireOSIfNeeded hardcoded three separate scopes: it only ran when
// service was stated, it only compared service+accelerator regardless of
// what the caller actually asked for, and it only ever demanded os. Only the
// third survives here, and only for the reason recorded on coverageDimensions.
// The subset now comes from the query.
func (s *MetadataStore) strictDimensionGaps(criteria *Criteria, applied []string) []strictGap {
	if s.jointlyCarriesAllStated(criteria, applied) {
		return nil
	}

	appliedSet := make(map[string]struct{}, len(applied))
	for _, name := range applied {
		appliedSet[name] = struct{}{}
	}

	gaps := make([]strictGap, 0, len(coverageDimensions))
	for _, dim := range coverageDimensions {
		if !dim.strict || isSpecifiedCriteriaValue(dim.value(criteria)) {
			continue // elective, or the caller already stated it
		}
		reachable := map[string]struct{}{}
		for _, value := range s.dimensionValues(dim) {
			probe := *criteria
			setCriteriaDimension(&probe, dim.name, value)
			// One overlay outside the applied set is enough to establish that
			// this value reaches something currently being skipped; the rest
			// of the matches and their chains cannot change the answer.
			if s.reachesUnappliedOverlay(&probe, appliedSet) {
				reachable[value] = struct{}{}
			}
		}
		if len(reachable) == 0 {
			continue
		}
		values := make([]string, 0, len(reachable))
		for v := range reachable {
			values = append(values, v)
		}
		sort.Strings(values)
		gaps = append(gaps, strictGap{dimension: dim.name, validValues: values})
	}
	if len(gaps) == 0 {
		return nil
	}
	return gaps
}

// reachesUnappliedOverlay reports whether resolving probe would pull in any
// overlay that is not already applied.
func (s *MetadataStore) reachesUnappliedOverlay(probe *Criteria, applied map[string]struct{}) bool {
	for _, match := range s.FindMatchingOverlays(probe) {
		for _, name := range s.inheritanceChainNames(match) {
			if _, already := applied[name]; !already {
				return true
			}
		}
	}
	return false
}

// dimensionValues returns every value the catalog declares for a dimension,
// sorted. Iteration order of s.Overlays is randomized, so the sort is what
// makes strictDimensionGaps deterministic.
func (s *MetadataStore) dimensionValues(dim coverageDimension) []string {
	seen := map[string]struct{}{}
	for _, overlay := range s.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		if v := dim.value(overlay.Spec.Criteria); isSpecifiedCriteriaValue(v) {
			seen[v] = struct{}{}
		}
	}
	values := make([]string, 0, len(seen))
	for v := range seen {
		values = append(values, v)
	}
	sort.Strings(values)
	return values
}

// inheritanceChainNames returns the overlay and every ancestor it inherits
// from, matching the appliedOverlays semantics used by coverage.
func (s *MetadataStore) inheritanceChainNames(overlay *RecipeMetadata) []string {
	names := []string{}
	for cur := overlay; cur != nil; {
		names = append(names, cur.Metadata.Name)
		if cur.Spec.Base == "" {
			break
		}
		next, ok := s.GetRecipeByName(cur.Spec.Base)
		if !ok {
			break
		}
		cur = next
	}
	return names
}

// strictGapError renders the strict-dimension failure. The message mirrors
// the retired guard so operator-facing wording does not regress, and the
// context uses its own key rather than `uncovered`: pkg/client/v1 relaxation
// CLEARS uncovered dimensions and retries, which here would discard the check
// and return the partial recipe that issue #1542 fixed.
//
// excluded/warnings are attached exactly as the completeness path attaches
// them. reachesUnappliedOverlay probes through the UNFILTERED overlay set, so
// on the evaluator path an overlay that would cover the combination but was
// removed by a failing constraint still counts as reachable. Without this
// context the caller is told to state an os, supplies it, and only then meets
// the real constraint failure. The demand itself is still correct — the
// combination genuinely is not covered — so this is a diagnosis aid, not a
// gate: the error stands either way.
func strictGapError(criteria *Criteria, gaps []strictGap, excluded []ExcludedOverlay, warnings []ConstraintWarning) error {
	clauses := make([]string, 0, len(gaps))
	entries := make([]map[string]any, 0, len(gaps))
	for _, gap := range gaps {
		clauses = append(clauses, fmt.Sprintf("%s (valid: %s)",
			gap.dimension, strings.Join(gap.validValues, ", ")))
		entries = append(entries, map[string]any{
			"dimension":   gap.dimension,
			"validValues": gap.validValues,
		})
	}
	ctx := map[string]any{"strictDimensions": entries}
	if len(excluded) > 0 {
		ctx["excludedOverlays"] = excluded
	}
	if len(warnings) > 0 {
		ctx["constraintWarnings"] = warnings
	}
	return aicrerrors.NewWithContext(aicrerrors.ErrCodeInvalidRequest,
		fmt.Sprintf("%s has no recipe covering that combination; specify %s",
			criteria.String(), strings.Join(clauses, ", ")),
		ctx)
}
