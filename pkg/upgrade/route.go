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

// toBounds returns t's `to` interval, with the ceiling derivedCeiling adds.
func toBounds(u *ComponentUpgrades, t *Transition) (bounds, error) {
	b, err := parseBounds(t.To)
	if err != nil {
		return bounds{}, err
	}
	if c, ok := derivedCeiling(u, b); ok {
		b.upper = c
	}
	return b, nil
}

// derivedCeiling returns the exclusive end of the minor, or under
// StepLimitMajor the major, holding to's floor. ok is false unless u has a step
// limit and to names a floor but no ceiling, or when that minor or major is the
// last.
func derivedCeiling(u *ComponentUpgrades, to bounds) (ceiling bound, ok bool) {
	if !to.upper.unbounded || to.lower.unbounded || to.lower.ver == nil {
		return bound{}, false
	}
	next, ok := unitStart(u, to.lower.ver, 1)
	if !ok {
		return bound{}, false
	}
	return below(next), true
}

// toText returns t's `to` as written, with the ceiling derivedCeiling adds.
func toText(u *ComponentUpgrades, t *Transition) string {
	b, err := parseBounds(t.To)
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
func stepCap(u *ComponentUpgrades, v *semver.Version) (limit bound, ok bool) {
	next, ok := unitStart(u, v, 2)
	if !ok {
		return bound{}, false
	}
	return below(next), true
}

// stepViolation returns why t's moves break u's step limit, or "" when they
// keep to it, u has none, or a range does not parse.
func stepViolation(u *ComponentUpgrades, t *Transition) string {
	if !hasStepLimit(u) {
		return ""
	}
	from, ferr := parseBounds(t.From)
	to, terr := toBounds(u, t)
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
//   - another transition's floor inside from sits in a later minor of from's
//     major, so from's lowest minor is not the last of that major
func crossMajorViolation(u *ComponentUpgrades, t *Transition, from, to bounds) string {
	lo := from.lower.ver
	if lo.Major() == math.MaxUint64 || to.lower.ver.Major() != lo.Major()+1 {
		return fmt.Sprintf("has a to range in major %d, more than one major above %s; "+
			"stepLimit minor allows one major per step", to.lower.ver.Major(), lo)
	}
	firstMinorEnd := below(semver.New(lo.Major()+1, 1, 0, "", ""))
	if upperAfter(to.upper, firstMinorEnd) {
		return fmt.Sprintf("has a to range reaching %s or later; a move into a new major under "+
			"stepLimit minor lands in its first minor", firstMinorEnd.ver)
	}
	for i := range u.Transitions {
		s := &u.Transitions[i]
		if s == t {
			continue
		}
		sto, err := toBounds(u, s)
		if err != nil || sto.lower.unbounded || sto.lower.ver == nil {
			continue
		}
		f := sto.lower.ver
		if f.Major() == lo.Major() && f.Minor() > lo.Minor() && from.contains(f) {
			return fmt.Sprintf("names %d.%d as the last minor of major %d, but transition %d starts "+
				"a later one at %s", lo.Major(), lo.Minor(), lo.Major(), i, f)
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
	from, ferr := parseBounds(t.From)
	to, terr := toBounds(u, t)
	if ferr != nil || terr != nil {
		return nil
	}
	return passagesOf(u, t, from, to)
}

// passagesOf returns the boundaries t's moves from a version in from to one in
// to pass, in crossingBefore order. A move passes another transition's floor
// when it starts below the floor, reaches it, and that transition's `from`
// admits a version the move goes by below it. A floor t shares counts only
// when the other transition is blocked and t is not.
func passagesOf(u *ComponentUpgrades, t *Transition, from, to bounds) []passage {
	if u == nil {
		return nil
	}
	ownTo, err := toBounds(u, t)
	if err != nil || ownTo.lower.unbounded || ownTo.lower.ver == nil {
		return nil
	}
	own := ownTo.lower
	var out []passage
	for i := range u.Transitions {
		s := &u.Transitions[i]
		if s == t {
			continue
		}
		sfrom, err := parseBounds(s.From)
		if err != nil {
			continue
		}
		sto, err := toBounds(u, s)
		if err != nil || sto.lower.unbounded || sto.lower.ver == nil {
			continue
		}
		f := sto.lower
		shared := boundaryKeyOf(f) == boundaryKeyOf(own)
		if (shared && (s.Verdict != VerdictBlocked || t.Verdict == VerdictBlocked)) ||
			!lowerBefore(from.lower, f) || !lowerAtOrBelow(f, to.upper) {

			continue
		}
		below := bounds{lower: from.lower, upper: bound{ver: f.ver, inclusive: !f.inclusive, family: f.family}}
		if !intervalsIntersect(sfrom, below) {
			continue
		}
		// Required are floors inside t's `to`, manual or blocked floors, and
		// required stops whose `from` admits a version the move starts from.
		stop := s.Verdict == VerdictSafe && s.RequiredStop && intervalsIntersect(sfrom, from)
		required := lowerBefore(own, f) || s.Verdict != VerdictSafe || stop
		out = append(out, passage{c: crossing{tr: s, floor: f}, index: i, required: required, stop: stop})
	}
	sort.SliceStable(out, func(i, j int) bool { return crossingBefore(out[i].c, out[j].c) })
	return out
}

// unlistedBy reports whether t must list p's floor in Skips and does not, or
// whether p is a stop, which no listing clears.
func (p passage) unlistedBy(t *Transition) bool {
	return p.stop || (p.required && !lists(t, p.c.floor.ver))
}

// lists reports whether t's Skips names v.
func lists(t *Transition, v *semver.Version) bool {
	return slices.ContainsFunc(t.Skips, func(s string) bool {
		sv, err := semver.NewVersion(s)
		return err == nil && sv.Equal(v)
	})
}

// describes reports whether t's moves from a version in from to one in to pass
// no floor t must list but does not.
func describes(u *ComponentUpgrades, t *Transition, from, to bounds) bool {
	return !slices.ContainsFunc(passagesOf(u, t, from, to), func(p passage) bool { return p.unlistedBy(t) })
}

// floorOf returns the interval holding only t's `to` floor, or the zero bounds
// when `to` does not parse.
func floorOf(u *ComponentUpgrades, t *Transition) bounds {
	to, err := toBounds(u, t)
	if err != nil {
		return bounds{}
	}
	return bounds{lower: to.lower, upper: bound{ver: to.lower.ver, inclusive: to.lower.inclusive, family: to.lower.family}}
}

// withinLimit returns u's transitions that keep to its step limit.
func withinLimit(u *ComponentUpgrades) sets.Set[*Transition] {
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

// intersect returns the interval a and b share. ok is false when they share no
// version.
func intersect(a, b bounds) (out bounds, ok bool) {
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
	start bounds
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

func (a route) then(c crossing, start bounds) route {
	out := route{hops: append(slices.Clone(a.hops), c), nonSafe: a.nonSafe, start: start}
	if c.tr.Verdict != VerdictSafe {
		out.nonSafe++
	}
	return out
}

// landing returns the versions in both a's `to` and b's `from`. ok is false
// when there are none or either range does not parse.
func landing(u *ComponentUpgrades, a, b *Transition) (l bounds, ok bool) {
	to, err := toBounds(u, a)
	if err != nil {
		return bounds{}, false
	}
	from, err := parseBounds(b.From)
	if err != nil {
		return bounds{}, false
	}
	return intersect(to, from)
}

// plan returns the best route over crossed from src to tgt, skipping records
// that break u's step limit. complete reports whether best reaches tgt. When
// it does not, best reaches furthest, and ok is false when no record describes
// a move from src.
func plan(u *ComponentUpgrades, crossed []crossing, src, tgt *semver.Version) (best route, complete, ok bool) {
	// Each hop's floor sits above the last, so a record's best route depends
	// only on the records below it, and one pass in floor order finds every
	// one.
	limited := withinLimit(u)
	var order []crossing
	for _, c := range crossed {
		if limited.Has(c.tr) {
			order = append(order, c)
		}
	}
	at := bounds{lower: pointAt(src), upper: pointAt(src)}
	reach := make(map[*Transition]route, len(order))
	for i, c := range order {
		var cand route
		found := false
		if fromCovers(c.tr, pointAt(src)) && describes(u, c.tr, at, floorOf(u, c.tr)) {
			cand, found = route{}.then(c, at), true
		}
		for _, p := range order[:i] {
			prev, has := reach[p.tr]
			if !has || !lowerBefore(p.floor, c.floor) {
				continue
			}
			l, meets := landing(u, p.tr, c.tr)
			if !meets || !lowerBefore(l.lower, c.floor) || !lowerAtOrBelow(l.lower, pointAt(tgt)) ||
				!describes(u, p.tr, prev.start, l) || !describes(u, c.tr, l, floorOf(u, c.tr)) {

				continue
			}
			if next := prev.then(c, l); !found || next.better(cand) {
				cand, found = next, true
			}
		}
		if found {
			reach[c.tr] = cand
		}
	}
	var partial route
	var partialTop bound
	for _, c := range order {
		r, has := reach[c.tr]
		if !has {
			continue
		}
		to, err := toBounds(u, c.tr)
		if err != nil {
			continue
		}
		// A route into tgt that passes an unlisted floor is neither complete
		// nor partial.
		if to.contains(tgt) {
			if describes(u, c.tr, r.start, bounds{lower: pointAt(tgt), upper: pointAt(tgt)}) && (!complete || r.better(best)) {
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
	lands bounds

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
//   - the joined move passes a floor joinPasses reports
func moves(u *ComponentUpgrades, r route, src, tgt *semver.Version) []move {
	target := bounds{lower: pointAt(tgt), upper: pointAt(tgt)}
	var out []move
	first, start := 0, bounds{lower: pointAt(src), upper: pointAt(src)}
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
		case joins && joinPasses(u, r.hops[first:i+1], start, end):
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

// joinPasses reports whether one move from start to end over run passes the
// floor of a record outside run that is a RequiredStop admitting a version in
// start, or manual or blocked and admitting a version the move goes by below
// the floor. A hop in run listing the floor clears a manual or blocked one
// unless that record admits a version in start. Rule 11 lets a hop list a
// stricter floor only for sources outside that record's `from`.
func joinPasses(u *ComponentUpgrades, run []crossing, start, end bounds) bool {
	for i := range u.Transitions {
		s := &u.Transitions[i]
		if (s.Verdict == VerdictSafe && !s.RequiredStop) || slices.ContainsFunc(run, func(c crossing) bool { return c.tr == s }) {
			continue
		}
		sfrom, err := parseBounds(s.From)
		if err != nil {
			continue
		}
		sto, err := toBounds(u, s)
		if err != nil || sto.lower.unbounded || sto.lower.ver == nil {
			continue
		}
		f := sto.lower
		if !lowerBefore(start.lower, f) || !lowerAtOrBelow(f, end.upper) {
			continue
		}
		by := bounds{lower: start.lower, upper: bound{ver: f.ver, inclusive: !f.inclusive, family: f.family}}
		if s.Verdict == VerdictSafe {
			by = start
		}
		if !intervalsIntersect(sfrom, by) {
			continue
		}
		if s.Verdict == VerdictSafe || intervalsIntersect(sfrom, start) ||
			!slices.ContainsFunc(run, func(c crossing) bool { return lists(c.tr, f.ver) }) {

			return true
		}
	}
	return false
}

// withinStep reports whether a move from start may reach end under u's step
// limit.
func withinStep(u *ComponentUpgrades, start *semver.Version, end bound) bool {
	limit, ok := stepCap(u, start)
	if !ok {
		return true
	}
	return end.ver != nil && end.at().Compare(limit.at()) < 0
}

// clipToStep returns l cut to what one step from start reaches under u's step
// limit.
func clipToStep(u *ComponentUpgrades, start *semver.Version, l bounds) bounds {
	limit, ok := stepCap(u, start)
	if !ok || !upperAfter(l.upper, limit) {
		return l
	}
	l.upper = limit
	return l
}

// rangeText returns b in the record range grammar.
func rangeText(b bounds) string {
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

// landsText returns the range m, a move before the last, lands in, written as
// its last hop's `to` when the landing is all of it.
func landsText(u *ComponentUpgrades, m move) string {
	last := m.hops[len(m.hops)-1].tr
	if to, err := toBounds(u, last); err == nil && boundEqual(to.lower, m.lands.lower) && boundEqual(to.upper, m.lands.upper) {
		return toText(u, last)
	}
	return rangeText(m.lands)
}

// boundEqual reports whether a and b are the same bound.
func boundEqual(a, b bound) bool {
	if a.unbounded || b.unbounded {
		return a.unbounded == b.unbounded
	}
	if a.ver == nil || b.ver == nil {
		return a.ver == nil && b.ver == nil && a.inclusive == b.inclusive
	}
	return a.inclusive == b.inclusive && a.at().Equal(b.at())
}

// landsPhrase returns the lowest version m, a move before the last, lands on
// when that is inclusive, and "a version in" its landing range otherwise.
func landsPhrase(u *ComponentUpgrades, m move) string {
	if m.lands.lower.inclusive && m.lands.lower.ver != nil {
		return m.lands.lower.ver.String()
	}
	return "a version in " + landsText(u, m)
}
