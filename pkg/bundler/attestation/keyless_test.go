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
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/url"
	"testing"
	"time"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
)

func TestNewKeylessAttester(t *testing.T) {
	attester := NewKeylessAttester("test-oidc-token")

	if attester == nil {
		t.Fatal("NewKeylessAttester() returned nil")
	}
}

func TestKeylessAttester_Identity(t *testing.T) {
	attester := NewKeylessAttester("test-oidc-token")

	// Identity is not known until after Attest() succeeds (Fulcio returns it).
	// Before signing, identity should be empty.
	if got := attester.Identity(); got != "" {
		t.Errorf("Identity() before Attest = %q, want empty string", got)
	}
}

func TestKeylessAttester_HasRekorEntry(t *testing.T) {
	attester := NewKeylessAttester("test-oidc-token")

	if !attester.HasRekorEntry() {
		t.Error("HasRekorEntry() = false, want true (keyless always uses Rekor)")
	}
}

func TestKeylessAttester_ImplementsAttester(t *testing.T) {
	var _ Attester = (*KeylessAttester)(nil)
}

// createTestCert generates a self-signed X.509 certificate with the given SANs.
func createTestCert(t *testing.T, emails []string, uris []*url.URL) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{
		SerialNumber:   big.NewInt(1),
		Subject:        pkix.Name{CommonName: "test"},
		NotBefore:      time.Now().Add(-time.Hour),
		NotAfter:       time.Now().Add(time.Hour),
		EmailAddresses: emails,
		URIs:           uris,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return certDER
}

// buildTestBundle creates a protobuf Bundle with the given certificate DER bytes.
func buildTestBundle(certDER []byte) *protobundle.Bundle {
	return &protobundle.Bundle{
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_Certificate{
				Certificate: &protocommon.X509Certificate{
					RawBytes: certDER,
				},
			},
		},
	}
}

func TestExtractSignerIdentity_Email(t *testing.T) {
	certDER := createTestCert(t, []string{"jdoe@company.com"}, nil)
	bundle := buildTestBundle(certDER)

	got := extractSignerIdentity(bundle)
	if got != "jdoe@company.com" {
		t.Errorf("extractSignerIdentity() = %q, want %q", got, "jdoe@company.com")
	}
}

func TestExtractSignerIdentity_URI(t *testing.T) {
	u, _ := url.Parse("https://github.com/NVIDIA/aicr/.github/workflows/on-tag.yaml@refs/tags/v1.0.0")
	certDER := createTestCert(t, nil, []*url.URL{u})
	bundle := buildTestBundle(certDER)

	got := extractSignerIdentity(bundle)
	if got != u.String() {
		t.Errorf("extractSignerIdentity() = %q, want %q", got, u.String())
	}
}

func TestExtractSignerIdentity_NilBundle(t *testing.T) {
	got := extractSignerIdentity(nil)
	if got != "" {
		t.Errorf("extractSignerIdentity(nil) = %q, want empty", got)
	}
}

func TestExtractSignerIdentity_NoCert(t *testing.T) {
	bundle := &protobundle.Bundle{
		VerificationMaterial: &protobundle.VerificationMaterial{},
	}

	got := extractSignerIdentity(bundle)
	if got != "" {
		t.Errorf("extractSignerIdentity() with no cert = %q, want empty", got)
	}
}

func TestExtractSignerIdentity_InvalidCertDER(t *testing.T) {
	bundle := buildTestBundle([]byte("not a certificate"))

	got := extractSignerIdentity(bundle)
	if got != "" {
		t.Errorf("extractSignerIdentity() with invalid cert = %q, want empty", got)
	}
}

func TestExtractSignerIdentity_CertChain(t *testing.T) {
	// Test the X509CertificateChain path (instead of single Certificate)
	certDER := createTestCert(t, []string{"chain@company.com"}, nil)
	bundle := &protobundle.Bundle{
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_X509CertificateChain{
				X509CertificateChain: &protocommon.X509CertificateChain{
					Certificates: []*protocommon.X509Certificate{
						{RawBytes: certDER},
					},
				},
			},
		},
	}

	got := extractSignerIdentity(bundle)
	if got != "chain@company.com" {
		t.Errorf("extractSignerIdentity() from chain = %q, want %q", got, "chain@company.com")
	}
}

func TestExtractSignerIdentity_EmptyCertChain(t *testing.T) {
	bundle := &protobundle.Bundle{
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_X509CertificateChain{
				X509CertificateChain: &protocommon.X509CertificateChain{
					Certificates: []*protocommon.X509Certificate{},
				},
			},
		},
	}

	got := extractSignerIdentity(bundle)
	if got != "" {
		t.Errorf("extractSignerIdentity() with empty chain = %q, want empty", got)
	}
}

func TestExtractSignerIdentity_NoSAN(t *testing.T) {
	// Cert with no email or URI SANs
	certDER := createTestCert(t, nil, nil)
	bundle := buildTestBundle(certDER)

	got := extractSignerIdentity(bundle)
	if got != "" {
		t.Errorf("extractSignerIdentity() with no SAN = %q, want empty", got)
	}
}
