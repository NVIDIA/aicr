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

package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPublisher_Publish(t *testing.T) {
	tests := []struct {
		name     string
		reg      AgentRegistration
		wantCM   string
		wantPort string
	}{
		{
			name: "publish MCP agent",
			reg: AgentRegistration{
				Name:         "aicrd",
				Protocol:     ProtocolMCP,
				Namespace:    "gpu-operator",
				ServiceName:  "aicrd",
				Port:         8080,
				Capabilities: []string{"recipe", "bundle", "validate"},
				Version:      "1.0.0",
			},
			wantCM:   "dns-aid-aicrd-mcp",
			wantPort: "8080",
		},
		{
			name: "publish A2A agent",
			reg: AgentRegistration{
				Name:         "chat",
				Protocol:     ProtocolA2A,
				Namespace:    "default",
				ServiceName:  "chat-svc",
				Port:         9090,
				Capabilities: []string{"conversation"},
				Version:      "2.0.0",
				PolicyURI:    "https://example.com/policy.json",
				Realm:        "production",
			},
			wantCM:   "dns-aid-chat-a2a",
			wantPort: "9090",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewSimpleClientset()
			p := NewPublisher(clientset,
				WithNamespace(tt.reg.Namespace),
			)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			err := p.Publish(ctx, tt.reg)
			require.NoError(t, err)

			// Verify agent ConfigMap was created
			cm, err := clientset.CoreV1().ConfigMaps(tt.reg.Namespace).Get(ctx, tt.wantCM, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Equal(t, "true", cm.Labels[labelAgent])
			assert.Equal(t, tt.reg.Name, cm.Labels[labelName])
			assert.Equal(t, string(tt.reg.Protocol), cm.Labels[labelProtocol])
			assert.Equal(t, tt.wantPort, cm.Data["port"])
			assert.Equal(t, tt.reg.Version, cm.Data["version"])

			// Verify capabilities in ConfigMap data
			var caps []string
			err = json.Unmarshal([]byte(cm.Data["capabilities"]), &caps)
			require.NoError(t, err)
			assert.Equal(t, tt.reg.Capabilities, caps)

			// Verify index ConfigMap was created
			indexCM, err := clientset.CoreV1().ConfigMaps(tt.reg.Namespace).Get(ctx, indexConfigMapName, metav1.GetOptions{})
			require.NoError(t, err)
			assert.Contains(t, indexCM.Data["agents"], fmt.Sprintf("%s:%s", tt.reg.Name, tt.reg.Protocol))
		})
	}
}

func TestPublisher_PublishUpdate(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	p := NewPublisher(clientset, WithNamespace("default"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reg := AgentRegistration{
		Name:         "aicrd",
		Protocol:     ProtocolMCP,
		Namespace:    "default",
		ServiceName:  "aicrd",
		Port:         8080,
		Capabilities: []string{"recipe"},
		Version:      "1.0.0",
	}

	// First publish
	err := p.Publish(ctx, reg)
	require.NoError(t, err)

	// Update with new version
	reg.Version = "2.0.0"
	reg.Capabilities = []string{"recipe", "bundle"}
	err = p.Publish(ctx, reg)
	require.NoError(t, err)

	// Verify updated
	cm, err := clientset.CoreV1().ConfigMaps("default").Get(ctx, "dns-aid-aicrd-mcp", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "2.0.0", cm.Data["version"])

	var caps []string
	err = json.Unmarshal([]byte(cm.Data["capabilities"]), &caps)
	require.NoError(t, err)
	assert.Equal(t, []string{"recipe", "bundle"}, caps)
}

func TestPublisher_Deregister(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	p := NewPublisher(clientset, WithNamespace("default"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Publish first
	reg := AgentRegistration{
		Name:         "aicrd",
		Protocol:     ProtocolMCP,
		Namespace:    "default",
		ServiceName:  "aicrd",
		Port:         8080,
		Capabilities: []string{"recipe"},
		Version:      "1.0.0",
	}
	err := p.Publish(ctx, reg)
	require.NoError(t, err)

	// Deregister
	err = p.Deregister(ctx, "aicrd", ProtocolMCP)
	require.NoError(t, err)

	// Verify ConfigMap deleted
	_, err = clientset.CoreV1().ConfigMaps("default").Get(ctx, "dns-aid-aicrd-mcp", metav1.GetOptions{})
	assert.True(t, err != nil, "expected ConfigMap to be deleted")

	// Verify index updated (empty)
	indexCM, err := clientset.CoreV1().ConfigMaps("default").Get(ctx, indexConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Empty(t, indexCM.Data["agents"])
}

func TestPublisher_DeregisterNonexistent(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	p := NewPublisher(clientset, WithNamespace("default"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Deregistering a non-existent agent should not error
	err := p.Deregister(ctx, "nonexistent", ProtocolMCP)
	require.NoError(t, err)
}

func TestPublisher_MultipleAgents(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	p := NewPublisher(clientset, WithNamespace("default"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	agents := []AgentRegistration{
		{Name: "aicrd", Protocol: ProtocolMCP, ServiceName: "aicrd", Port: 8080, Capabilities: []string{"recipe"}, Version: "1.0"},
		{Name: "chat", Protocol: ProtocolA2A, ServiceName: "chat", Port: 9090, Capabilities: []string{"chat"}, Version: "1.0"},
	}

	for _, reg := range agents {
		err := p.Publish(ctx, reg)
		require.NoError(t, err)
	}

	// Verify index has both agents
	indexCM, err := clientset.CoreV1().ConfigMaps("default").Get(ctx, indexConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, indexCM.Data["agents"], "aicrd:mcp")
	assert.Contains(t, indexCM.Data["agents"], "chat:a2a")
}

func TestPublisher_WithLabels(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	p := NewPublisher(clientset,
		WithNamespace("default"),
		WithLabels(map[string]string{"app": "aicr", "team": "gpu"}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	reg := AgentRegistration{
		Name:         "aicrd",
		Protocol:     ProtocolMCP,
		ServiceName:  "aicrd",
		Port:         8080,
		Capabilities: []string{"recipe"},
		Version:      "1.0.0",
	}

	err := p.Publish(ctx, reg)
	require.NoError(t, err)

	cm, err := clientset.CoreV1().ConfigMaps("default").Get(ctx, "dns-aid-aicrd-mcp", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "aicr", cm.Labels["app"])
	assert.Equal(t, "gpu", cm.Labels["team"])
	assert.Equal(t, "true", cm.Labels[labelAgent])
}

func TestPublisher_ValidationRejectsInvalidNames(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	p := NewPublisher(clientset, WithNamespace("default"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tests := []struct {
		name     string
		reg      AgentRegistration
		errMsg   string
	}{
		{
			name: "uppercase name",
			reg:  AgentRegistration{Name: "MyAgent", Protocol: ProtocolMCP, ServiceName: "svc", Version: "1.0"},
			errMsg: "not a valid DNS label",
		},
		{
			name: "name with slash",
			reg:  AgentRegistration{Name: "../etc", Protocol: ProtocolMCP, ServiceName: "svc", Version: "1.0"},
			errMsg: "not a valid DNS label",
		},
		{
			name: "empty name",
			reg:  AgentRegistration{Name: "", Protocol: ProtocolMCP, ServiceName: "svc", Version: "1.0"},
			errMsg: "not a valid DNS label",
		},
		{
			name: "name starting with hyphen",
			reg:  AgentRegistration{Name: "-bad", Protocol: ProtocolMCP, ServiceName: "svc", Version: "1.0"},
			errMsg: "not a valid DNS label",
		},
		{
			name: "name exceeding limit",
			reg:  AgentRegistration{Name: strings.Repeat("a", 250), Protocol: ProtocolMCP, ServiceName: "svc", Version: "1.0"},
			errMsg: "exceeds 253 character limit",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := p.Publish(ctx, tt.reg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errMsg)
		})
	}
}

func TestPublisher_ValidationAcceptsValidNames(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	p := NewPublisher(clientset, WithNamespace("default"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	validNames := []string{"aicrd", "my-agent", "agent1", "a", "x99"}
	for _, name := range validNames {
		t.Run(name, func(t *testing.T) {
			reg := AgentRegistration{
				Name:         name,
				Protocol:     ProtocolMCP,
				ServiceName:  "svc",
				Port:         8080,
				Capabilities: []string{"test"},
				Version:      "1.0",
			}
			err := p.Publish(ctx, reg)
			require.NoError(t, err)
		})
	}
}
