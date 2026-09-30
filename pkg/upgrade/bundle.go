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
	"io"
	"slices"
	"strings"

	"github.com/Masterminds/semver/v3"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// GuideFile is the bundle-root file WriteGuide renders into.
const GuideFile = "UPGRADING.md"

// BundlePin is a component's pinned version as a bundle ships it.
type BundlePin struct {
	Component string
	Version   string
}

// BundleNote is a transition whose `to` range contains the version a bundle
// pins and that asks something of an operator upgrading into it. Transition
// points into the Set it was selected from.
type BundleNote struct {
	Component  string
	Pin        string
	Transition *Transition
}

// BundleNotes selects, in pin order, the manual and blocked transitions whose
// `to` contains each pin.
//
// A bundle knows where it lands and not where the operator starts, so this is
// deliberately not Match: every selected transition is guidance conditional on
// its `from`, which the renderers state. A safe transition asks nothing and is
// left out; a pin that is not semver has no range to fall inside.
func BundleNotes(set Set, pins []BundlePin) []BundleNote {
	var notes []BundleNote
	for _, p := range pins {
		u := set[p.Component]
		if u == nil {
			continue
		}
		v, err := semver.NewVersion(p.Version)
		if err != nil {
			continue
		}
		for i := range u.Transitions {
			tr := &u.Transitions[i]
			if tr.Verdict != VerdictManual && tr.Verdict != VerdictBlocked {
				continue
			}
			b, err := parseBounds(tr.To)
			if err != nil || !b.contains(v) {
				continue
			}
			notes = append(notes, BundleNote{Component: p.Component, Pin: p.Version, Transition: tr})
		}
	}
	return notes
}

// WriteGuide renders the notes as the Markdown guide a bundle ships at
// GuideFile, with every step list narrowed to deployer. No notes writes
// nothing, so a caller that must not ship an empty file checks len(notes)
// before creating one.
func WriteGuide(w io.Writer, notes []BundleNote, deployer string) error {
	if err := checkRenderCall(w, deployer); err != nil {
		return err
	}
	if len(notes) == 0 {
		return nil
	}

	ew := &errWriter{w: w}
	ew.println("# Before You Upgrade")
	ew.println("")
	ew.printf("This bundle was generated for the `%s` deployer. The steps below apply only\n", mdInline(deployer))
	ew.println("when this bundle is applied over an existing installation of an earlier version.")
	ew.println("A fresh install can skip this file.")
	ew.println("")
	ew.println("Each entry covers only the boundary this bundle's pinned version lands in. To")
	ew.println("check your exact move, including any earlier boundary you would cross, run")
	ew.println("`aicr upgrade-check` against the recipe you deployed from.")
	for _, n := range notes {
		writeGuideSection(ew, n, deployer)
	}
	return wrapBundleErr(ew.err)
}

func writeGuideSection(ew *errWriter, n BundleNote, deployer string) {
	tr := n.Transition
	steps := stepsFor(tr.StepsByDeployer, deployer)

	ew.println("")
	ew.printf("## %s: %s\n", mdInline(n.Component), mdInline(string(tr.Verdict)))
	ew.println("")
	ew.printf("This bundle pins `%s`. Applies when upgrading from `%s` into `%s`.\n",
		mdInline(n.Pin), mdInline(tr.From), mdInline(tr.To))
	if tr.Summary != "" {
		ew.println("")
		ew.println(mdInline(tr.Summary))
	}
	if tr.Precondition != "" {
		ew.println("")
		ew.println("**Before you start:** " + mdInline(tr.Precondition))
	}
	if tr.Reversible != nil {
		// Notes print whenever present: they carry the caveat a bare yes or no
		// drops. A bare no is legal; Validate forbids only a bare yes.
		line := "**Reversible:** no."
		if *tr.Reversible {
			line = "**Reversible:** yes."
		}
		if tr.ReversibleNotes != "" {
			line += " " + mdInline(tr.ReversibleNotes)
		}
		ew.println("")
		ew.println(line)
	}
	if tr.Verdict == VerdictBlocked {
		// No intermediate version is named: under rule 2 the stopping point is
		// this record's own `to`, and its steps are how to reach it. The
		// version just below the floor is still inside `from`, so naming it
		// would be circular.
		line := fmt.Sprintf("**Blocked:** do not upgrade from `%s` into `%s` in one step.",
			mdInline(tr.From), mdInline(tr.To))
		if len(steps) > 0 {
			line += " Follow the steps below to get there safely."
		}
		ew.println("")
		ew.println(line)
	}

	ew.println("")
	if len(steps) == 0 {
		// A manual or blocked verdict says action is required, so the absence
		// is stated rather than read as nothing to do.
		where := "see the references below."
		if len(tr.References) == 0 {
			where = "see the component's upstream release notes."
		}
		ew.printf("No steps are recorded for the `%s` deployer; %s\n", mdInline(deployer), where)
	} else {
		ew.printf("**Steps for %s:**\n", mdInline(deployer))
		ew.println("")
		for i, s := range steps {
			marker := fmt.Sprintf("%d. ", i+1)
			line := fmt.Sprintf("%s`%s`: %s", marker, mdInline(s.ID), mdInline(s.Description))
			if s.Reason == "" {
				ew.println(line)
				continue
			}
			// The trailing backslash is a CommonMark hard break. Without it the
			// Why line is a lazy continuation and renders joined to the
			// description.
			ew.println(line + `\`)
			ew.printf("%s*Why:* %s\n", strings.Repeat(" ", len(marker)), mdInline(s.Reason))
		}
	}

	if len(tr.References) > 0 {
		ew.println("")
		ew.println("**References:**")
		ew.println("")
		for _, r := range tr.References {
			ew.println("- " + mdInline(r))
		}
	}
}

// WriteNotice renders the section a deployer README carries to point at
// GuideFile. It ends in a blank line so the heading that follows it stays
// separated. No notes writes nothing.
func WriteNotice(w io.Writer, notes []BundleNote, deployer string) error {
	if err := checkRenderCall(w, deployer); err != nil {
		return err
	}
	if len(notes) == 0 {
		return nil
	}

	ew := &errWriter{w: w}
	ew.println("## Before You Upgrade")
	ew.println("")
	ew.println("Upgrading an existing installation? The components below need action before")
	ew.printf("this bundle is applied over them. [%s](%s) has the steps\n", GuideFile, GuideFile)
	ew.printf("for the `%s` deployer. A fresh install can skip this.\n", mdInline(deployer))
	ew.println("")
	ew.println("| Component | Verdict | Applies when upgrading from |")
	ew.println("|-----------|---------|-----------------------------|")
	for _, n := range notes {
		ew.printf("| %s | %s | `%s` |\n",
			mdCell(n.Component), mdCell(string(n.Transition.Verdict)), mdCell(n.Transition.From))
	}
	ew.println("")
	return wrapBundleErr(ew.err)
}

// NoteLines returns one line per note for a CLI to print after writing a
// bundle, or nil when there are none.
func NoteLines(notes []BundleNote) []string {
	if len(notes) == 0 {
		return nil
	}
	lines := make([]string, 0, len(notes))
	for _, n := range notes {
		what := "requires manual steps when upgrading from " + mdInline(n.Transition.From)
		if n.Transition.Verdict == VerdictBlocked {
			what = "cannot be upgraded from " + mdInline(n.Transition.From) + " in one step"
		}
		lines = append(lines, fmt.Sprintf("%s %s; read %s before applying this bundle over an existing installation",
			mdInline(n.Component), what, GuideFile))
	}
	return lines
}

// checkRenderCall rejects a call that would panic, or that names a deployer no
// record can: stepsFor selects nothing for one, so every section would read as
// having no steps.
func checkRenderCall(w io.Writer, deployer string) error {
	if w == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "upgrade notes writer is required (got nil)")
	}
	if !slices.Contains(canonicalDeployers, deployer) {
		return errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("upgrade notes deployer %q is not one of %v", deployer, canonicalDeployers))
	}
	return nil
}

// mdInline renders record prose as one Markdown line. Record prose is folded
// YAML, so its line breaks carry no meaning, and one left in would end a list
// item or table row early; safe then escapes what control characters remain.
func mdInline(s string) string {
	return safe(strings.Join(strings.Fields(s), " "))
}

// mdCell is mdInline for a Markdown table cell, where a bare pipe starts a new
// column.
func mdCell(s string) string {
	return strings.ReplaceAll(mdInline(s), "|", `\|`)
}

func wrapBundleErr(err error) error {
	if err == nil {
		return nil
	}
	return errors.Wrap(errors.ErrCodeInternal, "failed to write bundle upgrade notes", err)
}
