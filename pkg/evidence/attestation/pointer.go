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
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

// sha256HexLen is the number of hex characters in a SHA-256 digest body
// (256 bits / 4 bits per hex char). A bundle digest is "sha256:" + exactly
// this many lowercase-hex characters.
const sha256HexLen = 64

// maxPointerReadBytes caps the bytes read when comparing two pointer files for
// content equality. Pointers are tiny; anything larger is a bug or hostile
// input, so reading more would only risk memory. Matches the 1 MiB verifier
// ceiling.
const maxPointerReadBytes = 1 << 20

// PointerInputs carries the pointer-file fields that are not derived
// from the Bundle itself. Leave Signer nil for unsigned bundles.
type PointerInputs struct {
	Bundle     *Bundle
	BundleOCI  string
	BundleHash string
	Signer     *PointerSigner
}

// BuildPointer assembles the pointer YAML schema 1.0 from a built bundle
// plus optional post-push/sign claims. Empty BundleOCI/BundleHash signal
// "not yet published".
func BuildPointer(in PointerInputs) (*Pointer, error) {
	if in.Bundle == nil || in.Bundle.Predicate == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "bundle and predicate are required")
	}

	att := PointerAttestation{
		Bundle: PointerBundle{
			OCI:           in.BundleOCI,
			Digest:        in.BundleHash,
			PredicateType: PredicateTypeV1,
		},
		Signer:     in.Signer,
		AttestedAt: in.Bundle.Predicate.AttestedAt.UTC().Truncate(time.Second),
	}

	return &Pointer{
		SchemaVersion: PointerSchemaVersion,
		Recipe:        in.Bundle.RecipeName,
		Attestations:  []PointerAttestation{att},
	}, nil
}

// MarshalPointer renders a pointer as deterministic YAML (recursively
// sorted keys, 2-space indent) via serializer.MarshalYAMLDeterministic —
// the same serializer emit.go uses for the recipe/snapshot, keeping the
// evidence outputs consistent.
//
// The 2-space indent is load-bearing: the pointer is committed to
// recipes/evidence/<recipe>.yaml, where the repo's .yamllint (spaces: 2)
// lints it. yaml.v3's default 4-space sequence indent would fail
// `make lint` and break the documented publish -> commit -> PR workflow.
func MarshalPointer(p *Pointer) ([]byte, error) {
	if p == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "pointer is required")
	}
	body, err := serializer.MarshalYAMLDeterministic(p)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to marshal pointer", err)
	}
	return body, nil
}

// PointerCopyToHint returns the repo-relative destination a built pointer
// should be committed to, following the #1347 Option A per-source layout
// recipes/evidence/<recipe>/<source>/<bundle-digest>.yaml. The <source>
// slug is derived from the signer's OIDC identity (SourceSlug) and the
// filename from the bundle digest (':' → '-' for path-safety).
//
// A pointer with no signer or no pushed digest has no committable
// destination — only signed, pushed bundles earn a place in the tree — so
// the hint returns guidance instead of a path.
func PointerCopyToHint(p *Pointer) string {
	suffix, err := canonicalPointerSuffix(p)
	if err != nil {
		return "(sign and push the bundle, then commit under recipes/evidence/<recipe>/<source>/)"
	}
	return "recipes/evidence/" + filepath.ToSlash(suffix)
}

// canonicalPointerSuffix returns the <recipe>/<source>/<bundle-digest>.yaml
// path (relative to the evidence root) a signed, pushed pointer belongs at —
// the single source of truth for the #1347 Option A per-source layout, shared
// by PointerCopyToHint and RelocatePointerToCanonical. The <source> segment is
// SourceSlug(signer), so an unsigned or unpushed pointer has no canonical
// suffix and yields an error.
func canonicalPointerSuffix(p *Pointer) (string, error) {
	if p == nil || len(p.Attestations) == 0 {
		return "", errors.New(errors.ErrCodeInvalidRequest, "pointer with at least one attestation is required")
	}
	att := p.Attestations[0]
	if att.Signer == nil {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			"pointer has no signer; its canonical <source> path is underivable")
	}
	if att.Bundle.Digest == "" {
		return "", errors.New(errors.ErrCodeInvalidRequest, "pointer has no pushed bundle digest")
	}
	// recipe and the bundle digest are the attacker-influenced segments that
	// reach a filesystem path in RelocatePointerToCanonical (source is hex).
	// Fail closed at this sink so a malicious value can't escape the evidence
	// tree on the os.Link relocation — defense in depth that does not depend on
	// the CI contract gate, which a direct `aicr evidence sign --relocate
	// <file>` invocation bypasses. filepath.IsLocal rejects "", "..", and
	// absolute paths.
	if !filepath.IsLocal(p.Recipe) {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			"pointer.recipe is not a local path segment: "+p.Recipe)
	}
	// The digest becomes a path leaf. validatePointer only enforces the
	// sha256: prefix when bundle.oci is set (not the hex body), so a digest like
	// "sha256:x/../../escaped" would survive and traverse on filepath.Join.
	// Require the canonical sha256:<hex> shape here: hex carries no path
	// separator or ".", so the derived filename cannot escape <recipe>/<source>/.
	if !isHexDigest(att.Bundle.Digest) {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			"pointer bundle digest is not a canonical sha256:<hex> value: "+att.Bundle.Digest)
	}
	source, err := SourceSlug(att.Signer.Issuer, att.Signer.Identity)
	if err != nil {
		return "", err
	}
	file := strings.ReplaceAll(att.Bundle.Digest, ":", "-") + ".yaml"
	return filepath.Join(p.Recipe, source, file), nil
}

// isHexDigest reports whether d is a canonical bundle digest: the literal
// "sha256:" followed by exactly sha256HexLen lowercase-hex characters. It is
// the fail-closed check before the digest is turned into a path leaf — hex
// contains no path separator or ".", so a value that passes cannot traverse
// out of the canonical pointer directory — and the exact-length requirement
// rejects malformed digests (e.g. "sha256:nothex" or a truncated hash) rather
// than admitting any hex-ish string.
func isHexDigest(d string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(d, prefix) {
		return false
	}
	hex := d[len(prefix):]
	if len(hex) != sha256HexLen {
		return false
	}
	for _, c := range hex {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// RelocatePointerToCanonical moves a freshly-signed pointer from its flat
// pending location to its canonical per-source path, returning the
// destination. The flat location is the only place an unsigned pointer can be
// committed: the nested <source> path segment derives from the signer
// (SourceSlug), which a `--no-sign` pointer does not have. Once the
// fork-based CI leg signs it (`aicr evidence sign`), this completes the
// commit-flat -> CI-sign -> CI-relocate-to-nested flow (#1530) by moving the
// now-signed pointer under <recipe>/<source>/<bundle-digest>.yaml, where the
// per-source contract gate (verifier.CheckEvidenceTree) requires it.
//
// The destination root is the directory currentPath sits in, so a flat
// pointer at recipes/evidence/<recipe>.yaml lands under
// recipes/evidence/<recipe>/<source>/. Parent directories are created. It is
// a no-op (returns currentPath) when the pointer already lives at its
// canonical path, and idempotent when the canonical path already holds this
// same pointer (same inode, or byte-identical after a git round trip) — it
// just drops the redundant flat source. It fails closed — without moving
// anything — when the pointer is unsigned, has no pushed digest, or the
// canonical path already holds a file with DIFFERENT content (the per-source
// pointer is immutable, so that is a genuine conflict for the operator to
// resolve, not something to overwrite).
func RelocatePointerToCanonical(currentPath string, p *Pointer) (string, error) {
	// canonicalPointerSuffix fails closed when the pointer is unsigned or has
	// no pushed digest — the two states with no derivable canonical home.
	relSuffix, err := canonicalPointerSuffix(p)
	if err != nil {
		return "", err
	}
	// The canonical layout is the <recipe>/<source>/<digest>.yaml suffix under
	// the evidence root. When currentPath already ends in that suffix the
	// pointer is at its canonical path — a no-op. Detecting it by suffix
	// (rather than recomputing from filepath.Dir) is what keeps a flat pointer
	// at <root>/<recipe>.yaml distinguishable from a nested one at
	// <root>/<recipe>/<source>/<digest>.yaml: only the latter ends in the
	// suffix, so the flat root is filepath.Dir, not a doubly-nested path.
	cp := filepath.Clean(currentPath)
	if cp == relSuffix || strings.HasSuffix(filepath.ToSlash(cp), "/"+filepath.ToSlash(relSuffix)) {
		return currentPath, nil
	}
	dest := filepath.Join(filepath.Dir(currentPath), relSuffix)
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to create canonical pointer directory", err)
	}
	// Move with an atomic no-clobber guarantee: os.Link fails with EEXIST if
	// dest already exists, so the exclusivity check and the placement are a
	// single operation — no TOCTOU gap (a Stat+Rename would let another writer
	// create dest after the Stat, and Rename silently clobbers it on Unix). The
	// per-source pointer is immutable, so a pre-existing dest is a real conflict
	// for the operator to resolve, not something to overwrite.
	if err := os.Link(currentPath, dest); err != nil {
		if os.IsExist(err) {
			// dest already exists. Treat it as a completed placement — not a
			// conflict — when it already holds this same pointer, so the move
			// finishes idempotently. Two cases qualify:
			//   - same inode: a prior run linked dest but stopped before
			//     removing the source (within one filesystem session); and
			//   - identical content: after a git round trip the flat source and
			//     the committed nested copy are distinct inodes but byte-equal
			//     (pointers serialize deterministically), so SameFile alone
			//     would wrongly report a conflict.
			// Only a dest holding DIFFERENT bytes is a real immutable-pointer
			// conflict for the operator to resolve.
			placed, eqErr := pointerAlreadyPlaced(currentPath, dest)
			if eqErr != nil {
				return "", eqErr
			}
			if placed {
				if rmErr := os.Remove(currentPath); rmErr != nil {
					return "", errors.Wrap(errors.ErrCodeInternal,
						"canonical pointer already placed but failed to remove the flat source: "+currentPath, rmErr)
				}
				return dest, nil
			}
			return "", errors.New(errors.ErrCodeConflict,
				"canonical pointer path already exists with different content, refusing to overwrite: "+dest)
		}
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to link pointer to canonical path", err)
	}
	// The link now holds dest; drop the flat source to complete the move. If
	// this fails, dest is already correct — surface the leftover source.
	if err := os.Remove(currentPath); err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal,
			"relocated pointer to canonical path but failed to remove the flat source: "+currentPath, err)
	}
	return dest, nil
}

// pointerAlreadyPlaced reports whether dest already holds the same pointer as
// currentPath — either the same inode (a partial move within one filesystem
// session) or byte-identical content (the committed-both case, where a git
// round trip leaves distinct inodes). It returns an error rather than a bool on
// a read failure so the caller fails closed instead of silently treating an
// unreadable dest as a match or a conflict.
func pointerAlreadyPlaced(currentPath, dest string) (bool, error) {
	if sameInode(currentPath, dest) {
		return true, nil
	}
	srcBytes, err := readPointerBytes(currentPath)
	if err != nil {
		return false, err
	}
	destBytes, err := readPointerBytes(dest)
	if err != nil {
		return false, err
	}
	return bytes.Equal(srcBytes, destBytes), nil
}

// sameInode reports whether a and b are the same underlying file (e.g. two
// hard links to one inode). A stat failure on either path reports false (treat
// as not-same; fail safe).
func sameInode(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// readPointerBytes reads a pointer file, bounded to maxPointerReadBytes so a
// bloated or hostile path cannot exhaust memory during the equality check. A
// file at or past the cap is rejected (a pointer is never that large).
func readPointerBytes(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to open pointer for comparison", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxPointerReadBytes+1))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read pointer for comparison", err)
	}
	if int64(len(data)) > maxPointerReadBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "pointer file exceeds size limit: "+path)
	}
	return data, nil
}

// WritePointer writes the pointer file to outputDir/pointer.yaml.
func WritePointer(outputDir string, p *Pointer) (string, error) {
	return WritePointerFile(filepath.Join(outputDir, PointerFilename), p)
}

// WritePointerFile writes the pointer to an exact path, overwriting it. Used
// to patch a committed pointer in place (e.g. `aicr evidence sign` filling in
// the signer block), where the destination is the recipes/evidence/<recipe>.yaml
// the caller read — not a generated pointer.yaml beside a bundle dir.
//
// The write is atomic: it writes to a temp file in the same directory and
// renames it into place, so a crash or write error mid-flight cannot leave a
// committed pointer truncated/corrupt. The temp's Close error is propagated
// (a writable Close can surface buffered-write failures), and a leftover temp
// is removed on any failure before the rename.
func WritePointerFile(path string, p *Pointer) (string, error) {
	body, err := MarshalPointer(p)
	if err != nil {
		return "", err
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".pointer-*.tmp") // 0o600 by default
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to create temp pointer file", err)
	}
	tmpName := tmp.Name()
	// Remove the temp on any path that does not rename it into place.
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	if _, werr := tmp.Write(body); werr != nil {
		_ = tmp.Close()
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to write temp pointer file", werr)
	}
	// Closing a writable handle flushes buffered data — propagate its error.
	if cerr := tmp.Close(); cerr != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to close temp pointer file", cerr)
	}
	if rerr := os.Rename(tmpName, path); rerr != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to rename pointer file into place", rerr)
	}
	committed = true
	return path, nil
}
