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
	"strings"

	"github.com/NVIDIA/aicr/pkg/collector/file"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

var (
	filePathKMod = "/proc/modules"
)

// collectKMod retrieves the list of loaded kernel modules from /proc/modules
// and returns them as a subtype with module names as keys.
func (c *Collector) collectKMod(ctx context.Context) (*measurement.Subtype, error) {
	// Check if context is canceled
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "kmod collection cancelled", err)
	}

	parser := file.NewParser()

	lines, err := parser.GetLines(filePathKMod)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to read kernel modules from %s", filePathKMod), err)
	}

	readings := make(map[string]measurement.Reading)

	for _, line := range lines {
		// Module name is the first field (space-separated)
		fields := strings.Fields(line)
		if len(fields) > 0 {
			readings[fields[0]] = measurement.Bool(true)
		}
	}

	res := &measurement.Subtype{
		Name: "kmod",
		Data: readings,
	}

	return res, nil
}
