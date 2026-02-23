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
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/manifest"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/registry"
	"sigs.k8s.io/yaml"
)

// componentDiscovery tracks the discovery results for a single component.
type componentDiscovery struct {
	name      string
	resources []recipe.ExpectedResource
}

// resolveExpectedResources discovers expected workload resources from two sources
// and merges them with any manually declared expectedResources:
//
//  1. Helm chart rendering via the Helm Go SDK (equivalent to `helm template`)
//  2. Manifest file rendering via shared pkg/manifest.Render() — same logic as the bundler
//
// Manual expectedResources take precedence over auto-discovered ones.
// Rendering failures for individual components are logged as warnings and do not
// block other components.
//
// Note: Phase 1 (helm template) requires network access for chart downloads
// (HTTP repos and OCI registries). Offline/air-gapped environments will see
// warnings for components with chart coordinates but can still use manually
// declared expectedResources.
func resolveExpectedResources(ctx context.Context, recipeResult *recipe.RecipeResult) error {
	summaries := make([]componentDiscovery, 0, len(recipeResult.ComponentRefs))

	for i := range recipeResult.ComponentRefs {
		if ctx.Err() != nil {
			code := errors.ErrCodeTimeout
			if ctx.Err() == context.Canceled {
				code = errors.ErrCodeInternal
			}
			return errors.Wrap(code, "context cancelled during expected resource discovery", ctx.Err())
		}

		ref := &recipeResult.ComponentRefs[i]

		// Load values once — needed for both chart rendering and manifestFile rendering.
		values, valErr := recipeResult.GetValuesForComponent(ref.Name)
		if valErr != nil {
			slog.Warn("failed to load values for component, using defaults",
				"component", ref.Name, "error", valErr)
			values = make(map[string]any)
		}

		var discovered []recipe.ExpectedResource

		// Phase 1: Render Helm chart via SDK (equivalent to `helm template`).
		if ref.Type == recipe.ComponentTypeHelm && ref.Source != "" && ref.Chart != "" {
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
	lines := make([]string, 0, len(summaries))
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

// renderHelmTemplate uses the Helm Go SDK to render a chart (equivalent to
// `helm template`), then extracts workload resources from the output.
// Supports both HTTP repos and OCI registries (oci:// prefix in source).
func renderHelmTemplate(ctx context.Context, ref recipe.ComponentRef, values map[string]any) ([]recipe.ExpectedResource, error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.ComponentRenderTimeout)
	defer cancel()

	settings := cli.New()

	regClient, err := registry.NewClient(
		registry.ClientOptCredentialsFile(settings.RegistryConfig),
	)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create helm registry client", err)
	}

	actionCfg := &action.Configuration{
		RegistryClient: regClient,
		Log: func(format string, v ...interface{}) {
			slog.Debug(fmt.Sprintf(format, v...))
		},
	}

	install := action.NewInstall(actionCfg)
	install.DryRun = true
	install.ClientOnly = true
	install.ReleaseName = ref.Name
	install.Namespace = ref.Namespace
	install.Replace = true
	if ref.Version != "" {
		install.Version = ref.Version
	}

	chartPath, err := locateChart(install, ref, settings)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(chartPath) // clean up downloaded chart archive/directory

	chrt, err := loader.Load(chartPath)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to load helm chart", err)
	}

	rel, err := install.RunWithContext(ctx, chrt, values)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "helm template rendering failed", err)
	}

	return extractWorkloadResources(rel.Manifest, ref.Namespace), nil
}

// locateChart resolves a chart reference to a local path by downloading it.
// Supports HTTP repos (via --repo equivalent) and OCI registries (oci:// prefix).
//
// Note: LocateChart does not accept a context — chart downloads are not
// cancellable via ComponentRenderTimeout. The timeout still bounds the
// subsequent rendering step (RunWithContext).
func locateChart(install *action.Install, ref recipe.ComponentRef, settings *cli.EnvSettings) (string, error) {
	if strings.HasPrefix(ref.Source, "oci://") {
		install.RepoURL = ""
		chartRef := ref.Source + "/" + ref.Chart
		path, err := install.LocateChart(chartRef, settings)
		if err != nil {
			return "", errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to locate OCI chart %s", chartRef), err)
		}
		return path, nil
	}

	install.RepoURL = ref.Source
	path, err := install.LocateChart(ref.Chart, settings)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to locate chart %s from %s", ref.Chart, ref.Source), err)
	}
	return path, nil
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
