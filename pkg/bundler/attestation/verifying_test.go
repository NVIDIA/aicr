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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	stderrors "errors"
	"strings"
	"testing"

	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"

	"github.com/NVIDIA/aicr/pkg/errors"
)

// requireErrCode fails the test unless err carries the given pkg/errors code.
func requireErrCode(t *testing.T, err error, code errors.ErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %v, got nil", code)
	}
	if !stderrors.Is(err, errors.New(code, "")) {
		t.Fatalf("expected error code %v, got %v", code, err)
	}
}

// anyCertIdentity returns a keyless VerificationIdentity that matches any
// OIDC-issued certificate, mirroring verifier.verifySigstoreBundle.
func anyCertIdentity(t *testing.T) VerificationIdentity {
	t.Helper()
	certID, err := verify.NewShortCertificateIdentity("", ".+", "", ".+")
	if err != nil {
		t.Fatalf("failed to build certificate identity: %v", err)
	}
	return NewKeylessVerificationIdentity(certID, nil)
}

func TestVerifyStatementWith_NilArgs(t *testing.T) {
	tests := []struct {
		name string
		id   VerificationIdentity
		tlog VerifyTransparencyPolicy
	}{
		{"nil identity", nil, NewRequireTLogPolicy()},
		{"nil transparency policy", anyCertIdentity(t), nil},
		{"both nil", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := VerifyStatementWith(context.Background(), []byte("{}"), tt.id, tt.tlog, []byte{1})
			requireErrCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}
}

func TestVerifyStatementWith_EmptyDigest(t *testing.T) {
	id := anyCertIdentity(t)
	tlog := NewRequireTLogPolicy()

	t.Run("nil digest", func(t *testing.T) {
		_, err := VerifyStatementWith(context.Background(), []byte("{}"), id, tlog, nil)
		requireErrCode(t, err, errors.ErrCodeInvalidRequest)
	})

	t.Run("empty digest", func(t *testing.T) {
		_, err := VerifyStatementWith(context.Background(), []byte("{}"), id, tlog, []byte{})
		requireErrCode(t, err, errors.ErrCodeInvalidRequest)
	})
}

func TestVerifyStatementWith_CancelledContext(t *testing.T) {
	id := anyCertIdentity(t)
	tlog := NewRequireTLogPolicy()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := VerifyStatementWith(ctx, []byte("{}"), id, tlog, []byte{1})
	requireErrCode(t, err, errors.ErrCodeTimeout)
}

func TestVerifyStatementWith_InvalidBundle(t *testing.T) {
	id := anyCertIdentity(t)
	tlog := NewRequireTLogPolicy()

	// Valid guards (non-nil id/tlog, non-empty digest, live context) but the
	// bundle bytes are not parseable as a Sigstore bundle: parsing fails as an
	// invalid-request error (malformed caller input) before any verification.
	// The signer identity is empty on the error path; capture it so the return
	// value is exercised.
	signer, err := VerifyStatementWith(context.Background(), []byte("not json"), id, tlog, []byte{1})
	requireErrCode(t, err, errors.ErrCodeInvalidRequest)
	if signer != "" {
		t.Errorf("signer identity = %q, want empty on error", signer)
	}
}

func TestLoadSigstoreBundleBytes(t *testing.T) {
	t.Run("invalid JSON", func(t *testing.T) {
		_, err := loadSigstoreBundleBytes([]byte("not json"))
		requireErrCode(t, err, errors.ErrCodeInvalidRequest)
		if !strings.Contains(err.Error(), "failed to parse sigstore bundle") {
			t.Errorf("error = %v, want parse failure message", err)
		}
	})

	t.Run("incomplete bundle", func(t *testing.T) {
		// Valid protobuf-JSON but an incomplete sigstore bundle (no content).
		bundleJSON := `{"mediaType":"application/vnd.dev.sigstore.bundle+json;version=0.3"}`
		_, err := loadSigstoreBundleBytes([]byte(bundleJSON))
		requireErrCode(t, err, errors.ErrCodeInvalidRequest)
		if !strings.Contains(err.Error(), "invalid sigstore bundle") {
			t.Errorf("error = %v, want invalid bundle message", err)
		}
	})
}

func TestSignerIdentityFromResult(t *testing.T) {
	tests := []struct {
		name   string
		result *verify.VerificationResult
		want   string
	}{
		{"nil result", nil, ""},
		{"nil signature", &verify.VerificationResult{}, ""},
		{
			"nil certificate (key-based)",
			&verify.VerificationResult{Signature: &verify.SignatureVerificationResult{}},
			"",
		},
		{
			"populated SAN (keyless)",
			&verify.VerificationResult{Signature: &verify.SignatureVerificationResult{
				Certificate: &certificate.Summary{
					SubjectAlternativeName: "https://github.com/NVIDIA/aicr/.github/workflows/on-tag.yaml@refs/tags/v1.0.0",
				},
			}},
			"https://github.com/NVIDIA/aicr/.github/workflows/on-tag.yaml@refs/tags/v1.0.0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := signerIdentityFromResult(tt.result); got != tt.want {
				t.Errorf("signerIdentityFromResult() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestContainsCertChainError(t *testing.T) {
	tests := []struct {
		name   string
		errMsg string
		want   bool
	}{
		{"unknown authority", "certificate signed by unknown authority", true},
		{"x509 unknown authority", "x509: certificate signed by unknown authority", true},
		{"cert chain", "failed to verify certificate chain", true},
		{"unable to verify", "unable to verify certificate", true},
		{"root cert", "root certificate not found", true},
		{"case insensitive", "Certificate Signed By Unknown Authority", true},
		// Expiry is not a stale-root condition: trust update cannot fix it, so a
		// bare x509 error must not trigger the stale-root remediation hint.
		{"x509 expiry not stale-root", "x509: certificate has expired", false},
		{"unrelated error", "connection refused", false},
		{"empty string", "", false},
		{"sigstore error", "sigstore verification failed", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containsCertChainError(tt.errMsg); got != tt.want {
				t.Errorf("containsCertChainError(%q) = %v, want %v", tt.errMsg, got, tt.want)
			}
		})
	}
}

func TestRequireTLogPolicy_VerifierOptions(t *testing.T) {
	opts := NewRequireTLogPolicy().VerifierOptions()
	if len(opts) != 2 {
		t.Fatalf("VerifierOptions() returned %d options, want 2", len(opts))
	}
}

func TestNoTLogVerifyPolicy_VerifierOptions(t *testing.T) {
	opts := NewNoTLogVerifyPolicy().VerifierOptions()
	// The offline policy carries exactly one option (WithNoObserverTimestamps),
	// which sigstore-go requires be specified exclusively.
	if len(opts) != 1 {
		t.Fatalf("VerifierOptions() returned %d options, want 1", len(opts))
	}
	if opts[0] == nil {
		t.Fatal("VerifierOptions()[0] is nil")
	}
}

// TestNoTLogVerifyPolicy_RoundTrip proves the offline relaxation is load-bearing:
// a key-signed bundle produced with NewNoTLogPolicy() (no Rekor entry, no
// timestamp) verifies fully offline with NewNoTLogVerifyPolicy(), while the same
// bundle FAILS under the default NewRequireTLogPolicy() (which demands a tlog
// inclusion proof / observer timestamp the air-gapped bundle does not carry).
func TestNoTLogVerifyPolicy_RoundTrip(t *testing.T) {
	ctx := context.Background()

	// One key drives both the offline signer and the matching key verifier.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signID := &fakeKeyIdentity{signer: &localECDSASigner{priv: priv}, uri: "awskms://test/key"}

	res, err := SignStatementWith(ctx, validStatementJSON(t), signID, NewNoTLogPolicy())
	if err != nil {
		t.Fatalf("SignStatementWith (offline, no tlog): %v", err)
	}

	// Build the key verification identity from the public half. Inject an empty
	// trust-root source (not the default public-good root, which would require a
	// TUF fetch) to keep the round trip fully offline: a key-based, no-timestamp
	// verify needs only the key material, exactly the air-gapped guarantee.
	offlineSrc := func(context.Context) (root.TrustedMaterial, error) {
		return root.TrustedMaterialCollection{}, nil
	}
	pemPath := writeTempPubPEM(t, &priv.PublicKey)
	verID, err := NewKeyVerificationIdentity(ctx, pemPath, offlineSrc)
	if err != nil {
		t.Fatalf("NewKeyVerificationIdentity: %v", err)
	}

	// validStatementJSON binds subject checksums.txt to an all-zero sha256, so
	// the artifact digest is 32 zero bytes.
	digest := make([]byte, 32)

	if _, err := VerifyStatementWith(ctx, res.BundleJSON, verID, NewNoTLogVerifyPolicy(), digest); err != nil {
		t.Fatalf("offline verify with NewNoTLogVerifyPolicy() must succeed, got %v", err)
	}

	// The relaxation is load-bearing: the same tlog-less bundle must fail the
	// default policy, which requires a transparency-log / observer timestamp.
	if _, err := VerifyStatementWith(ctx, res.BundleJSON, verID, NewRequireTLogPolicy(), digest); err == nil {
		t.Fatal("verify with NewRequireTLogPolicy() must fail for a tlog-less bundle, got nil error")
	}
}
