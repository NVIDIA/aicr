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

package bundler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/argocd"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/argocdhelm"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/helm"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	"github.com/NVIDIA/aicr/pkg/bundler/types"
	"github.com/NVIDIA/aicr/pkg/bundler/validations"
	"github.com/NVIDIA/aicr/pkg/bundler/verifier"
	"github.com/NVIDIA/aicr/pkg/component"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

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
// generates ArgoCD Application manifests.
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
// For ArgoCD output:
//   - app-of-apps.yaml: Parent ArgoCD Application
//   - <component>/application.yaml: ArgoCD Application per component
//   - <component>/values.yaml: Values for each component
//   - README.md: Deployment instructions
//
// Returns a result.Output summarizing the generation results.
func (b *DefaultBundler) Make(ctx context.Context, input recipe.RecipeInput, dir string) (*result.Output, error) {
	start := time.Now()

	// Reset warnings so they dont accumulate between multiple bundle generations
	b.warnings = nil

	// Validate input
	if input == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "recipe input cannot be nil")
	}

	// Only support RecipeResult format (not legacy Recipe)
	recipeResult, ok := input.(*recipe.RecipeResult)
	if !ok {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"bundle generation requires RecipeResult format")
	}

	if len(recipeResult.ComponentRefs) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"recipe must contain at least one component reference")
	}

	// Filter out disabled components.
	// Check order: --set overrides take precedence over recipe overrides.
	// This allows users to enable/disable components at bundle time:
	//   --set awsebscsidriver:enabled=false  (disable)
	//   --set awsebscsidriver:enabled=true   (re-enable)
	enabledRefs := make([]recipe.ComponentRef, 0, len(recipeResult.ComponentRefs))
	enabledSet := make(map[string]struct{})
	for _, ref := range recipeResult.ComponentRefs {
		if setEnabled, ok := b.getSetEnabledOverride(ref.Name); ok {
			if !setEnabled {
				slog.Info("skipping component disabled via --set", "component", ref.Name)
				continue
			}
			// --set enabled=true overrides recipe-level disabled
		} else if !ref.IsEnabled() {
			slog.Info("skipping disabled component", "component", ref.Name)
			continue
		}
		enabledRefs = append(enabledRefs, ref)
		enabledSet[ref.Name] = struct{}{}
	}
	recipeResult.ComponentRefs = enabledRefs

	// Filter DeploymentOrder to match enabled components
	filteredOrder := make([]string, 0, len(recipeResult.DeploymentOrder))
	for _, name := range recipeResult.DeploymentOrder {
		if _, ok := enabledSet[name]; ok {
			filteredOrder = append(filteredOrder, name)
		}
	}
	recipeResult.DeploymentOrder = filteredOrder

	if len(enabledRefs) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"recipe has no enabled components after filtering")
	}

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

	// Extract values for each component from the recipe
	componentValues, err := b.extractComponentValues(ctx, recipeResult)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to extract component values", err)
	}

	// Run component-specific validations
	if validationErr := b.runComponentValidations(ctx, recipeResult); validationErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"component validation failed", validationErr)
	}

	// Copy external data files before deployer construction so the file list
	// is available for both the deployer (checksum tracking) and post-generation
	// attestation. This is a no-op when --data is not set.
	dataFiles, err := b.copyDataFiles(dir)
	if err != nil {
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
	dynamicValues, err := b.buildDynamicValuesMap()
	if err != nil {
		return nil, err
	}

	switch b.Config.Deployer() {
	case config.DeployerArgoCDHelm:
		if b.Config.Attest() {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				"--attest is not yet supported with --deployer argocd-helm")
		}
		if provider := recipe.GetDataProvider(); provider != nil {
			if layered, ok := provider.(*recipe.LayeredDataProvider); ok && len(layered.ExternalFiles()) > 0 {
				return nil, errors.New(errors.ErrCodeInvalidRequest,
					"--data is not yet supported with --deployer argocd-helm")
			}
		}

		slog.Debug("generating argocd helm chart app-of-apps",
			"component_count", len(recipeResult.ComponentRefs),
			"dynamic_components", len(dynamicValues),
		)

		return &argocdhelm.Generator{
			RecipeResult:     recipeResult,
			ComponentValues:  componentValues,
			Version:          b.Config.Version(),
			RepoURL:          b.Config.RepoURL(),
			TargetRevision:   b.Config.TargetRevision(),
			IncludeChecksums: b.Config.IncludeChecksums(),
			DynamicValues:    dynamicValues,
		}, nil

	case config.DeployerArgoCD:
		if b.Config.HasDynamicValues() {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				"--dynamic is not supported with --deployer argocd; use --deployer argocd-helm instead")
		}

		slog.Debug("generating argocd applications",
			"component_count", len(recipeResult.ComponentRefs),
		)

		return &argocd.Generator{
			RecipeResult:     recipeResult,
			ComponentValues:  componentValues,
			Version:          b.Config.Version(),
			RepoURL:          b.Config.RepoURL(),
			TargetRevision:   b.Config.TargetRevision(),
			IncludeChecksums: b.Config.IncludeChecksums(),
		}, nil

	case config.DeployerHelm:
		componentManifests, manifestErr := b.collectComponentManifests(ctx, recipeResult)
		if manifestErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal,
				"failed to collect component manifests", manifestErr)
		}

		slog.Debug("generating helm bundle",
			"component_count", len(recipeResult.ComponentRefs),
		)

		return &helm.Generator{
			RecipeResult:       recipeResult,
			ComponentValues:    componentValues,
			Version:            b.Config.Version(),
			IncludeChecksums:   b.Config.IncludeChecksums(),
			ComponentManifests: componentManifests,
			DataFiles:          dataFiles,
			DynamicValues:      dynamicValues,
		}, nil

	default:
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported deployer type: %s", b.Config.Deployer()))
	}
}

// runDeployer executes a deployer and builds the result output.
// dataFiles is the list of external data file paths already copied by Make().
func (b *DefaultBundler) runDeployer(ctx context.Context, d deployer.Deployer, recipeResult *recipe.RecipeResult, dir string, dataFiles []string, start time.Time) (*result.Output, error) {
	output, err := d.Generate(ctx, dir)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to generate bundle", err)
	}

	totalFiles := len(output.Files)
	totalSize := output.TotalSize

	// Write recipe file (helm-only, preserves original behavior)
	if b.Config.Deployer() == config.DeployerHelm {
		recipeSize, writeErr := b.writeRecipeFile(recipeResult, dir)
		if writeErr != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to write recipe file", writeErr)
		}
		totalFiles++
		totalSize += recipeSize
	}

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
		return "argocd-applications", "ArgoCD applications"
	case config.DeployerArgoCDHelm:
		return "argocd-helm-chart", "ArgoCD Helm chart app-of-apps"
	default:
		return types.BundleType(dt), string(dt)
	}
}

// extractComponentValues extracts and processes values for each component in the recipe.
// It loads base values from the recipe, applies user overrides, and applies node selectors.
func (b *DefaultBundler) extractComponentValues(ctx context.Context, recipeResult *recipe.RecipeResult) (map[string]map[string]any, error) {
	componentValues := make(map[string]map[string]any)

	for _, ref := range recipeResult.ComponentRefs {
		if err := ctx.Err(); err != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled during component value extraction", err)
		}

		// Get base values from recipe
		values, err := recipeResult.GetValuesForComponent(ref.Name)
		if err != nil {
			slog.Warn("failed to get values for component, using empty map",
				"component", ref.Name,
				"error", err,
			)
			values = make(map[string]any)
		}

		// Apply user value overrides from --set flags.
		// Strip "enabled" key — it controls component inclusion, not Helm chart values.
		if overrides := b.getValueOverridesForComponent(ref.Name); len(overrides) > 0 {
			if _, has := overrides["enabled"]; has {
				filtered := make(map[string]string, len(overrides)-1)
				for k, v := range overrides {
					if k == "enabled" {
						continue
					}
					filtered[k] = v
				}
				overrides = filtered
			}
			if applyErr := component.ApplyMapOverrides(values, overrides); applyErr != nil {
				slog.Warn("failed to apply some value overrides",
					"component", ref.Name,
					"error", applyErr,
				)
			}
		}

		// Apply node selectors, tolerations, workload selector, and taints based on component type
		b.applyNodeSchedulingOverrides(ref.Name, values)

		componentValues[ref.Name] = values
	}

	return componentValues, nil
}

// getValueOverridesForComponent returns value overrides for a specific component.
// Uses the component registry to match both exact names and alternative override keys.
func (b *DefaultBundler) getValueOverridesForComponent(componentName string) map[string]string {
	if b.Config == nil {
		return nil
	}

	allOverrides := b.Config.ValueOverrides()
	if allOverrides == nil {
		return nil
	}

	// Check exact name first
	if overrides, ok := allOverrides[componentName]; ok {
		return overrides
	}

	// Use component registry to find component by any override key
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		// Fall back to non-hyphenated check if registry fails
		nonHyphenated := removeHyphens(componentName)
		if nonHyphenated != componentName {
			if overrides, ok := allOverrides[nonHyphenated]; ok {
				return overrides
			}
		}
		return nil
	}

	// Get the component config to access its value override keys
	comp := registry.Get(componentName)
	if comp == nil {
		return nil
	}

	// Check each alternative override key
	for _, key := range comp.ValueOverrideKeys {
		if overrides, ok := allOverrides[key]; ok {
			return overrides
		}
	}

	return nil
}

// getSetEnabledOverride checks if --set overrides contain an "enabled" key
// for the given component. Returns (value, true) if found, (false, false) otherwise.
// This allows --set awsebscsidriver:enabled=false to disable a component at bundle time.
func (b *DefaultBundler) getSetEnabledOverride(componentName string) (bool, bool) {
	overrides := b.getValueOverridesForComponent(componentName)
	if overrides == nil {
		return false, false
	}
	val, ok := overrides["enabled"]
	if !ok {
		return false, false
	}
	parsed, parseErr := strconv.ParseBool(val)
	if parseErr != nil {
		slog.Warn("invalid --set enabled value, ignoring override",
			"component", componentName, "value", val, "error", parseErr)
		return false, false
	}
	return parsed, true
}

// applyNodeSchedulingOverrides applies node selectors and tolerations to component values.
// Uses the component registry to determine the correct paths for each component.
func (b *DefaultBundler) applyNodeSchedulingOverrides(componentName string, values map[string]any) {
	if b.Config == nil {
		return
	}

	// Get component configuration from registry
	registry, err := recipe.GetComponentRegistry()
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

	// Apply system node selector
	if nodeSelector := b.Config.SystemNodeSelector(); len(nodeSelector) > 0 {
		if paths := comp.GetSystemNodeSelectorPaths(); len(paths) > 0 {
			component.ApplyNodeSelectorOverrides(values, nodeSelector, paths...)
		}
	}

	// Apply system tolerations
	if tolerations := b.Config.SystemNodeTolerations(); len(tolerations) > 0 {
		if paths := comp.GetSystemTolerationPaths(); len(paths) > 0 {
			component.ApplyTolerationsOverrides(values, tolerations, paths...)
		}
	}

	// Apply accelerated node selector
	if nodeSelector := b.Config.AcceleratedNodeSelector(); len(nodeSelector) > 0 {
		if paths := comp.GetAcceleratedNodeSelectorPaths(); len(paths) > 0 {
			component.ApplyNodeSelectorOverrides(values, nodeSelector, paths...)
		}
	}

	// Apply accelerated tolerations
	if tolerations := b.Config.AcceleratedNodeTolerations(); len(tolerations) > 0 {
		if paths := comp.GetAcceleratedTolerationPaths(); len(paths) > 0 {
			component.ApplyTolerationsOverrides(values, tolerations, paths...)
		}
	}

	// Apply workload selector
	if workloadSelector := b.Config.WorkloadSelector(); len(workloadSelector) > 0 {
		if paths := comp.GetWorkloadSelectorPaths(); len(paths) > 0 {
			component.ApplyNodeSelectorOverrides(values, workloadSelector, paths...)
		}
	}

	// Apply workload-gate taint (as string format for skyhook-operator)
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
}

// runComponentValidations executes all component-specific validations registered in the registry.
// Collects warnings and errors based on validation severity.
func (b *DefaultBundler) runComponentValidations(ctx context.Context, recipeResult *recipe.RecipeResult) error {
	if b.Config == nil {
		return nil
	}

	// Get component registry
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		slog.Debug("failed to load component registry for validations",
			"error", err,
		)
		return nil // Non-fatal, continue without validations
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
			msg := warning
			if !strings.HasPrefix(warning, "Warning: ") {
				msg = "Warning: " + warning
			}
			b.warnings = append(b.warnings, msg)
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
func (b *DefaultBundler) copyDataFiles(dir string) ([]string, error) {
	provider := recipe.GetDataProvider()

	// Check if the provider is a LayeredDataProvider with external files
	layered, ok := provider.(*recipe.LayeredDataProvider)
	if !ok {
		return nil, nil // No external data
	}

	externalFiles := layered.ExternalFiles()
	if len(externalFiles) == 0 {
		return nil, nil
	}

	// Copy the entire external directory into bundle/data/ using os.CopyFS
	dataDir := filepath.Join(dir, "data")
	externalFS := os.DirFS(layered.ExternalDir())
	if err := os.CopyFS(dataDir, externalFS); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to copy external data files", err)
	}

	// Build the list of copied files (relative to bundle dir)
	copiedFiles := make([]string, 0, len(externalFiles))
	for _, relPath := range externalFiles {
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
	digest, err := attestation.ComputeFileDigest(filepath.Join(dir, checksum.ChecksumFileName))
	if err != nil {
		// If checksums don't exist (IncludeChecksums=false), attestation is not possible
		slog.Debug("attestation not possible: checksums not available", "error", err)
		return nil, nil
	}

	// Build attestation subject with full SLSA metadata
	metadata := attestation.StatementMetadata{
		ToolVersion: b.Config.Version(),
		OutputDir:   dir,
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
		Digest:   map[string]string{"sha256": digest},
		Metadata: metadata,
	}

	// Find and add binary attestation as a resolved dependency
	binaryPath, err := os.Executable()
	if err == nil {
		binaryDigest, digestErr := attestation.ComputeFileDigest(binaryPath)
		if digestErr == nil {
			subject.ResolvedDependencies = append(subject.ResolvedDependencies, attestation.Dependency{
				URI:    fmt.Sprintf("file://%s", binaryPath),
				Digest: map[string]string{"sha256": binaryDigest},
			})
		}
	}

	// Add data files as resolved dependencies
	for _, dataFile := range dataFiles {
		dataPath := fmt.Sprintf("%s/%s", dir, dataFile)
		dataDigest, digestErr := attestation.ComputeFileDigest(dataPath)
		if digestErr == nil {
			subject.ResolvedDependencies = append(subject.ResolvedDependencies, attestation.Dependency{
				URI:    fmt.Sprintf("file://%s", dataFile),
				Digest: map[string]string{"sha256": dataDigest},
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
	attestDir := filepath.Join(dir, attestation.AttestationDir)
	if mkdirErr := os.MkdirAll(attestDir, 0755); mkdirErr != nil { //nolint:gosec // dir is filepath.Clean'd on entry; attestDir uses constant subpath
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create attestation directory", mkdirErr)
	}

	// Write bundle attestation
	bundleAttestPath := filepath.Join(dir, attestation.BundleAttestationFile)
	if writeErr := os.WriteFile(bundleAttestPath, bundleJSON, 0600); writeErr != nil { //nolint:gosec // path built from filepath.Clean'd dir + constant filename
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to write bundle attestation", writeErr)
	}
	attestFiles = append(attestFiles, attestation.BundleAttestationFile)
	slog.Info("bundle attestation written", "path", bundleAttestPath)

	// Copy binary attestation into bundle — errors are fatal since the user
	// opted into attestation (remove --attest to skip).
	binaryPath, execErr := os.Executable()
	if execErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"could not resolve executable path; remove --attest to skip", execErr)
	}

	binaryAttestPath, findErr := attestation.FindBinaryAttestation(binaryPath)
	if findErr != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound,
			"binary attestation not found; reinstall from a release archive or remove --attest to skip", findErr)
	}

	// REQ-6: Cryptographically verify binary attestation before attesting bundles.
	// Confirms the binary was built by NVIDIA CI (identity-pinned) and the
	// attestation binds to this specific binary's content.
	binaryDigest, digestErr := checksum.SHA256Raw(binaryPath)
	if digestErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to compute binary digest for provenance verification", digestErr)
	}

	identityPattern := verifier.TrustedRepositoryPattern
	if b.Config.CertificateIdentityRegexp() != "" {
		identityPattern = b.Config.CertificateIdentityRegexp()
		if err := verifier.ValidateIdentityPattern(identityPattern); err != nil {
			return nil, err
		}
		slog.Warn("using custom certificate identity pattern for binary attestation — "+
			"bundle will not pass verification with default settings",
			"pattern", identityPattern)
	}

	binaryBuilder, verifyErr := verifier.VerifyBinaryAttestation(ctx, binaryAttestPath, identityPattern, binaryDigest)
	if verifyErr != nil {
		return nil, errors.Wrap(errors.ErrCodeUnauthorized,
			"binary attestation verification failed; only NVIDIA-built binaries can attest bundles — "+
				"remove --attest to skip", verifyErr)
	}
	slog.Info("binary provenance verified", "builder", binaryBuilder)

	binaryAttestData, readErr := os.ReadFile(binaryAttestPath)
	if readErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"binary attestation exists but cannot be read: "+binaryAttestPath, readErr)
	}

	destPath := filepath.Join(dir, attestation.BinaryAttestationFile)
	if copyErr := os.WriteFile(destPath, binaryAttestData, 0600); copyErr != nil { //nolint:gosec // path built from filepath.Clean'd dir + constant filename
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to copy binary attestation into bundle", copyErr)
	}
	attestFiles = append(attestFiles, attestation.BinaryAttestationFile)
	slog.Info("binary attestation copied into bundle", "path", destPath)

	return attestFiles, nil
}

// writeRecipeFile serializes the recipe to the bundle directory.
func (b *DefaultBundler) writeRecipeFile(recipeResult *recipe.RecipeResult, dir string) (int64, error) {
	recipeData, err := yaml.Marshal(recipeResult)
	if err != nil {
		return 0, errors.Wrap(errors.ErrCodeInternal, "failed to serialize recipe", err)
	}

	recipePath := fmt.Sprintf("%s/recipe.yaml", dir)
	if err := os.WriteFile(recipePath, recipeData, 0600); err != nil {
		return 0, errors.Wrap(errors.ErrCodeInternal, "failed to write recipe file", err)
	}

	slog.Debug("wrote recipe file", "path", recipePath)
	return int64(len(recipeData)), nil
}

// buildDynamicValuesMap re-keys the config's dynamic values from user override keys
// (e.g., "gpuoperator") to component names (e.g., "gpu-operator") using the registry.
func (b *DefaultBundler) buildDynamicValuesMap() (map[string][]string, error) {
	if !b.Config.HasDynamicValues() {
		return make(map[string][]string), nil
	}

	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to load component registry for --dynamic resolution", err)
	}

	raw := b.Config.DynamicValues()
	result := make(map[string][]string, len(raw))
	for key, paths := range raw {
		comp := registry.GetByOverrideKey(key)
		if comp == nil {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("unknown component %q in --dynamic flag: not found in component registry", key))
		}
		result[comp.Name] = append(result[comp.Name], paths...)
	}

	return result, nil
}

// removeHyphens removes hyphens from a string.
func removeHyphens(s string) string {
	return strings.ReplaceAll(s, "-", "")
}

// collectComponentManifests gathers manifest file contents from all components,
// keyed by component name then manifest path.
func (b *DefaultBundler) collectComponentManifests(ctx context.Context, recipeResult *recipe.RecipeResult) (map[string]map[string][]byte, error) {
	result := make(map[string]map[string][]byte)

	for _, ref := range recipeResult.ComponentRefs {
		if err := ctx.Err(); err != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled while collecting component manifests", err)
		}

		if len(ref.ManifestFiles) == 0 {
			continue
		}

		componentManifests := make(map[string][]byte, len(ref.ManifestFiles))
		for _, manifestPath := range ref.ManifestFiles {
			content, err := recipe.GetManifestContent(manifestPath)
			if err != nil {
				return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to load manifest %s for component %s", manifestPath, ref.Name), err)
			}
			componentManifests[manifestPath] = content
		}
		result[ref.Name] = componentManifests
	}

	return result, nil
}
