// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"testing"
)

func TestNewKeylessAttester(t *testing.T) {
	attester := NewKeylessAttester("test-oidc-token")

	if attester == nil {
		t.Fatal("NewKeylessAttester() returned nil")
	}
}

func TestKeylessAttester_Identity(t *testing.T) {
	attester := NewKeylessAttester("test-oidc-token")

	// Identity is not known until after Attest() succeeds (Fulcio returns it).
	// Before signing, identity should be empty.
	if got := attester.Identity(); got != "" {
		t.Errorf("Identity() before Attest = %q, want empty string", got)
	}
}

func TestKeylessAttester_HasRekorEntry(t *testing.T) {
	attester := NewKeylessAttester("test-oidc-token")

	if !attester.HasRekorEntry() {
		t.Error("HasRekorEntry() = false, want true (keyless always uses Rekor)")
	}
}

func TestKeylessAttester_ImplementsAttester(t *testing.T) {
	var _ Attester = (*KeylessAttester)(nil)
}
