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

package os

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/NVIDIA/aicr/pkg/collector/file"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

var (
	// Keys to filter out from sysctl properties for privacy/security or noise reduction
	filterOutSysctlKeys = []string{
		"/proc/sys/dev/cdrom/*",
	}

	sysctlRoot      = "/proc/sys"
	sysctlNetPrefix = "/proc/sys/net"
)

// collectSysctl gathers sysctl configurations from /proc/sys, excluding /proc/sys/net
// and returns them as a subtype with file paths as keys and their contents as values.
func (c *Collector) collectSysctl(ctx context.Context) (*measurement.Subtype, error) {
	params := make(map[string]measurement.Reading)

	// Create parser for reading file contents
	parser := file.NewParser()

	err := filepath.WalkDir(sysctlRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to walk directory %s", path), err)
		}

		// Check if context is canceled
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Wrap(errors.ErrCodeTimeout, "sysctl collection cancelled", ctxErr)
		}

		// Skip symlinks to prevent directory traversal attacks
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		if d.IsDir() {
			return nil
		}

		// Ensure path is under root (defense in depth)
		if !strings.HasPrefix(path, sysctlRoot) {
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("path traversal detected: %s", path))
		}

		// Exclude network parameters
		if strings.HasPrefix(path, sysctlNetPrefix) {
			return nil
		}

		// Read file content using parser
		lines, err := parser.GetLines(path)
		if err != nil {
			// Skip files we can't read (some proc files are write-only or restricted)
			return nil
		}

		// Handle multi-line files with space-separated key-value pairs
		if len(lines) > 1 {
			allParsed := c.parseMultiLineKeyValue(path, lines, params)
			if allParsed {
				// All lines were successfully parsed as key-value pairs
				return nil
			}
		}

		// Store single-line or non-key-value content as-is
		// Join lines back if it's multi-line but not key-value format
		content := strings.Join(lines, "\n")
		params[path] = measurement.Str(content)

		return nil
	})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to collect sysctl parameters", err)
	}

	res := &measurement.Subtype{
		Name: "sysctl",
		Data: measurement.FilterOut(params, filterOutSysctlKeys),
	}

	return res, nil
}

// parseMultiLineKeyValue attempts to parse lines as space-separated key-value pairs.
// Returns true if all non-empty lines were successfully parsed as key-value pairs.
func (c *Collector) parseMultiLineKeyValue(path string, lines []string, params map[string]measurement.Reading) bool {
	tmp := make(map[string]measurement.Reading)

	for _, line := range lines {
		if line == "" {
			continue
		}

		// Check if line has space-separated key and value
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			// Create new entry with extended path: /proc/sys/path/key
			key := parts[0]
			value := strings.Join(parts[1:], " ")
			extendedPath := path + "/" + key
			tmp[extendedPath] = measurement.Str(value)
		} else {
			// Not a key-value pair format
			return false
		}
	}

	for k, v := range tmp {
		params[k] = v
	}

	return true
}
