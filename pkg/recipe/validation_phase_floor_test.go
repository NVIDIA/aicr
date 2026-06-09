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
// performance phase from warn-only to required. Default OFF until the
// Azure/OCI performance testbeds land.
//
// knownGaps allowlist: keyed by (overlay, phase) so a regression in a
// different phase is not silently masked. Add entries only as a tracked,
// time-bounded escape hatch when a new gap is uncovered and a follow-up
// issue is filed to close it; the stale-entry guard catches drift when
// the gap closes. See the in-file comment above the `knownGaps` map
// declaration for current entries and the issues that drain them.

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

// knownGaps lists (overlay, phase) pairs that fail the floor today.
// Each entry downgrades an Errorf to a Logf prefixed with "KNOWN GAP:"
// and is paired with a tracking issue. Reserve entries as a tracked,
// time-bounded escape hatch only when a follow-up issue exists to drain
// them; the stale-entry guard at the end of TestOverlayValidationPhaseFloor
// catches drift.
var knownGaps = map[string]map[string]bool{}

// classification captures the inputs that drive the per-intent floor.
type classification struct {
	Intent      CriteriaIntentType
	Service     CriteriaServiceType
	Platform    CriteriaPlatformType
	Accelerator CriteriaAcceleratorType
	IsKind      bool
}

// String renders a classification for failure messages.
func (c classification) String() string {
	return fmt.Sprintf("intent=%s service=%s accelerator=%s platform=%s kind=%t",
		c.Intent, c.Service, c.Accelerator, c.Platform, c.IsKind)
}

// isAcceleratorBound reports whether the classification corresponds to
// an accelerator-bound query. Accelerator-unbound classifications
// (intermediates without an `accelerator:` criterion) are exempt from
// the deployment + performance floors because both phases carry
// accelerator-specific values (gpu-operator version pin, NCCL bandwidth
// threshold) that live on accelerator-bound wildcards or concrete leaves.
func (c classification) isAcceleratorBound() bool {
	return c.Accelerator != "" && c.Accelerator != CriteriaAcceleratorAny
}

// requiresDeployment reports whether the per-intent floor requires the
// deployment phase for this classification.
func (c classification) requiresDeployment() bool {
	return c.isAcceleratorBound()
}

// requiresPerformance reports whether the per-intent floor recommends
// the performance phase for this classification.
func (c classification) requiresPerformance() bool {
	if c.IsKind || !c.isAcceleratorBound() {
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
		Intent:      criteria.Intent,
		Service:     criteria.Service,
		Platform:    criteria.Platform,
		Accelerator: criteria.Accelerator,
		IsKind:      criteria.Service == CriteriaServiceKind,
	}
}

// hasGPUOperatorVersionCheck reports whether the deployment phase
// declares the `gpu-operator-version` check by name. Without the
// check entry, the validator never runs the version assertion no
// matter what constraints are declared — the resolved recipe ships
// with a constraint that no runtime path evaluates.
func hasGPUOperatorVersionCheck(p *ValidationPhase) bool {
	if p == nil {
		return false
	}
	for _, name := range p.Checks {
		if name == "gpu-operator-version" {
			return true
		}
	}
	return false
}

// hasGPUOperatorVersionConstraint reports whether the deployment phase
// declares a `Deployment.gpu-operator.version` constraint. Without it
// the gpu-operator-version check at runtime is a no-op (it skips when
// no constraint targets that path), so the resolved recipe ships with
// the check name but no version assertion. See #969.
//
// Note: the two helpers are independent — a leaf can declare either
// in isolation. The floor test asserts both are present so neither
// half of the gate can silently no-op.
func hasGPUOperatorVersionConstraint(p *ValidationPhase) bool {
	if p == nil {
		return false
	}
	for _, c := range p.Constraints {
		if c.Name == "Deployment.gpu-operator.version" {
			return true
		}
	}
	return false
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
	t.Logf("knownGaps allowlist entries: %d", knownGapEntries())

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
					t.Logf("KNOWN GAP: %s", msg)
					return
				}
				t.Error(msg)
			}

			// Required: deployment for accelerator-bound classifications;
			// conformance for every classification.
			//
			// For deployment, the gate has two independent halves and
			// both must be present — either half missing turns the
			// version assertion into a no-op:
			//   • `gpu-operator-version` in deployment.checks (the
			//     validator only runs the check if its name is
			//     declared).
			//   • `Deployment.gpu-operator.version` in
			//     deployment.constraints (the check skips at runtime
			//     when no constraint targets that path).
			if class.requiresDeployment() {
				if result.Validation == nil || result.Validation.Deployment == nil {
					fail("deployment")
				} else {
					if !hasGPUOperatorVersionCheck(result.Validation.Deployment) {
						fail("deployment.checks.gpu-operator-version")
					}
					if !hasGPUOperatorVersionConstraint(result.Validation.Deployment) {
						fail("deployment.constraints.Deployment.gpu-operator.version")
					}
				}
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
// intent x service x platform x accelerator matrix.
func TestClassifyOverlay(t *testing.T) {
	tests := []struct {
		name               string
		intent             CriteriaIntentType
		service            CriteriaServiceType
		platform           CriteriaPlatformType
		accelerator        CriteriaAcceleratorType
		wantIsKind         bool
		wantRequiresDeploy bool
		wantRequiresPerf   bool
	}{
		// Accelerator-bound: deployment required; perf depends on intent/platform.
		{"training-eks-h100", CriteriaIntentTraining, CriteriaServiceEKS, CriteriaPlatformAny, CriteriaAcceleratorH100, false, true, true},
		{"training-aks-h100-kubeflow", CriteriaIntentTraining, CriteriaServiceAKS, CriteriaPlatformKubeflow, CriteriaAcceleratorH100, false, true, true},
		{"training-kind-h100", CriteriaIntentTraining, CriteriaServiceKind, CriteriaPlatformAny, CriteriaAcceleratorH100, true, true, false},
		{"inference-eks-h100-plain", CriteriaIntentInference, CriteriaServiceEKS, CriteriaPlatformAny, CriteriaAcceleratorH100, false, true, false},
		{"inference-eks-h100-dynamo", CriteriaIntentInference, CriteriaServiceEKS, CriteriaPlatformDynamo, CriteriaAcceleratorH100, false, true, true},
		{"inference-eks-h100-nim", CriteriaIntentInference, CriteriaServiceEKS, CriteriaPlatformNIM, CriteriaAcceleratorH100, false, true, true},
		{"inference-kind-h100-dynamo", CriteriaIntentInference, CriteriaServiceKind, CriteriaPlatformDynamo, CriteriaAcceleratorH100, true, true, false},
		// Accelerator-unbound intermediates: both deployment and perf
		// are exempt — version pins and NCCL thresholds live on
		// accelerator-bound wildcards and concrete leaves.
		{"training-eks-no-accelerator", CriteriaIntentTraining, CriteriaServiceEKS, CriteriaPlatformAny, "", false, false, false},
		{"training-gke-accelerator-any", CriteriaIntentTraining, CriteriaServiceGKE, CriteriaPlatformAny, CriteriaAcceleratorAny, false, false, false},
		{"inference-eks-dynamo-no-accelerator", CriteriaIntentInference, CriteriaServiceEKS, CriteriaPlatformDynamo, "", false, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Criteria{Intent: tt.intent, Service: tt.service, Platform: tt.platform, Accelerator: tt.accelerator}
			class := classifyOverlay(c)
			if class.IsKind != tt.wantIsKind {
				t.Errorf("IsKind = %v, want %v", class.IsKind, tt.wantIsKind)
			}
			if class.requiresDeployment() != tt.wantRequiresDeploy {
				t.Errorf("requiresDeployment() = %v, want %v",
					class.requiresDeployment(), tt.wantRequiresDeploy)
			}
			if class.requiresPerformance() != tt.wantRequiresPerf {
				t.Errorf("requiresPerformance() = %v, want %v",
					class.requiresPerformance(), tt.wantRequiresPerf)
			}
		})
	}
}

// TestGPUOperatorVersionGateHelpers exercises both halves of the
// deployment-phase gpu-operator-version gate independently. Each helper
// must return true only when its half is present so the floor test can
// surface a precise failure when only one half is declared.
func TestGPUOperatorVersionGateHelpers(t *testing.T) {
	tests := []struct {
		name           string
		phase          *ValidationPhase
		wantCheck      bool
		wantConstraint bool
	}{
		{
			name:           "nil phase",
			phase:          nil,
			wantCheck:      false,
			wantConstraint: false,
		},
		{
			name:           "empty phase",
			phase:          &ValidationPhase{},
			wantCheck:      false,
			wantConstraint: false,
		},
		{
			name: "check only — false-green scenario",
			phase: &ValidationPhase{
				Checks: []string{"operator-health", "gpu-operator-version"},
			},
			wantCheck:      true,
			wantConstraint: false,
		},
		{
			name: "constraint only — false-green scenario CodeRabbit flagged",
			phase: &ValidationPhase{
				Constraints: []Constraint{
					{Name: "Deployment.gpu-operator.version", Value: ">= v24.6.0"},
				},
			},
			wantCheck:      false,
			wantConstraint: true,
		},
		{
			name: "both present — passes the gate",
			phase: &ValidationPhase{
				Checks: []string{"operator-health", "gpu-operator-version", "check-nvidia-smi"},
				Constraints: []Constraint{
					{Name: "Deployment.gpu-operator.version", Value: ">= v24.6.0"},
				},
			},
			wantCheck:      true,
			wantConstraint: true,
		},
		{
			name: "unrelated check and constraint",
			phase: &ValidationPhase{
				Checks: []string{"operator-health", "expected-resources"},
				Constraints: []Constraint{
					{Name: "K8s.server.version", Value: ">= 1.32"},
				},
			},
			wantCheck:      false,
			wantConstraint: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasGPUOperatorVersionCheck(tt.phase); got != tt.wantCheck {
				t.Errorf("hasGPUOperatorVersionCheck() = %v, want %v", got, tt.wantCheck)
			}
			if got := hasGPUOperatorVersionConstraint(tt.phase); got != tt.wantConstraint {
				t.Errorf("hasGPUOperatorVersionConstraint() = %v, want %v", got, tt.wantConstraint)
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
