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
