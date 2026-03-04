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

package component

import (
	"context"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler/result"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/recipe"
)

// ComponentConfig defines the configuration for a bundler component.
// This struct captures all component-specific settings, allowing the generic
// MakeBundle function to handle the common bundling logic.
type ComponentConfig struct {
	// Name is the component identifier used in recipes (e.g., "gpu-operator").
	Name string

	// DisplayName is the human-readable name used in templates (e.g., "GPU Operator").
	DisplayName string

	// ValueOverrideKeys are alternative keys to check for value overrides.
	// The Name is always checked first, then these alternatives (e.g., ["gpuoperator"]).
	ValueOverrideKeys []string

	// SystemNodeSelectorPaths are Helm value paths for system component node selectors.
	// Example: ["operator.nodeSelector", "nfd.nodeSelector"]
	SystemNodeSelectorPaths []string

	// SystemTolerationPaths are Helm value paths for system component tolerations.
	// Example: ["operator.tolerations"]
	SystemTolerationPaths []string

	// AcceleratedNodeSelectorPaths are Helm value paths for GPU node selectors.
	// Example: ["daemonsets.nodeSelector"]
	AcceleratedNodeSelectorPaths []string

	// AcceleratedTolerationPaths are Helm value paths for GPU node tolerations.
	// Example: ["daemonsets.tolerations"]
	AcceleratedTolerationPaths []string

	// DefaultHelmRepository is the default Helm repository URL.
	DefaultHelmRepository string

	// DefaultHelmChart is the chart name (e.g., "nvidia/gpu-operator").
	DefaultHelmChart string

	// DefaultHelmChartVersion is the default chart version if not specified in recipe.
	DefaultHelmChartVersion string

	// TemplateGetter is the function that retrieves templates by name.
	// If nil, TemplateNames will be used with the embedded TemplatesFS.
	TemplateGetter TemplateFunc

	// TemplateNames lists the template files to embed (e.g., ["README.md"]).
	// Used with auto-generated template getter when TemplateGetter is nil.
	TemplateNames []string

	// CustomManifestFunc is an optional function to generate additional manifests.
	// It receives the values map, config map, and output directory.
	// It should return the list of generated file paths, or nil if no manifests were generated.
	CustomManifestFunc CustomManifestFunc

	// MetadataFunc creates component-specific metadata for templates.
	// If nil, the default BundleMetadata is used.
	MetadataFunc MetadataFunc

	// MetadataExtensions provides additional fields for BundleMetadata.
	// These are merged into the Extensions map of the generated metadata.
	// Use this instead of MetadataFunc for simple extensions.
	MetadataExtensions map[string]any
}

// CustomManifestFunc is a function type for generating custom manifests.
// It receives context, base bundler, values map, config map, and output directory.
// Returns slice of generated file paths (may be nil/empty if no manifests needed).
type CustomManifestFunc func(ctx context.Context, b *BaseBundler, values map[string]any, configMap map[string]string, dir string) ([]string, error)

// MetadataFunc is a function type for creating component-specific metadata.
type MetadataFunc func(configMap map[string]string) any

// BundleMetadata contains common metadata used for README and manifest template rendering.
// This is the default metadata structure used when MetadataFunc is not provided.
// The Extensions map allows component-specific fields without custom structs.
type BundleMetadata struct {
	// Common fields used by all components
	Namespace        string
	HelmRepository   string
	HelmChart        string
	HelmChartVersion string
	HelmReleaseName  string
	Version          string
	RecipeVersion    string

	// Extensions holds component-specific fields.
	// Templates can access these via {{ .Script.Extensions.FieldName }}
	Extensions map[string]any
}

// GenerateDefaultBundleMetadata creates default bundle metadata from config map.
func GenerateDefaultBundleMetadata(config map[string]string, name string, defaultHelmRepo string, defaultHelmChart string) *BundleMetadata {
	return &BundleMetadata{
		Namespace:        GetConfigValue(config, "namespace", name),
		HelmRepository:   GetConfigValue(config, "helm_repository", defaultHelmRepo),
		HelmChart:        defaultHelmChart,
		HelmChartVersion: GetConfigValue(config, "helm_chart_version", ""),
		HelmReleaseName:  name,
		Version:          GetBundlerVersion(config),
		RecipeVersion:    GetRecipeBundlerVersion(config),
		Extensions:       make(map[string]any),
	}
}

// GenerateBundleMetadataWithExtensions creates bundle metadata with custom extensions.
// This is used when components need additional fields beyond the standard ones.
func GenerateBundleMetadataWithExtensions(config map[string]string, cfg ComponentConfig) *BundleMetadata {
	meta := GenerateDefaultBundleMetadata(config, cfg.Name, cfg.DefaultHelmRepository, cfg.DefaultHelmChart)

	// Apply default chart version if specified in config
	if cfg.DefaultHelmChartVersion != "" && meta.HelmChartVersion == "" {
		meta.HelmChartVersion = cfg.DefaultHelmChartVersion
	}

	// Merge extensions from component config
	if cfg.MetadataExtensions != nil {
		for k, v := range cfg.MetadataExtensions {
			meta.Extensions[k] = v
		}
	}

	return meta
}

// enrichConfigFromRegistry enriches a ComponentConfig with values from the component registry.
// This allows bundlers to omit fields that are already defined in the registry.
func enrichConfigFromRegistry(cfg *ComponentConfig) {
	registry, err := recipe.GetComponentRegistry()
	if err != nil {
		slog.Debug("component registry not available, using bundler config as-is",
			"component", cfg.Name,
			"error", err,
		)
		return
	}

	comp := registry.Get(cfg.Name)
	if comp == nil {
		return // Component not in registry, use bundler config
	}

	// Fill in missing values from registry
	if cfg.DisplayName == "" {
		cfg.DisplayName = comp.DisplayName
	}
	if len(cfg.ValueOverrideKeys) == 0 {
		cfg.ValueOverrideKeys = comp.ValueOverrideKeys
	}
	if len(cfg.SystemNodeSelectorPaths) == 0 {
		cfg.SystemNodeSelectorPaths = comp.GetSystemNodeSelectorPaths()
	}
	if len(cfg.SystemTolerationPaths) == 0 {
		cfg.SystemTolerationPaths = comp.GetSystemTolerationPaths()
	}
	if len(cfg.AcceleratedNodeSelectorPaths) == 0 {
		cfg.AcceleratedNodeSelectorPaths = comp.GetAcceleratedNodeSelectorPaths()
	}
	if len(cfg.AcceleratedTolerationPaths) == 0 {
		cfg.AcceleratedTolerationPaths = comp.GetAcceleratedTolerationPaths()
	}
	if cfg.DefaultHelmRepository == "" {
		cfg.DefaultHelmRepository = comp.Helm.DefaultRepository
	}
	if cfg.DefaultHelmChart == "" {
		cfg.DefaultHelmChart = comp.Helm.DefaultChart
	}
	if cfg.DefaultHelmChartVersion == "" {
		cfg.DefaultHelmChartVersion = comp.Helm.DefaultVersion
	}
}

// MakeBundle generates a bundle using the generic bundling logic.
// This function handles the common steps: creating directories, applying overrides,
// writing values.yaml, generating README, generating checksums, and finalizing.
// Configuration is enriched from the component registry when values are not
// explicitly set in the ComponentConfig.
func MakeBundle(ctx context.Context, b *BaseBundler, input recipe.RecipeInput, outputDir string, cfg ComponentConfig) (*result.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "context cancelled", err)
	}

	start := time.Now()

	// Enrich config from registry (fills in missing values)
	enrichConfigFromRegistry(&cfg)

	slog.Debug("generating bundle",
		"component", cfg.Name,
		"output_dir", outputDir,
	)

	// Get component reference
	componentRef := input.GetComponentRef(cfg.Name)
	if componentRef == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			cfg.Name+" component not found in recipe")
	}

	// Get values from component reference
	values, err := input.GetValuesForComponent(cfg.Name)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal,
			"failed to get values for "+cfg.Name, err)
	}

	// Apply user value overrides from --set flags
	if overrides := getValueOverridesForComponent(b, cfg); len(overrides) > 0 {
		if applyErr := ApplyMapOverrides(values, overrides); applyErr != nil {
			slog.Warn("failed to apply some value overrides to values map", "error", applyErr)
		}
	}

	// Apply system node selectors
	if selectors := b.Config.SystemNodeSelector(); len(selectors) > 0 {
		ApplyNodeSelectorOverrides(values, selectors, cfg.SystemNodeSelectorPaths...)
	}

	// Apply system tolerations
	if tolerations := b.Config.SystemNodeTolerations(); len(tolerations) > 0 {
		ApplyTolerationsOverrides(values, tolerations, cfg.SystemTolerationPaths...)
	}

	// Apply accelerated node selectors
	if selectors := b.Config.AcceleratedNodeSelector(); len(selectors) > 0 {
		ApplyNodeSelectorOverrides(values, selectors, cfg.AcceleratedNodeSelectorPaths...)
	}

	// Apply accelerated tolerations
	if tolerations := b.Config.AcceleratedNodeTolerations(); len(tolerations) > 0 {
		ApplyTolerationsOverrides(values, tolerations, cfg.AcceleratedTolerationPaths...)
	}

	// Create bundle directory structure
	dirs, err := b.CreateBundleDir(outputDir, cfg.Name)
	if err != nil {
		return b.Result, errors.Wrap(errors.ErrCodeInternal,
			"failed to create bundle directory", err)
	}

	// Build config map with base settings for metadata extraction
	configMap := b.BuildConfigMapFromInput(input)
	configMap["namespace"] = cfg.Name
	configMap["helm_repository"] = componentRef.Source
	configMap["helm_chart_version"] = componentRef.Version

	// Add accelerator from criteria if available (for custom manifest generation)
	if criteria := input.GetCriteria(); criteria != nil {
		configMap["accelerator"] = string(criteria.Accelerator)
	}

	// Serialize values to YAML with header
	header := ValuesHeader{
		ComponentName:  cfg.DisplayName,
		BundlerVersion: configMap["bundler_version"],
		RecipeVersion:  configMap["recipe_version"],
	}
	valuesYAML, err := MarshalYAMLWithHeader(values, header)
	if err != nil {
		return b.Result, errors.Wrap(errors.ErrCodeInternal,
			"failed to serialize values to YAML", err)
	}

	// Write values.yaml
	valuesPath := filepath.Join(dirs.Root, "values.yaml")
	if err := b.WriteFile(valuesPath, valuesYAML, 0644); err != nil {
		return b.Result, errors.Wrap(errors.ErrCodeInternal,
			"failed to write values file", err)
	}

	// Generate custom manifests if the component has a CustomManifestFunc
	if cfg.CustomManifestFunc != nil {
		if _, err := cfg.CustomManifestFunc(ctx, b, values, configMap, dirs.Root); err != nil {
			return b.Result, err
		}
	}

	// Generate metadata for templates
	var metadata any
	if cfg.MetadataFunc != nil {
		metadata = cfg.MetadataFunc(configMap)
	} else {
		metadata = GenerateBundleMetadataWithExtensions(configMap, cfg)
	}

	// Create combined data for README (values map + metadata)
	readmeData := map[string]any{
		"Values": values,
		"Script": metadata, // "Script" key preserved for template compatibility
	}

	// Generate README (only if template exists)
	if b.Config.IncludeReadme() && cfg.TemplateGetter != nil {
		if _, hasReadme := cfg.TemplateGetter("README.md"); hasReadme {
			readmePath := filepath.Join(dirs.Root, "README.md")
			if err := b.GenerateFileFromTemplate(ctx, cfg.TemplateGetter, "README.md",
				readmePath, readmeData, 0644); err != nil {
				return b.Result, err
			}
		}
	}

	// Generate checksums file
	if b.Config.IncludeChecksums() {
		if err := b.GenerateChecksums(ctx, dirs.Root); err != nil {
			return b.Result, errors.Wrap(errors.ErrCodeInternal,
				"failed to generate checksums", err)
		}
	}

	// Finalize bundle generation
	b.Finalize(start)

	slog.Debug("bundle generated",
		"component", cfg.Name,
		"files", len(b.Result.Files),
		"size_bytes", b.Result.Size,
		"duration", b.Result.Duration.Round(time.Millisecond),
	)

	return b.Result, nil
}

// getValueOverridesForComponent retrieves value overrides for a component from config.
// It checks the component name first, then any alternative keys specified in the config.
func getValueOverridesForComponent(b *BaseBundler, cfg ComponentConfig) map[string]string {
	allOverrides := b.Config.ValueOverrides()
	if allOverrides == nil {
		return nil
	}

	// Check the component name first
	if overrides, ok := allOverrides[cfg.Name]; ok {
		return overrides
	}

	// Check alternative keys
	for _, key := range cfg.ValueOverrideKeys {
		if overrides, ok := allOverrides[key]; ok {
			return overrides
		}
	}

	return nil
}
