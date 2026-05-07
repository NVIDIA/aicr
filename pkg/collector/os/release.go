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
	"os"

	"github.com/NVIDIA/aicr/pkg/collector/file"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

var (
	filePathReleasePrimary  = "/etc/os-release"
	filePathReleaseFallback = "/usr/lib/os-release"
	fileKVDelRelease        = "="
)

// collectRelease gathers OS release information from /etc/os-release.
// It returns a measurement subtype containing key-value pairs of release data.
// Example keys include NAME, VERSION, ID, PRETTY_NAME, etc.
// Per freedesktop.org spec, falls back to /usr/lib/os-release if primary file doesn't exist.
//
//	NAME="Ubuntu"
//	ID=ubuntu
//	VERSION_ID="22.04"
//	PRETTY_NAME="Ubuntu 22.04.4 LTS"
func (c *Collector) collectRelease(ctx context.Context) (*measurement.Subtype, error) {
	// Check if context is canceled
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "release collection cancelled", err)
	}

	// Try primary location first, fall back to alternative per freedesktop.org spec
	root := filePathReleasePrimary
	if _, err := os.Stat(root); os.IsNotExist(err) {
		root = filePathReleaseFallback
	}

	parser := file.NewParser(
		file.WithKVDelimiter(fileKVDelRelease),
		file.WithVTrimChars(`"'`),

		// Remove surrounding quotes if any per freedesktop.org spec
		file.WithSkipComments(true),

		// Skip malformed lines (lines without '=' that got empty default value)
		// Empty values from GetMap with empty vDefault indicate malformed lines
		file.WithSkipEmptyValues(true),
	)

	params, err := parser.GetMap(root)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to read os release from %s", root), err)
	}

	// Pre-allocate with typical capacity (most files have 10-15 fields)
	readings := make(map[string]measurement.Reading, 15)

	for key, value := range params {
		readings[key] = measurement.Str(value)
	}

	res := &measurement.Subtype{
		Name: "release",
		Data: readings,
	}

	return res, nil
}
