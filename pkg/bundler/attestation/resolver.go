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

	// PromptWriter receives user-facing prompts emitted by the interactive
	// and device-code flows (verification URL + short code). Pass os.Stderr
	// for typical CLI behavior, io.Discard to suppress, or nil (treated as
	// io.Discard).
	PromptWriter io.Writer
}

// ResolveAttester returns the Attester implementation selected by opts.
//
// OIDC source precedence (highest first):
//  1. IdentityToken — explicit pre-fetched token.
//  2. AmbientURL+AmbientToken — GitHub Actions ambient OIDC.
//  3. DeviceFlow — RFC 8628 device-code flow.
//  4. Interactive browser flow (default).
//
// Errors from the OIDC helpers are returned as-is to preserve their
// pkg/errors classification (timeout / unavailable / internal).
func ResolveAttester(ctx context.Context, opts ResolveOptions) (Attester, error) {
	if !opts.Attest {
		return NewNoOpAttester(), nil
	}

	// 1. Pre-fetched identity token.
	if opts.IdentityToken != "" {
		slog.Info("using pre-fetched OIDC identity token for attestation")
		return NewKeylessAttester(opts.IdentityToken), nil
	}

	// 2. Ambient OIDC (GitHub Actions).
	if opts.AmbientURL != "" && opts.AmbientToken != "" {
		oidcToken, err := FetchAmbientOIDCToken(ctx, opts.AmbientURL, opts.AmbientToken)
		if err != nil {
			return nil, err
		}
		return NewKeylessAttester(oidcToken), nil
	}

	// 3. Device-code flow — works on headless hosts.
	if opts.DeviceFlow {
		oidcToken, err := FetchDeviceCodeOIDCToken(ctx, opts.PromptWriter)
		if err != nil {
			return nil, err
		}
		return NewKeylessAttester(oidcToken), nil
	}

	// 4. Interactive browser flow (default).
	slog.Info("no ambient OIDC token, attempting interactive authentication")
	oidcToken, err := FetchInteractiveOIDCToken(ctx, opts.PromptWriter)
	if err != nil {
		return nil, err
	}
	return NewKeylessAttester(oidcToken), nil
}
