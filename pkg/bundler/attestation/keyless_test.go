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
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
)

func TestNewKeylessAttester(t *testing.T) {
	attester := NewKeylessAttester("test-oidc-token", "", "", "", false)

	if attester == nil {
		t.Fatal("NewKeylessAttester() returned nil")
	}
}

// TestNewKeylessAttester_SigstoreURLs covers the private-Sigstore override:
// non-empty Fulcio/Rekor URLs are honored verbatim, and empty strings fall
// back to the public-good defaults so existing callers keep their behavior.
// See issue #408.
func TestNewKeylessAttester_SigstoreURLs(t *testing.T) {
	const (
		privFulcio = "https://fulcio.internal.example.com"
		privRekor  = "https://rekor.internal.example.com"
	)
	tests := []struct {
		name       string
		fulcio     string
		rekor      string
		wantFulcio string
		wantRekor  string
	}{
		{"both empty fall back to public defaults", "", "", defaults.SigstoreFulcioURL, defaults.SigstoreRekorURL},
		{"both overridden are honored", privFulcio, privRekor, privFulcio, privRekor},
		{"private Fulcio with public Rekor", privFulcio, "", privFulcio, defaults.SigstoreRekorURL},
		{"public Fulcio with private Rekor", "", privRekor, defaults.SigstoreFulcioURL, privRekor},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewKeylessAttester("test-oidc-token", tt.fulcio, tt.rekor, "", false)
			if a.fulcioURL != tt.wantFulcio {
				t.Errorf("fulcioURL = %q, want %q", a.fulcioURL, tt.wantFulcio)
			}
			if a.rekorURL != tt.wantRekor {
				t.Errorf("rekorURL = %q, want %q", a.rekorURL, tt.wantRekor)
			}
		})
	}
}

func TestKeylessAttester_Identity(t *testing.T) {
	attester := NewKeylessAttester("test-oidc-token", "", "", "", false)

	// Identity is not known until after Attest() succeeds (Fulcio returns it).
	// Before signing, identity should be empty.
	if got := attester.Identity(); got != "" {
		t.Errorf("Identity() before Attest = %q, want empty string", got)
	}
}

// fakeKeylessSign returns a stand-in for SignStatement that skips signing.
// It passes signer to build, stores the statement in got, and returns signer
// as the attestation's identity.
func fakeKeylessSign(signer string, got *[]byte) func(context.Context, func(string) ([]byte, error), SignOptions) (*SignedAttestation, error) {
	return func(_ context.Context, build func(string) ([]byte, error), _ SignOptions) (*SignedAttestation, error) {
		statementJSON, err := build(signer)
		if err != nil {
			return nil, err
		}
		*got = statementJSON
		return &SignedAttestation{BundleJSON: []byte("{}"), Identity: signer}, nil
	}
}

var keylessTestSubject = AttestSubject{
	Name:   "checksums.txt",
	Digest: map[string]string{"sha256": strings.Repeat("ab", 32)},
}

func TestKeylessAttester_BuilderIDIsSigner(t *testing.T) {
	const signer = "jdoe@company.com"
	var statementJSON []byte
	attester := NewKeylessAttester("test-oidc-token", "", "", "", false)
	attester.signStatement = fakeKeylessSign(signer, &statementJSON)

	if _, err := attester.Attest(t.Context(), keylessTestSubject); err != nil {
		t.Fatalf("Attest() error = %v", err)
	}

	if got := decodeStatement(t, statementJSON).Predicate.RunDetails.Builder.ID; got != signer {
		t.Errorf("builder.id = %q, want %q", got, signer)
	}
}

func TestKeylessAttester_HasRekorEntry(t *testing.T) {
	attester := NewKeylessAttester("test-oidc-token", "", "", "", false)

	if !attester.HasRekorEntry() {
		t.Error("HasRekorEntry() = false, want true (keyless always uses Rekor)")
	}
}

func TestKeylessAttester_ImplementsAttester(t *testing.T) {
	var _ Attester = (*KeylessAttester)(nil)
}
