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
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

// Collector collects operating system configuration including:
// - GRUB bootloader parameters from /proc/cmdline
// - Loaded kernel modules from /proc/modules
// - Sysctl parameters from /proc/sys
type Collector struct {
}

// Collect gathers all OS-level configurations and returns them as a single measurement
// with subtypes: grub, sysctl, kmod, and release.
// Individual sub-collectors degrade gracefully — if any sub-collector fails,
// a warning is logged and that subtype is skipped.
func (c *Collector) Collect(ctx context.Context) (*measurement.Measurement, error) {
	slog.Info("collecting OS configuration")

	ctx, cancel := context.WithTimeout(ctx, defaults.CollectorTimeout)
	defer cancel()

	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "OS collector context cancelled", err)
	}

	type subCollector struct {
		name string
		fn   func(context.Context) (*measurement.Subtype, error)
	}

	collectors := []subCollector{
		{"grub", c.collectGRUB},
		{"sysctl", c.collectSysctl},
		{"kmod", c.collectKMod},
		{"release", c.collectRelease},
	}

	subtypes := make([]measurement.Subtype, 0, len(collectors))

	for _, sc := range collectors {
		result, err := sc.fn(ctx)
		if err != nil {
			slog.Warn("failed to collect "+sc.name+" - skipping",
				slog.String("collector", sc.name),
				slog.String("error", err.Error()))
			continue
		}
		subtypes = append(subtypes, *result)
	}

	return &measurement.Measurement{
		Type:     measurement.TypeOS,
		Subtypes: subtypes,
	}, nil
}
