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
		violations = append(violations, validateRecord(s[name])...)
	}
	if len(violations) == 0 {
		return nil
	}
	return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
		"%d upgrade record violation(s):\n  - %s",
		len(violations), strings.Join(violations, "\n  - ")))
}

func validateRecord(u *ComponentUpgrades) []string {
	v := make([]string, 0, len(u.Transitions))
	for i := range u.Transitions {
		where := fmt.Sprintf("component %q transition %d", u.Component, i)
		v = append(v, checkVerdictFields(where, &u.Transitions[i])...)
		v = append(v, checkStepGroups(where, &u.Transitions[i])...)
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
