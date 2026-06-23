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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

func TestRenderMarkdown_ShowsRedactionPolicy(t *testing.T) {
	r := &VerifyResult{
		Predicate: &attestation.Predicate{
			Redaction: &attestation.RedactionInfo{
				Policy:  "minimal",
				Version: "v1",
				Applied: []string{"ctrf.tests.omit:stdout", "snapshot.measurements.allowlist"},
			},
		},
		Exit: ExitValidPassed,
	}
	out := RenderMarkdown(r)
	if !strings.Contains(out, "minimal") || !strings.Contains(out, "v1") {
		t.Errorf("expected redaction policy/version in markdown:\n%s", out)
	}
	if !strings.Contains(out, "snapshot.measurements.allowlist") {
		t.Errorf("expected applied rules in markdown:\n%s", out)
	}
}

func TestRenderMarkdown_NoRedactionSectionForFullBundle(t *testing.T) {
	r := &VerifyResult{
		Predicate: &attestation.Predicate{},
		Exit:      ExitValidPassed,
	}
	out := RenderMarkdown(r)
	if strings.Contains(out, "Redaction") {
		t.Errorf("full bundle must not render a redaction section:\n%s", out)
	}
}
