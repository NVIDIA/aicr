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

package health

import (
	"bytes"
	"context"
	stderrors "errors"
	"io/fs"
	"reflect"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

func TestClassifyResolve(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantState  string
		wantDetail bool
	}{
		{"nil error passes", nil, StatusPass, false},
		{"timeout is held unknown", errors.New(errors.ErrCodeTimeout, "deadline"), StatusUnknown, true},
		{"wrapped timeout is held unknown",
			errors.Wrap(errors.ErrCodeTimeout, "outer", stderrors.New("inner")), StatusUnknown, true},
		{"context deadline is held unknown", context.DeadlineExceeded, StatusUnknown, true},
		{"context canceled is held unknown", context.Canceled, StatusUnknown, true},
		{"internal fails (deterministic defect, not transient)",
			errors.New(errors.ErrCodeInternal, "boom"), StatusFail, true},
		{"not-found fails", errors.New(errors.ErrCodeNotFound, "missing"), StatusFail, true},
		{"invalid-request fails", errors.New(errors.ErrCodeInvalidRequest, "bad"), StatusFail, true},
		{"plain error fails", stderrors.New("plain"), StatusFail, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, detail := classifyResolve(tt.err)
			if state != tt.wantState {
				t.Errorf("state = %q, want %q", state, tt.wantState)
			}
			if (detail != "") != tt.wantDetail {
				t.Errorf("detail = %q, wantDetail %v", detail, tt.wantDetail)
			}
		})
	}
}

func TestIsTransient(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"timeout", errors.New(errors.ErrCodeTimeout, "t"), true},
		{"wrapped timeout", errors.Wrap(errors.ErrCodeTimeout, "o", stderrors.New("x")), true},
		{"context deadline", context.DeadlineExceeded, true},
		{"context canceled", context.Canceled, true},
		{"internal is not transient", errors.New(errors.ErrCodeInternal, "i"), false},
		{"wrapped internal is not transient", errors.Wrap(errors.ErrCodeInternal, "o", stderrors.New("x")), false},
		{"not-found", errors.New(errors.ErrCodeNotFound, "n"), false},
		{"invalid", errors.New(errors.ErrCodeInvalidRequest, "v"), false},
		{"plain", stderrors.New("p"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransient(tt.err); got != tt.want {
				t.Errorf("isTransient = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRollup(t *testing.T) {
	tests := []struct {
		name       string
		dimensions map[string]string
		want       string
	}{
		{"empty is pass", map[string]string{}, StatusPass},
		{"all pass", map[string]string{"a": StatusPass, "b": StatusPass}, StatusPass},
		{"any fail wins", map[string]string{"a": StatusPass, "b": StatusFail, "c": StatusWarn}, StatusFail},
		{"warn over pass", map[string]string{"a": StatusPass, "b": StatusWarn}, StatusWarn},
		{"warn over unknown", map[string]string{"a": StatusWarn, "b": StatusUnknown}, StatusWarn},
		{"fail over unknown", map[string]string{"a": StatusUnknown, "b": StatusFail}, StatusFail},
		{"unknown held surfaces over pass", map[string]string{"a": StatusPass, "b": StatusUnknown}, StatusUnknown},
		{"only unknown", map[string]string{"a": StatusUnknown}, StatusUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rollup(tt.dimensions); got != tt.want {
				t.Errorf("rollup = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClassifyChartPinned(t *testing.T) {
	helm := func(name, version string) recipe.ComponentRef {
		return recipe.ComponentRef{Name: name, Type: recipe.ComponentTypeHelm, Version: version}
	}
	disabledHelm := func(name, version string) recipe.ComponentRef {
		return recipe.ComponentRef{
			Name: name, Type: recipe.ComponentTypeHelm, Version: version,
			Overrides: map[string]any{"enabled": false},
		}
	}
	kustomize := func(name string) recipe.ComponentRef {
		return recipe.ComponentRef{Name: name, Type: recipe.ComponentTypeKustomize}
	}
	// manifestOnly models a manifest-only "Helm" component (e.g.
	// nodewright-customizations): type Helm, ships local manifestFiles, and has
	// no external chart/source/version to pin.
	manifestOnly := func(name string) recipe.ComponentRef {
		return recipe.ComponentRef{
			Name: name, Type: recipe.ComponentTypeHelm,
			ManifestFiles: []string{"components/" + name + "/manifests/tuning.yaml"},
		}
	}
	result := func(refs ...recipe.ComponentRef) *recipe.RecipeResult {
		return &recipe.RecipeResult{ComponentRefs: refs}
	}

	tests := []struct {
		name        string
		result      *recipe.RecipeResult
		wantState   string
		detailMatch string // substring that must appear in detail ("" = detail must be empty)
	}{
		{"nil result passes", nil, StatusPass, ""},
		{"all helm pinned passes", result(helm("a", "1.0.0"), helm("b", "2.1.0")), StatusPass, ""},
		{"unpinned helm fails", result(helm("a", "1.0.0"), helm("b", "")), StatusFail, "b"},
		{
			"multiple unpinned listed sorted",
			result(helm("zebra", ""), helm("alpha", ""), helm("ok", "1.0.0")),
			StatusFail, "alpha, zebra",
		},
		{"pure kustomize is vacuous pass", result(kustomize("k1"), kustomize("k2")), StatusPass, "not applicable"},
		{"no components is vacuous pass", result(), StatusPass, "not applicable"},
		{"mixed pinned helm and kustomize passes", result(helm("a", "1.0.0"), kustomize("k")), StatusPass, ""},
		{
			"disabled unpinned helm is skipped",
			result(helm("a", "1.0.0"), disabledHelm("off", "")),
			StatusPass, "",
		},
		{
			"only disabled unpinned helm is vacuous pass",
			result(disabledHelm("off", "")),
			StatusPass, "not applicable",
		},
		{
			"manifest-only helm alongside pinned chart passes",
			result(helm("a", "1.0.0"), manifestOnly("nodewright-customizations")),
			StatusPass, "",
		},
		{
			"manifest-only helm not flagged as unpinned",
			result(helm("b", ""), manifestOnly("nodewright-customizations")),
			StatusFail, "b",
		},
		{
			"only manifest-only helm is vacuous pass",
			result(manifestOnly("nodewright-customizations")),
			StatusPass, "not applicable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, detail := classifyChartPinned(tt.result)
			if state != tt.wantState {
				t.Errorf("state = %q, want %q", state, tt.wantState)
			}
			if tt.detailMatch == "" {
				if detail != "" {
					t.Errorf("detail = %q, want empty", detail)
				}
			} else if !strings.Contains(detail, tt.detailMatch) {
				t.Errorf("detail = %q, want substring %q", detail, tt.detailMatch)
			}
		})
	}
}

func TestClassifyConstraintsWellformed(t *testing.T) {
	constraint := func(name, value string) recipe.Constraint {
		return recipe.Constraint{Name: name, Value: value}
	}
	result := func(constraints ...recipe.Constraint) *recipe.RecipeResult {
		return &recipe.RecipeResult{Constraints: constraints}
	}
	withWarnings := func(r *recipe.RecipeResult, warnings ...recipe.ConstraintWarning) *recipe.RecipeResult {
		r.Metadata.ConstraintWarnings = warnings
		return r
	}
	withExcluded := func(r *recipe.RecipeResult, excluded ...recipe.ExcludedOverlay) *recipe.RecipeResult {
		r.Metadata.ExcludedOverlays = excluded
		return r
	}

	tests := []struct {
		name        string
		result      *recipe.RecipeResult
		wantState   string
		detailMatch string // substring that must appear in detail ("" = detail must be empty)
	}{
		{"nil result passes", nil, StatusPass, ""},
		{"no constraints passes", result(), StatusPass, ""},
		{
			"all well-formed constraints pass",
			result(constraint("K8s.server.version", ">= 1.32.4"), constraint("OS.release.name", "ubuntu")),
			StatusPass, "",
		},
		{
			"malformed path (too few segments) fails",
			result(constraint("bogus", "1.0")),
			StatusFail, "bogus",
		},
		{
			"malformed path (invalid measurement type) fails",
			result(constraint("NotAType.sub.key", "1.0")),
			StatusFail, "malformed path",
		},
		{
			"malformed value (empty after operator) fails",
			result(constraint("K8s.server.version", ">=")),
			StatusFail, "malformed value",
		},
		{
			"empty value fails",
			result(constraint("K8s.server.version", "")),
			StatusFail, "malformed value",
		},
		{
			// Whitespace-only values are trimmed to empty by the parser, so they
			// take the same fail path as an empty value.
			"whitespace-only value fails",
			result(constraint("K8s.server.version", "   ")),
			StatusFail, "malformed value",
		},
		{
			"parse failure beats resolution warning",
			withWarnings(
				result(constraint("bogus", "1.0")),
				recipe.ConstraintWarning{Overlay: "o", Constraint: "c", Reason: "r"},
			),
			StatusFail, "bogus",
		},
		// The two warn cases below inject Metadata.ConstraintWarnings /
		// ExcludedOverlays directly: with the production satisfiedEvaluator no
		// constraint fails, so these fields are never populated through Compute
		// (see classifyConstraintsWellformed). Direct injection is the only way
		// to exercise the warn branch, by design.
		{
			"well-formed with constraint warning warns",
			withWarnings(
				result(constraint("K8s.server.version", ">= 1.32.4")),
				recipe.ConstraintWarning{Overlay: "gke-overlay", Constraint: "K8s.server.version", Reason: "version too low"},
			),
			StatusWarn, "gke-overlay",
		},
		{
			"well-formed with excluded overlay warns",
			withExcluded(
				result(constraint("K8s.server.version", ">= 1.32.4")),
				recipe.ExcludedOverlay{Name: "dropped-overlay", Reason: recipe.ExcludedOverlayReasonConstraintFailed},
			),
			StatusWarn, "dropped-overlay",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, detail := classifyConstraintsWellformed(tt.result)
			if state != tt.wantState {
				t.Errorf("state = %q, want %q", state, tt.wantState)
			}
			if tt.detailMatch == "" {
				if detail != "" {
					t.Errorf("detail = %q, want empty", detail)
				}
			} else if !strings.Contains(detail, tt.detailMatch) {
				t.Errorf("detail = %q, want substring %q", detail, tt.detailMatch)
			}
		})
	}
}

func TestComputeCoverage(t *testing.T) {
	t.Run("nil result yields zero coverage", func(t *testing.T) {
		if got := computeCoverage(nil); !reflect.DeepEqual(got, DeclaredCoverage{}) {
			t.Errorf("got %+v, want zero value", got)
		}
	})
	t.Run("nil validation yields zero coverage", func(t *testing.T) {
		if got := computeCoverage(&recipe.RecipeResult{}); !reflect.DeepEqual(got, DeclaredCoverage{}) {
			t.Errorf("got %+v, want zero value", got)
		}
	})
	t.Run("populated phases recorded with sorted checks", func(t *testing.T) {
		res := &recipe.RecipeResult{Validation: &recipe.ValidationConfig{
			Deployment: &recipe.ValidationPhase{
				Checks:      []string{"gpu-operator-version", "check-nvidia-smi", "operator-health"},
				Constraints: []recipe.Constraint{{Name: "c1"}, {Name: "c2"}},
			},
			// Readiness/Performance/Conformance intentionally nil (minimal recipe).
		}}
		cov := computeCoverage(res)

		if !cov.Deployment.Declared {
			t.Error("Deployment.Declared = false, want true")
		}
		wantChecks := []string{"check-nvidia-smi", "gpu-operator-version", "operator-health"}
		if !reflect.DeepEqual(cov.Deployment.Checks, wantChecks) {
			t.Errorf("Deployment.Checks = %v, want sorted %v", cov.Deployment.Checks, wantChecks)
		}
		if cov.Deployment.Constraints != 2 {
			t.Errorf("Deployment.Constraints = %d, want 2", cov.Deployment.Constraints)
		}
		// A dropped phase must not be penalized — it is simply not declared.
		if cov.Readiness.Declared || cov.Performance.Declared || cov.Conformance.Declared {
			t.Errorf("undeclared phases marked declared: %+v", cov)
		}
	})
}

// TestDeclaredCoverageCompact pins the byte-exact R/D/P/C format shared by the
// recipe-health matrix (tools/health) and the `aicr recipe list` table
// (pkg/cli). The format must not drift: the committed matrix and the
// determinism/golden tests depend on it.
func TestDeclaredCoverageCompact(t *testing.T) {
	tests := []struct {
		name     string
		cov      DeclaredCoverage
		expected string
	}{
		{
			name:     "zero value",
			cov:      DeclaredCoverage{},
			expected: "R:0 D:0 P:0 C:0",
		},
		{
			name: "all phases populated",
			cov: DeclaredCoverage{
				Readiness:   PhaseCoverage{Checks: []string{"a", "b"}},
				Deployment:  PhaseCoverage{Checks: []string{"a", "b", "c", "d"}},
				Performance: PhaseCoverage{Checks: []string{"a"}},
				Conformance: PhaseCoverage{Checks: make([]string, 10)},
			},
			expected: "R:2 D:4 P:1 C:10",
		},
		{
			name: "counts only Checks, not Declared or Constraints",
			cov: DeclaredCoverage{
				Deployment: PhaseCoverage{Declared: true, Constraints: 5},
			},
			expected: "R:0 D:0 P:0 C:0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cov.Compact(); got != tt.expected {
				t.Errorf("Compact() = %q, want %q", got, tt.expected)
			}
		})
	}
}

// TestComputeEmbeddedCatalog resolves the real embedded catalog: every leaf
// must carry a resolves dimension and a status consistent with the rollup of
// its dimensions, and at least one leaf must resolve cleanly. Cleanly-resolved
// leaves also carry the chart_pinned dimension (a pure read of the result).
func TestComputeEmbeddedCatalog(t *testing.T) {
	report, err := Compute(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}
	if report.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", report.SchemaVersion, SchemaVersion)
	}
	if len(report.Combos) == 0 {
		t.Fatal("expected non-empty embedded catalog")
	}

	sawPass := false
	for _, combo := range report.Combos {
		if combo.LeafOverlay == "" {
			t.Error("combo missing leaf overlay name")
		}
		if combo.Criteria == nil {
			t.Errorf("combo %q missing criteria", combo.LeafOverlay)
		}
		state, ok := combo.Structure.Dimensions[DimResolves]
		if !ok {
			t.Errorf("combo %q missing resolves dimension", combo.LeafOverlay)
		}
		if want := rollup(combo.Structure.Dimensions); combo.Structure.Status != want {
			t.Errorf("combo %q status = %q, want rollup %q", combo.LeafOverlay, combo.Structure.Status, want)
		}
		if state == StatusPass {
			sawPass = true
			// A cleanly-resolved leaf is a pure-readable result, so chart_pinned
			// and constraints_wellformed must both have been scored.
			if _, ok := combo.Structure.Dimensions[DimChartPinned]; !ok {
				t.Errorf("combo %q resolved but missing chart_pinned dimension", combo.LeafOverlay)
			}
			if _, ok := combo.Structure.Dimensions[DimConstraintsWellformed]; !ok {
				t.Errorf("combo %q resolved but missing constraints_wellformed dimension", combo.LeafOverlay)
			}
		}
	}
	if !sawPass {
		t.Error("expected at least one cleanly-resolving leaf in the embedded catalog")
	}
}

// TestComputeDeterminism asserts byte-determinism on the non-error path:
// serializing two Compute runs through the deterministic marshaller yields
// identical bytes. Report carries map-typed fields whose nondeterminism only
// manifests at marshal time, so this compares marshaled output, not structs.
func TestComputeDeterminism(t *testing.T) {
	first, err := Compute(context.Background(), Options{})
	if err != nil {
		t.Fatalf("first Compute() error = %v", err)
	}
	second, err := Compute(context.Background(), Options{})
	if err != nil {
		t.Fatalf("second Compute() error = %v", err)
	}

	firstBytes, err := serializer.MarshalYAMLDeterministic(first)
	if err != nil {
		t.Fatalf("marshal first: %v", err)
	}
	secondBytes, err := serializer.MarshalYAMLDeterministic(second)
	if err != nil {
		t.Fatalf("marshal second: %v", err)
	}
	if !bytes.Equal(firstBytes, secondBytes) {
		t.Errorf("non-deterministic report:\n--- first ---\n%s\n--- second ---\n%s", firstBytes, secondBytes)
	}
}

// TestComputeEmptyCatalog uses a filter that matches no overlay: the report
// must be empty (no combos) without panicking.
func TestComputeEmptyCatalog(t *testing.T) {
	report, err := Compute(context.Background(), Options{
		Filter: &recipe.Criteria{Service: recipe.CriteriaServiceType("nonexistent-service")},
	})
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}
	if report.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", report.SchemaVersion, SchemaVersion)
	}
	if len(report.Combos) != 0 {
		t.Errorf("expected empty report, got %d combos", len(report.Combos))
	}
}

// TestComputeGradesDeterministicDefectAsFail drives a non-pass grade through
// the real builder: a leaf whose registry component declares a
// healthCheck.assertFile pointing at a missing path. The embedded read returns
// a bare fs.ErrNotExist, flattened to ErrCodeInternal in finalizeRecipeResult —
// a deterministic defect that must grade fail, not be held as unknown. This
// guards the narrowed isTransient bucket end-to-end.
func TestComputeGradesDeterministicDefectAsFail(t *testing.T) {
	provider := newInMemoryProvider(map[string][]byte{
		"overlays/base.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: base
spec:
  componentRefs: []
`),
		"overlays/broken-leaf.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: broken-leaf
spec:
  criteria:
    service: eks
  componentRefs:
    - name: broken-comp
`),
		"registry.yaml": []byte(`apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: broken-comp
    displayName: Broken Component
    helm:
      defaultRepository: https://charts.example.com
      defaultChart: example/broken-comp
    healthCheck:
      assertFile: components/broken-comp/missing-assert.yaml
`),
	})

	report, err := Compute(context.Background(), Options{Provider: provider})
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}

	var broken *ComboHealth
	for i := range report.Combos {
		if report.Combos[i].LeafOverlay == "broken-leaf" {
			broken = &report.Combos[i]
		}
	}
	if broken == nil {
		t.Fatalf("broken-leaf was not enumerated; combos = %+v", report.Combos)
	}
	if got := broken.Structure.Dimensions[DimResolves]; got != StatusFail {
		t.Errorf("resolves = %q, want %q — a deterministic defect must fail, not be held unknown", got, StatusFail)
	}
	if broken.Structure.Status != StatusFail {
		t.Errorf("status = %q, want %q", broken.Structure.Status, StatusFail)
	}
	if d := broken.Structure.Detail[DimResolves]; !strings.Contains(d, "assertFile") {
		t.Errorf("detail = %q, want it to surface the assertFile read failure", d)
	}
	// No RecipeResult to read, so the descriptor must be omitted (nil), not an
	// all-zero block a consumer could misread as "declares no checks".
	if broken.Structure.Coverage != nil {
		t.Errorf("Coverage = %+v, want nil on a failed resolve", *broken.Structure.Coverage)
	}
}

// TestComputeChartPinnedFailThroughBuilder drives a chart_pinned fail through
// the real builder: a leaf with a Helm component whose registry entry declares
// no default chart version, so the resolved ComponentRef carries an empty
// Version. The recipe resolves cleanly (resolves=pass) but chart_pinned must
// fail, dragging the rolled-up status to fail.
func TestComputeChartPinnedFailThroughBuilder(t *testing.T) {
	provider := newInMemoryProvider(map[string][]byte{
		"overlays/base.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: base
spec:
  componentRefs: []
`),
		"overlays/unpinned-leaf.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: unpinned-leaf
spec:
  criteria:
    service: eks
  componentRefs:
    - name: unpinned-helm
`),
		"registry.yaml": []byte(`apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: unpinned-helm
    displayName: Unpinned Helm Component
    helm:
      defaultRepository: https://charts.example.com
      defaultChart: example/unpinned-helm
`),
	})

	report, err := Compute(context.Background(), Options{Provider: provider})
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}

	var combo *ComboHealth
	for i := range report.Combos {
		if report.Combos[i].LeafOverlay == "unpinned-leaf" {
			combo = &report.Combos[i]
		}
	}
	if combo == nil {
		t.Fatalf("unpinned-leaf was not enumerated; combos = %+v", report.Combos)
	}
	if got := combo.Structure.Dimensions[DimResolves]; got != StatusPass {
		t.Errorf("resolves = %q, want %q (recipe should resolve cleanly)", got, StatusPass)
	}
	if got := combo.Structure.Dimensions[DimChartPinned]; got != StatusFail {
		t.Errorf("chart_pinned = %q, want %q (unpinned Helm chart)", got, StatusFail)
	}
	if combo.Structure.Status != StatusFail {
		t.Errorf("status = %q, want %q", combo.Structure.Status, StatusFail)
	}
	if d := combo.Structure.Detail[DimChartPinned]; !strings.Contains(d, "unpinned-helm") {
		t.Errorf("detail = %q, want it to name the unpinned component", d)
	}
	// The recipe resolved, so the descriptor must be populated (non-nil).
	if combo.Structure.Coverage == nil {
		t.Error("Coverage = nil, want non-nil on a resolved recipe")
	}
}

// TestComputeConstraintsWellformedFailThroughBuilder drives a
// constraints_wellformed fail through the real builder: a leaf carrying a
// malformed constraint (a path with too few segments). The recipe resolves
// cleanly (resolves=pass) but the snapshot-free parser rejects the constraint,
// so constraints_wellformed must fail and drag the rolled-up status to fail —
// a malformed constraint is never a silent pass. Hermetic: no snapshot.
func TestComputeConstraintsWellformedFailThroughBuilder(t *testing.T) {
	provider := newInMemoryProvider(map[string][]byte{
		"overlays/base.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: base
spec:
  componentRefs: []
`),
		"overlays/malformed-constraint-leaf.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: malformed-constraint-leaf
spec:
  criteria:
    service: eks
  componentRefs: []
  constraints:
    - name: not-a-valid-path
      value: ">= 1.0"
`),
		"registry.yaml": []byte(`apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components: []
`),
	})

	report, err := Compute(context.Background(), Options{Provider: provider})
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}

	var combo *ComboHealth
	for i := range report.Combos {
		if report.Combos[i].LeafOverlay == "malformed-constraint-leaf" {
			combo = &report.Combos[i]
		}
	}
	if combo == nil {
		t.Fatalf("malformed-constraint-leaf was not enumerated; combos = %+v", report.Combos)
	}
	if got := combo.Structure.Dimensions[DimResolves]; got != StatusPass {
		t.Errorf("resolves = %q, want %q (recipe should resolve cleanly)", got, StatusPass)
	}
	if got := combo.Structure.Dimensions[DimConstraintsWellformed]; got != StatusFail {
		t.Errorf("constraints_wellformed = %q, want %q (malformed constraint path)", got, StatusFail)
	}
	if combo.Structure.Status != StatusFail {
		t.Errorf("status = %q, want %q", combo.Structure.Status, StatusFail)
	}
	if d := combo.Structure.Detail[DimConstraintsWellformed]; !strings.Contains(d, "not-a-valid-path") {
		t.Errorf("detail = %q, want it to name the malformed constraint", d)
	}
}

// TestComputeAllDimensionsCoexistAndRollUpClean proves the end-to-end
// four-signal contract on one resolved leaf: the three graded dimensions
// (resolves, chart_pinned, constraints_wellformed) all pass, the
// declared_coverage descriptor is populated (not graded), and the rollup is
// pass. A pinned Helm component, a well-formed constraint, and a declared
// deployment phase are all present.
func TestComputeAllDimensionsCoexistAndRollUpClean(t *testing.T) {
	provider := newInMemoryProvider(map[string][]byte{
		"overlays/base.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: base
spec:
  componentRefs: []
`),
		"overlays/clean-leaf.yaml": []byte(`kind: RecipeMetadata
apiVersion: aicr.nvidia.com/v1alpha1
metadata:
  name: clean-leaf
spec:
  criteria:
    service: eks
  componentRefs:
    - name: pinned-helm
  constraints:
    - name: K8s.server.version
      value: ">= 1.32.4"
  validation:
    deployment:
      checks:
        - operator-health
`),
		"registry.yaml": []byte(`apiVersion: aicr.nvidia.com/v1alpha1
kind: ComponentRegistry
components:
  - name: pinned-helm
    displayName: Pinned Helm Component
    helm:
      defaultRepository: https://charts.example.com
      defaultChart: example/pinned-helm
      defaultVersion: 1.2.3
`),
	})

	report, err := Compute(context.Background(), Options{Provider: provider})
	if err != nil {
		t.Fatalf("Compute() error = %v", err)
	}

	var combo *ComboHealth
	for i := range report.Combos {
		if report.Combos[i].LeafOverlay == "clean-leaf" {
			combo = &report.Combos[i]
		}
	}
	if combo == nil {
		t.Fatalf("clean-leaf was not enumerated; combos = %+v", report.Combos)
	}

	for _, dim := range []string{DimResolves, DimChartPinned, DimConstraintsWellformed} {
		if got := combo.Structure.Dimensions[dim]; got != StatusPass {
			t.Errorf("dimension %q = %q, want %q", dim, got, StatusPass)
		}
	}
	if combo.Structure.Status != StatusPass {
		t.Errorf("status = %q, want %q (all graded dimensions pass)", combo.Structure.Status, StatusPass)
	}
	// declared_coverage is a descriptor, not graded: it must be populated but
	// must not move the rolled-up status.
	if combo.Structure.Coverage == nil {
		t.Fatal("Coverage = nil, want populated descriptor on a resolved leaf")
	}
	if !combo.Structure.Coverage.Deployment.Declared {
		t.Error("Coverage.Deployment.Declared = false, want true (deployment phase declared)")
	}
	if want := []string{"operator-health"}; !reflect.DeepEqual(combo.Structure.Coverage.Deployment.Checks, want) {
		t.Errorf("Coverage.Deployment.Checks = %v, want %v", combo.Structure.Coverage.Deployment.Checks, want)
	}
}

// TestComputeCatalogLoadError verifies Compute surfaces a catalog-load failure
// from a fake provider rather than panicking or returning a partial report.
func TestComputeCatalogLoadError(t *testing.T) {
	report, err := Compute(context.Background(), Options{Provider: &failingProvider{}})
	if err == nil {
		t.Fatal("expected error from failing provider")
	}
	if report != nil {
		t.Errorf("expected nil report on load error, got %+v", report)
	}
}

// failingProvider is a recipe.DataProvider whose catalog walk always fails,
// simulating an unreadable data source.
type failingProvider struct{}

func (p *failingProvider) ReadFile(_ context.Context, _ string) ([]byte, error) {
	return nil, stderrors.New("read failed")
}

func (p *failingProvider) WalkDir(_ context.Context, _ string, _ fs.WalkDirFunc) error {
	return stderrors.New("walk failed")
}

func (p *failingProvider) Source(_ string) string { return "failing-provider" }

// inMemoryProvider is a recipe.DataProvider backed by an in-memory file map,
// used to construct a minimal isolated catalog without touching the embedded
// FS. A read for a path not in the map returns fs.ErrNotExist, mirroring the
// embedded provider.
type inMemoryProvider struct {
	files map[string][]byte
}

func newInMemoryProvider(files map[string][]byte) *inMemoryProvider {
	return &inMemoryProvider{files: files}
}

func (p *inMemoryProvider) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	content, ok := p.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return content, nil
}

func (p *inMemoryProvider) WalkDir(ctx context.Context, _ string, fn fs.WalkDirFunc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for path := range p.files {
		if err := fn(path, inMemoryDirEntry{name: path}, nil); err != nil {
			return err
		}
	}
	return nil
}

func (p *inMemoryProvider) Source(path string) string { return "in-memory:" + path }

// inMemoryDirEntry is a minimal fs.DirEntry for in-memory files (all files).
type inMemoryDirEntry struct{ name string }

func (e inMemoryDirEntry) Name() string               { return e.name }
func (e inMemoryDirEntry) IsDir() bool                { return false }
func (e inMemoryDirEntry) Type() fs.FileMode          { return 0 }
func (e inMemoryDirEntry) Info() (fs.FileInfo, error) { return nil, fs.ErrNotExist }
