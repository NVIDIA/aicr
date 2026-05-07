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

package file

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// Options for configuring the Parser.
type Option func(*Parser)

// Parser parses configuration files with customizable settings.
type Parser struct {
	delimiter       string
	maxSize         int
	skipComments    bool
	kvDelimiter     string
	vDefault        string
	vTrimChars      string
	skipEmptyValues bool
}

// WithDelimiter sets the delimiter used to split entries in the file.
// Default is newline ("\n").
func WithDelimiter(delim string) Option {
	return func(p *Parser) {
		p.delimiter = delim
	}
}

// WithMaxSize sets the maximum size (in bytes) of the file to be parsed.
// Default is 1MB.
func WithMaxSize(size int) Option {
	return func(p *Parser) {
		p.maxSize = size
	}
}

// WithSkipComments sets whether to skip comment lines in the file.
// Default is true.
func WithSkipComments(skip bool) Option {
	return func(p *Parser) {
		p.skipComments = skip
	}
}

// WithKVDelimiter sets the key-value delimiter used in GetMap.
// Default is "=".
func WithKVDelimiter(kvDelim string) Option {
	return func(p *Parser) {
		p.kvDelimiter = kvDelim
	}
}

// WithVDefault sets the default value to use when a key has no associated value.
// Default is an empty string.
func WithVDefault(vDefault string) Option {
	return func(p *Parser) {
		p.vDefault = vDefault
	}
}

// WithVTrimChars sets characters to trim from values in GetMap.
// Default is no trimming.
func WithVTrimChars(trimChars string) Option {
	return func(p *Parser) {
		p.vTrimChars = trimChars
	}
}

// WithSkipEmptyValues sets whether to skip empty values when parsing the file.
// Default is false.
func WithSkipEmptyValues(skip bool) Option {
	return func(p *Parser) {
		p.skipEmptyValues = skip
	}
}

// NewParser creates a new file parser with the provided options.
// Default settings: newline delimiter ("\n"), 1MB max file size.
func NewParser(opts ...Option) *Parser {
	p := &Parser{
		delimiter:       "\n",
		maxSize:         defaults.FileParserMaxSize,
		skipComments:    true,
		kvDelimiter:     "=",
		vDefault:        "",
		vTrimChars:      "",
		skipEmptyValues: false,
	}

	// Apply options
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// GetMap reads the file at the given path and parses its content into a map.
// Each line is split into key-value pairs using the specified kvDel delimiter.
// If a line does not contain the delimiter, the value is set to vDefault.
// Returns an error if the file cannot be read or parsed.
func (p *Parser) GetMap(path string) (map[string]string, error) {
	parts, err := p.GetLines(path)
	if err != nil {
		return nil, err
	}

	result := make(map[string]string)
	for _, part := range parts {
		kv := strings.SplitN(part, p.kvDelimiter, 2)

		if len(kv) != 2 {
			slog.Debug("line without value, using default",
				"line", part,
				"delimiter", p.kvDelimiter,
			)
			key := strings.TrimSpace(kv[0])

			// Skip if skipEmptyValues is enabled and vDefault is empty
			if p.skipEmptyValues && p.vDefault == "" {
				slog.Debug("skipping entry with key-only and empty default",
					"key", key,
				)
				continue
			}

			result[key] = p.vDefault
			continue
		}

		key := strings.TrimSpace(kv[0])
		value := strings.TrimSpace(kv[1])

		// Trim value characters if specified
		if p.vTrimChars != "" {
			value = strings.Trim(value, p.vTrimChars)
		}

		// Skip empty values if configured
		if p.skipEmptyValues && value == "" {
			slog.Debug("skipping entry with empty value",
				"key", key,
			)
			continue
		}

		result[key] = value
	}

	return result, nil
}

// GetLines reads the file at the given path and splits its content into lines
// based on the configured delimiter. It returns a slice of non-empty lines.
// An error is returned if the file cannot be read, exceeds the maximum size,
// or contains invalid UTF-8 content.
func (p *Parser) GetLines(path string) ([]string, error) {
	if path == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "file path cannot be empty")
	}

	// Read file content
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to read file %q", path), err)
	}

	// Validate UTF-8
	if !utf8.Valid(b) {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("content of file %q is not valid UTF-8", path))
	}

	// Check file size
	if len(b) > p.maxSize {
		return nil, errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("file %q exceeds maximum size of %d bytes", path, p.maxSize))
	}

	// Split content by delimiter
	parts := strings.Split(string(b), p.delimiter)

	// Filter out empty strings
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		cleanPart := strings.TrimSpace(part)
		if cleanPart == "" {
			slog.Debug("skipping empty line from file", slog.String("path", path))
			continue
		}

		// Skip comment lines (shouldn't happen with GetMap, but being defensive)
		if p.skipComments && strings.HasPrefix(cleanPart, "#") {
			continue
		}

		result = append(result, cleanPart)
	}

	return result, nil
}
