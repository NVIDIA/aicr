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
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"io"
	"os"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// keyVerificationIdentity verifies a public-key-signed bundle attestation
// (#407 KMS signing, or a local PEM). Trust anchors are the public-good root
// (so the Rekor tlog, which #407 uploads to by default, still verifies)
// combined with the caller's public key (for the signature). The policy uses
// verify.WithKey() instead of a certificate identity. It is the key-based
// VerificationIdentity, dual of kmsIdentity on the signing side.
type keyVerificationIdentity struct {
	pub      crypto.PublicKey
	identity string // KMS URI, or "pem:<sha256-fingerprint>" for a local key
	src      TrustedRootSource
}

// NewKeyVerificationIdentity resolves keyRef to a public key and returns the
// key-based VerificationIdentity. keyRef is a KMS key URI
// (awskms:// | gcpkms:// | azurekms://) or a path to a local PEM public-key
// file. This factory is the only input-form branch in the feature: a KMS URI
// is resolved via the shared resolveKMSPublicKey seam (so signing and verifying
// agree on provider resolution and error classification), and anything else is
// read as a local PEM. Scheme detection reuses isKMSURI (kmsidentity.go), the
// single source of truth shared with the signing side.
func NewKeyVerificationIdentity(ctx context.Context, keyRef string, src TrustedRootSource) (VerificationIdentity, error) {
	if src == nil {
		src = PublicGoodTrustedRoot
	}
	if isKMSURI(keyRef) {
		// Verification needs only the public half; the live signer seam is
		// discarded. resolveKMSPublicKey already classifies the failure
		// (unknown scheme → ErrCodeInvalidRequest, provider init →
		// ErrCodeUnavailable), so its error is propagated as-is.
		_, pub, err := resolveKMSPublicKey(ctx, keyRef)
		if err != nil {
			return nil, err
		}
		// Store the scheme-normalized URI so the identity (BundleCreator,
		// --require-creator matching) is canonical regardless of caller casing,
		// matching how resolveKMSPublicKey resolves the key.
		return &keyVerificationIdentity{pub: pub, identity: normalizeURIScheme(keyRef), src: src}, nil
	}

	pub, fp, err := loadPEMPublicKey(keyRef)
	if err != nil {
		return nil, err
	}
	return &keyVerificationIdentity{pub: pub, identity: "pem:" + fp, src: src}, nil
}

// Identity returns the key identity (KMS URI or pem:<fp>). The verifier
// substitutes this as BundleCreator since a key-signed bundle has no cert SAN.
func (k *keyVerificationIdentity) Identity() string { return k.identity }

// TrustedMaterial combines the trust anchors from src (the public-good root by
// default, or a union with a private trusted_root.json under
// `verify --trust-root`; both carry the Rekor tlog, which #407 uploads to by
// default) with a bare-public-key trust anchor (for the signature itself). The
// key is wrapped in an always-valid ExpiringKey (zero start/end) because a raw
// public key carries no notBefore/notAfter the way a Fulcio certificate does,
// so there is no validity window to enforce.
func (k *keyVerificationIdentity) TrustedMaterial(ctx context.Context) (root.TrustedMaterial, error) {
	base, err := k.src(ctx)
	if err != nil {
		return nil, err
	}
	ver, err := signature.LoadVerifier(k.pub, crypto.SHA256)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "unsupported public key", err)
	}
	keyMaterial := root.NewTrustedPublicKeyMaterial(func(string) (root.TimeConstrainedVerifier, error) {
		return root.NewExpiringKey(ver, time.Time{}, time.Time{}), nil
	})
	return root.TrustedMaterialCollection{base, keyMaterial}, nil
}

// PolicyOption binds verification to the public key rather than a Fulcio
// certificate identity, the key-based dual of WithCertificateIdentity.
func (k *keyVerificationIdentity) PolicyOption() verify.PolicyOption { return verify.WithKey() }

// loadPEMPublicKey reads a PEM-encoded public key from path and returns the
// parsed key plus the hex SHA-256 fingerprint of its DER (PKIX) encoding. The
// read is bounded by defaults.MaxPublicKeyPEMBytes so an attacker-influenced
// path (a /proc symlink, an NFS mount) cannot OOM the process the way
// os.ReadFile would. A missing file or unparseable content is a user error
// (a bad --key argument), classified ErrCodeInvalidRequest, mirroring how an
// unrecognized KMS scheme is treated.
func loadPEMPublicKey(path string) (crypto.PublicKey, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", errors.Wrap(errors.ErrCodeInvalidRequest, "failed to open public key file", err)
	}
	// Read-only handle: a deferred Close without an error check is acceptable
	// per project rules (Close errors only matter for writable handles).
	defer func() { _ = f.Close() }()

	limited := io.LimitReader(f, defaults.MaxPublicKeyPEMBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", errors.Wrap(errors.ErrCodeInvalidRequest, "failed to read public key file", err)
	}
	if int64(len(data)) > defaults.MaxPublicKeyPEMBytes {
		return nil, "", errors.New(errors.ErrCodeInvalidRequest, "public key file exceeds size limit")
	}

	pub, err := cryptoutils.UnmarshalPEMToPublicKey(data)
	if err != nil {
		return nil, "", errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse PEM public key", err)
	}

	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, "", errors.Wrap(errors.ErrCodeInvalidRequest, "failed to encode public key", err)
	}
	sum := sha256.Sum256(der)
	return pub, hex.EncodeToString(sum[:]), nil
}
