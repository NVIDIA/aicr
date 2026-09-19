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
	"bytes"
	"encoding/json"
	stderrors "errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// The report is compared byte for byte against a committed golden rather than
// by substring match: the value is in the diff, and a substring check passes
// happily while a column, a step or a whole detail block silently disappears.
var update = flag.Bool("update", false, "update golden files")

// Every record and version below is synthetic. ADR-021's testing strategy
// forbids asserting a verdict for a real component, and nothing here reads the
// registry.
func syntheticSet() Set {
	return Set{
		// A safe record whose claim reaches past the jump it is being asked
		// about, which is the width the report has to state.
		"alpha-operator": {
			Component: "alpha-operator",
			Transitions: []Transition{{
				From:       "<1.3.0",
				To:         ">=1.2.1 <1.3.0",
				Verdict:    VerdictSafe,
				VerifiedBy: "uat: synthetic lane",
				Summary:    "Patch releases only. No schema or API changes.",
			}},
		},
		// Deployer-scoped steps: the GitOps path is one atomic commit, the
		// imperative path cannot merge the two operations.
		"beta-operator": {
			Component: "beta-operator",
			Transitions: []Transition{{
				From:         "<0.18.0",
				To:           ">=0.18.0 <0.20.0",
				Verdict:      VerdictManual,
				Summary:      "The legacy API group is renamed. A mirror controller copies existing objects.",
				Precondition: "No object is mid-rollout and no node is mid-package.",
				StepsByDeployer: []StepGroup{
					{
						Deployers: []string{"argocd", "argocd-helm", "flux"},
						Steps: []Step{{
							ID:          "rename-crs",
							Description: "In a single commit, remove the legacy manifests and add their replacements.",
							Reason:      "one commit lets the controller prune and adopt in a single sync.",
						}},
					},
					{
						Steps: []Step{
							{ID: "rename-crs", Description: "Rewrite apiVersion and kind, then apply."},
							{
								ID:          "delete-legacy-crs",
								Description: "Delete the legacy objects once their replacements are reconciling.",
								Reason:      "the admission webhook rejects the policy deletion while referencing objects exist.",
							},
						},
					},
				},
			}},
		},
		// One authored blocked record covering exactly this jump: the verdict
		// says not in one step, and the record's own steps say what to do
		// instead, so they render.
		"kappa-operator": {
			Component: "kappa-operator",
			Transitions: []Transition{{
				From:         "<4.0.0",
				To:           ">=4.0.0 <=4.0.0",
				Verdict:      VerdictBlocked,
				Summary:      "4.0.0 drops in-place conversion; the store has to be exported and reloaded.",
				Precondition: "No writer is connected to the store.",
				StepsByDeployer: []StepGroup{{Steps: []Step{
					{
						ID:          "export-store",
						Description: "Export the store with the 3.x tooling before touching the release.",
						Reason:      "4.0.0 cannot read the 3.x on-disk format, and the converter was removed.",
					},
					{
						ID:          "land-on-3-9-first",
						Description: "Upgrade to 3.9.x, reload the export there, then move to 4.0.0.",
					},
				}}},
			}},
		},
		// A record exists, but the jump falls entirely below its boundary. The
		// NOTES cell has to say that rather than "no record", which would send
		// a reader off to author one that is already written.
		"lambda-operator": {
			Component: "lambda-operator",
			Transitions: []Transition{{
				From: "<5.0.0", To: ">=5.0.0 <=5.0.0", Verdict: VerdictSafe,
				VerifiedBy: "uat: synthetic lane",
			}},
		},
		// One record covers the source, but the target lands above the ceiling
		// its `to` names, so the record's claim does not reach the move.
		"mu-operator": {
			Component: "mu-operator",
			Transitions: []Transition{{
				From: "<1.18.0", To: ">=1.18.0 <1.19.0", Verdict: VerdictSafe,
				VerifiedBy: "uat: synthetic lane",
			}},
		},
		// Two blocks across one jump: the report names where to stop instead
		// of composing both records' steps.
		"gamma-operator": {
			Component: "gamma-operator",
			Transitions: []Transition{
				{
					From:    "<2.0.0",
					To:      ">=2.0.0 <3.0.0",
					Verdict: VerdictManual,
					Summary: "Storage format changes; an in-place converter runs once.",
					StepsByDeployer: []StepGroup{{Steps: []Step{
						{ID: "run-converter", Description: "Run the bundled converter job before upgrading."},
					}}},
				},
				{
					From:    "<3.0.0",
					To:      ">=3.0.0 <=3.0.0",
					Verdict: VerdictManual,
					Summary: "The legacy CRD is removed outright.",
					StepsByDeployer: []StepGroup{{Steps: []Step{
						{ID: "confirm-migration", Description: "Confirm no legacy objects remain."},
					}}},
				},
			},
		},
	}
}

func syntheticTables() (from, to map[string]string) {
	from = map[string]string{
		"alpha-operator":   "1.2.0",
		"beta-operator":    "0.17.2",
		"gamma-operator":   "1.0.0",
		"delta-operator":   "0.18.0",
		"epsilon-operator": "main",
		"eta-operator":     "2.4.1",
		"theta-operator":   "0.13.0",
		"iota-operator":    "1.1.0",
		"kappa-operator":   "3.2.0",
		"lambda-operator":  "1.1.0",
		"mu-operator":      "1.17.0",
	}
	to = map[string]string{
		"alpha-operator":   "1.2.3",
		"beta-operator":    "0.18.1",
		"gamma-operator":   "3.0.0",
		"delta-operator":   "0.19.0",
		"epsilon-operator": "release-1",
		"zeta-operator":    "0.4.0",
		"theta-operator":   "0.11.0",
		"iota-operator":    "1.1.0",
		"kappa-operator":   "4.0.0",
		"lambda-operator":  "1.2.0",
		"mu-operator":      "1.25.0",
	}
	return from, to
}

// mixedReport covers every verdict and every change kind in one render: safe,
// manual, three shapes of blocked (gamma-operator, where two records are
// crossed and neither describes the jump, kappa-operator, where one record
// does, and mu-operator, where the one record that covers the source stops
// assessing below the target), both shapes of unknown (delta-operator with no
// record at all, lambda-operator with one no boundary falls inside),
// unversioned, added, removed, and a component that did not move at all
// (iota-operator, which produces no row).
func mixedReport(t *testing.T) *Report {
	t.Helper()
	from, to := syntheticTables()
	results := Match(syntheticSet(), from, to)
	if !RequiresDeployer(results) {
		t.Fatal("fixture no longer needs a deployer; it is supposed to contain a manual verdict")
	}
	return NewReport(results, ReportOptions{
		From:     "./bundles-v0.16.0",
		To:       "./bundles-v0.17.0",
		Deployer: "argocd",
	})
}

// replacementReport is its own golden because a replacement renders as one row
// whose FROM column carries a component name rather than a version.
func replacementReport(t *testing.T) *Report {
	t.Helper()
	set := Set{
		"newgate": {
			Component: "newgate",
			Replaces: &Replaces{
				Component:  "oldgate",
				Verdict:    VerdictManual,
				VerifiedBy: "uat: synthetic lane",
				Summary:    "oldgate is superseded by newgate for inference routing.",
				StepsByDeployer: []StepGroup{{Steps: []Step{
					{
						ID:          "port-route-resources",
						Description: "Re-author the oldgate route resources as newgate equivalents.",
						Reason:      "nothing migrates them automatically; field names and defaults differ.",
					},
					{
						ID:          "retire-oldgate",
						Description: "Uninstall the oldgate release once newgate is serving traffic.",
						Reason:      "AICR leaves it installed and will not remove it.",
					},
				}}},
			},
		},
	}
	results := Match(set,
		map[string]string{"oldgate": "1.9.0"},
		map[string]string{"newgate": "1.3.1"})
	return NewReport(results, ReportOptions{Deployer: "helm"})
}

// clusterSourceReport is a `--from cluster` run whose read found its
// components. Every count differs from every other, so a renderer that crosses
// two of them changes this golden: the Helm records and the Argo Applications
// are the pair that matters, being in different units.
func clusterSourceReport(t *testing.T) *Report {
	t.Helper()
	results := Match(syntheticSet(),
		map[string]string{"alpha-operator": "1.2.0", "lambda-operator": "1.1.0"},
		map[string]string{"alpha-operator": "1.2.3", "lambda-operator": "1.2.0"})
	return NewReport(results, ReportOptions{
		From:     "cluster",
		To:       "./bundles-v0.17.0",
		Deployer: "argocd",
		Source: &ReportSource{
			Kubeconfig: "/home/op/.kube/config",
			Context:    "prod-east",
			Matched:    2,
			Helm: ReportSourceHelm{
				Records: 37, Unattributed: 2, Unreadable: 1, Uninstalled: 3, StampedUnmatched: 0,
			},
			Argo: ReportSourceArgo{Applications: 5, Unattributed: 6, Unreadable: 7},
		},
	})
}

// emptyClusterReport is the case the banner exists for: the read matched
// nothing, so every row reads "added" and a reader skimming the rows alone
// would conclude the cluster is bare. The stamped-but-unmatched count is
// non-zero here because that is the realistic pairing — AICR wrote those
// records and the mapping no longer recognizes them.
func emptyClusterReport(t *testing.T) *Report {
	t.Helper()
	results := Match(syntheticSet(),
		map[string]string{},
		map[string]string{"alpha-operator": "1.2.3", "zeta-operator": "0.4.0"})
	return NewReport(results, ReportOptions{
		From:     "cluster",
		To:       "./bundles-v0.17.0",
		Deployer: "helm",
		Source: &ReportSource{
			Kubeconfig: "/home/op/.kube/config",
			Context:    "staging-west",
			Matched:    0,
			Helm: ReportSourceHelm{
				Records: 12, Unattributed: 1, Unreadable: 2, Uninstalled: 4, StampedUnmatched: 5,
			},
			Argo: ReportSourceArgo{},
		},
	})
}

func TestWriteTableGolden(t *testing.T) {
	tests := []struct {
		name   string
		golden string
		report func(*testing.T) *Report
	}{
		{"mixed", "report-mixed.golden", mixedReport},
		{"replacement", "report-replacement.golden", replacementReport},
		{"no changes", "report-empty.golden", func(*testing.T) *Report {
			return NewReport(nil, ReportOptions{From: "a.yaml", To: "b.yaml"})
		}},
		{"cluster source", "report-cluster-source.golden", clusterSourceReport},
		{"cluster matched nothing", "report-cluster-empty.golden", emptyClusterReport},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteTable(&buf, tt.report(t)); err != nil {
				t.Fatalf("WriteTable: %v", err)
			}
			compareGolden(t, tt.golden, buf.Bytes())
		})
	}
}

// TestReportJSONGolden pins the machine-readable shape a pipeline consumes.
// The CLI hands the same struct to pkg/serializer, which indents JSON with two
// spaces, so the golden is what a `--format json` run writes.
func TestReportJSONGolden(t *testing.T) {
	got, err := json.MarshalIndent(mixedReport(t), "", "  ")
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	compareGolden(t, "report-mixed.json.golden", append(got, '\n'))
}

func compareGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("create testdata dir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %s", path)
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // fixed test-local path
	if err != nil {
		t.Fatalf("read %s: %v\n\nRegenerate with:\n  go test ./pkg/upgrade/ -update", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s does not match.\n--- want ---\n%s\n--- got ---\n%s\n\nIf the change is intended:\n"+
			"  go test ./pkg/upgrade/ -update", path, want, got)
	}
}

func TestWriteTableRejectsMalformedCalls(t *testing.T) {
	tests := []struct {
		name   string
		writer *bytes.Buffer
		report *Report
	}{
		{"nil writer", nil, &Report{}},
		{"nil report", &bytes.Buffer{}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var w *bytes.Buffer
			if tt.writer != nil {
				w = tt.writer
			}
			var err error
			if w == nil {
				err = WriteTable(nil, tt.report)
			} else {
				err = WriteTable(w, tt.report)
			}
			if err == nil {
				t.Fatal("WriteTable accepted a malformed call, want ErrCodeInvalidRequest")
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

// TestWriteTablePropagatesWriteFailure covers the broken-pipe and full-disk
// path: a failing writer must surface rather than producing a silently short
// report.
func TestWriteTablePropagatesWriteFailure(t *testing.T) {
	err := WriteTable(failingWriter{}, mixedReport(t))
	if err == nil {
		t.Fatal("WriteTable on a failing writer returned nil")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {
		t.Errorf("error = %v, want ErrCodeInternal", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, stderrors.New("synthetic write failure")
}

// TestWriteTableEscapesArtifactControlCharacters pins that no artifact-derived
// field can emit a control byte into the table.
//
// Component names, versions and artifact paths come from a recipe or bundle the
// operator did not necessarily write, and the table is the surface they read a
// verdict off. Before this escaping, a crafted component name carrying a newline
// and an ANSI color sequence rendered as an extra row reading "safe", which is
// exactly the false confidence the verdicts exist to prevent.
func TestWriteTableEscapesArtifactControlCharacters(t *testing.T) {
	t.Parallel()

	forged := "grove\n\x1b[32mgpu-operator  v1.0.0  v2.0.0  safe      forged\x1b[0m"
	results := []ComponentResult{{
		Component:   forged,
		Change:      ChangeVersion,
		From:        "1.0.0\tspoof",
		To:          "2.0.0\r",
		Verdict:     VerdictUnknown,
		Reason:      ReasonNoRecord,
		Explanation: "prose\x1b[31m with \x07 bell",
	}}
	report := NewReport(results, ReportOptions{
		From: "from\nFORGED HEADER", To: "to\x1b[1m", Deployer: "helm",
	})

	var sb strings.Builder
	if err := WriteTable(&sb, report); err != nil {
		t.Fatalf("WriteTable() error = %v", err)
	}
	got := sb.String()

	for _, bad := range []struct{ name, seq string }{
		{"ESC", "\x1b"},
		{"carriage return", "\r"},
		{"bell", "\x07"},
	} {
		if strings.Contains(got, bad.seq) {
			t.Errorf("%s survived into table output:\n%s", bad.name, got)
		}
	}
	// The injected newline must not start a line of its own.
	if strings.Contains(got, "\nFORGED HEADER") {
		t.Errorf("an injected newline forged a header line:\n%s", got)
	}
	for _, want := range []string{`\n`, `\t`, `\r`, `\x1b`} {
		if !strings.Contains(got, want) {
			t.Errorf("control character not rendered visibly as %q:\n%s", want, got)
		}
	}
	// The real verdict still reads correctly beside the inert text.
	if !strings.Contains(got, "unknown") {
		t.Errorf("the true verdict is missing:\n%s", got)
	}
}

// TestWriteTableEscapesDetailBlock covers the prose path the row test cannot:
// writeDetail renders only for manual and blocked verdicts.
func TestWriteTableEscapesDetailBlock(t *testing.T) {
	t.Parallel()

	results := []ComponentResult{{
		Component: "comp", Change: ChangeVersion, From: "1.0.0", To: "2.0.0",
		Verdict: VerdictManual, Reason: ReasonRecorded,
		Explanation: "why\x07 bell",
		Transition: &Transition{
			Verdict:      VerdictManual,
			Summary:      "summary\x1b[31m red",
			Precondition: "pre\r\nline",
			StepsByDeployer: []StepGroup{{
				Steps: []Step{{
					ID:          "step\x1b[1m",
					Description: "do\nthis",
					Reason:      "because\x00nul",
				}},
			}},
		},
	}}
	report := NewReport(results, ReportOptions{From: "f", To: "t", Deployer: "helm"})

	var sb strings.Builder
	if err := WriteTable(&sb, report); err != nil {
		t.Fatalf("WriteTable() error = %v", err)
	}
	got := sb.String()

	for _, bad := range []struct{ name, seq string }{
		{"ESC", "\x1b"},
		{"carriage return", "\r"},
		{"bell", "\x07"},
		{"NUL", "\x00"},
	} {
		if strings.Contains(got, bad.seq) {
			t.Errorf("%s survived into the detail block:\n%s", bad.name, got)
		}
	}
	for _, want := range []string{`\x07`, `\x1b`, `\x00`, `\r`, `\n`} {
		if !strings.Contains(got, want) {
			t.Errorf("control character not rendered visibly as %q:\n%s", want, got)
		}
	}
}

// TestReportClusterSourceJSONGolden pins the machine-readable shape of the
// source block, which a pipeline reads instead of the table.
func TestReportClusterSourceJSONGolden(t *testing.T) {
	got, err := json.MarshalIndent(clusterSourceReport(t), "", "  ")
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	compareGolden(t, "report-cluster-source.json.golden", append(got, '\n'))
}

// TestWriteTableRendersSourceAboveTheRows pins the ordering the banner's whole
// purpose rests on. A source block rendered below a long table is read after
// the reader has already drawn a conclusion from rows that all say "added".
func TestWriteTableRendersSourceAboveTheRows(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := WriteTable(&buf, emptyClusterReport(t)); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	got := buf.String()

	source := strings.Index(got, "READ FROM CLUSTER")
	banner := strings.Index(got, "NOTHING INSTALLED WAS RECOGNIZED")
	rows := strings.Index(got, "\nCOMPONENT")
	if source < 0 || banner < 0 || rows < 0 {
		t.Fatalf("missing section: source=%d banner=%d rows=%d\n%s", source, banner, rows, got)
	}
	if source > rows {
		t.Errorf("the source block renders below the rows, at %d against %d:\n%s", source, rows, got)
	}
	if banner > rows {
		t.Errorf("the zero-match banner renders below the rows, at %d against %d:\n%s", banner, rows, got)
	}
}

// TestWriteTableZeroMatchBanner pins that the banner fires exactly when the
// read matched nothing, and that it names the two causes an operator acts on.
// Without it an empty cluster and a kubeconfig on the wrong context render
// identically: every row reads "added" either way.
func TestWriteTableZeroMatchBanner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		report     func(*testing.T) *Report
		wantBanner bool
	}{
		{"matched nothing", emptyClusterReport, true},
		{"matched something", clusterSourceReport, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			if err := WriteTable(&buf, tt.report(t)); err != nil {
				t.Fatalf("WriteTable: %v", err)
			}
			got := buf.String()
			if strings.Contains(got, "NOTHING INSTALLED WAS RECOGNIZED") != tt.wantBanner {
				t.Fatalf("zero-match banner present = %v, want %v:\n%s",
					!tt.wantBanner, tt.wantBanner, got)
			}
			if !tt.wantBanner {
				return
			}
			// The two causes are the point of the banner: an operator who
			// reads only "nothing was found" has no next action.
			for _, want := range []string{"not the cluster you meant", "deployer"} {
				if !strings.Contains(got, want) {
					t.Errorf("the banner does not name %q as a cause:\n%s", want, got)
				}
			}
		})
	}
}

// TestWriteTableArgoStampIsNotApplicable pins that the Argo line never reports
// a stamped-but-unmatched count.
//
// Argo has no such count to report: the generated Application carries only a
// sync-wave annotation, and the AICR stamp lives in the wrapper Chart.yaml the
// Application points at, which the reader never opens. Printing a zero there
// would read as "the mapping checked out" when nothing was checked.
func TestWriteTableArgoStampIsNotApplicable(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := WriteTable(&buf, emptyClusterReport(t)); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	argo := lineWithPrefix(t, buf.String(), "  argo ")
	if strings.Contains(argo, "stamped but unmatched") {
		t.Errorf("the argo line reports a stamped-unmatched count it never measured: %q", argo)
	}
	if !strings.Contains(argo, "no stamp to check") {
		t.Errorf("the argo line does not say the stamp check does not apply: %q", argo)
	}

	// The Helm line does carry the count, so the absence above is a statement
	// about Argo rather than the renderer dropping the field everywhere.
	helm := lineWithPrefix(t, buf.String(), "  helm ")
	if !strings.Contains(helm, "5 stamped but unmatched") {
		t.Errorf("the helm line does not report its stamped-unmatched count: %q", helm)
	}
}

// TestWriteTableSourceCountsAreNotInterchangeable pins each reader's count to
// its own label and its own unit. The two must never be summed — a Helm
// storage record is one revision of one release, an Argo Application is one
// component — and rendering them through one shared field name is how that
// distinction is lost.
func TestWriteTableSourceCountsAreNotInterchangeable(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := WriteTable(&buf, clusterSourceReport(t)); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	out := buf.String()

	tests := []struct {
		name   string
		prefix string
		want   []string
		reject []string
	}{
		{
			name:   "helm",
			prefix: "  helm ",
			want: []string{
				"37 storage records", "2 unattributed", "1 unreadable",
				"3 uninstalled", "0 stamped but unmatched",
			},
			reject: []string{"application"},
		},
		{
			name:   "argo",
			prefix: "  argo ",
			want:   []string{"5 applications", "6 unattributed", "7 unreadable"},
			reject: []string{"storage record", "uninstalled"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			line := lineWithPrefix(t, out, tt.prefix)
			for _, want := range tt.want {
				if !strings.Contains(line, want) {
					t.Errorf("%q missing from the %s line: %q", want, tt.name, line)
				}
			}
			for _, bad := range tt.reject {
				if strings.Contains(line, bad) {
					t.Errorf("%q leaked into the %s line from the other reader: %q", bad, tt.name, line)
				}
			}
		})
	}
}

func lineWithPrefix(t *testing.T, out, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("no line starting %q in:\n%s", prefix, out)
	return ""
}

// TestWriteTableOmitsSourceForArtifactComparison pins that a recipe-to-recipe
// or bundle-to-bundle run says nothing about a cluster it never read.
func TestWriteTableOmitsSourceForArtifactComparison(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := WriteTable(&buf, mixedReport(t)); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	if got := buf.String(); strings.Contains(got, "READ FROM CLUSTER") {
		t.Errorf("an artifact comparison rendered a cluster source block:\n%s", got)
	}
}

// TestWriteTableRendersUnknownSourceFieldsVisibly pins that a kubeconfig or
// context the caller could not resolve renders as an explicit gap rather than
// as a dropped line. An in-cluster run has no file and a merged multi-file
// KUBECONFIG has no single path, and a silently missing line reads as "nothing
// to say" rather than "not known".
func TestWriteTableRendersUnknownSourceFieldsVisibly(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	report := NewReport(nil, ReportOptions{From: "cluster", Source: &ReportSource{Matched: 3}})
	if err := WriteTable(&buf, report); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	out := buf.String()
	for _, prefix := range []string{"  kubeconfig ", "  context    "} {
		line := lineWithPrefix(t, out, prefix)
		if !strings.HasSuffix(line, "-") {
			t.Errorf("unresolved field did not render as a gap: %q", line)
		}
	}
}

// TestWriteTableEscapesSourceBlock covers the fields the cluster read adds to
// the terminal surface. A context name comes out of a kubeconfig the operator
// may not have written, and the source block sits above the verdicts, which is
// the best place in the output from which to forge one.
func TestWriteTableEscapesSourceBlock(t *testing.T) {
	t.Parallel()

	report := NewReport(nil, ReportOptions{
		From: "cluster",
		Source: &ReportSource{
			Kubeconfig: "/tmp/kc\nFORGED LINE",
			Context:    "ctx\x1b[31m red\r",
			Matched:    1,
		},
	})
	var buf bytes.Buffer
	if err := WriteTable(&buf, report); err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	got := buf.String()

	for _, bad := range []struct{ name, seq string }{
		{"ESC", "\x1b"},
		{"carriage return", "\r"},
	} {
		if strings.Contains(got, bad.seq) {
			t.Errorf("%s survived into the source block:\n%s", bad.name, got)
		}
	}
	if strings.Contains(got, "\nFORGED LINE") {
		t.Errorf("an injected newline forged a line in the source block:\n%s", got)
	}
	for _, want := range []string{`\n`, `\r`, `\x1b`} {
		if !strings.Contains(got, want) {
			t.Errorf("control character not rendered visibly as %q:\n%s", want, got)
		}
	}
}
