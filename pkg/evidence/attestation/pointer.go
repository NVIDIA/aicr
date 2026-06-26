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
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/serializer"
)

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
	const unpushed = "(sign and push the bundle, then commit under recipes/evidence/<recipe>/<source>/)"
	if p == nil || len(p.Attestations) == 0 {
		return unpushed
	}
	att := p.Attestations[0]
	if att.Signer == nil || att.Bundle.Digest == "" {
		return unpushed
	}
	source, err := SourceSlug(att.Signer.Issuer, att.Signer.Identity)
	if err != nil {
		return unpushed
	}
	file := strings.ReplaceAll(att.Bundle.Digest, ":", "-") + ".yaml"
	return "recipes/evidence/" + p.Recipe + "/" + source + "/" + file
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
