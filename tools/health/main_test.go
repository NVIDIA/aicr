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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/evidence/project"
	"github.com/NVIDIA/aicr/pkg/evidence/verifier"
	"github.com/NVIDIA/aicr/pkg/health"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/testgrid"
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

// TestEvidenceCell asserts the Evidence-column rendering: a present coordinate
// becomes a deep-link with no status/count token; everything else is pending.
func TestEvidenceCell(t *testing.T) {
	presence, err := testgrid.LoadPresence()
	if err != nil {
		t.Fatalf("LoadPresence() error = %v", err)
	}

	// A committed-present coordinate (eks / h100-ubuntu / training-kubeflow).
	present := &recipe.Criteria{
		Service:     recipe.CriteriaServiceEKS,
		Accelerator: recipe.CriteriaAcceleratorH100,
		OS:          recipe.CriteriaOSUbuntu,
		Intent:      recipe.CriteriaIntentTraining,
		Platform:    recipe.CriteriaPlatformKubeflow,
	}
	// Same coordinate but no platform — a distinct, not-present tab.
	absentTab := &recipe.Criteria{
		Service:     recipe.CriteriaServiceEKS,
		Accelerator: recipe.CriteriaAcceleratorH100,
		OS:          recipe.CriteriaOSUbuntu,
		Intent:      recipe.CriteriaIntentTraining,
	}
	// A wildcard/absent required dimension has no concrete coordinate.
	noCoord := &recipe.Criteria{Accelerator: recipe.CriteriaAcceleratorH100}

	tests := []struct {
		name     string
		crit     *recipe.Criteria
		presence *testgrid.Presence
		want     string
	}{
		{
			name:     "present renders deep-link",
			crit:     present,
			presence: presence,
			want:     "[eks/h100-ubuntu/training-kubeflow](https://validation.aicr.run/#/eks/h100-ubuntu/training-kubeflow)",
		},
		{"absent coordinate is pending", absentTab, presence, evidencePending},
		{"unmappable criteria is pending", noCoord, presence, evidencePending},
		{"nil presence degrades to pending", present, nil, evidencePending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evidenceCell(tt.crit, tt.presence, nil)
			if got != tt.want {
				t.Errorf("evidenceCell() = %q, want %q", got, tt.want)
			}
			// The Evidence cell must never carry a status/count token.
			for _, banned := range []string{"pass", "fail", "warn", "P:", "C:"} {
				if got != evidencePending && strings.Contains(got, banned) {
					t.Errorf("evidence cell %q leaked a status/count token %q", got, banned)
				}
			}
		})
	}
}

func writeTestPointer(t *testing.T, root, recipeSlug, sourceSlug, filename, issuer, identity string, attestedAt time.Time) string {
	t.Helper()
	dir := filepath.Join(root, recipeSlug, sourceSlug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	content := fmt.Sprintf(`schemaVersion: 1.0.0
recipe: %s
attestations:
  - attestedAt: %s
    bundle:
      digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
      oci: ghcr.io/nvidia/aicr-evidence:test
      predicateType: https://aicr.run/recipe-evidence/v1
    signer:
      identity: %s
      issuer: %s
`, recipeSlug, attestedAt.UTC().Format(time.RFC3339), identity, issuer)
	p := filepath.Join(dir, filename)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("writeFile %s: %v", filename, err)
	}
	return p
}

func writeTestPointerWithProfile(t *testing.T, root, recipeSlug, profile, sourceSlug, filename, issuer, identity string, attestedAt time.Time) string {
	t.Helper()
	dir := filepath.Join(root, recipeSlug, sourceSlug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	content := fmt.Sprintf(`schemaVersion: 1.0.0
recipe: %s
profile: %s
attestations:
  - attestedAt: %s
    bundle:
      digest: sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
      oci: ghcr.io/nvidia/aicr-evidence:test
      predicateType: %s
    signer:
      identity: %s
      issuer: %s
`, recipeSlug, profile, attestedAt.UTC().Format(time.RFC3339), attestation.PredicateTypeV2, identity, issuer)
	p := filepath.Join(dir, filename)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("writeFile %s: %v", filename, err)
	}
	return p
}

func TestEvidenceCellTrustClass(t *testing.T) {
	presence, err := testgrid.LoadPresence()
	if err != nil {
		t.Fatalf("LoadPresence() error = %v", err)
	}

	crit := &recipe.Criteria{
		Service:     recipe.CriteriaServiceEKS,
		Accelerator: recipe.CriteriaAcceleratorGB300,
		OS:          recipe.CriteriaOSUbuntu,
		Intent:      recipe.CriteriaIntentTraining,
		Platform:    recipe.CriteriaPlatformKubeflow,
	}

	ghaIssuer := "https://token.actions.githubusercontent.com"
	firstPartyIdentity := "https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml@refs/heads/main"

	commIssuer := "https://github.com/login/oauth"
	commIdentity := "contributor@example.com"
	commSlug, err := attestation.SourceSlug(commIssuer, commIdentity)
	if err != nil {
		t.Fatalf("SourceSlug(community): %v", err)
	}

	partnerIssuer := "https://oidc.partner.example"
	partnerIdentity := "partner-runner@partner.example"
	partnerSlug, err := attestation.SourceSlug(partnerIssuer, partnerIdentity)
	if err != nil {
		t.Fatalf("SourceSlug(partner): %v", err)
	}

	tempDir := t.TempDir()
	allowlistYAML := fmt.Sprintf(`schemaVersion: 1.0.0
firstParty:
  - label: aicr-uat-aws
    issuer: %s
    identityPattern: '^https://github\.com/NVIDIA/aicr/\.github/workflows/uat-aws\.yaml@refs/heads/.+$'
community:
  - label: community-contrib
    issuer: %s
    source: %s
partner:
  - label: partner-org
    issuer: %s
    source: %s
`, ghaIssuer, commIssuer, commSlug, partnerIssuer, partnerSlug)

	alPath := filepath.Join(tempDir, "allowlist.yaml")
	if err := os.WriteFile(alPath, []byte(allowlistYAML), 0o600); err != nil {
		t.Fatalf("WriteFile allowlist: %v", err)
	}
	al, err := project.LoadAllowlist(alPath)
	if err != nil {
		t.Fatalf("LoadAllowlist: %v", err)
	}

	assertCellTokens := func(t *testing.T, got string) {
		t.Helper()
		for _, banned := range []string{"pass", "fail", "warn", "P:", "C:"} {
			if strings.Contains(got, banned) {
				t.Errorf("evidence cell %q leaked a status/count token %q", got, banned)
			}
		}
	}

	t.Run("first-party trust class", func(t *testing.T) {
		dir := t.TempDir()
		writeTestPointer(t, dir, "gb300-eks-ubuntu-training-kubeflow", "src-fp", "ptr.yaml",
			ghaIssuer, firstPartyIdentity, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
		got := evidenceCellWithContext(crit, presence, al, nil, dir)
		want := "[eks/gb300-ubuntu/training-kubeflow](https://validation.aicr.run/#/eks/gb300-ubuntu/training-kubeflow) · first-party"
		if got != want {
			t.Errorf("evidenceCellWithContext() = %q, want %q", got, want)
		}
		assertCellTokens(t, got)
	})

	t.Run("community trust class", func(t *testing.T) {
		dir := t.TempDir()
		writeTestPointer(t, dir, "gb300-eks-ubuntu-training-kubeflow", commSlug, "ptr.yaml",
			commIssuer, commIdentity, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
		got := evidenceCellWithContext(crit, presence, al, nil, dir)
		want := "[eks/gb300-ubuntu/training-kubeflow](https://validation.aicr.run/#/eks/gb300-ubuntu/training-kubeflow) · community"
		if got != want {
			t.Errorf("evidenceCellWithContext() = %q, want %q", got, want)
		}
		assertCellTokens(t, got)
	})

	t.Run("partner trust class", func(t *testing.T) {
		dir := t.TempDir()
		writeTestPointer(t, dir, "gb300-eks-ubuntu-training-kubeflow", partnerSlug, "ptr.yaml",
			partnerIssuer, partnerIdentity, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
		got := evidenceCellWithContext(crit, presence, al, nil, dir)
		want := "[eks/gb300-ubuntu/training-kubeflow](https://validation.aicr.run/#/eks/gb300-ubuntu/training-kubeflow) · partner"
		if got != want {
			t.Errorf("evidenceCellWithContext() = %q, want %q", got, want)
		}
		assertCellTokens(t, got)
	})

	t.Run("unmatched signer falls back to link without trust class", func(t *testing.T) {
		dir := t.TempDir()
		writeTestPointer(t, dir, "gb300-eks-ubuntu-training-kubeflow", "unknown-slug", "ptr.yaml",
			"https://unknown-issuer.example", "user@unknown.example", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
		got := evidenceCellWithContext(crit, presence, al, nil, dir)
		want := "[eks/gb300-ubuntu/training-kubeflow](https://validation.aicr.run/#/eks/gb300-ubuntu/training-kubeflow)"
		if got != want {
			t.Errorf("evidenceCellWithContext() = %q, want %q", got, want)
		}
		assertCellTokens(t, got)
	})

	t.Run("missing pointer directory falls back to link without trust class", func(t *testing.T) {
		dir := t.TempDir()
		got := evidenceCellWithContext(crit, presence, al, nil, dir)
		want := "[eks/gb300-ubuntu/training-kubeflow](https://validation.aicr.run/#/eks/gb300-ubuntu/training-kubeflow)"
		if got != want {
			t.Errorf("evidenceCellWithContext() = %q, want %q", got, want)
		}
		assertCellTokens(t, got)
	})

	t.Run("malformed pointer is ignored and falls back to link", func(t *testing.T) {
		dir := t.TempDir()
		pDir := filepath.Join(dir, "gb300-eks-ubuntu-training-kubeflow", "bad-src")
		_ = os.MkdirAll(pDir, 0o755)
		_ = os.WriteFile(filepath.Join(pDir, "bad.yaml"), []byte("invalid: [yaml: content"), 0o600)
		got := evidenceCellWithContext(crit, presence, al, nil, dir)
		want := "[eks/gb300-ubuntu/training-kubeflow](https://validation.aicr.run/#/eks/gb300-ubuntu/training-kubeflow)"
		if got != want {
			t.Errorf("evidenceCellWithContext() = %q, want %q", got, want)
		}
		assertCellTokens(t, got)
	})

	t.Run("multiple pointers selects latest by attestedAt without artificial precedence", func(t *testing.T) {
		dir := t.TempDir()
		// Older first-party pointer
		writeTestPointer(t, dir, "gb300-eks-ubuntu-training-kubeflow", "src-fp", "old-fp.yaml",
			ghaIssuer, firstPartyIdentity, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
		// Newer community pointer
		writeTestPointer(t, dir, "gb300-eks-ubuntu-training-kubeflow", commSlug, "new-comm.yaml",
			commIssuer, commIdentity, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))

		// Community should be selected because it is newer, demonstrating no "first-party > community" ranking.
		got := evidenceCellWithContext(crit, presence, al, nil, dir)
		want := "[eks/gb300-ubuntu/training-kubeflow](https://validation.aicr.run/#/eks/gb300-ubuntu/training-kubeflow) · community"
		if got != want {
			t.Errorf("evidenceCellWithContext() = %q, want %q", got, want)
		}
		assertCellTokens(t, got)
	})

	t.Run("deterministic tie-breaker for identical attestedAt", func(t *testing.T) {
		dir := t.TempDir()
		sameTime := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
		// Pointer in "a-src" (lexicographically first directory)
		writeTestPointer(t, dir, "gb300-eks-ubuntu-training-kubeflow", "a-src", "ptr.yaml",
			commIssuer, commIdentity, sameTime)
		// Pointer in "z-src"
		writeTestPointer(t, dir, "gb300-eks-ubuntu-training-kubeflow", "z-src", "ptr.yaml",
			partnerIssuer, partnerIdentity, sameTime)

		got := evidenceCellWithContext(crit, presence, al, nil, dir)
		want := "[eks/gb300-ubuntu/training-kubeflow](https://validation.aicr.run/#/eks/gb300-ubuntu/training-kubeflow) · community"
		if got != want {
			t.Errorf("evidenceCellWithContext() = %q, want %q", got, want)
		}
		assertCellTokens(t, got)
	})

	t.Run("profile-bearing recipe result resolves profile-specific pointer", func(t *testing.T) {
		dir := t.TempDir()
		profileCrit := &recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorGB300,
			OS:          recipe.CriteriaOSUbuntu,
			Intent:      recipe.CriteriaIntentTraining,
			Platform:    recipe.CriteriaPlatformKubeflow,
		}
		res := &recipe.RecipeResult{
			Criteria: profileCrit,
			Metadata: recipe.RecipeResultMetadata{
				SelectedProfile: &recipe.SelectedProfile{
					Name:  "gpuStack",
					Value: "azure-managed",
				},
			},
		}
		// Suffixed slug: gb300-eks-ubuntu-training-kubeflow-gpustack-azure-managed
		profileSlug := "gb300-eks-ubuntu-training-kubeflow-gpustack-azure-managed"
		writeTestPointerWithProfile(t, dir, profileSlug, "gpuStack=azure-managed", commSlug, "ptr.yaml",
			commIssuer, commIdentity, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))

		got := evidenceCellWithContext(profileCrit, presence, al, res, dir)
		want := "[eks/gb300-ubuntu/training-kubeflow](https://validation.aicr.run/#/eks/gb300-ubuntu/training-kubeflow) · community"
		if got != want {
			t.Errorf("evidenceCellWithContext() = %q, want %q", got, want)
		}
		assertCellTokens(t, got)
	})

	t.Run("real repository evidence and allowlist", func(t *testing.T) {
		evidenceRoot := resolveEvidenceDir()
		realAl, err := project.LoadAllowlist(filepath.Join(evidenceRoot, verifier.AllowlistFileName))
		if err != nil {
			t.Fatalf("Load real allowlist: %v", err)
		}
		// Coordinate with committed community evidence: k0s/h200-ubuntu/training
		k0sCrit := &recipe.Criteria{
			Service:     recipe.CriteriaServiceK0s,
			Accelerator: recipe.CriteriaAcceleratorH200,
			OS:          recipe.CriteriaOSUbuntu,
			Intent:      recipe.CriteriaIntentTraining,
		}
		got := evidenceCellWithContext(k0sCrit, presence, realAl, nil, evidenceRoot)
		want := "[k0s/h200-ubuntu/training](https://validation.aicr.run/#/k0s/h200-ubuntu/training) · community"
		if got != want {
			t.Errorf("evidenceCellWithContext() = %q, want %q", got, want)
		}
		assertCellTokens(t, got)

		// Coordinate present in presence.yaml but with NO pointer committed: eks/gb200-ubuntu/inference-dynamo
		gb200Crit := &recipe.Criteria{
			Service:     recipe.CriteriaServiceEKS,
			Accelerator: recipe.CriteriaAcceleratorGB200,
			OS:          recipe.CriteriaOSUbuntu,
			Intent:      recipe.CriteriaIntentInference,
			Platform:    recipe.CriteriaPlatformDynamo,
		}
		gotGB200 := evidenceCellWithContext(gb200Crit, presence, realAl, nil, evidenceRoot)
		wantGB200 := "[eks/gb200-ubuntu/inference-dynamo](https://validation.aicr.run/#/eks/gb200-ubuntu/inference-dynamo)"
		if gotGB200 != wantGB200 {
			t.Errorf("evidenceCellWithContext() = %q, want %q", gotGB200, wantGB200)
		}
		assertCellTokens(t, gotGB200)
	})
}

func TestResolveEvidenceDir(t *testing.T) {
	dir := resolveEvidenceDir()
	if dir == "" {
		t.Fatal("resolveEvidenceDir() returned empty path")
	}
	alPath := filepath.Join(dir, verifier.AllowlistFileName)
	if _, err := os.Stat(alPath); err != nil {
		t.Fatalf("resolveEvidenceDir() = %q does not contain %s: %v", dir, verifier.AllowlistFileName, err)
	}
}

func TestRenderMatrixWithAllowlist(t *testing.T) {
	presence, err := testgrid.LoadPresence()
	if err != nil {
		t.Fatalf("LoadPresence() error = %v", err)
	}
	evidenceDir := resolveEvidenceDir()
	al, err := project.LoadAllowlist(filepath.Join(evidenceDir, verifier.AllowlistFileName))
	if err != nil {
		t.Fatalf("Load real allowlist: %v", err)
	}
	report := &health.Report{
		SchemaVersion: health.SchemaVersion,
		Combos: []health.ComboHealth{
			{
				Criteria: &recipe.Criteria{
					Service:     recipe.CriteriaServiceK0s,
					Accelerator: recipe.CriteriaAcceleratorH200,
					OS:          recipe.CriteriaOSUbuntu,
					Intent:      recipe.CriteriaIntentTraining,
				},
				LeafOverlay: "h200-k0s-ubuntu-training",
				Structure:   health.StructureHealth{Status: health.StatusPass},
			},
		},
	}
	var buf bytes.Buffer
	if err := renderMatrix(&buf, report, markdownOptions{
		Deterministic: true,
		NoTitle:       true,
		Presence:      presence,
		Allowlist:     al,
		EvidenceDir:   evidenceDir,
	}); err != nil {
		t.Fatalf("renderMatrix() error = %v", err)
	}
	out := buf.String()
	want := "[k0s/h200-ubuntu/training](https://validation.aicr.run/#/k0s/h200-ubuntu/training) · community"
	if !strings.Contains(out, want) {
		t.Errorf("renderMatrix with allowlist missing %q:\n%s", want, out)
	}
}

// TestRenderMatrixLinksPresentCoordinate asserts the full matrix render emits a
// deep-link for a present recipe while other recipes stay pending.
func TestRenderMatrixLinksPresentCoordinate(t *testing.T) {
	presence, err := testgrid.LoadPresence()
	if err != nil {
		t.Fatalf("LoadPresence() error = %v", err)
	}
	report := &health.Report{
		SchemaVersion: health.SchemaVersion,
		Combos: []health.ComboHealth{
			{
				Criteria: &recipe.Criteria{
					Service:     recipe.CriteriaServiceEKS,
					Accelerator: recipe.CriteriaAcceleratorH100,
					OS:          recipe.CriteriaOSUbuntu,
					Intent:      recipe.CriteriaIntentTraining,
					Platform:    recipe.CriteriaPlatformKubeflow,
				},
				LeafOverlay: "h100-eks-ubuntu-training-kubeflow",
				Structure:   health.StructureHealth{Status: health.StatusPass},
			},
			{
				Criteria:    &recipe.Criteria{Accelerator: recipe.CriteriaAcceleratorH100},
				LeafOverlay: "h100-any",
				Structure:   health.StructureHealth{Status: health.StatusPass},
			},
		},
	}
	var buf bytes.Buffer
	if err := renderMatrix(&buf, report, markdownOptions{Deterministic: true, NoTitle: true, Presence: presence}); err != nil {
		t.Fatalf("renderMatrix() error = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "[eks/h100-ubuntu/training-kubeflow](https://validation.aicr.run/#/eks/h100-ubuntu/training-kubeflow)") {
		t.Errorf("present recipe missing deep-link:\n%s", out)
	}
	if !strings.Contains(out, "| h100-any | — | h100 | — | — | — | pass | — | pending |") {
		t.Errorf("unmappable recipe should stay pending:\n%s", out)
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
	if err := run(context.Background(), outDir, "", "test-v1", true, true); err != nil {
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
	if err := run(context.Background(), filepath.Join(f, "sub"), "", "test-v1", true, true); err == nil {
		t.Fatal("expected error when out-dir parent is a file, got nil")
	}
}

// detailSampleReport exercises every renderDetail branch: a recipe that passes
// all graded dimensions, and one whose resolve failed (so chart_pinned and
// constraints_wellformed are absent from the map — never scored) and which
// carries a human-readable resolver note.
func detailSampleReport() *health.Report {
	return &health.Report{
		SchemaVersion: health.SchemaVersion,
		Combos: []health.ComboHealth{
			{
				LeafOverlay: "all-pass",
				Structure: health.StructureHealth{
					Status: health.StatusPass,
					Dimensions: map[string]string{
						health.DimResolves:              health.StatusPass,
						health.DimChartPinned:           health.StatusPass,
						health.DimConstraintsWellformed: health.StatusPass,
					},
				},
			},
			{
				LeafOverlay: "resolve-fail",
				Structure: health.StructureHealth{
					Status: health.StatusFail,
					Dimensions: map[string]string{
						health.DimResolves: health.StatusFail,
						// chart_pinned & constraints_wellformed absent — never scored.
					},
					Detail: map[string]string{
						health.DimResolves: "overlay merge failed:\r\nmissing base | overlay",
					},
				},
			},
			{
				LeafOverlay: "mixed-states",
				Structure: health.StructureHealth{
					Status: health.StatusWarn,
					Dimensions: map[string]string{
						// Exercises the warn and unknown tally arms.
						health.DimResolves:              health.StatusUnknown,
						health.DimChartPinned:           health.StatusWarn,
						health.DimConstraintsWellformed: health.StatusPass,
					},
				},
			},
		},
	}
}

func TestRenderDetailContent(t *testing.T) {
	var buf bytes.Buffer
	if err := renderDetail(&buf, detailSampleReport()); err != nil {
		t.Fatalf("renderDetail() error = %v", err)
	}
	out := buf.String()

	wantSubstrings := []string{
		"## Structural detail",
		"### Per-dimension tally",
		"| Dimension | Pass | Warn | Fail | Unknown | Not scored |",
		// resolves: pass (all-pass), fail (resolve-fail), unknown (mixed-states).
		"| resolves | 1 | 0 | 1 | 1 | 0 |",
		// chart_pinned: pass (all-pass), warn (mixed-states); resolve-fail unscored.
		"| chart_pinned | 1 | 1 | 0 | 0 | 1 |",
		// constraints_wellformed: pass (all-pass, mixed-states); resolve-fail unscored.
		"| constraints_wellformed | 2 | 0 | 0 | 0 | 1 |",
		"### Per-recipe",
		"| Recipe | resolves | chart_pinned | constraints_wellformed | Status | Notes |",
		"| all-pass | pass | pass | pass | pass |  |",
		// Absent dimensions render as the not-scored em dash; the note has its
		// CRLF flattened and pipe escaped so the table stays intact.
		"| resolve-fail | fail | — | — | fail | resolves: overlay merge failed: missing base \\| overlay |",
		// Warn/unknown dimension states surface verbatim in the per-recipe row.
		"| mixed-states | unknown | warn | pass | warn |  |",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(out, s) {
			t.Errorf("rendered detail missing %q\n--- full output ---\n%s", s, out)
		}
	}
}

func TestRenderDetailByteStable(t *testing.T) {
	var a, b bytes.Buffer
	if err := renderDetail(&a, detailSampleReport()); err != nil {
		t.Fatalf("renderDetail() first run error = %v", err)
	}
	if err := renderDetail(&b, detailSampleReport()); err != nil {
		t.Fatalf("renderDetail() second run error = %v", err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Errorf("deterministic detail differs across runs:\n--- run 1 ---\n%s\n--- run 2 ---\n%s", a.String(), b.String())
	}
}

func TestDimStateCell(t *testing.T) {
	dims := map[string]string{health.DimResolves: health.StatusPass}
	tests := []struct {
		name string
		key  string
		want string
	}{
		{"present state", health.DimResolves, "pass"},
		{"absent dimension is not-scored em dash", health.DimChartPinned, "—"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dimStateCell(dims, tt.key); got != tt.want {
				t.Errorf("dimStateCell(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestDetailNotes(t *testing.T) {
	tests := []struct {
		name string
		in   map[string]string
		want string
	}{
		{"empty map is blank", nil, ""},
		{"single note", map[string]string{health.DimResolves: "boom"}, "resolves: boom"},
		{
			"ordered by detailDimensions, pipe escaped, newline flattened",
			map[string]string{
				health.DimChartPinned: "no helm | to pin",
				health.DimResolves:    "line one\nline two",
			},
			"resolves: line one line two; chart_pinned: no helm \\| to pin",
		},
		{
			"CRLF and lone CR flattened",
			map[string]string{health.DimResolves: "a\r\nb\rc"},
			"resolves: a b c",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detailNotes(tt.in); got != tt.want {
				t.Errorf("detailNotes() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRunWritesSummaryDetail(t *testing.T) {
	outDir := t.TempDir()
	summaryPath := filepath.Join(t.TempDir(), "summary.md")
	// Seed prior content to prove the detail is appended, not truncated — the
	// $GITHUB_STEP_SUMMARY append contract.
	if err := os.WriteFile(summaryPath, []byte("PRIOR\n"), 0o644); err != nil {
		t.Fatalf("seed summary: %v", err)
	}
	if err := run(context.Background(), outDir, summaryPath, "test-v1", true, true); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	data, err := os.ReadFile(summaryPath)
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	got := string(data)
	if !strings.HasPrefix(got, "PRIOR\n") {
		t.Errorf("detail overwrote prior summary content instead of appending:\n%s", got)
	}
	if !strings.Contains(got, "## Structural detail") || !strings.Contains(got, "### Per-dimension tally") {
		t.Errorf("summary file missing per-dimension detail:\n%s", got)
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
	for _, marker := range []string{"{/* BEGIN AICR-HEALTH */}", "{/* END AICR-HEALTH */}"} {
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

// TestEveryPublishedCoordinateHasARow is the invariant the RetainNonLeaf wiring
// exists to hold (#2564): every coordinate in the committed presence manifest
// must appear in the generated matrix, so its Evidence deep-link is always
// rendered. Without the predicate a coordinate silently loses its row — and its
// only live validation.aicr.run link — the moment a platform sibling is added
// beneath its overlay.
//
// It drives run() rather than re-supplying the predicate itself, so it fails
// if run() ever stops passing RetainNonLeaf — the wiring, not just the
// predicate, is what ships. That matters because recipe-health-check is
// advisory and outside the merge gate, so nothing else would catch it.
func TestEveryPublishedCoordinateHasARow(t *testing.T) {
	presence, err := testgrid.LoadPresence()
	if err != nil {
		t.Fatalf("LoadPresence() error = %v", err)
	}
	paths := presence.Paths()
	if len(paths) == 0 {
		t.Fatal("presence manifest is empty; this test would pass vacuously")
	}

	outDir := t.TempDir()
	if runErr := run(context.Background(), outDir, "", "test-v1", true, true); runErr != nil {
		t.Fatalf("run() error = %v", runErr)
	}
	rendered, err := os.ReadFile(filepath.Join(outDir, matrixFile))
	if err != nil {
		t.Fatalf("read %s: %v", matrixFile, err)
	}

	// Assert on the rendered Evidence cell, which is what a reader follows —
	// a row alone is not enough if the deep-link is missing.
	for _, path := range paths {
		cell := "[" + path + "](" + testgrid.Origin + "/#/" + path + ")"
		if !strings.Contains(string(rendered), cell) {
			t.Errorf("published coordinate %q has no Evidence deep-link in the generated matrix; "+
				"its validation.aicr.run link would be dropped", path)
		}
	}
}

// TestHasPublishedEvidence covers the RetainNonLeaf predicate directly: a
// concrete coordinate listed in the manifest is retained, one that is not is
// dropped, criteria with no concrete coordinate can never match, and a nil
// presence degrades to the leaf-only behavior.
func TestHasPublishedEvidence(t *testing.T) {
	presence, err := testgrid.LoadPresence()
	if err != nil {
		t.Fatalf("LoadPresence() error = %v", err)
	}
	if len(presence.Paths()) == 0 {
		t.Fatal("presence manifest is empty; this test would pass vacuously")
	}

	published := &recipe.Criteria{
		Service:     recipe.CriteriaServiceRKE2,
		Accelerator: recipe.CriteriaAcceleratorVR200,
		OS:          recipe.CriteriaOSUbuntu,
		Intent:      recipe.CriteriaIntentTraining,
	}
	if co, coErr := recipe.CoordinateFor(published); coErr != nil {
		t.Fatalf("CoordinateFor(published) error = %v", coErr)
	} else if !presence.Has(co) {
		t.Fatalf("%q is no longer in the presence manifest; pick another published coordinate", co.Path())
	}

	// A concrete coordinate the manifest does not list. Asserted below rather
	// than assumed: any specific coordinate can gain evidence later (this case
	// previously used rke2/vr200 training-kubeflow, which did), so the test
	// fails loudly with a clear message instead of silently inverting.
	unpublished := &recipe.Criteria{
		Service:     recipe.CriteriaServiceRKE2,
		Accelerator: recipe.CriteriaAcceleratorVR200,
		OS:          recipe.CriteriaOSUbuntu,
		Intent:      recipe.CriteriaIntentInference,
	}
	if co, coErr := recipe.CoordinateFor(unpublished); coErr != nil {
		t.Fatalf("CoordinateFor(unpublished) error = %v", coErr)
	} else if presence.Has(co) {
		t.Fatalf("%q is now in the presence manifest; pick another unpublished coordinate", co.Path())
	}

	tests := []struct {
		name     string
		presence *testgrid.Presence
		criteria *recipe.Criteria
		want     bool
	}{
		{"published coordinate is retained", presence, published, true},
		{"unpublished coordinate is not retained", presence, unpublished, false},
		{"non-concrete criteria is not retained", presence, recipe.NewCriteria(), false},
		{"nil presence retains nothing", nil, published, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			retain := hasPublishedEvidence(tt.presence)
			if retain == nil {
				if tt.want {
					t.Fatal("hasPublishedEvidence returned nil, want a predicate")
				}
				return
			}
			if got := retain(recipe.CatalogEntry{Criteria: tt.criteria}); got != tt.want {
				t.Errorf("retain() = %v, want %v", got, tt.want)
			}
		})
	}
}
