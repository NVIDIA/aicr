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

package main

import (
	"context"
	stderrors "errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"aicr.run/pkg/bom"
	"aicr.run/pkg/errors"
	"aicr.run/pkg/recipe"
)

const declaredRereadHint = "Re-read the images its operator launches at that pin and update " + bom.DeclaredImagesFile

// declaredChartKey identifies one deployed chart pin.
type declaredChartKey struct {
	Repository, Chart, Version string
}

func (k declaredChartKey) String() string {
	return fmt.Sprintf("%s %s@%s", k.Repository, k.Chart, k.Version)
}

// manifestPin is one deployment of a manifest-only component. It holds the
// images its vendored manifests deploy and the recipe that deploys them.
type manifestPin struct {
	images []string
	by     string
}

// componentPins lists every pin of a component that the registry or a leaf
// recipe deploys, with the deployers of each. bom lists the chart versions
// the BOM surveys, at the registry repository and chart.
type componentPins struct {
	charts    map[declaredChartKey][]string
	manifests []manifestPin
	bom       []declaredChartKey
}

// TestDeclaredImagesAnchored checks every images.yaml against the pins AICR
// ships, the registry pin and every pin a leaf recipe deploys.
//   - Each deployed chart pin, and each deployed set of vendored manifests,
//     must have an entry. A pin bump therefore fails until the operator's
//     images are re-read at the new pin.
//   - Each version the BOM surveys must resolve to an entry, so this test and
//     make bom-docs agree.
//   - Each entry must match a deployed pin, so a stale entry fails.
//   - Each component in declaredRequiredComponents must keep its file.
func TestDeclaredImagesAnchored(t *testing.T) {
	ctx := t.Context()
	repoRoot := filepath.Join("..", "..")
	bomReg, err := loadRegistry(filepath.Join(repoRoot, "recipes", "registry.yaml"))
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	sources, err := loadRecipeSources(ctx, repoRoot)
	if err != nil {
		t.Fatalf("load recipe sources: %v", err)
	}
	variants, err := deriveVariants(bomReg, sources)
	if err != nil {
		t.Fatalf("derive BOM variants: %v", err)
	}
	bomComponents := indexComponentsByName(bomReg.Components)

	efs := recipe.GetEmbeddedFS()
	dp := recipe.NewEmbeddedDataProvider(efs, "")
	reg, err := recipe.GetComponentRegistryFor(dp)
	if err != nil {
		t.Fatalf("load component registry: %v", err)
	}
	leaves, err := recipe.ResolveLeaves(ctx, recipe.ResolveLeavesOptions{Provider: dp, Version: "declared-images"})
	if err != nil {
		t.Fatalf("resolve leaf recipes: %v", err)
	}

	files, err := fs.Glob(efs, path.Join("components", "*", bom.DeclaredImagesFile))
	if err != nil {
		t.Fatalf("glob %s: %v", bom.DeclaredImagesFile, err)
	}
	names := map[string]struct{}{}
	for _, f := range files {
		names[path.Base(path.Dir(f))] = struct{}{}
	}
	for name := range declaredRequiredComponents {
		names[name] = struct{}{}
	}

	for _, name := range slices.Sorted(maps.Keys(names)) {
		data, readErr := dp.ReadFile(ctx, path.Join("components", name, bom.DeclaredImagesFile))
		if readErr != nil {
			if stderrors.Is(readErr, fs.ErrNotExist) {
				t.Errorf("component %q is in declaredRequiredComponents but has no %s",
					name, bom.DeclaredImagesFile)
				continue
			}
			t.Fatalf("read %s of component %q: %v", bom.DeclaredImagesFile, name, readErr)
		}
		d, parseErr := bom.ParseDeclaredImages(name, data)
		if parseErr != nil {
			t.Errorf("component %q: %v", name, parseErr)
			continue
		}
		pins, pinErr := deployedPins(ctx, dp, reg, leaves, name)
		if pinErr != nil {
			t.Fatalf("component %q: %v", name, pinErr)
		}
		if c, ok := bomComponents[name]; ok && c.kind() == kindHelm {
			pins.bom = append(pins.bom, declaredChartKey{c.Helm.DefaultRepository, c.effectiveChart(), c.Helm.DefaultVersion})
			for _, v := range variants {
				if v.Name == name {
					pins.bom = append(pins.bom, declaredChartKey{c.Helm.DefaultRepository, c.effectiveChart(), v.Version})
				}
			}
		}
		for _, problem := range declaredProblems(d, pins) {
			t.Error(problem)
		}
	}
}

// deployedPins returns every pin of the named component that the registry or
// a leaf recipe deploys.
func deployedPins(
	ctx context.Context,
	dp recipe.DataProvider,
	reg *recipe.ComponentRegistry,
	leaves []recipe.ResolvedLeaf,
	name string,
) (*componentPins, error) {

	pins := &componentPins{charts: map[declaredChartKey][]string{}}
	add := func(ref recipe.ComponentRef, by string) error {
		if ref.HasExternalChart() {
			k := declaredChartKey{Repository: ref.Source, Chart: ref.EffectiveChart(), Version: ref.Version}
			pins.charts[k] = append(pins.charts[k], by)
			return nil
		}
		var images []string
		for _, p := range slices.Concat(ref.ManifestFiles, ref.PreManifestFiles) {
			content, err := dp.ReadFile(ctx, p)
			if err != nil {
				return errors.Wrap(errors.ErrCodeInternal, "read manifest "+p, err)
			}
			set, err := bom.ExtractImageSetFromYAML(content)
			if err != nil {
				return errors.Wrap(errors.ErrCodeInvalidRequest, "extract images from "+p, err)
			}
			images = append(images, set.Images...)
		}
		pins.manifests = append(pins.manifests, manifestPin{images: images, by: by})
		return nil
	}

	if cfg := reg.Get(name); cfg != nil {
		ref := recipe.ComponentRef{Name: name}
		ref.ApplyRegistryDefaults(cfg)
		if err := add(ref, "the registry"); err != nil {
			return nil, err
		}
	}
	for _, leaf := range leaves {
		if leaf.Err != nil {
			return nil, errors.PropagateOrWrap(leaf.Err, errors.ErrCodeInternal, "resolve leaf recipe "+leaf.Entry.Name)
		}
		for _, ref := range leaf.Result.ComponentRefs {
			if ref.Name != name || !ref.IsEnabled() {
				continue
			}
			if err := add(ref, "leaf recipe "+leaf.Entry.Name); err != nil {
				return nil, err
			}
		}
	}
	return pins, nil
}

// declaredProblems returns the deployed pins and BOM versions d has no entry
// for, and the entries of d that match neither.
func declaredProblems(d *bom.DeclaredImages, pins *componentPins) []string {
	var problems []string
	usedCharts := map[declaredChartKey]struct{}{}
	for _, k := range slices.SortedFunc(maps.Keys(pins.charts), func(a, b declaredChartKey) int {
		return strings.Compare(a.String(), b.String())
	}) {
		if d.Chart(k.Repository, k.Chart, k.Version) == nil {
			problems = append(problems, fmt.Sprintf("the %s of component %q has no entry for chart %s, deployed by %s. %s",
				bom.DeclaredImagesFile, d.Component, k, strings.Join(slices.Compact(slices.Sorted(slices.Values(pins.charts[k]))), ", "),
				declaredRereadHint))
			continue
		}
		usedCharts[k] = struct{}{}
	}
	for _, k := range pins.bom {
		entry, err := d.ChartFor(k.Repository, k.Chart, k.Version)
		if err != nil {
			problems = append(problems, fmt.Sprintf("the BOM surveys chart %s, which make bom-docs cannot resolve: %v", k, err))
			continue
		}
		usedCharts[declaredChartKey{Repository: entry.Repository, Chart: entry.Chart, Version: entry.SourceVersion}] = struct{}{}
	}
	usedManifests := map[string]struct{}{}
	for _, m := range pins.manifests {
		entry, err := d.ManifestFor(m.images)
		if err != nil {
			problems = append(problems, fmt.Sprintf("manifests deployed by %s: %v", m.by, err))
			continue
		}
		usedManifests[entry.OperatorImage] = struct{}{}
	}

	for _, c := range d.Charts {
		k := declaredChartKey{Repository: c.Repository, Chart: c.Chart, Version: c.SourceVersion}
		if _, used := usedCharts[k]; !used {
			problems = append(problems, fmt.Sprintf("the %s of component %q has an entry for chart %s, which "+
				"neither the registry, a leaf recipe nor the BOM deploys. Delete it", bom.DeclaredImagesFile, d.Component, k))
		}
	}
	for _, m := range d.Manifests {
		if _, used := usedManifests[m.OperatorImage]; !used {
			problems = append(problems, fmt.Sprintf("the %s of component %q has an entry for operator image %s, "+
				"which no deployed manifest runs. Delete it", bom.DeclaredImagesFile, d.Component, m.OperatorImage))
		}
	}
	return problems
}

func TestDeclaredProblems(t *testing.T) {
	d, err := bom.ParseDeclaredImages("op", []byte(`component: op
charts:
  - repository: oci://example.com/charts
    chart: op
    sourceVersion: 1.0.0
    images: [{image: "example.com/op/initc:1.0.0", source: env}]
manifests:
  - operatorImage: example.com/op/operator:v1
    images: [{image: "alpine:3.10", source: compiled-in default}]
`))
	if err != nil {
		t.Fatalf("ParseDeclaredImages() error = %v", err)
	}
	current := declaredChartKey{Repository: "oci://example.com/charts", Chart: "op", Version: "1.0.0"}
	bumped := declaredChartKey{Repository: "oci://example.com/charts", Chart: "op", Version: "2.0.0"}
	opV1 := manifestPin{images: []string{"example.com/op/operator:v1"}, by: "leaf recipe a"}
	opV2 := manifestPin{images: []string{"example.com/op/operator:v2"}, by: "leaf recipe b"}

	tests := []struct {
		name string
		pins *componentPins
		want []string // substrings, one per problem, in order
	}{
		{"every pin declared", &componentPins{
			charts:    map[declaredChartKey][]string{current: {"the registry", "leaf recipe a"}},
			manifests: []manifestPin{opV1},
		}, nil},
		{"chart bump without a declaration", &componentPins{
			charts:    map[declaredChartKey][]string{bumped: {"the registry"}},
			manifests: []manifestPin{opV1},
		}, []string{"no entry for chart oci://example.com/charts op@2.0.0, deployed by the registry",
			"entry for chart oci://example.com/charts op@1.0.0, which neither"}},
		{"second pin without a declaration", &componentPins{
			charts:    map[declaredChartKey][]string{current: {"the registry"}, bumped: {"leaf recipe b"}},
			manifests: []manifestPin{opV1},
		}, []string{"no entry for chart oci://example.com/charts op@2.0.0, deployed by leaf recipe b"}},
		{"manifest operator image bump", &componentPins{
			charts:    map[declaredChartKey][]string{current: {"the registry"}},
			manifests: []manifestPin{opV2},
		}, []string{"manifests deployed by leaf recipe b: ",
			"operator image example.com/op/operator:v1, which no deployed manifest runs"}},
		{"nothing deploys the component", &componentPins{charts: map[declaredChartKey][]string{}},
			[]string{"which neither the registry, a leaf recipe nor the BOM deploys", "which no deployed manifest runs"}},
		{"BOM version resolves from the registry repository", &componentPins{
			charts:    map[declaredChartKey][]string{},
			manifests: []manifestPin{opV1},
			bom:       []declaredChartKey{{Repository: "https://registry.example.com", Chart: "example/op", Version: "1.0.0"}},
		}, nil},
		{"BOM version with no entry", &componentPins{
			charts:    map[declaredChartKey][]string{current: {"the registry"}},
			manifests: []manifestPin{opV1},
			bom:       []declaredChartKey{bumped},
		}, []string{"the BOM surveys chart oci://example.com/charts op@2.0.0, which make bom-docs cannot resolve"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := declaredProblems(d, tt.pins)
			if len(got) != len(tt.want) {
				t.Fatalf("declaredProblems() = %q, want %d problems containing %q", got, len(tt.want), tt.want)
			}
			for i, want := range tt.want {
				if !strings.Contains(got[i], want) {
					t.Errorf("declaredProblems()[%d] = %q, want it to contain %q", i, got[i], want)
				}
			}
		})
	}
}
