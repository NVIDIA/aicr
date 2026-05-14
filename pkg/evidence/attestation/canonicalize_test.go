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
