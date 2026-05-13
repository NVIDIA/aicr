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

// TestExtractSignerClaims_Identity covers identity extraction across every
// shape of bundle VerificationMaterial the function has to handle: single
// Certificate vs X509CertificateChain, nil/missing/malformed certs, and the
// two production SAN forms (email for interactive OIDC, URI for workload
// OIDC). The companion TestExtractSignerClaims_PullsIssuerFromExtension in
// signing_test.go covers the issuer side.
func TestExtractSignerClaims_Identity(t *testing.T) {
	workloadURI, _ := url.Parse("https://github.com/NVIDIA/aicr/.github/workflows/on-tag.yaml@refs/tags/v1.0.0")

	tests := []struct {
		name   string
		bundle func(t *testing.T) *protobundle.Bundle
		want   string
	}{
		{
			name: "email SAN (interactive OIDC)",
			bundle: func(t *testing.T) *protobundle.Bundle {
				return buildTestBundle(createTestCert(t, []string{"jdoe@company.com"}, nil))
			},
			want: "jdoe@company.com",
		},
		{
			name: "URI SAN (workload OIDC)",
			bundle: func(t *testing.T) *protobundle.Bundle {
				return buildTestBundle(createTestCert(t, nil, []*url.URL{workloadURI}))
			},
			want: workloadURI.String(),
		},
		{
			name:   "nil bundle pointer",
			bundle: func(_ *testing.T) *protobundle.Bundle { return nil },
			want:   "",
		},
		{
			name: "verification material without cert",
			bundle: func(_ *testing.T) *protobundle.Bundle {
				return &protobundle.Bundle{
					VerificationMaterial: &protobundle.VerificationMaterial{},
				}
			},
			want: "",
		},
		{
			name: "invalid cert DER",
			bundle: func(_ *testing.T) *protobundle.Bundle {
				return buildTestBundle([]byte("not a certificate"))
			},
			want: "",
		},
		{
			name: "X509CertificateChain with one cert",
			bundle: func(t *testing.T) *protobundle.Bundle {
				certDER := createTestCert(t, []string{"chain@company.com"}, nil)
				return &protobundle.Bundle{
					VerificationMaterial: &protobundle.VerificationMaterial{
						Content: &protobundle.VerificationMaterial_X509CertificateChain{
							X509CertificateChain: &protocommon.X509CertificateChain{
								Certificates: []*protocommon.X509Certificate{{RawBytes: certDER}},
							},
						},
					},
				}
			},
			want: "chain@company.com",
		},
		{
			name: "X509CertificateChain with empty cert list",
			bundle: func(_ *testing.T) *protobundle.Bundle {
				return &protobundle.Bundle{
					VerificationMaterial: &protobundle.VerificationMaterial{
						Content: &protobundle.VerificationMaterial_X509CertificateChain{
							X509CertificateChain: &protocommon.X509CertificateChain{
								Certificates: []*protocommon.X509Certificate{},
							},
						},
					},
				}
			},
			want: "",
		},
		{
			name: "cert with no email or URI SAN",
			bundle: func(t *testing.T) *protobundle.Bundle {
				return buildTestBundle(createTestCert(t, nil, nil))
			},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := extractSignerClaims(tc.bundle(t))
			if got != tc.want {
				t.Errorf("identity = %q, want %q", got, tc.want)
			}
		})
	}
}
