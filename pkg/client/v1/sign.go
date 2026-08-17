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
	// so leaving that field false does not disable it. Every other field
	// (identity token, ambient OIDC, device flow, Fulcio/Rekor URLs,
	// signing config) is passed through as given.
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
// A signature that yields no bundle is treated as a failure rather than a
// silent success: it means the attester could not obtain an OIDC token.
func (c *Client) SignCatalog(ctx context.Context, opts CatalogSignOptions) (*CatalogSignResult, error) {
	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
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
	if result.BundleJSON == nil {
		return nil, errors.New(errors.ErrCodeUnauthorized,
			"attester produced no Sigstore bundle (is an OIDC token available?)")
	}
	return &CatalogSignResult{Digest: result.Digest, BundleJSON: result.BundleJSON}, nil
}
