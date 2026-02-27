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

package chainsaw

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeFetcher implements ResourceFetcher for testing.
type fakeFetcher struct {
	resources map[string]map[string]interface{}
}

func (f *fakeFetcher) Fetch(_ context.Context, apiVersion, kind, namespace, name string) (map[string]interface{}, error) {
	key := fmt.Sprintf("%s/%s/%s/%s", apiVersion, kind, namespace, name)
	obj, ok := f.resources[key]
	if !ok {
		return nil, fmt.Errorf("resource not found: %s", key)
	}
	return obj, nil
}

func TestRunEmpty(t *testing.T) {
	results := Run(t.Context(), nil, 2*time.Minute, &fakeFetcher{})
	if results != nil {
		t.Errorf("Run(nil) = %v, want nil", results)
	}
}

func TestRunSinglePass(t *testing.T) {
	fetcher := &fakeFetcher{
		resources: map[string]map[string]interface{}{
			"apps/v1/Deployment/gpu-operator/gpu-operator": {
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]interface{}{
					"name":      "gpu-operator",
					"namespace": "gpu-operator",
				},
				"status": map[string]interface{}{
					"availableReplicas": int64(1),
					"readyReplicas":     int64(1),
				},
			},
		},
	}

	asserts := []ComponentAssert{
		{
			Name: "gpu-operator",
			AssertYAML: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-operator
  namespace: gpu-operator
status:
  availableReplicas: 1`,
		},
	}

	results := Run(t.Context(), asserts, 10*time.Second, fetcher)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	if !results[0].Passed {
		t.Errorf("expected pass, got fail: output=%q error=%v", results[0].Output, results[0].Error)
	}
}

func TestRunSingleFieldMismatch(t *testing.T) {
	fetcher := &fakeFetcher{
		resources: map[string]map[string]interface{}{
			"apps/v1/Deployment/gpu-operator/gpu-operator": {
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]interface{}{
					"name":      "gpu-operator",
					"namespace": "gpu-operator",
				},
				"status": map[string]interface{}{
					"availableReplicas": int64(0),
				},
			},
		},
	}

	asserts := []ComponentAssert{
		{
			Name: "gpu-operator",
			AssertYAML: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-operator
  namespace: gpu-operator
status:
  availableReplicas: 1`,
		},
	}

	// Use minimal timeout so the retry loop exits quickly.
	results := Run(t.Context(), asserts, 1*time.Millisecond, fetcher)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	r := results[0]
	if r.Passed {
		t.Error("expected fail, got pass")
	}
	if r.Error == nil {
		t.Error("expected non-nil error")
	}
}

func TestRunSingleResourceNotFound(t *testing.T) {
	fetcher := &fakeFetcher{resources: map[string]map[string]interface{}{}}

	asserts := []ComponentAssert{
		{
			Name: "gpu-operator",
			AssertYAML: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-operator
  namespace: gpu-operator
status:
  availableReplicas: 1`,
		},
	}

	results := Run(t.Context(), asserts, 1*time.Millisecond, fetcher)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	r := results[0]
	if r.Passed {
		t.Error("expected fail, got pass")
	}
	if r.Error == nil {
		t.Fatal("expected non-nil error")
	}
	if !strings.Contains(r.Error.Error(), "failed to fetch") {
		t.Errorf("error %q should contain 'failed to fetch'", r.Error.Error())
	}
}

func TestRunMultipleComponents(t *testing.T) {
	fetcher := &fakeFetcher{
		resources: map[string]map[string]interface{}{
			"apps/v1/Deployment/gpu-operator/gpu-operator": {
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]interface{}{"name": "gpu-operator", "namespace": "gpu-operator"},
				"status":     map[string]interface{}{"availableReplicas": int64(1)},
			},
			"apps/v1/Deployment/network-operator/network-operator": {
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]interface{}{"name": "network-operator", "namespace": "network-operator"},
				"status":     map[string]interface{}{"availableReplicas": int64(1)},
			},
		},
	}

	asserts := []ComponentAssert{
		{
			Name: "gpu-operator",
			AssertYAML: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-operator
  namespace: gpu-operator
status:
  availableReplicas: 1`,
		},
		{
			Name: "network-operator",
			AssertYAML: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: network-operator
  namespace: network-operator
status:
  availableReplicas: 1`,
		},
	}

	results := Run(t.Context(), asserts, 10*time.Second, fetcher)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	for i, r := range results {
		if r.Component != asserts[i].Name {
			t.Errorf("results[%d].Component = %q, want %q", i, r.Component, asserts[i].Name)
		}
		if !r.Passed {
			t.Errorf("results[%d].Passed = false, want true: %v", i, r.Error)
		}
	}
}

func TestRunContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // cancel immediately

	asserts := []ComponentAssert{
		{
			Name:       "cancelled-component",
			AssertYAML: "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n",
		},
	}

	results := Run(ctx, asserts, 30*time.Second, &fakeFetcher{})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	r := results[0]
	if r.Component != "cancelled-component" {
		t.Errorf("Component = %q, want %q", r.Component, "cancelled-component")
	}
	if r.Passed {
		t.Error("expected Passed=false for cancelled context")
	}
	if r.Error == nil {
		t.Error("expected non-nil Error for cancelled context")
	}
}

func TestRunMultiDocumentYAML(t *testing.T) {
	fetcher := &fakeFetcher{
		resources: map[string]map[string]interface{}{
			"apps/v1/Deployment/gpu-operator/gpu-operator": {
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]interface{}{"name": "gpu-operator", "namespace": "gpu-operator"},
				"status":     map[string]interface{}{"availableReplicas": int64(1)},
			},
			"v1/Service/gpu-operator/gpu-operator": {
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata":   map[string]interface{}{"name": "gpu-operator", "namespace": "gpu-operator"},
			},
		},
	}

	multiDoc := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-operator
  namespace: gpu-operator
status:
  availableReplicas: 1
---
apiVersion: v1
kind: Service
metadata:
  name: gpu-operator
  namespace: gpu-operator`

	asserts := []ComponentAssert{
		{Name: "gpu-operator", AssertYAML: multiDoc},
	}

	results := Run(t.Context(), asserts, 10*time.Second, fetcher)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Passed {
		t.Errorf("expected pass for multi-doc: %v", results[0].Error)
	}
}

func TestSplitYAMLDocuments(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantLen int
		wantErr bool
	}{
		{
			name:    "single document",
			raw:     "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: test\n",
			wantLen: 1,
		},
		{
			name:    "two documents",
			raw:     "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\n",
			wantLen: 2,
		},
		{
			name:    "empty string",
			raw:     "",
			wantLen: 0,
		},
		{
			name:    "only separators",
			raw:     "---\n---\n",
			wantLen: 0,
		},
		{
			name:    "invalid YAML",
			raw:     ":\n  bad:\n    - [invalid",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docs, err := splitYAMLDocuments(tt.raw)
			if (err != nil) != tt.wantErr {
				t.Fatalf("splitYAMLDocuments() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && len(docs) != tt.wantLen {
				t.Errorf("got %d docs, want %d", len(docs), tt.wantLen)
			}
		})
	}
}

func TestAssertSingleDocumentMissingFields(t *testing.T) {
	tests := []struct {
		name        string
		doc         map[string]interface{}
		errContains string
	}{
		{
			name:        "missing apiVersion",
			doc:         map[string]interface{}{"kind": "Deployment", "metadata": map[string]interface{}{"name": "x"}},
			errContains: "missing required fields",
		},
		{
			name:        "missing kind",
			doc:         map[string]interface{}{"apiVersion": "v1", "metadata": map[string]interface{}{"name": "x"}},
			errContains: "missing required fields",
		},
		{
			name:        "missing name",
			doc:         map[string]interface{}{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]interface{}{}},
			errContains: "missing required fields",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := assertSingleDocument(t.Context(), tt.doc, &fakeFetcher{})
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.errContains) {
				t.Errorf("error %q should contain %q", err.Error(), tt.errContains)
			}
		})
	}
}

// fakeChainsawBinary implements ChainsawBinary for testing.
type fakeChainsawBinary struct {
	passed  bool
	output  string
	err     error
	lastDir string
}

func (f *fakeChainsawBinary) RunTest(_ context.Context, testDir string) (bool, string, error) {
	f.lastDir = testDir
	return f.passed, f.output, f.err
}

func TestIsChainsawTest(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "chainsaw test format",
			raw: `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: gpu-operator-health-check
spec:
  steps:
    - name: validate-deployment
      try:
        - assert:
            resource:
              apiVersion: apps/v1
              kind: Deployment`,
			want: true,
		},
		{
			name: "raw k8s resource",
			raw: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-operator
  namespace: gpu-operator
status:
  availableReplicas: 1`,
			want: false,
		},
		{
			name: "empty string",
			raw:  "",
			want: false,
		},
		{
			name: "has apiVersion but not Test kind",
			raw: `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Configuration`,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isChainsawTest(tt.raw)
			if got != tt.want {
				t.Errorf("isChainsawTest() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRunChainsawBinaryPass(t *testing.T) {
	fake := &fakeChainsawBinary{passed: true, output: "all tests passed"}

	asserts := []ComponentAssert{
		{
			Name: "gpu-operator",
			AssertYAML: `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: test
spec:
  steps: []`,
		},
	}

	results := Run(t.Context(), asserts, 10*time.Second, &fakeFetcher{},
		WithChainsawBinary(fake))

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	r := results[0]
	if !r.Passed {
		t.Errorf("expected pass, got fail: output=%q error=%v", r.Output, r.Error)
	}
	if r.Output != "all tests passed" {
		t.Errorf("output = %q, want %q", r.Output, "all tests passed")
	}
}

func TestRunChainsawBinaryFail(t *testing.T) {
	fake := &fakeChainsawBinary{passed: false, output: "assertion failed: availableReplicas expected 1 got 0"}

	asserts := []ComponentAssert{
		{
			Name: "gpu-operator",
			AssertYAML: `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: test
spec:
  steps: []`,
		},
	}

	results := Run(t.Context(), asserts, 10*time.Second, &fakeFetcher{},
		WithChainsawBinary(fake))

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	r := results[0]
	if r.Passed {
		t.Error("expected fail, got pass")
	}
	if !strings.Contains(r.Output, "assertion failed") {
		t.Errorf("output = %q, want to contain 'assertion failed'", r.Output)
	}
}

func TestRunChainsawBinaryNotConfigured(t *testing.T) {
	asserts := []ComponentAssert{
		{
			Name: "gpu-operator",
			AssertYAML: `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: test
spec:
  steps: []`,
		},
	}

	// No WithChainsawBinary option — binary not configured.
	results := Run(t.Context(), asserts, 10*time.Second, &fakeFetcher{})

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	r := results[0]
	if r.Passed {
		t.Error("expected fail when binary not configured")
	}
	if r.Error == nil {
		t.Fatal("expected non-nil error")
	}
	if !strings.Contains(r.Error.Error(), "chainsaw binary not configured") {
		t.Errorf("error = %q, want to contain 'chainsaw binary not configured'", r.Error.Error())
	}
}

func TestRunChainsawBinaryWritesCorrectFiles(t *testing.T) {
	yamlContent := `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: test
spec:
  steps: []`

	var capturedDir string
	fake := &fakeChainsawBinary{passed: true}

	asserts := []ComponentAssert{
		{Name: "gpu-operator", AssertYAML: yamlContent},
	}

	results := Run(t.Context(), asserts, 10*time.Second, &fakeFetcher{},
		WithChainsawBinary(fake))

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	capturedDir = fake.lastDir
	if capturedDir == "" {
		t.Fatal("expected RunTest to be called with a test directory")
	}

	// The temp dir is cleaned up after Run, so verify the captured path had the right structure.
	if !strings.Contains(capturedDir, "gpu-operator") {
		t.Errorf("test directory %q should contain component name 'gpu-operator'", capturedDir)
	}
}

func TestRunChainsawBinaryExecutionError(t *testing.T) {
	fake := &fakeChainsawBinary{
		err:    fmt.Errorf("binary not found"),
		output: "",
	}

	asserts := []ComponentAssert{
		{
			Name: "gpu-operator",
			AssertYAML: `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: test
spec:
  steps: []`,
		},
	}

	results := Run(t.Context(), asserts, 10*time.Second, &fakeFetcher{},
		WithChainsawBinary(fake))

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	r := results[0]
	if r.Passed {
		t.Error("expected fail on execution error")
	}
	if r.Error == nil {
		t.Fatal("expected non-nil error")
	}
}

func TestBackwardCompatibilityRawYAML(t *testing.T) {
	// Raw K8s YAML should still use the Go library path even with binary configured.
	fetcher := &fakeFetcher{
		resources: map[string]map[string]interface{}{
			"apps/v1/Deployment/gpu-operator/gpu-operator": {
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata":   map[string]interface{}{"name": "gpu-operator", "namespace": "gpu-operator"},
				"status":     map[string]interface{}{"availableReplicas": int64(1)},
			},
		},
	}

	asserts := []ComponentAssert{
		{
			Name: "gpu-operator",
			AssertYAML: `apiVersion: apps/v1
kind: Deployment
metadata:
  name: gpu-operator
  namespace: gpu-operator
status:
  availableReplicas: 1`,
		},
	}

	fake := &fakeChainsawBinary{passed: false, output: "should not be called"}

	results := Run(t.Context(), asserts, 10*time.Second, fetcher,
		WithChainsawBinary(fake))

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	// Should pass via Go library, not via binary.
	if !results[0].Passed {
		t.Errorf("expected pass via Go library: output=%q error=%v", results[0].Output, results[0].Error)
	}

	// Binary should not have been called.
	if fake.lastDir != "" {
		t.Error("binary should not have been called for raw K8s YAML")
	}
}

func TestRunChainsawBinaryTempDirCreated(t *testing.T) {
	yamlContent := `apiVersion: chainsaw.kyverno.io/v1alpha1
kind: Test
metadata:
  name: test
spec:
  steps: []`

	// Use a fake that verifies the test file exists when called.
	verifyingFake := &verifyingChainsawBinary{t: t}

	asserts := []ComponentAssert{
		{Name: "my-component", AssertYAML: yamlContent},
	}

	results := Run(t.Context(), asserts, 10*time.Second, &fakeFetcher{},
		WithChainsawBinary(verifyingFake))

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !results[0].Passed {
		t.Errorf("expected pass: %v", results[0].Error)
	}
}

// verifyingChainsawBinary checks that the test file exists when RunTest is called.
type verifyingChainsawBinary struct {
	t *testing.T
}

func (v *verifyingChainsawBinary) RunTest(_ context.Context, testDir string) (bool, string, error) {
	testFile := filepath.Join(testDir, "chainsaw-test.yaml")
	content, err := os.ReadFile(testFile)
	if err != nil {
		v.t.Errorf("expected test file at %s: %v", testFile, err)
		return false, "", err
	}
	if len(content) == 0 {
		v.t.Error("test file is empty")
		return false, "", fmt.Errorf("empty test file")
	}
	if !strings.Contains(string(content), "chainsaw.kyverno.io") {
		v.t.Errorf("test file content does not contain chainsaw API version: %s", content)
	}
	return true, "verified", nil
}
