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
	"testing"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestKMSAttesterContract(t *testing.T) {
	var _ Attester = (*KMSAttester)(nil)
	a := NewKMSAttester("gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k", SignOptions{UseTUFSigningConfig: true})
	if !a.HasRekorEntry() {
		t.Error("default KMS attester uploads to Rekor")
	}
	if a.Identity() != "gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k" {
		t.Errorf("Identity = %q", a.Identity())
	}
}

// TestKMSAttesterWithoutTransparencyLog verifies the functional option opts the
// attester out of the Rekor upload: HasRekorEntry reports false, while a
// default (no-option) attester still reports true. See #409.
func TestKMSAttesterWithoutTransparencyLog(t *testing.T) {
	const keyURI = "gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k"

	cases := []struct {
		name      string
		opts      []KMSAttesterOption
		wantRekor bool
	}{
		{"default records a Rekor entry", nil, true},
		{"WithoutTransparencyLog records no entry", []KMSAttesterOption{WithoutTransparencyLog()}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewKMSAttester(keyURI, SignOptions{UseTUFSigningConfig: true}, tc.opts...)
			if a.HasRekorEntry() != tc.wantRekor {
				t.Errorf("HasRekorEntry() = %v, want %v", a.HasRekorEntry(), tc.wantRekor)
			}
			if a.Identity() != keyURI {
				t.Errorf("Identity = %q, want %q", a.Identity(), keyURI)
			}
		})
	}
}

// TestKMSAttesterAttest exercises Attest end-to-end offline by injecting a
// fake key-based identity (local ECDSA signer, no cert provider) and the
// no-tlog policy, so no KMS provider or Rekor call is made. It asserts the
// attester produces a public-key Sigstore bundle.
func TestKMSAttesterAttest(t *testing.T) {
	const uri = "awskms://test/key"
	a := &KMSAttester{
		keyURI:   uri,
		identity: newFakeKeyIdentity(t, uri),
		tlog:     NewNoTLogPolicy(),
	}

	out, err := a.Attest(context.Background(), AttestSubject{
		Name:   "checksums.txt",
		Digest: map[string]string{"sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	})
	if err != nil {
		t.Fatalf("Attest: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("Attest returned empty bundle")
	}

	var b protobundle.Bundle
	if err := protojson.Unmarshal(out, &b); err != nil {
		t.Fatalf("bundle is not valid protobuf-JSON: %v", err)
	}
	if b.GetVerificationMaterial().GetPublicKey() == nil {
		t.Error("want public-key verification material for KMS signing")
	}
	// The no-tlog policy must produce a bundle with no transparency-log entries:
	// assert directly on Attest's output (HasRekorEntry only reports the policy
	// choice, not the emitted bundle).
	if got := len(b.GetVerificationMaterial().GetTlogEntries()); got != 0 {
		t.Errorf("no-tlog KMS attestation carries %d tlog entries, want 0", got)
	}
}
