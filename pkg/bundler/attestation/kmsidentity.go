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
	"bytes"
	"context"
	"crypto"
	"io"
	"strings"

	"github.com/sigstore/sigstore-go/pkg/sign"
	"github.com/sigstore/sigstore/pkg/signature"
	"github.com/sigstore/sigstore/pkg/signature/options"

	// Blank-import the cosign-style KMS providers so their schemes
	// (awskms:// | gcpkms:// | azurekms://) register with kms.Get via package
	// init. Without these, kms.Get falls through to the plugin path and rejects
	// every built-in scheme. HashiCorp Vault (hashivault://) is intentionally
	// omitted: its client libraries are MPL-2.0, which this project's license
	// policy disallows. See issue #407.
	_ "github.com/sigstore/sigstore/pkg/signature/kms/aws"
	_ "github.com/sigstore/sigstore/pkg/signature/kms/azure"
	_ "github.com/sigstore/sigstore/pkg/signature/kms/gcp"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// ctxKeySigningKey is the structured-error context key carrying the KMS key
// URI on KMS resolution failures.
const ctxKeySigningKey = "signingKey"

// kmsURISchemes are the cosign-style KMS key-URI scheme prefixes this project
// supports. It is the single source of truth shared by the signing side
// (NewKMSIdentity / resolveKMSPublicKey) and the verifying side
// (NewKeyVerificationIdentity), so the two cannot drift on what counts as a
// "KMS URI". The set mirrors the blank-imported provider packages above;
// HashiCorp Vault (hashivault://) is intentionally omitted (MPL-2.0 license,
// see issue #407).
var kmsURISchemes = []string{"awskms://", "gcpkms://", "azurekms://"}

// isKMSURI reports whether ref names a supported cosign-style KMS key
// (awskms:// | gcpkms:// | azurekms://) rather than a local PEM path. It is the
// input-form discriminator both signing and verifying use; resolveKMSPublicKey
// still classifies the precise kms.Get failure (unknown scheme vs provider
// init) once a candidate is handed to the provider registry.
//
// Scheme matching is case-insensitive (RFC 3986 schemes are case-insensitive),
// so a variant like GCPKMS:// is routed to the KMS path rather than mistaken
// for a local file; resolveKMSPublicKey lowercases the scheme before kms.Get.
func isKMSURI(ref string) bool {
	lower := strings.ToLower(ref)
	for _, scheme := range kmsURISchemes {
		if strings.HasPrefix(lower, scheme) {
			return true
		}
	}
	return false
}

// kmsIdentity signs with a KMS-held key. It carries no Fulcio certificate, so
// SignStatementWith records public-key verification material and uses the key
// URI as the signer identity. See issue #407.
type kmsIdentity struct{ keyURI string }

// NewKMSIdentity returns a SigningIdentity backed by the cosign-style KMS URI
// (awskms:// | gcpkms:// | azurekms://). The provider and key are resolved
// lazily on first Keypair() call so a bad URI or missing provider credentials
// surface at sign time. HashiCorp Vault is unsupported (MPL-2.0 license).
func NewKMSIdentity(keyURI string) SigningIdentity { return &kmsIdentity{keyURI: keyURI} }

// Keypair resolves the KMS provider for the key URI, reads the public key, and
// returns a sign.Keypair backed by the KMS signing seam. SHA-256 is fixed as
// the bundle digest algorithm (the cloud KMS defaults — ECDSA P-256 / RSA-2048
// — all sign over a SHA-256 digest).
func (k *kmsIdentity) Keypair(ctx context.Context) (sign.Keypair, error) {
	// resolveKMSPublicKey (kmspublickey.go) bounds the provider lookup +
	// PublicKey RPC, classifies kms.Get failures (unrecognized scheme →
	// ErrCodeInvalidRequest, provider init failure → ErrCodeUnavailable), and
	// reads the public key eagerly under ctx. Signing additionally needs the
	// live SignerVerifier seam for SignDigest, so both halves are reused here
	// without a second kms.Get RPC. Verification (#1152) reuses the same
	// resolver but consumes only the public key.
	sv, pub, err := resolveKMSPublicKey(ctx, k.keyURI)
	if err != nil {
		return nil, err
	}

	return newKMSKeypairFromSigner(&kmsSignerVerifier{sv: sv, pub: pub})
}

// CertProvider returns (nil, nil): KMS signing has no Fulcio certificate, which
// drives sign.Bundle down its public-key verification-material path.
func (k *kmsIdentity) CertProvider() (sign.CertificateProvider, *sign.CertificateProviderOptions) {
	return nil, nil
}

// FallbackIdentity is the KMS key URI, used as the audit identity since there
// is no Fulcio SAN to extract.
func (k *kmsIdentity) FallbackIdentity() string { return k.keyURI }

// kmsRemoteSigner is the minimal subset of sigstore's kms.SignerVerifier the
// adapter actually uses. Narrowing the dependency (kms.SignerVerifier satisfies
// it) keeps kmsSignerVerifier testable with a small fake instead of a full KMS
// client.
type kmsRemoteSigner interface {
	PublicKey(opts ...signature.PublicKeyOption) (crypto.PublicKey, error)
	SignMessage(message io.Reader, opts ...signature.SignOption) ([]byte, error)
}

// kmsSignerVerifier adapts a sigstore/sigstore KMS SignerVerifier to the
// kmsSigner seam consumed by kmsKeypair. The public key is captured eagerly in
// Keypair (where the request context is available) so Public() needs no I/O.
type kmsSignerVerifier struct {
	sv  kmsRemoteSigner
	pub crypto.PublicKey
}

// Public returns the public key captured when the KMS key was resolved.
func (a *kmsSignerVerifier) Public() (crypto.PublicKey, error) { return a.pub, nil }

// SignDigest signs a precomputed SHA-256 digest. It passes the digest via
// options.WithDigest so the underlying signer signs it directly rather than
// re-hashing: ComputeDigestForSigning returns a supplied digest verbatim when
// its length matches the hash size, so the empty message reader is never read
// and no double-hashing occurs. The signature therefore verifies over
// sha256(data) — the exact bytes kmsKeypair.SignData records as the bundle
// message digest. WithCryptoSignerOpts(crypto.SHA256) pins the hash so the
// length check passes; WithContext threads the caller's deadline into the RPC.
func (a *kmsSignerVerifier) SignDigest(ctx context.Context, digest []byte) ([]byte, error) {
	sig, err := a.sv.SignMessage(
		bytes.NewReader(nil),
		options.WithDigest(digest),
		options.WithCryptoSignerOpts(crypto.SHA256),
		options.WithContext(ctx),
	)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "KMS signing call failed", err)
	}
	return sig, nil
}
