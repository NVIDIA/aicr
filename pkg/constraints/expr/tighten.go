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

package expr

import (
	"strings"

	"github.com/NVIDIA/aicr/pkg/version"
)

// TightenOutcome reports how two same-named constraint expressions relate.
type TightenOutcome int

const (
	// TightenIncomparable means the pair is not a bounded version range on
	// both sides — an exact match, an equality or inequality term, an OR
	// alternative, or a value the version parser cannot read. Callers must
	// not merge these: "stricter" is undefined for them.
	TightenIncomparable TightenOutcome = iota

	// TightenUnchanged means the candidate admits everything the existing
	// expression already admits, so the intersection is the existing one.
	TightenUnchanged

	// TightenNarrowed means the intersection is strictly smaller than the
	// existing expression; the returned expression is that intersection.
	TightenNarrowed

	// TightenUnsatisfiable means the two expressions have no version in
	// common, so no cluster could satisfy both.
	TightenUnsatisfiable

	// TightenPrecisionMismatch means both sides bound the same direction but
	// are written at different precisions (">= 1.34" against ">= 1.34.1"), so
	// pkg/version compares them at the lower precision and reports them equal.
	// Which one is stricter is unknowable from the expressions alone.
	TightenPrecisionMismatch
)

// Tighten intersects two same-named constraint expressions and returns the
// combined expression together with how it relates to existing.
//
// Only bounded version ranges intersect: every term on both sides must use
// >=, >, <=, or < and neither side may carry an OR alternative. That covers
// the version floors and ceilings recipes actually compose (">= 1.34.1
// < 1.36.0" tightened by ">= 1.35" yields ">= 1.35 < 1.36.0") while leaving
// predicates whose values do not order — the node-set label constraints,
// exact matches, "!=", and same-direction bounds written at different
// precisions — reported as TightenIncomparable so the caller keeps rejecting
// them rather than silently merging a contradiction.
//
// The returned expression is written in the same grammar it was parsed from
// (a single AND clause), so it round-trips through ParseCompoundConstraint.
func Tighten(existing, candidate string) (string, TightenOutcome) {
	existingLower, existingUpper, ok := versionBounds(existing)
	if !ok {
		return "", TightenIncomparable
	}
	candidateLower, candidateUpper, ok := versionBounds(candidate)
	if !ok {
		return "", TightenIncomparable
	}

	lower, lowerNarrowed, ok := strongerBound(existingLower, candidateLower)
	if !ok {
		return "", TightenPrecisionMismatch
	}
	upper, upperNarrowed, ok := strongerBound(existingUpper, candidateUpper)
	if !ok {
		return "", TightenPrecisionMismatch
	}

	if !boundsSatisfiable(lower, upper) {
		return "", TightenUnsatisfiable
	}
	if !lowerNarrowed && !upperNarrowed {
		return existing, TightenUnchanged
	}

	terms := make([]string, 0, 4)
	terms = appendBound(terms, lower, droppedBound(existingLower, candidateLower, lower))
	terms = appendBound(terms, upper, droppedBound(existingUpper, candidateUpper, upper))
	return strings.Join(terms, " "), TightenNarrowed
}

// appendBound writes the winning bound and, where dropping the loser could
// widen the range, the loser as well.
//
// pkg/version compares at the lower of two precisions, so an actual written
// with fewer components than the bounds can satisfy ">= 1.34.1" while failing
// "> 1.34.0" — dropping the exclusive term would then admit a version the
// composition excluded. Both terms are AND-joined in the same clause, so
// keeping the loser states the intersection exactly. The loser is dropped as
// redundant when it is inclusive, or when the winner is exclusive too: only
// an exclusive bound can reject a version its own neighborhood admits, so
// only an inclusive winner can lose that exclusion.
func appendBound(terms []string, winner, dropped *bound) []string {
	if winner == nil {
		return terms
	}
	terms = append(terms, winner.String())
	if dropped != nil && !dropped.inclusive() && winner.inclusive() {
		terms = append(terms, dropped.String())
	}
	return terms
}

// droppedBound returns the same-direction bound that lost to winner, or nil
// when there was no contest.
func droppedBound(existing, candidate, winner *bound) *bound {
	if existing == nil || candidate == nil {
		return nil
	}
	if winner == candidate {
		return existing
	}
	return candidate
}

// bound is one ordering term of a version range, kept with its parsed
// version so comparisons do not re-parse.
type bound struct {
	operator Operator
	parsed   version.Version
	term     ParsedConstraint
}

func (b *bound) String() string {
	return b.term.String()
}

// inclusive reports whether the bound admits its own version (">=" and "<=").
func (b *bound) inclusive() bool {
	return b.operator == OperatorGTE || b.operator == OperatorLTE
}

// versionBounds reduces an expression to at most one lower and one upper
// bound. It reports false for anything that is not a single AND clause of
// parseable ordering terms, including a clause that repeats a direction in a
// way the caller should not silently reconcile.
func versionBounds(expression string) (lower, upper *bound, ok bool) {
	compound, err := ParseCompoundConstraint(expression)
	if err != nil || len(compound.Alternatives) != 1 {
		return nil, nil, false
	}

	for _, term := range compound.Alternatives[0] {
		parsed, err := version.ParseVersion(term.Value)
		if err != nil {
			return nil, nil, false
		}
		b := &bound{operator: term.Operator, parsed: parsed, term: term}
		var ok bool
		switch term.Operator {
		case OperatorGTE, OperatorGT:
			if lower, _, ok = strongerBound(lower, b); !ok {
				return nil, nil, false
			}
		case OperatorLTE, OperatorLT:
			if upper, _, ok = strongerBound(upper, b); !ok {
				return nil, nil, false
			}
		case OperatorEQ, OperatorNE, OperatorExact:
			return nil, nil, false
		default:
			return nil, nil, false
		}
	}
	if lower == nil && upper == nil {
		return nil, nil, false
	}
	return lower, upper, true
}

// strongerBound returns the more restrictive of two bounds in the same
// direction and reports whether that is the candidate. A nil bound is the
// absence of a limit, so the non-nil one always wins.
//
// It reports ok=false when the two versions are not orderable against each
// other: pkg/version compares at the lower of the two precisions, so ">= 1.32"
// and ">= 1.32.4" compare equal even though the second is strictly stricter.
// Picking either would be a guess, and the fail-open direction — silently
// keeping ">= 1.32" — would admit clusters the profile means to exclude, so
// the pair is reported unorderable and the caller keeps rejecting it.
func strongerBound(existing, candidate *bound) (stronger *bound, narrowed, ok bool) {
	switch {
	case candidate == nil:
		return existing, false, true
	case existing == nil:
		return candidate, true, true
	}

	cmp := candidate.parsed.Compare(existing.parsed)
	if cmp == 0 && candidate.parsed.Precision != existing.parsed.Precision {
		return nil, false, false
	}

	if candidate.operator == OperatorGTE || candidate.operator == OperatorGT {
		// Lower bounds: the higher version wins; at the same version the
		// exclusive form ">" admits strictly less than ">=".
		if cmp > 0 || (cmp == 0 && existing.inclusive() && !candidate.inclusive()) {
			return candidate, true, true
		}
		return existing, false, true
	}
	// Upper bounds: the lower version wins; "<" admits less than "<=".
	if cmp < 0 || (cmp == 0 && existing.inclusive() && !candidate.inclusive()) {
		return candidate, true, true
	}
	return existing, false, true
}

// boundsSatisfiable reports whether some version satisfies both bounds. An
// open side is always satisfiable; a closed range is empty when the floor is
// above the ceiling, or equal to it with either side exclusive.
func boundsSatisfiable(lower, upper *bound) bool {
	if lower == nil || upper == nil {
		return true
	}
	cmp := lower.parsed.Compare(upper.parsed)
	if cmp > 0 {
		return false
	}
	if cmp == 0 {
		// Equal at the lower of the two precisions (">= 1.35" against
		// "< 1.35.2") does not mean the range is empty, so an emptiness
		// verdict is only safe when both bounds carry the same precision.
		// Where it is not, the range is allowed through and constraint
		// evaluation — which fails closed — remains the backstop.
		if lower.parsed.Precision != upper.parsed.Precision {
			return true
		}
		return lower.inclusive() && upper.inclusive()
	}
	return true
}
