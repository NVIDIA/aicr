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

// TestInjectDRAChartVersionAnnotation_PositiveCase pins the happy
// path: both gpu-operator and nvidia-dra-driver-gpu enabled in the
// filtered recipe, gpu-operator version present → the
// aicr.nvidia.com/gpu-operator-chart-version annotation is written on
// both the controller and kubeletPlugin pod templates with the
// resolved chart version.
func TestInjectDRAChartVersionAnnotation_PositiveCase(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	componentValues := map[string]map[string]any{
		gpuOperatorComponentName: {},
		draComponentName:         {},
	}
	rr := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{Name: gpuOperatorComponentName, Version: "v26.4.0"},
			{Name: draComponentName, Version: "25.12.0"},
		},
	}

	b.injectDRAChartVersionAnnotation(componentValues, rr)

	for _, podPath := range []string{"controller", "kubeletPlugin"} {
		got := dig(componentValues[draComponentName], podPath, "podAnnotations", draChartVersionAnnotation)
		if got != "v26.4.0" {
			t.Errorf("podAnnotations[%s][%s] = %v, want v26.4.0",
				podPath, draChartVersionAnnotation, got)
		}
	}
}

// TestInjectDRAChartVersionAnnotation_DRAComponentDisabled pins the
// gating: when nvidia-dra-driver-gpu is absent from the filtered
// ComponentRefs (already filtered out as disabled by the caller), the
// helper is a no-op even if gpu-operator is enabled. This is the
// "negative case" the issue's acceptance criteria call out — proves
// the gating is correct and no warning fires.
func TestInjectDRAChartVersionAnnotation_DRAComponentDisabled(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	componentValues := map[string]map[string]any{
		gpuOperatorComponentName: {},
	}
	rr := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{Name: gpuOperatorComponentName, Version: "v26.4.0"},
		},
	}

	b.injectDRAChartVersionAnnotation(componentValues, rr)

	if _, ok := componentValues[draComponentName]; ok {
		t.Errorf("expected no DRA entry in componentValues when DRA component is disabled, got %v",
			componentValues[draComponentName])
	}
}

// TestInjectDRAChartVersionAnnotation_GPUOperatorDisabled pins the
// mirror gating: when gpu-operator is absent (disabled or filtered
// out), no annotation is written because there's no chart version to
// mirror.
func TestInjectDRAChartVersionAnnotation_GPUOperatorDisabled(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	componentValues := map[string]map[string]any{
		draComponentName: {},
	}
	rr := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{Name: draComponentName, Version: "25.12.0"},
		},
	}

	b.injectDRAChartVersionAnnotation(componentValues, rr)

	if got := componentValues[draComponentName]; len(got) != 0 {
		t.Errorf("expected unchanged DRA values when gpu-operator is disabled, got %v", got)
	}
}

// TestInjectDRAChartVersionAnnotation_GPUOperatorEmptyVersion pins the
// defensive branch: when gpu-operator IS enabled but its Version field
// is empty (the resolver shouldn't produce this in practice — see the
// helper docstring for why), the helper logs a warning and skips
// injection rather than writing an empty annotation value that would
// itself lock the DaemonSet to a meaningless pin and erase the
// "annotation never drifts from the chart pin" guarantee.
func TestInjectDRAChartVersionAnnotation_GPUOperatorEmptyVersion(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	componentValues := map[string]map[string]any{
		gpuOperatorComponentName: {},
		draComponentName:         {},
	}
	rr := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{Name: gpuOperatorComponentName, Version: ""},
			{Name: draComponentName, Version: "25.12.0"},
		},
	}

	b.injectDRAChartVersionAnnotation(componentValues, rr)

	if got := componentValues[draComponentName]; len(got) != 0 {
		t.Errorf("expected unchanged DRA values when gpu-operator Version is empty, got %v", got)
	}
}

// TestInjectDRAChartVersionAnnotation_PreservesExistingValues
// documents the additive contract: priorityClassName and any
// pre-existing podAnnotations from the values file (or from earlier
// override layers) survive injection. The helper only writes the one
// annotation key and leaves the rest of the controller / kubeletPlugin
// sections untouched.
func TestInjectDRAChartVersionAnnotation_PreservesExistingValues(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	componentValues := map[string]map[string]any{
		gpuOperatorComponentName: {},
		draComponentName: {
			"controller": map[string]any{
				"priorityClassName": "system-cluster-critical",
				"podAnnotations": map[string]any{
					"operator.example.com/other-key": "preserved",
				},
			},
			"kubeletPlugin": map[string]any{
				"priorityClassName": "system-node-critical",
			},
		},
	}
	rr := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{Name: gpuOperatorComponentName, Version: "v26.4.0"},
			{Name: draComponentName, Version: "25.12.0"},
		},
	}

	b.injectDRAChartVersionAnnotation(componentValues, rr)

	dra := componentValues[draComponentName]
	if got := dig(dra, "controller", "priorityClassName"); got != "system-cluster-critical" {
		t.Errorf("controller.priorityClassName = %v, want system-cluster-critical", got)
	}
	if got := dig(dra, "controller", "podAnnotations", "operator.example.com/other-key"); got != "preserved" {
		t.Errorf("controller.podAnnotations[operator.example.com/other-key] = %v, want preserved", got)
	}
	if got := dig(dra, "controller", "podAnnotations", draChartVersionAnnotation); got != "v26.4.0" {
		t.Errorf("controller.podAnnotations[%s] = %v, want v26.4.0", draChartVersionAnnotation, got)
	}
	if got := dig(dra, "kubeletPlugin", "priorityClassName"); got != "system-node-critical" {
		t.Errorf("kubeletPlugin.priorityClassName = %v, want system-node-critical", got)
	}
	if got := dig(dra, "kubeletPlugin", "podAnnotations", draChartVersionAnnotation); got != "v26.4.0" {
		t.Errorf("kubeletPlugin.podAnnotations[%s] = %v, want v26.4.0", draChartVersionAnnotation, got)
	}
}

// TestInjectDRAChartVersionAnnotation_OverridesUserSet pins the
// "internal annotation always reflects the actual chart version"
// invariant. A user --set that wrote a stale value into the
// annotation must be overwritten by the bundler-derived value;
// otherwise the rollout-trigger semantics break and the durable fix
// degrades into the same drift the manual annotation had. Injection
// happens AFTER extractComponentValues for exactly this reason.
func TestInjectDRAChartVersionAnnotation_OverridesUserSet(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	componentValues := map[string]map[string]any{
		gpuOperatorComponentName: {},
		draComponentName: {
			"controller": map[string]any{
				"podAnnotations": map[string]any{
					draChartVersionAnnotation: "v25.10.1-stale-from-user-set",
				},
			},
		},
	}
	rr := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{Name: gpuOperatorComponentName, Version: "v26.4.0"},
			{Name: draComponentName, Version: "25.12.0"},
		},
	}

	b.injectDRAChartVersionAnnotation(componentValues, rr)

	got := dig(componentValues[draComponentName], "controller", "podAnnotations", draChartVersionAnnotation)
	if got != "v26.4.0" {
		t.Errorf("expected user --set value to be overridden by injection: got %v, want v26.4.0", got)
	}
}

// TestInjectDRAChartVersionAnnotation_VersionVariants documents that
// the helper mirrors whatever string the resolver wrote into the
// gpu-operator ComponentRef. Helm chart pins ship with and without a
// leading "v"; the annotation tracks the exact string so the rendered
// pod-template diff reliably fires on a chart-pin change of any
// shape.
func TestInjectDRAChartVersionAnnotation_VersionVariants(t *testing.T) {
	tests := []struct {
		name    string
		version string
	}{
		{"v-prefixed", "v26.4.0"},
		{"unprefixed", "26.4.0"},
		{"pre-release", "v26.5.0-beta.1"},
		{"build metadata", "v26.4.0+nvidia.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, err := New()
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			componentValues := map[string]map[string]any{
				gpuOperatorComponentName: {},
				draComponentName:         {},
			}
			rr := &recipe.RecipeResult{
				ComponentRefs: []recipe.ComponentRef{
					{Name: gpuOperatorComponentName, Version: tt.version},
					{Name: draComponentName, Version: "25.12.0"},
				},
			}
			b.injectDRAChartVersionAnnotation(componentValues, rr)
			got := dig(componentValues[draComponentName], "controller", "podAnnotations", draChartVersionAnnotation)
			if got != tt.version {
				t.Errorf("controller annotation = %v, want %v", got, tt.version)
			}
		})
	}
}

// TestInjectDRAChartVersionAnnotation_NilInputs documents the
// nil-tolerant contract. The bundler's Make method only calls this
// after extractComponentValues returns a non-nil map and pretty much
// never with a nil recipe, but defensive nil-handling keeps the
// helper safe in unit tests and future callers.
func TestInjectDRAChartVersionAnnotation_NilInputs(t *testing.T) {
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
			b.injectDRAChartVersionAnnotation(tt.cv, tt.rr)
		})
	}
}

// TestInjectDRAChartVersionAnnotation_LazyDRAValuesMap pins one
// implementation detail worth fixing in place: when the DRA component
// is enabled but its componentValues entry hasn't been initialized
// yet (no values file, no overrides), the helper still injects by
// creating the nested map. This matters because every-deployer parity
// requires the annotation to appear in the rendered chart even when
// the DRA chart has no other values configured.
func TestInjectDRAChartVersionAnnotation_LazyDRAValuesMap(t *testing.T) {
	b, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	componentValues := map[string]map[string]any{
		gpuOperatorComponentName: {},
		// nvidia-dra-driver-gpu key intentionally absent.
	}
	rr := &recipe.RecipeResult{
		ComponentRefs: []recipe.ComponentRef{
			{Name: gpuOperatorComponentName, Version: "v26.4.0"},
			{Name: draComponentName, Version: "25.12.0"},
		},
	}

	b.injectDRAChartVersionAnnotation(componentValues, rr)

	if _, ok := componentValues[draComponentName]; !ok {
		t.Fatalf("expected helper to create DRA values entry on demand")
	}
	got := dig(componentValues[draComponentName], "controller", "podAnnotations", draChartVersionAnnotation)
	if got != "v26.4.0" {
		t.Errorf("controller annotation = %v, want v26.4.0", got)
	}
}

// dig walks a nested map[string]any tree by string keys and returns
// the leaf value, or nil if any intermediate key is missing or has
// the wrong type. Pulls the noisy type-assertion chain out of every
// table-test row so the assertions read like the data they describe.
func dig(m map[string]any, keys ...string) any {
	cur := any(m)
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}
