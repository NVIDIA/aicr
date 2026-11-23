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
	stderrors "errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"testing"

	"aicr.run/pkg/recipe/providertest"
)

func registryDoc(name string, components ...string) string {
	var b strings.Builder
	b.WriteString("apiVersion: aicr.run/v1beta1\nkind: ComponentRegistry\n")
	if name != "" {
		fmt.Fprintf(&b, "metadata:\n  name: %s\n", name)
	}
	if len(components) == 0 {
		b.WriteString("components: []\n")
		return b.String()
	}
	b.WriteString("components:\n")
	for _, c := range components {
		fmt.Fprintf(&b, "  - name: %s\n", c)
	}
	return b.String()
}

func overlayDoc(name, base, service string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: aicr.run/v1beta1\nkind: RecipeMetadata\nmetadata:\n  name: %s\nspec:\n", name)
	if base == "" && service == "" {
		return strings.TrimSuffix(b.String(), "\n") + " {}\n"
	}
	if base != "" {
		fmt.Fprintf(&b, "  base: %s\n", base)
	}
	if service != "" {
		fmt.Fprintf(&b, "  criteria:\n    service: %s\n", service)
	}
	return b.String()
}

func validatorsDoc(names ...string) string {
	var b strings.Builder
	b.WriteString("apiVersion: validator.nvidia.com/v1alpha1\nkind: ValidatorCatalog\nvalidators:\n")
	for _, n := range names {
		fmt.Fprintf(&b, "  - name: %s\n", n)
	}
	return b.String()
}

// mapCatalog is an in-memory catalog. files maps a path to its content.
func mapCatalog(label string, embedded bool, files map[string]string) CatalogInput {
	data := make(map[string][]byte, len(files))
	for name, content := range files {
		data[name] = []byte(content)
	}
	return CatalogInput{Provider: providertest.NewMap(data), Label: label, Embedded: embedded}
}

func nvidiaCatalog() CatalogInput {
	return mapCatalog("embedded", true, map[string]string{
		"registry.yaml":           registryDoc("nvidia", "gpu-operator"),
		"overlays/base.yaml":      overlayDoc("base", "", ""),
		"overlays/nv-leaf.yaml":   overlayDoc("nv-leaf", "base", "nv-service"),
		"components/gpu/vals.yml": "nv: true\n",
		"validators/catalog.yaml": validatorsDoc("nv-check"),
	})
}

func partnerCatalog() CatalogInput {
	return mapCatalog("./partner", false, map[string]string{
		"registry.yaml":              registryDoc("partner", "partner-operator"),
		"overlays/partner-leaf.yaml": overlayDoc("partner-leaf", "nv-leaf", "partner-service"),
		"components/p/vals.yml":      "partner: true\n",
		"validators/catalog.yaml":    validatorsDoc("partner-check"),
	})
}

func walkFiles(t *testing.T, p DataProvider) []string {
	t.Helper()
	var files []string
	err := p.WalkDir(t.Context(), "", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}
	return files
}

func TestComposedProviderUnion(t *testing.T) {
	t.Parallel()

	p, err := NewComposedDataProvider(t.Context(), []CatalogInput{nvidiaCatalog(), partnerCatalog()})
	if err != nil {
		t.Fatalf("NewComposedDataProvider: %v", err)
	}

	if got, want := p.Names(), []string{"nvidia", "partner"}; !slices.Equal(got, want) {
		t.Errorf("Names = %v, want %v", got, want)
	}

	registry, err := p.ReadFile(t.Context(), "registry.yaml")
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	for _, want := range []string{"gpu-operator", "partner-operator"} {
		if !strings.Contains(string(registry), want) {
			t.Errorf("composed registry lacks component %q:\n%s", want, registry)
		}
	}
	validators, err := p.ReadFile(t.Context(), "validators/catalog.yaml")
	if err != nil {
		t.Fatalf("read validators: %v", err)
	}
	for _, want := range []string{"nv-check", "partner-check"} {
		if !strings.Contains(string(validators), want) {
			t.Errorf("composed validator catalog lacks %q:\n%s", want, validators)
		}
	}

	wantFiles := []string{
		"components/gpu/vals.yml", "components/p/vals.yml", "overlays/base.yaml",
		"overlays/nv-leaf.yaml", "overlays/partner-leaf.yaml",
	}
	got := walkFiles(t, p)
	// registry.yaml and validators/catalog.yaml are served merged, and are
	// still walked, once each.
	for _, want := range append(wantFiles, "registry.yaml", "validators/catalog.yaml") {
		if !slices.Contains(got, want) {
			t.Errorf("WalkDir did not visit %q; got %v", want, got)
		}
	}
	seen := map[string]int{}
	for _, f := range got {
		seen[f]++
	}
	for f, n := range seen {
		if n != 1 {
			t.Errorf("WalkDir visited %q %d times, want once", f, n)
		}
	}

	tests := []struct {
		path        string
		wantSource  string
		wantCatalog string
	}{
		{"overlays/base.yaml", sourceEmbedded, "nvidia"},
		{"overlays/partner-leaf.yaml", sourceExternal, "partner"},
		{"components/p/vals.yml", sourceExternal, "partner"},
		{"registry.yaml", sourceMerged, "nvidia + partner"},
		{"validators/catalog.yaml", sourceMerged, "nvidia + partner"},
	}
	for _, tt := range tests {
		if got := p.Source(tt.path); got != tt.wantSource {
			t.Errorf("Source(%q) = %q, want %q", tt.path, got, tt.wantSource)
		}
		if got := p.CatalogOf(tt.path); got != tt.wantCatalog {
			t.Errorf("CatalogOf(%q) = %q, want %q", tt.path, got, tt.wantCatalog)
		}
	}

	if _, err := p.ReadFile(t.Context(), "components/missing.yaml"); !isNotExist(err) {
		t.Errorf("reading a path no catalog provides: err = %v, want fs.ErrNotExist", err)
	}
}

func isNotExist(err error) bool {
	return stderrors.Is(err, fs.ErrNotExist)
}

// TestComposedProviderOrderIndependent pins the contract that the result is
// the same whatever order the catalogs are given in.
func TestComposedProviderOrderIndependent(t *testing.T) {
	t.Parallel()

	a, err := NewComposedDataProvider(t.Context(), []CatalogInput{nvidiaCatalog(), partnerCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewComposedDataProvider(t.Context(), []CatalogInput{partnerCatalog(), nvidiaCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"registry.yaml", "validators/catalog.yaml", "overlays/nv-leaf.yaml"} {
		ga, _ := a.ReadFile(t.Context(), name)
		gb, _ := b.ReadFile(t.Context(), name)
		if string(ga) != string(gb) {
			t.Errorf("%s differs by catalog order:\n%s\n---\n%s", name, ga, gb)
		}
	}
	if fa, fb := walkFiles(t, a), walkFiles(t, b); !slices.Equal(fa, fb) {
		t.Errorf("WalkDir differs by catalog order:\n%v\n%v", fa, fb)
	}
}

func TestComposedProviderCollisionsFailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		a, b map[string]string
		want []string // every substring the error must contain
	}{
		{
			name: "component",
			a:    map[string]string{"registry.yaml": registryDoc("one", "shared")},
			b:    map[string]string{"registry.yaml": registryDoc("two", "shared")},
			want: []string{`component "shared"`, `"one"`, `"two"`},
		},
		{
			name: "validator",
			a: map[string]string{
				"registry.yaml":           registryDoc("one"),
				"validators/catalog.yaml": validatorsDoc("dup"),
			},
			b: map[string]string{
				"registry.yaml":           registryDoc("two"),
				"validators/catalog.yaml": validatorsDoc("dup"),
			},
			want: []string{`validator "dup"`, `"one"`, `"two"`},
		},
		{
			name: "same path",
			a: map[string]string{
				"registry.yaml":           registryDoc("one"),
				"components/foo/vals.yml": "a: 1\n",
			},
			b: map[string]string{
				"registry.yaml":           registryDoc("two"),
				"components/foo/vals.yml": "b: 2\n",
			},
			want: []string{`"components/foo/vals.yml"`, `"one"`, `"two"`},
		},
		{
			name: "overlay name in different files",
			a: map[string]string{
				"registry.yaml":   registryDoc("one"),
				"overlays/x.yaml": overlayDoc("same", "", "svc-a"),
			},
			b: map[string]string{
				"registry.yaml":   registryDoc("two"),
				"overlays/y.yaml": overlayDoc("same", "", "svc-b"),
			},
			want: []string{`overlay "same"`, `"one"`, `"two"`},
		},
		{
			name: "mixin name in different files",
			a: map[string]string{
				"registry.yaml": registryDoc("one"),
				"mixins/a.yaml": "apiVersion: aicr.run/v1beta1\nkind: RecipeMixin\nmetadata:\n  name: shared-mixin\nspec: {}\n",
			},
			b: map[string]string{
				"registry.yaml": registryDoc("two"),
				"mixins/b.yaml": "apiVersion: aicr.run/v1beta1\nkind: RecipeMixin\nmetadata:\n  name: shared-mixin\nspec: {}\n",
			},
			want: []string{`mixin "shared-mixin"`, `"one"`, `"two"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			for _, order := range [][]CatalogInput{
				{mapCatalog("./a", false, tt.a), mapCatalog("./b", false, tt.b)},
				{mapCatalog("./b", false, tt.b), mapCatalog("./a", false, tt.a)},
			} {
				_, err := NewComposedDataProvider(t.Context(), order)
				if err == nil {
					t.Fatal("expected a collision error")
				}
				for _, want := range tt.want {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
			}
		})
	}
}

func TestComposedProviderCatalogNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		inputs []CatalogInput
		want   string
	}{
		{
			name:   "no name",
			inputs: []CatalogInput{mapCatalog("./anon", false, map[string]string{"registry.yaml": registryDoc("")})},
			want:   "declares no metadata.name",
		},
		{
			name: "duplicate name",
			inputs: []CatalogInput{
				mapCatalog("./a", false, map[string]string{"registry.yaml": registryDoc("twin")}),
				mapCatalog("./b", false, map[string]string{"registry.yaml": registryDoc("twin")}),
			},
			want: `both declare the name "twin"`,
		},
		{
			name: "directory claims nvidia",
			inputs: []CatalogInput{
				mapCatalog("./fake", false, map[string]string{"registry.yaml": registryDoc("nvidia")}),
			},
			want: "reserved for the built-in catalog",
		},
		{
			name: "embedded declares another name",
			inputs: []CatalogInput{
				mapCatalog("embedded", true, map[string]string{"registry.yaml": registryDoc("other")}),
			},
			want: `want "nvidia"`,
		},
		{
			name:   "empty composition",
			inputs: nil,
			want:   "at least one catalog",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewComposedDataProvider(t.Context(), tt.inputs)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// TestComposedProviderCrossCatalogEdges covers the success criterion that a
// partner overlay can name an nvidia overlay as its base: it resolves when
// composed with nvidia and fails naming the missing overlay and the
// referencing catalog when composed without it.
func TestComposedProviderCrossCatalogEdges(t *testing.T) {
	t.Parallel()

	t.Run("with nvidia", func(t *testing.T) {
		t.Parallel()
		p, err := NewComposedDataProvider(t.Context(), []CatalogInput{nvidiaCatalog(), partnerCatalog()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { EvictCachedStore(p); EvictCachedRegistry(p); EvictCachedCriteriaRegistry(p) })
		store, err := LoadMetadataStoreFor(t.Context(), p)
		if err != nil {
			t.Fatalf("LoadMetadataStoreFor: %v", err)
		}
		chain, err := store.resolveInheritanceChain("partner-leaf")
		if err != nil {
			t.Fatalf("resolveInheritanceChain: %v", err)
		}
		var names []string
		for _, m := range chain {
			names = append(names, m.Metadata.Name)
		}
		if want := []string{"base", "nv-leaf", "partner-leaf"}; !slices.Equal(names, want) {
			t.Errorf("chain = %v, want %v", names, want)
		}
	})

	t.Run("without nvidia", func(t *testing.T) {
		t.Parallel()
		partner := mapCatalog("./partner", false, map[string]string{
			"registry.yaml":              registryDoc("partner"),
			"overlays/base.yaml":         overlayDoc("base", "", ""),
			"overlays/partner-leaf.yaml": overlayDoc("partner-leaf", "nv-leaf", "partner-service"),
		})
		_, err := NewComposedDataProvider(t.Context(), []CatalogInput{partner})
		if err == nil {
			t.Fatal("expected a dangling-edge error")
		}
		for _, want := range []string{`"nv-leaf"`, `"partner"`, "overlays/partner-leaf.yaml"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not contain %q", err, want)
			}
		}
	})
}

// TestComposedProviderNeverReadsEmbedded builds a composition of directory
// catalogs on external-only providers, which hold no embedded provider at all
// (a nil one panics on any read). Resolving through it therefore proves that
// no embedded byte is read when nvidia is not selected.
func TestComposedProviderNeverReadsEmbedded(t *testing.T) {
	t.Parallel()

	dirA := providertest.WriteDir(t, map[string][]byte{
		"registry.yaml":      []byte(registryDoc("alpha", "alpha-operator")),
		"overlays/base.yaml": []byte(overlayDoc("base", "", "")),
		"overlays/leaf.yaml": []byte(overlayDoc("alpha-leaf", "base", "alpha-service")),
	})
	dirB := providertest.WriteDir(t, map[string][]byte{
		"registry.yaml":        []byte(registryDoc("beta", "beta-operator")),
		"overlays/beta.yaml":   []byte(overlayDoc("beta-leaf", "alpha-leaf", "beta-service")),
		"components/b/val.yml": []byte("beta: true\n"),
	})
	var inputs []CatalogInput
	for _, dir := range []string{dirB, dirA} {
		dp, err := NewLayeredDataProvider(nil, LayeredProviderConfig{ExternalDir: dir, ExternalOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, CatalogInput{Provider: dp, Label: dir})
	}
	p, err := NewComposedDataProvider(t.Context(), inputs)
	if err != nil {
		t.Fatalf("NewComposedDataProvider: %v", err)
	}
	t.Cleanup(func() { EvictCachedStore(p); EvictCachedRegistry(p); EvictCachedCriteriaRegistry(p) })

	if got, want := p.Names(), []string{"alpha", "beta"}; !slices.Equal(got, want) {
		t.Errorf("Names = %v, want %v", got, want)
	}
	registry, err := p.ReadFile(t.Context(), "registry.yaml")
	if err != nil {
		t.Fatalf("read registry: %v", err)
	}
	for _, name := range []string{"alpha-operator", "beta-operator"} {
		if !strings.Contains(string(registry), name) {
			t.Errorf("composed registry lacks %q:\n%s", name, registry)
		}
	}
	store, err := LoadMetadataStoreFor(t.Context(), p)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}
	if _, err = store.resolveInheritanceChain("beta-leaf"); err != nil {
		t.Fatalf("resolveInheritanceChain: %v", err)
	}
	for _, path := range []string{"overlays/base.yaml", "components/b/val.yml", "registry.yaml"} {
		if src := p.Source(path); src == sourceEmbedded {
			t.Errorf("Source(%q) = %q, want a non-embedded source", path, src)
		}
	}
}

// TestLayeredOverComposed pins that --data keeps its meaning on top of a
// composition: one override layer, applied last, last wins.
func TestLayeredOverComposed(t *testing.T) {
	t.Parallel()

	composed, err := NewComposedDataProvider(t.Context(), []CatalogInput{nvidiaCatalog(), partnerCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	override := providertest.WriteDir(t, map[string][]byte{
		"registry.yaml":           []byte(registryDoc("", "override-operator")),
		"components/p/vals.yml":   []byte("override: true\n"),
		"components/new/vals.yml": []byte("new: true\n"),
	})
	layered, err := NewLayeredDataProviderOver(composed, LayeredProviderConfig{ExternalDir: override})
	if err != nil {
		t.Fatalf("NewLayeredDataProviderOver: %v", err)
	}

	got, err := layered.ReadFile(t.Context(), "components/p/vals.yml")
	if err != nil || string(got) != "override: true\n" {
		t.Errorf("override file = %q, %v; want the --data file to win", got, err)
	}
	got, err = layered.ReadFile(t.Context(), "components/gpu/vals.yml")
	if err != nil || string(got) != "nv: true\n" {
		t.Errorf("composed file = %q, %v; want it read through the composition", got, err)
	}
	registry, err := layered.ReadFile(t.Context(), "registry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gpu-operator", "partner-operator", "override-operator"} {
		if !strings.Contains(string(registry), want) {
			t.Errorf("registry lacks %q:\n%s", want, registry)
		}
	}
	if files := layered.ExternalFiles(); !slices.Equal(files, []string{"components/new/vals.yml", "components/p/vals.yml", "registry.yaml"}) {
		t.Errorf("ExternalFiles = %v, want only the override directory's files", files)
	}
}

func TestComposedWalkDirSkipDir(t *testing.T) {
	t.Parallel()

	p, err := NewComposedDataProvider(t.Context(), []CatalogInput{nvidiaCatalog(), partnerCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	var visited []string
	err = p.WalkDir(t.Context(), "", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path == "components" {
			return fs.SkipDir
		}
		visited = append(visited, path)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range visited {
		if strings.HasPrefix(path, "components/") {
			t.Errorf("visited %q after SkipDir on components", path)
		}
	}
}
