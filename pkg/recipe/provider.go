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
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
	"gopkg.in/yaml.v3"
)

// DataProvider abstracts access to recipe data files.
// This allows layering external directories over embedded data.
type DataProvider interface {
	// ReadFile reads a file by path (relative to data directory).
	ReadFile(path string) ([]byte, error)

	// WalkDir walks the directory tree rooted at root.
	WalkDir(root string, fn fs.WalkDirFunc) error

	// Source returns a description of where data came from (for debugging).
	Source(path string) string
}

// EmbeddedDataProvider wraps an embed.FS to implement DataProvider.
type EmbeddedDataProvider struct {
	fs     embed.FS
	prefix string // e.g., "data" to strip from paths
}

// NewEmbeddedDataProvider creates a provider from an embedded filesystem.
func NewEmbeddedDataProvider(efs embed.FS, prefix string) *EmbeddedDataProvider {
	return &EmbeddedDataProvider{
		fs:     efs,
		prefix: prefix,
	}
}

// ReadFile reads a file from the embedded filesystem.
func (p *EmbeddedDataProvider) ReadFile(path string) ([]byte, error) {
	fullPath := filepath.Join(p.prefix, path)
	slog.Debug("reading file from embedded provider", "path", path, "fullPath", fullPath)
	return p.fs.ReadFile(fullPath)
}

// WalkDir walks the embedded filesystem.
func (p *EmbeddedDataProvider) WalkDir(root string, fn fs.WalkDirFunc) error {
	fullRoot := filepath.Join(p.prefix, root)
	if fullRoot == "" {
		fullRoot = "." // embed.FS expects "." for root
	}
	slog.Debug("walking embedded filesystem", "root", root, "fullRoot", fullRoot)
	return fs.WalkDir(p.fs, fullRoot, func(path string, d fs.DirEntry, err error) error {
		// Strip the prefix before passing to callback
		var relPath string
		if p.prefix == "" {
			relPath = path
		} else {
			relPath = strings.TrimPrefix(path, p.prefix+"/")
			if relPath == p.prefix {
				relPath = ""
			}
		}
		return fn(relPath, d, err)
	})
}

// Source returns "embedded" for all paths.
func (p *EmbeddedDataProvider) Source(path string) string {
	return sourceEmbedded
}

// LayeredDataProvider overlays an external directory on top of embedded data.
// For registryFileName: merges external components with embedded (external takes precedence).
// For all other files: external completely replaces embedded if present.
type LayeredDataProvider struct {
	embedded    *EmbeddedDataProvider
	externalDir string

	// Cached merged registry (computed once on first access)
	mergedRegistryOnce sync.Once
	mergedRegistry     []byte
	mergedRegistryErr  error

	// Cached merged catalog (computed once on first access)
	mergedCatalogOnce sync.Once
	mergedCatalog     []byte
	mergedCatalogErr  error

	// Track which files came from external (for debugging)
	externalFiles map[string]bool
}

// LayeredProviderConfig configures the layered data provider.
type LayeredProviderConfig struct {
	// ExternalDir is the path to the external data directory.
	ExternalDir string

	// MaxFileSize is the maximum allowed file size in bytes (default: 10MB).
	MaxFileSize int64

	// AllowSymlinks allows symlinks in the external directory (default: false).
	AllowSymlinks bool
}

const (
	// DefaultMaxFileSize is the default maximum file size (10MB).
	DefaultMaxFileSize = 10 * 1024 * 1024

	// sourceEmbedded is the source name for embedded files.
	sourceEmbedded = "embedded"

	// sourceExternal is the source name for external files.
	sourceExternal = "external"

	// sourceMerged is the source name for files merged from both embedded and external.
	sourceMerged = "merged (" + sourceEmbedded + " + " + sourceExternal + ")"

	// registryFileName is the name of the component registry file.
	registryFileName = "registry.yaml"

	// catalogFileName is the name of the validator catalog file.
	catalogFileName = "validators/catalog.yaml"
)

// NewLayeredDataProvider creates a provider that layers external data over embedded.
// Returns an error if:
// - External directory doesn't exist
// - External directory doesn't contain registryFileName
// - Path traversal is detected
// - File size exceeds limits
func NewLayeredDataProvider(embedded *EmbeddedDataProvider, config LayeredProviderConfig) (*LayeredDataProvider, error) {
	slog.Debug("creating layered data provider",
		"external_dir", config.ExternalDir,
		"max_file_size", config.MaxFileSize,
		"allow_symlinks", config.AllowSymlinks)

	if config.MaxFileSize == 0 {
		config.MaxFileSize = DefaultMaxFileSize
	}

	// Validate external directory exists
	slog.Debug("validating external directory")
	info, err := os.Stat(config.ExternalDir)
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeNotFound,
			fmt.Sprintf("external data directory not found: %s", config.ExternalDir), err)
	}
	if !info.IsDir() {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("external data path is not a directory: %s", config.ExternalDir))
	}

	// Validate registryFileName exists in external directory
	registryPath := filepath.Join(config.ExternalDir, registryFileName)
	slog.Debug("checking for required registry file", "path", registryPath)
	if _, statErr := os.Stat(registryPath); statErr != nil {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("%s is required in external data directory: %s", registryFileName, config.ExternalDir))
	}
	slog.Debug("registry file found", "path", registryPath)

	// Validate external directory for security issues
	slog.Debug("scanning external directory for security issues")
	externalFiles := make(map[string]bool)
	err = filepath.WalkDir(config.ExternalDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to walk external directory", err)
		}
		if d.IsDir() {
			return nil
		}

		// Get relative path
		relPath, relErr := filepath.Rel(config.ExternalDir, path)
		if relErr != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to get relative path", relErr)
		}

		// Reject any path that escapes the external root or contains
		// reserved segments. filepath.IsLocal is the canonical check (rejects
		// absolute paths, drive letters, ".." segments, and reserved names)
		// and avoids the false positives a substring scan for ".." produces
		// on benign names like "foo..bak".
		if !filepath.IsLocal(relPath) {
			return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("path traversal or non-local path detected: %s", relPath))
		}

		// Check for symlinks
		if !config.AllowSymlinks {
			info, lstatErr := os.Lstat(path)
			if lstatErr != nil {
				return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to stat file", lstatErr)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
					fmt.Sprintf("symlinks not allowed: %s", relPath))
			}
		}

		// Check file size
		info, statErr := d.Info()
		if statErr != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to get file info", statErr)
		}
		if info.Size() > config.MaxFileSize {
			return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("file too large (%d bytes, max %d): %s", info.Size(), config.MaxFileSize, relPath))
		}

		externalFiles[relPath] = true
		slog.Debug("discovered external file",
			"path", relPath,
			"size", info.Size())
		return nil
	})
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "external directory validation failed", err)
	}

	slog.Info("layered data provider initialized",
		"external_dir", config.ExternalDir,
		"external_files", len(externalFiles))

	// Log all external files at debug level for troubleshooting
	for path := range externalFiles {
		slog.Debug("external file registered", "path", path)
	}

	return &LayeredDataProvider{
		embedded:      embedded,
		externalDir:   config.ExternalDir,
		externalFiles: externalFiles,
	}, nil
}

// ExternalFiles returns a sorted list of file paths that came from the external
// data directory. Paths are relative to the external directory root.
func (p *LayeredDataProvider) ExternalFiles() []string {
	files := make([]string, 0, len(p.externalFiles))
	for path := range p.externalFiles {
		files = append(files, path)
	}
	sort.Strings(files)
	return files
}

// ExternalDir returns the path to the external data directory.
func (p *LayeredDataProvider) ExternalDir() string {
	return p.externalDir
}

// ReadFile reads a file, checking external directory first.
// For registryFileName, returns merged content.
// For other files, external completely replaces embedded.
func (p *LayeredDataProvider) ReadFile(path string) ([]byte, error) {
	slog.Debug("reading file from layered provider", "path", path)

	// Special handling for registry file - merge instead of replace
	if path == registryFileName {
		slog.Debug("reading merged registry file")
		return p.getMergedRegistry()
	}

	// Special handling for catalog file - merge instead of replace (when external exists)
	if path == catalogFileName && p.externalFiles[catalogFileName] {
		slog.Debug("reading merged catalog file")
		return p.getMergedCatalog()
	}

	// Check external directory first
	if p.externalFiles[path] {
		externalPath := filepath.Join(p.externalDir, path)
		data, err := os.ReadFile(externalPath)
		if err != nil {
			return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, fmt.Sprintf("failed to read external file %s", path), err)
		}
		slog.Debug("read from external data directory", "path", path)
		return data, nil
	}

	// Fall back to embedded
	slog.Debug("falling back to embedded data", "path", path)
	return p.embedded.ReadFile(path)
}

// WalkDir walks both embedded and external directories.
// External files take precedence over embedded files.
func (p *LayeredDataProvider) WalkDir(root string, fn fs.WalkDirFunc) error {
	slog.Debug("walking layered data directory", "root", root)

	// Track files we've visited (to avoid duplicates)
	visited := make(map[string]bool)

	// Walk external directory first
	externalRoot := filepath.Join(p.externalDir, root)
	if _, err := os.Stat(externalRoot); err == nil {
		slog.Debug("walking external directory", "path", externalRoot)
		err := filepath.WalkDir(externalRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to walk external directory", err)
			}

			relPath, relErr := filepath.Rel(p.externalDir, path)
			if relErr != nil {
				return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to compute relative path", relErr)
			}

			// Strip root prefix if present
			if root != "" {
				relPath = strings.TrimPrefix(relPath, root+"/")
				if relPath == root {
					relPath = ""
				}
			}

			visited[relPath] = true
			slog.Debug("visiting external file", "path", relPath, "isDir", d.IsDir())
			return fn(relPath, d, nil)
		})
		if err != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to walk external directory tree", err)
		}
	}

	slog.Debug("walking embedded directory", "root", root)

	// Walk embedded, skipping already-visited paths
	return p.embedded.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to walk embedded directory", err)
		}
		if visited[path] {
			slog.Debug("skipping embedded file (external takes precedence)", "path", path)
			return nil // Skip, external takes precedence
		}
		slog.Debug("visiting embedded file", "path", path, "isDir", d.IsDir())
		return fn(path, d, err)
	})
}

// Source returns "external" or "embedded" depending on where the file comes from.
func (p *LayeredDataProvider) Source(path string) string {
	var source string
	switch {
	case path == registryFileName:
		// Always merged: registry.yaml is required in external dir (enforced by constructor).
		source = sourceMerged
	case path == catalogFileName && p.externalFiles[catalogFileName]:
		// Merged only when external catalog exists (catalog is optional).
		source = sourceMerged
	case p.externalFiles[path]:
		source = sourceExternal
	default:
		source = sourceEmbedded
	}
	slog.Debug("resolved file source", "path", path, "source", source)
	return source
}

// fileReader is a minimal interface for reading a file by path.
type fileReader interface {
	ReadFile(path string) ([]byte, error)
}

// mergeEmbeddedAndExternal loads a YAML file from both embedded and external sources,
// unmarshals each into type T, merges them using the provided function, and serializes
// the result back to YAML bytes.
func mergeEmbeddedAndExternal[T any](
	embedded fileReader, externalDir string,
	fileName string, merge func(embedded, external *T) *T,
) ([]byte, error) {

	kind := filepath.Base(fileName)
	slog.Debug("merging files", "file", kind)

	// Load embedded
	embeddedData, err := embedded.ReadFile(fileName)
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to read embedded "+kind, err)
	}

	var embeddedVal T
	if unmarshalErr := yaml.Unmarshal(embeddedData, &embeddedVal); unmarshalErr != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to parse embedded "+kind, unmarshalErr)
	}

	// Load external
	externalPath := filepath.Join(externalDir, fileName)
	externalData, err := os.ReadFile(externalPath)
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to read external "+kind, err)
	}

	var externalVal T
	if unmarshalErr := yaml.Unmarshal(externalData, &externalVal); unmarshalErr != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to parse external "+kind, unmarshalErr)
	}

	// Merge: external overrides embedded
	merged := merge(&embeddedVal, &externalVal)

	// Serialize merged result
	data, marshalErr := yaml.Marshal(merged)
	if marshalErr != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to serialize merged "+kind, marshalErr)
	}

	return data, nil
}

// getMergedRegistry returns the merged registryFileName content.
// External registry components are merged with embedded, with external taking precedence.
func (p *LayeredDataProvider) getMergedRegistry() ([]byte, error) {
	p.mergedRegistryOnce.Do(func() {
		p.mergedRegistry, p.mergedRegistryErr = mergeEmbeddedAndExternal(
			p.embedded, p.externalDir, registryFileName, mergeRegistries,
		)
	})

	return p.mergedRegistry, p.mergedRegistryErr
}

// mergeByName merges two slices by a name key. Items from external override
// embedded items with the same name. New items from external are appended.
// Embedded order is preserved.
func mergeByName[T any](embedded, external []T, getName func(T) string) []T {
	result := make([]T, 0, len(embedded)+len(external))

	extByName := make(map[string]T, len(external))
	for _, item := range external {
		if name := getName(item); name != "" {
			extByName[name] = item
		}
	}

	addedNames := make(map[string]bool, len(embedded))
	for _, item := range embedded {
		name := getName(item)
		if ext, found := extByName[name]; found {
			result = append(result, ext)
			slog.Debug("item overridden from external", "name", name)
		} else {
			result = append(result, item)
			slog.Debug("item retained from embedded", "name", name)
		}
		addedNames[name] = true
	}

	for _, item := range external {
		if name := getName(item); !addedNames[name] {
			result = append(result, item)
			slog.Debug("item added from external", "name", name)
		}
	}

	return result
}

// mergeRegistries merges external registry into embedded.
// Components with the same name are replaced by external version.
// New components from external are added.
func mergeRegistries(embedded, external *ComponentRegistry) *ComponentRegistry {
	slog.Debug("starting registry merge",
		"embedded_count", len(embedded.Components),
		"external_count", len(external.Components))

	if external.APIVersion != "" && external.APIVersion != embedded.APIVersion {
		slog.Warn("external registry has different API version",
			"embedded", embedded.APIVersion,
			"external", external.APIVersion)
	}

	return &ComponentRegistry{
		APIVersion: embedded.APIVersion,
		Kind:       embedded.Kind,
		Components: mergeByName(embedded.Components, external.Components,
			func(c ComponentConfig) string { return c.Name }),
	}
}

// catalogForMerge is a minimal representation for catalog merge operations.
// Uses generic map types to avoid importing pkg/validator/catalog (which would
// create a circular dependency). All validator entry fields are preserved through
// the map[string]interface{} round-trip.
type catalogForMerge struct {
	APIVersion string                   `yaml:"apiVersion"`
	Kind       string                   `yaml:"kind"`
	Metadata   map[string]interface{}   `yaml:"metadata"`
	Validators []map[string]interface{} `yaml:"validators"`
}

// getMergedCatalog returns the merged catalogFileName content.
// External catalog validators are merged with embedded, with external taking precedence by name.
func (p *LayeredDataProvider) getMergedCatalog() ([]byte, error) {
	p.mergedCatalogOnce.Do(func() {
		p.mergedCatalog, p.mergedCatalogErr = mergeEmbeddedAndExternal(
			p.embedded, p.externalDir, catalogFileName, mergeCatalogs,
		)
	})

	return p.mergedCatalog, p.mergedCatalogErr
}

// mergeCatalogs merges external catalog into embedded.
// Validators with the same name are replaced by external version.
// New validators from external are added.
func mergeCatalogs(embedded, external *catalogForMerge) *catalogForMerge {
	slog.Debug("starting catalog merge",
		"embedded_count", len(embedded.Validators),
		"external_count", len(external.Validators))

	if external.APIVersion != "" && external.APIVersion != embedded.APIVersion {
		slog.Warn("external catalog has different API version",
			"embedded", embedded.APIVersion,
			"external", external.APIVersion)
	}

	return &catalogForMerge{
		APIVersion: embedded.APIVersion,
		Kind:       embedded.Kind,
		Metadata:   embedded.Metadata,
		Validators: mergeByName(embedded.Validators, external.Validators,
			func(v map[string]interface{}) string { s, _ := v["name"].(string); return s }),
	}
}

// Global data provider (defaults to embedded, can be set for layered)
var (
	dataProviderMu         sync.RWMutex
	globalDataProvider     DataProvider
	dataProviderGeneration int // Incremented when provider changes
)

// SetDataProvider sets the global data provider.
// This should be called before any recipe operations if using external data.
// Note: This invalidates cached data, so callers should ensure this is called
// early in the application lifecycle.
func SetDataProvider(provider DataProvider) {
	dataProviderMu.Lock()
	defer dataProviderMu.Unlock()
	globalDataProvider = provider
	dataProviderGeneration++
	slog.Info("data provider set", "generation", dataProviderGeneration)
}

// GetDataProvider returns the global data provider.
// Returns the embedded provider if none was set.
func GetDataProvider() DataProvider {
	dataProviderMu.Lock()
	defer dataProviderMu.Unlock()
	if globalDataProvider == nil {
		slog.Debug("initializing default embedded data provider")
		globalDataProvider = NewEmbeddedDataProvider(GetEmbeddedFS(), "")
	}
	return globalDataProvider
}

func getDataProviderGeneration() int {
	dataProviderMu.RLock()
	defer dataProviderMu.RUnlock()
	return dataProviderGeneration
}
