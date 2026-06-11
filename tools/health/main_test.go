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
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/health"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// budgetCeiling is the compute-budget gate referenced by the ADR-009 epic
// acceptance criteria: the generator must score the full catalog under the
// sub-minute target. It is a regression backstop, not a benchmark — a healthy
// run is well under a second; this only fails if Compute regresses by orders
// of magnitude.
const budgetCeiling = 60 * time.Second

// sampleReport returns a small hand-built report exercising every rendering
// branch: a clean pass, a fail with nil Coverage (resolve failed), a warn, and
// an unspecified-dimension row.
func sampleReport() *health.Report {
	return &health.Report{
		SchemaVersion: health.SchemaVersion,
		Combos: []health.ComboHealth{
			{
				Criteria:    &recipe.Criteria{Accelerator: recipe.CriteriaAcceleratorH100},
				LeafOverlay: "h100-any",
				Structure: health.StructureHealth{
					Status:     health.StatusPass,
					Dimensions: map[string]string{health.DimResolves: health.StatusPass},
					Coverage: &health.DeclaredCoverage{
						Deployment:  health.PhaseCoverage{Declared: true, Checks: []string{"a", "b", "c", "d"}},
						Performance: health.PhaseCoverage{Declared: true, Checks: []string{"p"}},
					},
				},
			},
			{
				Criteria: &recipe.Criteria{
					Service:     recipe.CriteriaServiceEKS,
					Accelerator: recipe.CriteriaAcceleratorH100,
					OS:          recipe.CriteriaOSUbuntu,
					Intent:      recipe.CriteriaIntentTraining,
				},
				LeafOverlay: "h100-eks-ubuntu-training",
				Structure: health.StructureHealth{
					Status:     health.StatusFail,
					Dimensions: map[string]string{health.DimResolves: health.StatusFail},
					// nil Coverage — resolve failed, no RecipeResult to read.
				},
			},
		},
	}
}

func TestRenderMatrixContent(t *testing.T) {
	var buf bytes.Buffer
	if err := renderMatrix(&buf, sampleReport(), markdownOptions{Deterministic: true, NoTitle: true}); err != nil {
		t.Fatalf("renderMatrix() error = %v", err)
	}
	out := buf.String()

	wantSubstrings := []string{
		"## Summary",
		"- Recipes: **2**",
		"Pass: **1** · Warn: **0** · Fail: **1** · Unknown: **0**",
		"| Recipe | Service | Accelerator | OS | Intent | Platform | Status | Coverage | Evidence |",
		// Clean pass row: unspecified dims are em dashes; coverage counts checks.
		"| h100-any | — | h100 | — | — | — | pass | R:0 D:4 P:1 C:0 | pending |",
		// Fail row with nil Coverage renders an em dash, not an all-zero block.
		"| h100-eks-ubuntu-training | eks | h100 | ubuntu | training | — | fail | — | pending |",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(out, s) {
			t.Errorf("rendered output missing %q\n--- full output ---\n%s", s, out)
		}
	}
}

func TestRenderMatrixTitleAndStamp(t *testing.T) {
	// NoTitle=false emits the H1; Deterministic=false emits the generated stamp.
	var buf bytes.Buffer
	if err := renderMatrix(&buf, sampleReport(), markdownOptions{
		AICRVersion:   "v1.2.3",
		Deterministic: false,
		NoTitle:       false,
		Timestamp:     "2026-06-10T00:00:00Z",
	}); err != nil {
		t.Fatalf("renderMatrix() error = %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "# AICR Recipe Health\n") {
		t.Errorf("expected H1 title, got prefix %q", out[:min(40, len(out))])
	}
	if !strings.Contains(out, "_Generated 2026-06-10T00:00:00Z for aicr v1.2.3._") {
		t.Errorf("expected injected generated-stamp line, got:\n%s", out)
	}
}

func TestRenderMatrixDeterministicOmitsStamp(t *testing.T) {
	var buf bytes.Buffer
	if err := renderMatrix(&buf, sampleReport(), markdownOptions{
		AICRVersion:   "v1.2.3",
		Deterministic: true,
		NoTitle:       true,
		Timestamp:     "2026-06-10T00:00:00Z",
	}); err != nil {
		t.Fatalf("renderMatrix() error = %v", err)
	}
	if strings.Contains(buf.String(), "_Generated") {
		t.Errorf("deterministic mode must omit the generated-stamp line, got:\n%s", buf.String())
	}
}

func TestRenderMatrixByteStable(t *testing.T) {
	// The committed-golden determinism backstop: -deterministic output must be
	// byte-identical across runs so the regenerate-and-diff staleness check is
	// meaningful.
	opts := markdownOptions{AICRVersion: "main", Deterministic: true, NoTitle: true}
	var a, b bytes.Buffer
	if err := renderMatrix(&a, sampleReport(), opts); err != nil {
		t.Fatalf("renderMatrix() first run error = %v", err)
	}
	if err := renderMatrix(&b, sampleReport(), opts); err != nil {
		t.Fatalf("renderMatrix() second run error = %v", err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Errorf("deterministic output differs across runs:\n--- run 1 ---\n%s\n--- run 2 ---\n%s", a.String(), b.String())
	}
}

func TestDimCell(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"concrete value", "eks", "eks"},
		{"empty is em dash", "", "—"},
		{"any sentinel is em dash", recipe.CriteriaAnyValue, "—"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dimCell(tt.in); got != tt.want {
				t.Errorf("dimCell(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCoverageCell(t *testing.T) {
	tests := []struct {
		name string
		in   *health.DeclaredCoverage
		want string
	}{
		{"nil coverage is em dash", nil, "—"},
		{"non-nil formats per-phase check counts", &health.DeclaredCoverage{
			Readiness:   health.PhaseCoverage{Checks: []string{"r1", "r2"}},
			Deployment:  health.PhaseCoverage{Checks: []string{"d1"}},
			Conformance: health.PhaseCoverage{Checks: []string{"c1", "c2", "c3"}},
		}, "R:2 D:1 P:0 C:3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := coverageCell(tt.in); got != tt.want {
				t.Errorf("coverageCell() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunEndToEnd(t *testing.T) {
	outDir := t.TempDir()
	if err := run(context.Background(), outDir, "test-v1", true, true); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	data, err := os.ReadFile(filepath.Join(outDir, matrixFile))
	if err != nil {
		t.Fatalf("read %s: %v", matrixFile, err)
	}
	if len(data) == 0 {
		t.Fatal("rendered matrix is empty")
	}
	// Deterministic mode: no generated-stamp line.
	if strings.Contains(string(data), "_Generated") {
		t.Errorf("deterministic run emitted a generated-stamp line:\n%s", data)
	}
	if !strings.Contains(string(data), "| Recipe | Service |") {
		t.Errorf("rendered matrix missing the table header:\n%s", data)
	}
}

func TestRunMkdirError(t *testing.T) {
	// A regular file standing where out-dir's parent should be makes
	// os.MkdirAll fail, exercising run's mkdir error branch.
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := run(context.Background(), filepath.Join(f, "sub"), "test-v1", true, true); err == nil {
		t.Fatal("expected error when out-dir parent is a file, got nil")
	}
}

// TestComputeBudget is the compute-budget gate: Compute over the full embedded
// catalog must finish well under the ADR-009 sub-minute target.
func TestComputeBudget(t *testing.T) {
	start := time.Now()
	rep, err := health.Compute(context.Background(), health.Options{Version: "budget-test"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("health.Compute() error = %v", err)
	}
	if len(rep.Combos) == 0 {
		t.Fatal("expected a non-empty catalog")
	}
	t.Logf("Compute scored %d combos in %s (ceiling %s)", len(rep.Combos), elapsed, budgetCeiling)
	if elapsed > budgetCeiling {
		t.Errorf("Compute took %s, exceeding the %s budget ceiling", elapsed, budgetCeiling)
	}
}

// TestDocMarkersPresent is the marker-presence guard: the committed doc must
// retain the splice markers, or `make recipe-health-docs` silently no-ops and
// the matrix goes stale.
func TestDocMarkersPresent(t *testing.T) {
	root := repoRoot(t)
	docPath := filepath.Join(root, "docs", "user", "recipe-health.md")
	data, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("read %s: %v", docPath, err)
	}
	for _, marker := range []string{"<!-- BEGIN AICR-HEALTH -->", "<!-- END AICR-HEALTH -->"} {
		if !strings.Contains(string(data), marker) {
			t.Errorf("%s is missing splice marker %q", docPath, marker)
		}
	}
}

// repoRoot walks up from the test's working directory to the module root
// (the directory containing go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.mod walking up from test working directory")
		}
		dir = parent
	}
}
