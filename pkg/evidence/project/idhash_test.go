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
	"regexp"
	"testing"
)

func TestSignerIDHash(t *testing.T) {
	const (
		issuer   = "https://token.actions.githubusercontent.com"
		identity = "https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml@refs/heads/main"
	)

	hexRe := regexp.MustCompile(`^[0-9a-f]{12}$`)

	got := SignerIDHash(issuer, identity)
	if !hexRe.MatchString(got) {
		t.Fatalf("idHash = %q, want 12 lowercase hex chars", got)
	}

	// Determinism: same inputs -> same hash.
	if again := SignerIDHash(issuer, identity); again != got {
		t.Errorf("idHash not deterministic: %q != %q", got, again)
	}

	// Different signer -> different hash (collision would be a bug).
	if other := SignerIDHash(issuer, identity+"x"); other == got {
		t.Errorf("distinct identities collided on %q", got)
	}
	if other := SignerIDHash(issuer+"x", identity); other == got {
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
