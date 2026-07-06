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

package main

import (
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/tuning"
)

// markdownOptions configures table rendering.
type markdownOptions struct {
	AICRVersion   string
	Deterministic bool
	NoTitle       bool
	Timestamp     string // used only when non-deterministic and non-empty
}

// stickyWriter remembers the first write error so the caller checks once.
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

// renderTable writes the tuning-status matrix as Markdown. Report.Rows are
// already sorted by tuning.Compute, so rendering preserves that order.
func renderTable(w io.Writer, report *tuning.Report, opts markdownOptions) error {
	sw := &stickyWriter{w: w}

	preamble := false
	if !opts.NoTitle {
		fmt.Fprintf(sw, "# Nodewright Tuning Status\n\n")
		preamble = true
	}
	if !opts.Deterministic {
		ts := opts.Timestamp
		if ts == "" {
			ts = time.Now().UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(sw, "_Generated %s for aicr %s._\n\n", ts, opts.AICRVersion)
		preamble = true
	}
	// Ensure a blank line precedes the table so the spliced body is separated
	// from the marker/preamble above it (markdown MD058: tables must be
	// surrounded by blank lines). Title/timestamp blocks already end with one.
	if !preamble {
		fmt.Fprintln(sw)
	}

	headers := [5]string{"Service", "Accelerator", "Profile", "Setup", "Tuning"}
	rows := make([][5]string, len(report.Rows))
	for i, r := range report.Rows {
		rows[i] = [5]string{r.Service, r.Accelerator, r.Profile, pinCell(r.Setup), pinCell(r.Tuning)}
	}

	var widths [5]int
	for i, h := range headers {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if w := utf8.RuneCountInString(cell); w > widths[i] {
				widths[i] = w
			}
		}
	}

	writeRow(sw, headers, widths)
	writeSeparatorRow(sw, widths)
	for _, row := range rows {
		writeRow(sw, row, widths)
	}
	fmt.Fprintln(sw)

	if sw.err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write tuning-status markdown", sw.err)
	}
	return nil
}

// pinCell renders a package pin as "name version", or notApplicable when absent.
func pinCell(p tuning.PackagePin) string {
	if p.Name == "" {
		return tuning.NotApplicable
	}
	return p.Name + " " + p.Version
}

// writeRow renders one Markdown table row, left-justifying (right-padding)
// each cell to its column width. Width and padding are computed by rune
// count, not byte length, so any multi-byte glyph in a cell would still
// align correctly against ASCII cells.
func writeRow(w io.Writer, cells [5]string, widths [5]int) {
	fmt.Fprint(w, "|")
	for i, cell := range cells {
		fmt.Fprintf(w, " %s |", padCell(cell, widths[i]))
	}
	fmt.Fprintln(w)
}

// writeSeparatorRow renders the header/body divider row. Each cell is filled
// entirely with hyphens (no literal spaces) spanning the same total width as
// " <cell> " in a data row, so pipes stay aligned with the padded columns
// above and below. Every cell has at least 3 dashes, per Markdown table
// syntax; all columns here exceed that via their headers.
func writeSeparatorRow(w io.Writer, widths [5]int) {
	fmt.Fprint(w, "|")
	for _, width := range widths {
		dashes := width + 2
		if dashes < 3 {
			dashes = 3
		}
		fmt.Fprintf(w, "%s|", strings.Repeat("-", dashes))
	}
	fmt.Fprintln(w)
}

// padCell right-pads cell with ASCII spaces to width visual columns,
// counting runes rather than bytes.
func padCell(cell string, width int) string {
	pad := width - utf8.RuneCountInString(cell)
	if pad <= 0 {
		return cell
	}
	return cell + strings.Repeat(" ", pad)
}
