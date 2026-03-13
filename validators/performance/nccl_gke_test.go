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
	"os"
	"testing"

	"github.com/NVIDIA/aicr/pkg/recipe"
)

func TestSplitYAMLDocuments(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		wantLen  int
		wantKind []string // expected kind of each document
	}{
		{
			name:    "empty",
			content: "",
			wantLen: 0,
		},
		{
			name:    "single document",
			content: "apiVersion: v1\nkind: Pod\nmetadata:\n  name: test",
			wantLen: 1,
		},
		{
			name:    "two documents",
			content: "apiVersion: v1\nkind: Service\nmetadata:\n  name: svc1\n---\napiVersion: v1\nkind: Pod\nmetadata:\n  name: pod1",
			wantLen: 2,
		},
		{
			name:    "leading separator",
			content: "---\napiVersion: v1\nkind: Pod\nmetadata:\n  name: test",
			wantLen: 1,
		},
		{
			name:    "comment-only document skipped",
			content: "# This is a comment\n# Another comment\n---\napiVersion: v1\nkind: Pod\nmetadata:\n  name: test",
			wantLen: 1,
		},
		{
			name:    "four documents like GKE template",
			content: "apiVersion: v1\nkind: Service\nmetadata:\n  name: svc1\n---\napiVersion: v1\nkind: Service\nmetadata:\n  name: svc2\n---\napiVersion: v1\nkind: Pod\nmetadata:\n  name: pod1\n---\napiVersion: v1\nkind: Pod\nmetadata:\n  name: pod2",
			wantLen: 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docs := splitYAMLDocuments(tt.content)
			if len(docs) != tt.wantLen {
				t.Errorf("splitYAMLDocuments() returned %d docs, want %d", len(docs), tt.wantLen)
			}
		})
	}
}

func TestPeekKind(t *testing.T) {
	tests := []struct {
		name    string
		doc     string
		want    string
		wantErr bool
	}{
		{
			name: "Service",
			doc:  "apiVersion: v1\nkind: Service\nmetadata:\n  name: test",
			want: "Service",
		},
		{
			name: "Pod",
			doc:  "apiVersion: v1\nkind: Pod\nmetadata:\n  name: test",
			want: "Pod",
		},
		{
			name:    "no kind field",
			doc:     "apiVersion: v1\nmetadata:\n  name: test",
			wantErr: true,
		},
		{
			name:    "invalid YAML",
			doc:     "{{invalid",
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := peekKind(tt.doc)
			if (err != nil) != tt.wantErr {
				t.Errorf("peekKind() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("peekKind() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSplitYAMLDocumentsWithGKETemplate(t *testing.T) {
	// Verify the actual GKE testdata template produces the expected document count and kinds.
	content, err := testdataContent("h100", "gke", "nccl-test-tcpxo.yaml")
	if err != nil {
		t.Skipf("GKE testdata not available: %v", err)
	}

	docs := splitYAMLDocuments(content)

	// The GKE runtime.yaml should contain 4 documents: 2 Services + 2 Pods.
	if len(docs) != 4 {
		t.Fatalf("expected 4 documents from GKE runtime.yaml, got %d", len(docs))
	}

	expectedKinds := []string{"Service", "Service", "Pod", "Pod"}
	for i, doc := range docs {
		kind, err := peekKind(doc)
		if err != nil {
			t.Fatalf("doc %d: peekKind() error: %v", i, err)
		}
		if kind != expectedKinds[i] {
			t.Errorf("doc %d: kind = %q, want %q", i, kind, expectedKinds[i])
		}
	}
}

// testdataContent reads a testdata file and returns its content with template vars unsubstituted.
func testdataContent(accelerator, service, filename string) (string, error) {
	content, err := os.ReadFile(templatePath(
		recipe.CriteriaAcceleratorType(accelerator),
		recipe.CriteriaServiceType(service),
		filename,
	))
	if err != nil {
		return "", err
	}
	return string(content), nil
}
