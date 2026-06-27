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

package allowlist

import (
	"os"
	"path/filepath"
	"testing"
)

// repoAllowlistPath is the committed allowlist, four levels up from this
// package (pkg/evidence/allowlist -> repo root).
const repoAllowlistPath = "../../../recipes/evidence/allowlist.yaml"

// ghActions is the GitHub Actions OIDC issuer used by first-party patterns.
const ghActions = "https://token.actions.githubusercontent.com"

// TestLoad_CommittedAllowlist asserts the real recipes/evidence/allowlist.yaml
// loads and passes every Validate rule. This is the gate that keeps a
// hand-edited allowlist honest.
func TestLoad_CommittedAllowlist(t *testing.T) {
	al, err := Load(repoAllowlistPath)
	if err != nil {
		t.Fatalf("committed allowlist failed to load/validate: %v", err)
	}
	if len(al.FirstParty) == 0 {
		t.Error("committed allowlist has no first-party entries")
	}
	for _, e := range al.Community {
		if e.Source == "" {
			t.Errorf("community entry %q has no source slug", e.id())
		}
	}
}

// TestLoad_RejectsCleartextIdentity proves the privacy invariant: a stray
// `identity:` field (cleartext PII) is rejected by the strict decoder rather
// than silently ignored.
func TestLoad_RejectsCleartextIdentity(t *testing.T) {
	body := `schemaVersion: 1.0.0
community:
  - issuer: https://github.com/login/oauth
    identity: someone@example.com
    source: 7c4c0edc8c765a95a0f3afdb3bbb8e91
`
	p := filepath.Join(t.TempDir(), "allowlist.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil {
		t.Error("expected Load to reject a cleartext identity field")
	}
}

func TestClassify(t *testing.T) {
	al, err := Load(repoAllowlistPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	tests := []struct {
		name      string
		issuer    string
		identity  string
		wantClass Class
		wantOK    bool
	}{
		{
			// Community is empty (the demo signer's evidence was removed in the
			// #1499 follow-up), so a community-issuer signer is admitted as
			// "reported" only, not classified.
			name:     "community-issuer signer admitted as reported (no community entries)",
			issuer:   "https://github.com/login/oauth",
			identity: "community-signer@example.com",
			wantOK:   false,
		},
		{
			name:      "first-party pattern on main",
			issuer:    ghActions,
			identity:  "https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml@refs/heads/main",
			wantClass: ClassFirstParty,
			wantOK:    true,
		},
		{
			name:     "unknown signer admitted as reported",
			issuer:   "https://github.com/login/oauth",
			identity: "someone-else@example.com",
			wantOK:   false,
		},
		{
			name:     "right workflow wrong org does not match pattern",
			issuer:   ghActions,
			identity: "https://github.com/evil/aicr/.github/workflows/uat-aws.yaml@refs/heads/main",
			wantOK:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			class, entry, ok := al.Classify(tt.issuer, tt.identity)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if class != tt.wantClass {
				t.Errorf("class = %q, want %q", class, tt.wantClass)
			}
			if entry == nil {
				t.Error("entry is nil for a matched signer")
			}
		})
	}
}

func TestValidate_Rejects(t *testing.T) {
	tests := []struct {
		name string
		al   Allowlist
	}{
		{
			name: "unsupported schema",
			al:   Allowlist{SchemaVersion: "2.0.0"},
		},
		{
			name: "entry missing issuer",
			al: Allowlist{SchemaVersion: SchemaVersion, Community: []Entry{
				{Source: "7c4c0edc8c765a95a0f3afdb3bbb8e91"},
			}},
		},
		{
			name: "both source and pattern set",
			al: Allowlist{SchemaVersion: SchemaVersion, Community: []Entry{
				{Issuer: "https://github.com/login/oauth", Source: "7c4c0edc8c765a95a0f3afdb3bbb8e91", IdentityPattern: "^x@y$"},
			}},
		},
		{
			name: "neither source nor pattern",
			al: Allowlist{SchemaVersion: SchemaVersion, Community: []Entry{
				{Issuer: "https://github.com/login/oauth"},
			}},
		},
		{
			name: "malformed slug (too short)",
			al: Allowlist{SchemaVersion: SchemaVersion, Community: []Entry{
				{Issuer: "https://github.com/login/oauth", Source: "abc"},
			}},
		},
		{
			name: "malformed slug (non-hex)",
			al: Allowlist{SchemaVersion: SchemaVersion, Community: []Entry{
				{Issuer: "https://github.com/login/oauth", Source: "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"},
			}},
		},
		{
			name: "community entry uses identityPattern (cleartext PII in wrong tier)",
			al: Allowlist{SchemaVersion: SchemaVersion, Community: []Entry{
				{Issuer: "https://github.com/login/oauth", IdentityPattern: "^x@y$"},
			}},
		},
		{
			name: "first-party entry uses source (must pin a bounded pattern)",
			al: Allowlist{SchemaVersion: SchemaVersion, FirstParty: []Entry{
				{Issuer: ghActions, Source: "7c4c0edc8c765a95a0f3afdb3bbb8e91"},
			}},
		},
		{
			name: "pattern not anchored",
			al: Allowlist{SchemaVersion: SchemaVersion, FirstParty: []Entry{
				{Issuer: ghActions, Label: "x",
					IdentityPattern: `https://github\.com/NVIDIA/aicr@refs/heads/.+`},
			}},
		},
		{
			name: "over-broad wildcard org",
			al: Allowlist{SchemaVersion: SchemaVersion, FirstParty: []Entry{
				{Issuer: ghActions, Label: "x",
					IdentityPattern: `^https://github\.com/.+/aicr/\.github/workflows/x\.yaml@refs/heads/.+$`},
			}},
		},
		{
			name: "over-broad wildcard repo",
			al: Allowlist{SchemaVersion: SchemaVersion, FirstParty: []Entry{
				{Issuer: ghActions, Label: "x",
					IdentityPattern: `^https://github\.com/NVIDIA/.*@refs/heads/.+$`},
			}},
		},
		{
			name: "duplicate source across classes",
			al: Allowlist{SchemaVersion: SchemaVersion,
				Community: []Entry{{Issuer: "https://github.com/login/oauth", Source: "7c4c0edc8c765a95a0f3afdb3bbb8e91"}},
				Partner:   []Entry{{Issuer: "https://github.com/login/oauth", Source: "7c4c0edc8c765a95a0f3afdb3bbb8e91"}},
			},
		},
		{
			name: "duplicate label",
			al: Allowlist{SchemaVersion: SchemaVersion, Community: []Entry{
				{Label: "dup", Issuer: "https://github.com/login/oauth", Source: "7c4c0edc8c765a95a0f3afdb3bbb8e91"},
				{Label: "dup", Issuer: "https://github.com/login/oauth", Source: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			}},
		},
		{
			name: "overlapping patterns same workflow",
			al: Allowlist{SchemaVersion: SchemaVersion, FirstParty: []Entry{
				{Label: "a", Issuer: ghActions,
					IdentityPattern: `^https://github\.com/NVIDIA/aicr/\.github/workflows/x\.yaml@refs/heads/.+$`},
				{Label: "b", Issuer: ghActions,
					IdentityPattern: `^https://github\.com/NVIDIA/aicr/\.github/workflows/x\.yaml@refs/tags/.+$`},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.al.Validate(); err == nil {
				t.Errorf("Validate() = nil, want error")
			}
		})
	}
}

// TestValidate_AcceptsTightlyBoundedPattern confirms a legitimately bounded
// first-party pattern (literal org/repo/workflow, wildcard ref only) passes,
// as does a slug entry with no label.
func TestValidate_Accepts(t *testing.T) {
	al := Allowlist{
		SchemaVersion: SchemaVersion,
		FirstParty: []Entry{
			{Label: "x", Issuer: ghActions,
				IdentityPattern: `^https://github\.com/NVIDIA/aicr/\.github/workflows/x\.yaml@refs/heads/.+$`},
		},
		Community: []Entry{
			{Issuer: "https://github.com/login/oauth", Source: "7c4c0edc8c765a95a0f3afdb3bbb8e91"}, // no label
		},
	}
	if err := al.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}
