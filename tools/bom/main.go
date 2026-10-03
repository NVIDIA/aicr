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

// Command bom renders every Helm chart in recipes/registry.yaml at its pinned
// version and emits a CycloneDX 1.6 JSON BOM plus a Markdown summary listing
// every container image AICR can deploy.
//
// Usage: bom -repo-root <path> -out-dir <path>
//
// Outputs:
//
//	<out-dir>/bom.cdx.json
//	<out-dir>/bom.md
package main

import (
	"context"
	stderrors "errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	cdx "github.com/CycloneDX/cyclonedx-go"
	"github.com/NVIDIA/aicr/pkg/bom"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/helm"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

const crdsOnly = "CRDs only"

// expectedNoImages names the Helm components whose chart legitimately renders
// no container images, with the reason. In strict mode a Helm component that
// renders zero images and is not listed here fails, and a listed component
// that starts rendering images fails too, so the list cannot go stale.
var expectedNoImages = map[string]string{
	"agentgateway-crds":          crdsOnly,
	"mariadb-operator-crds":      crdsOnly,
	"prometheus-operator-crds":   crdsOnly,
	"slinky-slurm-operator-crds": crdsOnly,
}

// renderTimeout and renderRetryBackoff are vars so tests can shrink them.
var (
	renderTimeout      = 90 * time.Second
	renderRetryBackoff = 2 * time.Second
)

const (
	renderAttempts = 3
	// Component kinds reference the shared pkg/bom identifiers so the tool and
	// the BOM renderer cannot drift on the string values.
	kindHelm      = bom.TypeHelm
	kindKustomize = bom.TypeKustomize
	kindManifest  = bom.TypeManifest
)

// pinnedVersion returns the version the BOM advertises for a component, chosen
// by its effective deployment type: a Helm component's chart defaultVersion or
// a Kustomize component's defaultTag. Manifest components have neither and
// return "". Keeping this in lockstep with the freshness test's expectation
// (tools/bom/freshness_test.go) is what lets a Kustomize component pass the
// committed-BOM check — the generator must emit the same field the test reads.
func pinnedVersion(c component) string {
	switch c.kind() {
	case kindHelm:
		return c.Helm.DefaultVersion
	case kindKustomize:
		return c.Kustomize.DefaultTag
	default:
		return ""
	}
}

func main() {
	var (
		repoRoot      string
		outDir        string
		aicrVersion   string
		skipHelm      bool
		strict        bool
		deterministic bool
		noTitle       bool
	)
	flag.StringVar(&repoRoot, "repo-root", ".", "path to the AICR repository root")
	flag.StringVar(&outDir, "out-dir", "dist/bom", "directory to write bom.cdx.json and bom.md")
	flag.StringVar(&aicrVersion, "aicr-version", "dev", "AICR version label embedded in the BOM")
	flag.BoolVar(&skipHelm, "skip-helm", false, "skip helm template rendering (only walk embedded manifests)")
	flag.BoolVar(&strict, "strict", false, "fail if any component fails to render, is missing a pinned chart version, or a Helm chart renders no images and is not in expectedNoImages")
	flag.BoolVar(&deterministic, "deterministic", false, "suppress per-run metadata (timestamps, version churn) in the Markdown output for committable artifacts")
	flag.BoolVar(&noTitle, "no-title", false, "omit the H1 title in the Markdown output so the body can be embedded as a section of a larger document")
	flag.Parse()

	renderer := helm.Default()
	if err := run(repoRoot, outDir, aicrVersion, renderer, skipHelm, strict, deterministic, noTitle); err != nil {
		fmt.Fprintln(os.Stderr, "bom:", err)
		os.Exit(1)
	}
}

func run(repoRoot, outDir, aicrVersion string, renderer helm.Renderer, skipHelm, strict, deterministic, noTitle bool) error {
	registryPath := filepath.Join(repoRoot, "recipes", "registry.yaml")
	reg, err := loadRegistry(registryPath)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "load registry", err)
	}

	enableNoImagesCheck(reg.Components, strict)

	if mkErr := os.MkdirAll(outDir, 0o755); mkErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "mkdir out-dir", mkErr)
	}

	ctx := context.Background()

	results, err := surveyComponents(ctx, repoRoot, reg.Components, renderer, skipHelm)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "survey components")
	}

	// Version variants: explicit base/overlay/mixin Helm pins that differ
	// from the registry default, derived from the recipe data itself and
	// rendered at catalog parity (issue #1611).
	sources, err := loadRecipeSources(ctx, repoRoot)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "load recipe sources")
	}
	byName := indexComponentsByName(reg.Components)
	variants, err := deriveVariants(reg, sources)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "derive variants")
	}
	for i, v := range variants {
		surveyed, surveyErr := surveyVariant(
			ctx,
			repoRoot,
			v,
			byName[v.Name],
			renderer,
			skipHelm,
		)
		if surveyErr != nil {
			return errors.PropagateOrWrap(
				surveyErr,
				errors.ErrCodeInvalidRequest,
				fmt.Sprintf("survey component variant %q@%q", v.Name, v.Version),
			)
		}
		variants[i] = surveyed
	}

	if strict {
		var hardErrs []string
		for _, r := range results {
			// Shared rule with recipe resolution (IsEffectiveChartVersion):
			// a whitespace-only or bare-"v" defaultVersion would pass an
			// empty-string check here but fail ValidateCoherence at resolve
			// time — the gate must be at least as strict as the resolver it
			// guards. Padded values are equally rejected (deployers consume
			// the version verbatim).
			if r.Type == kindHelm &&
				(!recipe.IsEffectiveChartVersion(r.Version) || r.Version != strings.TrimSpace(r.Version)) {

				hardErrs = append(hardErrs, fmt.Sprintf("%s: chart version is not pinned", r.Name))
			}
			for _, w := range r.Warnings {
				hardErrs = append(hardErrs, r.Name+": "+w)
			}
		}
		for _, v := range variants {
			for _, w := range v.Warnings {
				hardErrs = append(hardErrs, v.Name+"@"+v.Version+": "+w)
			}
		}
		if len(hardErrs) > 0 {
			sort.Strings(hardErrs)
			for _, e := range hardErrs {
				fmt.Fprintln(os.Stderr, "strict:", e)
			}
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("strict mode: %d issues", len(hardErrs)))
		}
	}

	doc, err := bom.BuildBOMWithVariants(bom.Metadata{
		Name:           "aicr",
		Version:        aicrVersion,
		Description:    "NVIDIA AI Cluster Runtime",
		ToolName:       "aicr-bom",
		ToolVersion:    aicrVersion,
		RenderFidelity: bom.RenderFidelityCatalogParity,
	}, results, variants)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "build cyclonedx bom")
	}

	jsonPath := filepath.Join(outDir, "bom.cdx.json")
	jf, err := os.Create(jsonPath) //nolint:gosec // outDir is operator-supplied
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "create "+jsonPath, err)
	}
	enc := cdx.NewBOMEncoder(jf, cdx.BOMFileFormatJSON)
	enc.SetPretty(true)
	encErr := enc.EncodeVersion(doc, cdx.SpecVersion1_6)
	closeErr := jf.Close()
	if encErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "encode cyclonedx", encErr)
	}
	if closeErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "close "+jsonPath, closeErr)
	}

	mdPath := filepath.Join(outDir, "bom.md")
	mf, err := os.Create(mdPath) //nolint:gosec // outDir is operator-supplied
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "create "+mdPath, err)
	}
	mdErr := bom.WriteMarkdownWithVariants(mf, bom.Metadata{
		Name:           "aicr",
		Version:        aicrVersion,
		Description:    "NVIDIA AI Cluster Runtime",
		Deterministic:  deterministic,
		NoTitle:        noTitle,
		RenderFidelity: bom.RenderFidelityCatalogParity,
	}, results, variants)
	closeErr = mf.Close()
	if mdErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "render markdown", mdErr)
	}
	if closeErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "close "+mdPath, closeErr)
	}

	totalImages := 0
	for _, r := range results {
		totalImages += len(r.Images)
	}
	for _, v := range variants {
		totalImages += len(v.Images)
	}
	fmt.Printf("bom: wrote %s and %s (%d components, %d variants, %d image refs)\n",
		jsonPath, mdPath, len(results), len(variants), totalImages)
	return nil
}

// enableNoImagesCheck sets the zero-image check on every component.
func enableNoImagesCheck(components []component, on bool) {
	for i := range components {
		components[i].checkNoImages = on
	}
}

// noImagesIssue reports a mismatch between the number of images a Helm chart
// rendered and the component's expectedNoImages entry, or "" when they agree.
func noImagesIssue(name string, chartImages int) string {
	reason, listed := expectedNoImages[name]
	switch {
	case chartImages == 0 && !listed:
		return "chart rendered no images. Add it to expectedNoImages if that is intended"
	case chartImages > 0 && listed:
		return fmt.Sprintf("chart renders images. Remove it from expectedNoImages (listed as %q)", reason)
	}
	return ""
}

func indexComponentsByName(components []component) map[string]component {
	byName := make(map[string]component, len(components))
	for _, c := range components {
		byName[c.Name] = c
	}
	return byName
}

func surveyComponents(
	ctx context.Context,
	repoRoot string,
	components []component,
	renderer helm.Renderer,
	skipHelm bool,
) ([]bom.ComponentResult, error) {

	results := make([]bom.ComponentResult, 0, len(components))
	for _, c := range components {
		result, err := surveyComponent(ctx, repoRoot, c, renderer, skipHelm)
		if err != nil {
			return nil, errors.PropagateOrWrap(
				err,
				errors.ErrCodeInvalidRequest,
				fmt.Sprintf("survey component %q", c.Name),
			)
		}
		results = append(results, result)
	}
	return results, nil
}

// renderWithRetry renders in, retrying render failures (chart pulls are
// network calls that fail transiently) up to renderAttempts times with linear
// backoff. Each attempt gets its own timeout, and an attempt that hit it is
// not retried, so a hung chart costs one timeout rather than three. A failure
// that survives every attempt is returned, so a missing chart still surfaces.
func renderWithRetry(ctx context.Context, r helm.Renderer, in helm.ChartInput) ([]byte, error) {
	var (
		out []byte
		err error
	)
	for attempt := 1; attempt <= renderAttempts; attempt++ {
		var timedOut bool
		out, timedOut, err = renderOnce(ctx, r, in)
		// Only ErrCodeInternal (helm exited non-zero) is worth retrying. A
		// missing helm binary or an unconfigured chart will not fix itself.
		if err == nil || timedOut || attempt == renderAttempts ||
			!stderrors.Is(err, errors.New(errors.ErrCodeInternal, "")) {

			break
		}
		slog.Warn("chart render failed, retrying", "component", in.Name, "attempt", attempt, "error", err)
		select {
		case <-ctx.Done():
			return out, err
		case <-time.After(time.Duration(attempt) * renderRetryBackoff):
		}
	}
	return out, err
}

// renderOnce runs a single render attempt and reports whether it ran out of
// time.
func renderOnce(ctx context.Context, r helm.Renderer, in helm.ChartInput) (out []byte, timedOut bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, renderTimeout)
	defer cancel()
	out, err = r.Render(ctx, in)
	return out, ctx.Err() != nil, err
}

// renderHelmComponent shells out to `helm template` for c.
func renderHelmComponent(ctx context.Context, repoRoot string, c component, r helm.Renderer) ([]byte, []string) {
	var warnings []string

	valuesPath := filepath.Join(repoRoot, "recipes", "components", c.Name, "values.yaml")
	if _, err := os.Stat(valuesPath); err != nil {
		if os.IsNotExist(err) {
			valuesPath = ""
		} else {
			warnings = append(warnings, fmt.Sprintf("stat values.yaml: %v", err))
			valuesPath = ""
		}
	}
	out, err := renderWithRetry(ctx, r, helm.ChartInput{
		Name:       c.Name,
		Chart:      c.effectiveChart(),
		Repository: c.Helm.DefaultRepository,
		Version:    c.Helm.DefaultVersion,
		Namespace:  c.Helm.DefaultNamespace,
		ValuesPath: valuesPath,
	})
	if err != nil {
		warnings = append(warnings, err.Error())
	}
	return out, warnings
}

// surveyComponent renders the component's chart (if any) and walks its
// embedded manifests directory, returning the union of image refs.
func surveyComponent(
	ctx context.Context,
	repoRoot string,
	c component,
	r helm.Renderer,
	skipHelm bool,
) (bom.ComponentResult, error) {

	version := pinnedVersion(c)
	// Repository carries the Helm chart repo or, for a Kustomize component, its
	// source; pkg/bom names the property by effective type. Chart/Namespace are
	// Helm-only and stay empty for Kustomize.
	repository := c.Helm.DefaultRepository
	if c.kind() == kindKustomize {
		repository = c.Kustomize.DefaultSource
	}
	res := bom.ComponentResult{
		Name:        c.Name,
		DisplayName: c.DisplayName,
		Type:        c.kind(),
		Repository:  repository,
		Chart:       c.effectiveChart(),
		Version:     version,
		Namespace:   c.Helm.DefaultNamespace,
		Pinned:      version != "",
	}

	images := map[string]struct{}{}

	if c.kind() == kindHelm && !skipHelm {
		out, warnings := renderHelmComponent(ctx, repoRoot, c, r)
		res.Warnings = append(res.Warnings, warnings...)
		if len(out) > 0 {
			imgs, parseErr := bom.ExtractImagesFromYAML(out)
			if parseErr != nil {
				if bom.IsInvalidStructuredImageDescriptor(parseErr) {
					return res, wrapInvalidDescriptorSurveyError(
						parseErr,
						c.Name,
						"",
					)
				}
				res.Warnings = append(res.Warnings, "parse rendered yaml: "+parseErr.Error())
			}
			for _, i := range imgs {
				images[i] = struct{}{}
			}
		}
		// images holds only chart output here, so manifest images cannot
		// mask a chart that rendered nothing.
		if c.checkNoImages && len(res.Warnings) == 0 {
			if issue := noImagesIssue(c.Name, len(images)); issue != "" {
				res.Warnings = append(res.Warnings, issue)
			}
		}
	}

	manifestsDir := filepath.Join(repoRoot, "recipes", "components", c.Name, "manifests")
	if info, err := os.Stat(manifestsDir); err == nil && info.IsDir() {
		walkErr := filepath.WalkDir(manifestsDir, func(path string, d fs.DirEntry, werr error) error {
			if werr != nil {
				return werr
			}
			if d.IsDir() {
				return nil
			}
			if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
				return nil
			}
			data, rerr := os.ReadFile(path) //nolint:gosec // path is bounded by manifestsDir under the repo root
			if rerr != nil {
				return rerr
			}
			imgs, perr := bom.ExtractImagesFromYAML(data)
			if perr != nil {
				if bom.IsInvalidStructuredImageDescriptor(perr) {
					return wrapInvalidDescriptorSurveyError(perr, c.Name, path)
				}
				res.Warnings = append(res.Warnings, fmt.Sprintf("parse %s: %v", path, perr))
				return nil
			}
			for _, i := range imgs {
				images[i] = struct{}{}
			}
			return nil
		})
		if walkErr != nil {
			if bom.IsInvalidStructuredImageDescriptor(walkErr) {
				return res, errors.PropagateOrWrap(
					walkErr,
					errors.ErrCodeInvalidRequest,
					fmt.Sprintf("survey component %q manifests", c.Name),
				)
			}
			res.Warnings = append(res.Warnings, "walk manifests: "+walkErr.Error())
		}
	}

	res.Images = make([]string, 0, len(images))
	for i := range images {
		res.Images = append(res.Images, i)
	}
	sort.Strings(res.Images)
	return res, nil
}

func wrapInvalidDescriptorSurveyError(err error, componentName, manifestPath string) error {
	errContext := map[string]any{"component": componentName}
	message := fmt.Sprintf(
		"invalid structured image descriptor in component %q rendered chart",
		componentName,
	)
	if manifestPath != "" {
		errContext["manifest"] = manifestPath
		message = fmt.Sprintf(
			"invalid structured image descriptor in component %q manifest %q",
			componentName,
			manifestPath,
		)
	}
	return errors.WrapWithContext(
		errors.ErrCodeInvalidRequest,
		message,
		err,
		errContext,
	)
}
