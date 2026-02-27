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

	// registryFileName is the name of the component registry file.
	registryFileName = "registry.yaml"
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

		// Check for path traversal
		if strings.Contains(relPath, "..") {
			return aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
				fmt.Sprintf("path traversal detected: %s", relPath))
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
		source = "merged (" + sourceEmbedded + " + " + sourceExternal + ")"
	case p.externalFiles[path]:
		source = sourceExternal
	default:
		source = sourceEmbedded
	}
	slog.Debug("resolved file source", "path", path, "source", source)
	return source
}

// getMergedRegistry returns the merged registryFileName content.
// External registry components are merged with embedded, with external taking precedence.
func (p *LayeredDataProvider) getMergedRegistry() ([]byte, error) {
	p.mergedRegistryOnce.Do(func() {
		slog.Debug("merging registry files")

		// Load embedded registry
		embeddedData, err := p.embedded.ReadFile(registryFileName)
		if err != nil {
			p.mergedRegistryErr = aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to read embedded registry", err)
			return
		}

		var embeddedReg ComponentRegistry
		if unmarshalErr := yaml.Unmarshal(embeddedData, &embeddedReg); unmarshalErr != nil {
			p.mergedRegistryErr = aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to parse embedded registry", unmarshalErr)
			return
		}

		// Load external registry
		externalPath := filepath.Join(p.externalDir, registryFileName)
		externalData, err := os.ReadFile(externalPath)
		if err != nil {
			p.mergedRegistryErr = aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to read external registry", err)
			return
		}

		var externalReg ComponentRegistry
		if unmarshalErr := yaml.Unmarshal(externalData, &externalReg); unmarshalErr != nil {
			p.mergedRegistryErr = aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to parse external registry", unmarshalErr)
			return
		}

		// Validate schema version compatibility
		if externalReg.APIVersion != "" && externalReg.APIVersion != embeddedReg.APIVersion {
			slog.Warn("external registry has different API version",
				"embedded", embeddedReg.APIVersion,
				"external", externalReg.APIVersion)
		}

		// Merge: external components override embedded by name
		merged := mergeRegistries(&embeddedReg, &externalReg)

		// Serialize merged registry
		p.mergedRegistry, p.mergedRegistryErr = yaml.Marshal(merged)
		if p.mergedRegistryErr != nil {
			p.mergedRegistryErr = aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "failed to serialize merged registry", p.mergedRegistryErr)
			return
		}

		slog.Info("merged component registries",
			"embedded_components", len(embeddedReg.Components),
			"external_components", len(externalReg.Components),
			"merged_components", len(merged.Components))
	})

	return p.mergedRegistry, p.mergedRegistryErr
}

// mergeRegistries merges external registry into embedded.
// Components with the same name are replaced by external version.
// New components from external are added.
func mergeRegistries(embedded, external *ComponentRegistry) *ComponentRegistry {
	slog.Debug("starting registry merge",
		"embedded_count", len(embedded.Components),
		"external_count", len(external.Components))

	result := &ComponentRegistry{
		APIVersion: embedded.APIVersion,
		Kind:       embedded.Kind,
		Components: make([]ComponentConfig, 0, len(embedded.Components)+len(external.Components)),
	}

	// Index external components by name
	externalByName := make(map[string]*ComponentConfig)
	for i := range external.Components {
		comp := &external.Components[i]
		externalByName[comp.Name] = comp
		slog.Debug("indexed external component", "name", comp.Name)
	}

	// Add embedded components, replacing with external if present
	addedNames := make(map[string]bool)
	for _, comp := range embedded.Components {
		if ext, found := externalByName[comp.Name]; found {
			result.Components = append(result.Components, *ext)
			slog.Debug("component overridden from external", "name", comp.Name)
		} else {
			result.Components = append(result.Components, comp)
			slog.Debug("component retained from embedded", "name", comp.Name)
		}
		addedNames[comp.Name] = true
	}

	// Add new components from external that aren't in embedded
	for _, comp := range external.Components {
		if !addedNames[comp.Name] {
			result.Components = append(result.Components, comp)
			slog.Debug("component added from external", "name", comp.Name)
		}
	}

	return result
}

// Global data provider (defaults to embedded, can be set for layered)
var (
	globalDataProvider     DataProvider
	dataProviderGeneration int // Incremented when provider changes
)

// SetDataProvider sets the global data provider.
// This should be called before any recipe operations if using external data.
// Note: This invalidates cached data, so callers should ensure this is called
// early in the application lifecycle.
func SetDataProvider(provider DataProvider) {
	globalDataProvider = provider
	dataProviderGeneration++
	slog.Info("data provider set", "generation", dataProviderGeneration)
}

// GetDataProvider returns the global data provider.
// Returns the embedded provider if none was set.
func GetDataProvider() DataProvider {
	if globalDataProvider == nil {
		slog.Debug("initializing default embedded data provider")
		globalDataProvider = NewEmbeddedDataProvider(GetEmbeddedFS(), "")
	}
	return globalDataProvider
}

func getDataProviderGeneration() int {
	return dataProviderGeneration
}
