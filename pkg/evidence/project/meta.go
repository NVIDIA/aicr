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

package project

import (
	"encoding/json"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// MetaSchemaVersion is the schema constant the GP4 corroborate consumer
// expects in every meta.json. A mismatch only warns there, but the
// producer always writes the current version.
const MetaSchemaVersion = "aicr-corroboration-meta/v1"

// MetaFilename is the bundle-relative name of the synthesized metadata
// file in each run directory.
const MetaFilename = "meta.json"

// Coordinate is the recipe's placement in the navigation space, mirrored
// from recipe.Coordinate so meta.json carries the canonical segments the
// consumer round-trips through recipe.CoordinateFor.
type Coordinate struct {
	Group     string `json:"group"`
	Dashboard string `json:"dashboard"`
	Tab       string `json:"tab"`
}

// Signer is the verified-signer block of meta.json. Every field is
// derived from the cryptographically verified certificate/Rekor entry,
// never from an unverified pointer claim. IDHash is the source-dedup
// key (see SignerIDHash); Identity drives the display label in the grid.
type Signer struct {
	IDHash      string `json:"idHash"`
	Identity    string `json:"identity"`
	Issuer      string `json:"issuer"`
	Class       Class  `json:"class"`
	Allowlisted bool   `json:"allowlisted"`
}

// Meta is the synthesized per-run metadata the GP4 corroborate consumer
// reads. It is the authoritative coordinate source — the directory
// layout is convention only. Field order is fixed so the JSON encoding
// is deterministic across runs from identical input.
type Meta struct {
	SchemaVersion string     `json:"schemaVersion"`
	Coordinate    Coordinate `json:"coordinate"`
	Recipe        string     `json:"recipe"`
	Signer        Signer     `json:"signer"`
	RunID         string     `json:"runId"`
	AICRVersion   string     `json:"aicrVersion"`
	K8sVersion    string     `json:"k8sVersion"`
	K8sConstraint string     `json:"k8sConstraint"`
	BundleDigest  string     `json:"bundleDigest"`
	EvidenceRef   string     `json:"evidenceRef"`
	RekorLogIndex *int64     `json:"rekorLogIndex,omitempty"`
	AttestedAt    string     `json:"attestedAt"`
}

// MarshalDeterministic renders meta.json bytes that are byte-identical
// across runs from identical input. Meta is a flat struct with no Go
// maps, so encoding/json emits fields in declaration order; a trailing
// newline matches the conventional on-disk form.
func (m *Meta) MarshalDeterministic() ([]byte, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "marshal meta.json", err)
	}
	return append(data, '\n'), nil
}
