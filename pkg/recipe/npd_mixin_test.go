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
	"strings"
	"testing"
)

const npdMixin = "npd"

// npdComponent is the component the mixin introduces. Unlike the object-monitor
// and preflight mixins, which compose overrides onto already-chained
// nvsentinel, this one contributes a brand-new component name -- so it needs no
// mixinSafeOverridePaths entry and is not subject to the override allowlist.
const npdComponent = "node-problem-detector"

// TestMixinNPD_ContributesTheComponent pins the shape the mixin relies on: a
// name absent from every base chain, carrying a full componentRef. If some
// future overlay adds node-problem-detector to a base, this mixin silently
// becomes an override composition subject to the allowlist, and would start
// failing at compose time instead.
func TestMixinNPD_ContributesTheComponent(t *testing.T) {
	ctx, store := objectMonitorStore(t)
	if _, ok := store.Mixins[npdMixin]; !ok {
		t.Fatalf("%s mixin not present; check recipes/mixins/%s.yaml", npdMixin, npdMixin)
	}

	// Scan the shipped catalog, not the synthetic leaf below: the leaf is
	// built here and could never contain node-problem-detector, so asserting
	// against it would pass no matter what the real overlays declare.
	for _, ref := range store.Base.Spec.ComponentRefs {
		if ref.Name == npdComponent {
			t.Fatalf("recipes/overlays/base.yaml declares %s; this mixin would become an override "+
				"composition subject to mixinSafeOverridePaths", npdComponent)
		}
	}
	for name, overlay := range store.Overlays {
		if overlay == nil {
			continue
		}
		for _, ref := range overlay.Spec.ComponentRefs {
			if ref.Name == npdComponent {
				t.Fatalf("overlay %q declares %s; this mixin would become an override "+
					"composition subject to mixinSafeOverridePaths", name, npdComponent)
			}
		}
	}

	spec := nvsentinelLeaf([]string{npdMixin}, nil)
	if _, err := store.mergeMixins(ctx, &spec); err != nil {
		t.Fatalf("mergeMixins(%s): %v", npdMixin, err)
	}

	ref, ok := findComponentRefByName(spec.ComponentRefs, npdComponent)
	if !ok {
		t.Fatalf("%s missing from merged spec", npdComponent)
	}
	if ref.ValuesFile != "components/node-problem-detector/values.yaml" {
		t.Errorf("valuesFile = %q, want components/node-problem-detector/values.yaml", ref.ValuesFile)
	}
}

// TestNoShippedRecipeAdoptsNPDMixins keeps both halves of #2614 opt-in. The
// npd mixin changes a platform's component inventory and the object-monitor
// mixin turns on a controller; neither belongs in a recipe by default while
// the NPD policies are still STORE_ONLY and unvalidated on real hardware.
func TestNoShippedRecipeAdoptsNPDMixins(t *testing.T) {
	_, store := objectMonitorStore(t)

	for name, overlay := range store.Overlays {
		for _, mixin := range overlay.Spec.Mixins {
			if mixin == npdMixin || mixin == objectMonitorMixin {
				t.Errorf("overlay %q adopts mixin %q; both stay opt-in until the NPD policies are validated beyond STORE_ONLY", name, mixin)
			}
		}
	}
}

// TestNPDRegistryEntryIsPinnedAndGated guards the two things a third-party
// chart most needs: an exact version, and the gate that stops it being
// installed where the provider already runs one.
func TestNPDRegistryEntryIsPinnedAndGated(t *testing.T) {
	_, store := objectMonitorStore(t)

	registry, err := GetComponentRegistryFor(store.provider)
	if err != nil {
		t.Fatalf("GetComponentRegistryFor: %v", err)
	}
	component := registry.Get(npdComponent)
	if component == nil {
		t.Fatalf("%s missing from the registry", npdComponent)
	}
	if component.Helm.DefaultVersion == "" {
		t.Fatal("node-problem-detector has no pinned chart version; an unpinned chart fails recipe resolution")
	}
	if strings.Contains(component.Helm.DefaultVersion, "latest") {
		t.Errorf("chart version %q is not an exact pin", component.Helm.DefaultVersion)
	}

	var gated bool
	for _, v := range component.Validations {
		if v.Function == "CheckNPDNotDuplicatingProviderNPD" {
			gated = true
			if v.Severity != "error" {
				t.Errorf("CheckNPDNotDuplicatingProviderNPD severity = %q, want error -- two NPD instances fight silently", v.Severity)
			}
			// No conditions block, deliberately. `conditions` can only express
			// "run when the criteria match", which cannot say "permit only the
			// platforms we have verified". Re-adding one would silently narrow
			// the gate to those platforms and let every other one through,
			// which is the fail-open this allowlist exists to close.
			if len(v.Conditions) != 0 {
				t.Errorf("CheckNPDNotDuplicatingProviderNPD declares conditions %v; the platform allowlist belongs in the function, not the registry", v.Conditions)
			}
		}
	}
	if !gated {
		t.Error("node-problem-detector has no CheckNPDNotDuplicatingProviderNPD validation registered")
	}
}
