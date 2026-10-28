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

// Crd-inventory writes recipes/components/<name>/crds.yaml, the CRDs each
// pinned Helm chart installs, for every component whose health check asserts a
// CRD. A component gets an entry for its registry pin and for each pin a leaf
// recipe deploys, and loses its inventory when no CRD assertion needs it.
// TestHealthCheckCRDAssertionsAnchored reads these files offline.
//
// Crd-inventory reads the registry, recipes, health checks and manifests
// embedded at build time, so -repo-root must be the tree it was built from.
//
// Usage:
//
//	go run ./tools/crd-inventory            # regenerate every inventory
//	go run ./tools/crd-inventory -check     # non-zero when an inventory is stale
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/helm"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

func main() {
	var (
		repoRoot string
		check    bool
	)
	flag.StringVar(&repoRoot, "repo-root", ".", "path to the AICR repository root")
	flag.BoolVar(&check, "check", false, "compare against the committed inventories and fail when one is stale")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := generate(ctx, repoRoot, check)
	stop()
	if err != nil {
		slog.Error("crd-inventory failed", "error", err)
		os.Exit(1)
	}
}

func generate(ctx context.Context, repoRoot string, check bool) error {
	dp := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	leaves, err := recipe.ResolveLeaves(ctx, recipe.ResolveLeavesOptions{Provider: dp, Version: resolveVersion})
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "resolve leaf recipes")
	}
	return run(ctx, repoRoot, dp, leaves, helm.Default(), readImageCRDs, check)
}

// run writes under repoRoot the inventory of each component with a CRD
// assertion against an external chart in dp or leaves, and deletes every other
// inventory. If check is set, run changes nothing and reports each inventory
// that differs from the committed file. The returned error lists every
// problem found.
func run(ctx context.Context, repoRoot string, dp recipe.DataProvider, leaves []recipe.ResolvedLeaf,
	r helm.Renderer, readImage imageReader, check bool,
) error {

	reg, err := recipe.GetComponentRegistryFor(dp)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "load component registry")
	}
	all, err := collectAssertions(ctx, dp, reg, leaves)
	if err != nil {
		return err
	}

	var problems []string
	kept := sets.New[string]()
	for _, name := range slices.Sorted(maps.Keys(all)) {
		inv, found, buildErr := buildInventory(ctx, repoRoot, dp, r, readImage, name, all[name])
		if buildErr != nil {
			return buildErr
		}
		problems = append(problems, found...)
		if len(inv.Charts) == 0 {
			continue
		}
		kept.Insert(name)
		problem, writeErr := writeInventory(repoRoot, inv, check)
		if writeErr != nil {
			return writeErr
		}
		if problem != "" {
			problems = append(problems, problem)
		}
	}

	stale, err := filepath.Glob(inventoryPath(repoRoot, "*"))
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "list CRD inventories", err)
	}
	for _, path := range stale {
		name := filepath.Base(filepath.Dir(path))
		if kept.Has(name) {
			continue
		}
		if check {
			problems = append(problems, fmt.Sprintf("%s: %s belongs to no CRD assertion against a chart; "+
				"delete it with `make crd-inventory`", name, path))
			continue
		}
		if err := os.Remove(path); err != nil {
			return errors.WrapWithContext(errors.ErrCodeInternal, "delete CRD inventory", err,
				map[string]any{pathKey: path})
		}
		slog.Info("inventory deleted", "component", name)
	}

	if len(problems) > 0 {
		return errors.New(errors.ErrCodeConflict, strings.Join(problems, "\n"))
	}
	if check {
		slog.Info("every CRD inventory is current")
	}
	return nil
}

// buildInventory returns inv, the inventory of component name with an entry
// for each chart in a that installs a CRD. It also returns problems, listing
// each chart in a that installs no CRD and each asserted CRD that no entry or
// vendored manifest backs.
func buildInventory(ctx context.Context, repoRoot string, dp recipe.DataProvider, r helm.Renderer,
	readImage imageReader, name string, a *componentAssertions,
) (inv *inventory, problems []string, err error) {

	inv = &inventory{Component: name}
	if len(a.charts) == 0 {
		return inv, nil, nil
	}
	vendored, err := manifestCRDs(ctx, dp, name)
	if err != nil {
		return nil, nil, err
	}
	for _, k := range a.sortedCharts() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, nil, errors.WrapCtxErr(ctxErr, errors.ErrCodeTimeout, "crd-inventory interrupted")
		}
		ca := a.charts[k]
		entry, deriveErr := deriveChartCRDs(ctx, repoRoot, r, readImage, ca.ref)
		if deriveErr != nil {
			return nil, nil, deriveErr
		}
		if len(entry.CRDs) == 0 {
			problems = append(problems, fmt.Sprintf("%s: health checks assert CRDs %v, but chart %s installs none",
				name, slices.Sorted(maps.Keys(ca.crds)), k))
			continue
		}
		problems = append(problems, unanchored(name, ca.crds, entry, vendored)...)
		inv.Charts = append(inv.Charts, *entry)
	}
	return inv, problems, nil
}

// writeInventory writes inv under repoRoot. If check is set, it writes nothing
// and returns a problem when the committed file differs or is missing.
func writeInventory(repoRoot string, inv *inventory, check bool) (string, error) {
	rendered, err := renderInventory(inv)
	if err != nil {
		return "", err
	}
	path := inventoryPath(repoRoot, inv.Component)
	if check {
		committed, readErr := os.ReadFile(path) //nolint:gosec // inventory path under the repo root
		if readErr != nil || !bytes.Equal(committed, rendered) {
			return fmt.Sprintf("%s: %s is stale or missing; regenerate with `make crd-inventory`",
				inv.Component, path), nil
		}
		return "", nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", errors.WrapWithContext(errors.ErrCodeInternal, "create component directory", err,
			map[string]any{pathKey: path})
	}
	if err := os.WriteFile(path, rendered, 0o600); err != nil {
		return "", errors.WrapWithContext(errors.ErrCodeInternal, "write CRD inventory", err,
			map[string]any{pathKey: path})
	}
	slog.Info("inventory written", "component", inv.Component, "charts", len(inv.Charts))
	return "", nil
}

// deriveChartCRDs returns the CRDs ref's chart installs, rendered with AICR's
// values. For a component in imageCRDSources, the list also holds the CRDs its
// operator applies from its image.
func deriveChartCRDs(ctx context.Context, repoRoot string, r helm.Renderer, readImage imageReader,
	ref recipe.ComponentRef,
) (*chartCRDs, error) {

	valuesPath := filepath.Join(recipesDir(repoRoot), "components", ref.Name, "values.yaml")
	if _, err := os.Stat(valuesPath); err != nil {
		if !os.IsNotExist(err) {
			return nil, errors.WrapWithContext(errors.ErrCodeInternal, "stat values file", err,
				map[string]any{pathKey: valuesPath})
		}
		valuesPath = ""
	}

	out, err := r.Render(ctx, helm.ChartInput{
		Name:        ref.Name,
		Chart:       ref.EffectiveChart(),
		Repository:  ref.Source,
		Version:     ref.Version,
		Namespace:   ref.Namespace,
		ValuesPath:  valuesPath,
		IncludeCRDs: true,
	})
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "render chart for component "+ref.Name)
	}
	crds, err := parseCRDNames(out)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest,
			"parse rendered chart for component "+ref.Name)
	}
	entry := &chartCRDs{
		Repository:    ref.Source,
		Chart:         ref.EffectiveChart(),
		SourceVersion: ref.Version,
		CRDs:          crds,
	}

	src, fromImage := imageCRDSources[ref.Name]
	if !fromImage {
		return entry, nil
	}
	entry.Image, err = containerImage(out, src.Container)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "find CRD image for component "+ref.Name)
	}
	imageCRDs, err := readImage(ctx, entry.Image, src.Dir)
	if err != nil {
		return nil, err
	}
	entry.CRDs = slices.Compact(slices.Sorted(slices.Values(append(entry.CRDs, imageCRDs...))))
	return entry, nil
}
