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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/corroborate"
)

// docsDir is the relative path from tools/corroborate/ to the user docs root.
const docsDir = "../../docs/user"

// evidenceDashboardDoc is the GP6 public docs page that must name every State
// and Class constant.
const evidenceDashboardDoc = "evidence-dashboard.md"

// TestDocsStateNames is the GP6 drift-guard: it verifies that every
// corroboration State constant appears verbatim in the "Consensus model"
// section of docs/user/evidence-dashboard.md, and every source Class constant
// in the "Source classes" section, exactly as the Go code spells them — so the
// docs and the generator can never silently diverge. Scoping each check to its
// defining section (not the whole file) means deleting either table fails the
// test instead of passing on an incidental prose mention elsewhere.
//
// If this test fails after a rename in pkg/corroborate, update
// docs/user/evidence-dashboard.md to match.
func TestDocsStateNames(t *testing.T) {
	t.Parallel()

	docPath := filepath.Join(docsDir, evidenceDashboardDoc)
	data, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v (create docs/user/%s as part of GP6)", docPath, err, evidenceDashboardDoc)
	}
	doc := string(data)

	// Scope each check to the section that is supposed to define the tokens, and
	// assert the token's defining *table row* (a leading "| `<token>` |" cell),
	// not merely that the token appears somewhere in the section. State/Class
	// names also recur in the surrounding prose, so a plain substring check would
	// stay green even if the defining table were deleted — the very drift this
	// guard exists to catch. Requiring the table row means removing either table
	// fails the test.
	consensus := docSection(doc, "Consensus model")
	if consensus == "" {
		t.Fatalf("%s: missing \"## Consensus model\" section", evidenceDashboardDoc)
	}
	for _, st := range []corroborate.State{
		corroborate.StateConfirmed,
		corroborate.StateSingle,
		corroborate.StateContested,
		corroborate.StateFailing,
		corroborate.StateUntested,
	} {
		if row := tableRow(string(st)); !strings.Contains(consensus, row) {
			t.Errorf("%s: Consensus model section is missing the States-table row for %q (expected a cell %q) — the table must define it, not just prose",
				evidenceDashboardDoc, st, row)
		}
	}

	classes := docSection(doc, "Source classes")
	if classes == "" {
		t.Fatalf("%s: missing \"## Source classes\" section", evidenceDashboardDoc)
	}
	for _, cl := range []corroborate.Class{
		corroborate.ClassFirstParty,
		corroborate.ClassCommunity,
		corroborate.ClassPartner,
	} {
		if row := tableRow(string(cl)); !strings.Contains(classes, row) {
			t.Errorf("%s: Source classes section is missing the classes-table row for %q (expected a cell %q) — the table must define it, not just prose",
				evidenceDashboardDoc, cl, row)
		}
	}
}

// tableRow is the leading Markdown-table cell that defines a token, e.g.
// "| `CONFIRMED` |". Matching this (rather than the bare token) ties the guard
// to the defining table row, so deleting the table fails the test even though
// the token still appears in prose.
func tableRow(token string) string {
	return "| `" + token + "` |"
}

// docSection returns the body of the top-level ("## ") Markdown section whose
// heading line is exactly "## "+title, from just after that heading line to the
// next top-level heading line (or end of document). Matching is anchored to
// whole lines, so a "### "+title subsection, a fenced-code line, or prose that
// merely mentions the heading text does not match; nested "### " subsections
// within the section are kept. It returns "" when the section is absent so
// callers fail closed rather than matching a token from a different section.
func docSection(doc, title string) string {
	head := "## " + title
	lines := strings.Split(doc, "\n")
	start := -1
	for i, ln := range lines {
		if strings.TrimRight(ln, " \t") == head {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return ""
	}
	var b strings.Builder
	for _, ln := range lines[start:] {
		// A "## " prefix is exactly two hashes then a space; "### " lines start
		// with "###" and so are not H2 boundaries.
		if strings.HasPrefix(strings.TrimRight(ln, " \t"), "## ") {
			break
		}
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	return b.String()
}
