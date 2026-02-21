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
// with three subtypes: grub, kmod, and sysctl.
func (c *Collector) Collect(ctx context.Context) (*measurement.Measurement, error) {
	slog.Info("collecting OS configuration")

	ctx, cancel := context.WithTimeout(ctx, defaults.CollectorTimeout)
	defer cancel()

	// Check if context is canceled
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "OS collector context cancelled", err)
	}

	grub, err := c.collectGRUB(ctx)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to collect GRUB configuration", err)
	}

	sysctl, err := c.collectSysctl(ctx)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to collect sysctl configuration", err)
	}

	kmod, err := c.collectKMod(ctx)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to collect kernel modules", err)
	}

	release, err := c.collectRelease(ctx)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to collect OS release", err)
	}

	res := &measurement.Measurement{
		Type: measurement.TypeOS,
		Subtypes: []measurement.Subtype{
			*grub,
			*sysctl,
			*kmod,
			*release,
		},
	}

	return res, nil
}
