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
)

// writeFixture lays down a minimal signal tree under root.
func writeFixture(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
}

func rowByItem(m Matrix, item string) (Row, bool) {
	for _, r := range m.Rows {
		if r.Item == item {
			return r, true
		}
	}
	return Row{}, false
}

// wiredTrainingFixture sets up a repo where the scheduled AWS UAT workflow runs
// the training config via the runner script, the runner invokes a few verbs
// (incl. the argv-array `evidence verify`), chainsaw exercises recipe/validate,
// inference UAT assets exist but are unwired, and demos document `query`.
func wiredTrainingFixture(t *testing.T, root string) {
	writeFixture(t, root, map[string]string{
		// Scheduled UAT workflow: wires the runner + a *training* config only.
		".github/workflows/uat-aws.yaml": "" +
			"env:\n  TEST_CONFIG: tests/uat/aws/tests/h100-training-config.yaml\n" +
			"jobs:\n  uat:\n    steps:\n      - run: ./tests/uat/aws/run prep \"${TEST_CONFIG}\"\n",
		// Extensionless runner: the real nightly invocations, incl. the argv array.
		"tests/uat/aws/run": "#!/usr/bin/env bash\n" +
			"\"${AICR_BIN}\" snapshot --config \"${1}\"\n" +
			"\"${AICR_BIN}\" bundle -r recipe.yaml\n" +
			"args=(evidence verify ./evidence/pointer.yaml)\n" +
			"\"${AICR_BIN}\" \"${args[@]}\"\n",
		// chainsaw exercises recipe + validate (per-PR).
		"tests/chainsaw/cli/recipe-gen/chainsaw-test.yaml": "run: aicr recipe --service eks\nrun: ${AICR_BIN} validate -r r.yaml\n",
		// Inference UAT assets exist but no scheduled workflow wires an inference config.
		"tests/uat/aws/tests/cuj2-inference/test.yaml": "script: ${AICR_BIN} bundle\n",
		// demos document query only (not executable).
		"demos/cuj1-eks.md": "Run `aicr query --selector x` to inspect.\n",
	})
}

func TestBuildMatrixStatusFromSignals(t *testing.T) {
	root := t.TempDir()
	wiredTrainingFixture(t, root)

	m := BuildMatrix(root)

	tests := []struct {
		item       string
		wantStatus Status
		wantNote   bool
	}{
		{"recipe", StatusCovered, false},                 // chainsaw
		{"validate", StatusCovered, false},               // chainsaw
		{"bundle", StatusCovered, false},                 // wired UAT runner
		{"evidence verify", StatusCovered, false},        // argv-array in the runner script
		{"query", StatusNotYetCovered, true},             // demo-only → note
		{"diff", StatusNotYetCovered, false},             // no signal anywhere
		{"cuj1-training-kubeflow", StatusCovered, false}, // wired training intent + demo
		{"cuj2-inference-dynamo", StatusStubbed, true},   // assets present but unwired
	}
	for _, tt := range tests {
		t.Run(tt.item, func(t *testing.T) {
			r, ok := rowByItem(m, tt.item)
			if !ok {
				t.Fatalf("row %q not in matrix", tt.item)
			}
			if r.Status != tt.wantStatus {
				t.Errorf("status = %q, want %q (harnesses=%v)", r.Status, tt.wantStatus, r.Harnesses)
			}
			if (r.Note != "") != tt.wantNote {
				t.Errorf("note presence = %v (%q), want %v", r.Note != "", r.Note, tt.wantNote)
			}
		})
	}
}

// TestUnwiredUATNotLive guards the P0 fix: cuj2-inference must not be reported as
// live nightly H100 coverage when no scheduled workflow wires an inference config.
func TestUnwiredUATNotLive(t *testing.T) {
	root := t.TempDir()
	wiredTrainingFixture(t, root)
	m := BuildMatrix(root)

	r, ok := rowByItem(m, "cuj2-inference-dynamo")
	if !ok {
		t.Fatal("cuj2-inference-dynamo row missing")
	}
	if r.Harnesses[HarnessUAT] {
		t.Error("cuj2-inference must not be marked UAT-covered (no wired inference config)")
	}
	if r.Cadence == "nightly" || r.Hardware == "GPU (H100, real)" {
		t.Errorf("unwired CUJ must not claim nightly H100 coverage; got hardware=%q cadence=%q", r.Hardware, r.Cadence)
	}
}

func TestRenderDeterministic(t *testing.T) {
	root := t.TempDir()
	wiredTrainingFixture(t, root)
	// Two independent builds must render byte-identical output despite the
	// map-backed harness sets and verb scan.
	a := Render(BuildMatrix(root), true, false)
	b := Render(BuildMatrix(root), true, false)
	if a != b {
		t.Fatal("Render output is not deterministic across runs")
	}
}

func TestRenderMDXSafe(t *testing.T) {
	// The page must stay inside the check-docs-mdx gate: no HTML comments, no
	// bare braces, no autolinks in the generated body.
	out := Render(BuildMatrix(t.TempDir()), true, false)
	for _, bad := range []string{"<!--", "{", "<http://", "<https://"} {
		if strings.Contains(out, bad) {
			t.Errorf("generated body contains MDX-unsafe token %q", bad)
		}
	}
}

func TestNoTitleOmitsH1(t *testing.T) {
	with := Render(BuildMatrix(t.TempDir()), true, false)
	without := Render(BuildMatrix(t.TempDir()), true, true)
	if !strings.Contains(with, "# Recipe & CLI Coverage Matrix") {
		t.Error("expected H1 when noTitle=false")
	}
	if strings.Contains(without, "# Recipe & CLI Coverage Matrix") {
		t.Error("H1 must be omitted when noTitle=true")
	}
}
