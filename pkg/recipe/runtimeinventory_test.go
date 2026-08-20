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

import "testing"

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
