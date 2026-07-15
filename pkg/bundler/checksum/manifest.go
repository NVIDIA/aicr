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

package checksum

import (
	"context"
	"encoding/hex"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// Entry is one checksums.txt payload record.
type Entry struct {
	Digest string
	Path   string
}

// Manifest is a parsed checksums.txt payload inventory.
type Manifest struct {
	entries []Entry
	opts    InventoryOptions
}

// InventoryOptions defines caller-owned metadata paths admitted outside the
// checksums.txt payload. Their identity and bytes are preserved across verified
// staging, but checksums.txt does not authenticate their content. A caller MUST
// authenticate every present metadata path independently before granting trust.
type InventoryOptions struct {
	AllowedMetadataPaths []string
}

// ParseManifest strictly parses checksums.txt data without normalizing input.
func ParseManifest(ctx context.Context, data []byte, opts InventoryOptions) (*Manifest, error) {
	validatedOpts, err := validateInventoryOptions(opts)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(string(data), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "checksum manifest must contain at least one entry")
	}

	entries := make([]Entry, 0, len(lines))
	exactPaths := make(map[string]struct{}, len(lines))
	foldedPaths := make(map[string]struct{}, len(lines))
	for _, line := range lines {
		if err := contextErr(ctx, "checksum manifest parsing canceled"); err != nil {
			return nil, err
		}
		if line == "" {
			return nil, errors.New(errors.ErrCodeInvalidRequest, "checksum manifest contains a blank line")
		}

		digest, rel, ok := strings.Cut(line, "  ")
		if !ok {
			return nil, errors.New(errors.ErrCodeInvalidRequest, "checksum line must use exactly two separator spaces")
		}
		if err := validateDigest(digest); err != nil {
			return nil, err
		}
		if err := validatePayloadPath(rel, validatedOpts); err != nil {
			return nil, err
		}
		if err := addUniquePath(rel, exactPaths, foldedPaths); err != nil {
			return nil, err
		}
		entries = append(entries, Entry{Digest: digest, Path: rel})
	}

	return &Manifest{entries: entries, opts: validatedOpts}, nil
}

// Entries returns a defensive copy of the manifest entries in input order.
func (m *Manifest) Entries() []Entry {
	if m == nil {
		return nil
	}
	return append([]Entry(nil), m.entries...)
}

// Len returns the number of payload entries in the manifest.
func (m *Manifest) Len() int {
	if m == nil {
		return 0
	}
	return len(m.entries)
}

// MarshalText returns a canonical path-sorted checksums.txt representation.
func (m *Manifest) MarshalText() ([]byte, error) {
	if m == nil || len(m.entries) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "checksum manifest must contain at least one entry")
	}
	validatedOpts, err := validateInventoryOptions(m.opts)
	if err != nil {
		return nil, err
	}

	entries := append([]Entry(nil), m.entries...)
	exactPaths := make(map[string]struct{}, len(entries))
	foldedPaths := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if err := validateDigest(entry.Digest); err != nil {
			return nil, err
		}
		if err := validatePayloadPath(entry.Path, validatedOpts); err != nil {
			return nil, err
		}
		if err := addUniquePath(entry.Path, exactPaths, foldedPaths); err != nil {
			return nil, err
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})

	var out strings.Builder
	for _, entry := range entries {
		out.WriteString(entry.Digest)
		out.WriteString("  ")
		out.WriteString(entry.Path)
		out.WriteByte('\n')
	}
	return []byte(out.String()), nil
}

func validateDigest(digest string) error {
	if len(digest) != 64 || digest != strings.ToLower(digest) {
		return errors.New(errors.ErrCodeInvalidRequest, "checksum digest must be 64 lowercase hexadecimal characters")
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 {
		return errors.New(errors.ErrCodeInvalidRequest, "checksum digest must be 64 lowercase hexadecimal characters")
	}
	return nil
}

func validatePayloadPath(rel string, opts InventoryOptions) error {
	if err := validateCanonicalPath(rel); err != nil {
		return err
	}
	if foldPathKey(rel) == foldPathKey(ChecksumFileName) || isReservedByOptions(rel, opts) {
		return errors.New(errors.ErrCodeInvalidRequest, "checksum path uses the reserved metadata namespace")
	}
	return nil
}

func validateCanonicalPath(rel string) error {
	if rel == "" || rel == "." || strings.TrimSpace(rel) != rel {
		return errors.New(errors.ErrCodeInvalidRequest, "checksum path must be a non-empty clean relative path")
	}
	if strings.ContainsRune(rel, '\\') || strings.IndexFunc(rel, unicode.IsControl) >= 0 {
		return errors.New(errors.ErrCodeInvalidRequest, "checksum path contains a forbidden separator or control character")
	}
	if path.IsAbs(rel) || path.Clean(rel) != rel || !filepath.IsLocal(filepath.FromSlash(rel)) {
		return errors.New(errors.ErrCodeInvalidRequest, "checksum path is not local and canonical")
	}
	if len(rel) >= 2 && unicode.IsLetter(rune(rel[0])) && rel[1] == ':' {
		return errors.New(errors.ErrCodeInvalidRequest, "checksum path is drive-qualified")
	}
	return nil
}

func validateInventoryOptions(opts InventoryOptions) (InventoryOptions, error) {
	validated := InventoryOptions{
		AllowedMetadataPaths: append([]string(nil), opts.AllowedMetadataPaths...),
	}
	exactPaths := make(map[string]struct{}, len(validated.AllowedMetadataPaths))
	foldedPaths := make(map[string]struct{}, len(validated.AllowedMetadataPaths))
	for _, rel := range validated.AllowedMetadataPaths {
		if err := validateCanonicalPath(rel); err != nil {
			return InventoryOptions{}, err
		}
		if foldPathKey(rel) == foldPathKey(ChecksumFileName) {
			return InventoryOptions{}, errors.New(errors.ErrCodeInvalidRequest, "checksums.txt cannot be allowed metadata")
		}
		if err := addUniquePath(rel, exactPaths, foldedPaths); err != nil {
			return InventoryOptions{}, err
		}
	}
	return validated, nil
}

func addUniquePath(rel string, exactPaths, foldedPaths map[string]struct{}) error {
	if _, ok := exactPaths[rel]; ok {
		return errors.New(errors.ErrCodeInvalidRequest, "checksum manifest contains a duplicate path")
	}
	folded := foldPathKey(rel)
	if _, ok := foldedPaths[folded]; ok {
		return errors.New(errors.ErrCodeInvalidRequest, "checksum manifest contains a case-fold path alias")
	}
	exactPaths[rel] = struct{}{}
	foldedPaths[folded] = struct{}{}
	return nil
}

func isReservedByOptions(rel string, opts InventoryOptions) bool {
	foldedRel := foldPathKey(rel)
	for _, allowed := range opts.AllowedMetadataPaths {
		foldedAllowed := foldPathKey(allowed)
		if foldedRel == foldedAllowed {
			return true
		}
		namespace := path.Dir(foldedAllowed)
		if namespace != "." &&
			(foldedRel == namespace || strings.HasPrefix(foldedRel, namespace+"/")) {

			return true
		}
	}
	return false
}

// foldPathKey returns a canonical key for Unicode simple-fold-equivalent
// paths. Callers retain the original path for filesystem access and errors.
func foldPathKey(value string) string {
	return strings.Map(func(r rune) rune {
		folded := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < folded {
				folded = next
			}
		}
		return folded
	}, value)
}

func contextErr(ctx context.Context, message string) error {
	if err := ctx.Err(); err != nil {
		return errors.Wrap(errors.ErrCodeTimeout, message, err)
	}
	return nil
}
