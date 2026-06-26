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
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

const ghaIssuer = "https://token.actions.githubusercontent.com"

// sampleAllowlist mirrors the GP1 recipes/evidence/allowlist.yaml schema.
const sampleAllowlist = `schemaVersion: "1.0.0"
firstParty:
  - issuer: https://token.actions.githubusercontent.com
    identity: '^https://github\.com/NVIDIA/aicr/\.github/workflows/uat-(aws|gcp)\.yaml@refs/heads/main$'
community:
  - issuer: https://token.actions.githubusercontent.com
    identity: https://github.com/acme-gpu/aicr-attest/.github/workflows/attest.yaml@refs/heads/main
partner:
  - issuer: https://oidc.coreweave-lab.example
    identity: https://oidc.coreweave-lab.example/attest
`

func TestLoadAllowlistAndClassify(t *testing.T) {
	dir := t.TempDir()
	al, err := LoadAllowlist(writeFile(t, dir, "allowlist.yaml", sampleAllowlist))
	if err != nil {
		t.Fatalf("LoadAllowlist: %v", err)
	}

	tests := []struct {
		name        string
		issuer      string
		identity    string
		wantClass   Class
		wantAllowed bool
	}{
		{
			name:        "first-party regex (aws ref)",
			issuer:      ghaIssuer,
			identity:    "https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml@refs/heads/main",
			wantClass:   ClassFirstParty,
			wantAllowed: true,
		},
		{
			name:        "first-party regex (gcp ref)",
			issuer:      ghaIssuer,
			identity:    "https://github.com/NVIDIA/aicr/.github/workflows/uat-gcp.yaml@refs/heads/main",
			wantClass:   ClassFirstParty,
			wantAllowed: true,
		},
		{
			name:        "exact community match",
			issuer:      ghaIssuer,
			identity:    "https://github.com/acme-gpu/aicr-attest/.github/workflows/attest.yaml@refs/heads/main",
			wantClass:   ClassCommunity,
			wantAllowed: true,
		},
		{
			name:        "partner on its own issuer",
			issuer:      "https://oidc.coreweave-lab.example",
			identity:    "https://oidc.coreweave-lab.example/attest",
			wantClass:   ClassPartner,
			wantAllowed: true,
		},
		{
			name:        "right repo, wrong ref → not first-party",
			issuer:      ghaIssuer,
			identity:    "https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml@refs/heads/dev",
			wantClass:   ClassCommunity,
			wantAllowed: false,
		},
		{
			name:        "unlisted signer falls through to community",
			issuer:      ghaIssuer,
			identity:    "stranger@example.com",
			wantClass:   ClassCommunity,
			wantAllowed: false,
		},
		{
			name:        "right identity, wrong issuer",
			issuer:      "https://evil.example",
			identity:    "https://oidc.coreweave-lab.example/attest",
			wantClass:   ClassCommunity,
			wantAllowed: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			class, allowed := al.Classify(tt.issuer, tt.identity)
			if class != tt.wantClass || allowed != tt.wantAllowed {
				t.Errorf("Classify() = (%s, %v), want (%s, %v)", class, allowed, tt.wantClass, tt.wantAllowed)
			}
		})
	}
}

// TestClassify_NilAllowlist covers the interim state before GP1 ships the
// allowlist file: the built-in first-party heuristic admits AICR's own
// UAT identity, everything else fails closed to community.
func TestClassify_NilAllowlist(t *testing.T) {
	var al *Allowlist // nil
	tests := []struct {
		name        string
		issuer      string
		identity    string
		wantClass   Class
		wantAllowed bool
	}{
		{"first-party heuristic", ghaIssuer, "https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml@refs/heads/main", ClassFirstParty, true},
		{"foreign repo is community", ghaIssuer, "https://github.com/evil/aicr/.github/workflows/x.yaml@refs/heads/main", ClassCommunity, false},
		{"look-alike host not first-party", ghaIssuer, "https://github.com.evil.example/NVIDIA/aicr/x.yaml", ClassCommunity, false},
		{"community email", "https://github.com/login/oauth", "yuanchen97@gmail.com", ClassCommunity, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			class, allowed := al.Classify(tt.issuer, tt.identity)
			if class != tt.wantClass || allowed != tt.wantAllowed {
				t.Errorf("Classify() = (%s, %v), want (%s, %v)", class, allowed, tt.wantClass, tt.wantAllowed)
			}
		})
	}
}

func TestLoadAllowlist_Errors(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name    string
		content string
	}{
		{"empty issuer", "firstParty:\n  - issuer: \"\"\n    identity: x\n"},
		{"empty identity", "community:\n  - issuer: a\n    identity: \"\"\n"},
		{"over-broad unbounded regex", "partner:\n  - issuer: a\n    identity: '^https://github\\.com/.+/attest$'\n"},
		{"unsampleable char class", "partner:\n  - issuer: a\n    identity: '^https://github\\.com/acme[0-9]/attest$'\n"},
		{"unsampleable optional", "partner:\n  - issuer: a\n    identity: '^https://github\\.com/acmes?/attest$'\n"},
		{"bad regexp", "partner:\n  - issuer: a\n    identity: \"^(\"\n"},
		{"overlapping classes", "firstParty:\n  - issuer: a\n    identity: x\ncommunity:\n  - issuer: a\n    identity: x\n"},
		{"not yaml", "::: not yaml :::\n\t- x\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := writeFile(t, dir, "al-"+tt.name+".yaml", tt.content)
			if _, err := LoadAllowlist(p); err == nil {
				t.Errorf("LoadAllowlist(%q) = nil error, want failure", tt.name)
			}
		})
	}

	if _, err := LoadAllowlist(filepath.Join(dir, "does-not-exist.yaml")); err == nil {
		t.Error("LoadAllowlist(missing file) = nil error, want failure")
	}
}

// TestAllowlist_BoundedAndAnchored guards the anti-sybil matcher: a
// tightly-bounded regex must full-string match, not substring match.
func TestAllowlist_AnchoredFullMatch(t *testing.T) {
	dir := t.TempDir()
	al, err := LoadAllowlist(writeFile(t, dir, "al.yaml", sampleAllowlist))
	if err != nil {
		t.Fatal(err)
	}
	// A trailing suffix beyond the anchored pattern must NOT match.
	class, allowed := al.Classify(ghaIssuer,
		"https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml@refs/heads/main/extra")
	if allowed || class != ClassCommunity {
		t.Errorf("suffix beyond anchored regex matched: (%s, %v)", class, allowed)
	}
}
