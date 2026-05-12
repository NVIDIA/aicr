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
	"encoding/asn1"
	"log/slog"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	"github.com/sigstore/sigstore-go/pkg/sign"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// SignOptions controls cosign keyless OIDC signing for an in-toto
// Statement. Empty Fulcio/Rekor URLs fall back to Sigstore's
// public-good defaults.
type SignOptions struct {
	// OIDCToken is the ambient OIDC token used to obtain a Fulcio
	// signing certificate. Required.
	OIDCToken string

	// FulcioURL is the Fulcio certificate authority URL. Empty falls
	// back to DefaultFulcioURL.
	FulcioURL string

	// RekorURL is the Rekor transparency log URL. Empty falls back to
	// DefaultRekorURL.
	RekorURL string
}

// SignedAttestation is the result of SignStatement: a Sigstore bundle
// (DSSE envelope wrapping the in-toto Statement, plus Fulcio cert and
// Rekor inclusion proof) serialized as protobuf-JSON, together with
// the identity claims extracted from the Fulcio cert.
type SignedAttestation struct {
	// BundleJSON is the serialized Sigstore bundle in protobuf-JSON
	// form. Writable directly to an attestation.sigstore.json file
	// or pushable to an OCI artifact.
	BundleJSON []byte

	// Identity is the SAN claim from the Fulcio cert (email for
	// interactive OIDC, URI for workload OIDC). Empty if extraction
	// failed.
	Identity string

	// Issuer is the OIDC issuer URL recorded in the Fulcio cert
	// (Fulcio-specific X.509 extension). Empty if extraction failed.
	Issuer string

	// RekorLogIndex is the Rekor transparency-log inclusion-proof log
	// index. 0 if no Rekor entry exists.
	RekorLogIndex int64
}

// SignStatement DSSE-wraps an in-toto Statement and signs it via
// cosign keyless OIDC (Fulcio certificate issuance + Rekor
// transparency log entry). It is the predicate-agnostic signing
// primitive shared between `aicr bundle --attest` (SLSA Provenance v1
// predicate) and `aicr validate --emit-attestation` (recipe-evidence
// v1 predicate); callers construct statementJSON according to their
// own predicate type.
//
// statementJSON must be the protobuf-JSON-encoded intoto.Statement
// bytes — typically the output of attestation.BuildStatement for the
// bundle path, or of evidence/attestation.BuildStatement for the
// recipe-evidence path. SignStatement does not interpret the
// predicate.
//
// The returned SignedAttestation carries the Sigstore bundle plus the
// signer-identity claims extracted from the Fulcio certificate so
// callers do not need to re-parse the cert.
func SignStatement(ctx context.Context, statementJSON []byte, opts SignOptions) (*SignedAttestation, error) {
	if len(statementJSON) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "empty statement")
	}
	if opts.OIDCToken == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "OIDC token is required for keyless signing")
	}

	fulcioURL := opts.FulcioURL
	if fulcioURL == "" {
		fulcioURL = DefaultFulcioURL
	}
	rekorURL := opts.RekorURL
	if rekorURL == "" {
		rekorURL = DefaultRekorURL
	}

	content := &sign.DSSEData{
		Data:        statementJSON,
		PayloadType: "application/vnd.in-toto+json",
	}

	keypair, err := sign.NewEphemeralKeypair(nil)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create ephemeral keypair", err)
	}

	slog.Debug("signing in-toto statement", "fulcio", fulcioURL, "rekor", rekorURL)

	bundle, err := sign.Bundle(content, keypair, sign.BundleOptions{
		CertificateProvider: sign.NewFulcio(&sign.FulcioOptions{BaseURL: fulcioURL}),
		CertificateProviderOptions: &sign.CertificateProviderOptions{
			IDToken: opts.OIDCToken,
		},
		TransparencyLogs: []sign.Transparency{
			sign.NewRekor(&sign.RekorOptions{BaseURL: rekorURL}),
		},
		Context: ctx,
	})
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "sigstore signing failed", err)
	}

	bundleJSON, err := protojson.Marshal(bundle)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to marshal sigstore bundle", err)
	}

	identity, issuer := extractSignerClaims(bundle)
	rekorIndex := extractRekorLogIndex(bundle)

	slog.Info("in-toto statement signed", "identity", identity, "rekorLogIndex", rekorIndex)

	return &SignedAttestation{
		BundleJSON:    bundleJSON,
		Identity:      identity,
		Issuer:        issuer,
		RekorLogIndex: rekorIndex,
	}, nil
}

// extractSignerClaims parses the Fulcio cert and returns the SAN
// identity (email or URI) plus the OIDC issuer URL.
// Returns empty strings when the certificate or claims are absent.
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
		slog.Debug("failed to parse signing certificate for claim extraction", "error", err)
		return "", ""
	}

	// Fulcio certificates encode identity as SAN: email for interactive OIDC,
	// URI for workload identity (GitHub Actions OIDC).
	if len(parsed.EmailAddresses) > 0 {
		identity = parsed.EmailAddresses[0]
	} else if len(parsed.URIs) > 0 {
		identity = parsed.URIs[0].String()
	}

	// Fulcio embeds the OIDC issuer in extension OID 1.3.6.1.4.1.57264.1.1
	// (legacy) and 1.3.6.1.4.1.57264.1.8 (current). Try both.
	issuer = extractIssuerExtension(parsed)
	return identity, issuer
}

// extractIssuerExtension reads the Fulcio OIDC-issuer claim from the
// signing certificate. Two extensions encode it:
//   - Legacy OID 1.3.6.1.4.1.57264.1.1: raw UTF-8 bytes.
//   - Current OID 1.3.6.1.4.1.57264.1.8: DER-encoded ASN.1 UTF8String
//     (per https://github.com/sigstore/fulcio/blob/main/docs/oid-info.md).
//
// Treating both as raw bytes corrupts current-OID values by leaving the
// ASN.1 tag/length prefix in the returned string.
func extractIssuerExtension(cert *x509.Certificate) string {
	const (
		legacy  = "1.3.6.1.4.1.57264.1.1"
		current = "1.3.6.1.4.1.57264.1.8"
	)
	for _, ext := range cert.Extensions {
		switch ext.Id.String() {
		case current:
			var decoded string
			if _, err := asn1.Unmarshal(ext.Value, &decoded); err != nil {
				slog.Debug("failed to ASN.1-decode Fulcio issuer extension", "oid", current, "error", err)
				return ""
			}
			return decoded
		case legacy:
			return string(ext.Value)
		}
	}
	return ""
}

// extractRekorLogIndex returns the first Rekor transparency-log entry's
// LogIndex from a Sigstore bundle, or 0 if none.
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
