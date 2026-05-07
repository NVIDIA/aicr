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
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/sigstore/sigstore/pkg/oauthflow"
)

// FetchAmbientOIDCToken retrieves an OIDC identity token from the GitHub Actions
// ambient credential endpoint. This is used for keyless Fulcio signing in CI.
//
// Parameters:
//   - requestURL: the ACTIONS_ID_TOKEN_REQUEST_URL environment variable
//   - requestToken: the ACTIONS_ID_TOKEN_REQUEST_TOKEN environment variable
func FetchAmbientOIDCToken(ctx context.Context, requestURL, requestToken string) (string, error) {
	if requestURL == "" {
		return "", errors.New(errors.ErrCodeInvalidRequest, "OIDC request URL is empty")
	}

	ctx, cancel := context.WithTimeout(ctx, defaults.HTTPClientTimeout)
	defer cancel()

	u, err := url.Parse(requestURL)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to parse OIDC request URL", err)
	}
	q := u.Query()
	q.Set("audience", "sigstore")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to create OIDC request", err)
	}

	req.Header.Set("Authorization", "Bearer "+requestToken)

	client := defaults.NewHTTPClient(0)
	resp, err := client.Do(req) //nolint:gosec // URL is from ACTIONS_ID_TOKEN_REQUEST_URL (trusted GitHub Actions env var)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeUnavailable, "OIDC token request failed", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, defaults.MaxErrorBodySize))
		msg := "OIDC token request returned " + resp.Status + ": " + string(body)
		if readErr != nil {
			return "", errors.Wrap(errors.ErrCodeUnavailable, msg, readErr)
		}
		return "", errors.New(errors.ErrCodeUnavailable, msg)
	}

	var result struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, defaults.MaxErrorBodySize)).Decode(&result); err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to decode OIDC token response", err)
	}

	if result.Value == "" {
		return "", errors.New(errors.ErrCodeInternal, "OIDC token response contained empty value")
	}

	return result.Value, nil
}

// Sigstore public-good OIDC configuration.
const (
	SigstoreOIDCIssuer = "https://oauth2.sigstore.dev/auth"
	SigstoreClientID   = "sigstore"
)

// FetchInteractiveOIDCToken opens a browser for the user to authenticate with
// a Sigstore-supported identity provider (GitHub, Google, or Microsoft) and
// returns an OIDC identity token.
//
// msgOut receives any user-facing prompts emitted by the OIDC handshake (for
// example the OOB fallback URL). Pass os.Stderr for typical CLI behavior, or
// io.Discard to suppress prompts in tests / non-interactive callers. A nil
// writer is treated as io.Discard so the package never silently writes to
// stdout/stderr.
func FetchInteractiveOIDCToken(ctx context.Context, msgOut io.Writer) (string, error) {
	if msgOut == nil {
		msgOut = io.Discard
	}
	// Clone the package singleton so we inherit any default fields the
	// upstream may add later (HTMLPage today; potentially more), then
	// overwrite only what we need to inject.
	getter := *oauthflow.DefaultIDTokenGetter
	getter.Output = msgOut
	return runOIDCConnect(ctx, "interactive", &getter,
		"opening browser for Sigstore OIDC authentication...")
}

// FetchDeviceCodeOIDCToken authenticates the user against Sigstore's public-good
// OIDC issuer using the OAuth 2.0 Device Authorization Grant (RFC 8628). The
// user is shown a verification URL and code to enter on a separate device,
// which makes the flow work on headless hosts (no local browser callback).
//
// msgOut receives the verification URL, user code, and progress messages from
// the device handshake. See FetchInteractiveOIDCToken for the writer contract.
func FetchDeviceCodeOIDCToken(ctx context.Context, msgOut io.Writer) (string, error) {
	if msgOut == nil {
		msgOut = io.Discard
	}
	getter := oauthflow.NewDeviceFlowTokenGetterForIssuer(SigstoreOIDCIssuer)
	getter.MessagePrinter = func(s string) {
		_, _ = fmt.Fprintln(msgOut, s)
	}
	return runOIDCConnect(ctx, "device-code", getter,
		"starting Sigstore OIDC device-code authentication...")
}

// runOIDCConnect drives oauthflow.OIDConnect with the given TokenGetter under
// a context deadline. oauthflow.OIDConnect does not accept a context, so the
// call is run in a goroutine and a select cancels on timeout.
//
// Cancellation is honored before any background work starts (so a pre-canceled
// caller never spawns a browser/device-code handshake), and context.Canceled
// is reported separately from deadline expiry so callers can tell explicit
// cancellation apart from a real timeout.
//
// Known limitation: once the goroutine is launched, it cannot be canceled
// because oauthflow.OIDConnect is context-unaware (vendored upstream). After
// ctx.Done() the wrapper returns immediately, but the goroutine continues
// running until the HTTP layer times out on its own. This is acceptable for
// the CLI (the process exits shortly after), but long-lived callers should
// expect a brief background-resource overhang on cancel/timeout. Removing it
// requires either a context-aware fork of sigstore/oauthflow or replacing the
// dependency with a custom OIDC client.
func runOIDCConnect(ctx context.Context, label string, getter oauthflow.TokenGetter, startMsg string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", classifyOIDCContextError(label, err)
	}

	ctx, cancel := context.WithTimeout(ctx, defaults.OIDCAuthTimeout)
	defer cancel()

	slog.Info(startMsg)

	type oidcResult struct {
		token *oauthflow.OIDCIDToken
		err   error
	}

	ch := make(chan oidcResult, 1)
	go func() {
		token, err := oauthflow.OIDConnect(
			SigstoreOIDCIssuer,
			SigstoreClientID,
			"",
			"",
			getter,
		)
		ch <- oidcResult{token: token, err: err}
	}()

	select {
	case <-ctx.Done():
		return "", classifyOIDCContextError(label, ctx.Err())
	case result := <-ch:
		if result.err != nil {
			return "", errors.Wrap(errors.ErrCodeUnavailable, label+" OIDC authentication failed", result.err)
		}
		if result.token == nil || result.token.RawString == "" {
			return "", errors.New(errors.ErrCodeInternal, "OIDC authentication returned empty token")
		}
		slog.Info("authenticated successfully", "subject", result.token.Subject)
		return result.token.RawString, nil
	}
}

// classifyOIDCContextError wraps a context error into the appropriate
// pkg/errors structured error code. context.Canceled is a deliberate
// caller-side cancel (mapped to Unavailable so it's not mistaken for a
// service timeout); anything else — typically context.DeadlineExceeded
// — is treated as a true timeout.
func classifyOIDCContextError(label string, err error) error {
	if stderrors.Is(err, context.Canceled) {
		return errors.Wrap(errors.ErrCodeUnavailable,
			label+" OIDC authentication canceled", err)
	}
	return errors.Wrap(errors.ErrCodeTimeout,
		label+" OIDC authentication timed out", err)
}
