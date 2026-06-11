// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package main

import (
	"fmt"
	"io"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/health"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// evidencePending is the literal value rendered in the Evidence column for
// every recipe in V1. No attestations exist yet (recipes/evidence/ is empty),
// so the column is uniformly pending and never overstates what is known. The
// hand-written doc header explains why; see ADR-009 §3.
const evidencePending = "pending"

// markdownOptions configures matrix rendering.
type markdownOptions struct {
	// AICRVersion labels the run in the non-deterministic generated-stamp line.
	AICRVersion string

	// Deterministic suppresses per-run metadata (the generated timestamp) so
	// the output is byte-stable and committable.
	Deterministic bool

	// NoTitle omits the H1 title so the body can be spliced into a marked
	// region of a larger hand-written document.
	NoTitle bool

	// Timestamp, when non-empty and Deterministic is false, is rendered in the
	// generated-stamp line instead of wall-clock time. Ignored in deterministic
	// mode.
	Timestamp string
}

// stickyWriter wraps an io.Writer and remembers the first write error so the
// caller can check once at the end instead of after every Fprintf. Subsequent
// writes after a failure are no-ops.
type stickyWriter struct {
	w   io.Writer
	err error
}

func (s *stickyWriter) Write(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.w.Write(p)
	if err != nil {
		s.err = err
	}
	return n, err
}

// renderMatrix writes the recipe-health matrix as Markdown. The report's
// Combos are already sorted deterministically by health.Compute, so rendering
// preserves that order and adds no ordering of its own.
func renderMatrix(w io.Writer, report *health.Report, opts markdownOptions) error {
	sw := &stickyWriter{w: w}

	if !opts.NoTitle {
		fmt.Fprintf(sw, "# AICR Recipe Health\n\n")
	}
	if !opts.Deterministic {
		ts := opts.Timestamp
		if ts == "" {
			ts = time.Now().UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(sw, "_Generated %s for aicr %s._\n\n", ts, opts.AICRVersion)
	}

	writeSummary(sw, report)
	writeMatrix(sw, report)

	if sw.err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write recipe-health markdown", sw.err)
	}
	return nil
}

// writeSummary emits the recipe count and a rolled-up status tally.
func writeSummary(sw *stickyWriter, report *health.Report) {
	var pass, warn, fail, unknown int
	for _, c := range report.Combos {
		switch c.Structure.Status {
		case health.StatusPass:
			pass++
		case health.StatusWarn:
			warn++
		case health.StatusFail:
			fail++
		case health.StatusUnknown:
			unknown++
		}
	}

	fmt.Fprintf(sw, "## Summary\n\n")
	fmt.Fprintf(sw, "- Recipes: **%d**\n", len(report.Combos))
	fmt.Fprintf(sw, "- Pass: **%d** · Warn: **%d** · Fail: **%d** · Unknown: **%d**\n\n",
		pass, warn, fail, unknown)
}

// writeMatrix emits the per-recipe matrix table.
func writeMatrix(sw *stickyWriter, report *health.Report) {
	fmt.Fprintf(sw, "## Recipes\n\n")
	fmt.Fprintln(sw, "| Recipe | Service | Accelerator | OS | Intent | Platform | Status | Coverage | Evidence |")
	fmt.Fprintln(sw, "|--------|---------|-------------|----|--------|----------|--------|----------|----------|")
	for _, c := range report.Combos {
		crit := c.Criteria
		fmt.Fprintf(sw, "| %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			c.LeafOverlay,
			dimCell(string(crit.Service)),
			dimCell(string(crit.Accelerator)),
			dimCell(string(crit.OS)),
			dimCell(string(crit.Intent)),
			dimCell(string(crit.Platform)),
			c.Structure.Status,
			coverageCell(c.Structure.Coverage),
			evidencePending,
		)
	}
	fmt.Fprintln(sw)
}

// dimCell renders a single criteria dimension. An unspecified dimension ("any"
// or empty) renders as an em dash so the matrix reads as "not constrained"
// rather than the literal sentinel "any".
func dimCell(v string) string {
	if v == "" || v == recipe.CriteriaAnyValue {
		return "—"
	}
	return v
}

// coverageCell renders the compact per-phase declared-coverage summary
// (R:n D:n P:n C:n, counts of named checks per phase), per ADR-009 §3. A nil
// Coverage (the recipe did not resolve, so there is no RecipeResult to read)
// renders as an em dash rather than a misleading all-zero block.
func coverageCell(cov *health.DeclaredCoverage) string {
	if cov == nil {
		return "—"
	}
	return cov.Compact()
}
