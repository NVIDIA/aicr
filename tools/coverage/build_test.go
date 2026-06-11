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

func TestBuildMatrixStatusFromSignals(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, map[string]string{
		// chainsaw exercises recipe + validate (executable, per-PR)
		"tests/chainsaw/cli/recipe-gen/chainsaw-test.yaml": "run: aicr recipe --service eks\nrun: ${AICR_BIN} validate -r r.yaml\n",
		// aws UAT exercises bundle + the inference CUJ (executable, nightly)
		"tests/uat/aws/tests/cuj2-inference/test.yaml": "script: ${AICR_BIN} bundle -r r.yaml\n",
		"tests/uat/aws/tests/cuj1-training/test.yaml":  "script: aicr validate\n",
		// azure trees exist but are stubbed: trust update appears only here
		"tests/uat/azure/tests/cuj1-training/t.yaml": "run: aicr trust update\n",
		// demos document query only (not executable)
		"demos/cuj1-eks.md": "Run `aicr query --selector x` to inspect.\n",
	})

	m := BuildMatrix(root)

	tests := []struct {
		item       string
		wantStatus Status
		wantNote   bool
	}{
		{"recipe", StatusCovered, false},
		{"validate", StatusCovered, false},
		{"bundle", StatusCovered, false},
		{"trust update", StatusStubbed, true}, // azure-only → stubbed
		{"query", StatusNotYetCovered, true},  // demo-only → not-yet w/ note
		{"diff", StatusNotYetCovered, false},  // no signal anywhere
		{"cuj1-training-kubeflow", StatusCovered, false},
		{"cuj2-inference-dynamo", StatusCovered, false},
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

func TestRenderDeterministic(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, root, map[string]string{
		"tests/chainsaw/cli/recipe-gen/t.yaml": "run: aicr recipe\n",
		"demos/cuj2.md":                        "aicr bundle\n",
	})
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
		if containsOutsideCode(out, bad) {
			t.Errorf("generated body contains MDX-unsafe token %q", bad)
		}
	}
}

// containsOutsideCode is a coarse check: our generator emits no fenced code
// blocks, so any occurrence is "outside code" for gate purposes.
func containsOutsideCode(s, sub string) bool {
	return len(sub) > 0 && strings.Contains(s, sub)
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
