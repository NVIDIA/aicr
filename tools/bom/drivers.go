// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/NVIDIA/aicr/pkg/bom"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/mirror"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Chart values keys of the GPU Operator driver and its GDRCopy and GDS
// sidecars that decide whether, and with which tag, a leaf recipe deploys the
// images the chart renders.
const (
	driverValuesKey         = "driver"
	gdrcopyValuesKey        = "gdrcopy"
	gdsValuesKey            = "gds"
	driverVersionKey        = "version"
	driverEnabledKey        = "enabled"
	driverUsePrecompiledKey = "usePrecompiled"
)

// catalogResolveVersion stamps the recipe builder version for leaf resolution.
const catalogResolveVersion = "bom"

// chartSite is a chart values key that carries an operator-built driver image
// (driver, gdrcopy, gds) and the version a leaf resolves for it. An empty
// version means the leaf keeps the chart default.
type chartSite struct{ key, version string }

// catalogDriverTargets records which node OS and architecture each leaf recipe
// gives the operator-built driver images the BOM surveys (issue #3663). The
// operators append those to the policy version, so the catalog lists one
// reference per target that a recipe deploying the descriptor pins.
type catalogDriverTargets struct {
	// chart maps component name and chartSite to the targets of the leaves that
	// deploy that driver image.
	chart map[string]map[chartSite][]bom.DriverTarget
	// manifests maps a recipe ManifestFiles/PreManifestFiles path to the
	// targets of the leaves that list it.
	manifests map[string][]bom.DriverTarget
}

// loadCatalogDriverTargets resolves every leaf recipe under repoRoot and
// collects the driver targets. A leaf that fails to build is an error: a
// skipped leaf could drop an OS from the BOM.
func loadCatalogDriverTargets(ctx context.Context, repoRoot string) (catalogDriverTargets, error) {
	dp, err := recipe.NewLayeredDataProvider(
		recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), ""),
		recipe.LayeredProviderConfig{ExternalDir: filepath.Join(repoRoot, "recipes"), ExternalOnly: true},
	)
	if err != nil {
		return catalogDriverTargets{}, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "open recipes data")
	}
	leaves, err := recipe.ResolveLeaves(ctx, recipe.ResolveLeavesOptions{Provider: dp, Version: catalogResolveVersion})
	if err != nil {
		return catalogDriverTargets{}, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "resolve leaf recipes")
	}

	t := catalogDriverTargets{
		chart:     map[string]map[chartSite][]bom.DriverTarget{},
		manifests: map[string][]bom.DriverTarget{},
	}
	for _, leaf := range leaves {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return catalogDriverTargets{}, errors.Wrap(errors.ErrCodeTimeout, "driver target collection canceled", ctxErr)
		}
		if leaf.Err != nil {
			return catalogDriverTargets{}, errors.PropagateOrWrap(leaf.Err, errors.ErrCodeInternal,
				fmt.Sprintf("resolve leaf recipe %q", leaf.Entry.Name))
		}
		target, _ := mirror.DriverTargetForRecipe(leaf.Result)
		for _, ref := range leaf.Result.ComponentRefs {
			if !ref.IsEnabled() {
				continue
			}
			sites, valuesErr := chartDriverSites(ctx, leaf.Result, ref.Name)
			if valuesErr != nil {
				return catalogDriverTargets{}, valuesErr
			}
			for _, site := range sites {
				if t.chart[ref.Name] == nil {
					t.chart[ref.Name] = map[chartSite][]bom.DriverTarget{}
				}
				t.chart[ref.Name][site] = addTarget(t.chart[ref.Name][site], target)
			}
			for _, path := range slices.Concat(ref.ManifestFiles, ref.PreManifestFiles) {
				t.manifests[path] = addTarget(t.manifests[path], target)
			}
		}
	}
	return t, nil
}

// chartDriverSites returns the driver images the leaf deploys from the
// component's chart: the driver at the driver.version the leaf resolves, and
// the GDRCopy and GDS sidecars the leaf enables. Nothing is deployed when the
// driver is switched off or precompiled, because the sidecars are containers
// of the driver pod. A leaf that overrides driver.repository but not the
// version still matches the registry-default chart reference; the catalog
// renders the chart at registry coordinates, so this is at catalog parity.
func chartDriverSites(ctx context.Context, rec *recipe.RecipeResult, name string) ([]chartSite, error) {
	valCtx, cancel := context.WithTimeout(ctx, defaults.FileReadTimeout)
	defer cancel()
	values, err := rec.GetValuesForComponentWithContext(valCtx, name)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			fmt.Sprintf("load values for component %q", name))
	}
	driver, ok := values[driverValuesKey].(map[string]any)
	if !ok || driver[driverEnabledKey] == false || driver[driverUsePrecompiledKey] == true {
		return nil, nil
	}
	var sites []chartSite
	if version, ok := driver[driverVersionKey]; ok {
		sites = append(sites, chartSite{driverValuesKey, fmt.Sprint(version)})
	}
	for _, key := range []string{gdrcopyValuesKey, gdsValuesKey} {
		sub, ok := values[key].(map[string]any)
		if !ok || sub[driverEnabledKey] != true {
			continue
		}
		site := chartSite{key: key}
		if version, ok := sub[driverVersionKey]; ok {
			site.version = fmt.Sprint(version)
		}
		sites = append(sites, site)
	}
	return sites, nil
}

// addTarget appends t to targets unless present, keeping them sorted.
func addTarget(targets []bom.DriverTarget, t bom.DriverTarget) []bom.DriverTarget {
	if slices.Contains(targets, t) {
		return targets
	}
	targets = append(targets, t)
	slices.SortFunc(targets, func(a, b bom.DriverTarget) int {
		return cmp.Or(cmp.Compare(a.OSID, b.OSID), cmp.Compare(a.OSVersion, b.OSVersion), cmp.Compare(a.Arch, b.Arch))
	})
	return targets
}

// qualify returns the published references for drivers found in component's
// rendered chart (manifestRel empty) or in the manifest at manifestRel. A
// chart descriptor the render disables is still listed for the leaves that
// enable it (GDS is off in the chart values but on in the bcm-training overlay). A
// driver that no recipe deploys with a resolvable tag yields a warning, which
// fails -strict, so an unqualified reference never ships silently.
func (t catalogDriverTargets) qualify(component, manifestRel string, drivers []bom.DriverImage) (images, warnings []string) {
	for _, d := range drivers {
		var targets []bom.DriverTarget
		switch {
		case manifestRel != "" && d.Disabled:
			continue // a manifest is static: a descriptor it disables is never deployed
		case manifestRel != "":
			targets = t.manifests[manifestRel]
		default:
			targets = slices.Concat(
				t.chart[component][chartSite{d.Site, bom.ParseImageRef(d.Ref).Tag}],
				t.chart[component][chartSite{key: d.Site}])
			if d.Disabled && len(targets) == 0 {
				continue // off in the render and in every leaf
			}
		}
		if bom.ParseImageRef(d.Ref).Digest != "" {
			targets = []bom.DriverTarget{{}} // a digest names one image on any OS
		}
		resolved := false
		for _, target := range targets {
			if ref, _, ok := d.Qualify(target); ok {
				resolved = true
				if !slices.Contains(images, ref) {
					images = append(images, ref)
				}
			}
		}
		if !resolved {
			warnings = append(warnings, fmt.Sprintf(
				"driver image %s: no recipe that deploys it pins one OS version and architecture; cannot list a published tag", d.Ref))
		}
	}
	return images, warnings
}

// manifestRelPath returns the recipe-relative spelling of a component manifest
// file, as written in a recipe's ManifestFiles.
func manifestRelPath(component, relToManifestsDir string) string {
	return "components/" + component + "/manifests/" + filepath.ToSlash(relToManifestsDir)
}
