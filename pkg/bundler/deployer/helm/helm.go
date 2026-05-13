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

package helm

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

//go:embed templates/README.md.tmpl
var readmeTemplate string

//go:embed templates/deploy.sh.tmpl
var deployScriptTemplate string

//go:embed templates/undeploy.sh.tmpl
var undeployScriptTemplate string

// criteriaAny is the wildcard value for criteria fields.
const criteriaAny = "any"

// ComponentData contains data for rendering per-component template blocks.
// The helm deployer no longer owns per-component folder content (localformat
// does). ComponentData now carries only the fields needed by the orchestration
// templates: README.md's component table and deploy.sh / undeploy.sh
// name-matched special-case blocks.
type ComponentData struct {
	Name       string
	Namespace  string
	Repository string
	ChartName  string
	Version    string // Original version string (preserves 'v' prefix) for helm install --version
	IsOCI      bool
	Tag        string // Git ref for Kustomize-typed components (tag/branch/commit)
	Path       string // Path within the repository to the kustomization
}

// compile-time interface check
var _ deployer.Deployer = (*Generator)(nil)

// Generator creates per-component Helm bundles from recipe results.
// Configure it with the required fields, then call Generate.
type Generator struct {
	// RecipeResult contains the recipe metadata and component references.
	RecipeResult *recipe.RecipeResult

	// ComponentValues maps component names to their values.
	// These are collected from individual bundlers.
	ComponentValues map[string]map[string]any

	// Version is the bundler version (from CLI/bundler version).
	Version string

	// IncludeChecksums indicates whether to generate a checksums.txt file.
	IncludeChecksums bool

	// ComponentPreManifests maps component name → manifest path → content
	// for manifests that apply BEFORE each component's primary chart.
	// Forwarded to localformat.Options.ComponentPreManifests. Populated
	// from ComponentRef.PreManifestFiles.
	ComponentPreManifests map[string]map[string][]byte

	// ComponentPostManifests maps component name → manifest path → content
	// for manifests that apply AFTER each component's primary chart.
	// Each component's manifests are placed in its own manifests/ subdirectory.
	// Populated from ComponentRef.ManifestFiles.
	ComponentPostManifests map[string]map[string][]byte

	// DataFiles lists additional file paths (relative to output dir) to include
	// in checksum generation. Used for external data files copied into the bundle.
	DataFiles []string

	// DynamicValues maps component names to their dynamic value paths.
	// These paths are removed from values.yaml and written to cluster-values.yaml.
	DynamicValues map[string][]string

	// VendorCharts pulls upstream Helm chart bytes into the bundle at
	// bundle time so the resulting artifact is air-gap deployable.
	// Off by default. See pkg/bundler/deployer/localformat for the
	// vendoring shape (single wrapped folder per Helm component, with
	// charts/<chart>-<version>.tgz adjacent to a wrapper Chart.yaml).
	VendorCharts bool

	// vendorRecords is populated by Generate when VendorCharts is on.
	// Captured here so generateProvenanceFile can write provenance.yaml
	// without re-threading the slice through every helper call. The
	// field is unset (nil) when VendorCharts is off.
	vendorRecords []localformat.VendorRecord
}

// Generate creates a per-component Helm bundle from the configured generator fields.
// Per-component folder content (Chart.yaml, values.yaml, install.sh, templates/*)
// is delegated to pkg/bundler/deployer/localformat. The helm deployer owns only
// the top-level orchestration: README.md, deploy.sh, undeploy.sh, and checksums.
func (g *Generator) Generate(ctx context.Context, outputDir string) (*deployer.Output, error) {
	start := time.Now()

	output := &deployer.Output{
		Files: make([]string, 0),
	}

	if g.RecipeResult == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "RecipeResult is required")
	}

	// Create output directory
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to create output directory", err)
	}

	// Build sorted component data list (validates component names)
	components, err := g.buildComponentDataList()
	if err != nil {
		return nil, err
	}

	// Map ComponentData to localformat.Component and write per-component folders.
	// localformat owns: folder naming, values.yaml/cluster-values.yaml split,
	// Chart.yaml, templates/*, install.sh. The helm deployer just orchestrates.
	lfComponents := toLocalformatComponents(components, g.ComponentValues, g.DynamicValues)
	writeResult, err := localformat.Write(ctx, localformat.Options{
		OutputDir:              outputDir,
		Components:             lfComponents,
		ComponentPreManifests:  g.ComponentPreManifests,
		ComponentPostManifests: g.ComponentPostManifests,
		VendorCharts:           g.VendorCharts,
	})
	if err != nil {
		// localformat.Write returns StructuredErrors; propagate as-is.
		return nil, err
	}
	g.vendorRecords = writeResult.VendoredCharts
	for _, f := range writeResult.Folders {
		// localformat returns paths relative to outputDir. Downstream consumers
		// (checksum.WriteChecksums, output.TotalSize, deployment reporting) all
		// expect absolute paths, so resolve each entry via SafeJoin before
		// appending. SafeJoin also enforces containment.
		for _, rel := range f.Files {
			abs, joinErr := deployer.SafeJoin(outputDir, rel)
			if joinErr != nil {
				return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("path from localformat escapes outputDir: %s", rel), joinErr)
			}
			output.Files = append(output.Files, abs)
			if info, statErr := os.Stat(abs); statErr == nil {
				output.TotalSize += info.Size()
			}
		}
	}

	// Generate root README.md
	readmePath, readmeSize, err := g.generateRootREADME(ctx, components, outputDir)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to generate README.md", err)
	}
	output.Files = append(output.Files, readmePath)
	output.TotalSize += readmeSize

	// Generate deploy.sh
	deployPath, deploySize, err := g.generateDeployScript(ctx, components, outputDir)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to generate deploy.sh", err)
	}
	output.Files = append(output.Files, deployPath)
	output.TotalSize += deploySize

	// Generate undeploy.sh
	undeployPath, undeploySize, err := g.generateUndeployScript(ctx, components, outputDir)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to generate undeploy.sh", err)
	}
	output.Files = append(output.Files, undeployPath)
	output.TotalSize += undeploySize

	// Include external data files in the file list (for checksums)
	if err := output.AddDataFiles(outputDir, g.DataFiles); err != nil {
		return nil, err
	}

	// Generate provenance.yaml for vendored bundles. Written before
	// checksums.txt so the audit file is itself checksummed.
	if len(g.vendorRecords) > 0 {
		provPath, provSize, provErr := localformat.WriteProvenance(ctx, outputDir, g.vendorRecords)
		if provErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				"failed to generate provenance.yaml", provErr)
		}
		output.Files = append(output.Files, provPath)
		output.TotalSize += provSize
	}

	// Generate checksums.txt if requested
	if g.IncludeChecksums {
		if err := checksum.WriteChecksums(ctx, outputDir, output); err != nil {
			return nil, err
		}
	}

	output.Duration = time.Since(start)

	// Populate deployment steps for CLI output
	output.DeploymentSteps = []string{
		fmt.Sprintf("cd %s", outputDir),
		"chmod +x deploy.sh",
		"./deploy.sh",
	}

	slog.Debug("helm bundle generated",
		"files", len(output.Files),
		"total_size", output.TotalSize,
		"duration", output.Duration,
	)

	return output, nil
}

// buildComponentDataList builds a sorted list of ComponentData from the recipe.
// It validates that all component names are safe for use as directory names.
// Only the fields consumed by the orchestration templates are populated.
func (g *Generator) buildComponentDataList() ([]ComponentData, error) {
	// Sort by deployment order
	sorted := deployer.SortComponentRefsByDeploymentOrder(
		g.RecipeResult.ComponentRefs,
		g.RecipeResult.DeploymentOrder,
	)

	components := make([]ComponentData, 0, len(sorted))
	for _, ref := range sorted {
		if !deployer.IsSafePathComponent(ref.Name) {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid component name %q: must not contain path separators or parent directory references", ref.Name))
		}

		chartName := ref.Chart
		if chartName == "" {
			chartName = ref.Name
		}

		components = append(components, ComponentData{
			Name:       ref.Name,
			Namespace:  ref.Namespace,
			Repository: ref.Source,
			ChartName:  chartName,
			Version:    ref.Version,
			IsOCI:      strings.HasPrefix(ref.Source, "oci://"),
			Tag:        ref.Tag,
			Path:       ref.Path,
		})
	}

	return components, nil
}

// toLocalformatComponents maps the orchestration ComponentData list to the
// per-component inputs consumed by localformat.Write. Values and DynamicPaths
// are looked up by component name from the generator's maps.
func toLocalformatComponents(
	components []ComponentData,
	values map[string]map[string]any,
	dynamic map[string][]string,
) []localformat.Component {

	out := make([]localformat.Component, 0, len(components))
	for _, c := range components {
		out = append(out, localformat.Component{
			Name:         c.Name,
			Namespace:    c.Namespace,
			Repository:   c.Repository,
			ChartName:    c.ChartName,
			Version:      c.Version,
			IsOCI:        c.IsOCI,
			Tag:          c.Tag,
			Path:         c.Path,
			Values:       values[c.Name],
			DynamicPaths: dynamic[c.Name],
		})
	}
	return out
}

// generateRootREADME creates the root README.md with deployment instructions.
func (g *Generator) generateRootREADME(ctx context.Context, components []ComponentData, outputDir string) (string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}

	// Build criteria lines
	criteriaLines := []string{}
	if g.RecipeResult.Criteria != nil {
		c := g.RecipeResult.Criteria
		if c.Service != "" && c.Service != criteriaAny {
			criteriaLines = append(criteriaLines, fmt.Sprintf("- **Service**: %s", c.Service))
		}
		if c.Accelerator != "" && c.Accelerator != criteriaAny {
			criteriaLines = append(criteriaLines, fmt.Sprintf("- **Accelerator**: %s", c.Accelerator))
		}
		if c.Intent != "" && c.Intent != criteriaAny {
			criteriaLines = append(criteriaLines, fmt.Sprintf("- **Intent**: %s", c.Intent))
		}
		if c.OS != "" && c.OS != criteriaAny {
			criteriaLines = append(criteriaLines, fmt.Sprintf("- **OS**: %s", c.OS))
		}
	}

	data := readmeTemplateData{
		RecipeVersion:      g.RecipeResult.Metadata.Version,
		BundlerVersion:     g.Version,
		Components:         components,
		ComponentsReversed: reverseComponents(components),
		Criteria:           criteriaLines,
		Constraints:        g.RecipeResult.Constraints,
	}

	readmePath, readmeSize, err := deployer.GenerateFromTemplate(readmeTemplate, data, outputDir, "README.md")
	if err != nil {
		return "", 0, err
	}

	return readmePath, readmeSize, nil
}

// generateDeployScript creates the deploy.sh automation script.
func (g *Generator) generateDeployScript(ctx context.Context, components []ComponentData, outputDir string) (string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}

	data := deployTemplateData{
		BundlerVersion: g.Version,
		Components:     components,
	}

	deployPath, deploySize, err := deployer.GenerateFromTemplate(deployScriptTemplate, data, outputDir, "deploy.sh")
	if err != nil {
		return "", 0, err
	}

	// Make executable
	if err := os.Chmod(deployPath, 0755); err != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal, "failed to set deploy.sh permissions", err)
	}

	return deployPath, deploySize, nil
}

// generateUndeployScript creates the undeploy.sh automation script.
func (g *Generator) generateUndeployScript(ctx context.Context, components []ComponentData, outputDir string) (string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}

	reversed := reverseComponents(components)
	data := undeployTemplateData{
		BundlerVersion:     g.Version,
		ComponentsReversed: reversed,
		Namespaces:         uniqueNamespaces(reversed),
	}

	undeployPath, undeploySize, err := deployer.GenerateFromTemplate(undeployScriptTemplate, data, outputDir, "undeploy.sh")
	if err != nil {
		return "", 0, err
	}

	// Make executable
	if err := os.Chmod(undeployPath, 0755); err != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal, "failed to set undeploy.sh permissions", err)
	}

	return undeployPath, undeploySize, nil
}

// readmeTemplateData is the template data for root README.md generation.
type readmeTemplateData struct {
	RecipeVersion      string
	BundlerVersion     string
	Components         []ComponentData
	ComponentsReversed []ComponentData
	Criteria           []string
	Constraints        []recipe.Constraint
}

// deployTemplateData is the template data for deploy.sh generation.
type deployTemplateData struct {
	BundlerVersion string
	Components     []ComponentData
}

// undeployTemplateData is the template data for undeploy.sh generation.
type undeployTemplateData struct {
	BundlerVersion     string
	ComponentsReversed []ComponentData
	Namespaces         []string // unique namespaces in reverse-deployment order
}

// reverseComponents returns a reversed copy of the component list (for uninstall order).
func reverseComponents(components []ComponentData) []ComponentData {
	reversed := make([]ComponentData, len(components))
	for i, comp := range components {
		reversed[len(components)-1-i] = comp
	}
	return reversed
}

// uniqueNamespaces returns deduplicated namespaces from all components,
// preserving order. Every component in the uniform local-chart format is a
// helm release with a namespace — no more per-kind filtering needed.
func uniqueNamespaces(components []ComponentData) []string {
	seen := make(map[string]bool)
	var namespaces []string
	for _, c := range components {
		if c.Namespace != "" && !seen[c.Namespace] {
			seen[c.Namespace] = true
			namespaces = append(namespaces, c.Namespace)
		}
	}
	return namespaces
}
