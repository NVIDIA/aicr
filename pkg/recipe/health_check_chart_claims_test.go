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

package recipe

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// verifiedChartClaimPattern matches a "verified against chart X.Y.Z" comment,
// including one that comment wrapping splits across lines. It captures the raw
// token after "chart", so a claim that names no version fails the comparison
// instead of being skipped.
var verifiedChartClaimPattern = regexp.MustCompile(`(?i)verified\s+(?:#\s*)?against\s+(?:#\s*)?chart\s+(?:#\s*)?(\S+)`)

// chartClaim is one "verified against chart" claim: the version it names,
// without trailing sentence punctuation, and the line that version is on.
type chartClaim struct {
	line    int
	version string
}

func verifiedChartClaims(content string) []chartClaim {
	matches := verifiedChartClaimPattern.FindAllStringSubmatchIndex(content, -1)
	claims := make([]chartClaim, 0, len(matches))
	for _, m := range matches {
		claims = append(claims, chartClaim{
			line:    strings.Count(content[:m[2]], "\n") + 1,
			version: strings.TrimRight(content[m[2]:m[3]], ".,;:)"),
		})
	}
	return claims
}

// TestHealthCheckChartClaimsMatchRegistry ensures every "verified against
// chart X.Y.Z" claim in a component's health check names the chart version the
// registry pins for that component. The claim records which chart the check's
// CRD assertions were verified against. A defaultVersion bump that leaves it
// behind ships assertions nobody re-checked, and a CRD the new chart renames or
// removes then surfaces only as a health-check failure on a live cluster.
//
// A health check shared by several components is held to each of their pins,
// because it runs against each of their charts.
//
// When this test fails, render the pinned chart's CRDs (helm template <chart>
// --version <pin> --include-crds), confirm every CRD the check asserts is still
// installed, then update the claim.
//
// See https://github.com/NVIDIA/aicr/issues/1948.
func TestHealthCheckChartClaimsMatchRegistry(t *testing.T) {
	t.Parallel()

	registry, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}

	claims := 0
	for _, name := range registry.Names() {
		cfg := registry.Get(name)
		if cfg == nil || cfg.HealthCheck.AssertFile == "" {
			continue
		}
		data, err := defaultEmbeddedProvider.ReadFile(t.Context(), cfg.HealthCheck.AssertFile)
		if err != nil {
			t.Errorf("component %q: read %s: %v", name, cfg.HealthCheck.AssertFile, err)
			continue
		}
		for _, claim := range verifiedChartClaims(string(data)) {
			claims++
			if claim.version != cfg.Helm.DefaultVersion {
				t.Errorf("%s:%d says it was verified against chart %q, but component %q is pinned "+
					"to chart %q; re-verify the asserted CRDs against the pinned chart, "+
					"then update the claim",
					cfg.HealthCheck.AssertFile, claim.line, claim.version, name, cfg.Helm.DefaultVersion)
			}
		}
	}
	if claims == 0 {
		t.Fatal(`no health check carries a "verified against chart" claim; this guard would be vacuous`)
	}
}

func TestVerifiedChartClaims(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		want    []chartClaim
	}{
		{"single line", "# (verified against chart 3.22.2; re-verify)", []chartClaim{{1, "3.22.2"}}},
		{"wrapped before chart", "# (verified against\n# chart 26.4.1; re-verify).", []chartClaim{{2, "26.4.1"}}},
		{"wrapped before against", "  # (verified\n  # against chart 2.2.0)", []chartClaim{{2, "2.2.0"}}},
		{"prerelease at sentence end", "# Verified against chart v0.1.0-alpha.12.", []chartClaim{{1, "v0.1.0-alpha.12"}}},
		{"every claim", "# verified against chart 1.0\n# verified against chart 2.0", []chartClaim{{1, "1.0"}, {2, "2.0"}}},
		{"claim without a version", "# verified against chart defaults", []chartClaim{{1, "defaults"}}},
		{"not a claim", "# verified against the rendered chart: 1.3.0", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := verifiedChartClaims(tt.content); !slices.Equal(got, tt.want) {
				t.Errorf("verifiedChartClaims() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
