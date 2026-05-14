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

package attestation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/fingerprint"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/snapshotter"
	"github.com/NVIDIA/aicr/pkg/validator"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

func TestBuild_RejectsMissingRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		opts BuildOptions
	}{
		{"no OutputDir", BuildOptions{Recipe: &recipe.RecipeResult{}, RecipeYAML: []byte("a: b"), SnapshotYAML: []byte("a: b"), BOM: BOMInputs{Body: []byte("{}")}}},
		{"no Recipe", BuildOptions{OutputDir: "/tmp/x", RecipeYAML: []byte("a: b"), SnapshotYAML: []byte("a: b"), BOM: BOMInputs{Body: []byte("{}")}}},
		{"no RecipeYAML", BuildOptions{OutputDir: "/tmp/x", Recipe: &recipe.RecipeResult{}, SnapshotYAML: []byte("a: b"), BOM: BOMInputs{Body: []byte("{}")}}},
		{"no SnapshotYAML", BuildOptions{OutputDir: "/tmp/x", Recipe: &recipe.RecipeResult{}, RecipeYAML: []byte("a: b"), BOM: BOMInputs{Body: []byte("{}")}}},
		{"no BOM body", BuildOptions{OutputDir: "/tmp/x", Recipe: &recipe.RecipeResult{}, RecipeYAML: []byte("a: b"), SnapshotYAML: []byte("a: b")}},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Build(context.Background(), tt.opts); err == nil {
				t.Errorf("expected error for missing required field")
			}
		})
	}
}

func TestBuild_HappyPathWritesExpectedTree(t *testing.T) {
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
	snap := &snapshotter.Snapshot{}

	bom := []byte(`{"bomFormat":"CycloneDX","specVersion":"1.6","components":[{"name":"a"},{"name":"b"}]}`)
	report := &ctrf.Report{}
	report.Results.Summary = ctrf.Summary{Tests: 3, Passed: 2, Failed: 1, Skipped: 0}
	phaseResults := []*validator.PhaseResult{
		{Phase: validator.PhaseDeployment, Status: "passed", Report: report, Duration: time.Second},
	}

	bundle, err := Build(context.Background(), BuildOptions{
		OutputDir:    dir,
		Recipe:       rec,
		RecipeYAML:   []byte("apiVersion: aicr.nvidia.com/v1alpha1\nkind: RecipeResult\n"),
		Snapshot:     snap,
		SnapshotYAML: []byte("measurements: []\n"),
		BOM:          BOMInputs{Body: bom, CycloneDXVersion: "1.6"},
		PhaseResults: phaseResults,
		AICRVersion:  "v0.13.0",
		AttestedAt:   time.Date(2026, 5, 8, 10, 23, 11, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if bundle.RecipeName != "h100-eks-ubuntu-training" {
		t.Errorf("RecipeName = %q, want h100-eks-ubuntu-training", bundle.RecipeName)
	}

	mustReadFile(t, filepath.Join(bundle.SummaryDir, RecipeFilename))
	mustReadFile(t, filepath.Join(bundle.SummaryDir, SnapshotFilename))
	mustReadFile(t, filepath.Join(bundle.SummaryDir, BOMFilename))
	mustReadFile(t, filepath.Join(bundle.SummaryDir, "ctrf", "deployment.json"))

	manifestBytes := mustReadFile(t, filepath.Join(bundle.SummaryDir, ManifestFilename))
	var m Manifest
	if err := json.Unmarshal(manifestBytes, &m); err != nil {
		t.Fatalf("manifest unmarshal: %v", err)
	}
	if len(m.Files) == 0 {
		t.Errorf("manifest has no files")
	}
	for _, f := range m.Files {
		body := mustReadFile(t, filepath.Join(bundle.SummaryDir, f.Path))
		want := "sha256:" + hex.EncodeToString(sha256ToBytes(body))
		if f.SHA256 != want {
			t.Errorf("manifest hash mismatch for %s: got %s want %s", f.Path, f.SHA256, want)
		}
	}

	wantDigest := "sha256:" + hex.EncodeToString(sha256OfFile(t, filepath.Join(bundle.SummaryDir, ManifestFilename)))
	if bundle.Predicate.Manifest.Digest != wantDigest {
		t.Errorf("predicate manifest digest mismatch: got %s want %s", bundle.Predicate.Manifest.Digest, wantDigest)
	}
	if bundle.Predicate.Manifest.FileCount != len(m.Files) {
		t.Errorf("predicate manifest fileCount = %d, want %d", bundle.Predicate.Manifest.FileCount, len(m.Files))
	}

	// fingerprint.Match treats empty fingerprint slots as "unknown" rather
	// than "mismatch", so Matched stays true here.
	if !bundle.Predicate.CriteriaMatch.Matched {
		t.Errorf("expected matched=true when fingerprint is empty (unknowns are tolerated)")
	}
	accel, ok := bundle.Predicate.CriteriaMatch.Find(fingerprint.DimensionAccelerator)
	if !ok {
		t.Errorf("expected accelerator entry in PerDimension")
	} else if accel.Match != fingerprint.DimensionUnknown {
		t.Errorf("accelerator without fingerprint should be unknown; got %v", accel.Match)
	}

	if len(bundle.SubjectDigest) != 64 {
		t.Errorf("subject digest length = %d, want 64", len(bundle.SubjectDigest))
	}

	var stmt map[string]any
	if err := json.Unmarshal(bundle.StatementJSON, &stmt); err != nil {
		t.Fatalf("statement unmarshal: %v", err)
	}
	if stmt["predicateType"] != PredicateTypeV1 {
		t.Errorf("statement predicateType = %v", stmt["predicateType"])
	}

	if bundle.Predicate.BOM.ImageCount != 2 {
		t.Errorf("BOM imageCount = %d, want 2", bundle.Predicate.BOM.ImageCount)
	}
	if bundle.Predicate.BOM.Format != BOMFormat {
		t.Errorf("BOM format = %q", bundle.Predicate.BOM.Format)
	}
}

func TestBuild_RejectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Build(ctx, BuildOptions{
		OutputDir:    t.TempDir(),
		Recipe:       &recipe.RecipeResult{Criteria: &recipe.Criteria{Accelerator: "h100"}},
		RecipeYAML:   []byte("a: b\n"),
		Snapshot:     &snapshotter.Snapshot{},
		SnapshotYAML: []byte("a: b\n"),
		BOM:          BOMInputs{Body: []byte("{}"), CycloneDXVersion: "1.6"},
	})
	if err == nil {
		t.Errorf("expected error on canceled context")
	}
}

func TestNoOpSigner_ProducesEmptyBundle(t *testing.T) {
	res, err := NoOpSigner{}.Sign(context.Background(), []byte("any"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.BundleJSON) != 0 {
		t.Errorf("expected empty BundleJSON for NoOpSigner, got %d bytes", len(res.BundleJSON))
	}
}

func TestSignBundle_NoOpDoesNotWriteAttestation(t *testing.T) {
	dir := t.TempDir()
	bundle := &Bundle{
		SummaryDir:    dir,
		StatementJSON: []byte("{}"),
	}
	if _, err := SignBundle(context.Background(), bundle, NoOpSigner{}); err != nil {
		t.Fatalf("SignBundle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, AttestationFilename)); !os.IsNotExist(err) {
		t.Errorf("NoOpSigner should not write attestation file; stat err=%v", err)
	}
}

func TestBuild_WritesUnsignedStatement(t *testing.T) {
	dir := t.TempDir()
	rec := &recipe.RecipeResult{Criteria: &recipe.Criteria{Accelerator: "h100"}}
	bundle, err := Build(context.Background(), BuildOptions{
		OutputDir:    dir,
		Recipe:       rec,
		RecipeYAML:   []byte("a: b\n"),
		Snapshot:     &snapshotter.Snapshot{},
		SnapshotYAML: []byte("a: b\n"),
		BOM:          BOMInputs{Body: []byte("{}"), CycloneDXVersion: "1.6"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	stmtPath := filepath.Join(bundle.SummaryDir, StatementFilename)
	onDisk, err := os.ReadFile(stmtPath)
	if err != nil {
		t.Fatalf("expected unsigned statement on disk: %v", err)
	}
	if string(onDisk) != string(bundle.StatementJSON) {
		t.Errorf("on-disk statement diverges from bundle.StatementJSON")
	}

	// statement.intoto.json must NOT appear in the manifest (it's
	// outside the integrity chain — its hash derives from the manifest
	// digest, not the other way around).
	manifestBody := mustReadFile(t, filepath.Join(bundle.SummaryDir, ManifestFilename))
	var m Manifest
	if jErr := json.Unmarshal(manifestBody, &m); jErr != nil {
		t.Fatalf("unmarshal manifest: %v", jErr)
	}
	for _, f := range m.Files {
		if f.Path == StatementFilename || f.Path == AttestationFilename {
			t.Errorf("manifest must not enumerate %s", f.Path)
		}
	}
}

func TestRecipeNameFor_NilAndEmpty(t *testing.T) {
	if got := RecipeNameFor(nil); got != "" {
		t.Errorf("nil RecipeResult: got %q, want empty", got)
	}
	if got := RecipeNameFor(&recipe.RecipeResult{}); got != "" {
		t.Errorf("nil Criteria: got %q, want empty", got)
	}
	if got := RecipeNameFor(&recipe.RecipeResult{Criteria: &recipe.Criteria{}}); got != "recipe" {
		t.Errorf("empty Criteria: got %q, want %q", got, "recipe")
	}
	if got := RecipeNameFor(&recipe.RecipeResult{Criteria: &recipe.Criteria{
		Service: "any", Accelerator: "any",
	}}); got != "recipe" {
		t.Errorf("all-any Criteria: got %q, want %q", got, "recipe")
	}
}

func TestCriteriaOf_Nil(t *testing.T) {
	if criteriaOf(nil) != nil {
		t.Errorf("nil RecipeResult should yield nil criteria")
	}
}

func TestCountBOMComponents_MalformedErrors(t *testing.T) {
	if _, err := countBOMComponents([]byte("not json")); err == nil {
		t.Errorf("expected error on malformed BOM JSON")
	}
}

func TestCountBOMComponents_EmptyComponents(t *testing.T) {
	count, err := countBOMComponents([]byte(`{"bomFormat":"CycloneDX"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
}

func TestBuild_RejectsMalformedBOM(t *testing.T) {
	rec := &recipe.RecipeResult{Criteria: &recipe.Criteria{Accelerator: "h100"}}
	_, err := Build(context.Background(), BuildOptions{
		OutputDir:    t.TempDir(),
		Recipe:       rec,
		RecipeYAML:   []byte("a: b\n"),
		Snapshot:     &snapshotter.Snapshot{},
		SnapshotYAML: []byte("a: b\n"),
		BOM:          BOMInputs{Body: []byte("not json"), CycloneDXVersion: "1.6"},
	})
	if err == nil {
		t.Errorf("expected error on malformed BOM")
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return body
}

func sha256OfFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	h := sha256.Sum256(body)
	return h[:]
}

func contains(haystack []byte, needle string) bool {
	return indexOf(haystack, []byte(needle)) >= 0
}

func indexOf(haystack, needle []byte) int {
	if len(needle) == 0 {
		return 0
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
