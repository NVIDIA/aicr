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

package systemd

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/NVIDIA/eidos/pkg/errors"
	"github.com/NVIDIA/eidos/pkg/measurement"
	"github.com/coreos/go-systemd/v22/dbus"
)

var (
	// Keys to filter out from systemd properties for privacy/security or noise reduction
	filterOutSystemDKeys = []string{
		"AllowedCPUs",
		"AllowedMemoryNodes",
		"Asserts",
		"BPFProgram",
		"BusName",
		"Id",
		"*Credential*",
	}
)

// Collector is a collector that gathers configuration data from systemd services.
type Collector struct {
	Services []string
}

// Collect gathers configuration data from specified systemd services.
// It implements the Collector interface.
// If D-Bus is not available (e.g., on macOS, Windows, or minimal containers),
// it returns an empty measurement instead of failing.
func (s *Collector) Collect(ctx context.Context) (*measurement.Measurement, error) {
	slog.Info("collecting SystemD service configurations")

	services := s.Services
	if len(services) == 0 {
		services = []string{"containerd.service"}
	}
	subs := make([]measurement.Subtype, 0, len(services))

	conn, err := dbus.NewSystemdConnectionContext(ctx)
	if err != nil {
		slog.Warn("D-Bus not available - no systemd data will be collected",
			slog.String("error", err.Error()),
			slog.String("hint", "systemd/D-Bus is required for service status collection"))
		return noSystemDMeasurement(), nil
	}
	defer conn.Close()

	for _, service := range services {
		data, err := conn.GetAllPropertiesContext(ctx, service)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, fmt.Sprintf("failed to get unit properties for %s", service), err)
		}

		readings := make(map[string]measurement.Reading)
		for k, v := range data {
			readings[k] = measurement.ToReading(v)
		}

		subs = append(subs, measurement.Subtype{
			Name: service,
			Data: measurement.FilterOut(readings, filterOutSystemDKeys),
		})
	}

	res := &measurement.Measurement{
		Type:     measurement.TypeSystemD,
		Subtypes: subs,
	}

	return res, nil
}

// noSystemDMeasurement returns a valid measurement indicating no systemd data
// is available. This is used for graceful degradation when D-Bus is not accessible.
func noSystemDMeasurement() *measurement.Measurement {
	return &measurement.Measurement{
		Type:     measurement.TypeSystemD,
		Subtypes: []measurement.Subtype{},
	}
}
