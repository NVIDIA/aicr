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
	"os"
	"path/filepath"
	"strings"
	"testing"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protorekor "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/sign"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// localKeyIdentity signs with an in-memory key and no certificate, the same
// shape a KMS key signature takes, so a key-signed bundle can be produced
// offline.
type localKeyIdentity struct{ kp *sign.EphemeralKeypair }

func (l localKeyIdentity) Keypair(context.Context) (sign.Keypair, error) { return l.kp, nil }

func (localKeyIdentity) CertProvider() (sign.CertificateProvider, *sign.CertificateProviderOptions) {
	return nil, nil
}

func (localKeyIdentity) FallbackIdentity() string { return "awskms:///alias/test-evidence" }

func newLocalKeypair(t *testing.T) *sign.EphemeralKeypair {
	t.Helper()
	kp, err := sign.NewEphemeralKeypair(nil)
	if err != nil {
		t.Fatalf("NewEphemeralKeypair: %v", err)
	}
	return kp
}

// writePublicKeyPEM writes kp's public key to a PEM file and returns its path.
func writePublicKeyPEM(t *testing.T, kp *sign.EphemeralKeypair) string {
	t.Helper()
	pemStr, err := kp.GetPublicKeyPem()
	if err != nil {
		t.Fatalf("GetPublicKeyPem: %v", err)
	}
	path := filepath.Join(t.TempDir(), "evidence-signer.pub")
	if err := os.WriteFile(path, []byte(pemStr), 0o600); err != nil {
		t.Fatalf("write public key: %v", err)
	}
	return path
}

// buildKeySignedBundle builds an evidence bundle and signs its statement with
// kp and no transparency-log entry, the shape `--signing-key
// --tlog-upload=false` produces. Returns the bundle root and the signing
// result.
func buildKeySignedBundle(t *testing.T, kp *sign.EphemeralKeypair) (string, *bundleattest.SignedAttestation) {
	t.Helper()
	dir := buildTestBundle(t)
	summary := summaryDirOf(t, dir)
	stmt, err := os.ReadFile(filepath.Join(summary, attestation.StatementFilename))
	if err != nil {
		t.Fatalf("read statement: %v", err)
	}
	signed, err := bundleattest.SignStatementWith(t.Context(), stmt, localKeyIdentity{kp: kp}, bundleattest.NewNoTLogPolicy())
	if err != nil {
		t.Fatalf("SignStatementWith: %v", err)
	}
	if err := os.WriteFile(filepath.Join(summary, attestation.AttestationFilename), signed.BundleJSON, 0o600); err != nil {
		t.Fatalf("write signed attestation: %v", err)
	}
	return dir, signed
}

// TestVerify_KeySignedRoundTrip pins the contract between the two halves: the
// signer block a key-signed emission records (PointerSignerFromSignature) is
// exactly the signer the verifier reports for the same key, so the pointer
// cross-check passes and the per-source path the signer maps to is stable.
func TestVerify_KeySignedRoundTrip(t *testing.T) {
	kp := newLocalKeypair(t)
	dir, signed := buildKeySignedBundle(t, kp)

	wantFingerprint, err := bundleattest.PublicKeyFingerprint(kp.GetPublicKey())
	if err != nil {
		t.Fatalf("PublicKeyFingerprint: %v", err)
	}
	if signed.KeyFingerprint != wantFingerprint {
		t.Fatalf("SignedAttestation.KeyFingerprint = %q, want %q", signed.KeyFingerprint, wantFingerprint)
	}

	res, err := Verify(t.Context(), VerifyOptions{
		Input:      summaryDirOf(t, dir),
		Key:        writePublicKeyPEM(t, kp),
		IgnoreTLog: true,
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.Exit != ExitValidPassed {
		t.Fatalf("Exit = %d, want %d; steps = %+v", res.Exit, ExitValidPassed, res.Steps)
	}
	if res.Signer == nil {
		t.Fatal("Signer is nil for a verified key signature")
	}
	if res.Signer.Issuer != attestation.KeySignerIssuer || res.Signer.Identity != wantFingerprint {
		t.Errorf("Signer = %+v, want issuer %q identity %q", res.Signer, attestation.KeySignerIssuer, wantFingerprint)
	}
	if res.Signer.RekorLogIndex != nil {
		t.Errorf("RekorLogIndex = %d, want nil for a tlog-less signature", *res.Signer.RekorLogIndex)
	}

	claimed := attestation.PointerSignerFromSignature(signed)
	if err := CrossCheckPointerSigner(claimed, res.Signer); err != nil {
		t.Errorf("pointer signer %+v does not cross-check against the verified signer: %v", claimed, err)
	}
}

func TestVerifySignature_KeyMode(t *testing.T) {
	kp := newLocalKeypair(t)
	dir, _ := buildKeySignedBundle(t, kp)
	mat := &MaterializedBundle{BundleDir: summaryDirOf(t, dir)}
	keyPath := writePublicKeyPEM(t, kp)
	otherKeyPath := writePublicKeyPEM(t, newLocalKeypair(t))

	t.Run("the signing key verifies", func(t *testing.T) {
		res, err := VerifySignature(t.Context(), mat, VerifyOptions{Key: keyPath, IgnoreTLog: true})
		if err != nil {
			t.Fatalf("VerifySignature: %v", err)
		}
		if res.Signer == nil || res.Signer.Issuer != attestation.KeySignerIssuer {
			t.Errorf("Signer = %+v, want issuer %q", res.Signer, attestation.KeySignerIssuer)
		}
		if res.Predicate == nil || res.PredicateType == "" {
			t.Errorf("verified predicate missing: type %q, predicate %v", res.PredicateType, res.Predicate)
		}
	})

	tests := []struct {
		name     string
		opts     VerifyOptions
		wantCode errors.ErrorCode
		wantMsg  string
	}{
		{
			name:     "a different key fails",
			opts:     VerifyOptions{Key: otherKeyPath, IgnoreTLog: true},
			wantCode: errors.ErrCodeUnauthorized,
		},
		{
			name:     "the default tlog requirement fails a tlog-less signature",
			opts:     VerifyOptions{Key: keyPath},
			wantCode: errors.ErrCodeUnauthorized,
		},
		{
			name:     "keyless verification names the option that verifies it",
			opts:     VerifyOptions{},
			wantCode: errors.ErrCodeUnauthorized,
			wantMsg:  "--key",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := VerifySignature(t.Context(), mat, tt.opts)
			errorstest.WantReportedCode(t, err, tt.wantCode)
			if tt.wantMsg != "" && (err == nil || !strings.Contains(err.Error(), tt.wantMsg)) {
				t.Errorf("error %v does not mention %q", err, tt.wantMsg)
			}
		})
	}
}

func TestResolveSignerPolicy_RejectsOperatorErrors(t *testing.T) {
	keyPath := writePublicKeyPEM(t, newLocalKeypair(t))
	tests := []struct {
		name string
		opts VerifyOptions
	}{
		{"ignore tlog without a key", VerifyOptions{IgnoreTLog: true}},
		{"key with an issuer pin", VerifyOptions{Key: keyPath, ExpectedIssuer: "https://token.actions.githubusercontent.com"}},
		{"key with an identity pin", VerifyOptions{Key: keyPath, ExpectedIdentityRegexp: "^https://github.com/x/.*$"}},
		{"missing key file", VerifyOptions{Key: filepath.Join(t.TempDir(), "absent.pub")}},
		{"missing trust root", VerifyOptions{TrustRoot: filepath.Join(t.TempDir(), "absent.json")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveSignerPolicy(t.Context(), tt.opts)
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}
}

// TestVerify_OperatorErrorIsNotAVerdict pins that a bad --key fails the run
// before anything is read, instead of reporting the bundle invalid.
func TestVerify_OperatorErrorIsNotAVerdict(t *testing.T) {
	dir := buildTestBundle(t)
	res, err := Verify(t.Context(), VerifyOptions{
		Input: summaryDirOf(t, dir),
		Key:   filepath.Join(t.TempDir(), "absent.pub"),
	})
	if res != nil {
		t.Errorf("Verify returned a result %+v for an operator error, want nil", res)
	}
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
}

// TestVerifiedRekorLogIndex pins that a Rekor index is reported only when the
// transparency log was verified: under IgnoreTLog the entry was never checked.
func TestVerifiedRekorLogIndex(t *testing.T) {
	withEntry := &bundle.Bundle{Bundle: &protobundle.Bundle{
		VerificationMaterial: &protobundle.VerificationMaterial{
			TlogEntries: []*protorekor.TransparencyLogEntry{{LogIndex: 42}},
		},
	}}
	noEntry := &bundle.Bundle{Bundle: &protobundle.Bundle{VerificationMaterial: &protobundle.VerificationMaterial{}}}

	if got := verifiedRekorLogIndex(withEntry, false); got == nil || *got != 42 {
		t.Errorf("verified tlog: index = %v, want 42", got)
	}
	if got := verifiedRekorLogIndex(withEntry, true); got != nil {
		t.Errorf("ignored tlog: index = %d, want nil", *got)
	}
	if got := verifiedRekorLogIndex(noEntry, false); got != nil {
		t.Errorf("no entry: index = %d, want nil", *got)
	}
}

func TestComparableSignerClaim(t *testing.T) {
	idx := int64(42)
	claimed := &attestation.PointerSigner{Identity: "sha256:aa", Issuer: attestation.KeySignerIssuer, RekorLogIndex: &idx}

	if got := comparableSignerClaim(claimed, false); got != claimed {
		t.Errorf("verified tlog: claim = %+v, want the pointer's own", got)
	}
	got := comparableSignerClaim(claimed, true)
	if got.RekorLogIndex != nil || got.Identity != claimed.Identity || got.Issuer != claimed.Issuer {
		t.Errorf("ignored tlog: claim = %+v, want identity and issuer without an index", got)
	}
	if claimed.RekorLogIndex == nil {
		t.Error("comparableSignerClaim modified the pointer's claim")
	}
	if comparableSignerClaim(nil, true) != nil {
		t.Error("nil claim must stay nil")
	}
}
