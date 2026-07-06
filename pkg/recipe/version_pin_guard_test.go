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
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"testing"

	"github.com/NVIDIA/aicr/internal/versionpins"
)

// TestOverlayVersionPinsMatchRegistry guards the version-management model so
// the container-images BOM cannot silently advertise a chart version that no
// recipe installs. See issue #1424.
//
// Why this matters:
// The BOM (docs/user/container-images.md, rendered by tools/bom) reads each
// component's registry defaultVersion (recipes/registry.yaml). But at recipe
// resolution the registry default is only a FALLBACK: an overlay/mixin
// componentRef that sets `version` (Helm) or `tag` (Kustomize) overrides it
// (see ComponentRef.ApplyRegistryDefaults in metadata.go). So the BOM equals
// what recipes actually install ONLY when no overlay pin diverges from the
// registry default.
//
// The dangerous shape is a component whose SOLE consumer overlay pins a
// version different from the registry default: the registry default is then
// installed by zero recipes, yet the BOM advertises it. This is exactly the
// #1418 aws-efa bug (registry bumped v0.5.26 -> v0.5.29, but every EKS recipe
// still rendered v0.5.26, and nothing flagged it).
//
// The invariant enforced here: every overlay/mixin componentRefs version/tag
// MUST equal the component's registry defaultVersion/defaultTag, unless the
// divergence is explicitly declared in recipes/version-pin-exemptions.yaml
// with a reason. This makes the registry default the single source of truth:
// a component bump must update the registry default (which the BOM reads) and
// every overlay that pins it, or CI fails. Undeclared drift is a hard
// failure; declared divergences are, by definition, not silent.
//
// The exemption table is loaded (and statically validated — required fields,
// duplicates, pin != default) by internal/versionpins from the shared
// declarative file, so tools/bom can consume the same policy once #1611
// wires it into the BOM.
func TestOverlayVersionPinsMatchRegistry(t *testing.T) {
	ctx := context.Background()

	reg, err := GetComponentRegistry()
	if err != nil {
		t.Fatalf("GetComponentRegistry: %v", err)
	}
	store, err := loadMetadataStore(ctx)
	if err != nil {
		t.Fatalf("loadMetadataStore: %v", err)
	}

	// The test binary runs with the package directory as cwd, so the
	// repository's recipes/ tree is two levels up.
	exemptions, err := versionpins.Load(ctx, filepath.Join("..", "..", "recipes", versionpins.FileName))
	if err != nil {
		t.Fatalf("versionpins.Load: %v", err)
	}

	// Track which exemptions actually fire so a stale entry (e.g. after a pin
	// is re-aligned) fails the test instead of silently rotting. Static
	// validation (duplicates, empty fields, pin == default) already happened
	// in versionpins.Load.
	usedExemption := make(map[pinKey]bool, len(exemptions))
	exemptByKey := make(map[pinKey]versionpins.Exemption, len(exemptions))
	for _, e := range exemptions {
		exemptByKey[pinKey{source: e.Source, component: e.Component}] = e
	}

	checked := 0

	// checkRefs compares every pinned componentRef in one overlay/mixin source
	// against the registry default, honoring the exemption list.
	checkRefs := func(source string, refs []ComponentRef) {
		for i := range refs {
			ref := refs[i]

			// A componentRef pins its version via `version` (Helm) or `tag`
			// (Kustomize). Compare whichever is set against the matching
			// registry default; a ref that pins neither inherits the default
			// and cannot diverge.
			pin := ref.Version
			field := "version"
			if pin == "" && ref.Tag != "" {
				pin = ref.Tag
				field = "tag"
			}
			if pin == "" {
				continue
			}

			cfg := reg.Get(ref.Name)
			if cfg == nil {
				// Not a registry component (e.g. an in-tree kustomize
				// customization). The BOM does not render it from a registry
				// default, so it is out of scope for this guard.
				continue
			}

			def := cfg.Helm.DefaultVersion
			if field == "tag" {
				def = cfg.Kustomize.DefaultTag
			}
			if def == "" {
				// No registry default to diverge from. `make bom-pinning-check`
				// separately enforces that every Helm component is pinned.
				continue
			}

			checked++
			if pin == def {
				continue
			}

			k := pinKey{source: source, component: ref.Name}
			if e, ok := exemptByKey[k]; ok {
				usedExemption[k] = true
				// An exemption blesses ONE specific divergence, not the pair
				// forever. If either the pin or the registry default has moved
				// since the exemption was written, the documented justification
				// no longer describes reality — fail so the author re-reviews
				// (and re-cites) rather than letting a new divergence ride the
				// old exemption.
				if pin != e.ExpectedPin || def != e.ExpectedDefault {
					t.Errorf("out-of-date exemption entry for %s/%s: recipes/%s "+
						"blesses pin=%q vs default=%q, but the recipe now has %s=%q vs default=%q.\n"+
						"  Update the exemption's expectedPin/expectedDefault and re-justify the "+
						"divergence, or re-align the pin. See issue #1424.",
						source, ref.Name, versionpins.FileName, e.ExpectedPin, e.ExpectedDefault, field, pin, def)
					continue
				}
				t.Logf("exempted divergence: %s/%s pins %s=%q vs registry default %q — %s",
					source, ref.Name, field, pin, def, e.Reason)
				continue
			}

			t.Errorf("version drift: overlay/mixin %q pins %s.%s=%q but registry "+
				"defaultVersion=%q for component %q.\n"+
				"  The BOM (docs/user/container-images.md) renders the registry default, so it would\n"+
				"  advertise %q while this recipe installs %q. Re-align the pin to the registry default\n"+
				"  (or bump both together). If the divergence is intentional, add an entry to\n"+
				"  recipes/%s with a justification. See issue #1424.",
				source, ref.Name, field, pin, def, ref.Name, def, pin, versionpins.FileName)
		}
	}

	// base.yaml is held as store.Base, separate from the overlay map — and it
	// pins the largest share of components, so it must be checked explicitly.
	if store.Base != nil {
		checkRefs(baseRecipeName, store.Base.Spec.ComponentRefs)
	}
	for name, overlay := range store.Overlays {
		checkRefs(name, overlay.Spec.ComponentRefs)
	}
	for name, mixin := range store.Mixins {
		checkRefs(name, mixin.Spec.ComponentRefs)
	}

	// A stale exemption (pin since re-aligned) must fail so the list stays honest.
	var stale []string
	for k := range exemptByKey {
		if !usedExemption[k] {
			stale = append(stale, fmt.Sprintf("%s/%s", k.source, k.component))
		}
	}
	sort.Strings(stale)
	for _, s := range stale {
		t.Errorf("stale exemption entry %q in recipes/%s: the pin now matches the registry "+
			"default (or was removed). Delete the exemption.", s, versionpins.FileName)
	}

	// Sole-consumer enforcement: an exemption is only safe if the registry
	// default it diverges from is still installed by at least one real recipe.
	// Otherwise the BOM (which renders the default) advertises a version no
	// recipe installs — the exact fiction the guard exists to prevent, merely
	// made explicit rather than fixed. Resolve every overlay once and require
	// each exemption's registry default to appear, enabled, in some result.
	if len(exemptions) > 0 {
		assertExemptionDefaultsInstalled(ctx, t, store, reg, exemptions)
	}

	// A registry/overlay refactor that stops surfacing any pinned refs would
	// make this guard vacuous; fail loudly rather than pass on nothing.
	if checked == 0 {
		t.Fatal("no pinned componentRefs discovered — the version-pin guard would be vacuous; " +
			"verify loadMetadataStore and the recipes/overlays/ directory")
	}
	t.Logf("verified %d pinned componentRefs against registry defaults (%d declared exemptions)",
		checked, len(exemptions))
}

// assertExemptionDefaultsInstalled resolves every overlay with criteria and
// fails for any exemption whose component's registry default is not installed
// (enabled) by at least one resolved recipe. This enforces the documented
// "do NOT exempt a component whose only consumer diverges" policy: if the
// diverging overlay were the sole consumer, the registry default — which the
// BOM advertises — would be installed by zero recipes.
func assertExemptionDefaultsInstalled(
	ctx context.Context, t *testing.T, store *MetadataStore, reg *ComponentRegistry, exemptions []versionpins.Exemption,
) {

	t.Helper()

	// Resolve every overlay carrying criteria once; reuse across exemptions.
	var results []*RecipeResult
	for _, overlay := range store.Overlays {
		if overlay.Spec.Criteria == nil {
			continue
		}
		result, err := store.BuildRecipeResult(ctx, overlay.Spec.Criteria)
		if err != nil {
			t.Fatalf("BuildRecipeResult(%s): %v", overlay.Metadata.Name, err)
		}
		results = append(results, result)
	}
	if len(results) == 0 {
		t.Fatal("no overlays with criteria resolved — cannot verify exemption sole-consumer " +
			"policy; verify recipes/overlays/")
	}

	for _, e := range exemptions {
		cfg := reg.Get(e.Component)
		if cfg == nil {
			continue // unknown component is reported elsewhere
		}

		// Select the version field by the registry component's type. Comparing
		// against whichever of Version/Tag happens to match would let an
		// inactive field spoof a default installation (e.g. a Helm component
		// whose unused Tag coincidentally equals the default), so the active
		// field is chosen by type and the other is ignored.
		regType := cfg.GetType()
		kustomize := regType == ComponentTypeKustomize
		def := cfg.Helm.DefaultVersion
		if kustomize {
			def = cfg.Kustomize.DefaultTag
		}

		installed := false
		for _, r := range results {
			ref := r.GetComponentRef(e.Component)
			if ref == nil || !ref.IsEnabled() {
				continue
			}
			// Fail closed on refs whose deploy shape does not unambiguously
			// match the registry default's type. A leaf overlay can inherit
			// Type/Version from a Helm registry default while ALSO setting Tag
			// or Path (mergeComponentRef merges each field independently and no
			// resolver validation rejects the mix). The Helm/Helmfile/ArgoCD
			// deployers drop Type, and localformat classifies any ref with a
			// Tag or Path as Kustomize (see deployer/localformat.classify), so
			// such a ref actually installs Kustomize — it must NOT count as
			// installing the Helm default via its inactive inherited Version.
			// Require BOTH the declared Type and the deploy-classified type to
			// match the registry type before comparing the active field.
			deploysKustomize := ref.Tag != "" || ref.Path != ""
			if ref.Type != regType || deploysKustomize != kustomize {
				continue
			}
			if kustomize && ref.Path == "" {
				// localformat rejects a Kustomize deployment without a Path
				// (Path is required to build from), so such a ref never
				// actually installs the default — do not count it.
				continue
			}
			active := ref.Version
			if kustomize {
				active = ref.Tag
			}
			if active == def {
				installed = true
				break
			}
		}
		if !installed {
			t.Errorf("unsafe exemption entry for %s/%s in recipes/%s: the registry default %q is "+
				"installed by no resolved recipe, so the BOM advertises a version nothing installs.\n"+
				"  Either re-align the pin (delete the exemption) or move the registry default to a "+
				"version some recipe actually runs. See issue #1424.",
				e.Source, e.Component, versionpins.FileName, def)
		}
	}
}

// pinKey identifies a componentRef by the overlay/mixin that declares it and
// the component name, for exemption lookup.
type pinKey struct {
	source    string
	component string
}
