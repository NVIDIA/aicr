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
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/oci"
)

// PublishOptions controls a single Publish run. Publish operates on an
// already-emitted on-disk bundle so the cluster-bound validate step and
// the Fulcio/Rekor-bound signing step can run on different networks (see
// ADR-007 and issue #1130).
type PublishOptions struct {
	// BundleDir is the on-disk evidence directory. It may be either the
	// OutDir that `validate --emit-attestation` wrote (holds
	// summary-bundle/ and is where pointer.yaml is written) or the
	// summary-bundle/ directory itself.
	BundleDir string

	// Push is the OCI reference the summary bundle is pushed to. Unlike
	// Emit, Push is required: a publish with nothing to push is a no-op.
	Push        string
	PlainHTTP   bool
	InsecureTLS bool

	// AICRVersion is stamped into the pushed OCI manifest's annotations.
	// It does not alter the signed predicate, which is read verbatim from
	// the bundle's statement.intoto.json — the bundle bytes (and their
	// baked-in attestedAt timestamp) are signed as-is.
	AICRVersion string

	// OIDCResolve configures keyless-signing token resolution. Resolution
	// is deferred until adjacent to SignStatement so Fulcio's
	// nonce-binding window is respected.
	OIDCResolve bundleattest.ResolveOptions
}

// Publish signs and pushes an already-emitted recipe-evidence v1 bundle,
// then writes pointer.yaml beside it. It is the off-network second leg of
// the workflow whose first leg is `aicr validate --emit-attestation`
// (no --push): that step produces the unsigned on-disk bundle this one
// consumes.
//
// The output is identical to the one-shot `validate --emit-attestation
// --push` path: the predicate signed here is read verbatim from the
// bundle's statement.intoto.json, so the timestamp baked at emit time is
// preserved and the resulting signed artifact is content-identical
// regardless of which host ran which leg.
//
// Publish returns only an error: unlike Emit (whose EmitResult is exercised by
// tests), no in-repo caller consumes Publish's artifacts, and the populated
// success path needs a live push + Fulcio sign that unit tests can't reach. A
// future API handler that needs PushSummary/Sign can reintroduce the richer
// return when a real caller exists.
func Publish(ctx context.Context, opts PublishOptions) error {
	if opts.Push == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "push reference is required for publish")
	}
	// Validate the ref up front so a malformed reference doesn't waste a
	// Fulcio cert + Rekor inclusion proof on a sign the push would reject.
	if _, err := oci.ParseOutputTarget(opts.Push); err != nil {
		return errors.Wrap(errors.ErrCodeInvalidRequest, "invalid push reference", err)
	}

	bundle, outDir, err := loadOnDiskBundle(opts.BundleDir)
	if err != nil {
		return err
	}

	slog.Info("publishing evidence bundle",
		"summaryDir", bundle.SummaryDir,
		"recipe", bundle.RecipeName,
		"push", opts.Push)

	out, err := signAndPush(ctx, bundle, signPushOptions{
		Push:        opts.Push,
		PlainHTTP:   opts.PlainHTTP,
		InsecureTLS: opts.InsecureTLS,
		AICRVersion: opts.AICRVersion,
		OIDCResolve: opts.OIDCResolve,
	})
	if err != nil {
		return err
	}

	pointer, err := BuildPointer(buildPointerInputsFromOutcome(bundle, out))
	if err != nil {
		return err
	}
	pointerPath, err := WritePointer(outDir, pointer)
	if err != nil {
		return err
	}

	slog.Info("evidence pointer written",
		"path", pointerPath,
		"copyTo", "recipes/evidence/"+bundle.RecipeName+".yaml")

	if out.PushSummary != nil {
		slog.Info("evidence bundle pushed",
			"reference", out.PushSummary.Reference,
			"digest", out.PushSummary.Digest)
	}

	return nil
}

// loadOnDiskBundle resolves the summary-bundle directory and the
// pointer-output directory from a user-supplied path, then reconstructs
// the minimal *Bundle the sign+push leg needs by reading the bundle's
// unsigned in-toto Statement. The predicate is trusted as-is — Publish
// signs the pre-built bundle bytes verbatim; integrity auditing is the
// job of `aicr evidence verify`.
func loadOnDiskBundle(dir string) (*Bundle, string, error) {
	if dir == "" {
		return nil, "", errors.New(errors.ErrCodeInvalidRequest, "bundle directory is required")
	}
	summaryDir, outDir, err := resolveSummaryDir(dir)
	if err != nil {
		return nil, "", err
	}
	pred, stmt, err := readBundlePredicate(summaryDir)
	if err != nil {
		return nil, "", err
	}
	if pred.Recipe.Name == "" || pred.Recipe.Digest == "" {
		return nil, "", errors.New(errors.ErrCodeInvalidRequest,
			"bundle statement predicate is missing recipe.{name,digest}")
	}
	return &Bundle{
		SummaryDir:    summaryDir,
		RecipeName:    pred.Recipe.Name,
		SubjectDigest: pred.Recipe.Digest,
		Predicate:     pred,
		StatementJSON: stmt,
	}, outDir, nil
}

// resolveSummaryDir accepts either the summary-bundle root or a parent
// containing it (mirroring `aicr evidence verify`'s directory handling).
// It returns the summary directory to push and the directory pointer.yaml
// should be written to. When dir is the summary bundle itself, the
// pointer lands in its parent so the on-disk layout matches the one-shot
// `validate --emit-attestation --push` output (pointer.yaml beside
// summary-bundle/).
func resolveSummaryDir(dir string) (summaryDir, outDir string, err error) {
	clean := filepath.Clean(dir)
	if HasBundleMarkers(clean) {
		return clean, filepath.Dir(clean), nil
	}
	candidate := filepath.Join(clean, SummaryBundleDirName)
	if HasBundleMarkers(candidate) {
		return candidate, clean, nil
	}
	return "", "", errors.New(errors.ErrCodeInvalidRequest,
		"directory "+dir+" does not look like a summary bundle "+
			"(no recipe.yaml / manifest.json at root or under summary-bundle/)")
}

// HasBundleMarkers reports whether dir holds the two files every summary
// bundle carries at its root. recipe.yaml + manifest.json together are a
// reliable discriminator: the unsigned Statement and BOM share names with
// other artifacts, but this pair only co-occurs in a summary bundle. It is
// the single source of truth for "is this directory a summary bundle?",
// shared by the publish path here and the verifier's materialization.
func HasBundleMarkers(dir string) bool {
	for _, f := range []string{RecipeFilename, ManifestFilename} {
		info, statErr := os.Stat(filepath.Join(dir, f))
		if statErr != nil || info.IsDir() {
			return false
		}
	}
	return true
}

// readBundlePredicate reads the bundle's unsigned in-toto Statement and
// returns the predicate body plus the raw statement bytes. Mirrors the
// verifier's loadUnsignedPredicate: the predicate is trusted as-is.
func readBundlePredicate(summaryDir string) (*Predicate, []byte, error) {
	path := filepath.Join(summaryDir, StatementFilename)
	// Bound the read: a publish target may be an attacker-influenced bundle
	// root (extracted archive, symlinked path) where os.ReadFile would
	// allocate the whole file before any size check. Mirrors the verifier's
	// readBoundedFile against the same defaults cap.
	f, err := os.Open(path) //nolint:gosec // bundle-local path resolved by resolveSummaryDir
	if err != nil {
		return nil, nil, errors.Wrap(errors.ErrCodeNotFound, "failed to read in-toto Statement", err)
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(io.LimitReader(f, defaults.MaxAttestationFileBytes+1))
	if err != nil {
		return nil, nil, errors.Wrap(errors.ErrCodeInternal, "failed to read in-toto Statement", err)
	}
	if int64(len(body)) > defaults.MaxAttestationFileBytes {
		return nil, nil, errors.New(errors.ErrCodeInvalidRequest,
			"in-toto Statement exceeds maximum size of "+
				strconv.FormatInt(defaults.MaxAttestationFileBytes, 10)+" bytes")
	}
	var envelope struct {
		PredicateType string    `json:"predicateType"`
		Predicate     Predicate `json:"predicate"`
	}
	if uErr := json.Unmarshal(body, &envelope); uErr != nil {
		return nil, nil, errors.Wrap(errors.ErrCodeInvalidRequest, "statement is not valid JSON", uErr)
	}
	if envelope.PredicateType != PredicateTypeV1 {
		return nil, nil, errors.New(errors.ErrCodeInvalidRequest,
			"unexpected predicateType "+envelope.PredicateType)
	}
	return &envelope.Predicate, body, nil
}
