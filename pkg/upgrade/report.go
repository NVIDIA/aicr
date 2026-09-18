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
	"strings"
)

// ReportOptions labels a report with the artifacts it was computed from and
// names the deployer whose steps are rendered.
type ReportOptions struct {
	From     string
	To       string
	Deployer string
}

// Report is the presentable form of a match: one row per component whose
// deployment changes, with every step list already narrowed to one deployer.
//
// It is a projection of []ComponentResult rather than an alias for it. A result
// points into the Set and inherits its read-only contract; a report owns
// everything it carries, renders each semver distance as the phrase a reader
// sees, and names its fields for JSON and YAML consumers.
type Report struct {
	From       string            `json:"from,omitempty" yaml:"from,omitempty"`
	To         string            `json:"to,omitempty" yaml:"to,omitempty"`
	Deployer   string            `json:"deployer,omitempty" yaml:"deployer,omitempty"`
	Components []ReportComponent `json:"components" yaml:"components"`
	Summary    ReportSummary     `json:"summary" yaml:"summary"`
}

// ReportComponent is one row.
type ReportComponent struct {
	Component string     `json:"component" yaml:"component"`
	Change    ChangeKind `json:"change" yaml:"change"`

	// From is the source version, or on a replaced row the departing
	// component's name: two different pieces of software share no version
	// line to compare. Empty on an added row, as To is on a removed one.
	From string `json:"from,omitempty" yaml:"from,omitempty"`
	To   string `json:"to,omitempty" yaml:"to,omitempty"`

	// Verdict is empty on an added or removed row, which made no transition.
	Verdict Verdict `json:"verdict,omitempty" yaml:"verdict,omitempty"`

	// Notes is the one-line justification the table's last column shows.
	Notes string `json:"notes,omitempty" yaml:"notes,omitempty"`

	// Reason is the stable code for why Verdict is what it is, and Explanation
	// the sentence an operator reads. Both are empty exactly when Verdict is.
	Reason      Reason `json:"reason,omitempty" yaml:"reason,omitempty"`
	Explanation string `json:"explanation,omitempty" yaml:"explanation,omitempty"`

	// Jump is the distance between the two versions compared, and Covers the
	// wider distance the matched record's claim reaches over, both as phrases
	// ("1 patch", "11 minors"). Covers is empty when no single record matched
	// or the record names no ceiling.
	Jump   string `json:"jump,omitempty" yaml:"jump,omitempty"`
	Covers string `json:"covers,omitempty" yaml:"covers,omitempty"`

	Breaking  bool `json:"breaking" yaml:"breaking"`
	Downgrade bool `json:"downgrade" yaml:"downgrade"`

	// StoppedAt is the interval a blocked row must not enter in one step. Set
	// on every blocked version row, and empty on a blocked replaced row, which
	// joins two pieces of software rather than two versions of one. Such a row
	// carries a Summary, Precondition and Steps only when one record describes
	// the whole jump: rendering one record's instructions for a jump it does
	// not describe is the thing the verdict exists to prevent.
	StoppedAt string `json:"stoppedAt,omitempty" yaml:"stoppedAt,omitempty"`

	Summary      string `json:"summary,omitempty" yaml:"summary,omitempty"`
	Precondition string `json:"precondition,omitempty" yaml:"precondition,omitempty"`

	// Steps are the selected deployer's steps only. Empty when no deployer was
	// named, when the verdict needs none, when no single record describes the
	// jump, or when the record's groups do not cover the named deployer.
	Steps []ReportStep `json:"steps,omitempty" yaml:"steps,omitempty"`

	// FailsRun mirrors ComponentResult.FailsRun so a consumer reading only the
	// report reaches the same conclusion the exit code did.
	FailsRun bool `json:"failsRun" yaml:"failsRun"`
}

// ReportStep is one operator action, restated with JSON names.
type ReportStep struct {
	ID          string `json:"id" yaml:"id"`
	Description string `json:"description" yaml:"description"`
	Reason      string `json:"reason,omitempty" yaml:"reason,omitempty"`
}

// ReportSummary is the aggregate a pipeline reads instead of the rows.
type ReportSummary struct {
	// Components counts the rows, which is the number of components whose
	// deployment changes rather than the size of either table.
	Components int `json:"components" yaml:"components"`
	// Failing counts the rows whose verdict stops a strict run.
	Failing int `json:"failing" yaml:"failing"`
}

// FailsRun reports whether any row stops a strict run.
func (r *Report) FailsRun() bool {
	return r != nil && r.Summary.Failing > 0
}

// RequiresDeployer reports whether rendering these results would print steps,
// and therefore whether a deployer must be named.
//
// A blocked row counts only when one record describes the whole jump: the
// blocked results computed from several records, or from none that names the
// operator's starting point, deliberately render no steps, so demanding a
// deployer for one would reject a report over a flag nothing would consume.
//
// ADR-021 Decision 5 would infer the deployer from a `--to` bundle, but no
// bundle artifact records which deployer built it: the bundler writes
// provenance.yaml only under --vendor-charts, and it describes vendored charts
// rather than the deployer. Until a bundle carries that fact (NVIDIA/aicr#2767),
// every caller has to supply it, and the alternative of rendering every
// deployer's path is the failure deployer-scoping exists to prevent.
func RequiresDeployer(results []ComponentResult) bool {
	for _, r := range results {
		if r.Verdict == VerdictManual {
			return true
		}
		// A replacement carries its guidance on Replaces rather than
		// Transition, and checkReplaces admits a blocked one with required
		// step groups, so keying on Transition alone would let a blocked
		// replacement through with no deployer and then render nothing.
		if r.Verdict == VerdictBlocked && (r.Transition != nil || r.Replaces != nil) {
			return true
		}
	}
	return false
}

// NewReport projects match results into a report.
//
// It is pure and copies everything it reads, so the returned Report can outlive
// the Set the results point into.
func NewReport(results []ComponentResult, opts ReportOptions) *Report {
	rep := &Report{
		From:       opts.From,
		To:         opts.To,
		Deployer:   opts.Deployer,
		Components: make([]ReportComponent, 0, len(results)),
	}
	for _, r := range results {
		rep.Components = append(rep.Components, reportComponent(r, opts.Deployer))
	}
	rep.Summary.Components = len(rep.Components)
	for _, c := range rep.Components {
		if c.FailsRun {
			rep.Summary.Failing++
		}
	}
	return rep
}

func reportComponent(r ComponentResult, deployer string) ReportComponent {
	c := ReportComponent{
		Component:   r.Component,
		Change:      r.Change,
		From:        r.From,
		To:          r.To,
		Verdict:     r.Verdict,
		Jump:        spanPhrase(r.Jump),
		Covers:      spanPhrase(r.Span),
		Breaking:    r.Breaking,
		Downgrade:   r.Downgrade,
		StoppedAt:   r.StoppedAt,
		Reason:      r.Reason,
		Explanation: r.Explanation,
		FailsRun:    r.FailsRun(),
	}
	if r.Change == ChangeReplaced {
		c.From = r.ReplacedComponent
	}
	switch {
	case r.Replaces != nil:
		c.Summary = r.Replaces.Summary
		c.Steps = reportSteps(stepsFor(r.Replaces.StepsByDeployer, deployer))
	case r.Transition != nil:
		c.Summary = r.Transition.Summary
		c.Precondition = r.Transition.Precondition
		c.Steps = reportSteps(stepsFor(r.Transition.StepsByDeployer, deployer))
	}
	c.Notes = notes(r, c)
	return c
}

// stepsFor selects the group covering deployer: the one naming it, else the
// remainder group that omits deployers entirely. It mirrors checkStepGroups'
// partition, including that `deployers: []` is not the remainder: an explicit
// empty list claims nothing.
//
// An unnamed deployer selects nothing. There is no neutral group to fall back
// on: a remainder group is still one deployer's instructions, and handing it to
// a caller who named none is the guess the ADR forbids.
func stepsFor(groups []StepGroup, deployer string) []Step {
	if deployer == "" {
		return nil
	}
	var remainder []Step
	for _, g := range groups {
		if g.Deployers == nil {
			remainder = g.Steps
			continue
		}
		for _, d := range g.Deployers {
			if d == deployer {
				return g.Steps
			}
		}
	}
	return remainder
}

func reportSteps(steps []Step) []ReportStep {
	if len(steps) == 0 {
		return nil
	}
	out := make([]ReportStep, len(steps))
	for i, s := range steps {
		// The two shapes differ only in their tags, so the conversion is
		// exact. A field added to Step stops compiling here, which is the
		// point: whether it belongs in the report is a decision, not a
		// default.
		out[i] = ReportStep(s)
	}
	return out
}

// notes composes the table's last column: what changed, then what the verdict
// rests on, then how far the verdict's claim reaches when that is wider than
// the jump itself.
func notes(r ComponentResult, c ReportComponent) string {
	switch r.Change {
	case ChangeAdded:
		return "new component, nothing to do"
	case ChangeRemoved:
		return "stays installed; AICR does not uninstall it"
	case ChangeReplaced:
		parts := []string{"replaces " + r.ReplacedComponent}
		if r.Verdict == VerdictBlocked {
			// The version rows signal a block with "stops at <range>", which a
			// replacement has no boundary to fill in, so it would otherwise
			// read as an ordinary migration.
			parts = append(parts, "not in one step")
		}
		if len(c.Steps) > 0 {
			parts = append(parts, plural(len(c.Steps), "step", "steps"))
		}
		return strings.Join(parts, ", ")
	case ChangeVersion:
		// Composed below: a version change is the only kind whose notes
		// depend on the verdict.
	}

	var parts []string
	switch {
	case r.Downgrade:
		parts = append(parts, "downgrade")
	case c.Jump != "":
		parts = append(parts, c.Jump)
	}
	switch r.Verdict {
	case VerdictSafe:
		if r.Transition != nil && r.Transition.VerifiedBy != "" {
			parts = append(parts, "verified")
		}
	case VerdictManual:
		parts = append(parts, plural(len(c.Steps), "step", "steps"))
	case VerdictBlocked:
		parts = append(parts, "stops at "+r.StoppedAt)
		if len(c.Steps) > 0 {
			parts = append(parts, plural(len(c.Steps), "step", "steps"))
		}
	case VerdictUnknown:
		// The three gaps behind unknown get three cells because they close
		// differently: somebody authoring the first record, widening one that
		// exists, or nothing at all.
		switch {
		case r.Downgrade:
			parts = append(parts, "unassessable")
		case r.Reason == ReasonNoBoundaryCrossed:
			parts = append(parts, "record exists, no boundary here")
		default:
			parts = append(parts, "no record")
		}
		if r.Breaking && !r.Downgrade {
			// Descriptive: it sizes the gap for a reader deciding how hard to
			// look, having stopped deciding the exit code. Suppressed on a
			// downgrade, where "unassessable" is the whole story.
			parts = append(parts, "breaking boundary")
		}
	case VerdictUnversioned:
		parts = append(parts, "versions are not comparable")
	}
	if c.Covers != "" && r.Span != r.Jump {
		parts = append(parts, fmt.Sprintf("%s across %s", r.Verdict, c.Covers))
	}
	return strings.Join(parts, ", ")
}

// spanPhrase names a semver distance at the one level it is measured on.
func spanPhrase(s Span) string {
	switch {
	case s.Majors > 0:
		return plural(s.Majors, "major", "majors")
	case s.Minors > 0:
		return plural(s.Minors, "minor", "minors")
	case s.Patches > 0:
		return plural(s.Patches, "patch", "patches")
	}
	return ""
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
