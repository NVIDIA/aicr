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

// Package argocd provides Argo CD Application generation for recipes.
//
// # Bundle layout
//
// Per-component content (Chart.yaml, values.yaml, cluster-values.yaml,
// upstream.env, templates/) is delegated to pkg/bundler/deployer/localformat,
// which emits the uniform NNN-<name>/ folder layout shared by --deployer helm.
// argocd adds a single application.yaml inside each NNN-<name>/ folder plus
// the top-level app-of-apps.yaml and README.md.
//
// # Application shape rule
//
// The shape of each application.yaml branches on the localformat folder kind
// (Chart.yaml presence — see localformat.FolderKind):
//
//   - KindUpstreamHelm (Chart.yaml absent): today's multi-source Application
//     pointing at the upstream Helm repository plus a values $ref to the
//     user's git repo. Unchanged for current users.
//   - KindLocalHelm (Chart.yaml present): single-source path-based Application
//     pointing at the user's git/OCI repo with path: NNN-<name>. Argo's repo
//     server reads the wrapped chart bytes directly. Used for manifest-only
//     and kustomize-wrapped components, and (when enabled) vendored Helm.
//
// This mirrors the deploy.sh branching rule used by --deployer helm: one
// on-disk signal (Chart.yaml presence) drives every consumer.
package argocd

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

//go:embed templates/application.yaml.tmpl
var applicationTemplate string

//go:embed templates/app-of-apps.yaml.tmpl
var appOfAppsTemplate string

//go:embed templates/README.md.tmpl
var readmeTemplate string

// ApplicationData contains data for rendering an Argo CD Application.
//
// IsLocalChart drives the Application shape: when true, the rendered
// Application is single-source path-based (used for KindLocalHelm folders —
// manifest-only and kustomize-wrapped components). When false, the
// Application is multi-source upstream-helm (today's shape, unchanged for
// pure-Helm KindUpstreamHelm folders). BundleDir carries the NNN-<name>
// directory name for both kinds: it is the chart path for KindLocalHelm and
// the values $ref path for KindUpstreamHelm.
type ApplicationData struct {
	Name           string
	Namespace      string
	Repository     string // Upstream Helm repo (KindUpstreamHelm only)
	Chart          string // Helm chart name (KindUpstreamHelm only)
	Version        string // Helm chart version (KindUpstreamHelm only)
	SyncWave       int
	RepoURL        string // User's git/OCI repo (where the bundle is published)
	TargetRevision string // Target revision for the user's repo
	BundleDir      string // NNN-<name> directory inside the bundle
	IsLocalChart   bool   // true → path-based single-source; false → multi-source upstream-helm
}

// AppOfAppsData contains data for rendering the App of Apps manifest.
type AppOfAppsData struct {
	RepoURL        string
	TargetRevision string
	Path           string
}

// ReadmeData contains data for rendering the README.
type ReadmeData struct {
	RecipeVersion  string
	BundlerVersion string
	Components     []ApplicationData
}

// compile-time interface check
var _ deployer.Deployer = (*Generator)(nil)

// Generator creates Argo CD Applications from recipe results.
// Configure it with the required fields, then call Generate.
type Generator struct {
	// RecipeResult contains the recipe metadata and component references.
	RecipeResult *recipe.RecipeResult

	// ComponentValues maps component names to their values.
	ComponentValues map[string]map[string]any

	// Version is the generator version.
	Version string

	// RepoURL is the Git repository URL for the app-of-apps manifest.
	// If empty, a placeholder URL will be used.
	RepoURL string

	// TargetRevision is the target revision for the repo (default: "main").
	TargetRevision string

	// IncludeChecksums indicates whether to generate a checksums.txt file.
	IncludeChecksums bool

	// DataFiles lists additional file paths (relative to output dir) to include
	// in checksum generation. Used for external data files copied into the bundle.
	DataFiles []string

	// ComponentManifests maps component name → manifest path → rendered bytes.
	// Drives wrapping of manifest-only and mixed components into local Helm
	// charts via localformat.Write. Wired by the bundler. Components without
	// manifests do not appear in the map.
	ComponentManifests map[string]map[string][]byte

	// DynamicValues maps component names to their dynamic value paths. The
	// paths are removed from per-component values.yaml during the localformat
	// split. The associated cluster-values.yaml is stripped from the final
	// bundle — Argo CD's repo-server doesn't consume it, and --dynamic is
	// rejected at the CLI for --deployer argocd. Direct library callers must
	// leave this empty unless they also set AllowDynamicValueSplit, otherwise
	// Generate fails fast (the values would be silently removed from
	// values.yaml without a place to surface them).
	DynamicValues map[string][]string

	// AllowDynamicValueSplit lets a delegating caller (currently argocdhelm)
	// opt into forwarding DynamicValues so per-component values.yaml has
	// dynamic paths split out. The caller is responsible for surfacing those
	// paths elsewhere in its own bundle (argocdhelm rebuilds them at the
	// parent chart level). Standalone callers should leave this false.
	AllowDynamicValueSplit bool

	// VendorCharts pulls upstream Helm chart bytes into the bundle at
	// bundle time. Off by default. With the flag set, every Helm-typed
	// component emits a single wrapped chart folder (Chart.yaml +
	// charts/<chart>-<ver>.tgz) and the generated Argo Application uses
	// a path-based single source — registry egress at deploy time is no
	// longer required. See pkg/bundler/deployer/localformat for the
	// vendoring shape.
	VendorCharts bool

	// vendorRecords is populated by Generate when VendorCharts is on.
	// Captured here so VendorRecords() can expose it to callers
	// (currently argocdhelm) that need to write provenance.yaml without
	// re-pulling the charts. Unset (nil) when VendorCharts is off.
	vendorRecords []localformat.VendorRecord
}

// VendorRecords returns a copy of the audit records produced by the
// most recent Generate call when VendorCharts was on. Returns nil
// otherwise. Callers that compose argocd.Generator (argocdhelm) use
// this to thread the records into their own provenance.yaml.
//
// A copy is returned so callers can sort/filter/append without
// silently mutating the Generator's state for subsequent reads.
func (g *Generator) VendorRecords() []localformat.VendorRecord {
	if len(g.vendorRecords) == 0 {
		return nil
	}
	out := make([]localformat.VendorRecord, len(g.vendorRecords))
	copy(out, g.vendorRecords)
	return out
}

// resolveRepoSettings returns the effective repoURL and targetRevision,
// applying defaults when the input values are empty.
// isUnusedForArgoBundle reports whether base is a filename that
// localformat.Write emits for the helm deployer's orchestration but Argo
// CD's repo-server never consumes — see stripUnusedHelmFiles for the
// per-file rationale.
func isUnusedForArgoBundle(base string) bool {
	switch base {
	case "install.sh", "upstream.env", "cluster-values.yaml":
		return true
	}
	return false
}

// stripUnusedHelmFiles removes files that the helm deployer needs but Argo
// CD's repo-server never consumes:
//
//   - install.sh: Argo doesn't run shell scripts.
//   - upstream.env: Argo doesn't source shell env (CHART/REPO/VERSION are
//     baked into the Application's source field directly).
//   - cluster-values.yaml: --dynamic is rejected with --deployer argocd
//     (use --deployer argocd-helm for install-time values), so this file
//     is always an empty stub. Including it confuses users.
//
// Argo CD only reads application.yaml, values.yaml (multi-source
// helm.valueFiles for upstream-helm or local-chart Helm rendering for
// KindLocalHelm), and Chart.yaml/templates/ for local-helm. The kept set
// in each folder.Files is rewritten in place so the subsequent checksum
// tracking sees only the files that survive in the bundle.
func stripUnusedHelmFiles(outputDir string, folders []localformat.Folder) error {
	for i := range folders {
		kept := folders[i].Files[:0]
		for _, rel := range folders[i].Files {
			if isUnusedForArgoBundle(filepath.Base(rel)) {
				abs, joinErr := deployer.SafeJoin(outputDir, rel)
				if joinErr != nil {
					return errors.Wrap(errors.ErrCodeInvalidRequest,
						fmt.Sprintf("path from localformat escapes outputDir: %s", rel), joinErr)
				}
				if rmErr := os.Remove(abs); rmErr != nil && !os.IsNotExist(rmErr) {
					return errors.Wrap(errors.ErrCodeInternal,
						fmt.Sprintf("failed to remove unused file %s", rel), rmErr)
				}
				continue
			}
			kept = append(kept, rel)
		}
		folders[i].Files = kept
	}
	return nil
}

func resolveRepoSettings(g *Generator) (repoURL, targetRevision string) {
	repoURL = g.RepoURL
	if repoURL == "" {
		repoURL = "https://github.com/YOUR-ORG/YOUR-REPO.git"
	}
	targetRevision = g.TargetRevision
	if targetRevision == "" {
		targetRevision = "main"
	}
	return repoURL, targetRevision
}

// Generate creates Argo CD Applications from the configured generator fields.
//
// Per-component content (Chart.yaml, values.yaml, cluster-values.yaml,
// upstream.env, templates/) is delegated to localformat.Write, which emits
// the uniform NNN-<name>/ folder layout. argocd then drops application.yaml
// inside each NNN-<name>/ folder and writes the top-level app-of-apps.yaml.
//
//nolint:funlen // single-pass orchestration; further splitting just hides the linear flow.
func (g *Generator) Generate(ctx context.Context, outputDir string) (*deployer.Output, error) {
	start := time.Now()

	output := &deployer.Output{
		Files: make([]string, 0),
	}

	if g.RecipeResult == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "RecipeResult is required")
	}

	// Reject DynamicValues at the library boundary unless the caller
	// explicitly opts in. The strip-pass below removes cluster-values.yaml
	// from every NNN folder, so a standalone caller that populates
	// DynamicValues would have those splits silently dropped from the
	// bundle. argocdhelm sets AllowDynamicValueSplit because it surfaces
	// the dynamic paths at the parent chart level. The CLI path already
	// rejects --dynamic via bundler.go; this is the equivalent check at
	// the package boundary per the self-contained-business-logic guideline.
	if !g.AllowDynamicValueSplit {
		for name, paths := range g.DynamicValues {
			if len(paths) > 0 {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("DynamicValues is not supported with the argocd deployer (component %q has %d paths); use the argocd-helm deployer for install-time values",
						name, len(paths)))
			}
		}
	}

	// Create output directory
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to create output directory", err)
	}

	repoURL, targetRevision := resolveRepoSettings(g)

	// Sort components by deployment order; validate names early.
	components := deployer.SortComponentRefsByDeploymentOrder(
		g.RecipeResult.ComponentRefs,
		g.RecipeResult.DeploymentOrder,
	)
	for _, comp := range components {
		if !deployer.IsSafePathComponent(comp.Name) {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("invalid component name %q: must not contain path separators or parent directory references", comp.Name))
		}
	}

	// Delegate per-component folder writing to localformat.Write. localformat
	// owns NNN-<name>/ folder creation, Chart.yaml synthesis for wrapped
	// charts, the values.yaml/cluster-values.yaml split via DynamicPaths, and
	// install-related content. This deployer adds the Argo Application file
	// inside each folder plus the top-level app-of-apps.yaml afterwards.
	lfComponents := g.toLocalformatComponents(components)
	writeResult, lfErr := localformat.Write(ctx, localformat.Options{
		OutputDir:          outputDir,
		Components:         lfComponents,
		ComponentManifests: g.ComponentManifests,
		VendorCharts:       g.VendorCharts,
	})
	if lfErr != nil {
		// localformat.Write returns StructuredErrors; propagate as-is.
		return nil, lfErr
	}
	g.vendorRecords = writeResult.VendoredCharts
	folders := writeResult.Folders

	if err := stripUnusedHelmFiles(outputDir, folders); err != nil {
		return nil, err
	}

	// Track per-folder content paths for checksums.
	for _, f := range folders {
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

	// Build ApplicationData per folder and write application.yaml inside the
	// NNN-<name>/ directory. Branching on FolderKind selects the Application
	// shape (path-based single-source vs multi-source upstream-helm).
	appDataList := make([]ApplicationData, 0, len(folders))
	for i, f := range folders {
		select {
		case <-ctx.Done():
			return nil, errors.PropagateOrWrap(ctx.Err(), errors.ErrCodeTimeout, "context cancelled")
		default:
		}

		comp := findComponentRef(components, f.Parent)
		if comp == nil {
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("localformat returned folder for unknown component %q", f.Parent))
		}

		appData := buildApplicationData(*comp, f, i, repoURL, targetRevision)
		appDataList = append(appDataList, appData)

		folderDir, joinErr := deployer.SafeJoin(outputDir, f.Dir)
		if joinErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("folder path unsafe: %s", f.Dir), joinErr)
		}
		appPath, appSize, genErr := deployer.GenerateFromTemplate(applicationTemplate, appData, folderDir, "application.yaml")
		if genErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to generate application.yaml for %s", f.Name), genErr)
		}
		output.Files = append(output.Files, appPath)
		output.TotalSize += appSize
	}

	// Generate app-of-apps.yaml
	appOfAppsData := AppOfAppsData{
		RepoURL:        repoURL,
		TargetRevision: targetRevision,
		Path:           ".",
	}
	appOfAppsPath, appOfAppsSize, err := deployer.GenerateFromTemplate(appOfAppsTemplate, appOfAppsData, outputDir, "app-of-apps.yaml")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate app-of-apps.yaml", err)
	}
	output.Files = append(output.Files, appOfAppsPath)
	output.TotalSize += appOfAppsSize

	// Generate README.md
	readmeData := ReadmeData{
		RecipeVersion:  g.RecipeResult.Metadata.Version,
		BundlerVersion: g.Version,
		Components:     appDataList,
	}
	readmePath, readmeSize, err := deployer.GenerateFromTemplate(readmeTemplate, readmeData, outputDir, "README.md")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate README.md", err)
	}
	output.Files = append(output.Files, readmePath)
	output.TotalSize += readmeSize

	// Include external data files in the file list (for checksums).
	if err := output.AddDataFiles(outputDir, g.DataFiles); err != nil {
		return nil, err
	}

	// Emit provenance.yaml for vendored bundles. Same audit file as the
	// helm deployer — operators get the chart-yank lookup surface
	// regardless of which deployer they choose. Written before
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

	if g.IncludeChecksums {
		if err := checksum.WriteChecksums(ctx, outputDir, output); err != nil {
			return nil, err
		}
	}

	output.Duration = time.Since(start)

	// Populate deployment steps for CLI output
	output.DeploymentSteps = []string{
		"Push the generated files to your GitOps repository",
		fmt.Sprintf("kubectl apply -f %s/app-of-apps.yaml", outputDir),
	}
	// Add note if repo URL needs to be updated
	if g.RepoURL == "" {
		output.DeploymentNotes = []string{
			"Update app-of-apps.yaml with your repository URL before applying",
		}
	}

	slog.Debug("argocd applications generated",
		"components", len(appDataList),
		"files", len(output.Files),
		"size_bytes", output.TotalSize,
	)

	return output, nil
}

// toLocalformatComponents maps the sorted ComponentRefs to the per-component
// inputs consumed by localformat.Write. Mirrors helm.toLocalformatComponents;
// not lifted into a shared helper because the two callers carry slightly
// different fields (helm uses ComponentData, argocd uses ComponentRef).
func (g *Generator) toLocalformatComponents(refs []recipe.ComponentRef) []localformat.Component {
	out := make([]localformat.Component, 0, len(refs))
	for _, ref := range refs {
		chart := ref.Chart
		if chart == "" {
			chart = ref.Name
		}
		values := g.ComponentValues[ref.Name]
		if values == nil {
			values = make(map[string]any)
		}
		out = append(out, localformat.Component{
			Name:         ref.Name,
			Namespace:    ref.Namespace,
			Repository:   ref.Source,
			ChartName:    chart,
			Version:      ref.Version,
			IsOCI:        strings.HasPrefix(ref.Source, "oci://"),
			Tag:          ref.Tag,
			Path:         ref.Path,
			Values:       values,
			DynamicPaths: g.DynamicValues[ref.Name],
		})
	}
	return out
}

// findComponentRef returns the ComponentRef whose Name matches parent, or nil.
// Used to map a localformat-emitted folder back to its originating recipe
// component for Application generation. Mixed components emit two folders
// (primary + injected -post) — both have Folder.Parent == ref.Name.
func findComponentRef(refs []recipe.ComponentRef, parent string) *recipe.ComponentRef {
	for i := range refs {
		if refs[i].Name == parent {
			return &refs[i]
		}
	}
	return nil
}

// buildApplicationData constructs ApplicationData for a single folder. The
// FolderKind drives the Application shape — KindLocalHelm sets LocalChartPath
// (path-based single-source); KindUpstreamHelm leaves it empty (multi-source
// upstream-helm). The folder's name is used as the Application name to keep
// primary and injected -post folders distinct in Argo.
func buildApplicationData(comp recipe.ComponentRef, f localformat.Folder, syncWave int, repoURL, targetRevision string) ApplicationData {
	chart := comp.Chart
	if chart == "" {
		chart = comp.Name
	}
	data := ApplicationData{
		Name:           f.Name,
		Namespace:      comp.Namespace,
		SyncWave:       syncWave,
		RepoURL:        repoURL,
		TargetRevision: targetRevision,
		BundleDir:      f.Dir,
	}
	switch f.Kind {
	case localformat.KindLocalHelm:
		data.IsLocalChart = true
	case localformat.KindUpstreamHelm:
		// Mirror localformat's OCI URL-construction convention: the recipe's
		// source field carries registry+namespace ONLY, and the chart name
		// is appended by the consumer (see writeUpstreamHelmFolder in
		// localformat/upstream_helm.go). For OCI, Argo CD's Helm pull builds
		// the chart reference as `<repoURL>:<targetRevision>` and ignores
		// the chart field, so we have to bake the chart name into repoURL
		// for it to find the artifact.
		//
		// Tag handling: OCI tags are literal — `helm push` preserves the
		// recipe's "v1.3.0" verbatim, so stripping the `v` prefix here
		// produces a tag that doesn't exist. HTTPS Helm-chart-repo sources
		// use index.yaml with non-prefixed conventions, so normalize there.
		if strings.HasPrefix(comp.Source, "oci://") {
			data.Repository = strings.TrimRight(comp.Source, "/") + "/" + chart
			data.Version = comp.Version
		} else {
			data.Repository = comp.Source
			data.Version = deployer.NormalizeVersion(comp.Version)
		}
		data.Chart = chart
	}
	return data
}
