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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"net/url"
	"testing"
	"time"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	rekorv1 "github.com/sigstore/protobuf-specs/gen/pb-go/rekor/v1"
	"github.com/sigstore/sigstore-go/pkg/sign"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/NVIDIA/aicr/pkg/errors"

	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

func TestSignStatementFor_RejectsEmptyStatement(t *testing.T) {
	id := newFakeKeyIdentity(t, "awskms://test/key")
	_, err := signStatementFor(context.Background(), FixedStatement(nil), id, NewNoTLogPolicy())
	if errorstest.ReportedCode(err) != errors.ErrCodeInvalidRequest {
		t.Errorf("want ErrCodeInvalidRequest for empty statement, got %v", err)
	}
}

func TestSignStatement_RejectsMissingOIDCToken(t *testing.T) {
	_, err := SignStatement(context.Background(), FixedStatement([]byte("{}")), SignOptions{})
	if err == nil {
		t.Errorf("expected error when OIDCToken is unset")
	}
}

func TestSignStatement_RejectsNilBuilder(t *testing.T) {
	_, err := SignStatement(context.Background(), nil, SignOptions{OIDCToken: "test-oidc-token"})
	errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
}

// TestExtractSignerClaims covers a single Certificate and an
// X509CertificateChain, missing or malformed certificates, each SAN form
// Fulcio issues, and the email and URI SAN precedence sigstore-go
// verification uses.
func TestExtractSignerClaims(t *testing.T) {
	const oidcIssuer = "https://accounts.example.com"
	workloadURI, err := url.Parse("https://github.com/NVIDIA/aicr/.github/workflows/on-tag.yaml@refs/tags/v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	chainBundle := func(certs ...*protocommon.X509Certificate) *protobundle.Bundle {
		return &protobundle.Bundle{
			VerificationMaterial: &protobundle.VerificationMaterial{
				Content: &protobundle.VerificationMaterial_X509CertificateChain{
					X509CertificateChain: &protocommon.X509CertificateChain{Certificates: certs},
				},
			},
		}
	}

	tests := []struct {
		name         string
		bundle       *protobundle.Bundle
		wantIdentity string
		wantIssuer   string
	}{
		{
			name:         "email SAN (interactive OIDC)",
			bundle:       certBundle(testCertDER(t, &x509.Certificate{EmailAddresses: []string{"jdoe@company.com"}})),
			wantIdentity: "jdoe@company.com",
		},
		{
			name: "email SAN with Fulcio issuer extension",
			bundle: certBundle(testCertDER(t, &x509.Certificate{
				EmailAddresses:  []string{"jdoe@company.com"},
				ExtraExtensions: []pkix.Extension{fulcioIssuerExt(t, oidcIssuer)},
			})),
			wantIdentity: "jdoe@company.com",
			wantIssuer:   oidcIssuer,
		},
		{
			name:         "URI SAN (workload OIDC)",
			bundle:       certBundle(testCertDER(t, &x509.Certificate{URIs: []*url.URL{workloadURI}})),
			wantIdentity: workloadURI.String(),
		},
		{
			name: "URI SAN preferred over email SAN",
			bundle: certBundle(testCertDER(t, &x509.Certificate{
				EmailAddresses: []string{"jdoe@company.com"},
				URIs:           []*url.URL{workloadURI},
			})),
			wantIdentity: workloadURI.String(),
		},
		{
			name:   "Fulcio username SAN only",
			bundle: certBundle(testCertDER(t, &x509.Certificate{ExtraExtensions: []pkix.Extension{otherNameSANExt(t, "jdoe!example.com")}})),
		},
		{
			name:         "X509CertificateChain with one cert",
			bundle:       chainBundle(&protocommon.X509Certificate{RawBytes: testCertDER(t, &x509.Certificate{EmailAddresses: []string{"chain@company.com"}})}),
			wantIdentity: "chain@company.com",
		},
		{name: "nil bundle pointer", bundle: nil},
		{name: "bundle without verification material", bundle: &protobundle.Bundle{}},
		{name: "verification material without cert", bundle: &protobundle.Bundle{VerificationMaterial: &protobundle.VerificationMaterial{}}},
		{name: "invalid cert DER", bundle: certBundle([]byte("not a certificate"))},
		{name: "X509CertificateChain with empty cert list", bundle: chainBundle()},
		{name: "cert with no SAN", bundle: certBundle(testCertDER(t, &x509.Certificate{}))},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			identity, issuer := extractSignerClaims(tc.bundle)
			if identity != tc.wantIdentity {
				t.Errorf("identity = %q, want %q", identity, tc.wantIdentity)
			}
			if issuer != tc.wantIssuer {
				t.Errorf("issuer = %q, want %q", issuer, tc.wantIssuer)
			}
		})
	}
}

func TestExtractIssuerExtension(t *testing.T) {
	const issuerURL = "https://accounts.example.com"
	// asn1Encoded is the DER-encoded UTF8String form Fulcio uses for the
	// current OID; this matches the production wire format.
	asn1Encoded, err := asn1.Marshal(issuerURL)
	if err != nil {
		t.Fatalf("asn1.Marshal: %v", err)
	}
	currentOID := asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}
	legacyOID := asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1}

	tests := []struct {
		name string
		exts []pkix.Extension
		want string
	}{
		{
			name: "current OID with proper ASN.1 UTF8String",
			exts: []pkix.Extension{{Id: currentOID, Value: asn1Encoded}},
			want: issuerURL,
		},
		{
			name: "legacy OID with raw UTF-8 bytes",
			exts: []pkix.Extension{{Id: legacyOID, Value: []byte(issuerURL)}},
			want: issuerURL,
		},
		{
			name: "current OID with malformed ASN.1 returns empty",
			exts: []pkix.Extension{{Id: currentOID, Value: []byte("\xffnot valid asn1")}},
			want: "",
		},
		{
			// Trailing bytes after a well-formed UTF8String must be
			// rejected: a tag-stuffed extension that decodes cleanly
			// for the first value but carries appended data should
			// not silently pass through.
			name: "current OID with trailing bytes after valid UTF8String returns empty",
			exts: []pkix.Extension{{Id: currentOID, Value: append(append([]byte{}, asn1Encoded...), 0x42, 0x43)}},
			want: "",
		},
		{
			name: "no Fulcio issuer extension returns empty",
			exts: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 17}, Value: []byte("san placeholder")}},
			want: "",
		},
		{
			// Precedence is order-independent: current OID wins even when
			// the legacy OID appears earlier in the extension list. Pins
			// the two-pass scan against a regression to single-pass
			// switch-and-return, which would silently pick whichever
			// extension Fulcio happened to stamp first.
			name: "current OID takes precedence over legacy regardless of cert order",
			exts: []pkix.Extension{
				{Id: legacyOID, Value: []byte("legacy-only")},
				{Id: currentOID, Value: asn1Encoded},
			},
			want: issuerURL,
		},
		{
			// Symmetric: when only the legacy OID is present, the legacy
			// branch supplies the value.
			name: "legacy OID used when current OID is absent",
			exts: []pkix.Extension{{Id: legacyOID, Value: []byte("legacy-only")}},
			want: "legacy-only",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cert := &x509.Certificate{Extensions: tc.exts}
			if got := extractIssuerExtension(cert); got != tc.want {
				t.Errorf("extractIssuerExtension = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractRekorLogIndex(t *testing.T) {
	tests := []struct {
		name   string
		bundle *protobundle.Bundle
		want   int64
	}{
		{
			name: "first entry returned when multiple present",
			bundle: &protobundle.Bundle{
				VerificationMaterial: &protobundle.VerificationMaterial{
					TlogEntries: []*rekorv1.TransparencyLogEntry{
						{LogIndex: 42},
						{LogIndex: 999},
					},
				},
			},
			want: 42,
		},
		{
			name:   "no entries",
			bundle: &protobundle.Bundle{VerificationMaterial: &protobundle.VerificationMaterial{}},
			want:   0,
		},
		{
			name:   "nil VerificationMaterial",
			bundle: &protobundle.Bundle{},
			want:   0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractRekorLogIndex(tc.bundle); got != tc.want {
				t.Errorf("LogIndex = %d, want %d", got, tc.want)
			}
		})
	}
}

// --- helpers ---

// testCertDER returns a DER-encoded self-signed certificate with the SANs
// and extensions in template. It overwrites the serial number, subject and
// validity in template.
func testCertDER(t *testing.T, template *x509.Certificate) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template.SerialNumber = big.NewInt(1)
	template.Subject = pkix.Name{CommonName: "test"}
	template.NotBefore = time.Now().Add(-time.Hour)
	template.NotAfter = time.Now().Add(time.Hour)
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return der
}

// certBundle returns a Sigstore bundle whose verification material is
// certDER.
func certBundle(certDER []byte) *protobundle.Bundle {
	return &protobundle.Bundle{
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_Certificate{
				Certificate: &protocommon.X509Certificate{RawBytes: certDER},
			},
		},
	}
}

// fulcioIssuerExt encodes the OIDC issuer for the current Fulcio
// extension (OID 1.3.6.1.4.1.57264.1.8) as a DER-encoded ASN.1 UTF8String,
// matching how Fulcio itself stamps the value into real signing certs.
func fulcioIssuerExt(t *testing.T, oidcIssuer string) pkix.Extension {
	t.Helper()
	encoded, err := asn1.Marshal(oidcIssuer)
	if err != nil {
		t.Fatalf("marshal issuer ASN.1: %v", err)
	}
	return pkix.Extension{
		Id:    asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8},
		Value: encoded,
	}
}

// otherNameSANExt encodes name as the Fulcio username OtherName SAN.
func otherNameSANExt(t *testing.T, name string) pkix.Extension {
	t.Helper()
	ext, err := cryptoutils.MarshalOtherNameSAN(name, false)
	if err != nil {
		t.Fatalf("marshal OtherName SAN: %v", err)
	}
	return *ext
}

// TestSignStatementWithRejectsNilStrategies verifies SignStatementWith returns
// a structured ErrCodeInvalidRequest (rather than panicking) when the signing
// identity and/or transparency policy is nil.
func TestSignStatementWithRejectsNilStrategies(t *testing.T) {
	tests := []struct {
		name string
		id   SigningIdentity
		tlog TransparencyPolicy
	}{
		{"nil identity", nil, NewNoTLogPolicy()},
		{"nil policy", newFakeKeyIdentity(t, "awskms://test/key"), nil},
		{"both nil", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SignStatementWith(context.Background(), validStatementJSON(t), tt.id, tt.tlog)
			if errorstest.ReportedCode(err) != errors.ErrCodeInvalidRequest {
				t.Errorf("want ErrCodeInvalidRequest, got %v", err)
			}
		})
	}
}

// TestSignStatementWithKeyOnlyNoTLog exercises the composer's key-only,
// no-certificate, no-transparency-log branch fully offline: a local ECDSA
// signer (no KMS RPC) paired with NewNoTLogPolicy() (no Rekor call). It pins
// the contract that key-based signing produces public-key verification
// material, no tlog entries, and falls back to the identity's URI.
func TestSignStatementWithKeyOnlyNoTLog(t *testing.T) {
	id := newFakeKeyIdentity(t, "awskms://test/key")
	res, err := SignStatementWith(context.Background(), validStatementJSON(t), id, NewNoTLogPolicy())
	if err != nil {
		t.Fatalf("SignStatementWith: %v", err)
	}
	if res.Identity != "awskms://test/key" {
		t.Errorf("Identity = %q, want fallback URI", res.Identity)
	}
	if res.RekorLogIndex != 0 {
		t.Errorf("RekorLogIndex = %d, want 0 (no tlog)", res.RekorLogIndex)
	}
	var b protobundle.Bundle
	if err := protojson.Unmarshal(res.BundleJSON, &b); err != nil {
		t.Fatal(err)
	}
	if b.GetVerificationMaterial().GetPublicKey() == nil {
		t.Error("want public-key verification material (no cert) for key-based signing")
	}
	if len(b.GetVerificationMaterial().GetTlogEntries()) != 0 {
		t.Error("want no tlog entries")
	}
}

// TestSignStatementFor_PassesSigner checks the signer that the statement
// builder receives. Keyless signing passes the issued certificate's SAN, and
// key signing passes the fallback identity. Both cases run offline with no
// transparency log.
func TestSignStatementFor_PassesSigner(t *testing.T) {
	const email = "jdoe@company.com"
	tests := []struct {
		name string
		id   SigningIdentity
		want string
	}{
		{"keyless", newFakeCertIdentity(t, email), email},
		{"key", newFakeKeyIdentity(t, "awskms://test/signer"), "awskms://test/signer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			build := func(signer string) ([]byte, error) {
				got = signer
				return statementJSONFor(t, signer), nil
			}
			res, err := signStatementFor(context.Background(), build, tt.id, NewNoTLogPolicy())
			if err != nil {
				t.Fatalf("signStatementFor: %v", err)
			}
			if got != tt.want {
				t.Errorf("build signer = %q, want %q", got, tt.want)
			}
			if res.Identity != tt.want {
				t.Errorf("Identity = %q, want %q", res.Identity, tt.want)
			}
			var b protobundle.Bundle
			if err := protojson.Unmarshal(res.BundleJSON, &b); err != nil {
				t.Fatalf("unmarshal bundle: %v", err)
			}
			stmt := decodeStatement(t, b.GetDsseEnvelope().GetPayload())
			if id := stmt.Predicate.RunDetails.Builder.ID; id != tt.want {
				t.Errorf("signed builder.id = %q, want %q", id, tt.want)
			}
		})
	}
}

// TestSignStatementFor_ReusesIssuedCertificate checks that keyless signing
// retries a failed certificate request, requests one certificate per
// successful issuance, and signs the bundle with that certificate.
func TestSignStatementFor_ReusesIssuedCertificate(t *testing.T) {
	tests := []struct {
		name      string
		failures  int
		wantCalls int
	}{
		{"first request succeeds", 0, 1},
		{"first request fails", 1, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, ok := newFakeCertIdentity(t, "jdoe@company.com").(fakeCertIdentity)
			if !ok {
				t.Fatal("newFakeCertIdentity did not return a fakeCertIdentity")
			}
			provider := &countingCertProvider{inner: base.provider, failures: tt.failures}
			id := fakeCertIdentity{kp: base.kp, provider: provider}

			res, err := signStatementFor(context.Background(), func(signer string) ([]byte, error) {
				return statementJSONFor(t, signer), nil
			}, id, NewNoTLogPolicy())
			if err != nil {
				t.Fatalf("signStatementFor: %v", err)
			}
			if provider.calls != tt.wantCalls {
				t.Errorf("GetCertificate calls = %d, want %d", provider.calls, tt.wantCalls)
			}
			if len(provider.issued) != 1 {
				t.Fatalf("issued %d certificates, want 1", len(provider.issued))
			}
			var b protobundle.Bundle
			if err := protojson.Unmarshal(res.BundleJSON, &b); err != nil {
				t.Fatalf("unmarshal bundle: %v", err)
			}
			if got := b.GetVerificationMaterial().GetCertificate().GetRawBytes(); !bytes.Equal(got, provider.issued[0]) {
				t.Error("bundle certificate differs from the issued certificate")
			}
		})
	}
}

// countingCertProvider wraps a sign.CertificateProvider. It returns an error
// for its first failures calls and records each certificate inner issues.
type countingCertProvider struct {
	inner    sign.CertificateProvider
	failures int
	calls    int
	issued   [][]byte
}

func (p *countingCertProvider) GetCertificate(ctx context.Context, kp sign.Keypair, opts *sign.CertificateProviderOptions) ([]byte, error) {
	p.calls++
	if p.calls <= p.failures {
		return nil, errors.New(errors.ErrCodeUnavailable, "simulated Fulcio failure")
	}
	der, err := p.inner.GetCertificate(ctx, kp, opts)
	if err != nil {
		return nil, err
	}
	p.issued = append(p.issued, der)
	return der, nil
}

// fakeKeyIdentity is a test SigningIdentity backed by a local ECDSA signer
// (no KMS, no network) with no Fulcio certificate provider. It mirrors the
// production kmsIdentity contract: public-key signing with a fixed fallback
// URI as the audit identity.
type fakeKeyIdentity struct {
	signer kmsSigner
	uri    string
}

// newFakeKeyIdentity returns a SigningIdentity whose Keypair is a local ECDSA
// P-256 key wrapped via newKMSKeypairFromSigner, with no cert provider and the
// given fallback URI. Used to drive SignStatementWith down its key-only path
// offline.
func newFakeKeyIdentity(t *testing.T, uri string) SigningIdentity {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return &fakeKeyIdentity{signer: &localECDSASigner{priv: priv}, uri: uri}
}

func (f *fakeKeyIdentity) Keypair(_ context.Context) (sign.Keypair, error) {
	return newKMSKeypairFromSigner(f.signer)
}

func (f *fakeKeyIdentity) CertProvider() (sign.CertificateProvider, *sign.CertificateProviderOptions) {
	return nil, nil
}

func (f *fakeKeyIdentity) FallbackIdentity() string { return f.uri }

// validStatementJSON builds a valid protobuf-JSON in-toto statement via the
// package's BuildStatement so the payload is spec-valid (no hand-rolled JSON).
func validStatementJSON(t *testing.T) []byte {
	t.Helper()
	return statementJSONFor(t, "awskms://test/key")
}

// statementJSONFor builds a valid in-toto statement whose builder ID is
// builderID.
func statementJSONFor(t *testing.T, builderID string) []byte {
	t.Helper()
	stmt, err := BuildStatement(
		AttestSubject{
			Name:   "checksums.txt",
			Digest: map[string]string{"sha256": "0000000000000000000000000000000000000000000000000000000000000000"},
		},
		StatementMetadata{
			Recipe:      "test-recipe",
			ToolVersion: "v0.0.0-test",
			BuilderID:   builderID,
		},
	)
	if err != nil {
		t.Fatalf("BuildStatement: %v", err)
	}
	return stmt
}
