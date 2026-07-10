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

package v1

import (
	"context"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestStockCatalogResolvesDevicePluginPolicy is the production-default flip
// guard (issue #1327): EVERY resolvable overlay in the embedded stock catalog
// must resolve GPUAllocationPolicy == device-plugin-extended-resource with no
// error. Because dual advertisement (row 3) and the inert chart-guard waiver
// (row 5) are ErrCodeInvalidRequest at resolution time, a clean device-plugin
// resolution here also proves no stock recipe silently reintroduces either
// state — e.g. an overlay re-adding gpuResourcesEnabledOverride: true, or a
// values file flipping resources.gpus.enabled back on without the full
// three-part DRA opt-in.
//
// This test lives in pkg/validator/v1 (not pkg/recipe) because it exercises
// ResolveGPUAllocationPolicy itself — the same code path aicr validate uses —
// against the real embedded catalog; pkg/recipe cannot import this package.
//
// Guard scope: this is a HYDRATED-TUPLE acceptance sweep (the values the
// resolver and Helm both consume), not a rendered-manifest regression test —
// chart rendering is exercised by the bundler/e2e suites. Beyond the derived
// policy it asserts the EXPLICIT production pins are present (not merely
// implied by resolver fallbacks): gpuResourcesEnabledOverride explicitly
// false, the selected operator's devicePlugin.enabled explicitly true, and
// computeDomains.enabled true — with nonzero counters so the pin assertions
// cannot pass vacuously.
//
// Discovery mirrors TestDriverRootLockstep in pkg/recipe: iterate every
// overlay with non-nil Spec.Criteria and build the recipe those criteria
// resolve to. Production resolution is per-query (FindMatchingOverlays →
// filterToMaximalLeaves), so intermediate overlays that are directly
// resolvable for a criteria shape are covered too, not just leaves.
func TestStockCatalogResolvesDevicePluginPolicy(t *testing.T) {
	ctx := context.Background()
	store, err := recipe.LoadMetadataStoreFor(ctx, nil)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}

	overlayCount := 0
	draPinChecked := 0
	opPinChecked := 0
	for name, overlay := range store.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		overlayCount++

		t.Run(name, func(t *testing.T) {
			result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
			if err != nil {
				t.Fatalf("BuildRecipeResult: %v", err)
			}

			policy, err := ResolveGPUAllocationPolicy(ctx, result)
			if err != nil {
				t.Fatalf("ResolveGPUAllocationPolicy: %v\n"+
					"  Every stock recipe must resolve the device-plugin production default.\n"+
					"  A dual-advertisement or inert-waiver error here means this overlay (or a\n"+
					"  values file it pulls in) reintroduced a rejected #1327 configuration state.",
					err)
			}
			if policy != GPUAllocationPolicyDevicePluginExtendedResource {
				t.Errorf("policy = %q, want %q\n"+
					"  The stock catalog ships device-plugin whole-GPU allocation only; the DRA\n"+
					"  opt-in (dra-resource-claim) is an explicit, non-stock overlay decision\n"+
					"  (issue #1327).",
					policy, GPUAllocationPolicyDevicePluginExtendedResource)
			}

			// Explicit-pin guards: the policy assertion above is satisfied
			// by resolver FALLBACKS too (absent override → false, absent
			// devicePlugin.enabled → true), so deleting the explicit pins
			// would pass silently. The production tuple must be explicit
			// in the hydrated values.
			if draRef := enabledComponentRef(result, draDriverGPUComponentName); draRef != nil {
				values, err := result.GetValuesForComponentWithContext(ctx, draRef.Name)
				if err != nil {
					t.Fatalf("GetValuesForComponentWithContext(%s): %v", draRef.Name, err)
				}
				waiver, waiverSet, err := lookupBoolValueFound(values, draRef.Name, valuePathGPUsEnabledOverride)
				if err != nil {
					t.Fatalf("%s lookup: %v", valuePathGPUsEnabledOverride, err)
				}
				if !waiverSet || waiver {
					t.Errorf("%s: want explicitly false (present=%t value=%t)\n"+
						"  The armed-tripwire pin must be EXPLICIT in stock hydrated values, not a\n"+
						"  resolver fallback (issue #1327).",
						valuePathGPUsEnabledOverride, waiverSet, waiver)
				}
				// ComputeDomain guard: load-bearing for GB200/MNNVL IMEX —
				// a values change flipping it off would otherwise pass the
				// policy sweep silently.
				cdEnabled, err := lookupBoolValue(values, draRef.Name, valuePathComputeDomainsEnabled, false)
				if err != nil {
					t.Fatalf("computeDomains.enabled lookup: %v", err)
				}
				if !cdEnabled {
					t.Errorf("resources.computeDomains.enabled = false for enabled %s\n"+
						"  ComputeDomain/IMEX DRA is load-bearing for GB200/MNNVL platforms and must\n"+
						"  stay pinned true in the stock catalog (issue #1327).",
						draRef.Name)
				}
				draPinChecked++
			}
			opRef := enabledComponentRef(result, gpuOperatorComponentName)
			if opRef == nil {
				opRef = enabledComponentRef(result, gpuOperatorOCPComponentName)
			}
			if opRef != nil {
				values, err := result.GetValuesForComponentWithContext(ctx, opRef.Name)
				if err != nil {
					t.Fatalf("GetValuesForComponentWithContext(%s): %v", opRef.Name, err)
				}
				dpEnabled, dpSet, err := lookupBoolValueFound(values, opRef.Name, valuePathDevicePluginEnabled)
				if err != nil {
					t.Fatalf("%s lookup: %v", valuePathDevicePluginEnabled, err)
				}
				if !dpSet || !dpEnabled {
					t.Errorf("%s %s: want explicitly true (present=%t value=%t)\n"+
						"  The advertiser pin must be EXPLICIT in stock hydrated values, not the\n"+
						"  chart-default fallback (issue #1327).",
						opRef.Name, valuePathDevicePluginEnabled, dpSet, dpEnabled)
				}
				opPinChecked++
			}
		})
	}
	// The pin assertions must not pass vacuously: the stock catalog carries
	// enabled DRA and operator components on many recipes.
	if draPinChecked == 0 {
		t.Error("no overlay exercised the DRA explicit-pin assertions — sweep is vacuous")
	}
	if opPinChecked == 0 {
		t.Error("no overlay exercised the operator explicit-pin assertions — sweep is vacuous")
	}

	if overlayCount == 0 {
		t.Fatal("no overlays with criteria discovered — the sweep would be vacuous; " +
			"verify the recipes/overlays/ directory")
	}
	t.Logf("verified device-plugin allocation policy across %d stock overlays", overlayCount)
}
