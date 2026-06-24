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
	stderrors "errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	bundleattest "github.com/NVIDIA/aicr/pkg/bundler/attestation"
	"github.com/NVIDIA/aicr/pkg/errors"
)

func signableSignPointer() *Pointer {
	return &Pointer{
		SchemaVersion: PointerSchemaVersion,
		Recipe:        "h100-eks-ubuntu-training",
		Attestations: []PointerAttestation{{
			Bundle: PointerBundle{OCI: "ghcr.io/owner/aicr-evidence:tag", Digest: "sha256:abc", PredicateType: PredicateTypeV1},
		}},
	}
}

func TestValidateSignablePointer(t *testing.T) {
	signed := signableSignPointer()
	signed.Attestations[0].Signer = &PointerSigner{Identity: "u@x", Issuer: "iss"}
	noBundle := signableSignPointer()
	noBundle.Attestations[0].Bundle.OCI = ""
	noDigest := signableSignPointer()
	noDigest.Attestations[0].Bundle.Digest = ""
	twoAtts := signableSignPointer()
	twoAtts.Attestations = append(twoAtts.Attestations, twoAtts.Attestations[0])

	tests := []struct {
		name     string
		pointer  *Pointer
		wantErr  bool
		wantCode errors.ErrorCode
	}{
		{"valid unsigned", signableSignPointer(), false, ""},
		{"nil pointer", nil, true, errors.ErrCodeInvalidRequest},
		{"already signed", signed, true, errors.ErrCodeConflict},
		{"no bundle.oci", noBundle, true, errors.ErrCodeInvalidRequest},
		{"no bundle.digest", noDigest, true, errors.ErrCodeInvalidRequest},
		{"multiple attestations", twoAtts, true, errors.ErrCodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateSignablePointer(tt.pointer)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !stderrors.Is(err, errors.New(tt.wantCode, "")) {
				t.Errorf("error code = %v, want %v", err, tt.wantCode)
			}
		})
	}
}

func TestSignExisting_RejectsBadPointer(t *testing.T) {
	desc := MainArtifactDescriptor{Digest: "sha256:abc", MediaType: "application/json", Size: 1}
	noOCI := signableSignPointer()
	noOCI.Attestations[0].Bundle.OCI = ""
	noDigest := signableSignPointer()
	noDigest.Attestations[0].Bundle.Digest = ""
	alreadySigned := signableSignPointer()
	alreadySigned.Attestations[0].Signer = &PointerSigner{Identity: "u@x", Issuer: "iss"}
	multiAtt := signableSignPointer()
	multiAtt.Attestations = append(multiAtt.Attestations, multiAtt.Attestations[0])

	tests := []struct {
		name     string
		pointer  *Pointer
		path     string
		wantCode errors.ErrorCode
	}{
		{"nil pointer", nil, "p.yaml", errors.ErrCodeInvalidRequest},
		{"no attestations", &Pointer{}, "p.yaml", errors.ErrCodeInvalidRequest},
		{"empty path", signableSignPointer(), "", errors.ErrCodeInvalidRequest},
		{"empty bundle.oci", noOCI, "p.yaml", errors.ErrCodeInvalidRequest},
		{"empty bundle.digest", noDigest, "p.yaml", errors.ErrCodeInvalidRequest},
		{"already signed", alreadySigned, "p.yaml", errors.ErrCodeConflict},
		{"multiple attestations", multiAtt, "p.yaml", errors.ErrCodeInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := SignExisting(context.Background(), SignExistingOptions{
				Pointer: tt.pointer, PointerPath: tt.path, Artifact: desc,
			})
			if err == nil {
				t.Fatalf("expected error for %s", tt.name)
			}
			if !stderrors.Is(err, errors.New(tt.wantCode, "")) {
				t.Errorf("error code = %v, want %v", err, tt.wantCode)
			}
		})
	}
}

func TestSignExisting_RejectsIncompleteDescriptor(t *testing.T) {
	tests := []struct {
		name string
		desc MainArtifactDescriptor
	}{
		{"no digest", MainArtifactDescriptor{MediaType: "application/json", Size: 1}},
		{"no mediaType", MainArtifactDescriptor{Digest: "sha256:abc", Size: 1}},
		{"no size", MainArtifactDescriptor{Digest: "sha256:abc", MediaType: "application/json"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := SignExisting(context.Background(), SignExistingOptions{
				Pointer:     signableSignPointer(),
				PointerPath: "p.yaml",
				Artifact:    tt.desc,
			})
			if err == nil {
				t.Fatalf("expected error for incomplete descriptor %+v", tt.desc)
			}
			if !stderrors.Is(err, errors.New(errors.ErrCodeInvalidRequest, "")) {
				t.Errorf("error code = %v, want ErrCodeInvalidRequest", err)
			}
		})
	}
}

func TestPointerSignerFromSignature(t *testing.T) {
	if PointerSignerFromSignature(nil) != nil {
		t.Errorf("nil signature should yield nil signer")
	}

	withRekor := PointerSignerFromSignature(&bundleattest.SignedAttestation{
		Identity: "ci@github", Issuer: "https://token.actions.githubusercontent.com", RekorLogIndex: 42,
	})
	if withRekor == nil || withRekor.RekorLogIndex == nil || *withRekor.RekorLogIndex != 42 {
		t.Fatalf("expected rekor index 42; got %+v", withRekor)
	}
	if withRekor.Identity != "ci@github" {
		t.Errorf("identity mismatch: %+v", withRekor)
	}

	zeroRekor := PointerSignerFromSignature(&bundleattest.SignedAttestation{Identity: "u@x", RekorLogIndex: 0})
	if zeroRekor.RekorLogIndex != nil {
		t.Errorf("zero rekor index should map to nil; got *%d", *zeroRekor.RekorLogIndex)
	}
}

func TestWritePointerFile_WritesExactPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "h100-eks-ubuntu-training.yaml")
	p := &Pointer{
		SchemaVersion: PointerSchemaVersion,
		Recipe:        "h100-eks-ubuntu-training",
		Attestations: []PointerAttestation{{
			Bundle:     PointerBundle{OCI: "ghcr.io/x/aicr-evidence:tag", Digest: "sha256:abc", PredicateType: PredicateTypeV1},
			AttestedAt: time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC),
		}},
	}
	got, err := WritePointerFile(target, p)
	if err != nil {
		t.Fatalf("WritePointerFile: %v", err)
	}
	if got != target {
		t.Errorf("returned path = %q, want %q", got, target)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("pointer file not written at exact path: %v", err)
	}
}
