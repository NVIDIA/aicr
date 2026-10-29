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

package aicrjob

import (
	"encoding/json"
)

// Snapshot identity at AICR v1.0.0.
const (
	SnapshotKind       = "Snapshot"
	SnapshotAPIVersion = "aicr.run/v1"
)

// Snapshot is the subset of AICR's Snapshot the operator reads: identity,
// measurements and the fingerprint. The fingerprint is AICR's own derivation
// of the criteria dimensions from the measurements, with the measurement
// each came from; the operator checks completeness and conflicts against it
// and never derives criteria itself.
type Snapshot struct {
	APIVersion   string            `json:"apiVersion"`
	Kind         string            `json:"kind"`
	Metadata     map[string]string `json:"metadata"`
	Measurements []Measurement     `json:"measurements"`
	Fingerprint  *Fingerprint      `json:"fingerprint,omitempty"`
}

// Fingerprint is the criteria AICR derived from a snapshot's measurements.
type Fingerprint struct {
	Service     Dimension `json:"service"`
	Accelerator Dimension `json:"accelerator"`
	OS          Dimension `json:"os"`
}

// Dimension is one derived criteria dimension. Value is empty when no
// measurement backs it; Source names the measurement; Note explains an
// empty value, such as "multi-gpu" or "unknown-sku".
type Dimension struct {
	Value  string `json:"value,omitempty"`
	Source string `json:"source,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Fingerprint notes AICR v1.0.0 writes for an accelerator it cannot name.
const (
	NoteMultiGPU   = "multi-gpu"
	NoteUnknownSKU = "unknown-sku"
)

// Measurement is one collector's output.
type Measurement struct {
	Type     string    `json:"type"`
	Subtypes []Subtype `json:"subtypes,omitempty"`
}

// Subtype is one named group of readings. Readings are scalars; the operator
// keeps them raw and interprets only the ones it checks.
type Subtype struct {
	Name    string                     `json:"subtype,omitempty"`
	Data    map[string]json.RawMessage `json:"data,omitempty"`
	Context map[string]string          `json:"context,omitempty"`
}

// snapshotRequired are the fields the operator cannot work without:
// AICR's schema requires measurements, and the version gate needs metadata.
var snapshotRequired = withHeader("measurements")

// ParseSnapshot decodes a Snapshot and applies the contract checks: a
// supported apiVersion and kind, the required fields and the version gate.
// Unknown fields are ignored. Completeness of the measurements behind each
// criteria dimension is the resolver's check.
func ParseSnapshot(data []byte, declaredVersion string) (*Snapshot, error) {
	if err := requireFields(data, SnapshotKind, snapshotRequired); err != nil {
		return nil, err
	}
	s := &Snapshot{}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, unsupported("decode Snapshot: %v", err)
	}
	if s.Kind != SnapshotKind || s.APIVersion != SnapshotAPIVersion {
		return nil, unsupported("unsupported artifact %s %s", s.APIVersion, s.Kind)
	}
	if err := CheckArtifactVersion(declaredVersion, s.Metadata["version"]); err != nil {
		return nil, err
	}
	for i := range s.Measurements {
		if s.Measurements[i].Type == "" {
			return nil, unsupported("Snapshot measurement %d has no type", i)
		}
	}
	return s, nil
}
