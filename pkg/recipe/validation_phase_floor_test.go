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

// validation_phase_floor_test.go enforces a per-intent validation phase
// floor on every overlay production can return as a maximal-leaf
// candidate for some query. For each candidate it calls BuildRecipeResult
// with the overlay's own criteria — the same code path the CLI and API
// use — and asserts the resolved validation contains the required phases
// per the candidate's classification. Wildcard fragments (intent or
// service "any") are excluded because their criteria do not correspond
// to a meaningful user query.
//
// Closes the loophole that let GPU overlays drift to conformance-only
// without a CI gate (see issue #970, companion #969).
//
// Per-intent floor:
//   Training (non-Kind)               : deployment + conformance   [performance recommended]
//   Inference Dynamo / NIM (non-Kind) : deployment + conformance   [performance recommended]
//   Inference (plain)                 : deployment + conformance
//   Kind (any intent)                 : deployment + conformance
//
// Strict toggle: AICR_VALIDATION_FLOOR_STRICT=1 promotes the recommended
// performance phase from warn-only to required. Default OFF until #969
// closes the data gap and Azure/OCI performance testbeds land.
//
// knownGaps allowlist: keyed by (overlay, phase) so a regression in a
// different phase is not silently masked. Drain as #969 lands; new
// overlay/phase failures that are not allowlisted block CI.

package recipe

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

const strictEnvVar = "AICR_VALIDATION_FLOOR_STRICT"

// knownGaps lists (overlay, phase) pairs that fail the floor today and
// are tracked under #969. Each entry downgrades an Errorf to a Logf
// prefixed with "KNOWN GAP (#969):". Drain this map as #969 lands per-
// overlay fixes; delete the map entirely once empty. New (overlay, phase)
// failures not in this map block CI.
var knownGaps = map[string]map[string]bool{
	"aks":                                {"deployment": true},
	"aks-inference":                      {"deployment": true},
	"aks-training":                       {"deployment": true},
	"eks":                                {"deployment": true},
	"eks-inference":                      {"deployment": true},
	"eks-training":                       {"deployment": true},
	"gb200-oke-inference":                {"deployment": true},
	"gb200-oke-training":                 {"deployment": true},
	"gb200-oke-ubuntu-inference":         {"deployment": true},
	"gb200-oke-ubuntu-inference-dynamo":  {"deployment": true},
	"gb200-oke-ubuntu-training":          {"deployment": true},
	"gb200-oke-ubuntu-training-kubeflow": {"deployment": true},
	"gke-cos":                            {"deployment": true},
	"gke-cos-inference":                  {"deployment": true},
	"gke-cos-training":                   {"deployment": true},
	"h100-aks-inference":                 {"deployment": true},
	"h100-aks-ubuntu-inference":          {"deployment": true},
	"h100-eks-inference":                 {"deployment": true},
	"h100-eks-ubuntu-inference":          {"deployment": true},
	"h100-gke-cos-inference":             {"deployment": true},
	"h100-kind-inference":                {"deployment": true},
	"h100-kind-inference-dynamo":         {"deployment": true},
	"h100-kind-training":                 {"deployment": true},
	"h100-kind-training-kubeflow":        {"deployment": true},
	"h100-kind-training-slurm":           {"deployment": true},
	"kind":                               {"deployment": true},
	"kind-inference":                     {"deployment": true},
	"lke":                                {"deployment": true},
	"lke-inference":                      {"deployment": true},
	"lke-training":                       {"deployment": true},
	"oke":                                {"deployment": true},
	"oke-inference":                      {"deployment": true},
	"oke-training":                       {"deployment": true},
	"rtx-pro-6000-lke-inference":         {"deployment": true},
	"rtx-pro-6000-lke-training":          {"deployment": true},
	"rtx-pro-6000-lke-ubuntu-inference":  {"deployment": true},
	"rtx-pro-6000-lke-ubuntu-training":   {"deployment": true},
}

// classification captures the inputs that drive the per-intent floor.
type classification struct {
	Intent   CriteriaIntentType
	Service  CriteriaServiceType
	Platform CriteriaPlatformType
	IsKind   bool
}

// String renders a classification for failure messages.
func (c classification) String() string {
	return fmt.Sprintf("intent=%s service=%s platform=%s kind=%t",
		c.Intent, c.Service, c.Platform, c.IsKind)
}

// requiresPerformance reports whether the per-intent floor recommends
// the performance phase for this classification.
func (c classification) requiresPerformance() bool {
	if c.IsKind {
		return false
	}
	if c.Intent == CriteriaIntentTraining {
		return true
	}
	dynamoOrNIM := c.Platform == CriteriaPlatformDynamo || c.Platform == CriteriaPlatformNIM
	return c.Intent == CriteriaIntentInference && dynamoOrNIM
}

// classifyOverlay derives the classification from resolved criteria.
func classifyOverlay(criteria *Criteria) classification {
	return classification{
		Intent:   criteria.Intent,
		Service:  criteria.Service,
		Platform: criteria.Platform,
		IsKind:   criteria.Service == CriteriaServiceKind,
	}
}

// resolvedPhases returns the names of phases that are set on v.
func resolvedPhases(v *ValidationConfig) []string {
	if v == nil {
		return nil
	}
	var out []string
	if v.Readiness != nil {
		out = append(out, "readiness")
	}
	if v.Deployment != nil {
		out = append(out, "deployment")
	}
	if v.Performance != nil {
		out = append(out, "performance")
	}
	if v.Conformance != nil {
		out = append(out, "conformance")
	}
	return out
}

// enumerateGateableOverlays returns the names of every overlay production
// can return as a maximal-leaf candidate for some query — every overlay
// with concrete criteria, minus wildcard fragments whose intent or service
// is "any". Wildcard fragments are cross-cutting overlays composed onto
// specific queries — see docs/contributor/data.md#criteria-wildcard-overlays —
// not standalone user-facing entry points.
//
// Concrete intermediate overlays (e.g., h100-gke-cos-training) are NOT
// excluded merely because another overlay references them as spec.base.
// Production's filterToMaximalLeaves is per-query, so an intermediate is
// the maximal leaf for queries that don't narrow further (e.g., no
// platform specified); the gate must cover that case too.
func enumerateGateableOverlays(s *MetadataStore) []string {
	var out []string
	for name, overlay := range s.Overlays {
		c := overlay.Spec.Criteria
		if c == nil {
			continue
		}
		if c.Intent == CriteriaIntentAny || c.Service == CriteriaServiceAny {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// knownGapEntries totals the number of (overlay, phase) downgrade pairs
// in the allowlist for logging.
func knownGapEntries() int {
	n := 0
	for _, phases := range knownGaps {
		n += len(phases)
	}
	return n
}

// TestOverlayValidationPhaseFloor asserts every gateable overlay's
// production-resolved validation block contains the per-intent required
// phases. See file header for the floor matrix, the strict-mode toggle,
// and the allowlist contract.
func TestOverlayValidationPhaseFloor(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}

	strict := os.Getenv(strictEnvVar) == "1"
	t.Logf("strict mode (%s=1): %t", strictEnvVar, strict)
	t.Logf("knownGaps allowlist entries (#969): %d", knownGapEntries())

	overlays := enumerateGateableOverlays(store)
	t.Logf("gateable overlays discovered: %d", len(overlays))
	if len(overlays) == 0 {
		t.Fatal("no gateable overlays discovered; the floor check would be vacuous — " +
			"verify enumerateGateableOverlays and the recipes/overlays/ directory")
	}

	// triggered tracks which (overlay, phase) knownGaps entries actually
	// downgraded a failure during this run. Subtests run sequentially
	// (no t.Parallel), so plain map writes from the fail closure are safe.
	triggered := make(map[string]map[string]bool, len(knownGaps))

	for _, name := range overlays {
		t.Run(name, func(t *testing.T) {
			overlay := store.Overlays[name]
			class := classifyOverlay(overlay.Spec.Criteria)

			// Use the production resolver so the test gates the same
			// ValidationConfig the CLI and API actually produce — wildcard
			// overlay contributions and mixins included.
			result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
			if err != nil {
				t.Fatalf("BuildRecipeResult: %v", err)
			}
			phases := resolvedPhases(result.Validation)

			report := func(severity, kind, phase string) string {
				return fmt.Sprintf(
					"%s overlay %q [%s]\n  resolved phases: %s\n  missing %s: %s",
					severity, name, class,
					strings.Join(phases, ", "),
					kind, phase,
				)
			}

			// fail records a missing required phase. (overlay, phase)
			// pairs in knownGaps are downgraded to logs so the contract
			// can land before #969 closes the data gap.
			fail := func(phase string) {
				msg := report("FAIL", "required", phase)
				if knownGaps[name][phase] {
					if triggered[name] == nil {
						triggered[name] = map[string]bool{}
					}
					triggered[name][phase] = true
					t.Logf("KNOWN GAP (#969): %s", msg)
					return
				}
				t.Error(msg)
			}

			// Required: deployment + conformance for every classification.
			if result.Validation == nil || result.Validation.Deployment == nil {
				fail("deployment")
			}
			if result.Validation == nil || result.Validation.Conformance == nil {
				fail("conformance")
			}

			// Performance: warn-only by default; strict mode promotes to
			// required. Either way, the knownGaps lookup downgrades the
			// result so the allowlist contract holds in both modes.
			if class.requiresPerformance() && (result.Validation == nil || result.Validation.Performance == nil) {
				if strict {
					fail("performance")
				} else {
					t.Log(report("WARN", "recommended", "performance"))
				}
			}
		})
	}

	// Hygiene: every (overlay, phase) entry in knownGaps must have
	// downgraded at least one failure. Stale entries indicate the data
	// has caught up — remove them so a future regression in that phase
	// is not silently masked.
	var stale []string
	for name, phases := range knownGaps {
		for phase := range phases {
			if !triggered[name][phase] {
				stale = append(stale, fmt.Sprintf("%s:%s", name, phase))
			}
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("stale knownGaps entries — overlay/phase now meets the floor; "+
			"remove from knownGaps: %s", strings.Join(stale, ", "))
	}
}

// TestClassifyOverlay exercises the classification function across the
// intent x service x platform matrix.
func TestClassifyOverlay(t *testing.T) {
	tests := []struct {
		name             string
		intent           CriteriaIntentType
		service          CriteriaServiceType
		platform         CriteriaPlatformType
		wantIsKind       bool
		wantRequiresPerf bool
	}{
		{"training-eks-plain", CriteriaIntentTraining, CriteriaServiceEKS, CriteriaPlatformAny, false, true},
		{"training-aks-kubeflow", CriteriaIntentTraining, CriteriaServiceAKS, CriteriaPlatformKubeflow, false, true},
		{"training-kind", CriteriaIntentTraining, CriteriaServiceKind, CriteriaPlatformAny, true, false},
		{"inference-eks-plain", CriteriaIntentInference, CriteriaServiceEKS, CriteriaPlatformAny, false, false},
		{"inference-eks-dynamo", CriteriaIntentInference, CriteriaServiceEKS, CriteriaPlatformDynamo, false, true},
		{"inference-eks-nim", CriteriaIntentInference, CriteriaServiceEKS, CriteriaPlatformNIM, false, true},
		{"inference-kind-dynamo", CriteriaIntentInference, CriteriaServiceKind, CriteriaPlatformDynamo, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Criteria{Intent: tt.intent, Service: tt.service, Platform: tt.platform}
			class := classifyOverlay(c)
			if class.IsKind != tt.wantIsKind {
				t.Errorf("IsKind = %v, want %v", class.IsKind, tt.wantIsKind)
			}
			if class.requiresPerformance() != tt.wantRequiresPerf {
				t.Errorf("requiresPerformance() = %v, want %v",
					class.requiresPerformance(), tt.wantRequiresPerf)
			}
		})
	}
}

// TestResolvedPhases verifies the phase-name extractor for ValidationConfig.
func TestResolvedPhases(t *testing.T) {
	tests := []struct {
		name string
		in   *ValidationConfig
		want []string
	}{
		{"nil config", nil, nil},
		{"empty config", &ValidationConfig{}, nil},
		{
			"deployment + conformance",
			&ValidationConfig{Deployment: &ValidationPhase{}, Conformance: &ValidationPhase{}},
			[]string{"deployment", "conformance"},
		},
		{
			"all four",
			&ValidationConfig{
				Readiness:   &ValidationPhase{},
				Deployment:  &ValidationPhase{},
				Performance: &ValidationPhase{},
				Conformance: &ValidationPhase{},
			},
			[]string{"readiness", "deployment", "performance", "conformance"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolvedPhases(tt.in)
			if !equalStringSlice(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
