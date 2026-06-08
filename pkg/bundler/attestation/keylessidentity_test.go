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

func TestKeylessIdentity(t *testing.T) {
	id := NewKeylessIdentity("tok", "")
	cp, opts := id.CertProvider()
	if cp == nil {
		t.Fatal("CertProvider() = nil, want Fulcio provider")
	}
	if opts == nil || opts.IDToken != "tok" {
		t.Errorf("CertProviderOptions IDToken = %v, want tok", opts)
	}
	if id.FallbackIdentity() != "" {
		t.Errorf("FallbackIdentity() = %q, want empty", id.FallbackIdentity())
	}
	kp, err := id.Keypair(context.Background())
	if err != nil {
		t.Fatalf("Keypair() error = %v", err)
	}
	if kp == nil {
		t.Fatal("Keypair() = nil")
	}
}
