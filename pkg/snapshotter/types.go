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

package snapshotter

import (
	"github.com/NVIDIA/aicr/pkg/fingerprint"
	"github.com/NVIDIA/aicr/pkg/header"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

// NewSnapshot creates a new Snapshot instance with an initialized Measurements slice.
func NewSnapshot() *Snapshot {
	return &Snapshot{
		Measurements: make([]*measurement.Measurement, 0),
	}
}

// Snapshot represents a collected configuration snapshot from a system node.
// It contains metadata and measurements from various collectors including
// Kubernetes, GPU, OS configuration, and systemd services.
type Snapshot struct {
	header.Header `json:",inline" yaml:",inline"`

	// Fingerprint is a structured cluster identity derived from the
	// raw measurements: detected service, accelerator, OS,
	// Kubernetes server version, region, and node count. Populated
	// after all collectors finish so it reflects the final
	// measurement set.
	//
	// The embedded Fingerprint is advisory: it is a convenience for
	// humans reading the snapshot file, not an authoritative claim.
	// Consumers of the snapshot that bear trust — notably the
	// ADR-007 bundler when building the predicate body and the
	// evidence verifier when re-checking it — MUST recompute the
	// Fingerprint from Measurements via fingerprint.FromMeasurements
	// rather than read this field. The snapshot YAML is not signed
	// at this layer; an attacker controlling the file could swap
	// the embedded Fingerprint without touching the measurements
	// that back it.
	Fingerprint *fingerprint.Fingerprint `json:"fingerprint,omitempty" yaml:"fingerprint,omitempty"`

	// Measurements contains the collected measurements from various collectors.
	Measurements []*measurement.Measurement `json:"measurements" yaml:"measurements"`
}
