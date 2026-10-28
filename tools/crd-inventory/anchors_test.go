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
	"io/fs"
	"maps"
	"path"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestHealthCheckCRDAssertionsAnchored checks that every CRD a health check
// asserts is defined in the embedded recipe data. It covers each registry
// health check at its registry pin, and each health check a leaf recipe
// deploys at the leaf's pin, including inline asserts:
//   - A CRD asserted against an external chart must be in that component's
//     crds.yaml entry for the same repository, chart and version, or in its
//     vendored manifests.
//   - A component with no chart, such as an OLM or manifest-only wrapper, may
//     assert only CRDs that some crds.yaml or vendored manifest defines.
//   - A crds.yaml may hold only entries for charts some assertion is checked
//     against.
//
// When this test fails after a pin bump, run make crd-inventory, then fix any
// assertion it reports the new chart no longer installs.
func TestHealthCheckCRDAssertionsAnchored(t *testing.T) {
	ctx := t.Context()
	efs := recipe.GetEmbeddedFS()
	dp := recipe.NewEmbeddedDataProvider(efs, "")
	reg, err := recipe.GetComponentRegistryFor(dp)
	if err != nil {
		t.Fatalf("load component registry: %v", err)
	}
	leaves, err := recipe.ResolveLeaves(ctx, recipe.ResolveLeavesOptions{Provider: dp, Version: resolveVersion})
	if err != nil {
		t.Fatalf("resolve leaf recipes: %v", err)
	}
	all, err := collectAssertions(ctx, dp, reg, leaves)
	if err != nil {
		t.Fatalf("collect CRD assertions: %v", err)
	}

	inTree := inTreeCRDs(t, efs, dp)
	const regenerate = "run `make crd-inventory` and fix any assertion it reports the pinned chart no longer installs"
	checked := 0
	for _, name := range slices.Sorted(maps.Keys(all)) {
		a := all[name]
		for _, crd := range slices.Sorted(maps.Keys(a.chartless)) {
			checked++
			if !inTree.Has(crd) {
				t.Errorf("%s asserts CRD %q, but component %q has no chart and no in-tree inventory or "+
					"manifest defines that CRD", strings.Join(sets.List(a.chartless[crd]), ", "), crd, name)
			}
		}
		if len(a.charts) == 0 {
			continue
		}

		inv, err := readInventory(ctx, dp, name)
		if err != nil {
			t.Errorf("component %q asserts CRDs against a chart but has no readable CRD inventory (%v); %s",
				name, err, regenerate)
			continue
		}
		vendored, err := manifestCRDs(ctx, dp, name)
		if err != nil {
			t.Fatalf("component %q: %v", name, err)
		}
		for _, k := range a.sortedCharts() {
			ca := a.charts[k]
			checked += len(ca.crds)
			entry := inv.find(k)
			if entry == nil {
				t.Errorf("the CRD inventory of component %q has no entry for chart %s, deployed by %s; %s",
					name, k, strings.Join(sets.List(wheres(ca.crds)), ", "), regenerate)
				continue
			}
			for _, problem := range unanchored(name, ca.crds, entry, vendored) {
				t.Error(problem)
			}
		}
		for i := range inv.Charts {
			if k := inv.Charts[i].key(); a.charts[k] == nil {
				t.Errorf("the CRD inventory of component %q has an entry for chart %s, which no CRD assertion "+
					"is checked against; run `make crd-inventory`", name, k)
			}
		}
	}

	for _, name := range inventoryComponents(t, efs) {
		if a := all[name]; a == nil || len(a.charts) == 0 {
			t.Errorf("component %q has a CRD inventory but no CRD assertion against a chart; "+
				"run `make crd-inventory` to delete it", name)
		}
	}

	if checked == 0 {
		t.Fatal("no health check asserts a CRD; this guard would be vacuous")
	}
}

// wheres returns every place that asserts a CRD in s.
func wheres(s crdSources) sets.Set[string] {
	out := sets.New[string]()
	for _, w := range s {
		out = out.Union(w)
	}
	return out
}

// readInventory returns the CRD inventory of the named component in dp.
func readInventory(ctx context.Context, dp recipe.DataProvider, componentName string) (*inventory, error) {
	p := path.Join("components", componentName, inventoryFile)
	data, err := dp.ReadFile(ctx, p)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeNotFound, "read CRD inventory "+p)
	}
	return decodeInventory(data)
}

// inventoryComponents returns the names of the components in efs that have a
// CRD inventory.
func inventoryComponents(t *testing.T, efs fs.FS) []string {
	t.Helper()
	paths, err := fs.Glob(efs, path.Join("components", "*", inventoryFile))
	if err != nil {
		t.Fatalf("glob inventories: %v", err)
	}
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		names = append(names, path.Base(path.Dir(p)))
	}
	return names
}

// inTreeCRDs returns every CRD named by a CRD inventory or defined in a
// component's vendored manifests.
func inTreeCRDs(t *testing.T, efs fs.FS, dp recipe.DataProvider) sets.Set[string] {
	t.Helper()
	dirs, err := fs.ReadDir(efs, "components")
	if err != nil {
		t.Fatalf("read components: %v", err)
	}
	out := sets.New[string]()
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		names, err := manifestCRDs(t.Context(), dp, d.Name())
		if err != nil {
			t.Fatalf("component %q: %v", d.Name(), err)
		}
		out.Insert(names...)
	}
	for _, name := range inventoryComponents(t, efs) {
		inv, err := readInventory(t.Context(), dp, name)
		if err != nil {
			t.Fatalf("component %q: %v", name, err)
		}
		for i := range inv.Charts {
			out.Insert(inv.Charts[i].CRDs...)
		}
	}
	return out
}
