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
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/Masterminds/semver/v3"
	"golang.org/x/sync/errgroup"

	"github.com/NVIDIA/aicr/pkg/bom"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/helm"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const logKeyComponent = "component"

var mirrorRenderFloor = semver.MustParse(defaults.MirrorDefaultKubeVersion)

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

// mirrorCandidate is the one effective component/value model shared by
// profile-lock validation and discovery. Profile-owned values are hydrated and
// overridden once, validated, then passed unchanged to Helm rendering so the
// two boundaries cannot disagree about alias precedence or overlapping paths.
type mirrorCandidate struct {
	refs          []recipe.ComponentRef
	overrides     map[string]map[string]string
	profileValues map[string]map[string]any
}

// Discover takes a loaded RecipeResult and returns a MirrorList by
// rendering each component's chart and extracting images.
//
// Manifest and component-values reads both route through the recipe's bound
// DataProvider (rec.DataProvider()), so a recipe built against a `--data`
// overlay sees overlay-provided manifests and values consistently. When the
// recipe carries no bound provider, reads fall back to the package-global
// embedded provider inside recipe.GetManifestContentWithContext.
//
// A component whose values cannot be hydrated is a fatal error, not a warning.
// The returned MirrorList is a completeness claim — the full set of images an
// operator relocates into a disconnected registry — and a component that fails
// to hydrate renders nothing, so it would contribute zero images to an
// otherwise successful result. Transient, per-component conditions (a failed
// helm template, an unreadable manifest) remain warnings on the affected
// ComponentImages; deterministic resolution failures do not.
func (l *Lister) Discover(ctx context.Context, rec *recipe.RecipeResult) (*MirrorList, error) {
	if rec == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "recipe is required")
	}
	validated := *rec
	validated.ComponentRefs = append([]recipe.ComponentRef(nil), rec.ComponentRefs...)
	if err := validated.PrepareAndValidateWithContext(ctx); err != nil {
		return nil, err
	}
	rec = &validated

	// Build override lookup: component → path → value.
	overrideLookup := buildOverrideLookup(l.valueOverrides)
	candidate, err := prepareMirrorCandidate(ctx, rec, overrideLookup)
	if err != nil {
		return nil, err
	}

	var (
		mu      sync.Mutex
		results = make([]componentResult, 0, len(candidate.refs))
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(defaults.MirrorDiscoveryConcurrency)

	for i, compRef := range candidate.refs {
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
				values, prevalidated := candidate.profileValues[compRef.Name]
				if !prevalidated {
					// A values-load failure is deterministic and structural (a
					// missing or unreadable values file, or a provider-binding
					// problem), not something a retry fixes. Warning and
					// continuing would skip rendering entirely, so the component
					// contributes zero images while Discover still succeeds — an
					// air-gap relocation would then omit them and the component
					// would fail to pull in the disconnected environment, where
					// diagnosis is hardest. Fail closed, matching
					// prepareMirrorCandidate and the bundler.
					// Bound the read: gctx may carry no deadline (the CLI
					// root context does not), and a provider read that
					// blocks — a values file on a stalled network mount —
					// would otherwise hang discovery indefinitely. Cancel
					// eagerly rather than deferring, so the timer is released
					// before the render below rather than at goroutine exit.
					valCtx, cancelVal := context.WithTimeout(gctx, defaults.FileReadTimeout)
					var valErr error
					values, valErr = rec.GetValuesForComponentWithContext(valCtx, compRef.Name)
					cancelVal()
					if valErr != nil {
						return errors.PropagateOrWrap(valErr, errors.ErrCodeInternal,
							fmt.Sprintf("failed to load values for component %q", compRef.Name))
					}

					// Profile-owned values were already overridden and lock-
					// validated above. Other components apply the same
					// canonical/alias-resolved override map here.
					if applyErr := component.ApplyMapOverrides(
						values, candidate.overrides[compRef.Name],
					); applyErr != nil {
						return errors.WrapWithContext(errors.ErrCodeInvalidRequest,
							"failed to apply mirror value overrides",
							applyErr,
							map[string]any{logKeyComponent: compRef.Name})
					}
				}

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
						logKeyComponent, compRef.Name, "error", renderErr)
					ci.Warnings = append(ci.Warnings,
						fmt.Sprintf("helm template failed: %v", renderErr))
				} else {
					imgs, extractErr := bom.ExtractImagesFromYAML(rendered)
					if extractErr != nil {
						if bom.IsInvalidStructuredImageDescriptor(extractErr) {
							return errors.WrapWithContext(
								errors.ErrCodeInvalidRequest,
								fmt.Sprintf(
									"invalid structured image descriptor in component %q",
									compRef.Name,
								),
								extractErr,
								map[string]any{"component": compRef.Name},
							)
						}
						slog.Warn("image extraction failed",
							logKeyComponent, compRef.Name, "error", extractErr)
						ci.Warnings = append(ci.Warnings,
							fmt.Sprintf("image extraction failed: %v", extractErr))
					} else {
						allImages = append(allImages, imgs...)
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
			manifestSets := [][]string{
				compRef.ManifestFiles,
				compRef.PreManifestFiles,
			}
			for _, paths := range manifestSets {
				for _, mPath := range paths {
					if err := gctx.Err(); err != nil {
						return err
					}
					var manifestErr error
					allImages, manifestErr = extractManifestImages(
						gctx, dp, allImages, &ci, compRef.Name, mPath,
					)
					if manifestErr != nil {
						return manifestErr
					}
				}
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
// and appends extracted images to the accumulator. Read and general parse
// failures are recorded as warnings; invalid structured image descriptors are
// returned as errors so discovery cannot succeed with a known-incomplete image
// set. A nil provider falls back to the package-global embedded provider inside
// recipe.GetManifestContentWithContext.
func extractManifestImages(
	ctx context.Context,
	dp recipe.DataProvider,
	acc []string,
	ci *ComponentImages,
	compName, mPath string,
) ([]string, error) {

	content, readErr := recipe.GetManifestContentWithContext(ctx, dp, mPath)
	if readErr != nil {
		if ctx.Err() != nil {
			return acc, ctx.Err()
		}
		slog.Warn("failed to read manifest",
			logKeyComponent, compName, "path", mPath, "error", readErr)
		ci.Warnings = append(ci.Warnings,
			fmt.Sprintf("manifest read failed %s: %v", mPath, readErr))
		return acc, nil
	}
	imgs, extractErr := bom.ExtractImagesFromYAML(content)
	if extractErr != nil {
		if bom.IsInvalidStructuredImageDescriptor(extractErr) {
			return acc, errors.WrapWithContext(
				errors.ErrCodeInvalidRequest,
				fmt.Sprintf(
					"invalid structured image descriptor in component %q manifest %q",
					compName,
					mPath,
				),
				extractErr,
				map[string]any{"component": compName, "manifest": mPath},
			)
		}
		slog.Warn("manifest image extraction failed",
			"component", compName, "path", mPath, "error", extractErr)
		ci.Warnings = append(ci.Warnings,
			fmt.Sprintf("manifest image extraction failed %s: %v", mPath, extractErr))
		return acc, nil
	}
	return append(acc, imgs...), nil
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

// effectiveOverridesForComponent resolves overrides supplied under the
// canonical component name and every registry alias. It matches bundler
// precedence: the canonical name wins a same-path collision, followed by
// aliases in registry order. A registry failure is fatal whenever overrides
// are present: otherwise a valid registry-only alias could be silently
// discarded and mirror would inventory a different candidate than requested.
func effectiveOverridesForComponent(
	lookup map[string]map[string]string,
	ref recipe.ComponentRef,
	provider recipe.DataProvider,
) (map[string]string, error) {

	keys := []string{ref.Name}
	registry, err := recipe.GetComponentRegistryFor(provider)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"failed to resolve component aliases for mirror overrides")
	}
	if registry != nil {
		if cfg := registry.Get(ref.Name); cfg != nil {
			keys = append(keys, cfg.ValueOverrideKeys...)
		}
	}
	merged := make(map[string]string)
	for _, key := range slices.Backward(keys) {
		if overrides, ok := lookup[key]; ok {
			maps.Copy(merged, overrides)
		}
	}
	return merged, nil
}

func prepareMirrorCandidate(
	ctx context.Context,
	rec *recipe.RecipeResult,
	overrideLookup map[string]map[string]string,
) (*mirrorCandidate, error) {

	candidate := &mirrorCandidate{
		refs:          make([]recipe.ComponentRef, 0, len(rec.ComponentRefs)),
		overrides:     make(map[string]map[string]string, len(rec.ComponentRefs)),
		profileValues: make(map[string]map[string]any),
	}
	// The effective lock set includes the recomputed #1327 closure when
	// the profile owns advertisement, so mirror preparation hydrates and
	// guards closure-locked components exactly like declared ones.
	owned := rec.EffectiveLockSet()
	for _, ref := range rec.ComponentRefs {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, errors.Wrap(
				errors.ErrCodeTimeout, "mirror candidate preparation canceled", ctxErr)
		}
		var overrides map[string]string
		if len(overrideLookup) > 0 {
			var err error
			overrides, err = effectiveOverridesForComponent(
				overrideLookup, ref, rec.DataProvider(),
			)
			if err != nil {
				return nil, err
			}
		}
		if raw, exists := overrides[config.ComponentEnabledKey]; exists {
			enabled, parseErr := strconv.ParseBool(raw)
			if parseErr != nil {
				return nil, errors.WrapWithContext(errors.ErrCodeInvalidRequest,
					"invalid --set enabled value", parseErr,
					map[string]any{logKeyComponent: ref.Name, "value": raw})
			}
			delete(overrides, config.ComponentEnabledKey)
			if enabled && !ref.IsEnabled() {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("component %q is disabled by the recipe and cannot be re-enabled with --set",
						ref.Name))
			}
			if !enabled {
				continue
			}
		}
		if !ref.IsEnabled() {
			continue
		}

		candidate.refs = append(candidate.refs, ref)
		candidate.overrides[ref.Name] = overrides
		if _, profileOwned := owned[ref.Name]; !profileOwned {
			continue
		}

		// Same bound as the unowned path in Discover: cancel eagerly rather
		// than deferring, so timers do not accumulate across this loop.
		valCtx, cancelVal := context.WithTimeout(ctx, defaults.FileReadTimeout)
		values, valueErr := rec.GetValuesForComponentWithContext(valCtx, ref.Name)
		cancelVal()
		if valueErr != nil {
			return nil, errors.PropagateOrWrap(valueErr, errors.ErrCodeInternal,
				fmt.Sprintf("failed to load profile candidate values for component %q", ref.Name))
		}
		if applyErr := component.ApplyMapOverrides(values, overrides); applyErr != nil {
			return nil, errors.WrapWithContext(errors.ErrCodeInvalidRequest,
				"failed to apply mirror value overrides",
				applyErr,
				map[string]any{logKeyComponent: ref.Name})
		}
		candidate.profileValues[ref.Name] = values
	}
	if err := rec.ValidateProfileLock(ctx, candidate.refs, candidate.profileValues, nil); err != nil {
		return nil, err
	}
	return candidate, nil
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
// Returns defaults.MirrorDefaultKubeVersion if no valid constraint is found
// or the extracted version is below the render-safe floor.
func KubeVersionFromConstraints(constraints []recipe.Constraint) string {
	for _, c := range constraints {
		if c.Name == k8sConstraintName {
			kubeVersion := extractVersion(c.Value)
			parsedVersion, err := semver.NewVersion(kubeVersion)
			if err != nil {
				return defaults.MirrorDefaultKubeVersion
			}
			if parsedVersion.LessThan(mirrorRenderFloor) {
				return defaults.MirrorDefaultKubeVersion
			}
			return kubeVersion
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
