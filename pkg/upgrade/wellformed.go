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
	"slices"
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
	slices.Sort(names)

	violations := make([]string, 0, len(names))
	for _, name := range names {
		violations = append(violations, validateRecord(name, s[name], pins[name])...)
	}
	if len(violations) == 0 {
		return nil
	}
	return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
		"%d upgrade record violation(s):\n  - %s",
		len(violations), strings.Join(violations, "\n  - ")))
}

func validateRecord(name string, u *ComponentUpgrades, pin string) []string {
	if u == nil {
		// Validate is exported on an exported map type (see nonAuthorableVerdict
		// above), so a caller-built Set can hold a nil record without ever
		// going through Load, which never produces one.
		return []string{fmt.Sprintf("component %q has a nil record", name)}
	}
	v := make([]string, 0, len(u.Transitions))
	limited := hasStepLimit(u)
	if u.StepLimit != "" && !limited {
		v = append(v, fmt.Sprintf("component %q has stepLimit %q; it must be %q or %q",
			u.Component, u.StepLimit, StepLimitMinor, StepLimitMajor))
	}
	for i := range u.Transitions {
		where := fmt.Sprintf("component %q transition %d", u.Component, i)
		v = append(v, checkVerdictFields(where, &u.Transitions[i])...)
		v = append(v, checkStepGroups(where, &u.Transitions[i])...)
		v = append(v, checkPinCeiling(where, &u.Transitions[i], pin, limited)...)
		v = append(v, checkDirectional(where, &u.Transitions[i])...)
		v = append(v, checkHooks(where, &u.Transitions[i])...)
		v = append(v, checkAffectedResources(where, &u.Transitions[i])...)
		v = append(v, checkSkips(where, u, &u.Transitions[i])...)
		if msg := stepViolation(u, &u.Transitions[i]); msg != "" {
			v = append(v, where+" "+msg)
		}
	}
	v = append(v, checkCoverage(u.Component, u.Transitions, pin)...)
	v = append(v, checkDistinctMoves(u)...)
	v = append(v, checkReplaces(u.Component, u.Replaces)...)
	return v
}

// checkSkips implements rule 11, an addition to ADR-021. It returns a
// violation for each floor t's moves pass that t must list in Skips but does
// not, for each Skips entry that is invalid or names no floor they pass, and
// for each listed floor whose transition has a stricter verdict and a `from`
// overlapping t's.
func checkSkips(where string, u *ComponentUpgrades, t *Transition) []string {
	var v []string
	ps := passages(u, t)
	from, ferr := parseRange(t.From)
	for _, p := range ps {
		if ferr != nil || strictness(t.Verdict) < 0 || !skipsVersion(t, p.c.floor.ver) ||
			strictness(p.c.tr.Verdict) <= strictness(t.Verdict) {

			continue
		}
		if sfrom, err := parseRange(p.c.tr.From); err == nil && overlaps(sfrom, from) {
			v = append(v, fmt.Sprintf(
				"%s is %s but skips the %s boundary at %s (transition %d) for sources in that transition's from, "+
					"so narrow from to leave them out",
				where, t.Verdict, p.c.tr.Verdict, p.c.floor.ver, p.index))
		}
	}
	for _, s := range t.Skips {
		sv, err := parseRangeVersion(s, s)
		if err != nil {
			v = append(v, where+" skips an invalid version: "+err.Error())
			continue
		}
		if !slices.ContainsFunc(ps, func(p passage) bool { return p.c.floor.ver.Equal(sv) }) {
			v = append(v, fmt.Sprintf("%s skips %s, which is not the floor of a boundary its moves pass", where, s))
		}
	}
	for _, p := range ps {
		if !p.unclearedBy(t) {
			continue
		}
		if p.stop {
			v = append(v, fmt.Sprintf(
				"%s passes the required stop at %s (transition %d) for sources in both from ranges, "+
					"which no skips entry clears, so narrow from or to so the moves do not pass it",
				where, p.c.floor.ver, p.index))
			continue
		}
		v = append(v, fmt.Sprintf(
			"%s passes the boundary at %s (transition %d) in one step without listing it in skips; "+
				"list it if a cluster taking this transition may skip that boundary's guidance, "+
				"or narrow from or to so the moves do not pass it",
			where, p.c.floor.ver, p.index))
	}
	return v
}

// strictness returns v's position in safe, manual, blocked order, or -1 for a
// verdict that cannot be authored.
func strictness(v Verdict) int {
	return slices.Index([]Verdict{VerdictSafe, VerdictManual, VerdictBlocked}, v)
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
// packages, 245 of them k8s.io/client-go, which is the wrong price for six
// strings. wellformed_test.go carries that import and fails on drift.
//
// localformat is deliberately absent — it is the internal bundle-layout package
// every deployer consumes, not a selectable deployer.
var canonicalDeployers = []string{"argocd", "argocd-helm", "fleet", "flux", "helm", "helmfile", "terraform"}

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
// Only safe is held to the pin. ADR-021 originally held every verdict to it,
// reasoning that an author cannot have read the migration notes for a version
// nobody has released. That reasoning covers safe and only safe: safe is the
// vouching verdict, and vouching past what AICR ships is the false-confidence
// failure the ADR exists to prevent. manual and blocked are warnings carrying
// instructions, and holding those to the pin forced the wrong order of work —
// bump first, document after — when the order that actually qualifies an
// upgrade is to read the migration notes, write the record, then bump. A
// component AICR deliberately holds below a known-breaking release is the case
// that exposed this: the record most worth having described the release AICR
// would not ship, so the rule forbade exactly the guidance operators needed.
// Over-warning is the safe direction; a manual or blocked record reaching
// forward can never read as a pass.
//
// The obligation stays self-renewing without this: rule 3 fails any bump that
// leaves a from gap below the new pin, which is what forces a record back open.
//
// The non-comparable-pin failure is an addition to ADR-021 rather than a
// transcription of it. The ADR assigns such a pin the unversioned verdict at
// check time and does not make it an authoring error; failing closed here means
// a record cannot make version claims nobody can verify. Relaxing this later is
// the cheap direction if it proves wrong.
//
// When limited, a manual or blocked `to` may omit its ceiling.
func checkPinCeiling(where string, t *Transition, pin string, limited bool) []string {
	b, err := parseRange(t.To)
	if err != nil {
		// Not "already reported by the loader": Validate is exported on an
		// exported map type, so a Set can be built without ever going
		// through Load. Skipping here would fail open on rules 2, 3 and 7.
		return []string{where + " has an unparseable to range: " + err.Error()}
	}
	var v []string
	if b.upper.unbounded && (!limited || t.Verdict == VerdictSafe) {
		v = append(v, where+" has a to range with no upper bound; a record must name the ceiling of the block it describes")
	}
	if b.lower.unbounded {
		v = append(v, where+" has a to range with no lower bound; without one the record would apply to every target version")
	}
	// parseRange accepts an empty or contradictory interval (e.g.
	// ">=0.30.0 <=0.19.0") because such a versionRange is harmless. It
	// matches nothing. The ceiling check below only ever
	// compares b.upper against the pin, so an inverted to would sail through
	// it and validate a boundary at a version the record never actually
	// reaches. Both sides must be bounded to ask the question at all.
	if !b.lower.unbounded && !b.upper.unbounded {
		cmp := b.lower.comparedAt().Compare(b.upper.comparedAt())
		if cmp > 0 || (cmp == 0 && (!b.lower.inclusive || !b.upper.inclusive)) {
			return []string{fmt.Sprintf(
				"%s has a to range %q that matches no version: the lower bound %s is not below the upper bound %s",
				where, t.To, b.lower.ver, b.upper.ver)}
		}
	}
	if b.upper.unbounded {
		return v
	}
	// Everything below compares the ceiling against the pin, which only a
	// vouching verdict owes an answer to.
	if t.Verdict != VerdictSafe {
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
	if b.upper.comparedAt().Compare(pinVer) > 0 {
		v = append(v, fmt.Sprintf(
			"%s is safe with a to ceiling of %s which reaches past the pinned version %s; widen from backward instead, or say manual or blocked if this is guidance written ahead of the bump",
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
	fb, ferr := parseRange(t.From)
	tb, terr := parseRange(t.To)
	if ferr != nil {
		return []string{where + " has an unparseable from range: " + ferr.Error()}
	}
	if terr != nil {
		return nil // reported by checkPinCeiling
	}
	// parseRange accepts an empty or contradictory interval (e.g.
	// ">=0.20.0 <0.18.0") because such a versionRange is harmless. It
	// matches nothing. Here it would sail through the
	// disjointness check below and produce a record that can never apply, so
	// it is rejected as its own violation.
	if !fb.lower.unbounded && !fb.upper.unbounded {
		cmp := fb.lower.comparedAt().Compare(fb.upper.comparedAt())
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
	cmp := fb.upper.comparedAt().Compare(tb.lower.comparedAt())
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
// every record is the matcher's to report, not a malformed file. It runs up to
// the pin, which is why a single-transition record is checked too.
func checkCoverage(component string, trs []Transition, pin string) []string {
	if len(trs) == 0 {
		return nil
	}
	intervals := make([]versionRange, 0, len(trs))
	for i := range trs {
		b, err := parseRange(trs[i].From)
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
func checkCoverageReachesPin(component string, cur rangeEnd, pinVer *semver.Version, perr error) []string {
	if cur.unbounded || perr != nil || cur.ver.Compare(pinVer) >= 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"component %q leaves a version gap between %s and the pinned version %s that no record describes; an operator on a version in that range would match no transition",
		component, cur.ver, pinVer)}
}

// checkDistinctMoves implements rule 8, an addition to ADR-021. It returns a
// violation for each pair of u's transitions whose `from` ranges overlap and
// whose `to` ranges overlap, skipping ranges that do not parse.
func checkDistinctMoves(u *ComponentUpgrades) []string {
	type span struct{ from, to versionRange }
	spans := make([]*span, len(u.Transitions))
	for i := range u.Transitions {
		from, ferr := parseRange(u.Transitions[i].From)
		to, terr := targetRange(u, &u.Transitions[i])
		if ferr != nil || terr != nil {
			continue // reported elsewhere
		}
		spans[i] = &span{from: from, to: to}
	}
	var v []string
	for i, a := range spans {
		for j := i + 1; j < len(spans); j++ {
			b := spans[j]
			if a == nil || b == nil || !overlaps(a.from, b.from) || !overlaps(a.to, b.to) {
				continue
			}
			v = append(v, fmt.Sprintf(
				"component %q transitions %d and %d describe the same move: their from ranges overlap and "+
					"so do their to ranges, so a jump between the two overlaps would have two verdicts",
				u.Component, i, j))
		}
	}
	return v
}

// hookDir is where ADR-021 puts a hook manifest. The location is
// load-bearing rather than a convention, but only tools/bom actually covers
// it today: it walks each component's manifests/ tree with
// filepath.WalkDir, which descends into migrations/. The embedded-FS
// image-pin test (recipes/manifest_images_test.go) does not — it walks
// recipes.FS, and recipes/data.go's //go:embed pattern for that tree is
// components/*/manifests/*.yaml, whose `*` does not cross `/`, so a file
// under manifests/migrations/ is never embedded and that test never sees
// it. The first PR to add a real hook manifest must also add
// components/*/manifests/*/*.yaml to recipes/data.go's embed directive, or
// the hook's images stay outside the pin gate; that pattern cannot be added
// here because a //go:embed pattern matching zero files fails to compile,
// and no such file exists yet.
const hookDir = "manifests/migrations/"

// checkHooks implements rule 9, an addition to ADR-021. Hooks are the
// deliberate exception that lets a safe verdict carry work, so an unvalidated
// phase silently doing nothing is worse than a rejected record. file is gated
// with filepath.IsLocal rather than a substring scan for "..", per CLAUDE.md.
// The tree check runs against the cleaned, slash-normalized path rather than
// h.File itself: IsLocal alone accepts a ".." that stays under the bundle
// root while still walking out of hookDir (e.g.
// "manifests/migrations/../../values.yaml"), which a raw-string prefix test
// would miss.
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
		case !strings.HasPrefix(filepath.ToSlash(filepath.Clean(h.File)), hookDir):
			v = append(v, fmt.Sprintf(
				"%s hook %d file %q must live under %s, the tree tools/bom walks",
				where, i, h.File, hookDir))
		}
	}
	return v
}

// checkAffectedResources implements rule 10, returning a violation for each
// core Secret kind in t's affectedResources. A kind matches case-insensitively,
// ignoring surrounding space.
func checkAffectedResources(where string, t *Transition) []string {
	// Listing Secrets needs a permission that reads every Secret's data.
	var v []string
	for i, a := range t.AffectedResources {
		if strings.TrimSpace(a.Group) != "" {
			continue
		}
		for _, kind := range a.Kinds {
			// Discovery maps a lowercased kind to the same resource.
			if strings.EqualFold(strings.TrimSpace(kind), "Secret") {
				v = append(v, fmt.Sprintf(
					"%s affectedResources %d names core Secret, which the at-risk scan may not list "+
						"because the permission to list Secrets reads every Secret's data", where, i))
			}
		}
	}
	return v
}
