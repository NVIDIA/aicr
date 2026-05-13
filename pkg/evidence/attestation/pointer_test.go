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
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/NVIDIA/aicr/pkg/fingerprint"
)

func ptrInt64(v int64) *int64 { return &v }

func TestBuildPointer_RequiresBundle(t *testing.T) {
	if _, err := BuildPointer(PointerInputs{}); err == nil {
		t.Errorf("expected error when bundle is nil")
	}
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
