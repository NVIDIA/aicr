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

package recipe

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/chainsaw/validation"
	"github.com/NVIDIA/aicr/pkg/defaults"
	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"gopkg.in/yaml.v3"
)

// StaticDiagnostic describes a problem found in an authoring artifact.
type StaticDiagnostic struct {
	File     string `json:"file"`
	Field    string `json:"field"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// ValidateStatic validates an authoring file or, when path is empty, the
// effective catalog. dataDir layers external data over the embedded catalog.
// Validation does not contact a cluster.
func ValidateStatic(ctx context.Context, path, dataDir string) ([]StaticDiagnostic, error) {
	return validateStatic(ctx, path, dataDir, false)
}

// ValidateStaticDirectory validates a complete catalog without embedded fallback.
func ValidateStaticDirectory(ctx context.Context, dataDir string) ([]StaticDiagnostic, error) {
	if dataDir == "" {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "catalog directory is required")
	}
	return validateStatic(ctx, "", dataDir, true)
}

func validateStatic(ctx context.Context, path, dataDir string, externalOnly bool) (diagnostics []StaticDiagnostic, resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.StaticValidationTimeout)
	defer cancel()
	defer func() {
		if err := ctx.Err(); err != nil {
			diagnostics = nil
			resultErr = aicrerrors.WrapCtxErr(err, aicrerrors.ErrCodeTimeout, "static validation canceled")
		}
	}()
	dp := defaultEmbeddedProvider
	if dataDir != "" {
		layered, err := NewLayeredDataProvider(NewEmbeddedDataProvider(GetEmbeddedFS(), ""), LayeredProviderConfig{ExternalDir: dataDir, ExternalOnly: externalOnly})
		if err != nil {
			return nil, aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeInvalidRequest, "invalid catalog")
		}
		dp = layered
		defer EvictCachedStore(dp)
		defer EvictCachedRegistry(dp)
	}
	add := func(file, field string, err error) {
		diagnostics = append(diagnostics, staticDiagnostic(file, field, err))
	}
	var data []byte
	var hdr RecipeMetadataHeader
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "failed to open validation input", err)
		}
		data, err = io.ReadAll(io.LimitReader(f, defaults.MaxExternalDataFileBytes+1))
		_ = f.Close()
		if err != nil {
			return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to read validation input", err)
		}
		if int64(len(data)) > defaults.MaxExternalDataFileBytes {
			return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "validation input exceeds size limit")
		}
		if err := yaml.Unmarshal(data, &hdr); err != nil {
			add(path, "", err)
			return diagnostics, nil
		}
		if hdr.Kind == ComponentRegistryKind {
			var reg ComponentRegistry
			if err := decodeStaticYAML(data, &reg, false); err != nil {
				add(path, "", err)
				return diagnostics, nil
			}
			validateStaticRegistry(ctx, &reg, dp, path, dataDir == "", add)
			return diagnostics, nil
		}
		if hdr.Kind != RecipeMetadataKind && hdr.Kind != RecipeMixinKind {
			add(path, "kind", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("unsupported kind %q", hdr.Kind)))
			return diagnostics, nil
		}
	}
	if path == "" && dataDir != "" && !validateStaticExternalRegistry(dataDir, add) {
		return diagnostics, nil
	}
	registry, err := GetComponentRegistryFor(dp)
	if err != nil {
		add("registry.yaml", "components", err)
		return diagnostics, nil
	}
	if path == "" {
		validateStaticRegistry(ctx, registry, dp, "registry.yaml", false, add)
	}
	var sources map[string]string
	if path == "" {
		var walkErr error
		sources, walkErr = staticOverlaySources(ctx, dp, add)
		if walkErr != nil {
			add("overlays", "catalog", walkErr)
			return diagnostics, nil
		}
	}
	store, err := LoadMetadataStoreFor(ctx, dp)
	if err != nil {
		add(dataDir, "catalog", err)
		return diagnostics, nil
	}
	copyStore := *store
	copyStore.Overlays = maps.Clone(store.Overlays)
	copyStore.Mixins = maps.Clone(store.Mixins)
	store = &copyStore
	if path != "" {
		validateStaticAuthoringFile(ctx, path, data, hdr, store, registry, add)
		return diagnostics, nil
	}
	resultErr = validateStaticCatalog(ctx, store, registry, sources, add)
	return diagnostics, resultErr
}

func staticDiagnostic(file, field string, err error) StaticDiagnostic {
	for cause := err; cause != nil; cause = stderrors.Unwrap(cause) {
		var se *aicrerrors.StructuredError
		if stderrors.As(cause, &se) {
			if value, ok := se.Context["file"].(string); ok && value != "" {
				file = value
			}
			if value, ok := se.Context["location"].(string); ok && value != "" {
				field = value
			}
		}
	}
	return StaticDiagnostic{File: file, Field: field, Severity: "error", Message: err.Error()}
}

func validateStaticExternalRegistry(dataDir string, add func(string, string, error)) bool {
	raw, err := readExternalFile(dataDir, RegistryFileName, defaults.MaxExternalDataFileBytes, false)
	if err != nil {
		add(filepath.Join(dataDir, RegistryFileName), "components", err)
		return false
	}
	var external ComponentRegistry
	if err := yaml.Unmarshal(raw, &external); err != nil {
		add(filepath.Join(dataDir, RegistryFileName), "components", err)
		return false
	}
	seen := make(map[string]bool)
	for _, comp := range external.Components {
		if seen[comp.Name] {
			add(filepath.Join(dataDir, RegistryFileName), "components.name", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("duplicate component name %q", comp.Name)))
		}
		seen[comp.Name] = true
	}
	return true
}

func staticOverlaySources(ctx context.Context, dp DataProvider, add func(string, string, error)) (map[string]string, error) {
	sources := make(map[string]string)
	seen := make(map[string]string)
	walkErr := dp.WalkDir(ctx, "overlays", func(name string, entry fs.DirEntry, entryErr error) error {
		if entryErr != nil {
			return aicrerrors.PropagateOrWrap(entryErr, aicrerrors.ErrCodeInvalidRequest, "failed to walk overlay source")
		}
		if entry.IsDir() || !strings.HasSuffix(name, ".yaml") {
			return nil
		}
		content, readErr := dp.ReadFile(ctx, name)
		if readErr != nil {
			return aicrerrors.PropagateOrWrap(readErr, aicrerrors.ErrCodeInvalidRequest, "failed to read overlay source")
		}
		var h RecipeMetadataHeader
		if decodeErr := yaml.Unmarshal(content, &h); decodeErr != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "invalid overlay header", decodeErr)
		}
		if h.Kind != RecipeMetadataKind {
			return nil
		}
		var overlay RecipeMetadata
		if decodeErr := decodeStaticYAML(content, &overlay, true); decodeErr != nil {
			add(name, "spec", decodeErr)
		}
		if h.Metadata.Name == "" {
			return nil
		}
		if prior, ok := seen[h.Metadata.Name]; ok {
			add(name, "metadata.name", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("duplicate overlay name %q also declared in %s", h.Metadata.Name, prior)))
		}
		seen[h.Metadata.Name] = name
		sources[h.Metadata.Name] = name
		return nil
	})
	if walkErr != nil {
		return nil, aicrerrors.PropagateOrWrap(walkErr, aicrerrors.ErrCodeInvalidRequest, "failed to read overlay sources")
	}
	return sources, nil
}

func validateStaticAuthoringFile(ctx context.Context, path string, data []byte, hdr RecipeMetadataHeader, store *MetadataStore, registry *ComponentRegistry, add func(string, string, error)) {
	if hdr.Kind == RecipeMixinKind {
		var mix RecipeMixin
		if err := decodeStaticYAML(data, &mix, true); err != nil {
			add(path, "spec", err)
			return
		}
		if mix.Metadata.Name == "" {
			add(path, "metadata.name", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "metadata.name is required"))
		}
		if err := validateRecipeMixinCatalogHeader(mix.Kind, mix.APIVersion, path); err != nil {
			add(path, "apiVersion", err)
		}
		if err := validateConstraintPaths(mix.Spec.Constraints, path, locSpecConstraints); err != nil {
			add(path, locSpecConstraints, err)
		}
		validateStaticRefs(path, mix.Spec.ComponentRefs, registry, add)
		return
	}
	var overlay RecipeMetadata
	if err := decodeStaticYAML(data, &overlay, true); err != nil {
		add(path, "spec", err)
		return
	}
	if _, _, err := classifyRecipeMetadataCatalogHeader(&hdr, path); err != nil {
		add(path, "apiVersion", err)
	}
	if err := ValidateRecipeMetadataProfile(&overlay); err != nil {
		add(path, "spec.profile", err)
	}
	if err := validateSpecConstraintPaths(&overlay.Spec, path); err != nil {
		add(path, "spec.constraints", err)
	}
	if overlay.Metadata.Name == "" {
		add(path, "metadata.name", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "metadata.name is required"))
		return
	}
	store.Overlays[overlay.Metadata.Name] = &overlay
	validateStaticOverlay(ctx, store, &overlay, registry, path, add)
}

func validateStaticCatalog(ctx context.Context, store *MetadataStore, registry *ComponentRegistry, sources map[string]string, add func(string, string, error)) error {
	names := make([]string, 0, len(store.Overlays))
	for name := range store.Overlays {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return aicrerrors.WrapCtxErr(err, aicrerrors.ErrCodeTimeout, "static validation canceled")
		}
		source := sources[name]
		if source == "" {
			source = name
		}
		validateStaticOverlay(ctx, store, store.Overlays[name], registry, source, add)
	}
	mixinNames := make([]string, 0, len(store.Mixins))
	for name := range store.Mixins {
		mixinNames = append(mixinNames, name)
	}
	sort.Strings(mixinNames)
	for _, name := range mixinNames {
		validateStaticRefs("mixins/"+name+".yaml", store.Mixins[name].Spec.ComponentRefs, registry, add)
	}
	if store.Base != nil {
		validateStaticRefs("overlays/base.yaml", store.Base.Spec.ComponentRefs, registry, add)
	}
	return nil
}

func validateStaticRefs(file string, refs []ComponentRef, registry *ComponentRegistry, add func(string, string, error)) {
	if err := validateRefNames(refs); err != nil {
		add(file, "spec.componentRefs", err)
	}
	for _, ref := range refs {
		if registry.Get(ref.Name) == nil {
			add(file, "spec.componentRefs", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("unknown component %q", ref.Name)))
		}
	}
}

func validateStaticOverlay(ctx context.Context, store *MetadataStore, overlay *RecipeMetadata, registry *ComponentRegistry, file string, add func(string, string, error)) {
	validateStaticRefs(file, overlay.Spec.ComponentRefs, registry, add)
	if overlay.Spec.Profile != nil {
		values := make([]string, 0, len(overlay.Spec.Profile.Values))
		for name := range overlay.Spec.Profile.Values {
			values = append(values, name)
		}
		sort.Strings(values)
		for _, name := range values {
			for _, ref := range overlay.Spec.Profile.Values[name].ComponentRefs {
				if registry.Get(ref.Name) == nil {
					add(file, "spec.profile.values."+name+".componentRefs", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("unknown component %q", ref.Name)))
				}
			}
		}
	}

	if _, err := store.resolveInheritanceChain(overlay.Metadata.Name); err != nil {
		add(file, "spec.base", err)
		return
	}
	for _, name := range overlay.Spec.Mixins {
		if _, ok := store.Mixins[name]; !ok {
			add(file, "spec.mixins", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("mixin %q not found", name)))
		}
	}
	if overlay.Spec.Criteria != nil {
		criteria := *overlay.Spec.Criteria
		normalizedCriteria := criteria.FillUnsetWithAny()
		if overlay.Spec.Profile != nil {
			names := make([]string, 0, len(overlay.Spec.Profile.Values))
			for name := range overlay.Spec.Profile.Values {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				selection := overlay.Spec.Profile.Name + "=" + name
				if _, err := store.BuildRecipeResultWithProfile(ctx, normalizedCriteria, selection); err != nil {
					add(file, "spec.profile.values."+name, err)
				}
			}
		}
		result, err := store.BuildRecipeResult(ctx, normalizedCriteria)
		if err != nil {
			add(file, "spec.criteria", err)
		} else {
			found := false
			for _, name := range result.Metadata.AppliedOverlays {
				if name == overlay.Metadata.Name {
					found = true
					break
				}
			}
			if !found {
				add(file, "spec.criteria", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("overlay %q was not applied for its declared criteria", overlay.Metadata.Name)))
			}
		}
	}
}

func validateStaticRegistry(ctx context.Context, registry *ComponentRegistry, dp DataProvider, file string, local bool, add func(string, string, error)) {
	if err := validateComponentRegistryHeader(registry, file); err != nil {
		add(file, "apiVersion", err)
	}
	for _, err := range registry.Validate() {
		add(file, "components", err)
	}
	for _, comp := range registry.Components {
		if err := validateComponentClass(&comp); err != nil {
			add(file, "components.class", err)
		}
		if err := validateHelmDefaultDigest(&comp); err != nil {
			add(file, "components.helm.defaultDigest", err)
		}
		if comp.GetType() == ComponentTypeKustomize && len(comp.ManifestFiles) > 0 {
			add(file, "components.manifestFiles", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("component %q is Kustomize but declares manifestFiles", comp.Name)))
		}
		if comp.Name == ReservedDeployerKey || slices.Contains(comp.ValueOverrideKeys, ReservedDeployerKey) {
			add(file, "components.name", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("component %q uses reserved deployer name or override key", comp.Name)))
		}
		if comp.Helm.DefaultChart != "" && comp.Helm.DefaultVersion == "" {
			add(file, "components.helm.defaultVersion", aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, fmt.Sprintf("component %q requires a chart version", comp.Name)))
		}
		if comp.HealthCheck.AssertFile == "" {
			continue
		}
		var content []byte
		var err error
		if local {
			content, err = readExternalFile(filepath.Dir(file), comp.HealthCheck.AssertFile, defaults.MaxExternalDataFileBytes, false)
		}
		if !local || stderrors.Is(err, os.ErrNotExist) {
			content, err = dp.ReadFile(ctx, comp.HealthCheck.AssertFile)
		}
		if err != nil {
			add(file, "components.healthCheck.assertFile", err)
			continue
		}
		if validation.IsChainsawTest(string(content)) {
			if err := validation.ValidateTestReadOnly(comp.Name, string(content)); err != nil {
				add(file, "components.healthCheck.assertFile", err)
			}
		}
	}
}

func decodeStaticYAML(data []byte, target any, strict bool) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(strict)
	if err := decoder.Decode(target); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "invalid YAML", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !stderrors.Is(err, io.EOF) {
		if err != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "invalid trailing YAML", err)
		}
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "multiple YAML documents are not supported")
	}
	return nil
}
