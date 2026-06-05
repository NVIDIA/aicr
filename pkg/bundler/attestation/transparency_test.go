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
	"testing"

	"github.com/NVIDIA/aicr/pkg/defaults"
)

func TestRekorPolicyLogs(t *testing.T) {
	// sign.NewRekor does not expose its BaseURL, so the configured endpoint
	// cannot be read back from Logs(); assert the empty-URL fallback on the
	// concrete rekorPolicy.url instead (white-box), and confirm a Rekor client
	// is attached in both cases.
	tests := []struct {
		name    string
		url     string
		wantURL string
	}{
		{"explicit url", "https://rekor.example.com", "https://rekor.example.com"},
		{"empty falls back to default", "", defaults.SigstoreRekorURL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewRekorPolicy(tt.url)
			rp, ok := p.(rekorPolicy)
			if !ok {
				t.Fatalf("NewRekorPolicy returned %T, want rekorPolicy", p)
			}
			if rp.url != tt.wantURL {
				t.Errorf("rekorPolicy.url = %q, want %q", rp.url, tt.wantURL)
			}
			if got := len(p.Logs()); got != 1 {
				t.Errorf("Logs() len = %d, want 1", got)
			}
		})
	}
}

func TestNoTLogPolicyLogs(t *testing.T) {
	if got := NewNoTLogPolicy().Logs(); got != nil {
		t.Errorf("Logs() = %v, want nil", got)
	}
}
