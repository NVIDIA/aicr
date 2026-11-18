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
	"strings"
	"testing"

	"aicr.run/pkg/evidence/attestation"
	"aicr.run/pkg/validator"
	"aicr.run/pkg/validator/ctrf"
)

func TestVerify_ValidatorImagesMatchReports(t *testing.T) {
	const ref = "ghcr.io/nvidia/aicr-validators/deployment:edge"
	digest := "sha256:" + strings.Repeat("a", 64)
	pinned := ctrf.PodImage{Image: ref, Digest: digest, Platform: "linux/amd64"}

	phase := func(status string, images ...ctrf.PodImage) *validator.PhaseResult {
		report := &ctrf.Report{}
		report.Results.Summary = ctrf.Summary{Tests: 1, Passed: 1}
		report.Results.Tests = []ctrf.TestResult{{Name: "check-0", Status: status, Images: images}}
		return &validator.PhaseResult{Phase: validator.PhaseDeployment, Status: ctrf.StatusPassed, Report: report}
	}

	const (
		verified   = "validator images, and recipe identity"
		unverified = "tag-only validator images unverified"
	)
	tests := []struct {
		name    string
		phase   *validator.PhaseResult
		images  []attestation.ValidatorImage
		pass    string
		wantErr string
	}{
		{"recorded image listed", phase(ctrf.StatusPassed, pinned),
			[]attestation.ValidatorImage{attestation.ValidatorImage(pinned)}, verified, ""},
		{"skipped test's record listed", phase(ctrf.StatusSkipped, pinned),
			[]attestation.ValidatorImage{attestation.ValidatorImage(pinned)}, verified, ""},
		{"no records and no images", phase(ctrf.StatusPassed), nil, verified, ""},
		{"no records and tag-only images", phase(ctrf.StatusPassed),
			[]attestation.ValidatorImage{{Image: ref}}, unverified, ""},
		{"no records and a digested image", phase(ctrf.StatusPassed),
			[]attestation.ValidatorImage{attestation.ValidatorImage(pinned)}, "", "recorded by no phase report"},
		{"record not listed", phase(ctrf.StatusPassed, pinned), nil, "", "not listed by the predicate"},
		{"record listed by tag only", phase(ctrf.StatusPassed, pinned),
			[]attestation.ValidatorImage{{Image: ref}}, "", "recorded by no phase report"},
		{"undigested record dropped", phase(ctrf.StatusPassed, pinned, ctrf.PodImage{Image: ref}),
			[]attestation.ValidatorImage{attestation.ValidatorImage(pinned)}, "", "not listed by the predicate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := Verify(context.Background(), VerifyOptions{
				Input: summaryDirOf(t, buildBundleWith(t, tt.phase, tt.images)),
			})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			inv := stepByNumber(t, res, stepInventory)
			if tt.pass != "" {
				if inv.Status != StepPassed || !strings.Contains(inv.Detail, tt.pass) {
					t.Fatalf("inventory = %v (%s, rows %+v), want passed with %q", inv.Status, inv.Detail, inv.SubRows, tt.pass)
				}
				return
			}
			if res.Exit != ExitInvalid || inv.Status != StepFailed {
				t.Fatalf("exit %d, inventory %v, want invalid and failed", res.Exit, inv.Status)
			}
			found := false
			for _, row := range inv.SubRows {
				found = found || strings.Contains(row.Value, tt.wantErr)
			}
			if !found {
				t.Errorf("inventory rows = %+v, want one containing %q", inv.SubRows, tt.wantErr)
			}
		})
	}
}
