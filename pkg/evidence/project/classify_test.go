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
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return p
}

const (
	ghaIssuer       = "https://token.actions.githubusercontent.com"
	communityIssuer = "https://github.com/login/oauth"
	partnerIssuer   = "https://oidc.coreweave-lab.example"

	communityIdentity = "contributor@example.com"
	partnerIdentity   = "https://oidc.coreweave-lab.example/attest"
)

// sampleAllowlist renders a fixture in the canonical GP1
// recipes/evidence/allowlist.yaml schema: first-party CI pinned by an
// anchored identityPattern, community/partner keyed by the one-way source
// slug of the signer's (issuer, identity).
func sampleAllowlist(t *testing.T) string {
	t.Helper()
	communitySlug, err := attestation.SourceSlug(communityIssuer, communityIdentity)
	if err != nil {
		t.Fatalf("SourceSlug(community): %v", err)
	}
	partnerSlug, err := attestation.SourceSlug(partnerIssuer, partnerIdentity)
	if err != nil {
		t.Fatalf("SourceSlug(partner): %v", err)
	}
	return fmt.Sprintf(`schemaVersion: "1.0.0"
firstParty:
  - label: aicr-uat-aws
    issuer: %s
    identityPattern: '^https://github\.com/NVIDIA/aicr/\.github/workflows/uat-aws\.yaml@refs/heads/.+$'
  - label: aicr-uat-gcp
    issuer: %s
    identityPattern: '^https://github\.com/NVIDIA/aicr/\.github/workflows/uat-gcp\.yaml@refs/heads/.+$'
community:
  - label: acme-contributor
    issuer: %s
    source: %s
partner:
  - label: coreweave-lab
    issuer: %s
    source: %s
`, ghaIssuer, ghaIssuer, communityIssuer, communitySlug, partnerIssuer, partnerSlug)
}

func TestLoadAllowlistAndClassify(t *testing.T) {
	dir := t.TempDir()
	al, err := LoadAllowlist(writeFile(t, dir, "allowlist.yaml", sampleAllowlist(t)))
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
			name:        "first-party identityPattern (aws ref)",
			issuer:      ghaIssuer,
			identity:    "https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml@refs/heads/main",
			wantClass:   ClassFirstParty,
			wantAllowed: true,
		},
		{
			name:        "first-party identityPattern (gcp, non-main ref)",
			issuer:      ghaIssuer,
			identity:    "https://github.com/NVIDIA/aicr/.github/workflows/uat-gcp.yaml@refs/heads/uat-fix",
			wantClass:   ClassFirstParty,
			wantAllowed: true,
		},
		{
			name:        "community source-slug match",
			issuer:      communityIssuer,
			identity:    communityIdentity,
			wantClass:   ClassCommunity,
			wantAllowed: true,
		},
		{
			name:        "partner source-slug match on its own issuer",
			issuer:      partnerIssuer,
			identity:    partnerIdentity,
			wantClass:   ClassPartner,
			wantAllowed: true,
		},
		{
			name:        "sibling repo → not first-party",
			issuer:      ghaIssuer,
			identity:    "https://github.com/NVIDIA/other-repo/.github/workflows/uat-aws.yaml@refs/heads/main",
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
			identity:    partnerIdentity,
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

// TestLoadAllowlist_CanonicalFile is the #1505 acceptance check: the GP2
// producer must parse the real GP1 allowlist (identityPattern/source schema).
func TestLoadAllowlist_CanonicalFile(t *testing.T) {
	al, err := LoadAllowlist(filepath.Join("..", "..", "..", "recipes", "evidence", "allowlist.yaml"))
	if err != nil {
		t.Fatalf("LoadAllowlist(recipes/evidence/allowlist.yaml): %v", err)
	}
	for _, cloud := range []string{"aws", "gcp", "azure"} {
		identity := "https://github.com/NVIDIA/aicr/.github/workflows/uat-" + cloud + ".yaml@refs/heads/main"
		class, ok := al.Classify(ghaIssuer, identity)
		if class != ClassFirstParty || !ok {
			t.Errorf("canonical allowlist classified %s UAT signer as (%q,%v), want (first-party,true)", cloud, class, ok)
		}
	}
}

// TestClassify_NilAllowlist covers the interim state before an allowlist
// file is passed: the built-in first-party heuristic admits AICR's own
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
		{"first-party heuristic (azure)", ghaIssuer, "https://github.com/NVIDIA/aicr/.github/workflows/uat-azure.yaml@refs/heads/main", ClassFirstParty, true},
		{"foreign repo is community", ghaIssuer, "https://github.com/evil/aicr/.github/workflows/x.yaml@refs/heads/main", ClassCommunity, false},
		{"look-alike host not first-party", ghaIssuer, "https://github.com.evil.example/NVIDIA/aicr/x.yaml", ClassCommunity, false},
		{"community email", communityIssuer, "yuanchen97@gmail.com", ClassCommunity, false},
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
		{"empty issuer", "schemaVersion: \"1.0.0\"\nfirstParty:\n  - identityPattern: '^x@y$'\n"},
		{"legacy cleartext identity field", "schemaVersion: \"1.0.0\"\ncommunity:\n  - issuer: a\n    identity: x\n"},
		{"unsupported schemaVersion", "schemaVersion: \"9.9.9\"\npartner: []\n"},
		{"over-broad wildcard left of @", "schemaVersion: \"1.0.0\"\nfirstParty:\n  - issuer: a\n    identityPattern: '^https://github\\.com/.+/attest\\.yaml@refs/heads/main$'\n"},
		{"unanchored pattern", "schemaVersion: \"1.0.0\"\nfirstParty:\n  - issuer: a\n    identityPattern: 'x@y'\n"},
		{"malformed source slug", "schemaVersion: \"1.0.0\"\ncommunity:\n  - issuer: a\n    source: NOT-A-SLUG\n"},
		{"duplicate source slug across classes", "schemaVersion: \"1.0.0\"\ncommunity:\n  - issuer: a\n    source: f1f1cf33e7d868f95ea0f5b7542e6662\npartner:\n  - issuer: a\n    source: f1f1cf33e7d868f95ea0f5b7542e6662\n"},
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

// TestAllowlist_AnchoredFullMatch guards the anti-sybil matcher: an anchored
// identityPattern must full-string match — a probe with extra characters
// inside the literal segment must not match.
func TestAllowlist_AnchoredFullMatch(t *testing.T) {
	dir := t.TempDir()
	al, err := LoadAllowlist(writeFile(t, dir, "al.yaml", sampleAllowlist(t)))
	if err != nil {
		t.Fatal(err)
	}
	// Extra path bytes between the workflow file and the '@' must NOT match.
	class, allowed := al.Classify(ghaIssuer,
		"https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml.evil@refs/heads/main")
	if allowed || class != ClassCommunity {
		t.Errorf("probe beyond anchored pattern matched: (%s, %v)", class, allowed)
	}
}
