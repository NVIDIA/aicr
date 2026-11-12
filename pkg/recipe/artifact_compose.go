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
	"encoding/json"
	stderrors "errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/NVIDIA/aicr/pkg/artifact"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"gopkg.in/yaml.v3"
)

const (
	artifactSourceKey  = "source"
	artifactChartKey   = "chart"
	artifactVersionKey = "version"
)

// ComposeArtifacts activates only for an explicit or external default inventory.
// Without one, it returns the exact original provider without reading bindings.
func ComposeArtifacts(ctx context.Context, provider DataProvider, options artifact.Options) (DataProvider, error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.FileReadTimeout)
	defer cancel()
	active := options.Inventory != ""
	if p, ok := provider.(*LayeredDataProvider); ok && !active {
		_, err := os.Lstat(filepath.Join(p.externalDir, artifactInventoryPath))
		if err != nil && !stderrors.Is(err, fs.ErrNotExist) {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "locate artifact inventory", err)
		}
		active = err == nil
	}
	if !active {
		return provider, nil
	}
	embedded, external, err := captureArtifactLayers(ctx, provider)
	if err != nil {
		return nil, err
	}
	inventoryBytes := external[artifactInventoryPath]
	if options.Inventory != "" {
		inventoryBytes, err = readArtifactOverride(ctx, options.Inventory)
		if err != nil {
			return nil, err
		}
	}
	return composeCapturedArtifacts(ctx, embedded, external, inventoryBytes, options, artifactInventoryRelativePath(provider, options.Inventory))
}

func mergedArtifactBindings(embedded, external map[string][]byte) (*artifact.Bindings, error) {
	var layers []*artifact.Bindings
	for _, files := range []map[string][]byte{embedded, external} {
		if data, ok := files[artifactBindingsPath]; ok {
			binding, err := artifact.ParseBindings(data)
			if err != nil {
				return nil, err
			}
			layers = append(layers, binding)
		}
	}
	return artifact.Merge(layers...)
}

func composeCapturedArtifacts(ctx context.Context, embedded, external map[string][]byte, inventoryBytes []byte, options artifact.Options, inventoryPaths ...string) (*ArtifactProvider, error) {
	inventory, composeErr := artifact.ParseInventory(inventoryBytes)
	if composeErr != nil {
		return nil, composeErr
	}
	bindings, composeErr := mergedArtifactBindings(embedded, external)
	if composeErr != nil {
		return nil, composeErr
	}
	if err := artifact.Account(inventory, bindings); err != nil {
		return nil, err
	}
	origins := map[string]string{}
	files, composeErr := mergeCapturedCatalog(embedded, external, origins)
	if composeErr != nil {
		return nil, composeErr
	}
	p := &ArtifactProvider{origins: origins, files: files, embedded: embedded, external: external, inventoryBytes: bytes.Clone(inventoryBytes), inventory: inventory, bindings: bindings}
	p.bindingsBytes, composeErr = yaml.Marshal(bindings)
	if composeErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "encode retained merged bindings", composeErr)
	}
	var registry ComponentRegistry
	if err := yaml.Unmarshal(files[registryFileName], &registry); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "decode artifact base registry", err)
	}
	if err := validateComponentRegistryHeader(&registry, "artifact base registry"); err != nil {
		return nil, err
	}
	byName := artifactComponentsByName(&registry)
	if err := p.initializeProvenance(options, byName, inventoryPaths...); err != nil {
		return nil, err
	}
	oldCharts := map[string]HelmConfig{}
	changed := []string{}
	writes := map[string][]string{}
	for _, item := range inventory.Artifacts {
		if err := ctx.Err(); err != nil {
			return nil, errors.WrapCtxErr(err, errors.ErrCodeTimeout, "compose artifact catalog")
		}
		if ignored, ok := bindings.IgnoredArtifacts[item.Name]; ok {
			p.provenance.Ignored = append(p.provenance.Ignored, artifact.IgnoredIdentity{Artifact: item, Reason: ignored.Reason})
			continue
		}
		if binding, ok := bindings.Charts[item.Name]; ok {
			component := byName[binding.Component]
			if component == nil || component.GetType() != ComponentTypeHelm {
				return nil, artifact.Invalid("artifact-target-invalid", "chart target is not a Helm component: "+binding.Component)
			}
			old := component.Helm
			oldCharts[component.Name] = old
			repository, chart := path.Split(item.Reference)
			repository = strings.TrimSuffix(repository, "/")
			if old.DefaultRepository != repository || path.Base(old.DefaultChart) != chart || old.DefaultVersion != item.Version || old.DefaultDigest != item.ArchiveDigest || old.DefaultDigest == "" {
				changed = append(changed, component.Name)
			}
			component.Helm.DefaultRepository = repository
			component.Helm.DefaultChart = chart
			component.Helm.DefaultVersion = item.Version
			component.Helm.DefaultDigest = item.ArchiveDigest
			p.provenance.Applied = append(p.provenance.Applied, artifact.Applied{Artifact: item, Chart: &binding})
		}
		if binding, ok := bindings.Images[item.Name]; ok {
			for _, target := range binding.Targets {
				component := byName[target.Component]
				if component == nil || component.GetType() != ComponentTypeHelm {
					return nil, artifact.Invalid("artifact-target-invalid", "image target is not a Helm component: "+target.Component)
				}
				if target.Component == "gpu-operator" || target.Component == "network-operator" {
					return nil, artifact.Invalid("artifact-target-invalid", "operator image layout is unsupported: "+target.Component)
				}
				if target.Platform != nil && !target.Platform.Equal(item.Platform) {
					return nil, artifact.Invalid("artifact-target-invalid", "platform assertion differs for "+item.Name)
				}
				if err := p.applyImageTarget(item, target, writes); err != nil {
					return nil, err
				}
			}
			p.provenance.Applied = append(p.provenance.Applied, artifact.Applied{Artifact: item, Image: &binding})
		}
	}
	disableChangedArtifactCRDOwnership(&registry, changed)
	p.files[registryFileName], composeErr = yaml.Marshal(&registry)
	if composeErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "encode artifact registry", composeErr)
	}
	if err := p.rebindCatalogDefaults(ctx, oldCharts, byName); err != nil {
		return nil, err
	}
	slices.SortFunc(p.provenance.Applied, func(a, b artifact.Applied) int { return strings.Compare(a.Artifact.Name, b.Artifact.Name) })
	slices.SortFunc(p.provenance.Ignored, func(a, b artifact.IgnoredIdentity) int { return strings.Compare(a.Artifact.Name, b.Artifact.Name) })
	return p, nil
}

func artifactComponentsByName(registry *ComponentRegistry) map[string]*ComponentConfig {
	byName := map[string]*ComponentConfig{}
	for i := range registry.Components {
		byName[registry.Components[i].Name] = &registry.Components[i]
	}
	return byName
}

func disableChangedArtifactCRDOwnership(registry *ComponentRegistry, changed []string) {
	if len(changed) > 0 {
		owners := []string{}
		for i := range registry.Components {
			component := &registry.Components[i]
			if component.OwnsCRDs {
				owners = append(owners, component.Name)
			}
			component.OwnsCRDs = false
			component.OwnsCRDsExcludeSubcharts = nil
		}
		slog.Warn("artifact chart identities changed; disabling catalog CRD ownership", "changed", changed, "disabledOwners", owners)
	}
}

func (p *ArtifactProvider) initializeProvenance(options artifact.Options, byName map[string]*ComponentConfig, inventoryPaths ...string) error {
	var err error
	exceptions := append([]string{}, options.AllowCatalogPins...)
	slices.Sort(exceptions)
	for i, name := range exceptions {
		if byName[name] == nil || (i > 0 && exceptions[i-1] == name) {
			return artifact.Invalid("artifact-invalid", "unknown or duplicate allowCatalogPins component "+name)
		}
	}
	p.provenance = artifact.Provenance{InventoryDigest: digestArtifactBytes(p.inventoryBytes), AllowCatalogPins: exceptions, Verification: "metadata-only", Applied: []artifact.Applied{}, Ignored: []artifact.IgnoredIdentity{}}
	p.provenance.BindingsDigest, err = canonicalArtifactDigest(p.bindings)
	if err != nil {
		return err
	}
	excluded := p.excludedInputs(inventoryPaths...)
	p.bindingsAsData = !excluded[artifactBindingsPath]
	p.inventoryCapturePath = artifactInventoryPath
	if !excluded[artifactInventoryPath] && !bytes.Equal(p.external[artifactInventoryPath], p.inventoryBytes) {
		p.inventoryCapturePath = "artifacts/selected-inventory-" + strings.TrimPrefix(p.provenance.InventoryDigest, "sha256:") + ".yaml"
		if existing, exists := p.files[p.inventoryCapturePath]; exists && !bytes.Equal(existing, p.inventoryBytes) {
			return artifact.Invalid("artifact-invalid", "selected inventory capture path already exists: "+p.inventoryCapturePath)
		}
	}
	excluded = p.excludedInputs(append(append([]string{}, inventoryPaths...), p.inventoryCapturePath)...)
	for _, name := range inventoryPaths {
		if name != "" && name != artifactInventoryPath && excluded[name] {
			p.relocatedInventories = append(p.relocatedInventories, name)
		}
	}
	embeddedDigest, err := canonicalArtifactDigest(artifactFileList(p.embedded, excluded))
	if err != nil {
		return err
	}
	p.provenance.BaseCatalogDigest, err = canonicalArtifactDigest(map[string]any{"embedded": embeddedDigest, "external": artifactFileList(p.external, excluded)})
	if err != nil {
		return err
	}
	return nil
}

func (p *ArtifactProvider) excludedInputs(inventoryPaths ...string) map[string]bool {
	excluded := artifactExcludedFiles(p.files, inventoryPaths...)
	for _, item := range p.inventory.Artifacts {
		if binding, ok := p.bindings.Images[item.Name]; ok {
			for _, target := range binding.Targets {
				delete(excluded, target.ValuesFile)
			}
		}
	}
	return excluded
}

func artifactExcludedFiles(files map[string][]byte, inventoryPaths ...string) map[string]bool {
	excluded := map[string]bool{artifactBindingsPath: true, artifactInventoryPath: true}
	for _, name := range inventoryPaths {
		if name != "" {
			excluded[name] = true
		}
	}
	// A document used as catalog data must also participate in the base digest.
	var inspect func(any)
	inspect = func(value any) {
		switch v := value.(type) {
		case string:
			delete(excluded, v)
		case []any:
			for _, item := range v {
				inspect(item)
			}
		case map[string]any:
			for _, item := range v {
				inspect(item)
			}
		}
	}
	for name, data := range files {
		if name == artifactBindingsPath || name == artifactInventoryPath {
			continue
		}
		var value any
		if yaml.Unmarshal(data, &value) == nil {
			inspect(value)
		}
	}
	return excluded
}

func (p *ArtifactProvider) applyImageTarget(item artifact.Identity, target artifact.Target, writes map[string][]string) error {
	data, ok := p.files[target.ValuesFile]
	if !ok {
		return artifact.Invalid("artifact-target-invalid", "missing values file "+target.ValuesFile)
	}
	var values map[string]any
	if err := yaml.Unmarshal(data, &values); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "decode image target values", err)
	}
	updates := []struct{ pointer, value string }{
		{target.Write.Reference, strings.TrimPrefix(item.Reference, "oci://") + "@" + item.ManifestDigest},
		{target.Write.Repository, strings.TrimPrefix(item.Reference, "oci://")},
		{target.Write.Digest, item.ManifestDigest},
		{target.Write.ClearTag, ""},
	}
	for _, update := range updates {
		if update.pointer == "" {
			continue
		}
		parts, err := artifact.Pointer(update.pointer)
		if err != nil {
			return err
		}
		for _, scope := range []string{target.ValuesFile, "component:" + target.Component} {
			for _, previous := range writes[scope] {
				if previous == update.pointer || strings.HasPrefix(previous, update.pointer+"/") || strings.HasPrefix(update.pointer, previous+"/") {
					return artifact.Invalid("artifact-target-invalid", "overlapping image writes in "+scope)
				}
			}
		}
		writes[target.ValuesFile] = append(writes[target.ValuesFile], update.pointer)
		writes["component:"+target.Component] = append(writes["component:"+target.Component], update.pointer)
		parent := values
		for _, part := range parts[:len(parts)-1] {
			child, ok := parent[part].(map[string]any)
			if !ok {
				return artifact.Invalid("artifact-target-invalid", "image pointer parent must be an existing object: "+update.pointer)
			}
			parent = child
		}
		leaf := parts[len(parts)-1]
		if _, ok := parent[leaf].(string); !ok {
			return artifact.Invalid("artifact-target-invalid", "image pointer must name an existing string: "+update.pointer)
		}
		parent[leaf] = update.value
	}
	encoded, err := encodeComposedArtifactFile(target.ValuesFile, values)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "encode bound image values", err)
	}
	p.files[target.ValuesFile] = encoded
	return nil
}

func (p *ArtifactProvider) rebindCatalogDefaults(ctx context.Context, old map[string]HelmConfig, components map[string]*ComponentConfig) error {
	for name, data := range p.files {
		if err := ctx.Err(); err != nil {
			return errors.WrapCtxErr(err, errors.ErrCodeTimeout, "rebind catalog defaults")
		}
		var document map[string]any
		if err := yaml.Unmarshal(data, &document); err != nil {
			continue // The catalog loader reports malformed catalog documents.
		}
		kind, _ := document["kind"].(string)
		if kind != RecipeMetadataKind && kind != RecipeMixinKind {
			continue
		}
		spec, _ := document["spec"].(map[string]any)
		refs, _ := spec["componentRefs"].([]any)
		changed := false
		for _, value := range refs {
			ref, _ := value.(map[string]any)
			componentName, _ := ref["name"].(string)
			before, ok := old[componentName]
			if !ok {
				continue
			}
			after := components[componentName].Helm
			for _, field := range []struct{ key, old, new string }{{artifactSourceKey, before.DefaultRepository, after.DefaultRepository}, {artifactChartKey, before.DefaultChart, after.DefaultChart}, {artifactVersionKey, before.DefaultVersion, after.DefaultVersion}} {
				actual, ok := ref[field.key].(string)
				if !ok || actual == "" {
					continue
				}
				equal := actual == field.old
				if field.key == artifactChartKey {
					equal = path.Base(actual) == path.Base(field.old)
				}
				if equal {
					ref[field.key] = field.new
					changed = true
				}
			}
		}
		if changed {
			if documentErr := requireSingleArtifactCatalogDocument(data); documentErr != nil {
				return documentErr
			}
			encoded, err := encodeComposedArtifactFile(name, document)
			if err != nil {
				return errors.Wrap(errors.ErrCodeInternal, "encode rebound catalog defaults", err)
			}
			p.files[name] = encoded
		}
	}
	return nil
}

func validateStaticArtifacts(ctx context.Context, provider DataProvider, options artifact.Options) error {
	_, err := staticArtifactProvider(ctx, provider, options)
	return err
}

func staticArtifactProvider(ctx context.Context, provider DataProvider, options artifact.Options) (DataProvider, error) {
	embedded, external, captureErr := captureArtifactLayers(ctx, provider)
	if captureErr != nil {
		return nil, captureErr
	}
	if _, err := mergedArtifactBindings(embedded, external); err != nil {
		return nil, err
	}
	inventoryBytes, active := external[artifactInventoryPath]
	if options.Inventory != "" {
		inventoryBytes, captureErr = readArtifactOverride(ctx, options.Inventory)
		if captureErr != nil {
			return nil, captureErr
		}
		active = true
	}
	if !active {
		return provider, nil
	}
	return composeCapturedArtifacts(ctx, embedded, external, inventoryBytes, options, artifactInventoryRelativePath(provider, options.Inventory))
}

func artifactInventoryRelativePath(provider DataProvider, override string) string {
	layered, ok := provider.(*LayeredDataProvider)
	if !ok || override == "" {
		return ""
	}
	base, err := filepath.Abs(layered.externalDir)
	if err != nil {
		return ""
	}
	absolute, err := filepath.Abs(override)
	if err != nil {
		return ""
	}
	relative, err := filepath.Rel(base, absolute)
	if err != nil || !filepath.IsLocal(relative) {
		return ""
	}
	return filepath.ToSlash(relative)
}

func validateStaticArtifactInput(ctx context.Context, provider DataProvider, options artifact.Options, kind string, data []byte) error {
	embedded, external, captureErr := captureArtifactLayers(ctx, provider)
	if captureErr != nil {
		return captureErr
	}
	inventoryBytes, active := external[artifactInventoryPath]
	if options.Inventory != "" {
		inventoryBytes, captureErr = readArtifactOverride(ctx, options.Inventory)
		if captureErr != nil {
			return captureErr
		}
		active = true
	}
	if kind == artifact.InventoryKind {
		inventoryBytes = data
		active = true
	} else {
		external[artifactBindingsPath] = data
	}
	if _, err := mergedArtifactBindings(embedded, external); err != nil {
		return err
	}
	if !active {
		return nil
	}
	_, captureErr = composeCapturedArtifacts(ctx, embedded, external, inventoryBytes, options, artifactInventoryRelativePath(provider, options.Inventory))
	return captureErr
}

func encodeComposedArtifactFile(name string, value any) ([]byte, error) {
	if path.Ext(name) == ".json" {
		return json.Marshal(value)
	}
	return yaml.Marshal(value)
}

func requireSingleArtifactCatalogDocument(data []byte) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document any
	if err := decoder.Decode(&document); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "decode artifact catalog document", err)
	}
	if err := decoder.Decode(&document); !stderrors.Is(err, io.EOF) {
		return artifact.Invalid("artifact-invalid", "catalog rebinding requires one YAML document")
	}
	return nil
}
