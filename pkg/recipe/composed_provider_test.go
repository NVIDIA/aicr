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

	"gopkg.in/yaml.v3"

	aicrerrors "aicr.run/pkg/errors"
	"aicr.run/pkg/errors/errorstest"
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
		if gotSource := p.Source(tt.path); gotSource != tt.wantSource {
			t.Errorf("Source(%q) = %q, want %q", tt.path, gotSource, tt.wantSource)
		}
		if gotCatalog := p.CatalogOf(tt.path); gotCatalog != tt.wantCatalog {
			t.Errorf("CatalogOf(%q) = %q, want %q", tt.path, gotCatalog, tt.wantCatalog)
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
			name: "overlay name outside overlays/",
			a: map[string]string{
				"registry.yaml":   registryDoc("one"),
				"overlays/x.yaml": overlayDoc("same", "", "svc-a"),
			},
			b: map[string]string{
				"registry.yaml":    registryDoc("two"),
				"extra/other.yaml": overlayDoc("same", "", "svc-b"),
			},
			want: []string{`overlay "same"`, `"one"`, `"two"`, "extra/other.yaml"},
		},
		{
			name: "second base in a subdirectory of overlays/",
			a: map[string]string{
				"registry.yaml":      registryDoc("one"),
				"overlays/base.yaml": overlayDoc("base", "", ""),
			},
			b: map[string]string{
				"registry.yaml":             registryDoc("two"),
				"overlays/nested/base.yaml": overlayDoc("base", "", ""),
			},
			want: []string{"the base recipe", `"one"`, `"two"`, "overlays/nested/base.yaml"},
		},
		{
			name: "second base in another overlays directory",
			a: map[string]string{
				"registry.yaml":      registryDoc("one"),
				"overlays/base.yaml": overlayDoc("base", "", ""),
			},
			b: map[string]string{
				"registry.yaml":            registryDoc("two"),
				"extra/overlays/base.yaml": overlayDoc("base", "", ""),
			},
			want: []string{"the base recipe", `"one"`, `"two"`, "extra/overlays/base.yaml"},
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
				errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
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
			errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
		})
	}
}

// TestComposedProviderCatalogNameGrammar pins the rule on metadata.name: a
// lowercase DNS label. The name appears in errors now and in provenance later,
// so "NVIDIA" may not sit next to "nvidia", and "+" is the separator of the
// composed registry's own name.
func TestComposedProviderCatalogNameGrammar(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"a", "acme", "acme-2", "0day", strings.Repeat("a", maxCatalogNameLength)} {
		t.Run("valid "+name, func(t *testing.T) {
			t.Parallel()
			in := mapCatalog("./x", false, map[string]string{"registry.yaml": registryDoc(name)})
			if _, err := NewComposedDataProvider(t.Context(), []CatalogInput{in}); err != nil {
				t.Fatalf("name %q rejected: %v", name, err)
			}
		})
	}

	invalid := []string{"NVIDIA", "Nvidia", "ACME", "acme+beta", "a_b", "-a", "a-", "a b", "a.b", strings.Repeat("a", maxCatalogNameLength+1)}
	for _, name := range invalid {
		t.Run("invalid "+name, func(t *testing.T) {
			t.Parallel()
			in := mapCatalog("./x", false, map[string]string{"registry.yaml": registryDoc(name)})
			_, err := NewComposedDataProvider(t.Context(), []CatalogInput{in})
			if err == nil || !strings.Contains(err.Error(), "lowercase DNS label") {
				t.Fatalf("name %q: error = %v, want the DNS-label rule", name, err)
			}
			errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
		})
	}

	t.Run("the built-in catalog is held to it too", func(t *testing.T) {
		t.Parallel()
		in := mapCatalog("embedded", true, map[string]string{"registry.yaml": registryDoc("NVIDIA")})
		if _, err := NewComposedDataProvider(t.Context(), []CatalogInput{in}); err == nil {
			t.Fatal("expected the DNS-label rule to apply to the built-in catalog")
		}
	})
}

// TestComposedProviderOverlayNeedsBase pins that an overlay extending the base
// fails at construction, naming it, when no catalog supplies one, instead of
// later with a bare "base.yaml not found".
func TestComposedProviderOverlayNeedsBase(t *testing.T) {
	t.Parallel()

	for name, base := range map[string]string{"explicit spec.base: base": "base", "implicit root": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			partner := mapCatalog("./partner", false, map[string]string{
				"registry.yaml":      registryDoc("partner"),
				"overlays/leaf.yaml": overlayDoc("leaf", base, "svc"),
			})
			_, err := NewComposedDataProvider(t.Context(), []CatalogInput{partner})
			if err == nil {
				t.Fatal("expected an error: no composed catalog provides a base")
			}
			for _, want := range []string{`"partner"`, "overlays/leaf.yaml", "base recipe"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
		})
	}

	t.Run("a base from another catalog satisfies it", func(t *testing.T) {
		t.Parallel()
		partner := mapCatalog("./partner", false, map[string]string{
			"registry.yaml":      registryDoc("partner"),
			"overlays/leaf.yaml": overlayDoc("leaf", "base", "svc"),
		})
		if _, err := NewComposedDataProvider(t.Context(), []CatalogInput{nvidiaCatalog(), partner}); err != nil {
			t.Fatalf("composition with nvidia's base: %v", err)
		}
	})
}

// TestComposedProviderIgnoresNonCatalogFiles pins that only files a loader
// reads take part: two directory catalogs that each ship a README.md or a
// LICENSE are independent catalogs, not a collision.
func TestComposedProviderIgnoresNonCatalogFiles(t *testing.T) {
	t.Parallel()

	files := func(name string, extra map[string]string) CatalogInput {
		m := map[string]string{
			"registry.yaml":      registryDoc(name),
			"README.md":          name + "\n",
			"LICENSE":            name + "\n",
			"overlays/README.md": name + "\n",
		}
		for k, v := range extra {
			m[k] = v
		}
		return mapCatalog("./"+name, false, m)
	}
	p, err := NewComposedDataProvider(t.Context(), []CatalogInput{
		files("acme", map[string]string{"overlays/base.yaml": overlayDoc("base", "", "")}),
		files("beta", nil),
	})
	if err != nil {
		t.Fatalf("catalogs that both ship a README.md must compose: %v", err)
	}
	walked := walkFiles(t, p)
	for _, path := range []string{"README.md", "LICENSE", "overlays/README.md"} {
		if _, readErr := p.ReadFile(t.Context(), path); !isNotExist(readErr) {
			t.Errorf("ReadFile(%q) err = %v, want fs.ErrNotExist: not catalog content", path, readErr)
		}
		if slices.Contains(walked, path) {
			t.Errorf("WalkDir visited %q, which is not catalog content", path)
		}
	}
	if !slices.Contains(walked, "overlays/base.yaml") {
		t.Errorf("WalkDir lost catalog content; got %v", walked)
	}
}

// registryReferencing is a registry whose one component names files by path.
func registryReferencing(name, component, assertFile, upgradesFile string) string {
	var b strings.Builder
	b.WriteString(registryDoc(name, component))
	if assertFile != "" {
		fmt.Fprintf(&b, "    healthCheck:\n      assertFile: %s\n", assertFile)
	}
	if upgradesFile != "" {
		fmt.Fprintf(&b, "    upgrades:\n      file: %s\n", upgradesFile)
	}
	return b.String()
}

// TestComposedProviderServesReferencedFiles pins that a file a registry names
// is served even when it is neither under components/ nor a .yaml file: the
// metadata store reads healthCheck.assertFile by path, so a composition that
// dropped it would fail with a missing file for a catalog that is valid alone.
func TestComposedProviderServesReferencedFiles(t *testing.T) {
	t.Parallel()

	partner := mapCatalog("./partner", false, map[string]string{
		"registry.yaml":           registryReferencing("partner", "partner-operator", "assert/p-assert.yml", "notes/p-upgrades.json"),
		"assert/p-assert.yml":     "apiVersion: v1\nkind: Pod\n",
		"notes/p-upgrades.json":   "{}\n",
		"assert/unreferenced.yml": "unused: true\n",
	})
	p, err := NewComposedDataProvider(t.Context(), []CatalogInput{nvidiaCatalog(), partner})
	if err != nil {
		t.Fatalf("NewComposedDataProvider: %v", err)
	}
	walked := walkFiles(t, p)
	for _, path := range []string{"assert/p-assert.yml", "notes/p-upgrades.json"} {
		got, readErr := p.ReadFile(t.Context(), path)
		if readErr != nil {
			t.Errorf("ReadFile(%q): %v; a registry-named file must be served", path, readErr)
		}
		if len(got) == 0 {
			t.Errorf("ReadFile(%q) returned no content", path)
		}
		if !slices.Contains(walked, path) {
			t.Errorf("WalkDir did not visit %q; got %v", path, walked)
		}
		if gotCatalog := p.CatalogOf(path); gotCatalog != "partner" {
			t.Errorf("CatalogOf(%q) = %q, want partner", path, gotCatalog)
		}
	}
	// Naming one file does not make its neighbours catalog content.
	if _, readErr := p.ReadFile(t.Context(), "assert/unreferenced.yml"); !isNotExist(readErr) {
		t.Errorf("ReadFile of an unreferenced non-yaml file err = %v, want fs.ErrNotExist", readErr)
	}
}

// TestComposedProviderReferencedFileCollision pins that a referenced file is
// subject to the same collision rule as any other catalog file: when one
// catalog names a path and another ships it, the composition fails instead of
// quietly serving one of them.
func TestComposedProviderReferencedFileCollision(t *testing.T) {
	t.Parallel()

	namer := mapCatalog("./namer", false, map[string]string{
		"registry.yaml":     registryReferencing("namer", "namer-operator", "assert/shared.yml", ""),
		"assert/shared.yml": "from: namer\n",
	})
	shipper := mapCatalog("./shipper", false, map[string]string{
		"registry.yaml":     registryDoc("shipper"),
		"assert/shared.yml": "from: shipper\n",
	})
	_, err := NewComposedDataProvider(t.Context(), []CatalogInput{nvidiaCatalog(), namer, shipper})
	if err == nil {
		t.Fatal("two catalogs shipping a registry-named file composed, want a collision")
	}
	errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
	for _, want := range []string{`"assert/shared.yml"`, `"namer"`, `"shipper"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// TestComposedProviderMixinEdges covers spec.mixins edges: a partner overlay
// may use a mixin an nvidia-style catalog defines, and fails naming the mixin,
// the catalog and the file when that catalog is left out.
func TestComposedProviderMixinEdges(t *testing.T) {
	t.Parallel()

	mixin := "apiVersion: aicr.run/v1beta1\nkind: RecipeMixin\nmetadata:\n  name: shared-mixin\nspec: {}\n"
	leaf := "apiVersion: aicr.run/v1beta1\nkind: RecipeMetadata\nmetadata:\n  name: mixin-leaf\nspec:\n  base: base\n  mixins:\n    - shared-mixin\n  criteria:\n    service: mixin-service\n"
	owner := func() CatalogInput {
		return mapCatalog("./owner", false, map[string]string{
			"registry.yaml":      registryDoc("owner"),
			"overlays/base.yaml": overlayDoc("base", "", ""),
			"mixins/shared.yaml": mixin,
		})
	}
	user := func() CatalogInput {
		return mapCatalog("./user", false, map[string]string{
			"registry.yaml":      registryDoc("user"),
			"overlays/leaf.yaml": leaf,
		})
	}

	if _, err := NewComposedDataProvider(t.Context(), []CatalogInput{owner(), user()}); err != nil {
		t.Fatalf("a mixin defined by another composed catalog must resolve: %v", err)
	}

	_, err := NewComposedDataProvider(t.Context(), []CatalogInput{user()})
	if err == nil {
		t.Fatal("expected a dangling-mixin error")
	}
	for _, want := range []string{`mixin "shared-mixin"`, `"user"`, "overlays/leaf.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
}

// TestCatalogDocKindAgreesWithMetadataStore guards the classification against
// drift: over the embedded catalog, the number of overlays, mixins and bases
// found by catalogDocKind must equal what LoadMetadataStoreFor loads.
func TestCatalogDocKindAgreesWithMetadataStore(t *testing.T) {
	t.Parallel()

	dp := NewEmbeddedDataProvider(GetEmbeddedFS(), "")
	t.Cleanup(func() { EvictCachedStore(dp); EvictCachedRegistry(dp); EvictCachedCriteriaRegistry(dp) })
	store, err := LoadMetadataStoreFor(t.Context(), dp)
	if err != nil {
		t.Fatalf("LoadMetadataStoreFor: %v", err)
	}

	counts := map[string]int{}
	walkErr := dp.WalkDir(t.Context(), "", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		kind := catalogDocKind(path)
		if kind == "" {
			return nil
		}
		data, readErr := dp.ReadFile(t.Context(), path)
		if readErr != nil {
			return readErr
		}
		var doc nodeDoc
		if unmarshalErr := yaml.Unmarshal(data, &doc); unmarshalErr != nil {
			t.Errorf("%s does not parse as a node document: %v", path, unmarshalErr)
			return nil
		}
		if doc.isNodeOf(kind, path) {
			counts[kind]++
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("WalkDir: %v", walkErr)
	}
	if got, want := counts[nodeKindOverlay], len(store.Overlays); got != want {
		t.Errorf("overlays: classification found %d, the store loaded %d", got, want)
	}
	if got, want := counts[nodeKindMixin], len(store.Mixins); got != want {
		t.Errorf("mixins: classification found %d, the store loaded %d", got, want)
	}
	if store.Base == nil || counts[nodeKindBase] != 1 {
		t.Errorf("bases: classification found %d, the store has base %v", counts[nodeKindBase], store.Base != nil)
	}
}

func TestComposesExternalCatalogs(t *testing.T) {
	t.Parallel()

	nvidiaOnly, err := NewComposedDataProvider(t.Context(), []CatalogInput{nvidiaCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	withPartner, err := NewComposedDataProvider(t.Context(), []CatalogInput{nvidiaCatalog(), partnerCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	override := providertest.WriteDir(t, map[string][]byte{"registry.yaml": []byte(providertest.EmptyRegistryYAML)})
	cfg := LayeredProviderConfig{ExternalDir: override}
	layeredNvidia, err := NewLayeredDataProviderOver(nvidiaOnly, cfg)
	if err != nil {
		t.Fatal(err)
	}
	layeredPartner, err := NewLayeredDataProviderOver(withPartner, cfg)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := NewLayeredDataProvider(NewEmbeddedDataProvider(GetEmbeddedFS(), ""), cfg)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		dp   DataProvider
		want bool
	}{
		{"nil", nil, false},
		{"embedded", NewEmbeddedDataProvider(GetEmbeddedFS(), ""), false},
		{"nvidia only", nvidiaOnly, false},
		{"nvidia and a directory", withPartner, true},
		{"--data over nvidia only", layeredNvidia, false},
		{"--data over a directory catalog", layeredPartner, true},
		{"--data over embedded", plain, false},
	}
	for _, tt := range tests {
		if got := ComposesExternalCatalogs(tt.dp); got != tt.want {
			t.Errorf("%s: ComposesExternalCatalogs = %v, want %v", tt.name, got, tt.want)
		}
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
		names := make([]string, 0, len(chain))
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
		errorstest.WantReportedCode(t, err, aicrerrors.ErrCodeInvalidRequest)
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
	dirs := []string{dirB, dirA}
	inputs := make([]CatalogInput, 0, len(dirs))
	for _, dir := range dirs {
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

	// A path the override does not own keeps the provenance the composition
	// reports. Adding --data must not turn a partner overlay into an
	// "embedded" one.
	for path, want := range map[string]string{
		"overlays/nv-leaf.yaml":      sourceEmbedded,
		"overlays/partner-leaf.yaml": sourceExternal,
		"components/p/vals.yml":      sourceExternal, // the override owns it
		"components/new/vals.yml":    sourceExternal,
	} {
		if got := layered.Source(path); got != want {
			t.Errorf("Source(%q) through a --data layer = %q, want %q", path, got, want)
		}
	}
}

// TestLayeredOverComposedKeepsExternalValidation pins the consequence of that
// provenance: a partner overlay that sets criteria.nodes is rejected by the
// metadata store's fail-closed check for external overlays, with and without a
// --data layer on top of the composition.
func TestLayeredOverComposedKeepsExternalValidation(t *testing.T) {
	t.Parallel()

	partner := mapCatalog("./partner", false, map[string]string{
		"registry.yaml": registryDoc("partner"),
		"overlays/nodes-leaf.yaml": "apiVersion: aicr.run/v1beta1\nkind: RecipeMetadata\nmetadata:\n  name: nodes-leaf\n" +
			"spec:\n  base: nv-leaf\n  criteria:\n    service: partner-service\n    nodes: 8\n",
	})
	composed, err := NewComposedDataProvider(t.Context(), []CatalogInput{nvidiaCatalog(), partner})
	if err != nil {
		t.Fatal(err)
	}
	override := providertest.WriteDir(t, map[string][]byte{"registry.yaml": []byte(providertest.EmptyRegistryYAML)})
	layered, err := NewLayeredDataProviderOver(composed, LayeredProviderConfig{ExternalDir: override})
	if err != nil {
		t.Fatal(err)
	}
	for name, dp := range map[string]DataProvider{"composition": composed, "composition with --data": layered} {
		t.Cleanup(func() { EvictCachedStore(dp); EvictCachedRegistry(dp); EvictCachedCriteriaRegistry(dp) })
		_, err = LoadMetadataStoreFor(t.Context(), dp)
		if err == nil || !strings.Contains(err.Error(), "criteria.nodes") {
			t.Errorf("%s: LoadMetadataStoreFor error = %v, want the external criteria.nodes rejection", name, err)
		}
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

// TestComposedWalkDirSkipAll pins that fs.SkipAll ends the whole walk: each
// child walk consumes the sentinel, so without care the next catalog (or the
// base under a --data layer) would be walked and the callback called again.
func TestComposedWalkDirSkipAll(t *testing.T) {
	t.Parallel()

	composed, err := NewComposedDataProvider(t.Context(), []CatalogInput{nvidiaCatalog(), partnerCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	override := providertest.WriteDir(t, map[string][]byte{
		"registry.yaml":           []byte(providertest.EmptyRegistryYAML),
		"components/new/vals.yml": []byte("new: true\n"),
	})
	layered, err := NewLayeredDataProviderOver(composed, LayeredProviderConfig{ExternalDir: override})
	if err != nil {
		t.Fatal(err)
	}
	for name, dp := range map[string]DataProvider{"composition": composed, "composition with --data": layered} {
		t.Run(name, func(t *testing.T) {
			var files []string
			walkErr := dp.WalkDir(t.Context(), "", func(path string, d fs.DirEntry, entryErr error) error {
				if entryErr != nil || d.IsDir() {
					return entryErr
				}
				files = append(files, path)
				return fs.SkipAll
			})
			if walkErr != nil {
				t.Fatalf("WalkDir: %v", walkErr)
			}
			if len(files) != 1 {
				t.Errorf("callback ran for %v, want one file: fs.SkipAll must end the walk", files)
			}
		})
	}
}

// TestCaptureArtifactLayersRefusesComposedBase pins that an artifact is never
// built over a composition: its base layer would record partner catalogs as
// embedded.
func TestCaptureArtifactLayersRefusesComposedBase(t *testing.T) {
	t.Parallel()

	composed, err := NewComposedDataProvider(t.Context(), []CatalogInput{nvidiaCatalog(), partnerCatalog()})
	if err != nil {
		t.Fatal(err)
	}
	override := providertest.WriteDir(t, map[string][]byte{"registry.yaml": []byte(providertest.EmptyRegistryYAML)})
	layered, err := NewLayeredDataProviderOver(composed, LayeredProviderConfig{ExternalDir: override})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = captureArtifactLayers(t.Context(), layered)
	if err == nil || !strings.Contains(err.Error(), "composed catalog") {
		t.Fatalf("captureArtifactLayers error = %v, want the composed-catalog rejection", err)
	}
}
