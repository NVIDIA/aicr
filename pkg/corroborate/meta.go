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
	stderrors "errors"
	"io"
	"os"
	"syscall"

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

// readBoundedFile opens path and reads up to maxRunFileBytes, rejecting
// larger files instead of allocating them (os.ReadFile would OOM on an
// attacker-influenced path such as a /proc symlink or a network mount).
// Shared by the meta/CTRF readers.
//
// It refuses to follow symlinks and to read non-regular files (FIFOs, devices,
// sockets): a hostile input tree could otherwise point a meta.json at /proc or
// /dev to read unintended content or block on a pipe. The open is atomic with
// the symlink check (O_NOFOLLOW) so there is no window between an Lstat and the
// open in which the final path component could be swapped for a symlink (TOCTOU);
// the regular-file check then validates the opened descriptor itself, not the
// path. It preserves the open error classification (not-found vs symlink vs
// permission/other) so callers can tell a missing run apart from an I/O failure.
func readBoundedFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // O_NOFOLLOW rejects symlinks atomically; descriptor validated below; operator-supplied input tree / allowlist
	if err != nil {
		switch {
		case os.IsNotExist(err):
			return nil, errors.Wrap(errors.ErrCodeNotFound, "open "+path, err)
		case stderrors.Is(err, syscall.ELOOP):
			// O_NOFOLLOW on a symlink final component fails with ELOOP.
			return nil, errors.New(errors.ErrCodeInvalidRequest, path+" is a symlink (refusing to follow)")
		default:
			return nil, errors.Wrap(errors.ErrCodeInternal, "open "+path, err)
		}
	}
	defer func() { _ = f.Close() }()

	fi, err := f.Stat()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "stat "+path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New(errors.ErrCodeInvalidRequest, path+" is not a regular file")
	}

	data, err := io.ReadAll(io.LimitReader(f, int64(maxRunFileBytes)+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "read "+path, err)
	}
	if int64(len(data)) > maxRunFileBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest, path+" exceeds size limit")
	}
	return data, nil
}

// readRunMeta parses a meta.json file.
func readRunMeta(path string) (*RunMeta, error) {
	data, err := readBoundedFile(path)
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
	data, err := readBoundedFile(path)
	if err != nil {
		return nil, err
	}
	var r ctrf.Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "parse ctrf "+path, err)
	}
	return &r, nil
}
