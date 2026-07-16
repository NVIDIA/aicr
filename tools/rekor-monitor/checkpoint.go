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

package main

import (
	"archive/zip"
	"context"
	stderrors "errors"
	"io"
	"os"
	"path/filepath"

	rmfile "github.com/sigstore/rekor-monitor/pkg/util/file"
	tlog "github.com/transparency-dev/formats/log"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// maxCheckpointBytes bounds a checkpoint restored from an artifact zip. A Rekor
// v2 checkpoint is ~100 bytes; the cap guards against a malicious or corrupt
// artifact inflating on extraction.
const maxCheckpointBytes = 1 << 20 // 1 MiB

// maxArchiveBytes bounds the checkpoint artifact zip itself, rejected before we
// even open it so a large or corrupt archive cannot burn memory/CPU in
// zip.OpenReader or the member scan. A real checkpoint artifact is a few hundred
// bytes.
const maxArchiveBytes = 10 << 20 // 10 MiB

// checkpointStore persists the monitor's cursor (the last verified checkpoint)
// across CI runs. The live checkpoint lives at path; restoreZip, when set, is a
// GitHub-artifact zip the prior checkpoint is seeded from before reading.
type checkpointStore struct {
	path       string
	restoreZip string
}

// restore seeds path from restoreZip when that artifact was fetched. A missing
// zip is the expected first-run state (no-op); a present-but-unusable zip is an
// error, so the cursor is never silently reset (which would stop the monitor
// from ever scanning identity).
func (s checkpointStore) restore(ctx context.Context) error {
	if s.restoreZip == "" {
		return nil
	}
	if _, err := os.Stat(s.restoreZip); err != nil {
		if stderrors.Is(err, os.ErrNotExist) {
			return nil // no prior artifact fetched: first run
		}
		return errors.Wrap(errors.ErrCodeInternal, "failed to stat checkpoint artifact zip", err)
	}
	return extractCheckpointFromZip(ctx, s.restoreZip, s.path)
}

// read returns the persisted checkpoint, or nil when there is none yet. A
// missing or empty file is the "first run" signal (baseline only), not an error.
func (s checkpointStore) read() (*tlog.Checkpoint, error) {
	fi, err := os.Stat(s.path)
	switch {
	case stderrors.Is(err, os.ErrNotExist):
		return nil, nil //nolint:nilnil // first run: no checkpoint file yet
	case err != nil:
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to stat checkpoint file", err)
	case fi.Size() == 0:
		return nil, nil //nolint:nilnil // first run: empty checkpoint file
	}
	cp, err := rmfile.ReadLatestCheckpointRekorV2(s.path)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read checkpoint file", err)
	}
	return cp, nil
}

// write persists cur as the new cursor.
func (s checkpointStore) write(prev, cur *tlog.Checkpoint) error {
	if err := rmfile.WriteCheckpointRekorV2(cur, prev, s.path, false); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write checkpoint", err)
	}
	return nil
}

// extractCheckpointFromZip writes the checkpoint entry from a GitHub-artifact
// zip to destPath. GitHub serves artifacts as zip via the REST API, so we read
// them natively rather than shelling out to `unzip`. It selects the entry whose
// base name matches destPath, or the sole entry if the archive has exactly one.
func extractCheckpointFromZip(ctx context.Context, zipPath, destPath string) error {
	// Reject an oversized archive before opening it: zip.OpenReader and the
	// member scan both walk the whole archive, so the per-entry cap below is too
	// late to bound that work.
	fi, err := os.Stat(zipPath)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to stat checkpoint artifact zip", err)
	}
	if fi.Size() > maxArchiveBytes {
		return errors.New(errors.ErrCodeInvalidRequest, "checkpoint artifact zip exceeds size limit")
	}

	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "failed to open checkpoint artifact zip", err)
	}
	defer func() { _ = r.Close() }()

	entry, err := selectCheckpointEntry(ctx, r.File, filepath.Base(destPath))
	if err != nil {
		return err
	}
	if entry == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "checkpoint artifact zip did not contain "+filepath.Base(destPath))
	}

	rc, err := entry.Open()
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to open checkpoint zip entry", err)
	}
	defer func() { _ = rc.Close() }()

	data, err := io.ReadAll(io.LimitReader(rc, maxCheckpointBytes+1))
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to read checkpoint zip entry", err)
	}
	switch {
	case int64(len(data)) > maxCheckpointBytes:
		return errors.New(errors.ErrCodeInvalidRequest, "checkpoint zip entry exceeds size limit")
	case len(data) == 0:
		return errors.New(errors.ErrCodeInvalidRequest, "checkpoint zip entry is empty")
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create checkpoint directory", err)
	}
	if err := os.WriteFile(destPath, data, 0o600); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write restored checkpoint", err)
	}
	return nil
}

// selectCheckpointEntry picks the zip entry whose base name matches want, or the
// sole entry when the archive has exactly one file. It returns (nil, nil) when
// neither applies, and a non-nil error if ctx is canceled while walking the
// (attacker-influenceable) member list.
func selectCheckpointEntry(ctx context.Context, files []*zip.File, want string) (*zip.File, error) {
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, errors.Wrap(errors.ErrCodeTimeout, "checkpoint extraction canceled", err)
		}
		if filepath.Base(f.Name) == want {
			return f, nil
		}
	}
	if len(files) == 1 {
		return files[0], nil
	}
	return nil, nil //nolint:nilnil // no matching entry is a valid "not found", not an error
}
