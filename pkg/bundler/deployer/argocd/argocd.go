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
	bundlercfg "github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/localformat"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
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
//
// InlineValues drives the Application shape for KindUpstreamHelm when the
// bundle repo is OCI: instead of the multi-source $values pattern (Git-only
// in Argo CD), the Application is rendered single-source with helm.valuesObject
// inlined from ValuesYAML. See InlineUpstreamValues on Generator for the
// reason.
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
	InlineValues   bool   // KindUpstreamHelm + OCI → single-source with helm.valuesObject inlined
	ValuesYAML     string // Pre-indented YAML (8 spaces) for helm.valuesObject; used when InlineValues

	// DestinationServer is the spec.destination.server value; populated
	// from Generator.DestinationServer with DefaultDestinationServer as
	// the fallback. See #1625.
	DestinationServer string

	// Project is the spec.project value; populated from Generator.Project
	// with DefaultProject as the fallback. See #1625.
	Project string

	// CascadeDelete adds ResourcesFinalizer to the rendered Application.
	// See #1628.
	CascadeDelete bool
}

// AppOfAppsData contains data for rendering the App of Apps manifest.
type AppOfAppsData struct {
	RepoURL        string
	TargetRevision string
	Path           string
	// AppName is the parent Application's `metadata.name`. The argocd
	// deployer materializes a static manifest at bundle time, so the value
	// is baked into app-of-apps.yaml and is not overridable at apply time.
	// Defaults to DefaultAppName ("nvidia-stack"). See issue #1011.
	AppName string

	// CascadeDelete adds ResourcesFinalizer to the parent Application so
	// deleting it prunes the child Applications and their resources.
	// See #1628.
	CascadeDelete bool
}

// DefaultAppName is the parent App-of-Apps `metadata.name` written into
// the generated manifest when Generator.AppName is empty. Two AICR
// bundles installed into the same Argo CD namespace must carry distinct
// names so their parent Applications do not overwrite each other —
// see issue #1011.
const DefaultAppName = "nvidia-stack"

// DefaultDestinationServer is the in-cluster API server URL written into
// generated Application destinations when no deployer destinationServer
// override is supplied.
const DefaultDestinationServer = "https://kubernetes.default.svc"

// DefaultProject is the Argo CD project written into generated
// Applications when no deployer project override is supplied.
const DefaultProject = "default"

// HelmReleaseNameMaxLen is Helm's release-name length cap. Argo CD
// defaults the Helm release name to the Application's metadata.name
// (https://argo-cd.readthedocs.io/en/stable/user-guide/helm/#helm-release-name),
// so composed child names longer than this generate Applications that
// pass DNS-1123 validation but fail at sync with Helm's
// "release name exceeds max length of 53" error.
const HelmReleaseNameMaxLen = 53

// ResourcesFinalizer is Argo CD's cascading-deletion finalizer. When
// CascadeDelete is set it is added to the parent and every child
// Application so `kubectl delete` on the parent prunes managed
// resources. See #1628 and
// https://argo-cd.readthedocs.io/en/stable/user-guide/app_deletion/
const ResourcesFinalizer = "resources-finalizer.argocd.argoproj.io"

// syncWaveStride is the number of Argo CD sync-wave slots reserved per
// dependency level. Each component contributes up to four ordered folders
// within its level's band (-pre, primary, -post, -readiness), so reserving a
// fixed stride of that width keeps every level's band disjoint: level N+1's
// first wave is strictly greater than level N's last (its readiness gate).
// Components that share a level share a band and therefore deploy in
// parallel, while dependents in a later level still wait for the whole prior
// level — including its gate Jobs — to go Healthy. See waveForFolder.
const syncWaveStride = 4

// ReadmeData contains data for rendering the README.
type ReadmeData struct {
	RecipeVersion  string
	BundlerVersion string
	Components     []ApplicationData
	// AppName mirrors AppOfAppsData.AppName — the rendered README cites
	// the parent Application name in its `argocd app get/sync` examples,
	// so the value must match what app-of-apps.yaml was templated with.
	AppName string
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

	// ComponentPreManifests maps component name → manifest path → rendered
	// bytes for manifests that apply BEFORE each component's primary chart.
	// Forwarded to localformat.Options.ComponentPreManifests. Wired by the
	// bundler; populated from ComponentRef.PreManifestFiles. Components
	// without pre-manifests do not appear in the map.
	ComponentPreManifests map[string]map[string][]byte

	// ComponentPostManifests maps component name → manifest path → rendered
	// bytes for manifests that apply AFTER each component's primary chart.
	// Drives wrapping of manifest-only and mixed components into local Helm
	// charts via localformat.Write. Wired by the bundler; populated from
	// ComponentRef.ManifestFiles. Components without manifests do not
	// appear in the map.
	ComponentPostManifests map[string]map[string][]byte

	// ComponentReadiness maps component name → manifest path → rendered bytes
	// for the per-component readiness gate (a local Helm chart wrapping the
	// gate Job + RBAC + ConfigMap). localformat.Write emits it as a folder
	// immediately after the component's primary (and any -post) folder, so the
	// generated Argo CD Application inherits the next sync-wave index. Argo CD
	// blocks that wave on the gate Job via its built-in batch/Job health —
	// Progressing until the Job completes, Healthy on success, Degraded on
	// failure — the analog of helm --wait-for-jobs. No custom health Lua is
	// required. Populated by the bundler from readiness.yaml only when
	// --readiness-hooks is set; empty otherwise. See #904.
	ComponentReadiness map[string]map[string][]byte

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

	// Serial assigns each folder a strictly increasing sync-wave equal to its
	// linear position instead of the dependency-depth band (waveForFolder),
	// so components deploy one at a time in deployment order. Off by default
	// (parallel). Operator escape hatch wired from --serial; see
	// config.Serial. argocdhelm forwards its own Serial here.
	Serial bool

	// AppName overrides the parent App-of-Apps `metadata.name`. Empty
	// falls back to DefaultAppName ("nvidia-stack"). The value is baked
	// into the rendered app-of-apps.yaml at bundle time — Argo CD's
	// `argocd` deployer materializes a static manifest, not a Helm chart,
	// so the choice cannot be deferred to apply time. See issue #1011.
	AppName string

	// NamePrefix is prepended to every child Application metadata.name.
	// The parent Application name is covered by AppName. Composed names
	// are validated as DNS-1123 subdomains at generation time. See #1625.
	NamePrefix string

	// DestinationServer overrides spec.destination.server on child
	// Applications only; empty falls back to DefaultDestinationServer.
	// The parent stays on the control-plane cluster — Application CRs are
	// reconciled only from the cluster running Argo CD. See #1625.
	DestinationServer string

	// Project overrides spec.project on child Applications only; empty
	// falls back to DefaultProject. The parent stays in "default" — a
	// project able to create Applications in the Argo CD namespace is
	// effectively admin. See #1625.
	Project string

	// CascadeDelete adds ResourcesFinalizer to the parent and every child
	// Application. Baked at bundle time. See #1628.
	CascadeDelete bool

	// InlineUpstreamValues replaces the multi-source $values pattern for
	// KindUpstreamHelm Applications with a single source whose helm.valuesObject
	// is inlined from ComponentValues. Required when RepoURL is OCI because
	// Argo CD's $values multi-source ref is Git-only — the repo-server
	// attempts a `git ListRefs` against the OCI URL and fails with
	// `unsupported scheme "oci"`. Off by default to preserve the existing
	// multi-source shape for Git-backed deployments (operators retain
	// per-component values.yaml edit ergonomics). Wired by the bundler
	// when --deployer argocd targets oci://. Should remain off for
	// argocdhelm's inner Generator — that path relies on the multi-source
	// shape to transform it into a Helm template with dynamic merging.
	InlineUpstreamValues bool

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

// appName returns the parent App-of-Apps `metadata.name` for this
// Generator, applying the DefaultAppName fallback when the caller did
// not set Generator.AppName. Centralized so the same value flows into
// the rendered manifest, README examples, and any future audit fields.
func (g *Generator) appName() string {
	if g.AppName == "" {
		return DefaultAppName
	}
	return g.AppName
}

// destinationServer returns the effective child-Application destination,
// applying the DefaultDestinationServer fallback when unset.
func (g *Generator) destinationServer() string {
	if g.DestinationServer == "" {
		return DefaultDestinationServer
	}
	return g.DestinationServer
}

// project returns the effective child-Application project, applying the
// DefaultProject fallback when unset.
func (g *Generator) project() string {
	if g.Project == "" {
		return DefaultProject
	}
	return g.Project
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

	// Defense-in-depth: validate AppName at the deployer boundary so a
	// direct library caller (bypassing the CLI/API validation) cannot
	// produce a manifest that fails at apiserver admission. Empty is
	// accepted and resolves to DefaultAppName via appName().
	if err := bundlercfg.ValidateAppName(g.AppName); err != nil {
		return nil, err
	}
	if err := bundlercfg.ValidateNamePrefix(g.NamePrefix); err != nil {
		return nil, err
	}
	if err := bundlercfg.ValidateDestinationServer(g.DestinationServer); err != nil {
		return nil, err
	}
	// ValidateProject also enforces the per-label 63-character cap so the
	// generated project value stays within what Argo CD / the argocd-helm
	// install-time schema accept (see pkg/bundler/config.ValidateProject).
	if err := bundlercfg.ValidateProject(g.Project); err != nil {
		return nil, err
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
		OutputDir:              outputDir,
		Components:             lfComponents,
		ComponentPreManifests:  g.ComponentPreManifests,
		ComponentPostManifests: g.ComponentPostManifests,
		ComponentReadiness:     g.ComponentReadiness,
		VendorCharts:           g.VendorCharts,
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

	// Group components into dependency-depth levels so independent components
	// share a sync-wave band and roll out in parallel. This mirrors the
	// helmfile deployer, which consumes the same function to emit its
	// per-level sub-helmfiles — keeping ordering semantics identical across
	// deployers. levelOf maps each component to its level index; waveForFolder
	// turns (level, folder phase) into the concrete sync-wave.
	//
	// Computed unconditionally so cycle / undeclared-dependency validation
	// runs on both paths — Generate is exported, and a direct caller could
	// otherwise hand-build a RecipeResult with a bad graph and skip the check
	// in --serial mode. levelOf is only populated for the parallel path; in
	// --serial mode it stays empty and the folder loop uses each folder's
	// linear index as the sync-wave (one component at a time).
	levels, levelErr := recipe.ComponentRefsTopologicalLevels(components)
	if levelErr != nil {
		return nil, errors.PropagateOrWrap(levelErr, errors.ErrCodeInternal,
			"failed to compute dependency levels")
	}
	levelOf := make(map[string]int, len(components))
	if !g.Serial {
		for lvl, names := range levels {
			for _, name := range names {
				levelOf[name] = lvl
			}
		}
	}

	// Build ApplicationData per folder and write application.yaml inside the
	// NNN-<name>/ directory. Branching on FolderKind selects the Application
	// shape (path-based single-source vs multi-source upstream-helm).
	appDataList := make([]ApplicationData, 0, len(folders))
	appName := g.appName()
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

		var inlineValues map[string]any
		if g.InlineUpstreamValues && f.Kind == localformat.KindUpstreamHelm {
			inlineValues = g.ComponentValues[comp.Name]
		}

		wave := i
		if !g.Serial {
			wave = waveForFolder(f, levelOf[comp.Name])
		}
		appData, err := buildApplicationData(*comp, f, wave, repoURL, targetRevision, inlineValues, g.InlineUpstreamValues)
		if err != nil {
			return nil, err
		}
		appData.Name = g.NamePrefix + appData.Name
		if err := bundlercfg.ValidateAppName(appData.Name); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("deployer namePrefix produces invalid child Application name %q", appData.Name), err)
		}
		// Argo CD derives the Helm release name from the Application name
		// for Helm-rendered children, so a composed name over Helm's cap
		// passes DNS-1123 validation but fails at sync time. The cap is
		// applied uniformly to ALL children (including non-Helm ones) to
		// keep a single invariant rather than branching on folder kind.
		// Reject at bundle time instead.
		if len(appData.Name) > HelmReleaseNameMaxLen {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("deployer namePrefix produces child Application name %q (%d chars); child Application names are capped at %d characters because Argo CD derives the Helm release name from the Application name for Helm-rendered children", appData.Name, len(appData.Name), HelmReleaseNameMaxLen))
		}
		// A child that composes to the parent's name would overwrite the
		// parent Application CR in the argocd namespace.
		if appData.Name == appName {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("child Application name %q collides with the parent Application name; choose a different --app-name or deployer namePrefix", appData.Name))
		}
		appData.DestinationServer = g.destinationServer()
		appData.Project = g.project()
		appData.CascadeDelete = g.CascadeDelete
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
		AppName:        appName,
		CascadeDelete:  g.CascadeDelete,
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
		AppName:        appName,
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
		chart := ref.EffectiveChart()
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

// waveForFolder computes the Argo CD sync-wave for a localformat folder under
// level-based scheduling. Components sharing a dependency level share a wave
// band [level*syncWaveStride, level*syncWaveStride+3], so mutually independent
// components (e.g. cert-manager, nfd, nodewright-operator) deploy in parallel
// instead of one wave at a time. The per-phase offset within the band
// preserves the strict intra-component ordering the old linear scheme got for
// free from consecutive folder indices:
//
//	-pre        base + 0   (namespaces / PSS prereqs, before the chart)
//	primary     base + 1   (the component's chart or manifests)
//	-post       base + 2   (raw manifests applied after the chart's CRDs)
//	-readiness  base + 3   (gate Job; its wave blocks the next level)
//
// where base = level*syncWaveStride. Because the band width equals the phase
// count, level N+1's first wave ((level+1)*syncWaveStride) is strictly greater
// than level N's readiness wave, so Argo CD holds the next tier until every
// resource in the prior level — the gate Job included — is Healthy. That is
// the same barrier the linear scheme provided, now applied per dependency
// level rather than per component.
//
// The phase is derived from the folder name relative to its parent component
// (a primary folder has Name == Parent). A component literally named
// "<x>-pre"/"-post"/"-readiness" would collide, but localformat rejects those
// suffixes when a sibling injects the matching auxiliary folder and reserves
// "-readiness" outright, so the primary (Name == Parent) fallthrough is safe.
func waveForFolder(f localformat.Folder, level int) int {
	base := level * syncWaveStride
	switch f.Name {
	case f.Parent + "-pre":
		return base
	case f.Parent + "-post":
		return base + 2
	case f.Parent + "-readiness":
		return base + 3
	default: // primary: Name == Parent
		return base + 1
	}
}

// buildApplicationData constructs ApplicationData for a single folder. The
// FolderKind drives the Application shape — KindLocalHelm sets IsLocalChart
// (path-based single-source); KindUpstreamHelm leaves it empty (multi-source
// upstream-helm). The folder's name is used as the Application name to keep
// primary and injected -post folders distinct in Argo.
//
// When inline is true and the folder is KindUpstreamHelm, values are
// marshaled to deterministic YAML and indented 8 spaces for embedding under
// helm.valuesObject. The result is a single-source Application that avoids
// Argo CD's Git-only $values multi-source ref. Errors from value marshaling
// surface as ErrCodeInternal.
func buildApplicationData(comp recipe.ComponentRef, f localformat.Folder, syncWave int, repoURL, targetRevision string, values map[string]any, inline bool) (ApplicationData, error) {
	chart := comp.EffectiveChart()
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

		if inline {
			data.InlineValues = true
			yamlStr, err := renderInlineValuesYAML(values)
			if err != nil {
				return ApplicationData{}, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
					fmt.Sprintf("failed to render inline values for %s", comp.Name))
			}
			data.ValuesYAML = yamlStr
		}
	}
	return data, nil
}

// renderInlineValuesYAML marshals values to deterministic YAML and indents
// every line by 8 spaces so the result drops cleanly under
// `spec.source.helm.valuesObject:` (column 6) at column 8.
//
// Empty / nil values produce "        {}\n" so the template always emits a
// well-formed map node — Argo CD's schema validation rejects a bare key with
// no value. Trailing-blank lines from yaml.v3 are stripped to keep the
// rendered Application stable across runs.
func renderInlineValuesYAML(values map[string]any) (string, error) {
	if len(values) == 0 {
		return "        {}\n", nil
	}
	yamlBytes, err := serializer.MarshalYAMLDeterministic(values)
	if err != nil {
		return "", err
	}
	const indent = "        "
	lines := strings.Split(strings.TrimRight(string(yamlBytes), "\n"), "\n")
	var b strings.Builder
	for _, line := range lines {
		if line == "" {
			b.WriteString("\n")
			continue
		}
		b.WriteString(indent)
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String(), nil
}
