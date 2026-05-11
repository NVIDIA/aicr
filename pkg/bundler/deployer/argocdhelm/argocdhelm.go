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

// Package argocdhelm generates a Helm chart app-of-apps for Argo CD with
// dynamic install-time values.
//
// # How it works
//
// Rather than reimplementing Argo CD Application generation, this deployer
// delegates to the upstream argocd deployer (pkg/bundler/deployer/argocd) to
// produce per-component Applications in the uniform NNN-<name>/ layout, then
// wraps that output as a Helm chart:
//
//  1. Run argocd.Generate to a temp directory. Output is the NNN-<name>/
//     folder layout (Chart.yaml + templates/, or upstream.env, plus
//     application.yaml inside each folder).
//  2. Walk the NNN-<name>/ folders. For each:
//     - Copy the folder's content (excluding application.yaml) into the
//     bundle so path-based Argo Applications can resolve their `path:`
//     references at deploy time.
//     - Transform application.yaml into a Helm chart template under
//     templates/<name>.yaml. Branches on the Application's input shape:
//     - spec.sources (multi-source upstream-helm): flip to single-source +
//     helm.values that merges static .Files.Get values with dynamic
//     .Values.<key> overrides.
//     - spec.source (single-source path-based, e.g. manifest-only or
//     kustomize-wrapped): inject helm.values exposing just dynamic
//     .Values.<key>; the wrapped chart's own values.yaml is the static
//     layer.
//  3. Build a root values.yaml with ONLY dynamic paths (recipe defaults when
//     available, empty strings otherwise). Static values stay in chart files.
//  4. Write Chart.yaml + static/ + templates/ + values.yaml as a valid Helm
//     chart.
//
// This approach means changes to the Argo CD deployer (new component types,
// sync policies, etc.) automatically flow through without duplication.
//
// # When this deployer is used
//
// The bundler routes here when --deployer argocd-helm is specified.
// The --dynamic flag is optional — it pre-populates specific paths in the
// root values.yaml, but all values are overridable regardless.
//
// # Deployment requires a chart-source backend (git or OCI)
//
// The canonical deploy path is to apply the bundle's top-level
// `application.yaml` after publishing the bundle to an OCI Helm registry
// (or a git repo Argo can read). The bundle works end-to-end with
// `helm install ./bundle` only when the recipe contains pure-Helm
// components — multi-source Argo Applications fetch their charts from
// public Helm repos, so no published bundle is needed for those.
//
// Recipes that include manifest-only or mixed-component raw manifests
// produce path-based child Applications whose source is `path:
// NNN-<name>` against the published bundle URL. Argo's repo-server runs
// inside the cluster and only pulls from remote sources (git/OCI/Helm);
// there is no local-filesystem source type for Applications. Pushing
// the bundle to a chart-source backend is therefore mandatory for those
// recipes — that's the build-once-deploy-many architecture from
// ADR-025 / #516.
package argocdhelm

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

// yamlStringTag is the YAML resolved-tag for explicit scalar strings, used
// when emitting nodes that must serialize as quoted strings (e.g. Helm
// template placeholders that would otherwise be misparsed).
const yamlStringTag = "!!str"

// compile-time interface check
var _ deployer.Deployer = (*Generator)(nil)

// Generator creates Helm chart app-of-apps bundles by transforming flat Argo CD output.
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

	// DataFiles lists additional file paths (relative to output dir) to include
	// in checksum generation. Used for external data files copied into the bundle.
	DataFiles []string

	// ComponentManifests maps component name → manifest path → rendered bytes.
	// Forwarded to the delegated argocd.Generator so manifest-only and mixed
	// components are wrapped as local Helm charts in the underlying NNN-folder
	// layout. Without this, the delegated argocd output would silently skip
	// manifestFiles (today's broken behavior for manifest-only components).
	ComponentManifests map[string]map[string][]byte
}

// Generate creates a Helm chart app-of-apps by:
//  1. Delegating to the flat Argo CD deployer for proven Application generation
//  2. Transforming the output into a Helm chart with {{ .Values }} references
func (g *Generator) Generate(ctx context.Context, outputDir string) (*deployer.Output, error) {
	start := time.Now()

	if g.RecipeResult == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "RecipeResult is required")
	}

	// Step 1: Generate flat Argo CD output to a temp directory
	tmpDir, err := os.MkdirTemp("", "argocdhelm-*")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create temp directory", err)
	}
	defer os.RemoveAll(tmpDir)

	// When no explicit --target-revision (or OCI tag from -o oci://...:<tag>)
	// is set, default to the chart version. helm push uses Chart.yaml's
	// version as the OCI tag, so this default keeps the bundle's children
	// and parent Application referencing the same tag a subsequent
	// `helm push` would produce — without that, child Apps would point at
	// "main" (the upstream argocd deployer's git-shaped default) and fail
	// to resolve against the published chart.
	targetRevision := g.TargetRevision
	if targetRevision == "" {
		targetRevision = deployer.NormalizeVersionWithDefault(g.RecipeResult.Metadata.Version)
	}

	argocdGen := &argocd.Generator{
		RecipeResult:       g.RecipeResult,
		ComponentValues:    g.ComponentValues,
		Version:            g.Version,
		RepoURL:            g.RepoURL,
		TargetRevision:     targetRevision,
		IncludeChecksums:   false, // we generate our own checksums
		ComponentManifests: g.ComponentManifests,
		DynamicValues:      g.DynamicValues,
		// Opt into the DynamicValues split: per-component values.yaml has
		// dynamic paths removed; argocdhelm surfaces those paths at the
		// parent chart level via writeStaticValuesAndBuildStubs.
		AllowDynamicValueSplit: true,
	}

	if _, genErr := argocdGen.Generate(ctx, tmpDir); genErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate base Argo CD output", genErr)
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

	// Step 4: Walk the NNN-<name>/ folders the argocd deployer produced.
	// For each folder, copy its non-Application content into outputDir so
	// path-based Argo Applications can resolve `path: NNN-<name>` against
	// the bundle, then transform that folder's application.yaml into a
	// Helm chart template under templates/<name>.yaml.
	templatesDir, err := deployer.SafeJoin(outputDir, "templates")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to resolve templates directory", err)
	}
	if mkdirErr := os.MkdirAll(templatesDir, 0755); mkdirErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create templates directory", mkdirErr)
	}

	if processErr := g.processFolders(ctx, tmpDir, outputDir, templatesDir, output); processErr != nil {
		return nil, processErr
	}

	// Step 4b: Emit the parent Argo Application as a Helm template under
	// templates/aicr-stack.yaml. Its source.repoURL and source.targetRevision
	// are set from .Values at install time, which makes the bundle
	// URL-portable: the same artifact bytes work for any registry the user
	// chooses to push to. See writeParentApplicationTemplate for rationale.
	parentPath, parentSize, parentErr := g.writeParentApplicationTemplate(templatesDir)
	if parentErr != nil {
		return nil, parentErr
	}
	output.Files = append(output.Files, parentPath)
	output.TotalSize += parentSize

	// Step 5: Generate README
	readmePath, readmeSize, err := g.writeReadme(outputDir)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to write README", err)
	}
	output.Files = append(output.Files, readmePath)
	output.TotalSize += readmeSize

	// Include external data files in the file list (for checksums).
	if err := output.AddDataFiles(outputDir, g.DataFiles); err != nil {
		return nil, err
	}

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
		if err := checksum.WriteChecksums(ctx, outputDir, output); err != nil {
			return err
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

		// Defense-in-depth: argocd.Generator runs first and validates names,
		// but validate here too so this function is safe on its own terms.
		if !deployer.IsSafePathComponent(ref.Name) {
			return nil, 0, nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid component name %q: must not contain path separators or parent directory references", ref.Name))
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
					slog.Warn("dynamic path not found in component values; introducing empty placeholder",
						"component", ref.Name, "path", path)
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

// parentAppTemplate is the Helm template body for the parent Argo
// Application. It lives inside the chart's templates/ directory so that
// the user supplies repoURL and targetRevision at install time via
// --set, making the bundle URL-portable (same artifact bytes work for
// any registry the user chooses).
//
// The parent App's helm.valuesObject forwards .Values through, so when
// Argo subsequently renders the chart from OCI it sees the same per-
// cluster overrides the user passed at helm install — keeping the
// dynamic-values story working end-to-end.
const parentAppTemplate = `apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: aicr-stack
  namespace: argocd
spec:
  project: default
  source:
    repoURL: {{ required "repoURL is required: pass --set repoURL=<published bundle URL> (e.g., oci://<registry>/<path>/aicr-bundle)" .Values.repoURL | quote }}
    chart: aicr-bundle
    targetRevision: {{ .Values.targetRevision | default .Chart.Version | quote }}
    helm:
      valuesObject:
{{ toYaml .Values | indent 8 }}
  destination:
    server: https://kubernetes.default.svc
    namespace: argocd
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
      # ServerSideApply: required for charts whose CRDs exceed kubectl's
      # 262144-byte client-side annotation cap. See application.yaml.tmpl
      # in the upstream argocd deployer for the full rationale.
      - ServerSideApply=true
`

// writeParentApplicationTemplate writes the parent Application as a chart
// template. Helm renders it at install time with the user's --set values,
// avoiding the chicken-and-egg of baking the publish location into a
// bundle artifact.
//
// User flow:
//
//	# generate (URL-agnostic — no --repo / --target-revision needed)
//	aicr bundle -r recipe.yaml --deployer argocd-helm -o ./bundle
//	helm package ./bundle -d /tmp/
//	helm push /tmp/aicr-bundle-*.tgz oci://ghcr.io/myorg
//
//	# install — same URL passed once, used for parent + children
//	helm install aicr-bundle oci://ghcr.io/myorg/aicr-bundle --version <tag> \
//	  -n argocd \
//	  --set repoURL=oci://ghcr.io/myorg/aicr-bundle \
//	  --set targetRevision=<tag>
//
// The user could also `kubectl apply` the rendered parent App directly
// without invoking the chart's templating against the cluster — `helm
// template` against the bundle yields the parent + 14 children that can
// be applied without installing the chart.
func (g *Generator) writeParentApplicationTemplate(templatesDir string) (string, int64, error) {
	destPath, joinErr := deployer.SafeJoin(templatesDir, "aicr-stack.yaml")
	if joinErr != nil {
		return "", 0, joinErr
	}
	content := []byte(parentAppTemplate)
	if writeErr := os.WriteFile(destPath, content, 0600); writeErr != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			"failed to write parent Application template", writeErr)
	}
	return destPath, int64(len(content)), nil
}

// processFolders iterates the NNN-<name>/ folders the delegated argocd
// deployer wrote to tmpDir, copies each folder's non-Application content
// into outputDir (so path-based Argo Applications can resolve their `path:`
// references), and transforms each folder's application.yaml into a Helm
// chart template under templatesDir. Output's Files/TotalSize are updated
// in place. Extracted from Generate to keep that function under the funlen
// threshold; the loop body is too long to inline cleanly.
func (g *Generator) processFolders(ctx context.Context, tmpDir, outputDir, templatesDir string, output *deployer.Output) error {
	folderEntries, readErr := os.ReadDir(tmpDir)
	if readErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to list argocd output", readErr)
	}
	for _, e := range folderEntries {
		select {
		case <-ctx.Done():
			return errors.Wrap(errors.ErrCodeTimeout, "context cancelled", ctx.Err())
		default:
		}
		if !e.IsDir() || !isNNNFolder(e.Name()) {
			continue
		}
		folderName := e.Name()
		folderComponent, ok := stripNNNPrefix(folderName)
		if !ok {
			continue
		}

		// Mixed components emit a `-post` folder for raw manifests, alongside
		// the primary upstream-helm folder. The -post folder is not a
		// registered component on its own — its overrides flow from the
		// parent (e.g., 010-gpu-operator-post → gpu-operator). Resolve the
		// parent before looking up the override key.
		parentComponent := folderComponent
		if base, ok := strings.CutSuffix(folderComponent, "-post"); ok {
			if findComponentByName(g.RecipeResult.ComponentRefs, base) {
				parentComponent = base
			}
		}

		// Copy NNN-<name>/ content into the bundle so path-based Argo
		// Applications can resolve their `path:` references at deploy time.
		// Skip:
		//   - application.yaml: transformed below into a chart template.
		//   - install.sh, upstream.env: helm-deployer orchestration files
		//     (rendered by localformat for the `--deployer helm` use case).
		//     Argo's repo-server never invokes them — keeping them in the
		//     OCI bundle just bloats the image.
		copiedFiles, copySize, copyErr := copyFolderContent(tmpDir, outputDir, folderName,
			"application.yaml", "install.sh", "upstream.env")
		if copyErr != nil {
			return copyErr
		}
		output.Files = append(output.Files, copiedFiles...)
		output.TotalSize += copySize

		overrideKey, keyErr := resolveOverrideKey(parentComponent)
		if keyErr != nil {
			return keyErr
		}
		tmplPath, tmplSize, transformErr := transformApplication(tmpDir, templatesDir, folderName, folderComponent, overrideKey)
		if transformErr != nil {
			return transformErr
		}
		output.Files = append(output.Files, tmplPath)
		output.TotalSize += tmplSize
	}
	return nil
}

// transformApplication reads NNN-<folder>/application.yaml from srcDir, parses
// it as structured YAML, and rewrites it as a Helm chart template under
// templatesDir/<componentName>.yaml. The transformation branches on the input
// Application shape produced by the upstream argocd deployer:
//
//   - spec.sources (multi-source upstream-helm): flip to single-source +
//     helm.values that merges static values (.Files.Get) and dynamic overrides
//     (.Values.<overrideKey>).
//   - spec.source (single-source path-based): inject helm.values that exposes
//     just the dynamic .Values.<overrideKey> overrides; the wrapped chart at
//     the path provides its own values.yaml as the static layer.
func transformApplication(srcDir, templatesDir, folderName, componentName, overrideKey string) (string, int64, error) {
	if !deployer.IsSafePathComponent(componentName) {
		return "", 0, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid component name %q: must not contain path separators or parent directory references", componentName))
	}
	if !deployer.IsSafePathComponent(folderName) {
		return "", 0, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("invalid folder name %q: must not contain path separators or parent directory references", folderName))
	}
	componentDir, joinErr := deployer.SafeJoin(srcDir, folderName)
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

	spec, ok := app["spec"].(map[string]any)
	if !ok {
		return "", 0, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("application manifest for %s missing 'spec'", componentName))
	}

	// Detect input shape and apply the matching transformation.
	switch {
	case spec["sources"] != nil:
		if transformErr := convertToSingleSourceWithValues(app, componentName, overrideKey); transformErr != nil {
			return "", 0, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to transform multi-source application for %s", componentName), transformErr)
		}
	case spec["source"] != nil:
		if transformErr := injectValuesIntoSingleSource(app, overrideKey); transformErr != nil {
			return "", 0, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to inject values into single-source application for %s", componentName), transformErr)
		}
	default:
		return "", 0, errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("application manifest for %s has neither 'spec.source' nor 'spec.sources'", componentName))
	}

	// helm.values is a *yaml.Node with LiteralStyle, so yaml.Marshal emits
	// the raw Helm template as a block scalar that Helm evaluates at render
	// time (rather than a quoted YAML string).
	out, marshalErr := yaml.Marshal(app)
	if marshalErr != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to marshal transformed application for %s", componentName), marshalErr)
	}

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

// injectValuesIntoSingleSource adds a helm.values block to an existing
// single-source path-based Application AND rewrites the source's repoURL
// and targetRevision to Helm template directives so the rendered child
// Application picks up the user's install-time --set repoURL/.Values.
// This is what makes the bundle URL-portable: the published chart bytes
// don't bake in the publish location; the same bytes work for any
// registry the user pushes to.
//
// The path field stays baked (it's the NNN-<name>/ folder name inside
// the bundle, which is structural, not URL-dependent).
//
// # OCI publication and the path field
//
// Argo CD's OCI source type has two distinct shapes that are easy to
// conflate:
//
//   - OCI **Helm chart** source (Application has `source.chart` set):
//     the OCI artifact is pulled as a Helm chart and rendered. Per Argo's
//     OCI docs, path must be "." for this shape.
//   - OCI **generic artifact** source (Application has `source.path` set
//     and no `source.chart`): the OCI artifact is unpacked and treated
//     like a directory tree (similar to git), so path is meaningful and
//     subdirectory references resolve correctly.
//
// The bundle relies on both shapes simultaneously against the *same*
// published OCI artifact: the parent Application uses the Helm chart
// shape (it pulls and renders this very chart), while child Applications
// use the generic-artifact shape to read NNN-<name>/ subdirectories from
// the same artifact bytes. Argo CD supports this because the Application
// spec — not the registry-level repo configuration — determines how a
// given source is interpreted. (Generic OCI source support has been in
// Argo CD since v2.13; older versions, or registries configured solely
// as Helm chart repos with no OCI passthrough, will not work — that is
// a deployment-time concern documented in the package doc, not a
// generation-time bug.)
//
// Mirrors the missing-fields validation in convertToSingleSourceWithValues:
// if the upstream argocd template ever regresses to emitting an empty path
// or repoURL, the bundle should fail loudly here rather than produce a
// template that kubectl will reject at install time with cryptic schema
// errors.
func injectValuesIntoSingleSource(app map[string]any, overrideKey string) error {
	spec, ok := app["spec"].(map[string]any)
	if !ok {
		return errors.New(errors.ErrCodeInternal, "application manifest missing 'spec'")
	}
	source, ok := spec["source"].(map[string]any)
	if !ok {
		return errors.New(errors.ErrCodeInternal, "application manifest missing 'spec.source'")
	}

	repoURL, _ := source["repoURL"].(string)
	path, _ := source["path"].(string)
	if repoURL == "" || path == "" {
		var missing []string
		if repoURL == "" {
			missing = append(missing, "repoURL")
		}
		if path == "" {
			missing = append(missing, "path")
		}
		return errors.New(errors.ErrCodeInternal,
			fmt.Sprintf("path-based source missing fields: %s", strings.Join(missing, ", ")))
	}

	// Replace baked-in repoURL and targetRevision with Helm template
	// directives. Use SingleQuotedStyle so yaml.Marshal emits the value
	// in single quotes — that's the only YAML scalar style that doesn't
	// require escaping the embedded double quotes inside `required "..."`,
	// which would corrupt Helm's parsing of the template.
	source["repoURL"] = &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   yamlStringTag,
		Style: yaml.SingleQuotedStyle,
		Value: `{{ required "repoURL is required: pass --set repoURL=<published bundle URL> (e.g., oci://<registry>/<path>/aicr-bundle)" .Values.repoURL }}`,
	}
	source["targetRevision"] = &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   yamlStringTag,
		Style: yaml.SingleQuotedStyle,
		Value: `{{ .Values.targetRevision | default .Chart.Version }}`,
	}

	// Same column math as convertToSingleSourceWithValues — see the comment
	// there. yaml.Marshal renders `values:` at column 12, so nindent must be
	// >= 13 for content to stay inside the literal block scalar; 16 reads
	// cleanly aligned.
	valuesTmpl := fmt.Sprintf(
		`{{- $dynamic := index .Values %q | default dict -}}`+"\n"+
			`{{- toYaml $dynamic | nindent 16 }}`,
		overrideKey)

	helm, _ := source["helm"].(map[string]any)
	if helm == nil {
		helm = map[string]any{}
	}
	helm["values"] = &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   yamlStringTag,
		Style: yaml.LiteralStyle,
		Value: valuesTmpl,
	}
	source["helm"] = helm
	return nil
}

// isNNNFolder reports whether name matches the NNN-<rest> shape.
func isNNNFolder(name string) bool {
	if len(name) < 4 || name[3] != '-' {
		return false
	}
	for i := range 3 {
		if name[i] < '0' || name[i] > '9' {
			return false
		}
	}
	return true
}

// stripNNNPrefix returns the component name from an NNN-<name> folder, or
// (_, false) if the folder name does not match the prefix shape.
func stripNNNPrefix(name string) (string, bool) {
	if !isNNNFolder(name) {
		return "", false
	}
	return name[4:], true
}

// findComponentByName reports whether refs contains a ComponentRef with
// the given name. Used to distinguish a literal "<name>-post" component
// from a mixed-component injected `-post` folder.
//
// This heuristic is safe because pkg/bundler/deployer/localformat rejects
// at write time any recipe that would create a collision: if a mixed
// component "<name>" (Helm + manifests) is declared alongside a separately-
// declared "<name>-post" component, localformat.Write returns an error
// before any folder is emitted (see the collision-detection block in
// localformat/writer.go that scans `declared[c.Name+"-post"]`). So when
// argocdhelm sees an NNN-<name>-post folder, exactly one of these holds:
//
//   - A real component named "<name>-post" was declared (no `<name>` parent
//     in refs) — heuristic falls through, parentComponent stays unchanged.
//   - It's an injected -post folder for the mixed component "<name>" — the
//     parent IS in refs, heuristic correctly redirects to "<name>"'s key.
//
// Both cannot hold simultaneously. If the localformat collision check is
// ever loosened, this heuristic will silently route the wrong overrides;
// the localformat check is therefore load-bearing for this code path.
func findComponentByName(refs []recipe.ComponentRef, name string) bool {
	for _, r := range refs {
		if r.Name == name {
			return true
		}
	}
	return false
}

// copyFolderContent copies srcDir/folderName/* into outputDir/folderName/,
// excluding any file whose basename is in skip. Used to relocate the argocd
// deployer's NNN-folders into the argocdhelm bundle so path-based Argo
// Applications can resolve their `path: NNN-<name>` references at deploy
// time. Returns the absolute paths of files written and their total bytes.
func copyFolderContent(srcDir, outputDir, folderName string, skip ...string) ([]string, int64, error) {
	skipSet := make(map[string]struct{}, len(skip))
	for _, s := range skip {
		skipSet[s] = struct{}{}
	}
	srcFolder, joinErr := deployer.SafeJoin(srcDir, folderName)
	if joinErr != nil {
		return nil, 0, joinErr
	}
	dstFolder, joinErr := deployer.SafeJoin(outputDir, folderName)
	if joinErr != nil {
		return nil, 0, joinErr
	}
	if err := os.MkdirAll(dstFolder, 0755); err != nil {
		return nil, 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to create folder %s", folderName), err)
	}

	var files []string
	var total int64
	walkErr := filepath.Walk(srcFolder, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == srcFolder {
			return nil
		}
		rel, relErr := filepath.Rel(srcFolder, path)
		if relErr != nil {
			return relErr
		}
		dst, joinErr := deployer.SafeJoin(dstFolder, rel)
		if joinErr != nil {
			return joinErr
		}
		if info.IsDir() {
			//nolint:gosec // G122 -- dst is from SafeJoin (clamped to dstFolder); srcFolder is a freshly created os.MkdirTemp dir under our control with no symlinks.
			return os.MkdirAll(dst, info.Mode())
		}
		if _, skipped := skipSet[filepath.Base(path)]; skipped {
			return nil
		}
		//nolint:gosec // G122 -- path is rooted at srcFolder, a freshly created os.MkdirTemp dir we just wrote (no symlinks). filepath.Clean defends against any defense-in-depth concerns.
		data, readErr := os.ReadFile(filepath.Clean(path))
		if readErr != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to read %s", path), readErr)
		}
		//nolint:gosec // G703 -- dst is from SafeJoin, not user-controlled.
		if writeErr := os.WriteFile(dst, data, info.Mode().Perm()); writeErr != nil {
			return errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to write %s", dst), writeErr)
		}
		files = append(files, dst)
		total += int64(len(data))
		return nil
	})
	if walkErr != nil {
		return nil, 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to copy folder %s", folderName), walkErr)
	}
	return files, total, nil
}

// convertToSingleSourceWithValues mutates an Argo CD Application map,
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
	// Argo CD's spec.source.helm.values is a string field containing YAML text.
	// This template merges static values (from chart files) with dynamic overrides
	// (from .Values) at Helm render time.
	//
	// nindent must exceed the column of the `values:` key so the merged content
	// stays inside the literal block scalar. yaml.Marshal renders this manifest
	// with 4-space indent, placing `values:` at column 12 (spec=0, source=4,
	// helm=8, values=12). nindent 16 indents content to column 16 — safely
	// inside the block. Smaller values (e.g. 8) cause toYaml output to land as
	// siblings of `helm:` and break Application schema validation.
	valuesTmpl := fmt.Sprintf(
		`{{- $static := (.Files.Get "static/%s.yaml") | fromYaml | default dict -}}`+"\n"+
			`{{- $dynamic := index .Values %q | default dict -}}`+"\n"+
			`{{- mustMergeOverwrite $static $dynamic | toYaml | nindent 16 }}`,
		componentName, overrideKey)

	// Replace multi-source with single source + values. The values template is
	// wrapped in a yaml.Node with LiteralStyle so yaml.Marshal emits it as a
	// block scalar rather than a quoted string — Helm evaluates the raw
	// template text at render time.
	spec["source"] = map[string]any{
		"repoURL":        repoURL,
		"chart":          chart,
		"targetRevision": targetRevision,
		"helm": map[string]any{
			"values": &yaml.Node{
				Kind:  yaml.ScalarNode,
				Tag:   yamlStringTag,
				Style: yaml.LiteralStyle,
				Value: valuesTmpl,
			},
		},
	}
	delete(spec, "sources")

	return nil
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
	buf.WriteString("# Argo CD Helm Chart Deployment Bundle\n\n")
	buf.WriteString("This bundle is a Helm chart whose templates generate one Argo CD\n")
	buf.WriteString("Application per component plus a parent Application that manages\n")
	buf.WriteString("the chart deployment as a whole. The bundle is **URL-portable**:\n")
	buf.WriteString("the publish location is supplied at install time via `--set\n")
	buf.WriteString("repoURL=...`, not baked into the chart bytes.\n\n")

	buf.WriteString("## Deploy\n\n")
	buf.WriteString("```bash\n")
	buf.WriteString("# 1. Publish to your chart registry (any HTTPS OCI / Helm chart repo).\n")
	buf.WriteString("helm package . --destination /tmp/\n")
	buf.WriteString("helm push /tmp/aicr-bundle-*.tgz oci://<your-registry>/<path>\n\n")
	buf.WriteString("# 2. Install from the published chart — supply repoURL and\n")
	buf.WriteString("#    targetRevision so the parent Application and path-based child\n")
	buf.WriteString("#    Applications can pull from the registry you pushed to.\n")
	buf.WriteString("helm install aicr-bundle oci://<your-registry>/<path>/aicr-bundle \\\n")
	buf.WriteString("  --version <chart-version> -n argocd \\\n")
	buf.WriteString("  --set repoURL=oci://<your-registry>/<path>/aicr-bundle \\\n")
	buf.WriteString("  --set targetRevision=<chart-version>\n")
	buf.WriteString("```\n\n")

	buf.WriteString("`helm install` against this local directory works only when the\n")
	buf.WriteString("recipe contains pure-Helm components — child Applications whose\n")
	buf.WriteString("source is path-based (manifest-only, mixed `-post`) need Argo's\n")
	buf.WriteString("repo-server to fetch from a remote, so the chart must be published\n")
	buf.WriteString("first for those cases.\n\n")
	buf.WriteString("```bash\n# Local install (pure-Helm-only recipes)\nhelm install aicr-bundle . -n argocd \\\n  --set repoURL=oci://<your-registry>/<path>/aicr-bundle \\\n  --set targetRevision=<chart-version>")

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
