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
	"path/filepath"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// Validate reports every well-formedness violation across the set.
//
// It aggregates rather than failing on the first problem: an author fixing a
// record should see all of it in one run.
func (s Set) Validate(comps []Component) error {
	pins := make(map[string]string, len(comps))
	for _, c := range comps {
		pins[c.Name] = c.PinnedVersion
	}
	names := make([]string, 0, len(s))
	for name := range s {
		names = append(names, name)
	}
	sort.Strings(names)

	violations := make([]string, 0, len(names))
	for _, name := range names {
		violations = append(violations, validateRecord(s[name], pins[name])...)
	}
	if len(violations) == 0 {
		return nil
	}
	return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
		"%d upgrade record violation(s):\n  - %s",
		len(violations), strings.Join(violations, "\n  - ")))
}

func validateRecord(u *ComponentUpgrades, pin string) []string {
	v := make([]string, 0, len(u.Transitions))
	for i := range u.Transitions {
		where := fmt.Sprintf("component %q transition %d", u.Component, i)
		v = append(v, checkVerdictFields(where, &u.Transitions[i])...)
		v = append(v, checkStepGroups(where, &u.Transitions[i])...)
		v = append(v, checkPinCeiling(where, &u.Transitions[i], pin)...)
		v = append(v, checkDirectional(where, &u.Transitions[i])...)
		v = append(v, checkHooks(where, &u.Transitions[i])...)
	}
	v = append(v, checkCoverage(u.Component, u.Transitions, pin)...)
	v = append(v, checkDistinctBoundaries(u.Component, u.Transitions)...)
	v = append(v, checkReplaces(u.Component, u.Replaces)...)
	return v
}

// checkReplaces applies rules 4, 5 and 6 to a replaces block. Rules 2, 3 and 7
// do not apply: a replaces block carries no from/to ranges, because it
// describes a component swap rather than a version boundary.
//
// replaces.component is deliberately not checked against the registry. ADR-021
// says a replaces block joins the removed and added rows into one, so the
// superseded component is by definition already gone from the registry.
func checkReplaces(component string, r *Replaces) []string {
	if r == nil {
		return nil
	}
	where := fmt.Sprintf("component %q replaces block", component)
	var v []string
	if r.Component == "" {
		v = append(v, where+" names no component to supersede")
	}
	// Rules 4, 5 and 6 are verdict-and-steps shaped, which a replaces block
	// shares exactly, as are the verdict, summary and step-id field rules.
	// Reuse rather than restate.
	as := &Transition{
		Verdict:         r.Verdict,
		VerifiedBy:      r.VerifiedBy,
		Summary:         r.Summary,
		StepsByDeployer: r.StepsByDeployer,
	}
	v = append(v, checkVerdictFields(where, as)...)
	v = append(v, checkStepGroups(where, as)...)
	return v
}

// nonAuthorableVerdict reports a verdict a record must not carry. Validate is
// exported on an exported map type, so a caller can build a Set without going
// through Load, and every rule keyed to a verdict reads as satisfied when the
// verdict itself is garbage.
func nonAuthorableVerdict(where string, v Verdict) string {
	return fmt.Sprintf(
		"%s has verdict %q; only safe, manual and blocked may be authored (unknown and unversioned are computed)",
		where, v)
}

// checkVerdictFields implements rules 4 and 5 plus the field-level rules the
// ADR implies. Hooks are deliberately permitted on every verdict: a verdict
// describes what the operator must do, and a hook is AICR doing it instead.
func checkVerdictFields(where string, t *Transition) []string {
	var v []string
	steps := 0
	for _, g := range t.StepsByDeployer {
		steps += len(g.Steps)
	}
	switch t.Verdict {
	case VerdictSafe:
		if t.VerifiedBy == "" {
			v = append(v, where+" is safe but names no verifiedBy; a safe verdict must name the UAT lane, KWOK run, or upstream release note that backs it")
		}
		if steps > 0 {
			v = append(v, where+" is safe but must not carry steps")
		}
	case VerdictManual, VerdictBlocked:
		if len(t.StepsByDeployer) == 0 {
			v = append(v, fmt.Sprintf("%s is %s and must carry at least one step", where, t.Verdict))
		}
		// Per group, not per transition. A covered-but-empty group hands its
		// deployers a verdict promising steps with none for them, which is the
		// failure the partition rule exists to prevent, arriving by a different
		// door.
		for gi, g := range t.StepsByDeployer {
			if len(g.Steps) == 0 {
				v = append(v, fmt.Sprintf(
					"%s group %d is %s but carries no steps; every group must carry at least one",
					where, gi, t.Verdict))
			}
		}
	case VerdictUnknown, VerdictUnversioned:
		v = append(v, nonAuthorableVerdict(where, t.Verdict))
	default:
		v = append(v, nonAuthorableVerdict(where, t.Verdict))
	}
	if t.Summary == "" {
		v = append(v, where+" is missing summary")
	}
	// Only the affirmative claim needs notes. ADR-021's own worked example
	// carries reversible: false with none, and the rendering rationale — never
	// surface an unexplained "reversible: yes" — has nothing to say about a
	// transition that simply does not claim to be reversible.
	if t.Reversible != nil && *t.Reversible && t.ReversibleNotes == "" {
		v = append(v, where+" sets reversible: true but carries no reversibleNotes; renderers never surface the flag alone")
	}
	for gi, g := range t.StepsByDeployer {
		seen := make(map[string]bool, len(g.Steps))
		for _, st := range g.Steps {
			if st.ID == "" {
				v = append(v, fmt.Sprintf("%s group %d has a step with no id", where, gi))
				continue
			}
			if seen[st.ID] {
				v = append(v, fmt.Sprintf("%s group %d has a duplicate step id %q", where, gi, st.ID))
			}
			seen[st.ID] = true
			if st.Description == "" {
				v = append(v, fmt.Sprintf("%s group %d step %q has no description", where, gi, st.ID))
			}
		}
	}
	return v
}

// canonicalDeployers mirrors config.GetDeployerTypes(), sorted. It is declared
// here rather than imported: pkg/bundler/config transitively pulls 621
// packages, 245 of them k8s.io/client-go, which is the wrong price for five
// strings. wellformed_test.go carries that import and fails on drift.
//
// localformat is deliberately absent — it is the internal bundle-layout package
// every deployer consumes, not a selectable deployer.
var canonicalDeployers = []string{"argocd", "argocd-helm", "flux", "helm", "helmfile"}

// checkStepGroups implements rule 6. Groups partition the deployers: no two
// explicit groups may claim the same one, at most one group may omit deployers
// (it is *the* remainder), and a manual or blocked verdict must cover every
// deployer, or an operator receives a verdict promising steps with none for them.
func checkStepGroups(where string, t *Transition) []string {
	var v []string
	known := make(map[string]bool, len(canonicalDeployers))
	for _, d := range canonicalDeployers {
		known[d] = true
	}

	claimed := make(map[string]bool)
	remainders := 0
	for gi, g := range t.StepsByDeployer {
		// yaml.v3 distinguishes an absent key (nil) from `deployers: []`
		// (non-nil, empty). Only the absent form is the remainder group;
		// an explicit empty list almost certainly means the opposite of
		// what it would otherwise do.
		if g.Deployers == nil {
			remainders++
			continue
		}
		if len(g.Deployers) == 0 {
			v = append(v, fmt.Sprintf(
				"%s group %d sets deployers to an empty list; omit the key entirely to mean the remainder", where, gi))
			continue
		}
		inGroup := make(map[string]bool, len(g.Deployers))
		for _, d := range g.Deployers {
			switch {
			case !known[d]:
				v = append(v, fmt.Sprintf("%s group %d names %q, which is not a selectable deployer (want one of %v)",
					where, gi, d, canonicalDeployers))
			case inGroup[d]:
				v = append(v, fmt.Sprintf("%s group %d has deployer %q listed twice", where, gi, d))
			case claimed[d]:
				v = append(v, fmt.Sprintf("%s deployer %q is claimed by more than one group", where, d))
			default:
				claimed[d] = true
			}
			inGroup[d] = true
		}
	}
	if remainders > 1 {
		v = append(v, fmt.Sprintf(
			"%s has %d groups omitting deployers; more than one group omits deployers, but there is exactly one remainder",
			where, remainders))
	}
	if remainders == 0 && (t.Verdict == VerdictManual || t.Verdict == VerdictBlocked) {
		for _, d := range canonicalDeployers {
			if !claimed[d] {
				v = append(v, fmt.Sprintf("%s is %s but has no steps for deployer %q", where, t.Verdict, d))
			}
		}
	}
	return v
}

// checkPinCeiling implements rule 2. It fires per transition, so a record
// carrying only a replaces block is untouched: it has no `to` to compare.
//
// The non-comparable-pin failure is an addition to ADR-021 rather than a
// transcription of it. The ADR assigns such a pin the unversioned verdict at
// check time and does not make it an authoring error; failing closed here means
// a record cannot make version claims nobody can verify. Relaxing this later is
// the cheap direction if it proves wrong.
func checkPinCeiling(where string, t *Transition, pin string) []string {
	b, err := parseBounds(t.To, prereleaseAllowed)
	if err != nil {
		// Not "already reported by the loader": Validate is exported on an
		// exported map type, so a Set can be built without ever going
		// through Load. Skipping here would fail open on rules 2, 3 and 7.
		return []string{where + " has an unparseable to range: " + err.Error()}
	}
	var v []string
	if b.upper.unbounded {
		v = append(v, where+" has a to range with no upper bound; a record must name the ceiling of the block it describes")
	}
	if b.lower.unbounded {
		v = append(v, where+" has a to range with no lower bound; without one the record would apply to every target version")
	}
	if b.upper.unbounded {
		return v
	}
	if strings.Contains(pin, "+") {
		return append(v, fmt.Sprintf(
			"%s is pinned at %q, which carries build metadata; semver orders build metadata as equal, so such a bump would move past no ceiling",
			where, pin))
	}
	pinVer, perr := semver.NewVersion(pin)
	if perr != nil {
		return append(v, fmt.Sprintf(
			"%s is pinned at %q, which is not a comparable version, so no ceiling can be checked against it",
			where, pin))
	}
	if b.upper.ver.Compare(pinVer) > 0 {
		v = append(v, fmt.Sprintf(
			"%s has a to ceiling of %s which reaches past the pinned version %s; widen from backward instead",
			where, b.upper.ver, pinVer))
	}
	return v
}

// checkDirectional implements rule 7. A record describes going forward and says
// nothing about coming back, so `from`'s domain must not intersect
// [lower(to), ∞). Matching a forward record in reverse is the "negative check
// that passes on an ambiguous condition" anti-pattern, and it is easy to write
// by accident.
func checkDirectional(where string, t *Transition) []string {
	fb, ferr := parseBounds(t.From, prereleaseForbidden)
	tb, terr := parseBounds(t.To, prereleaseAllowed)
	if ferr != nil {
		return []string{where + " has an unparseable from range: " + ferr.Error()}
	}
	if terr != nil {
		return nil // reported by checkPinCeiling
	}
	// parseBounds accepts an empty or contradictory interval (e.g.
	// ">=0.20.0 <0.18.0") because at the bounds layer such a range is
	// harmless: it matches nothing. Here it would sail through the
	// disjointness check below and produce a record that can never apply, so
	// it is rejected as its own violation.
	if !fb.lower.unbounded && !fb.upper.unbounded {
		cmp := fb.lower.ver.Compare(fb.upper.ver)
		if cmp > 0 || (cmp == 0 && (!fb.lower.inclusive || !fb.upper.inclusive)) {
			return []string{fmt.Sprintf(
				"%s has a from range %q that matches no version: the lower bound %s is not below the upper bound %s",
				where, t.From, fb.lower.ver, fb.upper.ver)}
		}
	}
	if fb.upper.unbounded {
		return []string{where + " has a from range with no upper bound, so it cannot be shown to be forward-only"}
	}
	if tb.lower.unbounded {
		return nil // reported by checkPinCeiling
	}
	cmp := fb.upper.ver.Compare(tb.lower.ver)
	disjoint := cmp < 0 || (cmp == 0 && (!fb.upper.inclusive || !tb.lower.inclusive))
	if disjoint {
		return nil
	}
	return []string{fmt.Sprintf(
		"%s matches in reverse: from reaches %s but to starts at %s, so a downgrade would select this record",
		where, fb.upper.ver, tb.lower.ver)}
}

// checkCoverage implements rule 3: no *interior* hole between the from domains.
//
// ADR-021 words this as adjacency ("each from meets or overlaps the previous
// to"), but that formulation is vacuous for the <X-shaped from ranges its own
// example uses and passes on real gaps. The property its next sentence states —
// "a hole between them is a version range the component could be running that
// no record describes" — is coverage, and that is what this implements.
//
// Coverage starts at the lowest from floor, not at zero: requiring zero would
// reject the ADR's stated ordinary case (from ">=25.0 <26.0"). A version below
// every record resolves to the unknown verdict, not a malformed file. It runs
// up to the pin, which is why a single-transition record is checked too.
func checkCoverage(component string, trs []Transition, pin string) []string {
	if len(trs) == 0 {
		return nil
	}
	intervals := make([]bounds, 0, len(trs))
	for i := range trs {
		b, err := parseBounds(trs[i].From, prereleaseForbidden)
		if err != nil {
			// Rule 3 is a whole-record property, and the range that failed to
			// parse may be the one bridging the gap, so a hole computed from
			// what is left would be a false positive the author cannot act on.
			// Rule 2 is per transition, a property partial data can still
			// answer, which is why it reports and this does not.
			// checkDirectional names the unparseable range itself.
			return nil
		}
		intervals = append(intervals, b)
	}
	sort.Slice(intervals, func(i, j int) bool {
		return lowerBefore(intervals[i].lower, intervals[j].lower)
	})

	pinVer, perr := semver.NewVersion(pin)
	cur := intervals[0].upper
	var v []string
	for _, next := range intervals[1:] {
		if cur.unbounded {
			return v // everything above is covered
		}
		if !contiguous(cur, next.lower) {
			// Only an interior hole below the pin matters.
			if perr == nil && cur.ver.Compare(pinVer) >= 0 {
				break
			}
			v = append(v, fmt.Sprintf(
				"component %q leaves a version gap after %s that no record describes; an operator on a version in that range would match no transition",
				component, cur.ver))
		}
		// Merge with a running maximum: a wholly contained range must not
		// shrink the covered span.
		if next.upper.unbounded || (!cur.unbounded && upperAfter(next.upper, cur)) {
			cur = next.upper
		}
	}
	return append(v, checkCoverageReachesPin(component, cur, pinVer, perr)...)
}

// checkCoverageReachesPin closes rule 3's upper end. Comparing consecutive
// intervals alone stops at the highest from ceiling the file happens to name,
// so a record whose coverage falls short of the pin is accepted while every
// version between the two matches no transition — the exact shape a record has
// immediately after a pin bump that nobody extended it for.
func checkCoverageReachesPin(component string, cur bound, pinVer *semver.Version, perr error) []string {
	if cur.unbounded || perr != nil || cur.ver.Compare(pinVer) >= 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"component %q leaves a version gap between %s and the pinned version %s that no record describes; an operator on a version in that range would match no transition",
		component, cur.ver, pinVer)}
}

// lowerBefore orders lower bounds, unbounded first.
func lowerBefore(a, b bound) bool {
	if a.unbounded != b.unbounded {
		return a.unbounded
	}
	if a.unbounded {
		return false
	}
	if cmp := a.ver.Compare(b.ver); cmp != 0 {
		return cmp < 0
	}
	return a.inclusive && !b.inclusive
}

// upperAfter reports whether a extends coverage beyond b.
func upperAfter(a, b bound) bool {
	if a.unbounded {
		return true
	}
	if b.unbounded {
		return false
	}
	if cmp := a.ver.Compare(b.ver); cmp != 0 {
		return cmp > 0
	}
	return a.inclusive && !b.inclusive
}

// contiguous reports whether coverage continues from an upper bound into the
// next lower bound. Comparing versions alone is not enough: "<0.18.0" followed
// by ">0.18.0" leaves exactly 0.18.0 uncovered while the versions look equal.
func contiguous(upper, lower bound) bool {
	if upper.unbounded || lower.unbounded {
		return true
	}
	cmp := lower.ver.Compare(upper.ver)
	if cmp < 0 {
		return true
	}
	if cmp > 0 {
		return false
	}
	return upper.inclusive || lower.inclusive
}

// boundaryKey identifies a `to` floor by version and inclusivity as two
// comparable fields rather than one delimited string: `to` permits
// prereleases, and a prerelease tag can itself read as a delimiter suffix,
// so string concatenation cannot distinguish content from marker.
type boundaryKey struct {
	ver       string
	inclusive bool
}

// boundaryClaim is the transition that first claimed a boundary, kept with its
// `from` domain so a later claim on the same floor can be tested for overlap.
type boundaryClaim struct {
	idx  int
	from bounds
}

// checkDistinctBoundaries implements rule 8, an addition to ADR-021. A
// boundary is identified by the floor its `to` names, so two transitions
// sharing that floor describe the same boundary twice. ADR-021's matcher
// blocks any jump where more than one record applies, so a duplicate silently
// converts that boundary's crossings from manual to blocked.
//
// Sharing a floor is only a duplicate when the two `from` domains intersect.
// Where they are disjoint, ADR-021's own applies predicate lets at most one of
// them match any given source version, so the pair describes one boundary
// reached from two different blocks rather than the same boundary twice.
//
// Overlapping `from` domains stay legal across *different* floors on purpose: a
// jump spanning two real blocks must resolve to blocked, which is the ADR's
// design.
func checkDistinctBoundaries(component string, trs []Transition) []string {
	seen := make(map[boundaryKey][]boundaryClaim, len(trs))
	var v []string
	for i := range trs {
		b, err := parseBounds(trs[i].To, prereleaseAllowed)
		if err != nil || b.lower.unbounded {
			continue // reported elsewhere
		}
		from, ferr := parseBounds(trs[i].From, prereleaseForbidden)
		if ferr != nil {
			continue // reported by checkDirectional
		}
		key := boundaryKey{ver: b.lower.ver.String(), inclusive: b.lower.inclusive}
		dup, found := -1, false
		for _, c := range seen[key] {
			if intervalsIntersect(c.from, from) {
				dup, found = c.idx, true
				break
			}
		}
		if found {
			v = append(v, fmt.Sprintf(
				"component %q transitions %d and %d describe the same boundary (to starts at %s) for overlapping from ranges; a jump crossing it would resolve to blocked rather than the authored verdict",
				component, dup, i, key.ver))
			continue
		}
		seen[key] = append(seen[key], boundaryClaim{idx: i, from: from})
	}
	return v
}

// intervalsIntersect reports whether two intervals share at least one version.
func intervalsIntersect(a, b bounds) bool {
	return lowerAtOrBelow(a.lower, b.upper) && lowerAtOrBelow(b.lower, a.upper)
}

// lowerAtOrBelow reports whether a lower bound sits at or below an upper bound,
// so the pair admits at least one version. Unlike contiguous, which asks
// whether coverage continues across a shared point and accepts either side
// including it, sharing a version requires both sides to include it.
func lowerAtOrBelow(lo, hi bound) bool {
	if lo.unbounded || hi.unbounded {
		return true
	}
	cmp := lo.ver.Compare(hi.ver)
	if cmp != 0 {
		return cmp < 0
	}
	return lo.inclusive && hi.inclusive
}

// hookDir is where ADR-021 puts a hook manifest. The location is load-bearing
// rather than a convention: tools/bom walks only .../manifests, and
// recipes/manifest_images_test.go skips any path without /manifests/, so a
// hook outside that tree carries an image nothing pins and the BOM never sees.
const hookDir = "manifests/migrations/"

// checkHooks implements rule 9, an addition to ADR-021. Hooks are the
// deliberate exception that lets a safe verdict carry work, so an unvalidated
// phase silently doing nothing is worse than a rejected record. file is gated
// with filepath.IsLocal rather than a substring scan for "..", per CLAUDE.md.
func checkHooks(where string, t *Transition) []string {
	var v []string
	for i, h := range t.Hooks {
		switch h.Phase {
		case "pre-upgrade", "post-upgrade":
		default:
			v = append(v, fmt.Sprintf(
				"%s hook %d has phase %q, expected %q or %q", where, i, h.Phase, "pre-upgrade", "post-upgrade"))
		}
		switch {
		case h.File == "":
			v = append(v, fmt.Sprintf("%s hook %d has no file", where, i))
		case !filepath.IsLocal(h.File):
			v = append(v, fmt.Sprintf(
				"%s hook %d file %q must be a local path under the bundle", where, i, h.File))
		case !strings.HasPrefix(h.File, hookDir):
			v = append(v, fmt.Sprintf(
				"%s hook %d file %q must live under %s, the only tree the BOM and the image-pin test walk",
				where, i, h.File, hookDir))
		}
	}
	return v
}
