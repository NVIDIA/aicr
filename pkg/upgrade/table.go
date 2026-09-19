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

// sourceLine is the label-and-value shape of the cluster-source block, padded
// wide enough for its longest label ("kubeconfig").
const sourceLine = "  %-10s  %s\n"

// zeroMatchAdvice is what the banner says when the read matched nothing. It
// names the causes rather than only the finding: "nothing was found" on its
// own has no next action, and the action differs entirely by cause.
const zeroMatchAdvice = "Nothing was compared, and every row below therefore reads \"added\". A cluster with " +
	"none of these components installed reads this way, and so do the two likelier causes: this is not the " +
	"cluster you meant, or these components were installed by a deployer other than the one this check was " +
	"given, whose releases it does not look for."

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
		ew.printf("  from      %s\n", safe(r.From))
	}
	if r.To != "" {
		ew.printf("  to        %s\n", safe(r.To))
	}
	if r.Deployer != "" {
		ew.printf("  deployer  %s\n", safe(r.Deployer))
	}
	ew.println("")

	if r.Source != nil {
		writeSource(ew, r.Source)
	}

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
		from, to := rowCells(c)
		// Component and Notes go through safe() too: every field in this row
		// is artifact-derived, and one unescaped cell is enough to forge a row.
		ew.printf("%s\t%s\t%s\t%s\t%s\n",
			safe(c.Component), from, to, cell(string(c.Verdict)), safe(c.Notes))
	}
	if ew.err != nil {
		return wrapTableErr(ew.err)
	}
	return wrapTableErr(tw.Flush())
}

// rowCells renders the FROM and TO columns, which name whatever axis the row
// moved on.
//
// An identity row held its version, so printing that version in both columns
// is the one rendering that reads as nothing having happened, on the row where
// something did. The columns carry the fields that moved instead, as
// field=value so a reader is never left guessing what a bare namespace is.
// That substitution is the same one a replaced row makes when its FROM holds a
// component name: these two columns describe the move, not the version line.
// The displaced version moves into NOTES.
func rowCells(c ReportComponent) (from, to string) {
	if c.Change != ChangeIdentity || len(c.IdentityChanges) == 0 {
		return cell(c.From), cell(c.To)
	}
	fromFields := make([]string, len(c.IdentityChanges))
	toFields := make([]string, len(c.IdentityChanges))
	for i, ch := range c.IdentityChanges {
		fromFields[i] = ch.Field + "=" + ch.From
		toFields[i] = ch.Field + "=" + ch.To
	}
	return cell(strings.Join(fromFields, ", ")), cell(strings.Join(toFields, ", "))
}

// writeSource states which cluster the `from` table was read from, above the
// rows rather than below them.
//
// A cluster read that recognizes nothing is reported rather than failed, and
// every row under a zero match then reads "added". This block is the only
// thing that separates an empty cluster from a kubeconfig pointed at the wrong
// context, so it has to be read before the rows and not after: a reader who
// reaches a footnote under a long table has already drawn a conclusion from
// it.
//
// The per-reader lines come before the banner, because they are the evidence
// the banner's diagnosis rests on: records were read and none of them matched.
func writeSource(ew *errWriter, s *ReportSource) {
	ew.println("READ FROM CLUSTER")
	// Rendered even when empty. Neither is always knowable — an in-cluster run
	// has no kubeconfig file — and a dropped line reads as nothing to say
	// rather than as not known.
	ew.printf(sourceLine, "kubeconfig", cell(s.Kubeconfig))
	ew.printf(sourceLine, "context", cell(s.Context))
	ew.printf(sourceLine, "matched", plural(s.Matched, "component", "components"))
	ew.printf(sourceLine, "helm", strings.Join([]string{
		plural(s.Helm.Records, "storage record", "storage records"),
		fmt.Sprintf("%d unattributed", s.Helm.Unattributed),
		fmt.Sprintf("%d unreadable", s.Helm.Unreadable),
		fmt.Sprintf("%d uninstalled", s.Helm.Uninstalled),
		fmt.Sprintf("%d stamped but unmatched", s.Helm.StampedUnmatched),
	}, ", "))
	// The Argo line ends by saying the stamp check does not apply rather than
	// reporting it as zero: see ReportSourceArgo.
	ew.printf(sourceLine, "argo", strings.Join([]string{
		plural(s.Argo.Applications, "application", "applications"),
		fmt.Sprintf("%d unattributed", s.Argo.Unattributed),
		fmt.Sprintf("%d unreadable", s.Argo.Unreadable),
		"no stamp to check",
	}, ", "))

	if s.Matched == 0 {
		ew.println("")
		ew.println("  NOTHING INSTALLED WAS RECOGNIZED")
		writeParagraph(ew, "  ", zeroMatchAdvice)
	}
	ew.println("")
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
	if len(c.IdentityChanges) > 0 {
		// A row reaches this block on its version verdict, and the steps below
		// were authored for that boundary alone. Without this line an operator
		// works through them and ships the relocation unannounced.
		if c.Summary != "" || c.Explanation != "" {
			ew.println("")
		}
		writeParagraph(ew, "  ", "This hop also relocates the component: "+
			movedFieldsPhrase(c.IdentityChanges)+". No record assesses a relocation, and the "+
			"steps below neither perform it nor account for it.")
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
	ew.printf("  STEPS (deployer: %s)\n", safe(deployer))
	if len(c.Steps) == 0 {
		writeParagraph(ew, "    ", fmt.Sprintf(
			"This record declares no steps for deployer %q. Its verdict says operator action is "+
				"required, so treat the absence as an authoring gap rather than as nothing to do.", deployer))
		return wrapTableErr(ew.err)
	}
	for i, s := range c.Steps {
		ew.printf("    %d. %s\n", i+1, safe(s.ID))
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
		return fmt.Sprintf("%s replaces %s", safe(c.Component), safe(c.From))
	}
	return fmt.Sprintf("%s %s -> %s", safe(c.Component), safe(c.From), safe(c.To))
}

// writeParagraph emits record prose at a fixed indent, wrapped to a width a
// standard terminal shows without folding.
func writeParagraph(ew *errWriter, indent, text string) {
	// Every caller passes record prose: summary, precondition, explanation, a
	// step description or its reason. Escaping at this one point covers them all.
	for _, line := range wrapText(safe(text), reportWrapWidth-len(indent)) {
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

// safe renders artifact-derived text for a terminal. Component names, versions
// and artifact paths all come from a recipe or bundle the operator did not
// necessarily write, and the table is the surface they read a verdict off, so a
// crafted value carrying newlines or an ANSI sequence could forge a row or
// recolor one. Control characters are shown rather than executed.
//
// Only the table does this. JSON output stays faithful because encoding/json
// already escapes these bytes, and a consumer parsing it is not a terminal.
func safe(s string) string {
	if strings.IndexFunc(s, isControl) < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case isControl(r):
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isControl reports whether r is a C0 or C1 control character, or DEL. ESC
// (0x1b) is the one that matters most: it opens every ANSI sequence.
func isControl(r rune) bool {
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f)
}

func cell(s string) string {
	if s == "" {
		return "-"
	}
	return safe(s)
}

func wrapTableErr(err error) error {
	if err == nil {
		return nil
	}
	return errors.Wrap(errors.ErrCodeInternal, "failed to write upgrade report table output", err)
}
