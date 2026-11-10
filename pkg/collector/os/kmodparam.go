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
	stderrors "errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/NVIDIA/aicr/pkg/collector/file"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

// SubtypeKModParam is the OS subtype holding allowlisted kernel module
// parameters, keyed by their /sys/module path.
const SubtypeKModParam = "kmod-param"

// kmodParamPaths is the allowlist of module parameters collectKModParam
// reads. /sys/module is global to the host kernel, so a container's
// read-only sysfs sees the host's values without a host mount.
//
// ib_core's netns_mode is the RDMA subsystem's live network namespace mode
// ("Y" shared, "N" exclusive): `rdma system set netns` changes the same
// variable.
var kmodParamPaths = []string{
	"/sys/module/ib_core/parameters/netns_mode",
}

// collectKModParam reads each allowlisted module parameter. A parameter whose
// module is not loaded has no file and is omitted, so a constraint on it
// reads as unavailable rather than as a value.
func (c *Collector) collectKModParam(ctx context.Context) (*measurement.Subtype, error) {
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "kmod-param collection cancelled", err)
	}

	parser := file.NewParser()
	readings := make(map[string]measurement.Reading, len(kmodParamPaths))
	for _, path := range kmodParamPaths {
		lines, err := readHostFile(ctx, path, parser.GetLines)
		if err != nil {
			if stderrors.Is(err, fs.ErrNotExist) {
				continue
			}
			if errors.IsTransient(err) {
				return nil, err
			}
			return nil, errors.Wrap(errors.ErrCodeInternal,
				fmt.Sprintf("failed to read module parameter %s", path), err)
		}
		readings[path] = measurement.Str(strings.Join(lines, "\n"))
	}

	return &measurement.Subtype{Name: SubtypeKModParam, Data: readings}, nil
}
