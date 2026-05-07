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
)

func TestNoOpAttester_Attest(t *testing.T) {
	attester := NewNoOpAttester()

	result, err := attester.Attest(context.Background(), AttestSubject{
		Name:   "checksums.txt",
		Digest: map[string]string{"sha256": "abc123"},
	})

	if err != nil {
		t.Fatalf("Attest() returned error: %v", err)
	}
	if result != nil {
		t.Errorf("Attest() = %v, want nil", result)
	}
}

func TestNoOpAttester_Identity(t *testing.T) {
	attester := NewNoOpAttester()

	identity := attester.Identity()
	if identity != "" {
		t.Errorf("Identity() = %q, want empty string", identity)
	}
}

func TestNoOpAttester_HasRekorEntry(t *testing.T) {
	attester := NewNoOpAttester()

	if attester.HasRekorEntry() {
		t.Error("HasRekorEntry() = true, want false")
	}
}

func TestNoOpAttester_ImplementsAttester(t *testing.T) {
	var _ Attester = (*NoOpAttester)(nil)
}
