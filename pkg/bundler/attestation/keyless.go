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
	"log/slog"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/sigstore/sigstore-go/pkg/sign"
	"google.golang.org/protobuf/encoding/protojson"
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
func (k *KeylessAttester) Attest(ctx context.Context, subject AttestSubject) ([]byte, error) {
	// Build in-toto statement
	statementJSON, err := BuildStatement(subject, StatementMetadata{
		BuilderID: k.identity,
	})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to build attestation statement", err)
	}

	// Wrap as DSSE content (in-toto envelope)
	content := &sign.DSSEData{
		Data:        statementJSON,
		PayloadType: "application/vnd.in-toto+json",
	}

	// Create ephemeral keypair (ECDSA P-256, single-use)
	keypair, err := sign.NewEphemeralKeypair(nil)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create ephemeral keypair", err)
	}

	slog.Debug("signing bundle attestation",
		"fulcio", k.fulcioURL,
		"rekor", k.rekorURL,
	)

	// Sign with Fulcio certificate + Rekor transparency log
	bundle, err := sign.Bundle(content, keypair, sign.BundleOptions{
		CertificateProvider: sign.NewFulcio(&sign.FulcioOptions{
			BaseURL: k.fulcioURL,
		}),
		CertificateProviderOptions: &sign.CertificateProviderOptions{
			IDToken: k.oidcToken,
		},
		TransparencyLogs: []sign.Transparency{
			sign.NewRekor(&sign.RekorOptions{
				BaseURL: k.rekorURL,
			}),
		},
		Context: ctx,
	})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "sigstore signing failed", err)
	}

	// Marshal bundle to JSON (Sigstore bundle format)
	bundleJSON, err := protojson.Marshal(bundle)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to marshal sigstore bundle", err)
	}

	slog.Info("bundle attestation signed successfully")

	return bundleJSON, nil
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
