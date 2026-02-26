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

package bundler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/argocd"
	"github.com/NVIDIA/aicr/pkg/bundler/deployer/helm"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	"github.com/NVIDIA/aicr/pkg/bundler/validations"
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
		Config: config.NewConfig(),
	}

	for _, opt := range opts {
		opt(db)
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
	if err := b.runComponentValidations(ctx, recipeResult); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"component validation failed", err)
	}

	// Route based on deployer
	deployer := b.Config.Deployer()
	if deployer == config.DeployerArgoCD {
		return b.makeArgoCD(ctx, recipeResult, componentValues, dir, start)
	}
	return b.makeHelmBundle(ctx, recipeResult, componentValues, dir, start)
}

// makeHelmBundle generates a Helm per-component bundle.
func (b *DefaultBundler) makeHelmBundle(ctx context.Context, recipeResult *recipe.RecipeResult, componentValues map[string]map[string]any, dir string, start time.Time) (*result.Output, error) {
	slog.Debug("generating helm bundle",
		"component_count", len(recipeResult.ComponentRefs),
		"output_dir", dir,
	)

	// Collect manifest contents from components (keyed by component name)
	componentManifests, err := b.collectComponentManifests(ctx, recipeResult)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to collect component manifests", err)
	}

	// Generate per-component bundle
	generator := helm.NewGenerator()
	generatorInput := &helm.GeneratorInput{
		RecipeResult:       recipeResult,
		ComponentValues:    componentValues,
		Version:            b.Config.Version(),
		IncludeChecksums:   b.Config.IncludeChecksums(),
		ComponentManifests: componentManifests,
	}

	output, err := generator.Generate(ctx, generatorInput, dir)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to generate helm bundle", err)
	}

	// Write recipe file
	recipeSize, err := b.writeRecipeFile(recipeResult, dir)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to write recipe file", err)
	}

	// Build result output
	resultOutput := &result.Output{
		Results:       make([]*result.Result, 0),
		Errors:        make([]result.BundleError, 0),
		TotalDuration: time.Since(start),
		TotalSize:     output.TotalSize + recipeSize,
		TotalFiles:    len(output.Files) + 1, // +1 for recipe.yaml
		OutputDir:     dir,
	}

	// Add a single result for the helm bundle
	helmResult := &result.Result{
		Type:     "helm-bundle",
		Success:  true,
		Files:    output.Files,
		Size:     output.TotalSize,
		Duration: output.Duration,
	}
	resultOutput.Results = append(resultOutput.Results, helmResult)

	// Populate deployment info from generator output
	var notes []string
	if len(b.warnings) > 0 {
		notes = append(notes, b.warnings...)
	}
	resultOutput.Deployment = &result.DeploymentInfo{
		Type:  "Helm per-component bundle",
		Steps: output.DeploymentSteps,
		Notes: notes,
	}

	slog.Debug("helm bundle generation complete",
		"files", len(output.Files),
		"size_bytes", output.TotalSize,
		"duration", output.Duration,
	)

	return resultOutput, nil
}

// makeArgoCD generates ArgoCD Application manifests.
func (b *DefaultBundler) makeArgoCD(ctx context.Context, recipeResult *recipe.RecipeResult, componentValues map[string]map[string]any, dir string, start time.Time) (*result.Output, error) {
	slog.Debug("generating argocd applications",
		"component_count", len(recipeResult.ComponentRefs),
		"output_dir", dir,
	)

	// Generate ArgoCD applications
	generator := argocd.NewGenerator()
	generatorInput := &argocd.GeneratorInput{
		RecipeResult:     recipeResult,
		ComponentValues:  componentValues,
		Version:          b.Config.Version(),
		RepoURL:          b.Config.RepoURL(),
		IncludeChecksums: b.Config.IncludeChecksums(),
	}

	output, err := generator.Generate(ctx, generatorInput, dir)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to generate argocd applications", err)
	}

	// Build result output
	resultOutput := &result.Output{
		Results:       make([]*result.Result, 0),
		Errors:        make([]result.BundleError, 0),
		TotalDuration: time.Since(start),
		TotalSize:     output.TotalSize,
		TotalFiles:    len(output.Files),
		OutputDir:     dir,
	}

	// Add a single result for the ArgoCD applications
	argocdResult := &result.Result{
		Type:     "argocd-applications",
		Success:  true,
		Files:    output.Files,
		Size:     output.TotalSize,
		Duration: output.Duration,
	}
	resultOutput.Results = append(resultOutput.Results, argocdResult)

	// Populate deployment info from generator output
	notes := make([]string, 0)
	if len(output.DeploymentNotes) > 0 {
		notes = append(notes, output.DeploymentNotes...)
	}
	if len(b.warnings) > 0 {
		notes = append(notes, b.warnings...)
	}
	resultOutput.Deployment = &result.DeploymentInfo{
		Type:  "ArgoCD applications",
		Steps: output.DeploymentSteps,
		Notes: notes,
	}

	slog.Debug("argocd applications generation complete",
		"files", len(output.Files),
		"size_bytes", output.TotalSize,
		"duration", output.Duration,
	)

	return resultOutput, nil
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

		// Apply user value overrides from --set flags
		if overrides := b.getValueOverridesForComponent(ref.Name); len(overrides) > 0 {
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
