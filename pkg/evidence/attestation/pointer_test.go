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
	stderrors "errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/fingerprint"
)

func ptrInt64(v int64) *int64 { return &v }

// TestMarshalPointer_TwoSpaceIndent guards the indentation contract: the
// pointer is committed to recipes/evidence/<recipe>.yaml, where the repo's
// .yamllint (spaces: 2) lints it. yaml.v3's default 4-space sequence indent
// would fail `make lint`, so MarshalPointer must emit 2-space indentation.
func TestMarshalPointer_TwoSpaceIndent(t *testing.T) {
	bundle := &Bundle{
		RecipeName: "h100-eks-ubuntu-inference-dynamo",
		Predicate: &Predicate{
			SchemaVersion: PredicateSchemaVersion,
			AttestedAt:    time.Date(2026, 6, 3, 0, 52, 58, 0, time.UTC),
		},
	}
	rekorIdx := int64(1706788485)
	p, err := BuildPointer(PointerInputs{
		Bundle:     bundle,
		BundleOCI:  "ghcr.io/nvidia/aicr-evidence:v1",
		BundleHash: "sha256:da9d8838",
		Signer:     &PointerSigner{Identity: "test@example.com", Issuer: "https://oauth.example.com", RekorLogIndex: &rekorIdx},
	})
	if err != nil {
		t.Fatalf("BuildPointer: %v", err)
	}
	out, err := MarshalPointer(p)
	if err != nil {
		t.Fatalf("MarshalPointer: %v", err)
	}
	got := string(out)

	// The attestations sequence item must sit at 2-space indent, not 4.
	// (Keys are sorted, so attestations may be the first top-level key.)
	if !strings.Contains(got, "attestations:\n  - ") {
		t.Errorf("attestations sequence not at 2-space indent:\n%s", got)
	}
	if strings.Contains(got, "\n    - ") {
		t.Errorf("found 4-space sequence indent (would fail yamllint spaces:2):\n%s", got)
	}
	// Every indented line must use an even number of leading spaces.
	for _, line := range strings.Split(got, "\n") {
		if n := len(line) - len(strings.TrimLeft(line, " ")); n%2 != 0 {
			t.Errorf("odd leading-space count (%d) on line %q", n, line)
		}
	}
	// Round-trips to an equivalent pointer (re-indentation preserves content).
	var rt Pointer
	if err := yaml.Unmarshal(out, &rt); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if rt.Recipe != p.Recipe || len(rt.Attestations) != 1 || rt.Attestations[0].Bundle.OCI != p.Attestations[0].Bundle.OCI {
		t.Errorf("round-trip mismatch: %+v", rt)
	}
}

func TestBuildPointer_RequiresBundle(t *testing.T) {
	if _, err := BuildPointer(PointerInputs{}); err == nil {
		t.Errorf("expected error when bundle is nil")
	}
}

func TestPointerCopyToHint(t *testing.T) {
	signed := &Pointer{
		SchemaVersion: PointerSchemaVersion,
		Recipe:        "h100-gke-cos-training",
		Attestations: []PointerAttestation{{
			Bundle: PointerBundle{
				OCI:           "ghcr.io/yuanchen8911/aicr-evidence:x",
				Digest:        "sha256:33d4cf36622ead990c43a596f6f53c62b87d9fa4708f59b7e3f356f215e54317",
				PredicateType: PredicateTypeV1,
			},
			Signer: &PointerSigner{
				Identity: "yuanchen97@gmail.com",
				Issuer:   "https://github.com/login/oauth",
			},
		}},
	}
	// 7c4c0edc8c765a95a0f3afdb3bbb8e91 is SourceSlug(issuer, identity) for this signer.
	want := "recipes/evidence/h100-gke-cos-training/7c4c0edc8c765a95a0f3afdb3bbb8e91/sha256-33d4cf36622ead990c43a596f6f53c62b87d9fa4708f59b7e3f356f215e54317.yaml"
	if got := PointerCopyToHint(signed); got != want {
		t.Errorf("signed hint = %q, want %q", got, want)
	}

	// Unsigned / unpushed pointers have no committable destination.
	unsigned := &Pointer{
		SchemaVersion: PointerSchemaVersion,
		Recipe:        "h100-gke-cos-training",
		Attestations:  []PointerAttestation{{Bundle: PointerBundle{PredicateType: PredicateTypeV1}}},
	}
	if got := PointerCopyToHint(unsigned); strings.HasPrefix(got, "recipes/evidence/") {
		t.Errorf("unsigned hint should not be a path, got %q", got)
	}
}

func TestRelocatePointerToCanonical(t *testing.T) {
	signed := func() *Pointer {
		return &Pointer{
			SchemaVersion: PointerSchemaVersion,
			Recipe:        "h100-gke-cos-training",
			Attestations: []PointerAttestation{{
				Bundle: PointerBundle{
					OCI:           "ghcr.io/yuanchen8911/aicr-evidence:x",
					Digest:        "sha256:33d4cf36622ead990c43a596f6f53c62b87d9fa4708f59b7e3f356f215e54317",
					PredicateType: PredicateTypeV1,
				},
				Signer: &PointerSigner{
					Identity: "yuanchen97@gmail.com",
					Issuer:   "https://github.com/login/oauth",
				},
			}},
		}
	}

	t.Run("moves flat pending pointer to nested canonical path", func(t *testing.T) {
		root := t.TempDir()
		flat := filepath.Join(root, "h100-gke-cos-training.yaml")
		if _, err := WritePointerFile(flat, signed()); err != nil {
			t.Fatalf("write flat: %v", err)
		}

		dest, err := RelocatePointerToCanonical(flat, signed())
		if err != nil {
			t.Fatalf("RelocatePointerToCanonical: %v", err)
		}
		// 7c4c0edc8c765a95a0f3afdb3bbb8e91 is SourceSlug(issuer, identity).
		want := filepath.Join(root, "h100-gke-cos-training",
			"7c4c0edc8c765a95a0f3afdb3bbb8e91", "sha256-33d4cf36622ead990c43a596f6f53c62b87d9fa4708f59b7e3f356f215e54317.yaml")
		if dest != want {
			t.Errorf("dest = %q, want %q", dest, want)
		}
		if _, err := os.Stat(dest); err != nil {
			t.Errorf("destination not written: %v", err)
		}
		if _, err := os.Stat(flat); !os.IsNotExist(err) {
			t.Errorf("flat source should be gone, stat err = %v", err)
		}
	})

	t.Run("is a no-op when already at canonical path", func(t *testing.T) {
		root := t.TempDir()
		canonical := filepath.Join(root, "h100-gke-cos-training",
			"7c4c0edc8c765a95a0f3afdb3bbb8e91", "sha256-33d4cf36622ead990c43a596f6f53c62b87d9fa4708f59b7e3f356f215e54317.yaml")
		if err := os.MkdirAll(filepath.Dir(canonical), 0o750); err != nil {
			t.Fatal(err)
		}
		if _, err := WritePointerFile(canonical, signed()); err != nil {
			t.Fatalf("write canonical: %v", err)
		}
		dest, err := RelocatePointerToCanonical(canonical, signed())
		if err != nil {
			t.Fatalf("RelocatePointerToCanonical: %v", err)
		}
		if dest != canonical {
			t.Errorf("dest = %q, want unchanged %q", dest, canonical)
		}
		if _, err := os.Stat(canonical); err != nil {
			t.Errorf("canonical file should still exist: %v", err)
		}
	})

	t.Run("refuses to clobber a canonical pointer with different content", func(t *testing.T) {
		root := t.TempDir()
		flat := filepath.Join(root, "h100-gke-cos-training.yaml")
		if _, err := WritePointerFile(flat, signed()); err != nil {
			t.Fatalf("write flat: %v", err)
		}
		canonical := filepath.Join(root, "h100-gke-cos-training",
			"7c4c0edc8c765a95a0f3afdb3bbb8e91", "sha256-33d4cf36622ead990c43a596f6f53c62b87d9fa4708f59b7e3f356f215e54317.yaml")
		if err := os.MkdirAll(filepath.Dir(canonical), 0o750); err != nil {
			t.Fatal(err)
		}
		// A DIFFERENT pointer already occupies the canonical path (same
		// recipe/source/digest leaf, different bundle.oci → different bytes):
		// a genuine immutable-pointer conflict, not an idempotent re-placement.
		other := signed()
		other.Attestations[0].Bundle.OCI = "ghcr.io/someone-else/aicr-evidence:y"
		if _, err := WritePointerFile(canonical, other); err != nil {
			t.Fatalf("write canonical: %v", err)
		}
		// Assert the structured code, not just non-nil, so an INTERNAL
		// regression can't masquerade as the EEXIST conflict contract.
		if _, err := RelocatePointerToCanonical(flat, signed()); !stderrors.Is(err, errors.New(errors.ErrCodeConflict, "")) {
			t.Errorf("expected ErrCodeConflict when the canonical path holds different content, got %v", err)
		}
		// The flat source must be left in place on a refusal (no move happened).
		if _, err := os.Stat(flat); err != nil {
			t.Errorf("flat source should remain after refusal: %v", err)
		}
	})

	t.Run("recovers when dest holds byte-identical content (distinct inode)", func(t *testing.T) {
		// The committed-both case: a git round trip leaves the flat source and
		// the nested copy as distinct inodes but byte-equal. Retry must finish
		// the move (drop the redundant flat source) rather than conflict.
		root := t.TempDir()
		flat := filepath.Join(root, "h100-gke-cos-training.yaml")
		if _, err := WritePointerFile(flat, signed()); err != nil {
			t.Fatalf("write flat: %v", err)
		}
		canonical := filepath.Join(root, "h100-gke-cos-training",
			"7c4c0edc8c765a95a0f3afdb3bbb8e91", "sha256-33d4cf36622ead990c43a596f6f53c62b87d9fa4708f59b7e3f356f215e54317.yaml")
		if err := os.MkdirAll(filepath.Dir(canonical), 0o750); err != nil {
			t.Fatal(err)
		}
		// Write (not link) identical content → same bytes, different inode.
		if _, err := WritePointerFile(canonical, signed()); err != nil {
			t.Fatalf("write canonical: %v", err)
		}
		if sameInode(flat, canonical) {
			t.Fatal("precondition: flat and canonical should be distinct inodes")
		}
		dest, err := RelocatePointerToCanonical(flat, signed())
		if err != nil {
			t.Fatalf("expected idempotent recovery on identical content, got error: %v", err)
		}
		if dest != canonical {
			t.Errorf("dest = %q, want %q", dest, canonical)
		}
		if _, err := os.Stat(flat); !os.IsNotExist(err) {
			t.Errorf("flat source should be removed after recovery, stat err = %v", err)
		}
		if _, err := os.Stat(canonical); err != nil {
			t.Errorf("canonical should remain after recovery: %v", err)
		}
	})

	t.Run("recovers a partial move where dest is the same inode", func(t *testing.T) {
		// Simulate a prior run that linked dest but stopped before removing the
		// flat source (os.Link ok, os.Remove not yet). Retry must finish the
		// move (remove the stale source, return dest) — not fail with a
		// spurious clobber conflict.
		root := t.TempDir()
		flat := filepath.Join(root, "h100-gke-cos-training.yaml")
		if _, err := WritePointerFile(flat, signed()); err != nil {
			t.Fatalf("write flat: %v", err)
		}
		canonical := filepath.Join(root, "h100-gke-cos-training",
			"7c4c0edc8c765a95a0f3afdb3bbb8e91", "sha256-33d4cf36622ead990c43a596f6f53c62b87d9fa4708f59b7e3f356f215e54317.yaml")
		if err := os.MkdirAll(filepath.Dir(canonical), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(flat, canonical); err != nil {
			t.Fatalf("simulate prior link: %v", err)
		}
		dest, err := RelocatePointerToCanonical(flat, signed())
		if err != nil {
			t.Fatalf("expected idempotent recovery, got error: %v", err)
		}
		if dest != canonical {
			t.Errorf("dest = %q, want %q", dest, canonical)
		}
		if _, err := os.Stat(flat); !os.IsNotExist(err) {
			t.Errorf("flat source should be removed after recovery, stat err = %v", err)
		}
		if _, err := os.Stat(canonical); err != nil {
			t.Errorf("canonical should remain after recovery: %v", err)
		}
	})

	t.Run("rejects an unsigned pointer", func(t *testing.T) {
		unsigned := signed()
		unsigned.Attestations[0].Signer = nil
		if _, err := RelocatePointerToCanonical(filepath.Join(t.TempDir(), "x.yaml"), unsigned); err == nil {
			t.Error("expected an error for an unsigned pointer")
		}
	})

	t.Run("rejects a pointer with no digest", func(t *testing.T) {
		noDigest := signed()
		noDigest.Attestations[0].Bundle.Digest = ""
		if _, err := RelocatePointerToCanonical(filepath.Join(t.TempDir(), "x.yaml"), noDigest); err == nil {
			t.Error("expected an error for a pointer with no pushed digest")
		}
	})

	t.Run("rejects a digest that is not a canonical sha256:<64-hex>", func(t *testing.T) {
		// isHexDigest runs first in canonicalPointerSuffix (before any
		// filesystem op), so each of these is rejected by the guard, not by an
		// incidental os.Link failure. Covers: path traversal in the digest,
		// non-hex characters, and a hex body that isn't exactly 64 chars
		// (too short / too long).
		for _, digest := range []string{
			"sha256:../../etc/evil",
			"sha256:a/b",
			"sha256:..",
			"sha256:nothex" + strings.Repeat("z", 58), // 64 chars, non-hex
			"sha256:abc",                        // hex but too short
			"sha256:" + strings.Repeat("a", 63), // 63 hex: too short
			"sha256:" + strings.Repeat("a", 65), // 65 hex: too long
			"md5:" + strings.Repeat("a", 64),    // wrong algorithm prefix
		} {
			evil := signed()
			evil.Attestations[0].Bundle.Digest = digest
			if _, err := RelocatePointerToCanonical(filepath.Join(t.TempDir(), "x.yaml"), evil); err == nil {
				t.Errorf("digest %q: expected a rejection, got nil", digest)
			}
		}
	})

	t.Run("rejects a recipe with path traversal", func(t *testing.T) {
		// defense-in-depth: a recipe escaping the evidence tree must never
		// reach os.Rename, even when called directly (no CI gate in front).
		for _, recipe := range []string{"../../etc/evil", "/abs/evil", ".."} {
			evil := signed()
			evil.Recipe = recipe
			root := t.TempDir()
			flat := filepath.Join(root, "x.yaml")
			if err := os.WriteFile(flat, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := RelocatePointerToCanonical(flat, evil); err == nil {
				t.Errorf("recipe %q: expected a rejection, got nil", recipe)
			}
			// The flat source must be untouched (no move attempted).
			if _, err := os.Stat(flat); err != nil {
				t.Errorf("recipe %q: flat source should be untouched: %v", recipe, err)
			}
		}
	})
}

func TestBuildPointer_ProducesSingleAttestation(t *testing.T) {
	bundle := &Bundle{
		RecipeName: "h100-eks-ubuntu-training",
		Predicate: &Predicate{
			SchemaVersion: PredicateSchemaVersion,
			AttestedAt:    time.Date(2026, 5, 8, 10, 23, 11, 0, time.UTC),
			CriteriaMatch: fingerprint.MatchResult{Matched: true},
			Phases: map[Phase]PhaseSummary{
				PhaseDeployment: {Passed: 5, Failed: 0},
			},
			Fingerprint: fingerprint.Fingerprint{
				Accelerator: fingerprint.Dimension{Value: "h100"},
			},
		},
	}
	rekorIdx := int64(42)
	p, err := BuildPointer(PointerInputs{
		Bundle:     bundle,
		BundleOCI:  "ghcr.io/foo/aicr-evidence:abc",
		BundleHash: "sha256:abc",
		Signer: &PointerSigner{
			Identity:      "test@example.com",
			Issuer:        "https://oauth.example.com",
			RekorLogIndex: &rekorIdx,
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.SchemaVersion != PointerSchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", p.SchemaVersion, PointerSchemaVersion)
	}
	if p.Recipe != bundle.RecipeName {
		t.Errorf("Recipe = %q, want %q", p.Recipe, bundle.RecipeName)
	}
	if len(p.Attestations) != 1 {
		t.Fatalf("expected 1 attestation, got %d", len(p.Attestations))
	}
	att := p.Attestations[0]
	if att.Bundle.PredicateType != PredicateTypeV1 {
		t.Errorf("PredicateType = %q", att.Bundle.PredicateType)
	}
	if att.Bundle.OCI != "ghcr.io/foo/aicr-evidence:abc" {
		t.Errorf("OCI mismatch: %q", att.Bundle.OCI)
	}
	if att.Signer == nil {
		t.Fatalf("Signer should be non-nil for signed bundle")
	}
	if att.Signer.RekorLogIndex == nil || *att.Signer.RekorLogIndex != 42 {
		t.Errorf("RekorLogIndex = %v, want *int64(42)", att.Signer.RekorLogIndex)
	}
}

func TestBuildPointer_OmitsDenormalizedFields(t *testing.T) {
	// The pointer is a locator, not a denormalized cache of the
	// predicate. Reviewers fetch the bundle from PointerBundle.OCI to
	// read fingerprint / criteriaMatch / phaseSummary — duplicating
	// those at pointer level creates two sources of truth.
	bundle := &Bundle{
		RecipeName: "x",
		Predicate: &Predicate{
			AttestedAt:    time.Date(2026, 5, 8, 10, 23, 11, 0, time.UTC),
			CriteriaMatch: fingerprint.MatchResult{Matched: true},
			Phases: map[Phase]PhaseSummary{
				PhaseDeployment: {Passed: 12, Failed: 0, Skipped: 1},
			},
			Fingerprint: fingerprint.Fingerprint{
				Accelerator: fingerprint.Dimension{Value: "h100"},
			},
		},
	}
	p, err := BuildPointer(PointerInputs{Bundle: bundle})
	if err != nil {
		t.Fatalf("BuildPointer: %v", err)
	}
	body, err := MarshalPointer(p)
	if err != nil {
		t.Fatalf("MarshalPointer: %v", err)
	}
	for _, banned := range []string{"fingerprint", "criteriaMatch", "phaseSummary", "logsBundle"} {
		if contains(body, banned+":") {
			t.Errorf("pointer YAML must omit %q field; got:\n%s", banned, body)
		}
	}
}

func TestPointer_RoundTripsYAML(t *testing.T) {
	in := &Pointer{
		SchemaVersion: PointerSchemaVersion,
		Recipe:        "h100-eks",
		Attestations: []PointerAttestation{
			{
				Bundle: PointerBundle{
					OCI:           "ghcr.io/x/aicr-evidence:1",
					Digest:        "sha256:abc",
					PredicateType: PredicateTypeV1,
				},
				Signer:     &PointerSigner{Identity: "u@x", Issuer: "iss", RekorLogIndex: ptrInt64(7)},
				AttestedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			},
		},
	}
	body, err := MarshalPointer(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Pointer
	if err := yaml.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SchemaVersion != PointerSchemaVersion ||
		got.Recipe != "h100-eks" ||
		len(got.Attestations) != 1 ||
		got.Attestations[0].Bundle.OCI != "ghcr.io/x/aicr-evidence:1" {

		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestBuildPointer_UnsignedOmitsSigner(t *testing.T) {
	bundle := &Bundle{
		RecipeName: "x",
		Predicate: &Predicate{
			AttestedAt:    time.Now(),
			CriteriaMatch: fingerprint.MatchResult{Matched: true},
		},
	}
	p, err := BuildPointer(PointerInputs{Bundle: bundle})
	if err != nil {
		t.Fatalf("BuildPointer: %v", err)
	}
	if att := p.Attestations[0]; att.Signer != nil {
		t.Errorf("unsigned bundle should leave Signer nil; got %+v", att.Signer)
	}

	body, err := MarshalPointer(p)
	if err != nil {
		t.Fatalf("MarshalPointer: %v", err)
	}
	if contains(body, "signer:") {
		t.Errorf("unsigned pointer YAML must omit signer block; got:\n%s", body)
	}
}

func TestBuildPointer_SignedWithoutRekorOmitsLogIndex(t *testing.T) {
	bundle := &Bundle{
		RecipeName: "x",
		Predicate: &Predicate{
			AttestedAt:    time.Now(),
			CriteriaMatch: fingerprint.MatchResult{Matched: true},
		},
	}
	p, err := BuildPointer(PointerInputs{
		Bundle: bundle,
		Signer: &PointerSigner{Identity: "u@x", Issuer: "https://oauth.example.com"},
	})
	if err != nil {
		t.Fatalf("BuildPointer: %v", err)
	}
	att := p.Attestations[0]
	if att.Signer == nil {
		t.Fatalf("expected non-nil Signer for signed bundle")
	}
	if att.Signer.RekorLogIndex != nil {
		t.Errorf("RekorLogIndex should be nil when --no-rekor; got *%d", *att.Signer.RekorLogIndex)
	}

	body, err := MarshalPointer(p)
	if err != nil {
		t.Fatalf("MarshalPointer: %v", err)
	}
	if contains(body, "rekorLogIndex") {
		t.Errorf("signed-without-rekor pointer must omit rekorLogIndex; got:\n%s", body)
	}
}

func TestPointer_PrePushBundleFieldsEmpty(t *testing.T) {
	bundle := &Bundle{
		RecipeName: "x",
		Predicate: &Predicate{
			AttestedAt:    time.Now(),
			CriteriaMatch: fingerprint.MatchResult{Matched: true},
		},
	}
	p, err := BuildPointer(PointerInputs{Bundle: bundle})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	att := p.Attestations[0]
	if att.Bundle.OCI != "" || att.Bundle.Digest != "" {
		t.Errorf("pre-push pointer should leave bundle.{oci,digest} empty; got %+v", att.Bundle)
	}
	if att.Bundle.PredicateType != PredicateTypeV1 {
		t.Errorf("predicate type should be set even pre-push")
	}
}

func TestWritePointer_WritesValidYAML(t *testing.T) {
	dir := t.TempDir()
	bundle := &Bundle{
		RecipeName: "x",
		Predicate: &Predicate{
			AttestedAt:    time.Date(2026, 5, 8, 0, 0, 0, 0, time.UTC),
			CriteriaMatch: fingerprint.MatchResult{Matched: true},
		},
	}
	p, err := BuildPointer(PointerInputs{Bundle: bundle})
	if err != nil {
		t.Fatalf("BuildPointer: %v", err)
	}
	path, err := WritePointer(dir, p)
	if err != nil {
		t.Fatalf("WritePointer: %v", err)
	}
	if path == "" {
		t.Errorf("expected non-empty pointer path")
	}
	body := mustReadFile(t, path)
	if len(body) == 0 {
		t.Errorf("written pointer is empty")
	}
}
