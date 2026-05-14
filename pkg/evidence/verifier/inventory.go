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

package verifier

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// CheckInventory verifies the bundle's integrity chain:
//
//  1. sha256(manifest.json) matches expectedManifestDigest (the
//     predicate's Manifest.Digest field). This is what binds the
//     unsigned manifest to the predicate — without it, a tampered
//     bundle could rewrite manifest.json to match its own contents and
//     pass file-by-file hash checks.
//  2. Every file the manifest names exists, has the expected size,
//     and hashes to the recorded sha256.
//  3. No file in the bundle is unmanaged (i.e., not in the manifest).
//
// expectedManifestDigest must be the "sha256:<hex>" form from
// pred.Manifest.Digest. An empty value is rejected — the verifier
// refuses to operate without a predicate-side digest to compare against.
//
// ctx is honored between files (large bundles, hostile manifests with
// many entries) and during the bundle walk for stray-file detection.
//
// Returns per-file mismatch rows and an error summarizing the failure;
// both nil on success.
func CheckInventory(ctx context.Context, mat *MaterializedBundle, expectedManifestDigest string) ([]KV, error) {
	if mat == nil || mat.BundleDir == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "materialized bundle is required")
	}
	if expectedManifestDigest == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"expected manifest digest is required (from predicate.manifest.digest)")
	}
	body, err := os.ReadFile(filepath.Join(mat.BundleDir, attestation.ManifestFilename)) //nolint:gosec
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, "failed to read manifest.json", err)
	}
	gotManifestDigest := attestation.HashBytesSHA256(body)
	if gotManifestDigest != expectedManifestDigest {
		return []KV{{Key: attestation.ManifestFilename,
				Value: "sha256 mismatch (got " + gotManifestDigest +
					", want " + expectedManifestDigest + ")"}},
			errors.New(errors.ErrCodeInvalidRequest,
				"manifest.json digest does not match predicate.manifest.digest — "+
					"the manifest has been tampered or the predicate is wrong for this bundle")
	}

	var manifest attestation.Manifest
	if uErr := json.Unmarshal(body, &manifest); uErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "manifest.json is not valid JSON", uErr)
	}
	if !isSupportedManifestSchema(manifest.SchemaVersion) {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"unsupported manifest schemaVersion "+manifest.SchemaVersion+" (verifier supports 1.0.x)")
	}
	if len(manifest.Files) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "manifest.json has no files")
	}

	var mismatches []KV
	for _, e := range manifest.Files {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return mismatches, errors.Wrap(errors.ErrCodeUnavailable,
				"inventory check canceled", ctxErr)
		}
		got, hashErr := hashFile(mat.BundleDir, e.Path, e.Size)
		if hashErr != nil {
			mismatches = append(mismatches, KV{Key: e.Path, Value: hashErr.Error()})
			continue
		}
		want := strings.TrimPrefix(e.SHA256, "sha256:")
		if got != want {
			mismatches = append(mismatches, KV{
				Key:   e.Path,
				Value: "sha256 mismatch (got " + got + ", want " + want + ")",
			})
		}
	}

	extras, walkErr := findExtras(ctx, mat.BundleDir, manifest.Files)
	if walkErr != nil {
		mismatches = append(mismatches, KV{Key: "walk", Value: walkErr.Error()})
	}
	for _, p := range extras {
		mismatches = append(mismatches, KV{Key: p, Value: "file not in manifest.json (unsigned)"})
	}

	if len(mismatches) > 0 {
		return mismatches, errors.New(errors.ErrCodeInvalidRequest,
			"manifest inventory check failed for "+strconv.Itoa(len(mismatches))+" file(s)")
	}
	return nil, nil
}

func hashFile(bundleDir, rel string, expectedSize int64) (string, error) {
	// Reject non-local manifest paths before touching the filesystem.
	// A hostile manifest with rel="../../../etc/passwd" would otherwise
	// let the verifier stat and hash files outside bundleDir.
	localRel := filepath.FromSlash(rel)
	if !filepath.IsLocal(localRel) {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			"manifest entry "+rel+" is not a local path (rejecting potential traversal)")
	}
	full := filepath.Join(bundleDir, localRel)
	info, err := os.Stat(full)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeNotFound, "file missing from bundle: "+rel, err)
	}
	if info.IsDir() {
		return "", errors.New(errors.ErrCodeInvalidRequest, "manifest entry "+rel+" is a directory")
	}
	if expectedSize > 0 && info.Size() != expectedSize {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			"size mismatch for "+rel+
				" (got "+strconv.FormatInt(info.Size(), 10)+
				", want "+strconv.FormatInt(expectedSize, 10)+")")
	}
	got, hashErr := attestation.HashFileSHA256(full)
	if hashErr != nil {
		return "", errors.PropagateOrWrap(hashErr, errors.ErrCodeInternal,
			"failed to hash bundle file: "+rel)
	}
	return got, nil
}

// isSupportedManifestSchema accepts the 1.0.x family — same shape, the
// patch component is reserved for clarifying-only updates.
func isSupportedManifestSchema(v string) bool {
	return strings.HasPrefix(v, "1.0.") || v == "1.0"
}

// findExtras returns bundle-relative paths of files present on disk
// but not in manifest.Files, exempting the manifest itself and the
// in-toto Statement files. Honors ctx cancellation between entries.
func findExtras(ctx context.Context, bundleDir string, manifestFiles []attestation.ManifestFile) ([]string, error) {
	want := make(map[string]struct{}, len(manifestFiles))
	for _, f := range manifestFiles {
		want[f.Path] = struct{}{}
	}
	exempt := map[string]struct{}{
		attestation.ManifestFilename:    {},
		attestation.AttestationFilename: {},
		attestation.StatementFilename:   {},
	}
	var extras []string
	walkErr := filepath.WalkDir(bundleDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(bundleDir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if _, ok := want[rel]; ok {
			return nil
		}
		if _, ok := exempt[rel]; ok {
			return nil
		}
		extras = append(extras, rel)
		return nil
	})
	if walkErr != nil {
		// A canceled ctx surfaces here as context.Canceled / DeadlineExceeded
		// from the callback. Translate to ErrCodeUnavailable so cancellation
		// reads the same way it does in the per-file loop above.
		if stderrors.Is(walkErr, context.Canceled) || stderrors.Is(walkErr, context.DeadlineExceeded) {
			return nil, errors.Wrap(errors.ErrCodeUnavailable, "bundle walk canceled", walkErr)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to walk bundle dir", walkErr)
	}
	return extras, nil
}
