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

package evidence

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"testing"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

// The fingerprint read from a key signature's hint is the one AICR names
// the key by: "sha256:" and the hex SHA-256 of its PKIX DER encoding.
func TestKeyFingerprint(t *testing.T) {
	t.Parallel()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(der)
	hint := base64.StdEncoding.EncodeToString(sum[:])
	signed := `{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json","verificationMaterial":{"publicKey":{"hint":"` + hint +
		`"}},"dsseEnvelope":{"payload":"e30=","payloadType":"application/vnd.in-toto+json","signatures":[{"sig":"c2ln"}]}}` + "\n"
	got, err := KeyFingerprint([]byte(signed))
	if want := "sha256:" + hex.EncodeToString(sum[:]); err != nil || got != want {
		t.Fatalf("KeyFingerprint() = %q, %v; want %q", got, err, want)
	}
	if again, err := KeyFingerprint([]byte("\n" + signed)); err != nil || again != got {
		t.Fatalf("KeyFingerprint() after a blank line = %q, %v", again, err)
	}
	for name, attestation := range map[string]string{
		"keyless":      `{"verificationMaterial":{"certificate":{"rawBytes":"MII="}}}`,
		"short hint":   `{"verificationMaterial":{"publicKey":{"hint":"` + base64.StdEncoding.EncodeToString(sum[:8]) + `"}}}`,
		"hint not b64": `{"verificationMaterial":{"publicKey":{"hint":"not base64!"}}}`,
		"not a bundle": `statement`,
		"empty":        ``,
		"key ID only":  `{"verificationMaterial":{"publicKey":{}}}`,
		"hex not b64":  `{"verificationMaterial":{"publicKey":{"hint":"` + hex.EncodeToString(sum[:]) + `"}}}`,
	} {
		if _, err := KeyFingerprint([]byte(attestation)); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
			t.Errorf("%s: KeyFingerprint() = %v, want INVALID_REQUEST", name, err)
		}
	}
}
