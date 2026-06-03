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
	"strings"
	"testing"
)

func TestSanitizeOCITag(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"already a slug", "h100-eks-ubuntu-training", "h100-eks-ubuntu-training"},
		{"uppercase and spaces", "Foo Bar Baz", "foo-bar-baz"},
		{"slashes and colons", "ghcr.io/owner:tag", "ghcr.io-owner-tag"},
		{"leading and trailing junk trimmed", "--weird.name--", "weird.name"},
		{"empty falls back to recipe", "", defaultRecipeName},
		{"all-invalid falls back to recipe", "///", defaultRecipeName},
		{"length capped", strings.Repeat("a", 200), strings.Repeat("a", maxEvidenceTagSlug)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeOCITag(tt.in); got != tt.want {
				t.Errorf("sanitizeOCITag(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestManifestFingerprint(t *testing.T) {
	tests := []struct {
		name   string
		bundle *Bundle
		want   string
	}{
		{
			name:   "first 12 hex of manifest digest",
			bundle: &Bundle{Predicate: &Predicate{Manifest: ManifestRef{Digest: "sha256:" + strings.Repeat("a", 64)}}},
			want:   strings.Repeat("a", 12),
		},
		{
			name:   "nil predicate yields empty",
			bundle: &Bundle{},
			want:   "",
		},
		{
			name:   "missing digest yields empty",
			bundle: &Bundle{Predicate: &Predicate{}},
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := manifestFingerprint(tt.bundle); got != tt.want {
				t.Errorf("manifestFingerprint() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDeriveEvidenceTag(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	tests := []struct {
		name   string
		bundle *Bundle
		want   string
	}{
		{
			name:   "slug plus fingerprint",
			bundle: &Bundle{RecipeName: "h100-eks-ubuntu-training", Predicate: &Predicate{Manifest: ManifestRef{Digest: digest}}},
			want:   "h100-eks-ubuntu-training-" + strings.Repeat("b", 12),
		},
		{
			name:   "no fingerprint falls back to slug only",
			bundle: &Bundle{RecipeName: "h100-eks-ubuntu-training"},
			want:   "h100-eks-ubuntu-training",
		},
		{
			name:   "empty recipe name uses default slug",
			bundle: &Bundle{Predicate: &Predicate{Manifest: ManifestRef{Digest: digest}}},
			want:   defaultRecipeName + "-" + strings.Repeat("b", 12),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveEvidenceTag(tt.bundle); got != tt.want {
				t.Errorf("deriveEvidenceTag() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEffectiveEvidenceRef(t *testing.T) {
	bundle := &Bundle{
		RecipeName: "h100-eks-ubuntu-training",
		Predicate:  &Predicate{Manifest: ManifestRef{Digest: "sha256:" + strings.Repeat("c", 64)}},
	}
	wantTag := "h100-eks-ubuntu-training-" + strings.Repeat("c", 12)

	t.Run("operator tag preserved (no derivation)", func(t *testing.T) {
		got, err := effectiveEvidenceRef("ghcr.io/owner/aicr-evidence:my-tag", bundle)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// The ref is returned canonicalized, but the operator's tag is kept
		// as-is — no derived fingerprint tag is substituted.
		if !strings.HasSuffix(got, ":my-tag") {
			t.Errorf("got %q, want it to keep the operator tag :my-tag", got)
		}
		if strings.Contains(got, wantTag) {
			t.Errorf("got %q, should not derive a tag when one is supplied", got)
		}
	})

	t.Run("missing tag gets derived per-recipe tag", func(t *testing.T) {
		got, err := effectiveEvidenceRef("ghcr.io/owner/aicr-evidence", bundle)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.HasSuffix(got, ":"+wantTag) {
			t.Errorf("got %q, want suffix %q", got, ":"+wantTag)
		}
		if !strings.Contains(got, "ghcr.io/owner/aicr-evidence") {
			t.Errorf("got %q, want it to retain the repository", got)
		}
	})

	t.Run("non-OCI reference rejected", func(t *testing.T) {
		if _, err := effectiveEvidenceRef("/local/path", bundle); err == nil {
			t.Error("expected error for non-OCI reference, got nil")
		}
	})
}

func TestPush_RequiresTag(t *testing.T) {
	_, err := Push(context.Background(), PushOptions{
		SourceDir: t.TempDir(),
		Reference: "oci://ghcr.io/owner/aicr-evidence",
	})
	if err == nil {
		t.Fatal("expected error for tag-less reference, got nil")
	}
	if !strings.Contains(err.Error(), "must include a tag") {
		t.Errorf("error = %v, want it to mention a missing tag", err)
	}
}
