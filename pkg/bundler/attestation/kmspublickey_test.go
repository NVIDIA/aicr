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
	stderrors "errors"
	"testing"

	"github.com/NVIDIA/aicr/pkg/errors"
)

func TestResolveKMSPublicKey_UnknownScheme(t *testing.T) {
	_, _, err := resolveKMSPublicKey(context.Background(), "bogus://nope")
	if err == nil {
		t.Fatal("want error for unknown scheme")
	}
	if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
		t.Errorf("want ErrCodeInvalidRequest, got %v", err)
	}
}

// TestResolveKMSPublicKey_HashivaultProviderRegistered pins the blank import of
// the hashivault KMS provider (kmsidentity.go). A hashivault:// URI must route
// to a registered provider rather than be rejected as an unknown scheme. With
// VAULT_ADDR/BAO_ADDR unset the provider fails to initialize before any network
// call, which resolveKMSPublicKey classifies as ErrCodeUnavailable — distinct
// from the ErrCodeInvalidRequest returned for an unregistered scheme (see
// TestResolveKMSPublicKey_UnknownScheme). If the blank import were dropped,
// kms.Get would surface ProviderNotFoundError and the code would flip to
// ErrCodeInvalidRequest, failing this test.
func TestResolveKMSPublicKey_HashivaultProviderRegistered(t *testing.T) {
	t.Setenv("VAULT_ADDR", "")
	t.Setenv("BAO_ADDR", "")

	_, _, err := resolveKMSPublicKey(context.Background(), "hashivault://transit/keys/k")
	if err == nil {
		t.Fatal("want error resolving hashivault:// with VAULT_ADDR unset")
	}
	// A registered provider that cannot initialize (here: no VAULT_ADDR/BAO_ADDR)
	// is classified ErrCodeUnavailable — distinct from the ErrCodeInvalidRequest
	// reserved for an unregistered/unknown scheme. Asserting the specific code
	// both pins the blank import (an unregistered scheme would surface
	// ErrCodeInvalidRequest instead) and confirms the provider-init failure path.
	if !stderrors.Is(err, errors.New(errors.ErrCodeUnavailable, "")) {
		t.Errorf("want ErrCodeUnavailable (provider registered, init failed), got %v", err)
	}
}

func TestIsKMSURI(t *testing.T) {
	tests := []struct {
		name string
		ref  string
		want bool
	}{
		{"aws lowercase", "awskms://alias/key", true},
		{"gcp lowercase", "gcpkms://projects/p/locations/l/keyRings/r/cryptoKeys/k", true},
		{"azure lowercase", "azurekms://vault.vault.azure.net/keys/k", true},
		{"hashivault lowercase", "hashivault://transit/keys/k", true},
		{"aws uppercase scheme", "AWSKMS://alias/key", true},
		{"gcp uppercase scheme", "GCPKMS://projects/p/locations/l/keyRings/r/cryptoKeys/k", true},
		{"azure mixed case scheme", "AzureKMS://vault.vault.azure.net/keys/k", true},
		{"hashivault mixed case scheme", "HashiVault://transit/keys/k", true},
		{"local pem path", "./bundle-signer.pub", false},
		{"absolute pem path", "/etc/keys/signer.pem", false},
		{"scheme as substring not prefix", "prefix-awskms://alias/key", false},
		{"scheme without :// separator", "awskms:alias/key", false},
		{"scheme name only", "gcpkms", false},
		{"unsupported scheme", "fookms://x/y", false},
		{"unsupported scheme uppercase", "FOOKMS://x/y", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isKMSURI(tt.ref); got != tt.want {
				t.Errorf("isKMSURI(%q) = %v, want %v", tt.ref, got, tt.want)
			}
		})
	}
}

func TestNormalizeURIScheme(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"uppercase scheme", "GCPKMS://projects/P/keyRings/R", "gcpkms://projects/P/keyRings/R"},
		{"mixed case scheme", "AzureKMS://Vault/keys/K", "azurekms://Vault/keys/K"},
		{"hashivault mixed case scheme", "HashiVault://Transit/keys/K", "hashivault://Transit/keys/K"},
		{"already lowercase", "awskms://alias/Key", "awskms://alias/Key"},
		{"preserves path case", "awskms://arn:aws:kms:us-East-1:ABC", "awskms://arn:aws:kms:us-East-1:ABC"},
		{"no scheme separator", "/etc/keys/signer.pem", "/etc/keys/signer.pem"},
		{"leading separator unchanged", "://x", "://x"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeURIScheme(tt.in); got != tt.want {
				t.Errorf("normalizeURIScheme(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestKMSTimeoutError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode bool // true => expect ErrCodeTimeout, false => expect nil
	}{
		{"deadline exceeded", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, true},
		{"wrapped deadline", errors.Wrap(errors.ErrCodeInternal, "rpc", context.DeadlineExceeded), true},
		{"generic error", stderrors.New("kms down"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := kmsTimeoutError(tt.err, "awskms://alias/key")
			if tt.wantCode {
				if got == nil {
					t.Fatal("want ErrCodeTimeout, got nil")
				}
				if !stderrors.Is(got, errors.New(errors.ErrCodeTimeout, "")) {
					t.Errorf("want ErrCodeTimeout, got %v", got)
				}
			} else if got != nil {
				t.Errorf("want nil, got %v", got)
			}
		})
	}
}
