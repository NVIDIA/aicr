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
	"maps"
	"strings"
	"testing"
	"time"

	provenance "github.com/in-toto/attestation/go/predicates/provenance/v1"
	intoto "github.com/in-toto/attestation/go/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/NVIDIA/aicr/pkg/errors"
	"github.com/NVIDIA/aicr/pkg/errors/errorstest"
)

// decodedStatement holds the statement fields these tests assert on.
// StartedOn is nil when the key is absent.
type decodedStatement struct {
	Type          string `json:"_type"`
	PredicateType string `json:"predicateType"`
	Subject       []struct {
		Name   string            `json:"name"`
		Digest map[string]string `json:"digest"`
	} `json:"subject"`
	Predicate struct {
		BuildDefinition *struct {
			InternalParameters struct {
				ToolVersion string `json:"toolVersion"`
			} `json:"internalParameters"`
			ResolvedDependencies []struct {
				URI string `json:"uri"`
			} `json:"resolvedDependencies"`
		} `json:"buildDefinition"`
		RunDetails *struct {
			Builder struct {
				ID string `json:"id"`
			} `json:"builder"`
			Metadata struct {
				InvocationID string  `json:"invocationId"`
				StartedOn    *string `json:"startedOn"`
			} `json:"metadata"`
		} `json:"runDetails"`
	} `json:"predicate"`
}

// decodeStatement parses statement JSON into a decodedStatement. It fails
// the test when the JSON does not match the in-toto Statement and SLSA v1
// provenance schemas or fails their Validate checks, such as an empty
// builder.id.
func decodeStatement(t *testing.T, data []byte) decodedStatement {
	t.Helper()
	var schema intoto.Statement
	if err := protojson.Unmarshal(data, &schema); err != nil {
		t.Fatalf("statement is not an in-toto Statement: %v", err)
	}
	if err := schema.Validate(); err != nil {
		t.Fatalf("invalid in-toto Statement: %v", err)
	}
	predicateJSON, err := protojson.Marshal(schema.GetPredicate())
	if err != nil {
		t.Fatalf("marshal predicate: %v", err)
	}
	var pred provenance.Provenance
	if err := protojson.Unmarshal(predicateJSON, &pred); err != nil {
		t.Fatalf("predicate is not SLSA v1 provenance: %v", err)
	}
	if err := pred.Validate(); err != nil {
		t.Fatalf("invalid SLSA v1 provenance: %v", err)
	}

	var stmt decodedStatement
	if err := json.Unmarshal(data, &stmt); err != nil {
		t.Fatalf("invalid statement JSON: %v", err)
	}
	if stmt.Predicate.BuildDefinition == nil {
		t.Fatal("predicate missing buildDefinition")
	}
	if stmt.Predicate.RunDetails == nil {
		t.Fatal("predicate missing runDetails")
	}
	return stmt
}

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

	stmt := decodeStatement(t, data)
	if stmt.Type != "https://in-toto.io/Statement/v1" {
		t.Errorf("_type = %q, want https://in-toto.io/Statement/v1", stmt.Type)
	}
	if stmt.PredicateType != "https://slsa.dev/provenance/v1" {
		t.Errorf("predicateType = %q, want https://slsa.dev/provenance/v1", stmt.PredicateType)
	}
	if len(stmt.Subject) != 1 {
		t.Fatalf("subject has %d entries, want 1", len(stmt.Subject))
	}
	if stmt.Subject[0].Name != "checksums.txt" {
		t.Errorf("subject.name = %q, want checksums.txt", stmt.Subject[0].Name)
	}
	if got := stmt.Subject[0].Digest; !maps.Equal(got, subject.Digest) {
		t.Errorf("subject.digest = %v, want %v", got, subject.Digest)
	}
	if got := len(stmt.Predicate.BuildDefinition.ResolvedDependencies); got != 1 {
		t.Errorf("resolvedDependencies has %d entries, want 1", got)
	}
}

func TestBuildStatement_InvalidInput(t *testing.T) {
	tests := []struct {
		name     string
		subject  AttestSubject
		metadata StatementMetadata
	}{
		{"empty subject", AttestSubject{}, StatementMetadata{BuilderID: "jdoe@company.com"}},
		{"empty builder ID", keylessTestSubject, StatementMetadata{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := BuildStatement(tt.subject, tt.metadata)
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		})
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
		BuilderID:   "jdoe@company.com",
	}

	data, err := BuildStatement(subject, metadata)
	if err != nil {
		t.Fatalf("BuildStatement() error: %v", err)
	}

	stmt := decodeStatement(t, data)
	if got := stmt.Predicate.BuildDefinition.InternalParameters.ToolVersion; got != "v1.2.3" {
		t.Errorf("toolVersion = %q, want v1.2.3", got)
	}
}

func TestBuildStatement_NoDependencies(t *testing.T) {
	subject := AttestSubject{
		Name:   "checksums.txt",
		Digest: map[string]string{"sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	}

	data, err := BuildStatement(subject, StatementMetadata{BuilderID: "jdoe@company.com"})
	if err != nil {
		t.Fatalf("BuildStatement() error: %v", err)
	}

	decodeStatement(t, data)
}

// TestBuildStatement_Deterministic verifies that two runs of BuildStatement
// against identical inputs with Deterministic=true produce byte-identical
// statements. Required for SLSA-reproducible builds.
func TestBuildStatement_Deterministic(t *testing.T) {
	subject := AttestSubject{
		Name:   "checksums.txt",
		Digest: map[string]string{"sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	}
	metadata := StatementMetadata{
		Recipe:        "h100-eks-training",
		Components:    []string{"gpu-operator"},
		BuilderID:     "jdoe@company.com",
		ToolVersion:   "v1.2.3",
		Deterministic: true,
	}

	first, err := BuildStatement(subject, metadata)
	if err != nil {
		t.Fatalf("BuildStatement first run: %v", err)
	}
	second, err := BuildStatement(subject, metadata)
	if err != nil {
		t.Fatalf("BuildStatement second run: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("deterministic mode produced different statements:\nfirst:  %s\nsecond: %s", first, second)
	}

	md := decodeStatement(t, first).Predicate.RunDetails.Metadata
	if md.StartedOn != nil {
		t.Errorf("startedOn should be omitted in deterministic mode, got %q", *md.StartedOn)
	}
	if md.InvocationID == "" {
		t.Error("invocationId should be derived, got empty")
	}
}

// TestBuildStatement_InvocationIDOverride verifies that an explicit
// InvocationID is honored in non-deterministic mode.
func TestBuildStatement_InvocationIDOverride(t *testing.T) {
	subject := AttestSubject{
		Name:   "checksums.txt",
		Digest: map[string]string{"sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	}
	fixedID := "11111111-2222-3333-4444-555555555555"
	fixedTime := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	data, err := BuildStatement(subject, StatementMetadata{
		BuilderID:    "jdoe@company.com",
		InvocationID: fixedID,
		StartedOn:    fixedTime,
	})
	if err != nil {
		t.Fatalf("BuildStatement: %v", err)
	}
	md := decodeStatement(t, data).Predicate.RunDetails.Metadata
	if md.InvocationID != fixedID {
		t.Errorf("invocationId = %q, want %q", md.InvocationID, fixedID)
	}
	want := fixedTime.Format(time.RFC3339)
	switch {
	case md.StartedOn == nil:
		t.Errorf("startedOn missing, want %q", want)
	case *md.StartedOn != want:
		t.Errorf("startedOn = %q, want %q", *md.StartedOn, want)
	}
}

func TestValidateSubject(t *testing.T) {
	sha256Digest := map[string]string{"sha256": strings.Repeat("ab", 32)}
	tests := []struct {
		name    string
		subject AttestSubject
		wantErr bool
	}{
		{"valid", AttestSubject{Name: "checksums.txt", Digest: sha256Digest}, false},
		{"missing name", AttestSubject{Digest: sha256Digest}, true},
		{"missing digest", AttestSubject{Name: "checksums.txt"}, true},
		{"empty digest value", AttestSubject{Name: "checksums.txt", Digest: map[string]string{"sha256": ""}}, true},
		{"short sha256", AttestSubject{Name: "checksums.txt", Digest: map[string]string{"sha256": "abc123"}}, true},
		{"non-hex sha256", AttestSubject{Name: "checksums.txt", Digest: map[string]string{"sha256": strings.Repeat("zz", 32)}}, true},
		{"short sha512", AttestSubject{Name: "checksums.txt", Digest: map[string]string{"sha512": strings.Repeat("ab", 32)}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSubject(tt.subject)
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("validateSubject() error = %v", err)
				}
				return
			}
			errorstest.WantReportedCode(t, err, errors.ErrCodeInvalidRequest)
		})
	}
}
