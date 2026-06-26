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

package corroborate

import (
	"os"
	"path/filepath"
	"testing"
)

const ghIssuer = "https://token.actions.githubusercontent.com"

func loadExampleAllowlist(t *testing.T) *Allowlist {
	t.Helper()
	al, err := LoadAllowlist(filepath.Join("testdata", "allowlist.yaml"))
	if err != nil {
		t.Fatalf("LoadAllowlist: %v", err)
	}
	return al
}

func TestClassify(t *testing.T) {
	al := loadExampleAllowlist(t)
	tests := []struct {
		name      string
		issuer    string
		identity  string
		wantClass Class
		wantAllow bool
	}{
		{
			name:      "first-party from issuer pin (regex match)",
			issuer:    ghIssuer,
			identity:  "https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml@refs/heads/main",
			wantClass: ClassFirstParty, wantAllow: true,
		},
		{
			name:      "first-party gcp variant (alternation match)",
			issuer:    ghIssuer,
			identity:  "https://github.com/NVIDIA/aicr/.github/workflows/uat-gcp.yaml@refs/heads/main",
			wantClass: ClassFirstParty, wantAllow: true,
		},
		{
			name:      "community from allowlist (exact match)",
			issuer:    ghIssuer,
			identity:  "https://github.com/acme-gpu/aicr-attest/.github/workflows/attest.yaml@refs/heads/main",
			wantClass: ClassCommunity, wantAllow: true,
		},
		{
			name:      "partner from allowlist",
			issuer:    "https://oidc.coreweave-lab.example",
			identity:  "https://oidc.coreweave-lab.example/attest",
			wantClass: ClassPartner, wantAllow: true,
		},
		{
			name:      "verified-but-unknown is a reported community dot",
			issuer:    ghIssuer,
			identity:  "https://github.com/rogue-org/rogue-repo/.github/workflows/x.yaml@refs/heads/main",
			wantClass: ClassCommunity, wantAllow: false,
		},
		{
			name:      "right identity but wrong issuer does not match",
			issuer:    "https://evil.example",
			identity:  "https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml@refs/heads/main",
			wantClass: ClassCommunity, wantAllow: false,
		},
		{
			name:      "first-party regex does not match a sibling repo",
			issuer:    ghIssuer,
			identity:  "https://github.com/NVIDIA/other-repo/.github/workflows/uat-aws.yaml@refs/heads/main",
			wantClass: ClassCommunity, wantAllow: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotClass, gotAllow := al.Classify(tt.issuer, tt.identity)
			if gotClass != tt.wantClass || gotAllow != tt.wantAllow {
				t.Errorf("Classify(%q,%q) = (%q,%v), want (%q,%v)",
					tt.issuer, tt.identity, gotClass, gotAllow, tt.wantClass, tt.wantAllow)
			}
		})
	}
}

func TestAllowlistValidate(t *testing.T) {
	tests := []struct {
		name    string
		al      Allowlist
		wantErr bool
	}{
		{
			name: "valid disjoint allowlist",
			al: Allowlist{
				FirstParty: []AllowlistEntry{{Issuer: ghIssuer, Identity: `^https://github\.com/NVIDIA/aicr/\.github/workflows/uat-(aws|gcp)\.yaml@refs/heads/main$`}},
				Community:  []AllowlistEntry{{Issuer: ghIssuer, Identity: "https://github.com/acme/attest"}},
			},
		},
		{
			name: "over-broad wildcard org/repo is rejected",
			al: Allowlist{
				Community: []AllowlistEntry{{Issuer: ghIssuer, Identity: `^https://github\.com/.+/.+/\.github/workflows/attest\.yaml@refs/heads/main$`}},
			},
			wantErr: true,
		},
		{
			name: "over-broad .* is rejected",
			al: Allowlist{
				Community: []AllowlistEntry{{Issuer: ghIssuer, Identity: `^https://github\.com/acme/.*$`}},
			},
			wantErr: true,
		},
		{
			name: "over-broad segment class is rejected",
			al: Allowlist{
				Partner: []AllowlistEntry{{Issuer: ghIssuer, Identity: `^https://github\.com/[^/]+/attest$`}},
			},
			wantErr: true,
		},
		{
			name:    "empty issuer is rejected",
			al:      Allowlist{Community: []AllowlistEntry{{Identity: "https://github.com/acme/attest"}}},
			wantErr: true,
		},
		{
			name:    "empty identity is rejected",
			al:      Allowlist{Community: []AllowlistEntry{{Issuer: ghIssuer}}},
			wantErr: true,
		},
		{
			name: "uncompilable regex is rejected",
			al: Allowlist{
				Community: []AllowlistEntry{{Issuer: ghIssuer, Identity: "^https://github.com/acme/(attest"}},
			},
			wantErr: true,
		},
		{
			name: "duplicate exact identity across classes overlaps",
			al: Allowlist{
				Community: []AllowlistEntry{{Issuer: ghIssuer, Identity: "https://github.com/acme/attest"}},
				Partner:   []AllowlistEntry{{Issuer: ghIssuer, Identity: "https://github.com/acme/attest"}},
			},
			wantErr: true,
		},
		{
			name: "exact identity also covered by a foreign regex overlaps",
			al: Allowlist{
				FirstParty: []AllowlistEntry{{Issuer: ghIssuer, Identity: `^https://github\.com/NVIDIA/aicr/\.github/workflows/uat-aws\.yaml@refs/heads/main$`}},
				Community:  []AllowlistEntry{{Issuer: ghIssuer, Identity: "https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml@refs/heads/main"}},
			},
			wantErr: true,
		},
		{
			name: "same identity, different issuers do not overlap",
			al: Allowlist{
				Community: []AllowlistEntry{{Issuer: ghIssuer, Identity: "https://example.test/attest"}},
				Partner:   []AllowlistEntry{{Issuer: "https://other.example", Identity: "https://example.test/attest"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.al.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoadAllowlist(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		if _, err := LoadAllowlist(filepath.Join("testdata", "nope.yaml")); err == nil {
			t.Fatal("expected error for missing file")
		}
	})
	t.Run("malformed yaml", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "bad.yaml")
		if err := os.WriteFile(p, []byte("firstParty: [::: not yaml"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadAllowlist(p); err == nil {
			t.Fatal("expected parse error")
		}
	})
	t.Run("unsupported schemaVersion is rejected at load", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "future.yaml")
		body := "schemaVersion: \"9.9.9\"\ncommunity:\n  - issuer: " + ghIssuer + "\n    identity: https://github.com/acme/attest\n"
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadAllowlist(p); err == nil {
			t.Fatal("expected unsupported-schemaVersion rejection at load")
		}
	})
	t.Run("over-broad file is rejected at load", func(t *testing.T) {
		dir := t.TempDir()
		p := filepath.Join(dir, "broad.yaml")
		body := "schemaVersion: \"1.0.0\"\ncommunity:\n  - issuer: " + ghIssuer + "\n    identity: '^https://github\\.com/.+/.+/x$'\n"
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadAllowlist(p); err == nil {
			t.Fatal("expected over-broad rejection at load")
		}
	})
}

func TestOverBroadIdentity(t *testing.T) {
	tests := []struct {
		pattern string
		broad   bool
	}{
		{"https://github.com/acme/attest", false},                                                           // exact
		{`^https://github\.com/NVIDIA/aicr/\.github/workflows/uat-(aws|gcp)\.yaml@refs/heads/main$`, false}, // bounded regex
		{`^https://github\.com/acme/repo\.yaml{0,3}$`, false},                                               // bounded {n,m}
		{`^https://github\.com/acme/repos?$`, false},                                                        // ? is bounded
		{`^https://github\.com/.+/.+/x$`, true},
		{`^https://github\.com/acme/.*$`, true},
		{`^https://github\.com/[^/]+/x$`, true},     // char-class +
		{`^https://github\.com/[^/]*/x$`, true},     // char-class *
		{`^https://github\.com/\w+/x$`, true},       // perl-class + (missed by a substring scan)
		{`^https://github\.com/a{2,}/x$`, true},     // open-ended {n,}
		{`^https://github\.com/(acme|.+)/x$`, true}, // wildcard alternation branch (nested)
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			if _, got := overBroadIdentity(tt.pattern); got != tt.broad {
				t.Errorf("overBroadIdentity(%q) = %v, want %v", tt.pattern, got, tt.broad)
			}
		})
	}
}
