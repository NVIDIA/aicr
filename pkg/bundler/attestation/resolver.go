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
	"io"
	"log/slog"
	"sync"
)

// ResolveOptions selects an OIDC token source for keyless signing. Callers
// (CLI, API, tests) populate this from their own surface — flags, env vars,
// or hard-coded values — and ResolveAttester walks the precedence below
// without itself reading the runtime environment.
type ResolveOptions struct {
	// Attest gates attestation entirely. When false, ResolveAttester returns
	// a NoOpAttester regardless of the other fields.
	Attest bool

	// IdentityToken is a pre-fetched OIDC identity token (e.g., from
	// COSIGN_IDENTITY_TOKEN, a cloud workload-identity exchange, or another
	// cosign invocation). When non-empty it short-circuits all OIDC fetch
	// flows — the token is used as-is.
	IdentityToken string

	// AmbientURL and AmbientToken provide GitHub Actions ambient OIDC
	// credentials (the ACTIONS_ID_TOKEN_REQUEST_URL and
	// ACTIONS_ID_TOKEN_REQUEST_TOKEN env vars). Both must be non-empty to
	// activate the ambient branch.
	AmbientURL   string
	AmbientToken string

	// DeviceFlow opts in to the OAuth 2.0 Device Authorization Grant
	// (RFC 8628) for headless hosts where a browser callback is unavailable.
	DeviceFlow bool

	// FulcioURL and RekorURL point keyless signing at a private Sigstore
	// instance (a self-hosted Fulcio CA and/or Rekor transparency log).
	// Empty falls back to the public-good defaults (defaults.SigstoreFulcioURL
	// / defaults.SigstoreRekorURL); the two are independent, so an org can run
	// private Fulcio with public Rekor or vice versa. See issue #408.
	FulcioURL string
	RekorURL  string

	// SigningKey selects KMS-backed (key-based) signing instead of keyless OIDC.
	// When non-empty it is a cosign-style KMS URI (awskms:// | gcpkms:// |
	// azurekms://) and takes precedence over all OIDC source fields, which are
	// keyless-only. Mutual exclusivity with the keyless flags is enforced at the
	// CLI boundary (pkg/cli). See issue #407.
	SigningKey string

	// PromptWriter receives user-facing prompts emitted by the interactive
	// and device-code flows (verification URL + short code). Pass os.Stderr
	// for typical CLI behavior, io.Discard to suppress, or nil (treated as
	// io.Discard).
	PromptWriter io.Writer
}

// OIDCSourceKind identifies which keyless OIDC token source ResolveOIDCToken
// will consult for a given ResolveOptions. It is the single source of truth
// for the source-precedence decision, shared by ResolveOIDCToken and by
// callers (e.g. the CLI identity-disclosure gate) that must reason about
// whether an interactive login is about to open without driving the login
// themselves.
type OIDCSourceKind int

const (
	// OIDCSourceIdentityToken — a pre-fetched token (no login, no prompt).
	OIDCSourceIdentityToken OIDCSourceKind = iota
	// OIDCSourceAmbient — GitHub Actions ambient OIDC (no login, no prompt).
	OIDCSourceAmbient
	// OIDCSourceDeviceFlow — RFC 8628 device-code login (interactive).
	OIDCSourceDeviceFlow
	// OIDCSourceBrowser — interactive browser callback login (interactive).
	OIDCSourceBrowser
)

// Interactive reports whether the source opens a user-facing login (browser
// callback or device-code) that mints a Fulcio certificate from the signer's
// identity. The pre-fetched-token and ambient sources are non-interactive.
func (k OIDCSourceKind) Interactive() bool {
	return k == OIDCSourceDeviceFlow || k == OIDCSourceBrowser
}

// SelectOIDCSource reports which token source ResolveOIDCToken will use for
// opts, following the precedence below. It does not read the environment or
// perform any network/login work — it is a pure classification of opts.
//
// Precedence (highest first):
//  1. IdentityToken — explicit pre-fetched token.
//  2. AmbientURL+AmbientToken — GitHub Actions ambient OIDC (both required).
//  3. DeviceFlow — RFC 8628 device-code flow.
//  4. Interactive browser flow (default).
//
// KMS (SigningKey) signing is not an OIDC source: it is selected earlier, in
// ResolveAttester / ResolveAttesterLazy, before ResolveOIDCToken is reached,
// so it is intentionally outside this classifier.
func SelectOIDCSource(opts ResolveOptions) OIDCSourceKind {
	switch {
	case opts.IdentityToken != "":
		return OIDCSourceIdentityToken
	case opts.AmbientURL != "" && opts.AmbientToken != "":
		return OIDCSourceAmbient
	case opts.DeviceFlow:
		return OIDCSourceDeviceFlow
	default:
		return OIDCSourceBrowser
	}
}

// ResolveOIDCToken walks the OIDC source precedence chain and returns the
// resulting identity token string. Suitable for callers that build their
// own signer around a raw token and do not want the bundler's Attester
// abstraction. The branch taken is decided by SelectOIDCSource, so callers
// can predict it without driving a login.
//
// Errors from the OIDC helpers are returned as-is to preserve their
// pkg/errors classification (timeout / unavailable / internal). The
// function does not read the runtime environment itself — callers
// populate ResolveOptions from their own surface (flags, env vars).
func ResolveOIDCToken(ctx context.Context, opts ResolveOptions) (string, error) {
	switch SelectOIDCSource(opts) { //nolint:exhaustive // OIDCSourceBrowser is the default branch (the lowest-precedence fallback)
	case OIDCSourceIdentityToken:
		slog.Info("using pre-fetched OIDC identity token")
		return opts.IdentityToken, nil
	case OIDCSourceAmbient:
		return FetchAmbientOIDCToken(ctx, opts.AmbientURL, opts.AmbientToken)
	case OIDCSourceDeviceFlow:
		return FetchDeviceCodeOIDCToken(ctx, opts.PromptWriter)
	default: // OIDCSourceBrowser
		slog.Info("no ambient OIDC token, attempting interactive authentication")
		return FetchInteractiveOIDCToken(ctx, opts.PromptWriter)
	}
}

// ResolveAttester returns the Attester implementation selected by opts.
// Wraps ResolveOIDCToken with the NoOpAttester short-circuit for
// callers that gate attestation behind opts.Attest.
func ResolveAttester(ctx context.Context, opts ResolveOptions) (Attester, error) {
	if !opts.Attest {
		return NewNoOpAttester(), nil
	}
	if opts.SigningKey != "" {
		return NewKMSAttester(opts.SigningKey, opts.RekorURL), nil
	}
	token, err := ResolveOIDCToken(ctx, opts)
	if err != nil {
		return nil, err
	}
	return NewKeylessAttester(token, opts.FulcioURL, opts.RekorURL), nil
}

// ResolveAttesterLazy is the deferred-token variant of ResolveAttester.
// When opts.Attest is true the returned Attester resolves the OIDC token
// on first Attest() call, not at construction. Use this when there is a
// meaningful gap between attester setup and the first Attest() call:
// Fulcio binds the certificate to a fresh nonce at token-issue time, so a
// token resolved minutes ahead of signing can fail with
// "error processing the identity token" once the gap exceeds Fulcio's
// tolerance.
//
// The disabled (Attest=false) and NoOpAttester branches match
// ResolveAttester exactly so callers can swap entry points without
// changing the test surface.
//
//nolint:unparam // error return mirrors ResolveAttester so callers can swap entry points; reserved for future construction-time validation.
func ResolveAttesterLazy(_ context.Context, opts ResolveOptions) (Attester, error) {
	if !opts.Attest {
		return NewNoOpAttester(), nil
	}
	if opts.SigningKey != "" {
		return NewKMSAttester(opts.SigningKey, opts.RekorURL), nil
	}
	return NewLazyKeylessAttester(opts), nil
}

// LazyKeylessAttester defers OIDC token resolution to the first Attest()
// call. The underlying KeylessAttester is created on first use and cached
// for subsequent calls so a single attester produces consistent identity
// across the run.
//
// mu serializes lazy initialization (and the Identity() read) so the
// attester is safe to share across goroutines — bundler.Make does not
// invoke Attest concurrently today, but the Attester interface is held
// long enough across other call sites that defensive locking is cheaper
// than the next data-race bug.
type LazyKeylessAttester struct {
	opts ResolveOptions

	mu    sync.Mutex
	inner *KeylessAttester
}

// NewLazyKeylessAttester returns an Attester that resolves the OIDC token
// via the ResolveOIDCToken precedence chain on first Attest() call.
func NewLazyKeylessAttester(opts ResolveOptions) *LazyKeylessAttester {
	return &LazyKeylessAttester{opts: opts}
}

// Attest resolves the OIDC token on first call, then delegates to the
// cached KeylessAttester for this and every subsequent call. Resolver
// errors propagate as-is so the pkg/errors classification reaches the
// caller.
func (l *LazyKeylessAttester) Attest(ctx context.Context, subject AttestSubject) ([]byte, error) {
	l.mu.Lock()
	if l.inner == nil {
		token, err := ResolveOIDCToken(ctx, l.opts)
		if err != nil {
			l.mu.Unlock()
			return nil, err
		}
		l.inner = NewKeylessAttester(token, l.opts.FulcioURL, l.opts.RekorURL)
	}
	inner := l.inner
	l.mu.Unlock()
	return inner.Attest(ctx, subject)
}

// Identity returns the cached KeylessAttester's identity after the first
// successful Attest() call; empty string before that.
func (l *LazyKeylessAttester) Identity() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inner == nil {
		return ""
	}
	return l.inner.Identity()
}

// HasRekorEntry mirrors the eager attester: keyless signing always
// records a Rekor transparency log entry.
func (l *LazyKeylessAttester) HasRekorEntry() bool {
	return true
}
