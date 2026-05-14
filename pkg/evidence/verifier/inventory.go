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
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// CheckInventory recomputes every sha256 named in manifest.json and
// confirms no unmanaged files are present. This single check
// transitively binds every bundled file to the predicate via
// predicate.manifest.digest — there's no separate step needed for
// recipe.yaml, bom.cdx.json, or the CTRF reports.
//
// ctx is honored between files (large bundles, hostile manifests with
// many entries) and during the bundle walk for stray-file detection.
//
// Returns per-file mismatch rows and an error summarizing the failure;
// both nil on success.
func CheckInventory(ctx context.Context, mat *MaterializedBundle) ([]KV, error) {
	if mat == nil || mat.BundleDir == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "materialized bundle is required")
	}
	body, err := os.ReadFile(filepath.Join(mat.BundleDir, attestation.ManifestFilename)) //nolint:gosec
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeNotFound, "failed to read manifest.json", err)
	}
	var manifest attestation.Manifest
	if uErr := json.Unmarshal(body, &manifest); uErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "manifest.json is not valid JSON", uErr)
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
	full := filepath.Join(bundleDir, filepath.FromSlash(rel))
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
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to walk bundle dir", walkErr)
	}
	return extras, nil
}
