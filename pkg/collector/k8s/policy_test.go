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

package k8s

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/NVIDIA/aicr/pkg/measurement"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	fakeclient "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

// Helper to create a mock ClusterPolicy
func createMockClusterPolicy(name, namespace string) *unstructured.Unstructured {
	policy := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "nvidia.com/v1",
			"kind":       "ClusterPolicy",
			"metadata": map[string]any{
				"name": name,
			},
			"spec": map[string]any{
				"operator": map[string]any{
					"defaultRuntime": "containerd",
				},
				"driver": map[string]any{
					"enabled": true,
					"version": "550.54.15",
				},
			},
		},
	}
	if namespace != "" {
		policy.SetNamespace(namespace)
	}
	return policy
}

// Helper to create test collector with mocked clients
func createTestPolicyCollector() *Collector {
	// Create fake node for provider testing
	fakeNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
		Spec: corev1.NodeSpec{
			ProviderID: "aws:///us-west-2a/i-0123456789abcdef0",
		},
	}

	// Create fake kubernetes client
	fakeK8sClient := fakeclient.NewClientset(fakeNode)
	fakeDiscovery := fakeK8sClient.Discovery().(*fakediscovery.FakeDiscovery)
	fakeDiscovery.FakedServerVersion = &version.Info{
		GitVersion: "v1.28.0",
		Platform:   "linux/amd64",
		GoVersion:  "go1.20.7",
	}

	// Set up API resources for ClusterPolicy
	clusterPolicyResource := metav1.APIResource{
		Name:         "clusterpolicies",
		SingularName: "clusterpolicy",
		Namespaced:   false,
		Kind:         "ClusterPolicy",
	}

	fakeDiscovery.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "nvidia.com/v1",
			APIResources: []metav1.APIResource{clusterPolicyResource},
		},
	}

	// Create a simple rest config for dynamic client
	restConfig := &rest.Config{}

	return &Collector{
		ClientSet:  fakeK8sClient,
		RestConfig: restConfig,
	}
}

func TestPolicyCollector_Collect(t *testing.T) {
	t.Setenv("NODE_NAME", "test-node")

	// This test validates the structure when policies are found
	// Note: The dynamic client discovery is complex to mock fully,
	// so we're testing the parsing logic and structure
	ctx := context.TODO()

	collector := createTestPolicyCollector()

	m, err := collector.Collect(ctx)
	assert.NoError(t, err)
	assert.NotNil(t, m)
	assert.Equal(t, measurement.TypeK8s, m.Type)

	// Find the policy subtype
	var policySubtype *measurement.Subtype
	for i := range m.Subtypes {
		if m.Subtypes[i].Name == "policy" {
			policySubtype = &m.Subtypes[i]
			break
		}
	}
	assert.NotNil(t, policySubtype, "Expected to find policy subtype")
	// With our mock setup, we expect empty policies since we can't fully mock dynamic client
	assert.NotNil(t, policySubtype.Data)
}

func TestPolicyCollector_EmptyCluster(t *testing.T) {
	// Test behavior when no policies exist
	ctx := context.TODO()

	collector := createTestPolicyCollector()

	policies, err := collector.collectClusterPolicies(ctx)
	assert.NoError(t, err)
	assert.NotNil(t, policies)
	assert.Empty(t, policies, "Expected no policies in empty cluster")
}

func TestPolicyCollector_WithCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.TODO())
	cancel() // Cancel immediately

	collector := createTestPolicyCollector()

	m, err := collector.Collect(ctx)
	assert.Error(t, err)
	assert.Nil(t, m)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestPolicyCollector_ParsesClusterPolicySpec(t *testing.T) {
	// Test the spec parsing logic in isolation
	policy := createMockClusterPolicy("test-policy", "")

	// Extract spec
	spec, found, err := unstructured.NestedMap(policy.Object, "spec")
	assert.NoError(t, err)
	assert.True(t, found)
	assert.NotNil(t, spec)

	// Verify spec contains expected fields
	operator, found, err := unstructured.NestedMap(spec, "operator")
	assert.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "containerd", operator["defaultRuntime"])

	driver, found, err := unstructured.NestedMap(spec, "driver")
	assert.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, true, driver["enabled"])
	assert.Equal(t, "550.54.15", driver["version"])
}

func TestFlattenSpec(t *testing.T) {
	tests := []struct {
		name     string
		data     map[string]any
		prefix   string
		expected map[string]string
	}{
		{
			name: "simple string",
			data: map[string]any{
				"version": "550.54.15",
			},
			expected: map[string]string{
				"version": "550.54.15",
			},
		},
		{
			name: "nested map",
			data: map[string]any{
				"driver": map[string]any{
					"version": "580.82.07",
					"enabled": true,
				},
			},
			expected: map[string]string{
				"driver.version": "580.82.07",
				"driver.enabled": "true",
			},
		},
		{
			name: "bool value",
			data: map[string]any{
				"enabled": true,
			},
			expected: map[string]string{
				"enabled": "true",
			},
		},
		{
			name: "float64 value",
			data: map[string]any{
				"ratio": float64(3.14),
			},
			expected: map[string]string{
				"ratio": "3.14",
			},
		},
		{
			name: "int value",
			data: map[string]any{
				"count": 42,
			},
			expected: map[string]string{
				"count": "42",
			},
		},
		{
			name: "array value",
			data: map[string]any{
				"items": []any{"a", "b", "c"},
			},
			expected: map[string]string{
				"items": `["a","b","c"]`,
			},
		},
		{
			name: "empty array ignored",
			data: map[string]any{
				"items": []any{},
			},
			expected: map[string]string{},
		},
		{
			name:   "with prefix",
			prefix: "spec",
			data: map[string]any{
				"version": "1.0",
			},
			expected: map[string]string{
				"spec.version": "1.0",
			},
		},
		{
			name: "deep nesting",
			data: map[string]any{
				"a": map[string]any{
					"b": map[string]any{
						"c": "deep",
					},
				},
			},
			expected: map[string]string{
				"a.b.c": "deep",
			},
		},
		{
			name: "default type",
			data: map[string]any{
				"unknown": int64(99),
			},
			expected: map[string]string{
				"unknown": "99",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := make(map[string]measurement.Reading)
			flattenSpec(tt.data, tt.prefix, result)

			if len(result) != len(tt.expected) {
				t.Fatalf("got %d readings, want %d", len(result), len(tt.expected))
			}

			for key, wantVal := range tt.expected {
				got, exists := result[key]
				if !exists {
					t.Errorf("missing key %q", key)
					continue
				}
				if got.Any() != wantVal {
					t.Errorf("key %q = %v, want %q", key, got.Any(), wantVal)
				}
			}
		})
	}
}

func TestPolicyCollector_SpecSerialization(t *testing.T) {
	// Test that spec can be properly serialized to JSON
	policy := createMockClusterPolicy("test-policy", "")

	spec, found, err := unstructured.NestedMap(policy.Object, "spec")
	assert.NoError(t, err)
	assert.True(t, found)

	// Serialize to JSON
	specJSON, err := json.Marshal(spec)
	assert.NoError(t, err)
	assert.NotEmpty(t, specJSON)

	// Deserialize and verify
	var deserializedSpec map[string]any
	err = json.Unmarshal(specJSON, &deserializedSpec)
	assert.NoError(t, err)
	assert.NotNil(t, deserializedSpec)
}
