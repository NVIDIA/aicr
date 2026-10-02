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
	"encoding/hex"
	"testing"
)

// testIssuer and testIdentity are the verified (issuer, identity) fixture
// behind this file's golden hash values.
const (
	testIssuer   = "https://token.actions.githubusercontent.com"
	testIdentity = "https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml@refs/heads/main"
)

func TestSourceSlug(t *testing.T) {
	tests := []struct {
		name     string
		issuer   string
		identity string
		want     string
		wantErr  bool
	}{
		{
			// Frozen vector for the committed community pointer so the slug
			// scheme cannot drift without this test failing.
			name:     "community github oauth identity",
			issuer:   "https://github.com/login/oauth",
			identity: "yuanchen97@gmail.com",
			want:     "7c4c0edc8c765a95a0f3afdb3bbb8e91",
		},
		{
			name:     "first-party github actions oidc identity",
			issuer:   testIssuer,
			identity: testIdentity,
			want:     "a2f01812594e54d1a14278576fda2ed0",
		},
		{name: "empty issuer", issuer: "", identity: "x", wantErr: true},
		{name: "empty identity", issuer: "x", identity: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SourceSlug(tt.issuer, tt.identity)
			if (err != nil) != tt.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			if len(got) != SourceSlugLength {
				t.Errorf("slug length = %d, want %d", len(got), SourceSlugLength)
			}
		})
	}
}

// TestSourceSlug_DomainSeparation proves the newline separator prevents
// (issuer, identity) collisions where the concatenation would otherwise be
// ambiguous.
func TestSourceSlug_DomainSeparation(t *testing.T) {
	a, err := SourceSlug("ab", "c")
	if err != nil {
		t.Fatal(err)
	}
	b, err := SourceSlug("a", "bc")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("expected distinct slugs for ambiguous concatenation, both = %q", a)
	}
}

func TestHashIdentityPair(t *testing.T) {
	// This is the full, untruncated digest behind SourceSlug's own 32-char
	// prefix. Any change here breaks every persisted evidence path and
	// dedup key derived from it.
	const wantGolden = "a2f01812594e54d1a14278576fda2ed0a71b32b8b2287862271baf100c561b16"

	sum, err := HashIdentityPair(testIssuer, testIdentity)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := hex.EncodeToString(sum[:])
	if got != wantGolden {
		t.Errorf("digest = %q, want golden %q (algorithm changed?)", got, wantGolden)
	}

	// SourceSlug is exactly this digest's first SourceSlugLength hex chars.
	slug, err := SourceSlug(testIssuer, testIdentity)
	if err != nil {
		t.Fatalf("SourceSlug: %v", err)
	}
	if slug != got[:SourceSlugLength] {
		t.Errorf("SourceSlug = %q, want prefix of full digest %q", slug, got)
	}

	if _, err := HashIdentityPair("", testIdentity); err == nil {
		t.Error("expected error for empty issuer")
	}
	if _, err := HashIdentityPair(testIssuer, ""); err == nil {
		t.Error("expected error for empty identity")
	}
}
