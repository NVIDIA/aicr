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

// TestTransparencyForOptions covers the policy-selection precedence in
// SignOptions: an explicit SigningConfig file, then the TUF-cached v2 config,
// then the single Rekor URL (v1). The TUF branch is exercised via its own
// error surface (it needs a warmed cache) rather than a live selection here.
func TestTransparencyForOptions(t *testing.T) {
	// want encodes the expected policy per case:
	//   "signingConfig" — a signingConfigPolicy (explicit file).
	//   "notRekor"      — the TUF branch, which may succeed or error offline but
	//                     must never fall through to a Rekor v1 policy.
	//   "rekor"         — a rekorPolicy; wantURL "" asserts a non-empty
	//                     public-good default, otherwise an exact URL match.
	// ignoreErr tolerates the TUF branch's cold-cache/offline error.
	tests := []struct {
		name      string
		opts      SignOptions
		want      string
		wantURL   string
		ignoreErr bool
		network   bool // drives a live TUF fetch; skipped in -short
	}{
		{
			name: "signing config file yields a signing-config policy",
			opts: SignOptions{SigningConfigPath: "testdata/signing_config_v2.json"},
			want: "signingConfig",
		},
		{
			name: "signing config file takes precedence over rekor URL",
			opts: SignOptions{SigningConfigPath: "testdata/signing_config_v2.json", RekorURL: "https://rekor.example.com"},
			want: "signingConfig",
		},
		{
			// Drives a real trust.ResolveSigningConfig (network) on a cold cache.
			// Offline this is best-effort: on fetch failure p is nil, which still
			// satisfies "not a rekorPolicy"; the deterministic file cases above
			// carry the load-bearing precedence assertions. Skipped in -short.
			name:      "TUF v2 config takes precedence over rekor URL",
			opts:      SignOptions{UseTUFSigningConfig: true, RekorURL: "https://rekor.example.com"},
			want:      "notRekor",
			ignoreErr: true,
			network:   true,
		},
		{
			name: "default is Rekor v1 public-good",
			opts: SignOptions{},
			want: "rekor",
		},
		{
			name:    "explicit rekor URL yields a v1 policy at that URL",
			opts:    SignOptions{RekorURL: "https://rekor.internal.example.com"},
			want:    "rekor",
			wantURL: "https://rekor.internal.example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.network && testing.Short() {
				t.Skip("skipping live TUF fetch in short mode")
			}
			p, err := transparencyForOptions(context.Background(), tt.opts)
			if err != nil && !tt.ignoreErr {
				t.Fatalf("unexpected error: %v", err)
			}
			switch tt.want {
			case "signingConfig":
				if _, ok := p.(signingConfigPolicy); !ok {
					t.Errorf("got %T, want signingConfigPolicy", p)
				}
			case "notRekor":
				if _, ok := p.(rekorPolicy); ok {
					t.Error("got rekorPolicy; UseTUFSigningConfig must take precedence over RekorURL")
				}
			case "rekor":
				rp, ok := p.(rekorPolicy)
				if !ok {
					t.Fatalf("got %T, want rekorPolicy", p)
				}
				if tt.wantURL == "" {
					if rp.url == "" {
						t.Error("default rekorPolicy should fall back to the public-good URL, got empty")
					}
				} else if rp.url != tt.wantURL {
					t.Errorf("rekor URL = %q, want %q", rp.url, tt.wantURL)
				}
			}
		})
	}
}

// TestSignOptionsFromResolve guards the single ResolveOptions->SignOptions
// mapping shared by the keyless attester and the evidence signing paths: every
// signing-target field must copy through. A dropped field here is how a signing
// path silently reverts to the wrong Rekor (the evidence-signs-v1 bug this
// helper was introduced to prevent). See #1650.
func TestSignOptionsFromResolve(t *testing.T) {
	ro := ResolveOptions{
		FulcioURL:           "https://fulcio.example.com",
		RekorURL:            "https://rekor.example.com",
		SigningConfigPath:   "sc.json",
		UseTUFSigningConfig: true,
	}
	so := SignOptionsFromResolve("the-token", ro)

	if so.OIDCToken != "the-token" {
		t.Errorf("OIDCToken = %q, want the-token", so.OIDCToken)
	}
	if so.FulcioURL != ro.FulcioURL {
		t.Errorf("FulcioURL = %q, want %q", so.FulcioURL, ro.FulcioURL)
	}
	if so.RekorURL != ro.RekorURL {
		t.Errorf("RekorURL = %q, want %q", so.RekorURL, ro.RekorURL)
	}
	if so.SigningConfigPath != ro.SigningConfigPath {
		t.Errorf("SigningConfigPath = %q, want %q", so.SigningConfigPath, ro.SigningConfigPath)
	}
	if so.UseTUFSigningConfig != ro.UseTUFSigningConfig {
		t.Errorf("UseTUFSigningConfig = %v, want %v", so.UseTUFSigningConfig, ro.UseTUFSigningConfig)
	}
}

// TestResolveAttester_KMS verifies KMS signing resolves to a KMSAttester across
// the v2 default and both opt-outs — KMS signs to Rekor v2 by default, and a
// signing config (file or TUF) is valid with KMS (no longer rejected). Covers
// both resolver entry points.
func TestResolveAttester_KMS(t *testing.T) {
	cases := []struct {
		name string
		opts ResolveOptions
	}{
		{"KMS default (v2)", ResolveOptions{Attest: true, SigningKey: "awskms://alias/k", UseTUFSigningConfig: true}},
		{"KMS + signing config file", ResolveOptions{Attest: true, SigningKey: "awskms://alias/k", SigningConfigPath: "x.json"}},
		{"KMS + Rekor v1 URL", ResolveOptions{Attest: true, SigningKey: "awskms://alias/k", RekorURL: "https://rekor.example.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ResolveAttester(context.Background(), tc.opts)
			if err != nil {
				t.Fatalf("ResolveAttester: unexpected error: %v", err)
			}
			ka, ok := a.(*KMSAttester)
			if !ok {
				t.Fatalf("ResolveAttester: got %T, want *KMSAttester", a)
			}
			// The KMS path must carry the same signing target the mapper produces,
			// so a new field added to SignOptionsFromResolve reaches Rekor selection
			// on the KMS path too (the drift the mapper exists to prevent).
			want := SignOptionsFromResolve("", tc.opts)
			if ka.target != want {
				t.Errorf("KMS target = %+v, want %+v", ka.target, want)
			}
			la, err := ResolveAttesterLazy(context.Background(), tc.opts)
			if err != nil {
				t.Fatalf("ResolveAttesterLazy: unexpected error: %v", err)
			}
			if _, ok := la.(*KMSAttester); !ok {
				t.Errorf("ResolveAttesterLazy: got %T, want *KMSAttester", la)
			}
		})
	}
}
