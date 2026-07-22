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
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	bundlercfg "github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/argocd"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// yamlStringTag is the YAML resolved-tag for explicit scalar strings, used
// when emitting nodes that must serialize as quoted strings (e.g. Helm
// template placeholders that would otherwise be misparsed).
const yamlStringTag = "!!str"

// DefaultChartName is the Helm chart name used when ChartName is not set
// (typically when --output is a local directory). When --output is an OCI
// reference, the chart name is derived from the last path segment of the
// repository (e.g. "oci://ghcr.io/org/my-bundle:v1" → "my-bundle") so the
// parent Application's rendered source — `repoURL/<chart>:<tag>` for
// native OCI, `repoURL + source.chart` for HTTPS Helm repos — points at
// the actual chart artifact.
const DefaultChartName = "aicr-bundle"

// DefaultAppName is the parent Argo Application's `metadata.name` written
// into the chart when Generator.AppName is empty. Two AICR bundles
// installed into the same Argo CD namespace must carry distinct names —
// see issue #1011. The constant value is also referenced from the chart's
// rendered Helm template (`{{ .Values.appName | default "aicr-stack" }}`)
// so an operator who installs without --set appName still gets a working
// Application, just one that collides with any other bundle using the
// default.
const DefaultAppName = "aicr-stack"

// rootValuesAppNameKey is the .Values key that the parent Application
// template reads to assemble its metadata.name. Centralized so the
// constant name flows consistently into the rendered template, the
// root values.yaml, and any documentation that references it.
const rootValuesAppNameKey = "appName"

// rootValuesRepoURLKey and rootValuesTargetRevisionKey are surfaced in
// the root values.yaml with empty defaults so `helm show values`
// documents them. The parent App template's {{ required }} directive
// still enforces non-empty at render time — empty strings here only
// affect documentation surface, not runtime behavior.
const (
	rootValuesRepoURLKey        = "repoURL"
	rootValuesTargetRevisionKey = "targetRevision"
)

// rootValuesDeployerKey is the .Values map that carries deployer-level
// Argo Application options (namePrefix, destinationServer, project,
// includeRootApp).
// Reserved: the component registry must not use "deployer" as a
// component name or override key (guard test in pkg/recipe). See #1625.
const rootValuesDeployerKey = "deployer"

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

	// ChartName is the Helm chart name written into Chart.yaml and used
	// by the parent Application template — appended to `source.repoURL`
	// for `oci://` repoURLs (native OCI), or emitted as `source.chart`
	// alongside repoURL for HTTPS Helm repos. When empty, defaults to
	// DefaultChartName ("aicr-bundle"). When the bundle is published to
	// an OCI registry at a non-default artifact name, callers MUST set
	// this to the registry's last path segment (e.g. "my-bundle" for
	// "oci://ghcr.io/org/my-bundle:v1") — otherwise the assembled
	// `<parent namespace>/<chart>:<tag>` resolves to an artifact that
	// does not exist in the registry. See issue #1019.
	ChartName string

	// BundleChartVersion is the strict SemVer written into Chart.yaml when set.
	// Empty preserves the normalized recipe-version default. OCI callers pass
	// the semantic form decoded from the raw Distribution tag, never the raw tag.
	BundleChartVersion string

	// AppName overrides the parent Argo Application's `metadata.name`.
	// When empty, the rendered chart falls back to DefaultAppName
	// ("aicr-stack") via `{{ .Values.appName | default ... }}`. When set,
	// the value is written into the bundle's root values.yaml so it is
	// the chart's default at install time; operators can still override
	// it on the command line with `helm install --set appName=...`. This
	// is the multi-bundle collision fix — see issue #1011.
	AppName string

	// OCIParentNamespace is the OCI registry + repository path with the
	// chart-name segment stripped. When set, written as the default repoURL
	// in the bundle's root values.yaml so deploying from the push-target
	// registry requires no --set flags. Empty for local-directory output. See #1342.
	OCIParentNamespace string

	// NamePrefix is prepended to every child Application metadata.name.
	// The parent Application name is covered by AppName. Written into
	// the bundle's root values.yaml as an install-time default; operators
	// override with `helm install --set deployer.namePrefix=...`.
	// Composed names are validated as DNS-1123 subdomains at generation
	// time. See #1625.
	NamePrefix string

	// DestinationServer overrides spec.destination.server on child
	// Applications only; empty falls back to
	// argocd.DefaultDestinationServer. Written into the bundle's root
	// values.yaml as an install-time default; operators override with
	// `helm install --set deployer.destinationServer=...`. The parent
	// stays on the control-plane cluster — Application CRs are reconciled
	// only from the cluster running Argo CD. See #1625.
	DestinationServer string

	// Project overrides spec.project on child Applications only; empty
	// falls back to argocd.DefaultProject. Written into the bundle's root
	// values.yaml as an install-time default; operators override with
	// `helm install --set deployer.project=...`. The parent stays in
	// "default" — a project able to create Applications in the Argo CD
	// namespace is effectively admin. See #1625.
	Project string

	// CascadeDelete adds argocd.ResourcesFinalizer to the parent and
	// every child Application. Baked at bundle time — finalizers is a
	// list field that cannot round-trip as a `.Values.deployer.*`
	// template expression, so it is intentionally NOT install-time
	// overridable. See #1628.
	CascadeDelete bool

	// DynamicValues maps component names to their dynamic value paths.
	DynamicValues map[string][]string

	// DataFiles lists additional file paths (relative to output dir) to include
	// in checksum generation. Used for external data files copied into the bundle.
	DataFiles []string

	// ComponentPreManifests maps component name → manifest path → rendered
	// bytes for manifests that apply BEFORE each component's primary chart.
	// Forwarded to the delegated argocd.Generator. Populated from
	// ComponentRef.PreManifestFiles.
	ComponentPreManifests map[string]map[string][]byte

	// ComponentPostManifests maps component name → manifest path → rendered
	// bytes for manifests that apply AFTER each component's primary chart.
	// Forwarded to the delegated argocd.Generator so manifest-only and mixed
	// components are wrapped as local Helm charts in the underlying NNN-folder
	// layout. Without this, the delegated argocd output would silently skip
	// manifestFiles (today's broken behavior for manifest-only components).
	ComponentPostManifests map[string]map[string][]byte

	// ComponentReadiness maps component name → manifest path → rendered bytes
	// for the per-component readiness gate. Forwarded to the delegated
	// argocd.Generator, which emits it as a folder after the component's
	// primary chart so the child Application inherits the next sync-wave and
	// Argo CD blocks on the gate Job via built-in batch/Job health. Populated
	// by the bundler from readiness.yaml only when --readiness-hooks is set.
	// See #904.
	ComponentReadiness map[string]map[string][]byte

	// VendorCharts pulls upstream Helm chart bytes into the bundle at
	// bundle time so the resulting artifact is air-gap deployable.
	// Forwarded to the delegated argocd.Generator. Off by default.
	VendorCharts bool

	// Serial forces the delegated argocd.Generator to assign linear
	// per-folder sync-waves (one component at a time) instead of the
	// dependency-depth bands. Forwarded verbatim; the sync-wave annotation
	// survives the application.yaml -> chart template transform. Off by
	// default. Wired from --serial. See argocd.Generator.Serial.
	Serial bool
}

// Generate creates a Helm chart app-of-apps by:
//  1. Delegating to the flat Argo CD deployer for proven Application generation
//  2. Transforming the output into a Helm chart with {{ .Values }} references
func (g *Generator) Generate(ctx context.Context, outputDir string) (*deployer.Output, error) {
	start := time.Now()

	if g.RecipeResult == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "RecipeResult is required")
	}
	if g.BundleChartVersion != "" {
		if _, err := semver.StrictNewVersion(g.BundleChartVersion); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("bundle chart version %q is not strict semantic versioning", g.BundleChartVersion), err)
		}
	}

	// Defense-in-depth: validate AppName at the deployer boundary so a
	// direct library caller (bypassing CLI/API validation) cannot ship a
	// chart whose rendered Application would be rejected by apiserver
	// admission. Empty resolves to DefaultAppName via the template's
	// `.Values.appName | default` fallback at render time.
	if err := bundlercfg.ValidateAppName(g.AppName); err != nil {
		return nil, err
	}
	// Validate NamePrefix upfront (not only per-folder in processFolders)
	// so zero-folder recipes still reject a malformed prefix.
	if err := bundlercfg.ValidateNamePrefix(g.NamePrefix); err != nil {
		return nil, err
	}
	if err := bundlercfg.ValidateDestinationServer(g.DestinationServer); err != nil {
		return nil, err
	}
	// ValidateProject (not ValidateAppName) is load-bearing: it mirrors
	// IsDNS1123Subdomain exactly, matching the install-time
	// values.schema.json pattern, so a baked Project default can never
	// make the generated bundle fail its own schema at install time.
	if err := bundlercfg.ValidateProject(g.Project); err != nil {
		return nil, err
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
	targetRevision := g.targetRevision()

	argocdGen := &argocd.Generator{
		RecipeResult:           g.RecipeResult,
		ComponentValues:        g.ComponentValues,
		Version:                g.Version,
		RepoURL:                g.RepoURL,
		TargetRevision:         targetRevision,
		IncludeChecksums:       false, // we generate our own checksums
		ComponentPreManifests:  g.ComponentPreManifests,
		ComponentPostManifests: g.ComponentPostManifests,
		ComponentReadiness:     g.ComponentReadiness,
		DynamicValues:          g.DynamicValues,
		// Opt into the DynamicValues split: per-component values.yaml has
		// dynamic paths removed; argocdhelm surfaces those paths at the
		// parent chart level via writeStaticValuesAndBuildStubs.
		AllowDynamicValueSplit: true,
		VendorCharts:           g.VendorCharts,
		Serial:                 g.Serial,
		// Forward the effective parent name so the inner generator's
		// parent-collision check tests against THIS deployer's parent
		// ("aicr-stack" or --app-name), not argocd's own "nvidia-stack"
		// default. The inner app-of-apps.yaml is discarded, so the only
		// observable effect is a correct collision baseline.
		AppName: cmp.Or(g.AppName, DefaultAppName),
		// Forward CascadeDelete too: child finalizers survive the YAML
		// round-trip in transformApplication untouched. NamePrefix /
		// DestinationServer / Project are NOT forwarded — those fields
		// are rewritten into `.Values.deployer.*` template expressions
		// during transform, so forwarding would bake values the rewrite
		// then discards.
		CascadeDelete: g.CascadeDelete,
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
	chartName := g.chartName()
	chartPath, chartSize, err := writeChartYAML(outputDir, chartName, g.chartVersion())
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to write Chart.yaml", err)
	}
	output.Files = append(output.Files, chartPath)
	output.TotalSize += chartSize

	// Step 3: Write static values as chart files, the root values.yaml,
	// and the install-time values.schema.json gate.
	if valuesErr := g.writeValuesFiles(outputDir, output); valuesErr != nil {
		return nil, valuesErr
	}

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

// writeValuesFiles writes the chart's value surfaces: per-component
// static/<name>.yaml files, the root values.yaml (with appName, repoURL,
// targetRevision, and deployer defaults injected), and values.schema.json.
// Each written file is appended to output.Files/TotalSize.
func (g *Generator) writeValuesFiles(outputDir string, output *deployer.Output) error {
	staticFiles, staticSize, dynamicOnlyValues, err := g.writeStaticValuesAndBuildStubs(outputDir)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write static values", err)
	}
	output.Files = append(output.Files, staticFiles...)
	output.TotalSize += staticSize

	// Inject appName at the root of values.yaml when the caller chose a
	// non-default name. The parent App template reads `.Values.appName`
	// with the DefaultAppName fallback, so omitting the key on default
	// bundles keeps values.yaml empty when no other dynamic values exist.
	// Install-time `helm install --set appName=...` still overrides this.
	if g.AppName != "" {
		dynamicOnlyValues[rootValuesAppNameKey] = g.AppName
	}

	// Bake the OCI parent namespace as the repoURL default when the bundle was
	// pushed to a registry — `helm show values` and a plain `helm install` both
	// work without --set flags. For local output, OCIParentNamespace is "" and
	// the {{ required }} safety-net is unchanged. See #1342.
	dynamicOnlyValues[rootValuesRepoURLKey] = g.OCIParentNamespace
	dynamicOnlyValues[rootValuesTargetRevisionKey] = ""

	dynamicOnlyValues[rootValuesDeployerKey] = g.deployerValues()

	valuesPath, valuesSize, err := writeRootValuesFile(dynamicOnlyValues, outputDir)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write root values.yaml", err)
	}
	output.Files = append(output.Files, valuesPath)
	output.TotalSize += valuesSize

	schemaPath, schemaSize, err := writeValuesSchema(outputDir)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write values.schema.json", err)
	}
	output.Files = append(output.Files, schemaPath)
	output.TotalSize += schemaSize
	return nil
}

// deployerValues builds the root values.yaml `deployer:` map. Baked
// values become the chart's install-time defaults; every key is
// overridable with `helm install --set deployer.<key>=...`. cascadeDelete
// is intentionally absent — finalizers is a list field that cannot be
// round-tripped as a template expression, so it is bundle-time only.
func (g *Generator) deployerValues() map[string]any {
	destinationServer := g.DestinationServer
	if destinationServer == "" {
		destinationServer = argocd.DefaultDestinationServer
	}
	project := g.Project
	if project == "" {
		project = argocd.DefaultProject
	}
	return map[string]any{
		"namePrefix":        g.NamePrefix,
		"destinationServer": destinationServer,
		"project":           project,
		// includeRootApp gates the parent app-of-apps template. Baked true
		// so `helm show values` documents the default; consumers that bring
		// their own root Application (e.g. a controller pointing an Argo
		// Application at the published chart) set it to false to render
		// children-only and avoid two parents fighting over the same child
		// Applications. See #1723.
		"includeRootApp": true,
	}
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
		fmt.Sprintf("helm install %s .", g.chartName()),
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
			overrideKey, keyErr := resolveOverrideKey(ref.Name, g.RecipeResult.DataProvider())
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

// valuesSchemaProperty, valuesSchemaDeployerProps, valuesSchemaDeployer,
// valuesSchemaRootProps, and valuesSchema model the values.schema.json
// document with fixed struct field order — the output feeds checksums.txt
// and must be byte-deterministic across runs, so map[string]any (whose
// encoding/json key order depends on sorting, and whose shape invites
// accidental nondeterminism) is deliberately avoided.
type valuesSchemaProperty struct {
	Type      string `json:"type"`
	Pattern   string `json:"pattern,omitempty"`
	MaxLength int    `json:"maxLength,omitempty"`
}

type valuesSchemaDeployerProps struct {
	NamePrefix        valuesSchemaProperty `json:"namePrefix"`
	DestinationServer valuesSchemaProperty `json:"destinationServer"`
	Project           valuesSchemaProperty `json:"project"`
	IncludeRootApp    valuesSchemaProperty `json:"includeRootApp"`
}

type valuesSchemaDeployer struct {
	Type                 string                    `json:"type"`
	AdditionalProperties bool                      `json:"additionalProperties"`
	Properties           valuesSchemaDeployerProps `json:"properties"`
}

type valuesSchemaRootProps struct {
	Deployer valuesSchemaDeployer `json:"deployer"`
}

type valuesSchema struct {
	Schema     string                `json:"$schema"`
	Type       string                `json:"type"`
	Properties valuesSchemaRootProps `json:"properties"`
}

// writeValuesSchema emits values.schema.json constraining the deployer.*
// install-time inputs. Helm validates coalesced values against this schema
// on install/upgrade/template/lint, so a typo like
// `--set deployer.destinationSever=...` fails loudly instead of silently
// falling back to the in-cluster default (fail-closed at install time,
// mirroring the bundle-time allowlist in pkg/bundler/config). Only the
// deployer key is constrained: root-level additionalProperties must stay
// open for per-component override maps (gpuoperator: ...). cascadeDelete
// is deliberately omitted — it is bundle-time only, so an install-time
// `--set deployer.cascadeDelete=...` fails this schema by design.
func writeValuesSchema(outputDir string) (string, int64, error) {
	const schemaTypeString = "string"
	schemaPath, err := deployer.SafeJoin(outputDir, "values.schema.json")
	if err != nil {
		return "", 0, err
	}
	schema := valuesSchema{
		Schema: "http://json-schema.org/draft-07/schema#",
		Type:   "object",
		Properties: valuesSchemaRootProps{
			Deployer: valuesSchemaDeployer{
				Type:                 "object",
				AdditionalProperties: false,
				Properties: valuesSchemaDeployerProps{
					NamePrefix: valuesSchemaProperty{
						Type:    schemaTypeString,
						Pattern: `^$|^[a-z0-9][a-z0-9-]*$`,
					},
					DestinationServer: valuesSchemaProperty{
						Type: schemaTypeString,
						// `@` is forbidden to match the bundle-time Go
						// validator, which rejects embedded credentials
						// (https://u:p@host). The first character after
						// https:// must not be `:` or `/` so a hostname-less
						// URL (https://:6443, https:///path) fails closed,
						// matching ValidateHTTPSURL's Hostname() check.
						// The `^$|` alternative allows an explicit-empty
						// install-time value: the child template's
						// `| default` fallback then renders the baked
						// in-cluster default.
						Pattern: `^$|^https://[^'"\s@:/][^'"\s@]*$`,
					},
					Project: valuesSchemaProperty{
						Type: schemaTypeString,
						// Mirrors IsDNS1123Subdomain exactly (total length
						// capped at 253 via maxLength; per-label caps are
						// deliberately NOT enforced because Kubernetes
						// object names don't enforce them — a 64+-char
						// label is a legal AppProject name). The `^$|`
						// alternative allows an explicit-empty install-time
						// value to reset to the baked default via the child
						// template's `| default` fallback.
						Pattern:   `^$|^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`,
						MaxLength: 253,
					},
					// Boolean, not string: `--set deployer.includeRootApp=false`
					// arrives as a bool, while a quoted string "false" fails
					// this schema loudly instead of rendering as truthy in
					// the parent template's dig lookup.
					IncludeRootApp: valuesSchemaProperty{
						Type: "boolean",
					},
				},
			},
		},
	}
	data, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal, "failed to marshal values schema", err)
	}
	data = append(data, '\n')
	if writeErr := os.WriteFile(schemaPath, data, 0600); writeErr != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal, "failed to write values.schema.json", writeErr)
	}
	return schemaPath, int64(len(data)), nil
}

// rootValuesInstallHeader documents the install-time inputs surfaced
// in the bundle's root values.yaml. The empty defaults below let
// `helm show values` list the keys; the parent App template's
// {{ required }} directive enforces non-empty at render time. See
// issue #1020.
const rootValuesInstallHeader = `# Generated by AICR
#
# Install-time inputs (argocd-helm bundles):
#
#   repoURL          Parent OCI namespace or Helm chart repo URL — WITHOUT
#                    the chart name. The parent App template appends
#                    .Chart.Name itself to assemble the full reference.
#                    Pre-filled when pushed to an OCI registry; override
#                    with --set repoURL=oci://mirror when mirroring.
#                    Example: --set repoURL=oci://ghcr.io/nvidia
#
#   targetRevision   Chart version / OCI artifact tag.
#                    Defaults to .Chart.Version when unset at install time.
#                    Example: --set targetRevision=v1.0.0
#
#   deployer.namePrefix          Prefix prepended to every child Application
#                                name (multi-tenant collision avoidance).
#   deployer.destinationServer   Target cluster API URL for child
#                                Applications (default in-cluster).
#   deployer.project             Argo CD project for child Applications.
#   deployer.includeRootApp      Render the parent app-of-apps Application
#                                (default true). Set false when an external
#                                root Application already points at this
#                                chart, so only children render and the two
#                                roots do not fight over the same apps.
#
#   deployer.cascadeDelete is intentionally NOT an install-time value —
#   it is bundle-time only (aicr bundle --set deployer:cascadeDelete=true);
#   helm --set deployer.cascadeDelete=... fails the values schema.
#
---
`

// writeRootValuesFile writes the bundle's root values.yaml with a
// documentation header that explains the required install-time inputs.
// Parallels deployer.WriteValuesFile, with a richer header tailored to
// the argocd-helm contract.
func writeRootValuesFile(values map[string]any, outputDir string) (string, int64, error) {
	outputPath, err := deployer.SafeJoin(outputDir, "values.yaml")
	if err != nil {
		return "", 0, err
	}

	var buf strings.Builder
	buf.WriteString(rootValuesInstallHeader)

	if len(values) > 0 {
		// Deterministic marshal so values.yaml feeding checksums.txt
		// (and the bundle attestation) is byte-stable across runs.
		yamlBytes, err := serializer.MarshalYAMLDeterministic(values)
		if err != nil {
			return "", 0, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to marshal values")
		}
		buf.Write(yamlBytes)
	}

	content := buf.String()
	if err := os.WriteFile(outputPath, []byte(content), 0600); err != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal, "failed to write values file", err)
	}

	return outputPath, int64(len(content)), nil
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
//
// # Two render shapes, dispatched on repoURL scheme
//
// Argo CD has two distinct code paths for chart sources, and the parent
// Application must declare the shape that matches the user-supplied
// repoURL — they are NOT interchangeable:
//
//   - **Native OCI** (`oci://...`): the artifact at `repoURL:<tag>` is
//     fetched and unpacked, then `path` is resolved inside the unpacked
//     tree. `source.chart` is ignored. The template appends
//     `.Chart.Name` to repoURL so the resolved reference is the actual
//     OCI artifact (`oci://<namespace>/<chart>:<tag>`), and sets
//     `path: "."` so the chart renders from the artifact root.
//     User guide: https://argo-cd.readthedocs.io/en/stable/user-guide/oci/
//
//   - **Helm chart repository** (https://, http://, ChartMuseum,
//     GitHub Pages, etc.): the Helm repo at `repoURL` is queried for
//     the chart named in `source.chart`. The template emits the
//     classic `repoURL + chart` pair (no `path`).
//     User guide: https://argo-cd.readthedocs.io/en/stable/user-guide/helm/
//
// PR #1047 attempted a single-shape fix on the OCI path; that broke
// the Helm-repo path. PR #1019 conflated the two with a hardcoded
// chart name. Branching on the scheme here keeps both modes working.
//
// `.Chart.Name` resolves to the bundle chart's name in Chart.yaml,
// which the generator sets from `--output oci://reg/path/<name>` (or
// falls back to DefaultChartName "aicr-bundle"). In both shapes,
// callers MUST omit the chart name from `--set repoURL`; the template
// always assembles the full reference itself.
//
// The whole template is gated on `deployer.includeRootApp` (default true;
// `dig` rather than `default` because `default` would coerce an explicit
// `false` back to true). A consumer that supplies its OWN root Application
// pointing at the published chart — e.g. a CAPI addon provider — sets
// `deployer: {includeRootApp: false}` in that Application's
// `helm.valuesObject` to render children-only. Without the gate the chart
// renders this parent alongside the external root; both then own the same
// child Applications and the parent's automated prune+selfHeal fights the
// external root indefinitely. See issue #1723.
const parentAppTemplate = `{{- if dig "includeRootApp" true (.Values.deployer | default dict) -}}
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: {{ .Values.appName | default "aicr-stack" | quote }}
  namespace: argocd
spec:
  project: default
  source:
{{- $repoURL := required "repoURL is required: pass --set repoURL=<parent namespace> (e.g., oci://<registry>/<path> or https://charts.example.com) — do NOT include the chart name; this template assembles the full reference itself" .Values.repoURL | trimSuffix "/" }}
{{- if hasPrefix "oci://" $repoURL }}
    repoURL: {{ printf "%s/%s" $repoURL .Chart.Name | quote }}
    targetRevision: {{ .Values.targetRevision | default .Chart.Version | quote }}
    path: "."
{{- else }}
    repoURL: {{ $repoURL | quote }}
    chart: {{ .Chart.Name | quote }}
    targetRevision: {{ .Values.targetRevision | default .Chart.Version | quote }}
{{- end }}
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
{{- end }}
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
//	# install — --set repoURL is the PARENT NAMESPACE (no chart name).
//	# Both the parent App template and the path-based child templates
//	# append `/<chart-name>` themselves so Argo CD's native-OCI lookup
//	# resolves at `oci://<namespace>/<chart-name>:<tag>`. Including the
//	# chart name in --set repoURL would double-suffix everything. See
//	# issues #1018 / #1034 and PR #1047 (the regression).
//	helm install aicr-bundle oci://ghcr.io/myorg/aicr-bundle --version <tag> \
//	  -n argocd \
//	  --set repoURL=oci://ghcr.io/myorg \
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
	content := parentAppTemplate
	if g.CascadeDelete {
		content = strings.Replace(content,
			"  namespace: argocd\nspec:",
			"  namespace: argocd\n  finalizers:\n    - "+argocd.ResourcesFinalizer+"\nspec:", 1)
		// strings.Replace no-ops silently if the anchor string is ever
		// refactored out of parentAppTemplate — fail loudly instead of
		// shipping a bundle that ignores --set deployer:cascadeDelete.
		if !strings.Contains(content, argocd.ResourcesFinalizer) {
			return "", 0, errors.New(errors.ErrCodeInternal,
				"failed to inject finalizer into parent Application template")
		}
	}
	if writeErr := os.WriteFile(destPath, []byte(content), 0600); writeErr != nil {
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

		// Components emit synthetic `-pre`, `-post`, and `-readiness` folders
		// that bracket the primary upstream-helm folder. None of these
		// suffixed folders is a registered component on its own — all inherit
		// overrides from the parent (e.g., 006-gpu-operator-pre,
		// 010-gpu-operator-post, and 011-gpu-operator-readiness all flow from
		// gpu-operator). Resolve the parent before looking up the override
		// key; if no suffix matches a registered component, keep the original
		// name and let resolveOverrideKey surface the registry miss.
		//
		// Symmetry note: an earlier version handled only `-post`, then `-pre`
		// (PreManifestFiles, e.g. the gke-cos OS overlay's gpu-driver toolkit
		// prereq). `-readiness` (--readiness-hooks, see #904) joins them:
		// without the matching strip, resolveOverrideKey was called with
		// "gpu-operator-readiness" and failed `component %q not found in
		// registry` at bundle time.
		parentComponent := folderComponent
		for _, suffix := range []string{"-pre", "-post", "-readiness"} {
			if base, ok := strings.CutSuffix(folderComponent, suffix); ok {
				if findComponentByName(g.RecipeResult.ComponentRefs, base) {
					parentComponent = base
					break
				}
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

		// Validate the composed child name at bundle time: the rendered
		// template prepends `.Values.deployer.namePrefix` (defaulting to
		// the baked NamePrefix in root values.yaml), so a prefix that
		// composes into an invalid DNS-1123 name must fail here rather
		// than at apiserver admission.
		if nameErr := bundlercfg.ValidateAppName(g.NamePrefix + folderComponent); nameErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("deployer namePrefix produces invalid child Application name %q", g.NamePrefix+folderComponent), nameErr)
		}
		// Argo CD derives the Helm release name from the Application name
		// for Helm-rendered children, so a composed name over Helm's cap
		// passes DNS-1123 validation but fails at sync time. The cap is
		// applied uniformly to ALL children (including non-Helm ones) to
		// keep a single invariant rather than branching on folder kind.
		// Reject at bundle time; the rendered template guard covers
		// install-time --set deployer.namePrefix overrides.
		composedName := g.NamePrefix + folderComponent
		if len(composedName) > argocd.HelmReleaseNameMaxLen {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("deployer namePrefix produces child Application name %q (%d chars); child Application names are capped at %d characters because Argo CD derives the Helm release name from the Application name for Helm-rendered children", composedName, len(composedName), argocd.HelmReleaseNameMaxLen))
		}
		parentName := g.AppName
		if parentName == "" {
			parentName = DefaultAppName
		}
		if composedName == parentName {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("child Application name %q collides with the parent Application name; choose a different --app-name or deployer namePrefix", composedName))
		}

		overrideKey, keyErr := resolveOverrideKey(parentComponent, g.RecipeResult.DataProvider())
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

	if deployerErr := applyDeployerTemplates(app, componentName); deployerErr != nil {
		return "", 0, deployerErr
	}

	// helm.values is a *yaml.Node with LiteralStyle, so yaml.Marshal emits
	// the raw Helm template as a block scalar that Helm evaluates at render
	// time (rather than a quoted YAML string).
	out, marshalErr := yaml.Marshal(app)
	if marshalErr != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal,
			fmt.Sprintf("failed to marshal transformed application for %s", componentName), marshalErr)
	}
	out = append([]byte(childNameGuard(componentName)), out...)

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

// childNameGuard returns the Helm template guard block prepended to every
// child Application template. The bundle-time checks in processFolders
// cover baked NamePrefix values, but install-time `--set
// deployer.namePrefix=...` bypasses them — these guards fail `helm
// template`/`helm install` when the composed child name exceeds Helm's
// release-name cap (Argo CD uses the Application name as the Helm release
// name) or collides with the parent Application's name. The guard lines
// are Helm template control flow that renders to nothing; Helm template
// files need not be valid YAML pre-render. The appName fallback MUST
// mirror parentAppTemplate's `.Values.appName | default "aicr-stack"`.
// The `toString` coercion is load-bearing: a plain `--set appName=true`
// (or `=123`) arrives as a bool/int64 through Helm's type inference, and
// `eq` fails with "incompatible types for comparison" against the string
// $childName. The parent template survives the same input because it
// pipes through `quote`; the guard must coerce likewise.
func childNameGuard(componentName string) string {
	return fmt.Sprintf(
		`{{- $childName := printf "%%s%%s" ((.Values.deployer | default dict).namePrefix | default "") %q -}}
{{- if gt (len $childName) %d }}{{ fail (printf "deployer.namePrefix produces child Application name %%q (%%d chars): child Application names are capped at %d characters because Argo CD derives the Helm release name from the Application name for Helm-rendered children" $childName (len $childName)) }}{{ end -}}
{{- if eq $childName (.Values.appName | default %q | toString) }}{{ fail (printf "child Application name %%q collides with the parent Application name: choose a different deployer.namePrefix or appName" $childName) }}{{ end -}}
`, componentName, argocd.HelmReleaseNameMaxLen, argocd.HelmReleaseNameMaxLen, DefaultAppName)
}

// applyDeployerTemplates rewrites the child Application fields covered by
// the deployer: option vocabulary into install-time Helm expressions. The
// sprig `default` calls are nil-safety only — real defaults ship in the
// chart's root values.yaml deployer: map, which Helm merges under any
// install-time --set. metadata.finalizers (cascadeDelete) is NOT
// rewritten: it survives the YAML round-trip as baked bundle-time state.
func applyDeployerTemplates(app map[string]any, childName string) error {
	metadata, ok := app["metadata"].(map[string]any)
	if !ok {
		return errors.New(errors.ErrCodeInternal, "application manifest missing 'metadata'")
	}
	metadata["name"] = &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   yamlStringTag,
		Style: yaml.SingleQuotedStyle,
		Value: fmt.Sprintf(`{{ (.Values.deployer | default dict).namePrefix | default "" }}%s`, childName),
	}

	spec, ok := app["spec"].(map[string]any)
	if !ok {
		return errors.New(errors.ErrCodeInternal, "application manifest missing 'spec'")
	}
	spec["project"] = &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   yamlStringTag,
		Style: yaml.SingleQuotedStyle,
		Value: fmt.Sprintf(`{{ (.Values.deployer | default dict).project | default %q }}`, argocd.DefaultProject),
	}
	destination, ok := spec["destination"].(map[string]any)
	if !ok {
		return errors.New(errors.ErrCodeInternal, "application manifest missing 'spec.destination'")
	}
	destination["server"] = &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   yamlStringTag,
		Style: yaml.SingleQuotedStyle,
		Value: fmt.Sprintf(`{{ (.Values.deployer | default dict).destinationServer | default %q }}`, argocd.DefaultDestinationServer),
	}
	return nil
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
// # Publication backends and the path field
//
// Argo CD has two distinct chart-source code paths:
//
//   - **Helm chart repository** (HTTPS / HTTP, ChartMuseum, GitHub
//     Pages, etc.): repoURL is the registry, `source.chart` names the
//     chart. The parent App template emits this shape when repoURL
//     does not start with `oci://`. Pure-Helm bundles (no manifest-
//     only or mixed components) can deploy from this mode. User guide:
//     https://argo-cd.readthedocs.io/en/stable/user-guide/helm/
//
//   - **Native OCI** (`oci://...`): the artifact at
//     `repoURL:<targetRevision>` is fetched and unpacked, then `path`
//     is resolved inside the unpacked tree. `source.chart` is silently
//     ignored. The parent App template appends `.Chart.Name` to
//     repoURL and sets `path: "."` so the chart renders from the
//     artifact root; path-based child Applications resolve
//     `NNN-<name>/` subdirectories inside the same artifact bytes.
//     Bundles with manifest-only or mixed components REQUIRE this
//     mode (the path-based source type is only meaningful under
//     native OCI). User guide:
//     https://argo-cd.readthedocs.io/en/stable/user-guide/oci/
//
// Argo CD supports this multi-source-per-artifact pattern in the OCI
// case because the Application spec — not the registry-level repo
// configuration — determines how a
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
	//
	// Path-based child Applications have no `chart` field — Argo CD's
	// native OCI source uses `repoURL` directly as the full artifact
	// reference and then resolves `path` inside it. We therefore append
	// .Chart.Name here ourselves so the rendered value is
	// `<parent namespace>/<chart name>` (e.g. `oci://reg/org/my-bundle`),
	// matching the artifact `helm push` published the bundle as.
	//
	// The parent App template (parentAppTemplate above) uses the same
	// append-`.Chart.Name`-here shape — both parent and child rely on
	// Argo CD's native-OCI semantics, where `source.chart` is ignored
	// for `oci://` repoURLs. Without the append, --set repoURL=<parent
	// namespace> (the contract every other site in this file documents)
	// produces a child source pointing at `oci://reg/org:tag` — an
	// artifact that does not exist — and the child Application fails
	// to sync. See issue #1034 and PR #1047 / #1048 (parent-side
	// regression that proved the same contract applies to the parent).
	source["repoURL"] = &yaml.Node{
		Kind:  yaml.ScalarNode,
		Tag:   yamlStringTag,
		Style: yaml.SingleQuotedStyle,
		Value: `{{ required "repoURL is required: pass --set repoURL=<parent namespace> (e.g., oci://<registry>/<path>) — do NOT include the chart name; this template appends .Chart.Name to assemble the full OCI artifact reference" .Values.repoURL | trimSuffix "/" }}/{{ .Chart.Name }}`,
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

func writeChartYAML(outputDir, name, version string) (string, int64, error) {
	chartPath, err := deployer.SafeJoin(outputDir, "Chart.yaml")
	if err != nil {
		return "", 0, err
	}

	// Quote name and version so OCI artifact paths whose last segment is
	// a YAML reserved scalar ("null", "true", "false", "yes", "no",
	// "123", etc.) round-trip as strings instead of getting reinterpreted
	// by Helm's YAML parser as the underlying scalar type and producing
	// a chart with an empty Metadata.Name (or a type-mismatch error).
	// fmt %q emits a Go-quoted string that is also valid YAML for all
	// printable ASCII; OCI artifact path segments are constrained to
	// that charset by the docker reference grammar. See issue #1034.
	var buf strings.Builder
	buf.WriteString("apiVersion: v2\n")
	fmt.Fprintf(&buf, "name: %q\n", name)
	buf.WriteString("description: AICR deployment bundle with dynamic install-time values\n")
	buf.WriteString("type: application\n")
	fmt.Fprintf(&buf, "version: %q\n", version)

	content := buf.String()
	if writeErr := os.WriteFile(chartPath, []byte(content), 0600); writeErr != nil {
		return "", 0, errors.Wrap(errors.ErrCodeInternal, "failed to write Chart.yaml", writeErr)
	}
	return chartPath, int64(len(content)), nil
}

// chartName returns the Helm chart name for this Generator, applying the
// "aicr-bundle" default when ChartName is unset. Centralized so the value
// flows consistently into Chart.yaml, the README, and the finalize step.
func (g *Generator) chartName() string {
	if g.ChartName == "" {
		return DefaultChartName
	}
	return g.ChartName
}

func (g *Generator) chartVersion() string {
	if g.BundleChartVersion != "" {
		return g.BundleChartVersion
	}
	return deployer.NormalizeVersionWithDefault(g.RecipeResult.Metadata.Version)
}

func (g *Generator) targetRevision() string {
	if g.TargetRevision != "" {
		return g.TargetRevision
	}
	return g.chartVersion()
}

func (g *Generator) writeReadme(outputDir string) (string, int64, error) {
	readmePath, err := deployer.SafeJoin(outputDir, "README.md")
	if err != nil {
		return "", 0, err
	}

	chartName := g.chartName()

	var buf strings.Builder
	buf.WriteString("# Argo CD Helm Chart Deployment Bundle\n\n")
	buf.WriteString("This bundle is a Helm chart whose templates generate one Argo CD\n")
	buf.WriteString("Application per component plus a parent Application that manages\n")
	buf.WriteString("the chart deployment as a whole. The bundle's `repoURL` defaults\n")
	buf.WriteString("to the registry it was pushed to; override with `--set\n")
	buf.WriteString("repoURL=oci://mirror` when deploying from a different registry.\n\n")

	buf.WriteString("## Deploy\n\n")
	buf.WriteString("`<your-registry>/<path>` below is the **parent namespace** you\n")
	buf.WriteString("publish into. The chart name (`")
	buf.WriteString(chartName)
	buf.WriteString("`) is appended by Helm at\n")
	buf.WriteString("push time and by the parent Application at sync time — do NOT\n")
	buf.WriteString("include the chart name in `--set repoURL` or it will be appended\n")
	buf.WriteString("twice and the parent Application will fail to resolve.\n\n")
	buf.WriteString("```bash\n")
	buf.WriteString("# 1. Publish to your chart registry (any HTTPS OCI / Helm chart repo).\n")
	buf.WriteString("helm package . --destination /tmp/\n")
	fmt.Fprintf(&buf, "helm push /tmp/%s-*.tgz oci://<your-registry>/<path>\n\n", chartName)
	buf.WriteString("# 2. Install from the published chart — supply repoURL (the\n")
	buf.WriteString("#    parent namespace, NOT including the chart name) and\n")
	buf.WriteString("#    targetRevision so the parent Application and path-based child\n")
	buf.WriteString("#    Applications can pull from the registry you pushed to.\n")
	fmt.Fprintf(&buf, "helm install %s oci://<your-registry>/<path>/%s \\\n", chartName, chartName)
	buf.WriteString("  --version <chart-version> -n argocd \\\n")
	buf.WriteString("  --set repoURL=oci://<your-registry>/<path> \\\n")
	buf.WriteString("  --set targetRevision=<chart-version>\n")
	buf.WriteString("```\n\n")

	buf.WriteString("`helm install` against this local directory works only when the\n")
	buf.WriteString("recipe contains pure-Helm components — child Applications whose\n")
	buf.WriteString("source is path-based (manifest-only, mixed `-post`) need Argo's\n")
	buf.WriteString("repo-server to fetch from a remote, so the chart must be published\n")
	buf.WriteString("first for those cases.\n\n")
	fmt.Fprintf(&buf, "```bash\n# Local install (pure-Helm-only recipes)\nhelm install %s . -n argocd \\\n  --set repoURL=oci://<your-registry>/<path> \\\n  --set targetRevision=<chart-version>", chartName)

	dynamicSetFlags, flagsErr := buildDynamicSetFlags(g.DynamicValues, g.RecipeResult.DataProvider())
	if flagsErr != nil {
		return "", 0, flagsErr
	}
	if len(dynamicSetFlags) > 0 {
		buf.WriteString(" \\\n  " + strings.Join(dynamicSetFlags, " \\\n  "))
	}
	buf.WriteString("\n```\n")

	buf.WriteString("\n## Deployer Options\n\n")
	buf.WriteString("Child Application deployer options are install-time overridable:\n\n")
	buf.WriteString("- `--set deployer.namePrefix=<prefix>` — prefix prepended to every\n")
	buf.WriteString("  child Application name (multi-tenant collision avoidance).\n")
	buf.WriteString("- `--set deployer.destinationServer=<url>` — target cluster API URL\n")
	buf.WriteString("  for child Applications (default in-cluster).\n")
	buf.WriteString("- `--set deployer.project=<project>` — Argo CD project for child\n" +
		"  Applications.\n" +
		"- `--set deployer.includeRootApp=false` — render children-only,\n" +
		"  omitting the parent app-of-apps Application. Use when an external\n" +
		"  root Application (e.g. created by a controller) already points at\n" +
		"  this chart; two roots owning the same children fight via automated\n" +
		"  prune/selfHeal.\n\n")
	buf.WriteString("`cascadeDelete` is bundle-time only (finalizers cannot round-trip as\n")
	buf.WriteString("a template expression): `aicr bundle --set deployer:cascadeDelete=true`.\n")

	if len(g.DynamicValues) > 0 {
		buf.WriteString("\n## Dynamic Values\n\n")
		compNames := make([]string, 0, len(g.DynamicValues))
		for name := range g.DynamicValues {
			compNames = append(compNames, name)
		}
		sort.Strings(compNames)
		for _, name := range compNames {
			overrideKey, keyErr := resolveOverrideKey(name, g.RecipeResult.DataProvider())
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

func buildDynamicSetFlags(dynamicValues map[string][]string, provider recipe.DataProvider) ([]string, error) {
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
		overrideKey, keyErr := resolveOverrideKey(name, provider)
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
//
// provider is the recipe-bound DataProvider whose registry is consulted;
// nil falls back to the deprecated process-global registry for callers
// that pre-date per-recipe binding.
func resolveOverrideKey(componentName string, provider recipe.DataProvider) (string, error) {
	registry, err := recipe.GetComponentRegistryFor(provider)
	if err != nil {
		return "", errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"failed to load component registry for override key resolution")
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
