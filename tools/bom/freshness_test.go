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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCommittedBOMVersionsMatchRegistry asserts that the version column of the
// committed docs/user/container-images.md matches each component's registry
// pinned version. See issue #1424.
//
// TestOverlayVersionPinsMatchRegistry (pkg/recipe) enforces that recipes match
// the registry default; this test enforces the other half of #1424's
// acceptance — that the *committed BOM* matches the registry default too.
// Without it, a coordinated bump (registry defaultVersion and every overlay pin
// moved together) passes the recipe guard even when `make bom-docs` was never
// re-run, leaving the committed doc advertising the old version. `make
// bom-check` catches this but is opt-in and not wired into the merge gate; this
// test runs under `make test` and gates only the version column (no Helm
// rendering / network), so a stale version pin fails CI deterministically.
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
	docVersions := parseBOMVersionTable(t, string(data))
	if len(docVersions) == 0 {
		t.Fatal("no component rows parsed from container-images.md — the version-freshness " +
			"check would be vacuous; verify the doc's Components table format")
	}

	checked := 0
	for _, c := range reg.Components {
		// Only components the BOM renders with a pinned version are gated. A
		// component's pinned version is its Helm defaultVersion or, for
		// kustomize, its defaultTag; manifest components have neither and show
		// "—" in the doc.
		want := c.Helm.DefaultVersion
		if want == "" {
			want = c.Kustomize.DefaultTag
		}
		if want == "" {
			continue
		}

		got, ok := docVersions[c.Name]
		if !ok {
			t.Errorf("component %q (pinned %q) is missing from the Components table in "+
				"docs/user/container-images.md. Run `make bom-docs` and commit the result. See #1424.",
				c.Name, want)
			continue
		}
		checked++
		if got != want {
			t.Errorf("stale BOM: docs/user/container-images.md lists %q for component %q, "+
				"but the registry pins %q.\n"+
				"  Run `make bom-docs` and commit the regenerated doc so the BOM matches what "+
				"recipes install. See #1424.",
				got, c.Name, want)
		}
	}

	if checked == 0 {
		t.Fatal("no pinned components cross-checked against the BOM — the freshness check " +
			"would be vacuous; verify recipes/registry.yaml and the doc table")
	}
	t.Logf("verified %d pinned component versions against docs/user/container-images.md", checked)
}

// parseBOMVersionTable extracts component name -> pinned version from the
// Markdown "Components" table in container-images.md. It locates the header row
// to resolve the Component and Pinned Version column positions rather than
// hard-coding them, so a column reorder does not silently break the mapping.
func parseBOMVersionTable(t *testing.T, doc string) map[string]string {
	t.Helper()

	const (
		componentHeader = "component"
		versionHeader   = "pinned version"
	)

	out := map[string]string{}
	nameCol, verCol := -1, -1
	inTable := false

	for line := range strings.SplitSeq(doc, "\n") {
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
		out[name] = ver
	}
	return out
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
