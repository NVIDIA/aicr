// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

// Package evidence renders CNCF AI Conformance evidence markdown from validation results.
// Evidence includes both structural validation and behavioral test outputs.
package evidence

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator"
	"github.com/NVIDIA/aicr/pkg/validator/checks"
)

// Renderer generates CNCF conformance evidence documents from validation results.
type Renderer struct {
	outputDir string
}

// Option configures a Renderer.
type Option func(*Renderer)

// WithOutputDir sets the output directory for evidence files.
func WithOutputDir(dir string) Option {
	return func(r *Renderer) {
		r.outputDir = dir
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

// Render generates evidence markdown files from a validation result.
// Only checks with SubmissionRequirement=true produce evidence files.
// Checks with status "skipped" are excluded from evidence output.
func (r *Renderer) Render(ctx context.Context, result *validator.ValidationResult) error {
	if r.outputDir == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "evidence output directory not set")
	}

	conformance, ok := result.Phases["conformance"]
	if !ok {
		return errors.New(errors.ErrCodeNotFound, "no conformance phase in validation result")
	}

	// Build evidence entries grouped by EvidenceFile.
	entries := r.buildEntries(conformance)

	if len(entries) == 0 {
		return errors.New(errors.ErrCodeNotFound, "no submission checks found in conformance results")
	}

	// Create output directory.
	if err := os.MkdirAll(r.outputDir, 0o755); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create evidence directory", err)
	}

	// Render per-requirement evidence files.
	for _, entry := range entries {
		select {
		case <-ctx.Done():
			return errors.Wrap(errors.ErrCodeTimeout, "evidence rendering cancelled", ctx.Err())
		default:
		}
		if err := r.renderEvidence(entry); err != nil {
			return err
		}
	}

	// Render index.
	select {
	case <-ctx.Done():
		return errors.Wrap(errors.ErrCodeTimeout, "evidence rendering cancelled", ctx.Err())
	default:
	}
	return r.renderIndex(entries, result.RunID)
}

// buildEntries groups check results by EvidenceFile.
func (r *Renderer) buildEntries(conformance *validator.PhaseResult) []EvidenceEntry {
	now := time.Now().UTC()

	// Group by EvidenceFile, preserving order of first appearance.
	type fileGroup struct {
		check   *checks.Check
		checks  []CheckEntry
		hasFail bool
	}
	groupOrder := make([]string, 0)
	groups := make(map[string]*fileGroup)

	for _, cr := range conformance.Checks {
		// Skip checks with "skipped" status — never emit evidence for unexecuted checks.
		if cr.Status == validator.ValidationStatusSkipped {
			slog.Debug("skipping evidence for unexecuted check", "check", cr.Name)
			continue
		}

		// Dual-lookup: try check name first, then test name.
		check, ok := checks.ResolveCheck(cr.Name)
		if !ok {
			slog.Debug("check not found in registry, skipping evidence", "name", cr.Name)
			continue
		}

		// Only include submission requirements.
		if !check.SubmissionRequirement || check.EvidenceFile == "" {
			continue
		}

		entry := CheckEntry{
			Name:      check.Name,
			Status:    cr.Status,
			Reason:    cr.Reason,
			Duration:  cr.Duration,
			Artifacts: cr.Artifacts,
		}

		g, exists := groups[check.EvidenceFile]
		if !exists {
			g = &fileGroup{check: check}
			groups[check.EvidenceFile] = g
			groupOrder = append(groupOrder, check.EvidenceFile)
		}
		g.checks = append(g.checks, entry)
		if cr.Status == validator.ValidationStatusFail {
			g.hasFail = true
		}
	}

	entries := make([]EvidenceEntry, 0, len(groupOrder))
	for _, filename := range groupOrder {
		g := groups[filename]
		status := validator.ValidationStatusPass
		if g.hasFail {
			status = validator.ValidationStatusFail
		}
		entries = append(entries, EvidenceEntry{
			RequirementID: g.check.RequirementID,
			Title:         g.check.EvidenceTitle,
			Description:   g.check.EvidenceDescription,
			Filename:      filename,
			Checks:        g.checks,
			Status:        status,
			GeneratedAt:   now,
		})
	}
	return entries
}

// renderEvidence writes a single evidence markdown file.
func (r *Renderer) renderEvidence(entry EvidenceEntry) (err error) {
	tmpl, err := template.New("evidence").Funcs(templateFuncs()).Parse(evidenceTemplate)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to parse evidence template", err)
	}

	path := filepath.Join(r.outputDir, entry.Filename)
	f, err := os.Create(path)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create evidence file", err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = errors.Wrap(errors.ErrCodeInternal, "failed to close evidence file", closeErr)
		}
	}()

	if err := tmpl.Execute(f, entry); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to render evidence template", err)
	}
	slog.Debug("evidence file written", "file", path)
	return nil
}

// renderIndex writes the index.md summary file.
func (r *Renderer) renderIndex(entries []EvidenceEntry, runID string) (err error) {
	tmpl, err := template.New("index").Funcs(templateFuncs()).Parse(indexTemplate)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to parse index template", err)
	}

	path := filepath.Join(r.outputDir, "index.md")
	f, err := os.Create(path)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create index file", err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = errors.Wrap(errors.ErrCodeInternal, "failed to close index file", closeErr)
		}
	}()

	data := struct {
		GeneratedAt time.Time
		RunID       string
		Entries     []EvidenceEntry
	}{
		GeneratedAt: time.Now().UTC(),
		RunID:       runID,
		Entries:     entries,
	}

	if err := tmpl.Execute(f, data); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to render index template", err)
	}
	slog.Debug("evidence index written", "file", path)
	return nil
}

// templateFuncs returns the template function map.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"upper": func(s validator.ValidationStatus) string { return strings.ToUpper(string(s)) },
		"inc":   func(i int) int { return i + 1 },
	}
}
