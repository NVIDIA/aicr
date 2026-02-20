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

package validator

import (
	"bufio"
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/NVIDIA/eidos/pkg/defaults"
	"github.com/NVIDIA/eidos/pkg/errors"
	"github.com/NVIDIA/eidos/pkg/manifest"
	"github.com/NVIDIA/eidos/pkg/recipe"
	"sigs.k8s.io/yaml"
)

const (
	// helmCommand is the CLI command for Helm operations.
	helmCommand = "helm"
)

// componentDiscovery tracks the discovery results for a single component.
type componentDiscovery struct {
	name      string
	resources []recipe.ExpectedResource
}

// resolveExpectedResources discovers expected workload resources from two sources
// and merges them with any manually declared expectedResources:
//
//  1. Helm chart rendering via CLI subprocess (helm template)
//  2. Manifest file rendering via shared pkg/manifest.Render() — same logic as the bundler
//
// Manual expectedResources take precedence over auto-discovered ones.
// CLI tool availability is checked up front; missing tools cause a hard error if
// components of that type exist. Rendering failures for individual components are
// logged as warnings and do not block other components.
//
// Note: Phase 1 (helm template) requires network access for chart downloads
// (HTTP repos via --repo, OCI registries via oci:// prefix) and the helm CLI.
// Offline/air-gapped environments will see warnings for components with chart
// coordinates but can still use manually declared expectedResources.
func resolveExpectedResources(ctx context.Context, recipeResult *recipe.RecipeResult) error {
	// Check if helm CLI is needed and verify availability up front.
	needsHelm := false
	for i := range recipeResult.ComponentRefs {
		ref := &recipeResult.ComponentRefs[i]
		if ref.Type == recipe.ComponentTypeHelm && ref.Source != "" && ref.Chart != "" {
			needsHelm = true
			break
		}
	}

	if needsHelm {
		if _, err := exec.LookPath(helmCommand); err != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				"helm CLI not found but required for Helm chart resource discovery", err)
		}
	}

	var summaries []componentDiscovery

	for i := range recipeResult.ComponentRefs {
		ref := &recipeResult.ComponentRefs[i]

		// Load values once — needed for both chart rendering and manifestFile rendering.
		values, valErr := recipeResult.GetValuesForComponent(ref.Name)
		if valErr != nil {
			slog.Warn("failed to load values for component, using defaults",
				"component", ref.Name, "error", valErr)
			values = make(map[string]any)
		}

		var discovered []recipe.ExpectedResource

		// Phase 1: Render Helm chart via CLI subprocess.
		if ref.Type == recipe.ComponentTypeHelm && needsHelm && ref.Source != "" && ref.Chart != "" {
			slog.Debug("auto-discovering expected resources via helm template",
				"component", ref.Name, "chart", ref.Chart, "version", ref.Version)

			chartResources, err := renderHelmTemplate(ctx, *ref, values)
			if err != nil {
				slog.Warn("failed to render helm chart for expected resource discovery",
					"component", ref.Name, "error", err)
			} else {
				discovered = append(discovered, chartResources...)
			}
		}

		// Phase 2: Render manifestFiles (Go templates bundled alongside the chart).
		// Uses the same rendering logic as the bundler (pkg/manifest.Render).
		if len(ref.ManifestFiles) > 0 {
			manifestResources := renderManifestFiles(ctx, *ref, values)
			discovered = append(discovered, manifestResources...)
		}

		// Skip components that produced no discovered resources.
		if len(discovered) == 0 && len(ref.ExpectedResources) == 0 {
			continue
		}

		ref.ExpectedResources = mergeExpectedResources(ref.ExpectedResources, discovered)
		summaries = append(summaries, componentDiscovery{
			name:      ref.Name,
			resources: ref.ExpectedResources,
		})
	}

	logDiscoverySummary(summaries)
	return nil
}

// logDiscoverySummary logs a summary of all discovered expected resources.
func logDiscoverySummary(summaries []componentDiscovery) {
	if len(summaries) == 0 {
		slog.Info("no expected resources discovered")
		return
	}

	totalResources := 0
	var lines []string
	for _, s := range summaries {
		totalResources += len(s.resources)
		counts := countByKind(s.resources)
		lines = append(lines, fmt.Sprintf("  %s: %d resources (%s)", s.name, len(s.resources), counts))
	}

	slog.Info("discovered expected resources",
		"summary", fmt.Sprintf("\n%s\nTotal: %d resources across %d components",
			strings.Join(lines, "\n"), totalResources, len(summaries)))
}

// countByKind returns a human-readable summary like "2 Deployments, 1 DaemonSet".
func countByKind(resources []recipe.ExpectedResource) string {
	counts := make(map[string]int)
	for _, r := range resources {
		counts[r.Kind]++
	}

	var parts []string
	for _, kind := range []string{"Deployment", "DaemonSet", "StatefulSet"} {
		if n, ok := counts[kind]; ok {
			label := kind + "s"
			if n == 1 {
				label = kind
			}
			parts = append(parts, fmt.Sprintf("%d %s", n, label))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// renderHelmTemplate invokes `helm template` as a subprocess to render a chart,
// then extracts workload resources from the output.
// Supports both HTTP repos (--repo flag) and OCI registries (oci:// prefix in source).
func renderHelmTemplate(ctx context.Context, ref recipe.ComponentRef, values map[string]any) ([]recipe.ExpectedResource, error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.ComponentRenderTimeout)
	defer cancel()

	var args []string
	if strings.HasPrefix(ref.Source, "oci://") {
		// OCI registries: chart reference is <source>/<chart>, no --repo flag
		args = []string{"template", ref.Name, ref.Source + "/" + ref.Chart,
			"--namespace", ref.Namespace,
		}
	} else {
		// HTTP repos: use --repo flag with separate chart name
		args = []string{"template", ref.Name, ref.Chart,
			"--repo", ref.Source,
			"--namespace", ref.Namespace,
		}
	}
	if ref.Version != "" {
		args = append(args, "--version", ref.Version)
	}

	// Write values to a temp file if non-empty
	if len(values) > 0 {
		valuesFile, err := writeValuesToTempFile(values)
		if err != nil {
			return nil, err
		}
		defer os.Remove(valuesFile)
		args = append(args, "-f", valuesFile)
	}

	output, err := executeSubprocess(ctx, helmCommand, args...)
	if err != nil {
		return nil, err
	}

	return extractWorkloadResources(string(output), ref.Namespace), nil
}

// writeValuesToTempFile marshals values to a temporary YAML file.
// The caller is responsible for removing the file (defer os.Remove).
func writeValuesToTempFile(values map[string]any) (string, error) {
	data, err := yaml.Marshal(values)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to marshal values to YAML", err)
	}

	f, err := os.CreateTemp("", "eidos-values-*.yaml")
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to create temp values file", err)
	}

	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name()) //nolint:gosec // path from os.CreateTemp is safe
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to write temp values file", err)
	}

	if err := f.Close(); err != nil {
		os.Remove(f.Name()) //nolint:gosec // path from os.CreateTemp is safe
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to close temp values file", err)
	}

	return f.Name(), nil
}

// executeSubprocess runs a CLI command and returns its stdout.
func executeSubprocess(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	output, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if stderrors.As(err, &exitErr) {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("command %s failed (stderr: %s)", name, string(exitErr.Stderr)), err)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("command %s failed", name), err)
	}
	return output, nil
}

// renderManifestFiles renders each manifestFile as a Go template with
// Helm-compatible data, then extracts workload resources from the output.
// This uses the same rendering logic as the bundler (pkg/manifest.Render).
// Rendering errors for individual files are logged as warnings and skipped.
func renderManifestFiles(ctx context.Context, ref recipe.ComponentRef, values map[string]any) []recipe.ExpectedResource {
	var resources []recipe.ExpectedResource

	for _, manifestPath := range ref.ManifestFiles {
		if ctx.Err() != nil {
			slog.Warn("context cancelled during manifest file rendering",
				"component", ref.Name, "error", ctx.Err())
			break
		}
		content, err := recipe.GetManifestContent(manifestPath)
		if err != nil {
			slog.Warn("failed to load manifest file for resource discovery",
				"component", ref.Name, "path", manifestPath, "error", err)
			continue
		}

		rendered, err := manifest.Render(content, manifest.RenderInput{
			ComponentName: ref.Name,
			Namespace:     ref.Namespace,
			ChartName:     ref.Chart,
			ChartVersion:  strings.TrimPrefix(ref.Version, "v"),
			Values:        values,
		})
		if err != nil {
			slog.Debug("failed to render manifest template, skipping",
				"component", ref.Name, "path", manifestPath, "error", err)
			continue
		}

		extracted := extractWorkloadResources(string(rendered), ref.Namespace)
		resources = append(resources, extracted...)
	}

	return resources
}

// extractWorkloadResources parses rendered multi-document YAML and returns
// ExpectedResource entries for Deployment, DaemonSet, and StatefulSet resources.
func extractWorkloadResources(manifestContent string, defaultNamespace string) []recipe.ExpectedResource {
	var resources []recipe.ExpectedResource

	docs := splitYAMLDocuments(manifestContent)
	for _, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}

		// Parse just enough to get kind, name, namespace
		var meta struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}

		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil {
			// Skip unparseable documents
			continue
		}

		// Only extract workload resources
		switch meta.Kind {
		case "Deployment", "DaemonSet", "StatefulSet":
			ns := meta.Metadata.Namespace
			if ns == "" {
				ns = defaultNamespace
			}
			resources = append(resources, recipe.ExpectedResource{
				Kind:      meta.Kind,
				Name:      meta.Metadata.Name,
				Namespace: ns,
			})
		}
	}

	return resources
}

// mergeExpectedResources merges auto-discovered resources into manual ones.
// Manual entries take precedence on (Kind, Name, Namespace) conflict.
func mergeExpectedResources(manual, discovered []recipe.ExpectedResource) []recipe.ExpectedResource {
	type key struct {
		Kind      string
		Name      string
		Namespace string
	}

	seen := make(map[key]bool, len(manual))
	result := make([]recipe.ExpectedResource, 0, len(manual)+len(discovered))

	// Manual entries first (take precedence)
	for _, r := range manual {
		k := key{Kind: r.Kind, Name: r.Name, Namespace: r.Namespace}
		seen[k] = true
		result = append(result, r)
	}

	// Add discovered entries that aren't already present
	for _, r := range discovered {
		k := key{Kind: r.Kind, Name: r.Name, Namespace: r.Namespace}
		if !seen[k] {
			seen[k] = true
			result = append(result, r)
		}
	}

	return result
}

// splitYAMLDocuments splits a multi-document YAML string on "---" separators.
func splitYAMLDocuments(manifestContent string) []string {
	var docs []string
	var current strings.Builder

	scanner := bufio.NewScanner(strings.NewReader(manifestContent))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "---" {
			if current.Len() > 0 {
				docs = append(docs, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteString(line)
		current.WriteString("\n")
	}

	if current.Len() > 0 {
		docs = append(docs, current.String())
	}

	return docs
}
