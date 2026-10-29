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

package evidence

import (
	"bytes"
	"os"
	"strings"
	"testing"

	operrors "github.com/NVIDIA/aicr/pkg/operator/errors"
)

const (
	deployment  = "ghcr.io/nvidia/aicr-validators/deployment:v1.0.0@sha256:3c0a8553a346e6ba5a273a10f0b91574945b8b23cffc9d83db3107701e467790"
	conformance = "ghcr.io/nvidia/aicr-validators/conformance:v1.0.0@sha256:e091d51f600255c2166e5c1b42afd556880d498f2a9bb913a32da38fa28e038b"
	performance = "ghcr.io/nvidia/aicr-validators/performance:v1.0.0@sha256:96af7b9e4362917e081d7152efd063d10ba6a213825aae44a4b6fd826da4c4fe"
	evidenceDig = "sha256:2bd564d8ccf43108a775f66594a5972bff20a9ebed8a1f2d6b1a0a3e5c2a4b6f"
)

func pins() map[string]string {
	return map[string]string{Repository(deployment): deployment, Repository(conformance): conformance, Repository(performance): performance}
}

// The statement AICR v1.0.0 pushed from a kind run whose deployment checks
// all failed: validator images carry tags and empty digests.
func readStatement(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/v1.0.0/statement.intoto.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseAICRStatement(t *testing.T) {
	t.Parallel()
	s, err := ParseAICRStatement(readStatement(t))
	if err != nil {
		t.Fatalf("ParseAICRStatement() = %v", err)
	}
	if s.PredicateType != "https://aicr.run/recipe-evidence/v3" || s.AICRVersion != "1.0.0" || s.RecipeName != "h100-eks-ubuntu-training-kubeflow" ||
		len(s.ValidatorImages) != 3 || s.Phases["deployment"].Failed != 4 || s.Passed() {

		t.Fatalf("summary = %+v", s)
	}
	if err = CheckValidators(s.ValidatorImages, pins()); err != nil {
		t.Fatalf("CheckValidators() = %v", err)
	}
	s.Phases["deployment"] = PhaseResult{Passed: 4, Skipped: 2}
	if !s.Passed() {
		t.Error("all checks passing is not Passed")
	}
	s.Phases["conformance"] = PhaseResult{Skipped: 3}
	if s.Passed() {
		t.Error("a phase with nothing passing is Passed")
	}
	for name, data := range map[string]string{
		"not JSON":        "{",
		"other type":      `{"_type":"https://in-toto.io/Statement/v0.1","predicateType":"https://aicr.run/recipe-evidence/v3"}`,
		"other predicate": `{"_type":"https://in-toto.io/Statement/v1","predicateType":"https://slsa.dev/provenance/v1"}`,
	} {
		if _, err := ParseAICRStatement([]byte(data)); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
			t.Errorf("%s: ParseAICRStatement() = %v", name, err)
		}
	}
}

// A validator AICR names must be pinned, at the pinned tag, and at the
// pinned digest when AICR records one.
func TestCheckValidators(t *testing.T) {
	t.Parallel()
	tests := map[string]ValidatorImage{
		"unpinned repository": {Image: "registry.example.com/mirror/aicr-validators/deployment:v1.0.0"},
		"other tag":           {Image: "ghcr.io/nvidia/aicr-validators/deployment:latest"},
		"no tag":              {Image: "ghcr.io/nvidia/aicr-validators/deployment"},
		"other digest":        {Image: "ghcr.io/nvidia/aicr-validators/deployment:v1.0.0", Digest: evidenceDig},
		"other digest in ref": {Image: "ghcr.io/nvidia/aicr-validators/deployment:v1.0.0@" + evidenceDig},
	}
	for name, image := range tests {
		err := CheckValidators([]ValidatorImage{image}, pins())
		if operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest || !strings.Contains(err.Error(), "validator images differ") {
			t.Errorf("%s: CheckValidators() = %v", name, err)
		}
	}
	ok := []ValidatorImage{
		{Image: "ghcr.io/nvidia/aicr-validators/deployment:v1.0.0"},
		{Image: "ghcr.io/nvidia/aicr-validators/conformance:v1.0.0", Digest: "sha256:e091d51f600255c2166e5c1b42afd556880d498f2a9bb913a32da38fa28e038b"},
		{Image: performance},
	}
	if err := CheckValidators(ok, pins()); err != nil {
		t.Errorf("CheckValidators() = %v", err)
	}
	if err := CheckValidators(nil, pins()); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Errorf("evidence naming no validator: %v", err)
	}
}

func TestSplitRef(t *testing.T) {
	t.Parallel()
	for ref, want := range map[string][3]string{
		deployment: {"ghcr.io/nvidia/aicr-validators/deployment", "v1.0.0", "sha256:3c0a8553a346e6ba5a273a10f0b91574945b8b23cffc9d83db3107701e467790"},
		"localhost:5001/aicr-validators/deployment": {"localhost:5001/aicr-validators/deployment", "", ""},
		"localhost:5001/x:v1":                       {"localhost:5001/x", "v1", ""},
		"x@" + evidenceDig:                          {"x", "", evidenceDig},
	} {
		repo, tag, dgst := splitRef(ref)
		if [3]string{repo, tag, dgst} != want {
			t.Errorf("splitRef(%s) = %s %s %s", ref, repo, tag, dgst)
		}
	}
}

// The binding is deterministic, its subject is AICR's evidence manifest, and
// it reads back with every bound field.
func TestNewBinding(t *testing.T) {
	t.Parallel()
	s, err := ParseAICRStatement(readStatement(t))
	if err != nil {
		t.Fatal(err)
	}
	p := Predicate{
		Qualifying: true, RuntimeUID: "uid", Rollout: "rollout-0123456789abcdef-1", RunID: "0123456789ab",
		PlanDigest: "sha256:plan", InputDigest: "sha256:input", SnapshotDigest: "sha256:snap",
		Criteria:        map[string]string{"accelerator": "h100"},
		Charts:          []Chart{{Release: "kai-scheduler", Digest: "sha256:b"}, {Release: "gpu-operator", Digest: "sha256:a"}},
		Operator:        Image{Image: "ghcr.io/nvidia/aicr-operator:v0.1.0", Digest: "sha256:op"},
		AICR:            Image{Image: "ghcr.io/nvidia/aicr:v1.0.0@sha256:aicr", Version: "v1.0.0"},
		ValidatorImages: []ValidatorImage{{Image: deployment}},
		Approval:        Approval{State: "Approved", ApprovedBy: "alice", Reason: "window"},
	}
	ref := "registry.test/aicr/cluster/evidence/cluster@" + evidenceDig
	first, err := NewBinding(ref, evidenceDig, s, p)
	if err != nil {
		t.Fatalf("NewBinding() = %v", err)
	}
	again, err := NewBinding(ref, evidenceDig, s, p)
	if err != nil || !bytes.Equal(first, again) {
		t.Fatalf("NewBinding() is not deterministic: %v", err)
	}
	b, err := ParseBinding(first)
	if err != nil {
		t.Fatalf("ParseBinding() = %v", err)
	}
	got := b.Predicate
	if b.Subject[0].Name != ref || b.Subject[0].Digest["sha256"] != strings.TrimPrefix(evidenceDig, "sha256:") ||
		got.Result != "Failed" || got.PlanDigest != "sha256:plan" || got.Operator.Digest != "sha256:op" ||
		got.AICR.Image != p.AICR.Image || got.Approval.ApprovedBy != "alice" || got.Charts[0].Release != "gpu-operator" ||
		got.AICREvidence.Phases["deployment"].Failed != 4 || got.AICREvidence.RecipeName != s.RecipeName || !got.Qualifying {

		t.Fatalf("binding = %s", first)
	}
	if _, err = NewBinding(ref, "sha256:short", s, p); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Errorf("short evidence digest: %v", err)
	}
	if _, err = ParseBinding(readStatement(t)); operrors.CodeOf(err) != operrors.ErrCodeInvalidRequest {
		t.Errorf("AICR's statement parsed as a binding: %v", err)
	}
}
