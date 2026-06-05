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
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	stderrors "errors"
	"testing"

	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestKMSKeypairSignAndVerify(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kp, err := newKMSKeypairFromSigner(&localECDSASigner{priv: priv})
	if err != nil {
		t.Fatalf("newKMSKeypairFromSigner: %v", err)
	}

	if kp.GetHashAlgorithm() != protocommon.HashAlgorithm_SHA2_256 {
		t.Errorf("hash alg = %v", kp.GetHashAlgorithm())
	}
	if kp.GetKeyAlgorithm() != "ECDSA" {
		t.Errorf("key alg = %q", kp.GetKeyAlgorithm())
	}
	if kp.GetSigningAlgorithm() != protocommon.PublicKeyDetails_PKIX_ECDSA_P256_SHA_256 {
		t.Errorf("signing alg = %v", kp.GetSigningAlgorithm())
	}
	if _, ok := kp.GetPublicKey().(*ecdsa.PublicKey); !ok {
		t.Errorf("GetPublicKey type = %T, want *ecdsa.PublicKey", kp.GetPublicKey())
	}
	pem, err := kp.GetPublicKeyPem()
	if err != nil || pem == "" {
		t.Fatalf("GetPublicKeyPem: %v / %q", err, pem)
	}

	sig, digest, err := kp.SignData(context.Background(), []byte("hello"))
	if err != nil {
		t.Fatalf("SignData: %v", err)
	}
	if !ecdsa.VerifyASN1(&priv.PublicKey, digest, sig) {
		t.Error("signature does not verify over returned digest")
	}
}

func TestClassifyPublicKey(t *testing.T) {
	ecP256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecP384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsa2048, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsa4096, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		t.Fatal(err)
	}
	ed25519Pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		pub        crypto.PublicKey
		wantKeyAlg string
		wantSigAlg protocommon.PublicKeyDetails
		wantErr    bool
	}{
		{"ecdsa p256", &ecP256.PublicKey, "ECDSA", protocommon.PublicKeyDetails_PKIX_ECDSA_P256_SHA_256, false},
		{"rsa 2048", &rsa2048.PublicKey, "RSA", protocommon.PublicKeyDetails_PKIX_RSA_PKCS1V15_2048_SHA256, false},
		{"ecdsa p384 rejected", &ecP384.PublicKey, "", protocommon.PublicKeyDetails_PUBLIC_KEY_DETAILS_UNSPECIFIED, true},
		{"rsa 4096 rejected", &rsa4096.PublicKey, "", protocommon.PublicKeyDetails_PUBLIC_KEY_DETAILS_UNSPECIFIED, true},
		{"ed25519 unsupported type", ed25519Pub, "", protocommon.PublicKeyDetails_PUBLIC_KEY_DETAILS_UNSPECIFIED, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyAlg, sigAlg, err := classifyPublicKey(tt.pub)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
					t.Errorf("want ErrCodeInvalidRequest, got %v", err)
				}
				return
			}
			if keyAlg != tt.wantKeyAlg {
				t.Errorf("keyAlg = %q, want %q", keyAlg, tt.wantKeyAlg)
			}
			if sigAlg != tt.wantSigAlg {
				t.Errorf("sigAlg = %v, want %v", sigAlg, tt.wantSigAlg)
			}
		})
	}
}

// localECDSASigner is a test kmsSigner backed by an in-memory ECDSA key.
type localECDSASigner struct{ priv *ecdsa.PrivateKey }

func (s *localECDSASigner) Public() (crypto.PublicKey, error) { return &s.priv.PublicKey, nil }
func (s *localECDSASigner) SignDigest(_ context.Context, digest []byte) ([]byte, error) {
	return ecdsa.SignASN1(rand.Reader, s.priv, digest)
}
