// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/eidos/pkg/bundler/checksum"
	"github.com/NVIDIA/eidos/pkg/bundler/deployer/shared"
	"github.com/NVIDIA/eidos/pkg/errors"
	"github.com/NVIDIA/eidos/pkg/recipe"
)

//go:embed templates/README.md.tmpl
var readmeTemplate string

//go:embed templates/component-README.md.tmpl
var componentReadmeTemplate string

//go:embed templates/deploy.sh.tmpl
var deployScriptTemplate string

//go:embed templates/undeploy.sh.tmpl
var undeployScriptTemplate string

// criteriaAny is the wildcard value for criteria fields.
const criteriaAny = "any"

// ComponentData contains data for rendering per-component templates.
type ComponentData struct {
	Name         string
	Namespace    string
	Repository   string
	ChartName    string
	Version      string // Original version string (preserves 'v' prefix) for helm install --version
	ChartVersion string // Normalized version (no 'v' prefix) for chart metadata labels
	HasManifests bool
	HasChart     bool
	IsOCI        bool
}

// GeneratorInput contains all data needed to generate a per-component Helm bundle.
type GeneratorInput struct {
	// RecipeResult contains the recipe metadata and component references.
	RecipeResult *recipe.RecipeResult

	// ComponentValues maps component names to their values.
	// These are collected from individual bundlers.
	ComponentValues map[string]map[string]any

	// Version is the bundler version (from CLI/bundler version).
	Version string

	// IncludeChecksums indicates whether to generate a checksums.txt file.
	IncludeChecksums bool

	// ComponentManifests maps component name → manifest path → content.
	// Each component's manifests are placed in its own manifests/ subdirectory.
	ComponentManifests map[string]map[string][]byte
}

// GeneratorOutput contains the result of Helm bundle generation.
type GeneratorOutput struct {
	// Files contains the paths of generated files.
	Files []string

	// TotalSize is the total size of all generated files.
	TotalSize int64

	// Duration is the time taken to generate the bundle.
	Duration time.Duration

	// DeploymentSteps contains ordered deployment instructions for the user.
	DeploymentSteps []string
}

// Generator creates per-component Helm bundles from recipe results.
type Generator struct{}

// NewGenerator creates a new Helm bundle generator.
func NewGenerator() *Generator {
	return &Generator{}
}

// Generate creates a per-component Helm bundle from the given input.
func (g *Generator) Generate(ctx context.Context, input *GeneratorInput, outputDir string) (*GeneratorOutput, error) {
	start := time.Now()

	output := &GeneratorOutput{
		Files: make([]string, 0),
	}

	if input == nil || input.RecipeResult == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "input and recipe result are required")
	}

	// Create output directory
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to create output directory", err)
	}

	// Build sorted component data list (validates component names)
	components, err := g.buildComponentDataList(input)
	if err != nil {
		return nil, err
	}

	// Generate per-component directories
	files, size, err := g.generateComponentDirectories(ctx, input, components, outputDir)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to generate component directories", err)
	}
	output.Files = append(output.Files, files...)
	output.TotalSize += size

	// Generate root README.md
	readmePath, readmeSize, err := g.generateRootREADME(ctx, input, components, outputDir)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to generate README.md", err)
	}
	output.Files = append(output.Files, readmePath)
	output.TotalSize += readmeSize

	// Generate deploy.sh
	deployPath, deploySize, err := g.generateDeployScript(ctx, input, components, outputDir)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to generate deploy.sh", err)
	}
	output.Files = append(output.Files, deployPath)
	output.TotalSize += deploySize

	// Generate undeploy.sh
	undeployPath, undeploySize, err := g.generateUndeployScript(ctx, input, components, outputDir)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to generate undeploy.sh", err)
	}
	output.Files = append(output.Files, undeployPath)
	output.TotalSize += undeploySize

	// Generate checksums.txt if requested
	if input.IncludeChecksums {
		if err := checksum.GenerateChecksums(ctx, outputDir, output.Files); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				"failed to generate checksums", err)
		}
		checksumPath := checksum.GetChecksumFilePath(outputDir)
		info, statErr := os.Stat(checksumPath)
		if statErr == nil {
			output.Files = append(output.Files, checksumPath)
			output.TotalSize += info.Size()
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
func (g *Generator) buildComponentDataList(input *GeneratorInput) ([]ComponentData, error) {
	componentMap := make(map[string]recipe.ComponentRef)
	for _, ref := range input.RecipeResult.ComponentRefs {
		componentMap[ref.Name] = ref
	}

	// Sort by deployment order
	sorted := sortComponentRefsByDeploymentOrder(
		input.RecipeResult.ComponentRefs,
		input.RecipeResult.DeploymentOrder,
	)

	components := make([]ComponentData, 0, len(sorted))
	for _, ref := range sorted {
		if !shared.IsSafePathComponent(ref.Name) {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid component name %q: must not contain path separators or parent directory references", ref.Name))
		}

		hasManifests := false
		if input.ComponentManifests != nil {
			if m, ok := input.ComponentManifests[ref.Name]; ok && len(m) > 0 {
				hasManifests = true
			}
		}

		chartName := ref.Chart
		if chartName == "" {
			chartName = ref.Name
		}

		isOCI := strings.HasPrefix(ref.Source, "oci://")
		// Preserve version string as-is for deploy.sh --version flag.
		// Helm handles 'v' prefixes correctly via fuzzy matching.
		version := ref.Version

		components = append(components, ComponentData{
			Name:         ref.Name,
			Namespace:    ref.Namespace,
			Repository:   ref.Source,
			ChartName:    chartName,
			Version:      version,
			ChartVersion: normalizeVersion(ref.Version),
			HasManifests: hasManifests,
			HasChart:     ref.Source != "",
			IsOCI:        isOCI,
		})
	}

	return components, nil
}

// generateComponentDirectories creates per-component directories with values.yaml, README.md, and optional manifests.
func (g *Generator) generateComponentDirectories(ctx context.Context, input *GeneratorInput, components []ComponentData, outputDir string) ([]string, int64, error) {
	files := make([]string, 0, len(components)*3)
	var totalSize int64

	for i, comp := range components {
		select {
		case <-ctx.Done():
			return nil, 0, errors.Wrap(errors.ErrCodeInternal, "context cancelled", ctx.Err())
		default:
		}

		componentDir, err := shared.SafeJoin(outputDir, comp.Name)
		if err != nil {
			return nil, 0, err
		}
		if mkdirErr := os.MkdirAll(componentDir, 0755); mkdirErr != nil {
			return nil, 0, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to create directory for %s", comp.Name), mkdirErr)
		}

		// Write values.yaml
		values := input.ComponentValues[comp.Name]
		if values == nil {
			values = make(map[string]any)
		}
		valuesPath, valuesSize, err := shared.WriteValuesFile(values, componentDir, "values.yaml")
		if err != nil {
			return nil, 0, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to write values.yaml for %s", comp.Name), err)
		}
		files = append(files, valuesPath)
		totalSize += valuesSize

		// Write component README.md
		readmePath, readmeSize, err := shared.GenerateFromTemplate(componentReadmeTemplate, comp, componentDir, "README.md")
		if err != nil {
			return nil, 0, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to write README.md for %s", comp.Name), err)
		}
		files = append(files, readmePath)
		totalSize += readmeSize

		// Write manifests if present
		if input.ComponentManifests != nil {
			if manifests, ok := input.ComponentManifests[comp.Name]; ok && len(manifests) > 0 {
				manifestDir, manifestDirErr := shared.SafeJoin(componentDir, "manifests")
				if manifestDirErr != nil {
					return nil, 0, manifestDirErr
				}
				if err := os.MkdirAll(manifestDir, 0755); err != nil {
					return nil, 0, errors.Wrap(errors.ErrCodeInternal,
						fmt.Sprintf("failed to create manifests directory for %s", comp.Name), err)
				}

				// Sort manifest paths for deterministic output
				manifestPaths := make([]string, 0, len(manifests))
				for p := range manifests {
					manifestPaths = append(manifestPaths, p)
				}
				sort.Strings(manifestPaths)

				manifestsWritten := 0
				for _, manifestPath := range manifestPaths {
					content := manifests[manifestPath]
					filename := filepath.Base(manifestPath)
					outputPath, pathErr := shared.SafeJoin(manifestDir, filename)
					if pathErr != nil {
						return nil, 0, errors.New(errors.ErrCodeInvalidRequest,
							fmt.Sprintf("invalid manifest filename %q in component %s", filename, comp.Name))
					}

					rendered, renderErr := renderManifest(content, comp, input.ComponentValues[comp.Name])
					if renderErr != nil {
						return nil, 0, errors.WrapWithContext(errors.ErrCodeInternal, "failed to render manifest template", renderErr,
							map[string]any{"component": comp.Name, "filename": filename})
					}

					if !hasYAMLObjects(rendered) {
						slog.Debug("skipping empty manifest", "component", comp.Name, "filename", filename)
						continue
					}

					if err := os.WriteFile(outputPath, rendered, 0600); err != nil {
						return nil, 0, errors.WrapWithContext(errors.ErrCodeInternal, "failed to write manifest", err,
							map[string]any{"component": comp.Name, "filename": filename})
					}

					files = append(files, outputPath)
					totalSize += int64(len(rendered))
					manifestsWritten++

					slog.Debug("wrote manifest", "component", comp.Name, "filename", filename)
				}

				// If no manifests had content, remove the empty directory and update flag
				if manifestsWritten == 0 {
					if rmErr := os.RemoveAll(manifestDir); rmErr != nil {
						slog.Warn("failed to remove empty manifest directory", "dir", manifestDir, "error", rmErr)
					}
					components[i].HasManifests = false
				}
			}
		}
	}

	return files, totalSize, nil
}

// generateRootREADME creates the root README.md with deployment instructions.
func (g *Generator) generateRootREADME(ctx context.Context, input *GeneratorInput, components []ComponentData, outputDir string) (string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}

	// Build criteria lines
	criteriaLines := []string{}
	if input.RecipeResult.Criteria != nil {
		c := input.RecipeResult.Criteria
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

	// Build reversed component list for uninstall
	reversed := make([]ComponentData, len(components))
	for i, comp := range components {
		reversed[len(components)-1-i] = comp
	}

	data := struct {
		RecipeVersion      string
		BundlerVersion     string
		Components         []ComponentData
		ComponentsReversed []ComponentData
		Criteria           []string
		Constraints        []recipe.Constraint
	}{
		RecipeVersion:      input.RecipeResult.Metadata.Version,
		BundlerVersion:     input.Version,
		Components:         components,
		ComponentsReversed: reversed,
		Criteria:           criteriaLines,
		Constraints:        input.RecipeResult.Constraints,
	}

	readmePath, readmeSize, err := shared.GenerateFromTemplate(readmeTemplate, data, outputDir, "README.md")
	if err != nil {
		return "", 0, err
	}

	return readmePath, readmeSize, nil
}

// generateDeployScript creates the deploy.sh automation script.
func (g *Generator) generateDeployScript(ctx context.Context, input *GeneratorInput, components []ComponentData, outputDir string) (string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}

	data := struct {
		BundlerVersion string
		Components     []ComponentData
	}{
		BundlerVersion: input.Version,
		Components:     components,
	}

	deployPath, deploySize, err := shared.GenerateFromTemplate(deployScriptTemplate, data, outputDir, "deploy.sh")
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
func (g *Generator) generateUndeployScript(ctx context.Context, input *GeneratorInput, components []ComponentData, outputDir string) (string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", 0, err
	}

	// Build reversed component list for uninstall order
	reversed := make([]ComponentData, len(components))
	for i, comp := range components {
		reversed[len(components)-1-i] = comp
	}

	data := struct {
		BundlerVersion     string
		ComponentsReversed []ComponentData
	}{
		BundlerVersion:     input.Version,
		ComponentsReversed: reversed,
	}

	undeployPath, undeploySize, err := shared.GenerateFromTemplate(undeployScriptTemplate, data, outputDir, "undeploy.sh")
	if err != nil {
		return "", 0, err
	}

	// Make executable
	if err := os.Chmod(undeployPath, 0755); err != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal, "failed to set undeploy.sh permissions", err)
	}

	return undeployPath, undeploySize, nil
}

// manifestData provides Helm-compatible template data for rendering manifests.
type manifestData struct {
	ComponentData
	Values  map[string]any
	Release releaseData
	Chart   chartData
}

type releaseData struct {
	Namespace string
	Service   string
}

type chartData struct {
	Name    string
	Version string
}

// helmFuncMap returns Helm-compatible template functions for manifest rendering.
func helmFuncMap() template.FuncMap {
	return template.FuncMap{
		"toYaml": func(v any) string {
			out, err := yaml.Marshal(v)
			if err != nil {
				return ""
			}
			return strings.TrimSuffix(string(out), "\n")
		},
		"nindent": func(indent int, s string) string {
			pad := strings.Repeat(" ", indent)
			lines := strings.Split(s, "\n")
			for i, line := range lines {
				if line != "" {
					lines[i] = pad + line
				}
			}
			return "\n" + strings.Join(lines, "\n")
		},
		"toString": func(v any) string {
			return fmt.Sprintf("%v", v)
		},
		"default": func(def, val any) any {
			if val == nil {
				return def
			}
			if s, ok := val.(string); ok && s == "" {
				return def
			}
			return val
		},
	}
}

// renderManifest renders manifest content as a Go template with Helm-compatible
// data and functions. Manifests can use .Values, .Release, .Chart, and functions
// like toYaml, nindent, toString, and default.
func renderManifest(content []byte, data ComponentData, values map[string]any) ([]byte, error) {
	tmpl, err := template.New("manifest").Funcs(helmFuncMap()).Parse(string(content))
	if err != nil {
		return nil, err
	}

	md := manifestData{
		ComponentData: data,
		Values:        map[string]any{data.Name: values},
		Release: releaseData{
			Namespace: data.Namespace,
			Service:   "Helm",
		},
		Chart: chartData{
			Name:    data.ChartName,
			Version: data.ChartVersion,
		},
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, md); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// hasYAMLObjects returns true if content contains at least one YAML object
// (a non-comment, non-blank, non-separator line).
func hasYAMLObjects(content []byte) bool {
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || trimmed == "---" {
			continue
		}
		return true
	}
	return false
}

// normalizeVersion ensures version string is valid for Helm (semver without 'v' prefix for chart version)
func normalizeVersion(v string) string {
	// Remove 'v' prefix if present for chart version
	v = strings.TrimPrefix(v, "v")
	// Default to 0.1.0 if empty
	if v == "" {
		return "0.1.0"
	}
	return v
}

// SortComponentsByDeploymentOrder sorts component names according to deployment order.
func SortComponentsByDeploymentOrder(components []string, deploymentOrder []string) []string {
	orderMap := make(map[string]int)
	for i, name := range deploymentOrder {
		orderMap[name] = i
	}

	sorted := make([]string, len(components))
	copy(sorted, components)

	sort.Slice(sorted, func(i, j int) bool {
		orderI, okI := orderMap[sorted[i]]
		orderJ, okJ := orderMap[sorted[j]]
		if okI && okJ {
			return orderI < orderJ
		}
		if okI {
			return true
		}
		if okJ {
			return false
		}
		return sorted[i] < sorted[j]
	})

	return sorted
}

// sortComponentRefsByDeploymentOrder sorts component refs by deployment order.
func sortComponentRefsByDeploymentOrder(refs []recipe.ComponentRef, order []string) []recipe.ComponentRef {
	if len(order) == 0 {
		return refs
	}

	orderMap := make(map[string]int, len(order))
	for i, name := range order {
		orderMap[name] = i
	}

	sorted := make([]recipe.ComponentRef, len(refs))
	copy(sorted, refs)

	sort.SliceStable(sorted, func(i, j int) bool {
		orderI, okI := orderMap[sorted[i].Name]
		orderJ, okJ := orderMap[sorted[j].Name]

		if !okI && !okJ {
			return sorted[i].Name < sorted[j].Name
		}
		if !okI {
			return false
		}
		if !okJ {
			return true
		}
		return orderI < orderJ
	})

	return sorted
}
