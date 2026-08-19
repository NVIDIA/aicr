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

package validations

import (
	stderrors "errors"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestCheckNVSentinelRuntimeClassCoherence covers the runtime-class
// value comparison on synthetic recipes: the gate fires exactly when
// metadata-collector's resolved runtimeClassName differs from the GPU
// Operator's resolved operator.runtimeClass, either side unset treated
// as the shared chart default "nvidia".
func TestCheckNVSentinelRuntimeClassCoherence(t *testing.T) {
	t.Parallel()

	sentinelRef := func(overrides map[string]any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: nvsentinelComponent, Overrides: overrides}
	}
	gpuOpRef := func(name string, overrides map[string]any) recipe.ComponentRef {
		return recipe.ComponentRef{Name: name, Overrides: overrides}
	}
	operatorClass := func(class any) map[string]any {
		return map[string]any{"operator": map[string]any{"runtimeClass": class}}
	}
	collectorClass := func(class any) map[string]any {
		return map[string]any{"metadata-collector": map[string]any{"runtimeClassName": class}}
	}
	result := func(refs ...recipe.ComponentRef) *recipe.RecipeResult {
		return &recipe.RecipeResult{
			ComponentRefs: refs,
			Criteria:      &recipe.Criteria{Service: recipe.CriteriaServiceAKS},
		}
	}

	tests := []struct {
		name          string
		recipeResult  *recipe.RecipeResult
		bundlerConfig *config.Config
		conditions    map[string][]string
		wantBlocked   bool
		wantErrs      int
		wantContains  []string
	}{
		{
			name:         "nil recipe result → skipped",
			recipeResult: nil,
		},
		{
			name:         "no nvsentinel ref → skipped",
			recipeResult: result(gpuOpRef("gpu-operator", operatorClass("nvidia-container-runtime"))),
		},
		{
			name: "nvsentinel disabled → skipped",
			recipeResult: result(
				sentinelRef(map[string]any{"enabled": false}),
				gpuOpRef("gpu-operator", operatorClass("nvidia-container-runtime"))),
		},
		{
			name:         "no gpu-operator ref → skipped (nothing manages RuntimeClasses)",
			recipeResult: result(sentinelRef(nil)),
		},
		{
			name: "gpu-operator disabled → skipped",
			recipeResult: result(sentinelRef(nil),
				gpuOpRef("gpu-operator", map[string]any{
					"enabled":  false,
					"operator": map[string]any{"runtimeClass": "nvidia-container-runtime"},
				})),
		},
		{
			name:         "conditions mismatch → skipped",
			recipeResult: result(sentinelRef(nil), gpuOpRef("gpu-operator", operatorClass("nvidia-container-runtime"))),
			conditions:   map[string][]string{"service": {"eks"}},
		},
		{
			name:         "both sides unset → shared chart default nvidia → passes",
			recipeResult: result(sentinelRef(nil), gpuOpRef("gpu-operator", map[string]any{})),
		},
		{
			name: "operator-managed shape: operator.runtimeClass=nvidia, collector unset → passes",
			recipeResult: result(sentinelRef(nil),
				gpuOpRef("gpu-operator", operatorClass("nvidia"))),
		},
		{
			name: "azure-managed shape: operator nvidia-container-runtime, collector default nvidia → blocked",
			recipeResult: result(sentinelRef(nil),
				gpuOpRef("gpu-operator", operatorClass("nvidia-container-runtime"))),
			wantBlocked: true,
			wantContains: []string{
				`RuntimeClass "nvidia" not found`,
				"FailedCreate",
				"--set nv-sentinel:metadata-collector.runtimeClassName=nvidia-container-runtime",
			},
		},
		{
			name: "mismatch with collector explicitly set → blocked, remedy names the operator class",
			recipeResult: result(sentinelRef(collectorClass("nvidia")),
				gpuOpRef("gpu-operator", operatorClass("custom-runtime"))),
			wantBlocked: true,
			wantContains: []string{
				"--set nv-sentinel:metadata-collector.runtimeClassName=custom-runtime",
			},
		},
		{
			name: "collector aligned via recipe values → passes",
			recipeResult: result(sentinelRef(collectorClass("nvidia-container-runtime")),
				gpuOpRef("gpu-operator", operatorClass("nvidia-container-runtime"))),
		},
		{
			name: "collector aligned via the documented --set remedy → passes",
			recipeResult: result(sentinelRef(nil),
				gpuOpRef("gpu-operator", operatorClass("nvidia-container-runtime"))),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"nv-sentinel": {"metadata-collector.runtimeClassName": "nvidia-container-runtime"},
			})),
		},
		{
			name: "collector explicitly empty → field omitted from pod spec → passes",
			recipeResult: result(sentinelRef(collectorClass("")),
				gpuOpRef("gpu-operator", operatorClass("nvidia-container-runtime"))),
		},
		{
			name: "operator class explicitly empty → chart default nvidia → collector default matches → passes",
			recipeResult: result(sentinelRef(nil),
				gpuOpRef("gpu-operator", operatorClass(""))),
		},
		{
			name: "metadata-collector subchart disabled → no DaemonSet renders → passes",
			recipeResult: result(
				sentinelRef(map[string]any{
					"global": map[string]any{"metadataCollector": map[string]any{"enabled": false}},
				}),
				gpuOpRef("gpu-operator", operatorClass("nvidia-container-runtime"))),
		},
		{
			name: "non-string operator.runtimeClass → cannot verify → skipped",
			recipeResult: result(sentinelRef(nil),
				gpuOpRef("gpu-operator", operatorClass(true))),
		},
		{
			name: "non-string collector runtimeClassName → cannot verify → skipped",
			recipeResult: result(sentinelRef(collectorClass(42)),
				gpuOpRef("gpu-operator", operatorClass("nvidia-container-runtime"))),
		},
		{
			name: "operator retarget via --set on a matched recipe → blocked",
			recipeResult: result(sentinelRef(nil),
				gpuOpRef("gpu-operator", operatorClass("nvidia"))),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"gpuoperator": {"operator.runtimeClass": "nvidia-container-runtime"},
			})),
			wantBlocked: true,
		},
		{
			name: "--dynamic on the collector runtime-class path → blocked even when static values match",
			recipeResult: result(sentinelRef(nil),
				gpuOpRef("gpu-operator", operatorClass("nvidia"))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"metadata-collector.runtimeClassName"},
			})),
			wantBlocked:  true,
			wantContains: []string{"--dynamic nv-sentinel:metadata-collector.runtimeClassName"},
		},
		{
			name: "--dynamic on operator.runtimeClass → blocked even when static values match",
			recipeResult: result(sentinelRef(nil),
				gpuOpRef("gpu-operator", operatorClass("nvidia"))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"gpuoperator": {"operator.runtimeClass"},
			})),
			wantBlocked:  true,
			wantContains: []string{"--dynamic gpuoperator:operator.runtimeClass"},
		},
		{
			// Regression: with the collector's runtime class explicitly
			// empty the field is omitted from the pod spec and every pod
			// is admitted, so retargeting the OPERATOR's class at install
			// time cannot cause #2176 — pinned on a supported EKS shape.
			name: "explicit-empty collector + --dynamic on operator.runtimeClass → not gated",
			recipeResult: result(sentinelRef(collectorClass("")),
				gpuOpRef("gpu-operator", operatorClass("nvidia"))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"gpuoperator": {"operator.runtimeClass"},
			})),
		},
		{
			// The collector path stays hazardous in the explicit-empty
			// state: an install-time edit could replace the empty value
			// with a class no RuntimeClass matches.
			name: "explicit-empty collector + --dynamic on the collector class path → blocked",
			recipeResult: result(sentinelRef(collectorClass("")),
				gpuOpRef("gpu-operator", operatorClass("nvidia"))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"metadata-collector.runtimeClassName"},
			})),
			wantBlocked:  true,
			wantContains: []string{"--dynamic nv-sentinel:metadata-collector.runtimeClassName"},
		},
		{
			// Guard scoping: with the collector ENABLED and
			// verified, a dynamic on the enable condition only lets an
			// install-time edit disable it — the harmless direction — so
			// the path is guarded only when the disabled-skip clears the
			// gate.
			name: "--dynamic on the enable condition with the collector enabled → not gated",
			recipeResult: result(sentinelRef(nil),
				gpuOpRef("gpu-operator", operatorClass("nvidia"))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.metadataCollector.enabled"},
			})),
		},
		{
			// Regression: AKS with the collector statically aligned to
			// the operator's class AND statically disabled must not gate
			// the enable-condition dynamic — an install-time re-enable
			// deploys an admitted collector.
			name: "disabled + statically ALIGNED collector + enable dynamic → not gated (round 5)",
			recipeResult: result(
				sentinelRef(map[string]any{
					"metadata-collector": map[string]any{"runtimeClassName": "nvidia-container-runtime"},
					"global":             map[string]any{"metadataCollector": map[string]any{"enabled": false}},
				}),
				gpuOpRef("gpu-operator", operatorClass("nvidia-container-runtime"))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.metadataCollector.enabled"},
			})),
		},
		{
			// Regression: with the collector disabled and its class
			// explicitly empty, a re-enabled collector omits
			// runtimeClassName from the pod spec and is admitted under
			// ANY operator class — so an operator-class dynamic (even
			// together with the enable dynamic) must not arm the guard;
			// only the collector's own class path endangers this state.
			name: "disabled + explicit-empty class + enable AND operator-class dynamics → not gated",
			recipeResult: result(
				sentinelRef(map[string]any{
					"metadata-collector": map[string]any{"runtimeClassName": ""},
					"global":             map[string]any{"metadataCollector": map[string]any{"enabled": false}},
				}),
				gpuOpRef("gpu-operator", operatorClass("nvidia"))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.metadataCollector.enabled"},
				"gpuoperator": {"operator.runtimeClass"},
			})),
		},
		{
			// Blocked control for the row above: the collector's own
			// class path can be filled with a nonexistent class at
			// install time, so it keeps the guard armed.
			name: "disabled + explicit-empty class + enable AND collector-class dynamics → blocked",
			recipeResult: result(
				sentinelRef(map[string]any{
					"metadata-collector": map[string]any{"runtimeClassName": ""},
					"global":             map[string]any{"metadataCollector": map[string]any{"enabled": false}},
				}),
				gpuOpRef("gpu-operator", operatorClass("nvidia"))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"metadata-collector.runtimeClassName", "global.metadataCollector.enabled"},
			})),
			wantBlocked:  true,
			wantContains: []string{"global.metadataCollector.enabled"},
		},
		{
			// Alignment does not help when a class path is itself
			// dynamic: the verified state is install-time editable.
			name: "disabled + aligned collector + class-path AND enable dynamics → blocked",
			recipeResult: result(
				sentinelRef(map[string]any{
					"metadata-collector": map[string]any{"runtimeClassName": "nvidia-container-runtime"},
					"global":             map[string]any{"metadataCollector": map[string]any{"enabled": false}},
				}),
				gpuOpRef("gpu-operator", operatorClass("nvidia-container-runtime"))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"metadata-collector.runtimeClassName", "global.metadataCollector.enabled"},
			})),
			wantBlocked:  true,
			wantContains: []string{"global.metadataCollector.enabled", "not verifiably"},
		},
		{
			name: "--dynamic on the enable condition when the disabled-skip clears the gate → blocked",
			recipeResult: result(
				sentinelRef(map[string]any{
					"global": map[string]any{"metadataCollector": map[string]any{"enabled": false}},
				}),
				gpuOpRef("gpu-operator", operatorClass("nvidia-container-runtime"))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"global.metadataCollector.enabled"},
			})),
			wantBlocked:  true,
			wantContains: []string{"global.metadataCollector.enabled", "re-enabling it"},
		},
		{
			name: "--dynamic on an unrelated path → not gated",
			recipeResult: result(sentinelRef(nil),
				gpuOpRef("gpu-operator", operatorClass("nvidia"))),
			bundlerConfig: config.NewConfig(config.WithDynamicValues(map[string][]string{
				"nv-sentinel": {"platformConnector.resources.limits.cpu"},
				"gpuoperator": {"dcgmExporter.serviceMonitor.interval"},
			})),
		},
		{
			// gpu-operator branch of the fail-closed contract: the --set
			// below traverses a path whose parent the recipe pins to a
			// scalar, so the GPU Operator's effective values cannot be
			// reconstructed and the gate errors instead of skipping.
			name: "gpu-operator values unresolvable → hard error (fails closed)",
			recipeResult: result(sentinelRef(nil),
				gpuOpRef("gpu-operator", map[string]any{"operator": "scalar"})),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"gpuoperator": {"operator.runtimeClass": "nvidia"},
			})),
			wantErrs: 1,
		},
		{
			// nvsentinel branch of the same contract.
			name: "nvsentinel values unresolvable → hard error (fails closed)",
			recipeResult: result(sentinelRef(map[string]any{"metadata-collector": "scalar"}),
				gpuOpRef("gpu-operator", operatorClass("nvidia"))),
			bundlerConfig: config.NewConfig(config.WithValueOverrides(map[string]map[string]string{
				"nv-sentinel": {"metadata-collector.runtimeClassName": "nvidia"},
			})),
			wantErrs: 1,
		},
		{
			name: "subset bundle: gpu-operator only in the declared union → still blocked on mismatch",
			recipeResult: func() *recipe.RecipeResult {
				r := result(sentinelRef(nil))
				return r.WithDeclaredComponents([]recipe.ComponentRef{
					{Name: nvsentinelComponent},
					{Name: "gpu-operator", Overrides: operatorClass("nvidia-container-runtime")},
				})
			}(),
			wantBlocked: true,
		},
		{
			name: "OCP variant resolved by name → blocked on mismatch",
			recipeResult: &recipe.RecipeResult{
				Criteria: &recipe.Criteria{Service: recipe.CriteriaServiceOCP},
				ComponentRefs: []recipe.ComponentRef{
					sentinelRef(nil),
					gpuOpRef("gpu-operator-ocp", operatorClass("nvidia-container-runtime")),
				},
			},
			wantBlocked: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := tt.bundlerConfig
			if cfg == nil {
				cfg = config.NewConfig()
			}
			msgs, errs := CheckNVSentinelRuntimeClassCoherence(
				t.Context(), nvsentinelComponent, tt.recipeResult, cfg, tt.conditions)
			if len(errs) != tt.wantErrs {
				t.Fatalf("errs = %d (%v), want %d", len(errs), errs, tt.wantErrs)
			}
			for _, err := range errs {
				if !stderrors.Is(err, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "")) {
					t.Errorf("hard error code = %v, want ErrCodeInvalidRequest", err)
				}
			}
			blocked := len(msgs) > 0
			if blocked != tt.wantBlocked {
				t.Fatalf("blocked = %v (msgs %v), want %v", blocked, msgs, tt.wantBlocked)
			}
			if !blocked {
				return
			}
			for _, want := range tt.wantContains {
				if !strings.Contains(msgs[0], want) {
					t.Errorf("message missing %q:\n%s", want, msgs[0])
				}
			}
		})
	}
}

// TestCheckNVSentinelRuntimeClassCoherence_NilConfigRuns mirrors the
// driver-label gate's nil-config contract: since #2181 the AKS profile
// owns both operator.runtimeClass and
// metadata-collector.runtimeClassName, so the coherent state is
// reachable from resolved values alone and the values-only SDK path is
// no longer exempt.
func TestCheckNVSentinelRuntimeClassCoherence_NilConfigRuns(t *testing.T) {
	t.Parallel()

	rr := &recipe.RecipeResult{
		Criteria: &recipe.Criteria{Service: recipe.CriteriaServiceAKS},
		ComponentRefs: []recipe.ComponentRef{
			{Name: nvsentinelComponent},
			{Name: "gpu-operator", Overrides: map[string]any{
				"operator": map[string]any{"runtimeClass": "nvidia-container-runtime"},
			}},
		},
	}

	// An incoherent recipe is blocked whether or not a config is
	// supplied — the nil-config exemption is gone.
	msgs, errs := CheckNVSentinelRuntimeClassCoherence(
		t.Context(), nvsentinelComponent, rr, config.NewConfig(), nil)
	if len(errs) != 0 || len(msgs) != 1 {
		t.Fatalf("non-nil config: msgs = %v, errs = %v; want exactly one blocking message", msgs, errs)
	}

	msgs, errs = CheckNVSentinelRuntimeClassCoherence(t.Context(), nvsentinelComponent, rr, nil, nil)
	if len(errs) != 0 || len(msgs) != 1 {
		t.Fatalf("nil config: msgs = %v, errs = %v; want exactly one blocking message", msgs, errs)
	}

	// Control against a gate that blocks unconditionally: a recipe whose
	// two names already agree passes on the same nil-config path. This is
	// the shape #2181 gives every profiled AKS recipe.
	coherent := &recipe.RecipeResult{
		Criteria: &recipe.Criteria{Service: recipe.CriteriaServiceAKS},
		ComponentRefs: []recipe.ComponentRef{
			{Name: nvsentinelComponent, Overrides: map[string]any{
				"metadata-collector": map[string]any{"runtimeClassName": "nvidia-container-runtime"},
			}},
			{Name: "gpu-operator", Overrides: map[string]any{
				"operator": map[string]any{"runtimeClass": "nvidia-container-runtime"},
			}},
		},
	}
	msgs, errs = CheckNVSentinelRuntimeClassCoherence(t.Context(), nvsentinelComponent, coherent, nil, nil)
	if len(errs) != 0 || len(msgs) != 0 {
		t.Fatalf("nil config with coherent names: msgs = %v, errs = %v; want it to pass", msgs, errs)
	}
}

// TestNVSentinelRuntimeClassPlatformSpotChecks runs the gate against
// recipes resolved from the real embedded catalog. Unlike the
// driver-label matrix this is a value comparison, so the platform
// outcomes follow from the resolved values alone. Since #2181 the same
// gpuStack value owns both names, so every profiled AKS recipe is
// coherent by construction: azure-managed pairs
// nvidia-container-runtime with itself, operator-managed pairs nvidia
// with itself, and EKS keeps the chart default on both sides.
func TestNVSentinelRuntimeClassPlatformSpotChecks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		criteria    *recipe.Criteria
		profile     string
		overrides   map[string]map[string]string
		wantBlocked bool
	}{
		{
			name: "AKS azure-managed (default, profile pairs both names) → passes",
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceAKS, Accelerator: recipe.CriteriaAcceleratorH100,
				OS: recipe.CriteriaOSUbuntu, Intent: recipe.CriteriaIntentTraining,
			},
			wantBlocked: false,
		},
		{
			// Control against a vacuous pass on the row above: overriding
			// the profile-supplied name back to the chart default
			// reintroduces #2176 and must still block. (Bundle generation
			// rejects it earlier still, at the profile lock.)
			name: "AKS azure-managed with the profile value overridden to nvidia → blocked",
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceAKS, Accelerator: recipe.CriteriaAcceleratorH100,
				OS: recipe.CriteriaOSUbuntu, Intent: recipe.CriteriaIntentTraining,
			},
			overrides: map[string]map[string]string{
				"nv-sentinel": {"metadata-collector.runtimeClassName": "nvidia"},
			},
			wantBlocked: true,
		},
		{
			name: "AKS operator-managed → both nvidia → passes",
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceAKS, Accelerator: recipe.CriteriaAcceleratorH100,
				OS: recipe.CriteriaOSUbuntu, Intent: recipe.CriteriaIntentTraining,
			},
			profile:     "gpuStack=operator-managed",
			wantBlocked: false,
		},
		{
			name: "EKS → operator.runtimeClass at default → passes",
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceEKS, Accelerator: recipe.CriteriaAcceleratorH100,
				OS: recipe.CriteriaOSUbuntu, Intent: recipe.CriteriaIntentTraining,
			},
			wantBlocked: false,
		},
		{
			name: "GKE COS gke-default → operator.runtimeClass at default → passes",
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceGKE, Accelerator: recipe.CriteriaAcceleratorH100,
				OS: recipe.CriteriaOSCOS, Intent: recipe.CriteriaIntentTraining,
			},
			wantBlocked: false,
		},
		{
			name: "Kind → operator.runtimeClass at default → passes",
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceKind, Accelerator: recipe.CriteriaAcceleratorH100,
				Intent: recipe.CriteriaIntentTraining,
			},
			wantBlocked: false,
		},
		{
			name: "OKE default → operator.runtimeClass at default → passes",
			criteria: &recipe.Criteria{
				Service: recipe.CriteriaServiceOKE, Accelerator: recipe.CriteriaAcceleratorA100,
				OS: recipe.CriteriaOSOracleLinux, Intent: recipe.CriteriaIntentTraining,
			},
			wantBlocked: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, err := recipe.NewBuilder().BuildFromCriteriaWithProfile(t.Context(), tt.criteria, tt.profile)
			if err != nil {
				t.Fatalf("BuildFromCriteriaWithProfile(%v, %q) failed: %v", tt.criteria, tt.profile, err)
			}
			if result.GetComponentRef(nvsentinelComponent) == nil {
				t.Fatalf("resolved recipe has no %s component ref", nvsentinelComponent)
			}
			// Control against a vacuous pass: the gate also skips when
			// the GPU Operator ref is absent or disabled, so a catalog
			// change removing/disabling it would let wantBlocked:false
			// rows keep passing while silently losing their coverage.
			gpuOpName, gpuOpRef, _ := resolveGPUOperatorRef(result)
			if gpuOpRef == nil {
				t.Fatal("resolved recipe has no GPU Operator ref; the gate would skip and the row would pass vacuously")
			}
			if !gpuOpRef.IsEnabled() {
				t.Fatalf("GPU Operator %q is disabled; the gate would skip and the row would pass vacuously", gpuOpName)
			}
			cfg := config.NewConfig()
			if tt.overrides != nil {
				cfg = config.NewConfig(config.WithValueOverrides(tt.overrides))
			}
			msgs, errs := CheckNVSentinelRuntimeClassCoherence(
				t.Context(), nvsentinelComponent, result, cfg, nil)
			if len(errs) != 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			if blocked := len(msgs) > 0; blocked != tt.wantBlocked {
				t.Fatalf("blocked = %v, want %v (msgs: %v)", blocked, tt.wantBlocked, msgs)
			}
		})
	}
}

// TestResolvedStringValue covers the shared path walker.
func TestResolvedStringValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		values    map[string]any
		path      string
		wantValue string
		wantOK    bool
		wantValid bool
	}{
		{"absent top-level key", map[string]any{}, "a.b", "", false, true},
		{"absent leaf", map[string]any{"a": map[string]any{}}, "a.b", "", false, true},
		{"present string", map[string]any{"a": map[string]any{"b": "x"}}, "a.b", "x", true, true},
		{"present empty string", map[string]any{"a": map[string]any{"b": ""}}, "a.b", "", true, true},
		{"non-string leaf", map[string]any{"a": map[string]any{"b": 7}}, "a.b", "", false, false},
		{"non-map intermediate", map[string]any{"a": "scalar"}, "a.b", "", false, true},
		{"single segment", map[string]any{"a": "x"}, "a", "x", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			value, ok, valid := resolvedStringValue(tt.values, tt.path)
			if value != tt.wantValue || ok != tt.wantOK || valid != tt.wantValid {
				t.Errorf("resolvedStringValue(%v, %q) = (%q, %v, %v), want (%q, %v, %v)",
					tt.values, tt.path, value, ok, valid, tt.wantValue, tt.wantOK, tt.wantValid)
			}
		})
	}
}
