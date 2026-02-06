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

package serializer

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"

	"github.com/NVIDIA/eidos/pkg/errors"
)

// TemplateWriter executes Go templates with snapshot data.
// It supports sprig template functions for rich formatting capabilities.
type TemplateWriter struct {
	templatePath string
	output       io.Writer
	closer       io.Closer
}

// NewTemplateWriter creates a writer that outputs to the given io.Writer.
// The templatePath must point to a valid Go template file.
func NewTemplateWriter(templatePath string, output io.Writer) *TemplateWriter {
	if output == nil {
		output = os.Stdout
	}
	return &TemplateWriter{
		templatePath: templatePath,
		output:       output,
	}
}

// NewTemplateFileWriter creates a writer that outputs to a file.
// If outputPath is empty or "-", writes to stdout.
func NewTemplateFileWriter(templatePath, outputPath string) (*TemplateWriter, error) {
	if outputPath == "" || outputPath == "-" || outputPath == StdoutURI {
		return NewTemplateWriter(templatePath, os.Stdout), nil
	}

	file, err := os.Create(outputPath)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to create output file %q", outputPath), err)
	}

	return &TemplateWriter{
		templatePath: templatePath,
		output:       file,
		closer:       file,
	}, nil
}

// Serialize executes the template with the provided data and writes to output.
// The data is passed directly to the template, which can access all exported fields.
// The template can be loaded from a local file path or HTTP/HTTPS URL.
func (t *TemplateWriter) Serialize(ctx context.Context, data any) error {
	// Check context before starting
	if ctx.Err() != nil {
		return errors.Wrap(errors.ErrCodeTimeout, "context canceled before template execution", ctx.Err())
	}

	// Read template content (supports both file paths and URLs)
	templateContent, err := readTemplateContent(ctx, t.templatePath)
	if err != nil {
		return err
	}

	// Parse template with sprig functions
	tmpl, err := template.New("snapshot").Funcs(sprig.FuncMap()).Parse(string(templateContent))
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse template", err)
	}

	// Execute template
	if err := tmpl.Execute(t.output, data); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to execute template", err)
	}

	return nil
}

// Close releases any resources associated with the TemplateWriter.
// It should be called when done writing, especially for file-based writers.
// It's safe to call Close multiple times or on stdout-based writers.
func (t *TemplateWriter) Close() error {
	if t.closer != nil {
		return t.closer.Close()
	}
	return nil
}

// ValidateTemplateFile checks if the template file exists and is readable.
// For local files, validates existence and that it's not a directory.
// For URLs (http:// or https://), skips validation as we can't check without fetching.
// Returns nil if the file is valid, or an error describing the problem.
func ValidateTemplateFile(path string) error {
	// Skip validation for URLs - we can't easily check if a URL exists without fetching
	if isURL(path) {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return errors.New(errors.ErrCodeNotFound, fmt.Sprintf("template file not found: %s", path))
		}
		return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to access template file %q", path), err)
	}

	if info.IsDir() {
		return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("template path is a directory, not a file: %s", path))
	}

	return nil
}

// ExecuteTemplateToBytes executes a template with the given data and returns the result as bytes.
// The template can be loaded from a local file path or HTTP/HTTPS URL.
// This is useful for agent mode where we need to retrieve snapshot data and transform it.
func ExecuteTemplateToBytes(ctx context.Context, templatePath string, data any) ([]byte, error) {
	// Read template content (supports both file paths and URLs)
	templateContent, err := readTemplateContent(ctx, templatePath)
	if err != nil {
		return nil, err
	}

	// Parse template with sprig functions
	tmpl, err := template.New("snapshot").Funcs(sprig.FuncMap()).Parse(string(templateContent))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse template", err)
	}

	// Execute template to buffer
	var buf []byte
	writer := &bytesWriter{buf: &buf}
	if err := tmpl.Execute(writer, data); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to execute template", err)
	}

	return buf, nil
}

// bytesWriter is a simple io.Writer that appends to a byte slice.
type bytesWriter struct {
	buf *[]byte
}

func (w *bytesWriter) Write(p []byte) (n int, err error) {
	*w.buf = append(*w.buf, p...)
	return len(p), nil
}

// isURL checks if the path is an HTTP or HTTPS URL.
func isURL(path string) bool {
	return strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://")
}

// readTemplateContent reads template content from a file path or URL.
func readTemplateContent(ctx context.Context, path string) ([]byte, error) {
	if isURL(path) {
		httpReader := NewHTTPReader()
		content, err := httpReader.ReadWithContext(ctx, path)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeUnavailable, fmt.Sprintf("failed to fetch template from URL %q", path), err)
		}
		return content, nil
	}

	content, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New(errors.ErrCodeNotFound, fmt.Sprintf("template file not found: %s", path))
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to read template file %q", path), err)
	}
	return content, nil
}
