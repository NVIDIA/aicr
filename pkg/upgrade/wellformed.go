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
	}
	return v
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
		// Computed, never authored; Load rejects a record carrying either
		// before it can reach a Set for Validate to see.
	}
	if t.Reversible != nil && t.ReversibleNotes == "" {
		v = append(v, where+" sets reversible but carries no reversibleNotes; renderers never surface the flag alone")
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
