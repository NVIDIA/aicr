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

package attestation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/NVIDIA/aicr/pkg/bundler/checksum"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// BuildManifest walks bundleDir and computes a deterministic manifest
// inventorying every regular file. The manifest's own sha256 binds the
// unsigned files to the signed predicate via Predicate.Manifest.Digest.
// Paths in excludePaths are skipped (e.g., the manifest itself).
func BuildManifest(bundleDir string, excludePaths ...string) (*Manifest, error) {
	exclude := make(map[string]struct{}, len(excludePaths))
	for _, p := range excludePaths {
		exclude[filepath.ToSlash(p)] = struct{}{}
	}

	var entries []ManifestFile
	walkErr := filepath.WalkDir(bundleDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(bundleDir, path)
		if relErr != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to compute manifest entry path", relErr)
		}
		rel = filepath.ToSlash(rel)
		if _, skip := exclude[rel]; skip {
			return nil
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to stat manifest entry", infoErr)
		}

		digest, hashErr := HashFileSHA256(path)
		if hashErr != nil {
			return hashErr
		}

		entries = append(entries, ManifestFile{
			Path:      rel,
			Size:      info.Size(),
			SHA256:    "sha256:" + digest,
			MediaType: detectMediaType(rel),
		})
		return nil
	})
	if walkErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to walk bundle directory", walkErr)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})

	return &Manifest{
		SchemaVersion: ManifestSchemaVersion,
		Files:         entries,
	}, nil
}

// MarshalManifest renders the manifest as deterministic, indented JSON
// with a trailing newline. Calling MarshalManifest twice on equivalent
// manifests must produce byte-identical output.
func MarshalManifest(m *Manifest) ([]byte, error) {
	if m == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "cannot marshal nil manifest")
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to marshal manifest", err)
	}
	out = append(out, '\n')
	return out, nil
}

// WriteManifest writes manifest.json into bundleDir and returns its
// sha256 digest (for embedding in Predicate.Manifest.Digest).
func WriteManifest(bundleDir string, m *Manifest) (string, error) {
	body, err := MarshalManifest(m)
	if err != nil {
		return "", err
	}
	path := filepath.Join(bundleDir, ManifestFilename)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to write manifest", err)
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// HashFileSHA256 returns the lowercase hex sha256 of a file's contents,
// streaming the file rather than reading it whole.
func HashFileSHA256(path string) (string, error) {
	raw, err := checksum.SHA256Raw(path)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// HashBytesSHA256 returns the prefixed lowercase hex sha256 of a byte
// slice (e.g., "sha256:abc123...").
func HashBytesSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// detectMediaType returns a coarse media type based on file extension.
// The manifest uses this only as a hint for archival tooling; the
// integrity guarantee is the sha256. Order matters: more-specific
// suffixes (`.cdx.json`, `.intoto.jsonl`) are checked before generic
// ones (`.json`).
func detectMediaType(rel string) string {
	switch {
	case strings.HasSuffix(rel, ".cdx.json"):
		return "application/vnd.cyclonedx+json"
	case strings.HasSuffix(rel, ".intoto.jsonl"):
		return "application/vnd.in-toto+jsonl"
	case strings.HasSuffix(rel, ".json"):
		return "application/json"
	case strings.HasSuffix(rel, ".yaml"), strings.HasSuffix(rel, ".yml"):
		return "application/yaml"
	case strings.HasSuffix(rel, ".log"):
		return "text/plain"
	default:
		return ""
	}
}
