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

package component

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/bundler/config"
	"github.com/NVIDIA/aicr/pkg/bundler/result"
	"github.com/NVIDIA/aicr/pkg/bundler/types"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// BaseBundler provides common functionality for bundler implementations.
// Bundlers can use this to reuse standard operations and reduce boilerplate.
//
// Thread-safety: BaseBundler is safe for use by a single bundler instance.
// Do not share BaseBundler instances between concurrent bundler executions.
type BaseBundler struct {
	Config *config.Config
	Result *result.Result
}

// NewBaseBundler creates a new base bundler helper.
func NewBaseBundler(cfg *config.Config, bundlerType types.BundleType) *BaseBundler {
	if cfg == nil {
		cfg = config.NewConfig()
	}
	return &BaseBundler{
		Config: cfg,
		Result: result.New(bundlerType),
	}
}

// BundleDirectories holds the standard bundle directory structure.
type BundleDirectories struct {
	Root      string
	Scripts   string
	Manifests string
}

// CreateBundleDir creates the root bundle directory.
// Subdirectories (scripts, manifests) are created on-demand when files are written.
// Returns the bundle directories for easy access to each subdirectory path.
func (b *BaseBundler) CreateBundleDir(outputDir, bundleName string) (BundleDirectories, error) {
	bundleDir := filepath.Join(outputDir, bundleName)

	dirs := BundleDirectories{
		Root:      bundleDir,
		Scripts:   filepath.Join(bundleDir, "scripts"),
		Manifests: filepath.Join(bundleDir, "manifests"),
	}

	// Only create the root directory. Subdirectories will be created when needed.
	if err := os.MkdirAll(dirs.Root, 0755); err != nil {
		return dirs, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to create directory %s", dirs.Root), err)
	}

	slog.Debug("bundle directory created",
		"bundle", bundleName,
		"root", dirs.Root,
	)

	return dirs, nil
}

// WriteFile writes content to a file and tracks it in the result.
// The file is created with the specified permissions and automatically
// added to the result's file list with its size.
// Parent directories are created automatically if they don't exist.
func (b *BaseBundler) WriteFile(path string, content []byte, perm os.FileMode) error {
	// Ensure parent directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to create directory %s", dir), err)
	}

	if err := os.WriteFile(path, content, perm); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to write %s", path), err)
	}

	b.Result.AddFile(path, int64(len(content)))

	slog.Debug("file written",
		"path", path,
		"size_bytes", len(content),
		"permissions", perm,
	)

	return nil
}

// WriteFileString writes string content to a file.
// This is a convenience wrapper around WriteFile for string content.
func (b *BaseBundler) WriteFileString(path, content string, perm os.FileMode) error {
	return b.WriteFile(path, []byte(content), perm)
}

// RenderTemplate renders a template with the given data.
// The template is parsed and executed with the provided data structure.
// Returns the rendered content as a string.
func (b *BaseBundler) RenderTemplate(tmplContent, name string, data any) (string, error) {
	tmpl, err := template.New(name).Parse(tmplContent)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to parse template %s", name), err)
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to execute template %s", name), err)
	}

	return buf.String(), nil
}

// RenderAndWriteTemplate renders a template and writes it to a file.
// This combines RenderTemplate and WriteFile for convenience.
func (b *BaseBundler) RenderAndWriteTemplate(tmplContent, name, outputPath string, data any, perm os.FileMode) error {
	content, err := b.RenderTemplate(tmplContent, name, data)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to render template for writing", err)
	}

	return b.WriteFileString(outputPath, content, perm)
}

// GenerateChecksums creates a checksums.txt file for all generated files.
// The checksum file contains SHA256 hashes for verification of bundle integrity.
// Each line follows the format: "<hash>  <relative-path>"
func (b *BaseBundler) GenerateChecksums(ctx context.Context, bundleDir string) error {
	if err := checksum.GenerateChecksums(ctx, bundleDir, b.Result.Files); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to generate checksums", err)
	}

	// Add checksums.txt to the result files
	checksumPath := checksum.GetChecksumFilePath(bundleDir)
	info, err := os.Stat(checksumPath)
	if err == nil {
		b.Result.AddFile(checksumPath, info.Size())
	}

	return nil
}

// MakeExecutable changes file permissions to make a file executable.
// This is typically used for shell scripts after writing them.
func (b *BaseBundler) MakeExecutable(path string) error {
	if err := os.Chmod(path, 0755); err != nil {
		wrappedErr := errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to make %s executable", filepath.Base(path)), err)
		b.Result.AddError(wrappedErr)
		return wrappedErr
	}

	slog.Debug("file made executable", "path", path)
	return nil
}

// Finalize marks the bundler as successful and updates metrics.
// This should be called at the end of a successful bundle generation.
// It updates the result duration and marks success.
// Note: Bundlers should record their own Prometheus metrics after calling this.
func (b *BaseBundler) Finalize(start time.Time) {
	b.Result.Duration = time.Since(start)
	b.Result.MarkSuccess()

	slog.Debug("bundle generation finalized",
		"type", b.Result.Type,
		"files", len(b.Result.Files),
		"size_bytes", b.Result.Size,
		"duration", b.Result.Duration.Round(time.Millisecond),
	)
}

// CheckContext checks if the context has been canceled.
// This should be called periodically during long-running operations
// to allow for graceful cancellation.
func (b *BaseBundler) CheckContext(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return errors.Wrap(errors.ErrCodeTimeout, "context canceled during bundling", ctx.Err())
	default:
		return nil
	}
}

// AddError adds a non-fatal error to the result.
// These errors are collected but do not stop bundle generation.
func (b *BaseBundler) AddError(err error) {
	if err != nil {
		b.Result.AddError(err)
		slog.Warn("non-fatal error during bundle generation",
			"type", b.Result.Type,
			"error", err,
		)
	}
}

const (
	//
	bundlerVersionKey       = "bundler_version"
	recipeBundlerVersionKey = "recipe-version"
)

// GetBundlerVersion retrieves the bundler version from the config map.
func GetBundlerVersion(m map[string]string) string {
	if v, ok := m[bundlerVersionKey]; ok {
		return v
	}
	return "unknown"
}

// GetRecipeBundlerVersion retrieves the bundler version from the recipe config map.
func GetRecipeBundlerVersion(m map[string]string) string {
	if v, ok := m[recipeBundlerVersionKey]; ok {
		return v
	}
	return "unknown"
}

// buildBaseConfigMap creates a configuration map with common bundler settings.
// Returns a map containing bundler version.
// Bundlers can extend this map with their specific values.
func (b *BaseBundler) buildBaseConfigMap() map[string]string {
	config := make(map[string]string)

	config[bundlerVersionKey] = b.Config.Version()

	return config
}

// BuildConfigMapFromInput creates a configuration map from a RecipeInput.
// This includes base config from bundler settings plus recipe version.
// Use this when working with RecipeResult (new format) instead of Recipe.
func (b *BaseBundler) BuildConfigMapFromInput(input interface{ GetVersion() string }) map[string]string {
	config := b.buildBaseConfigMap()

	// Add recipe version if available
	if version := input.GetVersion(); version != "" {
		config[recipeBundlerVersionKey] = version
	}

	return config
}

// TemplateFunc is a function that retrieves templates by name.
// Returns the template content and whether it was found.
type TemplateFunc func(name string) (string, bool)

// GenerateFileFromTemplate is a convenience method that combines template retrieval,
// rendering, and file writing in one call. This reduces boilerplate in bundler
// implementations by handling the common pattern of:
// 1. Get template by name
// 2. Check if template exists
// 3. Render template with data
// 4. Write rendered content to file
//
// Example usage:
//
//	err := b.GenerateFileFromTemplate(ctx, GetTemplate, "values.yaml",
//	    filepath.Join(dir, "values.yaml"), data, 0644)
func (b *BaseBundler) GenerateFileFromTemplate(ctx context.Context, getTemplate TemplateFunc,
	templateName, outputPath string, data any, perm os.FileMode) error {

	if err := b.CheckContext(ctx); err != nil {
		// CheckContext already wraps ctx.Err() with ErrCodeTimeout; preserve
		// the inner code rather than re-wrapping with ErrCodeInternal.
		return err
	}

	tmpl, ok := getTemplate(templateName)
	if !ok {
		return errors.New(errors.ErrCodeNotFound, fmt.Sprintf("%s template not found", templateName))
	}

	return b.RenderAndWriteTemplate(tmpl, templateName, outputPath, data, perm)
}
