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
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
	"github.com/NVIDIA/aicr/pkg/errors"
)

// writeTempPubPEM marshals pub to PKIX DER, PEM-encodes it under a
// "PUBLIC KEY" block, writes it to a temp file, and returns the path.
func writeTempPubPEM(t *testing.T, pub interface{}) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("failed to marshal public key: %v", err)
	}
	block := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, block, 0o600); err != nil {
		t.Fatalf("failed to write temp PEM: %v", err)
	}
	return path
}

func TestNewKeyVerificationIdentity_UnknownScheme(t *testing.T) {
	_, err := NewKeyVerificationIdentity(context.Background(), "bogus://x")
	// A "bogus://" ref is not a recognized KMS URI, so it falls through to the
	// PEM path and fails to open as a file — both surface ErrCodeInvalidRequest.
	requireErrCode(t, err, errors.ErrCodeInvalidRequest)
}

func TestNewKeyVerificationIdentity_PEM(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	pemPath := writeTempPubPEM(t, &priv.PublicKey)

	id, err := NewKeyVerificationIdentity(context.Background(), pemPath)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	idStr := id.(interface{ Identity() string }).Identity()
	if !strings.HasPrefix(idStr, "pem:") {
		t.Errorf("expected identity to have pem: prefix, got %q", idStr)
	}

	tm, err := id.TrustedMaterial(context.Background())
	if err != nil {
		t.Fatalf("TrustedMaterial returned error: %v", err)
	}
	if tm == nil {
		t.Fatal("expected non-nil TrustedMaterial")
	}

	if id.PolicyOption() == nil {
		t.Error("expected non-nil PolicyOption")
	}
}

func TestNewKeyVerificationIdentity_BadPEM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(path, []byte("not a pem"), 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	_, err := NewKeyVerificationIdentity(context.Background(), path)
	requireErrCode(t, err, errors.ErrCodeInvalidRequest)
}

func TestNewKeyVerificationIdentity_MissingFile(t *testing.T) {
	_, err := NewKeyVerificationIdentity(context.Background(), "/no/such/key.pem")
	requireErrCode(t, err, errors.ErrCodeInvalidRequest)
}

func TestNewKeyVerificationIdentity_OversizePEM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.pem")
	oversized := make([]byte, defaults.MaxPublicKeyPEMBytes+1)
	if err := os.WriteFile(path, oversized, 0o600); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	_, err := NewKeyVerificationIdentity(context.Background(), path)
	requireErrCode(t, err, errors.ErrCodeInvalidRequest)
}
