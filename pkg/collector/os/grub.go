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

package os

import (
	"context"
	"fmt"

	"github.com/NVIDIA/eidos/pkg/collector/file"
	"github.com/NVIDIA/eidos/pkg/errors"
	"github.com/NVIDIA/eidos/pkg/measurement"
)

var (
	filePathGrub    = "/proc/cmdline"
	fileLineDelGrub = " "
	fileKVDelGrub   = "="

	// Keys to filter out from GRUB config for privacy/security
	filterOutGrubKeys = []string{
		"root",
	}
)

// collectGRUB retrieves the GRUB bootloader parameters from /proc/cmdline
// and returns them as a subtype with key-value pairs for each boot parameter.
func (c *Collector) collectGRUB(ctx context.Context) (*measurement.Subtype, error) {
	// Check if context is canceled
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	parser := file.NewParser(
		file.WithDelimiter(fileLineDelGrub),
		file.WithKVDelimiter(fileKVDelGrub),
	)

	params, err := parser.GetMap(filePathGrub)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to read GRUB params from %s", filePathGrub), err)
	}

	props := make(map[string]measurement.Reading)

	for k, v := range params {
		props[k] = measurement.Str(v)
	}

	res := &measurement.Subtype{
		Name: "grub",
		Data: measurement.FilterOut(props, filterOutGrubKeys),
	}

	return res, nil
}
