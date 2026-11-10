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

package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// emptyCell stands in for an empty value or an empty section so a column or
// a section never disappears from the layout.
const emptyCell = "-"

// writeRecipeSummary writes summary to w: a text overview for
// serializer.FormatTable, and the Summary document for YAML and JSON.
func writeRecipeSummary(ctx context.Context, w io.Writer, summary *aicr.RecipeSummary, format serializer.Format) error {
	if err := ctx.Err(); err != nil {
		return errors.WrapCtxErr(err, errors.ErrCodeTimeout, "recipe summary canceled")
	}
	if format != serializer.FormatTable {
		return writeComplexValue(w, summary, format)
	}

	var buf bytes.Buffer
	if err := renderRecipeSummary(ctx, &buf, summary); err != nil {
		return err
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write recipe summary", err)
	}
	return nil
}

// renderRecipeSummary renders the text overview into buf. Writes to a
// bytes.Buffer cannot fail, so only the tabwriter flush and the context check
// return errors.
func renderRecipeSummary(ctx context.Context, buf *bytes.Buffer, s *aicr.RecipeSummary) error {
	fmt.Fprintln(buf, "RECIPE SUMMARY")
	// The text view is not a document, so it shows the summarized recipe's
	// header rather than the summary's own.
	identity := [][2]string{
		{"API Version", orDash(s.Source.APIVersion)},
		{"Kind", orDash(s.Source.Kind)},
		{"Version", orDash(s.Version)},
		{"GPU Driver", s.GPUDriver},
	}
	if s.Profile != "" {
		identity = append(identity, [2]string{"Profile", s.Profile})
	}
	writeKeyValues(buf, "", identity)

	criteria := [][2]string{
		{"Service", orAny(string(s.Criteria.Service))},
		{"Accelerator", orAny(string(s.Criteria.Accelerator))},
		{"Intent", orAny(string(s.Criteria.Intent))},
		{"OS", orAny(string(s.Criteria.OS))},
		{"Platform", orAny(string(s.Criteria.Platform))},
	}
	if s.Criteria.Nodes != 0 {
		criteria = append(criteria, [2]string{"Nodes", strconv.Itoa(s.Criteria.Nodes)})
	}
	fmt.Fprintln(buf, "\nCRITERIA")
	writeKeyValues(buf, "  ", criteria)

	fmt.Fprintln(buf, "\nOVERLAYS")
	if len(s.AppliedOverlays) == 0 {
		fmt.Fprintf(buf, "  Applied:  %s\n", emptyCell)
	} else {
		fmt.Fprintf(buf, "  Applied:  %s\n", strings.Join(s.AppliedOverlays, ", "))
	}

	fmt.Fprintln(buf, "\nCONSTRAINTS")
	if len(s.Constraints) == 0 {
		fmt.Fprintf(buf, "  %s\n", emptyCell)
	}
	constraints := newSummaryTable(buf)
	for _, c := range s.Constraints {
		if err := ctx.Err(); err != nil {
			return errors.WrapCtxErr(err, errors.ErrCodeTimeout, "recipe summary canceled")
		}
		fmt.Fprintf(constraints, "  %s\t%s\n", c.Name, c.Value)
	}
	if err := constraints.Flush(); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to render constraints table", err)
	}

	fmt.Fprintf(buf, "\nCOMPONENTS (%d)\n", len(s.Components))
	components := newSummaryTable(buf)
	fmt.Fprintln(components, "NAME\tTYPE\tVERSION\tNAMESPACE\tDEPENDS ON")
	for _, c := range s.Components {
		if err := ctx.Err(); err != nil {
			return errors.WrapCtxErr(err, errors.ErrCodeTimeout, "recipe summary canceled")
		}
		fmt.Fprintf(components, "%s\t%s\t%s\t%s\t%s\n",
			c.Name, orDash(c.Type), orDash(c.Version), orDash(c.Namespace), joinOrDash(c.DependsOn))
	}
	if err := components.Flush(); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to render components table", err)
	}

	fmt.Fprintln(buf, "\nVALIDATION")
	if len(s.Validation) == 0 {
		fmt.Fprintf(buf, "  %s\n", emptyCell)
	}
	phases := make([][2]string, 0, len(s.Validation))
	for _, p := range s.Validation {
		if err := ctx.Err(); err != nil {
			return errors.WrapCtxErr(err, errors.ErrCodeTimeout, "recipe summary canceled")
		}
		phases = append(phases, [2]string{phaseTitle(p.Name), strings.Join(p.Checks, ", ")})
	}
	writeKeyValues(buf, "  ", phases)
	return nil
}

func newSummaryTable(buf *bytes.Buffer) *tabwriter.Writer {
	return tabwriter.NewWriter(buf, 0, 0, 2, ' ', 0)
}

// writeKeyValues writes rows as "Key:  value" lines with the values aligned.
func writeKeyValues(buf *bytes.Buffer, indent string, rows [][2]string) {
	tw := newSummaryTable(buf)
	for _, r := range rows {
		fmt.Fprintf(tw, "%s%s:\t%s\n", indent, r[0], r[1])
	}
	_ = tw.Flush() // flushing into a bytes.Buffer cannot fail
}

func phaseTitle(name string) string {
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

func orDash(s string) string {
	if s == "" {
		return emptyCell
	}
	return s
}

func joinOrDash(items []string) string {
	if len(items) == 0 {
		return emptyCell
	}
	return strings.Join(items, ",")
}
