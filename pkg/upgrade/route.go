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

package upgrade

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
	"k8s.io/apimachinery/pkg/util/sets"
)

// targetRange returns t's `to` interval, with the ceiling derivedCeiling adds.
func targetRange(u *ComponentUpgrades, t *Transition) (versionRange, error) {
	b, err := parseRange(t.To)
	if err != nil {
		return versionRange{}, err
	}
	if c, ok := derivedCeiling(u, b); ok {
		b.upper = c
	}
	return b, nil
}

// targetFloor returns the floor of t's `to` range, the lower bound every move
// through t lands at or above. ok is false when `to` does not parse or has no
// floor.
func targetFloor(u *ComponentUpgrades, t *Transition) (floor rangeEnd, ok bool) {
	to, err := targetRange(u, t)
	if err != nil || to.lower.unbounded || to.lower.ver == nil {
		return rangeEnd{}, false
	}
	return to.lower, true
}

// derivedCeiling returns the exclusive end of the minor, or under
// StepLimitMajor the major, holding to's floor. ok is false unless u has a step
// limit and to names a floor but no ceiling, or when that minor or major is the
// last.
func derivedCeiling(u *ComponentUpgrades, to versionRange) (ceiling rangeEnd, ok bool) {
	if !to.upper.unbounded || to.lower.unbounded || to.lower.ver == nil {
		return rangeEnd{}, false
	}
	next, ok := unitStart(u, to.lower.ver, 1)
	if !ok {
		return rangeEnd{}, false
	}
	return exclusiveEnd(next), true
}

// targetRangeText returns t's `to` as written, with the ceiling
// derivedCeiling adds.
func targetRangeText(u *ComponentUpgrades, t *Transition) string {
	b, err := parseRange(t.To)
	if err != nil {
		return t.To
	}
	if c, ok := derivedCeiling(u, b); ok {
		return t.To + " <" + c.ver.String()
	}
	return t.To
}

// minorStart returns the .0 release of the minor n above v's. ok is false when
// that minor overflows.
func minorStart(v *semver.Version, n uint64) (next *semver.Version, ok bool) {
	if v.Minor() > math.MaxUint64-n {
		return nil, false
	}
	return semver.New(v.Major(), v.Minor()+n, 0, "", ""), true
}

// majorStart returns the .0.0 release of the major n above v's. ok is false
// when that major overflows.
func majorStart(v *semver.Version, n uint64) (next *semver.Version, ok bool) {
	if v.Major() > math.MaxUint64-n {
		return nil, false
	}
	return semver.New(v.Major()+n, 0, 0, "", ""), true
}

// hasStepLimit reports whether u carries a known StepLimit.
func hasStepLimit(u *ComponentUpgrades) bool {
	return u != nil && (u.StepLimit == StepLimitMinor || u.StepLimit == StepLimitMajor)
}

// unitStart returns the first release of the minor, or under StepLimitMajor
// the major, n above v's. ok is false when u has no step limit or that version
// overflows.
func unitStart(u *ComponentUpgrades, v *semver.Version, n uint64) (next *semver.Version, ok bool) {
	switch {
	case !hasStepLimit(u):
		return nil, false
	case u.StepLimit == StepLimitMajor:
		return majorStart(v, n)
	default:
		return minorStart(v, n)
	}
}

// stepCap returns the exclusive limit of a one-step move from v, which also
// leaves out the limit's prereleases. ok is false when u has no step limit or
// the limit overflows.
func stepCap(u *ComponentUpgrades, v *semver.Version) (limit rangeEnd, ok bool) {
	next, ok := unitStart(u, v, 2)
	if !ok {
		return rangeEnd{}, false
	}
	return exclusiveEnd(next), true
}

// stepViolation returns why t's moves break u's step limit, or "" when they
// keep to it, u has none, or a range does not parse.
func stepViolation(u *ComponentUpgrades, t *Transition) string {
	if !hasStepLimit(u) {
		return ""
	}
	from, ferr := parseRange(t.From)
	to, terr := targetRange(u, t)
	if ferr != nil || terr != nil || to.lower.unbounded || to.lower.ver == nil {
		return ""
	}
	if from.lower.unbounded || from.lower.ver == nil {
		return fmt.Sprintf("has a from range with no lower bound, so stepLimit %s cannot bound its moves", u.StepLimit)
	}
	lo := from.lower.ver
	if u.StepLimit == StepLimitMinor && to.lower.ver.Major() > lo.Major() {
		return crossMajorViolation(u, t, from, to)
	}
	limit, ok := stepCap(u, lo)
	if !ok {
		return ""
	}
	if upperAfter(to.upper, limit) {
		return fmt.Sprintf("has a to range reaching %s or later, past the next %s above %s, "+
			"the lowest version its from admits; stepLimit %s allows one %s per step",
			limit.ver, u.StepLimit, lo, u.StepLimit, u.StepLimit)
	}
	return ""
}

// crossMajorViolation returns why t, whose `to` floor is in a later major than
// from's lower bound, breaks u's step limit, or "" when it does not. It breaks
// the limit when:
//
//   - to is more than one major above from
//   - to reaches past the new major's first minor
//   - another transition's floor sits in a later minor of from's major, so
//     from's lowest minor is not the last of that major
func crossMajorViolation(u *ComponentUpgrades, t *Transition, from, to versionRange) string {
	lo := from.lower.ver
	if lo.Major() == math.MaxUint64 || to.lower.ver.Major() != lo.Major()+1 {
		return fmt.Sprintf("has a to range in major %d, more than one major above %s; "+
			"stepLimit minor allows one major per step", to.lower.ver.Major(), lo)
	}
	firstMinorEnd := exclusiveEnd(semver.New(lo.Major()+1, 1, 0, "", ""))
	if upperAfter(to.upper, firstMinorEnd) {
		return fmt.Sprintf("has a to range reaching %s or later; a move into a new major under "+
			"stepLimit minor lands in its first minor", firstMinorEnd.ver)
	}
	for i := range u.Transitions {
		s := &u.Transitions[i]
		if s == t {
			continue
		}
		f, ok := targetFloor(u, s)
		if ok && f.ver.Major() == lo.Major() && f.ver.Minor() > lo.Minor() {
			return fmt.Sprintf("names %d.%d as the last minor of major %d, but transition %d starts "+
				"a later one at %s", lo.Major(), lo.Minor(), lo.Major(), i, f.ver)
		}
	}
	return ""
}

// passage is another transition's boundary that a transition's moves pass.
type passage struct {
	c crossing

	// index is the position of c.tr in the component's Transitions.
	index int

	// required reports whether the passing transition must list the floor in
	// its Skips.
	required bool

	// stop reports whether c.tr is a safe RequiredStop whose `from` overlaps
	// the passing transition's. No Skips entry clears such a passage.
	stop bool
}

// passages returns the boundaries t's moves pass, in crossingBefore order.
func passages(u *ComponentUpgrades, t *Transition) []passage {
	from, ferr := parseRange(t.From)
	to, terr := targetRange(u, t)
	if ferr != nil || terr != nil {
		return nil
	}
	return passagesBetween(u, t, from, to)
}

// passagesBetween returns the boundaries t's moves from a version in from to
// one in to pass, in crossingBefore order. A move passes another transition's
// floor when it starts below the floor, reaches it, and that transition's
// `from` admits a version the move goes by below it. A floor t shares counts
// only when the other transition is blocked and t is not.
func passagesBetween(u *ComponentUpgrades, t *Transition, from, to versionRange) []passage {
	own, ok := targetFloor(u, t)
	if !ok {
		return nil
	}
	var out []passage
	for _, r := range floorsBetween(u, from, to) {
		s, f := r.tr, r.floor
		shared := boundaryKeyOf(f) == boundaryKeyOf(own)
		if s == t || (shared && (s.Verdict != VerdictBlocked || t.Verdict == VerdictBlocked)) ||
			!overlaps(r.from, rangeUpTo(from.lower, f)) {

			continue
		}
		// Required are floors inside t's `to`, manual or blocked floors, and
		// required stops whose `from` admits a version the move starts from.
		stop := s.Verdict == VerdictSafe && s.RequiredStop && overlaps(r.from, from)
		required := lowerBefore(own, f) || s.Verdict != VerdictSafe || stop
		out = append(out, passage{c: crossing{tr: s, floor: f}, index: r.index, required: required, stop: stop})
	}
	sort.SliceStable(out, func(i, j int) bool { return crossingBefore(out[i].c, out[j].c) })
	return out
}

// transitionFloor is a transition with its parsed `from` and the floor of its
// `to`.
type transitionFloor struct {
	tr    *Transition
	index int
	from  versionRange
	floor rangeEnd
}

// floorsBetween returns, in declaration order, the transitions whose `to`
// floor sits above start's lower bound and at or below end's upper one. A
// transition whose `from` or `to` does not parse is left out.
func floorsBetween(u *ComponentUpgrades, start, end versionRange) []transitionFloor {
	if u == nil {
		return nil
	}
	var out []transitionFloor
	for i := range u.Transitions {
		s := &u.Transitions[i]
		from, err := parseRange(s.From)
		if err != nil {
			continue
		}
		f, ok := targetFloor(u, s)
		if !ok || !lowerBefore(start.lower, f) || !lowerAtOrBelow(f, end.upper) {
			continue
		}
		out = append(out, transitionFloor{tr: s, index: i, from: from, floor: f})
	}
	return out
}

// rangeUpTo returns the versions from lower up to, but not including, the first
// version floor admits.
func rangeUpTo(lower, floor rangeEnd) versionRange {
	return versionRange{lower: lower, upper: rangeEnd{ver: floor.ver, inclusive: !floor.inclusive, family: floor.family}}
}

// unclearedBy reports whether t must list p's floor in Skips and does not, or
// whether p is a stop, which no listing clears.
func (p passage) unclearedBy(t *Transition) bool {
	return p.stop || (p.required && !skipsVersion(t, p.c.floor.ver))
}

// skipsVersion reports whether t's Skips names v.
func skipsVersion(t *Transition, v *semver.Version) bool {
	return slices.ContainsFunc(t.Skips, func(s string) bool {
		sv, err := semver.NewVersion(s)
		return err == nil && sv.Equal(v)
	})
}

// describes reports whether t's moves from a version in from to one in to pass
// no floor t must list but does not.
func describes(u *ComponentUpgrades, t *Transition, from, to versionRange) bool {
	return !slices.ContainsFunc(passagesBetween(u, t, from, to), func(p passage) bool { return p.unclearedBy(t) })
}

// floorOnlyRange returns the interval holding only t's `to` floor, or the
// zero versionRange when `to` does not parse.
func floorOnlyRange(u *ComponentUpgrades, t *Transition) versionRange {
	to, err := targetRange(u, t)
	if err != nil {
		return versionRange{}
	}
	return versionRange{lower: to.lower, upper: rangeEnd{ver: to.lower.ver, inclusive: to.lower.inclusive, family: to.lower.family}}
}

// transitionsWithinStepLimit returns u's transitions that keep to its step
// limit.
func transitionsWithinStepLimit(u *ComponentUpgrades) sets.Set[*Transition] {
	out := sets.New[*Transition]()
	if u == nil {
		return out
	}
	for i := range u.Transitions {
		if t := &u.Transitions[i]; stepViolation(u, t) == "" {
			out.Insert(t)
		}
	}
	return out
}

// overlap returns the interval a and b share. ok is false when they share no
// version.
func overlap(a, b versionRange) (out versionRange, ok bool) {
	out = a
	if lowerBefore(a.lower, b.lower) {
		out.lower = b.lower
	}
	if upperAfter(a.upper, b.upper) {
		out.upper = b.upper
	}
	return out, lowerAtOrBelow(out.lower, out.upper)
}

// route is a sequence of records from the source toward the target, each
// starting inside the `to` of the one before.
type route struct {
	hops    []crossing
	nonSafe int

	// start is where the last hop starts, the source or the previous landing.
	start versionRange
}

// better reports whether a has fewer hops than b, then fewer non-safe hops,
// then earlier hops in crossingBefore order.
func (a route) better(b route) bool {
	if len(a.hops) != len(b.hops) {
		return len(a.hops) < len(b.hops)
	}
	if a.nonSafe != b.nonSafe {
		return a.nonSafe < b.nonSafe
	}
	for i := range a.hops {
		if a.hops[i].tr != b.hops[i].tr {
			return crossingBefore(a.hops[i], b.hops[i])
		}
	}
	return false
}

func (a route) withHop(c crossing, start versionRange) route {
	out := route{hops: append(slices.Clone(a.hops), c), nonSafe: a.nonSafe, start: start}
	if c.tr.Verdict != VerdictSafe {
		out.nonSafe++
	}
	return out
}

// landing returns the versions in both a's `to` and b's `from`. ok is false
// when there are none or either range does not parse.
func landing(u *ComponentUpgrades, a, b *Transition) (l versionRange, ok bool) {
	to, err := targetRange(u, a)
	if err != nil {
		return versionRange{}, false
	}
	from, err := parseRange(b.From)
	if err != nil {
		return versionRange{}, false
	}
	return overlap(to, from)
}

// plan returns the best route over crossed from src to tgt, skipping records
// that break u's step limit. complete reports whether best reaches tgt. When
// it does not, best reaches furthest, and ok is false when no record describes
// a move from src.
func plan(u *ComponentUpgrades, crossed []crossing, src, tgt *semver.Version) (best route, complete, ok bool) {
	// Each hop's floor sits above the last, so a record's best route depends
	// only on the records below it, and one pass in floor order finds every
	// one.
	limited := transitionsWithinStepLimit(u)
	var order []crossing
	for _, c := range crossed {
		if limited.Has(c.tr) {
			order = append(order, c)
		}
	}
	at := onlyVersion(src)
	reach := make(map[*Transition]route, len(order))
	for i, c := range order {
		var cand route
		found := false
		if fromCovers(c.tr, inclusiveEnd(src)) && describes(u, c.tr, at, floorOnlyRange(u, c.tr)) {
			cand, found = route{}.withHop(c, at), true
		}
		for _, p := range order[:i] {
			prev, has := reach[p.tr]
			if !has || !lowerBefore(p.floor, c.floor) {
				continue
			}
			l, meets := landing(u, p.tr, c.tr)
			if !meets || !lowerBefore(l.lower, c.floor) || !lowerAtOrBelow(l.lower, inclusiveEnd(tgt)) ||
				!describes(u, p.tr, prev.start, l) || !describes(u, c.tr, l, floorOnlyRange(u, c.tr)) {

				continue
			}
			if next := prev.withHop(c, l); !found || next.better(cand) {
				cand, found = next, true
			}
		}
		if found {
			reach[c.tr] = cand
		}
	}
	var partial route
	var partialTop rangeEnd
	for _, c := range order {
		r, has := reach[c.tr]
		if !has {
			continue
		}
		to, err := targetRange(u, c.tr)
		if err != nil {
			continue
		}
		// A route into tgt that passes an unlisted floor is neither complete
		// nor partial.
		if to.contains(tgt) {
			if describes(u, c.tr, r.start, onlyVersion(tgt)) && (!complete || r.better(best)) {
				best, complete = r, true
			}
			continue
		}
		if !ok || upperAfter(to.upper, partialTop) ||
			(!upperAfter(partialTop, to.upper) && r.better(partial)) {

			partial, partialTop, ok = r, to.upper, true
		}
	}
	if complete {
		return best, true, true
	}
	return partial, false, ok
}

// stopKind says why a move ends short of the target.
type stopKind int

const (
	stopSteps stopKind = iota
	stopRequired
	stopStepLimit
)

// move is one upgrade an operator performs, either a single hop or a run of
// safe hops taken in one step.
type move struct {
	hops []crossing

	// lands is where the move ends, the target for the last move.
	lands versionRange

	// why says what ends the move, and is zero on the last.
	why stopKind
}

// verdict returns the verdict of m's first hop, which for a run is safe.
func (m move) verdict() Verdict {
	return m.hops[0].tr.Verdict
}

// moves splits r, from src to tgt, into the upgrades an operator performs.
// Consecutive safe hops join into one move unless:
//
//   - the earlier hop is a RequiredStop
//   - the joined move breaks u's step limit
//   - joinMustSplit reports the joined move passes a floor it may not
func moves(u *ComponentUpgrades, r route, src, tgt *semver.Version) []move {
	target := onlyVersion(tgt)
	var out []move
	first, start := 0, onlyVersion(src)
	for i := 1; i < len(r.hops); i++ {
		prev, next := r.hops[i-1].tr, r.hops[i].tr
		end := target
		if i+1 < len(r.hops) {
			end, _ = landing(u, next, r.hops[i+1].tr)
		}
		why, joins := stopSteps, prev.Verdict == VerdictSafe && next.Verdict == VerdictSafe
		switch {
		case joins && prev.RequiredStop:
			why, joins = stopRequired, false
		case joins && !withinStep(u, start.lower.ver, end.lower):
			why, joins = stopStepLimit, false
		case joins && joinMustSplit(u, r.hops[first:i+1], start, end):
			joins = false
		}
		if joins {
			continue
		}
		l, _ := landing(u, prev, next)
		out = append(out, move{hops: r.hops[first:i], lands: clipToStep(u, start.lower.ver, l), why: why})
		first = i
		if l.lower.ver != nil {
			start = l
		}
	}
	return append(out, move{hops: r.hops[first:], lands: target})
}

// joinMustSplit reports whether joining run into one move from start to end
// would pass the floor of a record outside run that is a RequiredStop
// admitting a version in start, or manual or blocked and admitting a version
// the move goes by below the floor. A hop in run listing the floor clears a
// manual or blocked one unless that record admits a version in start. Rule 11
// lets a hop list a stricter floor only for sources outside that record's
// `from`.
func joinMustSplit(u *ComponentUpgrades, run []crossing, start, end versionRange) bool {
	for _, r := range floorsBetween(u, start, end) {
		s := r.tr
		if (s.Verdict == VerdictSafe && !s.RequiredStop) || slices.ContainsFunc(run, func(c crossing) bool { return c.tr == s }) {
			continue
		}
		if s.Verdict == VerdictSafe {
			if overlaps(r.from, start) {
				return true
			}
			continue
		}
		if !overlaps(r.from, rangeUpTo(start.lower, r.floor)) {
			continue
		}
		if overlaps(r.from, start) || !anyHopSkips(run, r.floor.ver) {
			return true
		}
	}
	return false
}

// anyHopSkips reports whether any hop in run lists v in its Skips.
func anyHopSkips(run []crossing, v *semver.Version) bool {
	return slices.ContainsFunc(run, func(c crossing) bool { return skipsVersion(c.tr, v) })
}

// withinStep reports whether a move from start may reach end under u's step
// limit.
func withinStep(u *ComponentUpgrades, start *semver.Version, end rangeEnd) bool {
	limit, ok := stepCap(u, start)
	if !ok {
		return true
	}
	return end.ver != nil && end.comparedAt().Compare(limit.comparedAt()) < 0
}

// clipToStep returns l cut to what one step from start reaches under u's step
// limit.
func clipToStep(u *ComponentUpgrades, start *semver.Version, l versionRange) versionRange {
	limit, ok := stepCap(u, start)
	if !ok || !upperAfter(l.upper, limit) {
		return l
	}
	l.upper = limit
	return l
}

// rangeText returns b in the record range grammar.
func rangeText(b versionRange) string {
	if !b.lower.unbounded && !b.upper.unbounded && b.lower.inclusive && b.upper.inclusive &&
		b.lower.ver.Equal(b.upper.ver) {

		return "=" + b.lower.ver.String()
	}
	var parts []string
	if !b.lower.unbounded {
		op := ">"
		if b.lower.inclusive {
			op = ">="
		}
		parts = append(parts, op+b.lower.ver.String())
	}
	if !b.upper.unbounded {
		op := "<"
		if b.upper.inclusive {
			op = "<="
		}
		parts = append(parts, op+b.upper.ver.String())
	}
	return strings.Join(parts, " ")
}

// moveLandingText returns the range m, a move before the last, lands in,
// written as its last hop's `to` when the landing is all of it.
func moveLandingText(u *ComponentUpgrades, m move) string {
	last := m.hops[len(m.hops)-1].tr
	if to, err := targetRange(u, last); err == nil && endsEqual(to.lower, m.lands.lower) && endsEqual(to.upper, m.lands.upper) {
		return targetRangeText(u, last)
	}
	return rangeText(m.lands)
}

// endsEqual reports whether a and b are the same range end.
func endsEqual(a, b rangeEnd) bool {
	if a.unbounded || b.unbounded {
		return a.unbounded == b.unbounded
	}
	if a.ver == nil || b.ver == nil {
		return a.ver == nil && b.ver == nil && a.inclusive == b.inclusive
	}
	return a.inclusive == b.inclusive && a.comparedAt().Equal(b.comparedAt())
}

// moveLandingPhrase returns the lowest version m, a move before the last,
// lands on when that is inclusive, and "a version in" its landing range
// otherwise.
func moveLandingPhrase(u *ComponentUpgrades, m move) string {
	if m.lands.lower.inclusive && m.lands.lower.ver != nil {
		return m.lands.lower.ver.String()
	}
	return "a version in " + moveLandingText(u, m)
}
