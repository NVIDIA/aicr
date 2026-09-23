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

package project

import (
	"encoding/hex"
	"regexp"
	"testing"

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// testIssuer and testIdentity are the verified (issuer, identity) fixture
// behind this file's golden hash values.
const (
	testIssuer   = "https://token.actions.githubusercontent.com"
	testIdentity = "https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml@refs/heads/main"
)

func TestSignerIDHash(t *testing.T) {
	hexRe := regexp.MustCompile(`^[0-9a-f]{32}$`)

	got := SignerIDHash(testIssuer, testIdentity)
	if !hexRe.MatchString(got) {
		t.Fatalf("idHash = %q, want 32 lowercase hex chars", got)
	}

	// Golden value: pins the persisted GP2-producer/GP4-consumer contract
	// against any change to idHashLen, the separator, or the encoding. If
	// this fails, the on-disk tree layout changed and both the bucket tree
	// and the consumer need a coordinated migration.
	const wantGolden = "a2f01812594e54d1a14278576fda2ed0"
	if got != wantGolden {
		t.Errorf("idHash = %q, want golden %q (algorithm changed?)", got, wantGolden)
	}

	// Determinism: same inputs -> same hash.
	if again := SignerIDHash(testIssuer, testIdentity); again != got {
		t.Errorf("idHash not deterministic: %q != %q", got, again)
	}

	// Different signer -> different hash (collision would be a bug).
	if other := SignerIDHash(testIssuer, testIdentity+"x"); other == got {
		t.Errorf("distinct identities collided on %q", got)
	}
	if other := SignerIDHash(testIssuer+"x", testIdentity); other == got {
		t.Errorf("distinct issuers collided on %q", got)
	}
}

// TestSignerIDHashSeparatorUnambiguous guards the field-boundary
// property: moving a character across the issuer/identity boundary must
// change the hash, or ("ab","c") and ("a","bc") would alias.
func TestSignerIDHashSeparatorUnambiguous(t *testing.T) {
	if SignerIDHash("ab", "c") == SignerIDHash("a", "bc") {
		t.Error("issuer/identity boundary is ambiguous — separator not effective")
	}
}

// TestSignerIDHashMatchesSharedDigest proves SignerIDHash's output equals
// the first idHashLen hex chars of attestation.HashIdentityPair's digest.
func TestSignerIDHashMatchesSharedDigest(t *testing.T) {
	sum, err := attestation.HashIdentityPair(testIssuer, testIdentity)
	if err != nil {
		t.Fatalf("HashIdentityPair: %v", err)
	}
	full := hex.EncodeToString(sum[:])
	if got := SignerIDHash(testIssuer, testIdentity); got != full[:idHashLen] {
		t.Errorf("SignerIDHash = %q, want prefix of shared digest %q", got, full)
	}
}
