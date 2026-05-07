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
	"encoding/json"
	"testing"
)

func TestBuildStatement(t *testing.T) {
	subject := AttestSubject{
		Name:   "checksums.txt",
		Digest: map[string]string{"sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		ResolvedDependencies: []Dependency{
			{
				URI:    "https://github.com/NVIDIA/aicr/releases/download/v1.0.0/aicr_linux_amd64",
				Digest: map[string]string{"sha256": "a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"},
			},
		},
	}

	metadata := StatementMetadata{
		Recipe:     "h100-eks-training",
		Components: []string{"gpu-operator", "network-operator"},
		BuilderID:  "jdoe@company.com",
	}

	data, err := BuildStatement(subject, metadata)
	if err != nil {
		t.Fatalf("BuildStatement() error: %v", err)
	}

	// Parse and validate the JSON structure
	var stmt map[string]any
	if err := json.Unmarshal(data, &stmt); err != nil {
		t.Fatalf("BuildStatement() produced invalid JSON: %v", err)
	}

	// Check in-toto statement type
	if got := stmt["_type"]; got != "https://in-toto.io/Statement/v1" {
		t.Errorf("_type = %v, want https://in-toto.io/Statement/v1", got)
	}

	// Check predicate type
	if got := stmt["predicateType"]; got != "https://slsa.dev/provenance/v1" {
		t.Errorf("predicateType = %v, want https://slsa.dev/provenance/v1", got)
	}

	// Check subject
	subjects, ok := stmt["subject"].([]any)
	if !ok || len(subjects) != 1 {
		t.Fatalf("subject should have 1 entry, got %v", stmt["subject"])
	}
	subj := subjects[0].(map[string]any)
	if subj["name"] != "checksums.txt" {
		t.Errorf("subject.name = %v, want checksums.txt", subj["name"])
	}

	// Check predicate has buildDefinition and runDetails
	predicate, predicateOK := stmt["predicate"].(map[string]any)
	if !predicateOK {
		t.Fatalf("predicate should be a map, got %T", stmt["predicate"])
	}
	if _, hasBuildDef := predicate["buildDefinition"]; !hasBuildDef {
		t.Error("predicate missing buildDefinition")
	}
	if _, hasRunDetails := predicate["runDetails"]; !hasRunDetails {
		t.Error("predicate missing runDetails")
	}

	// Check resolvedDependencies
	buildDef := predicate["buildDefinition"].(map[string]any)
	deps, ok := buildDef["resolvedDependencies"].([]any)
	if !ok || len(deps) != 1 {
		t.Fatalf("resolvedDependencies should have 1 entry, got %v", buildDef["resolvedDependencies"])
	}
}

func TestBuildStatement_EmptySubject(t *testing.T) {
	_, err := BuildStatement(AttestSubject{}, StatementMetadata{})
	if err == nil {
		t.Error("BuildStatement() with empty subject should return error")
	}
}

func TestBuildStatement_EmptyName(t *testing.T) {
	_, err := BuildStatement(AttestSubject{
		Name:   "",
		Digest: map[string]string{"sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	}, StatementMetadata{})
	if err == nil {
		t.Error("BuildStatement() with empty name should return error")
	}
}

func TestBuildStatement_WithToolVersion(t *testing.T) {
	subject := AttestSubject{
		Name:   "checksums.txt",
		Digest: map[string]string{"sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	}
	metadata := StatementMetadata{
		ToolVersion: "v1.2.3",
		Recipe:      "h100-eks-training",
		Components:  []string{"gpu-operator"},
	}

	data, err := BuildStatement(subject, metadata)
	if err != nil {
		t.Fatalf("BuildStatement() error: %v", err)
	}

	var stmt map[string]any
	if err := json.Unmarshal(data, &stmt); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// Navigate to internalParameters.toolVersion
	predicate := stmt["predicate"].(map[string]any)
	buildDef := predicate["buildDefinition"].(map[string]any)
	internalParams := buildDef["internalParameters"].(map[string]any)
	if got := internalParams["toolVersion"]; got != "v1.2.3" {
		t.Errorf("toolVersion = %v, want v1.2.3", got)
	}
}

func TestBuildStatement_NoDependencies(t *testing.T) {
	subject := AttestSubject{
		Name:   "checksums.txt",
		Digest: map[string]string{"sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	}

	data, err := BuildStatement(subject, StatementMetadata{})
	if err != nil {
		t.Fatalf("BuildStatement() error: %v", err)
	}

	var stmt map[string]any
	if err := json.Unmarshal(data, &stmt); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}

	// Should still have buildDefinition even with no deps
	predicate := stmt["predicate"].(map[string]any)
	if _, ok := predicate["buildDefinition"]; !ok {
		t.Error("missing buildDefinition")
	}
}

func TestBuildStatement_InvalidDigest(t *testing.T) {
	tests := []struct {
		name   string
		digest map[string]string
	}{
		{"empty sha256 value", map[string]string{"sha256": ""}},
		{"short sha256", map[string]string{"sha256": "abc123"}},
		{"long sha256", map[string]string{"sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855aa"}},
		{"empty other algo", map[string]string{"sha512": ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BuildStatement(AttestSubject{
				Name:   "checksums.txt",
				Digest: tt.digest,
			}, StatementMetadata{})
			if err == nil {
				t.Errorf("BuildStatement() with digest %v should return error", tt.digest)
			}
		})
	}
}
