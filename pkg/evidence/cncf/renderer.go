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

// Package cncf renders CNCF AI Conformance evidence markdown from CTRF
// reports against a published upstream checklist version.
//
// The upstream checklists (cncf/k8s-ai-conformance docs/AIConformance-*.yaml)
// are embedded verbatim under schemas/ and pinned by digest in checklist.go;
// ChecklistFor exposes every requirement of a version with its upstream id,
// description and level, and SupportedVersions lists the versions this build
// carries. requirements.go is AICR's side of the map: which conformance
// checks evidence which requirement, and the evidence page each renders to.
// A Renderer takes the checklist version from WithConformanceVersion
// (default LatestVersion); a check whose requirement that version does not
// list (dra_support from v1.36 on) renders as supporting evidence, not as a
// requirement row. aggregate.go computes each requirement's verdict
// fail-closed from the checks expected to evidence it (AggregateReport), the
// single source for the index and for submission packaging.
package cncf

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// Parsed templates cached at package level to avoid re-parsing on every render call.
var (
	parsedEvidenceTemplate = template.Must(template.New("evidence").Funcs(templateFuncs()).Parse(evidenceTemplate))
	parsedIndexTemplate    = template.Must(template.New("index").Funcs(templateFuncs()).Parse(indexTemplate))
)

// Renderer generates CNCF conformance evidence documents from CTRF reports.
type Renderer struct {
	outputDir string
	// version is the upstream checklist the evidence is rendered against.
	// Zero means LatestVersion.
	version Version
	// now overrides wall-clock time when non-zero. Set via WithNow for
	// reproducible builds (SLSA, signed artifacts) where the index file
	// and embedded entry timestamps must be byte-stable across runs.
	// When zero, each Render() call captures its own fresh wall-clock
	// timestamp and threads it through buildEntries and renderIndex so
	// the two outputs always agree within a single render.
	now time.Time
	// notApplicable maps a conditional MUST id to the justification that
	// renders it not applicable; see WithNotApplicable.
	notApplicable map[string]string
	// documented maps a requirement id to the vendor documentation that
	// attests it when the run only observed it; see WithDocumented.
	documented map[string]Documentation
	// noIndex skips index.md: the submission directory's README.md is its
	// index, see WithoutIndex.
	noIndex bool
}

// Option configures a Renderer.
type Option func(*Renderer)

// WithOutputDir sets the output directory for evidence files.
func WithOutputDir(dir string) Option {
	return func(r *Renderer) {
		r.outputDir = dir
	}
}

// WithNow injects a fixed reference time for reproducible builds. When
// non-zero, this value is used for both per-entry GeneratedAt and the
// rendered index timestamp instead of time.Now().
func WithNow(t time.Time) Option {
	return func(r *Renderer) {
		r.now = t
	}
}

// WithConformanceVersion selects the upstream checklist version the evidence
// is rendered against. The default is LatestVersion. An unsupported version
// is reported by Render with ErrCodeInvalidRequest.
func WithConformanceVersion(v Version) Option {
	return func(r *Renderer) {
		r.version = v
	}
}

// WithNotApplicable declares conditional MUST requirements not applicable,
// keyed by upstream id with the justification as value: the validate config
// key spec.validate.evidence.cncf.notApplicable. It is the only source of a
// not-applicable status. Only a conditional MUST (today cluster_autoscaling)
// may appear; Render reports any other id with ErrCodeInvalidRequest. A
// declared requirement renders not applicable even when its check ran, and
// that result is kept as an observation.
func WithNotApplicable(notApplicable map[string]string) Option {
	return func(r *Renderer) {
		r.notApplicable = notApplicable
	}
}

// WithDocumented attaches vendor documentation to requirements the run only
// observed, keyed by upstream id: the validate config key
// spec.validate.evidence.cncf.documented. An observed requirement with an
// entry is established by it (RequirementResult.Established); a requirement
// in any other status ignores its entry. Render reports a malformed map with
// ErrCodeInvalidRequest (ValidateDocumented).
func WithDocumented(documented map[string]Documentation) Option {
	return func(r *Renderer) {
		r.documented = documented
	}
}

// WithoutIndex renders the evidence pages only, no index.md. The submission
// directory sets it because README.md carries the requirement index there.
func WithoutIndex() Option {
	return func(r *Renderer) {
		r.noIndex = true
	}
}

// New creates a new evidence Renderer with the given options.
func New(opts ...Option) *Renderer {
	r := &Renderer{}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// renderTimestamp returns the timestamp to embed in a single Render
// call's output. Injected time (WithNow) takes precedence so reproducible
// builds remain byte-stable; otherwise a fresh wall-clock is captured per
// call (NOT memoized on the Renderer) so successive Render() calls on the
// same instance produce fresh timestamps in non-deterministic mode.
func (r *Renderer) renderTimestamp() time.Time {
	if !r.now.IsZero() {
		return r.now
	}
	return time.Now().UTC()
}

// Version returns the checklist version this Renderer renders against.
func (r *Renderer) Version() Version {
	if r.version == "" {
		return LatestVersion
	}
	return r.version
}

// Render generates evidence markdown files from a CTRF report. It is
// RenderWithResult without the aggregate; see there for the contract.
func (r *Renderer) Render(ctx context.Context, report *ctrf.Report) error {
	_, err := r.RenderWithResult(ctx, report)
	return err
}

// RenderWithResult aggregates the report against the checklist version
// (AggregateReport) and writes one evidence page per requirement that a
// non-skipped check evidenced, plus index.md listing every requirement of the
// checklist with its aggregate status. The aggregate is returned whenever
// the error is nil, including when nothing is written: a nil or empty report,
// or one with no submission-required result, writes no file and the aggregate
// reports every requirement not tested, except those WithNotApplicable
// declares, which stay not applicable.
//
// Errors:
//   - ErrCodeInvalidRequest when the output directory is unset, the
//     checklist version is unsupported, or WithNotApplicable names a
//     requirement that may not be declared not applicable.
//   - ErrCodeTimeout when ctx ends before rendering completes.
//   - ErrCodeInternal when a file cannot be written.
func (r *Renderer) RenderWithResult(ctx context.Context, report *ctrf.Report) (*Aggregate, error) {
	if r.outputDir == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "evidence output directory not set")
	}
	agg, err := AggregateSubmission(r.Version(), report, AggregateOptions{NotApplicable: r.notApplicable, Documented: r.documented})
	if err != nil {
		return nil, err
	}

	if report == nil || len(report.Results.Tests) == 0 {
		slog.Warn("no tests in CTRF report, skipping evidence rendering")
		return agg, nil
	}

	// Capture a single render-scoped timestamp threaded through both
	// per-entry GeneratedAt and the index header so the two artifacts
	// agree without depending on memoization that would persist across
	// subsequent Render() calls.
	renderNow := r.renderTimestamp()

	entries, err := r.buildEntries(report, agg, renderNow)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		slog.Warn("no submission-required checks found, skipping evidence rendering")
		return agg, nil
	}

	if err := os.MkdirAll(r.outputDir, 0o755); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create evidence directory", err)
	}

	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return nil, errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout, "rendering evidence")
		default:
		}
		if err := r.renderEvidence(entry); err != nil {
			return nil, errors.WrapWithContext(errors.ErrCodeInternal,
				"render evidence entry", err,
				map[string]any{
					"requirement": entry.RequirementID,
					"file":        entry.Filename,
				})
		}
	}

	select {
	case <-ctx.Done():
		return nil, errors.WrapCtxErr(ctx.Err(), errors.ErrCodeTimeout, "rendering evidence")
	default:
	}
	if r.noIndex {
		return agg, nil
	}
	if err := r.renderIndex(entries, agg, renderNow); err != nil {
		return nil, err
	}
	return agg, nil
}

// Reasons an index row carries NOT TESTED when no expected check produced a
// result: AICR ships no check for the requirement, or none of its checks ran.
const (
	untestedNoCheck = "no AICR check"
	untestedNotRun  = "not run"
)

// buildEntries groups the non-skipped CTRF results of submission checks by
// evidence page, in order of first appearance, and stamps each page with the
// aggregate verdict of its requirement. A skipped check gets no section: its
// reason is carried by the aggregate (CheckResult.Reason) and, when it left
// the requirement incomplete, by the page's result line.
func (r *Renderer) buildEntries(report *ctrf.Report, agg *Aggregate, now time.Time) ([]evidenceEntry, error) {
	type fileGroup struct {
		meta   *CheckEvidence
		checks []checkEntry
	}
	groupOrder := make([]string, 0)
	groups := make(map[string]*fileGroup)

	for _, test := range report.Results.Tests {
		if test.Status == ctrf.StatusSkipped {
			continue
		}

		meta, err := GetRequirement(r.Version(), test.Name)
		if err != nil {
			return nil, err
		}
		if meta == nil {
			// Not a submission-required check — skip.
			continue
		}

		ce := checkEntry{
			Name:     test.Name,
			Status:   test.Status,
			Message:  test.Message,
			Stdout:   test.Stdout,
			Duration: test.Duration,
		}

		g, exists := groups[meta.File]
		if !exists {
			g = &fileGroup{meta: meta}
			groups[meta.File] = g
			groupOrder = append(groupOrder, meta.File)
		}
		g.checks = append(g.checks, ce)
	}

	entries := make([]evidenceEntry, 0, len(groupOrder))
	for _, filename := range groupOrder {
		g := groups[filename]
		result := agg.Requirement(g.meta.RequirementID)
		if result == nil {
			result = supportingResult(agg, g.meta.RequirementID)
		}
		if result == nil {
			return nil, errors.New(errors.ErrCodeInternal, fmt.Sprintf(
				"requirement %q has results but no aggregate verdict", g.meta.RequirementID))
		}
		entries = append(entries, evidenceEntry{
			RequirementID: g.meta.RequirementID,
			Title:         g.meta.Title,
			Description:   g.meta.Description,
			Filename:      filename,
			Checks:        g.checks,
			Status:        result.Status,
			Reason:        result.Reason,
			Proof:         result.Proof,
			Level:         g.meta.Level,
			Supporting:    g.meta.Supporting,
			Version:       r.Version(),
			GeneratedAt:   now,
		})
	}

	return entries, nil
}

func supportingResult(agg *Aggregate, id string) *RequirementResult {
	for i := range agg.Supporting {
		if agg.Supporting[i].ID == id {
			return &agg.Supporting[i]
		}
	}
	return nil
}

func (r *Renderer) renderEvidence(entry evidenceEntry) (err error) {
	path := filepath.Join(r.outputDir, entry.Filename)
	f, createErr := os.Create(path)
	if createErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create evidence file", createErr)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = errors.Wrap(errors.ErrCodeInternal, "failed to close evidence file", closeErr)
		}
	}()

	if execErr := parsedEvidenceTemplate.Execute(f, entry); execErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to render evidence template", execErr)
	}
	slog.Debug("evidence file written", "file", path)
	return nil
}

func (r *Renderer) renderIndex(entries []evidenceEntry, agg *Aggregate, now time.Time) (err error) {
	path := filepath.Join(r.outputDir, "index.md")
	f, createErr := os.Create(path)
	if createErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create index file", createErr)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = errors.Wrap(errors.ErrCodeInternal, "failed to close index file", closeErr)
		}
	}()

	byRequirement := make(map[string]evidenceEntry, len(entries))
	data := struct {
		GeneratedAt time.Time
		Version     Version
		Passed      bool
		Rows        []indexRow
		Supporting  []evidenceEntry
	}{
		GeneratedAt: now,
		Version:     r.Version(),
		Passed:      agg.Passed(),
	}
	for _, e := range entries {
		if e.Supporting {
			data.Supporting = append(data.Supporting, e)
			continue
		}
		byRequirement[e.RequirementID] = e
	}
	// Every requirement of the checklist, in upstream order, whether or not
	// a check evidenced it, so the index shows the whole checklist.
	for _, res := range agg.Requirements {
		row := indexRow{
			RequirementID: res.ID,
			Level:         res.Level,
			Status:        res.Status,
			Reason:        tableCell(res.Reason),
		}
		if e, ok := byRequirement[res.ID]; ok {
			row.Title = e.Title
			row.Filename = e.Filename
		}
		data.Rows = append(data.Rows, row)
	}

	if execErr := parsedIndexTemplate.Execute(f, data); execErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to render index template", execErr)
	}
	slog.Debug("evidence index written", "file", path)
	return nil
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"upper":  strings.ToUpper,
		"add":    func(a, b int) int { return a + b },
		"join":   strings.Join,
		"result": formatResult,
		"cell":   tableCell,
		"trimV":  func(v Version) string { return strings.TrimPrefix(string(v), "v") },
		"links":  evidenceLinks,
	}
}

// tableCell makes s safe inside one Markdown table cell: whitespace,
// newlines included, collapses to single spaces and pipes are escaped. The
// not-applicable justification is the one user-supplied string that reaches
// the index table.
func tableCell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.ReplaceAll(s, "|", `\|`)
}

// evidenceLinks renders PRODUCT.yaml evidence references as Markdown links
// in one table cell: a URL links to itself, a local reference to its file
// (the fragment, which names a testcase, is shown and dropped from the
// target). Empty evidence reads as a dash.
func evidenceLinks(refs []string) string {
	if len(refs) == 0 {
		return "-"
	}
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
			out = append(out, "["+ref+"]("+ref+")")
			continue
		}
		target, _, _ := strings.Cut(ref, "#")
		out = append(out, "[`"+ref+"`]("+target+")")
	}
	return strings.Join(out, ", ")
}

// formatResult renders a verdict cell: the status in upper case, followed by
// the reason (the N/A justification, the failing checks, the unmet
// prerequisites) when there is one.
func formatResult(status RequirementStatus, reason string) string {
	out := strings.ToUpper(string(status))
	if reason != "" {
		out += " (" + reason + ")"
	}
	return out
}
