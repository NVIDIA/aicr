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

package corroborate

import (
	"encoding/json"
	"io"
	"os"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/validator/ctrf"
)

// RunMetaSchemaVersion is the meta.json schema GP4 reads (Contract 3, written
// by the GP2 ingest job).
const RunMetaSchemaVersion = "aicr-corroboration-meta/v1"

// maxRunFileBytes bounds each meta.json / ctrf/<phase>.json read. CTRF reports
// for a single recipe phase are small; anything larger is malformed or hostile.
const maxRunFileBytes = 16 << 20 // 16 MiB

// RunMeta is the verified, per-run metadata GP2 writes beside ctrf/ (Contract
// 3). Every field is sourced from the verified bundle predicate / snapshot —
// never the publish clock and never a free-text pointer field.
type RunMeta struct {
	SchemaVersion string            `json:"schemaVersion"`
	Coordinate    RunMetaCoordinate `json:"coordinate"`
	Recipe        string            `json:"recipe"`
	Signer        RunMetaSigner     `json:"signer"`
	RunID         string            `json:"runId"`
	AICRVersion   string            `json:"aicrVersion"`
	K8sVersion    string            `json:"k8sVersion"`
	K8sConstraint string            `json:"k8sConstraint"`
	BundleDigest  string            `json:"bundleDigest"`
	EvidenceRef   string            `json:"evidenceRef"`
	RekorLogIndex *int64            `json:"rekorLogIndex,omitempty"`
	AttestedAt    string            `json:"attestedAt"`
}

// RunMetaCoordinate is the GP2-derived coordinate carried in meta.json. GP4
// re-verifies it against pkg/recipe.CoordinateFor on the inverted criteria.
type RunMetaCoordinate struct {
	Group     string `json:"group"`
	Dashboard string `json:"dashboard"`
	Tab       string `json:"tab"`
}

// RunMetaSigner is the verified signer identity and its derived class.
type RunMetaSigner struct {
	IDHash      string `json:"idHash"`
	Identity    string `json:"identity"`
	Issuer      string `json:"issuer"`
	Class       string `json:"class"`
	Allowlisted bool   `json:"allowlisted"`
}

// readBoundedFile opens path and reads up to maxBytes, rejecting larger files
// instead of allocating them (os.ReadFile would OOM on an attacker-influenced
// path such as a /proc symlink or a network mount). Shared by the meta/CTRF
// readers and the allowlist loader.
func readBoundedFile(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // path is operator-supplied (input tree / allowlist)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, "open "+path, err)
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "read "+path, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest, path+" exceeds size limit")
	}
	return data, nil
}

// readRunMeta parses a meta.json file.
func readRunMeta(path string) (*RunMeta, error) {
	data, err := readBoundedFile(path, maxRunFileBytes)
	if err != nil {
		return nil, err
	}
	var m RunMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "parse meta "+path, err)
	}
	return &m, nil
}

// readCTRF parses a ctrf/<phase>.json file.
func readCTRF(path string) (*ctrf.Report, error) {
	data, err := readBoundedFile(path, maxRunFileBytes)
	if err != nil {
		return nil, err
	}
	var r ctrf.Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "parse ctrf "+path, err)
	}
	return &r, nil
}
