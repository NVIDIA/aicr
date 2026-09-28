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
	"strings"
	"testing"
)

func TestCanonicalizeRecipeYAML_SortsKeys(t *testing.T) {
	in := []byte("zoo: 1\napple: 2\n")
	got, err := CanonicalizeRecipeYAML(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(string(got), "apple:") {
		t.Errorf("expected canonical form to start with apple:, got %q", got)
	}
	idxApple := strings.Index(string(got), "apple:")
	idxZoo := strings.Index(string(got), "zoo:")
	if idxApple > idxZoo {
		t.Errorf("expected apple before zoo in sorted output: %q", got)
	}
}

func TestCanonicalizeRecipeYAML_StripsComments(t *testing.T) {
	in := []byte("# leading comment\nfoo: bar # trailing comment\n# tail comment\n")
	got, err := CanonicalizeRecipeYAML(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(string(got), "comment") {
		t.Errorf("expected canonicalize to strip comments, got %q", got)
	}
}

func TestCanonicalizeRecipeYAML_StableUnderReorder(t *testing.T) {
	a := []byte("foo: bar\nbaz: qux\n")
	b := []byte("baz: qux\nfoo: bar\n")
	ca, err := CanonicalizeRecipeYAML(a)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cb, err := CanonicalizeRecipeYAML(b)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(ca) != string(cb) {
		t.Errorf("expected identical canonical bytes regardless of input key order:\n%s\n---\n%s", ca, cb)
	}
}

func TestCanonicalizeRecipeYAML_StableUnderCommentChanges(t *testing.T) {
	a := []byte("foo: bar # original\n")
	b := []byte("# new leading\nfoo: bar\n")
	ca, _ := CanonicalizeRecipeYAML(a)
	cb, _ := CanonicalizeRecipeYAML(b)
	if string(ca) != string(cb) {
		t.Errorf("comment-only edit changed canonical form:\n%s\n---\n%s", ca, cb)
	}
}

func TestCanonicalizeRecipeYAML_NestedMappingsSorted(t *testing.T) {
	in := []byte(`outer:
  z: 1
  a:
    nested_b: 2
    nested_a: 1
`)
	got, err := CanonicalizeRecipeYAML(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := string(got)
	idxA := strings.Index(out, "a:")
	idxZ := strings.Index(out, "z:")
	if idxA == -1 || idxZ == -1 || idxA > idxZ {
		t.Errorf("expected a before z (nested), got:\n%s", out)
	}
	idxNA := strings.Index(out, "nested_a:")
	idxNB := strings.Index(out, "nested_b:")
	if idxNA == -1 || idxNB == -1 || idxNA > idxNB {
		t.Errorf("expected nested_a before nested_b, got:\n%s", out)
	}
}

func TestCanonicalizeRecipeYAML_EmptyInputErrors(t *testing.T) {
	if _, err := CanonicalizeRecipeYAML(nil); err == nil {
		t.Errorf("expected error on nil input")
	}
}

func TestSubjectDigest_DeterministicHex(t *testing.T) {
	in := []byte("foo: bar\nbaz: qux\n")
	d1, err := SubjectDigest(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	d2, err := SubjectDigest(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if d1 != d2 {
		t.Errorf("expected stable subject digest across calls; got %q vs %q", d1, d2)
	}
	if len(d1) != 64 {
		t.Errorf("expected 64 hex chars; got %d (%q)", len(d1), d1)
	}
}

func TestSubjectDigest_DiffersOnMaterialChange(t *testing.T) {
	a := []byte("foo: bar\n")
	b := []byte("foo: baz\n")
	da, _ := SubjectDigest(a)
	db, _ := SubjectDigest(b)
	if da == db {
		t.Errorf("expected different digests for material change; both %q", da)
	}
}

func TestSubjectDigest_BindsSlurmAccountingMode(t *testing.T) {
	t.Parallel()

	disabled := []byte(`apiVersion: aicr.run/v1beta2
kind: RecipeResult
configuration:
  slurm:
    accounting:
      mode: disabled
`)
	customerManaged := []byte(`apiVersion: aicr.run/v1beta2
kind: RecipeResult
configuration:
  slurm:
    accounting:
      mode: customer-managed
`)
	disabledDigest, err := SubjectDigest(disabled)
	if err != nil {
		t.Fatalf("SubjectDigest(disabled) error = %v", err)
	}
	customerDigest, err := SubjectDigest(customerManaged)
	if err != nil {
		t.Fatalf("SubjectDigest(customer-managed) error = %v", err)
	}
	if disabledDigest == customerDigest {
		t.Fatalf("accounting mode change did not change recipe digest: %s", disabledDigest)
	}
}

func TestCanonicalizeRecipeYAMLV3_StripsMetadataVersion(t *testing.T) {
	in := []byte("metadata:\n  version: 1.2.3\n  name: foo\nfoo: bar\n")
	got, err := CanonicalizeRecipeYAMLV3(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(string(got), "1.2.3") {
		t.Errorf("expected metadata.version to be stripped, got %q", got)
	}
	if !strings.Contains(string(got), "name: foo") {
		t.Errorf("expected other metadata fields to survive, got %q", got)
	}
}

func TestCanonicalizeRecipeYAMLV3_RemovesMetadataLeftEmptyByVersionStrip(t *testing.T) {
	versionOnly := []byte("metadata:\n  version: 1.2.3\nfoo: bar\n")
	noMetadata := []byte("foo: bar\n")

	got, err := CanonicalizeRecipeYAMLV3(versionOnly)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(string(got), "metadata") {
		t.Errorf("expected metadata key to be removed once empty, got %q", got)
	}
	want, err := CanonicalizeRecipeYAMLV3(noMetadata)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("recipe with only metadata.version should canonicalize identically to one with no metadata key:\n%s\n---\n%s", got, want)
	}
}

func TestCanonicalizeRecipeYAMLV3_PreservesExplicitlyEmptyMetadata(t *testing.T) {
	in := []byte("metadata: {}\nfoo: bar\n")
	got, err := CanonicalizeRecipeYAMLV3(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(got), "metadata") {
		t.Errorf("expected an already-empty metadata mapping (no version removed) to survive, got %q", got)
	}
}

func TestCanonicalizeRecipeYAMLV3_NoopWithoutMetadataOrVersion(t *testing.T) {
	for _, in := range [][]byte{
		[]byte("foo: bar\n"),
		[]byte("metadata:\n  name: foo\nfoo: bar\n"),
	} {
		v1, err := CanonicalizeRecipeYAML(in)
		if err != nil {
			t.Fatalf("CanonicalizeRecipeYAML(%q) error = %v", in, err)
		}
		v3, err := CanonicalizeRecipeYAMLV3(in)
		if err != nil {
			t.Fatalf("CanonicalizeRecipeYAMLV3(%q) error = %v", in, err)
		}
		if string(v1) != string(v3) {
			t.Errorf("expected V3 to equal V1 when there is no metadata.version to strip: %q vs %q", v1, v3)
		}
	}
}

func TestSubjectDigestV3_IgnoresMetadataVersion(t *testing.T) {
	a := []byte("metadata:\n  version: 1.0.0\nfoo: bar\n")
	b := []byte("metadata:\n  version: 2.0.0\nfoo: bar\n")
	da, err := SubjectDigestV3(a)
	if err != nil {
		t.Fatalf("SubjectDigestV3(a) error = %v", err)
	}
	db, err := SubjectDigestV3(b)
	if err != nil {
		t.Fatalf("SubjectDigestV3(b) error = %v", err)
	}
	if da != db {
		t.Errorf("expected V3 digest to ignore metadata.version: %q vs %q", da, db)
	}

	// SubjectDigest (V1/V2) has no such carve-out. The same version change
	// still changes the digest.
	da1, _ := SubjectDigest(a)
	db1, _ := SubjectDigest(b)
	if da1 == db1 {
		t.Errorf("expected V1/V2 digest to remain sensitive to metadata.version, both %q", da1)
	}
}

func TestSubjectDigestForType_DispatchesOnPredicateType(t *testing.T) {
	in := []byte("metadata:\n  version: 1.0.0\nfoo: bar\n")

	v3, err := SubjectDigestForType(in, PredicateTypeV3)
	if err != nil {
		t.Fatalf("SubjectDigestForType(v3) error = %v", err)
	}
	if want, _ := SubjectDigestV3(in); v3 != want {
		t.Errorf("SubjectDigestForType(v3) = %q, want %q", v3, want)
	}

	for _, pt := range []string{PredicateTypeV1, PredicateTypeV2, "unknown"} {
		got, err := SubjectDigestForType(in, pt)
		if err != nil {
			t.Fatalf("SubjectDigestForType(%s) error = %v", pt, err)
		}
		if want, _ := SubjectDigest(in); got != want {
			t.Errorf("SubjectDigestForType(%s) = %q, want legacy digest %q", pt, got, want)
		}
	}
}
