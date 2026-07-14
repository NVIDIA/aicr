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

package bundler

import (
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/argocd"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/argocdhelm"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/flux"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/helm"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/helmfile"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	"github.com/NVIDIA/aicr/pkg/bundler/types"
	"github.com/NVIDIA/aicr/pkg/bundler/validations"
	"github.com/NVIDIA/aicr/pkg/bundler/verifier"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/netutil"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// readBoundedFile streams a file through io.LimitReader against maxBytes.
// Used in place of os.ReadFile on paths that may be attacker-influenced
// (e.g., symlinks into /proc, NFS swaps) so the process cannot be forced
// to allocate an unbounded buffer before the size limit kicks in.
func readBoundedFile(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // caller validates path
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("file %q exceeds %d-byte limit", path, maxBytes))
	}
	return data, nil
}

const (
	// digestAlgoSHA256 is the algorithm key used in attestation digest maps.
	digestAlgoSHA256 = "sha256"

	// recipeFileName is the resolved recipe copied into Helm bundles.
	recipeFileName = "recipe.yaml"
)

// errCtxKeyComponent is the structured-error context key carrying the
// component name in bundle value-override failures.
const errCtxKeyComponent = "component"

// DefaultBundler generates Helm per-component bundles from recipes.
//
// The per-component approach produces a directory per component, each with its
// own values.yaml, README, and optional manifests. A root deploy.sh orchestrates
// installation in order:
//
//	chmod +x deploy.sh
//	./deploy.sh
//
// Thread-safety: DefaultBundler is safe for concurrent use.
type DefaultBundler struct {
	// Config provides bundler-specific configuration including value overrides.
	Config *config.Config

	// AllowLists defines which criteria values are permitted for bundle requests.
	// When set, the bundler validates that the recipe's criteria are within the allowed values.
	AllowLists *recipe.AllowLists

	// Attester signs bundle content. NoOpAttester is used when --attest is not set.
	Attester attestation.Attester

	// warnings stores warning messages to be added to deployment notes.
	warnings []string
}

// Option defines a functional option for configuring DefaultBundler.
type Option func(*DefaultBundler)

// WithConfig sets the bundler configuration.
// The config contains value overrides, node selectors, tolerations, etc.
func WithConfig(cfg *config.Config) Option {
	return func(db *DefaultBundler) {
		if cfg != nil {
			db.Config = cfg
		}
	}
}

// WithAttester sets the attestation provider for bundle signing.
func WithAttester(a attestation.Attester) Option {
	return func(db *DefaultBundler) {
		if a != nil {
			db.Attester = a
		}
	}
}

// WithAllowLists sets the criteria allowlists for the bundler.
// When configured, the bundler validates that recipe criteria are within allowed values.
func WithAllowLists(al *recipe.AllowLists) Option {
	return func(db *DefaultBundler) {
		db.AllowLists = al
	}
}

// New creates a new DefaultBundler with the given options.
//
// Example:
//
//	b, err := bundler.New(
//	    bundler.WithConfig(config.NewConfig(
//	        config.WithValueOverrides(overrides),
//	    )),
//	)
func New(opts ...Option) (*DefaultBundler, error) {
	db := &DefaultBundler{
		Config:   config.NewConfig(),
		Attester: attestation.NewNoOpAttester(),
	}

	for _, opt := range opts {
		opt(db)
	}

	// Fail fast: if attestation is requested, verify that the binary attestation
	// file exists before any expensive work (OIDC auth, recipe resolution, bundle
	// generation). Binaries installed via "go install" or manual download won't
	// have the attestation file that is included in release archives.
	if db.Config.Attest() {
		binaryPath, err := os.Executable()
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				"could not resolve executable path; remove --attest to skip", err)
		}
		if _, err := attestation.FindBinaryAttestation(binaryPath); err != nil {
			return nil, errors.New(errors.ErrCodeNotFound,
				fmt.Sprintf("binary attestation not found at %s\n\n"+
					"The --attest flag requires a binary installed using the install script, which\n"+
					"includes a cryptographic attestation from NVIDIA. Binaries installed via\n"+
					"\"go install\" or manual download do not include this file.\n\n"+
					"To fix:\n"+
					"  - Reinstall using the install script\n"+
					"  - Or remove --attest to generate bundles without attestation",
					binaryPath+attestation.AttestationFileSuffix))
		}
	}

	return db, nil
}

// NewWithConfig creates a new DefaultBundler with the given config.
// This is a convenience function equivalent to New(WithConfig(cfg)).
func NewWithConfig(cfg *config.Config) (*DefaultBundler, error) {
	return New(WithConfig(cfg))
}

// Make generates a deployment bundle from the given recipe.
// By default, generates a Helm per-component bundle. If deployer is set to "argocd",
// generates Argo CD Application manifests.
//
// For Helm per-component output:
//   - README.md: Root deployment guide with ordered steps
//   - deploy.sh: Automation script (0755)
//   - recipe.yaml: Copy of the input recipe
//   - <component>/values.yaml: Helm values per component
//   - <component>/README.md: Component install/upgrade/uninstall
//   - <component>/manifests/: Optional manifest files
//   - checksums.txt: SHA256 checksums of generated files
//
// For Argo CD output:
//   - app-of-apps.yaml: Parent Argo CD Application
//   - <component>/application.yaml: Argo CD Application per component
//   - <component>/values.yaml: Values for each component
//   - README.md: Deployment instructions
//
// Returns a result.Output summarizing the generation results.
func (b *DefaultBundler) Make(ctx context.Context, recipeResult *recipe.RecipeResult, dir string) (*result.Output, error) {
	start := time.Now()

	// Reset warnings so they dont accumulate between multiple bundle generations
	b.warnings = nil

	// Validate input
	if recipeResult == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "recipe result cannot be nil")
	}

	if len(recipeResult.ComponentRefs) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"recipe must contain at least one component reference")
	}

	// Reject incoherent component refs (e.g. a Helm ref that also carries a
	// Kustomize tag/path, which the deployers silently build as Kustomize)
	// before generating anything. DefaultBundler.Make is a public entry point
	// (docs/integrator/public-api.md) reachable without the CLI/server
	// validation boundaries, so it must apply the same coherence gate. Validate
	// a provider-preserving defensive copy — a struct copy keeps the bound
	// provider, and a fresh ComponentRefs slice keeps type back-fill/
	// canonicalization from mutating the caller's RecipeResult. See #1584.
	validated := *recipeResult
	validated.ComponentRefs = append([]recipe.ComponentRef(nil), recipeResult.ComponentRefs...)
	if err := validated.PrepareAndValidate(); err != nil {
		return nil, err
	}
	recipeResult = &validated

	enabledRefs, filteredOrder, filterErr := b.filterEnabledComponents(recipeResult)
	if filterErr != nil {
		return nil, filterErr
	}

	// Work on a shallow copy so the caller's RecipeResult is not mutated
	filtered := *recipeResult
	filtered.ComponentRefs = enabledRefs
	filtered.DeploymentOrder = filteredOrder
	recipeResult = &filtered

	// Set default output directory
	if dir == "" {
		dir = "."
	}

	// Create output directory
	if dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				"failed to create output directory", err)
		}
	}

	// Bundle-time override policy for GPU allocation-policy keys (#1327):
	// reject --dynamic declarations, warn on static overrides. Runs after
	// alias resolution against the recipe-bound registry, before values are
	// extracted, so REST/SDK callers get the same enforcement as the CLI.
	if policyErr := b.enforceAllocationPolicyOverrides(recipeResult.DataProvider()); policyErr != nil {
		return nil, policyErr
	}

	// Extract values for each component from the recipe
	componentValues, err := b.extractComponentValues(ctx, recipeResult)
	if err != nil {
		if _, ok := stderrors.AsType[*errors.StructuredError](err); ok {
			return nil, err
		}
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to extract component values", err)
	}

	// Bundler-derived annotations that must reflect the final resolved
	// recipe state, applied AFTER extractComponentValues so that user
	// --set overrides cannot defeat them. Every deployer (Helm,
	// helmfile, Flux, Argo CD, argocd-helm) sees the same final map.
	// See issue #973.
	b.injectDRAChartVersionAnnotation(componentValues, recipeResult)

	if warningErr := b.warnMissingStorageClassForPVCs(ctx, recipeResult, componentValues); warningErr != nil {
		return nil, warningErr
	}

	if exposureErr := b.resolveAgentgatewayExposure(componentValues); exposureErr != nil {
		return nil, exposureErr
	}

	// Run component-specific validations
	if validationErr := b.runComponentValidations(ctx, recipeResult); validationErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"component validation failed", validationErr)
	}

	// Copy external data files before deployer construction so the file list
	// is available for both the deployer (checksum tracking) and post-generation
	// attestation. This is a no-op when --data is not set.
	dataFiles, err := b.copyDataFiles(dir, recipeResult.DataProvider())
	if err != nil {
		if _, ok := stderrors.AsType[*errors.StructuredError](err); ok {
			return nil, err
		}
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to copy external data files", err)
	}

	// Build the deployer and run it
	d, err := b.buildDeployer(ctx, recipeResult, componentValues, dataFiles)
	if err != nil {
		return nil, err
	}
	return b.runDeployer(ctx, d, recipeResult, dir, dataFiles, start)
}

// buildDeployer constructs the appropriate deployer.Deployer based on config.
// It handles deployer-specific pre-flight validation and data collection.
func (b *DefaultBundler) buildDeployer(ctx context.Context, recipeResult *recipe.RecipeResult, componentValues map[string]map[string]any, dataFiles []string) (deployer.Deployer, error) {
	dynamicValues, err := b.buildDynamicValuesMap(recipeResult.DataProvider())
	if err != nil {
		return nil, err
	}

	slog.Debug("generating bundle",
		"deployer", b.Config.Deployer(),
		"component_count", len(recipeResult.ComponentRefs),
		"dynamic_components", len(dynamicValues),
	)

	// Readiness gate emission is wired for the deployers whose ordering
	// primitive can block on the gate Job:
	//   - helm: deploy.sh runs the folder's install.sh with
	//     `helm upgrade --install --wait --wait-for-jobs`.
	//   - argocd / argocd-helm: the gate folder inherits the next sync-wave,
	//     and Argo CD's built-in batch/Job health blocks that wave until the
	//     Job completes.
	// Flux and helmfile wrap each folder in HelmRelease / needs semantics that
	// need dedicated gating wiring (not yet implemented). Fail clearly rather
	// than silently dropping the opt-in flag and shipping a bundle without the
	// readiness gate the user asked for. See #904.
	if b.Config.ReadinessHooks() {
		switch b.Config.Deployer() {
		case config.DeployerHelm, config.DeployerArgoCD, config.DeployerArgoCDHelm:
			// supported
		case config.DeployerFlux, config.DeployerHelmfile:
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("--readiness-hooks is not supported with --deployer %q; supported deployers: helm, argocd, argocd-helm",
					b.Config.Deployer()))
		}
	}

	argoOpts, err := b.argoDeployerOptions()
	if err != nil {
		return nil, err
	}

	switch b.Config.Deployer() {
	case config.DeployerArgoCDHelm:
		// --repo is meaningful for --deployer argocd (baked into child
		// Application sources) but a no-op here: the argocd-helm bundle
		// is URL-portable and the publish location is supplied at
		// `helm install` time via `--set repoURL=...`. Warn loudly so
		// users don't think their flag value is taking effect.
		if b.Config.RepoURL() != "" {
			slog.Warn("--repo is ignored with --deployer argocd-helm; supply the URL at install time via `helm install --set repoURL=...`",
				"repo", b.Config.RepoURL())
		}
		componentPreManifests, err := b.collectComponentPreManifests(ctx, recipeResult)
		if err != nil {
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
				"failed to collect component pre-manifests")
		}
		componentPostManifests, err := b.collectComponentManifests(ctx, recipeResult)
		if err != nil {
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
				"failed to collect component post-manifests")
		}
		componentReadiness, err := b.collectComponentReadiness(ctx, recipeResult)
		if err != nil {
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
				"failed to collect component readiness gates")
		}
		return &argocdhelm.Generator{
			RecipeResult:           recipeResult,
			ComponentValues:        componentValues,
			Version:                b.Config.Version(),
			RepoURL:                b.Config.RepoURL(),
			TargetRevision:         b.Config.TargetRevision(),
			IncludeChecksums:       b.Config.IncludeChecksums(),
			DynamicValues:          dynamicValues,
			DataFiles:              dataFiles,
			ComponentPreManifests:  componentPreManifests,
			ComponentPostManifests: componentPostManifests,
			ComponentReadiness:     componentReadiness,
			VendorCharts:           b.Config.VendorCharts(),
			ChartName:              b.Config.BundleChartName(),
			AppName:                b.Config.AppName(),
			OCIParentNamespace:     b.Config.OCIParentNamespace(),
			NamePrefix:             argoOpts.NamePrefix,
			DestinationServer:      argoOpts.DestinationServer,
			Project:                argoOpts.Project,
			CascadeDelete:          argoOpts.CascadeDelete,
		}, nil

	case config.DeployerArgoCD:
		if b.Config.HasDynamicValues() {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				"dynamic declarations are not supported with deployer \"argocd\"; use deployer \"argocd-helm\" instead")
		}
		componentPreManifests, err := b.collectComponentPreManifests(ctx, recipeResult)
		if err != nil {
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
				"failed to collect component pre-manifests")
		}
		componentPostManifests, err := b.collectComponentManifests(ctx, recipeResult)
		if err != nil {
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
				"failed to collect component post-manifests")
		}
		componentReadiness, err := b.collectComponentReadiness(ctx, recipeResult)
		if err != nil {
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
				"failed to collect component readiness gates")
		}
		return &argocd.Generator{
			RecipeResult:           recipeResult,
			ComponentValues:        componentValues,
			Version:                b.Config.Version(),
			RepoURL:                b.Config.RepoURL(),
			TargetRevision:         b.Config.TargetRevision(),
			IncludeChecksums:       b.Config.IncludeChecksums(),
			DataFiles:              dataFiles,
			ComponentPreManifests:  componentPreManifests,
			ComponentPostManifests: componentPostManifests,
			ComponentReadiness:     componentReadiness,
			VendorCharts:           b.Config.VendorCharts(),
			AppName:                b.Config.AppName(),
			NamePrefix:             argoOpts.NamePrefix,
			DestinationServer:      argoOpts.DestinationServer,
			Project:                argoOpts.Project,
			CascadeDelete:          argoOpts.CascadeDelete,
			// Inline values when the bundle repo is OCI: Argo CD's $values
			// multi-source ref is Git-only (see #960), so an OCI repoURL
			// must use single-source with helm.valuesObject embedded.
			InlineUpstreamValues: strings.HasPrefix(b.Config.RepoURL(), "oci://"),
		}, nil

	case config.DeployerHelm:
		componentPreManifests, err := b.collectComponentPreManifests(ctx, recipeResult)
		if err != nil {
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
				"failed to collect component pre-manifests")
		}
		componentPostManifests, err := b.collectComponentManifests(ctx, recipeResult)
		if err != nil {
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
				"failed to collect component post-manifests")
		}
		componentReadiness, err := b.collectComponentReadiness(ctx, recipeResult)
		if err != nil {
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
				"failed to collect component readiness gates")
		}
		return &helm.Generator{
			RecipeResult:           recipeResult,
			ComponentValues:        componentValues,
			Version:                b.Config.Version(),
			IncludeChecksums:       b.Config.IncludeChecksums(),
			RecipeFile:             recipeFileName,
			ComponentPreManifests:  componentPreManifests,
			ComponentPostManifests: componentPostManifests,
			ComponentReadiness:     componentReadiness,
			DataFiles:              dataFiles,
			DynamicValues:          dynamicValues,
			VendorCharts:           b.Config.VendorCharts(),
		}, nil

	case config.DeployerFlux:
		componentPreManifests, preErr := b.collectComponentPreManifests(ctx, recipeResult)
		if preErr != nil {
			return nil, errors.PropagateOrWrap(preErr, errors.ErrCodeInternal,
				"failed to collect component pre-manifests")
		}
		componentManifests, manifestErr := b.collectComponentManifests(ctx, recipeResult)
		if manifestErr != nil {
			return nil, errors.PropagateOrWrap(manifestErr, errors.ErrCodeInternal,
				"failed to collect component manifests")
		}
		return &flux.Generator{
			RecipeResult:          recipeResult,
			ComponentValues:       componentValues,
			Version:               b.Config.Version(),
			RepoURL:               b.Config.RepoURL(),
			TargetRevision:        b.Config.TargetRevision(),
			IncludeChecksums:      b.Config.IncludeChecksums(),
			DataFiles:             dataFiles,
			ComponentPreManifests: componentPreManifests,
			ComponentManifests:    componentManifests,
			DynamicValues:         dynamicValues,
			Namespace:             b.Config.FluxNamespace(),
			OCISourceName:         b.Config.OCISourceName(),
			VendorCharts:          b.Config.VendorCharts(),
		}, nil

	case config.DeployerHelmfile:
		componentPreManifests, err := b.collectComponentPreManifests(ctx, recipeResult)
		if err != nil {
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
				"failed to collect component pre-manifests")
		}
		componentPostManifests, err := b.collectComponentManifests(ctx, recipeResult)
		if err != nil {
			return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
				"failed to collect component post-manifests")
		}
		return &helmfile.Generator{
			RecipeResult:           recipeResult,
			ComponentValues:        componentValues,
			Version:                b.Config.Version(),
			IncludeChecksums:       b.Config.IncludeChecksums(),
			ComponentPreManifests:  componentPreManifests,
			ComponentPostManifests: componentPostManifests,
			DataFiles:              dataFiles,
			DynamicValues:          dynamicValues,
			VendorCharts:           b.Config.VendorCharts(),
		}, nil

	default:
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported deployer type: %s", b.Config.Deployer()))
	}
}

// argoDeployerOptions parses the deployer-level Argo Application options
// (`--set deployer:<key>=<value>`) from the value overrides. Parsing lives
// here — in front of deployer construction — so both the CLI and API paths
// share a single validation point. The options only apply to the Argo CD
// generators; fail closed for every other deployer so a typo or unsupported
// combination never silently ships a misconfigured bundle. See #1625.
// Typed (--set-json / --set-file) deployer overrides are rejected up front:
// every deployer option is a scalar, so silently dropping typed input would
// ship a misconfigured artifact.
// Returns a zero-value options struct when no deployer overrides were set.
func (b *DefaultBundler) argoDeployerOptions() (*config.ArgoDeployerOptions, error) {
	if typed := b.Config.ValueOverridesTyped()[config.DeployerOverrideKey]; len(typed) > 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"deployer options do not support --set-json/--set-file; use --set deployer:<key>=<value>")
	}
	opts, err := config.ParseArgoDeployerOptions(
		b.Config.ValueOverrides()[config.DeployerOverrideKey])
	if err != nil {
		return nil, err
	}
	if opts == nil {
		return &config.ArgoDeployerOptions{}, nil
	}
	if d := b.Config.Deployer(); d != config.DeployerArgoCD && d != config.DeployerArgoCDHelm {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("--set deployer:<key> options are only supported with --deployer argocd or argocd-helm (got %q)", d))
	}
	return opts, nil
}

// runDeployer executes a deployer and builds the result output.
// dataFiles is the list of external data file paths already copied by Make().
func (b *DefaultBundler) runDeployer(ctx context.Context, d deployer.Deployer, recipeResult *recipe.RecipeResult, dir string, dataFiles []string, start time.Time) (*result.Output, error) {
	// Helm bundles include the resolved recipe. Write it before generation so
	// the Helm deployer can include it in checksums.txt and the resulting
	// attestation chain.
	if b.Config.Deployer() == config.DeployerHelm {
		if _, writeErr := b.writeRecipeFile(recipeResult, dir); writeErr != nil {
			return nil, errors.PropagateOrWrap(writeErr, errors.ErrCodeInternal, "failed to write recipe file")
		}
	}

	output, err := d.Generate(ctx, dir)
	if err != nil {
		if _, ok := stderrors.AsType[*errors.StructuredError](err); ok {
			return nil, err
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate bundle", err)
	}

	totalFiles := len(output.Files)
	totalSize := output.TotalSize

	// Attest bundle (skips internally when not configured)
	attestFiles, err := b.attestBundle(ctx, dir, dataFiles, recipeResult)
	if err != nil {
		return nil, err
	}
	totalFiles += len(attestFiles)

	// Map deployer type to result and deployment names
	resultType, deploymentType := deployerResultNames(b.Config.Deployer())

	// Build result
	resultOutput := &result.Output{
		Results:       make([]*result.Result, 0),
		Errors:        make([]result.BundleError, 0),
		TotalDuration: time.Since(start),
		TotalSize:     totalSize,
		TotalFiles:    totalFiles,
		OutputDir:     dir,
	}

	bundleResult := &result.Result{
		Type:     resultType,
		Success:  true,
		Files:    append(output.Files, attestFiles...),
		Size:     output.TotalSize,
		Duration: output.Duration,
	}
	resultOutput.Results = append(resultOutput.Results, bundleResult)

	// Deployment info
	var notes []string
	if len(output.DeploymentNotes) > 0 {
		notes = append(notes, output.DeploymentNotes...)
	}
	if len(b.warnings) > 0 {
		notes = append(notes, b.warnings...)
	}
	resultOutput.Deployment = &result.DeploymentInfo{
		Type:  deploymentType,
		Steps: output.DeploymentSteps,
		Notes: notes,
	}

	slog.Debug("bundle generation complete",
		"deployer", b.Config.Deployer(),
		"files", len(output.Files),
		"size_bytes", output.TotalSize,
		"duration", output.Duration,
	)

	return resultOutput, nil
}

// deployerResultNames returns the result type and deployment type display names
// for a given deployer type, preserving the human-readable names used in output.
func deployerResultNames(dt config.DeployerType) (types.BundleType, string) {
	switch dt {
	case config.DeployerHelm:
		return "helm-bundle", "Helm per-component bundle"
	case config.DeployerArgoCD:
		return "argocd-applications", "Argo CD applications"
	case config.DeployerArgoCDHelm:
		return "argocd-helm-chart", "Argo CD Helm chart app-of-apps"
	case config.DeployerFlux:
		return "flux-manifests", "Flux manifests"
	case config.DeployerHelmfile:
		return "helmfile-bundle", "Helmfile release graph"
	default:
		return types.BundleType(dt), string(dt)
	}
}

// extractComponentValues extracts and processes values for each component in the recipe.
// It loads base values from the recipe, applies user overrides, and applies node selectors.
func (b *DefaultBundler) extractComponentValues(ctx context.Context, recipeResult *recipe.RecipeResult) (map[string]map[string]any, error) {
	componentValues := make(map[string]map[string]any)
	provider := recipeResult.DataProvider()

	for _, ref := range recipeResult.ComponentRefs {
		if err := ctx.Err(); err != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled during component value extraction", err)
		}

		// Get base values from recipe
		values, err := recipeResult.GetValuesForComponentWithContext(ctx, ref.Name)
		if err != nil {
			slog.Warn("failed to get values for component, using empty map",
				"component", ref.Name,
				"error", err,
			)
			values = make(map[string]any)
		}

		// Apply user value overrides from --set flags.
		// Strip "enabled" key — it controls component inclusion, not Helm chart values.
		setOverrides := b.getValueOverridesForComponent(ref.Name, provider)
		if len(setOverrides) > 0 {
			if _, has := setOverrides["enabled"]; has {
				filtered := make(map[string]string, len(setOverrides)-1)
				for k, v := range setOverrides {
					if k == "enabled" {
						continue
					}
					filtered[k] = v
				}
				setOverrides = filtered
			}
			if applyErr := component.ApplyMapOverrides(values, setOverrides); applyErr != nil {
				// User-supplied --set overrides must produce the values the
				// user asked for; silently dropping them ships a bundle
				// that doesn't reflect the CLI inputs. Fail loudly so the
				// user can correct the typo or invalid path.
				return nil, errors.WrapWithContext(errors.ErrCodeInvalidRequest,
					"failed to apply --set value overrides",
					applyErr,
					map[string]any{errCtxKeyComponent: ref.Name})
			}
		}

		// Compute the set of scheduling paths the user explicitly populated
		// (recipe overlay's inline overrides + CLI --set). These take
		// precedence over CLI/config defaults inside
		// applyNodeSchedulingOverrides. Component default valuesFile values
		// are intentionally excluded — see authoritativeSchedulingPaths godoc.
		policy := b.computeSchedulingPathPolicy(&ref, provider, setOverrides)

		// Paths declared via --dynamic must not have scheduling values baked in.
		// Merging them into optOut causes applyNodeSchedulingOverrides to skip
		// injection for those paths; the path will be absent from values entirely
		// so operators can supply the toleration at install time without
		// rebuilding the bundle. See #1371.
		if dynPaths := b.dynamicPathSetFor(ref.Name, provider); len(dynPaths) > 0 {
			for path := range dynPaths {
				policy.optOut[path] = struct{}{}
			}
		}

		// Apply node selectors, tolerations, workload selector, and taints based on component type
		b.applyNodeSchedulingOverrides(ref.Name, values, provider, policy)

		// Apply structured --set-json / --set-file overrides last so they take
		// precedence over base values, scalar --set, and scheduling injection.
		// Unlike --set, these carry lists/objects: object values deep-merge into
		// any existing map at the path, while lists and scalars replace it. This
		// is the type-safe path for list/object fields like
		// agentgateway.allowedSourceRanges that --set cannot express. See #1161.
		if typedOverrides := b.getTypedValueOverridesForComponent(ref.Name, provider); len(typedOverrides) > 0 {
			// Reject the "enabled" toggle on the typed path here — below the CLI
			// boundary — so non-CLI/SDK callers that build TypedComponentPath
			// values directly are guarded too, not just the CLI flag parser.
			// Routing it through --set-json/--set-file would write a stray
			// literal `enabled:` into chart values rather than toggle the
			// component; it is honored only via scalar --set.
			if _, hasEnabled := typedOverrides[config.ComponentEnabledKey]; hasEnabled {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("component %q: %q is the enable/disable toggle and must be set with --set "+
						"(e.g. --set %s:%s=false), not --set-json/--set-file",
						ref.Name, config.ComponentEnabledKey, ref.Name, config.ComponentEnabledKey))
			}
			if applyErr := component.ApplyTypedOverrides(values, typedOverrides); applyErr != nil {
				return nil, errors.WrapWithContext(errors.ErrCodeInvalidRequest,
					"failed to apply --set-json/--set-file value overrides",
					applyErr,
					map[string]any{errCtxKeyComponent: ref.Name})
			}
		}

		componentValues[ref.Name] = values
	}

	return componentValues, nil
}

// componentOverrideKeys returns the candidate override-map keys for
// componentName, in priority order: the exact name first, then either its
// non-hyphenated form (when the registry is unavailable) or the registry's
// ValueOverrideKeys aliases. Shared by the string (--set) and typed
// (--set-json / --set-file) override lookups so both resolve component
// aliases identically. The provider argument scopes the registry lookup to
// the recipe's bound DataProvider; a nil provider falls back to the
// package-global registry via GetComponentRegistryFor.
func (b *DefaultBundler) componentOverrideKeys(componentName string, provider recipe.DataProvider) []string {
	keys := []string{componentName}

	registry, err := recipe.GetComponentRegistryFor(provider)
	if err != nil {
		// Registry unavailable: fall back to the non-hyphenated form only.
		if nonHyphenated := removeHyphens(componentName); nonHyphenated != componentName {
			keys = append(keys, nonHyphenated)
		}
		return keys
	}

	if comp := registry.Get(componentName); comp != nil {
		keys = append(keys, comp.ValueOverrideKeys...)
	}

	return keys
}

// mergeOverridesAcrossKeys merges the per-path override maps stored under every
// candidate key (the exact component name plus its registry aliases) into a
// single map, so overrides supplied under a mix of canonical and alias names —
// e.g. both gpu-operator and gpuoperator — are all honored rather than the
// first-matching key silently dropping the rest. When the same path appears
// under more than one key, the higher-priority key (earlier in keys: the exact
// name before its aliases) wins. Returns nil when no candidate key has
// overrides, preserving the callers' "no overrides" contract.
func mergeOverridesAcrossKeys[V any](allOverrides map[string]map[string]V, keys []string) map[string]V {
	var merged map[string]V
	// Apply in reverse priority order so earlier (higher-priority) keys
	// overwrite later ones on a path collision.
	for i := len(keys) - 1; i >= 0; i-- {
		overrides, ok := allOverrides[keys[i]]
		if !ok {
			continue
		}
		if merged == nil {
			merged = make(map[string]V, len(overrides))
		}
		for path, value := range overrides {
			merged[path] = value
		}
	}
	return merged
}

// getValueOverridesForComponent returns scalar (--set) value overrides for a
// specific component, merging overrides supplied under both the exact name and
// any registry override-key aliases via componentOverrideKeys. Returns nil when
// none apply.
func (b *DefaultBundler) getValueOverridesForComponent(componentName string, provider recipe.DataProvider) map[string]string {
	if b.Config == nil {
		return nil
	}

	allOverrides := b.Config.ValueOverrides()
	if allOverrides == nil {
		return nil
	}

	return mergeOverridesAcrossKeys(allOverrides, b.componentOverrideKeys(componentName, provider))
}

// getTypedValueOverridesForComponent returns structured (--set-json /
// --set-file) value overrides for a specific component, resolving and merging
// component aliases the same way getValueOverridesForComponent does. Returns
// nil when none apply.
func (b *DefaultBundler) getTypedValueOverridesForComponent(componentName string, provider recipe.DataProvider) map[string]any {
	if b.Config == nil {
		return nil
	}

	allOverrides := b.Config.ValueOverridesTyped()
	if allOverrides == nil {
		return nil
	}

	return mergeOverridesAcrossKeys(allOverrides, b.componentOverrideKeys(componentName, provider))
}

// filterEnabledComponents resolves the set of components to bundle by applying
// recipe-level overrides.enabled, bundle-time --set enabled toggles, and the
// positive bundlers component-name filter (config.WithBundlers, #1531), then
// returns the enabled refs (with dangling dependency edges pruned) alongside
// the deployment order filtered to those refs.
//
// Bundle-time --set can disable a component the recipe enabled
// (--set <c>:enabled=false), but it cannot re-enable a component the recipe
// deliberately disabled: the author disables a component because the platform
// already provides it (e.g. a CSP-managed cert-manager on OKE), so re-enabling
// would install a conflicting second copy and there is no authored deployment
// order for it. Such an attempt is rejected with ErrCodeInvalidRequest.
func (b *DefaultBundler) filterEnabledComponents(recipeResult *recipe.RecipeResult) ([]recipe.ComponentRef, []string, error) {
	// declaredSet is every component the recipe names, regardless of enabled
	// state. It distinguishes a declared-but-disabled dependency (prune the
	// edge — satisfied externally) from an undeclared one (keep it so topology
	// validation still errors on a malformed recipe).
	declaredSet := make(map[string]struct{}, len(recipeResult.ComponentRefs))
	for _, ref := range recipeResult.ComponentRefs {
		declaredSet[ref.Name] = struct{}{}
	}

	enabledRefs := make([]recipe.ComponentRef, 0, len(recipeResult.ComponentRefs))
	enabledSet := make(map[string]struct{})
	for _, ref := range recipeResult.ComponentRefs {
		setEnabled, ok, overrideErr := b.getSetEnabledOverride(ref.Name, recipeResult.DataProvider())
		if overrideErr != nil {
			return nil, nil, overrideErr
		}
		recipeEnabled := ref.IsEnabled()
		if ok {
			if setEnabled && !recipeEnabled {
				return nil, nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
					"component %q is disabled by the recipe and cannot be re-enabled with "+
						"--set %s:%s=true", ref.Name, ref.Name, config.ComponentEnabledKey))
			}
			if !setEnabled {
				slog.Info("skipping component disabled via --set", "component", ref.Name)
				continue
			}
			// setEnabled && recipeEnabled: explicit --set enabled=true is a no-op.
		} else if !recipeEnabled {
			slog.Info("skipping disabled component", "component", ref.Name)
			continue
		}
		enabledRefs = append(enabledRefs, ref)
		enabledSet[ref.Name] = struct{}{}
	}

	// Apply the positive component-name filter (POST /v1/bundle ?bundlers=…,
	// config.WithBundlers). Requested names must be declared AND enabled —
	// an unknown name is a typo the operator needs to hear about, and a
	// disabled one mirrors the --set re-enable rejection above (the recipe
	// author disabled it because the platform provides it). Enabled
	// components outside the requested set are skipped exactly like
	// disabled ones, so the dependency-edge pruning below treats them as
	// satisfied externally. See #1531.
	if b.Config != nil {
		if requested := b.Config.Bundlers(); len(requested) > 0 {
			requestedSet := make(map[string]struct{}, len(requested))
			for _, name := range requested {
				if _, declared := declaredSet[name]; !declared {
					declaredNames := make([]string, 0, len(recipeResult.ComponentRefs))
					for _, ref := range recipeResult.ComponentRefs {
						declaredNames = append(declaredNames, ref.Name)
					}
					return nil, nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
						"unknown component %q in bundlers filter; recipe declares: %s",
						name, strings.Join(declaredNames, ", ")))
				}
				if _, enabled := enabledSet[name]; !enabled {
					return nil, nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
						"component %q is disabled and cannot be selected via the bundlers filter", name))
				}
				requestedSet[name] = struct{}{}
			}
			kept := make([]recipe.ComponentRef, 0, len(requestedSet))
			for _, ref := range enabledRefs {
				if _, ok := requestedSet[ref.Name]; !ok {
					slog.Info("skipping component excluded by bundlers filter", "component", ref.Name)
					delete(enabledSet, ref.Name)
					continue
				}
				kept = append(kept, ref)
			}
			enabledRefs = kept
		}
	}

	if len(enabledRefs) == 0 {
		return nil, nil, errors.New(errors.ErrCodeInvalidRequest,
			"recipe has no enabled components after filtering")
	}

	// Prune dependency edges that point at a declared-but-disabled component
	// removed above. After filtering, such a dependency is no longer present in
	// the ref slice, so a deployer that recomputes ordering from these refs
	// (e.g. helmfile via ComponentRefsTopologicalLevels) would otherwise treat
	// the dangling edge as an undeclared dependency and fail with a false
	// circular-dependency error. The dependency is assumed satisfied externally
	// (the reason it was disabled). An edge to a genuinely undeclared component
	// is left intact so topology validation still errors on a malformed recipe.
	for i := range enabledRefs {
		deps := enabledRefs[i].DependencyRefs
		if len(deps) == 0 {
			continue
		}
		pruned := make([]string, 0, len(deps))
		for _, dep := range deps {
			_, isEnabled := enabledSet[dep]
			_, isDeclared := declaredSet[dep]
			if isDeclared && !isEnabled {
				// declared-but-disabled: satisfied externally, drop the edge.
				continue
			}
			pruned = append(pruned, dep)
		}
		if len(pruned) != len(deps) {
			enabledRefs[i].DependencyRefs = pruned
		}
	}

	// Filter DeploymentOrder to match enabled components, preserving the
	// recipe's authored order.
	filteredOrder := make([]string, 0, len(recipeResult.DeploymentOrder))
	for _, name := range recipeResult.DeploymentOrder {
		if _, ok := enabledSet[name]; ok {
			filteredOrder = append(filteredOrder, name)
		}
	}

	return enabledRefs, filteredOrder, nil
}

// getSetEnabledOverride checks if --set overrides contain an "enabled" key
// for the given component. Returns (value, true, nil) if found, (false, false, nil)
// when no override exists, or (_, _, err) if the override value cannot be parsed
// as a bool. A parse failure is fatal — silently ignoring it would ship a
// bundle whose enable/disable state doesn't match the operator's intent, which
// is the canonical misconfigured-artifact scenario the project rule targets.
// This allows --set awsebscsidriver:enabled=false to disable a component at bundle time.
// The provider argument scopes the registry lookup to the recipe's bound DataProvider.
func (b *DefaultBundler) getSetEnabledOverride(componentName string, provider recipe.DataProvider) (bool, bool, error) {
	overrides := b.getValueOverridesForComponent(componentName, provider)
	if overrides == nil {
		return false, false, nil
	}
	val, ok := overrides["enabled"]
	if !ok {
		return false, false, nil
	}
	parsed, parseErr := strconv.ParseBool(val)
	if parseErr != nil {
		return false, false, errors.WrapWithContext(errors.ErrCodeInvalidRequest,
			"invalid --set enabled value", parseErr,
			map[string]any{errCtxKeyComponent: componentName, "value": val})
	}
	return parsed, true, nil
}

// schedulingPathPolicy is the per-path policy the bundler applies during
// node-scheduling injection. It is derived from the recipe overlay's
// inline overrides (componentRefs[].overrides) and CLI --set values; the
// component's default valuesFile is intentionally NOT consulted so a
// chart-default toleration shipped in values.yaml (e.g., kueue's
// `controllerManager.tolerations: [{Exists}]`) does not silently turn
// --system-node-toleration into a no-op.
type schedulingPathPolicy struct {
	// optOut paths are populated by the overlay/--set with an explicitly
	// empty value (empty slice for tolerations, empty map for selectors).
	// Injection is skipped entirely — e.g. kind.yaml's
	// `daemonsets.tolerations: []` keeps GPU operands off the
	// control-plane by letting the chart default kick in.
	optOut map[string]struct{}
	// appendMode paths are populated by the overlay/--set with a
	// NON-empty toleration list — the overlay's intent is "ALSO tolerate
	// these", so CLI tolerations augment the overlay list rather than
	// replace it (e.g. bcm.yaml's `controller.tolerations` for the
	// BCM-master taints must coexist with the KWOK system-pool taint
	// passed via --system-node-toleration).
	appendMode map[string]struct{}
}

// computeSchedulingPathPolicy classifies every registry-declared scheduling
// path for the component into optOut / appendMode / (implicit) replace.
func (b *DefaultBundler) computeSchedulingPathPolicy(ref *recipe.ComponentRef, provider recipe.DataProvider, setOverrides map[string]string) schedulingPathPolicy {
	if ref == nil {
		return schedulingPathPolicy{}
	}
	registry, err := recipe.GetComponentRegistryFor(provider)
	if err != nil {
		return schedulingPathPolicy{}
	}
	comp := registry.Get(ref.Name)
	if comp == nil {
		return schedulingPathPolicy{}
	}
	allPaths := make([]string, 0, 16)
	allPaths = append(allPaths, comp.GetSystemNodeSelectorPaths()...)
	allPaths = append(allPaths, comp.GetSystemTolerationPaths()...)
	allPaths = append(allPaths, comp.GetAcceleratedNodeSelectorPaths()...)
	allPaths = append(allPaths, comp.GetAcceleratedTolerationPaths()...)
	allPaths = append(allPaths, comp.GetWorkloadSelectorPaths()...)
	return classifySchedulingPaths(ref.Overrides, setOverrides, allPaths)
}

// classifySchedulingPaths returns the opt-out / append classification for
// the given dot-notation paths. A path is opt-out when the overlay or --set
// resolves it to an empty slice/map; append when it resolves to a non-empty
// value; otherwise unclassified (treated as replace by the caller).
func classifySchedulingPaths(overrides map[string]any, setOverrides map[string]string, paths []string) schedulingPathPolicy {
	policy := schedulingPathPolicy{
		optOut:     make(map[string]struct{}),
		appendMode: make(map[string]struct{}),
	}
	for _, p := range paths {
		val, hasOverlay := overlayValueAt(overrides, p)
		_, hasSet := setOverrides[p]
		if !hasOverlay && !hasSet {
			continue
		}
		// Opt-out is gated on the OVERLAY only: --set passes string values
		// that cannot meaningfully represent a "no tolerations" sentinel
		// (an empty-list overlay literal is the canonical opt-out gesture).
		if hasOverlay && isEmptyOverlayValue(val) {
			policy.optOut[p] = struct{}{}
			continue
		}
		policy.appendMode[p] = struct{}{}
	}
	return policy
}

// overlayValueAt returns the value at the dot-notation path in the recipe
// overlay's inline overrides, plus whether the path resolved.
func overlayValueAt(overrides map[string]any, path string) (any, bool) {
	if overrides == nil {
		return nil, false
	}
	return component.GetValueByPath(overrides, path)
}

// isEmptyOverlayValue reports whether v is the recipe author's deliberate
// "no value here" sentinel — an empty slice/map, an explicit nil, or any
// representation that yields zero entries. Used to detect opt-out semantics
// (kind.yaml's `daemonsets.tolerations: []`).
func isEmptyOverlayValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	case map[any]any:
		return len(x) == 0
	default:
		return false
	}
}

// filterPaths returns paths not present in skip.
func filterPaths(paths []string, skip map[string]struct{}) []string {
	if len(paths) == 0 || len(skip) == 0 {
		return paths
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if _, blocked := skip[p]; !blocked {
			out = append(out, p)
		}
	}
	return out
}

// splitPaths partitions paths into (append-mode, replace-mode) based on
// policy.appendMode. Opt-out paths must already be filtered out by the caller.
func splitPaths(paths []string, appendMode map[string]struct{}) (appendPaths, replacePaths []string) {
	for _, p := range paths {
		if _, ok := appendMode[p]; ok {
			appendPaths = append(appendPaths, p)
		} else {
			replacePaths = append(replacePaths, p)
		}
	}
	return
}

// applyNodeSchedulingOverrides applies node selectors and tolerations to component values.
// Uses the component registry to determine the correct paths for each component.
// The provider argument scopes the registry lookup to the recipe's bound DataProvider;
// a nil provider falls back to the package-global registry via GetComponentRegistryFor.
//
// The policy argument carries the per-path opt-out / append classification
// computed once at the top of extractComponentValues. opt-out paths are
// skipped entirely; append-mode paths receive CLI tolerations appended to
// whatever the overlay already wrote (so bcm.yaml's BCM-master tolerations
// coexist with --system-node-toleration); other paths use REPLACE semantics
// so the documented system → accelerated overwrite for shared paths like
// NFD's worker.tolerations still produces "accelerated wins".
func (b *DefaultBundler) applyNodeSchedulingOverrides(componentName string, values map[string]any, provider recipe.DataProvider, policy schedulingPathPolicy) {
	if b.Config == nil {
		return
	}

	// Get component configuration from registry
	registry, err := recipe.GetComponentRegistryFor(provider)
	if err != nil {
		slog.Debug("failed to load component registry for node scheduling",
			"error", err,
			"component", componentName,
		)
		return
	}

	comp := registry.Get(componentName)
	if comp == nil {
		return // Unknown component, skip
	}

	// Apply system node selector. NodeSelector uses REPLACE semantics even
	// for overlay-set non-empty values — no current overlay sets selector
	// paths, and the cuj1-training contract assumes CLI replaces.
	if nodeSelector := b.Config.SystemNodeSelector(); len(nodeSelector) > 0 {
		if paths := filterPaths(comp.GetSystemNodeSelectorPaths(), policy.optOut); len(paths) > 0 {
			component.ApplyNodeSelectorOverrides(values, nodeSelector, paths...)
		}
	}

	// Apply system tolerations — split into append-mode (overlay had a
	// non-empty list, e.g. bcm) and replace-mode (no overlay).
	if tolerations := b.Config.SystemNodeTolerations(); len(tolerations) > 0 {
		if paths := filterPaths(comp.GetSystemTolerationPaths(), policy.optOut); len(paths) > 0 {
			appendPaths, replacePaths := splitPaths(paths, policy.appendMode)
			if len(replacePaths) > 0 {
				component.ApplyTolerationsOverrides(values, tolerations, replacePaths...)
			}
			if len(appendPaths) > 0 {
				component.AppendTolerationsOverrides(values, tolerations, appendPaths...)
			}
		}
	}

	// Apply accelerated node selector
	if nodeSelector := b.Config.AcceleratedNodeSelector(); len(nodeSelector) > 0 {
		if paths := filterPaths(comp.GetAcceleratedNodeSelectorPaths(), policy.optOut); len(paths) > 0 {
			component.ApplyNodeSelectorOverrides(values, nodeSelector, paths...)
		}
	}

	// Apply accelerated tolerations
	if tolerations := b.Config.AcceleratedNodeTolerations(); len(tolerations) > 0 {
		if paths := filterPaths(comp.GetAcceleratedTolerationPaths(), policy.optOut); len(paths) > 0 {
			appendPaths, replacePaths := splitPaths(paths, policy.appendMode)
			if len(replacePaths) > 0 {
				component.ApplyTolerationsOverrides(values, tolerations, replacePaths...)
			}
			if len(appendPaths) > 0 {
				component.AppendTolerationsOverrides(values, tolerations, appendPaths...)
			}
		}
	}

	// Apply workload selector
	if workloadSelector := b.Config.WorkloadSelector(); len(workloadSelector) > 0 {
		if paths := filterPaths(comp.GetWorkloadSelectorPaths(), policy.optOut); len(paths) > 0 {
			component.ApplyNodeSelectorOverrides(values, workloadSelector, paths...)
		}
	}

	// Apply workload-gate taint (as string format for nodewright-operator)
	if taint := b.Config.WorkloadGateTaint(); taint != nil {
		if paths := comp.GetAcceleratedTaintStrPaths(); len(paths) > 0 {
			taintStr := taint.ToString()
			overrides := make(map[string]string, len(paths))
			for _, path := range paths {
				overrides[path] = taintStr
			}
			if err := component.ApplyMapOverrides(values, overrides); err != nil {
				slog.Warn("failed to apply workload-gate taint",
					"component", componentName,
					"error", err,
				)
			}
		}
	}

	// Apply estimated node count to paths in nodeScheduling.nodeCountPaths.
	// ApplyMapOverrides uses convertMapValue, so numeric strings become ints in the values map; Helm gets integer type.
	if n := b.Config.EstimatedNodeCount(); n > 0 {
		if paths := comp.GetNodeCountPaths(); len(paths) > 0 {
			valStr := strconv.Itoa(n)
			overrides := make(map[string]string, len(paths))
			for _, path := range paths {
				overrides[path] = valStr
			}
			if err := component.ApplyMapOverrides(values, overrides); err != nil {
				// Failure is logged only; consider surfacing in bundle output in a future iteration.
				slog.Warn("failed to apply estimated node count",
					"component", componentName,
					"error", err,
				)
			}
		}
	}

	// Apply storage class to all registry-declared storageClassPaths, but only when the path
	// was not explicitly set via a per-component --set override. Overlay/default values in the
	// values map must not block injection; only CLI --set inputs take precedence.
	if sc := b.Config.StorageClass(); sc != "" {
		if paths := comp.GetStorageClassPaths(); len(paths) > 0 {
			explicitOverrides := b.getValueOverridesForComponent(componentName, provider)
			overrides := make(map[string]string, len(paths))
			for _, path := range paths {
				if _, isExplicit := explicitOverrides[path]; !isExplicit {
					overrides[path] = sc
				}
			}
			if len(overrides) > 0 {
				if err := component.ApplyMapOverrides(values, overrides); err != nil {
					slog.Warn("failed to apply storage class",
						"component", componentName,
						"error", err,
					)
				}
			}
		}
	}
}

// warnMissingStorageClassForPVCs emits a bundle note when a rendered component creates
// a PVC but leaves storageClassName unset, causing Kubernetes to rely on the
// target cluster's default StorageClass.
// dynamicPathSetFor returns the set of value paths declared as dynamic for
// componentName. Dynamic paths are excluded from scheduling injection so that
// the path stays absent from values entirely rather than carrying a baked-in
// value, letting operators supply tolerations at install time without
// rebuilding the bundle. See #1371.
func (b *DefaultBundler) dynamicPathSetFor(componentName string, provider recipe.DataProvider) map[string]struct{} {
	if b.Config == nil || !b.Config.HasDynamicValues() {
		return nil
	}
	registry, err := recipe.GetComponentRegistryFor(provider)
	if err != nil {
		slog.Debug("dynamicPathSetFor: failed to load registry, dynamic opt-out disabled",
			"component", componentName,
			"error", err,
		)
		return nil
	}
	raw := b.Config.DynamicValues()
	pathSet := make(map[string]struct{})
	for key, paths := range raw {
		comp := registry.GetByOverrideKey(key)
		if comp == nil {
			slog.Warn("dynamicPathSetFor: unresolved --dynamic override key, toleration will be baked in",
				"key", key,
				"component", componentName,
			)
			continue
		}
		if comp.Name != componentName {
			continue
		}
		for _, p := range paths {
			pathSet[p] = struct{}{}
		}
	}
	if len(pathSet) == 0 {
		return nil
	}
	return pathSet
}
func (b *DefaultBundler) warnMissingStorageClassForPVCs(ctx context.Context, recipeResult *recipe.RecipeResult, componentValues map[string]map[string]any) error {
	if b.Config == nil {
		return nil
	}

	registry, err := recipe.GetComponentRegistryFor(recipeResult.DataProvider())
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"failed to load component registry for storage class warnings")
	}

	for _, ref := range recipeResult.ComponentRefs {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(errors.ErrCodeTimeout,
				"context cancelled during storage class warning evaluation", err)
		}

		comp := registry.Get(ref.Name)
		if comp == nil {
			continue
		}

		values := componentValues[ref.Name]
		if values == nil {
			continue
		}

		for _, path := range comp.GetStorageClassPaths() {
			if !storageClassPathHasPVCSpec(values, path) || hasConfiguredStorageClass(values, path) {
				continue
			}

			msg := fmt.Sprintf(
				"%s renders a PVC without storageClassName at %s; set --storage-class <name> or --set %s:%s=<name> to avoid relying on the cluster default StorageClass",
				ref.Name,
				path,
				ref.Name,
				path,
			)
			b.appendWarning(msg)
			slog.Warn("component PVC storageClassName is unset",
				"component", ref.Name,
				"path", path,
			)
		}
	}

	return nil
}

// agentgateway component name and the value path that renders into the
// inference-gateway Service's spec.loadBalancerSourceRanges.
const (
	agentgatewayComponentName    = "agentgateway"
	agentgatewaySourceRangesPath = "allowedSourceRanges"
)

// agentgatewayDefaultSourceRanges is the private-by-default scope applied to the
// inference-gateway LoadBalancer when the operator supplies no allowedSourceRanges
// (and does not override it via a recipe componentRef). These are the RFC1918
// private ranges: they keep the gateway reachable from inside the cluster/VPC and
// from privately-routed peers, while denying the public internet (which includes
// VPN egress that presents a public source IP). Operators open specific public
// clients with --set-json agentgateway:allowedSourceRanges='["<cidr>"]', or
// expose it publicly with ["0.0.0.0/0"]. This is deliberately generic (not a
// customer-specific network) to avoid firewalling every deployment to one site.
// To flip to a deny-all default instead, replace this with a single unreachable
// CIDR such as "255.255.255.255/32". See #1373.
var agentgatewayDefaultSourceRanges = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
}

// agentgatewayExposure classifies how the agentgateway.allowedSourceRanges
// value scopes the inference-gateway LoadBalancer.
type agentgatewayExposure int

const (
	// exposureScoped: a non-empty list of valid CIDRs, none of them an
	// any-source range. The LoadBalancer is locked to trusted networks.
	exposureScoped agentgatewayExposure = iota
	// exposureOpen: a non-empty list that includes an any-source CIDR
	// (0.0.0.0/0 or ::/0) — a deliberate, explicit public opt-in.
	exposureOpen
	// exposureUnset: the value is missing or an empty list. Kubernetes treats
	// an empty loadBalancerSourceRanges as allow-all, so this would render an
	// internet-facing gateway by default.
	exposureUnset
	// exposureInvalid: the value is not a list of valid CIDR strings (e.g. a
	// bare scalar from a mistaken --set, a non-string entry, or an
	// unparseable CIDR) and would render an invalid Service.
	exposureInvalid
)

// resolveAgentgatewayExposure enforces a private-by-default posture for the
// agentgateway inference-gateway LoadBalancer. Kubernetes treats an empty
// loadBalancerSourceRanges as allow-all (0.0.0.0/0), so when the operator
// supplies no allowedSourceRanges (and no recipe componentRef override),
// emitting nothing would silently expose the gateway to the whole internet.
// Instead this injects a private RFC1918 default (see
// agentgatewayDefaultSourceRanges) into the merged values so the deployed
// gateway denies the public internet while staying reachable from inside the
// cluster/VPC. Operators scope to specific public clients via
// --set-json agentgateway:allowedSourceRanges='["<cidr>"]'; an explicit
// any-source opt-in is still permitted but logged loudly; an invalid value is
// rejected. It mutates componentValues only in the unset case. See #1373.
func (b *DefaultBundler) resolveAgentgatewayExposure(componentValues map[string]map[string]any) error {
	values := componentValues[agentgatewayComponentName]
	if values == nil {
		return nil
	}

	state, detail := classifyAgentgatewaySourceRanges(values, agentgatewaySourceRangesPath)
	switch state {
	case exposureScoped:
		return nil

	case exposureOpen:
		// Deliberate, explicit public opt-in: allowed, but logged loudly and
		// surfaced as a bundle warning so it is never silent.
		msg := fmt.Sprintf(
			"%s: inference-gateway is explicitly opened to the entire internet "+
				"(%s includes an any-source CIDR such as 0.0.0.0/0). This is a deliberate opt-in; "+
				"scope it to trusted networks unless public exposure is intended.",
			agentgatewayComponentName, agentgatewaySourceRangesPath,
		)
		b.appendWarning(msg)
		slog.Warn("agentgateway inference-gateway is explicitly opened via an any-source CIDR (deliberate public opt-in)",
			"component", agentgatewayComponentName,
			"path", agentgatewaySourceRangesPath,
		)
		return nil

	case exposureInvalid:
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf(
			"%s: %s must be a list of CIDR strings (%s); set it via "+
				"--set-json %s:%s='[\"<cidr>\"]' — a bare --set value renders an invalid Service",
			agentgatewayComponentName, agentgatewaySourceRangesPath, detail,
			agentgatewayComponentName, agentgatewaySourceRangesPath,
		))

	case exposureUnset:
		// Private-by-default: inject RFC1918 ranges so the gateway is never
		// emitted open to 0.0.0.0/0 without an explicit operator choice.
		ranges := make([]any, len(agentgatewayDefaultSourceRanges))
		for i, r := range agentgatewayDefaultSourceRanges {
			ranges[i] = r
		}
		values[agentgatewaySourceRangesPath] = ranges
		b.appendWarning(fmt.Sprintf(
			"%s: %s was not set; defaulting the inference-gateway to private ranges (%s) so it is "+
				"not exposed to the public internet. To allow specific public clients (e.g. a corporate VPN), "+
				"set --set-json %s:%s='[\"<cidr>\"]'; to expose it publicly, use [\"0.0.0.0/0\"]. "+
				"See docs/user/component-catalog.md.",
			agentgatewayComponentName, agentgatewaySourceRangesPath,
			strings.Join(agentgatewayDefaultSourceRanges, ", "),
			agentgatewayComponentName, agentgatewaySourceRangesPath,
		))
		slog.Info("defaulting agentgateway inference-gateway to private source ranges (allowedSourceRanges unset)",
			"component", agentgatewayComponentName,
			"path", agentgatewaySourceRangesPath,
			"ranges", agentgatewayDefaultSourceRanges,
		)
		return nil

	default:
		return errors.New(errors.ErrCodeInternal, fmt.Sprintf(
			"unhandled agentgateway exposure state %d", state))
	}
}

// classifyAgentgatewaySourceRanges inspects the value at path and reports how it
// scopes the LoadBalancer. The detail string is populated only for
// exposureInvalid to explain why the value was rejected. See #1373.
func classifyAgentgatewaySourceRanges(values map[string]any, path string) (agentgatewayExposure, string) {
	v, ok := component.GetValueByPath(values, path)
	if !ok || v == nil {
		return exposureUnset, ""
	}

	var items []string
	switch list := v.(type) {
	case []any:
		items = make([]string, 0, len(list))
		for _, e := range list {
			s, ok := e.(string)
			if !ok {
				return exposureInvalid, fmt.Sprintf("entry %v is not a string", e)
			}
			items = append(items, s)
		}
	case []string:
		items = list
	default:
		return exposureInvalid, fmt.Sprintf("got %T, not a list", v)
	}

	if len(items) == 0 {
		return exposureUnset, ""
	}

	hasAnySource := false
	for _, r := range items {
		if !netutil.IsValidCIDR(r) {
			return exposureInvalid, fmt.Sprintf("%q is not a valid CIDR", r)
		}
		if netutil.IsAnySourceCIDR(r) {
			hasAnySource = true
		}
	}
	if hasAnySource {
		return exposureOpen, ""
	}
	return exposureScoped, ""
}

func storageClassPathHasPVCSpec(values map[string]any, path string) bool {
	parentPath, ok := storageClassPathParent(path)
	if !ok {
		return false
	}

	parent, ok := component.GetValueByPath(values, parentPath)
	if !ok || parent == nil {
		return false
	}
	_, ok = parent.(map[string]any)
	return ok
}

func storageClassPathParent(path string) (string, bool) {
	idx := strings.LastIndex(path, ".")
	if idx <= 0 {
		return "", false
	}
	return path[:idx], true
}

func hasConfiguredStorageClass(values map[string]any, path string) bool {
	value, ok := component.GetValueByPath(values, path)
	if !ok || value == nil {
		return false
	}

	if s, ok := value.(string); ok {
		return strings.TrimSpace(s) != ""
	}
	return true
}

func (b *DefaultBundler) appendWarning(warning string) {
	if !strings.HasPrefix(warning, "Warning: ") {
		warning = "Warning: " + warning
	}
	b.warnings = append(b.warnings, warning)
}

// runComponentValidations executes all component-specific validations registered in the registry.
// Collects warnings and errors based on validation severity.
func (b *DefaultBundler) runComponentValidations(ctx context.Context, recipeResult *recipe.RecipeResult) error {
	if b.Config == nil {
		return nil
	}

	// Get component registry — required to know which validations to run.
	// A registry-load failure produces an unvalidated bundle, which is the
	// opposite of what this tool promises; surface the failure to the user.
	registry, err := recipe.GetComponentRegistryFor(recipeResult.DataProvider())
	if err != nil {
		return errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"failed to load component registry for validations")
	}

	// Iterate through components in recipe
	for _, ref := range recipeResult.ComponentRefs {
		if err := ctx.Err(); err != nil {
			return errors.Wrap(errors.ErrCodeTimeout, "context cancelled during validation", err)
		}

		// Get component config from registry
		comp := registry.Get(ref.Name)
		if comp == nil {
			continue // Unknown component, skip
		}

		// Get validations for this component
		componentValidations := comp.GetValidations()
		if len(componentValidations) == 0 {
			continue // No validations configured
		}

		// Run validations
		warnings, validationErrors := validations.RunValidations(
			ctx,
			ref.Name,
			componentValidations,
			recipeResult,
			b.Config,
		)

		// Collect warnings (prepend "Warning: " if not already present)
		for _, warning := range warnings {
			b.appendWarning(warning)
		}

		// Return first error (errors are blocking)
		if len(validationErrors) > 0 {
			return validationErrors[0]
		}
	}

	return nil
}

// copyDataFiles copies external data files from the --data directory into the bundle.
// Returns a list of relative paths to the copied files (e.g., "data/overrides.yaml").
// The provider argument supplies the LayeredDataProvider whose external directory is
// the source of truth; a nil provider falls back to the package-global provider so
// pre-WithDataProvider callers (legacy CLI path) still emit the same bundles.
func (b *DefaultBundler) copyDataFiles(dir string, provider recipe.DataProvider) ([]string, error) {
	// Check if the bound provider is a LayeredDataProvider with external
	// files. A nil-interface receiver returns (nil, false) from the type
	// assertion without panicking — equivalent to no external data.
	layered, ok := provider.(*recipe.LayeredDataProvider)
	if !ok {
		return nil, nil // No external data
	}

	externalFiles := layered.ExternalFiles()
	if len(externalFiles) == 0 {
		return nil, nil
	}

	// Copy the entire external directory into bundle/data/ using os.CopyFS
	dataDir, joinErr := deployer.SafeJoin(dir, "data")
	if joinErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "unsafe data directory path", joinErr)
	}
	externalFS := os.DirFS(layered.ExternalDir())
	if err := os.CopyFS(dataDir, externalFS); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to copy external data files", err)
	}

	// Build the list of copied files (relative to bundle dir)
	copiedFiles := make([]string, 0, len(externalFiles))
	for _, relPath := range externalFiles {
		if _, pathErr := deployer.SafeJoin(dataDir, relPath); pathErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "unsafe external data path", pathErr)
		}
		copiedFiles = append(copiedFiles, filepath.Join("data", relPath))
	}

	slog.Info("external data files copied into bundle", "count", len(copiedFiles))
	return copiedFiles, nil
}

// attestBundle signs the bundle checksums and copies the binary attestation into the bundle.
// dataFiles is the list of external data file paths (relative to bundle dir) to include
// in resolvedDependencies. Returns the list of attestation files added, or nil if skipped.
func (b *DefaultBundler) attestBundle(ctx context.Context, dir string, dataFiles []string, recipeResult *recipe.RecipeResult) ([]string, error) {
	dir = filepath.Clean(dir)
	if b.Attester == nil || b.Config == nil || !b.Config.Attest() {
		return nil, nil
	}

	// Read checksums.txt and compute its digest
	checksumPath, joinErr := deployer.SafeJoin(dir, checksum.ChecksumFileName)
	if joinErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "unsafe checksum path", joinErr)
	}
	digest, err := attestation.ComputeFileDigest(checksumPath)
	if err != nil {
		// If checksums don't exist (IncludeChecksums=false), attestation is not possible
		slog.Debug("attestation not possible: checksums not available", "error", err)
		return nil, nil
	}

	// Build attestation subject with full SLSA metadata
	metadata := attestation.StatementMetadata{
		ToolVersion:   b.Config.Version(),
		OutputDir:     dir,
		Deterministic: b.Config.Deterministic(),
	}

	if recipeResult != nil {
		if recipeResult.Criteria != nil {
			metadata.Recipe = recipeResult.Criteria.String()
		}
		components := make([]string, 0, len(recipeResult.ComponentRefs))
		for _, ref := range recipeResult.ComponentRefs {
			components = append(components, ref.Name)
		}
		metadata.Components = components
	}

	if len(dataFiles) > 0 {
		metadata.RecipeSource = "external"
	} else {
		metadata.RecipeSource = "embedded"
	}

	subject := attestation.AttestSubject{
		Name:     checksum.ChecksumFileName,
		Digest:   map[string]string{digestAlgoSHA256: digest},
		Metadata: metadata,
	}

	// Find and add binary attestation as a resolved dependency
	binaryPath, err := os.Executable()
	if err == nil {
		binaryDigest, digestErr := attestation.ComputeFileDigest(binaryPath)
		if digestErr == nil {
			subject.ResolvedDependencies = append(subject.ResolvedDependencies, attestation.Dependency{
				URI:    fmt.Sprintf("file://%s", binaryPath),
				Digest: map[string]string{digestAlgoSHA256: binaryDigest},
			})
		}
	}

	// Add data files as resolved dependencies
	for _, dataFile := range dataFiles {
		dataPath, pathErr := deployer.SafeJoin(dir, dataFile)
		if pathErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "unsafe data file path in attestation", pathErr)
		}
		dataDigest, digestErr := attestation.ComputeFileDigest(dataPath)
		if digestErr == nil {
			subject.ResolvedDependencies = append(subject.ResolvedDependencies, attestation.Dependency{
				URI:    fmt.Sprintf("file://%s", dataFile),
				Digest: map[string]string{digestAlgoSHA256: dataDigest},
			})
		}
	}

	// Sign
	bundleJSON, err := b.Attester.Attest(ctx, subject)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "bundle attestation failed", err)
	}

	// If attester returned nil (NoOp), nothing to write
	if bundleJSON == nil {
		return nil, nil
	}

	var attestFiles []string

	// Create attestation subdirectory
	attestDir, joinErr := deployer.SafeJoin(dir, attestation.AttestationDir)
	if joinErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "unsafe attestation directory path", joinErr)
	}
	if mkdirErr := os.MkdirAll(attestDir, 0755); mkdirErr != nil { //nolint:gosec // attestDir validated by SafeJoin
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create attestation directory", mkdirErr)
	}

	// Write bundle attestation
	bundleAttestPath, joinErr := deployer.SafeJoin(dir, attestation.BundleAttestationFile)
	if joinErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "unsafe bundle attestation path", joinErr)
	}
	if writeErr := os.WriteFile(bundleAttestPath, bundleJSON, 0600); writeErr != nil { //nolint:gosec // path validated by SafeJoin
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to write bundle attestation", writeErr)
	}
	attestFiles = append(attestFiles, attestation.BundleAttestationFile)
	slog.Info("bundle attestation written", "path", bundleAttestPath)

	// Copy binary attestation into bundle — errors are fatal since the user
	// opted into attestation (remove --attest to skip).
	if err := b.verifyAndCopyBinaryAttestation(ctx, dir); err != nil {
		return nil, err
	}
	attestFiles = append(attestFiles, attestation.BinaryAttestationFile)

	return attestFiles, nil
}

// verifyAndCopyBinaryAttestation resolves the running binary's attestation,
// cryptographically verifies it (REQ-6), and copies it into the bundle directory.
func (b *DefaultBundler) verifyAndCopyBinaryAttestation(ctx context.Context, dir string) error {
	binaryPath, execErr := os.Executable()
	if execErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"could not resolve executable path; remove --attest to skip", execErr)
	}

	binaryAttestPath, findErr := attestation.FindBinaryAttestation(binaryPath)
	if findErr != nil {
		return errors.Wrap(errors.ErrCodeNotFound,
			"binary attestation not found; reinstall from a release archive or remove --attest to skip", findErr)
	}

	// REQ-6: Cryptographically verify binary attestation before attesting bundles.
	// Confirms the binary was built by NVIDIA CI (identity-pinned) and the
	// attestation binds to this specific binary's content.
	binaryDigest, digestErr := checksum.SHA256Raw(binaryPath)
	if digestErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to compute binary digest for provenance verification", digestErr)
	}

	identityPattern := verifier.TrustedRepositoryPattern
	if b.Config.CertificateIdentityRegexp() != "" {
		identityPattern = b.Config.CertificateIdentityRegexp()
		if err := verifier.ValidateIdentityPattern(identityPattern); err != nil {
			return err
		}
		slog.Warn("using custom certificate identity pattern for binary attestation — "+
			"bundle will not pass verification with default settings",
			"pattern", identityPattern)
	}

	binaryBuilder, verifyErr := verifier.VerifyBinaryAttestation(ctx, binaryAttestPath, identityPattern, binaryDigest)
	if verifyErr != nil {
		return errors.Wrap(errors.ErrCodeUnauthorized,
			"binary attestation verification failed; only NVIDIA-built binaries can attest bundles — "+
				"remove --attest to skip", verifyErr)
	}
	slog.Info("binary provenance verified", "builder", binaryBuilder)

	binaryAttestData, readErr := readBoundedFile(binaryAttestPath, defaults.MaxAttestationFileBytes)
	if readErr != nil {
		return errors.Wrap(errors.ErrCodeInternal,
			"binary attestation exists but cannot be read: "+binaryAttestPath, readErr)
	}

	destPath, joinErr := deployer.SafeJoin(dir, attestation.BinaryAttestationFile)
	if joinErr != nil {
		return errors.Wrap(errors.ErrCodeInternal, "unsafe binary attestation path", joinErr)
	}
	if copyErr := os.WriteFile(destPath, binaryAttestData, 0600); copyErr != nil { //nolint:gosec // path validated by SafeJoin
		return errors.Wrap(errors.ErrCodeInternal,
			"failed to copy binary attestation into bundle", copyErr)
	}
	slog.Info("binary attestation copied into bundle", "path", destPath)

	return nil
}

// writeRecipeFile serializes the recipe to the bundle directory.
// Uses deterministic YAML marshaling so the bundle's recipe.yaml is
// byte-stable across runs — required because the file feeds checksums.txt
// which is in turn the subject of the bundle attestation.
func (b *DefaultBundler) writeRecipeFile(recipeResult *recipe.RecipeResult, dir string) (int64, error) {
	recipeData, err := serializer.MarshalYAMLDeterministic(recipeResult)
	if err != nil {
		return 0, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to serialize recipe")
	}

	recipePath, joinErr := deployer.SafeJoin(dir, recipeFileName)
	if joinErr != nil {
		return 0, errors.Wrap(errors.ErrCodeInternal, "unsafe recipe file path", joinErr)
	}
	if err := os.WriteFile(recipePath, recipeData, 0600); err != nil { //nolint:gosec // path validated by SafeJoin
		return 0, errors.Wrap(errors.ErrCodeInternal, "failed to write recipe file", err)
	}

	slog.Debug("wrote recipe file", "path", recipePath)
	return int64(len(recipeData)), nil
}

// buildDynamicValuesMap re-keys the config's dynamic values from user override keys
// (e.g., "gpuoperator") to component names (e.g., "gpu-operator") using the registry.
// The provider argument scopes the registry lookup to the recipe's bound DataProvider;
// a nil provider falls back to the package-global registry via GetComponentRegistryFor.
func (b *DefaultBundler) buildDynamicValuesMap(provider recipe.DataProvider) (map[string][]string, error) {
	if !b.Config.HasDynamicValues() {
		return make(map[string][]string), nil
	}

	registry, err := recipe.GetComponentRegistryFor(provider)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to load component registry for dynamic resolution")
	}

	raw := b.Config.DynamicValues()
	result := make(map[string][]string, len(raw))
	for key, paths := range raw {
		comp := registry.GetByOverrideKey(key)
		if comp == nil {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("unknown component %q in dynamic declaration: not found in component registry", key))
		}
		result[comp.Name] = append(result[comp.Name], paths...)
	}

	return result, nil
}

// removeHyphens removes hyphens from a string.
func removeHyphens(s string) string {
	return strings.ReplaceAll(s, "-", "")
}

// missingManifestMessage formats remediation guidance for an fs.ErrNotExist manifest miss.
func missingManifestMessage(manifestPath, componentName string, hasExternalData bool) string {
	if hasExternalData {
		return fmt.Sprintf(
			"manifest %q (referenced by component %q) not found in embedded data or in the --data directory. "+
				"If the recipe was generated by a different AICR version, regenerate with `aicr recipe ...` and re-bundle. "+
				"If using --data, verify the manifest path exists in the external directory.",
			manifestPath, componentName)
	}
	return fmt.Sprintf(
		"manifest %q (referenced by component %q) not found in this binary's embedded data. "+
			"This usually means the recipe was generated by an older AICR version — regenerate with `aicr recipe ...` and re-bundle.",
		manifestPath, componentName)
}

// manifestPhase selects which slice of ComponentRef.{Pre,}ManifestFiles
// the collector reads. Pre- and post-phase share one collector body so
// any future change to manifest loading (auth, caching, validation,
// path normalization) lands in exactly one place.
//
// phasePostManifests is intentionally the iota zero value: a zero-value
// or unspecified manifestPhase falls through to the legacy
// post-manifests behavior, never the newer pre-manifests path.
type manifestPhase int

const (
	phasePostManifests manifestPhase = iota
	phasePreManifests
)

// collectComponentManifestsByPhase gathers manifest file contents from
// all components for the requested phase, keyed by component name then
// manifest path. The body is shared between phases via the manifestPhase
// switch; per-call-site behavior is identical except for which slice is
// read off each ComponentRef.
func (b *DefaultBundler) collectComponentManifestsByPhase(
	ctx context.Context,
	recipeResult *recipe.RecipeResult,
	phase manifestPhase,
) (map[string]map[string][]byte, error) {

	result := make(map[string]map[string][]byte)
	provider := recipeResult.DataProvider()

	for _, ref := range recipeResult.ComponentRefs {
		if err := ctx.Err(); err != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout,
				"context cancelled while collecting component manifests", err)
		}

		var paths []string
		switch phase {
		case phasePreManifests:
			paths = ref.PreManifestFiles
		case phasePostManifests:
			paths = ref.ManifestFiles
		default:
			return nil, errors.New(errors.ErrCodeInternal,
				fmt.Sprintf("unknown manifest phase %d", phase))
		}
		if len(paths) == 0 {
			continue
		}

		componentManifests := make(map[string][]byte, len(paths))
		for _, manifestPath := range paths {
			content, err := recipe.GetManifestContentWithContext(ctx, provider, manifestPath)
			if err != nil {
				if stderrors.Is(err, fs.ErrNotExist) {
					// Use the bound provider for the type assertion. A nil
					// interface returns (nil, false) without panicking, so
					// callers without a layered provider correctly report
					// "no external data" in the error message.
					_, hasExternalData := provider.(*recipe.LayeredDataProvider)
					return nil, errors.New(errors.ErrCodeInvalidRequest,
						missingManifestMessage(manifestPath, ref.Name, hasExternalData))
				}
				return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
					fmt.Sprintf("failed to load manifest %s for component %s",
						manifestPath, ref.Name))
			}
			componentManifests[manifestPath] = content
		}
		result[ref.Name] = componentManifests
	}

	return result, nil
}

// collectComponentManifests preserves the original entry point used by
// existing call sites — equivalent to the post-phase call.
func (b *DefaultBundler) collectComponentManifests(ctx context.Context, recipeResult *recipe.RecipeResult) (map[string]map[string][]byte, error) {
	return b.collectComponentManifestsByPhase(ctx, recipeResult, phasePostManifests)
}

// collectComponentPreManifests gathers the pre-phase manifests (those
// the bundler will emit BEFORE each component's primary chart). Wired
// into each deployer call site in buildDeployer alongside the
// post-phase collector. Also folds in any synthesized pre-manifests
// (e.g. GKE critical-priority ResourceQuotas — see issue #915) so
// every deployer benefits from the same fix without per-deployer
// branching.
func (b *DefaultBundler) collectComponentPreManifests(ctx context.Context, recipeResult *recipe.RecipeResult) (map[string]map[string][]byte, error) {
	pre, err := b.collectComponentManifestsByPhase(ctx, recipeResult, phasePreManifests)
	if err != nil {
		return nil, err
	}
	return b.injectGKECriticalPriorityQuotas(pre, recipeResult)
}

// gkeCriticalPriorityQuotaPodFloor is the smallest `pods` cap the
// synthesized ResourceQuota carries. The cap is an admission allowlist
// — not a real capacity gate — so the value is intentionally generous.
// The floor handles recipes that did not declare a node count (Nodes
// defaults to 0 in CriteriaSpec when --nodes is omitted on both recipe
// and bundle), so demos and small clusters do not need to specify it.
const gkeCriticalPriorityQuotaPodFloor = 32

// gkeCriticalPriorityQuotaPodsPerNode is the multiplier applied to the
// recipe's declared node count. gpu-operator alone runs ~8-10
// critical-priority DaemonSet pods per GPU node (driver, toolkit,
// device-plugin, GFD, DCGM, DCGM exporter, MIG manager, validator)
// plus the controller Deployment; 32× covers steady-state plus
// rolling-update churn (old + new pods during a chart upgrade) with a
// ~3× safety margin.
const gkeCriticalPriorityQuotaPodsPerNode = 32

// gkeCriticalPriorityQuotaName is the metadata.name of the synthesized
// ResourceQuota. Stable across runs so idempotent re-apply by the
// deployer (helmfile / argocd / flux) updates the existing object
// rather than creating duplicates.
const gkeCriticalPriorityQuotaName = "aicr-gke-critical-pods"

// gkeCriticalPriorityQuotaFilename is the manifest filename injected
// into the pre-manifests map. It is unique to this synthesized object
// and namespaced under a directory prefix so it cannot collide with a
// real PreManifestFiles path declared in a component overlay.
const gkeCriticalPriorityQuotaFilename = "aicr/synthesized/gke-critical-pods-quota.yaml"

// injectGKECriticalPriorityQuotas appends a synthesized ResourceQuota
// pre-manifest to every component whose ComponentConfig declares
// GKECriticalPriority=true, when the recipe targets GKE. GKE Standard
// ships a kube-system ResourceQuota scoped to the system-*-critical
// PriorityClasses; per the Kubernetes spec, once any cluster-wide
// quota scopes by PriorityClass for those values, pods that request a
// matching priority class can only be created in namespaces that have
// a matching quota. Without the synthesized quota, gpu-operator (and
// any other marked component) hits a 10-minute helmfile-apply timeout
// when its first pod is rejected by admission. See issue #915.
//
// Non-GKE recipes return the input map unchanged, so the additive
// nature of the fix is preserved across services.
func (b *DefaultBundler) injectGKECriticalPriorityQuotas(
	pre map[string]map[string][]byte,
	recipeResult *recipe.RecipeResult,
) (map[string]map[string][]byte, error) {

	if recipeResult == nil || recipeResult.Criteria == nil ||
		recipeResult.Criteria.Service != recipe.CriteriaServiceGKE {

		return pre, nil
	}

	registry, err := recipe.GetComponentRegistryFor(recipeResult.DataProvider())
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal,
			"failed to load component registry for GKE quota synthesis")
	}

	pods := computeGKECriticalPriorityQuotaPods(recipeResult.Criteria.Nodes)

	if pre == nil {
		pre = make(map[string]map[string][]byte)
	}

	for _, ref := range recipeResult.ComponentRefs {
		cfg := registry.Get(ref.Name)
		if cfg == nil || !cfg.GKECriticalPriority {
			continue
		}
		if ref.Namespace == "" {
			// Defensive — the recipe resolver fills Namespace from the
			// registry's defaultNamespace before bundling. An empty
			// namespace here would produce an invalid ResourceQuota,
			// so skip with a warning rather than emit broken YAML.
			slog.Warn("skipping GKE critical-priority quota: component has no namespace",
				"component", ref.Name)
			continue
		}
		manifest, err := renderGKECriticalPriorityQuota(ref.Namespace, pods)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to render GKE critical-priority quota for %s", ref.Name), err)
		}
		if pre[ref.Name] == nil {
			pre[ref.Name] = make(map[string][]byte)
		}
		pre[ref.Name][gkeCriticalPriorityQuotaFilename] = manifest
	}

	return pre, nil
}

// computeGKECriticalPriorityQuotaPods returns the `hard.pods` value for
// the synthesized ResourceQuota. nodeCount of 0 (the CriteriaSpec
// default when --nodes is omitted) falls through to the floor.
func computeGKECriticalPriorityQuotaPods(nodeCount int) int {
	if nodeCount <= 0 {
		return gkeCriticalPriorityQuotaPodFloor
	}
	pods := nodeCount * gkeCriticalPriorityQuotaPodsPerNode
	if pods < gkeCriticalPriorityQuotaPodFloor {
		return gkeCriticalPriorityQuotaPodFloor
	}
	return pods
}

// renderGKECriticalPriorityQuota returns the YAML for a ResourceQuota
// that admits pods with system-*-critical priority classes in the
// given namespace. Uses serializer.MarshalYAMLDeterministic so the
// bytes are stable across runs — the synthesized manifest is part of
// the bundle artifact (checksummed and optionally attested), and
// yaml.v3 walks randomized Go map order, which would otherwise
// produce a different SHA on every invocation.
func renderGKECriticalPriorityQuota(namespace string, pods int) ([]byte, error) {
	quota := map[string]any{
		"apiVersion": "v1",
		"kind":       "ResourceQuota",
		"metadata": map[string]any{
			"name":      gkeCriticalPriorityQuotaName,
			"namespace": namespace,
		},
		"spec": map[string]any{
			"hard": map[string]any{
				"pods": strconv.Itoa(pods),
			},
			"scopeSelector": map[string]any{
				"matchExpressions": []map[string]any{
					{
						"operator":  "In",
						"scopeName": "PriorityClass",
						"values": []string{
							"system-node-critical",
							"system-cluster-critical",
						},
					},
				},
			},
		},
	}
	return serializer.MarshalYAMLDeterministic(quota)
}

// draChartVersionAnnotation is the key written onto the
// nvidia-dra-driver-gpu controller and kubelet-plugin pod templates.
// Its value mirrors the resolved gpu-operator componentRef version
// so that any gpu-operator chart bump produces a rendered pod-template
// diff that forces helm upgrade (and every other deployer) to re-roll
// the DaemonSet — clearing the kubelet plugin's stale NVML handle that
// would otherwise pin to the pre-migration driver state.
const draChartVersionAnnotation = header.Domain + "/gpu-operator-chart-version"

// draComponentName / gpuOperatorComponentName are the registry-level
// component names this injection couples together. Both must be
// enabled in the filtered resolved recipe before the annotation is
// written; recipes that disable either remain untouched.
const (
	draComponentName         = "nvidia-dra-driver-gpu"
	gpuOperatorComponentName = "gpu-operator"
)

// injectDRAChartVersionAnnotation writes the resolved gpu-operator
// chart version into the nvidia-dra-driver-gpu controller and
// kubelet-plugin podAnnotations on the bundler's componentValues map.
// Replaces the prior hand-maintained value in
// recipes/components/nvidia-dra-driver-gpu/values.yaml — see #973.
//
// Why this exists:
//
// PR #965 mitigated the stale-NVML class of bug (gpu-operator chart
// bump → k8s-driver-manager reloads kernel modules async → DRA kubelet
// plugin's NVML handle goes stale → CDI spec generation fails) by
// hard-coding the gpu-operator chart version into the DRA pod-template
// annotation. The annotation works as long as it stays in lockstep
// with the chart pin, but the coupling depends on a maintainer
// remembering to bump both in the same PR. A future PR that bumps
// gpu-operator and forgets the annotation produces identical rendered
// DaemonSet manifests, so helm upgrade skips the re-roll and stale
// NVML returns silently. Bundler-derived injection removes the manual
// step entirely.
//
// Trigger gating: BOTH gpu-operator and nvidia-dra-driver-gpu must be
// enabled in the filtered recipe. The caller has already removed
// disabled components from recipeResult.ComponentRefs at this point,
// so iterating the filtered slice gives the right gating for free.
// Recipes that disable either component leave componentValues
// untouched.
//
// Injection point: called from DefaultBundler.Make AFTER
// extractComponentValues (so the values map is populated and user
// --set overrides have already been applied) and BEFORE buildDeployer
// (so every deployer — Helm, helmfile, Flux, Argo CD, argocd-helm —
// receives the same final map). Placing the call after --set means a
// user override of this specific annotation key is intentionally NOT
// honored; the annotation must always reflect the actual resolved
// gpu-operator chart version, or the rollout-trigger semantics break.
//
// Mutates componentValues in place; nested controller / kubeletPlugin
// / podAnnotations maps are created lazily so existing values under
// either path (priorityClassName, other annotations, etc.) are
// preserved.
func (b *DefaultBundler) injectDRAChartVersionAnnotation(
	componentValues map[string]map[string]any,
	recipeResult *recipe.RecipeResult,
) {

	if componentValues == nil || recipeResult == nil {
		return
	}

	var gpuOperatorEnabled, draEnabled bool
	var gpuOperatorVersion string
	for _, ref := range recipeResult.ComponentRefs {
		switch ref.Name {
		case gpuOperatorComponentName:
			gpuOperatorEnabled = true
			gpuOperatorVersion = ref.Version
		case draComponentName:
			draEnabled = true
		}
	}
	if !draEnabled || !gpuOperatorEnabled {
		// Either component disabled: nothing to mirror. Silent skip
		// matches the "no chart pin, no rollout trigger" semantic and
		// is exercised by the disabled-component unit tests.
		return
	}
	if gpuOperatorVersion == "" {
		// gpu-operator is enabled but the resolver produced an empty
		// Version string. This shouldn't happen in normal recipe
		// resolution; if it does the rollout-trigger semantics break
		// silently — the same drift class this helper exists to
		// eliminate. Warn so operators have a debugging signal, then
		// skip injection rather than write an empty annotation value
		// that itself would lock the DaemonSet to a meaningless pin.
		slog.Warn("gpu-operator enabled with empty Version, skipping DRA chart-version annotation injection",
			"component", gpuOperatorComponentName,
			"draComponent", draComponentName)
		return
	}

	draValues := componentValues[draComponentName]
	if draValues == nil {
		draValues = make(map[string]any)
		componentValues[draComponentName] = draValues
	}

	// Both pod templates carry the same annotation. The controller is
	// a Deployment (one replica per chart) and the kubeletPlugin is the
	// DaemonSet whose NVML handle is at risk; rolling both keeps the
	// two halves of the chart consistent across upgrades.
	//
	// The `, _` on the type assertions below is deliberate: values come
	// from `extractComponentValues`, which produces `map[string]any` via
	// yaml.v3 decoding, so a wrong-type value at `controller` /
	// `kubeletPlugin` / `podAnnotations` cannot happen in practice. If
	// it ever did (e.g., a hand-crafted unit test or a malformed
	// override that's also broken for the DRA chart itself), the helper
	// silently replaces the wrong-type value with a fresh map and lands
	// the annotation on top.
	for _, podPath := range []string{"controller", "kubeletPlugin"} {
		section, _ := draValues[podPath].(map[string]any)
		if section == nil {
			section = make(map[string]any)
			draValues[podPath] = section
		}
		annotations, _ := section["podAnnotations"].(map[string]any)
		if annotations == nil {
			annotations = make(map[string]any)
			section["podAnnotations"] = annotations
		}
		annotations[draChartVersionAnnotation] = gpuOperatorVersion
	}
}
