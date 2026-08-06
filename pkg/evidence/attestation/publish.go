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
	stderrors "errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/internal/boundedio"
	"github.com/NVIDIA/aicr/pkg/oci"
	"github.com/NVIDIA/aicr/pkg/recipe"
	"github.com/NVIDIA/aicr/pkg/serializer"
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

	// NoSign pushes the unsigned bundle and writes a pointer with an empty
	// signer block instead of signing. The Fulcio/Rekor leg is deferred to
	// `aicr evidence sign` (or the fork-based CI workflow). When false,
	// Publish signs as before.
	NoSign bool

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

// Publish signs and pushes an already-emitted recipe-evidence bundle,
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

	bundle, outDir, err := loadOnDiskBundle(ctx, opts.BundleDir)
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
		NoSign:      opts.NoSign,
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
		"copyTo", PointerCopyToHint(pointer))

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
func loadOnDiskBundle(ctx context.Context, dir string) (*Bundle, string, error) {
	if dir == "" {
		return nil, "", errors.New(errors.ErrCodeInvalidRequest, "bundle directory is required")
	}
	summaryDir, outDir, err := resolveSummaryDir(ctx, dir)
	if err != nil {
		return nil, "", err
	}
	pred, stmt, err := readBundlePredicate(ctx, summaryDir)
	if err != nil {
		return nil, "", err
	}
	if pred.Recipe.Name == "" || pred.Recipe.Digest == "" {
		return nil, "", errors.New(errors.ErrCodeInvalidRequest,
			"bundle statement predicate is missing recipe.{name,digest}")
	}
	profile, advertiser, descriptorIdentity, err := readBundleRecipeProfile(ctx, summaryDir)
	if err != nil {
		return nil, "", err
	}
	bundle := &Bundle{
		SummaryDir:               summaryDir,
		RecipeName:               pred.Recipe.Name,
		Profile:                  profile,
		Advertiser:               advertiser,
		PolicyDescriptorIdentity: descriptorIdentity,
		SubjectDigest:            pred.Recipe.Digest,
		Predicate:                pred,
		StatementJSON:            stmt,
	}
	// Fail closed at load time — before Publish's sign/push side effects —
	// when the recipe's profile selection and the statement's predicate
	// profile block disagree: signing and pushing such a bundle would spend
	// a Fulcio cert and registry writes on an artifact whose pointer the
	// verifier rejects.
	if err := ValidateBundleProfileCoherence(bundle); err != nil {
		return nil, "", err
	}
	return bundle, outDir, nil
}

// withFileReadTimeout runs fn — a blocking local-filesystem syscall (os.Open /
// io.ReadAll / os.Stat) — under a FileReadTimeout-bounded derivative of ctx,
// returning fn's own error, or an ErrCodeTimeout error if the bound elapses
// first. It exists because a bare os.Open/os.Stat against a hung NFS/FUSE mount
// blocks in the syscall indefinitely: no downstream sign/push timeout can
// unblock it, so the read itself must be time-bounded to surface a dead mount
// as a timeout rather than an indefinite hang.
//
// fn runs in a goroutine so the caller unblocks the instant ctx expires. The
// accepted tradeoff: on timeout the parked goroutine stays blocked in the
// syscall until the kernel returns or the process exits — fine for a
// short-lived CLI leaf command (evidence publish/sign), which is the only
// caller of these bundle readers.
func withFileReadTimeout(ctx context.Context, what string, fn func() error) error {
	return boundedio.Do(ctx, what, fn)
}

// readBundleRecipeProfile decodes the summary bundle's recipe.yaml and
// returns the canonical name=value selection it carries, its declared
// advertiser, and the recipe-scoped policy-descriptor identity recomputed
// from its closure-contributing descriptor entries — or "" for all three
// on an unprofiled recipe. Publish
// reconstructs the pointer from the on-disk bundle, so the recorded
// selection must come from the attested recipe bytes — the predicate
// carries only the (suffixed) recipe name. The read is size-bounded like
// readBundlePredicate: the publish target may be an attacker-influenced
// bundle root where os.ReadFile would allocate the whole file before any
// size check. It is also time-bounded via withFileReadTimeout so the read
// cannot hang indefinitely on a dead NFS/FUSE mount.
func readBundleRecipeProfile(ctx context.Context, summaryDir string) (profile, advertiser, descriptorIdentity string, err error) {
	path := filepath.Join(summaryDir, RecipeFilename)
	var body []byte
	if rerr := withFileReadTimeout(ctx, "bundle recipe.yaml", func() error {
		f, oErr := os.Open(path) //nolint:gosec // bundle-local path resolved by resolveSummaryDir
		if oErr != nil {
			return errors.Wrap(errors.ErrCodeNotFound, "failed to read bundle recipe.yaml", oErr)
		}
		defer func() { _ = f.Close() }()
		b, rErr := io.ReadAll(io.LimitReader(f, defaults.MaxRecipePOSTBytes+1))
		if rErr != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to read bundle recipe.yaml", rErr)
		}
		body = b
		return nil
	}); rerr != nil {
		return "", "", "", rerr
	}
	if int64(len(body)) > defaults.MaxRecipePOSTBytes {
		return "", "", "", errors.New(errors.ErrCodeInvalidRequest,
			"bundle recipe.yaml exceeds maximum size of "+
				strconv.FormatInt(defaults.MaxRecipePOSTBytes, 10)+" bytes")
	}
	rec, err := recipe.DecodeRecipeResult(body, serializer.FormatYAML)
	if err != nil {
		return "", "", "", errors.PropagateOrWrap(err, errors.ErrCodeInvalidRequest,
			"failed to parse bundle recipe.yaml")
	}
	return ProfileSelectionString(rec), ProfileAdvertiserString(rec), profileDescriptorIdentityOf(rec), nil
}

// resolveSummaryDir accepts either the summary-bundle root or a parent
// containing it (mirroring `aicr evidence verify`'s directory handling).
// It returns the summary directory to push and the directory pointer.yaml
// should be written to. When dir is the summary bundle itself, the
// pointer lands in its parent so the on-disk layout matches the one-shot
// `validate --emit-attestation --push` output (pointer.yaml beside
// summary-bundle/).
func resolveSummaryDir(ctx context.Context, dir string) (summaryDir, outDir string, err error) {
	clean := filepath.Clean(dir)
	ok, err := HasBundleMarkers(ctx, clean)
	if err != nil {
		return "", "", err
	}
	if ok {
		return clean, filepath.Dir(clean), nil
	}
	candidate := filepath.Join(clean, SummaryBundleDirName)
	ok, err = HasBundleMarkers(ctx, candidate)
	if err != nil {
		return "", "", err
	}
	if ok {
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
//
// The two os.Stat calls are time-bounded so a dead NFS/FUSE mount surfaces as
// a timeout instead of an indefinite hang. The (bool, error) shape keeps a
// genuine "not a bundle" answer distinct from a fault:
//
//   - absent (ENOENT) or is-a-dir → (false, nil). A missing marker is a real
//     answer, and the probe sites try more than one candidate directory, so
//     this must stay cheap and non-fatal.
//   - any other stat error (EACCES/EIO/ESTALE) → (false, err). These say
//     nothing about whether dir is a bundle. Collapsing them to "not a bundle"
//     reported a storage fault to the operator as INVALID_REQUEST "does not
//     look like a summary bundle", sending them to debug their input instead
//     of their mount.
func HasBundleMarkers(ctx context.Context, dir string) (bool, error) {
	return hasBundleMarkersWithStat(ctx, dir, os.Stat)
}

func hasBundleMarkersWithStat(
	ctx context.Context,
	dir string,
	statFn func(string) (os.FileInfo, error),
) (bool, error) {

	for _, f := range []string{RecipeFilename, ManifestFilename} {
		path := filepath.Join(dir, f)
		var present bool
		var statErr error
		// Label with the full path (not just f) so a timeout names which
		// candidate directory stalled — the probe sites try more than one.
		if err := withFileReadTimeout(ctx, "bundle marker "+path, func() error {
			var info os.FileInfo
			info, statErr = statFn(path)
			present = statErr == nil && !info.IsDir()
			return nil
		}); err != nil {
			return false, err
		}
		// ENOTDIR/ENAMETOOLONG mean the path cannot name a marker at all
		// (e.g. the operator passed a file where a bundle dir was expected).
		// syscall.Errno.Is maps only ENOENT to fs.ErrNotExist, so these must
		// be listed explicitly or a typo becomes an INTERNAL error.
		if statErr != nil && !stderrors.Is(statErr, fs.ErrNotExist) &&
			!stderrors.Is(statErr, syscall.ENOTDIR) && !stderrors.Is(statErr, syscall.ENAMETOOLONG) {

			return false, errors.Wrap(errors.ErrCodeUnavailable,
				"failed to probe bundle marker "+path, statErr)
		}
		if !present {
			return false, nil
		}
	}
	return true, nil
}

// readBundlePredicate reads the bundle's unsigned in-toto Statement and
// returns the predicate body plus the raw statement bytes. Mirrors the
// verifier's loadUnsignedPredicate: the predicate is trusted as-is.
func readBundlePredicate(ctx context.Context, summaryDir string) (*Predicate, []byte, error) {
	path := filepath.Join(summaryDir, StatementFilename)
	// Bound the read two ways: by size — a publish target may be an
	// attacker-influenced bundle root (extracted archive, symlinked path)
	// where os.ReadFile would allocate the whole file before any size check
	// (mirrors the verifier's readBoundedFile against the same defaults cap)
	// — and by time, via withFileReadTimeout, so a dead NFS/FUSE mount
	// surfaces as a timeout instead of hanging indefinitely.
	var body []byte
	if rerr := withFileReadTimeout(ctx, "in-toto Statement", func() error {
		f, oErr := os.Open(path) //nolint:gosec // bundle-local path resolved by resolveSummaryDir
		if oErr != nil {
			return errors.Wrap(errors.ErrCodeNotFound, "failed to read in-toto Statement", oErr)
		}
		defer func() { _ = f.Close() }()
		b, rErr := io.ReadAll(io.LimitReader(f, defaults.MaxAttestationFileBytes+1))
		if rErr != nil {
			return errors.Wrap(errors.ErrCodeInternal, "failed to read in-toto Statement", rErr)
		}
		body = b
		return nil
	}); rerr != nil {
		return nil, nil, rerr
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
	if cErr := ValidatePredicateTypeCoherence(envelope.PredicateType, &envelope.Predicate); cErr != nil {
		return nil, nil, cErr
	}
	return &envelope.Predicate, body, nil
}
