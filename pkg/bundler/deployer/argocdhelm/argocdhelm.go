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
//     into a single-source template with valuesObject: {{ .Values.<key> }}
//  3. Build a root values.yaml with ONLY dynamic paths (recipe defaults when
//     available, empty strings otherwise). Static values stay in chart files.
//  4. Write Chart.yaml + static/ + templates/ + values.yaml as a valid Helm chart
//
// This approach means changes to the ArgoCD deployer (new component types,
// sync policies, etc.) automatically flow through without duplication.
//
// # When this deployer is used
//
// The bundler routes here when --deployer argocd AND --dynamic flags are both
// present. Without --dynamic, the standard flat ArgoCD deployer is used.
package argocdhelm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/argocd"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/shared"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// compile-time interface check
var _ deployer.Deployer = (*Generator)(nil)

// HasDynamicValues reports whether this deployer has dynamic install-time values.
func (g *Generator) HasDynamicValues() bool {
	return len(g.DynamicValues) > 0
}

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

// sourcesPattern matches the multi-source block in ArgoCD Application manifests.
// Used to replace it with a single-source + valuesObject for the Helm chart.
var sourcesPattern = regexp.MustCompile(`(?ms)  sources:\n.*?(?:  destination:)`)

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

	argocdInput := &argocd.GeneratorInput{
		RecipeResult:     g.RecipeResult,
		ComponentValues:  g.ComponentValues,
		Version:          g.Version,
		RepoURL:          g.RepoURL,
		TargetRevision:   g.TargetRevision,
		IncludeChecksums: false, // we generate our own checksums
	}

	argocdGen := argocd.NewGenerator()
	if _, genErr := argocdGen.Generate(ctx, argocdInput, tmpDir); genErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate base ArgoCD output", genErr)
	}

	// Step 2: Create Helm chart output structure
	if mkdirErr := os.MkdirAll(outputDir, 0755); mkdirErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create output directory", mkdirErr)
	}

	output := &deployer.Output{Files: make([]string, 0)}

	// Write Chart.yaml
	chartPath, chartSize, err := writeChartYAML(outputDir, shared.NormalizeVersionWithDefault(g.RecipeResult.Metadata.Version))
	if err != nil {
		return nil, err
	}
	output.Files = append(output.Files, chartPath)
	output.TotalSize += chartSize

	// Step 3: Write static values as chart files and build dynamic-only root values.yaml
	staticFiles, staticSize, dynamicOnlyValues, err := g.writeStaticValuesAndBuildStubs(outputDir)
	if err != nil {
		return nil, err
	}
	output.Files = append(output.Files, staticFiles...)
	output.TotalSize += staticSize

	valuesPath, valuesSize, err := shared.WriteValuesFile(dynamicOnlyValues, outputDir, "values.yaml")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to write root values.yaml", err)
	}
	output.Files = append(output.Files, valuesPath)
	output.TotalSize += valuesSize

	// Step 4: Transform Application manifests into Helm templates
	templatesDir, err := shared.SafeJoin(outputDir, "templates")
	if err != nil {
		return nil, err
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
		hasDynamic := len(g.DynamicValues[ref.Name]) > 0
		tmplPath, tmplSize, transformErr := transformApplication(tmpDir, templatesDir, ref.Name, overrideKey, hasDynamic)
		if transformErr != nil {
			return nil, transformErr
		}
		output.Files = append(output.Files, tmplPath)
		output.TotalSize += tmplSize
	}

	// Step 5: Generate README
	readmePath, readmeSize, err := g.writeReadme(outputDir)
	if err != nil {
		return nil, err
	}
	output.Files = append(output.Files, readmePath)
	output.TotalSize += readmeSize

	// Generate checksums if requested
	if g.IncludeChecksums {
		if checksumErr := checksum.GenerateChecksums(ctx, outputDir, output.Files); checksumErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate checksums", checksumErr)
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

	slog.Debug("argocd helm chart generated",
		"components", len(g.RecipeResult.ComponentRefs),
		"dynamic_components", len(g.DynamicValues),
		"files", len(output.Files),
		"size_bytes", output.TotalSize,
	)

	return output, nil
}

// writeStaticValuesAndBuildStubs writes each component's values to static/<name>.yaml
// and builds the dynamic-only stubs map for the root values.yaml.
func (g *Generator) writeStaticValuesAndBuildStubs(outputDir string) ([]string, int64, map[string]any, error) {
	staticDir, err := shared.SafeJoin(outputDir, "static")
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
		staticValues := deepCopyMap(values)

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
		staticPath, staticSize, staticErr := shared.WriteValuesFile(staticValues, staticDir, ref.Name+".yaml")
		if staticErr != nil {
			return nil, 0, nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to write static values for %s", ref.Name), staticErr)
		}
		files = append(files, staticPath)
		totalSize += staticSize
	}

	return files, totalSize, dynamicOnlyValues, nil
}

// transformApplication reads a flat ArgoCD Application manifest and converts
// its multi-source helm block to a single-source with valuesObject that loads
// static values from chart files and merges dynamic overrides from .Values.
func transformApplication(srcDir, templatesDir, componentName, overrideKey string, hasDynamic bool) (string, int64, error) {
	componentDir, joinErr := shared.SafeJoin(srcDir, componentName)
	if joinErr != nil {
		return "", 0, joinErr
	}
	srcPath, joinErr := shared.SafeJoin(componentDir, "application.yaml")
	if joinErr != nil {
		return "", 0, joinErr
	}
	content, err := os.ReadFile(srcPath)
	if err != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to read application.yaml for %s", componentName), err)
	}

	transformed, transformErr := transformMultiSourceToValuesObject(string(content), componentName, overrideKey, hasDynamic)
	if transformErr != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to transform application.yaml for %s", componentName), transformErr)
	}

	destPath, pathErr := shared.SafeJoin(templatesDir, componentName+".yaml")
	if pathErr != nil {
		return "", 0, pathErr
	}

	if writeErr := os.WriteFile(destPath, []byte(transformed), 0600); writeErr != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to write template for %s", componentName), writeErr)
	}
	return destPath, int64(len(transformed)), nil
}

// transformMultiSourceToValuesObject replaces the ArgoCD multi-source block
// with a single-source block that loads static values from chart files and
// merges dynamic overrides from .Values.
//
// Static values are loaded via .Files.Get from the chart's static/ directory.
// When the component has dynamic values, they are merged on top using
// mustMergeOverwrite so user-provided values win.
func transformMultiSourceToValuesObject(content, componentName, overrideKey string, hasDynamic bool) (string, error) {
	repoURL, chart, targetRevision, err := extractFirstSourceValues(content)
	if err != nil {
		return "", err
	}

	if !sourcesPattern.MatchString(content) {
		return "", errors.New(errors.ErrCodeInternal,
			"ArgoCD Application manifest does not contain expected multi-source block; "+
				"the upstream template format may have changed")
	}

	var replacement strings.Builder
	fmt.Fprintf(&replacement, "  source:\n")
	fmt.Fprintf(&replacement, "    repoURL: %s\n", repoURL)
	fmt.Fprintf(&replacement, "    chart: %s\n", chart)
	fmt.Fprintf(&replacement, "    targetRevision: %s\n", targetRevision)
	fmt.Fprintf(&replacement, "    helm:\n")

	if hasDynamic {
		// Merge static (from chart file) + dynamic (from .Values) at render time
		fmt.Fprintf(&replacement, "      valuesObject: |-\n")
		fmt.Fprintf(&replacement, `        {{- $static := (.Files.Get "static/%s.yaml") | fromYaml -}}`+"\n", componentName)
		fmt.Fprintf(&replacement, `        {{- $dynamic := index .Values %q | default dict -}}`+"\n", overrideKey)
		fmt.Fprintf(&replacement, "        {{- mustMergeOverwrite $static $dynamic | toYaml | nindent 8 }}\n")
	} else {
		// Static only — load directly from chart file
		fmt.Fprintf(&replacement, "      valuesObject: |-\n")
		fmt.Fprintf(&replacement, `        {{- (.Files.Get "static/%s.yaml") | fromYaml | toYaml | nindent 8 }}`+"\n", componentName)
	}

	replacement.WriteString("  destination:")

	return sourcesPattern.ReplaceAllLiteralString(content, replacement.String()), nil
}

// copyAsTemplate copies an application.yaml from the flat output to templates/ as-is.
func copyAsTemplate(srcDir, templatesDir, componentName string) (string, int64, error) {
	componentDir, joinErr := shared.SafeJoin(srcDir, componentName)
	if joinErr != nil {
		return "", 0, joinErr
	}
	srcPath, joinErr := shared.SafeJoin(componentDir, "application.yaml")
	if joinErr != nil {
		return "", 0, joinErr
	}
	content, err := os.ReadFile(srcPath)
	if err != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to read application.yaml for %s", componentName), err)
	}

	destPath, pathErr := shared.SafeJoin(templatesDir, componentName+".yaml")
	if pathErr != nil {
		return "", 0, pathErr
	}

	if writeErr := os.WriteFile(destPath, content, 0600); writeErr != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to write template for %s", componentName), writeErr)
	}
	return destPath, int64(len(content)), nil
}

// extractFirstSourceValues parses the multi-source block and extracts
// repoURL, chart, and targetRevision from the first source entry.
// Returns an error if any required field is missing, which indicates the
// upstream ArgoCD template format has changed.
func extractFirstSourceValues(content string) (repoURL, chart, targetRevision string, err error) {
	lines := strings.Split(content, "\n")
	inFirstSource := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "- repoURL:") {
			if !inFirstSource {
				inFirstSource = true
				repoURL = strings.TrimSpace(strings.TrimPrefix(trimmed, "- repoURL:"))
				repoURL = strings.Trim(repoURL, "'")
			}
			continue
		}
		if !inFirstSource {
			continue
		}
		if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "ref:") {
			break // hit second source entry or ref field
		}
		if strings.HasPrefix(trimmed, "chart:") {
			chart = strings.TrimSpace(strings.TrimPrefix(trimmed, "chart:"))
		}
		if strings.HasPrefix(trimmed, "targetRevision:") {
			targetRevision = strings.TrimSpace(strings.TrimPrefix(trimmed, "targetRevision:"))
		}
	}

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
	if len(missing) > 0 {
		return "", "", "", errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("failed to extract source fields [%s] from ArgoCD Application manifest; "+
				"the upstream template format may have changed", strings.Join(missing, ", ")))
	}

	return repoURL, chart, targetRevision, nil
}

func writeChartYAML(outputDir, version string) (string, int64, error) {
	chartPath, err := shared.SafeJoin(outputDir, "Chart.yaml")
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
	readmePath, err := shared.SafeJoin(outputDir, "README.md")
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

func deepCopyMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	data, err := yaml.Marshal(m)
	if err != nil {
		slog.Warn("deepCopyMap: yaml.Marshal failed, falling back to shallow copy", "error", err)
		result := make(map[string]any, len(m))
		for k, v := range m {
			result[k] = v
		}
		return result
	}
	var result map[string]any
	if unmarshalErr := yaml.Unmarshal(data, &result); unmarshalErr != nil {
		slog.Warn("deepCopyMap: yaml.Unmarshal failed, falling back to shallow copy", "error", unmarshalErr)
		result = make(map[string]any, len(m))
		for k, v := range m {
			result[k] = v
		}
	}
	return result
}
