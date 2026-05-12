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
)

func TestSignStatement_RejectsEmptyStatement(t *testing.T) {
	_, err := SignStatement(context.Background(), nil, SignOptions{OIDCToken: "tok"})
	if err == nil {
		t.Errorf("expected error for empty statement")
	}
}

func TestSignStatement_RejectsMissingOIDCToken(t *testing.T) {
	_, err := SignStatement(context.Background(), []byte("{}"), SignOptions{})
	if err == nil {
		t.Errorf("expected error when OIDCToken is unset")
	}
}

func TestExtractSignerClaims_FromEmail(t *testing.T) {
	bundle := bundleWithEmailCert(t, "jdoe@company.com", "")
	identity, issuer := extractSignerClaims(bundle)
	if identity != "jdoe@company.com" {
		t.Errorf("identity = %q, want jdoe@company.com", identity)
	}
	if issuer != "" {
		t.Errorf("issuer = %q, want empty (no extension)", issuer)
	}
}

func TestExtractSignerClaims_FromURI(t *testing.T) {
	u, err := url.Parse("https://github.com/login/oauth")
	if err != nil {
		t.Fatal(err)
	}
	bundle := bundleWithURICert(t, u, "")
	identity, _ := extractSignerClaims(bundle)
	if identity != u.String() {
		t.Errorf("identity = %q, want %q", identity, u.String())
	}
}

func TestExtractSignerClaims_PullsIssuerFromExtension(t *testing.T) {
	const oidcIssuer = "https://accounts.example.com"
	bundle := bundleWithEmailCert(t, "jdoe@company.com", oidcIssuer)
	identity, issuer := extractSignerClaims(bundle)
	if identity != "jdoe@company.com" {
		t.Errorf("identity = %q", identity)
	}
	if issuer != oidcIssuer {
		t.Errorf("issuer = %q, want %q", issuer, oidcIssuer)
	}
}

func TestExtractSignerClaims_NilBundle(t *testing.T) {
	identity, issuer := extractSignerClaims(&protobundle.Bundle{})
	if identity != "" || issuer != "" {
		t.Errorf("expected empty claims for empty bundle; got %q / %q", identity, issuer)
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

// bundleWithEmailCert produces a Sigstore bundle containing a self-signed
// X.509 cert with an email SAN and an optional Fulcio OIDC-issuer
// extension (OID 1.3.6.1.4.1.57264.1.8). Used by claim-extraction tests.
func bundleWithEmailCert(t *testing.T, email, oidcIssuer string) *protobundle.Bundle {
	t.Helper()
	template := &x509.Certificate{
		SerialNumber:   big.NewInt(1),
		Subject:        pkix.Name{CommonName: "test"},
		NotBefore:      time.Now(),
		NotAfter:       time.Now().Add(time.Hour),
		EmailAddresses: []string{email},
	}
	if oidcIssuer != "" {
		template.ExtraExtensions = []pkix.Extension{fulcioIssuerExt(t, oidcIssuer)}
	}
	return bundleFromTemplate(t, template)
}

// bundleWithURICert produces a Sigstore bundle containing a self-signed
// X.509 cert with a URI SAN — used to exercise the workload-OIDC path
// (GitHub Actions, Kubernetes service account, etc.).
func bundleWithURICert(t *testing.T, u *url.URL, oidcIssuer string) *protobundle.Bundle {
	t.Helper()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-uri"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
		URIs:         []*url.URL{u},
	}
	if oidcIssuer != "" {
		template.ExtraExtensions = []pkix.Extension{fulcioIssuerExt(t, oidcIssuer)}
	}
	return bundleFromTemplate(t, template)
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

func bundleFromTemplate(t *testing.T, template *x509.Certificate) *protobundle.Bundle {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return &protobundle.Bundle{
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_Certificate{
				Certificate: &protocommon.X509Certificate{RawBytes: der},
			},
		},
	}
}
