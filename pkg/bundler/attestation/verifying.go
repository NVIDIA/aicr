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
	"strings"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// VerificationIdentity is the verification dual of SigningIdentity: it supplies
// the trust anchors and the signer-matching policy for one verification flavor.
// It is one of the two composable axes of VerifyStatementWith; pair it with a
// VerifyTransparencyPolicy.
//
// The keyless implementation lives in keylessverifyidentity.go; it loads the
// Sigstore public-good trusted root and pins the signer via a Fulcio
// certificate identity. Key-based identities (#1152, KMS public key) follow in
// a later task and supply a public key instead of a certificate matcher.
type VerificationIdentity interface {
	// TrustedMaterial returns the trust anchors (Fulcio CAs, Rekor keys, or a
	// bare public key) used to validate the bundle. Resolved with ctx so a
	// slow trust-root load honors the caller's deadline; the error is already
	// classified by the implementation.
	TrustedMaterial(ctx context.Context) (root.TrustedMaterial, error)

	// PolicyOption returns the sigstore-go policy option that binds the
	// verification to a signer: WithCertificateIdentity for keyless, WithKey
	// for key-based.
	PolicyOption() verify.PolicyOption
}

// VerifyTransparencyPolicy is the dual of TransparencyPolicy: it decides which
// transparency-log and timestamp guarantees a verification operation requires.
// It is the second of the two composable axes of VerifyStatementWith; pair it
// with a VerificationIdentity.
type VerifyTransparencyPolicy interface {
	// VerifierOptions returns the sigstore-go verifier options that encode the
	// transparency/timestamp requirements (e.g. WithTransparencyLog,
	// WithObserverTimestamps).
	VerifierOptions() []verify.VerifierOption
}

// VerifyStatementWith verifies a Sigstore bundle against the given identity and
// transparency policy, binding it to artifactDigest. It is the composable core
// dual of SignStatementWith, shared by keyless verification
// (verifier.verifySigstoreBundle / VerifyBinaryAttestation) and the key-based
// flow (#1152). Returns the signer identity (Fulcio SAN) on success; for
// key-based bundles there is no certificate and the returned identity is "".
func VerifyStatementWith(ctx context.Context, bundleBytes []byte,
	id VerificationIdentity, tlog VerifyTransparencyPolicy, artifactDigest []byte) (string, error) {

	if id == nil || tlog == nil {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			"verification identity and transparency policy are required")
	}
	// Refuse to verify without content binding — an empty digest would let a
	// valid signature over unrelated content masquerade as verified.
	if len(artifactDigest) == 0 {
		return "", errors.New(errors.ErrCodeInvalidRequest,
			"artifact digest is required for attestation verification")
	}
	if err := ctx.Err(); err != nil {
		return "", errors.Wrap(errors.ErrCodeTimeout, "context cancelled before attestation verification", err)
	}

	b, err := loadSigstoreBundleBytes(bundleBytes)
	if err != nil {
		return "", err
	}

	tm, err := id.TrustedMaterial(ctx)
	if err != nil {
		return "", err // already classified by the identity
	}

	v, err := verify.NewVerifier(tm, tlog.VerifierOptions()...)
	if err != nil {
		return "", errors.Wrap(errors.ErrCodeInternal, "failed to create sigstore verifier", err)
	}

	result, err := v.Verify(b, verify.NewPolicy(
		verify.WithArtifactDigest("sha256", artifactDigest),
		id.PolicyOption(),
	))
	if err != nil {
		// Detect staleness: if the error mentions certificate chain issues,
		// suggest updating the trusted root.
		if containsCertChainError(err.Error()) {
			return "", errors.New(errors.ErrCodeUnauthorized,
				"sigstore verification failed — the signing certificate may have been issued "+
					"by a CA not present in your trusted root. This usually means Sigstore rotated "+
					"their keys since your last update.\n\n  To fix: aicr trust update")
		}
		return "", errors.Wrap(errors.ErrCodeUnauthorized, "sigstore verification failed", err)
	}

	return signerIdentityFromResult(result), nil
}

// loadSigstoreBundleBytes parses protobuf-JSON bundle bytes into a sigstore-go
// Bundle. It is the parse half of verifier.loadSigstoreBundle, extracted here
// so both the verifier's bounded-read wrapper and VerifyStatementWith share one
// definition. Callers are responsible for bounding the size of bundleBytes.
func loadSigstoreBundleBytes(bundleBytes []byte) (*bundle.Bundle, error) {
	var pb protobundle.Bundle
	if err := protojson.Unmarshal(bundleBytes, &pb); err != nil {
		// Malformed bundle bytes are invalid caller-supplied input, not a server
		// fault: classify as ErrCodeInvalidRequest (400) rather than 500.
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to parse sigstore bundle", err)
	}

	b, err := bundle.NewBundle(&pb)
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid sigstore bundle", err)
	}

	return b, nil
}

// signerIdentityFromResult extracts the signer SAN (Subject Alternative Name)
// from a verification result's Fulcio certificate summary. Returns "" when the
// bundle was signed with a public key (no certificate) or the result is
// otherwise missing the certificate — callers must not treat "" as an error.
func signerIdentityFromResult(result *verify.VerificationResult) string {
	if result == nil || result.Signature == nil || result.Signature.Certificate == nil {
		return ""
	}
	return result.Signature.Certificate.SubjectAlternativeName
}

// containsCertChainError reports whether an error message indicates a
// certificate-chain verification failure, which typically means the local
// trusted root is stale (Sigstore rotated keys since the last "aicr trust
// update"). Lives here so both the signing-adjacent verification core and the
// verifier package share one definition.
func containsCertChainError(errMsg string) bool {
	// Match specific stale-root phrases only. A bare "x509" substring is too
	// broad: it also catches unrelated cert failures such as
	// "x509: certificate has expired", which "aicr trust update" cannot fix, and
	// would misdirect operators. Genuine stale-root errors still match the
	// phrases below (e.g. "x509: certificate signed by unknown authority").
	staleIndicators := []string{
		"certificate signed by unknown authority",
		"certificate chain",
		"unable to verify certificate",
		"root certificate",
	}
	lower := strings.ToLower(errMsg)
	for _, indicator := range staleIndicators {
		if strings.Contains(lower, indicator) {
			return true
		}
	}
	return false
}
