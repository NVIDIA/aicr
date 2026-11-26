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
	"context"
	stderrors "errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	aicrerrors "aicr.run/pkg/errors"
	"aicr.run/pkg/serializer"
	"gopkg.in/yaml.v3"
)

// NvidiaCatalogName is the declared name of the built-in catalog. The name is
// reserved: only the embedded catalog may declare it.
const NvidiaCatalogName = "nvidia"

// maxCatalogNameLength bounds a declared catalog name, the DNS-label limit.
const maxCatalogNameLength = 63

// catalogNamePattern is the grammar of metadata.name in registry.yaml: a
// lowercase DNS label. The names appear in errors today and in provenance
// later, so they are fixed now: one spelling per catalog (no "NVIDIA" next to
// "nvidia") and no "+", the separator of the composed registry's own name.
var catalogNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Node kinds the composition tracks. They name the documents the metadata
// store loads and appear in collision and dangling-edge errors.
const (
	nodeKindOverlay = "overlay"
	nodeKindMixin   = "mixin"
	nodeKindBase    = "base"
)

// CatalogInput is one catalog handed to NewComposedDataProvider.
type CatalogInput struct {
	// Provider serves the catalog's files. For a directory catalog this is a
	// LayeredDataProvider in external-only mode, which never reads the
	// embedded catalog.
	Provider DataProvider

	// Label says where the catalog came from (a directory path, "embedded").
	// It names the catalog in errors raised before its declared name is known.
	Label string

	// Embedded marks the built-in catalog, the only one allowed to declare
	// NvidiaCatalogName.
	Embedded bool
}

type composedCatalog struct {
	name     string
	label    string
	embedded bool
	provider DataProvider
	registry *ComponentRegistry
}

// ComposedDataProvider is the graph union of several named catalogs.
//
// Composition is by node identity, not by file path or load order: the
// catalogs are sorted by declared name, so the result is the same whatever
// order they are given in. Every collision fails closed at construction and
// names both catalogs:
//
//   - the same component name in two registry.yaml files
//   - the same validator name in two validators/catalog.yaml files
//   - the same overlay or mixin name in two catalogs
//   - the same file path in two catalogs
//   - a second base, or an overlay that extends a base no catalog provides
//
// Overlays and bases are recognized the way the metadata store loads them, by
// document, wherever the file sits. Only files a loader reads take part;
// registry.yaml and validators/catalog.yaml are served merged and every other
// catalog file from the one catalog that owns it. A ComposedDataProvider is
// immutable once built and, as a pointer, is a stable key for the per-provider
// caches (registryCache, the metadata store, the criteria registry).
//
// Source reports "embedded" for the built-in catalog and "external" for any
// other, the two values the rest of the package acts on (strict criteria,
// evidence). CatalogOf reports the declared catalog name.
type ComposedDataProvider struct {
	catalogs []composedCatalog // sorted by declared name
	owner    map[string]int    // file path -> index into catalogs
	// referenced holds the data-root paths the catalogs' registries name
	// (an assertFile, an upgrades file, a manifest). A loader reads them by
	// path whatever their extension or directory, so they take part like any
	// other catalog content.
	referenced map[string]bool

	registry      []byte
	validators    []byte
	hasValidators bool
	validatorsSrc string
	registrySrc   string
}

// NewComposedDataProvider composes the given catalogs. Each must serve a
// registry.yaml that declares metadata.name; the built-in catalog declares
// NvidiaCatalogName and no other catalog may.
//
// Everything is validated eagerly, on ctx, so a bad composition fails here
// rather than on first read, and nothing is cached against a canceled context.
func NewComposedDataProvider(ctx context.Context, inputs []CatalogInput) (*ComposedDataProvider, error) {
	if len(inputs) == 0 {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest, "a catalog composition needs at least one catalog")
	}

	catalogs := make([]composedCatalog, 0, len(inputs))
	for i, in := range inputs {
		label := in.Label
		if label == "" {
			label = fmt.Sprintf("#%d", i+1)
		}
		if in.Provider == nil {
			return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("catalog %s has no provider", label))
		}
		reg, err := readCatalogRegistry(ctx, in.Provider, label)
		if err != nil {
			return nil, err
		}
		name := reg.Metadata.Name
		switch {
		case name == "":
			return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("catalog %s declares no metadata.name in %s; every catalog passed to --with must declare one", label, registryFileName))
		case len(name) > maxCatalogNameLength || !catalogNamePattern.MatchString(name):
			return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("catalog %s declares the name %q in %s; a name must be a lowercase DNS label "+
					"(lowercase letters, digits and '-', starting and ending with a letter or digit, at most %d characters)",
					label, name, registryFileName, maxCatalogNameLength))
		case in.Embedded && name != NvidiaCatalogName:
			return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("the built-in catalog declares name %q, want %q", name, NvidiaCatalogName))
		case !in.Embedded && name == NvidiaCatalogName:
			return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("catalog %s declares the name %q, which is reserved for the built-in catalog", label, NvidiaCatalogName))
		}
		catalogs = append(catalogs, composedCatalog{
			name: name, label: label, embedded: in.Embedded, provider: in.Provider, registry: reg,
		})
	}

	// Sort by (name, label) so neither the contents nor the text of any error
	// depends on the order the catalogs were given in.
	sort.SliceStable(catalogs, func(i, j int) bool {
		if catalogs[i].name != catalogs[j].name {
			return catalogs[i].name < catalogs[j].name
		}
		return catalogs[i].label < catalogs[j].label
	})
	for i := 1; i < len(catalogs); i++ {
		if catalogs[i].name == catalogs[i-1].name {
			return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("catalogs %s and %s both declare the name %q; names must be unique within a composition",
					catalogs[i-1].label, catalogs[i].label, catalogs[i].name))
		}
	}

	p := &ComposedDataProvider{catalogs: catalogs, owner: make(map[string]int), referenced: make(map[string]bool)}
	p.collectReferencedFiles()

	validatorCatalogs, err := p.indexFiles(ctx)
	if err != nil {
		return nil, err
	}
	if err = p.checkNodeNames(ctx); err != nil {
		return nil, err
	}
	if err = p.mergeRegistries(); err != nil {
		return nil, err
	}
	if err = p.mergeValidatorCatalogs(ctx, validatorCatalogs); err != nil {
		return nil, err
	}

	names := p.Names()
	slog.Debug("composed catalogs", "catalogs", names, "files", len(p.owner))
	return p, nil
}

func readCatalogRegistry(ctx context.Context, dp DataProvider, label string) (*ComponentRegistry, error) {
	data, err := dp.ReadFile(ctx, registryFileName)
	if err != nil {
		return nil, aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to read %s from catalog %s", registryFileName, label))
	}
	var reg ComponentRegistry
	if unmarshalErr := yaml.Unmarshal(data, &reg); unmarshalErr != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("failed to parse %s from catalog %s", registryFileName, label), unmarshalErr)
	}
	if headerErr := validateComponentRegistryHeader(&reg, fmt.Sprintf("catalog %s %s", label, registryFileName)); headerErr != nil {
		return nil, headerErr
	}
	return &reg, nil
}

// isCatalogContent reports whether LoadMetadataStoreFor reads path by walking.
// It keeps every file under components/ and parses every other .yaml file.
// Files a registry names are read by path instead and are covered by
// carriesContent. A README.md or LICENSE that two catalogs both ship is neither,
// so it is never indexed, served or walked.
func isCatalogContent(path string) bool {
	return strings.Contains(path, "components/") || strings.HasSuffix(path, ".yaml")
}

// carriesContent reports whether path is catalog content: a file the metadata
// store walks to, or one a registry refers to by path. Only such files can
// shadow one another.
func (p *ComposedDataProvider) carriesContent(path string) bool {
	return isCatalogContent(path) || p.referenced[path]
}

// collectReferencedFiles records the files the catalogs' registries name:
// healthCheck.assertFile, upgrades.file and manifestFiles. They are read
// through the provider by path, and nothing requires them to end in .yaml or
// sit under components/ (an assertFile may be checks/x/assert.yml). The set is
// the union over all catalogs, so a path one catalog names and another ships
// is still a collision rather than a file that silently belongs to one of them.
func (p *ComposedDataProvider) collectReferencedFiles() {
	add := func(ref string) {
		if ref != "" {
			p.referenced[filepath.ToSlash(ref)] = true
		}
	}
	for i := range p.catalogs {
		for j := range p.catalogs[i].registry.Components {
			c := &p.catalogs[i].registry.Components[j]
			add(c.HealthCheck.AssertFile)
			add(c.Upgrades.File)
			for _, f := range c.ManifestFiles {
				add(f)
			}
		}
	}
}

// indexFiles records which catalog owns each catalog-content file and fails
// when two catalogs provide the same path. registry.yaml and
// validators/catalog.yaml are exempt: they are merged by node name instead.
// It returns the indexes of the catalogs that ship a validator catalog.
func (p *ComposedDataProvider) indexFiles(ctx context.Context) ([]int, error) {
	var validatorCatalogs []int
	for i := range p.catalogs {
		c := p.catalogs[i]
		walkErr := c.provider.WalkDir(ctx, "", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel := filepath.ToSlash(path)
			switch rel {
			case registryFileName:
				return nil
			case catalogFileName:
				validatorCatalogs = append(validatorCatalogs, i)
				return nil
			}
			if !p.carriesContent(rel) {
				return nil
			}
			if prev, dup := p.owner[rel]; dup {
				return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					fmt.Sprintf("catalog collision: file %q is provided by both catalog %q and catalog %q",
						rel, p.catalogs[prev].name, c.name))
			}
			p.owner[rel] = i
			return nil
		})
		if walkErr != nil {
			return nil, aicrerrors.PropagateOrWrap(walkErr, aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("failed to scan catalog %q", c.name))
		}
	}
	return validatorCatalogs, nil
}

// catalogDocKind says how LoadMetadataStoreFor treats a catalog file: the node
// kind it can define, or "" when the store loads it as something else. The
// rules and their order mirror the store's walk (metadata_store.go), because a
// composition has to agree with the store about which files are nodes. Notably
// an overlay is any RecipeMetadata document at any path, not only one under
// overlays/, and any base.yaml whose path contains "overlays/" is the root of
// resolution, so a document outside overlays/ can shadow a built-in overlay or
// replace the base just as one inside it can.
func catalogDocKind(path string) string {
	filename := filepath.Base(path)
	switch {
	case path == artifactBindingsPath || path == artifactInventoryPath:
		return ""
	case strings.Contains(path, "checks/"):
		return ""
	case strings.HasPrefix(path, "mixins/"):
		if strings.HasSuffix(filename, ".yaml") {
			return nodeKindMixin
		}
		return ""
	case strings.Contains(path, "components/"):
		return ""
	case !strings.HasSuffix(filename, ".yaml"):
		return ""
	case filename == "data-v1.yaml" || filename == registryFileName:
		return ""
	case filename == "base.yaml" && strings.Contains(path, "overlays/"):
		return nodeKindBase
	default:
		return nodeKindOverlay
	}
}

// nodeDoc is the part of an overlay, base or mixin document the composition
// reads: its identity and its edges.
type nodeDoc struct {
	Kind       string `yaml:"kind"`
	APIVersion string `yaml:"apiVersion"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Base   string   `yaml:"base"`
		Mixins []string `yaml:"mixins"`
	} `yaml:"spec"`
}

// isNodeOf reports whether the document is a node of the given kind: a
// RecipeMixin for a mixin, a supported RecipeMetadata for an overlay or base.
// A document that is neither is left for the metadata store to report with its
// path, so the composition never fails on something the store would skip.
func (d *nodeDoc) isNodeOf(kind, path string) bool {
	if kind == nodeKindMixin {
		return d.Kind == RecipeMixinKind
	}
	hdr := RecipeMetadataHeader{Kind: d.Kind, APIVersion: d.APIVersion}
	isMetadata, _, err := classifyRecipeMetadataCatalogHeader(&hdr, path)
	return err == nil && isMetadata
}

// checkNodeNames validates the overlay, base and mixin nodes of the
// composition:
//
//   - two catalogs may not define an overlay, or a mixin, under the same
//     metadata.name, wherever the files sit, because resolution looks nodes up
//     by name and one would silently shadow the other
//   - two catalogs may not both supply the base, because the store keeps one
//     and the later file would silently replace the other
//   - every spec.base and spec.mixins edge must reach a node some catalog
//     defines; a dangling edge names the missing node and the catalog and file
//     that referenced it, which is how a partner overlay that extends an
//     nvidia overlay fails when nvidia is left out
//   - an overlay that extends the base (spec.base empty or "base") needs a
//     base to exist, so a composition without nvidia fails here, naming the
//     overlay, rather than later with a bare "base.yaml not found"
func (p *ComposedDataProvider) checkNodeNames(ctx context.Context) error {
	type nodeKey struct{ kind, name string }
	type nodeOwner struct {
		catalog int
		path    string
	}
	type edge struct {
		catalog    int
		path       string
		targetKind string // nodeKindOverlay or nodeKindMixin
		target     string
	}

	paths := make([]string, 0, len(p.owner))
	for path := range p.owner {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	seen := make(map[nodeKey]nodeOwner)
	var (
		base     *nodeOwner
		rootRefs []nodeOwner
		edges    []edge
	)
	for _, path := range paths {
		kind := catalogDocKind(path)
		if kind == "" {
			continue
		}
		idx := p.owner[path]
		data, err := p.catalogs[idx].provider.ReadFile(ctx, path)
		if err != nil {
			return aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("failed to read %s from catalog %q", path, p.catalogs[idx].name))
		}
		var doc nodeDoc
		// A file that does not parse is reported by the metadata store with
		// its path; this check only looks at identities and edges.
		if yaml.Unmarshal(data, &doc) != nil || !doc.isNodeOf(kind, path) {
			continue
		}

		if kind == nodeKindBase {
			switch {
			case base == nil:
				base = &nodeOwner{catalog: idx, path: path}
			case base.catalog != idx:
				return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					fmt.Sprintf("catalog collision: the base recipe is provided by both catalog %q (%s) and catalog %q (%s); "+
						"a composition has exactly one base",
						p.catalogs[base.catalog].name, base.path, p.catalogs[idx].name, path))
			}
			continue
		}

		if doc.Metadata.Name == "" {
			continue
		}
		key := nodeKey{kind: kind, name: doc.Metadata.Name}
		if prev, dup := seen[key]; dup {
			if prev.catalog != idx {
				return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					fmt.Sprintf("catalog collision: %s %q is defined by both catalog %q (%s) and catalog %q (%s)",
						kind, doc.Metadata.Name, p.catalogs[prev.catalog].name, prev.path, p.catalogs[idx].name, path))
			}
		} else {
			seen[key] = nodeOwner{catalog: idx, path: path}
		}
		if kind != nodeKindOverlay {
			continue
		}
		if doc.Spec.Base == "" || doc.Spec.Base == baseRecipeName {
			rootRefs = append(rootRefs, nodeOwner{catalog: idx, path: path})
		} else {
			edges = append(edges, edge{catalog: idx, path: path, targetKind: nodeKindOverlay, target: doc.Spec.Base})
		}
		for _, mixin := range doc.Spec.Mixins {
			edges = append(edges, edge{catalog: idx, path: path, targetKind: nodeKindMixin, target: mixin})
		}
	}

	for _, e := range edges {
		if _, ok := seen[nodeKey{kind: e.targetKind, name: e.target}]; ok {
			continue
		}
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("catalog %q: %s references %s %q, which no composed catalog defines (composed: %s); "+
				"add the catalog that provides it with --with",
				p.catalogs[e.catalog].name, e.path, e.targetKind, e.target, strings.Join(p.Names(), ", ")))
	}
	if base == nil && len(rootRefs) > 0 {
		ref := rootRefs[0]
		return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("catalog %q: %s extends the base recipe, which no composed catalog provides (composed: %s); "+
				"add the catalog that supplies overlays/base.yaml, such as nvidia, with --with",
				p.catalogs[ref.catalog].name, ref.path, strings.Join(p.Names(), ", ")))
	}
	return nil
}

// mergeRegistries unions the components of every catalog and serializes the
// result as the composition's registry.yaml.
func (p *ComposedDataProvider) mergeRegistries() error {
	var components []ComponentConfig
	componentOwner := make(map[string]int)
	names := make([]string, 0, len(p.catalogs))
	for i := range p.catalogs {
		c := p.catalogs[i]
		names = append(names, c.name)
		for j := range c.registry.Components {
			comp := c.registry.Components[j]
			if prev, dup := componentOwner[comp.Name]; dup {
				return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					fmt.Sprintf("catalog collision: component %q is defined by both catalog %q and catalog %q",
						comp.Name, p.catalogs[prev].name, c.name))
			}
			componentOwner[comp.Name] = i
			components = append(components, comp)
		}
	}
	first := p.catalogs[0].registry
	data, err := serializer.MarshalYAMLDeterministic(&ComponentRegistry{
		APIVersion: first.APIVersion,
		Kind:       first.Kind,
		Metadata:   RegistryMetadata{Name: strings.Join(names, "+")},
		Components: components,
	})
	if err != nil {
		return aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeInternal, "failed to serialize composed "+registryFileName)
	}
	p.registry = data
	all := make([]int, len(p.catalogs))
	for i := range all {
		all[i] = i
	}
	p.registrySrc = p.sourceOf(all)
	return nil
}

// mergeValidatorCatalogs unions the validators of the catalogs that ship a
// validators/catalog.yaml. A composition where none does serves none.
func (p *ComposedDataProvider) mergeValidatorCatalogs(ctx context.Context, indexes []int) error {
	if len(indexes) == 0 {
		return nil
	}
	merged := &catalogForMerge{}
	validatorOwner := make(map[string]int)
	for n, idx := range indexes {
		c := p.catalogs[idx]
		data, err := c.provider.ReadFile(ctx, catalogFileName)
		if err != nil {
			return aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("failed to read %s from catalog %q", catalogFileName, c.name))
		}
		var cat catalogForMerge
		if unmarshalErr := yaml.Unmarshal(data, &cat); unmarshalErr != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("failed to parse %s from catalog %q", catalogFileName, c.name), unmarshalErr)
		}
		if n == 0 {
			merged.APIVersion, merged.Kind, merged.Metadata = cat.APIVersion, cat.Kind, cat.Metadata
		} else if cat.APIVersion != "" && cat.APIVersion != merged.APIVersion {
			slog.Warn("catalog has a different validator catalog API version",
				"catalog", c.name, "version", cat.APIVersion, "expected", merged.APIVersion)
		}
		for _, v := range cat.Validators {
			name, _ := v["name"].(string)
			if name != "" {
				if prev, dup := validatorOwner[name]; dup {
					return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
						fmt.Sprintf("catalog collision: validator %q is defined by both catalog %q and catalog %q",
							name, p.catalogs[prev].name, c.name))
				}
				validatorOwner[name] = idx
			}
			merged.Validators = append(merged.Validators, v)
		}
	}
	// The merged bytes feed the catalog digest, so the map-valued validators
	// are serialized with sorted keys.
	data, err := serializer.MarshalYAMLDeterministic(merged)
	if err != nil {
		return aicrerrors.PropagateOrWrap(err, aicrerrors.ErrCodeInternal, "failed to serialize composed "+catalogFileName)
	}
	p.validators = data
	p.hasValidators = true
	p.validatorsSrc = p.sourceOf(indexes)
	return nil
}

// sourceOf classifies a merged file by the catalogs that contribute to it.
func (p *ComposedDataProvider) sourceOf(indexes []int) string {
	var hasEmbedded, hasExternal bool
	for _, i := range indexes {
		if p.catalogs[i].embedded {
			hasEmbedded = true
		} else {
			hasExternal = true
		}
	}
	switch {
	case hasEmbedded && hasExternal:
		return sourceMerged
	case hasEmbedded:
		return sourceEmbedded
	default:
		return sourceExternal
	}
}

// ComposesExternalCatalogs reports whether dp is a composition, or a --data
// layer over one, that includes a catalog other than the built-in one. Files of
// such a catalog are not carried into a bundle's data/ directory, so a bundle
// built from it must not claim to use embedded recipe data only.
func ComposesExternalCatalogs(dp DataProvider) bool {
	switch p := dp.(type) {
	case *ComposedDataProvider:
		for i := range p.catalogs {
			if !p.catalogs[i].embedded {
				return true
			}
		}
	case *LayeredDataProvider:
		return ComposesExternalCatalogs(p.embedded)
	}
	return false
}

// Names returns the declared names of the composed catalogs, sorted.
func (p *ComposedDataProvider) Names() []string {
	names := make([]string, len(p.catalogs))
	for i := range p.catalogs {
		names[i] = p.catalogs[i].name
	}
	return names
}

// CatalogOf returns the declared name of the catalog that owns path, or the
// names of the contributing catalogs joined with " + " for a merged file. It
// returns "" for a path no catalog provides.
func (p *ComposedDataProvider) CatalogOf(path string) string {
	rel := filepath.ToSlash(path)
	switch rel {
	case registryFileName:
		return strings.Join(p.Names(), " + ")
	case catalogFileName:
		if !p.hasValidators {
			return ""
		}
		return strings.Join(p.Names(), " + ")
	}
	if i, ok := p.owner[rel]; ok {
		return p.catalogs[i].name
	}
	return ""
}

// ReadFile reads path from the catalog that owns it. registry.yaml and
// validators/catalog.yaml are the merged documents.
func (p *ComposedDataProvider) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeTimeout, fmt.Sprintf("context canceled before reading %q", path), err)
	}
	rel := filepath.ToSlash(path)
	switch rel {
	case registryFileName:
		return p.registry, nil
	case catalogFileName:
		if !p.hasValidators {
			return nil, &fs.PathError{Op: "open", Path: rel, Err: fs.ErrNotExist}
		}
		return p.validators, nil
	}
	i, ok := p.owner[rel]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: rel, Err: fs.ErrNotExist}
	}
	slog.Debug("reading file from composed provider", "path", rel, "catalog", p.catalogs[i].name)
	return p.catalogs[i].provider.ReadFile(ctx, rel)
}

// WalkDir walks the union of the catalogs. Each file is visited once and each
// directory is visited once, from the first catalog (by declared name) that
// has it. A callback that returns fs.SkipDir for a directory skips that
// directory in every catalog.
func (p *ComposedDataProvider) WalkDir(ctx context.Context, root string, fn fs.WalkDirFunc) error {
	if err := ctx.Err(); err != nil {
		return aicrerrors.Wrap(aicrerrors.ErrCodeTimeout, fmt.Sprintf("context canceled before walking %q", root), err)
	}
	seen := make(map[string]bool)
	var skipped []string
	// fs.SkipAll ends the whole walk, not just one catalog's. Each child walk
	// consumes the sentinel and returns nil, so it is recorded here and every
	// later callback and catalog is cut short.
	stopped := false
	for i := range p.catalogs {
		err := p.catalogs[i].provider.WalkDir(ctx, root, func(path string, d fs.DirEntry, err error) error {
			if stopped {
				return fs.SkipAll
			}
			if err != nil {
				fnErr := fn(path, d, err)
				if stderrors.Is(fnErr, fs.SkipAll) {
					stopped = true
				}
				return fnErr
			}
			if composedUnderSkipped(skipped, path) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if !d.IsDir() && !p.servesFile(path) {
				return nil
			}
			if seen[path] {
				return nil
			}
			seen[path] = true
			fnErr := fn(path, d, nil)
			switch {
			case stderrors.Is(fnErr, fs.SkipAll):
				stopped = true
			case d.IsDir() && stderrors.Is(fnErr, fs.SkipDir):
				skipped = append(skipped, path)
			}
			return fnErr
		})
		if err != nil {
			return err
		}
		if stopped {
			return nil
		}
	}
	return nil
}

// servesFile reports whether the composition serves path: the two merged
// documents and catalog content. Other files, such as a README.md, are
// neither read nor walked.
func (p *ComposedDataProvider) servesFile(path string) bool {
	rel := filepath.ToSlash(path)
	return rel == registryFileName || rel == catalogFileName || p.carriesContent(rel)
}

func composedUnderSkipped(skipped []string, path string) bool {
	for _, s := range skipped {
		if path == s || strings.HasPrefix(path, s+"/") {
			return true
		}
	}
	return false
}

// Source reports "embedded" for files the built-in catalog provides and
// "external" for files any other catalog provides, the two values the rest of
// the package acts on. A merged file reports the merged value. Use CatalogOf
// for the declared catalog name.
func (p *ComposedDataProvider) Source(path string) string {
	rel := filepath.ToSlash(path)
	switch rel {
	case registryFileName:
		return p.registrySrc
	case catalogFileName:
		if p.hasValidators {
			return p.validatorsSrc
		}
	}
	if i, ok := p.owner[rel]; ok && !p.catalogs[i].embedded {
		return sourceExternal
	}
	return sourceEmbedded
}

// Close closes every composed provider that holds resources, such as a
// materialized OCI catalog.
func (p *ComposedDataProvider) Close() error {
	var errs []error
	for i := range p.catalogs {
		if closer, ok := p.catalogs[i].provider.(io.Closer); ok {
			if err := closer.Close(); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return stderrors.Join(errs...)
}
