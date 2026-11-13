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
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"

	"github.com/sigstore/sigstore-go/pkg/root"
)

// KeylessAttester signs bundle content using Sigstore keyless OIDC signing
// (Fulcio for certificates, Rekor for transparency logging).
type KeylessAttester struct {
	oidcToken           string
	fulcioURL           string
	rekorURL            string
	signingConfigPath   string
	useTUFSigningConfig bool
	signingConfig       *root.SigningConfig
	signStatement       func(context.Context, func(signer string) ([]byte, error), SignOptions) (*SignedAttestation, error)
	identity            string
}

// NewKeylessAttester returns a new KeylessAttester targeting the given Fulcio
// and Rekor endpoints. Empty fulcioURL or rekorURL falls back to the Sigstore
// public-good default, so callers that do not run private infrastructure pass
// "" for both. Non-empty values point signing at a private Sigstore instance
// (issue #408).
//
// signingConfigPath, when non-empty, is the path to a Sigstore SigningConfig
// (e.g. the TUF-distributed signing_config_rekor_v2 target) that drives
// transparency-log and timestamp-authority selection; it takes precedence over
// useTUFSigningConfig and rekorURL. useTUFSigningConfig selects the Rekor v2
// signing config from the local TUF cache instead. Both are how signing targets
// Rekor v2 (issue #1650).
func NewKeylessAttester(oidcToken, fulcioURL, rekorURL, signingConfigPath string, useTUFSigningConfig bool) *KeylessAttester {
	if fulcioURL == "" {
		fulcioURL = defaults.SigstoreFulcioURL
	}
	if rekorURL == "" {
		rekorURL = defaults.SigstoreRekorURL
	}
	return &KeylessAttester{
		oidcToken:           oidcToken,
		fulcioURL:           fulcioURL,
		rekorURL:            rekorURL,
		signingConfigPath:   signingConfigPath,
		useTUFSigningConfig: useTUFSigningConfig,
		signStatement:       SignStatement,
	}
}

// NewKeylessAttesterFromOptions builds a KeylessAttester from a ResolveOptions.
//
// Preferred over NewKeylessAttester for anything driven by a ResolveOptions:
// the positional form cannot carry ResolveOptions.SigningConfig, so a caller
// that validated a config before signing would silently fall back to re-reading
// SigningConfigPath — the exact check-then-reload gap that field exists to
// close. Adding a signing field here also cannot be dropped by one call site,
// matching the SignOptionsFromResolve contract.
func NewKeylessAttesterFromOptions(oidcToken string, o ResolveOptions) *KeylessAttester {
	a := NewKeylessAttester(oidcToken, o.FulcioURL, o.RekorURL, o.SigningConfigPath, o.UseTUFSigningConfig)
	a.signingConfig = o.SigningConfig
	return a
}

// resolveOptions projects the attester's signing target back into a
// ResolveOptions for SignOptionsFromResolve.
//
// Split out of Attest so the round trip is reachable from a test without
// signing anything. Every field dropped here is a field the attester silently
// stops honoring — SigningConfig in particular, whose whole purpose is that the
// config validated before signing is the one signed with.
func (k *KeylessAttester) resolveOptions() ResolveOptions {
	return ResolveOptions{
		FulcioURL:           k.fulcioURL,
		RekorURL:            k.rekorURL,
		SigningConfigPath:   k.signingConfigPath,
		UseTUFSigningConfig: k.useTUFSigningConfig,
		SigningConfig:       k.signingConfig,
	}
}

// Attest returns the Sigstore bundle, as JSON, for a DSSE-signed in-toto SLSA
// provenance statement about subject. It signs with keyless OIDC through
// Fulcio and Rekor. The statement's builder ID is the signing certificate's
// email or URI SAN, and Attest fails when the certificate has neither. It
// returns an ErrCodeInvalidRequest error for an invalid subject without
// requesting a certificate.
func (k *KeylessAttester) Attest(ctx context.Context, subject AttestSubject) ([]byte, error) {
	if err := validateSubject(subject); err != nil {
		return nil, err
	}
	build := func(signer string) ([]byte, error) {
		metadata := subject.Metadata
		metadata.BuilderID = signer
		statementJSON, err := BuildStatement(subject, metadata)
		if err != nil {
			return nil, errors.Wrap(errors.ErrCodeInternal, "failed to build attestation statement", err)
		}
		return statementJSON, nil
	}

	res, err := k.signStatement(ctx, build, SignOptionsFromResolve(k.oidcToken, k.resolveOptions()))
	if err != nil {
		return nil, err
	}

	k.identity = res.Identity
	// Identity is PII for interactive OIDC; surface it at Debug only,
	// matching the SignStatement contract. Callers that need the value
	// for audit logs read it back via Identity().
	slog.Info("bundle attestation signed successfully")
	slog.Debug("bundle attestation signer", "identity", k.identity)
	return res.BundleJSON, nil
}

// Identity returns the attester's identity. This is populated from the
// signing certificate after a successful Attest() call. Before signing,
// returns empty string.
func (k *KeylessAttester) Identity() string {
	return k.identity
}

// HasRekorEntry returns true — keyless attestations always include a
// Rekor transparency log entry.
func (k *KeylessAttester) HasRekorEntry() bool {
	return true
}
