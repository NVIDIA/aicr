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
	stderrors "errors"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/verify"
	"google.golang.org/protobuf/encoding/protojson"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/evidence/internal/boundedio"
)

// ErrUnsignedBundle is returned by VerifySignature when no Sigstore
// Bundle file is present in the bundle directory. Callers translate
// this to a Skipped step row.
var ErrUnsignedBundle = errors.New(errors.ErrCodeNotFound, "no signature attached (unsigned bundle)")

// SignatureResult is what a successful VerifySignature returns.
type SignatureResult struct {
	// Signer holds the verified signer: the OIDC claims from the verifying
	// cert, or the verifying key's fingerprint for a key signature.
	Signer *SignerClaims

	// Predicate is the cryptographically anchored predicate body
	// extracted from the verified DSSE payload. Callers should prefer
	// this over the unsigned statement.intoto.json when present —
	// THIS is the value the signer attested to.
	Predicate *attestation.Predicate

	// PredicateType is the predicateType recorded alongside Predicate in
	// the verified DSSE payload.
	PredicateType string
}

// signerPolicy is the resolved signer half of a VerifyOptions: the trust
// anchors, the key to verify against when Key is set, and whether a
// transparency-log entry is required. Resolving it once, before anything is
// pulled, lets an operator error (a bad --key or --trust-root, a conflicting
// flag) fail the run instead of reading as a verdict on the bundle.
type signerPolicy struct {
	src  bundleattest.TrustedRootSource
	tlog bundleattest.VerifyTransparencyPolicy

	// key and keyFingerprint are set only in key mode.
	key            bundleattest.VerificationIdentity
	keyFingerprint string
}

// resolveSignerPolicy validates the signer options and resolves the trust
// root and, in key mode, the public key. A KMS key URI makes a live
// GetPublicKey call here.
func resolveSignerPolicy(ctx context.Context, opts VerifyOptions) (*signerPolicy, error) {
	if opts.IgnoreTLog && opts.Key == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"ignoring the transparency log requires a key: "+
				"a keyless certificate cannot be verified without a trusted timestamp")
	}
	if opts.Key != "" && (opts.ExpectedIssuer != "" || opts.ExpectedIdentityRegexp != "") {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"the expected issuer and identity pins apply to keyless certificates; "+
				"a key already pins the signer, so they cannot be combined with it")
	}

	p := &signerPolicy{
		src:  bundleattest.PublicGoodTrustedRoot,
		tlog: bundleattest.NewRequireTLogPolicy(),
	}
	if opts.TrustRoot != "" {
		src, err := bundleattest.NewUnionTrustedRoot(opts.TrustRoot)
		if err != nil {
			return nil, err
		}
		p.src = src
	}
	if opts.IgnoreTLog {
		p.tlog = bundleattest.NewNoTLogVerifyPolicy()
	}
	if opts.Key == "" {
		return p, nil
	}

	id, err := bundleattest.NewKeyVerificationIdentity(ctx, opts.Key, p.src)
	if err != nil {
		return nil, err
	}
	fp, ok := id.(interface{ KeyFingerprint() string })
	if !ok || fp.KeyFingerprint() == "" {
		return nil, errors.New(errors.ErrCodeInternal, "key verification identity carries no key fingerprint")
	}
	p.key, p.keyFingerprint = id, fp.KeyFingerprint()
	return p, nil
}

// identity returns the verification identity for this run: the resolved key,
// or the keyless certificate pins over the trust root.
func (p *signerPolicy) identity(opts VerifyOptions) (bundleattest.VerificationIdentity, error) {
	if p.key != nil {
		return p.key, nil
	}
	matcher, err := buildIdentityMatcher(opts)
	if err != nil {
		return nil, err
	}
	return bundleattest.NewKeylessVerificationIdentity(matcher, p.src), nil
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
	policy, err := resolveSignerPolicy(ctx, opts)
	if err != nil {
		return nil, err
	}
	return verifySignature(ctx, mat, opts, policy)
}

func verifySignature(ctx context.Context, mat *MaterializedBundle, opts VerifyOptions, policy *signerPolicy) (*SignatureResult, error) {
	if err := ctx.Err(); err != nil {
		// An operator abort must not be reported as a timeout: ErrCodeTimeout
		// is the retryable/transient bucket, and a deliberate Ctrl-C is not
		// something a CI gate should re-run.
		if stderrors.Is(err, context.Canceled) {
			return nil, errors.Wrap(errors.ErrCodeCanceled, "canceled before signature verify", err)
		}
		return nil, errors.Wrap(errors.ErrCodeTimeout, "timed out before signature verify", err)
	}

	sigPath := filepath.Join(mat.BundleDir, attestation.AttestationFilename)
	// Bounded: this stat decides signed-vs-unsigned, so on a wedged mount an
	// unbounded call would hang the verifier before any read is attempted.
	var info os.FileInfo
	var statErr error
	if bErr := boundedio.Do(ctx, "signature file "+attestation.AttestationFilename, func() error {
		info, statErr = os.Stat(sigPath)
		return nil
	}); bErr != nil {
		return nil, bErr
	}
	if statErr != nil && os.IsNotExist(statErr) {
		return nil, ErrUnsignedBundle
	}
	if statErr != nil {
		// A mount that cannot answer has not told us whether the bundle is
		// signed. Reporting this as Internal let classifyFailure fall through
		// to the step class — "signature" — so a storage fault read as
		// "this bundle's signature failed verification".
		if boundedio.IsStorageFault(statErr) {
			return nil, errors.Wrap(errors.ErrCodeUnavailable,
				"could not read signature file (storage fault)", statErr)
		}
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to stat signature file", statErr)
	}
	if info.Size() > defaults.MaxSigstoreBundleSize {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"signature file exceeds maximum size — refusing to parse")
	}

	sigBundle, parseErr := loadSigstoreBundle(ctx, sigPath)
	if parseErr != nil {
		return nil, parseErr
	}

	stmtBytes, payloadErr := extractStatementBytes(sigBundle)
	if payloadErr != nil {
		return nil, payloadErr
	}

	subjectHex, predicateType, predicate, parseStmtErr := parseStatement(stmtBytes)
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

	if modeErr := checkSigningMode(sigBundle, policy.key != nil); modeErr != nil {
		return nil, modeErr
	}

	identity, idErr := policy.identity(opts)
	if idErr != nil {
		return nil, idErr
	}
	trustedMaterial, trustErr := identity.TrustedMaterial(ctx)
	if trustErr != nil {
		return nil, trustErr
	}

	v, vErr := verify.NewVerifier(trustedMaterial, policy.tlog.VerifierOptions()...)
	if vErr != nil {
		return nil, errors.Wrap(errors.ErrCodeInternal, "failed to create sigstore verifier", vErr)
	}

	result, verifyErr := v.Verify(sigBundle, verify.NewPolicy(
		verify.WithArtifactDigest("sha256", digestBytes),
		identity.PolicyOption(),
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
	switch {
	case policy.key != nil:
		claims.Identity, claims.Issuer = policy.keyFingerprint, attestation.KeySignerIssuer
	case result != nil && result.Signature != nil && result.Signature.Certificate != nil:
		claims.Identity = result.Signature.Certificate.SubjectAlternativeName
		claims.Issuer = result.Signature.Certificate.Issuer
	}
	claims.RekorLogIndex = verifiedRekorLogIndex(sigBundle, opts.IgnoreTLog)

	if opts.IgnoreTLog {
		slog.Warn("signature verified without a transparency log — the signing time is unproven",
			"identity", claims.Identity)
	}

	// Surface the no-pin footgun: without --expected-issuer or
	// --expected-identity-regexp, ANY Fulcio-issued cert from ANY OIDC
	// provider passes the identity policy. The signature is still
	// cryptographically valid, but the verifier hasn't said anything
	// about *who* signed. Operators reviewing the report need to know
	// that default verification accepts every signer. A key is itself
	// the pin, so key mode never warns.
	if policy.key == nil && opts.ExpectedIssuer == "" && opts.ExpectedIdentityRegexp == "" {
		slog.Warn("signature verified but no signer pinned — any Fulcio identity will pass",
			"identity", claims.Identity,
			"issuer", claims.Issuer,
			"hint", "consider --expected-issuer / --expected-identity-regexp to fail on unexpected signers")
	}

	return &SignatureResult{Signer: claims, Predicate: predicate, PredicateType: predicateType}, nil
}

// checkSigningMode fails a bundle whose signature kind does not match the
// verification mode, naming the option that would verify it. sigstore-go
// rejects the mismatch too, but its message does not say what to do.
func checkSigningMode(b *bundle.Bundle, keyMode bool) error {
	content, err := b.VerificationContent()
	if err != nil {
		return nil // malformed material: let sigstore-go report it
	}
	keySigned := content.PublicKey() != nil
	switch {
	case keyMode && !keySigned:
		return errors.New(errors.ErrCodeUnauthorized,
			"sigstore verification failed: the bundle is signed with a keyless certificate, not a key; "+
				"verify it without --key, pinning the signer with --expected-issuer / --expected-identity-regexp")
	case !keyMode && keySigned:
		return errors.New(errors.ErrCodeUnauthorized,
			"sigstore verification failed: the bundle is signed with a key, not a keyless certificate; "+
				"verify it with --key <KMS key URI or PEM public key>")
	}
	return nil
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

func loadSigstoreBundle(ctx context.Context, path string) (*bundle.Bundle, error) {
	data, err := readBoundedFile(ctx, path, "sigstore bundle", defaults.MaxSigstoreBundleSize)
	if err != nil {
		return nil, err
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
// predicateType and predicate body from an in-toto Statement JSON.
func parseStatement(stmtBytes []byte) (subjectHex, predicateType string, predicate *attestation.Predicate, err error) {
	var stmt struct {
		Subject []struct {
			Digest map[string]string `json:"digest"`
		} `json:"subject"`
		PredicateType string                `json:"predicateType"`
		Predicate     attestation.Predicate `json:"predicate"`
	}
	if uErr := json.Unmarshal(stmtBytes, &stmt); uErr != nil {
		return "", "", nil, errors.Wrap(errors.ErrCodeInvalidRequest,
			"DSSE payload is not a valid in-toto Statement", uErr)
	}
	if len(stmt.Subject) == 0 {
		return "", "", nil, errors.New(errors.ErrCodeInvalidRequest, "Statement has no subject")
	}
	subjectHex = stmt.Subject[0].Digest["sha256"]
	if subjectHex == "" {
		return "", "", nil, errors.New(errors.ErrCodeInvalidRequest, "Statement subject has no sha256 digest")
	}
	if cErr := attestation.ValidatePredicateTypeCoherence(stmt.PredicateType, &stmt.Predicate); cErr != nil {
		return "", "", nil, cErr
	}
	return subjectHex, stmt.PredicateType, &stmt.Predicate, nil
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

// verifiedRekorLogIndex returns the bundle's Rekor log index, or nil when it
// has none or when ignoreTLog means no transparency-log entry was verified.
func verifiedRekorLogIndex(b *bundle.Bundle, ignoreTLog bool) *int64 {
	if ignoreTLog {
		return nil
	}
	idx := rekorLogIndex(b)
	if idx <= 0 {
		return nil
	}
	return &idx
}

// comparableSignerClaim returns the pointer signer claim to cross-check against
// the verified signer. Under ignoreTLog the transparency log was not consulted,
// so the pointer's Rekor index is neither confirmed nor refuted and is dropped
// from a copy; claimed itself is never modified.
func comparableSignerClaim(claimed *attestation.PointerSigner, ignoreTLog bool) *attestation.PointerSigner {
	if claimed == nil || !ignoreTLog || claimed.RekorLogIndex == nil {
		return claimed
	}
	c := *claimed
	c.RekorLogIndex = nil
	return &c
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
				", signature says "+actual.Identity)
	}
	if claimed.Issuer != "" && claimed.Issuer != actual.Issuer {
		return errors.New(errors.ErrCodeInvalidRequest,
			"signer issuer mismatch: pointer claims "+claimed.Issuer+
				", signature says "+actual.Issuer)
	}
	if claimed.RekorLogIndex != nil {
		if actual.RekorLogIndex == nil {
			return errors.New(errors.ErrCodeInvalidRequest,
				"pointer claims a Rekor log index but the bundle has no Rekor entry "+
					"(was the bundle signed with --tlog-upload=false, or is the pointer stale?)")
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
