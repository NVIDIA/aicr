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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/artifact"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"gopkg.in/yaml.v3"
)

const (
	artifactBindingsPath  = "artifacts/bindings.yaml"
	artifactInventoryPath = "artifacts/inventory.yaml"
)

// ArtifactProvider retains original input bytes independently of composed files.
// All reads return copies; no operation falls back to the captured directory.
type ArtifactProvider struct {
	files                map[string][]byte
	embedded             map[string][]byte
	external             map[string][]byte
	inventoryBytes       []byte
	inventory            *artifact.Inventory
	bindings             *artifact.Bindings
	bindingsBytes        []byte
	bindingsAsData       bool
	inventoryCapturePath string
	relocatedInventories []string
	provenance           artifact.Provenance
	origins              map[string]string
}

func (p *ArtifactProvider) ReadFile(ctx context.Context, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.WrapCtxErr(err, errors.ErrCodeTimeout, "read captured artifact catalog")
	}
	data, ok := p.files[path.Clean(filepath.ToSlash(name))]
	if !ok {
		return nil, &fs.PathError{Op: "read", Path: name, Err: fs.ErrNotExist}
	}
	return bytes.Clone(data), nil
}

func (p *ArtifactProvider) Source(name string) string {
	if source, ok := p.origins[path.Clean(filepath.ToSlash(name))]; ok {
		return source
	}
	return sourceEmbedded
}

func (p *ArtifactProvider) WalkDir(ctx context.Context, root string, fn fs.WalkDirFunc) error {
	entries := map[string]snapshotEntry{".": {name: ".", dir: true}}
	for name, data := range p.files {
		entries[name] = snapshotEntry{name: path.Base(name), size: int64(len(data))}
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			entries[dir] = snapshotEntry{name: path.Base(dir), dir: true}
		}
	}
	root = path.Clean(root)
	if _, ok := entries[root]; !ok {
		return fn(root, nil, &fs.PathError{Op: "walk", Path: root, Err: fs.ErrNotExist})
	}
	var walk func(string) error
	walk = func(name string) error {
		if err := ctx.Err(); err != nil {
			return errors.WrapCtxErr(err, errors.ErrCodeTimeout, "walk captured artifact catalog")
		}
		entry := entries[name]
		if err := fn(name, entry, nil); err != nil {
			return err
		}
		if !entry.dir {
			return nil
		}
		var children []string
		for child := range entries {
			if child != name && path.Dir(child) == name {
				children = append(children, child)
			}
		}
		slices.Sort(children)
		for _, child := range children {
			if err := walk(child); err != nil {
				if stderrors.Is(err, fs.SkipDir) {
					if entries[child].dir {
						continue
					}
					return nil
				}
				return err
			}
		}
		return nil
	}
	err := walk(root)
	if stderrors.Is(err, fs.SkipDir) || stderrors.Is(err, fs.SkipAll) {
		return nil
	}
	return err
}

type snapshotEntry struct {
	name string
	size int64
	dir  bool
}

func (e snapshotEntry) Name() string { return e.name }
func (e snapshotEntry) Size() int64  { return e.size }
func (e snapshotEntry) Mode() fs.FileMode {
	if e.dir {
		return fs.ModeDir | 0o500
	}
	return 0o400
}
func (e snapshotEntry) ModTime() time.Time         { return time.Time{} }
func (e snapshotEntry) IsDir() bool                { return e.dir }
func (e snapshotEntry) Sys() any                   { return nil }
func (e snapshotEntry) Type() fs.FileMode          { return e.Mode().Type() }
func (e snapshotEntry) Info() (fs.FileInfo, error) { return e, nil }

// ArtifactInputs returns independent copies of retained external inputs for bundling.
// The selected inventory uses a separate path when the default is also catalog data.
func (p *ArtifactProvider) ArtifactInputs() map[string][]byte {
	result := cloneArtifactFiles(p.external)
	for _, name := range p.relocatedInventories {
		delete(result, name)
	}
	result[p.inventoryCapturePath] = bytes.Clone(p.inventoryBytes)
	// Preserve external bytes, including when the document also serves as
	// catalog data. Embedded dispositions already belong to the pinned base.
	if _, external := result[artifactBindingsPath]; !external && !p.bindingsAsData {
		result[artifactBindingsPath] = bytes.Clone(p.bindingsBytes)
	}
	return result
}

func cloneArtifactFiles(files map[string][]byte) map[string][]byte {
	result := make(map[string][]byte, len(files))
	for name, data := range files {
		result[name] = bytes.Clone(data)
	}
	return result
}

func captureArtifactFiles(ctx context.Context, provider DataProvider) (map[string][]byte, error) {
	files := map[string][]byte{}
	total := 0
	err := provider.WalkDir(ctx, ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return errors.Wrap(errors.ErrCodeInvalidRequest, "capture catalog entry", walkErr)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return artifact.Invalid("artifact-invalid", "catalog snapshot requires regular files: "+name)
		}
		if len(files) >= defaults.ArtifactCatalogFiles {
			return artifact.Invalid("artifact-invalid", "catalog file count exceeds limit")
		}
		data, err := provider.ReadFile(ctx, name)
		if err != nil {
			return errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest, "capture catalog file")
		}
		total += len(data)
		if total > defaults.ArtifactCatalogBytes {
			return artifact.Invalid("artifact-invalid", "catalog capture exceeds byte limit")
		}
		files[name] = bytes.Clone(data)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func captureArtifactLayers(ctx context.Context, provider DataProvider) (map[string][]byte, map[string][]byte, error) {
	switch p := provider.(type) {
	case *EmbeddedDataProvider:
		embedded, err := captureArtifactFiles(ctx, p)
		return embedded, map[string][]byte{}, err
	case *LayeredDataProvider:
		embedded := map[string][]byte{}
		if !p.externalOnly {
			var err error
			embedded, err = captureArtifactFiles(ctx, p.embedded)
			if err != nil {
				return nil, nil, err
			}
		}
		if info, err := os.Stat(p.externalDir); err != nil || !info.IsDir() {
			return nil, nil, errors.Wrap(errors.ErrCodeInvalidRequest, "external catalog directory unavailable during capture", err)
		}
		// Capture a raw external-only view, never the live merged provider. Layering
		// subsequently operates exclusively on these retained bytes.
		externalView := &LayeredDataProvider{externalOnly: true, externalDir: p.externalDir, maxFileSize: p.maxFileSize}
		external, err := captureArtifactFiles(ctx, externalView)
		return embedded, external, err
	default:
		return nil, nil, artifact.Invalid("artifact-source-unsupported", "artifact composition requires an embedded or filesystem source")
	}
}

func mergeCapturedCatalog(embedded, external map[string][]byte, origins map[string]string) (map[string][]byte, error) {
	for name := range external {
		origins[name] = sourceExternal
	}
	files := maps.Clone(embedded)
	maps.Copy(files, external)
	if len(embedded) == 0 {
		return files, nil
	}
	if externalData, ok := external[registryFileName]; ok {
		var base, overlay ComponentRegistry
		if err := yaml.Unmarshal(embedded[registryFileName], &base); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "decode captured base registry", err)
		}
		if err := yaml.Unmarshal(externalData, &overlay); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "decode captured external registry", err)
		}
		if err := validateComponentRegistryHeader(&overlay, "external registry"); err != nil {
			return nil, err
		}
		data, err := yaml.Marshal(mergeRegistries(&base, &overlay))
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "encode captured registry", err)
		}
		files[registryFileName] = data
		origins[registryFileName] = sourceMerged
	}
	if externalData, ok := external[catalogFileName]; ok {
		var base, overlay catalogForMerge
		if err := yaml.Unmarshal(embedded[catalogFileName], &base); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "decode captured validator catalog", err)
		}
		if err := yaml.Unmarshal(externalData, &overlay); err != nil {
			return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "decode captured validator overlay", err)
		}
		data, err := yaml.Marshal(mergeCatalogs(&base, &overlay))
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "encode captured validator catalog", err)
		}
		files[catalogFileName] = data
		origins[catalogFileName] = sourceMerged
	}
	for name, data := range external {
		if strings.HasPrefix(name, "overlays/") && path.Ext(name) == ".yaml" {
			if base, ok := embedded[name]; ok {
				merged, patched, err := patchOverlayProfile(name, base, data)
				if err != nil {
					return nil, err
				}
				files[name] = merged
				if patched {
					origins[name] = sourceEmbedded
				}
			}
		}
	}
	return files, nil
}

func digestArtifactBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func canonicalArtifactDigest(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "encode artifact digest input", err)
	}
	canonical, err := jsoncanonicalizer.Transform(data)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInvalidRequest, "canonicalize artifact digest input", err)
	}
	return digestArtifactBytes(canonical), nil
}

func artifactFileList(files map[string][]byte, excluded map[string]bool) [][3]any {
	names := make([]string, 0, len(files))
	for name := range files {
		if !excluded[name] {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	result := make([][3]any, 0, len(names))
	for _, name := range names {
		data := files[name]
		result = append(result, [3]any{name, len(data), digestArtifactBytes(data)})
	}
	return result
}

func readArtifactOverride(ctx context.Context, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.WrapCtxErr(err, errors.ErrCodeTimeout, "read inventory override")
	}
	info, err := os.Lstat(name)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "open artifact inventory", err)
	}
	if !info.Mode().IsRegular() {
		return nil, artifact.Invalid("artifact-invalid", "inventory must be a regular file")
	}
	return readExternalFile(filepath.Dir(name), filepath.Base(name), defaults.ArtifactDocumentBytes, false)
}
