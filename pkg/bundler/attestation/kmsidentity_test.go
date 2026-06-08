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
	"crypto/elliptic"
	"crypto/rand"
	stderrors "errors"
	"io"
	"testing"

	"github.com/sigstore/sigstore/pkg/signature"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestKMSIdentityFallbackIdentity(t *testing.T) {
	id := NewKMSIdentity("awskms://arn:aws:kms:us-east-1:111:key/abc")
	if id.FallbackIdentity() != "awskms://arn:aws:kms:us-east-1:111:key/abc" {
		t.Errorf("FallbackIdentity = %q", id.FallbackIdentity())
	}
	cp, opts := id.CertProvider()
	if cp != nil || opts != nil {
		t.Error("KMS identity must have no cert provider")
	}
}

func TestKMSIdentityUnknownScheme(t *testing.T) {
	id := NewKMSIdentity("file:///tmp/key") // not a KMS scheme
	_, err := id.Keypair(context.Background())
	if err == nil {
		t.Fatal("expected error for unsupported scheme")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("want ErrCodeInvalidRequest, got %v", err)
	}
}

// fakeRemoteSigner is a minimal kmsRemoteSigner for exercising the
// kmsSignerVerifier adapter without a real KMS client.
type fakeRemoteSigner struct {
	pub    crypto.PublicKey
	sig    []byte
	signer error
}

func (f *fakeRemoteSigner) PublicKey(_ ...signature.PublicKeyOption) (crypto.PublicKey, error) {
	return f.pub, nil
}

func (f *fakeRemoteSigner) SignMessage(_ io.Reader, _ ...signature.SignOption) ([]byte, error) {
	if f.signer != nil {
		return nil, f.signer
	}
	return f.sig, nil
}

func TestKMSSignerVerifierAdapter(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("public key is the captured key", func(t *testing.T) {
		a := &kmsSignerVerifier{sv: &fakeRemoteSigner{}, pub: &priv.PublicKey}
		got, err := a.Public()
		if err != nil {
			t.Fatalf("Public: %v", err)
		}
		if got != crypto.PublicKey(&priv.PublicKey) {
			t.Error("Public did not return the captured key")
		}
	})

	t.Run("sign digest forwards signer output", func(t *testing.T) {
		want := []byte("signature-bytes")
		a := &kmsSignerVerifier{sv: &fakeRemoteSigner{sig: want}, pub: &priv.PublicKey}
		got, err := a.SignDigest(context.Background(), []byte("01234567890123456789012345678901"))
		if err != nil {
			t.Fatalf("SignDigest: %v", err)
		}
		if string(got) != string(want) {
			t.Errorf("SignDigest = %q, want %q", got, want)
		}
	})

	t.Run("sign digest wraps signer error as unavailable", func(t *testing.T) {
		a := &kmsSignerVerifier{sv: &fakeRemoteSigner{signer: stderrors.New("kms down")}, pub: &priv.PublicKey}
		_, err := a.SignDigest(context.Background(), []byte("01234567890123456789012345678901"))
		if !stderrors.Is(err, errors.New(errors.ErrCodeUnavailable, "")) {
			t.Errorf("want ErrCodeUnavailable, got %v", err)
		}
	})
}
