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
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

func TestEvidenceCmd_RegistersVerifySubcommand(t *testing.T) {
	cmd := evidenceCmd()
	if cmd.Name != "evidence" {
		t.Errorf("Name = %q, want evidence", cmd.Name)
	}
	found := false
	for _, sub := range cmd.Commands {
		if sub.Name == "verify" {
			found = true
		}
	}
	if !found {
		t.Errorf("evidence verify subcommand not registered")
	}
}

func TestEvidenceVerifyCmd_HasExpectedFlags(t *testing.T) {
	cmd := evidenceVerifyCmd()
	wanted := []string{
		"output", "format",
		"expected-issuer", "expected-identity-regexp", "bundle",
		"registry-plain-http", "registry-insecure-tls", "allow-unpinned-tag",
	}
	for _, name := range wanted {
		found := false
		for _, f := range cmd.Flags {
			if f.Names()[0] == name {
				found = true
			}
		}
		if !found {
			t.Errorf("missing expected flag: --%s", name)
		}
	}
}

func buildCLITestBundle(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	rec := &recipe.RecipeResult{
		Kind:       "RecipeResult",
		APIVersion: "aicr.nvidia.com/v1alpha1",
		Criteria: &recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorH100,
			OS:          "ubuntu",
			Intent:      "training",
		},
	}
	bom := []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","components":[{"name":"a"}]}`)
	report := &ctrf.Report{}
	report.Results.Summary = ctrf.Summary{Tests: 1, Passed: 1}
	bundle, err := attestation.Build(context.Background(), attestation.BuildOptions{
		OutputDir:    dir,
		Recipe:       rec,
		RecipeYAML:   []byte("apiVersion: aicr.nvidia.com/v1alpha1\nkind: RecipeResult\n"),
		Snapshot:     &snapshotter.Snapshot{},
		SnapshotYAML: []byte("measurements: []\n"),
		BOM:          attestation.BOMInputs{Body: bom, CycloneDXVersion: "1.6"},
		PhaseResults: []*validator.PhaseResult{
			{Phase: validator.PhaseDeployment, Status: "passed", Report: report, Duration: time.Second},
		},
		AICRVersion: "v0.13.0",
		AttestedAt:  time.Date(2026, 5, 8, 10, 23, 11, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("build bundle: %v", err)
	}
	return bundle.SummaryDir
}

func TestEvidenceVerifyCmd_RunAgainstFixture(t *testing.T) {
	bundleDir := buildCLITestBundle(t)

	root := newRootCmd()
	var out bytes.Buffer
	root.Writer = &out

	err := root.Run(context.Background(), []string{"aicr", "evidence", "verify", bundleDir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Evidence verification") {
		t.Errorf("output missing header; got %q", got)
	}
	if !strings.Contains(got, "bundle valid") {
		t.Errorf("output missing verdict; got %q", got)
	}
}

func TestEvidenceVerifyCmd_RunJSON(t *testing.T) {
	bundleDir := buildCLITestBundle(t)

	root := newRootCmd()
	var out bytes.Buffer
	root.Writer = &out

	err := root.Run(context.Background(), []string{"aicr", "evidence", "verify", "--format", "json", bundleDir})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `"steps":`) {
		t.Errorf("JSON output missing steps array; got %q", got)
	}
}

func TestEvidenceVerifyCmd_RejectsEmptyInput(t *testing.T) {
	root := newRootCmd()
	var out bytes.Buffer
	root.Writer = &out
	root.ErrWriter = &out
	if err := root.Run(context.Background(), []string{"aicr", "evidence", "verify"}); err == nil {
		t.Errorf("expected error for missing positional arg")
	}
}

func TestEvidenceVerifyCmd_OutputMarkdownToFile(t *testing.T) {
	bundleDir := buildCLITestBundle(t)
	out := filepath.Join(t.TempDir(), "summary.md")

	root := newRootCmd()
	var w bytes.Buffer
	root.Writer = &w

	err := root.Run(context.Background(), []string{
		"aicr", "evidence", "verify", "-o", out, bundleDir,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, readErr := os.ReadFile(out)
	if readErr != nil {
		t.Fatalf("read markdown: %v", readErr)
	}
	if !strings.Contains(string(body), "Evidence verification") {
		t.Errorf("--output file missing header; got %q", body)
	}
	// Stdout should be empty when -o is set (no double-rendering).
	if w.Len() != 0 {
		t.Errorf("stdout should be empty when -o is set; got %q", w.String())
	}
}

func TestEvidenceVerifyCmd_OutputJSONToFile(t *testing.T) {
	bundleDir := buildCLITestBundle(t)
	out := filepath.Join(t.TempDir(), "result.json")

	root := newRootCmd()
	var w bytes.Buffer
	root.Writer = &w

	err := root.Run(context.Background(), []string{
		"aicr", "evidence", "verify", "-o", out, "-t", "json", bundleDir,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	body, readErr := os.ReadFile(out)
	if readErr != nil {
		t.Fatalf("read json: %v", readErr)
	}
	if !strings.Contains(string(body), `"steps":`) {
		t.Errorf("--output json file missing steps; got %q", body)
	}
}
