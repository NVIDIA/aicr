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

package aicr

import (
	"context"
	"time"

	bundleverifier "github.com/NVIDIA/aicr/pkg/bundler/verifier"
	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
	evattest "github.com/NVIDIA/aicr/pkg/evidence/attestation"
	evverifier "github.com/NVIDIA/aicr/pkg/evidence/verifier"
	recipecat "github.com/NVIDIA/aicr/pkg/recipe/catalog"
)

// TrustedIdentityPattern is the default certificate-identity regexp that
// binary-attestation verification pins to: NVIDIA's release workflow on tag
// refs.
//
// Override it only to pin a DIFFERENT WORKFLOW within NVIDIA/aicr — a
// pre-release or e2e build, say. Verifying a fork is not possible through this
// option and is not meant to be: ValidateIdentityPattern requires every
// override to begin with https://github.com/NVIDIA/aicr/, so a fork's
// certificate identity can never satisfy it.
const TrustedIdentityPattern = bundleverifier.TrustedRepositoryPattern

// Evidence verification verdicts, mirroring the "exit" field on an
// EvidenceVerification. Re-exported so a consumer can branch on the verdict
// without importing pkg/evidence/verifier.
//
// The verdict is NOT the process exit code. VerifyEvidence returns a verdict
// and a nil error whenever verification ran to completion — including when it
// concluded the bundle is invalid. A non-nil error means verification could
// not be performed at all (bad options, closed Client).
const (
	// EvidenceExitValidPassed: bundle valid, every check passed.
	EvidenceExitValidPassed = evverifier.ExitValidPassed

	// EvidenceExitValidPhaseFailures: bundle valid, but the validator
	// results recorded inside it show phase failures. Informational — the
	// evidence itself is sound.
	EvidenceExitValidPhaseFailures = evverifier.ExitValidPhaseFailures

	// EvidenceExitInvalid: bundle invalid (signature, schema, or integrity
	// failure).
	EvidenceExitInvalid = evverifier.ExitInvalid

	// EvidenceExitIncomplete: verification did not complete, so no verdict
	// was reached. Read FailureCause.Class to tell an environmental fault
	// from an operator abort (EvidenceCauseCanceled).
	EvidenceExitIncomplete = evverifier.ExitIncomplete
)

// EvidenceCauseCanceled is the EvidenceVerification.FailureCause.Class value
// marking an operator-aborted run, as opposed to the environmental faults that
// also produce EvidenceExitIncomplete. A CI gate branches on this to tell "we
// could not check this" from "the run was canceled".
const EvidenceCauseCanceled = evverifier.CauseCanceled

// BundleVerifyOptions configures Client.VerifyBundle.
//
// The fields mirror pkg/config.VerifySpec one-for-one so a caller holding an
// AICRConfig can populate this struct without a translation table: the first
// three come from spec.verify.trust and the next three from
// spec.verify.policy. IgnoreTLog has no config counterpart by design — it
// weakens the trust floor by dropping the transparency-log requirement, and
// keeping it out of the schema means a checked-in file can never silently
// disable that check.
type BundleVerifyOptions struct {
	// CertificateIdentityRegexp overrides the identity pattern that binary
	// attestation verification pins to. Must BEGIN with
	// "https://github.com/NVIDIA/aicr/" (a leading "^" is allowed) and must
	// not use top-level alternation, so it stays confined to the repository;
	// VerifyBundle rejects a pattern that does not before doing any work.
	// Empty uses TrustedIdentityPattern.
	CertificateIdentityRegexp string

	// Key verifies a key-signed bundle attestation instead of a keyless
	// one: a KMS key URI (awskms:// | gcpkms:// | azurekms:// |
	// hashivault://) or a path to a local PEM public key. Independent of
	// CertificateIdentityRegexp, which pins the separate binary
	// attestation; the two coexist.
	Key string

	// TrustRoot is a path to a private Sigstore trusted_root.json (from a
	// self-hosted Fulcio/Rekor). ADDITIVE to AICR's built-in public-good
	// root, so NVIDIA-signed and privately-signed bundles both verify.
	TrustRoot string

	// MinTrustLevel is the trust floor the verified bundle must reach: one
	// of "verified", "attested", "unverified", "unknown", or the
	// meta-value "max".
	//
	// EMPTY MEANS "max" — auto-detect the highest level this bundle could
	// achieve and require it. This deliberately differs from the
	// underlying pkg/bundler/verifier.Policy, where an empty value skips
	// the trust check entirely: a caller who does not think about the
	// trust floor should get the strict default, not no gate. Lower the
	// floor by naming a level explicitly.
	MinTrustLevel string

	// RequireCreator pins the OIDC identity on the bundle attestation's
	// signing certificate. Empty accepts any creator.
	RequireCreator string

	// CLIVersionConstraint constrains the aicr version recorded in the
	// attestation predicate. Supports >=, >, <=, <, ==, != ; a bare
	// version (e.g. "0.16.0") is treated as ">= 0.16.0".
	CLIVersionConstraint string

	// IgnoreTLog skips transparency-log verification so a bundle produced
	// by `bundle --signing-key ... --tlog-upload=false` verifies with no
	// transparency-log network calls. REQUIRES Key — the air-gapped path
	// is key-based, and VerifyBundle rejects the combination otherwise.
	// Insecure relative to the default: with no transparency log there is
	// no trusted timestamp proving when the signature was made.
	IgnoreTLog bool

	// Timeout overrides the facade's operation cap for this call.
	//
	// Nil -- the zero value -- keeps defaults.VerifyOperationTimeout, so a
	// caller who never considered this gets today's behavior. A pointer to 0
	// imposes NO facade cap and runs under the caller's context unchanged. A
	// positive value sets an explicit cap.
	//
	// The pointer exists to make 0 mean "uncapped" rather than "default",
	// matching WithValidationTimeout(0). A plain duration cannot distinguish
	// unset from zero, so it would have had to spell uncapped some other way --
	// and a caller who learned 0-means-uncapped from ValidateState would then
	// get the opposite here, silently capped when they asked for unbounded.
	Timeout *time.Duration
}

// BundleVerifyReport is the per-check outcome of bundle verification.
//
// Deliberate transparent alias of pkg/bundler/verifier.VerifyResult. It is a
// flat, read-only report whose fields are already the documented JSON contract
// of `aicr verify --format json`; owning a translated copy would duplicate
// that contract without insulating consumers from anything, since any field
// change would have to propagate to stay useful.
type BundleVerifyReport = bundleverifier.VerifyResult

// BundleVerification pairs the verification report with the outcome of the
// policy assertions in BundleVerifyOptions.
type BundleVerification struct {
	// Report is the per-check verification outcome. Never nil on a nil
	// error.
	Report *BundleVerifyReport

	// PolicyFailure describes the first policy assertion the bundle failed
	// (trust floor, required creator, version constraint), or is empty
	// when every assertion passed.
	//
	// A policy failure is DATA, not an error: VerifyBundle still returns
	// the full Report so a caller can render or log why the bundle fell
	// short. Callers that need a failed policy to abort should check this
	// field (and Report.Errors) explicitly.
	PolicyFailure string
}

// VerifyBundle verifies a deployment bundle's checksums and attestation chain,
// then evaluates the policy assertions carried in opts.
//
// Verification is offline: the checksum and attestation chain resolve against
// the locally cached or embedded Sigstore trusted root. The one network path
// is a KMS URI in opts.Key, which makes a live GetPublicKey call to resolve
// the key.
//
// A non-nil error means verification could not be performed — bad options, a
// missing or unreadable bundle directory, a malformed trust root. A bundle
// that verified but failed is reported through the returned value:
// Report.Errors for verification failures and PolicyFailure for policy ones.
//
// This method does not touch the Client's recipe catalog. It hangs off Client
// so that a single configured Client is the one object a consumer needs, and
// so config-driven verification has a home. Any open Client will do — a
// hot-path caller can construct one against EmbeddedSource and reuse it,
// rather than building one per verification.
func (c *Client) VerifyBundle(ctx context.Context, bundleDir string, opts BundleVerifyOptions) (*BundleVerification, error) {
	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if bundleDir == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "bundle directory is required")
	}
	if err := c.assertOpen(); err != nil {
		return nil, err
	}

	// Reject the offline/air-gapped combination up front so the caller gets
	// a clear message instead of a downstream verification failure.
	if opts.IgnoreTLog && opts.Key == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"IgnoreTLog requires Key: offline verification is key-based "+
				"(verify a bundle signed with `bundle --signing-key ... --tlog-upload=false`)")
	}
	if opts.CertificateIdentityRegexp != "" {
		// Already coded ErrCodeInvalidRequest; propagating as-is keeps the
		// "must contain NVIDIA/aicr" message the caller needs.
		if err := bundleverifier.ValidateIdentityPattern(opts.CertificateIdentityRegexp); err != nil {
			return nil, err
		}
	}

	ctx, cancel := applyVerifyCap(ctx, opts.Timeout)
	defer cancel()

	report, err := bundleverifier.Verify(ctx, bundleDir, &bundleverifier.VerifyOptions{
		CertificateIdentityRegexp: opts.CertificateIdentityRegexp,
		Key:                       opts.Key,
		TrustRoot:                 opts.TrustRoot,
		IgnoreTLog:                opts.IgnoreTLog,
	})
	if err != nil {
		// Verify returns coded errors (ErrCodeNotFound for a missing bundle
		// dir, ErrCodeInvalidRequest for a bad trust root); preserve them.
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "bundle verification failed")
	}

	minTrustLevel := opts.MinTrustLevel
	if minTrustLevel == "" {
		minTrustLevel = maxTrustLevel
	}
	policyFailure, err := report.CheckPolicy(bundleverifier.Policy{
		MinTrustLevel:     minTrustLevel,
		RequireCreator:    opts.RequireCreator,
		VersionConstraint: opts.CLIVersionConstraint,
	})
	if err != nil {
		// An unparseable trust level or version constraint is a caller
		// input fault, already coded by CheckPolicy.
		return nil, err
	}

	return &BundleVerification{Report: report, PolicyFailure: policyFailure}, nil
}

// maxTrustLevel is the meta-value that resolves to the highest trust level a
// given bundle could achieve. Named rather than spelled inline so the default
// in BundleVerifyOptions.MinTrustLevel stays in sync with the CLI flag default.
const maxTrustLevel = "max"

// EvidenceVerifyOptions configures Client.VerifyEvidence.
type EvidenceVerifyOptions struct {
	// Input selects the bundle, in any of three auto-detected forms: a
	// pointer file path (recipes/evidence/<recipe>/<source>/<digest>.yaml),
	// an OCI reference (with or without an oci:// prefix), or an unpacked
	// bundle directory. Required.
	Input string

	// BundleRef overrides the OCI reference when Input does not carry one
	// — a pointer file whose bundle.oci is empty.
	BundleRef string

	// ExpectedIssuer pins the OIDC issuer URL on the signing certificate.
	// Empty allows any issuer.
	ExpectedIssuer string

	// ExpectedIdentityRegexp pins the signer's SubjectAlternativeName via
	// regex. Empty allows any identity.
	ExpectedIdentityRegexp string

	// PlainHTTP forces HTTP for registry traffic (local-registry tests).
	PlainHTTP bool

	// InsecureTLS disables registry TLS verification (self-signed certs).
	InsecureTLS bool

	// AllowUnpinnedTag opts into accepting an OCI reference that resolves
	// to a tag rather than a digest. Off by default because a tag can be
	// rewritten by the registry, so "verify this artifact at this tag" is
	// not content-addressable.
	AllowUnpinnedTag bool

	// Timeout overrides the facade's operation cap for this call.
	//
	// Nil -- the zero value -- keeps defaults.VerifyOperationTimeout, so a
	// caller who never considered this gets today's behavior. A pointer to 0
	// imposes NO facade cap and runs under the caller's context unchanged. A
	// positive value sets an explicit cap.
	//
	// The pointer exists to make 0 mean "uncapped" rather than "default",
	// matching WithValidationTimeout(0). A plain duration cannot distinguish
	// unset from zero, so it would have had to spell uncapped some other way --
	// and a caller who learned 0-means-uncapped from ValidateState would then
	// get the opposite here, silently capped when they asked for unbounded.
	Timeout *time.Duration
}

// EvidenceVerification is the outcome of recipe-evidence bundle verification:
// the verdict, the recovered predicate and pointer, the signer's claims, and
// the per-step results.
//
// Deliberate transparent alias of pkg/evidence/verifier.VerifyResult. The
// result is a deep tree — it reaches into the evidence pointer, the in-toto
// predicate, the per-step records, and the signer claims — and every field is
// read-only output. A facade-owned copy would mean owning five more nested
// types whose shape is still evolving alongside the evidence predicate, for no
// consumer benefit: a caller reads the verdict and the predicate through
// `:=` and never names this type.
type EvidenceVerification = evverifier.VerifyResult

// VerifyEvidence verifies a recipe-evidence bundle's signature (when present)
// and manifest hash chain, and surfaces the predicate it recovered.
//
// A non-nil error means verification could not be attempted (bad options,
// closed Client). Everything else — including "this bundle is invalid" — comes
// back as a verdict on the returned value; read EvidenceVerification.Exit and
// compare against the EvidenceExit* constants.
//
// Unlike bundle verification this can reach the network: a pointer or OCI
// input pulls the artifact from its registry.
//
// That makes one interaction worth knowing. By default the call is capped by
// defaults.VerifyOperationTimeout, so a slow registry can trip it even when the
// caller's own context allowed longer. A cap breach returns an ERROR, not
// EvidenceExitIncomplete — so a gate that distinguishes "could not check this"
// from "checked it and it failed" must treat a context-deadline error as the
// former, alongside the Incomplete verdict.
//
// Set EvidenceVerifyOptions.Timeout to a pointer to 0 to remove the facade cap
// and let the caller's context govern, which is the fix for a registry that is
// simply slow rather than broken. The caller's own deadline still applies; the
// option can only relax the facade's ceiling, never extend the context it was
// given.
//
// The Client's recipe catalog is not consulted, so any open Client will do —
// including one a hot-path caller keeps around and reuses.
func (c *Client) VerifyEvidence(ctx context.Context, opts EvidenceVerifyOptions) (*EvidenceVerification, error) {
	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if opts.Input == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest,
			"evidence input is required (pointer path, OCI reference, or bundle directory)")
	}
	if err := c.assertOpen(); err != nil {
		return nil, err
	}

	ctx, cancel := applyVerifyCap(ctx, opts.Timeout)
	defer cancel()

	result, err := evverifier.Verify(ctx, evverifier.VerifyOptions{
		Input:                  opts.Input,
		BundleRef:              opts.BundleRef,
		ExpectedIssuer:         opts.ExpectedIssuer,
		ExpectedIdentityRegexp: opts.ExpectedIdentityRegexp,
		PlainHTTP:              opts.PlainHTTP,
		InsecureTLS:            opts.InsecureTLS,
		AllowUnpinnedTag:       opts.AllowUnpinnedTag,
	})
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "evidence verification failed")
	}
	return result, nil
}

// CatalogVerifyOptions configures Client.VerifyCatalog.
type CatalogVerifyOptions struct {
	// CertificateIdentityRegexp overrides the NVIDIA CI identity pattern.
	// Must BEGIN with "https://github.com/NVIDIA/aicr/" (a leading "^" is
	// allowed) and must not use top-level alternation. Empty uses
	// TrustedIdentityPattern.
	CertificateIdentityRegexp string

	// Timeout overrides the facade's operation cap for this call.
	//
	// Nil -- the zero value -- keeps defaults.VerifyOperationTimeout, so a
	// caller who never considered this gets today's behavior. A pointer to 0
	// imposes NO facade cap and runs under the caller's context unchanged. A
	// positive value sets an explicit cap.
	//
	// The pointer exists to make 0 mean "uncapped" rather than "default",
	// matching WithValidationTimeout(0). A plain duration cannot distinguish
	// unset from zero, so it would have had to spell uncapped some other way --
	// and a caller who learned 0-means-uncapped from ValidateState would then
	// get the opposite here, silently capped when they asked for unbounded.
	Timeout *time.Duration
}

// CatalogVerification is what VerifyCatalog returns on success.
type CatalogVerification struct {
	// Identity is the SubjectAlternativeName claim from the signing
	// certificate.
	Identity string

	// Digest is the hex-encoded SHA-256 of the catalog content that was
	// verified.
	Digest string
}

// VerifyCatalog recomputes the deterministic digest over this Client's recipe
// catalog (registry.yaml plus validators/catalog.yaml) and verifies it against
// the Sigstore bundle at bundlePath, pinning to NVIDIA CI identity. The bundle
// ships as the recipe-catalog.sigstore.json release asset alongside each
// tagged aicr binary.
//
// The digest is computed over THIS Client's DataProvider, not the process-wide
// embedded catalog — the same binding ComputeHealth uses. A Client built on an
// EmbeddedSource verifies the catalog NVIDIA signed. A Client whose source
// layers external data over the embedded tree is verifying different content,
// so verification will not match the released signature; that is the correct
// answer to "is the catalog I am resolving against the signed one", not a bug.
//
// A verification failure is returned as an error, not as a report: unlike
// bundle and evidence verification there is no partial verdict to render.
func (c *Client) VerifyCatalog(ctx context.Context, bundlePath string, opts CatalogVerifyOptions) (*CatalogVerification, error) {
	if c == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if bundlePath == "" {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "catalog Sigstore bundle path is required")
	}
	if opts.CertificateIdentityRegexp != "" {
		if err := bundleverifier.ValidateIdentityPattern(opts.CertificateIdentityRegexp); err != nil {
			return nil, err
		}
	}

	c.mu.RLock()
	if c.builder == nil {
		c.mu.RUnlock()
		return nil, errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	dp := c.dp
	c.inflight.Add(1)
	c.mu.RUnlock()
	defer c.inflight.Done()

	ctx, cancel := applyVerifyCap(ctx, opts.Timeout)
	defer cancel()

	result, err := recipecat.Verify(ctx, bundlePath, dp, recipecat.VerifyOptions{
		CertificateIdentityRegexp: opts.CertificateIdentityRegexp,
	})
	if err != nil {
		return nil, errors.PropagateOrWrap(err, errors.ErrCodeInternal, "recipe catalog verification failed")
	}
	return &CatalogVerification{Identity: result.Identity, Digest: result.Digest}, nil
}

// RecipeDigestOptions configures Client.RecipeDigest.
type RecipeDigestOptions struct {
	// Path is a recipe or overlay file, as a path, an HTTP(S) URL, or a
	// cm://namespace/name ConfigMap URI. Required.
	Path string

	// Kubeconfig resolves cm:// URIs. Empty uses the standard KUBECONFIG,
	// ~/.kube/config, then in-cluster discovery chain.
	Kubeconfig string

	// Profile is a name=value configuration-profile selection, with the
	// same semantics as `aicr recipe --profile`. It applies only to
	// overlay inputs: an already-hydrated recipe carries its own
	// metadata.selectedProfile, and combining the two is rejected.
	Profile string

	// Timeout overrides the facade's operation cap for this call.
	//
	// Nil -- the zero value -- keeps defaults.VerifyOperationTimeout, so a
	// caller who never considered this gets today's behavior. A pointer to 0
	// imposes NO facade cap and runs under the caller's context unchanged. A
	// positive value sets an explicit cap.
	//
	// The pointer exists to make 0 mean "uncapped" rather than "default",
	// matching WithValidationTimeout(0). A plain duration cannot distinguish
	// unset from zero, so it would have had to spell uncapped some other way --
	// and a caller who learned 0-means-uncapped from ValidateState would then
	// get the opposite here, silently capped when they asked for unbounded.
	Timeout *time.Duration
}

// RecipeDigest returns the canonical digest of a resolved recipe — the
// lowercase hex SHA-256 of its canonical YAML, byte-for-byte the value stored
// in predicate.recipe.digest by an evidence-emitting validation run.
//
// It is the producer-side companion to VerifyEvidence: a CI gate verifies a
// committed evidence pointer, reads predicate.recipe.digest out of the verified
// bundle, and compares it against RecipeDigest of the recipe on the branch to
// detect evidence that has gone stale.
//
// Hydration resolves against THIS Client's DataProvider, so the digest
// reflects the same catalog the Client would resolve and validate with.
func (c *Client) RecipeDigest(ctx context.Context, opts RecipeDigestOptions) (string, error) {
	if c == nil {
		return "", errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized")
	}
	if ctx == nil {
		return "", errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if opts.Path == "" {
		return "", errors.New(errors.ErrCodeInvalidRequest, "recipe path is required")
	}

	c.mu.RLock()
	if c.builder == nil {
		c.mu.RUnlock()
		return "", errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	dp := c.dp
	clientVersion := c.version
	c.inflight.Add(1)
	c.mu.RUnlock()
	defer c.inflight.Done()

	ctx, cancel := applyVerifyCap(ctx, opts.Timeout)
	defer cancel()

	digest, err := evattest.ComputeRecipeDigestWithProfile(
		ctx, dp, opts.Path, opts.Kubeconfig, clientVersion, opts.Profile)
	if err != nil {
		return "", errors.PropagateOrWrap(err, errors.ErrCodeInternal, "failed to compute recipe digest")
	}
	return digest, nil
}

// BinaryAttestationVerifyOptions configures VerifyBinaryAttestation.
type BinaryAttestationVerifyOptions struct {
	// Attestation is the raw Sigstore bundle for the binary. Required.
	//
	// Taking bytes rather than a path is deliberate: it lets a caller
	// verify the EXACT content it is about to use, with no
	// verify-then-reread window in which the file could change.
	Attestation []byte

	// BinaryDigest is the RAW (not hex-encoded) SHA-256 of the binary the
	// attestation must cover. Required.
	BinaryDigest []byte

	// IdentityRegexp overrides the certificate identity the attestation is
	// pinned to. Empty uses TrustedIdentityPattern. Must BEGIN with
	// "https://github.com/NVIDIA/aicr/" (a leading "^" is allowed) and must
	// not use top-level alternation, so the override stays confined to the
	// repository; see ValidateIdentityPattern.
	IdentityRegexp string
}

// VerifyBinaryAttestation verifies an aicr binary's own provenance attestation
// against a certificate identity and the binary's digest, returning the
// verified certificate subject.
//
// Use it to prove the aicr binary being embedded or executed was built by
// NVIDIA CI, before trusting anything it produces. It is package-level rather
// than a Client method because it involves no recipe catalog and no
// configurable policy — there is nothing for a Client to contribute.
func VerifyBinaryAttestation(ctx context.Context, opts BinaryAttestationVerifyOptions) (string, error) {
	if ctx == nil {
		return "", errors.New(errors.ErrCodeInvalidRequest, "context is required (got nil)")
	}
	if len(opts.Attestation) == 0 {
		return "", errors.New(errors.ErrCodeInvalidRequest, "binary attestation bytes are required")
	}
	if len(opts.BinaryDigest) == 0 {
		return "", errors.New(errors.ErrCodeInvalidRequest, "binary digest is required")
	}

	identity := opts.IdentityRegexp
	if identity == "" {
		identity = TrustedIdentityPattern
	} else if err := bundleverifier.ValidateIdentityPattern(identity); err != nil {
		return "", err
	}

	subject, err := bundleverifier.VerifyBinaryAttestationData(ctx, opts.Attestation, identity, opts.BinaryDigest)
	if err != nil {
		return "", err // already coded
	}
	return subject, nil
}

// ValidateIdentityPattern reports whether pattern is usable as a
// certificate-identity override, rejecting anything that does not stay pinned
// to the NVIDIA/aicr repository. Call it to validate operator-supplied input
// before handing it to VerifyBundle or VerifyBinaryAttestation, both of which
// apply the same check internally.
func ValidateIdentityPattern(pattern string) error {
	return bundleverifier.ValidateIdentityPattern(pattern)
}

// TrustLevels returns the bundle trust levels that
// BundleVerifyOptions.MinTrustLevel accepts, sorted alphabetically (NOT by
// rank). Intended for building help text, shell completions, and input
// validation. The meta-value "max" is deliberately absent: it is a policy
// instruction rather than a level a bundle can be at.
//
// That matters when validating BundleVerifyOptions.MinTrustLevel input: this
// list alone is NOT the accepted set. Accept "max" and the empty string too,
// or the check rejects the very default the option documents.
//
// Each call returns a fresh slice, so a caller may sort or filter it.
func TrustLevels() []string {
	return bundleverifier.GetTrustLevels()
}

// RenderEvidenceJSON renders an EvidenceVerification as the structured JSON
// document `aicr evidence verify --format json` emits.
func RenderEvidenceJSON(r *EvidenceVerification) ([]byte, error) {
	if r == nil {
		return nil, errors.New(errors.ErrCodeInvalidRequest, "nil EvidenceVerification")
	}
	return evverifier.RenderJSON(r)
}

// RenderEvidenceMarkdown renders an EvidenceVerification as the Markdown
// summary `aicr evidence verify` prints by default. Returns an empty string
// for a nil result.
func RenderEvidenceMarkdown(r *EvidenceVerification) string {
	if r == nil {
		return ""
	}
	return evverifier.RenderMarkdown(r)
}

// assertOpen reports whether the Client is constructed and not yet closed.
//
// Used by the entry points that do NOT touch the Client's DataProvider or its
// per-provider caches, so they reject a closed Client for the same reason
// every other method does without registering in the inflight WaitGroup —
// that group exists to let Close drain cache-using work before evicting, and
// these operations have no cache to protect.
func (c *Client) assertOpen() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.builder == nil {
		return errors.New(errors.ErrCodeInvalidRequest, "aicr client not initialized (or already closed)")
	}
	return nil
}

// applyVerifyCap wraps ctx with the facade's verify cap unless the caller
// overrode it.
//
// Nil keeps the default cap. A pointer to zero (or negative) imposes no cap and
// returns the caller's context unchanged, which is what lets a verification
// behind a slow registry run to completion instead of being cut off at five
// minutes and reported as an error.
//
// That distinction matters beyond convenience for VerifyEvidence: a cap breach
// surfaces as an error, but "could not check" has a dedicated verdict
// (EvidenceExitIncomplete) that CI gates branch on. Cutting a slow pull short
// delivered that outcome through the wrong channel.
func applyVerifyCap(ctx context.Context, timeout *time.Duration) (context.Context, context.CancelFunc) {
	if timeout == nil {
		return context.WithTimeout(ctx, defaults.VerifyOperationTimeout)
	}
	if *timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, *timeout)
}
