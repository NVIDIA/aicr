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

package cncf

import (
	"fmt"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// RequirementStatus is the aggregate verdict of one checklist requirement,
// computed fail-closed from every check expected to evidence it.
type RequirementStatus string

// Requirement verdicts. Only StatusPassed is positive evidence: it needs a
// complete proof path, every check present and passed. StatusFailed records a
// disproven capability, StatusNotTested an unmet test prerequisite (a check
// skipped, crashed, pending or never run), and StatusNotApplicable a
// justification from configuration, never from a check.
const (
	StatusPassed        RequirementStatus = "passed"
	StatusFailed        RequirementStatus = "failed"
	StatusNotTested     RequirementStatus = "not tested"
	StatusNotApplicable RequirementStatus = "not applicable"
)

// extraKeySkipReason is the validators' skipReason CTRF extra key, carried
// as the NOT TESTED reason of a skipped check.
const extraKeySkipReason = "skipReason"

// conditionalMusts are the MUST requirements upstream allows a product to
// declare not applicable with a justification. Every other requirement is
// unconditional: a SHOULD a product does not claim renders Not Implemented,
// never N/A.
var conditionalMusts = map[string]bool{
	"cluster_autoscaling": true,
}

// alternativeProofs lists, per requirement, the proof paths that each
// independently evidence it. A path is a conjunction of checks; the
// requirement passes when any one path is complete. A requirement absent here
// has a single path made of all its expected checks.
var alternativeProofs = map[string][][]string{
	// secure-accelerator-access evidences ordinary Kubernetes recipes and
	// deliberately skips on Slinky Slurm recipes, where
	// slinky-slurm-gpu-access evidences the Slurm access path instead.
	"secure_accelerator_access": {{checkSecureAccess}, {checkSlinkySlurmGPUAccess}},
}

// CheckResult is one expected check's contribution to a requirement.
type CheckResult struct {
	// Name is the conformance check name.
	Name string

	// Present is false when the report carries no result for the check.
	Present bool

	// Status is the CTRF status of the result, empty when not Present.
	Status string

	// Reason is the skip reason code (extra.skipReason) of a skipped result,
	// or the message of a failed one; empty otherwise.
	Reason string
}

// RequirementResult is the aggregate verdict of one requirement with the
// evidence behind it.
type RequirementResult struct {
	// ID is the upstream requirement identifier.
	ID string

	// Level is the requirement's level at the aggregated checklist version;
	// empty for supporting evidence (a requirement the version does not list).
	Level Level

	// Status is the verdict.
	Status RequirementStatus

	// Reason explains a verdict other than StatusPassed: the N/A
	// justification, the failing checks, or the unmet prerequisites.
	Reason string

	// Proof names the checks whose passed results establish StatusPassed,
	// the complete proof path that was taken. Nil otherwise.
	Proof []string

	// Checks are every check expected to evidence the requirement, in page
	// order, with the result the report carried for each. A requirement AICR
	// has no check for has none.
	Checks []CheckResult

	// Page is the evidence file the requirement renders to, empty when AICR
	// has no check for it.
	Page string
}

// Applicable reports whether the requirement counts toward the run verdict:
// a MUST not declared not applicable.
func (r *RequirementResult) Applicable() bool {
	return r.Level == LevelMust && r.Status != StatusNotApplicable
}

// Aggregate is the requirement status aggregation of one CTRF report against
// one checklist version. It is the single source for the evidence index and
// for submission packaging.
type Aggregate struct {
	// Version is the checklist version aggregated against.
	Version Version

	// Requirements holds every requirement of the checklist, in upstream
	// order.
	Requirements []RequirementResult

	// Supporting holds results for checks that ran but whose requirement
	// Version does not list (dra_support from v1.36 on). Level is empty and
	// they never affect Passed.
	Supporting []RequirementResult
}

// Requirement returns the result for the given upstream id, or nil.
func (a *Aggregate) Requirement(id string) *RequirementResult {
	for i := range a.Requirements {
		if a.Requirements[i].ID == id {
			return &a.Requirements[i]
		}
	}
	return nil
}

// IncompleteMusts returns every applicable MUST whose status is not
// StatusPassed, in checklist order. A SHOULD never appears here, whatever
// its status.
func (a *Aggregate) IncompleteMusts() []RequirementResult {
	var out []RequirementResult
	for _, r := range a.Requirements {
		if r.Applicable() && r.Status != StatusPassed {
			out = append(out, r)
		}
	}
	return out
}

// Passed reports whether every applicable MUST passed. SHOULDs do not
// participate: a SHOULD with no claim renders Not Implemented without failing
// the run.
func (a *Aggregate) Passed() bool {
	return len(a.IncompleteMusts()) == 0
}

// Verdict is the outcome of a submission run: nil when every applicable MUST
// passed, else an error naming each MUST that is not established with its
// status and reason. A submission run derives its exit status from this, not
// from the per-check verdicts, so a SHOULD never fails it. The evidence has
// been written when it is consulted, so the directory still documents what
// was and was not shown.
//
// Errors:
//   - ErrCodeInternal when an applicable MUST requirement is not established.
func (a *Aggregate) Verdict() error {
	incomplete := a.IncompleteMusts()
	if len(incomplete) == 0 {
		return nil
	}
	parts := make([]string, 0, len(incomplete))
	for _, r := range incomplete {
		parts = append(parts, fmt.Sprintf("%s %s (%s)", r.ID, r.Status, r.Reason))
	}
	return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
		"CNCF submission not established: %d applicable MUST requirement(s) of the %s checklist did not pass: %s",
		len(incomplete), a.Version, strings.Join(parts, "; ")))
}

// ChecksFailed reports whether any test in the report carries a failing CTRF
// status (failed or other). It is the per-check rule every validate run
// applies, read from the merged report rather than from phase statuses so
// the SDK and the CLI judge the same document.
func ChecksFailed(report *ctrf.Report) bool {
	if report == nil {
		return false
	}
	for _, t := range report.Results.Tests {
		if ctrf.IsFailingStatus(t.Status) {
			return true
		}
	}
	return false
}

// Outcome is the exit rule of a submission run, shared by the CLI and the
// SDK so the two cannot disagree: a failed or errored check in report fails
// the run (the per-check rule every run applies, which a flag never changes;
// checks with no requirement mapping are visible only here), and Verdict is
// added on top. perCheck false skips the per-check rule, which is what
// `--fail-on-error=false` means; the verdict applies regardless. A nil
// receiver contributes no verdict.
//
// Errors:
//   - ErrCodeInternal when a check failed or errored, when an applicable
//     MUST requirement is not established, or both; the message says which.
func (a *Aggregate) Outcome(report *ctrf.Report, perCheck bool) error {
	var verdict error
	if a != nil {
		verdict = a.Verdict()
	}
	failed := perCheck && ChecksFailed(report)
	switch {
	case failed && verdict != nil:
		return errors.Wrap(errors.ErrCodeInternal,
			"validation failed: one or more checks did not pass, and the CNCF submission is not established", verdict)
	case verdict != nil:
		return verdict
	case failed:
		return errors.New(errors.ErrCodeInternal, "validation failed: one or more checks did not pass")
	}
	return nil
}

// ValidateNotApplicable checks the notApplicable map a caller supplies: each
// key must be a conditional MUST of the checklist at version v (today only
// cluster_autoscaling), and each justification must be non-empty.
//
// Errors:
//   - ErrCodeInvalidRequest when v is unsupported, a key is not a conditional
//     MUST of that version, or a justification is blank.
func ValidateNotApplicable(v Version, notApplicable map[string]string) error {
	cl, err := ChecklistFor(v)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(notApplicable))
	for id := range notApplicable {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		req := cl.Requirement(id)
		switch {
		case req == nil:
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"notApplicable: %q is not a requirement of the %s checklist", id, v))
		case req.Level != LevelMust || !conditionalMusts[id]:
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"notApplicable: %q is not a conditional MUST; only %s may be declared not applicable, "+
					"a SHOULD without a claim renders Not Implemented", id, joinSorted(conditionalMusts)))
		case strings.TrimSpace(notApplicable[id]) == "":
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
				"notApplicable: %q needs a justification", id))
		}
	}
	return nil
}

// AggregateReport computes the requirement status of every requirement of
// the checklist at version v from the report's test results. A nil report
// aggregates as if no check ran.
//
// A requirement passes only when one of its proof paths is complete: every
// check on the path present and passed, and no path failed. Any failed check
// on any path fails the requirement, whatever the other paths did; a skipped,
// pending, other or absent check leaves its path incomplete. The requirement
// is not tested when no path failed and none is complete. notApplicable overrides the
// computed status of the requirements it names with StatusNotApplicable; the
// results of their checks are kept as observations.
//
// Errors:
//   - ErrCodeInvalidRequest when v is unsupported or notApplicable names a
//     requirement that may not be declared not applicable.
func AggregateReport(v Version, report *ctrf.Report, notApplicable map[string]string) (*Aggregate, error) {
	cl, err := ChecklistFor(v)
	if err != nil {
		return nil, err
	}
	if err := ValidateNotApplicable(v, notApplicable); err != nil {
		return nil, err
	}

	results := make(map[string][]ctrf.TestResult)
	if report != nil {
		for _, t := range report.Results.Tests {
			results[t.Name] = append(results[t.Name], t)
		}
	}

	agg := &Aggregate{Version: v}
	for _, req := range cl.Requirements {
		page := pageForRequirement(req.ID)
		r := aggregateRequirement(req.ID, req.Level, page, results)
		if justification, ok := notApplicable[req.ID]; ok {
			r.Status = StatusNotApplicable
			r.Reason = strings.TrimSpace(justification)
			r.Proof = nil
		}
		agg.Requirements = append(agg.Requirements, r)
	}

	// Supporting evidence: pages whose requirement the version does not list,
	// recorded only when one of their checks ran.
	for i := range evidencePages {
		page := &evidencePages[i]
		if cl.Requirement(page.RequirementID) != nil {
			continue
		}
		r := aggregateRequirement(page.RequirementID, "", page, results)
		if anyPresent(r.Checks) {
			agg.Supporting = append(agg.Supporting, r)
		}
	}
	return agg, nil
}

func aggregateRequirement(id string, level Level, page *evidencePage, results map[string][]ctrf.TestResult) RequirementResult {
	r := RequirementResult{ID: id, Level: level}
	if page == nil {
		r.Status = StatusNotTested
		r.Reason = untestedNoCheck
		return r
	}
	r.Page = page.File
	r.Checks = make([]CheckResult, 0, len(page.Checks))
	for _, name := range page.Checks {
		r.Checks = append(r.Checks, checkResult(name, results[name]))
	}
	if !anyPresent(r.Checks) {
		r.Status = StatusNotTested
		r.Reason = untestedNotRun
		return r
	}

	byName := make(map[string]CheckResult, len(r.Checks))
	for _, c := range r.Checks {
		byName[c.Name] = c
	}
	// Every path is judged before the verdict: a failed check on any path is
	// a disproven capability on this cluster, so it fails the requirement
	// even when an alternative path passed. An alternative may be absent or
	// skipped, not failed.
	var proof []string
	var failed, incomplete []string
	for _, path := range proofPaths(id, page.Checks) {
		status, reason := evaluatePath(path, byName)
		switch status {
		case StatusPassed:
			if proof == nil {
				proof = append([]string(nil), path...)
			}
		case StatusFailed:
			failed = append(failed, reason)
		case StatusNotTested, StatusNotApplicable:
			incomplete = append(incomplete, reason)
		}
	}
	switch {
	case len(failed) > 0:
		r.Status = StatusFailed
		r.Reason = strings.Join(failed, "; ")
	case proof != nil:
		r.Status = StatusPassed
		r.Proof = proof
	default:
		r.Status = StatusNotTested
		r.Reason = strings.Join(incomplete, "; ")
	}
	return r
}

// evaluatePath judges one proof path: failed when any check on it failed,
// passed when every check is present and passed, not tested otherwise. The
// reason names the checks that kept it from passing.
func evaluatePath(path []string, byName map[string]CheckResult) (RequirementStatus, string) {
	var failed, incomplete []string
	for _, name := range path {
		c := byName[name]
		switch {
		case !c.Present:
			incomplete = append(incomplete, name+" not run")
		case c.Status == ctrf.StatusPassed:
		case c.Status == ctrf.StatusFailed:
			failed = append(failed, name+" failed")
		case c.Status == ctrf.StatusSkipped:
			reason := name + " skipped"
			if c.Reason != "" {
				reason += ": " + c.Reason
			}
			incomplete = append(incomplete, reason)
		default:
			incomplete = append(incomplete, name+" "+c.Status)
		}
	}
	switch {
	case len(failed) > 0:
		return StatusFailed, strings.Join(append(failed, incomplete...), ", ")
	case len(incomplete) > 0:
		return StatusNotTested, strings.Join(incomplete, ", ")
	}
	return StatusPassed, ""
}

// checkResult folds every result the report carries for one check into a
// CheckResult. A check reported more than once (the same check in two merged
// phases) passes only when every result passed; the worst status wins, in
// the order failed, then any non-passed status, then passed.
func checkResult(name string, tests []ctrf.TestResult) CheckResult {
	if len(tests) == 0 {
		return CheckResult{Name: name}
	}
	c := CheckResult{Name: name, Present: true, Status: ctrf.StatusPassed}
	for _, t := range tests {
		switch t.Status {
		case ctrf.StatusPassed:
		case ctrf.StatusFailed:
			c.Status = ctrf.StatusFailed
			c.Reason = t.Message
		default:
			if c.Status != ctrf.StatusFailed {
				c.Status = t.Status
				c.Reason = t.Extra[extraKeySkipReason]
			}
		}
	}
	return c
}

// proofPaths returns the proof paths of a requirement: its alternatives when
// it has any, else the single path of every expected check.
func proofPaths(id string, expected []string) [][]string {
	if paths, ok := alternativeProofs[id]; ok {
		return paths
	}
	return [][]string{expected}
}

func pageForRequirement(id string) *evidencePage {
	for i := range evidencePages {
		if evidencePages[i].RequirementID == id {
			return &evidencePages[i]
		}
	}
	return nil
}

func anyPresent(checks []CheckResult) bool {
	for _, c := range checks {
		if c.Present {
			return true
		}
	}
	return false
}

func joinSorted(set map[string]bool) string {
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return strings.Join(ids, ", ")
}
