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
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
)

// afterV2 is inside the v2 rekor validity window (validFor.start 2026-01-01) in
// testdata/signing_config_v2.json; beforeV2 is after v1 + TSA start but before
// v2, so the highest-version-wins selection falls back to v1.
var (
	afterV2  = time.Date(2026, 7, 8, 0, 0, 0, 0, time.UTC)
	beforeV2 = time.Date(2025, 8, 1, 0, 0, 0, 0, time.UTC)
)

func loadSigningConfig(t *testing.T, name string) *root.SigningConfig {
	t.Helper()
	sc, err := root.NewSigningConfigFromPath("testdata/" + name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return sc
}

func TestSelectSigningServices(t *testing.T) {
	tests := []struct {
		name         string
		config       string
		now          time.Time
		wantRekorURL string
		wantRekorAPI uint32
		wantTSAURL   string // "" means expect no TSA
		wantErr      bool
	}{
		{
			// The v2 config lists both v2 and v1 with selector ANY; at current
			// time SelectServices prefers the highest API version, so it picks v2.
			name:         "v2 config selects v2 at current time",
			config:       "signing_config_v2.json",
			now:          afterV2,
			wantRekorURL: "https://log2025-1.rekor.sigstore.dev",
			wantRekorAPI: 2,
			wantTSAURL:   "https://timestamp.sigstore.dev/api/v1/timestamp",
		},
		{
			// Before the v2 validity window, the same config falls back to v1.
			name:         "v2 config falls back to v1 before v2 window",
			config:       "signing_config_v2.json",
			now:          beforeV2,
			wantRekorURL: "https://rekor.sigstore.dev",
			wantRekorAPI: 1,
			wantTSAURL:   "https://timestamp.sigstore.dev/api/v1/timestamp",
		},
		{
			name:         "v1 config selects v1",
			config:       "signing_config_v1.json",
			now:          afterV2,
			wantRekorURL: "https://rekor.sigstore.dev",
			wantRekorAPI: 1,
			wantTSAURL:   "https://timestamp.sigstore.dev/api/v1/timestamp",
		},
		{
			name:         "v2-only no TSA selects v2 with empty TSA set",
			config:       "signing_config_v2_no_tsa.json",
			now:          afterV2,
			wantRekorURL: "https://log2025-1.rekor.sigstore.dev",
			wantRekorAPI: 2,
			wantTSAURL:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := loadSigningConfig(t, tt.config)
			rekor, tsa, err := selectSigningServices(sc, tt.now)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if len(rekor) != 1 {
				t.Fatalf("want 1 rekor service, got %d", len(rekor))
			}
			if rekor[0].URL != tt.wantRekorURL {
				t.Errorf("rekor URL = %q, want %q", rekor[0].URL, tt.wantRekorURL)
			}
			if rekor[0].MajorAPIVersion != tt.wantRekorAPI {
				t.Errorf("rekor API = %d, want %d", rekor[0].MajorAPIVersion, tt.wantRekorAPI)
			}
			if tt.wantTSAURL == "" {
				if len(tsa) != 0 {
					t.Errorf("want no TSA, got %d", len(tsa))
				}
				return
			}
			if len(tsa) != 1 {
				t.Fatalf("want 1 TSA, got %d", len(tsa))
			}
			if tsa[0].URL != tt.wantTSAURL {
				t.Errorf("TSA URL = %q, want %q", tsa[0].URL, tt.wantTSAURL)
			}
		})
	}
}

// TestSelectSigningServices_V1WithUnusableTSA verifies that a TSA which is
// outside its validity window does not fail a Rekor v1 selection: v1 carries its
// own signed entry timestamp and uses no TSA, so an unusable TSA is dropped, not
// fatal. The v2-requires-TSA rule stays with the caller.
func TestSelectSigningServices_V1WithUnusableTSA(t *testing.T) {
	sc := loadSigningConfig(t, "signing_config_v1.json")
	// v1 rekor is valid from 2021-01-12; the TSA only from 2025-07-04.
	beforeTSA := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	rekor, tsa, err := selectSigningServices(sc, beforeTSA)
	if err != nil {
		t.Fatalf("selectSigningServices should not fail on an unusable TSA for v1: %v", err)
	}
	if len(rekor) != 1 || rekor[0].MajorAPIVersion != 1 {
		t.Errorf("want a single v1 rekor service, got %+v", rekor)
	}
	if len(tsa) != 0 {
		t.Errorf("want no TSA selected, got %d", len(tsa))
	}
	// The policy builds successfully too — v1 needs no TSA. Exercise the
	// warn-on-v1-downgrade branch (TUF path): a v1 selection must still build a
	// working policy, the warning is a visibility side effect only.
	if _, err := newSigningConfigPolicy(sc, beforeTSA, true); err != nil {
		t.Errorf("newSigningConfigPolicy for v1 without a usable TSA: %v", err)
	}
}

func TestNewSigningConfigPolicy(t *testing.T) {
	tests := []struct {
		name     string
		config   string
		now      time.Time
		wantLogs int
		wantTSAs int
		wantErr  bool
	}{
		{
			name:     "v2 config yields one log and one TSA",
			config:   "signing_config_v2.json",
			now:      afterV2,
			wantLogs: 1,
			wantTSAs: 1,
		},
		{
			name:     "v1 config yields one log and one TSA",
			config:   "signing_config_v1.json",
			now:      afterV2,
			wantLogs: 1,
			wantTSAs: 1,
		},
		{
			// A v2 selection with no timestamp authority must fail closed:
			// v2 has no inline timestamp, so the bundle would lack trusted time.
			name:    "v2 without TSA fails closed",
			config:  "signing_config_v2_no_tsa.json",
			now:     afterV2,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := loadSigningConfig(t, tt.config)
			policy, err := newSigningConfigPolicy(sc, tt.now, false)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got := len(policy.Logs()); got != tt.wantLogs {
				t.Errorf("Logs() = %d, want %d", got, tt.wantLogs)
			}
			if got := len(policy.TimestampAuthorities()); got != tt.wantTSAs {
				t.Errorf("TimestampAuthorities() = %d, want %d", got, tt.wantTSAs)
			}
		})
	}
}

func TestNewSigningConfigPolicyNil(t *testing.T) {
	if _, err := newSigningConfigPolicy(nil, afterV2, false); err == nil {
		t.Fatal("want error for nil signing config, got nil")
	}
}

func TestNewSigningConfigPolicyFromPath(t *testing.T) {
	// The public loader uses time.Now(); the v2 window opened 2026-01-01, so at
	// test-run time it selects v2 + TSA and succeeds.
	policy, err := NewSigningConfigPolicyFromPath("testdata/signing_config_v2.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(policy.Logs()) != 1 || len(policy.TimestampAuthorities()) != 1 {
		t.Errorf("want 1 log + 1 TSA, got %d + %d", len(policy.Logs()), len(policy.TimestampAuthorities()))
	}

	if _, err := NewSigningConfigPolicyFromPath("testdata/does_not_exist.json"); err == nil {
		t.Error("want error for missing file, got nil")
	}
}
