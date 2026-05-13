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

	"github.com/NVIDIA/aicr/pkg/errors"
)

// Sigstore public-good instance URLs.
const (
	DefaultFulcioURL = "https://fulcio.sigstore.dev"
	DefaultRekorURL  = "https://rekor.sigstore.dev"
)

// KeylessAttester signs bundle content using Sigstore keyless OIDC signing
// (Fulcio for certificates, Rekor for transparency logging).
type KeylessAttester struct {
	oidcToken string
	fulcioURL string
	rekorURL  string
	identity  string
}

// NewKeylessAttester returns a new KeylessAttester configured for Sigstore
// public-good infrastructure.
func NewKeylessAttester(oidcToken string) *KeylessAttester {
	return &KeylessAttester{
		oidcToken: oidcToken,
		fulcioURL: DefaultFulcioURL,
		rekorURL:  DefaultRekorURL,
	}
}

// Attest creates a DSSE-signed in-toto SLSA provenance statement for the
// given subject using keyless OIDC signing via Fulcio and Rekor.
// Returns the Sigstore bundle as serialized JSON.
//
// The Sigstore-signing plumbing (DSSE wrap, ephemeral keypair, Fulcio
// cert, Rekor entry, claim extraction) is delegated to SignStatement
// so other packages can call the same primitive directly with their
// own predicate types.
func (k *KeylessAttester) Attest(ctx context.Context, subject AttestSubject) ([]byte, error) {
	metadata := subject.Metadata
	metadata.BuilderID = k.identity
	statementJSON, err := BuildStatement(subject, metadata)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to build attestation statement", err)
	}

	res, err := SignStatement(ctx, statementJSON, SignOptions{
		OIDCToken: k.oidcToken,
		FulcioURL: k.fulcioURL,
		RekorURL:  k.rekorURL,
	})
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
