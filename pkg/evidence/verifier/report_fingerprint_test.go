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
	"github.com/NVIDIA/aicr/pkg/fingerprint"
)

func TestRenderMarkdown_FingerprintVerdicts(t *testing.T) {
	tests := []struct {
		name        string
		match       fingerprint.MatchResult
		wantVerdict string
		wantRow     string
	}{
		{
			name: "all matched",
			match: fingerprint.MatchResult{Matched: true, PerDimension: []fingerprint.DimensionDiff{
				{Dimension: fingerprint.DimensionService, RecipeRequires: "eks", FingerprintProvides: "eks", Match: fingerprint.DimensionMatched},
			}},
			wantVerdict: "✓ all recipe criteria dimensions satisfied",
			wantRow:     "| service | matched recipe=eks snapshot=eks |",
		},
		{
			name: "opt-in-only service",
			match: fingerprint.MatchResult{Matched: true, PerDimension: []fingerprint.DimensionDiff{
				{Dimension: fingerprint.DimensionService, RecipeRequires: "generic", FingerprintProvides: "metal3", Match: fingerprint.DimensionNotInferable},
			}},
			wantVerdict: "✓ criteria satisfied; opt-in-only dimensions recorded as not-inferable",
			wantRow:     "| service | not-inferable recipe=generic snapshot=metal3 |",
		},
		{
			name: "mismatch",
			match: fingerprint.MatchResult{Matched: false, PerDimension: []fingerprint.DimensionDiff{
				{Dimension: fingerprint.DimensionService, RecipeRequires: "eks", FingerprintProvides: "metal3", Match: fingerprint.DimensionMismatched},
			}},
			wantVerdict: "✗ one or more criteria dimensions mismatched",
			wantRow:     "| service | mismatched recipe=eks snapshot=metal3 |",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			md := RenderMarkdown(&VerifyResult{
				Predicate: &attestation.Predicate{CriteriaMatch: tt.match},
				Exit:      ExitValidPassed,
			})
			if !strings.Contains(md, tt.wantVerdict) {
				t.Errorf("verdict %q missing from:\n%s", tt.wantVerdict, md)
			}
			if !strings.Contains(md, tt.wantRow) {
				t.Errorf("row %q missing from:\n%s", tt.wantRow, md)
			}
		})
	}
}
