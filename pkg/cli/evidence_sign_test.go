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

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
)

func TestEvidenceSignCmd_HasRelocateFlag(t *testing.T) {
	cmd := evidenceSignCmd()
	found := false
	for _, f := range cmd.Flags {
		if f.Names()[0] == flagRelocate {
			found = true
		}
	}
	if !found {
		t.Errorf("missing expected flag: --%s", flagRelocate)
	}
}

// signedFlatPointerYAML is a valid, already-signed single-attestation pointer
// for the recipe, as committed flat at recipes/evidence/<recipe>.yaml before
// relocation.
func signedFlatPointerYAML(recipe string) string {
	return "schemaVersion: 1.0.0\n" +
		"recipe: " + recipe + "\n" +
		"attestations:\n  - bundle:\n      oci: ghcr.io/yuanchen8911/aicr-evidence:x\n" +
		"      digest: sha256:33d4cf36\n" +
		"      predicateType: " + attestation.PredicateTypeV1 + "\n" +
		"    signer:\n      identity: yuanchen97@gmail.com\n" +
		"      issuer: https://github.com/login/oauth\n" +
		"    attestedAt: 2026-06-23T18:24:27Z\n"
}

// TestEvidenceSignCmd_RelocateOnlyForAlreadySigned exercises the idempotent
// recovery path: `aicr evidence sign --relocate` on a pointer that is already
// signed must move it to its canonical per-source path without re-signing (no
// network, no Fulcio), so a prior run that signed but failed to relocate can
// be re-driven cleanly.
func TestEvidenceSignCmd_RelocateOnlyForAlreadySigned(t *testing.T) {
	const recipe = "h100-gke-cos-training"
	root := t.TempDir()
	flat := filepath.Join(root, recipe+".yaml")
	if err := os.WriteFile(flat, []byte(signedFlatPointerYAML(recipe)), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.Writer = &out
	cmd.ErrWriter = &out
	if err := cmd.Run(context.Background(),
		[]string{"aicr", "evidence", "sign", flat, "--relocate", "--yes"}); err != nil {
		t.Fatalf("evidence sign --relocate (already signed): %v", err)
	}

	// 7c4c0edc8c765a95a0f3afdb3bbb8e91 is SourceSlug(issuer, identity).
	want := filepath.Join(root, recipe, "7c4c0edc8c765a95a0f3afdb3bbb8e91", "sha256-33d4cf36.yaml")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("pointer not relocated to canonical path %q: %v", want, err)
	}
	if _, err := os.Stat(flat); !os.IsNotExist(err) {
		t.Errorf("flat pending pointer should be gone, stat err = %v", err)
	}
}

// TestEvidenceSignCmd_RejectsAlreadySignedWithoutRelocate confirms the guard
// is unchanged without --relocate: an already-signed pointer is never
// re-signed (it fails closed before any network work).
func TestEvidenceSignCmd_RejectsAlreadySignedWithoutRelocate(t *testing.T) {
	const recipe = "h100-gke-cos-training"
	flat := filepath.Join(t.TempDir(), recipe+".yaml")
	if err := os.WriteFile(flat, []byte(signedFlatPointerYAML(recipe)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.Writer = &out
	cmd.ErrWriter = &out
	if err := cmd.Run(context.Background(),
		[]string{"aicr", "evidence", "sign", flat, "--yes"}); err == nil {
		t.Fatal("expected an error signing an already-signed pointer without --relocate")
	}
}
