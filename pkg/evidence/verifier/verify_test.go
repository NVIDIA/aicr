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

package verifier

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

func stepByNumber(t *testing.T, r *VerifyResult, step int) *StepResult {
	t.Helper()
	for i := range r.Steps {
		if r.Steps[i].Step == step {
			return &r.Steps[i]
		}
	}
	t.Fatalf("step %d not recorded; got %+v", step, r.Steps)
	return nil
}

func TestVerify_DirectoryHappyPath(t *testing.T) {
	bundleDir := buildTestBundle(t)
	summary := summaryDirOf(t, bundleDir)

	res, err := Verify(context.Background(), VerifyOptions{Input: summary})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Input != InputFormDir {
		t.Errorf("Input = %v, want dir", res.Input)
	}
	if got := stepByNumber(t, res, stepMaterialize).Status; got != StepPassed {
		t.Errorf("materialize = %v, want passed", got)
	}
	if got := stepByNumber(t, res, stepSignature).Status; got != StepSkipped {
		t.Errorf("signature = %v, want skipped (unsigned bundle)", got)
	}
	if got := stepByNumber(t, res, stepPredicate).Status; got != StepPassed {
		t.Errorf("predicate = %v, want passed", got)
	}
	if got := stepByNumber(t, res, stepInventory).Status; got != StepPassed {
		t.Errorf("inventory = %v, want passed", got)
	}
	if res.Exit != ExitValidPassed {
		t.Errorf("Exit = %d, want %d", res.Exit, ExitValidPassed)
	}
	if res.Signer != nil {
		t.Errorf("Signer should be nil for unsigned bundle; got %+v", res.Signer)
	}
	if res.Predicate == nil {
		t.Errorf("Predicate is nil; expected parsed predicate")
	}
}

func TestVerify_TamperedFileFails(t *testing.T) {
	bundleDir := buildTestBundle(t)
	summary := summaryDirOf(t, bundleDir)

	recipePath := filepath.Join(summary, "recipe.yaml")
	if err := os.WriteFile(recipePath, []byte("apiVersion: aicr.nvidia.com/v1alpha1\nkind: RecipeResult\nmaterialEdit: 1\n"), 0o600); err != nil {
		t.Fatalf("write recipe: %v", err)
	}

	res, err := Verify(context.Background(), VerifyOptions{Input: summary})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Exit != ExitInvalid {
		t.Errorf("Exit = %d, want %d", res.Exit, ExitInvalid)
	}
	if got := stepByNumber(t, res, stepInventory).Status; got != StepFailed {
		t.Errorf("inventory = %v, want failed", got)
	}
}

func TestVerify_StrayFileFails(t *testing.T) {
	bundleDir := buildTestBundle(t)
	summary := summaryDirOf(t, bundleDir)

	if err := os.WriteFile(filepath.Join(summary, "stray.txt"), []byte("rogue"), 0o600); err != nil {
		t.Fatalf("write stray: %v", err)
	}

	res, err := Verify(context.Background(), VerifyOptions{Input: summary})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Exit != ExitInvalid {
		t.Errorf("Exit = %d, want %d", res.Exit, ExitInvalid)
	}
	inv := stepByNumber(t, res, stepInventory)
	found := false
	for _, row := range inv.SubRows {
		if row.Key == "stray.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected stray.txt in inventory sub-rows; got %+v", inv.SubRows)
	}
}

func TestVerify_RendersMarkdownAndJSON(t *testing.T) {
	bundleDir := buildTestBundle(t)
	res, err := Verify(context.Background(), VerifyOptions{Input: summaryDirOf(t, bundleDir)})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	md := RenderMarkdown(res)
	if !strings.Contains(md, "Evidence verification") {
		t.Errorf("Markdown missing header; got %q", md)
	}
	if !strings.Contains(md, "Verification steps") {
		t.Errorf("Markdown missing steps section")
	}
	js, err := RenderJSON(res)
	if err != nil {
		t.Fatalf("RenderJSON: %v", err)
	}
	if !strings.Contains(string(js), `"steps":`) {
		t.Errorf("JSON output missing steps array")
	}
}

func TestVerify_EmptyInputErrors(t *testing.T) {
	if _, err := Verify(context.Background(), VerifyOptions{}); err == nil {
		t.Errorf("expected error for empty Input")
	}
}

// readManifestDigest computes "sha256:<hex>" of the bundle's
// manifest.json file. Test helper for cases that need the digest the
// predicate would carry for the (possibly mutated) manifest on disk.
func readManifestDigest(t *testing.T, bundleDir string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(bundleDir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	return attestation.HashBytesSHA256(body)
}

func TestCheckInventory_TamperedManifestDigestFails(t *testing.T) {
	bundleDir := buildTestBundle(t)
	summary := summaryDirOf(t, bundleDir)

	// Pass a digest that does not match manifest.json's actual sha256.
	// Simulates a producer rewriting the manifest after the predicate
	// was signed (or a bundle paired with the wrong predicate).
	rows, err := CheckInventory(context.Background(),
		&MaterializedBundle{BundleDir: summary},
		"sha256:0000000000000000000000000000000000000000000000000000000000000000")
	if err == nil {
		t.Fatalf("expected error for manifest-digest mismatch")
	}
	found := false
	for _, r := range rows {
		if r.Key == "manifest.json" && strings.Contains(r.Value, "sha256 mismatch") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected manifest.json sub-row reporting sha256 mismatch; got %+v", rows)
	}
}

func TestCheckInventory_RejectsTraversal(t *testing.T) {
	bundleDir := buildTestBundle(t)
	summary := summaryDirOf(t, bundleDir)

	// Replace manifest.json with one that names a path outside the
	// bundle, then compute its digest so the manifest-digest gate
	// passes and the per-entry traversal check is what fires.
	mfPath := filepath.Join(summary, "manifest.json")
	body := []byte(`{
  "schemaVersion": "1.0.0",
  "files": [
    {
      "path": "../../../etc/passwd",
      "size": 1,
      "sha256": "sha256:0000000000000000000000000000000000000000000000000000000000000000"
    }
  ]
}
`)
	if err := os.WriteFile(mfPath, body, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	rows, err := CheckInventory(context.Background(),
		&MaterializedBundle{BundleDir: summary},
		readManifestDigest(t, summary))
	if err == nil {
		t.Fatalf("expected error for traversal entry")
	}
	found := false
	for _, r := range rows {
		if strings.Contains(r.Value, "not a local path") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected sub-row to report rejected traversal; got %+v", rows)
	}
}

func TestCheckInventory_RejectsUnsupportedSchema(t *testing.T) {
	bundleDir := buildTestBundle(t)
	summary := summaryDirOf(t, bundleDir)

	mfPath := filepath.Join(summary, "manifest.json")
	body := []byte(`{
  "schemaVersion": "2.0.0",
  "files": [{"path": "recipe.yaml", "size": 1, "sha256": "sha256:0"}]
}
`)
	if err := os.WriteFile(mfPath, body, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	_, err := CheckInventory(context.Background(),
		&MaterializedBundle{BundleDir: summary},
		readManifestDigest(t, summary))
	if err == nil {
		t.Fatalf("expected error for unsupported manifest schemaVersion")
	}
	if !strings.Contains(err.Error(), "schemaVersion") {
		t.Errorf("expected error to mention schemaVersion; got %v", err)
	}
}

func TestCheckInventory_RespectsCancellation(t *testing.T) {
	bundleDir := buildTestBundle(t)
	summary := summaryDirOf(t, bundleDir)
	mat := &MaterializedBundle{BundleDir: summary}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := CheckInventory(ctx, mat, readManifestDigest(t, summary))
	if err == nil {
		t.Errorf("CheckInventory(canceled ctx) = nil, want error")
	}
}

func TestRenderMarkdown_FailedStepDetailsListsFiles(t *testing.T) {
	// Build a result with a failed inventory step that carries per-file
	// sub-rows. The rendered Markdown must list the file names — not
	// just count them — so the maintainer can see what failed.
	r := &VerifyResult{
		Exit: ExitInvalid,
		Steps: []StepResult{
			{Step: 4, Name: "manifest-hash-check", Status: StepFailed,
				Detail: "manifest inventory check failed for 2 file(s)",
				SubRows: []KV{
					{Key: "ctrf/deployment.json", Value: "sha256 mismatch"},
					{Key: "stray.txt", Value: "file not in manifest.json (unsigned)"},
				}},
		},
	}
	md := RenderMarkdown(r)
	if !strings.Contains(md, "Failed check details") {
		t.Errorf("missing Failed check details section; got %q", md)
	}
	if !strings.Contains(md, "ctrf/deployment.json") {
		t.Errorf("rendered Markdown should name ctrf/deployment.json; got %q", md)
	}
	if !strings.Contains(md, "stray.txt") {
		t.Errorf("rendered Markdown should name stray.txt; got %q", md)
	}
}

func TestRenderMarkdown_HeaderDistinguishesFailedFromUnsigned(t *testing.T) {
	tests := []struct {
		name       string
		steps      []StepResult
		signer     *SignerClaims
		wantSub    string
		wantNotSub string
	}{
		{
			name:    "passed signature shows identity",
			steps:   []StepResult{{Step: stepSignature, Name: "signature-verify", Status: StepPassed}},
			signer:  &SignerClaims{Identity: "alice@example.com", Issuer: "https://issuer"},
			wantSub: "alice@example.com",
		},
		{
			name:    "skipped signature shows unsigned bundle",
			steps:   []StepResult{{Step: stepSignature, Name: "signature-verify", Status: StepSkipped}},
			wantSub: "unsigned bundle",
		},
		{
			name:       "failed signature does NOT claim unsigned",
			steps:      []StepResult{{Step: stepSignature, Name: "signature-verify", Status: StepFailed, Detail: "x"}},
			wantSub:    "signature verification failed",
			wantNotSub: "unsigned",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &VerifyResult{Steps: tt.steps, Signer: tt.signer}
			md := RenderMarkdown(r)
			if !strings.Contains(md, tt.wantSub) {
				t.Errorf("output should contain %q; got %q", tt.wantSub, md)
			}
			if tt.wantNotSub != "" && strings.Contains(md, tt.wantNotSub) {
				t.Errorf("output should NOT contain %q; got %q", tt.wantNotSub, md)
			}
		})
	}
}

func TestRenderMarkdown_NoFailedStepDetailsSectionWhenAllPass(t *testing.T) {
	r := &VerifyResult{
		Exit: ExitValidPassed,
		Steps: []StepResult{
			{Step: 1, Name: "materialize-bundle", Status: StepPassed},
		},
	}
	md := RenderMarkdown(r)
	if strings.Contains(md, "Failed check details") {
		t.Errorf("should not render Failed check details when nothing failed; got %q", md)
	}
}

func TestVerify_PredicateParseFailureRecordedAsPredicateStep(t *testing.T) {
	bundleDir := buildTestBundle(t)
	summary := summaryDirOf(t, bundleDir)

	// Corrupt the in-toto Statement so loadPredicate fails.
	if err := os.WriteFile(filepath.Join(summary, "statement.intoto.json"),
		[]byte("not json"), 0o600); err != nil {
		t.Fatalf("write statement: %v", err)
	}

	res, err := Verify(context.Background(), VerifyOptions{Input: summary})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Exit != ExitInvalid {
		t.Errorf("Exit = %d, want %d", res.Exit, ExitInvalid)
	}
	pred := stepByNumber(t, res, stepPredicate)
	if pred.Status != StepFailed {
		t.Errorf("predicate step status = %v, want failed", pred.Status)
	}
	if pred.Name != "predicate-parse" {
		t.Errorf("predicate step name = %q, want predicate-parse", pred.Name)
	}
}
