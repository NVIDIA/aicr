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
	"strings"
	"text/tabwriter"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// reportWrapWidth is the column the detail blocks wrap prose at, indent
// included, leaving an 80-column terminal a margin rather than exactly filling
// it.
const reportWrapWidth = 78

// errWriter retains the first write error so a renderer checks once rather than
// after every line. Writes after an error are no-ops.
type errWriter struct {
	w   io.Writer
	err error
}

func (ew *errWriter) printf(format string, args ...any) {
	if ew.err != nil {
		return
	}
	_, ew.err = fmt.Fprintf(ew.w, format, args...)
}

func (ew *errWriter) println(s string) {
	if ew.err != nil {
		return
	}
	_, ew.err = fmt.Fprintln(ew.w, s)
}

// WriteTable writes the report as a human-readable table followed by the detail
// block each row that needs operator attention owns.
//
// A nil report is a malformed call rather than an empty check, and returns
// ErrCodeInvalidRequest: reporting "no component changes" for a programming
// error would read as an all-clear.
func WriteTable(w io.Writer, r *Report) error {
	if w == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "upgrade report table writer is required (got nil)")
	}
	if r == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "upgrade report is required (got nil)")
	}

	ew := &errWriter{w: w}
	ew.println("UPGRADE CHECK")
	if r.From != "" {
		ew.printf("  from      %s\n", r.From)
	}
	if r.To != "" {
		ew.printf("  to        %s\n", r.To)
	}
	if r.Deployer != "" {
		ew.printf("  deployer  %s\n", r.Deployer)
	}
	ew.println("")

	if len(r.Components) == 0 {
		ew.println("NO COMPONENT CHANGES")
		return wrapTableErr(ew.err)
	}

	if err := writeRows(w, r.Components); err != nil {
		return err
	}

	for i := range r.Components {
		if err := writeDetail(ew, &r.Components[i], r.Deployer); err != nil {
			return err
		}
	}

	ew.println("")
	needs := "need"
	if r.Summary.Failing == 1 {
		needs = "needs"
	}
	ew.printf("%s, %d %s attention\n",
		plural(r.Summary.Components, "component change", "component changes"), r.Summary.Failing, needs)
	return wrapTableErr(ew.err)
}

func writeRows(w io.Writer, rows []ReportComponent) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	ew := &errWriter{w: tw}
	ew.println("COMPONENT\tFROM\tTO\tVERDICT\tNOTES")
	ew.println("---------\t----\t--\t-------\t-----")
	for _, c := range rows {
		ew.printf("%s\t%s\t%s\t%s\t%s\n",
			c.Component, cell(c.From), cell(c.To), cell(string(c.Verdict)), c.Notes)
	}
	if ew.err != nil {
		return wrapTableErr(ew.err)
	}
	return wrapTableErr(tw.Flush())
}

// writeDetail renders the block below the table for a row the operator has to
// act on. A safe, unknown or unversioned row has nothing to add that the table
// did not already say.
func writeDetail(ew *errWriter, c *ReportComponent, deployer string) error {
	if c.Verdict != VerdictManual && c.Verdict != VerdictBlocked {
		return nil
	}
	ew.println("")
	ew.printf("%s  (%s)\n", heading(c), c.Verdict)
	if c.Summary != "" {
		writeParagraph(ew, "  ", c.Summary)
	}
	if c.Explanation != "" {
		// The NOTES column stays terse, so the sentence naming the versions
		// and the action lives here rather than in the row.
		if c.Summary != "" {
			ew.println("")
		}
		writeParagraph(ew, "  ", c.Explanation)
	}

	if c.Verdict == VerdictBlocked && c.Reason != ReasonRecorded {
		// Deliberately no steps: no single record describes the whole jump,
		// and an intermediate record's work never runs on one straight past it.
		// A blocked row whose own record covers the move falls through, because
		// those steps are that author saying how to make it safely.
		return wrapTableErr(ew.err)
	}

	if c.Precondition != "" {
		ew.println("")
		ew.println("  PRECONDITION")
		writeParagraph(ew, "    ", c.Precondition)
	}

	ew.println("")
	ew.printf("  STEPS (deployer: %s)\n", deployer)
	if len(c.Steps) == 0 {
		writeParagraph(ew, "    ", fmt.Sprintf(
			"This record declares no steps for deployer %q. Its verdict says operator action is "+
				"required, so treat the absence as an authoring gap rather than as nothing to do.", deployer))
		return wrapTableErr(ew.err)
	}
	for i, s := range c.Steps {
		ew.printf("    %d. %s\n", i+1, s.ID)
		writeParagraph(ew, "       ", s.Description)
		if s.Reason != "" {
			writeParagraph(ew, "       ", "Why: "+s.Reason)
		}
	}
	return wrapTableErr(ew.err)
}

// heading names what the row is about, which differs by change kind: a
// replacement joins two components rather than two versions of one.
func heading(c *ReportComponent) string {
	if c.Change == ChangeReplaced {
		return fmt.Sprintf("%s replaces %s", c.Component, c.From)
	}
	return fmt.Sprintf("%s %s -> %s", c.Component, c.From, c.To)
}

// writeParagraph emits record prose at a fixed indent, wrapped to a width a
// standard terminal shows without folding.
func writeParagraph(ew *errWriter, indent, text string) {
	for _, line := range wrapText(text, reportWrapWidth-len(indent)) {
		ew.println(indent + line)
	}
}

// wrapText greedily re-flows text to width columns.
//
// Whitespace is normalized rather than preserved: record prose is folded YAML,
// so its line breaks are an artifact of how the author wrapped the source file
// and carry no meaning here. A word longer than width is left whole, because a
// URL or a semver range split across lines is worse than a long line.
func wrapText(text string, width int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	if width <= 0 {
		return []string{strings.Join(words, " ")}
	}
	lines := make([]string, 0, 1+len(text)/width)
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			lines = append(lines, line)
			line = w
			continue
		}
		line += " " + w
	}
	return append(lines, line)
}

func cell(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func wrapTableErr(err error) error {
	if err == nil {
		return nil
	}
	return errors.Wrap(errors.ErrCodeInternal, "failed to write upgrade report table output", err)
}
