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
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"gopkg.in/yaml.v3"
)

// The AKS family is the first embedded adopter of an ADR-015 configuration
// profile (gpuStack on recipes/overlays/aks.yaml). These tests qualify both
// declared values through the ordinary Builder against the embedded catalog —
// no external data directory — pinning the ownership surface and the override
// tuple each value delivers.

func aksCriteria() *Criteria {
	return &Criteria{
		Service:     CriteriaServiceAKS,
		Accelerator: CriteriaAcceleratorH100,
		OS:          CriteriaOSUbuntu,
		Intent:      CriteriaIntentTraining,
	}
}

func TestAKSGPUStackProfile(t *testing.T) {
	wantOwned := map[string][]string{
		"gpu-operator": {
			"driver.enabled", "enabled", "operator.runtimeClass", "toolkit.enabled",
		},
		"nvidia-dra-driver-gpu": {"enabled", "nvidiaDriverRoot"},
	}

	tests := []struct {
		name           string
		selection      string
		wantValue      string
		wantDriver     bool
		wantToolkit    bool
		wantRuntime    string
		wantDriverRoot string
		wantConstraint string
	}{
		{
			// The declaration default matches the AKS "Driver only" install
			// profile the family has always shipped.
			name:           "default is azure-managed",
			selection:      "",
			wantValue:      "azure-managed",
			wantDriver:     false,
			wantToolkit:    false,
			wantRuntime:    "nvidia-container-runtime",
			wantDriverRoot: "/",
			wantConstraint: "Install",
		},
		{
			name:           "operator value flips the four paths together",
			selection:      "gpuStack=operator-managed",
			wantValue:      "operator-managed",
			wantDriver:     true,
			wantToolkit:    true,
			wantRuntime:    "nvidia",
			wantDriverRoot: "/run/nvidia/driver",
			wantConstraint: "None",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := NewBuilder().BuildFromCriteriaWithProfile(
				t.Context(), aksCriteria(), tt.selection)
			if err != nil {
				t.Fatalf("BuildFromCriteriaWithProfile() error = %v", err)
			}
			if result.APIVersion != RecipeProfileAPIVersion {
				t.Fatalf("apiVersion = %q, want %q", result.APIVersion, RecipeProfileAPIVersion)
			}
			selected := result.Metadata.SelectedProfile
			if selected == nil || selected.Name != "gpuStack" || selected.Value != tt.wantValue {
				t.Fatalf("selectedProfile = %#v, want gpuStack=%s", selected, tt.wantValue)
			}
			if !reflect.DeepEqual(selected.OwnedPaths, wantOwned) {
				t.Fatalf("ownedPaths = %#v, want %#v", selected.OwnedPaths, wantOwned)
			}

			operator := result.GetComponentRef("gpu-operator")
			if operator == nil {
				t.Fatal("gpu-operator componentRef missing")
			}
			driver, ok := operator.Overrides["driver"].(map[string]any)
			if !ok {
				t.Fatalf("gpu-operator overrides.driver = %#v, want map", operator.Overrides["driver"])
			}
			toolkit, ok := operator.Overrides["toolkit"].(map[string]any)
			if !ok {
				t.Fatalf("gpu-operator overrides.toolkit = %#v, want map", operator.Overrides["toolkit"])
			}
			op, ok := operator.Overrides["operator"].(map[string]any)
			if !ok {
				t.Fatalf("gpu-operator overrides.operator = %#v, want map", operator.Overrides["operator"])
			}
			if enabled, _ := driver["enabled"].(bool); enabled != tt.wantDriver {
				t.Fatalf("driver.enabled = %v, want %v", driver["enabled"], tt.wantDriver)
			}
			if enabled, _ := toolkit["enabled"].(bool); enabled != tt.wantToolkit {
				t.Fatalf("toolkit.enabled = %v, want %v", toolkit["enabled"], tt.wantToolkit)
			}
			if rc, _ := op["runtimeClass"].(string); rc != tt.wantRuntime {
				t.Fatalf("operator.runtimeClass = %v, want %q", op["runtimeClass"], tt.wantRuntime)
			}
			dra := result.GetComponentRef("nvidia-dra-driver-gpu")
			if dra == nil {
				t.Fatal("nvidia-dra-driver-gpu componentRef missing")
			}
			if root, _ := dra.Overrides["nvidiaDriverRoot"].(string); root != tt.wantDriverRoot {
				t.Fatalf("nvidiaDriverRoot = %v, want %q", dra.Overrides["nvidiaDriverRoot"], tt.wantDriverRoot)
			}

			// The selected value's distinguishing constraint is recorded in
			// the composed recipe (evaluated only when a snapshot supplies
			// the K8s.aks-gpu-pools.gpu-driver projection).
			found := ""
			matches := 0
			for _, constraint := range result.Constraints {
				if constraint.Name == "K8s.aks-gpu-pools.gpu-driver" {
					found = constraint.Value
					matches++
				}
			}
			if matches != 1 {
				t.Fatalf("gpu-driver constraint matches = %d, want exactly 1", matches)
			}
			if found != tt.wantConstraint {
				t.Fatalf("gpu-driver constraint = %q, want %q", found, tt.wantConstraint)
			}

			if err := result.PrepareAndValidateWithContext(t.Context()); err != nil {
				t.Fatalf("PrepareAndValidateWithContext() error = %v", err)
			}
		})
	}
}

// TestAKSChildrenInheritTheDeclaration pins that the declaration on the aks
// ancestor reaches every AKS leaf, not only the criteria used above.
func TestAKSChildrenInheritTheDeclaration(t *testing.T) {
	criteria := aksCriteria()
	criteria.Platform = CriteriaPlatformKubeflow

	result, err := NewBuilder().BuildFromCriteriaWithProfile(
		t.Context(), criteria, "gpuStack=operator-managed")

	if err != nil {
		t.Fatalf("BuildFromCriteriaWithProfile() error = %v", err)
	}
	selected := result.Metadata.SelectedProfile
	if selected == nil || selected.Value != "operator-managed" {
		t.Fatalf("selectedProfile = %#v, want gpuStack=operator-managed on the kubeflow leaf", selected)
	}
}

// TestAKSDefaultKeepsPreProfileEffectiveValues pins the adoption's
// no-behavior-change guarantee for the default (azure-managed)
// path: an ordinary criteria-only resolution — no snapshot, no --profile —
// must deliver the same components with byte-identical effective values as
// the SAME catalog resolved pre-profile (declaration stripped, legacy
// apiVersion). The only permitted deltas are the profile artifacts
// themselves: apiVersion, metadata.selectedProfile, and the recorded
// distinguishing constraint.
func TestAKSDefaultKeepsPreProfileEffectiveValues(t *testing.T) {
	ctx := t.Context()

	// Fresh stores (not the cached singleton — the legacy variant is
	// mutated below and must not leak into other tests).
	profiledStore, err := buildMetadataStore(ctx, defaultEmbeddedProvider)
	if err != nil {
		t.Fatalf("build profiled store: %v", err)
	}
	legacyStore, err := buildMetadataStore(ctx, defaultEmbeddedProvider)
	if err != nil {
		t.Fatalf("build legacy store: %v", err)
	}
	aksOverlay, ok := legacyStore.Overlays["aks"]
	if !ok {
		t.Fatal("embedded catalog is missing the aks overlay")
	}
	aksOverlay.Spec.Profile = nil
	aksOverlay.APIVersion = RecipeAPIVersion

	profiled, err := profiledStore.BuildRecipeResult(ctx, aksCriteria())
	if err != nil {
		t.Fatalf("profiled resolution: %v", err)
	}
	legacy, err := legacyStore.BuildRecipeResult(ctx, aksCriteria())
	if err != nil {
		t.Fatalf("legacy resolution: %v", err)
	}

	if profiled.Metadata.SelectedProfile == nil ||
		profiled.Metadata.SelectedProfile.Value != "azure-managed" {

		t.Fatalf("profiled default = %#v, want gpuStack=azure-managed", profiled.Metadata.SelectedProfile)
	}
	if legacy.Metadata.SelectedProfile != nil {
		t.Fatalf("legacy resolution unexpectedly profiled: %#v", legacy.Metadata.SelectedProfile)
	}

	// Same component set.
	names := func(r *RecipeResult) []string {
		out := make([]string, 0, len(r.ComponentRefs))
		for i := range r.ComponentRefs {
			out = append(out, r.ComponentRefs[i].Name)
		}
		sort.Strings(out)
		return out
	}
	profiledNames, legacyNames := names(profiled), names(legacy)
	if !reflect.DeepEqual(profiledNames, legacyNames) {
		t.Fatalf("component sets diverged:\nprofiled: %v\nlegacy:   %v", profiledNames, legacyNames)
	}

	// Byte-identical effective values for every component — the profile
	// fragment must be value-identical to what the family always shipped.
	for _, name := range profiledNames {
		got, err := profiled.GetValuesForComponentWithContext(ctx, name)
		if err != nil {
			t.Fatalf("profiled values for %s: %v", name, err)
		}
		want, err := legacy.GetValuesForComponentWithContext(ctx, name)
		if err != nil {
			t.Fatalf("legacy values for %s: %v", name, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("effective values for %s diverged from the pre-profile catalog", name)
		}
	}

	// Constraint delta is exactly the recorded distinguishing constraint.
	extra := make([]string, 0, 1)
	legacyConstraints := make(map[string]int, len(legacy.Constraints))
	for _, c := range legacy.Constraints {
		legacyConstraints[c.Name+"="+c.Value]++
	}
	for _, c := range profiled.Constraints {
		key := c.Name + "=" + c.Value
		if legacyConstraints[key] > 0 {
			legacyConstraints[key]--
			continue
		}
		extra = append(extra, key)
	}
	if len(extra) != 1 || extra[0] != "K8s.aks-gpu-pools.gpu-driver=Install" {
		t.Fatalf("constraint delta = %v, want exactly [K8s.aks-gpu-pools.gpu-driver=Install]", extra)
	}
	for key, n := range legacyConstraints {
		if n != 0 {
			t.Fatalf("legacy constraint %s missing from the profiled resolution", key)
		}
	}
}

// TestAKSLegacyExternalShadowStaysUnprofiled pins the external-catalog
// upgrade hazard documented in docs/integrator/data-extension.md
// ("Converting a family to a configuration profile"): an external --data
// catalog replaces embedded overlay files wholesale, and a pre-PR external
// overlays/aks.yaml — authored on the legacy apiVersion, before the family
// declared gpuStack — is a VALID overlay with no profile declaration.
// Upgrading AICR with such a catalog therefore silently shadows the embedded
// declaration: resolution must succeed UNPROFILED (no error, no
// selectedProfile, legacy apiVersion on the result, no
// K8s.aks-gpu-pools.gpu-driver constraint). This is confirmed, intentional
// replacement semantics, not a defect; a load-time detection of an external
// overlay shadowing an embedded profile declaration (with an explicit
// opt-out) is a possible follow-up. The test pins the current behavior so
// any future change to it is deliberate.
func TestAKSLegacyExternalShadowStaysUnprofiled(t *testing.T) {
	ctx := t.Context()

	// Author the pre-PR external shadow from the real embedded overlay:
	// copy recipes/overlays/aks.yaml, strip spec.profile, and downgrade to
	// the legacy apiVersion — exactly what a --data catalog forked before
	// the family's conversion looks like.
	embeddedAKS, err := defaultEmbeddedProvider.ReadFile(ctx, "overlays/aks.yaml")
	if err != nil {
		t.Fatalf("read embedded overlays/aks.yaml: %v", err)
	}
	var doc map[string]any
	if unmarshalErr := yaml.Unmarshal(embeddedAKS, &doc); unmarshalErr != nil {
		t.Fatalf("parse embedded aks overlay: %v", unmarshalErr)
	}
	doc["apiVersion"] = RecipeAPIVersion
	spec, ok := doc["spec"].(map[string]any)
	if !ok {
		t.Fatalf("embedded aks overlay spec = %T, want map", doc["spec"])
	}
	if _, declared := spec["profile"]; !declared {
		t.Fatal("embedded aks overlay no longer declares spec.profile; the shadow scenario needs updating")
	}
	delete(spec, "profile")
	legacyAKS, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("serialize legacy aks overlay: %v", err)
	}

	// Build the external --data directory the layered provider consumes:
	// registry.yaml is mandatory at the root (merged, not replaced); the
	// legacy overlay replaces the embedded file wholesale by path, and
	// every other catalog file (values, mixins, leaves) falls through to
	// the embedded layer.
	extDir := t.TempDir()
	registry := "apiVersion: aicr.run/v1alpha2\nkind: ComponentRegistry\ncomponents: []\n"
	if writeErr := os.WriteFile(filepath.Join(extDir, "registry.yaml"), []byte(registry), 0o600); writeErr != nil {
		t.Fatalf("write external registry.yaml: %v", writeErr)
	}
	if mkdirErr := os.MkdirAll(filepath.Join(extDir, "overlays"), 0o750); mkdirErr != nil {
		t.Fatalf("create external overlays dir: %v", mkdirErr)
	}
	if writeErr := os.WriteFile(filepath.Join(extDir, "overlays", "aks.yaml"), legacyAKS, 0o600); writeErr != nil {
		t.Fatalf("write external overlays/aks.yaml: %v", writeErr)
	}

	// The same layered provider construction the --data flag uses
	// (pkg/client/v1 buildDataProvider for a FilesystemSource).
	layered, err := NewLayeredDataProvider(
		NewEmbeddedDataProvider(GetEmbeddedFS(), "."),
		LayeredProviderConfig{ExternalDir: extDir},
	)
	if err != nil {
		t.Fatalf("construct layered provider: %v", err)
	}
	if source := layered.Source("overlays/aks.yaml"); source != CatalogSourceExternal {
		t.Fatalf("overlays/aks.yaml source = %q, want %q (shadow not in effect)",
			source, CatalogSourceExternal)
	}

	// Fresh store for the layered provider (not the cached singleton — the
	// temp directory is test-scoped and must not populate the global cache).
	store, err := buildMetadataStore(ctx, layered)
	if err != nil {
		t.Fatalf("build store over layered provider: %v", err)
	}
	aksOverlay, ok := store.Overlays["aks"]
	if !ok {
		t.Fatal("layered catalog is missing the aks overlay")
	}
	// Control: the external replacement actually loaded — the store's aks
	// overlay is the legacy, declaration-free variant, not the embedded one.
	if aksOverlay.Spec.Profile != nil {
		t.Fatalf("aks overlay still declares a profile (%#v); external shadow did not replace it",
			aksOverlay.Spec.Profile)
	}
	if aksOverlay.APIVersion != RecipeAPIVersion {
		t.Fatalf("aks overlay apiVersion = %q, want legacy %q", aksOverlay.APIVersion, RecipeAPIVersion)
	}

	result, err := store.BuildRecipeResult(ctx, aksCriteria())
	if err != nil {
		t.Fatalf("legacy-shadow resolution must succeed unprofiled, got error: %v", err)
	}

	if result.Metadata.SelectedProfile != nil {
		t.Fatalf("selectedProfile = %#v, want nil (unprofiled legacy shadow)",
			result.Metadata.SelectedProfile)
	}
	if result.APIVersion != RecipeAPIVersion {
		t.Fatalf("apiVersion = %q, want legacy %q", result.APIVersion, RecipeAPIVersion)
	}
	for _, c := range result.Constraints {
		if c.Name == "K8s.aks-gpu-pools.gpu-driver" {
			t.Fatalf("unexpected pool constraint on unprofiled resolution: %s=%s", c.Name, c.Value)
		}
	}
}
