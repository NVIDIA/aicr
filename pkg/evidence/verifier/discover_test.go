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

package verifier

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

// repoEvidenceRoot and repoAllowlist locate the committed evidence tree
// relative to this package (pkg/evidence/verifier -> repo root).
const (
	repoEvidenceRoot = "../../../recipes/evidence"
	repoAllowlist    = "../../../recipes/evidence/allowlist.yaml"
)

// testIssuer is the OIDC issuer shared by the synthetic pointers below; it
// matches the committed community entry in the allowlist.
const testIssuer = "https://github.com/login/oauth"

// testRecipe is the recipe directory the synthetic pointers live under.
const testRecipe = "h100-gke-cos-training"

// pointerYAML renders a minimal but schema-valid single-attestation pointer
// for testRecipe, signed by identity under testIssuer. The pointer's recipe
// field is always testRecipe; tests exercise a recipe/directory mismatch by
// writing the file under a different directory, not by changing this field.
func pointerYAML(oci, digest, identity string) string {
	return "schemaVersion: 1.0.0\n" +
		"recipe: " + testRecipe + "\n" +
		"attestations:\n" +
		"  - bundle:\n" +
		"      oci: " + oci + "\n" +
		"      digest: " + digest + "\n" +
		"      predicateType: " + attestation.PredicateTypeV1 + "\n" +
		"    signer:\n" +
		"      identity: " + identity + "\n" +
		"      issuer: " + testIssuer + "\n" +
		"    attestedAt: 2026-06-23T18:24:27Z\n"
}

func writePointer(t *testing.T, root, recipe, source, file, body string) {
	t.Helper()
	dir := filepath.Join(root, recipe, source)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// mustSlug derives the source slug for identity under testIssuer, or fails.
func mustSlug(t *testing.T, identity string) string {
	t.Helper()
	s, err := attestation.SourceSlug(testIssuer, identity)
	if err != nil {
		t.Fatalf("SourceSlug: %v", err)
	}
	return s
}

// TestDiscoverPointers_TwoSourcesOneRecipe is the #1347 two-sources-one-recipe
// case: two parties submit evidence for the same recipe under their own
// <source>/ directories; both files are found by glob discovery and neither
// overwrites the other.
func TestDiscoverPointers_TwoSourcesOneRecipe(t *testing.T) {
	root := t.TempDir()

	const idA = "alice@example.com"
	srcA := mustSlug(t, idA)
	const idB = "bob@example.com"
	srcB := mustSlug(t, idB)

	writePointer(t, root, testRecipe, srcA, "sha256-aaa.yaml",
		pointerYAML("ghcr.io/alice/e:v1", "sha256:aaa", idA))
	writePointer(t, root, testRecipe, srcB, "sha256-bbb.yaml",
		pointerYAML("ghcr.io/bob/e:v1", "sha256:bbb", idB))

	got, err := DiscoverPointers(root, testRecipe)
	if err != nil {
		t.Fatalf("DiscoverPointers: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("discovered %d pointers, want 2: %v", len(got), got)
	}
	// Both files parse independently as single-attestation pointers.
	for _, p := range got {
		if _, err := LoadAndValidatePointer(p); err != nil {
			t.Errorf("discovered pointer %s failed to validate: %v", p, err)
		}
	}
	if srcA == srcB {
		t.Fatal("expected distinct source slugs for distinct identities")
	}
}

func TestDiscoverPointers_EmptyRecipe(t *testing.T) {
	got, err := DiscoverPointers(t.TempDir(), "no-such-recipe")
	if err != nil {
		t.Fatalf("DiscoverPointers: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d, want 0", len(got))
	}
}

// TestCheckEvidenceTree_CommittedTreeClean asserts the real committed
// recipes/evidence/ tree passes the contract against the committed allowlist.
func TestCheckEvidenceTree_CommittedTreeClean(t *testing.T) {
	problems, err := CheckEvidenceTree(repoEvidenceRoot, repoAllowlist, false)
	if err != nil {
		t.Fatalf("CheckEvidenceTree: %v", err)
	}
	for _, p := range problems {
		t.Errorf("unexpected problem: %s", p)
	}
}

// TestCheckEvidenceTree_RejectsSquat is the path-ownership test: a pointer
// signed by one identity but committed under a different party's <source>/
// directory is rejected.
func TestCheckEvidenceTree_RejectsSquat(t *testing.T) {
	root := t.TempDir()
	const victimID = "yuanchen97@gmail.com"
	const attackerID = "attacker@example.com"

	victimSource := mustSlug(t, victimID)
	// Attacker writes under the victim's directory but signs as themselves.
	writePointer(t, root, testRecipe, victimSource, "sha256-evil.yaml",
		pointerYAML("ghcr.io/attacker/e:v1", "sha256:evil", attackerID))

	problems, err := CheckEvidenceTree(root, repoAllowlist, false)
	if err != nil {
		t.Fatalf("CheckEvidenceTree: %v", err)
	}
	if len(problems) == 0 {
		t.Fatal("expected a path-ownership/squat rejection, got none")
	}
}

func TestCheckEvidenceTree_RejectsUnsigned(t *testing.T) {
	root := t.TempDir()
	body := "schemaVersion: 1.0.0\nrecipe: " + testRecipe + "\n" +
		"attestations:\n  - bundle:\n      oci: ghcr.io/x/e:v1\n      digest: sha256:abc\n" +
		"      predicateType: " + attestation.PredicateTypeV1 + "\n    attestedAt: 2026-06-23T18:24:27Z\n"
	writePointer(t, root, testRecipe, "7c4c0edc8c765a95a0f3afdb3bbb8e91", "sha256-abc.yaml", body)

	problems, err := CheckEvidenceTree(root, repoAllowlist, false)
	if err != nil {
		t.Fatalf("CheckEvidenceTree: %v", err)
	}
	if len(problems) == 0 {
		t.Fatal("expected unsigned pointer to be rejected")
	}
}

// TestCheckEvidenceTree_RejectsUnlisted rejects a correctly-pathed pointer
// whose verified signer is not in the allowlist.
func TestCheckEvidenceTree_RejectsUnlisted(t *testing.T) {
	root := t.TempDir()
	const id = "stranger@example.com"
	src := mustSlug(t, id)
	writePointer(t, root, testRecipe, src, "sha256-abc.yaml",
		pointerYAML("ghcr.io/x/e:v1", "sha256:abc", id))

	problems, err := CheckEvidenceTree(root, repoAllowlist, false)
	if err != nil {
		t.Fatalf("CheckEvidenceTree: %v", err)
	}
	if len(problems) == 0 {
		t.Fatal("expected unlisted signer to be rejected")
	}
}

// TestCheckEvidenceTree_RejectsLooseFiles rejects a pointer placed directly
// in a recipe directory (not under a <source>/ subdir) and an unexpected
// non-allowlist file at the evidence root.
func TestCheckEvidenceTree_RejectsLooseFiles(t *testing.T) {
	root := t.TempDir()
	// Loose pointer directly under the recipe dir.
	if err := os.MkdirAll(filepath.Join(root, testRecipe), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, testRecipe, "loose.yaml"), []byte("x: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Unexpected file at the evidence root.
	if err := os.WriteFile(filepath.Join(root, "stray.yaml"), []byte("x: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	problems, err := CheckEvidenceTree(root, repoAllowlist, false)
	if err != nil {
		t.Fatalf("CheckEvidenceTree: %v", err)
	}
	if len(problems) < 2 {
		t.Fatalf("expected at least 2 problems (loose file + stray root file), got %d: %v",
			len(problems), problems)
	}
	// Exercise the String() formatter used by the CI tool's output.
	if s := problems[0].String(); s == "" || !strings.Contains(s, ": ") {
		t.Errorf("TreeProblem.String() = %q, want %q-style", s, "path: message")
	}
}

// flatPendingYAML renders an unsigned single-attestation pointer for
// testRecipe that references a pushed bundle — the commit-flat intermediate
// of the two-phase publish flow (#1530).
func flatPendingYAML(recipe string) string {
	return "schemaVersion: 1.0.0\n" +
		"recipe: " + recipe + "\n" +
		"attestations:\n  - bundle:\n      oci: ghcr.io/x/aicr-evidence:v1\n" +
		"      digest: sha256:abc\n" +
		"      predicateType: " + attestation.PredicateTypeV1 + "\n" +
		"    attestedAt: 2026-06-23T18:24:27Z\n"
}

// TestCheckEvidenceTree_AcceptsFlatPendingPointer accepts a flat, unsigned
// <recipe>.yaml at the root — the transient commit-flat state CI signs and
// relocates. The per-source contract gate must not reject it on the PR before
// the signing leg can run.
func TestCheckEvidenceTree_AcceptsFlatPendingPointer(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, testRecipe+".yaml"),
		[]byte(flatPendingYAML(testRecipe)), 0o600); err != nil {
		t.Fatal(err)
	}
	problems, err := CheckEvidenceTree(root, repoAllowlist, true)
	if err != nil {
		t.Fatalf("CheckEvidenceTree: %v", err)
	}
	for _, p := range problems {
		t.Errorf("unexpected problem for a valid flat pending pointer: %s", p)
	}
}

// TestCheckEvidenceTree_RejectsFlatPendingWhenNotAllowed is the merge-gate
// guard (mchmarny, #1538): with allowPending=false — the posture the blocking
// contract gate runs under — a flat unsigned pending pointer is rejected, so it
// cannot merge to a protected branch before the sign+relocate leg has run.
func TestCheckEvidenceTree_RejectsFlatPendingWhenNotAllowed(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, testRecipe+".yaml"),
		[]byte(flatPendingYAML(testRecipe)), 0o600); err != nil {
		t.Fatal(err)
	}
	problems, err := CheckEvidenceTree(root, repoAllowlist, false)
	if err != nil {
		t.Fatalf("CheckEvidenceTree: %v", err)
	}
	if len(problems) == 0 {
		t.Fatal("expected a flat pending pointer to be rejected when allowPending=false")
	}
}

// TestCheckEvidenceTree_RejectsSignedFlatPointer rejects a SIGNED pointer left
// flat at the root: a signed pointer has a derivable <source> and must live
// nested under <recipe>/<source>/.
func TestCheckEvidenceTree_RejectsSignedFlatPointer(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, testRecipe+".yaml"),
		[]byte(pointerYAML("ghcr.io/x/e:v1", "sha256:abc", "alice@example.com")), 0o600); err != nil {
		t.Fatal(err)
	}
	problems, err := CheckEvidenceTree(root, repoAllowlist, true)
	if err != nil {
		t.Fatalf("CheckEvidenceTree: %v", err)
	}
	if len(problems) == 0 {
		t.Fatal("expected a signed flat pointer to be rejected (must be nested)")
	}
}

// TestCheckEvidenceTree_RejectsMisnamedFlatPointer rejects a flat pending
// pointer whose filename does not match its recipe field.
func TestCheckEvidenceTree_RejectsMisnamedFlatPointer(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "wrong-name.yaml"),
		[]byte(flatPendingYAML(testRecipe)), 0o600); err != nil {
		t.Fatal(err)
	}
	problems, err := CheckEvidenceTree(root, repoAllowlist, true)
	if err != nil {
		t.Fatalf("CheckEvidenceTree: %v", err)
	}
	if len(problems) == 0 {
		t.Fatal("expected a misnamed flat pending pointer to be rejected")
	}
}

// TestCheckEvidenceTree_RejectsUnpushedFlatPointer rejects a flat pending
// pointer that references no pushed bundle (nothing to sign later).
func TestCheckEvidenceTree_RejectsUnpushedFlatPointer(t *testing.T) {
	root := t.TempDir()
	body := "schemaVersion: 1.0.0\nrecipe: " + testRecipe + "\n" +
		"attestations:\n  - bundle:\n      predicateType: " + attestation.PredicateTypeV1 + "\n" +
		"    attestedAt: 2026-06-23T18:24:27Z\n"
	if err := os.WriteFile(filepath.Join(root, testRecipe+".yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	problems, err := CheckEvidenceTree(root, repoAllowlist, true)
	if err != nil {
		t.Fatalf("CheckEvidenceTree: %v", err)
	}
	if len(problems) == 0 {
		t.Fatal("expected a flat pending pointer with no pushed bundle to be rejected")
	}
}

// TestCheckEvidenceTree_MissingAllowlist surfaces an operational error
// (distinct from a contract violation) when the allowlist is unreadable.
func TestCheckEvidenceTree_MissingAllowlist(t *testing.T) {
	if _, err := CheckEvidenceTree(t.TempDir(), filepath.Join(t.TempDir(), "nope.yaml"), false); err == nil {
		t.Error("expected an error for a missing allowlist")
	}
}

// TestCheckEvidenceTree_RejectsRecipeMismatch rejects a pointer whose recipe
// field disagrees with the directory it sits in.
func TestCheckEvidenceTree_RejectsRecipeMismatch(t *testing.T) {
	root := t.TempDir()
	const id = "yuanchen97@gmail.com"
	src := mustSlug(t, id)
	// File lives under gb200-... but the pointer claims testRecipe (h100-...).
	writePointer(t, root, "gb200-eks-ubuntu-training", src, "sha256-abc.yaml",
		pointerYAML("ghcr.io/x/e:v1", "sha256:abc", id))

	problems, err := CheckEvidenceTree(root, repoAllowlist, false)
	if err != nil {
		t.Fatalf("CheckEvidenceTree: %v", err)
	}
	if len(problems) == 0 {
		t.Fatal("expected recipe/directory mismatch to be rejected")
	}
}
