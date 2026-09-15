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
	}
	return from, to
}

// mixedReport covers every verdict and every change kind in one render: safe,
// manual, both shapes of blocked (gamma-operator, where two records are crossed
// and neither describes the jump, and kappa-operator, where one record does),
// unknown, unversioned, added, removed, and a component that did not move at
// all (iota-operator, which produces no row).
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
