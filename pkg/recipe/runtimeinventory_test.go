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
	"slices"
	"strings"
	"testing"
)

// runtimeInventoryTestResult is a minimal recipe that declares the runtime
// inventory component alongside an unrelated one, so a case can tell "the
// selection dropped k8s-aibom" from "the selection dropped everything".
func runtimeInventoryTestResult() *RecipeResult {
	return &RecipeResult{
		Kind:       RecipeResultKind,
		APIVersion: "aicr.run/v1alpha2",
		ComponentRefs: []ComponentRef{
			{Name: "gpu-operator", Type: ComponentTypeHelm, Namespace: "gpu-operator"},
			{
				Name:               runtimeInventoryComponentName,
				Type:               ComponentTypeHelm,
				Namespace:          "k8s-aibom-system",
				HealthCheckAsserts: "apiVersion: chainsaw.kyverno.io/v1alpha1\nkind: Test\n",
			},
		},
	}
}

func TestParseRuntimeInventoryMode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    RuntimeInventoryMode
		wantErr bool
	}{
		{name: "enabled", input: "enabled", want: RuntimeInventoryEnabled},
		{name: "disabled", input: "disabled", want: RuntimeInventoryDisabled},
		{name: "empty", input: "", wantErr: true},
		{name: "unknown", input: "off", wantErr: true},
		{name: "wrong case", input: "Disabled", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseRuntimeInventoryMode(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseRuntimeInventoryMode() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ParseRuntimeInventoryMode() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestApplyBuildConfigRuntimeInventory covers the ADR-019 requirement that
// stock adoption carry "generation-time, recipe-recorded selection and opt-out
// semantics". A bundle-time --set was rejected there because it changes
// neither the recipe nor its health checks, so both are asserted here.
func TestApplyBuildConfigRuntimeInventory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		mode          *RuntimeInventoryMode
		wantRecorded  bool
		wantInstalled bool
	}{
		{
			// No flag: the overlay's own declaration stands and the recipe
			// records nothing, so a stock recipe is unchanged by this feature.
			name: "absent leaves the recipe untouched", mode: nil,
			wantRecorded: false, wantInstalled: true,
		},
		{
			name: "enabled records the decision", mode: modePtr(RuntimeInventoryEnabled),
			wantRecorded: true, wantInstalled: true,
		},
		{
			name: "disabled removes the component", mode: modePtr(RuntimeInventoryDisabled),
			wantRecorded: true, wantInstalled: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := runtimeInventoryTestResult()
			if err := applyBuildConfig(result, &buildConfig{runtimeInventoryMode: tt.mode}); err != nil {
				t.Fatalf("applyBuildConfig() error = %v", err)
			}

			gotMode, present := result.RuntimeInventoryMode()
			if present != tt.wantRecorded {
				t.Fatalf("RuntimeInventoryMode() present = %v, want %v", present, tt.wantRecorded)
			}
			if tt.wantRecorded {
				if gotMode != *tt.mode {
					t.Errorf("recorded mode = %q, want %q", gotMode, *tt.mode)
				}
				// A recipe carrying configuration must say so in its
				// apiVersion, or a consumer cannot tell it apart from a
				// plain resolved recipe.
				if result.APIVersion != ConfiguredRecipeResultAPIVersion {
					t.Errorf("APIVersion = %q, want %q", result.APIVersion, ConfiguredRecipeResultAPIVersion)
				}
			}

			ref := result.GetComponentRef(runtimeInventoryComponentName)
			if ref == nil {
				t.Fatalf("%s ref missing entirely; selection must disable it, not delete it",
					runtimeInventoryComponentName)
			}
			if got := ref.IsEnabled(); got != tt.wantInstalled {
				t.Errorf("IsEnabled() = %v, want %v (overrides: %v)", got, tt.wantInstalled, ref.Overrides)
			}

			// The health check travels with the component: it lives on the
			// same ref, so disabling the component takes its check out of
			// deployment validation. This is the half a bundle-time --set
			// could not achieve.
			if !tt.wantInstalled && ref.IsEnabled() {
				t.Error("component still enabled, so its health check would still run")
			}

			// An unrelated component must be untouched either way.
			if other := result.GetComponentRef("gpu-operator"); other == nil || !other.IsEnabled() {
				t.Error("gpu-operator was affected by runtime inventory selection")
			}
		})
	}
}

// TestApplyBuildConfigRuntimeInventoryRejectsAbsentComponent pins the
// fail-closed direction. Selecting a mode on a recipe that does not resolve the
// component is a mistake — a wrong --service, a typo, a recipe that never had
// it — and silently succeeding would report a decision the recipe cannot honor.
func TestApplyBuildConfigRuntimeInventoryRejectsAbsentComponent(t *testing.T) {
	t.Parallel()

	for _, mode := range []RuntimeInventoryMode{RuntimeInventoryEnabled, RuntimeInventoryDisabled} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			result := &RecipeResult{
				Kind:       RecipeResultKind,
				APIVersion: "aicr.run/v1alpha2",
				ComponentRefs: []ComponentRef{
					{Name: "gpu-operator", Type: ComponentTypeHelm, Namespace: "gpu-operator"},
				},
			}
			err := applyBuildConfig(result, &buildConfig{runtimeInventoryMode: &mode})
			if err == nil {
				t.Fatal("applyBuildConfig() error = nil, want an error for a recipe without the component")
			}
			if result.Configuration != nil {
				t.Error("Configuration recorded despite the error; the recipe must not claim a mode it cannot honor")
			}
		})
	}
}

func modePtr(m RuntimeInventoryMode) *RuntimeInventoryMode { return &m }

// TestApplyBuildConfigPreservesBothConfigurations covers a regression found in
// review: applyBuildConfig applies the runtime inventory selection first, and
// the accounting path then assigned a fresh RecipeConfiguration, discarding it.
//
// The failure was silent and asymmetric. The component override survived on the
// ref, so the component really was disabled, while the recipe no longer
// recorded why — precisely the "recipe records the decision" contract ADR-019
// requires. Both sections must survive a resolve that selects both.
func TestApplyBuildConfigPreservesBothConfigurations(t *testing.T) {
	t.Parallel()

	result := runtimeInventoryTestResult()
	// A Slurm recipe that also declares the runtime inventory component is the
	// shape that reaches both paths: accounting requires platform=slurm, and
	// the inventory selection requires the component to be declared.
	result.ComponentRefs = append(result.ComponentRefs,
		ComponentRef{Name: slinkySlurmComponentName, Type: ComponentTypeHelm, Namespace: "slurm"},
		ComponentRef{Name: mariaDBOperatorCRDsComponentName, Type: ComponentTypeHelm, Namespace: "slurm"},
		ComponentRef{Name: mariaDBOperatorComponentName, Type: ComponentTypeHelm, Namespace: "slurm"},
		ComponentRef{Name: slurmAccountingMariaDBComponentName, Type: ComponentTypeHelm, Namespace: "slurm"},
	)

	accounting := AccountingModeDisabled
	inventory := RuntimeInventoryDisabled
	if err := applyBuildConfig(result, &buildConfig{
		accountingMode:       &accounting,
		runtimeInventoryMode: &inventory,
	}); err != nil {
		t.Fatalf("applyBuildConfig() error = %v", err)
	}

	if _, present := result.RuntimeInventoryMode(); !present {
		t.Error("runtime inventory configuration was dropped when accounting was also selected")
	}
	if _, present := result.AccountingMode(); !present {
		t.Error("accounting configuration missing")
	}

	// The component override must survive too, so the two records agree.
	ref := result.GetComponentRef(runtimeInventoryComponentName)
	if ref == nil || ref.IsEnabled() {
		t.Error("runtime inventory component should be disabled")
	}
}

// TestWithRuntimeInventoryModeOption exercises the BuildOption constructor
// rather than assembling buildConfig directly. Tests that reach past the
// public option cannot catch a defect in the option itself, and the option is
// the surface SDK callers actually use.
func TestWithRuntimeInventoryModeOption(t *testing.T) {
	t.Parallel()

	cfg := &buildConfig{}
	WithRuntimeInventoryMode(RuntimeInventoryDisabled)(cfg)

	if cfg.runtimeInventoryMode == nil {
		t.Fatal("WithRuntimeInventoryMode() left runtimeInventoryMode nil")
	}
	if *cfg.runtimeInventoryMode != RuntimeInventoryDisabled {
		t.Errorf("mode = %q, want %q", *cfg.runtimeInventoryMode, RuntimeInventoryDisabled)
	}

	// A nil option must be tolerated by the code that applies options, not
	// merely be nil. Asserting the local variable is nil is a tautology and
	// would still pass if resolveBuildConfig invoked it and panicked.
	var nilOpt BuildOption
	got, err := resolveBuildConfig(nil, nilOpt, WithRuntimeInventoryMode(RuntimeInventoryEnabled))
	if err != nil {
		t.Fatalf("resolveBuildConfig() with a nil option error = %v", err)
	}
	if got.runtimeInventoryMode == nil || *got.runtimeInventoryMode != RuntimeInventoryEnabled {
		t.Errorf("a nil option interfered with the following option: %v", got.runtimeInventoryMode)
	}
}

// TestApplyRuntimeInventoryRecomputesDeploymentOrder covers a defect found in
// review: disabling the component left it listed in DeploymentOrder.
//
// TopologicalSort emits enabled components only, and the recompute used to sit
// inside the accounting branch, which a runtime-inventory-only build never
// reaches. The emitted recipe then contradicted itself — deploymentOrder named
// a component the same document marked disabled. The bundler re-filters by
// IsEnabled so nothing mis-deployed, but `aicr query --selector deploymentOrder`
// reported it.
func TestApplyRuntimeInventoryRecomputesDeploymentOrder(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name      string
		mode      RuntimeInventoryMode
		wantInOrd bool
	}{
		{name: "disabled drops it from deployment order", mode: RuntimeInventoryDisabled, wantInOrd: false},
		{name: "enabled keeps it", mode: RuntimeInventoryEnabled, wantInOrd: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result := runtimeInventoryTestResult()
			result.DeploymentOrder = []string{"gpu-operator", runtimeInventoryComponentName}

			if err := applyBuildConfig(result, &buildConfig{runtimeInventoryMode: &tt.mode}); err != nil {
				t.Fatalf("applyBuildConfig() error = %v", err)
			}

			inOrder := slices.Contains(result.DeploymentOrder, runtimeInventoryComponentName)
			if inOrder != tt.wantInOrd {
				t.Errorf("%s in deploymentOrder = %v, want %v (order=%v)",
					runtimeInventoryComponentName, inOrder, tt.wantInOrd, result.DeploymentOrder)
			}

			// The invariant that matters: deploymentOrder and IsEnabled agree.
			for _, ref := range result.ComponentRefs {
				listed := slices.Contains(result.DeploymentOrder, ref.Name)
				if listed != ref.IsEnabled() {
					t.Errorf("component %q: inDeploymentOrder=%v IsEnabled=%v; these must agree",
						ref.Name, listed, ref.IsEnabled())
				}
			}
		})
	}
}

// TestApplyRuntimeInventoryRejectsIncoherentEnable covers a coherence gap found
// in review: mode=enabled writes install:true, but IsEnabled fails closed on
// either the `enabled` or the `install` key. An overlay that already set
// `enabled: false` therefore leaves the component disabled while the recipe
// records mode: enabled — stating a decision it does not implement.
func TestApplyRuntimeInventoryRejectsIncoherentEnable(t *testing.T) {
	t.Parallel()

	result := runtimeInventoryTestResult()
	ref := result.GetComponentRef(runtimeInventoryComponentName)
	ref.Overrides = map[string]any{"enabled": false}

	mode := RuntimeInventoryEnabled
	err := applyBuildConfig(result, &buildConfig{runtimeInventoryMode: &mode})
	if err == nil {
		t.Fatal("applyBuildConfig() error = nil, want rejection of an enable the recipe cannot honor")
	}
	// The pre-write guard in applyRuntimeInventoryMode now catches this before
	// any override is written, so the rejection comes from there rather than
	// from the post-write coherence check below it. Both refuse the same thing;
	// this asserts the behavior (an enable the recipe cannot honor is refused)
	// rather than which of the two guards produced the message.
	if !strings.Contains(err.Error(), "cannot be re-enabled") {
		t.Errorf("error = %v, want a rejection of an enable the recipe cannot honor", err)
	}
}

// TestDeepCopyPreservesRuntimeInventory covers a defect found in review:
// RecipeResult.DeepCopy allocated a fresh RecipeConfiguration and cloned only
// Slurm, so the runtime inventory record was dropped rather than aliased.
//
// Client.AdoptRecipe always deep-copies, so an adopted recipe kept the
// component override the selection applied while losing the configuration that
// explains it — a recipe acting on a decision it no longer records.
func TestDeepCopyPreservesRuntimeInventory(t *testing.T) {
	t.Parallel()

	result := runtimeInventoryTestResult()
	mode := RuntimeInventoryDisabled
	if err := applyBuildConfig(result, &buildConfig{runtimeInventoryMode: &mode}); err != nil {
		t.Fatalf("applyBuildConfig() error = %v", err)
	}

	clone := result.DeepCopy()
	got, present := clone.RuntimeInventoryMode()
	if !present {
		t.Fatal("DeepCopy dropped configuration.runtimeInventory")
	}
	if got != RuntimeInventoryDisabled {
		t.Errorf("cloned mode = %q, want %q", got, RuntimeInventoryDisabled)
	}

	// A copy, not an alias: mutating the clone must not reach the original.
	clone.Configuration.RuntimeInventory.Mode = RuntimeInventoryEnabled
	if orig, _ := result.RuntimeInventoryMode(); orig != RuntimeInventoryDisabled {
		t.Errorf("original mode = %q after mutating the clone; the pointer is shared", orig)
	}

	// The sibling section must survive the same copy.
	if clone.Configuration.Slurm != nil && result.Configuration.Slurm == nil {
		t.Error("clone invented a Slurm section")
	}
}

// TestQueryHydrationExposesRuntimeInventory covers the second half of the same
// class: the recipe recorded the selection but `aicr query --selector
// configuration.runtimeInventory.mode` returned NOT_FOUND, because hydration
// projected only Configuration.Slurm.
func TestQueryHydrationExposesRuntimeInventory(t *testing.T) {
	t.Parallel()

	result := runtimeInventoryTestResult()
	mode := RuntimeInventoryDisabled
	if err := applyBuildConfig(result, &buildConfig{runtimeInventoryMode: &mode}); err != nil {
		t.Fatalf("applyBuildConfig() error = %v", err)
	}

	hydrated, err := HydrateResult(result)
	if err != nil {
		t.Fatalf("Hydrate() error = %v", err)
	}
	got, err := Select(hydrated, "configuration.runtimeInventory.mode")
	if err != nil {
		t.Fatalf("Select(configuration.runtimeInventory.mode) error = %v", err)
	}
	if got != string(RuntimeInventoryDisabled) {
		t.Errorf("selector returned %v, want %q", got, RuntimeInventoryDisabled)
	}
}
