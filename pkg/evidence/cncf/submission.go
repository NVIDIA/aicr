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
	"context"
	stderrors "errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/cncf/upstreamvalidator"
	"github.com/NVIDIA/aicr/pkg/evidence/redact"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// SubmissionInput is everything WriteSubmission renders from: the run's
// report and the configuration that interprets it.
type SubmissionInput struct {
	// Report is the merged CTRF report of the run; its conformance results
	// are the only source of statuses.
	Report *ctrf.Report

	// Version is the checklist version; zero means LatestVersion.
	Version Version

	// VersionExplicit records that Version was named by the caller
	// (spec.validate.evidence.cncf.conformanceVersion) rather than derived
	// from the cluster's Kubernetes minor; README.md says so beside the
	// checklist, since a submission filed against a chosen checklist must
	// show the choice.
	VersionExplicit bool

	// Options carry notApplicable and documented from the validate config.
	Options AggregateOptions

	// Vendor is spec.validate.evidence.cncf.vendor, required.
	Vendor Vendor

	// Platform is what README.md records about the cluster and recipe.
	Platform Platform

	// Tool is the AICR build, recorded in README.md.
	Tool Tool

	// Now stamps the artifacts; zero uses the wall clock.
	Now time.Time
}

// SubmissionResult is what WriteSubmission produced.
type SubmissionResult struct {
	// Aggregate is the requirement aggregation the directory was rendered
	// from; the caller's exit status is its Outcome.
	Aggregate *Aggregate

	// Complete is true when every applicable MUST is established and the
	// pinned upstream validator accepted the directory, which then lives at
	// Dir, the requested path. When false, Dir is the kept staging copy
	// (<requested>.incomplete/vX.Y/<name>) that documents what was and was
	// not shown; nothing was written to the requested path.
	Complete bool
	Dir      string

	// ValidatorOutput is the upstream validator's report when it ran.
	ValidatorOutput string

	// Redaction lists what the redaction pass changed, as README.md states
	// it.
	Redaction []string
}

// StagedDirContextKey is the key under which an error returned after the
// redaction pass completed carries, in its StructuredError.Context, the
// staged product directory that was kept for inspection; errors before that
// point carry none, since the tree was removed.
const StagedDirContextKey = "stagedDir"

// IncompleteSuffix is appended to the requested directory to name the
// staging tree a submission run writes into. A run that completes moves its
// product directory out of it and removes it; a run that does not leaves it
// in place for inspection.
const IncompleteSuffix = ".incomplete"

// versionSegment matches a path segment the upstream validator would read
// as the checklist version, which it takes from the first such segment of
// the product path.
var versionSegment = regexp.MustCompile(`(?:^|/)v\d+\.\d+/`)

// ValidateSubmissionDir checks the requested directory before a run so the
// refusal costs no check: it must be absent or empty, its staging sibling
// must be absent, and no segment of its absolute path may look like a
// checklist version ("v1.2/"), which the upstream validator would read as
// the version to validate against.
//
// Errors:
//   - ErrCodeInvalidRequest for each condition above.
//   - ErrCodeInternal when the path cannot be resolved or read.
func ValidateSubmissionDir(dir string) error {
	if dir == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "submission directory is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "resolve submission directory", err)
	}
	if versionSegment.MatchString(filepath.ToSlash(filepath.Dir(abs)) + "/") {
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"submission directory %s: a parent path segment of the form vX.Y would be read by the upstream validator as the checklist version; choose another path", abs))
	}
	entries, err := os.ReadDir(abs)
	switch {
	case err == nil && len(entries) > 0:
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"submission directory %s exists and is not empty; the submission is written to a fresh directory", abs))
	case err != nil && !stderrors.Is(err, fs.ErrNotExist):
		return errors.Wrap(errors.ErrCodeInternal, "read submission directory", err)
	}
	if _, err := os.Stat(abs + IncompleteSuffix); err == nil {
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%s exists: an earlier submission run left it, either because an applicable MUST was not established or because the upstream validator did not accept the directory (its README.md Validation section says which); inspect it, then remove it; every run exits 2 while it exists", abs+IncompleteSuffix))
	} else if !stderrors.Is(err, fs.ErrNotExist) {
		return errors.Wrap(errors.ErrCodeInternal, "stat incomplete submission directory", err)
	}
	return nil
}

// WriteSubmission renders the CNCF submission directory for one conformance
// run into dir: PRODUCT.yaml, README.md, junit.xml, e2e.log and the
// evidence pages under evidence/, redacted in one pass, then checked by the
// pinned upstream validator.
//
// The directory is written under a staging tree, <dir>.incomplete, laid out
// as the validator expects (docs/AIConformance-X.Y.yaml beside
// vX.Y/<name>/PRODUCT.yaml). It is moved to dir only when every applicable
// MUST is established and the validator accepts it, and the staging tree is
// then removed. Otherwise nothing reaches dir: when a MUST is not
// established the validator is not run, since the artifacts hold a failure
// it would reject, and the staging tree stays for inspection with the
// result's Dir naming the product directory inside it; when the validator
// fails or cannot run, the error says why and the tree stays as well.
//
// Statuses, evidence references and the export rule are documented on
// buildProductRows and exportTests. The redaction pass (redact.TextScrubber)
// aliases identifiers in every file with one alias table, preserving the
// vendor's own URLs.
//
// Every input refusal below happens before the staging tree is created, so
// a refused run leaves nothing behind.
//
// Errors:
//   - ErrCodeInvalidRequest when in.Report is nil, in.Vendor is malformed,
//     in.Options is malformed for the resolved version, in.Version is
//     unsupported, or dir fails ValidateSubmissionDir.
//   - ErrCodeConflict when the staging tree <dir>.incomplete appeared
//     between the pre-flight and the claim (another run, or a tree left
//     behind); it is never removed by this run.
//   - ErrCodeUnavailable when the upstream validator could not run (it
//     needs the CNCF landscape and every cited URL to answer).
//   - ErrCodeInternal when a file cannot be written or the validator
//     rejected the directory; the message carries the validator's report.
//   - ErrCodeTimeout, or ErrCodeCanceled on cancellation, when ctx ends.
//
// An error returned after the redaction pass completed carries the kept
// product directory in its context under StagedDirContextKey.
func WriteSubmission(ctx context.Context, dir string, in SubmissionInput) (*SubmissionResult, error) {
	if in.Report == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "a CTRF report is required to write a submission")
	}
	if err := in.Vendor.Validate(); err != nil {
		return nil, err
	}
	if err := ValidateSubmissionDir(dir); err != nil {
		return nil, err
	}
	version := in.Version
	if version == "" {
		version = LatestVersion
	}
	cl, err := ChecklistFor(version)
	if err != nil {
		return nil, err
	}
	// Against the resolved version, before the staging tree exists: a
	// refusal here leaves nothing behind to block the next run.
	if err = ValidateNotApplicable(version, in.Options.NotApplicable); err != nil {
		return nil, err
	}
	if err = ValidateDocumented(version, in.Options.Documented, in.Options.NotApplicable); err != nil {
		return nil, err
	}
	if in.Now.IsZero() {
		in.Now = time.Now()
	}
	in.Now = in.Now.UTC()
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "resolve submission directory", err)
	}
	st, err := newStaging(abs, version)
	if err != nil {
		return nil, err
	}
	res, err := st.write(ctx, in, cl)
	if err != nil {
		return nil, err
	}
	if verdict := res.Aggregate.Verdict(); verdict != nil {
		slog.Warn("CNCF submission directory is incomplete and was not validated; the staged copy is kept",
			"dir", st.productDir, "error", verdict)
		return res, nil
	}
	output, err := upstreamvalidator.Run(ctx, filepath.Join(st.productDir, productFile), st.schemaDir)
	res.ValidatorOutput = output
	if err != nil {
		slog.Error("upstream validator did not accept the CNCF submission directory; the staged copy is kept", "dir", st.productDir)
		if rerr := st.writeReadme(validatorFailureState(err)); rerr != nil {
			slog.Warn("could not record the validator outcome in README.md", "error", rerr)
		}
		return nil, st.staged(err)
	}
	if err := st.writeReadme(validationState{Outcome: validationAccepted}); err != nil {
		return nil, st.staged(err)
	}
	if err := st.promote(abs); err != nil {
		return nil, st.staged(err)
	}
	res.Complete, res.Dir = true, abs
	return res, nil
}

// staging is the tree a submission run writes into, laid out as the
// upstream validator reads it: the schema it compares against under docs/,
// the product under vX.Y/<name>/.
type staging struct {
	root        string
	schemaDir   string
	productDir  string
	evidenceDir string
	version     Version

	// readme holds the scrubbed inputs README.md is rendered from, so it can
	// be rewritten once the validator outcome is known.
	readme *readmeInputs
}

func newStaging(abs string, version Version) (*staging, error) {
	st := &staging{root: abs + IncompleteSuffix, version: version}
	st.schemaDir = filepath.Join(st.root, "docs")
	st.productDir = filepath.Join(st.root, string(version), filepath.Base(abs))
	st.evidenceDir = filepath.Join(st.productDir, evidenceDirName)
	// The root is claimed with a plain Mkdir so a tree another run left or
	// is writing is a conflict, never something this run removes on failure.
	if err := os.MkdirAll(filepath.Dir(st.root), 0o755); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "create submission parent directory", err)
	}
	if err := os.Mkdir(st.root, 0o755); err != nil {
		if stderrors.Is(err, fs.ErrExist) {
			return nil, errors.New(errors.ErrCodeConflict, fmt.Sprintf(
				"%s exists: an earlier submission run left its incomplete directory, or another run is writing it; inspect and remove it first", st.root))
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "create submission staging directory", err)
	}
	for _, d := range []string{st.schemaDir, st.evidenceDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			st.discard()
			return nil, errors.Wrap(errors.ErrCodeInternal, "create submission staging directory", err)
		}
	}
	schema, err := schemaFS.ReadFile(schemaPath(version))
	if err != nil {
		st.discard()
		return nil, errors.Wrap(errors.ErrCodeInternal, "embedded CNCF checklist missing: "+schemaPath(version), err)
	}
	if err := writeFile(filepath.Join(st.schemaDir, filepath.Base(schemaPath(version))), schema); err != nil {
		st.discard()
		return nil, err
	}
	return st, nil
}

// discard removes the staging tree after a failure that happened before
// anything worth inspecting was written; a failure to remove it is logged,
// since the original error is what the caller reports.
func (st *staging) discard() {
	if err := os.RemoveAll(st.root); err != nil && !stderrors.Is(err, fs.ErrNotExist) {
		slog.Warn("could not remove the submission staging tree", "dir", st.root, "error", err)
	}
}

// write renders the evidence pages, the artifacts, PRODUCT.yaml and
// README.md into the product directory, redacted with one alias table. The
// pages are scrubbed in place once rendered; the artifacts, PRODUCT.yaml and
// README.md are rendered from scrubbed data, so vendor metadata never passes
// through the scrubber and run-derived text always does. Until the
// redaction pass has completed the staging tree may hold raw check output,
// so an error or cancellation before that point removes the whole tree;
// afterwards it is kept for inspection.
func (st *staging) write(ctx context.Context, in SubmissionInput, cl *Checklist) (res *SubmissionResult, err error) {
	redacted := false
	defer func() {
		if err != nil && !redacted {
			st.discard()
		}
	}()
	r := New(WithOutputDir(st.evidenceDir), WithConformanceVersion(st.version), WithNow(in.Now),
		WithNotApplicable(in.Options.NotApplicable), WithDocumented(in.Options.Documented), WithoutIndex())
	agg, err := r.RenderWithResult(ctx, in.Report)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, errors.WrapCtxErr(err, errors.ErrCodeTimeout, "writing submission")
	}
	pages, err := listPages(st.evidenceDir)
	if err != nil {
		return nil, err
	}
	scrubber := &redact.TextScrubber{Preserve: in.preserved(), NodeNames: in.Platform.NodeNames}
	if err = scrubPages(st.evidenceDir, pages, scrubber); err != nil {
		return nil, err
	}
	exported := scrubExported(exportTests(in.Report, agg), scrubber)
	scrubbedAgg := scrubAggregate(agg, scrubber)
	rows := buildProductRows(cl, scrubbedAgg, pages)
	redacted = true

	junit, err := renderJUnit(exported, in.Now)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "render junit.xml", err)
	}
	product, err := renderProduct(st.version, in.Vendor, cl, rows)
	if err != nil {
		return nil, err
	}
	for name, data := range map[string][]byte{junitFile: junit, e2eLogFile: renderE2ELog(exported, in.Now), productFile: product} {
		if err = writeFile(filepath.Join(st.productDir, name), data); err != nil {
			return nil, err
		}
	}
	st.readme = &readmeInputs{in: in, agg: agg, rows: rows, supporting: scrubbedAgg.Supporting, exported: exported, redaction: scrubber.Report()}
	state := validationState{Outcome: validationNotRun, Detail: "the run has not reached the validator"}
	if verdict := agg.Verdict(); verdict != nil {
		state.Detail = fmt.Sprintf("%d applicable MUST requirement(s) not established (%s), so the directory was not submitted to it",
			len(agg.IncompleteMusts()), incompleteMustIDs(agg))
	}
	if err = st.writeReadme(state); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, st.staged(errors.WrapCtxErr(err, errors.ErrCodeTimeout, "writing submission"))
	}
	return &SubmissionResult{Aggregate: agg, Dir: st.productDir, Redaction: st.readme.redaction}, nil
}

// validatorFailureState maps a validator error to the README outcome. Only
// a report that rejected the directory is a rejection; a validator that
// could not start (ErrCodeUnavailable, the CNCF landscape unreachable) or
// that was interrupted before a verdict (ErrCodeCanceled, ErrCodeTimeout)
// did not run.
func validatorFailureState(err error) validationState {
	const tail = "; the directory was not written to the submission path"
	switch {
	case stderrors.Is(err, errors.New(errors.ErrCodeUnavailable, "")):
		return validationState{Outcome: validationNotRun, Detail: "the validator could not start (" + err.Error() + ")" + tail}
	case stderrors.Is(err, errors.New(errors.ErrCodeCanceled, "")), stderrors.Is(err, errors.New(errors.ErrCodeTimeout, "")):
		return validationState{Outcome: validationNotRun, Detail: "the validator was interrupted before it reached a verdict (" + err.Error() + ")" + tail}
	}
	return validationState{Outcome: validationRejected, Detail: "its report is in the run log" + tail}
}

// staged returns err with the kept product directory in its context under
// StagedDirContextKey, so a caller can tell the operator where the redacted
// copy is and that it must be removed before the next run. A structured
// error keeps its code; any other error is INTERNAL.
func (st *staging) staged(err error) error {
	code := errors.ErrCodeInternal
	if se, ok := stderrors.AsType[*errors.StructuredError](err); ok && se.Code != "" {
		code = se.Code
	}
	return errors.WrapWithContext(code, "submission not promoted; the staged copy is kept at "+st.productDir, err,
		map[string]any{StagedDirContextKey: st.productDir})
}

// writeReadme renders README.md with the given validation outcome. It is
// called once when the artifacts are written and again once the upstream
// validator has run, so the kept copy states what happened.
func (st *staging) writeReadme(state validationState) error {
	readme, err := renderReadme(st.readme, state)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(st.productDir, readmeFile), readme)
}

func incompleteMustIDs(agg *Aggregate) string {
	incomplete := agg.IncompleteMusts()
	ids := make([]string, 0, len(incomplete))
	for _, r := range incomplete {
		ids = append(ids, r.ID)
	}
	return strings.Join(ids, ", ")
}

// scrubExported returns the exported tests with their messages scrubbed.
func scrubExported(tests []exportedTest, s *redact.TextScrubber) []exportedTest {
	out := make([]exportedTest, len(tests))
	for i, t := range tests {
		t.Message = s.Scrub(t.Message)
		out[i] = t
	}
	return out
}

// scrubAggregate returns a copy of the aggregate whose run-derived reasons
// are scrubbed. A not-applicable requirement's reason is the vendor's
// justification and the documented attestation is the vendor's text, so both
// are kept verbatim, as the upstream descriptions and evidence references
// are; PRODUCT.yaml and README rows are built from this copy.
func scrubAggregate(agg *Aggregate, s *redact.TextScrubber) *Aggregate {
	out := &Aggregate{Version: agg.Version}
	scrub := func(results []RequirementResult) []RequirementResult {
		copied := make([]RequirementResult, len(results))
		for i, r := range results {
			if r.Status != StatusNotApplicable {
				r.Reason = s.Scrub(r.Reason)
			}
			copied[i] = r
		}
		return copied
	}
	out.Requirements = scrub(agg.Requirements)
	out.Supporting = scrub(agg.Supporting)
	return out
}

// promote moves the product directory to the requested path, which
// ValidateSubmissionDir found absent or empty (an empty directory is removed
// so the rename lands at the path itself), and removes the staging tree.
func (st *staging) promote(abs string) error {
	if err := os.Remove(abs); err != nil && !stderrors.Is(err, fs.ErrNotExist) {
		return errors.Wrap(errors.ErrCodeInternal, "replace empty submission directory", err)
	}
	if err := os.Rename(st.productDir, abs); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "move submission directory into place", err)
	}
	// The directory is in place; a staging tree that cannot be removed is
	// worth a warning, not a failure of the completed run.
	if err := os.RemoveAll(st.root); err != nil {
		slog.Warn("submission written, but the empty staging tree could not be removed", "dir", st.root, "error", err)
	}
	return nil
}

// preserved lists the vendor-supplied URLs and the contact address, which
// may appear inside run-derived text the scrubber reads (a documentation URL
// cited by a check, say) and must stay whole there. The other vendor fields
// never pass through the scrubber: PRODUCT.yaml and README.md are rendered
// from them directly, so a platform version such as "1" protects nothing
// elsewhere.
func (in *SubmissionInput) preserved() []string {
	out := []string{in.Vendor.WebsiteURL, in.Vendor.DocumentationURL, in.Vendor.RepoURL,
		in.Vendor.ProductLogoURL, in.Vendor.K8sConformanceURL, in.Vendor.ContactEmail}
	for _, d := range in.Options.Documented {
		out = append(out, d.Evidence...)
	}
	return out
}

// listPages returns the evidence page files the renderer wrote.
func listPages(evidenceDir string) (map[string]bool, error) {
	entries, err := os.ReadDir(evidenceDir)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "list evidence pages", err)
	}
	pages := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.Type().IsRegular() {
			pages[e.Name()] = true
		}
	}
	return pages, nil
}

// scrubPages rewrites each evidence page through the scrubber, in name
// order so the aliases minted along the way are the same on every run.
func scrubPages(evidenceDir string, pages map[string]bool, s *redact.TextScrubber) error {
	names := make([]string, 0, len(pages))
	for name := range pages {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(evidenceDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, "read evidence page for redaction", err)
		}
		if err := writeFile(path, []byte(s.Scrub(string(data)))); err != nil {
			return err
		}
	}
	return nil
}

func writeFile(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // submission files are meant to be read
		return errors.Wrap(errors.ErrCodeInternal, "write "+filepath.Base(path), err)
	}
	return nil
}
