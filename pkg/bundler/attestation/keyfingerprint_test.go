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
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/sign"
	"github.com/sigstore/sigstore/pkg/cryptoutils"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

// fingerprintTestPEM and fingerprintTestWant pin PublicKeyFingerprint to an
// external derivation: the want value is
// `openssl pkey -pubin -outform DER | shasum -a 256` over this key.
const (
	fingerprintTestPEM = `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEFw8t7XDoBg3Et5cI0nMBWVgjoyuA
woqUYnBZbUgnH2PvI3HR2FYAnZyVBYF4TOxpOAD3MuAJnePB1gPbSARbJQ==
-----END PUBLIC KEY-----
`
	fingerprintTestWant = "sha256:d93f4a5e57c66728cb33edea59e9fc0fdf246a5361a0b8c50905e497ef0e5c2b"
)

func TestPublicKeyFingerprint(t *testing.T) {
	pub, err := cryptoutils.UnmarshalPEMToPublicKey([]byte(fingerprintTestPEM))
	if err != nil {
		t.Fatalf("parse test key: %v", err)
	}
	got, err := PublicKeyFingerprint(pub)
	if err != nil {
		t.Fatalf("PublicKeyFingerprint: %v", err)
	}
	if got != fingerprintTestWant {
		t.Errorf("PublicKeyFingerprint = %q, want %q", got, fingerprintTestWant)
	}

	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if fp, _ := PublicKeyFingerprint(&other.PublicKey); fp == got {
		t.Error("two different keys share a fingerprint")
	}

	for name, bad := range map[string]any{"nil key": nil, "unsupported key type": struct{}{}} {
		t.Run(name, func(t *testing.T) {
			_, err := PublicKeyFingerprint(bad)
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}
}

// TestNewKeyVerificationIdentity_KeyFingerprint pins that a verifier reading a
// PEM file reports the same fingerprint a signer computes from the key itself.
func TestNewKeyVerificationIdentity_KeyFingerprint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signer.pub")
	if err := os.WriteFile(path, []byte(fingerprintTestPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := NewKeyVerificationIdentity(context.Background(), path, nil)
	if err != nil {
		t.Fatalf("NewKeyVerificationIdentity: %v", err)
	}
	fp, ok := id.(interface{ KeyFingerprint() string })
	if !ok {
		t.Fatal("key verification identity does not expose KeyFingerprint")
	}
	if fp.KeyFingerprint() != fingerprintTestWant {
		t.Errorf("KeyFingerprint = %q, want %q", fp.KeyFingerprint(), fingerprintTestWant)
	}
	if want := "pem:" + strings.TrimPrefix(fingerprintTestWant, "sha256:"); id.(interface{ Identity() string }).Identity() != want {
		t.Errorf("Identity = %q, want %q", id.(interface{ Identity() string }).Identity(), want)
	}
}

// selfSignedCertProvider issues a certificate for the signing key from a
// throwaway CA, so SignStatementWith's keyless branch runs offline.
type selfSignedCertProvider struct{ ca *ecdsa.PrivateKey }

func (p selfSignedCertProvider) GetCertificate(_ context.Context, kp sign.Keypair, _ *sign.CertificateProviderOptions) ([]byte, error) {
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "aicr-test"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(10 * time.Minute),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	return x509.CreateCertificate(rand.Reader, tmpl, tmpl, kp.GetPublicKey(), p.ca)
}

type fakeCertIdentity struct {
	kp       sign.Keypair
	provider sign.CertificateProvider
}

func (f fakeCertIdentity) Keypair(context.Context) (sign.Keypair, error) { return f.kp, nil }

func (f fakeCertIdentity) CertProvider() (sign.CertificateProvider, *sign.CertificateProviderOptions) {
	return f.provider, &sign.CertificateProviderOptions{}
}

func (fakeCertIdentity) FallbackIdentity() string { return "" }

// TestSignStatementWith_KeyFingerprint pins which signatures carry a key
// fingerprint: a key signature does, and it names the signing key; a
// certificate signature never does, since evidence would otherwise record a
// keyless signer as a key signer.
func TestSignStatementWith_KeyFingerprint(t *testing.T) {
	ctx := context.Background()

	t.Run("key signature", func(t *testing.T) {
		priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		id := &fakeKeyIdentity{signer: &localECDSASigner{priv: priv}, uri: "awskms://test/key"}
		res, err := SignStatementWith(ctx, validStatementJSON(t), id, NewNoTLogPolicy())
		if err != nil {
			t.Fatalf("SignStatementWith: %v", err)
		}
		want, err := PublicKeyFingerprint(&priv.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		if res.KeyFingerprint != want {
			t.Errorf("KeyFingerprint = %q, want %q", res.KeyFingerprint, want)
		}
	})

	t.Run("certificate signature", func(t *testing.T) {
		kp, err := sign.NewEphemeralKeypair(nil)
		if err != nil {
			t.Fatal(err)
		}
		ca, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		id := fakeCertIdentity{kp: kp, provider: selfSignedCertProvider{ca: ca}}
		res, err := SignStatementWith(ctx, validStatementJSON(t), id, NewNoTLogPolicy())
		if err != nil {
			t.Fatalf("SignStatementWith: %v", err)
		}
		if res.KeyFingerprint != "" {
			t.Errorf("KeyFingerprint = %q for a certificate signature, want empty", res.KeyFingerprint)
		}
	})
}

func TestSignStatementWithKey_RejectsBeforeSigning(t *testing.T) {
	tests := []struct {
		name string
		opts ResolveOptions
	}{
		{"no signing key", ResolveOptions{DisableTLogUpload: true}},
		{"signing key with device flow", ResolveOptions{SigningKey: "awskms:///alias/k", DeviceFlow: true, DisableTLogUpload: true}},
		{"unsupported key scheme", ResolveOptions{SigningKey: "file:///tmp/key", DisableTLogUpload: true}},
		{"unreadable signing config", ResolveOptions{SigningKey: "awskms:///alias/k", SigningConfigPath: "/no/such/signing_config.json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SignStatementWithKey(context.Background(), validStatementJSON(t), tt.opts)
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}
}
