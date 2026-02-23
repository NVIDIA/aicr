// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"net"
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

	client := &http.Client{
		Timeout: defaults.HTTPClientTimeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   defaults.HTTPConnectTimeout,
				KeepAlive: defaults.HTTPKeepAlive,
			}).DialContext,
			TLSHandshakeTimeout:   defaults.HTTPTLSHandshakeTimeout,
			ResponseHeaderTimeout: defaults.HTTPResponseHeaderTimeout,
			IdleConnTimeout:       defaults.HTTPIdleConnTimeout,
			ExpectContinueTimeout: defaults.HTTPExpectContinueTimeout,
		},
	}
	resp, err := client.Do(req) //nolint:gosec // URL is from ACTIONS_ID_TOKEN_REQUEST_URL (trusted GitHub Actions env var)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeUnavailable, "OIDC token request failed", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", errors.New(errors.ErrCodeUnavailable,
			"OIDC token request returned "+resp.Status+": "+string(body))
	}

	var result struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
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
func FetchInteractiveOIDCToken() (string, error) {
	slog.Info("opening browser for Sigstore OIDC authentication...")

	token, err := oauthflow.OIDConnect(
		SigstoreOIDCIssuer,
		SigstoreClientID,
		"",
		"",
		oauthflow.DefaultIDTokenGetter,
	)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeUnavailable, "interactive OIDC authentication failed", err)
	}

	if token.RawString == "" {
		return "", errors.New(errors.ErrCodeInternal, "OIDC authentication returned empty token")
	}

	slog.Info("authenticated successfully", "subject", token.Subject)
	return token.RawString, nil
}
