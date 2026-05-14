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

package verifier

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

func i64ptr(v int64) *int64 { return &v }

func TestCrossCheckPointerSigner(t *testing.T) {
	tests := []struct {
		name      string
		claimed   *attestation.PointerSigner
		actual    *SignerClaims
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "no claim, no actual",
			claimed: nil,
			actual:  nil,
		},
		{
			name:    "no claim, signed bundle (claim-agnostic pointer)",
			claimed: nil,
			actual:  &SignerClaims{Identity: "x", Issuer: "y"},
		},
		{
			name:      "claim but no actual (pointer says signed, bundle is unsigned)",
			claimed:   &attestation.PointerSigner{Identity: "x", Issuer: "y"},
			actual:    nil,
			wantErr:   true,
			errSubstr: "no signature",
		},
		{
			name:    "identity matches",
			claimed: &attestation.PointerSigner{Identity: "alice@x", Issuer: "https://ghap"},
			actual:  &SignerClaims{Identity: "alice@x", Issuer: "https://ghap"},
		},
		{
			name:      "identity mismatch",
			claimed:   &attestation.PointerSigner{Identity: "alice@x", Issuer: "https://ghap"},
			actual:    &SignerClaims{Identity: "mallory@x", Issuer: "https://ghap"},
			wantErr:   true,
			errSubstr: "identity mismatch",
		},
		{
			name:      "issuer mismatch",
			claimed:   &attestation.PointerSigner{Identity: "alice@x", Issuer: "https://good"},
			actual:    &SignerClaims{Identity: "alice@x", Issuer: "https://evil"},
			wantErr:   true,
			errSubstr: "issuer mismatch",
		},
		{
			name:    "rekor index matches",
			claimed: &attestation.PointerSigner{Identity: "x", Issuer: "y", RekorLogIndex: i64ptr(42)},
			actual:  &SignerClaims{Identity: "x", Issuer: "y", RekorLogIndex: i64ptr(42)},
		},
		{
			name:      "rekor index mismatch",
			claimed:   &attestation.PointerSigner{Identity: "x", Issuer: "y", RekorLogIndex: i64ptr(42)},
			actual:    &SignerClaims{Identity: "x", Issuer: "y", RekorLogIndex: i64ptr(99)},
			wantErr:   true,
			errSubstr: "Rekor log index mismatch",
		},
		{
			name:      "pointer claims rekor, actual has none",
			claimed:   &attestation.PointerSigner{Identity: "x", Issuer: "y", RekorLogIndex: i64ptr(42)},
			actual:    &SignerClaims{Identity: "x", Issuer: "y"},
			wantErr:   true,
			errSubstr: "no Rekor entry",
		},
		{
			name:    "pointer issuer empty, no comparison",
			claimed: &attestation.PointerSigner{Identity: "x"},
			actual:  &SignerClaims{Identity: "x", Issuer: "anything"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CrossCheckPointerSigner(tt.claimed, tt.actual)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.errSubstr != "" && !strings.Contains(err.Error(), tt.errSubstr) {
				t.Errorf("error %q should contain %q", err.Error(), tt.errSubstr)
			}
		})
	}
}

func TestIsDigestPinned(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"sha256:abc123", true},
		{"sha256:" + strings.Repeat("a", 64), true},
		{"v1", false},
		{"latest", false},
		{"", false},
		{"sha512:abc", false}, // we only pin on sha256 today
	}
	for _, tt := range tests {
		if got := isDigestPinned(tt.in); got != tt.want {
			t.Errorf("isDigestPinned(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestMaterializeBundle_RejectsTagOnlyOCI(t *testing.T) {
	// Direct OCI input with a tag-only ref should fail at the pin
	// check before any network call. parseOCIReference accepts both
	// forms; the digest-pin enforcement is in materializeOCIRefRequireDigest.
	form, err := DetectInputForm("ghcr.io/owner/repo:v1")
	if err != nil {
		t.Fatalf("DetectInputForm: %v", err)
	}
	_, err = MaterializeBundle(t.Context(),
		VerifyOptions{Input: "ghcr.io/owner/repo:v1"}, form, nil)
	if err == nil {
		t.Fatalf("expected error for tag-only OCI reference")
	}
	if !strings.Contains(err.Error(), "tag-only") {
		t.Errorf("error should mention tag-only; got %v", err)
	}
}

func TestMaterializeBundle_TagOnlyWithAllowFlag(t *testing.T) {
	// With AllowUnpinnedTag, the pin check is bypassed and execution
	// proceeds into oras.Copy. Use 127.0.0.1:1 (a port nothing should
	// be listening on) so connection fails fast — never reaching the
	// real internet — and a 250ms per-test context so flaky DNS or a
	// slow stack can't pay the 2-minute pull timeout. The assertion
	// is only that the pin-error message is NOT in the result.
	const unreachable = "127.0.0.1:1/repo:v1"
	form, err := DetectInputForm(unreachable)
	if err != nil {
		t.Fatalf("DetectInputForm: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	_, err = MaterializeBundle(ctx,
		VerifyOptions{Input: unreachable, AllowUnpinnedTag: true},
		form, nil)
	if err == nil {
		t.Fatalf("expected pull error (unreachable registry), got nil")
	}
	if strings.Contains(err.Error(), "tag-only") {
		t.Errorf("AllowUnpinnedTag should bypass tag-pin check; got %v", err)
	}
}
