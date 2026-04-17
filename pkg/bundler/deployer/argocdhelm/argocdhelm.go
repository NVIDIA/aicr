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

// Package argocdhelm generates a Helm chart app-of-apps for ArgoCD with
// dynamic install-time values.
//
// # How it works
//
// Rather than reimplementing ArgoCD Application generation, this deployer
// delegates to the existing flat ArgoCD deployer (pkg/bundler/deployer/argocd)
// to produce proven Application manifests, then transforms the output into a
// Helm chart:
//
//  1. Generate flat ArgoCD output to a temp directory (app-of-apps.yaml,
//     per-component application.yaml + values.yaml)
//  2. For each Helm component, transform the multi-source Application manifest
//     into a single-source template with helm.values containing {{ .Values.<key> }}
//  3. Build a root values.yaml with ONLY dynamic paths (recipe defaults when
//     available, empty strings otherwise). Static values stay in chart files.
//  4. Write Chart.yaml + static/ + templates/ + values.yaml as a valid Helm chart
//
// This approach means changes to the ArgoCD deployer (new component types,
// sync policies, etc.) automatically flow through without duplication.
//
// # When this deployer is used
//
// The bundler routes here when --deployer argocd-helm is specified.
// The --dynamic flag is optional — it pre-populates specific paths in the
// root values.yaml, but all values are overridable regardless.
package argocdhelm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/argocd"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// compile-time interface check
var _ deployer.Deployer = (*Generator)(nil)

// Generator creates Helm chart app-of-apps bundles by transforming flat ArgoCD output.
// Configure it with the required fields, then call Generate.
type Generator struct {
	RecipeResult     *recipe.RecipeResult
	ComponentValues  map[string]map[string]any
	Version          string
	RepoURL          string
	TargetRevision   string
	IncludeChecksums bool

	// DynamicValues maps component names to their dynamic value paths.
	DynamicValues map[string][]string
}

// Generate creates a Helm chart app-of-apps by:
//  1. Delegating to the flat ArgoCD deployer for proven Application generation
//  2. Transforming the output into a Helm chart with {{ .Values }} references
func (g *Generator) Generate(ctx context.Context, outputDir string) (*deployer.Output, error) {
	start := time.Now()

	if g.RecipeResult == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "RecipeResult is required")
	}

	// Step 1: Generate flat ArgoCD output to a temp directory
	tmpDir, err := os.MkdirTemp("", "argocdhelm-*")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create temp directory", err)
	}
	defer os.RemoveAll(tmpDir)

	argocdGen := &argocd.Generator{
		RecipeResult:     g.RecipeResult,
		ComponentValues:  g.ComponentValues,
		Version:          g.Version,
		RepoURL:          g.RepoURL,
		TargetRevision:   g.TargetRevision,
		IncludeChecksums: false, // we generate our own checksums
	}

	if _, genErr := argocdGen.Generate(ctx, tmpDir); genErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate base ArgoCD output", genErr)
	}

	// Step 2: Create Helm chart output structure
	if mkdirErr := os.MkdirAll(outputDir, 0755); mkdirErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create output directory", mkdirErr)
	}

	output := &deployer.Output{Files: make([]string, 0)}

	// Write Chart.yaml
	chartPath, chartSize, err := writeChartYAML(outputDir, deployer.NormalizeVersionWithDefault(g.RecipeResult.Metadata.Version))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to write Chart.yaml", err)
	}
	output.Files = append(output.Files, chartPath)
	output.TotalSize += chartSize

	// Step 3: Write static values as chart files and build dynamic-only root values.yaml
	staticFiles, staticSize, dynamicOnlyValues, err := g.writeStaticValuesAndBuildStubs(outputDir)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to write static values", err)
	}
	output.Files = append(output.Files, staticFiles...)
	output.TotalSize += staticSize

	valuesPath, valuesSize, err := deployer.WriteValuesFile(dynamicOnlyValues, outputDir, "values.yaml")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to write root values.yaml", err)
	}
	output.Files = append(output.Files, valuesPath)
	output.TotalSize += valuesSize

	// Step 4: Transform Application manifests into Helm templates
	templatesDir, err := deployer.SafeJoin(outputDir, "templates")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to resolve templates directory", err)
	}
	if mkdirErr := os.MkdirAll(templatesDir, 0755); mkdirErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create templates directory", mkdirErr)
	}

	for _, ref := range g.RecipeResult.ComponentRefs {
		select {
		case <-ctx.Done():
			return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled", ctx.Err())
		default:
		}

		// Non-Helm components (Kustomize, manifest-only) have no multi-source
		// block to transform — copy their Application template as-is.
		isHelmChart := ref.Type != recipe.ComponentTypeKustomize && ref.Source != ""
		if !isHelmChart {
			tmplPath, tmplSize, cpErr := copyAsTemplate(tmpDir, templatesDir, ref.Name)
			if cpErr != nil {
				return nil, cpErr
			}
			output.Files = append(output.Files, tmplPath)
			output.TotalSize += tmplSize
			continue
		}

		overrideKey, keyErr := resolveOverrideKey(ref.Name)
		if keyErr != nil {
			return nil, keyErr
		}
		tmplPath, tmplSize, transformErr := transformApplication(tmpDir, templatesDir, ref.Name, overrideKey)
		if transformErr != nil {
			return nil, transformErr
		}
		output.Files = append(output.Files, tmplPath)
		output.TotalSize += tmplSize
	}

	// Step 5: Generate README
	readmePath, readmeSize, err := g.writeReadme(outputDir)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to write README", err)
	}
	output.Files = append(output.Files, readmePath)
	output.TotalSize += readmeSize

	// Generate checksums and finalize output
	if err := g.finalizeOutput(ctx, output, outputDir, start); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to finalize output", err)
	}

	slog.Debug("argocd helm chart generated",
		"components", len(g.RecipeResult.ComponentRefs),
		"dynamic_components", len(g.DynamicValues),
		"files", len(output.Files),
		"size_bytes", output.TotalSize,
	)

	return output, nil
}

// finalizeOutput generates checksums (if requested) and sets deployment metadata.
func (g *Generator) finalizeOutput(ctx context.Context, output *deployer.Output, outputDir string, start time.Time) error {
	if g.IncludeChecksums {
		if checksumErr := checksum.GenerateChecksums(ctx, outputDir, output.Files); checksumErr != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to generate checksums", checksumErr)
		}
		checksumPath := checksum.GetChecksumFilePath(outputDir)
		info, statErr := os.Stat(checksumPath)
		if statErr == nil {
			output.Files = append(output.Files, checksumPath)
			output.TotalSize += info.Size()
		}
	}
	output.Duration = time.Since(start)
	output.DeploymentSteps = []string{
		fmt.Sprintf("cd %s", outputDir),
		"helm install aicr-bundle .",
	}
	return nil
}

// writeStaticValuesAndBuildStubs writes each component's values to static/<name>.yaml
// and builds the dynamic-only stubs map for the root values.yaml.
func (g *Generator) writeStaticValuesAndBuildStubs(outputDir string) ([]string, int64, map[string]any, error) {
	staticDir, err := deployer.SafeJoin(outputDir, "static")
	if err != nil {
		return nil, 0, nil, err
	}
	if mkdirErr := os.MkdirAll(staticDir, 0755); mkdirErr != nil {
		return nil, 0, nil, errors.Wrap(errors.ErrCodeInternal, "failed to create static directory", mkdirErr)
	}

	var files []string
	var totalSize int64
	dynamicOnlyValues := make(map[string]any)

	for _, ref := range g.RecipeResult.ComponentRefs {
		// Skip non-Helm components (Kustomize, manifest-only)
		isHelmChart := ref.Type != recipe.ComponentTypeKustomize && ref.Source != ""
		if !isHelmChart {
			continue
		}

		values := g.ComponentValues[ref.Name]
		if values == nil {
			values = make(map[string]any)
		}

		// Deep-copy values so we can remove dynamic paths without mutating the input
		staticValues := component.DeepCopyMap(values)

		// Extract dynamic paths: remove from static, build stubs for root values.yaml.
		// When the path exists in static values, the resolved default is preserved
		// so users see what value to override. When the path doesn't exist, an
		// empty string stub is created.
		if dynPaths, ok := g.DynamicValues[ref.Name]; ok {
			overrideKey, keyErr := resolveOverrideKey(ref.Name)
			if keyErr != nil {
				return nil, 0, nil, keyErr
			}
			stubs := make(map[string]any)
			for _, path := range dynPaths {
				if val, found := component.GetValueByPath(staticValues, path); found {
					component.RemoveValueByPath(staticValues, path)
					component.SetValueByPath(stubs, path, val)
				} else {
					component.SetValueByPath(stubs, path, "")
				}
			}
			dynamicOnlyValues[overrideKey] = stubs
		}

		// Write static values (dynamic paths removed)
		staticPath, staticSize, staticErr := deployer.WriteValuesFile(staticValues, staticDir, ref.Name+".yaml")
		if staticErr != nil {
			return nil, 0, nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to write static values for %s", ref.Name), staticErr)
		}
		files = append(files, staticPath)
		totalSize += staticSize
	}

	return files, totalSize, dynamicOnlyValues, nil
}

// transformApplication reads a flat ArgoCD Application manifest, parses it as
// structured YAML, replaces the multi-source block with a single-source +
// helm.values Helm template, and writes the result as a chart template file.
func transformApplication(srcDir, templatesDir, componentName, overrideKey string) (string, int64, error) {
	if !deployer.IsSafePathComponent(componentName) {
		return "", 0, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid component name %q: must not contain path separators or parent directory references", componentName))
	}
	componentDir, joinErr := deployer.SafeJoin(srcDir, componentName)
	if joinErr != nil {
		return "", 0, joinErr
	}
	srcPath, joinErr := deployer.SafeJoin(componentDir, "application.yaml")
	if joinErr != nil {
		return "", 0, joinErr
	}
	content, err := os.ReadFile(srcPath)
	if err != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to read application.yaml for %s", componentName), err)
	}

	// Parse as structured YAML to avoid fragile regex/string manipulation.
	var app map[string]any
	if unmarshalErr := yaml.Unmarshal(content, &app); unmarshalErr != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to parse application.yaml for %s", componentName), unmarshalErr)
	}

	if transformErr := convertToSingleSourceWithValues(app, componentName, overrideKey); transformErr != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to transform application.yaml for %s", componentName), transformErr)
	}

	// Marshal to YAML, then replace the quoted values string with the
	// raw Helm template expression. yaml.Marshal wraps the template in quotes
	// (since it contains {{ }}), but ArgoCD needs the raw template text so
	// Helm can evaluate it at render time.
	out, marshalErr := yaml.Marshal(app)
	if marshalErr != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to marshal transformed application for %s", componentName), marshalErr)
	}
	out = fixValuesTemplate(out, app)

	destPath, pathErr := deployer.SafeJoin(templatesDir, componentName+".yaml")
	if pathErr != nil {
		return "", 0, pathErr
	}

	if writeErr := os.WriteFile(destPath, out, 0600); writeErr != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to write template for %s", componentName), writeErr)
	}
	return destPath, int64(len(out)), nil
}

// convertToSingleSourceWithValues mutates an ArgoCD Application map,
// replacing the multi-source "sources" block with a single "source" that
// loads static values from a chart file and merges dynamic overrides.
//
// All Helm components use the merge pattern — every value is overridable
// at install time via --set, not just paths declared with --dynamic.
func convertToSingleSourceWithValues(app map[string]any, componentName, overrideKey string) error {
	spec, ok := app["spec"].(map[string]any)
	if !ok {
		return errors.New(errors.ErrCodeInternal, "application manifest missing 'spec'")
	}

	sources, ok := spec["sources"].([]any)
	if !ok || len(sources) == 0 {
		return errors.New(errors.ErrCodeInternal, "application manifest missing 'spec.sources'")
	}

	// First source entry has the chart reference
	firstSource, ok := sources[0].(map[string]any)
	if !ok {
		return errors.New(errors.ErrCodeInternal, "first source entry is not a map")
	}

	repoURL, _ := firstSource["repoURL"].(string)
	chart, _ := firstSource["chart"].(string)
	targetRevision, _ := firstSource["targetRevision"].(string)

	if repoURL == "" || chart == "" || targetRevision == "" {
		var missing []string
		if repoURL == "" {
			missing = append(missing, "repoURL")
		}
		if chart == "" {
			missing = append(missing, "chart")
		}
		if targetRevision == "" {
			missing = append(missing, "targetRevision")
		}
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("first source entry missing fields: %s", strings.Join(missing, ", ")))
	}

	// Build the Helm template expression for values.
	// ArgoCD's spec.source.helm.values is a string field containing YAML text.
	// This template merges static values (from chart files) with dynamic overrides
	// (from .Values) at Helm render time.
	valuesTmpl := fmt.Sprintf(
		`{{- $static := (.Files.Get "static/%s.yaml") | fromYaml | default dict -}}`+"\n"+
			`{{- $dynamic := index .Values %q | default dict -}}`+"\n"+
			`{{- mustMergeOverwrite $static $dynamic | toYaml | nindent 8 }}`,
		componentName, overrideKey)

	// Replace multi-source with single source + values
	spec["source"] = map[string]any{
		"repoURL":        repoURL,
		"chart":          chart,
		"targetRevision": targetRevision,
		"helm": map[string]any{
			"values": valuesTmpl,
		},
	}
	delete(spec, "sources")

	return nil
}

// fixValuesTemplate replaces the yaml.Marshal-quoted values string with a raw
// block scalar. yaml.Marshal wraps strings containing {{ }} in quotes, but the
// output needs to be a raw Helm template that Helm evaluates at render time.
// ArgoCD's spec.source.helm.values is a string field, so |- block scalar is correct.
func fixValuesTemplate(marshaled []byte, app map[string]any) []byte {
	spec, _ := app["spec"].(map[string]any)
	source, _ := spec["source"].(map[string]any)
	helm, _ := source["helm"].(map[string]any)
	tmpl, _ := helm["values"].(string)
	if tmpl == "" {
		return marshaled
	}

	// Build the raw block scalar version
	var raw strings.Builder
	raw.WriteString("      values: |-\n")
	for _, line := range strings.Split(tmpl, "\n") {
		raw.WriteString("        " + line + "\n")
	}

	// Replace whatever yaml.Marshal produced for values with the raw version.
	result := string(marshaled)
	start := strings.Index(result, "      values:")
	if start == -1 {
		return marshaled
	}
	// Find the end of the values value (next line at indent <= 6 spaces)
	rest := result[start:]
	lines := strings.Split(rest, "\n")
	end := len(lines[0]) + 1
	for _, line := range lines[1:] {
		if line == "" || (len(line) > 0 && len(line)-len(strings.TrimLeft(line, " ")) <= 6 && strings.TrimSpace(line) != "") {
			break
		}
		end += len(line) + 1
	}

	return []byte(result[:start] + raw.String() + result[start+end:])
}

func copyAsTemplate(srcDir, templatesDir, componentName string) (string, int64, error) {
	if !deployer.IsSafePathComponent(componentName) {
		return "", 0, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid component name %q: must not contain path separators or parent directory references", componentName))
	}
	componentDir, joinErr := deployer.SafeJoin(srcDir, componentName)
	if joinErr != nil {
		return "", 0, joinErr
	}
	srcPath, joinErr := deployer.SafeJoin(componentDir, "application.yaml")
	if joinErr != nil {
		return "", 0, joinErr
	}
	content, err := os.ReadFile(srcPath)
	if err != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to read application.yaml for %s", componentName), err)
	}

	destPath, pathErr := deployer.SafeJoin(templatesDir, componentName+".yaml")
	if pathErr != nil {
		return "", 0, pathErr
	}

	if writeErr := os.WriteFile(destPath, content, 0600); writeErr != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to write template for %s", componentName), writeErr)
	}
	return destPath, int64(len(content)), nil
}

func writeChartYAML(outputDir, version string) (string, int64, error) {
	chartPath, err := deployer.SafeJoin(outputDir, "Chart.yaml")
	if err != nil {
		return "", 0, err
	}

	var buf strings.Builder
	buf.WriteString("apiVersion: v2\n")
	buf.WriteString("name: aicr-bundle\n")
	buf.WriteString("description: AICR deployment bundle with dynamic install-time values\n")
	buf.WriteString("type: application\n")
	fmt.Fprintf(&buf, "version: %s\n", version)

	content := buf.String()
	if writeErr := os.WriteFile(chartPath, []byte(content), 0600); writeErr != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal, "failed to write Chart.yaml", writeErr)
	}
	return chartPath, int64(len(content)), nil
}

func (g *Generator) writeReadme(outputDir string) (string, int64, error) {
	readmePath, err := deployer.SafeJoin(outputDir, "README.md")
	if err != nil {
		return "", 0, err
	}

	var buf strings.Builder
	buf.WriteString("# ArgoCD Helm Chart Deployment Bundle\n\n")
	buf.WriteString("This bundle is a Helm chart that generates ArgoCD Application manifests.\n")
	buf.WriteString("Dynamic values are supplied at install time using `helm install --set`.\n\n")
	buf.WriteString("## Install\n\n```bash\nhelm install aicr-bundle .")

	dynamicSetFlags, flagsErr := buildDynamicSetFlags(g.DynamicValues)
	if flagsErr != nil {
		return "", 0, flagsErr
	}
	if len(dynamicSetFlags) > 0 {
		buf.WriteString(" \\\n  " + strings.Join(dynamicSetFlags, " \\\n  "))
	}
	buf.WriteString("\n```\n")

	if len(g.DynamicValues) > 0 {
		buf.WriteString("\n## Dynamic Values\n\n")
		compNames := make([]string, 0, len(g.DynamicValues))
		for name := range g.DynamicValues {
			compNames = append(compNames, name)
		}
		sort.Strings(compNames)
		for _, name := range compNames {
			overrideKey, keyErr := resolveOverrideKey(name)
			if keyErr != nil {
				return "", 0, keyErr
			}
			for _, path := range g.DynamicValues[name] {
				fmt.Fprintf(&buf, "- `%s.%s`\n", overrideKey, path)
			}
		}
	}

	content := buf.String()
	if writeErr := os.WriteFile(readmePath, []byte(content), 0600); writeErr != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal, "failed to write README.md", writeErr)
	}
	return readmePath, int64(len(content)), nil
}

func buildDynamicSetFlags(dynamicValues map[string][]string) ([]string, error) {
	if len(dynamicValues) == 0 {
		return nil, nil
	}
	var flags []string
	compNames := make([]string, 0, len(dynamicValues))
	for name := range dynamicValues {
		compNames = append(compNames, name)
	}
	sort.Strings(compNames)
	for _, name := range compNames {
		overrideKey, keyErr := resolveOverrideKey(name)
		if keyErr != nil {
			return nil, keyErr
		}
		for _, path := range dynamicValues[name] {
			flags = append(flags, fmt.Sprintf("--set %s.%s=VALUE", overrideKey, path))
		}
	}
	return flags, nil
}

// resolveOverrideKey returns the valueOverrideKey for a component (e.g., "gpuOperator"
// for "gpu-operator"). Returns an error if the registry is unavailable or the component
// has no override keys — using the wrong key would produce a broken chart.
func resolveOverrideKey(componentName string) (string, error) {
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal,
			"failed to load component registry for override key resolution", err)
	}
	comp := registry.Get(componentName)
	if comp == nil {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q not found in registry", componentName))
	}
	if len(comp.ValueOverrideKeys) == 0 {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("component %q has no valueOverrideKeys in registry", componentName))
	}
	return comp.ValueOverrideKeys[0], nil
}
