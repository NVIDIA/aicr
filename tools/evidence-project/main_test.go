// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NVIDIA/aicr/pkg/evidence/attestation"
	"github.com/NVIDIA/aicr/pkg/evidence/verifier"
	"github.com/NVIDIA/aicr/pkg/fingerprint"
)

func TestCheckTrustedRegistry(t *testing.T) {
	trusted := []string{"ghcr.io/nvidia", "ghcr.io/nvidia/aicr-evidence"}
	tests := []struct {
		name    string
		ref     string
		trusted []string
		wantErr bool
	}{
		{"exact repo match", "ghcr.io/nvidia/aicr-evidence@sha256:abc", trusted, false},
		{"prefix segment match", "ghcr.io/nvidia/aicr-evidence/h100@sha256:abc", trusted, false},
		{"tagged ref under prefix", "ghcr.io/nvidia/aicr-evidence:h100-eks", trusted, false},
		{"sibling not under prefix", "ghcr.io/nvidia-evil/aicr-evidence@sha256:abc", trusted, true},
		{"foreign registry", "quay.io/someone/aicr-evidence@sha256:abc", trusted, true},
		{"empty allowlist rejects all", "ghcr.io/nvidia/aicr-evidence@sha256:abc", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkTrustedRegistry(tt.ref, tt.trusted)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkTrustedRegistry(%q) err = %v, wantErr %v", tt.ref, err, tt.wantErr)
			}
		})
	}
}

func TestParseTrusted(t *testing.T) {
	got := parseTrusted("  ghcr.io/nvidia , , ghcr.io/x/y  ,")
	want := []string{"ghcr.io/nvidia", "ghcr.io/x/y"}
	if len(got) != len(want) {
		t.Fatalf("parseTrusted = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("parseTrusted[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolveRef(t *testing.T) {
	dir := t.TempDir()

	// OCI form strips the oci:// scheme.
	if ref, err := resolveRef(verifier.InputFormOCI, "oci://ghcr.io/x/y@sha256:abc", "", nil); err != nil || ref != "ghcr.io/x/y@sha256:abc" {
		t.Errorf("OCI resolveRef = %q, %v", ref, err)
	}
	// Directory form has no remote ref.
	if ref, err := resolveRef(verifier.InputFormDir, dir, "", nil); err != nil || ref != "" {
		t.Errorf("dir resolveRef = %q, %v", ref, err)
	}
	// -bundle override wins for a pointer (preloaded pointer is ignored).
	if ref, err := resolveRef(verifier.InputFormPointer, "", "ghcr.io/x/y@sha256:abc", nil); err != nil || ref != "ghcr.io/x/y@sha256:abc" {
		t.Errorf("pointer override resolveRef = %q, %v", ref, err)
	}

	// Preloaded pointer supplies bundle.oci.
	pointer := filepath.Join(dir, "p.yaml")
	if err := os.WriteFile(pointer, []byte(`schemaVersion: "1.0.0"
recipe: h100-eks-ubuntu-training
attestations:
  - attestedAt: 2026-06-23T18:24:27Z
    bundle:
      digest: sha256:8274b6a1da24aa9782dc12162bf6a38265c30a852585ca64cfad5718efbbdec3
      oci: ghcr.io/nvidia/aicr-evidence:h100-eks-ubuntu-training-abc
      predicateType: https://aicr.nvidia.com/recipe-evidence/v1
    signer:
      identity: ci@example.com
      issuer: https://token.actions.githubusercontent.com
`), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := verifier.LoadAndValidatePointer(pointer)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := resolveRef(verifier.InputFormPointer, "", "", p)
	if err != nil || ref != "ghcr.io/nvidia/aicr-evidence:h100-eks-ubuntu-training-abc" {
		t.Errorf("pointer resolveRef = %q, %v", ref, err)
	}
}

// TestRun_ArgValidation exercises the fail-closed argument checks that
// must hold before any network pull: required flags and required pins.
func TestRun_ArgValidation(t *testing.T) {
	tests := []struct {
		name string
		o    options
	}{
		{"missing in/out", options{issuer: "i", identityRE: "r"}},
		{"missing issuer pin", options{in: "x", out: "y", identityRE: "r"}},
		{"missing identity pin", options{in: "x", out: "y", issuer: "i"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := run(context.Background(), tt.o, &buf); err == nil {
				t.Errorf("run(%s) = nil error, want rejection", tt.name)
			}
		})
	}
}

// TestRun_UntrustedRegistryRejected confirms an OCI input whose registry
// is not in the trusted list fails before any pull is attempted.
func TestRun_UntrustedRegistryRejected(t *testing.T) {
	o := options{
		in:         "oci://quay.io/stranger/aicr-evidence@sha256:abc",
		out:        t.TempDir(),
		issuer:     "https://token.actions.githubusercontent.com",
		identityRE: "^https://github.com/NVIDIA/aicr/",
		trusted:    "ghcr.io/nvidia",
	}
	var buf bytes.Buffer
	if err := run(context.Background(), o, &buf); err == nil {
		t.Error("run() with untrusted registry = nil error, want rejection")
	}
}

// TestRun_NoTrustedRegistryRejected confirms a remote input with no
// trusted-registry configured fails closed (unless --allow-unpinned-tag).
func TestRun_NoTrustedRegistryRejected(t *testing.T) {
	o := options{
		in:         "oci://ghcr.io/nvidia/aicr-evidence@sha256:abc",
		out:        t.TempDir(),
		issuer:     "https://token.actions.githubusercontent.com",
		identityRE: "^https://github.com/NVIDIA/aicr/",
	}
	var buf bytes.Buffer
	if err := run(context.Background(), o, &buf); err == nil {
		t.Error("run() with no trusted registry = nil error, want rejection")
	}
}

// TestRun_AllowUnpinnedStillGatesPinnedRef confirms --allow-unpinned-tag
// only relaxes the unpinned-tag restriction: a digest-pinned oci:// ref
// whose registry is not on the trusted allowlist is still rejected before
// any pull, even with the flag set.
func TestRun_AllowUnpinnedStillGatesPinnedRef(t *testing.T) {
	o := options{
		in:            "oci://quay.io/stranger/aicr-evidence@sha256:abc",
		out:           t.TempDir(),
		issuer:        "https://token.actions.githubusercontent.com",
		identityRE:    "^https://github.com/NVIDIA/aicr/",
		trusted:       "ghcr.io/nvidia",
		allowUnpinned: true,
	}
	var buf bytes.Buffer
	if err := run(context.Background(), o, &buf); err == nil {
		t.Error("run() with pinned untrusted ref + allow-unpinned = nil error, want rejection")
	}
}

// TestRun_UnsignedBundleRejected drives a directory input all the way
// through materialize + verify: a bundle with the marker files but no
// signature fails verification, and run() refuses to ingest it. This is
// the fail-closed guarantee — unverified evidence never reaches the tree.
func TestRun_UnsignedBundleRejected(t *testing.T) {
	bundle := t.TempDir()
	for _, f := range []string{"recipe.yaml", "manifest.json"} {
		if err := os.WriteFile(filepath.Join(bundle, f), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	o := options{
		in:         bundle, // directory input: no remote ref, registry gate skipped
		out:        t.TempDir(),
		issuer:     "https://token.actions.githubusercontent.com",
		identityRE: "^https://github.com/NVIDIA/aicr/",
	}
	var buf bytes.Buffer
	if err := run(context.Background(), o, &buf); err == nil {
		t.Error("run() with unsigned bundle = nil error, want rejection")
	}
	// Nothing should have been written to the output tree.
	if entries, _ := os.ReadDir(filepath.Join(o.out, "results")); len(entries) != 0 {
		t.Errorf("unverified bundle produced %d output entries, want 0", len(entries))
	}
}

// verifiedBundleDir writes a minimal bundle directory (recipe.yaml) and
// returns it; synthesizeVerified reads recipe.yaml for the coordinate.
func verifiedBundleDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "recipe.yaml"), []byte(`criteria:
  service: eks
  accelerator: h100
  os: ubuntu
  intent: training
constraints:
  - name: K8s.server.version
    value: ">= 1.32.4"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func fakeVerifyResult() *verifier.VerifyResult {
	idx := int64(42)
	return &verifier.VerifyResult{
		Exit: verifier.ExitValidPassed,
		Signer: &verifier.SignerClaims{
			Identity:      "https://github.com/NVIDIA/aicr/.github/workflows/uat-aws.yaml@refs/heads/main",
			Issuer:        "https://token.actions.githubusercontent.com",
			RekorLogIndex: &idx,
		},
		Predicate: &attestation.Predicate{
			AttestedAt:  time.Date(2026, 6, 25, 5, 57, 23, 0, time.UTC),
			AICRVersion: "dev",
			Recipe:      attestation.RecipeRef{Name: "h100-eks-ubuntu-training"},
			Fingerprint: fingerprint.Fingerprint{K8sVersion: fingerprint.Dimension{Value: "1.35.5"}},
			Manifest:    attestation.ManifestRef{Digest: "sha256:abc"},
		},
	}
}

func TestSynthesizeVerified_HappyPath(t *testing.T) {
	out := t.TempDir()
	var buf bytes.Buffer
	err := synthesizeVerified(context.Background(), fakeVerifyResult(), verifiedBundleDir(t),
		nil, "ghcr.io/nvidia/aicr-evidence@sha256:abc", "", out, &buf)
	if err != nil {
		t.Fatalf("synthesizeVerified: %v", err)
	}
	// First-party heuristic classifies NVIDIA/aicr as first-party.
	if !bytes.Contains(buf.Bytes(), []byte("first-party")) {
		t.Errorf("output missing first-party classification: %q", buf.String())
	}
	want := filepath.Join(out, "results", "eks", "h100-ubuntu", "training")
	if entries, _ := os.ReadDir(want); len(entries) != 1 {
		t.Errorf("expected 1 idHash dir under %s, got %d", want, len(entries))
	}
}

func TestSynthesizeVerified_Rejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*verifier.VerifyResult)
	}{
		{"invalid exit", func(vr *verifier.VerifyResult) { vr.Exit = verifier.ExitInvalid }},
		{"nil signer", func(vr *verifier.VerifyResult) { vr.Signer = nil }},
		{"empty signer identity", func(vr *verifier.VerifyResult) { vr.Signer.Identity = "" }},
		{"nil predicate", func(vr *verifier.VerifyResult) { vr.Predicate = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vr := fakeVerifyResult()
			tt.mutate(vr)
			var buf bytes.Buffer
			err := synthesizeVerified(context.Background(), vr, verifiedBundleDir(t),
				nil, "", "", t.TempDir(), &buf)
			if err == nil {
				t.Errorf("synthesizeVerified(%s) = nil error, want rejection", tt.name)
			}
		})
	}
}
