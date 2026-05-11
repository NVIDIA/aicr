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
	p, err := BuildPointer(PointerInputs{
		Bundle:     bundle,
		BundleOCI:  "ghcr.io/foo/aicr-evidence:abc",
		BundleHash: "sha256:abc",
		Signer: PointerSigner{
			Identity:      "test@example.com",
			Issuer:        "https://oauth.example.com",
			RekorLogIndex: 42,
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
	if att.Signer.RekorLogIndex != 42 {
		t.Errorf("RekorLogIndex = %d", att.Signer.RekorLogIndex)
	}
	if att.Fingerprint.Accelerator != "h100" {
		t.Errorf("denormalized accelerator missing: %q", att.Fingerprint.Accelerator)
	}
	if !att.CriteriaMatch.Matched {
		t.Errorf("expected matched=true denormalized")
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
				Signer:        PointerSigner{Identity: "u@x", Issuer: "iss", RekorLogIndex: 7},
				AttestedAt:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
				CriteriaMatch: PointerCriteriaMatch{Matched: true},
				PhaseSummary: map[Phase]PointerPhaseStat{
					PhaseDeployment: {Passed: 1},
				},
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

func TestPointerLogsBundle_PopulatesWhenSet(t *testing.T) {
	bundle := &Bundle{
		RecipeName: "x",
		Predicate: &Predicate{
			AttestedAt:    time.Now(),
			CriteriaMatch: fingerprint.MatchResult{Matched: true},
		},
	}
	p, err := BuildPointer(PointerInputs{
		Bundle: bundle,
		LogsBundle: &PointerLogsBundle{
			OCI:    "ghcr.io/x/y-logs:1",
			Digest: "sha256:def",
		},
	})
	if err != nil {
		t.Fatalf("BuildPointer: %v", err)
	}
	att := p.Attestations[0]
	if att.LogsBundle == nil || att.LogsBundle.OCI != "ghcr.io/x/y-logs:1" {
		t.Errorf("LogsBundle pointer field not populated correctly; got %+v", att.LogsBundle)
	}
}

func TestPointer_FullFingerprintRoundTrip(t *testing.T) {
	bundle := &Bundle{
		RecipeName: "x",
		Predicate: &Predicate{
			AttestedAt:    time.Now(),
			CriteriaMatch: fingerprint.MatchResult{Matched: true},
			Fingerprint: fingerprint.Fingerprint{
				Service:     fingerprint.Dimension{Value: "eks"},
				Accelerator: fingerprint.Dimension{Value: "h100"},
				OS:          fingerprint.OSDimension{Value: "ubuntu", Version: "22.04"},
				K8sVersion:  fingerprint.Dimension{Value: "1.33.4"},
				Region:      fingerprint.Dimension{Value: "us-west-2"},
			},
		},
	}
	p, err := BuildPointer(PointerInputs{Bundle: bundle})
	if err != nil {
		t.Fatalf("BuildPointer: %v", err)
	}
	fp := p.Attestations[0].Fingerprint
	if fp.Service != "eks" || fp.Accelerator != "h100" || fp.OS != "ubuntu" ||
		fp.K8sVersion != "1.33.4" || fp.Region != "us-west-2" {

		t.Errorf("fingerprint denormalization mismatch: %+v", fp)
	}
}
