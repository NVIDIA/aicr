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

// Crd-inventory writes recipes/components/<name>/crds.yaml, the CRDs the pinned
// Helm chart installs, for every component whose health check asserts a CRD.
// TestHealthCheckCRDAssertionsAnchored checks the assertions against these
// files offline. The registry, health checks and manifests are read from the
// recipe data embedded at build time, so -repo-root must be the tree the tool
// was built from.
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
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

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
	dp := recipe.NewEmbeddedDataProvider(recipe.GetEmbeddedFS(), "")
	err := run(ctx, repoRoot, dp, helm.Default(), readImageCRDs, check)
	stop()
	if err != nil {
		slog.Error("crd-inventory failed", "error", err)
		os.Exit(1)
	}
}

// run writes, under repoRoot, the inventory of every Helm component in dp
// whose health check asserts a CRD. If check is set, run writes nothing and
// compares each inventory with the committed file. The returned error lists
// every problem found.
func run(ctx context.Context, repoRoot string, dp recipe.DataProvider, r helm.Renderer, readImage imageReader,
	check bool,
) error {

	reg, err := recipe.GetComponentRegistryFor(dp)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "load component registry")
	}

	var problems []string
	for i := range reg.Components {
		c := &reg.Components[i]
		if err := ctx.Err(); err != nil {
			return errors.WrapCtxErr(err, errors.ErrCodeTimeout, "crd-inventory interrupted")
		}
		ref, hasChart := chartRef(c)
		if !hasChart {
			continue
		}
		asserted, err := assertedCRDs(ctx, dp, c)
		if err != nil {
			return err
		}
		if len(asserted) == 0 {
			continue
		}

		inv, err := deriveInventory(ctx, repoRoot, r, readImage, ref)
		if err != nil {
			return err
		}
		if len(inv.CRDs) == 0 {
			problems = append(problems, fmt.Sprintf("%s: health check asserts CRDs %v, but chart %s %s installs none",
				c.Name, asserted, inv.Chart, inv.SourceVersion))
			continue
		}
		vendored, err := manifestCRDs(ctx, dp, c.Name)
		if err != nil {
			return err
		}
		for _, name := range asserted {
			if !slices.Contains(inv.CRDs, name) && !slices.Contains(vendored, name) {
				problems = append(problems, fmt.Sprintf(
					"%s: health check asserts CRD %q, which neither chart %s %s nor the component's manifests install",
					c.Name, name, inv.Chart, inv.SourceVersion))
			}
		}

		rendered, err := renderInventory(inv)
		if err != nil {
			return err
		}
		path := inventoryPath(repoRoot, c.Name)
		if check {
			committed, readErr := os.ReadFile(path) //nolint:gosec // inventory path under the repo root
			if readErr != nil || !bytes.Equal(committed, rendered) {
				problems = append(problems, fmt.Sprintf("%s: %s is stale or missing; regenerate with `make crd-inventory`",
					c.Name, path))
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return errors.WrapWithContext(errors.ErrCodeInternal, "create component directory", err,
				map[string]any{pathKey: path})
		}
		if err := os.WriteFile(path, rendered, 0o600); err != nil {
			return errors.WrapWithContext(errors.ErrCodeInternal, "write CRD inventory", err,
				map[string]any{pathKey: path})
		}
		slog.Info("inventory written", "component", c.Name, "version", inv.SourceVersion, "crds", len(inv.CRDs))
	}

	if len(problems) > 0 {
		return errors.New(errors.ErrCodeConflict, strings.Join(problems, "\n"))
	}
	if check {
		slog.Info("every CRD inventory is current")
	}
	return nil
}

// deriveInventory returns the inventory of ref's chart rendered with AICR's
// values. For a component in imageCRDSources, the inventory also lists the
// CRDs its operator applies from its image.
func deriveInventory(ctx context.Context, repoRoot string, r helm.Renderer, readImage imageReader,
	ref recipe.ComponentRef,
) (*inventory, error) {

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
	inv := &inventory{
		Component:     ref.Name,
		Chart:         ref.EffectiveChart(),
		SourceVersion: ref.Version,
		CRDs:          crds,
	}

	src, fromImage := imageCRDSources[ref.Name]
	if !fromImage {
		return inv, nil
	}
	inv.Image, err = containerImage(out, src.Container)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "find CRD image for component "+ref.Name)
	}
	imageCRDs, err := readImage(ctx, inv.Image, src.Dir)
	if err != nil {
		return nil, err
	}
	inv.CRDs = slices.Compact(slices.Sorted(slices.Values(append(inv.CRDs, imageCRDs...))))
	return inv, nil
}
