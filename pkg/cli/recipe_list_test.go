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
	"encoding/json"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/health"
)

// runRecipeList drives `recipe list` end-to-end through urfave/cli with a
// captured writer and returns the rendered output.
func runRecipeList(t *testing.T, args ...string) string {
	t.Helper()
	var buf bytes.Buffer
	parent := &cli.Command{
		Name:     "aicr",
		Commands: []*cli.Command{recipeListCmd()},
		Writer:   &buf,
	}
	runArgs := append([]string{"aicr", "list"}, args...)
	if err := parent.Run(context.Background(), runArgs); err != nil {
		t.Fatalf("recipe list %v: %v", args, err)
	}
	return buf.String()
}

// TestRecipeList_TableHealthColumns asserts the table carries the structural
// status + compact coverage columns atop the #1208 criteria/leaf/source
// columns.
func TestRecipeList_TableHealthColumns(t *testing.T) {
	out := runRecipeList(t)

	header := firstLine(out)
	for _, col := range []string{"NAME", "IS_LEAF", "STATUS", "COVERAGE", "SOURCE"} {
		if !strings.Contains(header, col) {
			t.Errorf("table header missing %q column; got: %s", col, header)
		}
	}

	// At least one leaf row must render a valid status and an R/D/P/C coverage
	// summary.
	if !strings.Contains(out, "R:") || !strings.Contains(out, "C:") {
		t.Errorf("expected a compact coverage cell (R:.. C:..) in output:\n%s", out)
	}
	if !containsAnyStatus(out) {
		t.Errorf("expected at least one rolled-up status token in output:\n%s", out)
	}
}

// TestRecipeList_JSONHealth asserts json carries the full declared_coverage and
// per-dimension status map under a health object, without dropping the #1208
// catalog fields.
func TestRecipeList_JSONHealth(t *testing.T) {
	out := runRecipeList(t, "--format", "json")

	var entries []struct {
		Name     string                  `json:"name"`
		IsLeaf   bool                    `json:"is_leaf"`
		Source   string                  `json:"source"`
		Criteria map[string]any          `json:"criteria"`
		Health   *health.StructureHealth `json:"health"`
	}
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("unmarshal json output: %v\n%s", err, out)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one catalog entry")
	}

	var sawLeafHealth bool
	for _, e := range entries {
		if e.Name == "" {
			t.Error("entry missing #1208 name field")
		}
		if e.Source == "" {
			t.Error("entry missing #1208 source field")
		}
		if !e.IsLeaf {
			if e.Health != nil {
				t.Errorf("non-leaf %q should not carry health", e.Name)
			}
			continue
		}
		if e.Health == nil {
			t.Errorf("leaf %q missing health object", e.Name)
			continue
		}
		sawLeafHealth = true
		if e.Health.Status == "" {
			t.Errorf("leaf %q health missing status", e.Name)
		}
		if e.Health.Dimensions == nil {
			t.Errorf("leaf %q health missing per-dimension status map", e.Name)
		}
		if _, ok := e.Health.Dimensions[health.DimResolves]; !ok {
			t.Errorf("leaf %q health dimensions missing %q", e.Name, health.DimResolves)
		}
		if e.Health.Coverage == nil {
			t.Errorf("leaf %q health missing declared_coverage", e.Name)
		}
	}
	if !sawLeafHealth {
		t.Fatal("expected at least one leaf entry carrying health")
	}
}

// TestRecipeList_YAMLHealth confirms the yaml format emits the health block and
// — critically — keeps the #1208 catalog fields at the TOP LEVEL. yaml.v3 does
// not auto-inline an anonymous struct field the way encoding/json does, so
// without an explicit inline tag the #1208 fields would nest under a
// "catalogentry:" key and break every existing YAML consumer. This test guards
// that regression.
func TestRecipeList_YAMLHealth(t *testing.T) {
	out := runRecipeList(t, "--format", "yaml")
	if strings.Contains(out, "catalogentry:") {
		t.Errorf("yaml nests #1208 fields under catalogentry: (lost inline tag)\n%s", out)
	}
	// The #1208 fields must marshal as top-level mapping keys (list items use a
	// two-space indent, so a top-level key appears as "  name:"). criteria sorts
	// first and renders on the "- " dash line, so it is checked separately.
	for _, key := range []string{"  name:", "  is_leaf:", "  source:"} {
		if !strings.Contains(out, key) {
			t.Errorf("yaml output missing top-level %q\n%s", strings.TrimSpace(key), out)
		}
	}
	if !strings.Contains(out, "- criteria:") {
		t.Errorf("yaml output missing top-level criteria on a list item\n%s", out)
	}
	if !strings.Contains(out, "health:") {
		t.Errorf("yaml output missing health block:\n%s", out)
	}
	if !strings.Contains(out, "coverage:") || !strings.Contains(out, "dimensions:") {
		t.Errorf("yaml health block missing coverage/dimensions:\n%s", out)
	}
}

// TestRecipeList_NonLeafStatusPlaceholder asserts non-leaf rows render the "-"
// placeholder in both the STATUS and COVERAGE cells. "eks-training" is a stable
// non-leaf base overlay in the embedded catalog.
func TestRecipeList_NonLeafStatusPlaceholder(t *testing.T) {
	out := runRecipeList(t)
	row := lineContaining(out, "eks-training ")
	if row == "" {
		t.Fatalf("expected a non-leaf eks-training row in output:\n%s", out)
	}
	fields := strings.Fields(row)
	// Layout: NAME SERVICE ACCELERATOR INTENT OS PLATFORM IS_LEAF STATUS COVERAGE SOURCE
	// A non-leaf row collapses STATUS and COVERAGE to "-", so the trailing
	// fields are: ... false - - embedded (10 fields total).
	if got := len(fields); got != 10 {
		t.Fatalf("expected 10 columns in non-leaf row, got %d: %q", got, row)
	}
	if fields[6] != "false" {
		t.Errorf("eks-training should be non-leaf (IS_LEAF=false), got %q in %q", fields[6], row)
	}
	if fields[7] != healthNotApplicable || fields[8] != healthNotApplicable {
		t.Errorf("non-leaf row should render %q in STATUS/COVERAGE, got STATUS=%q COVERAGE=%q",
			healthNotApplicable, fields[7], fields[8])
	}
}

// TestRecipeList_FilteredJSONHealth drives the CLI filter end-to-end and
// confirms ListCatalog and ComputeHealth agree on the same narrowed set: every
// returned leaf carries a health block and matches the filter.
func TestRecipeList_FilteredJSONHealth(t *testing.T) {
	out := runRecipeList(t, "--service", "eks", "--intent", "training", "--format", "json")

	var entries []struct {
		Name     string                  `json:"name"`
		IsLeaf   bool                    `json:"is_leaf"`
		Criteria map[string]any          `json:"criteria"`
		Health   *health.StructureHealth `json:"health"`
	}
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("unmarshal filtered json: %v\n%s", err, out)
	}
	if len(entries) == 0 {
		t.Fatal("expected eks+training overlays in the catalog")
	}
	var sawLeaf bool
	for _, e := range entries {
		if svc, _ := e.Criteria["Service"].(string); svc != "eks" {
			t.Errorf("filter leaked non-eks overlay %q (service=%v)", e.Name, e.Criteria["Service"])
		}
		if e.IsLeaf {
			sawLeaf = true
			if e.Health == nil {
				t.Errorf("filtered leaf %q missing health block", e.Name)
			}
		}
	}
	if !sawLeaf {
		t.Fatal("expected at least one leaf in the eks+training filter")
	}
}

// TestRecipeList_EmptyResult exercises the no-match branch: an impossible filter
// combination renders the empty-overlays message without panicking on the empty
// health map.
func TestRecipeList_EmptyResult(t *testing.T) {
	out := runRecipeList(t, "--service", "eks", "--accelerator", "gb200", "--os", "cos")
	if !strings.Contains(out, "(no matching overlays)") {
		t.Errorf("expected empty-result message, got:\n%s", out)
	}
}

// TestRecipeList_EmptyResultNoHealth exercises the empty + --no-health
// intersection: the nil healthByName map must not panic when there are zero
// rows, and the empty-overlays message still renders.
func TestRecipeList_EmptyResultNoHealth(t *testing.T) {
	out := runRecipeList(t, "--no-health", "--service", "eks", "--accelerator", "gb200", "--os", "cos")
	if !strings.Contains(out, "(no matching overlays)") {
		t.Errorf("expected empty-result message, got:\n%s", out)
	}
	if strings.Contains(firstLine(out), "STATUS") {
		t.Errorf("--no-health empty result should omit the STATUS column; got header: %s", firstLine(out))
	}
}

// TestRecipeList_NoHealthTable asserts --no-health renders the pre-#1228 table
// shape: the STATUS/COVERAGE columns are gone and every row carries exactly the
// eight #1208 columns, with no coverage/status tokens leaking in.
func TestRecipeList_NoHealthTable(t *testing.T) {
	out := runRecipeList(t, "--no-health")

	header := firstLine(out)
	for _, col := range []string{"NAME", "IS_LEAF", "SOURCE"} {
		if !strings.Contains(header, col) {
			t.Errorf("table header missing %q column; got: %s", col, header)
		}
	}
	for _, col := range []string{"STATUS", "COVERAGE"} {
		if strings.Contains(header, col) {
			t.Errorf("--no-health header should omit %q column; got: %s", col, header)
		}
	}
	// No compact coverage cell should appear anywhere in the body.
	if strings.Contains(out, "R:") || strings.Contains(out, "C:") {
		t.Errorf("--no-health output leaked a coverage cell:\n%s", out)
	}
	// A stable leaf row ("gb200-any") must collapse to exactly 8 fields.
	row := lineContaining(out, "gb200-any ")
	if row == "" {
		t.Fatalf("expected a gb200-any row in --no-health output:\n%s", out)
	}
	if got := len(strings.Fields(row)); got != 8 {
		t.Fatalf("expected 8 columns in --no-health row, got %d: %q", got, row)
	}
}

// TestRecipeList_NoHealthSkipFlagAlias confirms the --skip-health alias is wired
// to the same flag as --no-health and produces byte-identical output.
func TestRecipeList_NoHealthSkipFlagAlias(t *testing.T) {
	alias := runRecipeList(t, "--skip-health")
	primary := runRecipeList(t, "--no-health")
	if alias != primary {
		t.Errorf("--skip-health output should equal --no-health output\nalias:\n%s\nprimary:\n%s", alias, primary)
	}
	if strings.Contains(firstLine(alias), "STATUS") {
		t.Errorf("--skip-health should omit the STATUS column; got header: %s", firstLine(alias))
	}
}

// TestRecipeList_NoHealthJSON asserts --no-health drops the health block from
// every json entry — including leaves that would otherwise carry one — yielding
// the pre-#1228 shape (#1208 fields only).
func TestRecipeList_NoHealthJSON(t *testing.T) {
	out := runRecipeList(t, "--no-health", "--format", "json")

	if strings.Contains(out, "\"health\"") {
		t.Errorf("--no-health json must not contain a health block:\n%s", out)
	}

	var entries []struct {
		Name   string                  `json:"name"`
		IsLeaf bool                    `json:"is_leaf"`
		Source string                  `json:"source"`
		Health *health.StructureHealth `json:"health"`
	}
	if err := json.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("unmarshal json output: %v\n%s", err, out)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one catalog entry")
	}
	var sawLeaf bool
	for _, e := range entries {
		if e.Name == "" || e.Source == "" {
			t.Errorf("entry missing #1208 name/source field: %+v", e)
		}
		if e.IsLeaf {
			sawLeaf = true
		}
		if e.Health != nil {
			t.Errorf("--no-health entry %q should carry no health block", e.Name)
		}
	}
	if !sawLeaf {
		t.Fatal("expected at least one leaf entry to confirm health is suppressed even for leaves")
	}
}

// TestRecipeList_NoHealthYAML asserts --no-health drops the health block from
// the yaml output while keeping the #1208 fields at the top level. It decodes
// the YAML and asserts no entry carries a "health" key (rather than a fragile
// substring scan), and that at least one leaf entry is present — so the test
// fails loudly if the catalog ever renders empty.
func TestRecipeList_NoHealthYAML(t *testing.T) {
	out := runRecipeList(t, "--no-health", "--format", "yaml")

	var entries []map[string]any
	if err := yaml.Unmarshal([]byte(out), &entries); err != nil {
		t.Fatalf("unmarshal yaml output: %v\n%s", err, out)
	}
	if len(entries) == 0 {
		t.Fatal("expected at least one catalog entry in yaml output")
	}
	var sawLeaf bool
	for _, e := range entries {
		if _, ok := e["health"]; ok {
			t.Errorf("--no-health yaml entry %v must not carry a health key", e["name"])
		}
		if e["name"] == nil || e["source"] == nil {
			t.Errorf("yaml entry missing #1208 name/source field: %v", e)
		}
		if leaf, _ := e["is_leaf"].(bool); leaf {
			sawLeaf = true
		}
	}
	if !sawLeaf {
		t.Fatal("expected at least one leaf entry to confirm health is suppressed even for leaves")
	}
}

// TestCompactCoverage exercises the compact coverage formatter directly.
func TestCompactCoverage(t *testing.T) {
	tests := []struct {
		name string
		cov  *health.DeclaredCoverage
		want string
	}{
		{
			name: "nil coverage renders placeholder",
			cov:  nil,
			want: "-",
		},
		{
			name: "all phases empty",
			cov:  &health.DeclaredCoverage{},
			want: "R:0 D:0 P:0 C:0",
		},
		{
			name: "mixed check counts",
			cov: &health.DeclaredCoverage{
				Readiness:   health.PhaseCoverage{Declared: true, Checks: []string{"a", "b"}},
				Deployment:  health.PhaseCoverage{Declared: true, Checks: []string{"a", "b", "c", "d"}},
				Performance: health.PhaseCoverage{Declared: true, Checks: []string{"a"}},
				Conformance: health.PhaseCoverage{Declared: true, Checks: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}},
			},
			want: "R:2 D:4 P:1 C:10",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compactCoverage(tt.cov); got != tt.want {
				t.Errorf("compactCoverage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// lineContaining returns the first line of s containing sub, or "".
func lineContaining(s, sub string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) {
			return line
		}
	}
	return ""
}

func containsAnyStatus(s string) bool {
	for _, st := range []string{health.StatusPass, health.StatusWarn, health.StatusFail, health.StatusUnknown} {
		if strings.Contains(s, st) {
			return true
		}
	}
	return false
}
