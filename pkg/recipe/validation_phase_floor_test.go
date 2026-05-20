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
// floor on every leaf overlay. For each user-selectable overlay it walks
// the spec.base chain, resolves the merged ValidationConfig per
// pkg/recipe/metadata.go Merge semantics, classifies the overlay by
// intent/service/platform, and asserts the resolved validation contains
// the required phases.
//
// Closes the loophole that let 27 of 41 GPU overlays drift to
// conformance-only without a CI gate (see issue #970, companion #969).
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
// knownGaps allowlist: overlays tracked by #969 are listed below so the
// contract can land independently of the data fix. Entries downgrade
// failures to logs. Empty this map as #969 closes; the test fails any
// NEW overlay added without the floor because it is not allowlisted.

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

// knownGaps lists leaf overlays that fail the floor today and are tracked
// under #969. Each entry downgrades an Errorf to a Logf prefixed with
// "KNOWN GAP (#969):". Drain this map as #969 lands; delete the map and
// gap handling once empty.
var knownGaps = map[string]bool{
	"gb200-oke-ubuntu-inference-dynamo":  true,
	"gb200-oke-ubuntu-training-kubeflow": true,
	"h100-kind-inference-dynamo":         true,
	"h100-kind-training-kubeflow":        true,
	"h100-kind-training-slurm":           true,
	"rtx-pro-6000-lke-ubuntu-inference":  true,
	"rtx-pro-6000-lke-ubuntu-training":   true,
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

// resolveValidation walks the base: chain for the named recipe and
// returns the merged ValidationConfig using the same Merge semantics
// the production resolver uses.
func resolveValidation(s *MetadataStore, name string) (*ValidationConfig, error) {
	chain, err := s.resolveInheritanceChain(name)
	if err != nil {
		return nil, err
	}
	merged := &RecipeMetadataSpec{}
	for _, recipe := range chain {
		merged.Merge(&recipe.Spec)
	}
	return merged.Validation, nil
}

// discoverLeafOverlays returns the names of every user-selectable leaf
// overlay. A leaf has criteria, is not referenced as another overlay's
// spec.base, and is not a criteria-wildcard fragment (one whose
// criteria.intent or criteria.service is "any"). Wildcard overlays are
// cross-cutting fragments applied via the resolver's wildcard-match
// path — see docs/contributor/data.md#criteria-wildcard-overlays — not
// user-selectable entry points subject to the per-intent floor.
// Sorted for deterministic test output.
func discoverLeafOverlays(s *MetadataStore) []string {
	referencedAsBase := make(map[string]bool)
	for _, overlay := range s.Overlays {
		if overlay.Spec.Base != "" {
			referencedAsBase[overlay.Spec.Base] = true
		}
	}
	var leaves []string
	for name, overlay := range s.Overlays {
		if referencedAsBase[name] {
			continue
		}
		c := overlay.Spec.Criteria
		if c == nil {
			continue
		}
		if c.Intent == CriteriaIntentAny || c.Service == CriteriaServiceAny {
			continue
		}
		leaves = append(leaves, name)
	}
	sort.Strings(leaves)
	return leaves
}

// TestOverlayValidationPhaseFloor asserts every leaf overlay's resolved
// validation block contains the per-intent required phases. See file
// header for the floor matrix and the strict-mode toggle.
func TestOverlayValidationPhaseFloor(t *testing.T) {
	ctx := context.Background()
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}

	strict := os.Getenv(strictEnvVar) == "1"
	t.Logf("strict mode (%s=1): %t", strictEnvVar, strict)
	t.Logf("knownGaps allowlist size (#969): %d", len(knownGaps))

	leaves := discoverLeafOverlays(store)
	t.Logf("leaf overlays discovered: %d", len(leaves))

	for _, name := range leaves {
		t.Run(name, func(t *testing.T) {
			overlay := store.Overlays[name]
			class := classifyOverlay(overlay.Spec.Criteria)

			validation, err := resolveValidation(store, name)
			if err != nil {
				t.Fatalf("resolveValidation: %v", err)
			}
			phases := resolvedPhases(validation)

			report := func(severity, kind, phase string) string {
				return fmt.Sprintf(
					"%s overlay %q [%s]\n  resolved phases: %s\n  missing %s: %s",
					severity, name, class,
					strings.Join(phases, ", "),
					kind, phase,
				)
			}

			// fail records a missing required phase. Overlays in knownGaps
			// are downgraded to logs so the contract can land before #969
			// finishes closing the data gap.
			fail := func(phase string) {
				msg := report("FAIL", "required", phase)
				if knownGaps[name] {
					t.Logf("KNOWN GAP (#969): %s", msg)
					return
				}
				t.Error(msg)
			}

			// Required: deployment + conformance for every classification.
			if validation == nil || validation.Deployment == nil {
				fail("deployment")
			}
			if validation == nil || validation.Conformance == nil {
				fail("conformance")
			}

			// Performance: warn-only by default; strict mode promotes to
			// required. Either way, knownGaps downgrades the result to
			// preserve the allowlist contract.
			if class.requiresPerformance() && (validation == nil || validation.Performance == nil) {
				if strict {
					fail("performance (strict)")
				} else {
					t.Log(report("WARN", "recommended", "performance"))
				}
			}
		})
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
