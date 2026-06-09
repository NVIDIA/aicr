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
	stderrors "errors"
	"strings"

	"github.com/sigstore/sigstore/pkg/signature/kms"
	"github.com/sigstore/sigstore/pkg/signature/options"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// resolveKMSPublicKey looks up a cosign-style KMS key URI
// (awskms:// | gcpkms:// | azurekms://), reads its public half, and returns
// both the live SignerVerifier seam and the resolved public key. It is shared
// by KMS signing (kmsidentity.go, which uses the returned signer for
// SignDigest) and KMS verification (which needs only the public key) so both
// agree on provider resolution and error classification.
//
// SHA-256 is fixed as the digest algorithm: the cloud KMS defaults
// (ECDSA P-256 / RSA-2048) all sign over a SHA-256 digest.
//
// kms.Get fails for two distinct reasons that must not be conflated at the HTTP
// boundary (the server path, #1150). An unrecognized scheme (or a missing
// out-of-process plugin) surfaces as ProviderNotFoundError and is a client
// error — the request named a key we cannot handle (ErrCodeInvalidRequest). A
// matched provider that then fails to initialize (credential resolution,
// network, IMDS) is an upstream-availability failure, not a bad request
// (ErrCodeUnavailable).
//
// The PublicKey RPC is read eagerly here, where ctx is in scope, so it honors
// the caller's deadline instead of falling back to a background context inside
// a downstream adapter whose Public() takes no context.
func resolveKMSPublicKey(ctx context.Context, keyURI string) (kmsRemoteSigner, crypto.PublicKey, error) {
	// Bound the provider lookup + PublicKey RPC so a caller that passes an
	// unbounded context cannot hang here. When invoked from SignStatementWith
	// the parent context is already bounded by SigstoreSignTimeout, so this
	// nests harmlessly (the tighter deadline wins).
	ctx, cancel := context.WithTimeout(ctx, defaults.SigstoreSignTimeout)
	defer cancel()

	// kms.Get matches provider schemes case-sensitively, so normalize the scheme
	// first: a variant like GCPKMS:// would otherwise fail to resolve.
	keyURI = normalizeURIScheme(keyURI)

	sv, err := kms.Get(ctx, keyURI, crypto.SHA256)
	if err != nil {
		if te := kmsTimeoutError(err, keyURI); te != nil {
			return nil, nil, te
		}
		var notFound *kms.ProviderNotFoundError
		if stderrors.As(err, &notFound) {
			return nil, nil, errors.WrapWithContext(errors.ErrCodeInvalidRequest,
				"unsupported or unrecognized KMS signing-key scheme", err,
				map[string]interface{}{ctxKeySigningKey: keyURI})
		}
		return nil, nil, errors.WrapWithContext(errors.ErrCodeUnavailable,
			"failed to initialize KMS provider for signing key", err,
			map[string]interface{}{ctxKeySigningKey: keyURI})
	}

	pub, err := sv.PublicKey(options.WithContext(ctx))
	if err != nil {
		if te := kmsTimeoutError(err, keyURI); te != nil {
			return nil, nil, te
		}
		return nil, nil, errors.WrapWithContext(errors.ErrCodeUnavailable,
			"failed to read KMS public key", err,
			map[string]interface{}{ctxKeySigningKey: keyURI})
	}

	return sv, pub, nil
}

// normalizeURIScheme lowercases the scheme component of a URI (the part before
// "://"), leaving the remainder untouched. RFC 3986 schemes are
// case-insensitive, but some consumers (e.g. kms.Get) match them
// case-sensitively. The opaque part after "://" (which may be a case-sensitive
// ARN or resource path) is preserved. Returns uri unchanged when it has no
// "://" separator or begins with one.
func normalizeURIScheme(uri string) string {
	if i := strings.Index(uri, "://"); i > 0 {
		return strings.ToLower(uri[:i]) + uri[i:]
	}
	return uri
}

// kmsTimeoutError classifies a context deadline or cancellation surfaced by a
// KMS RPC as ErrCodeTimeout (504 at the HTTP boundary), distinct from the
// ErrCodeUnavailable used for provider-init / network failures. It mirrors the
// deadline handling in SignStatementWith. Returns nil when err is not a context
// timeout or cancellation.
func kmsTimeoutError(err error, keyURI string) error {
	if stderrors.Is(err, context.DeadlineExceeded) || stderrors.Is(err, context.Canceled) {
		return errors.WrapWithContext(errors.ErrCodeTimeout,
			"KMS operation timed out", err,
			map[string]interface{}{ctxKeySigningKey: keyURI})
	}
	return nil
}
