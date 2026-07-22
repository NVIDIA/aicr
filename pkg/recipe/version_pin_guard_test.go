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
	"sort"
	"strings"
	"testing"
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
// (see ComponentRef.ApplyRegistryDefaults in metadata.go). Declared
// divergent pins surface in the BOM's variants table (#1611), so a
// represented divergence is truthful; the untruthful shape is a default row
// left with no consumers.
//
// The dangerous shape is a component whose SOLE consumer overlay pins a
// version different from the registry default: the registry default is then
// installed by zero recipes, yet the BOM advertises it. This is exactly the
// #1418 aws-efa bug (registry bumped v0.5.26 -> v0.5.29, but every EKS recipe
// still rendered v0.5.26, and nothing flagged it).
//
// The invariant enforced here (per #1616): a base/overlay/mixin componentRef
// MUST NOT pin a version/tag at all, unless the pin is an intentional
// divergence explicitly declared in versionPinExemptions with a reason. This
// makes the registry default the single source of truth: a component bump
// updates the registry default (which the BOM reads) in one place, and an
// external --data registry that overrides defaultVersion takes effect for
// every overlay that does not intentionally diverge. A non-exempted pin fails
// whether it diverges (undeclared drift) or equals the default (redundant —
// it doubles bump churn and shields the overlay from external registry
// overrides); declared divergences are, by definition, not silent.
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

	// Track which exemptions actually fire so a stale entry (e.g. after a pin
	// is re-aligned) fails the test instead of silently rotting.
	usedExemption := make(map[pinKey]bool, len(versionPinExemptions))
	exemptByKey := make(map[pinKey]versionPinExemption, len(versionPinExemptions))
	for _, e := range versionPinExemptions {
		k := pinKey{source: e.source, component: e.component}
		if _, dup := exemptByKey[k]; dup {
			t.Errorf("duplicate versionPinExemptions entry for source=%q component=%q",
				e.source, e.component)
		}
		if e.reason == "" {
			t.Errorf("versionPinExemptions entry for source=%q component=%q has no reason",
				e.source, e.component)
		}
		if e.field == "tag" {
			// Fail closed until the BOM can represent a Kustomize variant:
			// tools/bom/variants.go derives the Version-variants table from
			// Helm version pins only, so an exempted divergent tag would pass
			// this guard AND the BOM freshness gate while staying invisible
			// in the committed variants table — the #1424 fiction with a
			// green CI. Extend the BOM variants pipeline to Kustomize tags
			// before accepting a tag exemption.
			t.Errorf("versionPinExemptions entry for source=%q component=%q binds field=\"tag\", "+
				"which is not supported yet: the BOM variants pipeline derives variants from "+
				"Helm version pins only, so this divergence would be invisible in "+
				"docs/user/container-images.md. Extend tools/bom/variants.go first.",
				e.source, e.component)
		} else if e.field != "version" {
			t.Errorf("versionPinExemptions entry for source=%q component=%q has field=%q, "+
				"want \"version\" (Helm) — the exemption must bind to the deployment type's "+
				"pin field so a type migration cannot reuse it",
				e.source, e.component, e.field)
		}
		if e.expectedPin == "" || e.expectedDefault == "" {
			t.Errorf("versionPinExemptions entry for source=%q component=%q must set both "+
				"expectedPin and expectedDefault so drift within the exemption is caught",
				e.source, e.component)
		}
		if e.expectedPin == e.expectedDefault {
			t.Errorf("versionPinExemptions entry for source=%q component=%q has expectedPin == "+
				"expectedDefault (%q); an exemption documents a DIVERGENCE — delete it instead",
				e.source, e.component, e.expectedPin)
		}
		exemptByKey[k] = e
	}

	checked := 0
	refsSeen := 0

	// checkRefs compares every pinned componentRef in one overlay/mixin source
	// against the registry default, honoring the exemption list.
	checkRefs := func(source string, refs []ComponentRef) {
		for i := range refs {
			ref := refs[i]
			refsSeen++

			cfg := reg.Get(ref.Name)
			if cfg == nil {
				// Not a registry component (e.g. an in-tree kustomize
				// customization). The BOM does not render it from a registry
				// default, so it is out of scope for this guard.
				continue
			}

			k := pinKey{source: source, component: ref.Name}
			// markExemptionUsed suppresses the stale-exemption sweep for refs
			// that fail closed below: the ref still declares its (broken)
			// divergence, so a second "delete the exemption" error would be
			// misleading — the fail-closed error is the actionable signal.
			markExemptionUsed := func() {
				if _, exempted := exemptByKey[k]; exempted {
					usedExemption[k] = true
				}
			}

			// An explicit ref.Type that contradicts the registry component's
			// type escapes both selectPin branches (such a ref pins neither
			// field the registry type implies), and resolution follows
			// ref.Type without inheriting the registry default — the recipe
			// would deploy one shape while the BOM advertises the other. Fail
			// closed; an empty Type inherits the registry type and is fine.
			if refTypeMismatch(ref, cfg) {
				t.Errorf("type mismatch: overlay/mixin %q declares component %q as %s but the "+
					"registry defines it as %s; align the ref's type with the registry entry "+
					"(or change the registry entry) so the version-pin guard and the BOM stay "+
					"truthful.",
					source, ref.Name, ref.Type, cfg.GetType())
				markExemptionUsed()
				continue
			}

			// Pin-field selection is driven by the REGISTRY component's type,
			// never by whichever ref field happens to be populated: a stray
			// tag on a Helm component (or version on a Kustomize one) would
			// otherwise select the other type's default — empty on a
			// well-formed registry entry — and silently skip the guard. Fail
			// closed on the stray field instead; resolution coherence rejects
			// the mixed shape for Helm refs, and the guard mirrors that at
			// declaration level.
			sel := selectPin(ref, cfg)
			if sel.stray != "" {
				t.Errorf("mixed-field pin: overlay/mixin %q sets %s=%q on %s component %q; "+
					"the %s field does not apply to this component type — remove it (a "+
					"mixed-field ref deploys ambiguously and cannot be checked against a "+
					"registry default).",
					source, sel.strayField, sel.stray, cfg.GetType(), ref.Name, sel.strayField)
				markExemptionUsed()
				continue
			}
			pin, field := sel.pin, sel.field
			if pin == "" {
				// A ref that pins nothing inherits the registry default and
				// cannot diverge.
				continue
			}
			def, defField := sel.def, sel.defField
			if def == "" {
				// No registry default to diverge from. `make bom-pinning-check`
				// separately enforces that every Helm component is pinned.
				continue
			}

			checked++
			if pin == def {
				t.Errorf("redundant pin: overlay/mixin %q pins %s.%s=%q, which equals the "+
					"registry %s for component %q. Remove the pin — resolution falls "+
					"back to the registry default, and a redundant pin doubles bump churn and "+
					"shields the overlay from external registry %s overrides. "+
					"See issue #1616.",
					source, ref.Name, field, pin, defField, ref.Name, defField)
				continue
			}

			if e, ok := exemptByKey[k]; ok {
				usedExemption[k] = true
				// An exemption blesses ONE specific divergence, not the pair
				// forever. If either the pin or the registry default has moved
				// since the exemption was written — or the component migrated
				// deployment types so the pin now lives on a different field —
				// the documented justification no longer describes reality:
				// fail so the author re-reviews (and re-cites) rather than
				// letting a new divergence ride the old exemption.
				if e.field != field {
					t.Errorf("out-of-date versionPinExemptions entry for %s/%s: exemption "+
						"declares field=%q but the recipe now pins %s=%q; a deployment-type "+
						"migration is a new divergence — re-review and re-justify it. See issue #1424.",
						source, ref.Name, e.field, field, pin)
					continue
				}
				if pin != e.expectedPin || def != e.expectedDefault {
					t.Errorf("out-of-date versionPinExemptions entry for %s/%s: exemption "+
						"blesses pin=%q vs default=%q, but the recipe now has %s=%q vs registry %s=%q.\n"+
						"  Update the exemption's expectedPin/expectedDefault and re-justify the "+
						"divergence, or re-align the pin. See issue #1424.",
						source, ref.Name, e.expectedPin, e.expectedDefault, field, pin, defField, def)
					continue
				}
				t.Logf("exempted divergence: %s/%s pins %s=%q vs registry default %q — %s",
					source, ref.Name, field, pin, def, e.reason)
				continue
			}

			t.Errorf("version drift: overlay/mixin %q pins %s.%s=%q but registry "+
				"%s=%q for component %q.\n"+
				"  The BOM (docs/user/container-images.md) renders the registry default, so it would\n"+
				"  advertise %q while this recipe installs %q. Remove the pin (the registry default\n"+
				"  applies at resolution) or bump the registry default instead. If the divergence is\n"+
				"  intentional, add an entry to versionPinExemptions in version_pin_guard_test.go\n"+
				"  with a justification. See issues #1424 and #1616.",
				source, ref.Name, field, pin, defField, def, ref.Name, def, pin)
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
		t.Errorf("stale versionPinExemptions entry %q: the pin now matches the registry "+
			"default (or was removed). Delete the exemption.", s)
	}

	// Sole-consumer enforcement: an exemption is only safe if the registry
	// default it diverges from is still installed by at least one real recipe.
	// Otherwise the BOM (which renders the default) advertises a version no
	// recipe installs — the exact fiction the guard exists to prevent, merely
	// made explicit rather than fixed. Resolve every overlay once and require
	// each exemption's registry default to appear, enabled, in some result.
	if len(versionPinExemptions) > 0 {
		assertExemptionDefaultsInstalled(ctx, t, store, reg)
	}

	// Zero PINNED refs is the healthy steady state after #1616 (pins exist
	// only for exempted divergences), so the vacuity sentinel is the ref
	// count: a registry/overlay refactor that stops surfacing componentRefs
	// entirely would make this guard vacuous — fail loudly rather than pass
	// on nothing.
	if refsSeen == 0 {
		t.Fatal("no componentRefs discovered — the version-pin guard would be vacuous; " +
			"verify loadMetadataStore and the recipes/overlays/ directory")
	}
	t.Logf("verified %d pinned componentRefs against registry defaults (%d declared exemptions)",
		checked, len(versionPinExemptions))
}

// assertExemptionDefaultsInstalled resolves every overlay with criteria and
// fails for any exemption whose component's registry default is not installed
// (enabled) by at least one resolved recipe. This enforces the documented
// "do NOT exempt a component whose only consumer diverges" policy: if the
// diverging overlay were the sole consumer, the registry default — which the
// BOM advertises — would be installed by zero recipes.
func assertExemptionDefaultsInstalled(ctx context.Context, t *testing.T, store *MetadataStore, reg *ComponentRegistry) {
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

	for _, e := range versionPinExemptions {
		cfg := reg.Get(e.component)
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
			ref := r.GetComponentRef(e.component)
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
			t.Errorf("unsafe versionPinExemptions entry for %s/%s: the registry default %q is "+
				"installed by no resolved recipe, so the BOM advertises a version nothing installs.\n"+
				"  Either re-align the pin (delete the exemption) or move the registry default to a "+
				"version some recipe actually runs. See issue #1424.",
				e.source, e.component, def)
		}
	}
}

// pinKey identifies a componentRef by the overlay/mixin that declares it and
// the component name, for exemption lookup.
type pinKey struct {
	source    string
	component string
}

// pinSelection maps a declared componentRef to the pin field and registry
// default the guard compares, selected by the registry component's type. The
// other type's field, if set, lands in stray/strayField so the caller can
// fail closed on the mixed shape instead of comparing against an empty
// default and silently skipping.
type pinSelection struct {
	pin, field        string // the type-matched declared pin ("" when unpinned)
	def, defField     string // the matching registry default and its field name
	stray, strayField string // a pin declared on the other type's field, if any
}

// refTypeMismatch reports whether a ref explicitly declares a deployment
// type different from its registry component's type (case-insensitively,
// matching coherenceProblem's treatment). An empty ref.Type inherits the
// registry type and never mismatches.
func refTypeMismatch(ref ComponentRef, cfg *ComponentConfig) bool {
	return ref.Type != "" && !strings.EqualFold(string(ref.Type), string(cfg.GetType()))
}

// TestRefTypeMismatch pins the mismatch contract the guard fails closed on.
func TestRefTypeMismatch(t *testing.T) {
	helmCfg := &ComponentConfig{
		Name: "helm-comp",
		Helm: HelmConfig{DefaultRepository: "https://charts.example.com", DefaultVersion: "1.2.3"},
	}
	kustomizeCfg := &ComponentConfig{
		Name:      "kustomize-comp",
		Kustomize: KustomizeConfig{DefaultSource: "https://github.com/example/app", DefaultTag: "v9"},
	}

	tests := []struct {
		name string
		ref  ComponentRef
		cfg  *ComponentConfig
		want bool
	}{
		{"empty type inherits", ComponentRef{Name: "helm-comp"}, helmCfg, false},
		{"matching helm type", ComponentRef{Name: "helm-comp", Type: ComponentTypeHelm}, helmCfg, false},
		{"matching helm type lowercase", ComponentRef{Name: "helm-comp", Type: "helm"}, helmCfg, false},
		{"kustomize ref on helm component", ComponentRef{Name: "helm-comp", Type: ComponentTypeKustomize}, helmCfg, true},
		{"helm ref on kustomize component", ComponentRef{Name: "kustomize-comp", Type: ComponentTypeHelm}, kustomizeCfg, true},
		{"matching kustomize type", ComponentRef{Name: "kustomize-comp", Type: ComponentTypeKustomize}, kustomizeCfg, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := refTypeMismatch(tt.ref, tt.cfg); got != tt.want {
				t.Errorf("refTypeMismatch() = %v, want %v", got, tt.want)
			}
		})
	}
}

// selectPin performs the type-driven selection for one ref against its
// registry config.
func selectPin(ref ComponentRef, cfg *ComponentConfig) pinSelection {
	if cfg.GetType() == ComponentTypeKustomize {
		return pinSelection{
			pin: ref.Tag, field: "tag",
			def: cfg.Kustomize.DefaultTag, defField: "kustomize.defaultTag",
			stray: ref.Version, strayField: "version",
		}
	}
	return pinSelection{
		pin: ref.Version, field: "version",
		def: cfg.Helm.DefaultVersion, defField: "helm.defaultVersion",
		stray: ref.Tag, strayField: "tag",
	}
}

// TestSelectPin pins the type-driven selection contract, including the
// mixed-field stray detection that must fail closed in the guard.
func TestSelectPin(t *testing.T) {
	helmCfg := &ComponentConfig{
		Name: "helm-comp",
		Helm: HelmConfig{DefaultRepository: "https://charts.example.com", DefaultVersion: "1.2.3"},
	}
	kustomizeCfg := &ComponentConfig{
		Name:      "kustomize-comp",
		Kustomize: KustomizeConfig{DefaultSource: "https://github.com/example/app", DefaultTag: "v9"},
	}

	tests := []struct {
		name string
		ref  ComponentRef
		cfg  *ComponentConfig
		want pinSelection
	}{
		{
			name: "helm version pin",
			ref:  ComponentRef{Name: "helm-comp", Version: "1.2.3"},
			cfg:  helmCfg,
			want: pinSelection{pin: "1.2.3", field: "version", def: "1.2.3", defField: "helm.defaultVersion", strayField: "tag"},
		},
		{
			name: "helm unpinned",
			ref:  ComponentRef{Name: "helm-comp"},
			cfg:  helmCfg,
			want: pinSelection{field: "version", def: "1.2.3", defField: "helm.defaultVersion", strayField: "tag"},
		},
		{
			name: "helm with stray tag fails closed",
			ref:  ComponentRef{Name: "helm-comp", Tag: "v9"},
			cfg:  helmCfg,
			want: pinSelection{field: "version", def: "1.2.3", defField: "helm.defaultVersion", stray: "v9", strayField: "tag"},
		},
		{
			name: "kustomize tag pin",
			ref:  ComponentRef{Name: "kustomize-comp", Tag: "v9"},
			cfg:  kustomizeCfg,
			want: pinSelection{pin: "v9", field: "tag", def: "v9", defField: "kustomize.defaultTag", strayField: "version"},
		},
		{
			name: "kustomize with stray version fails closed",
			ref:  ComponentRef{Name: "kustomize-comp", Version: "1.2.3", Tag: "v9"},
			cfg:  kustomizeCfg,
			want: pinSelection{pin: "v9", field: "tag", def: "v9", defField: "kustomize.defaultTag", stray: "1.2.3", strayField: "version"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectPin(tt.ref, tt.cfg); got != tt.want {
				t.Errorf("selectPin() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// versionPinExemption documents a componentRef whose overlay/mixin
// version/tag pin is INTENTIONALLY different from the component's registry
// default (helm.defaultVersion or kustomize.defaultTag, per field).
//
// Add an entry ONLY when an overlay must legitimately run a different chart
// version than the registry default (e.g. a platform validated against an
// older chart). Do NOT exempt a component whose only consumer diverges — that
// leaves the registry default (and therefore the BOM) advertising a version no
// recipe installs, the precise failure this guard exists to prevent.
//
// expectedPin/expectedDefault bind the exemption to ONE specific divergence:
// if either the recipe's pin or the registry default later moves, the guard
// fails so the divergence is re-reviewed and re-justified rather than a new,
// unvetted version silently inheriting the old blessing.
type versionPinExemption struct {
	source          string // overlay/mixin metadata.name that declares the pin
	component       string // componentRef name
	field           string // pin field this exemption binds to: "version" (Helm). "tag" is rejected until the BOM variants pipeline supports Kustomize tags.
	expectedPin     string // the exact divergent version/tag this exemption blesses
	expectedDefault string // the registry default at the time the exemption was written
	reason          string // why the divergence is intentional (cite an issue/PR)
}

var versionPinExemptions = []versionPinExemption{
	{
		source:          "aks",
		component:       "kube-prometheus-stack",
		field:           "version",
		expectedPin:     "83.7.0",
		expectedDefault: "84.4.0",
		reason: "AKS is pinned to chart 83.7.0 to match its validated working cluster " +
			"state (#700); the registry default (84.4.0) tracks the base/EKS/GKE line and " +
			"is installed by every non-AKS recipe, so the BOM's default is not fictional.",
	},
}
