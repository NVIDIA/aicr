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

package verifier

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/trust"
)

// ErrUnsignedBundle is returned by VerifySignature when no Sigstore
// Bundle file is present in the bundle directory. Callers translate
// this to a Skipped step row.
var ErrUnsignedBundle = errors.New(errors.ErrCodeNotFound, "no signature attached (unsigned bundle)")

// SignatureResult is what a successful VerifySignature returns.
type SignatureResult struct {
	// Signer holds OIDC claims extracted from the verifying cert.
	Signer *SignerClaims

	// Predicate is the cryptographically anchored predicate body
	// extracted from the verified DSSE payload. Callers should prefer
	// this over the unsigned statement.intoto.json when present —
	// THIS is the value the signer attested to.
	Predicate *attestation.Predicate
}

// VerifySignature performs sigstore-go verification of the bundle's
// in-toto Statement signature. Returns ErrUnsignedBundle when no
// attestation.intoto.jsonl is present.
//
// For OCI inputs the subject digest in the signed Statement is locked
// to the actual pulled artifact digest — a mismatch means someone
// substituted the bundle and re-pointed at a different signature.
func VerifySignature(ctx context.Context, mat *MaterializedBundle, opts VerifyOptions) (*SignatureResult, error) {
	if mat == nil || mat.BundleDir == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "materialized bundle is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, errors.Wrap(errors.ErrCodeTimeout, "context canceled before signature verify", err)
	}

	sigPath := filepath.Join(mat.BundleDir, attestation.AttestationFilename)
	info, statErr := os.Stat(sigPath)
	if statErr != nil && os.IsNotExist(statErr) {
		return nil, ErrUnsignedBundle
	}
	if statErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to stat signature file", statErr)
	}
	if info.Size() > defaults.MaxSigstoreBundleSize {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"signature file exceeds maximum size — refusing to parse")
	}

	sigBundle, parseErr := loadSigstoreBundle(sigPath)
	if parseErr != nil {
		return nil, parseErr
	}

	stmtBytes, payloadErr := extractStatementBytes(sigBundle)
	if payloadErr != nil {
		return nil, payloadErr
	}

	subjectHex, predicate, parseStmtErr := parseStatement(stmtBytes)
	if parseStmtErr != nil {
		return nil, parseStmtErr
	}
	digestBytes, decodeErr := hex.DecodeString(subjectHex)
	if decodeErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"signed statement subject digest is not valid hex", decodeErr)
	}
	if mat.Digest != "" {
		want := strings.TrimPrefix(mat.Digest, "sha256:")
		if want != subjectHex {
			return nil, errors.New(errors.ErrCodeInvalidRequest,
				"signed subject digest ("+subjectHex+
					") does not match pulled artifact digest ("+want+")")
		}
	}

	trustedMaterial, trustErr := trust.GetTrustedMaterial()
	if trustErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to load trusted root", trustErr)
	}

	identity, idErr := buildIdentityMatcher(opts)
	if idErr != nil {
		return nil, idErr
	}

	v, vErr := verify.NewVerifier(trustedMaterial,
		verify.WithObserverTimestamps(1),
		verify.WithTransparencyLog(1),
	)
	if vErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create sigstore verifier", vErr)
	}

	result, verifyErr := v.Verify(sigBundle, verify.NewPolicy(
		verify.WithArtifactDigest("sha256", digestBytes),
		verify.WithCertificateIdentity(identity),
	))
	if verifyErr != nil {
		if isCertChainError(verifyErr.Error()) {
			return nil, errors.New(errors.ErrCodeUnauthorized,
				"sigstore verification failed — trusted root may be stale.\n\n  To fix: aicr trust update")
		}
		return nil, errors.New(errors.ErrCodeUnauthorized,
			"sigstore verification failed: "+sanitizeSigstoreError(verifyErr))
	}

	claims := &SignerClaims{}
	if result != nil && result.Signature != nil && result.Signature.Certificate != nil {
		claims.Identity = result.Signature.Certificate.SubjectAlternativeName
		claims.Issuer = result.Signature.Certificate.Issuer
	}
	if idx := rekorLogIndex(sigBundle); idx > 0 {
		i := idx
		claims.RekorLogIndex = &i
	}

	// Surface the no-pin footgun: without --expected-issuer or
	// --expected-identity-regexp, ANY Fulcio-issued cert from ANY OIDC
	// provider passes the identity policy. The signature is still
	// cryptographically valid, but the verifier hasn't said anything
	// about *who* signed. Operators reviewing the report need to know
	// that default verification accepts every signer.
	if opts.ExpectedIssuer == "" && opts.ExpectedIdentityRegexp == "" {
		slog.Warn("signature verified but no signer pinned — any Fulcio identity will pass",
			"identity", claims.Identity,
			"issuer", claims.Issuer,
			"hint", "consider --expected-issuer / --expected-identity-regexp to fail on unexpected signers")
	}

	return &SignatureResult{Signer: claims, Predicate: predicate}, nil
}

func buildIdentityMatcher(opts VerifyOptions) (verify.CertificateIdentity, error) {
	issuerLit, issuerRe := "", ".+"
	if opts.ExpectedIssuer != "" {
		issuerLit, issuerRe = opts.ExpectedIssuer, ""
	}
	idLit, idRe := "", ".+"
	if opts.ExpectedIdentityRegexp != "" {
		idLit, idRe = "", opts.ExpectedIdentityRegexp
	}
	identity, err := verify.NewShortCertificateIdentity(issuerLit, issuerRe, idLit, idRe)
	if err != nil {
		return verify.CertificateIdentity{}, errors.Wrap(errors.ErrCodeInternal,
			"failed to build certificate identity matcher", err)
	}
	return identity, nil
}

func loadSigstoreBundle(path string) (*bundle.Bundle, error) {
	data, err := os.ReadFile(path) //nolint:gosec // bundle-local path
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to read sigstore bundle", err)
	}
	var pb protobundle.Bundle
	if uErr := protojson.Unmarshal(data, &pb); uErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid sigstore bundle JSON", uErr)
	}
	b, ctorErr := bundle.NewBundle(&pb)
	if ctorErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "invalid sigstore bundle", ctorErr)
	}
	return b, nil
}

// extractStatementBytes pulls the DSSE payload bytes (the in-toto
// Statement JSON) out of a Sigstore Bundle. Handles both raw-JSON and
// base64-encoded payload shapes that upstream parsers may produce.
func extractStatementBytes(b *bundle.Bundle) ([]byte, error) {
	if b == nil || b.Bundle == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "nil bundle")
	}
	env := b.GetDsseEnvelope()
	if env == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "bundle has no DSSE envelope")
	}
	payload := env.GetPayload()
	if len(payload) == 0 {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "DSSE envelope has empty payload")
	}
	if looksLikeJSON(payload) {
		return payload, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(string(payload))
	if err != nil {
		return nil, errors.Wrap(errors.ErrCodeInvalidRequest, "failed to decode DSSE payload", err)
	}
	return decoded, nil
}

// parseStatement extracts the subject sha256 (hex, no prefix) plus the
// predicate body from an in-toto Statement JSON.
func parseStatement(stmtBytes []byte) (subjectHex string, predicate *attestation.Predicate, err error) {
	var stmt struct {
		Subject []struct {
			Digest map[string]string `json:"digest"`
		} `json:"subject"`
		PredicateType string                `json:"predicateType"`
		Predicate     attestation.Predicate `json:"predicate"`
	}
	if uErr := json.Unmarshal(stmtBytes, &stmt); uErr != nil {
		return "", nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"DSSE payload is not a valid in-toto Statement", uErr)
	}
	if len(stmt.Subject) == 0 {
		return "", nil, errors.New(errors.ErrCodeInvalidRequest, "Statement has no subject")
	}
	subjectHex = stmt.Subject[0].Digest["sha256"]
	if subjectHex == "" {
		return "", nil, errors.New(errors.ErrCodeInvalidRequest, "Statement subject has no sha256 digest")
	}
	if stmt.PredicateType != attestation.PredicateTypeV1 {
		return "", nil, errors.New(errors.ErrCodeInvalidRequest,
			"unexpected predicateType "+stmt.PredicateType)
	}
	return subjectHex, &stmt.Predicate, nil
}

func looksLikeJSON(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}

func rekorLogIndex(b *bundle.Bundle) int64 {
	if b == nil || b.Bundle == nil {
		return 0
	}
	vm := b.GetVerificationMaterial()
	if vm == nil {
		return 0
	}
	entries := vm.GetTlogEntries()
	if len(entries) == 0 {
		return 0
	}
	return entries[0].GetLogIndex()
}

// CrossCheckPointerSigner compares the verified signer claims against
// what the pointer file claimed. Returns nil when the pointer makes no
// signer claim, when there's no actual signer to compare against, or
// when every claimed field matches. A mismatch produces an error that
// names the specific field and both sides — the verifier surfaces it
// so a malicious pointer that names a different signer than the
// actual bundle fails loudly.
func CrossCheckPointerSigner(claimed *attestation.PointerSigner, actual *SignerClaims) error {
	if claimed == nil {
		return nil
	}
	if actual == nil {
		return errors.New(errors.ErrCodeInvalidRequest,
			"pointer claims a signer ("+claimed.Identity+
				", issuer "+claimed.Issuer+") but the bundle carries no signature")
	}
	if claimed.Identity != "" && claimed.Identity != actual.Identity {
		return errors.New(errors.ErrCodeInvalidRequest,
			"signer identity mismatch: pointer claims "+claimed.Identity+
				", cert says "+actual.Identity)
	}
	if claimed.Issuer != "" && claimed.Issuer != actual.Issuer {
		return errors.New(errors.ErrCodeInvalidRequest,
			"signer issuer mismatch: pointer claims "+claimed.Issuer+
				", cert says "+actual.Issuer)
	}
	if claimed.RekorLogIndex != nil {
		if actual.RekorLogIndex == nil {
			return errors.New(errors.ErrCodeInvalidRequest,
				"pointer claims a Rekor log index but the bundle has no Rekor entry "+
					"(was the bundle signed with --no-rekor, or is the pointer stale?)")
		}
		if *claimed.RekorLogIndex != *actual.RekorLogIndex {
			return errors.New(errors.ErrCodeInvalidRequest,
				"Rekor log index mismatch: pointer claims "+
					strconv.FormatInt(*claimed.RekorLogIndex, 10)+
					", actual entry "+strconv.FormatInt(*actual.RekorLogIndex, 10))
		}
	}
	return nil
}

// sanitizeSigstoreError strips Go format-string artifacts that
// sigstore-go produces when its threshold-not-met paths wrap an empty
// errors.Join(...) chain with %w. The literal "%!w(<nil>)" leaks into
// user-visible error messages; this helper removes it so the surface
// reads as "threshold not met for verified signed timestamps: 0 < 1"
// instead of "...: 0 < 1; error: %!w(<nil>)".
//
// Tracked upstream; safe to remove when sigstore-go's tsa.go and
// similar wraps guard against nil joins.
func sanitizeSigstoreError(err error) string {
	msg := err.Error()
	for _, suffix := range []string{
		"; error: %!w(<nil>)",
		": %!w(<nil>)",
		" %!w(<nil>)",
		"%!w(<nil>)",
	} {
		msg = strings.ReplaceAll(msg, suffix, "")
	}
	return msg
}

// isCertChainError reports whether the sigstore error string signals
// a stale trusted-root condition. Used to suggest `aicr trust update`.
func isCertChainError(msg string) bool {
	stale := []string{
		"certificate signed by unknown authority",
		"certificate chain",
		"x509",
		"unable to verify certificate",
		"root certificate",
	}
	lower := strings.ToLower(msg)
	for _, s := range stale {
		if strings.Contains(lower, s) {
			return true
		}
	}
	return false
}
