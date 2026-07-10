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

package mirror

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"golang.org/x/sync/errgroup"

	"github.com/NVIDIA/aicr/pkg/bom"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/helm"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Option configures a Lister.
type Option func(*Lister)

// WithVersion sets the CLI version for metadata.
func WithVersion(v string) Option {
	return func(l *Lister) { l.version = v }
}

// WithValueOverrides sets component value overrides that affect which
// images appear in rendered charts.
func WithValueOverrides(overrides []config.ComponentPath) Option {
	return func(l *Lister) { l.valueOverrides = overrides }
}

// WithHelmRenderer sets a custom renderer (used in tests to inject
// canned YAML without requiring the helm binary).
func WithHelmRenderer(r helm.Renderer) Option {
	return func(l *Lister) { l.helmRenderer = r }
}

// Lister discovers container images and Helm charts from a recipe.
type Lister struct {
	version        string
	kubeVersion    string
	valueOverrides []config.ComponentPath
	helmRenderer   helm.Renderer
}

// WithKubeVersion sets the Kubernetes version passed to `helm template
// --kube-version`. If unset, defaults.MirrorDefaultKubeVersion is used.
func WithKubeVersion(v string) Option {
	return func(l *Lister) { l.kubeVersion = v }
}

// NewLister creates a Lister with the given options.
func NewLister(opts ...Option) *Lister {
	l := &Lister{}
	for _, opt := range opts {
		opt(l)
	}
	if l.kubeVersion == "" {
		l.kubeVersion = defaults.MirrorDefaultKubeVersion
	}
	if l.helmRenderer == nil {
		l.helmRenderer = helm.Default()
	}
	return l
}

// componentResult holds the discovery output for a single component.
type componentResult struct {
	index  int
	images ComponentImages
	chart  *ChartRef
}

// Discover takes a loaded RecipeResult and returns a MirrorList by
// rendering each component's chart and extracting images.
//
// Manifest and component-values reads both route through the recipe's bound
// DataProvider (rec.DataProvider()), so a recipe built against a `--data`
// overlay sees overlay-provided manifests and values consistently. When the
// recipe carries no bound provider, reads fall back to the package-global
// embedded provider inside recipe.GetManifestContentWithContext.
func (l *Lister) Discover(ctx context.Context, rec *recipe.RecipeResult) (*MirrorList, error) {
	if rec == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "recipe is required")
	}

	// Build override lookup: component → path → value.
	overrideLookup := buildOverrideLookup(l.valueOverrides)

	var (
		mu      sync.Mutex
		results = make([]componentResult, 0, len(rec.ComponentRefs))
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(defaults.MirrorDiscoveryConcurrency)

	for i, compRef := range rec.ComponentRefs {
		if !compRef.IsEnabled() {
			continue
		}

		g.Go(func() error {
			// Bail early if context is already canceled.
			if gctx.Err() != nil {
				return gctx.Err()
			}

			ci := ComponentImages{
				Component: compRef.Name,
				Type:      strings.ToLower(string(compRef.Type)),
			}

			var allImages []string

			// Helm components with an external chart: render it and extract
			// images. Manifest-only refs (neither chart nor source) have no
			// chart to render — their images come from the manifest scan
			// below, so invoking Helm with a fabricated chart name would only
			// produce a spurious warning.
			if compRef.Type == recipe.ComponentTypeHelm && compRef.HasExternalChart() {
				values, valErr := rec.GetValuesForComponentWithContext(gctx, compRef.Name)
				if valErr != nil {
					slog.Warn("failed to load values for component",
						"component", compRef.Name, "error", valErr)
					ci.Warnings = append(ci.Warnings,
						fmt.Sprintf("failed to load values: %v", valErr))
				} else {
					// Apply --set overrides by component name and override keys.
					applyValueOverrides(values, overrideLookup[compRef.Name])
					keyOverrides, keyErr := overridesByKey(overrideLookup, compRef)
					if keyErr != nil {
						return keyErr
					}
					applyValueOverrides(values, keyOverrides)

					// Source-only refs omit the chart name; EffectiveChart
					// applies the same component-name fallback the deployers
					// use, so the mirror inventory matches what deploys.
					rendered, renderErr := l.helmRenderer.Render(gctx, helm.ChartInput{
						Name:        compRef.Name,
						Chart:       compRef.EffectiveChart(),
						Repository:  compRef.Source,
						Version:     compRef.Version,
						Namespace:   compRef.Namespace,
						Values:      values,
						KubeVersion: l.kubeVersion,
						APIVersions: defaults.MirrorExtraAPIVersions,
					})
					if renderErr != nil {
						// Context cancellation is fatal — propagate it.
						if gctx.Err() != nil {
							return gctx.Err()
						}
						slog.Warn("helm template failed for component",
							"component", compRef.Name, "error", renderErr)
						ci.Warnings = append(ci.Warnings,
							fmt.Sprintf("helm template failed: %v", renderErr))
					} else {
						imgs, extractErr := bom.ExtractImagesFromYAML(rendered)
						if extractErr != nil {
							slog.Warn("image extraction failed",
								"component", compRef.Name, "error", extractErr)
							ci.Warnings = append(ci.Warnings,
								fmt.Sprintf("image extraction failed: %v", extractErr))
						} else {
							allImages = append(allImages, imgs...)
						}
					}
				}
			}

			// Both types: scan ManifestFiles and PreManifestFiles. Use the
			// recipe-bound DataProvider so `--data` overlays that shadow an
			// embedded manifest path are honored, matching the values-loading
			// path above. A nil provider falls back to embedded inside
			// GetManifestContentWithContext.
			if gctx.Err() != nil {
				return gctx.Err()
			}
			dp := rec.DataProvider()
			for _, mPath := range compRef.ManifestFiles {
				allImages = extractManifestImages(gctx, dp, allImages, &ci, compRef.Name, mPath)
			}
			for _, mPath := range compRef.PreManifestFiles {
				allImages = extractManifestImages(gctx, dp, allImages, &ci, compRef.Name, mPath)
			}

			slices.Sort(allImages)
			ci.Images = slices.Compact(allImages)

			// Build ChartRef for Helm components. A source-only ref has an
			// external chart whose name falls back to the component name
			// (matching the deployers); a ref with neither chart nor source
			// is manifest-only and has no chart to mirror.
			var chartRef *ChartRef
			if compRef.Type == recipe.ComponentTypeHelm && compRef.HasExternalChart() {
				chartRef = &ChartRef{
					Name:       compRef.Name,
					Repository: compRef.Source,
					Chart:      compRef.EffectiveChart(),
					Version:    compRef.Version,
					Namespace:  compRef.Namespace,
				}
			}

			mu.Lock()
			results = append(results, componentResult{
				index:  i,
				images: ci,
				chart:  chartRef,
			})
			mu.Unlock()

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "component discovery failed")
	}

	// Sort results by original deployment order.
	sortByIndex(results)

	// Assemble MirrorList.
	ml := &MirrorList{
		Components: make([]ComponentImages, 0, len(results)),
		Charts:     make([]ChartRef, 0),
		Metadata: MirrorListMetadata{
			RecipeVersion: l.version,
		},
	}

	if rec.Criteria != nil {
		ml.Metadata.Criteria = rec.Criteria.String()
	}

	var globalImages []string
	for _, r := range results {
		ml.Components = append(ml.Components, r.images)
		globalImages = append(globalImages, r.images.Images...)
		if r.chart != nil {
			ml.Charts = append(ml.Charts, *r.chart)
		}
	}
	slices.Sort(globalImages)
	ml.Images = slices.Compact(globalImages)

	return ml, nil
}

// extractManifestImages reads a manifest file from the supplied DataProvider
// and appends extracted images to the accumulator, recording warnings on
// failure. A nil provider falls back to the package-global embedded provider
// inside recipe.GetManifestContentWithContext.
func extractManifestImages(ctx context.Context, dp recipe.DataProvider, acc []string, ci *ComponentImages, compName, mPath string) []string {
	content, readErr := recipe.GetManifestContentWithContext(ctx, dp, mPath)
	if readErr != nil {
		slog.Warn("failed to read manifest",
			"component", compName, "path", mPath, "error", readErr)
		ci.Warnings = append(ci.Warnings,
			fmt.Sprintf("manifest read failed %s: %v", mPath, readErr))
		return acc
	}
	imgs, extractErr := bom.ExtractImagesFromYAML(content)
	if extractErr != nil {
		ci.Warnings = append(ci.Warnings,
			fmt.Sprintf("manifest image extraction failed %s: %v", mPath, extractErr))
		return acc
	}
	return append(acc, imgs...)
}

// buildOverrideLookup converts a slice of ComponentPath overrides into
// a nested map keyed by component name → path → value.
func buildOverrideLookup(overrides []config.ComponentPath) map[string]map[string]string {
	lookup := make(map[string]map[string]string)
	for _, cp := range overrides {
		if !cp.HasValue() {
			continue
		}
		if lookup[cp.Component] == nil {
			lookup[cp.Component] = make(map[string]string)
		}
		lookup[cp.Component][cp.Path] = *cp.Value
	}
	return lookup
}

// applyValueOverrides applies flat key=value overrides to a nested values
// map. Keys use dot-notation (e.g., "driver.version").
func applyValueOverrides(values map[string]any, overrides map[string]string) {
	for path, val := range overrides {
		setNestedValue(values, path, val)
	}
}

// setNestedValue sets a dot-separated path in a nested map.
func setNestedValue(m map[string]any, path, value string) {
	parts := strings.Split(path, ".")
	current := m
	for i, part := range parts {
		if i == len(parts)-1 {
			current[part] = component.ConvertMapValue(value)
			return
		}
		next, ok := current[part]
		if !ok {
			next = make(map[string]any)
			current[part] = next
		}
		if nextMap, ok := next.(map[string]any); ok {
			current = nextMap
		} else {
			// Overwrite non-map intermediate with a map.
			nextMap := make(map[string]any)
			current[part] = nextMap
			current = nextMap
		}
	}
}

// overridesByKey returns overrides that match a component by its
// valueOverrideKeys from the registry. This bridges the gap between
// --set flag keys (e.g., "gpuoperator:driver.version") and the
// component name (e.g., "gpu-operator").
func overridesByKey(lookup map[string]map[string]string, ref recipe.ComponentRef) (map[string]string, error) {
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "overridesByKey: failed to get component registry")
	}
	if registry == nil {
		return map[string]string{}, nil
	}
	cfg := registry.Get(ref.Name)
	if cfg == nil {
		return map[string]string{}, nil
	}
	merged := make(map[string]string)
	for _, key := range cfg.ValueOverrideKeys {
		if overrides, ok := lookup[key]; ok {
			for path, val := range overrides {
				merged[path] = val
			}
		}
	}
	return merged, nil
}

// sortByIndex sorts componentResult slices by their original index
// (deployment order).
func sortByIndex(results []componentResult) {
	slices.SortFunc(results, func(a, b componentResult) int {
		return a.index - b.index
	})
}

// k8sConstraintName is the recipe constraint name for the Kubernetes
// server version (e.g., ">= 1.32.4").
const k8sConstraintName = "K8s.server.version"

// KubeVersionFromConstraints extracts a concrete Kubernetes version from
// the recipe's K8s.server.version constraint. The constraint value is
// typically a semver range like ">= 1.32.4"; this function extracts the
// version digits so it can be passed to `helm template --kube-version`.
// Returns defaults.MirrorDefaultKubeVersion if no constraint is found.
func KubeVersionFromConstraints(constraints []recipe.Constraint) string {
	for _, c := range constraints {
		if c.Name == k8sConstraintName {
			return extractVersion(c.Value)
		}
	}
	return defaults.MirrorDefaultKubeVersion
}

// extractVersion extracts the first semver-like version (major.minor or
// major.minor.patch) from a constraint expression like ">= 1.32.4".
func extractVersion(expr string) string {
	// Strip comparison operators and whitespace.
	s := strings.TrimLeft(expr, "><=!~ ")
	// Take the first word (handles "1.32.4 <2.0" ranges).
	if idx := strings.IndexByte(s, ' '); idx >= 0 {
		s = s[:idx]
	}
	// Strip any leading 'v' prefix.
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return defaults.MirrorDefaultKubeVersion
	}
	return s
}
