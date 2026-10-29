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

package resolver

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/NVIDIA/aicr/pkg/operator/aicrjob"
	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
	"github.com/NVIDIA/aicr/pkg/operator/plan"
	"github.com/NVIDIA/aicr/pkg/operator/registry"
)

// ReasonChartSourceUnsupported marks a release whose rendered directory holds
// neither an upstream chart reference nor a local chart.
const ReasonChartSourceUnsupported = "ChartSourceUnsupported"

// ChartDelivery serves every chart from the runtime registry:
// upstream charts are mirrored with their digests verified, local charts are
// packaged deterministically.
type ChartDelivery interface {
	MirrorHTTPChart(ctx context.Context, up registry.UpstreamChart, target registry.Repository) (*registry.MirroredChart, error)
	MirrorOCIChart(ctx context.Context, up registry.UpstreamChart, target registry.Repository) (*registry.MirroredChart, error)
	PushWrapper(ctx context.Context, target registry.Repository, files map[string][]byte) (*registry.MirroredChart, error)
}

// BuildInput is everything Build needs beyond the bundle.
type BuildInput struct {
	Bundle *registry.Bundle
	// BundleURI is the bundle's pinned reference, recorded as provenance.
	BundleURI string
	// DeclaredVersion is the installation's aicr.version.
	DeclaredVersion string
	RuntimeUID      string
	Engine          plan.Engine
	EnginePolicy    string
	// Charts is the runtime registry repository mirrored charts go below.
	Charts registry.Repository
	// Wrappers is the repository wrapper charts go below, one per release.
	Wrappers    registry.Repository
	InputDigest string
	Provenance  plan.Provenance
	// Recipe identifies the recipe Job's RecipeResult and its ConfigMap.
	Recipe *plan.Recipe
}

// Built is an assembled plan and the content it references: each release's
// values and, with a recipe, the bundle's recipe that validation reads.
type Built struct {
	Plan             *plan.Document
	Values           []*plan.Content
	ValidationRecipe *plan.Content
	// ChartDefaults are each component's chart default values, from its
	// primary release's archive, for the override path check.
	ChartDefaults map[string]map[string]any
}

// Build assembles the plan for a rendered bundle: releases in
// AICR's order with inferred phases, dependency edges from the bundle's own
// recipe, merged values, and each chart mirrored into the runtime registry
// and pinned by digest. It fails closed: any contract mismatch is an error.
func Build(ctx context.Context, delivery ChartDelivery, in *BuildInput) (*Built, error) {
	info, err := aicrjob.ParseBundleInfo(in.Bundle.Files[aicrjob.BundleInfoFile], in.DeclaredVersion)
	if err != nil {
		return nil, err
	}
	recipeData := in.Bundle.Files[info.Build.Recipe.Path]
	recipe, err := info.ParseBundleRecipe(recipeData, in.DeclaredVersion)
	if err != nil {
		return nil, err
	}
	components := make(map[string]*aicrjob.ComponentRef, len(recipe.ComponentRefs))
	for i := range recipe.ComponentRefs {
		components[recipe.ComponentRefs[i].Name] = &recipe.ComponentRefs[i]
	}
	byComponent := map[string][]*aicrjob.BundleRelease{}
	for i := range info.Layout.Releases {
		r := &info.Layout.Releases[i]
		byComponent[r.Component] = append(byComponent[r.Component], r)
	}

	built := &Built{ChartDefaults: map[string]map[string]any{}, Plan: &plan.Document{
		SchemaVersion: plan.SchemaVersion,
		RuntimeUID:    in.RuntimeUID,
		Engine:        in.Engine,
		EnginePolicy:  in.EnginePolicy,
		InputDigest:   in.InputDigest,
		Provenance:    in.Provenance,
	}}
	if in.Recipe != nil {
		// The bundle's recipe drops what the render flags turned off, so
		// validation never requires a component the bundle does not deploy.
		recipeJSON, jsonErr := info.BundleRecipeJSON(recipeData)
		if jsonErr != nil {
			return nil, jsonErr
		}
		built.ValidationRecipe = plan.RecipeContent(recipeJSON)
		r := *in.Recipe
		ref := built.ValidationRecipe.Ref()
		r.ValidationConfigMapRef = &ref
		built.Plan.Recipe = &r
	}
	built.Plan.Provenance.Source = plan.SourceResolved
	built.Plan.Provenance.Bundle = in.BundleURI
	for i := range info.Layout.Releases {
		br := &info.Layout.Releases[i]
		ref, ok := components[br.Component]
		if !ok {
			return nil, operrors.New(operrors.ErrCodeInvalidRequest,
				fmt.Sprintf("release %q names component %q, which the bundle recipe lacks", br.Name, br.Component))
		}
		rel, values, archive, relErr := buildRelease(ctx, delivery, in, br, ref, byComponent)
		if relErr != nil {
			return nil, relErr
		}
		built.Plan.Releases = append(built.Plan.Releases, *rel)
		built.Values = append(built.Values, values)
		if rel.Phase == plan.PhasePrimary {
			if built.ChartDefaults[rel.Component], err = chartDefaults(archive); err != nil {
				return nil, err
			}
		}
	}
	if built.Plan.DesiredStateDigest, err = plan.ComputeDesiredStateDigest(built.Plan); err != nil {
		return nil, err
	}
	if err = built.Plan.Validate(); err != nil {
		return nil, err
	}
	return built, nil
}

func buildRelease(ctx context.Context, delivery ChartDelivery, in *BuildInput, br *aicrjob.BundleRelease,
	ref *aicrjob.ComponentRef, byComponent map[string][]*aicrjob.BundleRelease) (*plan.Release, *plan.Content, []byte, error) {

	if br.Namespace == "" {
		return nil, nil, nil, operrors.New(operrors.ErrCodeInvalidRequest, fmt.Sprintf("release %q has no namespace", br.Name))
	}
	files := in.Bundle.Dir(br.Path)
	valuesJSON, err := releaseValues(files)
	if err != nil {
		return nil, nil, nil, operrors.Wrap(operrors.ErrCodeInvalidRequest, "values of release "+br.Name, err)
	}
	values := plan.ValuesContent(br.Name, valuesJSON)
	rel := &plan.Release{
		ID:              br.Name,
		Component:       br.Component,
		Phase:           br.Phase,
		DependsOn:       dependencies(br, ref, byComponent),
		TargetNamespace: br.Namespace,
		Values:          values.Ref(),
	}
	if br.Phase == plan.PhaseReadiness {
		rel.ReadinessFor = br.Component
	}
	var m *registry.MirroredChart
	switch {
	case files["upstream.env"] != nil:
		up, envErr := parseUpstreamEnv(files["upstream.env"])
		if envErr != nil {
			return nil, nil, nil, operrors.Wrap(operrors.ErrCodeInvalidRequest, "upstream.env of release "+br.Name, envErr)
		}
		if m, err = mirror(ctx, delivery, in, br, ref, up); err != nil {
			return nil, nil, nil, err
		}
		rel.Chart = plan.Chart{
			Source:     plan.ChartMirrored,
			Repository: m.Repository.URI(),
			Version:    up.Version,
			Digest:     m.Digest,
			Upstream:   &plan.UpstreamChart{Repository: upstreamRef(up), ArchiveDigest: m.ArchiveDigest},
		}
	case files["Chart.yaml"] != nil:
		if m, err = delivery.PushWrapper(ctx, in.Wrappers.Child(br.Name), files); err != nil {
			return nil, nil, nil, err
		}
		meta, metaErr := registry.ReadChartMeta(m.Archive)
		if metaErr != nil {
			return nil, nil, nil, metaErr
		}
		rel.Chart = plan.Chart{Source: plan.ChartWrapper, Repository: m.Repository.URI(), Version: meta.Version, Digest: m.Digest}
	default:
		return nil, nil, nil, unsupportedChart(br, "its directory holds neither upstream.env nor Chart.yaml")
	}
	if rel.OwnsCRDs, err = chartOwnsCRDs(m.Archive); err != nil {
		return nil, nil, nil, err
	}
	return rel, values, m.Archive, nil
}

// mirror copies an upstream chart after checking that upstream.env names the
// chart the bundle recipe pins. HTTP(S) upstreams carry the repository in
// REPO; OCI upstreams carry the full chart reference in CHART and no REPO.
func mirror(ctx context.Context, delivery ChartDelivery, in *BuildInput, br *aicrjob.BundleRelease,
	ref *aicrjob.ComponentRef, up *registry.UpstreamChart) (*registry.MirroredChart, error) {

	disagree := func() error {
		return operrors.New(operrors.ErrCodeInvalidRequest, fmt.Sprintf(
			"release %q upstream %s %s %s disagrees with the bundle recipe's %s %s from %s",
			br.Name, up.Repository, up.Chart, up.Version, ref.Chart, ref.Version, ref.Source))
	}
	if up.Version != ref.Version {
		return nil, disagree()
	}
	if up.Repository == "" {
		if up.Chart != strings.TrimSuffix(ref.Source, "/")+"/"+ref.Chart {
			return nil, disagree()
		}
		return delivery.MirrorOCIChart(ctx, *up, in.Charts)
	}
	if up.Chart != ref.Chart || up.Repository != ref.Source {
		return nil, disagree()
	}
	return delivery.MirrorHTTPChart(ctx, *up, in.Charts)
}

// upstreamRef is the upstream location a plan records for a mirrored chart.
func upstreamRef(up *registry.UpstreamChart) string {
	if up.Repository == "" {
		return up.Chart
	}
	return up.Repository
}

// dependencies returns a release's dependency edges. An injected release
// depends on its component's primary release; a primary depends on its own
// pre release and on every non-pre release of the components the recipe says
// it depends on, so a dependent waits for a dependency's readiness gate.
func dependencies(br *aicrjob.BundleRelease, ref *aicrjob.ComponentRef, byComponent map[string][]*aicrjob.BundleRelease) []string {
	var deps []string
	switch br.Phase {
	case plan.PhasePost, plan.PhaseReadiness:
		return []string{br.Component}
	case plan.PhasePrimary:
		for _, r := range byComponent[br.Component] {
			if r.Phase == plan.PhasePre {
				deps = append(deps, r.Name)
			}
		}
	case plan.PhasePre:
	}
	for _, c := range ref.DependencyRefs {
		for _, r := range byComponent[c] {
			if r.Phase != plan.PhasePre {
				deps = append(deps, r.Name)
			}
		}
	}
	slices.Sort(deps)
	return slices.Compact(deps)
}

func unsupportedChart(br *aicrjob.BundleRelease, why string) error {
	return operrors.New(operrors.ErrCodeInvalidRequest, fmt.Sprintf("release %q: %s", br.Name, why)).
		WithReason(ReasonChartSourceUnsupported)
}

// parseUpstreamEnv reads AICR's upstream.env: CHART, REPO and VERSION as
// shell assignments of literal values. Anything that would need a shell to
// evaluate is rejected; the operator never runs it.
func parseUpstreamEnv(raw []byte) (*registry.UpstreamChart, error) {
	vars := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %q is not an assignment", line)
		}
		switch {
		case len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'':
			value = value[1 : len(value)-1]
		case strings.ContainsAny(value, "$`\"'\\ "):
			return nil, fmt.Errorf("value of %s is not a literal", key)
		}
		if strings.Contains(value, "'") {
			return nil, fmt.Errorf("value of %s is not a literal", key)
		}
		vars[key] = value
	}
	if vars["CHART"] == "" || vars["VERSION"] == "" {
		return nil, errors.New("CHART or VERSION is missing")
	}
	return &registry.UpstreamChart{Repository: vars["REPO"], Chart: vars["CHART"], Version: vars["VERSION"]}, nil
}

// maxTemplateBytes bounds one chart file the CRD scan reads.
const maxTemplateBytes = 8 << 20

// maxNestedCharts bounds how deep the CRD scan follows packaged subcharts.
const maxNestedCharts = 2

// crdKind matches a CustomResourceDefinition declared in YAML or JSON, quoted
// or not.
var crdKind = regexp.MustCompile(`"?kind"?\s*:\s*["']?CustomResourceDefinition\b`)

// chartOwnsCRDs reports whether a chart can install CustomResourceDefinitions
// (any CRD change is a crdChange). It fails closed: a
// crds/ directory, any file but the values that declares one (a template, or
// a file a template includes), a packaged subchart that does, and anything
// the scan cannot read in full all count.
func chartOwnsCRDs(archive []byte) (bool, error) {
	return scanCRDs(archive, 0)
}

func scanCRDs(archive []byte, depth int) (bool, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return false, operrors.New(operrors.ErrCodeInvalidRequest, "chart archive is not gzip")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, operrors.New(operrors.ErrCodeInvalidRequest, "chart archive is not a tar archive")
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		name := path.Clean(h.Name)
		if slices.Contains(strings.Split(name, "/"), "crds") || h.Size > maxTemplateBytes {
			return true, nil
		}
		if base := path.Base(name); base == "values.yaml" || base == "values.yml" || base == "values.schema.json" {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxTemplateBytes))
		if err != nil {
			return false, operrors.New(operrors.ErrCodeInvalidRequest, "chart archive is truncated")
		}
		if strings.HasSuffix(name, ".tgz") {
			if depth >= maxNestedCharts {
				return true, nil
			}
			if owns, nestedErr := scanCRDs(data, depth+1); owns || nestedErr != nil {
				return true, nil
			}
			continue
		}
		if crdKind.Match(data) {
			return true, nil
		}
	}
}
