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
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"

	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	"github.com/sigstore/sigstore-go/pkg/sign"
	"github.com/sigstore/sigstore/pkg/cryptoutils"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// kmsSigner is the minimal signing seam the KMS keypair needs. Production
// wraps a sigstore/sigstore KMS SignerVerifier; tests inject a local signer.
type kmsSigner interface {
	// Public returns the signer's public key.
	Public() (crypto.PublicKey, error)
	// SignDigest signs a precomputed SHA-256 digest, returning an ASN.1
	// (DER) signature for ECDSA or PKCS#1 v1.5 for RSA — the encodings
	// sign.Bundle expects.
	SignDigest(ctx context.Context, digest []byte) ([]byte, error)
}

// kmsKeypair adapts a kmsSigner to sigstore-go's sign.Keypair so KMS-held
// keys flow through the same sign.Bundle path as ephemeral keys. SHA-256 is
// fixed; the public-key type drives GetKeyAlgorithm / GetSigningAlgorithm.
type kmsKeypair struct {
	signer kmsSigner
	pub    crypto.PublicKey
	keyAlg string
	sigAlg protocommon.PublicKeyDetails
	hint   []byte
}

// Compile-time assertion that kmsKeypair satisfies sign.Keypair.
var _ sign.Keypair = (*kmsKeypair)(nil)

func newKMSKeypairFromSigner(s kmsSigner) (*kmsKeypair, error) {
	pub, err := s.Public()
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeUnavailable, "failed to read KMS public key", err)
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to marshal KMS public key", err)
	}
	h := sha256.Sum256(der)
	keyAlg, sigAlg, err := classifyPublicKey(pub)
	if err != nil {
		return nil, err
	}
	return &kmsKeypair{
		signer: s,
		pub:    pub,
		keyAlg: keyAlg,
		sigAlg: sigAlg,
		hint:   []byte(base64.StdEncoding.EncodeToString(h[:])),
	}, nil
}

func (k *kmsKeypair) GetHashAlgorithm() protocommon.HashAlgorithm {
	return protocommon.HashAlgorithm_SHA2_256
}
func (k *kmsKeypair) GetSigningAlgorithm() protocommon.PublicKeyDetails { return k.sigAlg }
func (k *kmsKeypair) GetHint() []byte                                   { return k.hint }
func (k *kmsKeypair) GetKeyAlgorithm() string                           { return k.keyAlg }
func (k *kmsKeypair) GetPublicKey() crypto.PublicKey                    { return k.pub }

func (k *kmsKeypair) GetPublicKeyPem() (string, error) {
	pem, err := cryptoutils.MarshalPublicKeyToPEM(k.pub)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to PEM-encode KMS public key", err)
	}
	return string(pem), nil
}

// SignData hashes data with SHA-256, signs the digest via the KMS, and returns
// (signature, digest) — matching the sign.Keypair contract where the second
// value is the bytes recorded as the bundle's message digest.
func (k *kmsKeypair) SignData(ctx context.Context, data []byte) ([]byte, []byte, error) {
	digest := sha256.Sum256(data)
	sig, err := k.signer.SignDigest(ctx, digest[:])
	if err != nil {
		return nil, nil, errors.PropagateOrWrap(err, errors.ErrCodeUnavailable, "KMS sign failed")
	}
	return sig, digest[:], nil
}

// classifyPublicKey maps a public key to its sigstore key-algorithm label and
// PublicKeyDetails. Only ECDSA P-256 and RSA-2048 (the cloud KMS defaults) are
// supported; extend only when a real key needs it (YAGNI).
//
// It fails closed: an ECDSA key on a non-P-256 curve or an RSA key that is not
// 2048-bit is rejected rather than silently labeled with the wrong
// PublicKeyDetails, which would otherwise emit a bundle whose declared
// algorithm mismatches the key and fail verification.
//
// RSA-3072/4096 (supported by GCP KMS and Azure Key Vault, and sometimes
// required for compliance) are intentionally not enabled yet; add the
// corresponding PublicKeyDetails cases here when a concrete key needs them.
func classifyPublicKey(pub crypto.PublicKey) (string, protocommon.PublicKeyDetails, error) {
	switch key := pub.(type) {
	case *ecdsa.PublicKey:
		if key.Curve != elliptic.P256() {
			return "", protocommon.PublicKeyDetails_PUBLIC_KEY_DETAILS_UNSPECIFIED,
				errors.New(errors.ErrCodeInvalidRequest, "unsupported ECDSA curve: only P-256 KMS keys are supported")
		}
		return "ECDSA", protocommon.PublicKeyDetails_PKIX_ECDSA_P256_SHA_256, nil
	case *rsa.PublicKey:
		if key.N.BitLen() != 2048 {
			return "", protocommon.PublicKeyDetails_PUBLIC_KEY_DETAILS_UNSPECIFIED,
				errors.New(errors.ErrCodeInvalidRequest, "unsupported RSA key size: only 2048-bit KMS keys are supported")
		}
		return "RSA", protocommon.PublicKeyDetails_PKIX_RSA_PKCS1V15_2048_SHA256, nil
	default:
		return "", protocommon.PublicKeyDetails_PUBLIC_KEY_DETAILS_UNSPECIFIED,
			errors.New(errors.ErrCodeInvalidRequest, "unsupported KMS key type")
	}
}
