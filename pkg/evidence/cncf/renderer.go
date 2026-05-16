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

// Package cncf renders CNCF AI Conformance evidence markdown from CTRF reports.
package cncf

import (
	"context"
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
	// now overrides wall-clock time when non-zero. Set via WithNow for
	// reproducible builds (SLSA, signed artifacts) where the index file
	// and embedded entry timestamps must be byte-stable across runs.
	// When zero, each Render() call captures its own fresh wall-clock
	// timestamp and threads it through buildEntries and renderIndex so
	// the two outputs always agree within a single render.
	now time.Time
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

// Render generates evidence markdown files from a CTRF report.
// Groups test results by CNCF requirement. Only submission-required
// checks produce evidence. Skipped tests are excluded.
func (r *Renderer) Render(ctx context.Context, report *ctrf.Report) error {
	if r.outputDir == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "evidence output directory not set")
	}

	if report == nil || len(report.Results.Tests) == 0 {
		slog.Warn("no tests in CTRF report, skipping evidence rendering")
		return nil
	}

	if err := os.MkdirAll(r.outputDir, 0o755); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create evidence directory", err)
	}

	// Capture a single render-scoped timestamp threaded through both
	// per-entry GeneratedAt and the index header so the two artifacts
	// agree without depending on memoization that would persist across
	// subsequent Render() calls.
	renderNow := r.renderTimestamp()

	entries := r.buildEntries(report, renderNow)
	if len(entries) == 0 {
		slog.Warn("no submission-required checks found, skipping evidence rendering")
		return nil
	}

	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return errors.Wrap(errors.ErrCodeTimeout, "evidence rendering canceled", ctx.Err())
		default:
		}
		if err := r.renderEvidence(entry); err != nil {
			return errors.WrapWithContext(errors.ErrCodeInternal,
				"render evidence entry", err,
				map[string]any{
					"requirement": entry.RequirementID,
					"file":        entry.Filename,
				})
		}
	}

	select {
	case <-ctx.Done():
		return errors.Wrap(errors.ErrCodeTimeout, "evidence rendering canceled", ctx.Err())
	default:
	}
	return r.renderIndex(entries, renderNow)
}

// buildEntries groups CTRF test results by requirement.
func (r *Renderer) buildEntries(report *ctrf.Report, now time.Time) []evidenceEntry {
	// Group by evidence file, preserving order of first appearance.
	type fileGroup struct {
		meta    *requirementMeta
		checks  []checkEntry
		hasFail bool
	}
	groupOrder := make([]string, 0)
	groups := make(map[string]*fileGroup)

	for _, test := range report.Results.Tests {
		if test.Status == ctrf.StatusSkipped {
			continue
		}

		meta := GetRequirement(test.Name)
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
		if test.Status == ctrf.StatusFailed {
			g.hasFail = true
		}
	}

	entries := make([]evidenceEntry, 0, len(groupOrder))
	for _, filename := range groupOrder {
		g := groups[filename]
		status := ctrf.StatusPassed
		if g.hasFail {
			status = ctrf.StatusFailed
		}
		entries = append(entries, evidenceEntry{
			RequirementID: g.meta.RequirementID,
			Title:         g.meta.Title,
			Description:   g.meta.Description,
			Filename:      filename,
			Checks:        g.checks,
			Status:        status,
			GeneratedAt:   now,
		})
	}

	return entries
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

func (r *Renderer) renderIndex(entries []evidenceEntry, now time.Time) (err error) {
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

	data := struct {
		GeneratedAt time.Time
		Entries     []evidenceEntry
	}{
		GeneratedAt: now,
		Entries:     entries,
	}

	if execErr := parsedIndexTemplate.Execute(f, data); execErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to render index template", execErr)
	}
	slog.Debug("evidence index written", "file", path)
	return nil
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"upper": strings.ToUpper,
		"add":   func(a, b int) int { return a + b },
		"join":  strings.Join,
	}
}
