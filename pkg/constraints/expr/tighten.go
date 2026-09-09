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

// fullPrecision is the component count pkg/version treats as fully
// significant (major.minor.patch).
const fullPrecision = 3

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

	terms := make([]string, 0, 4)
	terms, lowerKept := appendBound(terms, lower, droppedBound(existingLower, candidateLower, lower))
	terms, upperKept := appendBound(terms, upper, droppedBound(existingUpper, candidateUpper, upper))

	// A retained loser still restricts, so it narrows even when the winning
	// bound came from the composition. Reporting Unchanged there would drop
	// the profile's own restriction.
	if !lowerNarrowed && !upperNarrowed && !lowerKept && !upperKept {
		return existing, TightenUnchanged
	}
	return strings.Join(terms, " "), TightenNarrowed
}

// appendBound writes the winning bound and, where dropping the loser could
// widen the range, the loser as well. It reports whether the loser was kept.
//
// pkg/version compares at the lower of two precisions, so an actual written
// with fewer components than the bounds can satisfy ">= 1.34.1" while failing
// "> 1.34.0" — dropping the exclusive term would then admit a version the
// composition excluded. Both terms are AND-joined in the same clause, so
// keeping the loser states the intersection exactly. The loser is dropped as
// redundant when it is inclusive, or when the winner is exclusive too: only
// an exclusive bound can reject a version its own neighborhood admits, so
// only an inclusive winner can lose that exclusion.
func appendBound(terms []string, winner, dropped *bound) ([]string, bool) {
	if winner == nil {
		return terms, false
	}
	terms = append(terms, winner.String())
	if dropped != nil && !dropped.inclusive() && winner.inclusive() {
		return append(terms, dropped.String()), true
	}
	return terms, false
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

		// One bound per direction. A clause stating two — which is the shape
		// this package itself emits when it retains an exclusive loser —
		// cannot be reduced to one without losing a restriction, and picking
		// the stronger would silently drop the other. Refusing keeps that
		// failure closed; the expression still evaluates normally, it just
		// does not take part in another intersection.
		switch term.Operator {
		case OperatorGTE, OperatorGT:
			if lower != nil {
				return nil, nil, false
			}
			lower = b
		case OperatorLTE, OperatorLT:
			if upper != nil {
				return nil, nil, false
			}
			upper = b
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

// boundsSatisfiable reports whether some version satisfies both bounds.
//
// An open side is always satisfiable. A closed range is decided on the order
// pkg/version.Compare defines, over full-precision endpoints keyed by their
// position in it — including the GKE build dimension, where the bare numeric
// core precedes every build of itself. Working in that order rather than
// approximating it is what keeps the adjacent cases honest: nothing sits
// between "-gke.100" and "-gke.101", nor between a bare core and its
// "-gke.0", while a bare core and the next patch always have builds between
// them.
func boundsSatisfiable(lower, upper *bound) bool {
	if lower == nil || upper == nil {
		return true
	}
	low, lowAdmitted := lower.limit()
	high, highAdmitted := upper.limit()

	// Normalize the floor to the first version it admits, which the discrete
	// finest dimension always makes representable, and then read the answer
	// straight off the order.
	first := versionKeyOf(low)
	if !lowAdmitted {
		first = first.next()
	}

	cmp := first.compare(versionKeyOf(high))
	if highAdmitted {
		return cmp <= 0
	}
	return cmp < 0
}

// limit returns the full-precision version where the bound's half-line ends,
// and whether that version is itself admitted.
//
// A bound written at full precision is its own endpoint. A coarser one
// quantifies over a whole band of versions, and zero-padding it is only
// right for half the operators: ">= 1.35" does start at 1.35.0, but
// "> 1.35" excludes every 1.35.x — pkg/version compares an actual at the
// bound's precision, so 1.35.9 reads as equal to 1.35 and fails — which
// makes its true endpoint 1.36.0. Symmetrically "< 1.35" ends at 1.35.0
// while "<= 1.35" runs to just below 1.36.0. Bumping the last significant
// component covers the two that step past their own band; the endpoint of a
// coarse bound is then admitted exactly when it is a lower bound.
func (b *bound) limit() (version.Version, bool) {
	if b.parsed.Precision >= fullPrecision {
		return b.parsed, b.inclusive()
	}

	// Compare stops at the lower of the two precisions and only reaches the
	// GKE build dimension once whole numeric cores match, so a bound written
	// below full precision never has its own suffix consulted when it is
	// evaluated. Carrying that suffix into the endpoint would invent an
	// ordering the evaluator does not apply.
	coarse := b.parsed
	coarse.Extras = ""

	isLower := b.operator == OperatorGTE || b.operator == OperatorGT
	if b.operator == OperatorGT || b.operator == OperatorLTE {
		return bumpLastSignificant(coarse), isLower
	}
	return atFullPrecision(coarse), isLower
}

// atFullPrecision returns v with every component significant, so a bound
// written as "1.36" compares as the "1.36.0" its range starts at.
func atFullPrecision(v version.Version) version.Version {
	v.Precision = fullPrecision
	return v
}

// bumpLastSignificant returns the next version after v's band: the component
// v's precision stops at is incremented and the rest zeroed, so "1.35"
// becomes 1.36.0 and "1" becomes 2.0.0.
func bumpLastSignificant(v version.Version) version.Version {
	if v.Precision <= 1 {
		v.Major++
		v.Minor = 0
	} else {
		v.Minor++
	}
	v.Patch = 0
	v.Precision = fullPrecision
	return v
}

// versionKey is a full-precision version's position in the total order
// pkg/version.Compare defines, as a tuple ordered lexicographically.
//
// The build component carries the whole GKE dimension. Compare ranks a
// "-gke.N" build above the bare numeric core it builds on and orders two
// builds numerically, so ranking a bare core 0 and a build N at N+1
// reproduces that order exactly. Extras that are not a valid GKE build are
// never compared, which the same rank of 0 expresses.
type versionKey struct {
	major int
	minor int
	patch int
	build int64
}

func versionKeyOf(v version.Version) versionKey {
	key := versionKey{major: v.Major, minor: v.Minor, patch: v.Patch}
	if build, isGKE := version.ExtractGKEBuild(v.Extras); isGKE {
		key.build = build + 1
	}
	return key
}

// next returns the version immediately after k. The build component is a
// non-negative integer and is the finest dimension ordered, so every key has
// an immediate successor: the one after a bare core is its "-gke.0" build.
// That is what lets an exclusive floor be restated as the inclusive floor one
// step up, which in turn makes emptiness a plain comparison.
func (k versionKey) next() versionKey {
	k.build++
	return k
}

func (k versionKey) compare(other versionKey) int {
	switch {
	case k.major != other.major:
		return compareInt(int64(k.major), int64(other.major))
	case k.minor != other.minor:
		return compareInt(int64(k.minor), int64(other.minor))
	case k.patch != other.patch:
		return compareInt(int64(k.patch), int64(other.patch))
	default:
		return compareInt(k.build, other.build)
	}
}

func compareInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
