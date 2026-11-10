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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
	"github.com/NVIDIA/aicr/pkg/validator"
)

// syntheticSummary is a fixed Summary, so the renderer tests do not depend on
// any shipped recipe.
func syntheticSummary() *recipe.Summary {
	return &recipe.Summary{
		APIVersion: recipe.RecipeSummaryAPIVersion,
		Kind:       recipe.RecipeSummaryKind,
		Source:     recipe.SummarySource{APIVersion: "aicr.run/v1", Kind: "RecipeResult"},
		Version:    "v1.2.3",
		GPUDriver:  recipe.GPUDriverStatePreinstalled,
		Profile:    "gpuStack=operator-managed",
		Criteria: recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
			Intent:      recipe.CriteriaIntentTraining,
			Nodes:       8,
		},
		AppliedOverlays: []string{"base", "eks", "h100-eks-training"},
		Constraints: []recipe.SummaryConstraint{
			{Name: "K8s.server.version", Value: ">= 1.32.4"},
			{Name: "OS.release.ID", Value: "ubuntu"},
		},
		Components: []recipe.SummaryComponent{
			{Name: "network-operator", Type: "Helm", Version: "24.10.0", Namespace: "network-operator"},
			{
				Name: "a-component-with-a-rather-long-name", Type: "Kustomize", Version: "v1.0.0",
				DependsOn: []string{"network-operator", "gpu-operator"},
			},
			{Name: "gpu-operator", Type: "Helm"},
		},
		Validation: []recipe.SummaryPhase{
			{Name: recipe.SummaryPhaseDeployment, Checks: []string{"operator-health", "expected-resources"}},
			{Name: recipe.SummaryPhaseConformance, Checks: []string{"platform-health"}},
		},
	}
}

func TestWriteRecipeSummaryTextGolden(t *testing.T) {
	tests := []struct {
		name    string
		summary *recipe.Summary
		golden  string
	}{
		{"full", syntheticSummary(), "query-summary-full.golden"},
		{"empty sections", &recipe.Summary{GPUDriver: "unknown"}, "query-summary-empty.golden"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeRecipeSummary(t.Context(), &buf, tt.summary, serializer.FormatTable); err != nil {
				t.Fatalf("writeRecipeSummary() error = %v", err)
			}
			path := filepath.Join("testdata", tt.golden)
			if *updateGolden {
				if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(path) //nolint:gosec // fixed test-local path
			if err != nil {
				t.Fatalf("read %s: %v\n\nGenerate with: go test ./pkg/cli/ -run TestWriteRecipeSummaryTextGolden -update", path, err)
			}
			if got := buf.String(); got != string(want) {
				t.Errorf("summary does not match %s.\n--- want ---\n%s\n--- got ---\n%s\n"+
					"If the change is intended: go test ./pkg/cli/ -run TestWriteRecipeSummaryTextGolden -update",
					path, want, got)
			}
		})
	}
}

func TestWriteRecipeSummaryAlignsLongNames(t *testing.T) {
	var buf bytes.Buffer
	if err := writeRecipeSummary(t.Context(), &buf, syntheticSummary(), serializer.FormatTable); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(buf.String(), "\n")
	col := -1
	for _, line := range lines {
		if !strings.HasPrefix(line, "NAME") && !strings.HasPrefix(line, "network-operator") &&
			!strings.HasPrefix(line, "a-component") && !strings.HasPrefix(line, "gpu-operator") {

			continue
		}
		// Every table row puts TYPE or its value after the same padded name column.
		idx := strings.Index(line, "  ")
		for idx >= 0 && idx < len(line) && line[idx] == ' ' {
			idx++
		}
		if col == -1 {
			col = idx
		} else if idx != col {
			t.Errorf("TYPE column starts at %d in %q, want %d", idx, line, col)
		}
	}
	if col == -1 {
		t.Fatal("components table not found in output")
	}
}

func TestWriteRecipeSummaryStructuredIsStable(t *testing.T) {
	for _, format := range []serializer.Format{serializer.FormatYAML, serializer.FormatJSON, serializer.FormatTable} {
		t.Run(string(format), func(t *testing.T) {
			var first, second bytes.Buffer
			for _, buf := range []*bytes.Buffer{&first, &second} {
				if err := writeRecipeSummary(t.Context(), buf, syntheticSummary(), format); err != nil {
					t.Fatalf("writeRecipeSummary() error = %v", err)
				}
			}
			if first.Len() == 0 || !bytes.Equal(first.Bytes(), second.Bytes()) {
				t.Errorf("output differs between runs or is empty:\n%s\n---\n%s", first.String(), second.String())
			}
		})
	}

	var buf bytes.Buffer
	if err := writeRecipeSummary(t.Context(), &buf, syntheticSummary(), serializer.FormatJSON); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"gpuDriver": "preinstalled"`) {
		t.Errorf("JSON output missing gpuDriver:\n%s", buf.String())
	}
}

func TestWriteRecipeSummaryCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := writeRecipeSummary(ctx, &bytes.Buffer{}, syntheticSummary(), serializer.FormatTable)
	errorstest.WantReportedCode(t, err, errors.ErrCodeCanceled)
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }

func TestWriteRecipeSummaryWriteError(t *testing.T) {
	for _, format := range []serializer.Format{serializer.FormatTable, serializer.FormatYAML, serializer.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			err := writeRecipeSummary(t.Context(), failingWriter{}, syntheticSummary(), format)
			errorstest.WantReportedCode(t, err, errors.ErrCodeInternal)
		})
	}
}

// TestBuildSummaryPhaseOrderMatchesValidator pins the summary's phase order to
// the order the validator runs. pkg/recipe cannot import pkg/validator, so the
// summary keeps that order by hand; this package imports both.
func TestBuildSummaryPhaseOrderMatchesValidator(t *testing.T) {
	phase := func(check string) *recipe.ValidationPhase { return &recipe.ValidationPhase{Checks: []string{check}} }
	summary, err := recipe.BuildSummary(&recipe.RecipeResult{Validation: &recipe.ValidationConfig{
		Readiness:   phase("ready"),
		Deployment:  phase("dep"),
		Performance: phase("perf"),
		Conformance: phase("conf"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(summary.Validation))
	for _, p := range summary.Validation {
		got = append(got, p.Name)
	}
	want := make([]string, 0, 1+len(validator.PhaseOrder))
	want = append(want, recipe.SummaryPhaseReadiness)
	for _, p := range validator.PhaseOrder {
		want = append(want, string(p))
	}
	if !slices.Equal(got, want) {
		t.Errorf("summary phase order = %v, want readiness then validator.PhaseOrder %v", got, want)
	}
}
