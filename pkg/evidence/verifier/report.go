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
	"encoding/json"
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// RenderMarkdown produces the PR-comment-shaped summary. Signed
// predicate fields (fingerprint, phase counts, BOM info) are surfaced
// directly — when the signature step passed, the predicate body is
// cryptographically anchored to the Fulcio cert claims shown on the
// Signer line.
func RenderMarkdown(r *VerifyResult) string {
	if r == nil {
		return "## Evidence verification — (no result)\n"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Evidence verification")
	if r.RecipeName != "" {
		fmt.Fprintf(&b, " — %s", r.RecipeName)
	}
	b.WriteString("\n\n")

	writeHeader(&b, r)
	writeFingerprint(&b, r.Predicate)
	writePhases(&b, r.Predicate)
	writeBOM(&b, r.Predicate)
	writeSteps(&b, r)
	writeFailedStepDetails(&b, r)
	writeVerdict(&b, r)
	return b.String()
}

// writeFailedStepDetails enumerates the sub-rows of any failed step so
// the maintainer sees exactly which files / dimensions / constraints
// caused the failure. Markdown tables can't render nested lists; this
// section follows the steps table and breaks failures out as bullets.
func writeFailedStepDetails(b *strings.Builder, r *VerifyResult) {
	failedWithRows := 0
	for _, s := range r.Steps {
		if s.Status == StepFailed && len(s.SubRows) > 0 {
			failedWithRows++
		}
	}
	if failedWithRows == 0 {
		return
	}
	b.WriteString("### Failed check details\n")
	for _, s := range r.Steps {
		if s.Status != StepFailed || len(s.SubRows) == 0 {
			continue
		}
		fmt.Fprintf(b, "- **%s**\n", s.Name)
		for _, row := range s.SubRows {
			fmt.Fprintf(b, "  - `%s` — %s\n", row.Key, row.Value)
		}
	}
	b.WriteString("\n")
}

func writeHeader(b *strings.Builder, r *VerifyResult) {
	switch {
	case r.Signer != nil && r.Signer.Identity != "":
		fmt.Fprintf(b, "**Signer:** %s", r.Signer.Identity)
		if r.Signer.Issuer != "" {
			fmt.Fprintf(b, " (issuer %s)", r.Signer.Issuer)
		}
		if r.Signer.RekorLogIndex != nil {
			fmt.Fprintf(b, "  •  **Rekor:** index %d", *r.Signer.RekorLogIndex)
		}
		b.WriteString("\n")
	case signatureStepStatus(r) == StepFailed:
		// A signed bundle whose signature didn't verify is meaningfully
		// different from a bundle that carries no signature at all —
		// don't claim "unsigned" when verification actually failed.
		b.WriteString("**Signer:** _signature verification failed (see Verification steps)_\n")
	default:
		b.WriteString("**Signer:** _unsigned bundle_\n")
	}
	if r.Predicate != nil {
		fmt.Fprintf(b, "**AICR:** %s  •  **Schema:** %s",
			r.Predicate.AICRVersion, r.Predicate.SchemaVersion)
	}
	if r.BundleDigest != "" {
		fmt.Fprintf(b, "  •  **Bundle digest:** %s", r.BundleDigest)
	}
	b.WriteString("\n\n")
}

// signatureStepStatus returns the recorded status of the signature
// step, or "" if no signature step was recorded.
func signatureStepStatus(r *VerifyResult) StepStatus {
	for _, s := range r.Steps {
		if s.Step == stepSignature {
			return s.Status
		}
	}
	return ""
}

func writeFingerprint(b *strings.Builder, p *attestation.Predicate) {
	if p == nil || len(p.CriteriaMatch.PerDimension) == 0 {
		return
	}
	verdict := "✓ all recipe criteria dimensions satisfied"
	if !p.CriteriaMatch.Matched {
		verdict = "✗ one or more criteria dimensions mismatched"
	}
	fmt.Fprintf(b, "### Cluster fingerprint\n%s\n\n| Dimension | Outcome |\n|---|---|\n", verdict)
	for _, d := range p.CriteriaMatch.PerDimension {
		req := d.RecipeRequires
		got := d.FingerprintProvides
		if req == "" {
			req = "(any)"
		}
		if got == "" {
			got = "(not captured)"
		}
		fmt.Fprintf(b, "| %s | %s recipe=%s snapshot=%s |\n", d.Dimension, d.Match, req, got)
	}
	b.WriteString("\n")
}

func writePhases(b *strings.Builder, p *attestation.Predicate) {
	if p == nil || len(p.Phases) == 0 {
		return
	}
	b.WriteString("### Phase results\n")
	for _, ph := range attestation.AllPhases {
		s, ok := p.Phases[ph]
		if !ok {
			continue
		}
		marker := "✓"
		if s.Failed > 0 {
			marker = "✗"
		}
		fmt.Fprintf(b, "- %s **%s** passed=%d failed=%d skipped=%d\n",
			marker, ph, s.Passed, s.Failed, s.Skipped)
	}
	b.WriteString("\n")
}

func writeBOM(b *strings.Builder, p *attestation.Predicate) {
	if p == nil || p.BOM.Format == "" {
		return
	}
	fmt.Fprintf(b, "### BOM\n%s %s — %d components (digest %s)\n\n",
		p.BOM.Format, p.BOM.Version, p.BOM.ImageCount, p.BOM.Digest)
}

func writeSteps(b *strings.Builder, r *VerifyResult) {
	b.WriteString("### Verification steps\n| # | Step | Status | Detail |\n|---|---|---|---|\n")
	for _, s := range r.Steps {
		fmt.Fprintf(b, "| %d | %s | %s | %s |\n",
			s.Step, s.Name, s.Status, escapeMD(s.Detail))
	}
	b.WriteString("\n")
}

func writeVerdict(b *strings.Builder, r *VerifyResult) {
	b.WriteString("---\n")
	switch r.Exit {
	case ExitValidPassed:
		b.WriteString("**Verdict:** bundle valid — all checks passed (exit 0)\n")
	case ExitValidPhaseFailures:
		b.WriteString("**Verdict:** bundle valid; recorded validator phase results show failures (exit 1, informational)\n")
	case ExitInvalid:
		b.WriteString("**Verdict:** bundle invalid — integrity check(s) failed (exit 2)\n")
	}
}

// escapeMD escapes pipe characters so table rows don't break and
// collapses newlines because table rows are single-line.
func escapeMD(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "|", `\|`)
}

// RenderJSON serializes the VerifyResult deterministically.
func RenderJSON(r *VerifyResult) ([]byte, error) {
	out, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to marshal verify result", err)
	}
	return append(out, '\n'), nil
}
