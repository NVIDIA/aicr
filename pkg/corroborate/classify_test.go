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

func TestClassifySigner(t *testing.T) {
	al := loadExampleAllowlist(t)
	tests := []struct {
		name      string
		issuer    string
		identity  string
		wantClass Class
		wantAllow bool
	}{
		{
			name:      "first-party from identityPattern (aws)",
			issuer:    ghIssuer,
			identity:  "https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml@refs/heads/main",
			wantClass: ClassFirstParty, wantAllow: true,
		},
		{
			name:      "first-party gcp variant on a non-main branch ref",
			issuer:    ghIssuer,
			identity:  "https://github.com/NVIDIA/aicr/.github/workflows/uat-gcp.yaml@refs/heads/uat-fix",
			wantClass: ClassFirstParty, wantAllow: true,
		},
		{
			name:      "community from source slug",
			issuer:    ghIssuer,
			identity:  "https://github.com/acme-gpu/aicr-attest/.github/workflows/attest.yaml@refs/heads/main",
			wantClass: ClassCommunity, wantAllow: true,
		},
		{
			name:      "partner from source slug",
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
			name:      "first-party pattern does not match a sibling repo",
			issuer:    ghIssuer,
			identity:  "https://github.com/NVIDIA/other-repo/.github/workflows/uat-aws.yaml@refs/heads/main",
			wantClass: ClassCommunity, wantAllow: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotClass, gotAllow := classifySigner(al, tt.issuer, tt.identity)
			if gotClass != tt.wantClass || gotAllow != tt.wantAllow {
				t.Errorf("classifySigner(%q,%q) = (%q,%v), want (%q,%v)",
					tt.issuer, tt.identity, gotClass, gotAllow, tt.wantClass, tt.wantAllow)
			}
		})
	}
}

// TestLoadAllowlist_CanonicalFile is the #1505 acceptance check: GP4's loader
// must parse the real GP1 allowlist (identityPattern/source schema).
func TestLoadAllowlist_CanonicalFile(t *testing.T) {
	al, err := LoadAllowlist(filepath.Join("..", "..", "recipes", "evidence", "allowlist.yaml"))
	if err != nil {
		t.Fatalf("LoadAllowlist(recipes/evidence/allowlist.yaml): %v", err)
	}
	for _, cloud := range []string{"aws", "gcp", "azure"} {
		identity := "https://github.com/NVIDIA/aicr/.github/workflows/uat-" + cloud + ".yaml@refs/heads/main"
		class, ok := classifySigner(al, ghIssuer, identity)
		if class != ClassFirstParty || !ok {
			t.Errorf("canonical allowlist classified %s UAT signer as (%q,%v), want (first-party,true)", cloud, class, ok)
		}
	}
}

func TestLoadAllowlist(t *testing.T) {
	writeAllowlist := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "allowlist.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{
			name: "valid canonical schema",
			body: "schemaVersion: \"1.0.0\"\nfirstParty:\n  - issuer: " + ghIssuer +
				"\n    identityPattern: '^https://github\\.com/NVIDIA/aicr/\\.github/workflows/uat-aws\\.yaml@refs/heads/.+$'\n",
		},
		{
			name:    "malformed yaml fails closed",
			body:    "firstParty: [::: not yaml",
			wantErr: true,
		},
		{
			name: "legacy cleartext identity field is rejected",
			body: "schemaVersion: \"1.0.0\"\ncommunity:\n  - issuer: " + ghIssuer +
				"\n    identity: https://github.com/acme/attest\n",
			wantErr: true,
		},
		{
			name: "unsupported schemaVersion is rejected at load",
			body: "schemaVersion: \"9.9.9\"\ncommunity:\n  - issuer: " + ghIssuer +
				"\n    source: f1f1cf33e7d868f95ea0f5b7542e6662\n",
			wantErr: true,
		},
		{
			name: "over-broad identityPattern (wildcard left of @) is rejected",
			body: "schemaVersion: \"1.0.0\"\nfirstParty:\n  - issuer: " + ghIssuer +
				"\n    identityPattern: '^https://github\\.com/.+/x\\.yaml@refs/heads/main$'\n",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadAllowlist(writeAllowlist(t, tt.body))
			if (err != nil) != tt.wantErr {
				t.Errorf("LoadAllowlist() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		if _, err := LoadAllowlist(filepath.Join("testdata", "nope.yaml")); err == nil {
			t.Fatal("expected error for missing file")
		}
	})
}
