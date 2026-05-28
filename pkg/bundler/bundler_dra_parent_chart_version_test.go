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

package bundler

import (
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestInjectDRAParentChartVersionValue_PositiveCase pins the happy
// path: gpu-operator + nvidia-dra-driver-gpu both enabled, gpu-operator
// version is normalized (leading 'v' stripped) and written under
// gpu-operator componentValues at the documented key, so the
// synthesized gpu-operator-post chart's manifest template can read it
// at Helm install time as
// `{{ index .Values "gpu-operator" "_aicrParentChartVersion" }}`.
func TestInjectDRAParentChartVersionValue_PositiveCase(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	componentValues := map[string]map[string]any{
		"gpu-operator":          {},
		"nvidia-dra-driver-gpu": {},
	}
	rr := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Version: "v26.4.0"},
			{Name: "nvidia-dra-driver-gpu", Version: "25.12.0"},
		},
	}

	b.injectDRAParentChartVersionValue(componentValues, rr)

	got, ok := componentValues["gpu-operator"][draParentChartVersionValueKey].(string)
	if !ok {
		t.Fatalf("expected string value at gpu-operator[%s], got %T (%v)",
			draParentChartVersionValueKey, componentValues["gpu-operator"][draParentChartVersionValueKey],
			componentValues["gpu-operator"])
	}
	// NormalizeVersionWithDefault strips the leading 'v'.
	if got != "26.4.0" {
		t.Errorf("gpu-operator[%s] = %q, want %q (normalized, no leading v)",
			draParentChartVersionValueKey, got, "26.4.0")
	}
}

// TestInjectDRAParentChartVersionValue_DRAComponentDisabled documents
// the looser gating from the second Codex review on PR #1030: the
// rollout-hook manifest is wired into gpu-operator's manifestFiles in
// base.yaml and ships whenever gpu-operator is enabled, regardless of
// whether nvidia-dra-driver-gpu is also enabled. If the injection
// gated on DRA-enabled, a recipe with --set
// nvidia-dra-driver-gpu:enabled=false would emit the hook with
// `<nil>` placeholders for the parent version and produce an invalid
// DNS-1123 Job name. Inject unconditionally when gpu-operator is
// present; the hook script's runtime "no DRA DaemonSet → exit 0"
// branch keeps the Job a no-op when DRA is in fact absent.
func TestInjectDRAParentChartVersionValue_DRAComponentDisabled(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	componentValues := map[string]map[string]any{
		"gpu-operator": {},
	}
	rr := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Version: "v26.4.0"},
		},
	}

	b.injectDRAParentChartVersionValue(componentValues, rr)

	got, ok := componentValues["gpu-operator"][draParentChartVersionValueKey].(string)
	if !ok {
		t.Fatalf("expected the parent version to be injected even when DRA is disabled, got %T (%v)",
			componentValues["gpu-operator"][draParentChartVersionValueKey],
			componentValues["gpu-operator"])
	}
	if got != "26.4.0" {
		t.Errorf("gpu-operator[%s] = %q, want %q",
			draParentChartVersionValueKey, got, "26.4.0")
	}
}

// TestInjectDRAParentChartVersionValue_DriverVersionKey pins the
// driver-version surfacing that closes the values-only re-fire gap
// from PR #1030 cross-review (Codex finding #2). On non-hook
// deployers (Helmfile / Argo CD / argocd-helm where stripHelmHooks
// removes the upgrade-hook annotations), the Job ships as a regular
// chart resource — `kubectl apply` on the same name is a no-op, so
// the Job name must include driver.version for a values-only
// override (--set gpuoperator:driver.version=…) to trigger a fresh
// resource. The bundler reads driver.version from the merged
// componentValues map (registry defaults + recipe overlay + user
// --set, all applied by extractComponentValues), so this works for
// every driver.version source.
func TestInjectDRAParentChartVersionValue_DriverVersionKey(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	componentValues := map[string]map[string]any{
		"gpu-operator": {
			"driver": map[string]any{
				"version": "570.86.16",
			},
		},
		"nvidia-dra-driver-gpu": {},
	}
	rr := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Version: "v26.4.0"},
			{Name: "nvidia-dra-driver-gpu", Version: "25.12.0"},
		},
	}

	b.injectDRAParentChartVersionValue(componentValues, rr)

	gotDriver, ok := componentValues["gpu-operator"][draDriverVersionValueKey].(string)
	if !ok {
		t.Fatalf("expected string value at gpu-operator[%s], got %T",
			draDriverVersionValueKey, componentValues["gpu-operator"][draDriverVersionValueKey])
	}
	if gotDriver != "570.86.16" {
		t.Errorf("gpu-operator[%s] = %q, want %q",
			draDriverVersionValueKey, gotDriver, "570.86.16")
	}
}

// TestInjectDRAParentChartVersionValue_DriverVersionAbsent covers the
// host-managed-driver case (GKE COS, OKE, Kind, Talos overlays):
// driver.enabled=false and driver.version is unset. The Job-name
// slug template falls back to the chart-version-only shape, so the
// bundler emits `_aicrDriverVersion` as the empty string explicitly
// — leaving it nil would render `<nil>` through `toString`.
func TestInjectDRAParentChartVersionValue_DriverVersionAbsent(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	componentValues := map[string]map[string]any{
		"gpu-operator": {},
	}
	rr := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Version: "v26.4.0"},
		},
	}

	b.injectDRAParentChartVersionValue(componentValues, rr)

	got, ok := componentValues["gpu-operator"][draDriverVersionValueKey]
	if !ok {
		t.Fatalf("expected %s key to be present (even if empty); not found", draDriverVersionValueKey)
	}
	if got != "" {
		t.Errorf("gpu-operator[%s] = %q, want empty string", draDriverVersionValueKey, got)
	}
}

// TestInjectDRAParentChartVersionValue_EmptyVersionFallback covers
// the synthetic-recipe edge case from PR #1030 cross-review
// (CodeRabbit finding #6): if a recipe / overlay clears the
// gpu-operator componentRef Version to "", the manifest still ships
// (it's wired unconditionally into base.yaml), so the bundler must
// inject a non-empty value or the rendered Job name becomes
// `aicr-dra-rollout-hook-<nil>-r1` — invalid DNS-1123. The fallback
// path uses NormalizeVersionWithDefault's "0.1.0" default so the
// bundle remains apply-able; a slog.Warn surfaces the underlying
// recipe gap.
func TestInjectDRAParentChartVersionValue_EmptyVersionFallback(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	componentValues := map[string]map[string]any{
		"gpu-operator": {},
	}
	rr := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Version: ""},
		},
	}

	b.injectDRAParentChartVersionValue(componentValues, rr)

	got, ok := componentValues["gpu-operator"][draParentChartVersionValueKey].(string)
	if !ok {
		t.Fatalf("expected fallback string value at gpu-operator[%s], got %T (%v); empty Version must not skip injection",
			draParentChartVersionValueKey, componentValues["gpu-operator"][draParentChartVersionValueKey],
			componentValues["gpu-operator"])
	}
	// NormalizeVersionWithDefault returns "0.1.0" for empty input.
	if got != "0.1.0" {
		t.Errorf("gpu-operator[%s] = %q, want %q (default fallback)",
			draParentChartVersionValueKey, got, "0.1.0")
	}
}

// TestInjectDRAParentChartVersionValue_GPUOperatorDisabled is the
// mirror gating case: with no gpu-operator componentRef the helper
// has no version to mirror, so it leaves componentValues untouched.
func TestInjectDRAParentChartVersionValue_GPUOperatorDisabled(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	componentValues := map[string]map[string]any{
		"nvidia-dra-driver-gpu": {},
	}
	rr := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{Name: "nvidia-dra-driver-gpu", Version: "25.12.0"},
		},
	}

	b.injectDRAParentChartVersionValue(componentValues, rr)

	if v, ok := componentValues["gpu-operator"]; ok {
		t.Errorf("expected no gpu-operator entry when gpu-operator is disabled, got %v", v)
	}
}

// TestInjectDRAParentChartVersionValue_OverridesUserSet documents the
// "internal key always reflects the resolved version" invariant. A
// user --set that wrote a stale value into the synthetic key must be
// overwritten by the bundler-derived value; otherwise the
// rollout-trigger semantics break exactly the same way the manual
// annotation in #973 did, and we lose the durability guarantee.
// Injection runs AFTER extractComponentValues specifically for this.
func TestInjectDRAParentChartVersionValue_OverridesUserSet(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	componentValues := map[string]map[string]any{
		"gpu-operator": {
			draParentChartVersionValueKey: "v25.10.1-stale-from-user-set",
		},
		"nvidia-dra-driver-gpu": {},
	}
	rr := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{Name: "gpu-operator", Version: "v26.4.0"},
			{Name: "nvidia-dra-driver-gpu", Version: "25.12.0"},
		},
	}

	b.injectDRAParentChartVersionValue(componentValues, rr)

	got := componentValues["gpu-operator"][draParentChartVersionValueKey]
	if got != "26.4.0" {
		t.Errorf("user --set value should be overridden by the injected resolved version: got %v, want \"26.4.0\"", got)
	}
}

// TestInjectDRAParentChartVersionValue_NilInputs documents the
// nil-tolerant contract. Make calls this after extractComponentValues
// returns a non-nil map and almost never with a nil recipe, but
// defensive nil-handling makes the helper safe in unit tests and
// future callers.
func TestInjectDRAParentChartVersionValue_NilInputs(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	tests := []struct {
		name string
		cv   map[string]map[string]any
		rr   *recipe.RecipeResult
	}{
		{"nil componentValues", nil, &recipe.RecipeResult{}},
		{"nil recipe result", map[string]map[string]any{}, nil},
		{"both nil", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Should not panic.
			b.injectDRAParentChartVersionValue(tt.cv, tt.rr)
		})
	}
}
