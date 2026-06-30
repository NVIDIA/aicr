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

package uatbroker

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"gopkg.in/yaml.v3"
)

// maxRegistryBytes bounds the registry file read so an attacker-influenced
// path cannot OOM the process; the registry is a small hand-edited file.
const maxRegistryBytes int64 = 1 << 20 // 1 MiB

// ParseRegistry parses and validates a reservations.yaml document.
func ParseRegistry(data []byte) (*Registry, error) {
	var reg Registry
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "parse reservation registry", err)
	}
	if err := reg.Validate(); err != nil {
		return nil, err
	}
	return &reg, nil
}

// LoadRegistryFile reads, size-bounds, parses, and validates the
// reservation registry at path.
func LoadRegistryFile(path string) (*Registry, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied registry path (CLI flag), size-bounded below
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "open reservation registry "+path, err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxRegistryBytes+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "read reservation registry "+path, err)
	}
	if int64(len(data)) > maxRegistryBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "reservation registry "+path+" exceeds size limit")
	}
	return ParseRegistry(data)
}

// Validate enforces registry invariants: at least one row; every row has the
// required fields; cloud is recognized; gpu-count is positive; and names are
// unique (the name is the lease key, so a duplicate would make the lease
// ambiguous).
func (r *Registry) Validate() error {
	if len(r.Reservations) == 0 {
		return errors.New(errors.ErrCodeInvalidRequest, "reservation registry has no reservations")
	}
	seen := make(map[string]bool, len(r.Reservations))
	for i := range r.Reservations {
		res := &r.Reservations[i]
		if strings.TrimSpace(res.Name) == "" {
			return errors.New(errors.ErrCodeInvalidRequest, fmt.Sprintf("reservation[%d] has an empty name", i))
		}
		if seen[res.Name] {
			return errors.New(errors.ErrCodeInvalidRequest, "duplicate reservation name "+res.Name)
		}
		seen[res.Name] = true
		if !validClouds[res.Cloud] {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("reservation %s has unknown cloud %q (want %s or %s)", res.Name, res.Cloud, CloudAWS, CloudGCP))
		}
		for _, f := range []struct{ key, val string }{
			{"reservation-id", res.ReservationID},
			{"accelerator", res.Accelerator},
			{"cluster-config-path", res.ClusterConfigPath},
			{"test-config-dir", res.TestConfigDir},
		} {
			if strings.TrimSpace(f.val) == "" {
				return errors.New(errors.ErrCodeInvalidRequest,
					fmt.Sprintf("reservation %s has an empty %s", res.Name, f.key))
			}
		}
		if res.GPUCount <= 0 {
			return errors.New(errors.ErrCodeInvalidRequest,
				fmt.Sprintf("reservation %s has a non-positive gpu-count (%d)", res.Name, res.GPUCount))
		}
	}
	return nil
}

// Lookup returns the reservation row with the given name, or an
// ErrCodeNotFound error when no row matches.
func (r *Registry) Lookup(name string) (*Reservation, error) {
	for i := range r.Reservations {
		if r.Reservations[i].Name == name {
			// Return a copy so callers cannot mutate the registry's internal
			// slice (and bypass Validate) through the returned pointer.
			res := r.Reservations[i]
			return &res, nil
		}
	}
	return nil, errors.New(errors.ErrCodeNotFound, "reservation "+name+" not found in registry")
}

// Names returns the reservation names in registry (document) order.
func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.Reservations))
	for i := range r.Reservations {
		names = append(names, r.Reservations[i].Name)
	}
	return names
}
