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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// AICR-BOM section markers. `make bom-docs` regenerates only the content
// between these markers (see the awk splice in the Makefile), so the freshness
// check parses ONLY that generated region — never the surrounding hand-written
// prose, which could otherwise contain an unrelated pipe table.
const (
	bomBeginMarker = "<!-- BEGIN AICR-BOM -->"
	bomEndMarker   = "<!-- END AICR-BOM -->"
	// bomUnpinnedSentinel is what the Markdown writer renders in the version
	// column for a component with no pinned version (see pkg/bom/markdown.go).
	bomUnpinnedSentinel = "—"
)

// TestCommittedBOMVersionsMatchRegistry asserts that the committed
// docs/user/container-images.md is an exact projection of the registry's
// pinned versions. See issue #1424.
//
// TestOverlayVersionPinsMatchRegistry (pkg/recipe) enforces that recipes match
// the registry default; this test enforces the other half of #1424's
// acceptance — that the *committed BOM* matches the registry too. Without it, a
// coordinated bump (registry defaultVersion and every overlay pin moved
// together) passes the recipe guard even when `make bom-docs` was never re-run,
// leaving the committed doc advertising the old version. `make bom-check`
// catches this by re-rendering but is opt-in; this test gates only the version
// column (no Helm rendering / network) and runs under `make test`, so a stale
// pin fails CI deterministically.
//
// The check is bidirectional and exact:
//   - every registry component must have a row in the generated table
//     (a registry addition that skipped `make bom-docs` fails);
//   - every generated row must correspond to a registry component
//     (a component removed from the registry but left in the doc fails);
//   - duplicate rows for one component are rejected (a hand-edited or
//     doubled row cannot shadow the generated value);
//   - for each pinned component the doc version must equal the registry
//     version for its effective type (Helm defaultVersion or Kustomize
//     defaultTag), matching what the BOM generator emits.
func TestCommittedBOMVersionsMatchRegistry(t *testing.T) {
	// tools/bom is two levels below the repo root; tests run with CWD set to
	// the package directory.
	repoRoot := filepath.Join("..", "..")

	reg, err := loadRegistry(filepath.Join(repoRoot, "recipes", "registry.yaml"))
	if err != nil {
		t.Fatalf("loadRegistry: %v", err)
	}

	docPath := filepath.Join(repoRoot, "docs", "user", "container-images.md")
	data, err := os.ReadFile(docPath) //nolint:gosec // fixed in-repo doc path
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}

	section, err := extractGeneratedSection(string(data))
	if err != nil {
		t.Fatalf("locate generated BOM section in %s: %v\n"+
			"  Run `make bom-docs` to regenerate the doc with its AICR-BOM markers.", docPath, err)
	}

	docVersions, err := parseBOMVersionTable(section)
	if err != nil {
		t.Fatalf("parse Components table in %s: %v", docPath, err)
	}
	if len(docVersions) == 0 {
		t.Fatal("no component rows parsed from the generated section of container-images.md — the " +
			"version-freshness check would be vacuous; verify the doc's Components table format")
	}

	// Bidirectional set comparison BEFORE version comparison: the committed BOM
	// must list exactly the registry's components, no more and no fewer.
	regNames := make(map[string]bool, len(reg.Components))
	for _, c := range reg.Components {
		regNames[c.Name] = true
	}

	var missing, orphan []string
	for name := range regNames {
		if _, ok := docVersions[name]; !ok {
			missing = append(missing, name)
		}
	}
	for name := range docVersions {
		if !regNames[name] {
			orphan = append(orphan, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(orphan)
	for _, n := range missing {
		t.Errorf("component %q is in recipes/registry.yaml but has no row in the Components table of "+
			"docs/user/container-images.md.\n"+
			"  Run `make bom-docs` and commit the regenerated doc. See #1424.", n)
	}
	for _, n := range orphan {
		t.Errorf("stale BOM row %q: it appears in docs/user/container-images.md but is not a component "+
			"in recipes/registry.yaml (removed or renamed?).\n"+
			"  Run `make bom-docs` and commit the regenerated doc so the BOM is an exact registry "+
			"projection. See #1424.", n)
	}

	checked, pinned := 0, 0
	for _, c := range reg.Components {
		got, ok := docVersions[c.Name]
		if !ok {
			// Already reported above as a missing row.
			continue
		}
		checked++

		// Compare EVERY row, not only pinned ones. pinnedVersion selects the
		// field by effective type (Helm defaultVersion or Kustomize
		// defaultTag); a component with neither (manifest, or a Kustomize
		// component whose tag was cleared) has no pin and the generator renders
		// the bomUnpinnedSentinel ("—"). Skipping unpinned rows would let the
		// doc advertise a fabricated version (e.g. a manifest row edited from
		// "—" to "v9.9.9", or a stale tag left after the registry tag was
		// removed) pass silently — so map an empty pin to the sentinel and
		// assert it too.
		want := pinnedVersion(c)
		if want == "" {
			if got != bomUnpinnedSentinel {
				t.Errorf("stale BOM: docs/user/container-images.md lists %q for component %q, "+
					"which has no pinned version in the registry (expected the %q sentinel).\n"+
					"  Run `make bom-docs` and commit the regenerated doc. See #1424.",
					got, c.Name, bomUnpinnedSentinel)
			}
			continue
		}
		pinned++
		if got != want {
			t.Errorf("stale BOM: docs/user/container-images.md lists %q for component %q, "+
				"but the registry pins %q.\n"+
				"  Run `make bom-docs` and commit the regenerated doc so the BOM matches what "+
				"recipes install. See #1424.",
				got, c.Name, want)
		}
	}

	if checked == 0 {
		t.Fatal("no component rows cross-checked against the BOM — the freshness check " +
			"would be vacuous; verify recipes/registry.yaml and the doc table")
	}
	t.Logf("verified %d component rows (%d pinned) against docs/user/container-images.md", checked, pinned)
}

// extractGeneratedSection returns the text between the AICR-BOM begin/end
// markers, exclusive. It requires EXACTLY one begin and one end marker in the
// correct order: a doc missing its generated region, or one with a second
// (stale) generated section appended, fails loudly rather than silently
// parsing only the first pair.
func extractGeneratedSection(doc string) (string, error) {
	if n := strings.Count(doc, bomBeginMarker); n != 1 {
		return "", fmt.Errorf("expected exactly one %q marker, found %d", bomBeginMarker, n)
	}
	if n := strings.Count(doc, bomEndMarker); n != 1 {
		return "", fmt.Errorf("expected exactly one %q marker, found %d", bomEndMarker, n)
	}
	begin := strings.Index(doc, bomBeginMarker) + len(bomBeginMarker)
	end := strings.Index(doc, bomEndMarker)
	if end < begin {
		return "", fmt.Errorf("%q marker appears before %q", bomEndMarker, bomBeginMarker)
	}
	return doc[begin:end], nil
}

// parseBOMVersionTable extracts component name -> pinned version from the
// Markdown "Components" table in the generated section. It resolves the
// Component and Pinned Version columns from the header row (so a column reorder
// does not silently break the mapping) and rejects duplicate component rows so
// a doubled or hand-edited row cannot shadow the generated value.
func parseBOMVersionTable(section string) (map[string]string, error) {
	const (
		componentHeader = "component"
		versionHeader   = "pinned version"
	)

	out := map[string]string{}
	nameCol, verCol := -1, -1
	inTable := false

	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "|") {
			// A blank/non-table line ends the current table; reset so a later
			// unrelated pipe table cannot be misread with these columns.
			if trimmed == "" {
				inTable = false
				nameCol, verCol = -1, -1
			}
			continue
		}

		cells := splitMarkdownRow(trimmed)

		// Until a header row is found, keep searching every pipe row. Both
		// columns must resolve in the SAME row before we commit — a row that
		// carries only one of the two headers is not a match and does not stop
		// the search, so a preceding unrelated table cannot lock us out.
		if !inTable {
			n, v := -1, -1
			for i, c := range cells {
				switch strings.ToLower(strings.TrimSpace(c)) {
				case componentHeader:
					n = i
				case versionHeader:
					v = i
				}
			}
			if n >= 0 && v >= 0 {
				nameCol, verCol = n, v
				inTable = true
			}
			continue
		}

		// Separator row (|---|---|); skip.
		if strings.Trim(trimmed, "|-: ") == "" {
			continue
		}
		if nameCol >= len(cells) || verCol >= len(cells) {
			continue
		}

		name := strings.TrimSpace(cells[nameCol])
		ver := strings.TrimSpace(cells[verCol])
		if name == "" || ver == "" {
			continue
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("duplicate row for component %q in the Components table — "+
				"a doubled or hand-edited row cannot shadow the generated value; run `make bom-docs`", name)
		}
		out[name] = ver
	}
	return out, nil
}

// isDelimiterRow reports whether cells form a valid GFM delimiter row for a
// table with want columns: exactly want cells, each matching `:?-+:?`.
func isDelimiterRow(cells []string, want int) bool {
	if len(cells) != want {
		return false
	}
	for _, c := range cells {
		c = strings.TrimSpace(c)
		c = strings.TrimPrefix(c, ":")
		c = strings.TrimSuffix(c, ":")
		if c == "" || strings.Trim(c, "-") != "" {
			return false
		}
	}
	return true
}

// splitMarkdownRow splits a "| a | b | c |" row into its trimmed inner cells,
// dropping the empty leading/trailing segments the outer pipes produce.
func splitMarkdownRow(row string) []string {
	parts := strings.Split(row, "|")
	cells := make([]string, 0, len(parts))
	for _, p := range parts {
		cells = append(cells, strings.TrimSpace(p))
	}
	// Drop the empty fields created by the leading and trailing pipe.
	if len(cells) > 0 && cells[0] == "" {
		cells = cells[1:]
	}
	if len(cells) > 0 && cells[len(cells)-1] == "" {
		cells = cells[:len(cells)-1]
	}
	return cells
}

// TestCommittedBOMVariantsMatchRecipePins asserts that the committed doc's
// "Version variants" table is an exact projection of the variants derived
// from recipe data: every explicit base/overlay/mixin Helm pin that differs
// from its registry default (issue #1611). The check is bidirectional and
// exact, mirroring TestCommittedBOMVersionsMatchRegistry:
//   - every derived variant must have a row (a new divergent pin that skipped
//     `make bom-docs` fails);
//   - every row must correspond to a derived variant (a re-aligned or removed
//     pin that left a stale row fails);
//   - the Declared By column must list exactly the sorted declaring sources;
//   - when nothing diverges, the table must be absent entirely.
//
// Variant discovery reads the registry and recipe sources from the repo root
// and deliberately has no dependency on the version-pin guard's exemption
// policy: the pins are the source facts; the guard decides whether a
// divergence is ALLOWED, not what is deployed.
func TestCommittedBOMVariantsMatchRecipePins(t *testing.T) {
	repoRoot := filepath.Join("..", "..")

	reg, err := loadRegistry(filepath.Join(repoRoot, "recipes", "registry.yaml"))
	if err != nil {
		t.Fatalf("loadRegistry: %v", err)
	}
	sources, err := loadRecipeSources(context.Background(), repoRoot)
	if err != nil {
		t.Fatalf("loadRecipeSources: %v", err)
	}
	derived, err := deriveVariants(reg, sources)
	if err != nil {
		t.Fatalf("deriveVariants: %v", err)
	}

	docPath := filepath.Join(repoRoot, "docs", "user", "container-images.md")
	data, err := os.ReadFile(docPath) //nolint:gosec // fixed in-repo doc path
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	section, err := extractGeneratedSection(string(data))
	if err != nil {
		t.Fatalf("locate generated BOM section in %s: %v", docPath, err)
	}

	docVariants, tablePresent, err := parseBOMVariantsTable(section)
	if err != nil {
		t.Fatalf("parse Version variants table in %s: %v", docPath, err)
	}

	if len(derived) == 0 {
		// The generator omits the table entirely when nothing diverges, so a
		// PRESENT table — even an empty one — is stale doc state, not merely
		// zero rows.
		if tablePresent {
			t.Fatalf("no divergent pins derived from recipes, but the doc still has a Version "+
				"variants table (%d rows).\n"+
				"  Run `make bom-docs` and commit the regenerated doc. See #1611.", len(docVariants))
		}
		t.Log("no divergent pins and no variants table — vacuously consistent")
		return
	}
	if !tablePresent {
		t.Fatalf("%d variants derived from recipe pins, but the doc has no Version variants table.\n"+
			"  Run `make bom-docs` and commit the regenerated doc. See #1611.", len(derived))
	}

	derivedByKey := make(map[variantKey][]string, len(derived))
	for _, v := range derived {
		derivedByKey[variantKey{name: v.Name, version: v.Version}] = v.Sources
	}

	for k, wantSources := range derivedByKey {
		gotSources, ok := docVariants[k]
		if !ok {
			t.Errorf("derived variant %s@%s (pinned by %s) has no row in the Version variants table of "+
				"docs/user/container-images.md.\n"+
				"  Run `make bom-docs` and commit the regenerated doc. See #1611.",
				k.name, k.version, strings.Join(wantSources, ", "))
			continue
		}
		if want := strings.Join(wantSources, ", "); gotSources != want {
			t.Errorf("variant %s@%s Declared By = %q, want %q.\n"+
				"  Run `make bom-docs` and commit the regenerated doc. See #1611.",
				k.name, k.version, gotSources, want)
		}
	}
	for k := range docVariants {
		if _, ok := derivedByKey[k]; !ok {
			t.Errorf("stale variant row %s@%s: it appears in docs/user/container-images.md but no "+
				"base/overlay/mixin pin diverges from the registry default at that version.\n"+
				"  Run `make bom-docs` and commit the regenerated doc. See #1611.", k.name, k.version)
		}
	}
	t.Logf("verified %d variant rows against %d derived variants", len(docVariants), len(derived))
}

// parseBOMVariantsTable extracts (component, variant version) -> declared-by
// from the "Version variants" table, plus whether the table was present at
// all (the generator omits it entirely when nothing diverges, so absence and
// emptiness are distinct facts). The header must match the generator's exact
// four columns — a hand edit that drops or renames a column (e.g. Images)
// must fail the gate loudly (via the heading/table mismatch below), not
// parse a narrower table; the headers also differ from the Components table
// ("Variant Version" vs "Pinned Version") so the two parsers cannot conflate
// tables. Malformed rows are errors, not skips — a silently dropped row
// would weaken the bidirectional guarantee.
func parseBOMVariantsTable(section string) (map[variantKey]string, bool, error) {
	// The exact header pkg/bom's writeMarkdownDoc emits, lowercased.
	wantHeader := []string{"component", "variant version", "declared by", "images"}
	const (
		nameCol = 0
		verCol  = 1
		srcCol  = 2
	)

	const sectionHeading = "## version variants"

	out := map[variantKey]string{}
	headerCols := 0
	inTable := false
	expectSeparator := false
	// underHeading tracks whether the scanner is inside the "## Version
	// variants" SECTION (between its heading and the next "## " heading):
	// a matching table elsewhere in the doc must not count, and a variants
	// heading whose own table is missing must fail the heading/table match.
	underHeading := false
	headings, tables := 0, 0

	for _, line := range strings.Split(section, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.EqualFold(trimmed, sectionHeading) {
			headings++
			underHeading = true
			continue
		}
		if strings.HasPrefix(trimmed, "## ") {
			underHeading = false
		}
		if !strings.HasPrefix(trimmed, "|") {
			// A matched header MUST be immediately followed by its delimiter
			// row; a blank line, prose, or a new heading here means the
			// delimiter is missing, so fail closed rather than silently
			// resetting state and counting the table as present-but-empty.
			if expectSeparator {
				return nil, headings > 0, fmt.Errorf("Version variants table header is not followed by " +
					"a valid Markdown delimiter row — run `make bom-docs`")
			}
			// ANY non-table line (blank, prose, or a new heading) ends the
			// current table: rows may not resume after an interruption, and
			// a table state must never survive into a different section.
			inTable = false
			expectSeparator = false
			continue
		}

		cells := splitMarkdownRow(trimmed)
		if !inTable {
			if !underHeading {
				continue
			}
			match := len(cells) == len(wantHeader)
			for i := 0; match && i < len(cells); i++ {
				match = strings.ToLower(strings.TrimSpace(cells[i])) == wantHeader[i]
			}
			if match {
				headerCols = len(cells)
				inTable = true
				expectSeparator = true
				tables++
			}
			continue
		}

		// GFM requires the delimiter row for a table to exist at all, with
		// one `---` cell per header column; a header followed by data rows,
		// a bare "||||", or a one-cell delimiter renders as plain text (or a
		// different table), so the gate must reject rather than parse it.
		if expectSeparator {
			if !isDelimiterRow(cells, headerCols) {
				return nil, headings > 0, fmt.Errorf("Version variants table header is not followed by "+
					"a valid Markdown delimiter row (got %q) — run `make bom-docs`", trimmed)
			}
			expectSeparator = false
			continue
		}
		if len(cells) != headerCols {
			return nil, headings > 0, fmt.Errorf("malformed Version variants row %q: %d cells, want the "+
				"header's %d — run `make bom-docs`", trimmed, len(cells), headerCols)
		}
		name := strings.TrimSpace(cells[nameCol])
		ver := strings.TrimSpace(cells[verCol])
		src := strings.TrimSpace(cells[srcCol])
		if name == "" || ver == "" {
			return nil, headings > 0, fmt.Errorf("malformed Version variants row %q: empty component or "+
				"version cell — run `make bom-docs`", trimmed)
		}
		k := variantKey{name: name, version: ver}
		if _, dup := out[k]; dup {
			return nil, headings > 0, fmt.Errorf("duplicate row for variant %s@%s in the Version variants "+
				"table — run `make bom-docs`", name, ver)
		}
		out[k] = src
	}

	// Presence is anchored on the "## Version variants" HEADING, not merely a
	// well-formed table header: a heading whose table is missing/malformed,
	// a table without its heading, or duplicated sections are all stale or
	// hand-mangled doc state.
	present := headings > 0
	// The header may be the last line of the section, so the loop can end
	// while still awaiting the delimiter row: fail closed here too.
	if expectSeparator {
		return nil, present, fmt.Errorf("Version variants table header is not followed by a valid " +
			"Markdown delimiter row — run `make bom-docs`")
	}
	if headings > 1 || tables > 1 {
		return nil, present, fmt.Errorf("expected at most one Version variants section, found %d headings "+
			"and %d tables — run `make bom-docs`", headings, tables)
	}
	if headings != tables {
		return nil, present, fmt.Errorf("Version variants heading/table mismatch (%d headings, %d tables) — "+
			"run `make bom-docs`", headings, tables)
	}
	return out, present, nil
}
