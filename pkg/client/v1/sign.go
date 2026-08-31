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

package aicr

import (
	"context"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/errors"
	evattest "github.com/NVIDIA/aicr/pkg/evidence/attestation"
	recipecat "github.com/NVIDIA/aicr/pkg/recipe/catalog"

	"github.com/sigstore/sigstore-go/pkg/root"
)

// The signing entry points below deliberately impose NO facade-level timeout,
// unlike their verification counterparts. Keyless OIDC can block on a human
// completing a browser or device-code flow, so a fixed cap would cut short a
// run that works today. The caller's context governs; pass one with a deadline
// for unattended use.

// EvidencePublishOptions configures Client.PublishEvidence.
type EvidencePublishOptions struct {
	// BundleDir is the on-disk evidence directory: either the output
	// directory an evidence-emitting validation run wrote (which holds
	// summary-bundle/ and receives pointer.yaml) or the summary-bundle/
	// directory itself. Required.
	BundleDir string

	// Push is the OCI reference the summary bundle is pushed to. Required
	// — a publish with nothing to push is a no-op.
	Push string

	// PlainHTTP forces HTTP for registry traffic (local-registry tests).
	PlainHTTP bool

	// InsecureTLS disables registry TLS verification (self-signed certs).
	InsecureTLS bool

	// NoSign pushes the bundle unsigned and writes a pointer with an empty
	// signer block, deferring the Fulcio/Rekor leg. No OIDC flow runs, so
	// OIDCResolve is ignored.
	NoSign bool

	// OIDCResolve carries keyless-signing token-resolution inputs,
	// consumed only when NoSign is false. Resolution is deferred until
	// adjacent to signing so Fulcio's nonce-binding window is respected.
	OIDCResolve OIDCResolveOptions
}

// PublishEvidence signs and pushes an already-emitted on-disk evidence bundle,
// then writes pointer.yaml beside it.
//
// It is the off-network second leg of the workflow whose first leg is an
// evidence-emitting validation run that did not push: that step produces the
// unsigned on-disk bundle this one consumes. Splitting them lets the
// cluster-bound step run where the cluster is reachable and the Sigstore-bound
// step run where Fulcio and Rekor are. The result is content-identical to the
// one-shot path, because the predicate signed here is read verbatim from the
// bundle's statement.intoto.json.
//
// Interactive keyless-signing disclosure is intentionally NOT performed here,
// matching EmitRecipeEvidence: prompting is a UI concern the caller owns, and
// this method must be able to run unattended from a server or library.
func (c *Client) PublishEvidence(ctx context.Context, opts EvidencePublishOptions) error {
	if c == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if opts.BundleDir == "" {
		return errors.New(errors.ErrCodeInvalidRequest, "evidence bundle directory is required")
	}
	if opts.Push == "" {
		return errors.New(errors.ErrCodeInvalidRequest,
			"push reference is required: publishing an evidence bundle without pushing it is a no-op")
	}
	if err := c.assertOpen(); err != nil {
		return err
	}

	// c.version is read without holding c.mu, unlike the snapshot in
	// SignCatalog and RecipeDigest. That is safe by the Client's own contract
	// — version is set once in NewClient and never mutated, as its field
	// comment states — and MergeReports reads it the same way. Those other two
	// methods take the lock for the DataProvider, which IS cleared by Close,
	// and pick up version while they are already holding it; this method needs
	// no provider, so there is no lock to piggyback on. Do NOT copy this
	// pattern for a field that Close mutates.
	err := evattest.Publish(ctx, evattest.PublishOptions{
		BundleDir:   opts.BundleDir,
		Push:        opts.Push,
		PlainHTTP:   opts.PlainHTTP,
		InsecureTLS: opts.InsecureTLS,
		NoSign:      opts.NoSign,
		AICRVersion: c.version,
		OIDCResolve: opts.OIDCResolve,
	})
	// Publish already returns coded pkg/errors; PropagateOrWrap preserves
	// those and only classifies an uncoded error that slips through.
	return errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to publish evidence bundle")
}

// CatalogSignOptions configures Client.SignCatalog.
type CatalogSignOptions struct {
	// Output is the path the Sigstore bundle is written to. Empty computes
	// and signs the digest but writes no file, leaving the serialized
	// bundle available on CatalogSignResult.BundleJSON.
	Output string

	// OIDCResolve carries the keyless-signing token-resolution inputs.
	// SignCatalog sets Attest itself — signing is the whole operation —
	// so leaving that field false does not disable it.
	//
	// Four fields are REJECTED rather than passed through, because
	// VerifyCatalog cannot verify what they would produce: SigningKey,
	// FulcioURL, RekorURL, and DisableTLogUpload. See SignCatalog's godoc
	// for the full statement of that constraint, including what it does
	// NOT cover.
	OIDCResolve OIDCResolveOptions
}

// CatalogSignResult is what SignCatalog returns on success.
type CatalogSignResult struct {
	// Digest is the hex-encoded SHA-256 of the combined catalog content
	// that was signed.
	Digest string

	// BundleJSON is the serialized Sigstore bundle. Never nil on a nil
	// error.
	BundleJSON []byte
}

// SignCatalog computes the deterministic digest over this Client's recipe
// catalog (registry.yaml plus validators/catalog.yaml), signs it via Sigstore
// keyless OIDC, and optionally writes the resulting bundle to opts.Output.
// VerifyCatalog is its counterpart.
//
// As with VerifyCatalog, the digest is computed over THIS Client's
// DataProvider, so what gets signed is the catalog the Client resolves with.
//
// # Signing modes are constrained to what VerifyCatalog can verify
//
// VerifyCatalog verifies against the public-good Sigstore root, requires a
// transparency-log entry, and pins the certificate to the GitHub Actions OIDC
// issuer. It exposes no key, no trust-root, and no offline option, because the
// recipe catalog is a release artifact NVIDIA signs — not something a consumer
// re-signs privately.
//
// SignCatalog therefore REJECTS the four OIDCResolve settings that would
// produce a signature its own counterpart could not check:
//
//   - SigningKey — a key-signed catalog has no verification path at all.
//   - FulcioURL — a certificate from a private CA does not chain to the
//     public-good root.
//   - RekorURL — an entry in a private transparency log cannot be verified
//     against the public-good root either. The flag can name a public-good v1
//     URL as well, which would verify, but the two are indistinguishable from
//     the URL alone, so this fails closed.
//   - DisableTLogUpload — verification requires a transparency-log entry.
//
// Each is rejected with ErrCodeInvalidRequest before any signing work runs, so
// the failure is immediate and explains itself rather than surfacing later as
// an unverifiable artifact.
//
// SigningConfigPath is checked rather than rejected. It passes through because
// the release path requires it — naming the public-good Rekor v2 target is its
// normal use — but a signing config can itself name a private Fulcio or Rekor,
// which would otherwise make the four rejections above bypassable by moving the
// same endpoints into a file. Every Fulcio, Rekor, OIDC-provider, and
// timestamp-authority URL in it must therefore be HTTPS under the sigstore.dev
// domain, matched on a label boundary so a lookalike host is rejected.
//
// The config that passes that check is the config signed with: the parsed value
// is handed to the signing path rather than re-read from the path, so the file
// cannot change between the check and the use.
//
// If private catalog signing is ever needed, both halves have to move
// together — widening this without widening VerifyCatalog is what this guard
// exists to prevent.
//
// A signature that yields no bundle is treated as a failure rather than a
// silent success: it means the attester could not obtain an OIDC token.
func (c *Client) SignCatalog(ctx context.Context, opts CatalogSignOptions) (*CatalogSignResult, error) {
	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	validatedSigningConfig, err := rejectUnverifiableCatalogSigning(opts.OIDCResolve)
	if err != nil {
		return nil, err
	}

	c.mu.RLock()
	if c.builder == nil {
		c.mu.RUnlock()
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	dp := c.dp
	clientVersion := c.version
	c.inflight.Add(1)
	c.mu.RUnlock()
	defer c.inflight.Done()

	resolve := opts.OIDCResolve
	resolve.Attest = true
	// Sign with the config that was validated, not a fresh read of the path.
	resolve.SigningConfig = validatedSigningConfig

	// Lazy resolution: Fulcio binds the certificate to a fresh nonce at
	// token-issue time, so a token resolved ahead of the first Attest call
	// can fail once the gap exceeds Fulcio's tolerance.
	attester, err := bundleattest.ResolveAttesterLazy(ctx, resolve)
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeUnauthorized, "could not resolve OIDC attester")
	}

	result, err := recipecat.Sign(ctx, dp, recipecat.SignOptions{
		Attester:    attester,
		Output:      opts.Output,
		ToolVersion: clientVersion,
	})
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "recipe catalog signing failed")
	}
	// The nil-result arm is defensive: Sign pairs every nil return with an
	// error today. Checking it anyway keeps a future (nil, nil) from turning
	// into a panic across a stability-guaranteed boundary, where a coded error
	// is the contract.
	if result == nil || result.BundleJSON == nil {
		return nil, errors.New(errors.ErrCodeUnauthorized,
			"attester produced no Sigstore bundle (is an OIDC token available?)")
	}
	return &CatalogSignResult{Digest: result.Digest, BundleJSON: result.BundleJSON}, nil
}

// rejectUnverifiableCatalogSigning enforces the sign/verify symmetry stated in
// SignCatalog's godoc: every signing mode it accepts must be one VerifyCatalog
// can check. Kept separate from SignCatalog so the invariant is testable
// without an OIDC token, which the signing path itself requires.
func rejectUnverifiableCatalogSigning(resolve OIDCResolveOptions) (*root.SigningConfig, error) {
	// settingKey names the offending field in the error's structured context so
	// a caller can branch on which setting to drop.
	const settingKey = "setting"

	switch {
	case resolve.SigningKey != "":
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"key-based catalog signing is not supported: VerifyCatalog verifies keyless GitHub OIDC certificates only, so a key-signed catalog could not be verified through the facade",
			map[string]any{settingKey: "OIDCResolve.SigningKey"})
	case resolve.FulcioURL != "":
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"catalog signing against a private Fulcio is not supported: VerifyCatalog verifies against the public-good Sigstore root, which a private CA's certificate does not chain to",
			map[string]any{settingKey: "OIDCResolve.FulcioURL"})
	case resolve.RekorURL != "":
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"catalog signing against an explicit Rekor URL is not supported: VerifyCatalog verifies transparency-log entries against the public-good root, and a private log's entries do not verify there. Use the Rekor v2 default, or SigningConfigPath",
			map[string]any{settingKey: "OIDCResolve.RekorURL"})
	case resolve.DisableTLogUpload:
		return nil, errors.NewWithContext(errors.ErrCodeInvalidRequest,
			"catalog signing without a transparency-log entry is not supported: VerifyCatalog requires one",
			map[string]any{settingKey: "OIDCResolve.DisableTLogUpload"})
	}

	// SigningConfigPath is not rejected outright -- it is the normal way to name
	// the public-good Rekor v2 target, and the release hook depends on it -- but
	// a signing config can itself name a private Fulcio or Rekor. Without this,
	// the four rejections above were bypassable by putting the same endpoints in
	// a file, and the catalog signed successfully while remaining unverifiable
	// by its documented counterpart.
	//
	// The parsed config is RETURNED, not discarded. Validating here and letting
	// the signing path re-read the same path would check one read and sign with
	// another; the caller sets ResolveOptions.SigningConfig so the bytes that
	// passed this check are the bytes signed with.
	if resolve.SigningConfigPath == "" {
		return nil, nil //nolint:nilnil // no config named: nothing to validate, nothing to pass on
	}

	sc, err := bundleattest.LoadSigningConfigForValidation(resolve.SigningConfigPath)
	if err != nil {
		return nil, err
	}
	if err := bundleattest.ValidateSigningConfigIsPublicGood(sc); err != nil {
		return nil, err
	}
	return sc, nil
}
