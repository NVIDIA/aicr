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

// Package trust manages Sigstore trusted root material for offline attestation
// verification.
//
// # Trusted Root Resolution
//
// The trusted root (trusted_root.json) contains Fulcio CA certificates and Rekor
// public keys needed to verify Sigstore attestation bundles. Resolution follows
// three layers in priority order:
//
//  1. Local cache (~/.sigstore/root/) — written by Update(), read by
//     GetTrustedMaterial() with ForceCache. No network access.
//  2. Embedded TUF root — compiled into the binary via sigstore-go's
//     //go:embed directive. Used to bootstrap the TUF update chain when no
//     local cache exists. Updated when the sigstore-go dependency is updated.
//  3. TUF update — Update() contacts the Sigstore TUF CDN
//     (tuf-repo-cdn.sigstore.dev), verifies the update chain cryptographically
//     from the embedded root, and writes the latest trusted_root.json to the
//     local cache.
//
// Verification (GetTrustedMaterial) is always fully offline. Trust material is
// updated only when the user explicitly runs "aicr trust update".
//
// # Key Rotation
//
// Sigstore rotates keys a few times per year. When rotation causes verification
// to fail (signing certificate chains to a CA not in the local root), the
// verifier detects this and surfaces an actionable error directing the user to
// run "aicr trust update".
package trust

import (
	"context"
	stderrors "errors"
	"io"
	"log/slog"
	"os"
	"syscall"

	prototrustroot "github.com/sigstore/protobuf-specs/gen/pb-go/trustroot/v1"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	tufmd "github.com/theupdateframework/go-tuf/v2/metadata"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// classifyTUFError maps a go-tuf error to the appropriate pkg/errors code.
//
//   - Transport/download failures (network, HTTP non-2xx, length-mismatch
//     during fetch) → ErrCodeUnavailable.
//   - Signature/structure verification failures (bad signature, expired
//     metadata, hash mismatch on a verified blob) → ErrCodeUnauthorized.
//   - Everything else (ErrRepository, ErrBadVersionNumber, ErrValue,
//     ErrType, ErrRuntime, …) → ErrCodeInternal. The default is NOT
//     Unauthorized: a repository or runtime fault should not be reported
//     as a trust-chain failure, and the human-readable messages downstream
//     switch on the returned code.
func classifyTUFError(err error) errors.ErrorCode {
	var (
		dlErr    *tufmd.ErrDownload
		dlHTTP   *tufmd.ErrDownloadHTTP
		dlLen    *tufmd.ErrDownloadLengthMismatch
		unsigned *tufmd.ErrUnsignedMetadata
		hashMis  *tufmd.ErrLengthOrHashMismatch
		expired  *tufmd.ErrExpiredMetadata
	)
	switch {
	case stderrors.As(err, &dlErr), stderrors.As(err, &dlHTTP), stderrors.As(err, &dlLen):
		return errors.ErrCodeUnavailable
	case stderrors.As(err, &unsigned), stderrors.As(err, &hashMis), stderrors.As(err, &expired):
		return errors.ErrCodeUnauthorized
	default:
		return errors.ErrCodeInternal
	}
}

// GetTrustedMaterial returns Sigstore trusted material for offline verification.
// Uses the sigstore-go TUF client with ForceCache to avoid network calls.
// Falls back to the embedded TUF root if no cache exists.
func GetTrustedMaterial() (root.TrustedMaterial, error) {
	opts := tuf.DefaultOptions().WithForceCache()

	client, err := tuf.New(opts)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to initialize TUF client", err)
	}

	return trustedMaterialFromClient(client)
}

// signingConfigTarget is the TUF target name for the Sigstore signing config
// that routes signing to Rekor v2. AICR signs to Rekor v2 by default, so this is
// the config the signer resolves unless the caller opts out to a Rekor v1 URL.
// Sigstore distributes it alongside the default (v1) signing config; the
// public-good default still points *other* clients at Rekor v1, so we consume
// this v2 target explicitly. When Sigstore makes v2 the ecosystem default this
// can move to the default "signing_config.v0.2.json" target. See NVIDIA/aicr#1650.
const signingConfigTarget = "signing_config_rekor_v2.v0.2.json"

// GetSigningConfig returns the Sigstore signing config that targets Rekor v2,
// read from the local TUF cache (ForceCache, no network). The cache is populated
// by Update ("aicr trust update"). It is the sign-side counterpart to
// GetTrustedMaterial: the trusted root answers "is this signature valid?", the
// signing config answers "which Rekor/TSA endpoints do I sign to?". Both are
// resolved offline and refreshed only on explicit update.
func GetSigningConfig() (*root.SigningConfig, error) {
	client, err := newCacheTUFClient()
	if err != nil {
		return nil, err
	}
	return signingConfigFromClient(client)
}

// newCacheTUFClient builds a ForceCache (offline) TUF client. Shared by the
// signing-config cache readers so the construction and its error classification
// stay in one place.
func newCacheTUFClient() (*tuf.Client, error) {
	client, err := tuf.New(tuf.DefaultOptions().WithForceCache())
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to initialize TUF client", err)
	}
	return client, nil
}

// ResolveSigningConfig returns the Rekor v2 signing config, preferring the local
// TUF cache (offline) and falling back to a bounded network TUF fetch when the
// cache is cold. Because AICR signs to Rekor v2 by default, signers must succeed
// without a prior explicit "aicr trust update"; signing is already an online
// operation, so a one-time fetch of the endpoint config is acceptable and it
// also warms the cache for next time. Bounded by defaults.TUFUpdateTimeout.
func ResolveSigningConfig(ctx context.Context) (*root.SigningConfig, error) {
	if sc, err := GetSigningConfig(); err == nil {
		return sc, nil
	}
	// Cold cache: fetch from the TUF CDN. Mirrors Update's bounded-goroutine
	// pattern because the underlying tuf.New / Refresh calls do not take context.
	ctx, cancel := context.WithTimeout(ctx, defaults.TUFUpdateTimeout)
	defer cancel()

	slog.Info("signing config not cached; fetching via TUF...")

	type result struct {
		sc  *root.SigningConfig
		err error
	}
	ch := make(chan result, 1)
	go func() {
		client, err := tuf.New(tuf.DefaultOptions())
		if err != nil {
			ch <- result{err: errors.Wrap(errors.ErrCodeInternal, "failed to initialize TUF client for signing config fetch", err)}
			return
		}
		if refreshErr := client.Refresh(); refreshErr != nil {
			code := classifyTUFError(refreshErr)
			ch <- result{err: errors.Wrap(code, "TUF refresh failed while fetching signing config", refreshErr)}
			return
		}
		sc, err := signingConfigFromClient(client)
		ch <- result{sc: sc, err: err}
	}()

	select {
	case <-ctx.Done():
		return nil, errors.Wrap(errors.ErrCodeTimeout, "signing config fetch timed out", ctx.Err())
	case r := <-ch:
		return r.sc, r.err
	}
}

// SigningConfigJSON returns the raw, TUF-verified bytes of the Rekor v2 signing
// config from the local cache (ForceCache, no network). Use this to materialize
// the config to a file for tools that take a signing-config path (e.g. `cosign
// attest-blob --signing-config`); GetSigningConfig returns the parsed form for
// in-process signing. The cache is populated by Update.
func SigningConfigJSON() ([]byte, error) {
	client, err := newCacheTUFClient()
	if err != nil {
		return nil, err
	}
	return signingConfigTargetBytes(client)
}

// signingConfigTargetBytes fetches the raw signing-config target from a TUF
// client (cache read on ForceCache, download-and-cache on a network client) and
// classifies the error consistently. Shared by SigningConfigJSON (raw bytes) and
// signingConfigFromClient (parsed).
func signingConfigTargetBytes(client *tuf.Client) ([]byte, error) {
	scJSON, err := client.GetTarget(signingConfigTarget)
	if err != nil {
		code := classifyTUFError(err)
		msg := "failed to get signing config from TUF"
		switch code { //nolint:exhaustive // only the three codes classifyTUFError can return are interesting
		case errors.ErrCodeUnavailable:
			msg = "failed to get signing config from TUF (transport error)"
		case errors.ErrCodeUnauthorized:
			msg = "failed to get signing config from TUF (signature or verification error)"
		}
		return nil, errors.Wrap(code, msg, err)
	}
	return scJSON, nil
}

// signingConfigFromClient loads and parses the signing config target from a TUF
// client.
func signingConfigFromClient(client *tuf.Client) (*root.SigningConfig, error) {
	scJSON, err := signingConfigTargetBytes(client)
	if err != nil {
		return nil, err
	}

	sc, err := root.NewSigningConfigFromJSON(scJSON)
	if err != nil {
		// Bytes came from the verified TUF target / cache, not user input — a
		// parse failure means the cache is corrupt or the payload changed shape.
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to parse signing config", err)
	}
	return sc, nil
}

// Update fetches the latest Sigstore trusted root via TUF CDN
// and updates the local cache. Bounded by defaults.TUFUpdateTimeout
// (longer than a single-request HTTPClientTimeout because TUF refreshes
// download multiple metadata files from a CDN).
//
// Known limitation: the underlying tuf.New / client.Refresh calls do not
// accept context, so on ctx.Done() we return an error but the goroutine
// continues running in the background until the network operation
// completes naturally. This is acceptable for the CLI-only call sites
// today (the goroutine is reaped on process exit). If callers from a
// long-running daemon are added, switch to a TUF client that supports
// context cancellation.
func Update(ctx context.Context) (root.TrustedMaterial, error) {
	ctx, cancel := context.WithTimeout(ctx, defaults.TUFUpdateTimeout)
	defer cancel()

	slog.Info("fetching latest Sigstore trusted root via TUF...")

	type updateResult struct {
		material root.TrustedMaterial
		err      error
	}

	ch := make(chan updateResult, 1)
	go func() {
		opts := tuf.DefaultOptions()

		client, err := tuf.New(opts)
		if err != nil {
			// tuf.New only performs local config setup (parses the embedded
			// root, computes cache paths). Failures here are configuration
			// or filesystem problems, not network — Internal, not Unavailable.
			ch <- updateResult{err: errors.Wrap(errors.ErrCodeInternal, "failed to initialize TUF client for update", err)}
			return
		}

		if refreshErr := client.Refresh(); refreshErr != nil {
			// Distinguish transport errors (server unreachable, HTTP failure)
			// from verification errors (signature, hash, expiry) using
			// go-tuf's typed error sentinels. Operators get a more
			// actionable code: Unavailable for "try again later",
			// Unauthorized for "trust chain broke; root may need update",
			// Internal for repository/runtime faults that aren't either.
			code := classifyTUFError(refreshErr)
			msg := "TUF refresh failed"
			switch code { //nolint:exhaustive // only the three codes classifyTUFError can return are interesting
			case errors.ErrCodeUnavailable:
				msg = "TUF refresh failed (transport error)"
			case errors.ErrCodeUnauthorized:
				msg = "TUF refresh failed (signature or expiry verification)"
			}
			ch <- updateResult{err: errors.Wrap(code, msg, refreshErr)}
			return
		}

		material, err := trustedMaterialFromClient(client)
		if err == nil {
			// Warm the Rekor v2 signing config target into the same TUF cache so
			// signers can read it offline via GetSigningConfig. Best-effort: this
			// is additive to trust update's primary job (the trusted root), so a
			// fetch failure (e.g. a target-name change) warns rather than fails
			// the whole update — a signer that needs it gets a clear error from
			// GetSigningConfig at sign time.
			if _, scErr := signingConfigFromClient(client); scErr != nil {
				slog.Warn("signing config not refreshed during trust update",
					"error", scErr, "target", signingConfigTarget)
			}
		}
		ch <- updateResult{material: material, err: err}
	}()

	select {
	case <-ctx.Done():
		return nil, errors.Wrap(errors.ErrCodeTimeout, "TUF update timed out", ctx.Err())
	case result := <-ch:
		if result.err != nil {
			return nil, result.err
		}
		if result.material == nil {
			return nil, errors.New(errors.ErrCodeInternal, "TUF update returned nil trusted material")
		}

		slog.Info("trusted root updated successfully",
			"fulcio_cas", len(result.material.FulcioCertificateAuthorities()),
			"rekor_logs", len(result.material.RekorLogs()),
		)

		return result.material, nil
	}
}

// trustedMaterialFromClient loads the trusted root from a TUF client.
func trustedMaterialFromClient(client *tuf.Client) (root.TrustedMaterial, error) {
	// GetTarget can fail with transport, download, or verification errors.
	// Classify with the same helper used by the refresh path so operators
	// see the right code (Unavailable for retryable transport, Unauthorized
	// for signature/expiry, Internal for repository/runtime faults).
	trustedRootJSON, err := client.GetTarget("trusted_root.json")
	if err != nil {
		code := classifyTUFError(err)
		msg := "failed to get trusted root from TUF"
		switch code { //nolint:exhaustive // only the three codes classifyTUFError can return are interesting
		case errors.ErrCodeUnavailable:
			msg = "failed to get trusted root from TUF (transport error)"
		case errors.ErrCodeUnauthorized:
			msg = "failed to get trusted root from TUF (signature or verification error)"
		}
		return nil, errors.Wrap(code, msg, err)
	}

	// The bytes came from the TUF target / local cache, not from user input —
	// a parse failure here means the cache is corrupt or the upstream payload
	// changed shape. Classify as Internal (5xx), not InvalidRequest (4xx).
	return parseTrustedRoot(trustedRootJSON, errors.ErrCodeInternal)
}

// parseTrustedRoot parses sigstore trusted_root.json bytes into TrustedMaterial.
// parseErrCode classifies structural failures: ErrCodeInternal for bytes aicr
// produced (the TUF cache target), ErrCodeInvalidRequest for a user-supplied
// --trust-root file.
func parseTrustedRoot(data []byte, parseErrCode errors.ErrorCode) (root.TrustedMaterial, error) {
	var trustedRootPB prototrustroot.TrustedRoot
	if err := protojson.Unmarshal(data, &trustedRootPB); err != nil {
		return nil, errors.Wrap(parseErrCode, "failed to parse trusted root", err)
	}
	trustedRoot, err := root.NewTrustedRootFromProtobuf(&trustedRootPB)
	if err != nil {
		return nil, errors.Wrap(parseErrCode, "invalid trusted root", err)
	}
	return trustedRoot, nil
}

// LoadTrustedMaterialFromFile reads a sigstore-go trusted_root.json from a
// user-supplied path and returns its TrustedMaterial, for verifying bundles
// signed against a private Fulcio/Rekor (aicr verify --trust-root). The read is
// bounded by defaults.MaxTrustedRootBytes so an attacker-influenced path cannot
// OOM the process, and the path must be a regular file so a FIFO or device node
// cannot block the read indefinitely. A missing/unreadable/non-regular/oversized/
// malformed file is a user error (ErrCodeInvalidRequest), unlike the TUF-cache
// path which is Internal.
func LoadTrustedMaterialFromFile(path string) (root.TrustedMaterial, error) {
	// Open with O_NONBLOCK and validate the resulting descriptor, rather than
	// stat-ing the pathname first. O_NONBLOCK means opening a FIFO/device never
	// blocks in the kernel open() (a regular-file read ignores it), and
	// inspecting the opened descriptor with fstat closes the stat/open TOCTOU
	// window: a pathname swapped between a pre-open stat and the open could
	// otherwise slip a non-regular file (or a blocking FIFO) past the guard. The
	// size cap below only bounds bytes once reads start.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // user-supplied verifier input, regular-file checked on the descriptor + bounded below
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to open trust root file", err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to stat trust root file", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "trust root file must be a regular file")
	}

	limited := io.LimitReader(f, defaults.MaxTrustedRootBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to read trust root file", err)
	}
	if int64(len(data)) > defaults.MaxTrustedRootBytes {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "trust root file exceeds size limit")
	}
	return parseTrustedRoot(data, errors.ErrCodeInvalidRequest)
}
