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
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	stdsort "sort"
	"testing"
	"testing/fstest"

	"gopkg.in/yaml.v3"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/upgrade"
)

// upgradeBaseRecipesEnv names a directory holding the recipes tree of the
// change's merge base. tools/check-upgrade-records extracts it and sets this;
// without it TestChangedPinsHaveUpgradeRecords has nothing to compare against.
const upgradeBaseRecipesEnv = "AICR_UPGRADE_BASE_RECIPES"

// registryPinSource is the versionPin source of a registry default.
const registryPinSource = "registry.yaml"

// The ADR-021 Decision 10 coverage gate is two rules, and neither suffices
// alone. No record assesses an upgrade into a pin outside every record's `to`
// range: upgrade-check reports unknown, or blocked past a record's highest
// ceiling, and either reads the same as "nobody has assessed this".
//
//   - TestRecordedComponentsCoverTheirPins: a record must cover every pin it
//     governs. Hermetic, so it runs under `make test`.
//   - TestChangedPinsHaveUpgradeRecords: against the merge base, a pin no
//     record governs may not move, and a record may not disappear while its
//     component remains. A deleted record moves no pin, so only a comparison
//     with the base can see it.
//
// A record governs a pin unless it is ahead of it (upgrade.AheadOf): guidance
// written before the bump it describes speaks for no version yet, so that pin
// is treated as recordless until it reaches the record.
// Components that predate records are not gated until a pin moves, and nobody
// upgrades into a component's first pin, so a component new to the registry
// needs no record. Neither rule asserts a verdict.

// TestRecordedComponentsCoverTheirPins is the hermetic half of the coverage
// gate.
func TestRecordedComponentsCoverTheirPins(t *testing.T) {
	view, set, err := loadCoverageView(t.Context())
	if err != nil {
		t.Fatalf("loading recipes: %v", err)
	}
	if len(view.pins) == 0 {
		t.Fatal("no pinned component versions found; the coverage gate would be vacuous, " +
			"so verify recipes/registry.yaml and loadMetadataStore")
	}
	for _, v := range recordedPinViolations(set, view.pins) {
		t.Error(v)
	}

	governed, covered := 0, 0
	for _, p := range view.pins {
		if governs(set[p.component], p.version) {
			governed++
		}
		if set[p.component].Covers(p.version) {
			covered++
		}
	}
	t.Logf("%d of %d pin(s) governed by a transition record; %d covered", governed, len(view.pins), covered)
}

// TestChangedPinsHaveUpgradeRecords is the merge-base half of the coverage
// gate. It skips without upgradeBaseRecipesEnv; tools/check-upgrade-records
// sets it and fails on anything but a PASS.
func TestChangedPinsHaveUpgradeRecords(t *testing.T) {
	baseDir := os.Getenv(upgradeBaseRecipesEnv)
	if baseDir == "" {
		t.Skipf("%s is not set; run tools/check-upgrade-records to compare against the merge base",
			upgradeBaseRecipesEnv)
	}

	base, err := loadBaseCoverageView(os.DirFS(baseDir))
	if err != nil {
		t.Fatalf("reading the merge base's recipes from %s: %v", baseDir, err)
	}
	if len(base.components) == 0 {
		t.Fatalf("the merge base's registry in %s lists no components; the comparison would be vacuous", baseDir)
	}
	cur, set, err := loadCoverageView(t.Context())
	if err != nil {
		t.Fatalf("loading recipes: %v", err)
	}

	for _, v := range changedPinViolations(base, cur, set) {
		t.Error(v)
	}
	t.Logf("compared %d pin(s) and %d record(s) against the merge base's %d pin(s) and %d record(s)",
		len(cur.pins), len(cur.recorded), len(base.pins), len(base.recorded))
}

// TestBaseCoverageViewMatchesEmbedded keeps the merge base's tolerant reader
// honest without a merge base: over this tree it must see exactly what the
// strict loaders see, and so report no move against it.
func TestBaseCoverageViewMatchesEmbedded(t *testing.T) {
	base, err := loadBaseCoverageView(os.DirFS(filepath.Join("..", "..", "recipes")))
	if err != nil {
		t.Fatalf("loadBaseCoverageView: %v", err)
	}
	cur, set, err := loadCoverageView(t.Context())
	if err != nil {
		t.Fatalf("loading recipes: %v", err)
	}
	if !reflect.DeepEqual(base, cur) {
		t.Errorf("the tolerant reader disagrees with the strict loaders:\n  tolerant %+v\n  strict   %+v", base, cur)
	}
	if v := changedPinViolations(base, cur, set); len(v) > 0 {
		t.Errorf("a tree compared with itself reports %d move(s): %v", len(v), v)
	}
}

// coverageView is what the coverage gate reads from one recipes tree.
// sources names every recipe a componentRef pin could come from, pinned or
// not, so an override dropped from a surviving recipe can be told apart from
// a recipe deleted outright.
type coverageView struct {
	components map[string]bool
	recorded   map[string]bool
	sources    map[string]bool
	pins       []versionPin
}

// loadCoverageView reads the embedded recipes through the strict loaders the
// binary uses, and returns the records with it.
func loadCoverageView(ctx context.Context) (coverageView, upgrade.Set, error) {
	reg, err := GetComponentRegistry()
	if err != nil {
		return coverageView{}, nil, err
	}
	store, err := loadMetadataStore(ctx)
	if err != nil {
		return coverageView{}, nil, err
	}
	set, _, err := LoadUpgradeRecords(ctx, defaultEmbeddedProvider)
	if err != nil {
		return coverageView{}, nil, err
	}
	return newCoverageView(reg, store), set, nil
}

// loadBaseCoverageView reads the merge base's recipes tolerantly: only the
// fields the gate compares, unknown fields ignored, nothing validated. The
// head's strict loaders would fail on a base that predates a schema change
// the same change migrates its data for, which is not a coverage question.
// Records are not loaded either; the base only has to say which exist.
func loadBaseCoverageView(fsys fs.FS) (coverageView, error) {
	var regDoc struct {
		Components []struct {
			Name string `yaml:"name"`
			Helm struct {
				DefaultVersion string `yaml:"defaultVersion"`
			} `yaml:"helm"`
			Kustomize struct {
				DefaultSource string `yaml:"defaultSource"`
				DefaultTag    string `yaml:"defaultTag"`
			} `yaml:"kustomize"`
			Upgrades struct {
				File string `yaml:"file"`
			} `yaml:"upgrades"`
		} `yaml:"components"`
	}
	if err := decodeTolerant(fsys, registryPinSource, &regDoc); err != nil {
		return coverageView{}, err
	}
	reg := &ComponentRegistry{byName: make(map[string]*ComponentConfig, len(regDoc.Components))}
	for _, c := range regDoc.Components {
		reg.Components = append(reg.Components, ComponentConfig{
			Name:      c.Name,
			Helm:      HelmConfig{DefaultVersion: c.Helm.DefaultVersion},
			Kustomize: KustomizeConfig{DefaultSource: c.Kustomize.DefaultSource, DefaultTag: c.Kustomize.DefaultTag},
			Upgrades:  UpgradesConfig{File: c.Upgrades.File},
		})
	}
	for i := range reg.Components {
		reg.byName[reg.Components[i].Name] = &reg.Components[i]
	}

	store := &MetadataStore{Overlays: map[string]*RecipeMetadata{}, Mixins: map[string]*RecipeMixin{}}
	overlays, err := readRecipeDocs(fsys, "overlays", "RecipeMetadata")
	if err != nil {
		return coverageView{}, err
	}
	for file, doc := range overlays {
		m := &RecipeMetadata{Spec: RecipeMetadataSpec{ComponentRefs: doc.refs()}}
		m.Metadata.Name = doc.Metadata.Name
		if file == "overlays/base.yaml" {
			store.Base = m
			continue
		}
		store.Overlays[doc.Metadata.Name] = m
	}
	mixins, err := readRecipeDocs(fsys, "mixins", "RecipeMixin")
	if err != nil {
		return coverageView{}, err
	}
	for _, doc := range mixins {
		m := &RecipeMixin{}
		m.Metadata.Name = doc.Metadata.Name
		m.Spec.ComponentRefs = doc.refs()
		store.Mixins[doc.Metadata.Name] = m
	}
	return newCoverageView(reg, store), nil
}

// tolerantRecipeDoc is the part of an overlay or mixin the gate compares.
type tolerantRecipeDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		ComponentRefs []struct {
			Name    string `yaml:"name"`
			Type    string `yaml:"type"`
			Version string `yaml:"version"`
			Tag     string `yaml:"tag"`
		} `yaml:"componentRefs"`
	} `yaml:"spec"`
}

func (d *tolerantRecipeDoc) refs() []ComponentRef {
	refs := make([]ComponentRef, 0, len(d.Spec.ComponentRefs))
	for _, r := range d.Spec.ComponentRefs {
		refs = append(refs, ComponentRef{Name: r.Name, Type: ComponentType(r.Type), Version: r.Version, Tag: r.Tag})
	}
	return refs
}

// readRecipeDocs decodes every dir/*.yaml of the given kind, keyed by path.
// Other kinds are skipped, as the strict loader skips them.
func readRecipeDocs(fsys fs.FS, dir, kind string) (map[string]*tolerantRecipeDoc, error) {
	files, err := fs.Glob(fsys, path.Join(dir, "*.yaml"))
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "listing "+dir, err)
	}
	docs := make(map[string]*tolerantRecipeDoc, len(files))
	for _, f := range files {
		doc := &tolerantRecipeDoc{}
		if err := decodeTolerant(fsys, f, doc); err != nil {
			return nil, err
		}
		if doc.Kind == kind {
			docs[f] = doc
		}
	}
	return docs, nil
}

func decodeTolerant(fsys fs.FS, name string, out any) error {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeNotFound, "reading "+name, err)
	}
	if err := yaml.Unmarshal(data, out); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "decoding "+name, err)
	}
	return nil
}

func newCoverageView(reg *ComponentRegistry, store *MetadataStore) coverageView {
	v := coverageView{
		components: make(map[string]bool, len(reg.Components)),
		recorded:   make(map[string]bool),
		sources:    make(map[string]bool, len(store.Overlays)+len(store.Mixins)+1),
		pins:       collectVersionPins(reg, store),
	}
	for i := range reg.Components {
		c := &reg.Components[i]
		v.components[c.Name] = true
		if c.Upgrades.File != "" {
			v.recorded[c.Name] = true
		}
	}
	if store.Base != nil {
		v.sources[baseSource()] = true
	}
	for name := range store.Overlays {
		v.sources[overlaySource(name)] = true
	}
	for name := range store.Mixins {
		v.sources[mixinSource(name)] = true
	}
	return v
}

func baseSource() string               { return "base " + baseRecipeName }
func overlaySource(name string) string { return "overlay " + name }
func mixinSource(name string) string   { return "mixin " + name }

// versionPin is one place a component's version is pinned: its registry
// default, or a base, overlay, or mixin componentRef that overrides it.
type versionPin struct {
	component string
	version   string
	source    string
}

// collectVersionPins returns every pin the coverage gate checks, sorted by
// component then source. An overlay override is a version some recipe really
// installs, so leaving it out is a hole in the gate.
//
// A ref whose type contradicts its registry entry, or that pins the other
// type's field, is skipped rather than guessed at;
// TestOverlayVersionPinsMatchRegistry fails on both shapes.
func collectVersionPins(reg *ComponentRegistry, store *MetadataStore) []versionPin {
	var pins []versionPin
	for i := range reg.Components {
		c := &reg.Components[i]
		if v := pinnedVersionFor(c); v != "" {
			pins = append(pins, versionPin{component: c.Name, version: v, source: registryPinSource})
		}
	}
	addRefs := func(source string, refs []ComponentRef) {
		for i := range refs {
			ref := refs[i]
			cfg := reg.Get(ref.Name)
			if cfg == nil || refTypeMismatch(ref, cfg) {
				continue
			}
			if sel := selectPin(ref, cfg); sel.pin != "" {
				pins = append(pins, versionPin{component: ref.Name, version: sel.pin, source: source})
			}
		}
	}
	if store.Base != nil {
		addRefs(baseSource(), store.Base.Spec.ComponentRefs)
	}
	for name, overlay := range store.Overlays {
		addRefs(overlaySource(name), overlay.Spec.ComponentRefs)
	}
	for name, mixin := range store.Mixins {
		addRefs(mixinSource(name), mixin.Spec.ComponentRefs)
	}
	stdsort.Slice(pins, func(i, j int) bool {
		if pins[i].component != pins[j].component {
			return pins[i].component < pins[j].component
		}
		return pins[i].source < pins[j].source
	})
	return pins
}

// governs reports whether u speaks for version: it has transitions and is not
// ahead of it. A record without transitions, such as one carrying only a
// replaces block, describes no version, which is how the matcher reads it too.
func governs(u *upgrade.ComponentUpgrades, version string) bool {
	return u != nil && len(u.Transitions) > 0 && !u.AheadOf(version)
}

// recordedPinViolations reports every pin a record governs but does not
// cover. Pins no record governs are left to changedPinViolations.
func recordedPinViolations(set upgrade.Set, pins []versionPin) []string {
	var violations []string
	for _, p := range pins {
		u := set[p.component]
		if !governs(u, p.version) || u.Covers(p.version) {
			continue
		}
		violations = append(violations, fmt.Sprintf(
			"component %q is pinned at %s in %s, but no transition in its record has a `to` range covering it, "+
				"so no record assesses an upgrade into it (upgrade-check reports unknown, or blocked past the "+
				"record's highest ceiling); extend the record (docs/contributor/upgrade-records.md)",
			p.component, p.version, p.source))
	}
	return violations
}

// pinMoves returns every version cur pins that the merge base did not pin from
// the same source. A source pinning the component at both is compared
// directly. A surviving base, overlay, or mixin that adds an override is
// compared against the registry default it inherited at the base, and one that
// drops its override against cur's registry default. A source new to cur moves
// only to a version the base pinned nowhere, so renaming an overlay is not a
// bump.
// Sources are compared as written, not as resolved: what an overlay inherits
// through spec.base or spec.mixins is not followed.
func pinMoves(base, cur coverageView) []versionPin {
	type sourcePin struct{ component, source string }
	type componentVersion struct{ component, version string }
	baseAt := make(map[sourcePin]string, len(base.pins))
	basePinned := make(map[componentVersion]bool, len(base.pins))
	baseDefaults := make(map[string]string)
	for _, p := range base.pins {
		baseAt[sourcePin{p.component, p.source}] = p.version
		basePinned[componentVersion{p.component, p.version}] = true
		if p.source == registryPinSource {
			baseDefaults[p.component] = p.version
		}
	}

	var moves []versionPin
	curAt := make(map[sourcePin]bool, len(cur.pins))
	defaults := make(map[string]string)
	for _, p := range cur.pins {
		curAt[sourcePin{p.component, p.source}] = true
		if p.source == registryPinSource {
			defaults[p.component] = p.version
		}
		if prev, ok := baseAt[sourcePin{p.component, p.source}]; ok {
			if prev != p.version {
				moves = append(moves, p)
			}
			continue
		}
		if base.sources[p.source] {
			if p.version != baseDefaults[p.component] {
				moves = append(moves, p)
			}
			continue
		}
		if !basePinned[componentVersion{p.component, p.version}] {
			moves = append(moves, p)
		}
	}
	for _, p := range base.pins {
		if p.source == registryPinSource || !cur.sources[p.source] || curAt[sourcePin{p.component, p.source}] {
			continue
		}
		if d := defaults[p.component]; d != "" && d != p.version {
			moves = append(moves, versionPin{
				component: p.component,
				version:   d,
				source:    p.source + " (its override of " + p.version + " was removed, so it falls back to the registry default)",
			})
		}
	}
	return moves
}

// changedPinViolations compares cur against the merge base: a move no record
// governs fails unless the component is new, and a record that existed on the
// base fails when it is gone while its component remains. A move a record
// governs is recordedPinViolations' to judge.
func changedPinViolations(base, cur coverageView, set upgrade.Set) []string {
	type componentVersion struct{ component, version string }
	var violations []string
	reported := make(map[componentVersion]bool)
	for _, m := range pinMoves(base, cur) {
		k := componentVersion{m.component, m.version}
		if !base.components[m.component] || governs(set[m.component], m.version) || reported[k] {
			continue
		}
		reported[k] = true
		gap := "has no transition record"
		switch u := set[m.component]; {
		case u != nil && len(u.Transitions) == 0:
			gap = "has a record with no transitions"
		case u != nil:
			gap = "has a transition record that describes only later versions"
		}
		violations = append(violations, fmt.Sprintf(
			"component %q moves to %s in %s and %s. No record assesses an upgrade into it "+
				"(upgrade-check reports unknown); write one whose `to` range covers %s "+
				"(docs/contributor/upgrade-records.md)",
			m.component, m.version, m.source, gap, m.version))
	}

	removed := make([]string, 0)
	for name := range base.recorded {
		if cur.components[name] && !cur.recorded[name] {
			removed = append(removed, name)
		}
	}
	stdsort.Strings(removed)
	for _, name := range removed {
		violations = append(violations, fmt.Sprintf(
			"component %q had a transition record at the merge base and has none now; restore it, "+
				"or remove the component from the registry if that is what this change means", name))
	}
	return violations
}

func TestRecordedPinViolations(t *testing.T) {
	set := upgrade.Set{
		"alpha": {Component: "alpha", Transitions: []upgrade.Transition{{To: ">=1.0.0 <=1.2.0"}}},
		"ahead": {Component: "ahead", Transitions: []upgrade.Transition{{To: "=2.0.0"}}},
		"swap":  {Component: "swap", Replaces: &upgrade.Replaces{Component: "legacy-swap", Verdict: upgrade.VerdictManual}},
	}
	pin := func(component, version, source string) versionPin {
		return versionPin{component: component, version: version, source: source}
	}
	tests := []struct {
		name string
		pins []versionPin
		want int
	}{
		{"covered", []versionPin{pin("alpha", "1.2.0", registryPinSource)}, 0},
		{"bumped past the record's to", []versionPin{pin("alpha", "1.3.0", registryPinSource)}, 1},
		{"an overlay override below the record is not governed yet", []versionPin{
			pin("alpha", "1.1.0", registryPinSource), pin("alpha", "0.9.9-rc.1", "overlay aks"),
		}, 0},
		{"an overlay override past the record", []versionPin{
			pin("alpha", "1.1.0", registryPinSource), pin("alpha", "1.5.0", "overlay aks"),
		}, 1},
		{"a record narrowed below its pin", []versionPin{pin("alpha", "v1.2.1", registryPinSource)}, 1},
		{"a non-semver pin is governed and never covered", []versionPin{pin("alpha", "main", registryPinSource)}, 1},
		{"a record ahead of its pin governs nothing yet", []versionPin{pin("ahead", "1.9.0", registryPinSource)}, 0},
		{"a pin that has reached the record ahead of it", []versionPin{pin("ahead", "2.0.1", registryPinSource)}, 1},
		{"a component without a record is not this rule's", []versionPin{pin("beta", "9.0.0", registryPinSource)}, 0},
		{"a replaces-only record governs no pin", []versionPin{pin("swap", "1.0.0", registryPinSource)}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := recordedPinViolations(set, tt.pins); len(got) != tt.want {
				t.Errorf("violations = %d, want %d:\n%v", len(got), tt.want, got)
			}
		})
	}
}

func TestChangedPinViolations(t *testing.T) {
	record := &upgrade.ComponentUpgrades{Component: "alpha", Transitions: []upgrade.Transition{{To: "=1.0.0"}}}
	ahead := &upgrade.ComponentUpgrades{Component: "beta", Transitions: []upgrade.Transition{{To: "=3.0.0"}}}
	replacesOnly := &upgrade.ComponentUpgrades{
		Component: "beta",
		Replaces:  &upgrade.Replaces{Component: "legacy-beta", Verdict: upgrade.VerdictManual},
	}
	pin := func(component, version, source string) versionPin {
		return versionPin{component: component, version: version, source: source}
	}
	// view derives components and sources from its pins; extra names a
	// component or source that pins nothing.
	view := func(recorded []string, extra []string, pins ...versionPin) coverageView {
		v := coverageView{components: map[string]bool{}, recorded: map[string]bool{}, sources: map[string]bool{}, pins: pins}
		for _, p := range pins {
			v.components[p.component] = true
			if p.source != registryPinSource {
				v.sources[p.source] = true
			}
		}
		for _, e := range extra {
			v.components[e] = true
			v.sources[e] = true
		}
		for _, r := range recorded {
			v.recorded[r] = true
		}
		return v
	}

	tests := []struct {
		name      string
		base, cur coverageView
		set       upgrade.Set
		want      int
	}{
		{
			name: "nothing moved",
			base: view(nil, nil, pin("beta", "2.0.0", registryPinSource)),
			cur:  view(nil, nil, pin("beta", "2.0.0", registryPinSource)),
		},
		{
			name: "a recordless component bumped",
			base: view(nil, nil, pin("beta", "2.0.0", registryPinSource)),
			cur:  view(nil, nil, pin("beta", "2.1.0", registryPinSource)),
			want: 1,
		},
		{
			name: "an overlay override bumped to a version another source already pins",
			base: view(nil, nil, pin("beta", "2.1.0", registryPinSource), pin("beta", "2.0.0", "overlay aks")),
			cur:  view(nil, nil, pin("beta", "2.1.0", registryPinSource), pin("beta", "2.1.0", "overlay aks")),
			want: 1,
		},
		{
			name: "a new overlay override",
			base: view(nil, nil, pin("beta", "2.0.0", registryPinSource)),
			cur:  view(nil, nil, pin("beta", "2.0.0", registryPinSource), pin("beta", "1.9.0", "overlay aks")),
			want: 1,
		},
		{
			name: "an override dropped, so its recipes move to the registry default",
			base: view(nil, nil, pin("beta", "2.1.0", registryPinSource), pin("beta", "2.0.0", "overlay aks")),
			cur:  view(nil, []string{"overlay aks"}, pin("beta", "2.1.0", registryPinSource)),
			want: 1,
		},
		{
			name: "a surviving overlay adds an override another source already pins",
			base: view(nil, []string{"overlay eks"},
				pin("beta", "2.1.0", registryPinSource), pin("beta", "2.0.0", "overlay aks")),
			cur: view(nil, []string{"overlay eks"},
				pin("beta", "2.1.0", registryPinSource), pin("beta", "2.0.0", "overlay aks"),
				pin("beta", "2.0.0", "overlay eks")),
			want: 1,
		},
		{
			name: "a surviving overlay adds an override equal to what it inherited",
			base: view(nil, []string{"overlay eks"}, pin("beta", "2.1.0", registryPinSource)),
			cur: view(nil, []string{"overlay eks"},
				pin("beta", "2.1.0", registryPinSource), pin("beta", "2.1.0", "overlay eks")),
		},
		{
			name: "a bump of a component whose record only replaces another",
			base: view(nil, nil, pin("beta", "2.0.0", registryPinSource)),
			cur:  view([]string{"beta"}, nil, pin("beta", "2.1.0", registryPinSource)),
			set:  upgrade.Set{"beta": replacesOnly},
			want: 1,
		},
		{
			name: "an overlay deleted along with its override",
			base: view(nil, nil, pin("beta", "2.1.0", registryPinSource), pin("beta", "2.0.0", "overlay aks")),
			cur:  view(nil, nil, pin("beta", "2.1.0", registryPinSource)),
		},
		{
			name: "an overlay renamed, its version already pinned at the base",
			base: view(nil, nil, pin("beta", "2.0.0", registryPinSource), pin("beta", "1.9.0", "overlay aks")),
			cur:  view(nil, nil, pin("beta", "2.0.0", registryPinSource), pin("beta", "1.9.0", "overlay azure")),
		},
		{
			name: "a bump reported once though two sources move to it",
			base: view(nil, nil, pin("beta", "2.0.0", registryPinSource), pin("beta", "2.0.0", "overlay aks")),
			cur:  view(nil, nil, pin("beta", "2.1.0", registryPinSource), pin("beta", "2.1.0", "overlay aks")),
			want: 1,
		},
		{
			name: "a bump a record governs is the hermetic rule's to judge",
			base: view(nil, nil, pin("alpha", "0.9.0", registryPinSource)),
			cur:  view([]string{"alpha"}, nil, pin("alpha", "1.0.0", registryPinSource)),
			set:  upgrade.Set{"alpha": record},
		},
		{
			name: "a bump below a record written ahead of it",
			base: view(nil, nil, pin("beta", "2.0.0", registryPinSource)),
			cur:  view([]string{"beta"}, nil, pin("beta", "2.1.0", registryPinSource)),
			set:  upgrade.Set{"beta": ahead},
			want: 1,
		},
		{
			name: "a component new in this change",
			base: view(nil, nil, pin("beta", "2.0.0", registryPinSource)),
			cur:  view(nil, nil, pin("beta", "2.0.0", registryPinSource), pin("gamma", "0.1.0", registryPinSource)),
		},
		{
			name: "a record deleted while the pin stays put",
			base: view([]string{"alpha"}, nil, pin("alpha", "1.0.0", registryPinSource)),
			cur:  view(nil, nil, pin("alpha", "1.0.0", registryPinSource)),
			want: 1,
		},
		{
			name: "a record deleted along with its component",
			base: view([]string{"alpha"}, nil, pin("alpha", "1.0.0", registryPinSource)),
			cur:  view(nil, nil),
		},
		{
			name: "a record deleted and the pin bumped reports both",
			base: view([]string{"alpha"}, nil, pin("alpha", "1.0.0", registryPinSource)),
			cur:  view(nil, nil, pin("alpha", "1.1.0", registryPinSource)),
			want: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := changedPinViolations(tt.base, tt.cur, tt.set); len(got) != tt.want {
				t.Errorf("violations = %d, want %d:\n%v", len(got), tt.want, got)
			}
		})
	}
}

func TestLoadBaseCoverageView(t *testing.T) {
	const registry = `
apiVersion: aicr.run/v1alpha1
kind: ComponentRegistry
components:
  - name: chart
    fieldRemovedSinceTheBase: true
    upgrades:
      file: components/chart/upgrades.yaml
    helm:
      defaultVersion: 1.0.0
  - name: kust
    kustomize:
      defaultSource: https://example.invalid/k
      defaultTag: v2.0.0
`
	fsys := fstest.MapFS{
		"registry.yaml": {Data: []byte(registry)},
		"overlays/base.yaml": {Data: []byte(
			"kind: RecipeMetadata\nmetadata: {name: base}\nspec: {componentRefs: [{name: chart, version: 0.9.0}]}\n")},
		"overlays/aks.yaml": {Data: []byte(
			"kind: RecipeMetadata\nmetadata: {name: aks}\nspec:\n  criteria: {service: not-a-service-anymore}\n" +
				"  componentRefs: [{name: chart, version: 1.1.0}]\n")},
		"overlays/notes.yaml": {Data: []byte("kind: SomethingElse\nmetadata: {name: notes}\n")},
		"mixins/tagged.yaml": {Data: []byte(
			"kind: RecipeMixin\nmetadata: {name: tagged}\nspec: {componentRefs: [{name: kust, tag: v2.1.0}]}\n")},
	}

	got, err := loadBaseCoverageView(fsys)
	if err != nil {
		t.Fatalf("loadBaseCoverageView: %v", err)
	}
	want := coverageView{
		components: map[string]bool{"chart": true, "kust": true},
		recorded:   map[string]bool{"chart": true},
		sources:    map[string]bool{"base base": true, "overlay aks": true, "mixin tagged": true},
		pins: []versionPin{
			{component: "chart", version: "0.9.0", source: "base base"},
			{component: "chart", version: "1.1.0", source: "overlay aks"},
			{component: "chart", version: "1.0.0", source: registryPinSource},
			{component: "kust", version: "v2.1.0", source: "mixin tagged"},
			{component: "kust", version: "v2.0.0", source: registryPinSource},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("loadBaseCoverageView =\n  %+v\nwant\n  %+v", got, want)
	}

	broken := []struct {
		name string
		fsys fstest.MapFS
	}{
		{"no registry", fstest.MapFS{"overlays/base.yaml": fsys["overlays/base.yaml"]}},
		{"registry that is not YAML", fstest.MapFS{"registry.yaml": {Data: []byte("components: [\n")}}},
		{"overlay that is not YAML", fstest.MapFS{
			"registry.yaml":     fsys["registry.yaml"],
			"overlays/bad.yaml": {Data: []byte("kind: [\n")},
		}},
	}
	for _, tt := range broken {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := loadBaseCoverageView(tt.fsys); err == nil {
				t.Error("want an error, not an empty view the comparison would pass against")
			}
		})
	}
}

func TestCollectVersionPins(t *testing.T) {
	reg := &ComponentRegistry{Components: []ComponentConfig{
		{Name: "chart", Helm: HelmConfig{DefaultVersion: "1.0.0"}},
		{Name: "kust", Kustomize: KustomizeConfig{DefaultSource: "https://example.invalid/k", DefaultTag: "v2.0.0"}},
		{Name: "manifests"},
	}}
	reg.byName = make(map[string]*ComponentConfig, len(reg.Components))
	for i := range reg.Components {
		reg.byName[reg.Components[i].Name] = &reg.Components[i]
	}
	tagged := &RecipeMixin{}
	tagged.Spec.ComponentRefs = []ComponentRef{
		{Name: "kust", Tag: "v2.1.0"},
		{Name: "chart", Tag: "stray"},
	}
	store := &MetadataStore{
		Base: &RecipeMetadata{Spec: RecipeMetadataSpec{ComponentRefs: []ComponentRef{
			{Name: "chart"},
		}}},
		Overlays: map[string]*RecipeMetadata{
			"aks": {Spec: RecipeMetadataSpec{ComponentRefs: []ComponentRef{
				{Name: "chart", Version: "1.1.0"},
				{Name: "not-in-registry", Version: "9.9.9"},
				{Name: "kust", Type: ComponentTypeHelm, Version: "3.0.0"},
			}}},
		},
		Mixins: map[string]*RecipeMixin{"tagged": tagged},
	}

	got := collectVersionPins(reg, store)
	want := []versionPin{
		{component: "chart", version: "1.1.0", source: "overlay aks"},
		{component: "chart", version: "1.0.0", source: registryPinSource},
		{component: "kust", version: "v2.1.0", source: "mixin tagged"},
		{component: "kust", version: "v2.0.0", source: registryPinSource},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("collectVersionPins =\n  %v\nwant\n  %v", got, want)
	}
}
