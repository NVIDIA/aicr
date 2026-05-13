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

package flux

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// Output file names used across generation.
const (
	fileChart         = "Chart.yaml"
	fileConfigMap     = "configmap-values.yaml"
	fileHelmRelease   = "helmrelease.yaml"
	fileKustomization = "kustomization.yaml"
	fileReadme        = "README.md"
)

//go:embed templates/configmap-values.yaml.tmpl
var configMapTemplate string

//go:embed templates/helmrelease.yaml.tmpl
var helmReleaseTemplate string

//go:embed templates/helmrepo-source.yaml.tmpl
var helmRepoSourceTemplate string

//go:embed templates/gitrepo-source.yaml.tmpl
var gitRepoSourceTemplate string

//go:embed templates/chart.yaml.tmpl
var chartTemplate string

//go:embed templates/kustomization.yaml.tmpl
var kustomizationTemplate string

//go:embed templates/README.md.tmpl
var readmeTemplate string

// DependsOnRef is a Flux dependsOn reference to another resource.
// All HelmReleases live in flux-system, so no namespace is needed.
type DependsOnRef struct {
	Name string
}

// RootKustomizationData carries data for the root kustomization.yaml.
type RootKustomizationData struct {
	Resources []string
}

// ReadmeData carries data for the README.md template.
type ReadmeData struct {
	BundlerVersion string
	Components     []ComponentSummary
}

// ComponentSummary is used in README rendering.
type ComponentSummary struct {
	Name         string
	Type         string
	Version      string
	Namespace    string
	DependsOnStr string
}

// compile-time interface check
var _ deployer.Deployer = (*Generator)(nil)

// Generator creates Flux manifests from recipe results.
// Configure it with the required fields, then call Generate.
type Generator struct {
	// RecipeResult contains the recipe metadata and component references.
	RecipeResult *recipe.RecipeResult

	// ComponentValues maps component names to their values.
	ComponentValues map[string]map[string]any

	// Version is the generator version.
	Version string

	// RepoURL is the Git repository URL for GitRepository source CRs.
	// If empty, a placeholder URL will be used.
	RepoURL string

	// TargetRevision is the target revision for GitRepository refs (default: "main").
	TargetRevision string

	// IncludeChecksums indicates whether to generate a checksums.txt file.
	IncludeChecksums bool

	// DataFiles lists additional file paths (relative to output dir) to include
	// in checksum generation. Used for external data files copied into the bundle.
	DataFiles []string

	// ComponentManifests maps component name → manifest path → rendered bytes.
	// Drives generation of local Helm charts for manifest-only and mixed
	// components. Components without manifests do not appear in the map.
	ComponentManifests map[string]map[string][]byte

	// DynamicValues maps component names to their dynamic value paths.
	// When non-empty, dynamic paths are split from inline values into a
	// ConfigMap and referenced via spec.valuesFrom in the HelmRelease.
	DynamicValues map[string][]string

	// VendorCharts pulls upstream Helm chart bytes into the bundle at
	// bundle time so the resulting artifact is air-gap deployable.
	// Off by default. With the flag set, vendorable Helm-typed components
	// emit a local wrapper chart (Chart.yaml + charts/<chart>-<ver>.tgz)
	// and HelmRelease CRs reference the GitRepository source instead of
	// HelmRepository.
	VendorCharts bool

	// Puller fetches upstream chart bytes when VendorCharts is set. nil
	// resolves to a default *CLIChartPuller; tests inject a stub here
	// without touching package state. Ignored when VendorCharts is false.
	Puller localformat.ChartPuller

	// vendorRecords is populated by Generate when VendorCharts is on.
	// Captured here so provenance.yaml can be written after component
	// generation without re-threading the slice through every helper.
	vendorRecords []localformat.VendorRecord
}

// resolveTargetRevision returns the effective target revision, defaulting to "main".
func (g *Generator) resolveTargetRevision() string {
	if g.TargetRevision != "" {
		return g.TargetRevision
	}
	return "main"
}

// resolveRepoURL returns the effective repo URL, using a placeholder if empty.
func (g *Generator) resolveRepoURL() string {
	if g.RepoURL != "" {
		return g.RepoURL
	}
	return "https://github.com/YOUR_ORG/YOUR_REPO.git"
}

// writeTemplate renders a template to disk and tracks the file in output.
func writeTemplate(output *deployer.Output, tmpl string, data any, dir, filename, errMsg string) error {
	path, size, err := deployer.GenerateFromTemplate(tmpl, data, dir, filename)
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal, errMsg)
	}
	output.Files = append(output.Files, path)
	output.TotalSize += size
	return nil
}

// Generate produces Flux manifests in the given output directory.
func (g *Generator) Generate(ctx context.Context, outputDir string) (*deployer.Output, error) {
	start := time.Now()

	if g.RecipeResult == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "recipe result is required")
	}

	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled before generation", err)
	}

	output := &deployer.Output{}

	// Filter enabled components and sort by deployment order.
	enabledRefs := filterEnabled(g.RecipeResult.ComponentRefs)
	sortedRefs := deployer.SortComponentRefsByDeploymentOrder(enabledRefs, g.RecipeResult.DeploymentOrder)

	// Validate component names.
	for _, ref := range sortedRefs {
		if !deployer.IsSafePathComponent(ref.Name) {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("unsafe component name: %q", ref.Name))
		}
	}

	// Create sources directory.
	sourcesDir, err := deployer.SafeJoin(outputDir, "sources")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(sourcesDir, 0750); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create sources directory", err)
	}

	// Resolve the chart puller for vendored bundles.
	puller := g.Puller
	if g.VendorCharts && puller == nil {
		puller = &localformat.CLIChartPuller{}
	}

	// Collect and deduplicate sources. When vendoring, skip HelmRepository
	// sources for components that will reference vendored local charts.
	helmSources := collectHelmSources(sortedRefs, g.VendorCharts)
	gitSources := collectGitSources(g.resolveRepoURL(), g.resolveTargetRevision())

	// Write source CRs.
	if err := g.writeSources(helmSources, gitSources, sourcesDir, output); err != nil {
		return nil, err
	}

	// Track resources for root kustomization.yaml.
	// Namespace creation is handled by HelmRelease install.createNamespace: true,
	// so no separate Namespace manifests are needed.
	var resources []string

	// Add source file paths to resources list.
	resources = append(resources, sourceResourcePaths(helmSources, gitSources)...)

	// Generate per-component resources.
	for i, ref := range sortedRefs {
		if err := ctx.Err(); err != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled during component generation", err)
		}
		compResources, compErr := g.generateComponentResources(
			ctx, ref, i, sortedRefs, outputDir, helmSources, gitSources, puller, output)
		if compErr != nil {
			return nil, compErr
		}
		resources = append(resources, compResources...)
	}

	// Write root kustomization.yaml.
	sort.Strings(resources)
	if err := writeTemplate(output, kustomizationTemplate, RootKustomizationData{Resources: resources},
		outputDir, fileKustomization, "failed to write root kustomization.yaml"); err != nil {
		return nil, err
	}

	// Write README.md.
	readmeData := ReadmeData{
		BundlerVersion: deployer.NormalizeVersionWithDefault(g.Version),
		Components:     buildComponentSummaries(sortedRefs, g.ComponentManifests),
	}
	if err := writeTemplate(output, readmeTemplate, readmeData,
		outputDir, fileReadme, "failed to write README.md"); err != nil {
		return nil, err
	}

	// Emit provenance.yaml for vendored bundles. Written before
	// checksums so the audit file is itself checksummed.
	if len(g.vendorRecords) > 0 {
		provPath, provSize, provErr := localformat.WriteProvenance(ctx, outputDir, g.vendorRecords)
		if provErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				"failed to generate provenance.yaml", provErr)
		}
		output.Files = append(output.Files, provPath)
		output.TotalSize += provSize
	}

	// Add data files to output.
	if len(g.DataFiles) > 0 {
		if err := output.AddDataFiles(outputDir, g.DataFiles); err != nil {
			return nil, err
		}
	}

	// Write checksums if requested.
	if g.IncludeChecksums {
		if err := checksum.WriteChecksums(ctx, outputDir, output); err != nil {
			return nil, err
		}
	}

	output.Duration = time.Since(start)
	output.DeploymentSteps = []string{
		"Push this bundle to your Git repository",
		"Create a Flux Kustomization pointing to the bundle path",
		"Monitor reconciliation with: flux get helmreleases -A",
	}
	notes := []string{
		"Ensure Flux is installed on your cluster before applying",
	}
	if len(g.DynamicValues) > 0 {
		notes = append(notes,
			"ConfigMaps with dynamic values have been generated. Edit them before applying to customize per-cluster settings.")
	}
	if len(g.vendorRecords) > 0 {
		notes = append(notes,
			"This bundle contains vendored Helm charts. No upstream registry access is required at deploy time. See provenance.yaml for chart provenance details.")
	}
	output.DeploymentNotes = notes

	slog.Debug("flux bundle generated",
		"components", len(sortedRefs),
		"files", len(output.Files),
		"size_bytes", output.TotalSize,
		"duration", output.Duration,
	)

	return output, nil
}

// generateComponentResources generates all Flux resources for a single component
// and returns the resource paths to include in the root kustomization.yaml.
func (g *Generator) generateComponentResources(ctx context.Context, ref recipe.ComponentRef, index int,
	sortedRefs []recipe.ComponentRef, outputDir string,
	helmSources map[string]*HelmRepoSourceData, gitSources map[string]*GitRepoSourceData,
	puller localformat.ChartPuller,
	output *deployer.Output) ([]string, error) {

	compDir, err := deployer.SafeJoin(outputDir, ref.Name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(compDir, 0750); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to create component directory %s", ref.Name), err)
	}

	dependsOn := buildDependsOn(sortedRefs, index)
	hasManifests := len(g.ComponentManifests[ref.Name]) > 0
	var resources []string

	switch ref.Type { //nolint:exhaustive // only Helm is supported; default rejects others
	case recipe.ComponentTypeHelm:
		// Manifest-only Helm component: no chart or source, only manifests.
		// Package as a local Helm chart so Flux renders the templates natively.
		if ref.Chart == "" && ref.Source == "" && hasManifests {
			wroteCM, genErr := g.generateManifestHelmChart(ref.Name, ref.Name, ref.Namespace, compDir,
				g.ComponentManifests[ref.Name], gitSources, dependsOn, output)
			if genErr != nil {
				return nil, genErr
			}
			res := []string{filepath.Join(ref.Name, fileHelmRelease)}
			if wroteCM {
				res = append(res, filepath.Join(ref.Name, fileConfigMap))
			}
			return res, nil
		}

		// Vendored Helm component: pull chart tarball, write wrapper,
		// reference GitRepository instead of HelmRepository. Mixed
		// components still produce a separate -post inline chart for
		// manifests (the existing flow handles them correctly).
		if g.VendorCharts && isVendorable(ref) {
			wroteCM, rec, vendErr := g.generateVendoredHelmComponent(
				ctx, ref, compDir, dependsOn, gitSources, puller, output)
			if vendErr != nil {
				return nil, vendErr
			}
			g.vendorRecords = append(g.vendorRecords, rec)
			resources = append(resources, filepath.Join(ref.Name, fileHelmRelease))
			if wroteCM {
				resources = append(resources, filepath.Join(ref.Name, fileConfigMap))
			}
			slog.Info("wrote vendored chart for flux",
				"component", ref.Name,
				"chart", rec.Chart, "version", rec.Version, "sha256", rec.SHA256)
		} else {
			wroteCM, helmErr := g.generateHelmComponent(ref, compDir, dependsOn, helmSources, output)
			if helmErr != nil {
				return nil, helmErr
			}
			resources = append(resources, filepath.Join(ref.Name, fileHelmRelease))
			if wroteCM {
				resources = append(resources, filepath.Join(ref.Name, fileConfigMap))
			}
		}

		// Handle mixed components (Helm + manifests).
		// Post-manifests are packaged as a local Helm chart with dependsOn
		// referencing the primary HelmRelease — same for both vendored and
		// non-vendored paths.
		if hasManifests {
			postName := ref.Name + "-post"
			postDir, postErr := deployer.SafeJoin(outputDir, postName)
			if postErr != nil {
				return nil, postErr
			}
			if postErr := os.MkdirAll(postDir, 0750); postErr != nil {
				return nil, errors.Wrap(errors.ErrCodeInternal,
					fmt.Sprintf("failed to create post directory %s", postName), postErr)
			}

			postDependsOn := []DependsOnRef{{Name: ref.Name}}
			postWroteCM, postGenErr := g.generateManifestHelmChart(ref.Name, postName, ref.Namespace, postDir,
				g.ComponentManifests[ref.Name], gitSources, postDependsOn, output)
			if postGenErr != nil {
				return nil, postGenErr
			}
			resources = append(resources, filepath.Join(postName, fileHelmRelease))
			if postWroteCM {
				resources = append(resources, filepath.Join(postName, fileConfigMap))
			}
		}

	default:
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported component type %q for component %q", ref.Type, ref.Name))
	}

	return resources, nil
}

// isVendorable maps a ComponentRef to the localformat.ShouldVendor predicate.
func isVendorable(ref recipe.ComponentRef) bool {
	return localformat.ShouldVendor(localformat.Component{
		Name:       ref.Name,
		Repository: ref.Source,
		Tag:        ref.Tag,
		Path:       ref.Path,
	})
}

// writeSources writes HelmRepository and GitRepository source CRs to the sources directory.
func (g *Generator) writeSources(helmSources map[string]*HelmRepoSourceData,
	gitSources map[string]*GitRepoSourceData, sourcesDir string, output *deployer.Output) error {

	// Write Helm sources in sorted order.
	for _, key := range slices.Sorted(maps.Keys(helmSources)) {
		src := helmSources[key]
		filename := fmt.Sprintf("helmrepo-%s.yaml", src.Name)
		if err := writeTemplate(output, helmRepoSourceTemplate, src, sourcesDir, filename,
			fmt.Sprintf("failed to write HelmRepository source %s", src.Name)); err != nil {
			return err
		}
	}

	// Write Git sources in sorted order.
	for _, key := range slices.Sorted(maps.Keys(gitSources)) {
		src := gitSources[key]
		filename := fmt.Sprintf("gitrepo-%s.yaml", src.Name)
		if err := writeTemplate(output, gitRepoSourceTemplate, src, sourcesDir, filename,
			fmt.Sprintf("failed to write GitRepository source %s", src.Name)); err != nil {
			return err
		}
	}

	return nil
}

// filterEnabled returns only the components that are enabled for deployment.
func filterEnabled(refs []recipe.ComponentRef) []recipe.ComponentRef {
	enabled := make([]recipe.ComponentRef, 0, len(refs))
	for _, ref := range refs {
		if ref.IsEnabled() {
			enabled = append(enabled, ref)
		}
	}
	return enabled
}

// buildDependsOn returns a dependsOn reference to the immediate predecessor
// in the deployment order. All components produce HelmReleases, so every
// component at index N depends on component at index N-1.
func buildDependsOn(sortedRefs []recipe.ComponentRef, index int) []DependsOnRef {
	if index == 0 {
		return nil
	}
	prev := sortedRefs[index-1]
	return []DependsOnRef{{Name: prev.Name}}
}

// nonAlphanumericRe collapses runs of non-DNS characters into a single hyphen.
var nonAlphanumericRe = regexp.MustCompile(`[^a-z0-9-]+`)

// sanitizeSourceName converts a URL to a Kubernetes-safe DNS-1123 label
// by stripping the scheme and common suffixes, then replacing everything
// non-alphanumeric with hyphens, truncated to 63 characters.
func sanitizeSourceName(rawURL string) string {
	// Strip scheme prefixes so "https" doesn't appear in the name.
	s := strings.ToLower(rawURL)
	for _, prefix := range []string{"oci://", "https://", "http://"} {
		s = strings.TrimPrefix(s, prefix)
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	s = strings.Trim(nonAlphanumericRe.ReplaceAllString(s, "-"), "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	if s == "" {
		return "default-source"
	}
	return s
}

// sourceName looks up a pre-computed name from the source map, falling back to sanitizeSourceName.
func sourceName[V any](sourceURL string, sources map[string]V, nameFunc func(V) string) string {
	if src, ok := sources[sourceURL]; ok {
		return nameFunc(src)
	}
	return sanitizeSourceName(sourceURL)
}

// sourceResourcePaths returns sorted resource paths for all source CRs.
func sourceResourcePaths(helmSources map[string]*HelmRepoSourceData, gitSources map[string]*GitRepoSourceData) []string {
	paths := make([]string, 0, len(helmSources)+len(gitSources))
	for _, src := range helmSources {
		paths = append(paths, filepath.Join("sources", fmt.Sprintf("helmrepo-%s.yaml", src.Name)))
	}
	for _, src := range gitSources {
		paths = append(paths, filepath.Join("sources", fmt.Sprintf("gitrepo-%s.yaml", src.Name)))
	}
	sort.Strings(paths)
	return paths
}

// buildComponentSummaries builds the component summary list for the README.
func buildComponentSummaries(sortedRefs []recipe.ComponentRef, manifests map[string]map[string][]byte) []ComponentSummary {
	summaries := make([]ComponentSummary, 0, len(sortedRefs))
	for i, ref := range sortedRefs {
		version := ref.Version
		if version == "" {
			version = ref.Tag
		}

		dependsOnStr := "-"
		if deps := buildDependsOn(sortedRefs, i); len(deps) > 0 {
			dependsOnStr = deps[0].Name
		}

		summaries = append(summaries, ComponentSummary{
			Name:         ref.Name,
			Type:         "HelmRelease",
			Version:      version,
			Namespace:    ref.Namespace,
			DependsOnStr: dependsOnStr,
		})

		// Mixed components (Helm chart + manifests) produce a post HelmRelease
		// that depends on the primary.
		isMixed := ref.Chart != "" && ref.Source != "" && len(manifests[ref.Name]) > 0
		if isMixed {
			summaries = append(summaries, ComponentSummary{
				Name:         ref.Name + "-post",
				Type:         "HelmRelease",
				Namespace:    ref.Namespace,
				DependsOnStr: ref.Name,
			})
		}
	}
	return summaries
}
