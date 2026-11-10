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
	v1 "github.com/NVIDIA/aicr/pkg/validator/v1"
)

// RequirementStatus is the aggregate verdict of one checklist requirement,
// computed fail-closed from every check expected to evidence it.
type RequirementStatus string

// Requirement verdicts. Only StatusPassed is positive evidence: it needs a
// complete proof path, every check present and passed as a demonstration.
// StatusObserved records a path whose checks passed but at least one marked
// its result observation-only (extraKeyObservationOnly): the check recorded
// the cluster's arrangement without driving a behavioral test, so the
// requirement is not demonstrated and a MUST is not established by it.
// StatusFailed records a disproven capability, StatusNotTested an unmet test
// prerequisite (a check skipped, crashed, pending or never run), and
// StatusNotApplicable a justification from configuration, never from a check.
const (
	StatusPassed        RequirementStatus = "passed"
	StatusObserved      RequirementStatus = "observed"
	StatusFailed        RequirementStatus = "failed"
	StatusNotTested     RequirementStatus = "not tested"
	StatusNotApplicable RequirementStatus = "not applicable"
)

// CTRF extra keys the aggregation reads, the shared definitions of
// pkg/validator/ctrf that the validators publish under the same names.
const (
	// extraKeySkipReason is the skipReason code, carried as the NOT TESTED
	// reason of a skipped check.
	extraKeySkipReason = ctrf.ExtraKeySkipReason

	// extraKeyObservationOnly marks a passed result as observation-only; the
	// value is extraValueTrue.
	extraKeyObservationOnly = ctrf.ExtraKeyObservationOnly

	// extraKeyResultSchema is the result-contract version the validator
	// image stamped on the result; it tells the Reason of an unmarked pass
	// of a check known to be observation-only (v1.IsObservationOnlyCheck).
	extraKeyResultSchema = ctrf.ExtraKeyResultSchema

	// extraValueTrue is the published form of a true boolean extra.
	extraValueTrue = ctrf.ExtraValueTrue

	// CheckResult.Reason of an observation-only check whose pass carries no
	// observationOnly marker, by what the result-contract stamp says.
	reasonResultSchemaAbsent  = "result contract marker absent, so the pass is read as an observation"
	reasonMarkerDroppedPrefix = "observation-only marker absent from a result stamped with contract version "
	reasonSchemaUnknownPrefix = "result contract version unknown to this reader: "
)

// observationOnlyReason explains why an unmarked pass of a check known to be
// observation-only is read as an observation rather than a demonstration,
// from the result-contract stamp it carries: none (the image predates the
// marker), the known version (the marker line was dropped in transit) or a
// version this reader does not know (its markers may differ).
func observationOnlyReason(stamp string) string {
	switch stamp {
	case "":
		return reasonResultSchemaAbsent
	case ctrf.ResultSchemaVersion:
		return reasonMarkerDroppedPrefix + stamp
	default:
		return reasonSchemaUnknownPrefix + stamp
	}
}

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
	// the message of a failed one, or the contract mismatch of a passed
	// result the aggregation read as an observation (see ObservationOnly);
	// empty otherwise.
	Reason string

	// ObservationOnly is true when a passed result carries the
	// observation-only marker (extra.observationOnly): the check recorded
	// what it saw and drove no behavioral test, so its pass demonstrates
	// nothing. It is also true, with Reason saying so, for every unmarked
	// pass of a check known to be observation-only (v1.IsObservationOnlyCheck),
	// whatever its result-contract stamp (extra.resultSchema) says: the stamp
	// only decides whether Reason reports an image that predates the marker
	// or a marker that went missing. Always false for a result that did not
	// pass.
	ObservationOnly bool
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
	// justification, the failing checks, the observation-only checks, or
	// the unmet prerequisites.
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

	// Documented is the vendor's manual attestation of a requirement the run
	// only observed (StatusObserved): the public documentation PRODUCT.yaml
	// cites instead of the testcase. Nil otherwise; it is never set on a
	// passed, failed, not-tested or not-applicable requirement, whatever
	// the configuration declared, since documentation attests only what a
	// check observed and did not disprove.
	Documented *Documentation
}

// Applicable reports whether the requirement counts toward the run verdict:
// a MUST not declared not applicable.
func (r *RequirementResult) Applicable() bool {
	return r.Level == LevelMust && r.Status != StatusNotApplicable
}

// Established reports whether the requirement is shown: demonstrated by a
// complete proof path (StatusPassed), or observed by a check and attested
// by vendor documentation (StatusObserved with Documented). PRODUCT.yaml
// renders an established requirement Implemented.
func (r *RequirementResult) Established() bool {
	return r.Status == StatusPassed || (r.Status == StatusObserved && r.Documented != nil)
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

// IncompleteMusts returns every applicable MUST that is not Established, in
// checklist order: failed, not tested, and observed without a documented
// attestation alike, since none of them shows the requirement. A SHOULD
// never appears here, whatever its status.
func (a *Aggregate) IncompleteMusts() []RequirementResult {
	var out []RequirementResult
	for _, r := range a.Requirements {
		if r.Applicable() && !r.Established() {
			out = append(out, r)
		}
	}
	return out
}

// Passed reports whether every applicable MUST is established: demonstrated
// by a complete proof path, or observed and attested by vendor documentation
// (RequirementResult.Established). SHOULDs do not participate: a SHOULD with
// no claim renders Not Implemented without failing the run.
func (a *Aggregate) Passed() bool {
	return len(a.IncompleteMusts()) == 0
}

// Verdict is the requirement half of a submission run's exit status: nil when
// every applicable MUST is established, else an error naming each MUST that
// is not, with its status and reason. Outcome adds it to the per-check
// rule; on its own it never fails on a SHOULD. The evidence has been written
// when it is consulted, so the directory still documents what was and was
// not shown.
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
		"CNCF submission not established: %d applicable MUST requirement(s) of the %s checklist are not established: %s",
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

// AggregateOptions are the configuration inputs of an aggregation: the
// validate config keys spec.validate.evidence.cncf.notApplicable and
// spec.validate.evidence.cncf.documented.
type AggregateOptions struct {
	// NotApplicable maps a conditional MUST id to the justification that
	// renders it not applicable; see ValidateNotApplicable.
	NotApplicable map[string]string

	// Documented maps a requirement id to the vendor documentation that
	// attests it when the run only observed it; see ValidateDocumented. An
	// entry for a requirement in any other status is ignored, and the
	// aggregate's Reason says so for a MUST that stays incomplete.
	Documented map[string]Documentation
}

// AggregateReport is AggregateSubmission with only NotApplicable set; see
// there for the contract.
func AggregateReport(v Version, report *ctrf.Report, notApplicable map[string]string) (*Aggregate, error) {
	return AggregateSubmission(v, report, AggregateOptions{NotApplicable: notApplicable})
}

// AggregateSubmission computes the requirement status of every requirement
// of the checklist at version v from the report's test results. A nil
// report aggregates as if no check ran.
//
// A requirement passes only when one of its proof paths is complete: every
// check on the path present and passed as a demonstration, and no path
// failed. Any failed check on any path fails the requirement, whatever the
// other paths did; a skipped, pending, other or absent check leaves its path
// incomplete. A path whose checks all passed but one of them marked its
// result observation-only is observed, not complete: the requirement is
// observed when no path failed and none is complete, and not tested when no
// path is observed either. opts.NotApplicable overrides the computed status
// of the requirements it names with StatusNotApplicable; the results of
// their checks are kept as observations. opts.Documented attaches the
// vendor's documentation to a requirement whose computed status is observed,
// which establishes it (RequirementResult.Established) without changing the
// status.
//
// Errors:
//   - ErrCodeInvalidRequest when v is unsupported, opts.NotApplicable names
//     a requirement that may not be declared not applicable, or
//     opts.Documented is malformed (ValidateDocumented).
func AggregateSubmission(v Version, report *ctrf.Report, opts AggregateOptions) (*Aggregate, error) {
	cl, err := ChecklistFor(v)
	if err != nil {
		return nil, err
	}
	notApplicable := opts.NotApplicable
	if err := ValidateNotApplicable(v, notApplicable); err != nil {
		return nil, err
	}
	if err := ValidateDocumented(v, opts.Documented, notApplicable); err != nil {
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
		if doc, ok := opts.Documented[req.ID]; ok {
			if r.Status == StatusObserved {
				d := Documentation{Evidence: append([]string(nil), doc.Evidence...), Notes: strings.TrimSpace(doc.Notes)}
				r.Documented = &d
			} else if r.Applicable() && !r.Established() {
				r.Reason += "; documented attestation ignored: it applies only to an observed requirement"
			}
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
	// even when an alternative path passed. An alternative may be absent,
	// skipped or observed, not failed.
	var proof []string
	var failed, observed, incomplete []string
	for _, path := range proofPaths(id, page.Checks) {
		status, reason := evaluatePath(path, byName)
		switch status {
		case StatusPassed:
			if proof == nil {
				proof = append([]string(nil), path...)
			}
		case StatusFailed:
			failed = append(failed, reason)
		case StatusObserved:
			observed = append(observed, reason)
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
	case len(observed) > 0:
		r.Status = StatusObserved
		r.Reason = strings.Join(append(observed, incomplete...), "; ")
	default:
		r.Status = StatusNotTested
		r.Reason = strings.Join(incomplete, "; ")
	}
	return r
}

// evaluatePath judges one proof path: failed when any check on it failed,
// not tested when any check is absent, skipped, pending or other, observed
// when every check passed but one of them only as an observation, and
// passed otherwise. The reason names the checks that kept it from passing.
func evaluatePath(path []string, byName map[string]CheckResult) (RequirementStatus, string) {
	var failed, observed, incomplete []string
	for _, name := range path {
		c := byName[name]
		switch {
		case !c.Present:
			incomplete = append(incomplete, name+" not run")
		case c.Status == ctrf.StatusPassed && c.ObservationOnly:
			reason := name + " observed, not demonstrated"
			if c.Reason != "" {
				reason += ": " + c.Reason
			}
			observed = append(observed, reason)
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
		return StatusFailed, strings.Join(append(append(failed, observed...), incomplete...), ", ")
	case len(incomplete) > 0:
		return StatusNotTested, strings.Join(append(incomplete, observed...), ", ")
	case len(observed) > 0:
		return StatusObserved, strings.Join(observed, ", ")
	}
	return StatusPassed, ""
}

// checkResult folds every result the report carries for one check into a
// CheckResult. A check reported more than once (the same check in two merged
// phases) passes only when every result passed; the worst status wins, in
// the order failed, then any non-passed status, then passed. A passed
// result marked observation-only marks the fold: one observation among the
// passes keeps the check from demonstrating anything. So does any passed
// result of a check known to be observation-only (v1.IsObservationOnlyCheck)
// that lacks the marker: whether the image predates the marker, dropped the
// marker line, or speaks a contract version this reader does not know, its
// pass is never read as a demonstration, and the Reason says which (#3703).
// A reason set by an earlier skipped or failed result of the same check is
// kept, since the fold's status is that result's.
func checkResult(name string, tests []ctrf.TestResult) CheckResult {
	if len(tests) == 0 {
		return CheckResult{Name: name}
	}
	c := CheckResult{Name: name, Present: true, Status: ctrf.StatusPassed}
	for _, t := range tests {
		switch t.Status {
		case ctrf.StatusPassed:
			switch {
			case t.Extra[extraKeyObservationOnly] == extraValueTrue:
				c.ObservationOnly = true
			case v1.IsObservationOnlyCheck(name):
				c.ObservationOnly = true
				if c.Status == ctrf.StatusPassed {
					c.Reason = observationOnlyReason(t.Extra[extraKeyResultSchema])
				}
			}
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
	if c.Status != ctrf.StatusPassed {
		c.ObservationOnly = false
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
