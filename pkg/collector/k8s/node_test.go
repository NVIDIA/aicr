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
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNodeCollector_CollectNode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Setenv("NODE_NAME", testNodeName)

	ctx := context.TODO()
	collector := createTestCollector()

	nodeData, err := collector.collectNode(ctx)
	assert.NoError(t, err)
	assert.NotNil(t, nodeData)

	// Verify expected node data fields
	if assert.Contains(t, nodeData, "source-node") {
		assert.Equal(t, testNodeName, nodeData["source-node"].Any())
	}

	if assert.Contains(t, nodeData, "provider") {
		assert.Equal(t, "eks", nodeData["provider"].Any())
	}

	if assert.Contains(t, nodeData, "provider-id") {
		assert.Equal(t, "aws:///us-west-2a/i-0123456789abcdef0", nodeData["provider-id"].Any())
	}
}

func TestNodeCollector_CollectNodeWithFullDetails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	nodeName := "detailed-node"
	t.Setenv("NODE_NAME", nodeName)

	// Create a node with full details
	fakeNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
		Spec: corev1.NodeSpec{
			ProviderID: "gce://my-project/us-central1-a/gke-cluster-node",
		},
		Status: corev1.NodeStatus{
			NodeInfo: corev1.NodeSystemInfo{
				ContainerRuntimeVersion: "containerd://1.7.2",
				KubeletVersion:          "v1.28.0",
				KernelVersion:           "5.15.0-91-generic",
				OperatingSystem:         "linux",
				OSImage:                 "Ubuntu 22.04.3 LTS",
			},
		},
	}

	collector := createTestCollector()
	// Replace the default node
	_, err := collector.ClientSet.CoreV1().Nodes().Create(context.TODO(), fakeNode, metav1.CreateOptions{})
	assert.NoError(t, err)

	ctx := context.TODO()
	nodeData, err := collector.collectNode(ctx)
	assert.NoError(t, err)
	assert.NotNil(t, nodeData)

	// Verify all fields
	assert.Equal(t, nodeName, nodeData["source-node"].Any())
	assert.Equal(t, "gke", nodeData["provider"].Any())
	assert.Equal(t, "gce://my-project/us-central1-a/gke-cluster-node", nodeData["provider-id"].Any())
	assert.Equal(t, "containerd://1.7.2", nodeData["container-runtime-id"].Any())
	assert.Equal(t, "containerd", nodeData["container-runtime-name"].Any())
	assert.Equal(t, "1.7.2", nodeData["container-runtime-version"].Any())
	assert.Equal(t, "v1.28.0", nodeData["kubelet-version"].Any())
	assert.Equal(t, "5.15.0-91-generic", nodeData["kernel-version"].Any())
	assert.Equal(t, "linux", nodeData["operating-system"].Any())
	assert.Equal(t, "Ubuntu 22.04.3 LTS", nodeData["os-image"].Any())
}

func TestNodeCollector_CollectNodeNoProviderID(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	nodeName := "no-provider-node"
	t.Setenv("NODE_NAME", nodeName)

	// Create a node without provider ID
	fakeNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeName,
		},
		Spec: corev1.NodeSpec{
			ProviderID: "", // Empty provider ID
		},
	}

	collector := createTestCollector()
	_, err := collector.ClientSet.CoreV1().Nodes().Create(context.TODO(), fakeNode, metav1.CreateOptions{})
	assert.NoError(t, err)

	ctx := context.TODO()
	nodeData, err := collector.collectNode(ctx)
	assert.NoError(t, err)
	assert.NotNil(t, nodeData)

	// Should have source-node but no provider fields
	assert.Contains(t, nodeData, "source-node")
	assert.NotContains(t, nodeData, "provider")
	assert.NotContains(t, nodeData, "provider-id")
}

func TestNodeCollector_CollectNodeNoEnvironment(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Clear all node name environment variables
	t.Setenv("NODE_NAME", "")
	t.Setenv("KUBERNETES_NODE_NAME", "")
	t.Setenv("HOSTNAME", "")

	ctx := context.TODO()
	collector := createTestCollector()

	nodeData, err := collector.collectNode(ctx)
	assert.Error(t, err)
	assert.Nil(t, nodeData)
	assert.Contains(t, err.Error(), "node name not set in environment")
}

func TestNodeCollector_CollectNodeNotFound(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Setenv("NODE_NAME", "non-existent-node")

	ctx := context.TODO()
	collector := createTestCollector()

	nodeData, err := collector.collectNode(ctx)
	assert.Error(t, err)
	assert.Nil(t, nodeData)
	assert.Contains(t, err.Error(), "failed to get node")
}

func TestNodeCollector_CollectNodeCanceledContext(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	t.Setenv("NODE_NAME", testNodeName)

	ctx, cancel := context.WithCancel(context.TODO())
	cancel() // Cancel immediately

	collector := createTestCollector()

	nodeData, err := collector.collectNode(ctx)
	assert.Error(t, err)
	assert.Nil(t, nodeData)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestParseProvider(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		want       string
	}{
		{
			name:       "AWS EKS",
			providerID: "aws:///us-west-2a/i-0123456789abcdef0",
			want:       "eks",
		},
		{
			name:       "GCP GKE",
			providerID: "gce://my-project/us-central1-a/gke-cluster-default-pool-node-abc123",
			want:       "gke",
		},
		{
			name:       "Azure AKS",
			providerID: "azure:///subscriptions/12345678-1234-1234-1234-123456789012/resourceGroups/my-rg/providers/Microsoft.Compute/virtualMachines/aks-nodepool1-12345678-vmss000000",
			want:       "aks",
		},
		{
			name:       "OCI OKE",
			providerID: "oci://ocid1.instance.oc1.phx.abcdef123456",
			want:       "oke",
		},
		{
			name:       "empty provider",
			providerID: "",
			want:       "",
		},
		{
			name:       "unknown format",
			providerID: "custom-provider://some-id",
			want:       "custom-provider",
		},
		{
			name:       "no scheme separator",
			providerID: "just-a-string",
			want:       "just-a-string",
		},
		{
			name:       "uppercase provider normalized",
			providerID: "AWS:///us-west-2a/i-abc123",
			want:       "eks",
		},
		{
			name:       "provider with whitespace",
			providerID: " gce ://project/zone/node",
			want:       "gke",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseProvider(tt.providerID)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestGetNodeName(t *testing.T) {
	tests := []struct {
		name     string
		setEnv   map[string]string
		expected string
	}{
		{
			name:     "NODE_NAME set",
			setEnv:   map[string]string{"NODE_NAME": "test-node-1"},
			expected: "test-node-1",
		},
		{
			name:     "KUBERNETES_NODE_NAME fallback",
			setEnv:   map[string]string{"KUBERNETES_NODE_NAME": "k8s-node-2"},
			expected: "k8s-node-2",
		},
		{
			name:     "HOSTNAME fallback",
			setEnv:   map[string]string{"HOSTNAME": "host-3"},
			expected: "host-3",
		},
		{
			name:     "NODE_NAME takes precedence",
			setEnv:   map[string]string{"NODE_NAME": "node-1", "KUBERNETES_NODE_NAME": "node-2", "HOSTNAME": "node-3"},
			expected: "node-1",
		},
		{
			name:     "KUBERNETES_NODE_NAME over HOSTNAME",
			setEnv:   map[string]string{"KUBERNETES_NODE_NAME": "k8s-node", "HOSTNAME": "hostname"},
			expected: "k8s-node",
		},
		{
			name:     "no env vars",
			setEnv:   map[string]string{},
			expected: "",
		},
		{
			name:     "empty string values ignored",
			setEnv:   map[string]string{"NODE_NAME": "", "KUBERNETES_NODE_NAME": "", "HOSTNAME": "fallback-host"},
			expected: "fallback-host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear environment
			t.Setenv("NODE_NAME", "")
			t.Setenv("KUBERNETES_NODE_NAME", "")
			t.Setenv("HOSTNAME", "")

			// Set test environment
			for k, v := range tt.setEnv {
				t.Setenv(k, v)
			}

			got := GetNodeName()
			assert.Equal(t, tt.expected, got)
		})
	}
}
