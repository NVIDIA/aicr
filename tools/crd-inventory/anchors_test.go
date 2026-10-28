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
	"path"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// TestHealthCheckCRDAssertionsAnchored checks that every CRD a health check
// asserts is defined in the embedded recipe data:
//   - A component with a chart must have a crds.yaml derived from its pinned
//     chart version, and each CRD its check asserts must be in that file or in
//     its vendored manifests.
//   - A component with no chart, such as an OLM or manifest-only wrapper, may
//     assert only CRDs that some crds.yaml or vendored manifest defines.
//   - No other component may have a crds.yaml.
//
// When this test fails after a pin bump, run make crd-inventory, then fix any
// assertion it reports the new chart no longer installs.
func TestHealthCheckCRDAssertionsAnchored(t *testing.T) {
	ctx := t.Context()
	efs := recipe.GetEmbeddedFS()
	dp := recipe.NewEmbeddedDataProvider(efs, "")
	reg, loadErr := recipe.GetComponentRegistryFor(dp)
	if loadErr != nil {
		t.Fatalf("load component registry: %v", loadErr)
	}

	inTree := inTreeCRDs(t, efs, dp)
	needsInventory := sets.New[string]()
	checked := 0
	for i := range reg.Components {
		c := &reg.Components[i]
		asserted, err := assertedCRDs(ctx, dp, c)
		if err != nil {
			t.Errorf("component %q: %v", c.Name, err)
			continue
		}
		if len(asserted) == 0 {
			continue
		}
		checked += len(asserted)

		ref, hasChart := chartRef(c)
		if !hasChart {
			for _, name := range asserted {
				if !inTree.Has(name) {
					t.Errorf("%s asserts CRD %q, but component %q has no chart and no in-tree inventory or "+
						"manifest defines that CRD", c.HealthCheck.AssertFile, name, c.Name)
				}
			}
			continue
		}

		needsInventory.Insert(c.Name)
		inv, err := readInventory(ctx, dp, c.Name)
		if err != nil {
			t.Errorf("component %q asserts CRDs %v but has no readable CRD inventory (%v); run `make crd-inventory`",
				c.Name, asserted, err)
			continue
		}
		if inv.Chart != ref.EffectiveChart() || inv.SourceVersion != ref.Version {
			t.Errorf("the CRD inventory of component %q was derived from chart %s %s, but it is pinned to %s %s; "+
				"run `make crd-inventory` and fix any assertion it reports the pinned chart no longer installs",
				c.Name, inv.Chart, inv.SourceVersion, ref.EffectiveChart(), ref.Version)
			continue
		}
		vendored, err := manifestCRDs(ctx, dp, c.Name)
		if err != nil {
			t.Fatalf("component %q: %v", c.Name, err)
		}
		for _, name := range asserted {
			if !slices.Contains(inv.CRDs, name) && !slices.Contains(vendored, name) {
				t.Errorf("%s asserts CRD %q, which neither chart %s %s nor component %q's manifests install",
					c.HealthCheck.AssertFile, name, inv.Chart, inv.SourceVersion, c.Name)
			}
		}
	}

	for _, name := range inventoryComponents(t, efs) {
		if !needsInventory.Has(name) {
			t.Errorf("component %q has a CRD inventory but is not a Helm component whose health check asserts a CRD; "+
				"delete it", name)
		}
	}

	if checked == 0 {
		t.Fatal("no health check asserts a CRD; this guard would be vacuous")
	}
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
		out.Insert(inv.CRDs...)
	}
	return out
}
