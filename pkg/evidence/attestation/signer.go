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
	"crypto/x509"
	"log/slog"
	"os"
	"path/filepath"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	"github.com/sigstore/sigstore-go/pkg/sign"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// Sigstore public-good instance URLs. Mirrored from
// pkg/bundler/attestation; intentionally duplicated to keep this
// package independent of the Helm-bundle attestation surface (the two
// have different predicate types and may evolve independently).
const (
	DefaultFulcioURL = "https://fulcio.sigstore.dev"
	DefaultRekorURL  = "https://rekor.sigstore.dev"
)

// SignResult describes a successful sign() call.
type SignResult struct {
	// BundleJSON is the serialized Sigstore bundle (.sigstore.json),
	// equivalent to a DSSE-wrapped in-toto Statement plus verification
	// material (Fulcio cert, Rekor inclusion proof).
	BundleJSON []byte

	// Identity is the OIDC subject claim from the Fulcio cert
	// (e.g., contributor email or GitHub Actions workflow URI).
	Identity string

	// Issuer is the OIDC issuer URL recorded in the Fulcio cert.
	Issuer string

	// RekorLogIndex is the Rekor inclusion-proof log index, or 0 when
	// no Rekor entry was created (NoOpSigner).
	RekorLogIndex int64
}

// Signer signs the in-toto Statement carrying the v1 recipe-evidence
// predicate. Implementations:
//
//   - KeylessSigner — Sigstore public-good keyless OIDC.
//   - NoOpSigner    — no-op; returns BundleJSON=nil for tests.
//
// The statementJSON parameter is the unsigned bytes from
// attestation.BuildStatement; the signer DSSE-wraps it and produces
// a Sigstore bundle.
type Signer interface {
	Sign(ctx context.Context, statementJSON []byte) (*SignResult, error)
}

// KeylessSigner signs via Fulcio + Rekor with the supplied OIDC token.
// Token discovery is the caller's responsibility (env, ambient, etc.).
type KeylessSigner struct {
	OIDCToken string
	FulcioURL string
	RekorURL  string
}

// NewKeylessSigner returns a signer wired to Sigstore public-good URLs.
func NewKeylessSigner(oidcToken string) *KeylessSigner {
	return &KeylessSigner{
		OIDCToken: oidcToken,
		FulcioURL: DefaultFulcioURL,
		RekorURL:  DefaultRekorURL,
	}
}

// Sign DSSE-wraps and signs the statement.
//
//nolint:unparam // *SignResult is part of the Signer interface; tests exercise error paths only.
func (k *KeylessSigner) Sign(ctx context.Context, statementJSON []byte) (*SignResult, error) {
	if len(statementJSON) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "empty statement")
	}
	if k.OIDCToken == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "OIDC token is required for keyless signing")
	}

	content := &sign.DSSEData{
		Data:        statementJSON,
		PayloadType: "application/vnd.in-toto+json",
	}

	keypair, err := sign.NewEphemeralKeypair(nil)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create ephemeral keypair", err)
	}

	slog.Debug("signing recipe-evidence attestation", "fulcio", k.FulcioURL, "rekor", k.RekorURL)

	bundle, err := sign.Bundle(content, keypair, sign.BundleOptions{
		CertificateProvider: sign.NewFulcio(&sign.FulcioOptions{BaseURL: k.FulcioURL}),
		CertificateProviderOptions: &sign.CertificateProviderOptions{
			IDToken: k.OIDCToken,
		},
		TransparencyLogs: []sign.Transparency{
			sign.NewRekor(&sign.RekorOptions{BaseURL: k.RekorURL}),
		},
		Context: ctx,
	})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "sigstore signing failed", err)
	}

	identity, issuer := extractSignerClaims(bundle)
	bundleJSON, err := protojson.Marshal(bundle)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to marshal sigstore bundle", err)
	}

	rekorIndex := extractRekorLogIndex(bundle)

	slog.Info("recipe evidence signed", "identity", identity, "rekorLogIndex", rekorIndex)

	return &SignResult{
		BundleJSON:    bundleJSON,
		Identity:      identity,
		Issuer:        issuer,
		RekorLogIndex: rekorIndex,
	}, nil
}

// NoOpSigner returns BundleJSON=nil with no signing performed. Used
// in tests and when the operator runs `aicr validate --emit-attestation`
// without `--push`: the unsigned statement is left in the bundle
// directory so a follow-up `cosign attest` invocation can sign it.
type NoOpSigner struct{}

// Sign returns an empty SignResult. The caller is responsible for
// writing or skipping attestation.intoto.jsonl appropriately.
func (NoOpSigner) Sign(_ context.Context, _ []byte) (*SignResult, error) {
	return &SignResult{}, nil
}

// SignBundle signs the bundle's StatementJSON, writes
// attestation.intoto.jsonl into the summary directory, and returns
// the SignResult so callers can populate the pointer's signer block.
//
// When the signer is a NoOpSigner, the attestation file is not
// written; the bundle ships with the unsigned statement available as
// Bundle.StatementJSON only.
//
//nolint:unparam // *SignResult feeds the pointer file's signer block; tests exercise only error paths.
func SignBundle(ctx context.Context, b *Bundle, s Signer) (*SignResult, error) {
	if b == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "bundle is required")
	}
	if s == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "signer is required (use NoOpSigner for unsigned)")
	}

	res, err := s.Sign(ctx, b.StatementJSON)
	if err != nil {
		return nil, err
	}
	if len(res.BundleJSON) == 0 {
		return res, nil
	}
	out := filepath.Join(b.SummaryDir, AttestationFilename)
	if err := os.WriteFile(out, res.BundleJSON, 0o600); err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to write signed attestation", err)
	}
	return res, nil
}

// extractSignerClaims parses the Fulcio cert and returns the SAN
// identity (email or URI) plus the OIDC issuer URL claim.
// Returns empty strings when the certificate or claims are not present.
func extractSignerClaims(bundle *protobundle.Bundle) (identity, issuer string) {
	if bundle.GetVerificationMaterial() == nil {
		return "", ""
	}

	var certDER []byte
	if cert := bundle.GetVerificationMaterial().GetCertificate(); cert != nil {
		certDER = cert.GetRawBytes()
	} else if chain := bundle.GetVerificationMaterial().GetX509CertificateChain(); chain != nil {
		certs := chain.GetCertificates()
		if len(certs) > 0 {
			certDER = certs[0].GetRawBytes()
		}
	}
	if len(certDER) == 0 {
		return "", ""
	}

	parsed, err := x509.ParseCertificate(certDER)
	if err != nil {
		slog.Debug("failed to parse signing certificate for identity extraction", "error", err)
		return "", ""
	}

	if len(parsed.EmailAddresses) > 0 {
		identity = parsed.EmailAddresses[0]
	} else if len(parsed.URIs) > 0 {
		identity = parsed.URIs[0].String()
	}

	// Fulcio embeds the OIDC issuer in extension OID 1.3.6.1.4.1.57264.1.1
	// (legacy) and 1.3.6.1.4.1.57264.1.8 (current). We try both.
	issuer = extractIssuerExtension(parsed)
	return identity, issuer
}

func extractIssuerExtension(cert *x509.Certificate) string {
	const (
		legacy  = "1.3.6.1.4.1.57264.1.1"
		current = "1.3.6.1.4.1.57264.1.8"
	)
	for _, ext := range cert.Extensions {
		if ext.Id.String() == current || ext.Id.String() == legacy {
			return string(ext.Value)
		}
	}
	return ""
}

// extractRekorLogIndex returns the first Rekor transparency-log
// entry's LogIndex from a Sigstore bundle, or 0 if none.
func extractRekorLogIndex(bundle *protobundle.Bundle) int64 {
	vm := bundle.GetVerificationMaterial()
	if vm == nil {
		return 0
	}
	entries := vm.GetTlogEntries()
	if len(entries) == 0 {
		return 0
	}
	return entries[0].GetLogIndex()
}
