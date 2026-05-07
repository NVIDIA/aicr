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

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseAssertFilesIncludesReferencedAssertions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	clusterDir := filepath.Join(root, "cluster")
	sharedDir := filepath.Join(root, "common")
	if err := os.MkdirAll(clusterDir, 0o755); err != nil {
		t.Fatalf("mkdir cluster: %v", err)
	}
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		t.Fatalf("mkdir common: %v", err)
	}

	writeTestFile(t, filepath.Join(clusterDir, "assert-local.yaml"), `
apiVersion: v1
kind: Namespace
metadata:
  name: local
`)
	writeTestFile(t, filepath.Join(sharedDir, "assert-shared.yaml"), `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shared
  namespace: shared-ns
`)
	writeTestFile(t, filepath.Join(clusterDir, "chainsaw-test.yaml"), `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
spec:
  steps:
    - try:
        - assert:
            file: assert-local.yaml
        - assert:
            file: ../common/assert-shared.yaml
`)

	resources, err := parseAssertFiles(clusterDir)
	if err != nil {
		t.Fatalf("parse assert files: %v", err)
	}
	if len(resources) != 2 {
		t.Fatalf("resource count = %d, want 2", len(resources))
	}

	got := map[string]string{}
	for _, resource := range resources {
		got[resource.Metadata.Name] = resource.SourceFile
	}
	if got["local"] != "assert-local.yaml" {
		t.Fatalf("local source = %q, want assert-local.yaml", got["local"])
	}
	if got["shared"] != "assert-shared.yaml" {
		t.Fatalf("shared source = %q, want assert-shared.yaml", got["shared"])
	}
}

func TestParseAssertFilesWithoutChainsawTestKeepsDirectoryScanning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "assert-only.yaml"), `
apiVersion: v1
kind: Namespace
metadata:
  name: only
`)

	resources, err := parseAssertFiles(dir)
	if err != nil {
		t.Fatalf("parse assert files: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("resource count = %d, want 1", len(resources))
	}
	if resources[0].Metadata.Name != "only" {
		t.Fatalf("resource name = %q, want only", resources[0].Metadata.Name)
	}
}

func TestParseAssertFilesMultiDocumentChainsawTest(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	suiteDir := filepath.Join(root, "suite")
	sharedDir := filepath.Join(root, "common")
	if err := os.MkdirAll(suiteDir, 0o755); err != nil {
		t.Fatalf("mkdir suite: %v", err)
	}
	if err := os.MkdirAll(sharedDir, 0o755); err != nil {
		t.Fatalf("mkdir common: %v", err)
	}

	writeTestFile(t, filepath.Join(sharedDir, "assert-first.yaml"), `
apiVersion: v1
kind: Namespace
metadata:
  name: first
`)
	writeTestFile(t, filepath.Join(sharedDir, "assert-second.yaml"), `
apiVersion: v1
kind: Namespace
metadata:
  name: second
`)
	// Multi-document chainsaw-test.yaml: two Test documents referencing different files.
	writeTestFile(t, filepath.Join(suiteDir, "chainsaw-test.yaml"), `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
spec:
  steps:
    - try:
        - assert:
            file: ../common/assert-first.yaml
---
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
spec:
  steps:
    - try:
        - assert:
            file: ../common/assert-second.yaml
`)

	resources, err := parseAssertFiles(suiteDir)
	if err != nil {
		t.Fatalf("parse assert files: %v", err)
	}
	if len(resources) != 2 {
		t.Fatalf("resource count = %d, want 2", len(resources))
	}

	got := map[string]bool{}
	for _, r := range resources {
		got[r.Metadata.Name] = true
	}
	if !got["first"] {
		t.Fatal("missing resource 'first' from first YAML document")
	}
	if !got["second"] {
		t.Fatal("missing resource 'second' from second YAML document")
	}
}

func TestParseAssertFilesInvalidAssertionYAML(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "assert-bad.yaml"), `
apiVersion: v1
kind: Namespace
metadata:
  name: [invalid yaml
`)

	if _, err := parseAssertFiles(dir); err == nil {
		t.Fatal("expected error for invalid assertion YAML, got nil")
	}
}

func TestReferencedAssertFilesInvalidChainsawYAML(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "chainsaw-test.yaml"), `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
spec:
  steps:
    - try:
        - assert:
            file: [invalid yaml
`)

	if _, err := referencedAssertFiles(dir); err == nil {
		t.Fatal("expected error for invalid chainsaw-test.yaml, got nil")
	}
}

func TestParseAssertFilesMissingReferencedAssertion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "chainsaw-test.yaml"), `
apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
spec:
  steps:
    - try:
        - assert:
            file: assert-missing.yaml
`)

	if _, err := parseAssertFiles(dir); err == nil {
		t.Fatal("expected error for missing referenced assertion file, got nil")
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
