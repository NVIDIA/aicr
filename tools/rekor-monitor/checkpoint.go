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
	"strconv"
	"strings"

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
	if err := extractCheckpointFromZip(ctx, s.restoreZip, s.path); err != nil {
		return err
	}
	// The scan-progress companion is optional: an artifact written before this
	// feature shipped (or a first run) has only the checkpoint, and its absence
	// means "start the window from the beginning" (scannedTo = 0).
	if err := extractProgressFromZip(ctx, s.restoreZip, s.progressPath()); err != nil {
		return err
	}
	// The scan-trend companion is likewise optional; its absence means "no prior
	// convergence history" (remaining = 0, stallCount = 0).
	return extractStallFromZip(ctx, s.restoreZip, s.stallPath())
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

// progressPath is the companion file holding scannedTo (the last log index the
// identity scan has completed within the current [checkpoint, head] window). It
// lets the scan resume across runs so a large backlog is caught up incrementally
// rather than re-scanned (and timed out) from scratch every pass. Kept beside
// the signed checkpoint so both travel in the same artifact.
func (s checkpointStore) progressPath() string { return s.path + ".scan" }

// readProgress returns the persisted scannedTo, or 0 when there is none (a fresh
// window, or the first run after this feature ships: absent companion degrades
// to a full-window scan). A present-but-unparseable file is an error rather than
// a silent reset, which would re-scan (and possibly re-time-out) the backlog.
func (s checkpointStore) readProgress() (int64, error) {
	data, err := os.ReadFile(s.progressPath()) //nolint:gosec // path is a workflow-controlled constant
	if err != nil {
		if stderrors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, errors.Wrap(errors.ErrCodeInternal, "failed to read scan-progress file", err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, errors.Wrap(errors.ErrCodeInternal, "failed to parse scan-progress file", err)
	}
	if n < 0 {
		return 0, errors.New(errors.ErrCodeInternal, "scan-progress index is negative")
	}
	return n, nil
}

// writeProgress persists scannedTo as a single decimal integer. It creates the
// parent directory first: the mid-scan chunk save can be the first write on a
// fresh run (before any checkpoint write that would otherwise create it), so a
// nested --file path whose directory does not yet exist must not fail the pass.
func (s checkpointStore) writeProgress(scannedTo int64) error {
	line := strconv.FormatInt(scannedTo, 10) + "\n"
	if err := os.MkdirAll(filepath.Dir(s.progressPath()), 0o750); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create scan-progress directory", err)
	}
	//nolint:gosec // progressPath derives from the workflow-controlled checkpoint path
	if err := os.WriteFile(s.progressPath(), []byte(line), 0o600); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write scan-progress file", err)
	}
	return nil
}

// stallPath is the companion tracking catch-up convergence across runs (see
// scanTrend for the fields). It lets observe distinguish a converging catch-up
// (remaining trending down: healthy) from a diverging or glacial one (the log
// outpacing the scan) so the latter can page instead of reporting clean
// indefinitely. Carried in the same artifact as the checkpoint.
func (s checkpointStore) stallPath() string { return s.path + ".stall" }

// scanTrend is the persisted catch-up convergence state (the .stall companion):
//   - bestRemaining: the smallest `remaining` seen in this window so far (a
//     low-water mark). Comparing against the watermark rather than the prior pass
//     means a single lucky decrease inside an oscillating divergence cannot reset
//     the stall count.
//   - stall: consecutive partial passes that failed to beat bestRemaining.
//   - passes: total partial passes in this window (an absolute "behind too long"
//     bound that trips even a glacial-but-monotone catch-up the watermark misses).
//
// All three reset when the window advances (resetScanTrend).
type scanTrend struct {
	bestRemaining int64
	stall         int
	passes        int
}

// readScanTrend returns the persisted trend, or the zero value when the companion
// is absent (a fresh window, or the first run after this shipped). A two-field
// file (the pre-passes format) is accepted with passes defaulting to 0. A
// present-but-malformed file is an error rather than a silent reset, so a bad
// companion never masks a real divergence.
func (s checkpointStore) readScanTrend() (scanTrend, error) {
	data, rerr := os.ReadFile(s.stallPath()) //nolint:gosec // path is a workflow-controlled constant
	if rerr != nil {
		if stderrors.Is(rerr, os.ErrNotExist) {
			return scanTrend{}, nil
		}
		return scanTrend{}, errors.Wrap(errors.ErrCodeInternal, "failed to read scan-trend file", rerr)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return scanTrend{}, nil
	}
	if len(fields) != 2 && len(fields) != 3 {
		return scanTrend{}, errors.New(errors.ErrCodeInternal, "scan-trend file is malformed")
	}
	best, perr := strconv.ParseInt(fields[0], 10, 64)
	if perr != nil || best < 0 {
		return scanTrend{}, errors.New(errors.ErrCodeInternal, "scan-trend best-remaining is invalid")
	}
	stall, perr := strconv.Atoi(fields[1])
	if perr != nil || stall < 0 {
		return scanTrend{}, errors.New(errors.ErrCodeInternal, "scan-trend stall count is invalid")
	}
	passes := 0
	if len(fields) == 3 {
		passes, perr = strconv.Atoi(fields[2])
		if perr != nil || passes < 0 {
			return scanTrend{}, errors.New(errors.ErrCodeInternal, "scan-trend pass count is invalid")
		}
	}
	return scanTrend{bestRemaining: best, stall: stall, passes: passes}, nil
}

// writeScanTrend persists the trend as three space-separated integers.
func (s checkpointStore) writeScanTrend(t scanTrend) error {
	line := strconv.FormatInt(t.bestRemaining, 10) + " " + strconv.Itoa(t.stall) + " " + strconv.Itoa(t.passes) + "\n"
	if err := os.MkdirAll(filepath.Dir(s.stallPath()), 0o750); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to create scan-trend directory", err)
	}
	//nolint:gosec // stallPath derives from the workflow-controlled checkpoint path
	if err := os.WriteFile(s.stallPath(), []byte(line), 0o600); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write scan-trend file", err)
	}
	return nil
}

// resetScanTrend clears the trend companion, so a fresh window reads (0, 0). Kept
// separate from writeScanTrend so an advance path expresses intent ("no history")
// rather than writing a magic zero pair.
func (s checkpointStore) resetScanTrend() error {
	if err := os.Remove(s.stallPath()); err != nil && !stderrors.Is(err, os.ErrNotExist) {
		return errors.Wrap(errors.ErrCodeInternal, "failed to reset scan-trend file", err)
	}
	return nil
}

// openArtifactZip stat-checks, size-bounds, and opens the checkpoint artifact
// zip, shared by the checkpoint and scan-progress extractors. The oversize
// rejection happens before opening because zip.OpenReader and the member scan
// both walk the whole archive, so the per-entry cap is too late to bound that
// work. The caller must Close the returned reader.
func openArtifactZip(zipPath string) (*zip.ReadCloser, error) {
	fi, err := os.Stat(zipPath)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to stat checkpoint artifact zip", err)
	}
	if fi.Size() > maxArchiveBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "checkpoint artifact zip exceeds size limit")
	}
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to open checkpoint artifact zip", err)
	}
	return r, nil
}

// extractCheckpointFromZip writes the checkpoint entry from a GitHub-artifact
// zip to destPath. GitHub serves artifacts as zip via the REST API, so we read
// them natively rather than shelling out to `unzip`. It selects the entry whose
// base name matches destPath, or the sole entry if the archive has exactly one.
func extractCheckpointFromZip(ctx context.Context, zipPath, destPath string) error {
	r, err := openArtifactZip(zipPath)
	if err != nil {
		return err
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

// maxProgressBytes bounds the scan-progress companion read from an artifact zip.
// It holds a single decimal integer; the cap guards a corrupt/malicious member.
const maxProgressBytes = 4 << 10 // 4 KiB

// maxStallBytes bounds the scan-trend companion (two small integers).
const maxStallBytes = 4 << 10 // 4 KiB

// extractProgressFromZip writes the scan-progress companion (base name of
// destPath) from the artifact zip to destPath, if present.
func extractProgressFromZip(ctx context.Context, zipPath, destPath string) error {
	return extractOptionalCompanionFromZip(ctx, zipPath, destPath, maxProgressBytes, "scan-progress")
}

// extractStallFromZip writes the scan-trend companion from the artifact zip to
// destPath, if present.
func extractStallFromZip(ctx context.Context, zipPath, destPath string) error {
	return extractOptionalCompanionFromZip(ctx, zipPath, destPath, maxStallBytes, "scan-trend")
}

// extractOptionalCompanionFromZip writes the entry whose base name matches
// destPath from the artifact zip to destPath, if present. Unlike the checkpoint,
// a companion is OPTIONAL: a missing entry is a no-op (not an error), because
// artifacts written before a given companion shipped contain only the checkpoint.
// It matches by exact base name only (no sole-entry fallback), so it never
// mistakes the checkpoint (or another companion) for this one. label names the
// companion in error messages; maxBytes bounds a corrupt/malicious member.
func extractOptionalCompanionFromZip(ctx context.Context, zipPath, destPath string, maxBytes int64, label string) error {
	r, err := openArtifactZip(zipPath)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()

	want := filepath.Base(destPath)
	var entry *zip.File
	for _, f := range r.File {
		if cerr := ctx.Err(); cerr != nil {
			return errors.Wrap(errors.ErrCodeTimeout, label+" extraction canceled", cerr)
		}
		if filepath.Base(f.Name) == want {
			entry = f
			break
		}
	}
	if entry == nil {
		return nil // optional: no companion in this artifact
	}

	rc, err := entry.Open()
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to open "+label+" zip entry", err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, maxBytes+1))
	if err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to read "+label+" zip entry", err)
	}
	if int64(len(data)) > maxBytes {
		return errors.New(errors.ErrCodeInvalidRequest, label+" zip entry exceeds size limit")
	}
	if err := os.WriteFile(destPath, data, 0o600); err != nil {
		return errors.Wrap(errors.ErrCodeInternal, "failed to write restored "+label, err)
	}
	return nil
}
