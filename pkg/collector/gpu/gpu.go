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

package gpu

import (
	"context"
	"log/slog"
	"time"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

// CollectorOption configures a Collector.
type CollectorOption func(*Collector)

// WithHardwareDetector sets the hardware detector used for GPU detection.
// When not set, Collect returns a GPU measurement with no subtypes.
func WithHardwareDetector(d HardwareDetector) CollectorOption {
	return func(c *Collector) {
		c.hardwareDetector = d
	}
}

// NewCollector creates a GPU collector with the given options.
func NewCollector(opts ...CollectorOption) *Collector {
	c := &Collector{}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Collector collects GPU information via driver-free NFD/PCI enumeration:
// presence, count, kernel-module state, and the accelerator SKU resolved from
// the PCI device ID. It requires neither the NVIDIA driver nor nvidia-smi.
type Collector struct {
	// hardwareDetector provides GPU detection via PCI enumeration. When nil,
	// Collect returns a GPU measurement with no subtypes.
	hardwareDetector HardwareDetector
}

// subtypeHardware is the measurement subtype name for NFD-based hardware detection.
const subtypeHardware = "hardware"

// Collect retrieves GPU information from the hardware detector (NFD PCI
// enumeration). It degrades gracefully: when no detector is configured or
// detection fails, it returns a GPU measurement with no subtypes rather than
// an error, so a snapshot on a node without sysfs (e.g. macOS) still succeeds.
func (s *Collector) Collect(ctx context.Context) (*measurement.Measurement, error) {
	slog.Info("collecting GPU information")

	// Use parent context deadline if it's sooner than our default timeout.
	deadline, ok := ctx.Deadline()
	timeout := defaults.CollectorTimeout
	if ok {
		if remaining := time.Until(deadline); remaining < timeout && remaining > 0 {
			timeout = remaining
		}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "GPU collection cancelled", err)
	}

	var subtypes []measurement.Subtype
	if s.hardwareDetector != nil {
		info, err := s.hardwareDetector.Detect(ctx)
		if err != nil {
			slog.Warn("GPU hardware detection failed",
				slog.String("error", err.Error()))
		} else if info != nil {
			subtypes = append(subtypes, hardwareSubtype(info))
		}
	}

	return &measurement.Measurement{
		Type:     measurement.TypeGPU,
		Subtypes: subtypes,
	}, nil
}

// hardwareSubtype converts HardwareInfo into a measurement subtype.
func hardwareSubtype(info *HardwareInfo) measurement.Subtype {
	data := map[string]measurement.Reading{
		measurement.KeyGPUPresent:         measurement.Bool(info.GPUPresent),
		measurement.KeyGPUCount:           measurement.Int(info.GPUCount),
		measurement.KeyGPUDriverLoaded:    measurement.Bool(info.DriverLoaded),
		measurement.KeyGPUDetectionSource: measurement.Str(info.DetectionSource),
	}
	// Only emit the model when the device ID resolved to a known SKU; an
	// empty value would otherwise read as "detected but blank."
	if info.SKU != "" {
		data[measurement.KeyGPUModel] = measurement.Str(info.SKU)
	}
	return measurement.Subtype{
		Name: subtypeHardware,
		Data: data,
	}
}
